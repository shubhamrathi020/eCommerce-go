package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// User is a row of the users table.
type User struct {
	ID              string
	Name            string
	Email           string
	Phone           *string
	Roles           []string
	PasswordHash    string
	EmailVerified   bool
	VerifyTokenHash *string
	ResetTokenHash  *string
	ResetExpires    *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

const userCols = `"id","name","email","phone","roles"::text[],"passwordHash","emailVerified","verifyTokenHash","resetTokenHash","resetExpires","createdAt","updatedAt"`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Name, &u.Email, &u.Phone, &u.Roles, &u.PasswordHash, &u.EmailVerified, &u.VerifyTokenHash, &u.ResetTokenHash, &u.ResetExpires, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if u.Roles == nil {
		u.Roles = []string{"customer"}
	}
	return &u, nil
}

// CreateUser inserts a customer.
func CreateUser(ctx context.Context, q DBTX, name, email, passwordHash, verifyTokenHash string) (*User, error) {
	now := Now()
	return scanUser(q.QueryRow(ctx, `INSERT INTO users ("id","name","email","passwordHash","verifyTokenHash","roles","createdAt","updatedAt") VALUES ($1,$2,$3,$4,$5,ARRAY['customer']::"Role"[],$6,$6) RETURNING `+userCols,
		NewID(), name, email, passwordHash, verifyTokenHash, now))
}

// UserByEmail finds a user by (already lower-cased) email; nil when none.
func UserByEmail(ctx context.Context, q DBTX, email string) (*User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE "email"=$1`, email))
}

// UserByID finds a user by id; nil when none.
func UserByID(ctx context.Context, q DBTX, id string) (*User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE "id"=$1`, id))
}

// UserByVerifyHash finds the user a verification token belongs to.
func UserByVerifyHash(ctx context.Context, q DBTX, hash string) (*User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE "verifyTokenHash"=$1`, hash))
}

// UserByResetHash finds the user a password-reset token belongs to.
func UserByResetHash(ctx context.Context, q DBTX, hash string) (*User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE "resetTokenHash"=$1`, hash))
}

// MarkEmailVerified verifies an address and spends its token.
func MarkEmailVerified(ctx context.Context, q DBTX, id string) error {
	_, err := q.Exec(ctx, `UPDATE users SET "emailVerified"=true,"verifyTokenHash"=NULL,"updatedAt"=$2 WHERE "id"=$1`, id, Now())
	return err
}

// SetResetToken stores the hash of a password-reset token and when it expires.
func SetResetToken(ctx context.Context, q DBTX, id, hash string, expires time.Time) error {
	_, err := q.Exec(ctx, `UPDATE users SET "resetTokenHash"=$2,"resetExpires"=$3,"updatedAt"=$4 WHERE "id"=$1`, id, hash, expires.UTC(), Now())
	return err
}

// SetPassword replaces the password hash; clearReset also spends a reset token.
func SetPassword(ctx context.Context, q DBTX, id, hash string, clearReset bool) error {
	if clearReset {
		_, err := q.Exec(ctx, `UPDATE users SET "passwordHash"=$2,"resetTokenHash"=NULL,"resetExpires"=NULL,"updatedAt"=$3 WHERE "id"=$1`, id, hash, Now())
		return err
	}
	_, err := q.Exec(ctx, `UPDATE users SET "passwordHash"=$2,"updatedAt"=$3 WHERE "id"=$1`, id, hash, Now())
	return err
}

// UpdateProfile changes the name and phone ("" clears the phone).
func UpdateProfile(ctx context.Context, q DBTX, id, name, phone string) (*User, error) {
	var p *string
	if phone != "" {
		p = &phone
	}
	return scanUser(q.QueryRow(ctx, `UPDATE users SET "name"=$2,"phone"=$3,"updatedAt"=$4 WHERE "id"=$1 RETURNING `+userCols, id, name, p, Now()))
}

// DeleteUser removes the user; addresses and sessions go with it (ON DELETE CASCADE).
func DeleteUser(ctx context.Context, q DBTX, id string) error {
	_, err := q.Exec(ctx, `DELETE FROM users WHERE "id"=$1`, id)
	return err
}

// LoginAttempt is brute-force bookkeeping for one email address.
type LoginAttempt struct {
	Email       string
	Count       int
	FirstAt     time.Time
	LockedUntil *time.Time
}

// LoginAttemptFor returns the attempt record for an email, nil when none.
func LoginAttemptFor(ctx context.Context, q DBTX, email string) (*LoginAttempt, error) {
	var a LoginAttempt
	err := q.QueryRow(ctx, `SELECT "email","count","firstAt","lockedUntil" FROM login_attempts WHERE "email"=$1`, email).Scan(&a.Email, &a.Count, &a.FirstAt, &a.LockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

// UpsertLoginAttempt records a failed attempt.
func UpsertLoginAttempt(ctx context.Context, q DBTX, a LoginAttempt) error {
	var locked *time.Time
	if a.LockedUntil != nil {
		t := a.LockedUntil.UTC()
		locked = &t
	}
	_, err := q.Exec(ctx, `INSERT INTO login_attempts ("email","count","firstAt","lockedUntil") VALUES ($1,$2,$3,$4)
		ON CONFLICT ("email") DO UPDATE SET "count"=EXCLUDED."count","firstAt"=EXCLUDED."firstAt","lockedUntil"=EXCLUDED."lockedUntil"`,
		a.Email, a.Count, a.FirstAt.UTC(), locked)
	return err
}

// DeleteLoginAttempts clears the record after a good sign-in.
func DeleteLoginAttempts(ctx context.Context, q DBTX, email string) error {
	_, err := q.Exec(ctx, `DELETE FROM login_attempts WHERE "email"=$1`, email)
	return err
}

// RefreshToken is a row of refresh_tokens (only hashes are stored).
type RefreshToken struct {
	ID        string
	UserID    string
	TokenHash string
	Family    string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// CreateRefreshToken stores a new refresh token hash.
func CreateRefreshToken(ctx context.Context, q DBTX, userID, family, hash string, expires time.Time) error {
	_, err := q.Exec(ctx, `INSERT INTO refresh_tokens ("id","userId","family","tokenHash","expiresAt","createdAt") VALUES ($1,$2,$3,$4,$5,$6)`,
		NewID(), userID, family, hash, expires.UTC(), Now())
	return err
}

// RefreshTokenByHash finds a stored refresh token; nil when unknown.
func RefreshTokenByHash(ctx context.Context, q DBTX, hash string) (*RefreshToken, error) {
	var t RefreshToken
	err := q.QueryRow(ctx, `SELECT "id","userId","tokenHash","family","expiresAt","revokedAt" FROM refresh_tokens WHERE "tokenHash"=$1`, hash).Scan(&t.ID, &t.UserID, &t.TokenHash, &t.Family, &t.ExpiresAt, &t.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

// ClaimRefreshToken atomically turns a live token into a rotated one; only one request can win.
func ClaimRefreshToken(ctx context.Context, q DBTX, id string) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE refresh_tokens SET "revokedAt"=$2 WHERE "id"=$1 AND "revokedAt" IS NULL`, id, Now())
	return tag.RowsAffected() == 1, err
}

// SetReplacedBy records which token replaced this one.
func SetReplacedBy(ctx context.Context, q DBTX, id, hash string) error {
	_, err := q.Exec(ctx, `UPDATE refresh_tokens SET "replacedBy"=$2 WHERE "id"=$1`, id, hash)
	return err
}

// RevokeFamily revokes every live token of one sign-in.
func RevokeFamily(ctx context.Context, q DBTX, family string) error {
	_, err := q.Exec(ctx, `UPDATE refresh_tokens SET "revokedAt"=$2 WHERE "family"=$1 AND "revokedAt" IS NULL`, family, Now())
	return err
}

// RevokeAllForUser signs the user out everywhere, optionally keeping one family.
func RevokeAllForUser(ctx context.Context, q DBTX, userID, exceptFamily string) error {
	if exceptFamily != "" {
		_, err := q.Exec(ctx, `UPDATE refresh_tokens SET "revokedAt"=$3 WHERE "userId"=$1 AND "revokedAt" IS NULL AND "family"<>$2`, userID, exceptFamily, Now())
		return err
	}
	_, err := q.Exec(ctx, `UPDATE refresh_tokens SET "revokedAt"=$2 WHERE "userId"=$1 AND "revokedAt" IS NULL`, userID, Now())
	return err
}
