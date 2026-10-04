package auth

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
)

// Email is the address shape check shared with the Nest API.
var Email = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// The brute-force policy, same as the mock adapter and the Nest API.
const (
	MaxFailedAttempts = 5
	LockoutMinutes    = 15
	resetMinutes      = 30
)

// SignedIn is what a successful sign-in hands to the handler: the public session plus the two tokens.
type SignedIn struct {
	Session     domain.Session
	AccessToken string
	Refresh     IssuedRefresh
}

// Service is the identity business logic.
type Service struct {
	store     *db.Store
	passwords *Passwords
	tokens    *Tokens
	mail      *Mail
}

// NewService builds the service.
func NewService(store *db.Store, passwords *Passwords, tokens *Tokens, mail *Mail) *Service {
	return &Service{store: store, passwords: passwords, tokens: tokens, mail: mail}
}

// ToUser maps a database row to the public user contract. Hashes never leave this function.
func ToUser(u *db.User) domain.User {
	roles := make([]domain.Role, len(u.Roles))
	for i, r := range u.Roles {
		roles[i] = domain.Role(r)
	}
	out := domain.User{
		ID: u.ID, Name: u.Name, Email: u.Email, Roles: roles, Permissions: domain.PermissionsFor(roles),
		EmailVerified: u.EmailVerified, CreatedAt: domain.ISO(u.CreatedAt),
	}
	if u.Phone != nil && *u.Phone != "" {
		out.Phone = *u.Phone
	}
	return out
}

// ToSession wraps a user with when the session ends.
func ToSession(u *db.User, expiresAt time.Time) domain.Session {
	return domain.Session{User: ToUser(u), ExpiresAt: domain.ISO(expiresAt)}
}

func rolesOf(u *db.User) []domain.Role { return ToUser(u).Roles }

// Register creates an account and signs it in.
func (s *Service) Register(ctx context.Context, name, email, password string) (*SignedIn, error) {
	name = strings.TrimSpace(name)
	email = strings.ToLower(strings.TrimSpace(email))
	fields := map[string]string{}
	if name == "" {
		fields["name"] = "Name is required"
	} else if utf8.RuneCountInString(name) > 80 {
		fields["name"] = "Use 80 characters or fewer"
	}
	if !Email.MatchString(email) || len(email) > 254 {
		fields["email"] = "Enter a valid email address"
	}
	if problem := domain.PasswordProblem(password); problem != "" {
		fields["password"] = problem
	}
	if err := apperr.AssertNoFieldErrors(fields); err != nil {
		return nil, err
	}
	neutral := apperr.New(apperr.Validation, "We could not create the account with these details. If you already have an account, sign in or reset your password.")
	if existing, err := db.UserByEmail(ctx, s.store.Pool, email); err != nil {
		return nil, err
	} else if existing != nil {
		// Neutral wording, identical to the mock: never confirm that an address is registered.
		return nil, neutral
	}
	hash, err := s.passwords.Hash(password)
	if err != nil {
		return nil, err
	}
	verify := RandomToken()
	user, err := db.CreateUser(ctx, s.store.Pool, name, email, hash, SHA256(verify))
	if err != nil {
		if db.IsUniqueViolation(err) {
			return nil, neutral
		}
		return nil, err
	}
	s.mail.Send(OutboxMail{To: user.Email, Subject: "Verify your email address", Body: fmt.Sprintf("Hi %s, please confirm your email address to finish setting up your account.", user.Name), Link: "/account/verify-email?token=" + verify})
	return s.signIn(ctx, user)
}

// Login checks credentials with brute-force lockout.
func (s *Service) Login(ctx context.Context, rawEmail, password string) (*SignedIn, error) {
	email := strings.ToLower(strings.TrimSpace(rawEmail))
	now := time.Now().UTC()
	attempts, err := db.LoginAttemptFor(ctx, s.store.Pool, email)
	if err != nil {
		return nil, err
	}
	if attempts != nil && attempts.LockedUntil != nil && attempts.LockedUntil.After(now) {
		minutes := int(attempts.LockedUntil.Sub(now).Minutes())
		if attempts.LockedUntil.Sub(now) > time.Duration(minutes)*time.Minute {
			minutes++
		}
		if minutes < 1 {
			minutes = 1
		}
		plural := "s"
		if minutes == 1 {
			plural = ""
		}
		return nil, apperr.New(apperr.Forbidden, fmt.Sprintf("Too many failed attempts. Try again in %d minute%s.", minutes, plural))
	}
	user, err := db.UserByEmail(ctx, s.store.Pool, email)
	if err != nil {
		return nil, err
	}
	hash := ""
	if user != nil {
		hash = user.PasswordHash
	}
	// Verify even when the account does not exist, so response time does not reveal it.
	ok := s.passwords.Verify(hash, password)
	if user == nil || !ok {
		fresh := attempts == nil || now.Sub(attempts.FirstAt) > LockoutMinutes*time.Minute
		count := 1
		firstAt := now
		if !fresh {
			count = attempts.Count + 1
			firstAt = attempts.FirstAt
		}
		var locked *time.Time
		if count >= MaxFailedAttempts {
			t := now.Add(LockoutMinutes * time.Minute)
			locked = &t
		}
		if err := db.UpsertLoginAttempt(ctx, s.store.Pool, db.LoginAttempt{Email: email, Count: count, FirstAt: firstAt, LockedUntil: locked}); err != nil {
			return nil, err
		}
		return nil, apperr.New(apperr.Unauthorized, "Incorrect email or password.")
	}
	if err := db.DeleteLoginAttempts(ctx, s.store.Pool, email); err != nil {
		return nil, err
	}
	return s.signIn(ctx, user)
}

// Refresh exchanges the refresh cookie for a new access token, a rotated refresh token and the current session.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*SignedIn, error) {
	userID, refresh, err := s.tokens.Rotate(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	user, err := db.UserByID(ctx, s.store.Pool, userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, apperr.New(apperr.Unauthorized, "Please sign in.")
	}
	access, err := s.tokens.AccessToken(user.ID, rolesOf(user))
	if err != nil {
		return nil, err
	}
	return &SignedIn{Session: ToSession(user, refresh.ExpiresAt), AccessToken: access, Refresh: refresh}, nil
}

// Logout revokes the sign-in the cookie belongs to.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if family := s.tokens.FamilyOf(ctx, refreshToken); family != "" {
		return s.tokens.RevokeFamily(ctx, family)
	}
	return nil
}

// Session is the current session for a user id.
func (s *Service) Session(ctx context.Context, userID string) (*domain.Session, error) {
	user, err := db.UserByID(ctx, s.store.Pool, userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, apperr.New(apperr.Unauthorized, "Please sign in.")
	}
	sess := ToSession(user, time.Now().Add(15*time.Minute))
	return &sess, nil
}

// VerifyEmail spends an emailed token.
func (s *Service) VerifyEmail(ctx context.Context, token string) error {
	var user *db.User
	var err error
	if token != "" {
		user, err = db.UserByVerifyHash(ctx, s.store.Pool, SHA256(token))
		if err != nil {
			return err
		}
	}
	if user == nil {
		return apperr.New(apperr.Validation, "This verification link is invalid or has already been used.")
	}
	return db.MarkEmailVerified(ctx, s.store.Pool, user.ID)
}

// RequestPasswordReset always succeeds, so the response never reveals whether the address has an account.
func (s *Service) RequestPasswordReset(ctx context.Context, rawEmail string) error {
	user, err := db.UserByEmail(ctx, s.store.Pool, strings.ToLower(strings.TrimSpace(rawEmail)))
	if err != nil || user == nil {
		return err
	}
	token := RandomToken()
	if err := db.SetResetToken(ctx, s.store.Pool, user.ID, SHA256(token), time.Now().Add(resetMinutes*time.Minute)); err != nil {
		return err
	}
	s.mail.Send(OutboxMail{To: user.Email, Subject: "Reset your password", Body: fmt.Sprintf("Use this link within %d minutes to choose a new password. If you did not ask for it, ignore this email.", resetMinutes), Link: "/account/reset-password?token=" + token})
	return nil
}

// ResetPassword sets a new password from an emailed token and signs out every device.
func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	if problem := domain.PasswordProblem(newPassword); problem != "" {
		return apperr.WithFields(apperr.Validation, problem, map[string]string{"password": problem})
	}
	var user *db.User
	var err error
	if token != "" {
		user, err = db.UserByResetHash(ctx, s.store.Pool, SHA256(token))
		if err != nil {
			return err
		}
	}
	if user == nil || user.ResetExpires == nil || user.ResetExpires.Before(time.Now().UTC()) {
		return apperr.New(apperr.Validation, "This reset link is invalid or has expired.")
	}
	hash, err := s.passwords.Hash(newPassword)
	if err != nil {
		return err
	}
	if err := db.SetPassword(ctx, s.store.Pool, user.ID, hash, true); err != nil {
		return err
	}
	if err := db.DeleteLoginAttempts(ctx, s.store.Pool, user.Email); err != nil {
		return err
	}
	// A password reset signs out every device (the old password may have been stolen).
	return s.tokens.RevokeAllForUser(ctx, user.ID, "")
}

func (s *Service) signIn(ctx context.Context, user *db.User) (*SignedIn, error) {
	refresh, err := s.tokens.IssueRefresh(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	access, err := s.tokens.AccessToken(user.ID, rolesOf(user))
	if err != nil {
		return nil, err
	}
	return &SignedIn{Session: ToSession(user, refresh.ExpiresAt), AccessToken: access, Refresh: refresh}, nil
}
