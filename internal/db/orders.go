package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Order is a row of orders.
type Order struct {
	ID                string
	Status            string
	PaymentStatus     string
	PaymentMethod     string
	UserID            *string
	GuestToken        *string
	Lines             json.RawMessage
	Totals            json.RawMessage
	CouponCode        *string
	ShippingMethod    string
	ContactName       string
	ContactEmail      string
	ContactPhone      string
	AddressLine1      string
	AddressLine2      *string
	AddressCity       string
	AddressState      string
	AddressPincode    string
	Timeline          json.RawMessage
	Notes             json.RawMessage
	IdempotencyKey    string
	PaymentDeadline   *time.Time
	ProviderOrderID   *string
	ProviderPaymentID *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const orderCols = `"id","status","paymentStatus","paymentMethod","userId","guestToken","lines","totals","couponCode","shippingMethod","contactName","contactEmail","contactPhone","addressLine1","addressLine2","addressCity","addressState","addressPincode","timeline","notes","idempotencyKey","paymentDeadline","providerOrderId","providerPaymentId","createdAt","updatedAt"`

func (o *Order) dest() []any {
	return []any{&o.ID, &o.Status, &o.PaymentStatus, &o.PaymentMethod, &o.UserID, &o.GuestToken, &o.Lines, &o.Totals, &o.CouponCode, &o.ShippingMethod, &o.ContactName, &o.ContactEmail, &o.ContactPhone, &o.AddressLine1, &o.AddressLine2, &o.AddressCity, &o.AddressState, &o.AddressPincode, &o.Timeline, &o.Notes, &o.IdempotencyKey, &o.PaymentDeadline, &o.ProviderOrderID, &o.ProviderPaymentID, &o.CreatedAt, &o.UpdatedAt}
}

func scanOrder(row pgx.Row) (*Order, error) {
	var o Order
	err := row.Scan(o.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}

func scanOrders(rows pgx.Rows) ([]Order, error) {
	defer rows.Close()
	out := []Order{}
	for rows.Next() {
		var o Order
		if err := rows.Scan(o.dest()...); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OrderByID finds an order; nil when none.
func OrderByID(ctx context.Context, q DBTX, id string) (*Order, error) {
	return scanOrder(q.QueryRow(ctx, `SELECT `+orderCols+` FROM orders WHERE "id"=$1`, id))
}

// OrderByIdempotencyKey finds the order a retry key already created.
func OrderByIdempotencyKey(ctx context.Context, q DBTX, key string) (*Order, error) {
	return scanOrder(q.QueryRow(ctx, `SELECT `+orderCols+` FROM orders WHERE "idempotencyKey"=$1`, key))
}

// OrderByProviderOrderID finds an order by the payment provider's order id.
func OrderByProviderOrderID(ctx context.Context, q DBTX, providerOrderID string) (*Order, error) {
	return scanOrder(q.QueryRow(ctx, `SELECT `+orderCols+` FROM orders WHERE "providerOrderId"=$1 LIMIT 1`, providerOrderID))
}

// InsertOrder creates an order row.
func InsertOrder(ctx context.Context, q DBTX, o *Order) error {
	now := Now()
	_, err := q.Exec(ctx, `INSERT INTO orders (`+orderCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19::jsonb,$20::jsonb,$21,$22,$23,$24,$25,$26)`,
		o.ID, o.Status, o.PaymentStatus, o.PaymentMethod, o.UserID, o.GuestToken, string(o.Lines), string(o.Totals), o.CouponCode, o.ShippingMethod, o.ContactName, o.ContactEmail, o.ContactPhone,
		o.AddressLine1, o.AddressLine2, o.AddressCity, o.AddressState, o.AddressPincode, string(o.Timeline), string(o.Notes), o.IdempotencyKey, utc(o.PaymentDeadline), o.ProviderOrderID, o.ProviderPaymentID, now, now)
	return err
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// OrderUpdate lists the columns an update may change; nil fields are left alone.
type OrderUpdate struct {
	Status            *string
	PaymentStatus     *string
	Timeline          []byte
	Notes             []byte
	ProviderOrderID   *string
	ProviderPaymentID *string
	PaymentDeadline   *time.Time
}

// UpdateOrder changes the listed columns and returns the new row.
func UpdateOrder(ctx context.Context, q DBTX, id string, u OrderUpdate) (*Order, error) {
	sets := []string{`"updatedAt"=$2`}
	args := []any{id, Now()}
	add := func(col, cast string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf(`"%s"=$%d%s`, col, len(args), cast))
	}
	if u.Status != nil {
		add("status", "", *u.Status)
	}
	if u.PaymentStatus != nil {
		add("paymentStatus", "", *u.PaymentStatus)
	}
	if u.Timeline != nil {
		add("timeline", "::jsonb", string(u.Timeline))
	}
	if u.Notes != nil {
		add("notes", "::jsonb", string(u.Notes))
	}
	if u.ProviderOrderID != nil {
		add("providerOrderId", "", *u.ProviderOrderID)
	}
	if u.ProviderPaymentID != nil {
		add("providerPaymentId", "", *u.ProviderPaymentID)
	}
	if u.PaymentDeadline != nil {
		add("paymentDeadline", "", u.PaymentDeadline.UTC())
	}
	return scanOrder(q.QueryRow(ctx, `UPDATE orders SET `+strings.Join(sets, ",")+` WHERE "id"=$1 RETURNING `+orderCols, args...))
}

// OrdersForUser lists a signed-in customer's orders, newest first.
func OrdersForUser(ctx context.Context, q DBTX, userID string) ([]Order, error) {
	rows, err := q.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE "userId"=$1 ORDER BY "createdAt" DESC, "id" DESC`, userID)
	if err != nil {
		return nil, err
	}
	return scanOrders(rows)
}

// OrdersForGuest lists the orders placed on one device.
func OrdersForGuest(ctx context.Context, q DBTX, token string) ([]Order, error) {
	rows, err := q.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE "userId" IS NULL AND "guestToken"=$1 ORDER BY "createdAt" DESC, "id" DESC`, token)
	if err != nil {
		return nil, err
	}
	return scanOrders(rows)
}

// ExpiredUnpaidOrders are pending-payment orders past their payment deadline.
func ExpiredUnpaidOrders(ctx context.Context, q DBTX, now time.Time) ([]Order, error) {
	rows, err := q.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE "status"='pending_payment' AND "paymentDeadline" < $1`, now.UTC())
	if err != nil {
		return nil, err
	}
	return scanOrders(rows)
}

// AdminOrderFilter narrows the admin order list.
type AdminOrderFilter struct {
	Q             string
	Status        string
	PaymentMethod string
	From, To      string
}

// SearchOrders returns one page of orders for the admin list and the total match count.
func SearchOrders(ctx context.Context, q DBTX, f AdminOrderFilter, page, pageSize int) ([]Order, int, int, error) {
	conds := []string{"TRUE"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.Status != "" {
		add(`"status"=$%d`, f.Status)
	}
	if f.PaymentMethod != "" {
		add(`"paymentMethod"=$%d`, f.PaymentMethod)
	}
	if f.From != "" {
		if t, err := time.Parse(time.RFC3339, f.From); err == nil {
			add(`"createdAt">=$%d`, t.UTC())
		} else if t, err := time.Parse("2006-01-02", f.From); err == nil {
			add(`"createdAt">=$%d`, t.UTC())
		}
	}
	if f.To != "" {
		if t, err := time.Parse("2006-01-02", f.To); err == nil {
			add(`"createdAt"<=$%d`, t.UTC().Add(24*time.Hour-time.Millisecond))
		}
	}
	if s := strings.TrimSpace(f.Q); s != "" {
		args = append(args, "%"+escapeLike(s)+"%")
		n := len(args)
		conds = append(conds, fmt.Sprintf(`("id" ILIKE $%d OR "contactName" ILIKE $%d OR "contactEmail" ILIKE $%d)`, n, n, n))
	}
	where := strings.Join(conds, " AND ")
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM orders WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, 0, err
	}
	pages := (total + pageSize - 1) / pageSize
	if pages < 1 {
		pages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := q.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE `+where+fmt.Sprintf(` ORDER BY "createdAt" DESC, "id" DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, 0, err
	}
	list, err := scanOrders(rows)
	return list, total, page, err
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}
