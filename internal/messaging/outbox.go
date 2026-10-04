package messaging

import (
	"context"
	"strings"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/cache"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

const (
	relayPoll     = 2 * time.Second
	relayLockTTL  = 10 * time.Second
	relayBatch    = 20
	relayLockName = "lock:outbox-relay"
)

// Relay polls outbox_events for unpublished rows and publishes each to RabbitMQ. It is guarded by a Redis lock
// (SET NX EX): with several API replicas only the one holding the lock for a tick queries and publishes. The lock is
// released right after the tick's work, so this instance's own next tick two seconds later can run. If the broker is
// down, the row's attempts and last error are recorded and publishedAt stays null, so the next tick tries the same
// row again: no event is ever silently dropped, only delayed.
type Relay struct {
	store  *db.Store
	rabbit *Rabbit
	rdb    *cache.Redis
}

// NewRelay builds the relay.
func NewRelay(store *db.Store, rabbit *Rabbit, rdb *cache.Redis) *Relay {
	return &Relay{store: store, rabbit: rabbit, rdb: rdb}
}

// Run ticks until ctx ends.
func (r *Relay) Run(ctx context.Context) {
	t := time.NewTicker(relayPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.tick(ctx); err != nil && ctx.Err() == nil {
				httpx.Logger.Error("tick failed: "+err.Error(), "logger", "OutboxRelay")
			}
		}
	}
}

func (r *Relay) tick(ctx context.Context) error {
	got, err := r.rdb.Client.SetNX(ctx, r.rdb.Key(relayLockName), "1", relayLockTTL).Result()
	if err != nil || !got {
		return err
	}
	defer r.rdb.Client.Del(context.Background(), r.rdb.Key(relayLockName))
	rows, err := db.UnpublishedOutbox(ctx, r.store.Pool, relayBatch)
	if err != nil {
		return err
	}
	for _, row := range rows {
		var payload any = row.Payload
		if err := r.rabbit.Publish(ctx, row.RoutingKey, rawJSON(row.Payload), PublishOptions{MessageID: row.ID}); err != nil {
			_ = db.MarkOutboxFailed(ctx, r.store.Pool, row.ID, row.Attempts+1, err.Error())
			httpx.Logger.Warn("publish failed for outbox event "+row.ID+" ("+row.RoutingKey+"): "+err.Error(), "logger", "OutboxRelay")
			continue
		}
		_ = payload
		if err := db.MarkOutboxPublished(ctx, r.store.Pool, row.ID, row.Attempts+1); err != nil {
			return err
		}
	}
	return nil
}

// rawJSON is a payload that is already JSON and must not be encoded a second time.
type rawJSON []byte

// MarshalJSON returns the bytes unchanged.
func (r rawJSON) MarshalJSON() ([]byte, error) { return []byte(r), nil }

var _ = strings.TrimSpace
