package commerce

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
)

// PaymentResult is what the checkout widget hands back after a payment.
type PaymentResult struct {
	ProviderPaymentID string
	ProviderOrderID   string
	Signature         string
}

// Payments: initiate creates the Razorpay order the checkout widget needs; confirm verifies the signature
// server-side before an order is ever marked paid. The amount always comes from the order the server already
// priced, never from anything the client sends.
type Payments struct {
	store     *db.Store
	orders    *Orders
	cart      *Cart
	inventory *Inventory
	razorpay  *Razorpay
	events    EventPublisher
	cfg       *config.Config
}

// NewPayments builds the payment service.
func NewPayments(store *db.Store, orders *Orders, cart *Cart, inventory *Inventory, razorpay *Razorpay, events EventPublisher, cfg *config.Config) *Payments {
	return &Payments{store: store, orders: orders, cart: cart, inventory: inventory, razorpay: razorpay, events: events, cfg: cfg}
}

func totalOf(row *db.Order) (domain.Money, int64) {
	var t domain.CartTotals
	_ = json.Unmarshal(row.Totals, &t)
	return t.Total, t.Total.Amount
}

// Initiate creates the provider-side order for a pending online-payment order.
func (p *Payments) Initiate(ctx context.Context, orderID string, owner Owner) (*domain.PaymentSession, error) {
	if err := p.orders.SweepExpired(ctx); err != nil {
		return nil, err
	}
	row, err := p.orders.RowForOwner(ctx, orderID, owner)
	if err != nil {
		return nil, err
	}
	if row.Status != "pending_payment" {
		return nil, apperr.New(apperr.Validation, "This order does not need a payment.")
	}
	if row.PaymentStatus == "failed" {
		// A previous attempt released the stock hold; reclaim it before letting them try again.
		if err := p.inventory.Take(ctx, StockLinesOf(row)); err != nil {
			return nil, err
		}
		deadline := time.Now().UTC().Add(PaymentWindowMinutes * time.Minute)
		row, err = db.UpdateOrder(ctx, p.store.Pool, orderID, db.OrderUpdate{PaymentStatus: strPtr("pending"), PaymentDeadline: &deadline})
		if err != nil {
			return nil, err
		}
	}
	_, amount := totalOf(row)
	providerID, err := p.razorpay.CreateOrder(ctx, amount, row.ID)
	if err != nil {
		return nil, err
	}
	if _, err := db.UpdateOrder(ctx, p.store.Pool, orderID, db.OrderUpdate{ProviderOrderID: &providerID}); err != nil {
		return nil, err
	}
	return &domain.PaymentSession{OrderID: orderID, ProviderOrderID: providerID, KeyID: p.cfg.RazorpayKeyID, Amount: amount, Currency: "INR"}, nil
}

// Confirm verifies a payment signature and marks the order paid and confirmed.
func (p *Payments) Confirm(ctx context.Context, orderID string, owner Owner, result PaymentResult) (*domain.Order, error) {
	if err := p.orders.SweepExpired(ctx); err != nil {
		return nil, err
	}
	row, err := p.orders.RowForOwner(ctx, orderID, owner)
	if err != nil {
		return nil, err
	}
	if row.PaymentStatus == "paid" {
		out := ToOrder(row)
		return &out, nil
	}
	if row.Status == "cancelled" {
		return nil, apperr.New(apperr.Validation, "The payment window for this order expired and its items were released. Please place the order again.")
	}
	if row.Status != "pending_payment" {
		return nil, apperr.New(apperr.Validation, "This order can no longer be paid.")
	}
	valid := row.ProviderOrderID != nil && result.ProviderOrderID == *row.ProviderOrderID && p.razorpay.VerifyPaymentSignature(result.ProviderOrderID, result.ProviderPaymentID, result.Signature)
	if !valid {
		return nil, apperr.New(apperr.Validation, "Payment verification failed. If money was deducted it will be refunded.")
	}
	out, err := p.markPaid(ctx, row, result.ProviderPaymentID, nil)
	if err != nil {
		return nil, err
	}
	_ = p.cart.ReplaceWithEmpty(ctx, OwnerKey(owner.UserID, owner.GuestToken), domain.ShippingMethodID(row.ShippingMethod))
	return out, nil
}

// markPaid marks an order paid and confirmed once, however many times the browser and the webhook report the same payment:
// only the report that actually changes the row writes the confirmation email and publishes the live order event.
func (p *Payments) markPaid(ctx context.Context, row *db.Order, providerPaymentID string, note *domain.OrderNote) (*domain.Order, error) {
	now := domain.ISO(time.Now())
	timeline := append(timelineOf(row), domain.TimelineEntry{Status: "paid", Label: "Payment received", At: now}, domain.TimelineEntry{Status: "confirmed", Label: "Order confirmed", At: now})
	update := db.OrderUpdate{Status: strPtr("confirmed"), PaymentStatus: strPtr("paid"), ProviderPaymentID: &providerPaymentID, Timeline: mustJSON(timeline), OnlyIfUnpaid: true}
	if note != nil {
		update.Notes = mustJSON(append([]domain.OrderNote{*note}, notesOf(row)...))
	}
	total, _ := totalOf(row)
	var updated *db.Order
	err := p.store.Tx(ctx, func(tx pgx.Tx) error {
		var e error
		updated, e = db.UpdateOrder(ctx, tx, row.ID, update)
		if e != nil || updated == nil {
			return e
		}
		return db.WriteOutbox(ctx, tx, "payment.confirmed", map[string]any{"orderId": row.ID, "email": row.ContactEmail, "name": row.ContactName, "amount": total})
	})
	if err != nil {
		return nil, err
	}
	if updated == nil { // already paid: report the order as it is now
		current, err := db.OrderByID(ctx, p.store.Pool, row.ID)
		if err != nil || current == nil {
			return nil, err
		}
		out := ToOrder(current)
		return &out, nil
	}
	out := ToOrder(updated)
	p.events.Publish(ctx, out)
	return &out, nil
}

func newNoteID() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return "note_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b)
}

func notesOf(row *db.Order) []domain.OrderNote {
	var n []domain.OrderNote
	_ = json.Unmarshal(row.Notes, &n)
	return n
}

// Fail records a failed attempt and gives its stock back; a retry (via Initiate) claims it again.
func (p *Payments) Fail(ctx context.Context, orderID string, owner Owner, reason string) (*domain.Order, error) {
	row, err := p.orders.RowForOwner(ctx, orderID, owner)
	if err != nil {
		return nil, err
	}
	if row.PaymentStatus == "paid" {
		out := ToOrder(row)
		return &out, nil
	}
	_ = p.inventory.GiveBack(ctx, StockLinesOf(row))
	note := systemNote("Payment attempt failed: " + reason)
	notes := append([]domain.OrderNote{note}, notesOf(row)...)
	updated, err := db.UpdateOrder(ctx, p.store.Pool, orderID, db.OrderUpdate{PaymentStatus: strPtr("failed"), Notes: mustJSON(notes)})
	if err != nil {
		return nil, err
	}
	out := ToOrder(updated)
	return &out, nil
}

func systemNote(text string) domain.OrderNote {
	return domain.OrderNote{ID: newNoteID(), At: domain.ISO(time.Now()), Author: "system", Text: text}
}

// HandleWebhook applies a verified Razorpay event idempotently. It is the safety net for a payment the browser never reported
// (closed tab, lost connection, a UPI request approved later).
//
// Only a captured payment changes anything. Razorpay sends payment.failed for every failed attempt, also while its window is
// still open and the shopper tries another method, so a failure here must not give the stock back (the browser reports a real
// give-up through Fail, and the payment-deadline sweep releases an abandoned order).
func (p *Payments) HandleWebhook(ctx context.Context, body []byte) {
	var event struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID      string `json:"id"`
					OrderID string `json:"order_id"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
	}
	if json.Unmarshal(body, &event) != nil {
		return
	}
	payment := event.Payload.Payment.Entity
	if event.Event != "payment.captured" || payment.OrderID == "" || payment.ID == "" {
		return
	}
	order, err := db.OrderByProviderOrderID(ctx, p.store.Pool, payment.OrderID)
	if err != nil || order == nil || order.PaymentStatus == "paid" || order.Status != "pending_payment" {
		return
	}
	var shortfall *domain.OrderNote
	if order.PaymentStatus == "failed" {
		// The browser reported a failure, which released the stock hold, before this payment was captured: take it again.
		if err := p.inventory.Take(ctx, StockLinesOf(order)); err != nil {
			// The money is taken, so the order is confirmed anyway; staff see why it may not be shippable.
			n := systemNote("Paid after its stock hold was released, and the items could not be reserved again. Check stock before shipping, or cancel and refund.")
			shortfall = &n
		}
	}
	_, _ = p.markPaid(ctx, order, payment.ID, shortfall)
}
