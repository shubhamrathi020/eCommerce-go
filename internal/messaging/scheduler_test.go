package messaging

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
)

// Needs the test PostgreSQL (the same one the Nest API tests use); skipped when it is not reachable.
func testStore(t *testing.T) *db.Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgresql://ecommerce:ecommerce@localhost:5432/ecommerce_test?schema=public"
	}
	store, err := db.Open(context.Background(), url)
	if err != nil {
		t.Skipf("test database not reachable: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestRemindsAnAbandonedCartOnceAndStaysQuietForAFreshOne(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	user, err := db.CreateUser(ctx, store.Pool, "Rina Shah", "rina-go-test@example.com", "hash", "verify-"+db.NewID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(ctx, `DELETE FROM carts WHERE "ownerKey"=$1`, "user:"+user.ID)
		_, _ = store.Pool.Exec(ctx, `DELETE FROM outbox_events WHERE "payload"->>'email'=$1`, user.Email)
		_ = db.DeleteUser(ctx, store.Pool, user.ID)
	})
	key := "user:" + user.ID
	if err := db.UpsertCart(ctx, store.Pool, key, []byte(`[{"variantId":"p-0002-v1","quantity":2,"seenPrice":100}]`), nil, "standard", false); err != nil {
		t.Fatal(err)
	}
	s := NewScheduler(store, nil, nil, &config.Config{AbandonedCartMinutes: 60})
	count := func() int {
		var n int
		_ = store.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE "routingKey"='cart.abandoned' AND "payload"->>'email'=$1`, user.Email).Scan(&n)
		return n
	}

	if err := s.RemindAbandonedCarts(ctx); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatal("a cart that was just touched must not be reminded about")
	}
	if err := db.BackdateCart(ctx, store.Pool, key, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RemindAbandonedCarts(ctx); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatalf("expected one reminder for the abandoned cart, got %d", count())
	}
	if err := s.RemindAbandonedCarts(ctx); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatalf("a second pass must not send a second reminder, got %d", count())
	}
}
