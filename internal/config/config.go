// Package config reads and validates the environment once. It uses the same variable names and defaults as the
// Nest API, so one set of environment files, Kubernetes secrets and compose settings serves both servers.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Limit is a fixed-window rate limit.
type Limit struct {
	Limit         int
	WindowSeconds int
}

// RateLimits are the named per-route buckets plus the blanket per-IP ceiling.
type RateLimits struct {
	Global   Limit
	Search   Limit
	Coupon   Limit
	Checkout Limit
}

// Config is the typed, validated configuration. The server refuses to start without it.
type Config struct {
	Port       string
	Production bool
	Test       bool

	DatabaseURL string

	MongoURL    string
	MongoDBName string

	MeiliURL         string
	MeiliMasterKey   string
	MeiliIndexSuffix string

	RedisURL       string
	RedisKeyPrefix string

	RabbitMQURL string
	MQPrefix    string

	AbandonedCartMinutes int
	RateLimits           RateLimits

	JWTAccessSecret  string
	JWTRefreshSecret string

	RazorpayKeyID         string
	RazorpayKeySecret     string
	RazorpayWebhookSecret string
	RazorpayEnabled       bool

	CORSOrigins []string

	AccessTokenMinutes int
	RefreshTokenDays   int

	RateLimit bool
}

// Load reads the process environment.
func Load() (*Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom reads configuration through a lookup function (tests pass a map).
func LoadFrom(get func(string) string) (*Config, error) {
	var problems []string
	trim := func(key string) string { return strings.TrimSpace(get(key)) }
	need := func(key string) string {
		v := trim(key)
		if v == "" {
			problems = append(problems, key+" is required")
		}
		return v
	}
	integer := func(key string, fallback int) int {
		raw := trim(key)
		if raw == "" {
			return fallback
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil || n <= 0 {
			problems = append(problems, key+" must be a positive number")
			return fallback
		}
		return int(n)
	}

	env := trim("NODE_ENV")
	if env == "" {
		env = trim("APP_ENV")
	}
	production := env == "production"
	test := env == "test"

	c := &Config{
		DatabaseURL:    need("DATABASE_URL"),
		MongoURL:       need("MONGODB_URL"),
		MeiliURL:       need("MEILI_URL"),
		MeiliMasterKey: trim("MEILI_MASTER_KEY"),
		RedisURL:       need("REDIS_URL"),
		RabbitMQURL:    need("RABBITMQ_URL"),
		Production:     production,
		Test:           test,
	}
	c.JWTAccessSecret = need("JWT_ACCESS_SECRET")
	c.JWTRefreshSecret = need("JWT_REFRESH_SECRET")
	for _, kv := range [][2]string{{"JWT_ACCESS_SECRET", c.JWTAccessSecret}, {"JWT_REFRESH_SECRET", c.JWTRefreshSecret}} {
		if kv[1] != "" && len(kv[1]) < 32 {
			problems = append(problems, kv[0]+" must be at least 32 characters")
		}
	}
	if c.JWTAccessSecret != "" && c.JWTAccessSecret == c.JWTRefreshSecret {
		problems = append(problems, "JWT_ACCESS_SECRET and JWT_REFRESH_SECRET must differ")
	}

	c.RazorpayKeyID = trim("RAZORPAY_KEY_ID")
	c.RazorpayKeySecret = trim("RAZORPAY_KEY_SECRET")
	c.RazorpayWebhookSecret = trim("RAZORPAY_WEBHOOK_SECRET")
	if c.RazorpayKeyID != "" && !strings.HasPrefix(c.RazorpayKeyID, "rzp_test_") {
		// This project only ever runs in test mode; a live key would mean real money. Never proceed.
		problems = append(problems, "RAZORPAY_KEY_ID must be a test-mode key (starts with 'rzp_test_'); live keys are refused")
	}
	if c.RazorpayKeyID != "" && c.RazorpayKeySecret == "" {
		problems = append(problems, "RAZORPAY_KEY_SECRET is required when RAZORPAY_KEY_ID is set")
	}
	c.RazorpayEnabled = c.RazorpayKeyID != ""

	c.RateLimits = RateLimits{
		Global:   Limit{integer("RATE_LIMIT_GLOBAL_PER_MIN", 300), 60},
		Search:   Limit{integer("RATE_LIMIT_SEARCH_PER_MIN", 60), 60},
		Coupon:   Limit{integer("RATE_LIMIT_COUPON_PER_MIN", 20), 60},
		Checkout: Limit{integer("RATE_LIMIT_CHECKOUT_PER_MIN", 20), 60},
	}
	c.AbandonedCartMinutes = integer("ABANDONED_CART_MINUTES", 60)

	if len(problems) > 0 {
		return nil, fmt.Errorf("Invalid API configuration:\n - %s", strings.Join(problems, "\n - "))
	}

	c.Port = trim("PORT")
	if c.Port == "" {
		c.Port = "3333"
	}
	c.MongoDBName = "ecommerce_catalog"
	if test {
		c.MongoDBName = "ecommerce_catalog_test"
	} else if v := trim("MONGODB_DB_NAME"); v != "" {
		c.MongoDBName = v
	}
	if test {
		c.MeiliIndexSuffix = "_test"
		c.RedisKeyPrefix = "ecom:test:"
		c.MQPrefix = "ecom.test."
	} else {
		c.RedisKeyPrefix = "ecom:"
		c.MQPrefix = "ecom."
	}
	for _, o := range strings.Split(get("CORS_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}
	c.AccessTokenMinutes = 15
	c.RefreshTokenDays = 30
	c.RateLimit = !test || trim("RATE_LIMIT") == "on"
	return c, nil
}
