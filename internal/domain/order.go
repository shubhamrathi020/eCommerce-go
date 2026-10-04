package domain

// OrderStatus is where an order is in its life.
type OrderStatus string

// PaymentStatus is where an order's payment is.
type PaymentStatus string

// ContactDetails is who an order is for.
type ContactDetails struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone"`
}

// Address is a delivery address.
type Address struct {
	Line1   string `json:"line1"`
	Line2   string `json:"line2,omitempty"`
	City    string `json:"city"`
	State   string `json:"state"`
	Pincode string `json:"pincode"`
}

// TimelineEntry is one step in an order's history.
type TimelineEntry struct {
	Status string `json:"status"`
	Label  string `json:"label"`
	At     string `json:"at,omitempty"`
}

// Order is an order as shoppers see it.
type Order struct {
	ID             string           `json:"id"`
	Status         OrderStatus      `json:"status"`
	PaymentStatus  PaymentStatus    `json:"paymentStatus"`
	PaymentMethod  PaymentMethod    `json:"paymentMethod"`
	Lines          []CartLine       `json:"lines"`
	Totals         CartTotals       `json:"totals"`
	CouponCode     string           `json:"couponCode,omitempty"`
	ShippingMethod ShippingMethodID `json:"shippingMethod"`
	Contact        ContactDetails   `json:"contact"`
	Address        Address          `json:"address"`
	Timeline       []TimelineEntry  `json:"timeline"`
	CreatedAt      string           `json:"createdAt"`
	UserID         string           `json:"userId,omitempty"`
}

// OrderNote is a staff or system note on an order.
type OrderNote struct {
	ID     string `json:"id"`
	At     string `json:"at"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

// AdminOrderDetail is an order with its staff-only fields.
type AdminOrderDetail struct {
	Order
	Notes       []OrderNote   `json:"notes"`
	AllowedNext []OrderStatus `json:"allowedNext"`
}

// AdminOrderRow is one line of the admin order list.
type AdminOrderRow struct {
	ID            string        `json:"id"`
	CreatedAt     string        `json:"createdAt"`
	CustomerName  string        `json:"customerName"`
	ItemCount     int64         `json:"itemCount"`
	Total         Money         `json:"total"`
	Status        OrderStatus   `json:"status"`
	PaymentStatus PaymentStatus `json:"paymentStatus"`
	PaymentMethod PaymentMethod `json:"paymentMethod"`
}

// PaymentSession is the data the payment provider window needs.
type PaymentSession struct {
	OrderID         string `json:"orderId"`
	ProviderOrderID string `json:"providerOrderId"`
	KeyID           string `json:"keyId"`
	Amount          int64  `json:"amount"`
	Currency        string `json:"currency"`
}

// OrderTransitions is the allowed order state machine: the one place this is defined.
var OrderTransitions = map[OrderStatus][]OrderStatus{
	"pending_payment": {"cancelled"},
	"confirmed":       {"packed", "cancelled"},
	"packed":          {"shipped", "cancelled"},
	"shipped":         {"delivered"},
	"delivered":       {},
	"cancelled":       {},
}

// CanTransition reports whether an order may move from one status to another.
func CanTransition(from, to OrderStatus) bool {
	for _, s := range OrderTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}
