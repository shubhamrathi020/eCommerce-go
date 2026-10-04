package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// SHA256 is the hex SHA-256 of a value: right for high-entropy random tokens (unlike passwords, they cannot be guessed).
func SHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// RandomToken is 32 random bytes, base64url-encoded without padding.
func RandomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// IssuedRefresh is a newly created refresh token and when it expires.
type IssuedRefresh struct {
	Token     string
	ExpiresAt time.Time
}

// Tokens issues short-lived access tokens and rotating refresh tokens. Refresh tokens are random, stored only as
// hashes, rotated on every use and grouped in a "family" per sign-in. Presenting one that was already rotated
// means it was copied: the whole family is revoked (reuse detection).
type Tokens struct {
	cfg   *config.Config
	store *db.Store
}

// NewTokens builds the token service.
func NewTokens(cfg *config.Config, store *db.Store) *Tokens { return &Tokens{cfg: cfg, store: store} }

// AccessToken signs a JWT carrying the user's id, roles and permissions.
func (t *Tokens) AccessToken(userID string, roles []domain.Role) (string, error) {
	now := time.Now()
	claims := httpx.Claims{
		Roles:       roles,
		Permissions: domain.PermissionsFor(roles),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Audience:  jwt.ClaimStrings{"ecom-api"},
			Issuer:    "ecom-api",
			ID:        httpx.NewUUID(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Duration(t.cfg.AccessTokenMinutes) * time.Minute)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(t.cfg.JWTAccessSecret))
}

// IssueRefresh starts a new family (a fresh sign-in).
func (t *Tokens) IssueRefresh(ctx context.Context, userID string) (IssuedRefresh, error) {
	return t.create(ctx, userID, httpx.NewUUID())
}

// Rotate exchanges a refresh token for a new one. It fails with unauthorized when the token is missing, unknown,
// expired, revoked or reused.
func (t *Tokens) Rotate(ctx context.Context, token string) (string, IssuedRefresh, error) {
	if token == "" {
		return "", IssuedRefresh{}, apperr.New(apperr.Unauthorized, "Please sign in.")
	}
	row, err := db.RefreshTokenByHash(ctx, t.store.Pool, SHA256(token))
	if err != nil {
		return "", IssuedRefresh{}, err
	}
	if row == nil {
		return "", IssuedRefresh{}, apperr.New(apperr.Unauthorized, "Please sign in.")
	}
	// Atomically claim the token: only one request can turn it from live into rotated.
	claimed, err := db.ClaimRefreshToken(ctx, t.store.Pool, row.ID)
	if err != nil {
		return "", IssuedRefresh{}, err
	}
	if !claimed {
		if err := db.RevokeFamily(ctx, t.store.Pool, row.Family); err != nil {
			return "", IssuedRefresh{}, err
		}
		return "", IssuedRefresh{}, apperr.New(apperr.Unauthorized, "For your safety you have been signed out. Please sign in again.")
	}
	if !row.ExpiresAt.After(time.Now().UTC()) {
		return "", IssuedRefresh{}, apperr.New(apperr.Unauthorized, "Your session has expired. Please sign in again.")
	}
	refresh, err := t.create(ctx, row.UserID, row.Family)
	if err != nil {
		return "", IssuedRefresh{}, err
	}
	if err := db.SetReplacedBy(ctx, t.store.Pool, row.ID, SHA256(refresh.Token)); err != nil {
		return "", IssuedRefresh{}, err
	}
	return row.UserID, refresh, nil
}

// FamilyOf is the family of a presented refresh token, "" when it is unknown.
func (t *Tokens) FamilyOf(ctx context.Context, token string) string {
	if token == "" {
		return ""
	}
	row, err := db.RefreshTokenByHash(ctx, t.store.Pool, SHA256(token))
	if err != nil || row == nil {
		return ""
	}
	return row.Family
}

// RevokeFamily revokes every live token of one sign-in.
func (t *Tokens) RevokeFamily(ctx context.Context, family string) error {
	return db.RevokeFamily(ctx, t.store.Pool, family)
}

// RevokeAllForUser signs the user out everywhere, optionally keeping one family (the device making the change).
func (t *Tokens) RevokeAllForUser(ctx context.Context, userID, exceptFamily string) error {
	return db.RevokeAllForUser(ctx, t.store.Pool, userID, exceptFamily)
}

func (t *Tokens) create(ctx context.Context, userID, family string) (IssuedRefresh, error) {
	token := RandomToken()
	expires := time.Now().UTC().Add(time.Duration(t.cfg.RefreshTokenDays) * 24 * time.Hour)
	if err := db.CreateRefreshToken(ctx, t.store.Pool, userID, family, SHA256(token), expires); err != nil {
		return IssuedRefresh{}, err
	}
	return IssuedRefresh{Token: token, ExpiresAt: expires}, nil
}
