// Package db is the Go counterpart of the Prisma client: typed queries against the same PostgreSQL tables.
// The Nest API's Prisma migrations remain the single owner of the schema; this package only reads and writes it.
package db

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX is what both a pool and a transaction can do, so a query works inside or outside a transaction.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store owns the connection pool.
type Store struct{ Pool *pgxpool.Pool }

// Open connects using a Prisma-style DATABASE_URL (its `?schema=public` parameter is translated, since
// PostgreSQL itself does not know it).
func Open(ctx context.Context, rawURL string) (*Store, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	schema := q.Get("schema")
	q.Del("schema")
	if schema != "" && schema != "public" {
		q.Set("search_path", schema)
	}
	u.RawQuery = q.Encode()
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.Pool.Close() }

// Ping checks the database answers.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.Pool.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// Tx runs fn in a transaction, committing when it returns nil and rolling back otherwise.
func (s *Store) Tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// ErrNotFound is returned by lookups that find nothing.
var ErrNotFound = errors.New("not found")

// IsUniqueViolation reports whether err is a PostgreSQL unique-constraint violation.
func IsUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

var cuidCounter atomic.Uint32

// NewID returns a Prisma-style cuid: 'c' + time + counter + fingerprint + random, 25 characters.
func NewID() string {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 36)
	counter := leftPad(strconv.FormatUint(uint64(cuidCounter.Add(1)%1679616), 36), 4)
	fp := leftPad(randBase36(2), 4)
	return "c" + ts + counter + fp + leftPad(randBase36(8), 8)
}

func randBase36(n int) string {
	max := new(big.Int).Exp(big.NewInt(36), big.NewInt(int64(n)), nil)
	v, _ := rand.Int(rand.Reader, max)
	return strings.ToLower(v.Text(36))
}

func leftPad(s string, n int) string {
	if len(s) >= n {
		return s[len(s)-n:]
	}
	return strings.Repeat("0", n-len(s)) + s
}

// Now is the current time in UTC (the database stores naive UTC timestamps).
func Now() time.Time { return time.Now().UTC() }
