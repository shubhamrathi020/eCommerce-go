package commerce

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/cache"
	"github.com/shubhamrathi020/eCommerce-go/internal/catalog"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

var statusLabel = map[string]string{"packed": "Packed", "shipped": "Shipped", "delivered": "Delivered", "cancelled": "Order cancelled"}

// AdminOrders is back-office order management against real orders. It reuses OrderTransitions, the exact same
// state machine the customer-facing cancel already enforces.
type AdminOrders struct {
	store     *db.Store
	orders    *Orders
	inventory *Inventory
	razorpay  *Razorpay
	coupons   *cache.Coupons
	events    EventPublisher
}

// NewAdminOrders builds the admin order service.
func NewAdminOrders(store *db.Store, orders *Orders, inventory *Inventory, razorpay *Razorpay, coupons *cache.Coupons, events EventPublisher) *AdminOrders {
	return &AdminOrders{store: store, orders: orders, inventory: inventory, razorpay: razorpay, coupons: coupons, events: events}
}

// AdminOrderQuery narrows the admin order list.
type AdminOrderQuery struct {
	Q, Status, PaymentMethod, From, To string
	Page, PageSize                     int
}

// List is one page of orders.
func (a *AdminOrders) List(ctx context.Context, q AdminOrderQuery) (*catalog.Paged[domain.AdminOrderRow], error) {
	rows, total, page, err := db.SearchOrders(ctx, a.store.Pool, db.AdminOrderFilter{Q: q.Q, Status: q.Status, PaymentMethod: q.PaymentMethod, From: q.From, To: q.To}, q.Page, max(1, q.PageSize))
	if err != nil {
		return nil, err
	}
	items := make([]domain.AdminOrderRow, len(rows))
	for i := range rows {
		o := ToOrder(&rows[i])
		var count int64
		for _, l := range o.Lines {
			count += l.Quantity
		}
		items[i] = domain.AdminOrderRow{ID: o.ID, CreatedAt: o.CreatedAt, CustomerName: o.Contact.Name, ItemCount: count, Total: o.Totals.Total, Status: o.Status, PaymentStatus: o.PaymentStatus, PaymentMethod: o.PaymentMethod}
	}
	return &catalog.Paged[domain.AdminOrderRow]{Items: items, Total: total, Page: page, PageSize: max(1, q.PageSize)}, nil
}

// Get is one order with its staff-only fields.
func (a *AdminOrders) Get(ctx context.Context, id string) (*domain.AdminOrderDetail, error) {
	row, err := db.OrderByID(ctx, a.store.Pool, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, apperr.New(apperr.NotFound, "Order not found")
	}
	order := ToOrder(row)
	notes := notesOf(row)
	if notes == nil {
		notes = []domain.OrderNote{}
	}
	next := domain.OrderTransitions[order.Status]
	if next == nil {
		next = []domain.OrderStatus{}
	}
	return &domain.AdminOrderDetail{Order: order, Notes: notes, AllowedNext: next}, nil
}

func humanStatus(s string) string { return strings.Replace(s, "_", " ", 1) }

// Advance moves an order along its state machine.
func (a *AdminOrders) Advance(ctx context.Context, id, status, actor string) (*domain.AdminOrderDetail, error) {
	row, err := db.OrderByID(ctx, a.store.Pool, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, apperr.New(apperr.NotFound, "Order not found")
	}
	if !domain.CanTransition(domain.OrderStatus(row.Status), domain.OrderStatus(status)) {
		return nil, apperr.New(apperr.Validation, fmt.Sprintf("An order that is %s cannot move to %s.", humanStatus(row.Status), humanStatus(status)))
	}
	if status == "cancelled" {
		_ = a.inventory.GiveBack(ctx, StockLinesOf(row))
		if row.CouponCode != nil {
			a.coupons.Release(ctx, *row.CouponCode)
		}
		// A real refund, automatically, when this was a captured online payment and keys are configured;
		// otherwise the order is left refund_pending for a manual refund.
		payStatus := row.PaymentStatus
		if row.PaymentStatus == "paid" {
			payStatus = "refund_pending"
			if a.razorpay.Enabled() && row.ProviderPaymentID != nil {
				_, amount := totalOf(row)
				go a.attemptRefund(row.ID, *row.ProviderPaymentID, amount)
			}
		}
		kept := []domain.TimelineEntry{}
		for _, t := range timelineOf(row) {
			if t.Status == "placed" || t.Status == "paid" || t.Status == "confirmed" {
				kept = append(kept, t)
			}
		}
		kept = append(kept, domain.TimelineEntry{Status: "cancelled", Label: statusLabel["cancelled"] + " (by staff)", At: domain.ISO(time.Now())})
		err = a.store.Tx(ctx, func(tx pgx.Tx) error {
			if _, e := db.UpdateOrder(ctx, tx, id, db.OrderUpdate{Status: &status, PaymentStatus: &payStatus, Timeline: mustJSON(kept)}); e != nil {
				return e
			}
			return db.WriteOutbox(ctx, tx, "order.cancelled", map[string]any{"orderId": id, "email": row.ContactEmail, "name": row.ContactName, "byStaff": true})
		})
	} else {
		label := statusLabel[status]
		if label == "" {
			label = status
		}
		timeline := append(timelineOf(row), domain.TimelineEntry{Status: status, Label: label, At: domain.ISO(time.Now())})
		_, err = db.UpdateOrder(ctx, a.store.Pool, id, db.OrderUpdate{Status: &status, Timeline: mustJSON(timeline)})
	}
	if err != nil {
		return nil, err
	}
	a.addNoteInternal(ctx, id, actor, "Status changed to "+humanStatus(status)+".")
	updated, err := db.OrderByID(ctx, a.store.Pool, id)
	if err != nil || updated == nil {
		return nil, err
	}
	a.events.Publish(ctx, ToOrder(updated))
	return a.Get(ctx, id)
}

// attemptRefund records the refund; it never fails the caller if Razorpay is briefly unreachable.
func (a *AdminOrders) attemptRefund(orderID, providerPaymentID string, amount int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.razorpay.Refund(ctx, providerPaymentID, amount); err != nil {
		a.addNoteInternal(ctx, orderID, "system", "Automatic refund failed ("+err.Error()+"); refund it manually in the Razorpay dashboard.")
		return
	}
	a.addNoteInternal(ctx, orderID, "system", "Refund issued via Razorpay.")
	if row, err := db.OrderByID(ctx, a.store.Pool, orderID); err == nil && row != nil {
		_ = db.WriteOutbox(ctx, a.store.Pool, "payment.refunded", map[string]any{"orderId": orderID, "email": row.ContactEmail, "name": row.ContactName, "amount": domain.INRInt(amount)})
	}
}

// AddNote adds a staff note.
func (a *AdminOrders) AddNote(ctx context.Context, id, actor, text string) (*domain.AdminOrderDetail, error) {
	row, err := db.OrderByID(ctx, a.store.Pool, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, apperr.New(apperr.NotFound, "Order not found")
	}
	a.addNoteInternal(ctx, id, actor, text)
	return a.Get(ctx, id)
}

func (a *AdminOrders) addNoteInternal(ctx context.Context, id, actor, text string) {
	row, err := db.OrderByID(ctx, a.store.Pool, id)
	if err != nil || row == nil {
		return
	}
	note := domain.OrderNote{ID: newNoteID(), At: domain.ISO(time.Now()), Author: actor, Text: text}
	_, _ = db.UpdateOrder(ctx, a.store.Pool, id, db.OrderUpdate{Notes: mustJSON(append([]domain.OrderNote{note}, notesOf(row)...))})
}

var _ = httpx.Logger
