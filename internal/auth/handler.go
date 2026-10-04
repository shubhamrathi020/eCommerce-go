package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// RefreshCookie is the HttpOnly cookie holding the refresh token. It is scoped to the auth routes, so the
// token is never sent with ordinary API calls.
const (
	RefreshCookie = "rt"
	refreshPath   = "/auth"
	GuestCookie   = "gcid"
)

// CartMerger moves a guest cart into a user's cart at sign-in (implemented by the commerce module).
type CartMerger interface {
	MergeGuestIntoUser(ctx context.Context, guestOwnerKey, userID string) error
}

// SignInResponse is the body of every sign-in style endpoint. The refresh token only ever travels as a cookie.
type SignInResponse struct {
	Session     domain.Session `json:"session"`
	AccessToken string         `json:"accessToken"`
}

// Handler serves the /auth routes.
type Handler struct {
	svc      *Service
	cfg      *config.Config
	guards   *httpx.Guards
	throttle *httpx.Throttle
	carts    CartMerger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, cfg *config.Config, guards *httpx.Guards, throttle *httpx.Throttle, carts CartMerger) *Handler {
	return &Handler{svc: svc, cfg: cfg, guards: guards, throttle: throttle, carts: carts}
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	strict := func(name string) func(httpx.HandlerFunc) httpx.HandlerFunc {
		return h.throttle.Limit("auth."+name, 10, time.Minute)
	}
	mux.HandleFunc("POST /auth/register", httpx.Wrap(httpx.Chain(h.register, strict("register"))))
	mux.HandleFunc("POST /auth/login", httpx.Wrap(httpx.Chain(h.login, strict("login"))))
	mux.HandleFunc("POST /auth/refresh", httpx.Wrap(httpx.Chain(h.refresh, httpx.CSRF)))
	mux.HandleFunc("POST /auth/logout", httpx.Wrap(httpx.Chain(h.logout, httpx.CSRF)))
	mux.HandleFunc("GET /auth/me", httpx.Wrap(httpx.Chain(h.me, h.guards.Require())))
	mux.HandleFunc("POST /auth/verify-email", httpx.Wrap(httpx.Chain(h.verifyEmail, strict("verify-email"))))
	mux.HandleFunc("POST /auth/password-reset/request", httpx.Wrap(httpx.Chain(h.requestReset, strict("reset-request"))))
	mux.HandleFunc("POST /auth/password-reset", httpx.Wrap(httpx.Chain(h.resetPassword, strict("reset"))))
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) error {
	body := httpx.ReadObject(r, "name", "email", "password")
	name, email, password := body.Str("name", 200), body.Str("email", 320), body.Str("password", 200)
	if err := body.Err(); err != nil {
		return err
	}
	signed, err := h.svc.Register(r.Context(), name, email, password)
	if err != nil {
		return err
	}
	h.mergeGuestCart(r, signed)
	return h.respond(w, r, http.StatusCreated, signed)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) error {
	body := httpx.ReadObject(r, "email", "password")
	email, password := body.Str("email", 320), body.Str("password", 200)
	if err := body.Err(); err != nil {
		return err
	}
	signed, err := h.svc.Login(r.Context(), email, password)
	if err != nil {
		return err
	}
	h.mergeGuestCart(r, signed)
	return h.respond(w, r, http.StatusOK, signed)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) error {
	signed, err := h.svc.Refresh(r.Context(), httpx.Cookie(r, RefreshCookie))
	if err != nil {
		httpx.ClearCookie(w, RefreshCookie, refreshPath, h.cfg.Production)
		return err
	}
	return h.respond(w, r, http.StatusOK, signed)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) error {
	if err := h.svc.Logout(r.Context(), httpx.Cookie(r, RefreshCookie)); err != nil {
		return err
	}
	httpx.ClearCookie(w, RefreshCookie, refreshPath, h.cfg.Production)
	httpx.NoContent(w)
	return nil
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) error {
	user, err := httpx.MustUser(r)
	if err != nil {
		return err
	}
	sess, err := h.svc.Session(r.Context(), user.ID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, sess)
	return nil
}

func (h *Handler) verifyEmail(w http.ResponseWriter, r *http.Request) error {
	body := httpx.ReadObject(r, "token")
	token := body.Str("token", 200)
	if err := body.Err(); err != nil {
		return err
	}
	if err := h.svc.VerifyEmail(r.Context(), token); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h *Handler) requestReset(w http.ResponseWriter, r *http.Request) error {
	body := httpx.ReadObject(r, "email")
	email := body.Str("email", 320)
	if err := body.Err(); err != nil {
		return err
	}
	if err := h.svc.RequestPasswordReset(r.Context(), email); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) error {
	body := httpx.ReadObject(r, "token", "password")
	token, password := body.Str("token", 200), body.Str("password", 200)
	if err := body.Err(); err != nil {
		return err
	}
	if err := h.svc.ResetPassword(r.Context(), token, password); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h *Handler) respond(w http.ResponseWriter, r *http.Request, status int, signed *SignedIn) error {
	httpx.SetCookie(w, RefreshCookie, signed.Refresh.Token, refreshPath, h.cfg.Production, signed.Refresh.ExpiresAt, 0)
	httpx.WriteJSON(w, status, SignInResponse{Session: signed.Session, AccessToken: signed.AccessToken})
	return nil
}

// mergeGuestCart moves whatever was in the guest cart (identified by the gcid cookie, if any) into the
// now-signed-in user's own cart.
func (h *Handler) mergeGuestCart(r *http.Request, signed *SignedIn) {
	if h.carts == nil {
		return
	}
	if token := httpx.Cookie(r, GuestCookie); token != "" {
		_ = h.carts.MergeGuestIntoUser(r.Context(), "guest:"+token, signed.Session.User.ID)
	}
}
