// Package httpx is the HTTP plumbing shared by every module: error rendering in the frontend's ApiError shape,
// request ids and access logs, security headers, CORS, cookies and client-IP detection. It mirrors the pipeline
// the Nest API builds in configure-app.ts so both servers answer identically.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	userKey
)

// Logger writes one JSON object per line to stdout, like the Nest API's structured logs.
var Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
	ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			a.Key = "time"
		}
		return a
	},
}))

var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// NewUUID returns a random version-4 UUID.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// RequestID is the correlation id for this request ("" outside a request).
func RequestID(r *http.Request) string {
	v, _ := r.Context().Value(requestIDKey).(string)
	return v
}

// HandlerFunc is a handler that returns an error instead of writing it. Wrap turns it into an http.HandlerFunc.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Wrap renders a returned error as the standard ApiError JSON.
func Wrap(h HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			WriteError(w, r, err)
		}
	}
}

// ApiError is the JSON error body.
type ApiError struct {
	Code      apperr.Code       `json:"code"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
	RequestID string            `json:"requestId,omitempty"`
}

// HTTPError is a framework-level error with a status (unknown route, rate limit, bad body).
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

var codeForStatus = map[int]apperr.Code{400: apperr.Validation, 401: apperr.Unauthorized, 403: apperr.Forbidden, 404: apperr.NotFound, 409: apperr.Conflict, 422: apperr.Validation, 429: apperr.Forbidden}

// WriteError turns every error into the ApiError shape. Unexpected errors are logged with the request id and
// returned as a generic message: no stack traces or internals leak.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status := 500
	body := ApiError{}
	var ae *apperr.Error
	var he *HTTPError
	switch {
	case errors.As(err, &ae):
		status = ae.Status()
		body = ApiError{Code: ae.Code, Message: ae.Message, Fields: ae.Fields}
	case errors.As(err, &he):
		status = he.Status
		code, ok := codeForStatus[status]
		if !ok {
			code = apperr.Validation
			if status >= 500 {
				code = apperr.Unknown
			}
		}
		msg := he.Message
		if status == 429 {
			msg = "Too many requests. Please wait a moment and try again."
		} else if status == 404 {
			msg = "Not found"
		}
		body = ApiError{Code: code, Message: msg}
	default:
		Logger.Error("unhandled error", "logger", "ApiError", "requestId", RequestID(r), "method", r.Method, "path", r.URL.Path, "error", err.Error())
		body = ApiError{Code: apperr.Unknown, Message: "Something went wrong. Please try again."}
	}
	body.RequestID = RequestID(r)
	WriteJSON(w, status, body)
}

// WriteJSON writes a JSON body with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteText writes a plain string the way Nest sends a returned string (text/html).
func WriteText(w http.ResponseWriter, status int, s string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(s))
}

// NoContent writes an empty 204.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// NotFound is the catch-all handler for unknown routes.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, &HTTPError{Status: 404, Message: "Not found"})
}

// statusRecorder remembers the status code so the access log and metrics can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush lets server-sent events through.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Observer is told about every finished request (the metrics module).
type Observer func(method, route string, status int, seconds float64)

// RequestContext assigns the correlation id (a caller-supplied one is reused only when it looks like an id),
// writes the structured access log, recovers from panics, and reports to the observer.
func RequestContext(observe Observer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("x-request-id")
			if !safeID.MatchString(id) {
				id = NewUUID()
			}
			r.Header.Set("x-request-id", id)
			w.Header().Set("x-request-id", id)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			r = r.WithContext(ctx)
			rec := &statusRecorder{ResponseWriter: w, status: 200}
			started := time.Now()
			defer func() {
				if p := recover(); p != nil {
					Logger.Error("panic", "logger", "ApiError", "requestId", id, "method", r.Method, "path", r.URL.Path, "error", fmt.Sprint(p), "stack", string(debug.Stack()))
					if !rec.wrote {
						WriteError(rec, r, errors.New("panic"))
					}
				}
				elapsed := time.Since(started)
				Logger.Info(fmt.Sprintf(`{"requestId":%q,"method":%q,"path":%q,"status":%d,"ms":%.1f}`, id, r.Method, r.URL.Path, rec.status, float64(elapsed.Microseconds())/1000), "logger", "HTTP")
				if observe != nil && r.URL.Path != "/metrics" {
					observe(r.Method, routeLabel(r), rec.status, elapsed.Seconds())
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// routeLabel is the matched path pattern (/orders/:id), never the raw URL, so metrics cardinality stays bounded.
func routeLabel(r *http.Request) string {
	p := r.Pattern
	if p == "" || p == "/" {
		return "unmatched"
	}
	if i := strings.Index(p, " "); i >= 0 {
		p = p[i+1:]
	}
	out := strings.Builder{}
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out.WriteString("/:" + strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}"))
		} else if seg != "" {
			out.WriteString("/" + seg)
		}
	}
	if out.Len() == 0 {
		return "/"
	}
	return out.String()
}

// SecurityHeaders applies the helmet defaults, with a "nothing" content-security-policy for a JSON API.
func SecurityHeaders() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", "default-src 'none';frame-ancestors 'none'")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Origin-Agent-Cluster", "?1")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Strict-Transport-Security", "max-age=15552000; includeSubDomains")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-DNS-Prefetch-Control", "off")
			h.Set("X-Download-Options", "noopen")
			h.Set("X-Frame-Options", "SAMEORIGIN")
			h.Set("X-Permitted-Cross-Domain-Policies", "none")
			h.Set("X-XSS-Protection", "0")
			next.ServeHTTP(w, r)
		})
	}
}

// CORS allows credentials only for the listed origins; anything else gets no CORS headers at all.
func CORS(origins []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			h := w.Header()
			h.Add("Vary", "Origin")
			if origin != "" && allowed[origin] {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
					h.Add("Vary", "Access-Control-Request-Headers")
					h.Set("Access-Control-Allow-Methods", "GET,HEAD,PUT,PATCH,POST,DELETE")
					h.Set("Access-Control-Allow-Headers", "content-type,authorization,x-csrf,x-request-id")
					h.Set("Access-Control-Max-Age", "600")
					h.Set("Content-Length", "0")
					w.WriteHeader(http.StatusNoContent)
					return
				}
				h.Set("Access-Control-Expose-Headers", "x-request-id")
			} else if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Content-Length", "0")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP is the caller's address with one trusted proxy hop (the last X-Forwarded-For entry), else the socket.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Cookie reads a cookie value ("" when absent).
func Cookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// SetCookie writes an HttpOnly, SameSite=Lax cookie.
func SetCookie(w http.ResponseWriter, name, value, path string, secure bool, expires time.Time, maxAge time.Duration) {
	c := &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode}
	if !expires.IsZero() {
		c.Expires = expires.UTC()
	}
	if maxAge > 0 {
		c.MaxAge = int(maxAge.Seconds())
		c.Expires = time.Now().Add(maxAge).UTC()
	}
	http.SetCookie(w, c)
}

// ClearCookie expires a cookie.
func ClearCookie(w http.ResponseWriter, name, path string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(0, 0).UTC()})
}
