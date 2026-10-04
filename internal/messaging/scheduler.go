package messaging

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/cache"
	"github.com/shubhamrathi020/eCommerce-go/internal/commerce"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

const (
	sweepInterval     = 30 * time.Second
	abandonedInterval = 5 * time.Minute
)

// Scheduler runs the scheduled jobs actively instead of lazily on the next request. Each job is guarded by a Redis
// lock released right after it finishes: with several API replicas only whichever wins the lock for a tick does the
// work. The TTL is only a crash-safety net, not the normal release path.
type Scheduler struct {
	store  *db.Store
	rdb    *cache.Redis
	orders *commerce.Orders
	cfg    *config.Config
}

// NewScheduler builds the scheduler.
func NewScheduler(store *db.Store, rdb *cache.Redis, orders *commerce.Orders, cfg *config.Config) *Scheduler {
	return &Scheduler{store: store, rdb: rdb, orders: orders, cfg: cfg}
}

// Run starts both timers and blocks until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	sweep, abandoned := time.NewTicker(sweepInterval), time.NewTicker(abandonedInterval)
	defer sweep.Stop()
	defer abandoned.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			if err := s.withLock(ctx, "sweep-expired", 20*time.Second, s.orders.SweepExpired); err != nil && ctx.Err() == nil {
				httpx.Logger.Error("reservation-expiry sweep failed: "+err.Error(), "logger", "Scheduler")
			}
		case <-abandoned.C:
			if err := s.withLock(ctx, "abandoned-cart", 60*time.Second, s.RemindAbandonedCarts); err != nil && ctx.Err() == nil {
				httpx.Logger.Error("abandoned-cart reminder failed: "+err.Error(), "logger", "Scheduler")
			}
		}
	}
}

func (s *Scheduler) withLock(ctx context.Context, name string, ttl time.Duration, job func(context.Context) error) error {
	got, err := s.rdb.Client.SetNX(ctx, s.rdb.Key("lock:"+name), "1", ttl).Result()
	if err != nil || !got {
		return err
	}
	defer s.rdb.Client.Del(context.Background(), s.rdb.Key("lock:"+name))
	return job(ctx)
}

// RemindAbandonedCarts sends one reminder per cart per "abandoned" episode: a signed-in customer's cart that has sat
// untouched, non-empty, for the configured minutes and has not been reminded since its last real change. Guest carts
// are skipped: there is no email to send to until checkout.
func (s *Scheduler) RemindAbandonedCarts(ctx context.Context) error {
	cutoff := time.Now().Add(-time.Duration(s.cfg.AbandonedCartMinutes) * time.Minute)
	carts, err := db.AbandonedCarts(ctx, s.store.Pool, cutoff, 50)
	if err != nil {
		return err
	}
	for _, c := range carts {
		userID := c.OwnerKey[len("user:"):]
		user, err := db.UserByID(ctx, s.store.Pool, userID)
		if err != nil {
			return err
		}
		if user == nil {
			continue
		}
		var items []struct {
			Quantity int `json:"quantity"`
		}
		_ = json.Unmarshal(c.Items, &items)
		count := 0
		for _, i := range items {
			count += i.Quantity
		}
		if count == 0 {
			continue
		}
		if err := db.WriteOutbox(ctx, s.store.Pool, "cart.abandoned", map[string]any{"email": user.Email, "name": user.Name, "itemCount": count}); err != nil {
			return err
		}
		if err := db.MarkCartReminded(ctx, s.store.Pool, c.ID); err != nil {
			return err
		}
	}
	return nil
}
