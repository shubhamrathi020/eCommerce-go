package domain

import (
	"regexp"
	"unicode"
)

// Role is a user role stored in the database (customer or admin; "seller" exists only in the frontend mock).
type Role string

var (
	customerPermissions = []string{"profile:write:own", "address:write:own", "order:read:own", "order:cancel:own", "return:write:own", "support:write:own"}
	sellerPermissions   = []string{"seller:portal"}
	adminPermissions    = []string{"seller:manage", "product:read", "product:write", "order:read:any", "order:refund", "user:read", "coupon:write", "review:moderate", "content:write", "inventory:write", "notification:manage", "return:manage", "support:manage", "promotion:manage", "recommendation:manage", "analytics:read", "system:read", "system:write"}
)

// PermissionsFor derives the permissions a set of roles grants. The single source of truth, shared with the
// frontend mock; the order matches the TypeScript original (customer first, then seller, then admin).
func PermissionsFor(roles []Role) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(list []string) {
		for _, p := range list {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	add(customerPermissions)
	for _, r := range roles {
		if r == "seller" {
			add(sellerPermissions)
		}
	}
	for _, r := range roles {
		if r == "admin" {
			add(adminPermissions)
		}
	}
	return out
}

var (
	lowerRE = regexp.MustCompile(`[a-z]`)
	upperRE = regexp.MustCompile(`[A-Z]`)
)

// PasswordProblem returns why a password is too weak, or "" when it is fine: at least 8 characters with
// upper case, lower case and a digit.
func PasswordProblem(password string) string {
	if len([]rune(password)) < 8 {
		return "Use at least 8 characters."
	}
	hasDigit := false
	for _, r := range password {
		if unicode.IsDigit(r) && r < 128 {
			hasDigit = true
			break
		}
	}
	if !lowerRE.MatchString(password) || !upperRE.MatchString(password) || !hasDigit {
		return "Use upper case, lower case and a number."
	}
	return ""
}

// User is the public user shape. Password and token hashes never appear here.
type User struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Email         string   `json:"email"`
	Phone         string   `json:"phone,omitempty"`
	Roles         []Role   `json:"roles"`
	Permissions   []string `json:"permissions"`
	EmailVerified bool     `json:"emailVerified"`
	CreatedAt     string   `json:"createdAt"`
}

// Session is what the browser knows about the signed-in user.
type Session struct {
	User      User   `json:"user"`
	ExpiresAt string `json:"expiresAt"`
}

// SavedAddress is an address-book entry.
type SavedAddress struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	Name      string  `json:"name"`
	Phone     string  `json:"phone"`
	Address   Address `json:"address"`
	IsDefault bool    `json:"isDefault"`
}

// ProfileExport is the account export's profile (a User without permissions).
type ProfileExport struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	Phone         string `json:"phone,omitempty"`
	Roles         []Role `json:"roles"`
	EmailVerified bool   `json:"emailVerified"`
	CreatedAt     string `json:"createdAt"`
}

// AccountExport is the "download my data" payload.
type AccountExport struct {
	ExportedAt string         `json:"exportedAt"`
	Profile    ProfileExport  `json:"profile"`
	Addresses  []SavedAddress `json:"addresses"`
	OrderIDs   []string       `json:"orderIds"`
}
