// Package accounts is the signed-in customer's own profile, password, data export and account deletion.
package accounts

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/shubhamrathi020/eCommerce-go/internal/addresses"
	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/auth"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

var phoneRE = regexp.MustCompile(`^[6-9][0-9]{9}$`)

// Service is the accounts business logic.
type Service struct {
	store     *db.Store
	passwords *auth.Passwords
	tokens    *auth.Tokens
}

// NewService builds the service.
func NewService(store *db.Store, passwords *auth.Passwords, tokens *auth.Tokens) *Service {
	return &Service{store: store, passwords: passwords, tokens: tokens}
}

func (s *Service) user(ctx context.Context, id string) (*db.User, error) {
	u, err := db.UserByID(ctx, s.store.Pool, id)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, apperr.New(apperr.Unauthorized, "Please sign in.")
	}
	return u, nil
}

// UpdateProfile changes the name and phone.
func (s *Service) UpdateProfile(ctx context.Context, userID, rawName, phone string) (*domain.User, error) {
	if _, err := s.user(ctx, userID); err != nil {
		return nil, err
	}
	fields := map[string]string{}
	name := strings.TrimSpace(rawName)
	if name == "" {
		fields["name"] = "Name is required"
	} else if utf8.RuneCountInString(name) > 80 {
		fields["name"] = "Use 80 characters or fewer"
	}
	if phone != "" && !phoneRE.MatchString(phone) {
		fields["phone"] = "Enter a valid 10-digit mobile number"
	}
	if err := apperr.AssertNoFieldErrors(fields); err != nil {
		return nil, err
	}
	u, err := db.UpdateProfile(ctx, s.store.Pool, userID, name, phone)
	if err != nil {
		return nil, err
	}
	out := auth.ToUser(u)
	return &out, nil
}

// ChangePassword signs out every other device; the one making the change (identified by its refresh cookie) stays signed in.
func (s *Service) ChangePassword(ctx context.Context, userID, current, next, refreshToken string) error {
	u, err := s.user(ctx, userID)
	if err != nil {
		return err
	}
	if !s.passwords.Verify(u.PasswordHash, current) {
		return apperr.WithFields(apperr.Validation, "Your current password is incorrect.", map[string]string{"currentPassword": "Incorrect password"})
	}
	if problem := domain.PasswordProblem(next); problem != "" {
		return apperr.WithFields(apperr.Validation, problem, map[string]string{"newPassword": problem})
	}
	hash, err := s.passwords.Hash(next)
	if err != nil {
		return err
	}
	if err := db.SetPassword(ctx, s.store.Pool, userID, hash, false); err != nil {
		return err
	}
	return s.tokens.RevokeAllForUser(ctx, userID, s.tokens.FamilyOf(ctx, refreshToken))
}

// Export is the "download my data" payload.
func (s *Service) Export(ctx context.Context, userID string) (*domain.AccountExport, error) {
	u, err := s.user(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.AddressesFor(ctx, s.store.Pool, userID)
	if err != nil {
		return nil, err
	}
	user := auth.ToUser(u)
	list := make([]domain.SavedAddress, len(rows))
	for i := range rows {
		list[i] = addresses.ToSavedAddress(&rows[i])
	}
	return &domain.AccountExport{
		ExportedAt: domain.ISO(db.Now()),
		Profile:    domain.ProfileExport{ID: user.ID, Name: user.Name, Email: user.Email, Phone: user.Phone, Roles: user.Roles, EmailVerified: user.EmailVerified, CreatedAt: user.CreatedAt},
		Addresses:  list,
		OrderIDs:   []string{},
	}, nil
}

// Delete removes the account and everything that belongs only to it (addresses, sessions) in one step.
func (s *Service) Delete(ctx context.Context, userID, password string) error {
	u, err := s.user(ctx, userID)
	if err != nil {
		return err
	}
	if !s.passwords.Verify(u.PasswordHash, password) {
		return apperr.WithFields(apperr.Validation, "That password is not correct.", map[string]string{"password": "Incorrect password"})
	}
	return s.store.Tx(ctx, func(tx pgx.Tx) error {
		if err := db.DeleteLoginAttempts(ctx, tx, u.Email); err != nil {
			return err
		}
		return db.DeleteUser(ctx, tx, userID)
	})
}

// Handler serves the account routes.
type Handler struct {
	svc    *Service
	cfg    *config.Config
	guards *httpx.Guards
}

// NewHandler builds the handler.
func NewHandler(svc *Service, cfg *config.Config, guards *httpx.Guards) *Handler {
	return &Handler{svc: svc, cfg: cfg, guards: guards}
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	own := h.guards.Require("profile:write:own")
	mux.HandleFunc("PATCH /account/profile", httpx.Wrap(httpx.Chain(h.updateProfile, own)))
	mux.HandleFunc("POST /auth/change-password", httpx.Wrap(httpx.Chain(h.changePassword, own)))
	mux.HandleFunc("GET /account/export", httpx.Wrap(httpx.Chain(h.export, h.guards.Require())))
	mux.HandleFunc("POST /account/delete", httpx.Wrap(httpx.Chain(h.delete, own)))
}

func (h *Handler) updateProfile(w http.ResponseWriter, r *http.Request) error {
	user, _ := httpx.MustUser(r)
	body := httpx.ReadObject(r, "name", "phone")
	name := body.Str("name", 200)
	phone, _ := body.OptStr("phone", 20)
	if err := body.Err(); err != nil {
		return err
	}
	u, err := h.svc.UpdateProfile(r.Context(), user.ID, name, phone)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) error {
	user, _ := httpx.MustUser(r)
	body := httpx.ReadObject(r, "currentPassword", "newPassword")
	current, next := body.Str("currentPassword", 200), body.Str("newPassword", 200)
	if err := body.Err(); err != nil {
		return err
	}
	if err := h.svc.ChangePassword(r.Context(), user.ID, current, next, httpx.Cookie(r, auth.RefreshCookie)); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h *Handler) export(w http.ResponseWriter, r *http.Request) error {
	user, _ := httpx.MustUser(r)
	out, err := h.svc.Export(r.Context(), user.ID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) error {
	user, _ := httpx.MustUser(r)
	body := httpx.ReadObject(r, "password")
	password := body.Str("password", 200)
	if err := body.Err(); err != nil {
		return err
	}
	if err := h.svc.Delete(r.Context(), user.ID, password); err != nil {
		return err
	}
	httpx.ClearCookie(w, auth.RefreshCookie, "/auth", h.cfg.Production)
	httpx.NoContent(w)
	return nil
}
