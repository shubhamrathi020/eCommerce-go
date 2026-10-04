package domain

import (
	"math"
	"sort"
	"strings"
	"time"
)

// LowStockThreshold: stock at or below this many units (across variants) is shown as "Only N left".
const LowStockThreshold = 5

func totalStock(p *Product) int64 {
	var sum int64
	for _, v := range p.Variants {
		sum += v.Stock
	}
	return sum
}

// StockStatusOf is a product's availability and, when low, how many units are left.
func StockStatusOf(p *Product) (StockStatus, *int64) {
	stock := totalStock(p)
	if stock == 0 {
		for _, v := range p.Variants {
			if v.Backorder != nil {
				return BackOrder, nil
			}
		}
		return OutOfStock, nil
	}
	if stock <= LowStockThreshold {
		return LowStock, &stock
	}
	return InStock, nil
}

// CheapestVariant is the variant with the lowest price (the first one wins a tie).
func CheapestVariant(p *Product) *Variant {
	best := &p.Variants[0]
	for i := range p.Variants {
		if p.Variants[i].Price.Amount < best.Price.Amount {
			best = &p.Variants[i]
		}
	}
	return best
}

func variantDiscount(v *Variant) int {
	if v.MRP == nil || v.MRP.Amount <= v.Price.Amount {
		return 0
	}
	return int(jsRound(float64(v.MRP.Amount-v.Price.Amount) / float64(v.MRP.Amount) * 100))
}

// BestDiscount is the biggest percentage discount across a product's variants.
func BestDiscount(p *Product) int {
	best := 0
	for i := range p.Variants {
		if d := variantDiscount(&p.Variants[i]); d > best {
			best = d
		}
	}
	return best
}

// ToSummary turns a product into the lightweight card shape.
func ToSummary(p *Product) ProductSummary {
	cheapest := CheapestVariant(p)
	minPrice, maxPrice := p.Variants[0].Price.Amount, p.Variants[0].Price.Amount
	for _, v := range p.Variants {
		if v.Price.Amount < minPrice {
			minPrice = v.Price.Amount
		}
		if v.Price.Amount > maxPrice {
			maxPrice = v.Price.Amount
		}
	}
	status, left := StockStatusOf(p)
	var buyable []*Variant
	for i := range p.Variants {
		if p.Variants[i].Stock > 0 || p.Variants[i].Backorder != nil {
			buyable = append(buyable, &p.Variants[i])
		}
	}
	s := ProductSummary{
		ID:           p.ID,
		Slug:         p.Slug,
		Title:        p.Title,
		BrandName:    p.BrandName,
		CategoryName: p.CategoryPath[len(p.CategoryPath)-1].Name,
		PriceMin:     INRInt(minPrice),
		PriceMax:     INRInt(maxPrice),
		Rating:       SummaryRate{Average: p.Rating.Average, Count: p.Rating.Count},
		StockStatus:  status,
		VariantCount: len(p.Variants),
	}
	if len(p.Images) > 0 {
		s.Image = p.Images[0]
	}
	if len(p.Images) > 1 {
		hover := p.Images[1]
		s.HoverImage = &hover
	}
	if variantDiscount(cheapest) >= 1 && cheapest.MRP != nil {
		mrp := *cheapest.MRP
		s.MRPMin = &mrp
	}
	s.StockLeft = left
	if len(p.Variants) == 1 && len(buyable) == 1 {
		s.QuickAddVariantID = buyable[0].ID
	}
	return s
}

// SortProducts orders products for a listing. Out-of-stock products rank last; ties break on id so order is stable.
func SortProducts(products []Product, key SortKey, scores map[string]float64) []Product {
	out := append([]Product(nil), products...)
	cmp := func(a, b *Product) float64 {
		switch key {
		case "featured":
			return b.Popularity - a.Popularity
		case "price-asc":
			return float64(CheapestVariant(a).Price.Amount - CheapestVariant(b).Price.Amount)
		case "price-desc":
			return float64(CheapestVariant(b).Price.Amount - CheapestVariant(a).Price.Amount)
		case "newest":
			return float64(strings.Compare(b.CreatedAt, a.CreatedAt))
		case "rating":
			if d := b.Rating.Average - a.Rating.Average; d != 0 {
				return d
			}
			return float64(b.Rating.Count - a.Rating.Count)
		case "discount":
			return float64(BestDiscount(b) - BestDiscount(a))
		default: // relevance
			if d := scores[b.ID] - scores[a.ID]; d != 0 {
				return d
			}
			return b.Popularity - a.Popularity
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := &out[i], &out[j]
		aOut, bOut := 0, 0
		if totalStock(a) == 0 {
			aOut = 1
		}
		if totalStock(b) == 0 {
			bOut = 1
		}
		if aOut != bOut {
			return aOut < bOut
		}
		if c := cmp(a, b); c != 0 {
			return c < 0
		}
		return strings.Compare(a.ID, b.ID) < 0
	})
	return out
}

const dayMS = 86_400_000

// ComputeServiceability applies the deterministic delivery rules by pin-code prefix.
func ComputeServiceability(pincode string, now time.Time) Serviceability {
	first := int(pincode[0] - '0')
	if first == 9 {
		return Serviceability{Serviceable: false, Pincode: pincode}
	}
	days := 2 + (first % 4)
	cod := first%2 == 0
	date := now.Add(time.Duration(days) * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	return Serviceability{Serviceable: true, Pincode: pincode, EstimatedDays: &days, EstimatedDate: date, CODAvailable: &cod}
}

// ISO formats a time the way JavaScript's Date#toISOString does.
func ISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

var _ = math.MaxInt64
var _ = dayMS
