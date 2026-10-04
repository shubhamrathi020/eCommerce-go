package commerce

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/cache"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
)

var (
	emailRE = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
	phoneRE = regexp.MustCompile(`^[6-9][0-9]{9}$`)
)

// PaymentWindowMinutes is how long an online-payment order holds its stock before a sweep cancels it.
const PaymentWindowMinutes = 15

// EventPublisher pushes an order's new state to anyone watching it (the live tracking stream).
type EventPublisher interface {
	Publish(ctx context.Context, order domain.Order)
}

// Owner says whose order this is: a signed-in user, or a guest identified by the cart cookie.
type Owner struct {
	UserID     string
	GuestToken string
}

// StockLinesOf are the lines an order holds stock for.
func StockLinesOf(o *db.Order) []StockLine {
	var lines []domain.CartLine
	_ = json.Unmarshal(o.Lines, &lines)
	out := make([]StockLine, len(lines))
	for i, l := range lines {
		out[i] = StockLine{VariantID: l.VariantID, Quantity: l.Quantity}
	}
	return out
}

// ToOrder maps a database row to the public contract.
func ToOrder(row *db.Order) domain.Order {
	address := domain.Address{Line1: row.AddressLine1, City: row.AddressCity, State: row.AddressState, Pincode: row.AddressPincode}
	if row.AddressLine2 != nil && *row.AddressLine2 != "" {
		address.Line2 = *row.AddressLine2
	}
	o := domain.Order{
		ID: row.ID, Status: domain.OrderStatus(row.Status), PaymentStatus: domain.PaymentStatus(row.PaymentStatus), PaymentMethod: domain.PaymentMethod(row.PaymentMethod),
		ShippingMethod: domain.ShippingMethodID(row.ShippingMethod), Contact: domain.ContactDetails{Name: row.ContactName, Email: row.ContactEmail, Phone: row.ContactPhone},
		Address: address, CreatedAt: domain.ISO(row.CreatedAt),
	}
	_ = json.Unmarshal(row.Lines, &o.Lines)
	_ = json.Unmarshal(row.Totals, &o.Totals)
	_ = json.Unmarshal(row.Timeline, &o.Timeline)
	if o.Lines == nil {
		o.Lines = []domain.CartLine{}
	}
	if o.Timeline == nil {
		o.Timeline = []domain.TimelineEntry{}
	}
	if row.CouponCode != nil && *row.CouponCode != "" {
		o.CouponCode = *row.CouponCode
	}
	if row.UserID != nil && *row.UserID != "" {
		o.UserID = *row.UserID
	}
	return o
}

func assertDeliverable(pincode string) error {
	switch domain.DeliverabilityProblem(pincode, func(p string) bool { return domain.ComputeServiceability(p, time.Now()).Serviceable }) {
	case "invalid":
		return apperr.WithFields(apperr.Validation, "Enter a valid 6-digit pin code", map[string]string{"pincode": "Invalid pin code"})
	case "not_deliverable":
		return apperr.WithFields(apperr.Validation, "Sorry, we do not deliver to this pin code yet.", map[string]string{"pincode": "Not deliverable"})
	}
	return nil
}

// Orders is the order state machine, idempotent placement and stock holds.
type Orders struct {
	store     *db.Store
	cart      *Cart
	inventory *Inventory
	coupons   *cache.Coupons
	events    EventPublisher
}

// NewOrders builds the order service.
func NewOrders(store *db.Store, cart *Cart, inventory *Inventory, coupons *cache.Coupons, events EventPublisher) *Orders {
	return &Orders{store: store, cart: cart, inventory: inventory, coupons: coupons, events: events}
}

func newOrderID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "ORD-" + strings.ToUpper(strconv.FormatInt(time.Now().UnixMilli(), 36)) + strings.ToUpper(hex.EncodeToString(b))
}

func timelineOf(o *db.Order) []domain.TimelineEntry {
	var t []domain.TimelineEntry
	_ = json.Unmarshal(o.Timeline, &t)
	return t
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func strPtr(s string) *string { return &s }

// SweepExpired cancels unpaid orders whose payment window ran out, releasing their stock hold. It runs lazily at
// the top of every order-touching call and actively every 30 seconds by the scheduler.
func (o *Orders) SweepExpired(ctx context.Context) error {
	now := time.Now().UTC()
	expired, err := db.ExpiredUnpaidOrders(ctx, o.store.Pool, now)
	if err != nil {
		return err
	}
	for i := range expired {
		order := &expired[i]
		timeline := append(timelineOf(order), domain.TimelineEntry{Status: "cancelled", Label: "Cancelled: payment window expired", At: domain.ISO(now)})
		var updated *db.Order
		// Claim the order first (only one sweeper can move it out of pending_payment), then release what it held.
		err := o.store.Tx(ctx, func(tx pgx.Tx) error {
			var claimErr error
			updated, claimErr = o.claimExpired(ctx, tx, order.ID, timeline)
			if claimErr != nil || updated == nil {
				return claimErr
			}
			return db.WriteOutbox(ctx, tx, "order.expired", map[string]any{"orderId": order.ID, "email": order.ContactEmail, "name": order.ContactName})
		})
		if err != nil {
			return err
		}
		if updated == nil {
			continue
		}
		_ = o.inventory.GiveBack(ctx, StockLinesOf(order))
		if order.CouponCode != nil {
			o.coupons.Release(ctx, *order.CouponCode)
		}
		o.events.Publish(ctx, ToOrder(updated))
	}
	return nil
}

func (o *Orders) claimExpired(ctx context.Context, tx pgx.Tx, id string, timeline []domain.TimelineEntry) (*db.Order, error) {
	row, err := db.OrderByID(ctx, tx, id)
	if err != nil || row == nil || row.Status != "pending_payment" {
		return nil, err
	}
	return db.UpdateOrder(ctx, tx, id, db.OrderUpdate{Status: strPtr("cancelled"), PaymentStatus: strPtr("failed"), Timeline: mustJSON(timeline)})
}

func matchesOwner(row *db.Order, owner Owner) bool {
	if owner.UserID != "" {
		return row.UserID != nil && *row.UserID == owner.UserID
	}
	return row.UserID == nil && owner.GuestToken != "" && row.GuestToken != nil && *row.GuestToken == owner.GuestToken
}

// PlaceRequest is a validated order request.
type PlaceRequest struct {
	IdempotencyKey string
	Contact        domain.ContactDetails
	Address        domain.Address
	PaymentMethod  string
}

// Place creates an order from the owner's cart. Retrying with the same idempotency key returns the original order.
func (o *Orders) Place(ctx context.Context, ownerKey string, owner Owner, req PlaceRequest) (*domain.Order, error) {
	if err := o.SweepExpired(ctx); err != nil {
		return nil, err
	}
	if existing, err := db.OrderByIdempotencyKey(ctx, o.store.Pool, req.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		out := ToOrder(existing)
		return &out, nil
	}

	fields := map[string]string{}
	if strings.TrimSpace(req.Contact.Name) == "" {
		fields["name"] = "Name is required"
	}
	if !emailRE.MatchString(req.Contact.Email) {
		fields["email"] = "Enter a valid email address"
	}
	if !phoneRE.MatchString(req.Contact.Phone) {
		fields["phone"] = "Enter a valid 10-digit mobile number"
	}
	if strings.TrimSpace(req.Address.Line1) == "" {
		fields["line1"] = "Address is required"
	}
	if strings.TrimSpace(req.Address.City) == "" {
		fields["city"] = "City is required"
	}
	if strings.TrimSpace(req.Address.State) == "" {
		fields["state"] = "State is required"
	}
	if err := apperr.AssertNoFieldErrors(fields); err != nil {
		return nil, err
	}
	if err := assertDeliverable(req.Address.Pincode); err != nil {
		return nil, err
	}

	cart, err := o.cart.Get(ctx, ownerKey)
	if err != nil {
		return nil, err
	}
	if len(cart.Lines) == 0 {
		return nil, apperr.New(apperr.Validation, "Your cart is empty.")
	}
	if cart.Blocked {
		return nil, apperr.New(apperr.Validation, "Some items in your cart are out of stock. Remove them to continue.")
	}
	cod := req.PaymentMethod == "cod"
	if cod {
		if enabled, reason := domain.CODEligibility(req.Address.Pincode, cart.Totals.Total.Amount); !enabled {
			if reason == "" {
				reason = "Cash on delivery is not available."
			}
			return nil, apperr.New(apperr.Validation, reason)
		}
	}

	// A capped coupon is claimed the same way stock is: atomically, before the order exists, so concurrent
	// checkouts can never together exceed its redemption cap.
	couponCode := ""
	if cart.Coupon != nil {
		couponCode = cart.Coupon.Code
	}
	if couponCode != "" {
		ok, err := o.coupons.TryClaim(ctx, couponCode)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, apperr.WithFields(apperr.Validation, "This coupon has reached its usage limit. Please remove it to continue.", map[string]string{"code": "Usage limit reached"})
		}
	}
	// Stock is claimed before the order exists, so two shoppers can never both get the last unit.
	stockLines := make([]StockLine, len(cart.Lines))
	var itemCount int64
	for i, l := range cart.Lines {
		stockLines[i] = StockLine{VariantID: l.VariantID, Quantity: l.Quantity}
		itemCount += l.Quantity
	}
	if err := o.inventory.Take(ctx, stockLines); err != nil {
		if couponCode != "" {
			o.coupons.Release(ctx, couponCode)
		}
		return nil, err
	}

	now := time.Now().UTC()
	id := newOrderID()
	timeline := []domain.TimelineEntry{{Status: "placed", Label: "Order placed", At: domain.ISO(now)}}
	status, payStatus := "pending_payment", "pending"
	var deadline *time.Time
	if cod {
		status, payStatus = "confirmed", "cod"
		timeline = append(timeline, domain.TimelineEntry{Status: "confirmed", Label: "Order confirmed", At: domain.ISO(now)})
	} else {
		d := now.Add(PaymentWindowMinutes * time.Minute)
		deadline = &d
	}
	row := &db.Order{
		ID: id, Status: status, PaymentStatus: payStatus, PaymentMethod: req.PaymentMethod,
		Lines: mustJSON(cart.Lines), Totals: mustJSON(cart.Totals), ShippingMethod: string(cart.ShippingMethod),
		ContactName: strings.TrimSpace(req.Contact.Name), ContactEmail: strings.TrimSpace(req.Contact.Email), ContactPhone: req.Contact.Phone,
		AddressLine1: strings.TrimSpace(req.Address.Line1), AddressCity: strings.TrimSpace(req.Address.City), AddressState: strings.TrimSpace(req.Address.State), AddressPincode: req.Address.Pincode,
		Timeline: mustJSON(timeline), Notes: []byte("[]"), IdempotencyKey: req.IdempotencyKey, PaymentDeadline: deadline,
	}
	if owner.UserID != "" {
		row.UserID = strPtr(owner.UserID)
	} else if owner.GuestToken != "" {
		row.GuestToken = strPtr(owner.GuestToken)
	}
	if couponCode != "" {
		row.CouponCode = strPtr(couponCode)
	}
	if l2 := strings.TrimSpace(req.Address.Line2); l2 != "" {
		row.AddressLine2 = strPtr(l2)
	}

	txErr := o.store.Tx(ctx, func(tx pgx.Tx) error {
		if err := db.InsertOrder(ctx, tx, row); err != nil {
			return err
		}
		// The order-confirmation email rides the same transaction as the order row itself.
		return db.WriteOutbox(ctx, tx, "order.placed", map[string]any{
			"orderId": id, "email": strings.TrimSpace(req.Contact.Email), "name": strings.TrimSpace(req.Contact.Name),
			"total": cart.Totals.Total, "itemCount": itemCount, "cod": cod,
		})
	})
	if txErr != nil {
		// Unique idempotencyKey race: a concurrent request with the same key won first. Give the stock and the
		// coupon claim back, and return that order instead of creating a duplicate.
		raced, _ := db.OrderByIdempotencyKey(ctx, o.store.Pool, req.IdempotencyKey)
		_ = o.inventory.GiveBack(ctx, stockLines)
		if couponCode != "" {
			o.coupons.Release(ctx, couponCode)
		}
		if raced != nil {
			out := ToOrder(raced)
			return &out, nil
		}
		return nil, apperr.New(apperr.Conflict, "Could not place the order. Please try again.")
	}
	if cod {
		_ = o.cart.ReplaceWithEmpty(ctx, ownerKey, cart.ShippingMethod)
	}
	placed, err := db.OrderByID(ctx, o.store.Pool, id)
	if err != nil || placed == nil {
		return nil, err
	}
	out := ToOrder(placed)
	o.events.Publish(ctx, out)
	return &out, nil
}

// Get returns one of the owner's orders.
func (o *Orders) Get(ctx context.Context, id string, owner Owner) (*domain.Order, error) {
	if err := o.SweepExpired(ctx); err != nil {
		return nil, err
	}
	row, err := db.OrderByID(ctx, o.store.Pool, id)
	if err != nil {
		return nil, err
	}
	if row == nil || !matchesOwner(row, owner) {
		return nil, apperr.New(apperr.NotFound, "Order not found")
	}
	out := ToOrder(row)
	return &out, nil
}

// List returns the owner's orders, newest first.
func (o *Orders) List(ctx context.Context, owner Owner) ([]domain.Order, error) {
	if err := o.SweepExpired(ctx); err != nil {
		return nil, err
	}
	var rows []db.Order
	var err error
	if owner.UserID != "" {
		rows, err = db.OrdersForUser(ctx, o.store.Pool, owner.UserID)
	} else {
		if owner.GuestToken == "" {
			return []domain.Order{}, nil // no cookie, so no orders "placed on this device"
		}
		rows, err = db.OrdersForGuest(ctx, o.store.Pool, owner.GuestToken)
	}
	if err != nil {
		return nil, err
	}
	out := make([]domain.Order, len(rows))
	for i := range rows {
		out[i] = ToOrder(&rows[i])
	}
	return out, nil
}

// RowForOwner is the raw row for the payment module, ownership-checked the same way.
func (o *Orders) RowForOwner(ctx context.Context, id string, owner Owner) (*db.Order, error) {
	row, err := db.OrderByID(ctx, o.store.Pool, id)
	if err != nil {
		return nil, err
	}
	if row == nil || !matchesOwner(row, owner) {
		return nil, apperr.New(apperr.NotFound, "Order not found")
	}
	return row, nil
}

// Cancel cancels one of the owner's orders (not once shipped), restoring stock.
func (o *Orders) Cancel(ctx context.Context, id string, owner Owner) (*domain.Order, error) {
	if err := o.SweepExpired(ctx); err != nil {
		return nil, err
	}
	row, err := db.OrderByID(ctx, o.store.Pool, id)
	if err != nil {
		return nil, err
	}
	if row == nil || !matchesOwner(row, owner) {
		return nil, apperr.New(apperr.NotFound, "Order not found")
	}
	if row.Status == "cancelled" {
		out := ToOrder(row)
		return &out, nil
	}
	if row.Status == "shipped" || row.Status == "delivered" {
		return nil, apperr.New(apperr.Validation, "This order has already shipped and can no longer be cancelled.")
	}
	_ = o.inventory.GiveBack(ctx, StockLinesOf(row))
	if row.CouponCode != nil {
		o.coupons.Release(ctx, *row.CouponCode)
	}
	kept := []domain.TimelineEntry{}
	for _, t := range timelineOf(row) {
		if t.Status == "placed" || t.Status == "paid" || t.Status == "confirmed" {
			kept = append(kept, t)
		}
	}
	kept = append(kept, domain.TimelineEntry{Status: "cancelled", Label: "Order cancelled", At: domain.ISO(time.Now())})
	payStatus := row.PaymentStatus
	if payStatus == "paid" {
		payStatus = "refund_pending"
	}
	var updated *db.Order
	err = o.store.Tx(ctx, func(tx pgx.Tx) error {
		var e error
		updated, e = db.UpdateOrder(ctx, tx, id, db.OrderUpdate{Status: strPtr("cancelled"), PaymentStatus: &payStatus, Timeline: mustJSON(kept)})
		if e != nil {
			return e
		}
		return db.WriteOutbox(ctx, tx, "order.cancelled", map[string]any{"orderId": id, "email": row.ContactEmail, "name": row.ContactName})
	})
	if err != nil {
		return nil, err
	}
	out := ToOrder(updated)
	o.events.Publish(ctx, out)
	return &out, nil
}
