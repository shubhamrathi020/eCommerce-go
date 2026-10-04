// Package cache is Redis: cache-aside reads with tagged invalidation and stampede protection, per-route rate
// limits and atomic coupon-redemption counters. Nothing personalised or private is ever cached.
package cache

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// Redis is the one connection the API uses; every key it touches is namespaced by a prefix so a test run can
// never collide with development data in the same Redis.
type Redis struct {
	Client *redis.Client
	Prefix string
}

// OpenRedis connects and verifies the connection.
func OpenRedis(ctx context.Context, cfg *config.Config) (*Redis, error) {
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, err
	}
	opts.MaxRetries = 2
	c := redis.NewClient(opts)
	if err := c.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return &Redis{Client: c, Prefix: cfg.RedisKeyPrefix}, nil
}

// Key applies the namespace.
func (r *Redis) Key(k string) string { return r.Prefix + k }

// Ping checks Redis answers.
func (r *Redis) Ping(ctx context.Context) error { return r.Client.Ping(ctx).Err() }

// Close disconnects.
func (r *Redis) Close() { _ = r.Client.Close() }

// Stats are the cache and rate-limit counters (reset on every restart; in-process, not persisted).
type Stats struct {
	Hits             int64            `json:"hits"`
	Misses           int64            `json:"misses"`
	Coalesced        int64            `json:"coalesced"`
	RateLimitBlocked map[string]int64 `json:"rateLimitBlocked"`
}

type flight struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Cache is cache-aside with tags and per-process stampede protection: two requests on this instance for the same
// key share one backend fetch.
type Cache struct {
	rdb      *Redis
	mu       sync.Mutex
	inflight map[string]*flight
	stats    Stats
}

// New builds a cache over a Redis connection.
func New(rdb *Redis) *Cache {
	return &Cache{rdb: rdb, inflight: map[string]*flight{}, stats: Stats{RateLimitBlocked: map[string]int64{}}}
}

// Stats returns a snapshot of the counters.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	blocked := make(map[string]int64, len(c.stats.RateLimitBlocked))
	for k, v := range c.stats.RateLimitBlocked {
		blocked[k] = v
	}
	return Stats{Hits: c.stats.Hits, Misses: c.stats.Misses, Coalesced: c.stats.Coalesced, RateLimitBlocked: blocked}
}

// RecordRateLimitBlocked counts a refused request for a bucket.
func (c *Cache) RecordRateLimitBlocked(bucket string) {
	c.mu.Lock()
	c.stats.RateLimitBlocked[bucket]++
	c.mu.Unlock()
}

// GetOrSet returns the cached value for key, or computes it, caches it for ttl and records it under every tag
// (so InvalidateTag can find and drop it later). A Redis outage degrades to "always compute, never cache".
func GetOrSet[T any](ctx context.Context, c *Cache, key string, ttl time.Duration, tags func(T) []string, factory func() (T, error)) (T, error) {
	var zero T
	if raw, err := c.rdb.Client.Get(ctx, c.rdb.Key(key)).Result(); err == nil {
		var v T
		if json.Unmarshal([]byte(raw), &v) == nil {
			c.mu.Lock()
			c.stats.Hits++
			c.mu.Unlock()
			return v, nil
		}
	}

	c.mu.Lock()
	if f, ok := c.inflight[key]; ok {
		c.stats.Coalesced++
		c.mu.Unlock()
		f.wg.Wait()
		if f.err != nil {
			return zero, f.err
		}
		return f.val.(T), nil
	}
	c.stats.Misses++
	f := &flight{}
	f.wg.Add(1)
	c.inflight[key] = f
	c.mu.Unlock()

	value, err := factory()
	if err == nil {
		// Best-effort: a Redis write failure degrades to "computed but not cached".
		if raw, mErr := json.Marshal(value); mErr == nil {
			pipe := c.rdb.Client.Pipeline()
			pipe.Set(ctx, c.rdb.Key(key), raw, ttl)
			for _, tag := range tags(value) {
				pipe.SAdd(ctx, c.rdb.Key("tag:"+tag), key)
			}
			_, _ = pipe.Exec(ctx)
		}
	}
	f.val, f.err = value, err
	c.mu.Lock()
	delete(c.inflight, key)
	c.mu.Unlock()
	f.wg.Done()
	return value, err
}

// InvalidateTag drops every cache entry ever tagged with tag. Best-effort: TTLs are the backstop.
func (c *Cache) InvalidateTag(ctx context.Context, tag string) {
	tagKey := c.rdb.Key("tag:" + tag)
	keys, err := c.rdb.Client.SMembers(ctx, tagKey).Result()
	if err != nil || len(keys) == 0 {
		return
	}
	full := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		full = append(full, c.rdb.Key(k))
	}
	full = append(full, tagKey)
	_ = c.rdb.Client.Del(ctx, full...).Err()
}

// RateLimiter enforces the named Redis buckets (search, coupon, checkout): a plain fixed-window counter per
// client IP, per bucket, per window. It fails open on a Redis outage: a limiter that takes the site down on a
// cache blip is worse than a brief lapse in limiting.
type RateLimiter struct {
	rdb    *Redis
	cache  *Cache
	cfg    *config.Config
	limits map[string]config.Limit
}

// NewRateLimiter builds the limiter.
func NewRateLimiter(rdb *Redis, cache *Cache, cfg *config.Config) *RateLimiter {
	return &RateLimiter{rdb: rdb, cache: cache, cfg: cfg, limits: map[string]config.Limit{
		"search": cfg.RateLimits.Search, "coupon": cfg.RateLimits.Coupon, "checkout": cfg.RateLimits.Checkout,
	}}
}

// Bucket guards a route with one of the named buckets.
func (l *RateLimiter) Bucket(bucket string) func(httpx.HandlerFunc) httpx.HandlerFunc {
	return func(next httpx.HandlerFunc) httpx.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) error {
			if !l.cfg.RateLimit {
				return next(w, r)
			}
			lim := l.limits[bucket]
			now := time.Now().Unix()
			windowStart := now / int64(lim.WindowSeconds) * int64(lim.WindowSeconds)
			key := l.rdb.Key("ratelimit:" + bucket + ":" + strconv.FormatInt(windowStart, 10) + ":" + httpx.ClientIP(r))
			count, err := l.rdb.Client.Incr(r.Context(), key).Result()
			if err != nil {
				return next(w, r) // Redis unavailable: fail open
			}
			if count == 1 {
				_ = l.rdb.Client.Expire(r.Context(), key, time.Duration(lim.WindowSeconds)*time.Second).Err()
			}
			if int(count) > lim.Limit {
				l.cache.RecordRateLimitBlocked(bucket)
				w.Header().Set("Retry-After", strconv.FormatInt(windowStart+int64(lim.WindowSeconds)-now, 10))
				return &httpx.HTTPError{Status: 429, Message: "Too many requests. Please wait a moment and try again."}
			}
			return next(w, r)
		}
	}
}

// cappedCoupons: coupons with a hard cap on total redemptions.
var cappedCoupons = map[string]int64{"WELCOME10": 500}

// Coupons is an atomic Redis counter, so redemptions across concurrent checkouts (or several API instances) can
// never exceed a coupon's cap.
type Coupons struct{ rdb *Redis }

// NewCoupons builds the counter.
func NewCoupons(rdb *Redis) *Coupons { return &Coupons{rdb: rdb} }

func (c *Coupons) key(code string) string {
	return c.rdb.Key("coupon:redemptions:" + strings.ToUpper(code))
}

// CapFor is the cap for a code, ok=false when it has none.
func (c *Coupons) CapFor(code string) (int64, bool) {
	v, ok := cappedCoupons[strings.ToUpper(code)]
	return v, ok
}

// TryClaim atomically claims one redemption slot; false when the cap is reached.
func (c *Coupons) TryClaim(ctx context.Context, code string) (bool, error) {
	cap, ok := c.CapFor(code)
	if !ok {
		return true, nil
	}
	n, err := c.rdb.Client.Incr(ctx, c.key(code)).Result()
	if err != nil {
		return false, err
	}
	if n > cap {
		_ = c.rdb.Client.Decr(ctx, c.key(code)).Err()
		return false, nil
	}
	return true, nil
}

// Release gives a claimed slot back (an order using this coupon was cancelled).
func (c *Coupons) Release(ctx context.Context, code string) {
	if _, ok := c.CapFor(code); ok {
		_ = c.rdb.Client.Decr(ctx, c.key(code)).Err()
	}
}

// StatsHandler serves the admin cache-stats and resilience endpoints.
type StatsHandler struct {
	cache  *Cache
	guards *httpx.Guards
	breaks func() []any
}

// NewStatsHandler builds the handler; breakers returns the circuit-breaker stats to report.
func NewStatsHandler(cache *Cache, guards *httpx.Guards, breakers func() []any) *StatsHandler {
	return &StatsHandler{cache: cache, guards: guards, breaks: breakers}
}

// Register mounts the routes.
func (h *StatsHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/system/cache-stats", httpx.Wrap(httpx.Chain(func(w http.ResponseWriter, r *http.Request) error {
		s := h.cache.Stats()
		total := s.Hits + s.Misses
		ratio := 0.0
		if total > 0 {
			ratio = float64(int(float64(s.Hits)/float64(total)*100+0.5)) / 100
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"hits": s.Hits, "misses": s.Misses, "coalesced": s.Coalesced, "rateLimitBlocked": s.RateLimitBlocked, "hitRatio": ratio})
		return nil
	}, h.guards.Require("system:read"))))
	mux.HandleFunc("GET /admin/system/resilience", httpx.Wrap(httpx.Chain(func(w http.ResponseWriter, r *http.Request) error {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"circuitBreakers": h.breaks()})
		return nil
	}, h.guards.Require("system:read"))))
}

var _ = apperr.Validation
