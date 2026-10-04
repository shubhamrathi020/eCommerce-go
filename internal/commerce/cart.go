// Package commerce is the shopping flow: server-side carts, stock holds, orders, payments and back-office orders.
package commerce

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/catalog"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// GuestCookie identifies an anonymous shopper's cart across requests: not a secret, just an opaque per-browser id,
// so unlike the refresh-token cookie it is long-lived.
const GuestCookie = "gcid"

const oneYear = 365 * 24 * time.Hour

// GuestToken returns the guest cart token from the request, "" when there is none.
func GuestToken(r *http.Request) string { return httpx.Cookie(r, GuestCookie) }

// EnsureGuestToken returns the guest cart token, creating and setting a fresh cookie the first time one is needed.
func EnsureGuestToken(w http.ResponseWriter, r *http.Request, secure bool) string {
	if t := GuestToken(r); t != "" {
		return t
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	httpx.SetCookie(w, GuestCookie, token, "/", secure, time.Time{}, oneYear)
	return token
}

// OwnerKey is the Cart/Order row key for whoever is making this request.
func OwnerKey(userID, guestToken string) string {
	if userID != "" {
		return "user:" + userID
	}
	return "guest:" + guestToken
}

// Cart is the server-side cart: one row per owner, priced fresh from the real catalog on every read. The client
// never computes money.
type Cart struct {
	store *db.Store
	mongo *catalog.Mongo
}

// NewCart builds the cart service.
func NewCart(store *db.Store, mongo *catalog.Mongo) *Cart { return &Cart{store: store, mongo: mongo} }

// productsFor loads the published products that contain any of the given variants: the only ones pricing needs.
func (c *Cart) productsFor(ctx context.Context, variantIDs []string) ([]domain.Product, error) {
	if len(variantIDs) == 0 {
		return []domain.Product{}, nil
	}
	var docs []domain.ProductDoc
	cur, err := c.mongo.Products().Find(ctx, bson.M{"status": "published", "variants.id": bson.M{"$in": variantIDs}})
	if err != nil {
		return nil, err
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]domain.Product, len(docs))
	for i := range docs {
		out[i] = catalog.Normalize(docs[i].Product)
	}
	return out, nil
}

func (c *Cart) readRow(ctx context.Context, ownerKey string) (domain.StoredCart, error) {
	row, err := db.CartByOwner(ctx, c.store.Pool, ownerKey)
	if err != nil {
		return domain.StoredCart{}, err
	}
	if row == nil {
		return domain.EmptyStoredCart(), nil
	}
	stored := domain.StoredCart{Items: []domain.StoredItem{}, ShippingMethod: domain.ShippingMethodID(row.ShippingMethod)}
	if err := json.Unmarshal(row.Items, &stored.Items); err != nil {
		return domain.StoredCart{}, err
	}
	if row.CouponCode != nil {
		stored.CouponCode = *row.CouponCode
	}
	return stored, nil
}

func (c *Cart) writeRow(ctx context.Context, ownerKey string, stored domain.StoredCart, resetReminder bool) error {
	items := stored.Items
	if items == nil {
		items = []domain.StoredItem{}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	var coupon *string
	if stored.CouponCode != "" {
		coupon = &stored.CouponCode
	}
	return db.UpsertCart(ctx, c.store.Pool, ownerKey, raw, coupon, string(stored.ShippingMethod), resetReminder)
}

func variantIDsOf(stored domain.StoredCart, extra ...string) []string {
	ids := []string{}
	for _, i := range stored.Items {
		ids = append(ids, i.VariantID)
	}
	return append(ids, extra...)
}

func (c *Cart) priced(ctx context.Context, ownerKey string) (domain.PricedCart, error) {
	stored, err := c.readRow(ctx, ownerKey)
	if err != nil {
		return domain.PricedCart{}, err
	}
	products, err := c.productsFor(ctx, variantIDsOf(stored))
	if err != nil {
		return domain.PricedCart{}, err
	}
	result := domain.PriceCart(stored, products, time.Now())
	if err := c.writeRow(ctx, ownerKey, result.Stored, false); err != nil {
		return domain.PricedCart{}, err
	}
	return result, nil
}

// mutate applies change, then re-prices (acknowledging current prices). A real content change means the cart is
// being actively used, so any pending abandoned-cart reminder flag is cleared.
func (c *Cart) mutate(ctx context.Context, ownerKey string, extraVariant string, change func(domain.StoredCart, []domain.Product) (domain.StoredCart, error)) (domain.PricedCart, error) {
	stored, err := c.readRow(ctx, ownerKey)
	if err != nil {
		return domain.PricedCart{}, err
	}
	products, err := c.productsFor(ctx, variantIDsOf(stored, extraVariant))
	if err != nil {
		return domain.PricedCart{}, err
	}
	next, err := change(stored, products)
	if err != nil {
		return domain.PricedCart{}, err
	}
	ack := next
	ack.Items = make([]domain.StoredItem, len(next.Items))
	for i, item := range next.Items {
		ack.Items[i] = item
		if _, v := findVariant(products, item.VariantID); v != nil {
			ack.Items[i].SeenPrice = v.Price.Amount
		}
	}
	result := domain.PriceCart(ack, products, time.Now())
	if err := c.writeRow(ctx, ownerKey, result.Stored, true); err != nil {
		return domain.PricedCart{}, err
	}
	return result, nil
}

func findVariant(products []domain.Product, id string) (*domain.Product, *domain.Variant) {
	for i := range products {
		for j := range products[i].Variants {
			if products[i].Variants[j].ID == id {
				return &products[i], &products[i].Variants[j]
			}
		}
	}
	return nil, nil
}

// Get is the priced cart.
func (c *Cart) Get(ctx context.Context, ownerKey string) (*domain.Cart, error) {
	r, err := c.priced(ctx, ownerKey)
	if err != nil {
		return nil, err
	}
	return &r.Cart, nil
}

// Add puts a variant in the cart.
func (c *Cart) Add(ctx context.Context, ownerKey, variantID string, quantity int64) (*domain.Cart, error) {
	r, err := c.mutate(ctx, ownerKey, variantID, func(stored domain.StoredCart, products []domain.Product) (domain.StoredCart, error) {
		_, variant := findVariant(products, variantID)
		if variant == nil {
			return stored, apperr.New(apperr.NotFound, "This product is no longer available.")
		}
		if variant.Stock == 0 && variant.Backorder == nil {
			return stored, apperr.New(apperr.Validation, "Sorry, this item is out of stock.")
		}
		var existing *domain.StoredItem
		for i := range stored.Items {
			if stored.Items[i].VariantID == variantID {
				existing = &stored.Items[i]
			}
		}
		max := int64(domain.MaxLineQuantity)
		if variant.Backorder == nil && variant.Stock < max {
			max = variant.Stock
		}
		if existing != nil && existing.Quantity >= max {
			return stored, apperr.New(apperr.Validation, fmt.Sprintf("You already have the maximum quantity (%d) of this item.", max))
		}
		items := make([]domain.StoredItem, 0, len(stored.Items)+1)
		if existing != nil {
			for _, i := range stored.Items {
				if i.VariantID == variantID {
					i.Quantity = min(max, i.Quantity+quantity)
				}
				items = append(items, i)
			}
		} else {
			items = append(items, stored.Items...)
			items = append(items, domain.StoredItem{VariantID: variantID, Quantity: min(max, max64(1, quantity)), SeenPrice: variant.Price.Amount})
		}
		stored.Items = items
		return stored, nil
	})
	if err != nil {
		return nil, err
	}
	return &r.Cart, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// SetQuantity changes a line's quantity; zero or less removes it.
func (c *Cart) SetQuantity(ctx context.Context, ownerKey, variantID string, quantity int64) (*domain.Cart, error) {
	r, err := c.mutate(ctx, ownerKey, "", func(stored domain.StoredCart, _ []domain.Product) (domain.StoredCart, error) {
		items := []domain.StoredItem{}
		for _, i := range stored.Items {
			if i.VariantID == variantID {
				if quantity <= 0 {
					continue
				}
				i.Quantity = min(quantity, domain.MaxLineQuantity)
			}
			items = append(items, i)
		}
		stored.Items = items
		return stored, nil
	})
	if err != nil {
		return nil, err
	}
	return &r.Cart, nil
}

// Remove drops a line.
func (c *Cart) Remove(ctx context.Context, ownerKey, variantID string) (*domain.Cart, error) {
	return c.SetQuantity(ctx, ownerKey, variantID, 0)
}

// ApplyCoupon validates and applies a coupon code.
func (c *Cart) ApplyCoupon(ctx context.Context, ownerKey, code string) (*domain.Cart, error) {
	current, err := c.priced(ctx, ownerKey)
	if err != nil {
		return nil, err
	}
	evaluated := domain.EvaluateCoupon(code, current.Cart.Totals.Subtotal.Amount, time.Now())
	if !evaluated.OK {
		return nil, apperr.WithFields(apperr.Validation, evaluated.Message, map[string]string{"code": evaluated.Message})
	}
	r, err := c.mutate(ctx, ownerKey, "", func(stored domain.StoredCart, _ []domain.Product) (domain.StoredCart, error) {
		stored.CouponCode = evaluated.Coupon.Code
		return stored, nil
	})
	if err != nil {
		return nil, err
	}
	return &r.Cart, nil
}

// RemoveCoupon takes the coupon off.
func (c *Cart) RemoveCoupon(ctx context.Context, ownerKey string) (*domain.Cart, error) {
	r, err := c.mutate(ctx, ownerKey, "", func(stored domain.StoredCart, _ []domain.Product) (domain.StoredCart, error) {
		stored.CouponCode = ""
		return stored, nil
	})
	if err != nil {
		return nil, err
	}
	return &r.Cart, nil
}

// SetShippingMethod picks standard or express delivery.
func (c *Cart) SetShippingMethod(ctx context.Context, ownerKey string, method domain.ShippingMethodID) (*domain.Cart, error) {
	r, err := c.mutate(ctx, ownerKey, "", func(stored domain.StoredCart, _ []domain.Product) (domain.StoredCart, error) {
		stored.ShippingMethod = method
		return stored, nil
	})
	if err != nil {
		return nil, err
	}
	return &r.Cart, nil
}

// Clear empties the cart (and drops its coupon).
func (c *Cart) Clear(ctx context.Context, ownerKey string) (*domain.Cart, error) {
	r, err := c.mutate(ctx, ownerKey, "", func(stored domain.StoredCart, _ []domain.Product) (domain.StoredCart, error) {
		return domain.StoredCart{Items: []domain.StoredItem{}, ShippingMethod: stored.ShippingMethod}, nil
	})
	if err != nil {
		return nil, err
	}
	return &r.Cart, nil
}

// MergeGuestIntoUser runs right after a successful login or register: the guest cart's items move into the
// account's cart (quantities add up, capped) and the guest cart is removed.
func (c *Cart) MergeGuestIntoUser(ctx context.Context, guestOwnerKey, userID string) error {
	if guestOwnerKey == "" {
		return nil
	}
	guest, err := c.readRow(ctx, guestOwnerKey)
	if err != nil || len(guest.Items) == 0 {
		return err
	}
	userKey := "user:" + userID
	mine, err := c.readRow(ctx, userKey)
	if err != nil {
		return err
	}
	items := append([]domain.StoredItem{}, mine.Items...)
	for _, g := range guest.Items {
		found := false
		for i := range items {
			if items[i].VariantID == g.VariantID {
				items[i].Quantity = min(domain.MaxLineQuantity, items[i].Quantity+g.Quantity)
				found = true
			}
		}
		if !found {
			items = append(items, g)
		}
	}
	mine.Items = items
	if err := c.writeRow(ctx, userKey, mine, false); err != nil {
		return err
	}
	_ = db.DeleteCart(ctx, c.store.Pool, guestOwnerKey)
	return nil
}

// ReplaceWithEmpty empties a cart outright (after an order is placed or paid).
func (c *Cart) ReplaceWithEmpty(ctx context.Context, ownerKey string, shipping domain.ShippingMethodID) error {
	return c.writeRow(ctx, ownerKey, domain.StoredCart{Items: []domain.StoredItem{}, ShippingMethod: shipping}, false)
}
