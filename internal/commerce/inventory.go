package commerce

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/cache"
	"github.com/shubhamrathi020/eCommerce-go/internal/catalog"
)

// StockLine is a quantity of one variant.
type StockLine struct {
	VariantID string `json:"variantId"`
	Quantity  int64  `json:"quantity"`
}

// Inventory moves stock atomically on the catalog store: a variant's `stock` field in Mongo is the one source of
// truth. Each line is decremented with a conditional update that MongoDB guarantees is atomic per document, so two
// concurrent shoppers can never both take the last unit. Several lines in one order are not atomic as a group
// (standalone MongoDB has no multi-document transactions); if a later line fails the earlier ones are rolled
// back with compensating increments, so the process never oversells.
type Inventory struct {
	mongo *catalog.Mongo
	cache *cache.Cache
}

// NewInventory builds the stock service.
func NewInventory(m *catalog.Mongo, c *cache.Cache) *Inventory { return &Inventory{mongo: m, cache: c} }

func (i *Inventory) takeOne(ctx context.Context, line StockLine) error {
	res, err := i.mongo.Products().UpdateOne(ctx,
		bson.M{"variants": bson.M{"$elemMatch": bson.M{"id": line.VariantID, "stock": bson.M{"$gte": line.Quantity}}}},
		bson.M{"$inc": bson.M{"variants.$.stock": -line.Quantity}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return apperr.New(apperr.Validation, "Some items in your cart are no longer available.")
	}
	i.invalidate(ctx, line.VariantID)
	return nil
}

func (i *Inventory) giveBackOne(ctx context.Context, line StockLine) error {
	_, err := i.mongo.Products().UpdateOne(ctx, bson.M{"variants.id": line.VariantID}, bson.M{"$inc": bson.M{"variants.$.stock": line.Quantity}})
	if err != nil {
		return err
	}
	i.invalidate(ctx, line.VariantID)
	return nil
}

// invalidate: an order moving stock is exactly as much a catalog write as an admin edit is, so the cached product
// page and every cached listing must not show a stale stock status.
func (i *Inventory) invalidate(ctx context.Context, variantID string) {
	i.cache.InvalidateTag(ctx, "variant:"+variantID)
	i.cache.InvalidateTag(ctx, "catalog:listings")
}

// Take decrements every line, or none: it rolls back whatever already succeeded if a later line fails.
func (i *Inventory) Take(ctx context.Context, lines []StockLine) error {
	taken := []StockLine{}
	for _, line := range lines {
		if err := i.takeOne(ctx, line); err != nil {
			for _, t := range taken {
				_ = i.giveBackOne(ctx, t)
			}
			return err
		}
		taken = append(taken, line)
	}
	return nil
}

// GiveBack returns stock, e.g. on cancellation or a payment-window timeout.
func (i *Inventory) GiveBack(ctx context.Context, lines []StockLine) error {
	for _, line := range lines {
		if err := i.giveBackOne(ctx, line); err != nil {
			return err
		}
	}
	return nil
}
