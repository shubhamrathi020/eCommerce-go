// Package auth is identity: registration, sign-in, rotating refresh tokens, email verification and password reset.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// OWASP-recommended Argon2id settings (19 MiB memory, 2 iterations, 1 lane): the same ones the Nest API uses,
// and the hash is the standard PHC string, so users created by either server can sign in on the other.
const (
	argonMemory  = 19_456
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Passwords hashes and verifies passwords.
type Passwords struct {
	once  sync.Once
	dummy string
}

// Hash returns a PHC-format Argon2id hash.
func (p *Passwords) Hash(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// Verify checks a password against a stored hash. With no hash (unknown account) it checks against a
// precomputed dummy, so the response time does not reveal whether the account exists.
func (p *Passwords) Verify(hash string, password string) bool {
	if hash == "" {
		p.once.Do(func() { p.dummy, _ = p.Hash("dummy-password-for-timing") })
		hash = p.dummy
	}
	parts := strings.Split(hash, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m, t uint32
	var par uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &par); err != nil {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, par, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
