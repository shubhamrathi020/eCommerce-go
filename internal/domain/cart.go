package domain

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// ShippingMethodID is "standard" or "express".
type ShippingMethodID string

// PaymentMethod is "razorpay" or "cod".
type PaymentMethod string

// CartLine is one priced line of a cart.
type CartLine struct {
	ProductID         string            `json:"productId"`
	VariantID         string            `json:"variantId"`
	Slug              string            `json:"slug"`
	Title             string            `json:"title"`
	BrandName         string            `json:"brandName"`
	Image             ImageRef          `json:"image"`
	Options           map[string]string `json:"options"`
	UnitPrice         Money             `json:"unitPrice"`
	MRP               *Money            `json:"mrp,omitempty"`
	Quantity          int64             `json:"quantity"`
	MaxQuantity       int64             `json:"maxQuantity"`
	LineTotal         Money             `json:"lineTotal"`
	TaxIncluded       Money             `json:"taxIncluded"`
	Issue             string            `json:"issue,omitempty"`
	Backorder         *Backorder        `json:"backorder,omitempty"`
	PreviousUnitPrice *Money            `json:"previousUnitPrice,omitempty"`
}

// AppliedCoupon is the coupon on a cart.
type AppliedCoupon struct {
	Code         string `json:"code"`
	Description  string `json:"description"`
	Discount     Money  `json:"discount"`
	FreeShipping bool   `json:"freeShipping"`
}

// CartTotals are the money totals of a cart.
type CartTotals struct {
	ItemCount            int64  `json:"itemCount"`
	Subtotal             Money  `json:"subtotal"`
	MRPSavings           Money  `json:"mrpSavings"`
	CouponDiscount       Money  `json:"couponDiscount"`
	Shipping             Money  `json:"shipping"`
	TaxIncluded          Money  `json:"taxIncluded"`
	Total                Money  `json:"total"`
	AmountToFreeShipping *Money `json:"amountToFreeShipping,omitempty"`
}

// Cart is a fully priced cart.
type Cart struct {
	Lines          []CartLine       `json:"lines"`
	Coupon         *AppliedCoupon   `json:"coupon,omitempty"`
	ShippingMethod ShippingMethodID `json:"shippingMethod"`
	Totals         CartTotals       `json:"totals"`
	Notices        []string         `json:"notices"`
	Blocked        bool             `json:"blocked"`
}

// ShippingOption is a delivery choice at checkout.
type ShippingOption struct {
	ID            ShippingMethodID `json:"id"`
	Label         string           `json:"label"`
	Price         Money            `json:"price"`
	EstimatedDays int              `json:"estimatedDays"`
	EstimatedDate string           `json:"estimatedDate"`
}

// PaymentOption is a payment choice at checkout.
type PaymentOption struct {
	Method  PaymentMethod `json:"method"`
	Label   string        `json:"label"`
	Enabled bool          `json:"enabled"`
	Reason  string        `json:"reason,omitempty"`
}

// StoredItem is one persisted cart item: the facts a cart needs before pricing.
type StoredItem struct {
	VariantID string `json:"variantId"`
	Quantity  int64  `json:"quantity"`
	SeenPrice int64  `json:"seenPrice"`
}

// StoredCart is the persisted shape; a real backend stores the same facts server-side.
type StoredCart struct {
	Items          []StoredItem     `json:"items"`
	CouponCode     string           `json:"couponCode,omitempty"`
	ShippingMethod ShippingMethodID `json:"shippingMethod"`
}

// EmptyStoredCart is a new empty cart.
func EmptyStoredCart() StoredCart {
	return StoredCart{Items: []StoredItem{}, ShippingMethod: "standard"}
}

const (
	MaxLineQuantity       = 10
	FreeShippingThreshold = 49_900
	StandardShipping      = 4_900
	ExpressShipping       = 9_900
	CODMaxTotal           = 500_000
)

// Coupon is a discount code.
type Coupon struct {
	Code        string
	Description string
	Kind        string // percent | flat | free_shipping
	Value       int64
	MinSubtotal int64
	MaxDiscount int64 // 0 = no cap
	ExpiresAt   string
}

// Coupons is the list of coupons the shop knows.
var Coupons = []Coupon{
	{Code: "WELCOME10", Description: "10% off up to ₹500", Kind: "percent", Value: 10, MinSubtotal: 99_900, MaxDiscount: 50_000},
	{Code: "FLAT100", Description: "₹100 off", Kind: "flat", Value: 10_000, MinSubtotal: 69_900},
	{Code: "FREESHIP", Description: "Free shipping", Kind: "free_shipping"},
	{Code: "EXPIRED50", Description: "50% off", Kind: "percent", Value: 50, ExpiresAt: "2025-12-31T23:59:59Z"},
}

// gstByRoot: GST rates by top-level category id; prices already include GST.
var gstByRoot = map[string]float64{"cat-grocery": 0.05, "cat-books": 0, "cat-fashion": 0.12}

const defaultGST = 0.18

func gstRateFor(p *Product) float64 {
	if r, ok := gstByRoot[p.CategoryPath[0].ID]; ok {
		return r
	}
	return defaultGST
}

// CouponResult is the outcome of checking a coupon against a subtotal.
type CouponResult struct {
	OK       bool
	Coupon   *Coupon
	Discount int64
	Message  string
}

// EvaluateCoupon checks a code against the subtotal at a moment in time.
func EvaluateCoupon(code string, subtotal int64, now time.Time) CouponResult {
	want := strings.ToUpper(strings.TrimSpace(code))
	var coupon *Coupon
	for i := range Coupons {
		if Coupons[i].Code == want {
			coupon = &Coupons[i]
			break
		}
	}
	if coupon == nil {
		return CouponResult{Message: "This coupon code is not valid."}
	}
	if coupon.ExpiresAt != "" {
		if exp, err := time.Parse(time.RFC3339, coupon.ExpiresAt); err == nil && exp.Before(now) {
			return CouponResult{Message: "This coupon has expired."}
		}
	}
	if subtotal < coupon.MinSubtotal {
		return CouponResult{Message: fmt.Sprintf("Add %s more to use this coupon.", FormatMoney(INRInt(coupon.MinSubtotal-subtotal)))}
	}
	var discount int64
	switch coupon.Kind {
	case "percent":
		discount = subtotal * coupon.Value / 100
		if coupon.MaxDiscount > 0 && discount > coupon.MaxDiscount {
			discount = coupon.MaxDiscount
		}
	case "flat":
		discount = coupon.Value
		if subtotal < discount {
			discount = subtotal
		}
	}
	return CouponResult{OK: true, Coupon: coupon, Discount: discount}
}

// ShippingFee is the delivery charge for a method after discounts.
func ShippingFee(method ShippingMethodID, amountAfterDiscount int64, freeShippingCoupon bool) int64 {
	if amountAfterDiscount <= 0 || freeShippingCoupon {
		return 0
	}
	if method == "express" {
		return ExpressShipping
	}
	if amountAfterDiscount >= FreeShippingThreshold {
		return 0
	}
	return StandardShipping
}

// PricedCart is a priced cart plus the stored cart after clamping.
type PricedCart struct {
	Cart   Cart
	Stored StoredCart
}

// PriceCart recomputes a full cart from the catalog. Pure: all money logic lives here, never in the client.
func PriceCart(stored StoredCart, products []Product, now time.Time) PricedCart {
	notices := []string{}
	lines := []CartLine{}
	keptItems := []StoredItem{}

	variantOf := func(variantID string) (*Product, *Variant) {
		for i := range products {
			for j := range products[i].Variants {
				if products[i].Variants[j].ID == variantID {
					return &products[i], &products[i].Variants[j]
				}
			}
		}
		return nil, nil
	}

	for _, item := range stored.Items {
		product, variant := variantOf(item.VariantID)
		if product == nil {
			notices = append(notices, "An item in your cart is no longer available and was removed.")
			continue
		}
		max := int64(MaxLineQuantity)
		if variant.Backorder == nil && variant.Stock < max {
			max = variant.Stock
		}
		quantity := item.Quantity
		limit := max
		if limit < 1 {
			limit = 1
		}
		if quantity > limit {
			quantity = limit
		}
		issue := ""
		var previous *Money

		if variant.Stock == 0 && variant.Backorder == nil {
			issue = "out_of_stock"
			quantity = item.Quantity
			notices = append(notices, fmt.Sprintf("%s is out of stock. Remove it to continue.", product.Title))
		} else if quantity < item.Quantity {
			issue = "quantity_reduced"
			notices = append(notices, fmt.Sprintf("Only %d of %s available. Quantity updated.", variant.Stock, product.Title))
		}
		if issue != "out_of_stock" && item.SeenPrice != variant.Price.Amount {
			if issue == "" {
				issue = "price_changed"
			}
			p := INRInt(item.SeenPrice)
			previous = &p
			notices = append(notices, fmt.Sprintf("Price of %s updated from %s to %s.", product.Title, FormatMoney(INRInt(item.SeenPrice)), FormatMoney(variant.Price)))
		}

		lineTotal := variant.Price.Amount * quantity
		rate := gstRateFor(product)
		image := ImageRef{}
		if len(variant.Images) > 0 {
			image = variant.Images[0]
		} else if len(product.Images) > 0 {
			image = product.Images[0]
		}
		opts := variant.Options
		if opts == nil {
			opts = map[string]string{}
		}
		line := CartLine{
			ProductID: product.ID, VariantID: variant.ID, Slug: product.Slug, Title: product.Title, BrandName: product.BrandName,
			Image: image, Options: opts, UnitPrice: variant.Price, Quantity: quantity, MaxQuantity: max,
			LineTotal:   INRInt(lineTotal),
			TaxIncluded: INR(float64(lineTotal) - float64(lineTotal)/(1+rate)),
		}
		if variant.MRP != nil && variant.MRP.Amount > variant.Price.Amount {
			mrp := *variant.MRP
			line.MRP = &mrp
		}
		if variant.Backorder != nil && quantity > variant.Stock {
			b := *variant.Backorder
			line.Backorder = &b
		}
		if issue != "" {
			line.Issue = issue
		}
		if previous != nil {
			line.PreviousUnitPrice = previous
		}
		lines = append(lines, line)
		kept := item
		kept.Quantity = quantity
		keptItems = append(keptItems, kept)
	}

	var subtotal, mrpSavings int64
	var tax float64
	var itemCount int64
	blocked := false
	for _, l := range lines {
		if l.Issue == "out_of_stock" {
			blocked = true
			continue
		}
		subtotal += l.LineTotal.Amount
		if l.MRP != nil {
			mrpSavings += (l.MRP.Amount - l.UnitPrice.Amount) * l.Quantity
		}
		tax += float64(l.TaxIncluded.Amount)
		itemCount += l.Quantity
	}

	couponCode := stored.CouponCode
	var coupon *AppliedCoupon
	var discount int64
	if couponCode != "" {
		result := EvaluateCoupon(couponCode, subtotal, now)
		if result.OK {
			discount = result.Discount
			coupon = &AppliedCoupon{Code: result.Coupon.Code, Description: result.Coupon.Description, Discount: INRInt(discount), FreeShipping: result.Coupon.Kind == "free_shipping"}
		} else {
			notices = append(notices, fmt.Sprintf("Coupon %s was removed. %s", couponCode, result.Message))
			couponCode = ""
		}
	}

	afterDiscount := subtotal - discount
	freeShip := coupon != nil && coupon.FreeShipping
	shipping := ShippingFee(stored.ShippingMethod, afterDiscount, freeShip)
	total := afterDiscount + shipping
	var toFree int64
	if stored.ShippingMethod == "standard" && !freeShip && afterDiscount > 0 && afterDiscount < FreeShippingThreshold {
		toFree = FreeShippingThreshold - afterDiscount
	}

	var taxShare float64
	if subtotal > 0 {
		taxShare = tax * float64(afterDiscount) / float64(subtotal)
	}
	totals := CartTotals{
		ItemCount: itemCount, Subtotal: INRInt(subtotal), MRPSavings: INRInt(mrpSavings), CouponDiscount: INRInt(discount),
		Shipping: INRInt(shipping), TaxIncluded: INR(taxShare), Total: INRInt(total),
	}
	if toFree > 0 {
		m := INRInt(toFree)
		totals.AmountToFreeShipping = &m
	}
	cart := Cart{Lines: lines, ShippingMethod: stored.ShippingMethod, Totals: totals, Notices: uniqueStrings(notices), Blocked: blocked, Coupon: coupon}

	out := StoredCart{Items: keptItems, ShippingMethod: stored.ShippingMethod, CouponCode: couponCode}
	return PricedCart{Cart: cart, Stored: out}
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

var pincodeRE = regexp.MustCompile(`^[1-9][0-9]{5}$`)

// DeliverabilityProblem says why a pin code cannot be delivered to: "invalid", "not_deliverable" or "" (it can).
func DeliverabilityProblem(pincode string, serviceable func(string) bool) string {
	if !pincodeRE.MatchString(pincode) {
		return "invalid"
	}
	if !serviceable(pincode) {
		return "not_deliverable"
	}
	return ""
}

// CODEligibility says whether cash on delivery is offered and, if not, why.
func CODEligibility(pincode string, total int64) (bool, string) {
	if total > CODMaxTotal {
		return false, "Cash on delivery is available for orders up to ₹5,000."
	}
	if strings.HasPrefix(pincode, "7") || strings.HasPrefix(pincode, "8") {
		return false, "Cash on delivery is not available for this pin code."
	}
	return true, ""
}

var _ = math.Pi
