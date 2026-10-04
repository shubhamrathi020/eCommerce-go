package httpx

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
)

// AuthUser is what an authenticated request knows about its caller (from the short-lived access token).
type AuthUser struct {
	ID          string
	Roles       []domain.Role
	Permissions []string
}

// Has reports whether the user holds a permission.
func (u *AuthUser) Has(p string) bool {
	for _, x := range u.Permissions {
		if x == p {
			return true
		}
	}
	return false
}

// Claims is the access-token payload; identical to the Nest API's so tokens interoperate.
type Claims struct {
	Roles       []domain.Role `json:"roles"`
	Permissions []string      `json:"permissions"`
	jwt.RegisteredClaims
}

const tokenAudience = "ecom-api"

// Guards holds what the request guards need.
type Guards struct {
	cfg *config.Config
}

// NewGuards builds the guards.
func NewGuards(cfg *config.Config) *Guards { return &Guards{cfg: cfg} }

// ParseToken verifies a bearer access token (HS256, audience and issuer ecom-api) and returns its user.
func (g *Guards) ParseToken(token string) (*AuthUser, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return []byte(g.cfg.JWTAccessSecret), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(tokenAudience), jwt.WithIssuer(tokenAudience), jwt.WithExpirationRequired())
	if err != nil {
		return nil, err
	}
	return &AuthUser{ID: claims.Subject, Roles: claims.Roles, Permissions: claims.Permissions}, nil
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return h[7:]
	}
	return ""
}

// Require is the Nest AuthGuard plus @RequirePermissions: a valid bearer token with every listed permission.
func (g *Guards) Require(permissions ...string) func(HandlerFunc) HandlerFunc {
	return func(next HandlerFunc) HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) error {
			token := bearer(r)
			if token == "" {
				return apperr.New(apperr.Unauthorized, "Please sign in.")
			}
			user, err := g.ParseToken(token)
			if err != nil {
				return apperr.New(apperr.Unauthorized, "Your session has expired. Please sign in again.")
			}
			for _, p := range permissions {
				if !user.Has(p) {
					return apperr.New(apperr.Forbidden, "You do not have permission to do that.")
				}
			}
			return next(w, WithUser(r, user))
		}
	}
}

// Optional is the OptionalAuthGuard: decodes a bearer token when one is present and valid and never refuses
// the request either way (an invalid token is treated as no token, so the caller is a guest).
func (g *Guards) Optional(next HandlerFunc) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if token := bearer(r); token != "" {
			if user, err := g.ParseToken(token); err == nil {
				r = WithUser(r, user)
			}
		}
		return next(w, r)
	}
}

// CSRF is the CsrfGuard: endpoints that act on a cookie additionally require a custom header, which a
// cross-site page cannot send without a CORS preflight that only our own origins pass.
func CSRF(next HandlerFunc) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if r.Header.Get("x-csrf") != "1" {
			return apperr.New(apperr.Forbidden, "Missing CSRF header.")
		}
		return next(w, r)
	}
}

// Chain applies middlewares so the first listed runs outermost.
func Chain(h HandlerFunc, mws ...func(HandlerFunc) HandlerFunc) HandlerFunc {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// WithUser stores the authenticated user on the request.
func WithUser(r *http.Request, u *AuthUser) *http.Request {
	return r.WithContext(contextWith(r, userKey, u))
}

// User returns the authenticated user, or nil for a guest.
func User(r *http.Request) *AuthUser {
	u, _ := r.Context().Value(userKey).(*AuthUser)
	return u
}

// MustUser returns the authenticated user or the standard "please sign in" error.
func MustUser(r *http.Request) (*AuthUser, error) {
	if u := User(r); u != nil {
		return u, nil
	}
	return nil, apperr.New(apperr.Unauthorized, "Please sign in.")
}

// Throttle is an in-memory fixed-window limiter per (route, client IP), the equivalent of @nestjs/throttler.
type Throttle struct {
	mu      sync.Mutex
	hits    map[string]*window
	enabled bool
}

type window struct {
	count int
	reset time.Time
}

// NewThrottle builds a limiter; when disabled it never refuses.
func NewThrottle(enabled bool) *Throttle {
	return &Throttle{hits: map[string]*window{}, enabled: enabled}
}

// Limit refuses a request over limit per window with 429.
func (t *Throttle) Limit(name string, limit int, window time.Duration) func(HandlerFunc) HandlerFunc {
	return func(next HandlerFunc) HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) error {
			if t.enabled && !t.allow(name+"|"+ClientIP(r), limit, window, w) {
				return &HTTPError{Status: 429, Message: "Too many requests. Please wait a moment and try again."}
			}
			return next(w, r)
		}
	}
}

// Global applies the blanket per-IP ceiling to every route.
func (t *Throttle) Global(limit int, win time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if t.enabled && p != "/healthz" && p != "/readyz" && p != "/metrics" && !t.allow("global|"+ClientIP(r), limit, win, w) {
				WriteError(w, r, &HTTPError{Status: 429, Message: "Too many requests. Please wait a moment and try again."})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (t *Throttle) allow(key string, limit int, win time.Duration, w http.ResponseWriter) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	wd, ok := t.hits[key]
	if !ok || now.After(wd.reset) {
		wd = &window{reset: now.Add(win)}
		t.hits[key] = wd
	}
	wd.count++
	if wd.count > limit {
		w.Header().Set("Retry-After", itoa(int(time.Until(wd.reset).Seconds())+1))
		return false
	}
	return true
}
