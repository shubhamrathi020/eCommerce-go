package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cart is a row of carts.
type Cart struct {
	ID             string
	OwnerKey       string
	Items          json.RawMessage
	CouponCode     *string
	ShippingMethod string
	ReminderSentAt *time.Time
	UpdatedAt      time.Time
}

const cartCols = `"id","ownerKey","items","couponCode","shippingMethod","reminderSentAt","updatedAt"`

func scanCart(row pgx.Row) (*Cart, error) {
	var c Cart
	err := row.Scan(&c.ID, &c.OwnerKey, &c.Items, &c.CouponCode, &c.ShippingMethod, &c.ReminderSentAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// CartByOwner finds a cart; nil when none.
func CartByOwner(ctx context.Context, q DBTX, ownerKey string) (*Cart, error) {
	return scanCart(q.QueryRow(ctx, `SELECT `+cartCols+` FROM carts WHERE "ownerKey"=$1`, ownerKey))
}

// UpsertCart writes a cart's contents; resetReminder also clears the abandoned-cart reminder flag.
func UpsertCart(ctx context.Context, q DBTX, ownerKey string, items []byte, coupon *string, shipping string, resetReminder bool) error {
	now := Now()
	if resetReminder {
		_, err := q.Exec(ctx, `INSERT INTO carts ("id","ownerKey","items","couponCode","shippingMethod","updatedAt") VALUES ($1,$2,$3::jsonb,$4,$5,$6)
			ON CONFLICT ("ownerKey") DO UPDATE SET "items"=EXCLUDED."items","couponCode"=EXCLUDED."couponCode","shippingMethod"=EXCLUDED."shippingMethod","reminderSentAt"=NULL,"updatedAt"=EXCLUDED."updatedAt"`,
			NewID(), ownerKey, string(items), coupon, shipping, now)
		return err
	}
	_, err := q.Exec(ctx, `INSERT INTO carts ("id","ownerKey","items","couponCode","shippingMethod","updatedAt") VALUES ($1,$2,$3::jsonb,$4,$5,$6)
		ON CONFLICT ("ownerKey") DO UPDATE SET "items"=EXCLUDED."items","couponCode"=EXCLUDED."couponCode","shippingMethod"=EXCLUDED."shippingMethod","updatedAt"=EXCLUDED."updatedAt"`,
		NewID(), ownerKey, string(items), coupon, shipping, now)
	return err
}

// DeleteCart removes a cart (used after a guest cart is merged).
func DeleteCart(ctx context.Context, q DBTX, ownerKey string) error {
	_, err := q.Exec(ctx, `DELETE FROM carts WHERE "ownerKey"=$1`, ownerKey)
	return err
}

// AbandonedCarts are signed-in carts untouched since the cutoff, with items, not yet reminded.
func AbandonedCarts(ctx context.Context, q DBTX, cutoff time.Time, limit int) ([]Cart, error) {
	rows, err := q.Query(ctx, `SELECT `+cartCols+` FROM carts WHERE "updatedAt" < $1 AND "reminderSentAt" IS NULL AND "ownerKey" LIKE 'user:%' AND "items" <> '[]'::jsonb LIMIT $2`, cutoff.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cart{}
	for rows.Next() {
		var c Cart
		if err := rows.Scan(&c.ID, &c.OwnerKey, &c.Items, &c.CouponCode, &c.ShippingMethod, &c.ReminderSentAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkCartReminded records that the reminder went out.
func MarkCartReminded(ctx context.Context, q DBTX, id string) error {
	_, err := q.Exec(ctx, `UPDATE carts SET "reminderSentAt"=$2 WHERE "id"=$1`, id, Now())
	return err
}

// OutboxEvent is a row of outbox_events.
type OutboxEvent struct {
	ID         string
	RoutingKey string
	Payload    json.RawMessage
	Attempts   int
}

// WriteOutbox inserts an event; call it with the same transaction as the domain write it belongs to.
func WriteOutbox(ctx context.Context, q DBTX, routingKey string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO outbox_events ("id","routingKey","payload","createdAt") VALUES ($1,$2,$3::jsonb,$4)`, NewID(), routingKey, string(b), Now())
	return err
}

// UnpublishedOutbox returns the oldest events not yet relayed.
func UnpublishedOutbox(ctx context.Context, q DBTX, limit int) ([]OutboxEvent, error) {
	rows, err := q.Query(ctx, `SELECT "id","routingKey","payload","attempts" FROM outbox_events WHERE "publishedAt" IS NULL ORDER BY "createdAt" ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxEvent{}
	for rows.Next() {
		var e OutboxEvent
		if err := rows.Scan(&e.ID, &e.RoutingKey, &e.Payload, &e.Attempts); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkOutboxPublished records a successful publish.
func MarkOutboxPublished(ctx context.Context, q DBTX, id string, attempts int) error {
	_, err := q.Exec(ctx, `UPDATE outbox_events SET "publishedAt"=$2,"attempts"=$3 WHERE "id"=$1`, id, Now(), attempts)
	return err
}

// MarkOutboxFailed records a failed publish attempt.
func MarkOutboxFailed(ctx context.Context, q DBTX, id string, attempts int, message string) error {
	if len(message) > 500 {
		message = message[:500]
	}
	_, err := q.Exec(ctx, `UPDATE outbox_events SET "attempts"=$2,"lastError"=$3 WHERE "id"=$1`, id, attempts, message)
	return err
}

// CountUnpublishedOutbox is the backlog gauge.
func CountUnpublishedOutbox(ctx context.Context, q DBTX) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE "publishedAt" IS NULL`).Scan(&n)
	return n, err
}
