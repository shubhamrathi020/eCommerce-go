package domain

// ImageRef is one product or banner image.
type ImageRef struct {
	URL    string `json:"url"`
	Alt    string `json:"alt"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// Brand is a product brand.
type Brand struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Collection groups products carrying a tag.
type Collection struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Tag         string `json:"tag"`
}

// Backorder is present when a variant can be bought at zero stock.
type Backorder struct {
	ExpectedDate string `json:"expectedDate,omitempty"`
}

// Variant is one purchasable option combination of a product.
type Variant struct {
	ID        string            `json:"id"`
	SKU       string            `json:"sku"`
	Options   map[string]string `json:"options"`
	Price     Money             `json:"price"`
	MRP       *Money            `json:"mrp,omitempty"`
	Stock     int64             `json:"stock"`
	Backorder *Backorder        `json:"backorder,omitempty"`
	Images    []ImageRef        `json:"images,omitempty"`
}

// RatingSummary is a product's review summary.
type RatingSummary struct {
	Average      float64  `json:"average"`
	Count        int64    `json:"count"`
	Distribution [5]int64 `json:"distribution"`
}

// CategoryRef is a breadcrumb entry.
type CategoryRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// SEO holds optional search-engine overrides.
type SEO struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// Product is the full catalog product shown to shoppers.
type Product struct {
	ID           string         `json:"id"`
	Slug         string         `json:"slug"`
	Title        string         `json:"title"`
	BrandID      string         `json:"brandId"`
	BrandName    string         `json:"brandName"`
	CategoryID   string         `json:"categoryId"`
	CategoryPath []CategoryRef  `json:"categoryPath"`
	Description  string         `json:"description"`
	Highlights   []string       `json:"highlights"`
	Images       []ImageRef     `json:"images"`
	Attributes   map[string]any `json:"attributes"`
	VariantAxes  []string       `json:"variantAxes"`
	Variants     []Variant      `json:"variants"`
	Rating       RatingSummary  `json:"rating"`
	Tags         []string       `json:"tags"`
	CreatedAt    string         `json:"createdAt"`
	Popularity   float64        `json:"popularity"`
	SlugHistory  []string       `json:"slugHistory,omitempty"`
	SellerID     string         `json:"sellerId,omitempty"`
	SEO          *SEO           `json:"seo,omitempty"`
}

// ProductDoc is the MongoDB document: a Product plus admin-only bookkeeping that never reaches a shopper.
type ProductDoc struct {
	DocID     string `json:"_id"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updatedAt"`
	Product   `json:",inline"`
}

// StockStatus is a product's availability.
type StockStatus string

const (
	InStock    StockStatus = "in_stock"
	LowStock   StockStatus = "low_stock"
	OutOfStock StockStatus = "out_of_stock"
	BackOrder  StockStatus = "backorder"
)

// ProductSummary is the lightweight shape for cards and lists.
type ProductSummary struct {
	ID                string      `json:"id"`
	Slug              string      `json:"slug"`
	Title             string      `json:"title"`
	BrandName         string      `json:"brandName"`
	CategoryName      string      `json:"categoryName"`
	Image             ImageRef    `json:"image"`
	HoverImage        *ImageRef   `json:"hoverImage,omitempty"`
	PriceMin          Money       `json:"priceMin"`
	PriceMax          Money       `json:"priceMax"`
	MRPMin            *Money      `json:"mrpMin,omitempty"`
	Rating            SummaryRate `json:"rating"`
	StockStatus       StockStatus `json:"stockStatus"`
	StockLeft         *int64      `json:"stockLeft,omitempty"`
	VariantCount      int         `json:"variantCount"`
	QuickAddVariantID string      `json:"quickAddVariantId,omitempty"`
}

// SummaryRate is the compact rating on a card.
type SummaryRate struct {
	Average float64 `json:"average"`
	Count   int64   `json:"count"`
}

// AttributeDef describes a category attribute.
type AttributeDef struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Unit        string   `json:"unit,omitempty"`
	Filterable  bool     `json:"filterable"`
	VariantAxis bool     `json:"variantAxis"`
	Values      []string `json:"values,omitempty"`
}

// Category is a catalog category.
type Category struct {
	ID            string         `json:"id"`
	Slug          string         `json:"slug"`
	Name          string         `json:"name"`
	ParentID      string         `json:"parentId,omitempty"`
	ImageURL      string         `json:"imageUrl,omitempty"`
	Order         int            `json:"order"`
	AttributeDefs []AttributeDef `json:"attributeDefs,omitempty"`
}

// CategoryNode is a category with resolved children.
type CategoryNode struct {
	Category `json:",inline"`
	Children []*CategoryNode `json:"children"`
}

// CategoryDoc is the MongoDB document for a category.
type CategoryDoc struct {
	DocID    string `json:"_id"`
	Category `json:",inline"`
}

// BrandDoc is the MongoDB document for a brand.
type BrandDoc struct {
	DocID string `json:"_id"`
	Brand `json:",inline"`
}

// CollectionDoc is the MongoDB document for a collection.
type CollectionDoc struct {
	DocID      string `json:"_id"`
	Collection `json:",inline"`
}

// Review is a product review.
type Review struct {
	ID        string `json:"id"`
	ProductID string `json:"productId"`
	Author    string `json:"author"`
	Rating    int    `json:"rating"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	Verified  bool   `json:"verified"`
	Helpful   int    `json:"helpful"`
	Status    string `json:"status,omitempty"`
	Mine      bool   `json:"mine,omitempty"`
	Voted     bool   `json:"voted,omitempty"`
}

// ReviewDoc is the MongoDB document for a review.
type ReviewDoc struct {
	DocID  string `json:"_id"`
	Review `json:",inline"`
}

// ReviewPage is one page of reviews with the product's rating summary.
type ReviewPage struct {
	Items   []Review      `json:"items"`
	Total   int           `json:"total"`
	Summary RatingSummary `json:"summary"`
}

// Serviceability says whether and how fast an area can be delivered to.
type Serviceability struct {
	Serviceable   bool   `json:"serviceable"`
	Pincode       string `json:"pincode"`
	EstimatedDays *int   `json:"estimatedDays,omitempty"`
	EstimatedDate string `json:"estimatedDate,omitempty"`
	CODAvailable  *bool  `json:"codAvailable,omitempty"`
}

// FacetOption is one selectable facet value.
type FacetOption struct {
	Value    string `json:"value"`
	Label    string `json:"label"`
	Count    int    `json:"count"`
	Selected bool   `json:"selected"`
}

// Facet is a group of filter options.
type Facet struct {
	Key     string        `json:"key"`
	Label   string        `json:"label"`
	Options []FacetOption `json:"options"`
}

// PriceBounds are the min and max price across a listing.
type PriceBounds struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// ListingResult is one page of a category, brand, collection or search listing.
type ListingResult struct {
	Items         []ProductSummary `json:"items"`
	Total         int              `json:"total"`
	Page          int              `json:"page"`
	PageSize      int              `json:"pageSize"`
	Facets        []Facet          `json:"facets"`
	PriceBounds   PriceBounds      `json:"priceBounds"`
	Title         string           `json:"title"`
	Breadcrumb    []CategoryRef    `json:"breadcrumb"`
	CorrectedFrom string           `json:"correctedFrom,omitempty"`
}

// SortKey orders a listing.
type SortKey string

// ListingQuery is a parsed listing request.
type ListingQuery struct {
	CategorySlug   string
	BrandSlug      string
	CollectionSlug string
	Q              string
	Filters        map[string][]string
	PriceMin       *int64
	PriceMax       *int64
	Sort           SortKey
	Page           int
	PageSize       int
}

// SearchSuggestions feed the search box dropdown.
type SearchSuggestions struct {
	Queries    []string         `json:"queries"`
	Products   []ProductSummary `json:"products"`
	Categories []NameSlug       `json:"categories"`
	Brands     []NameSlug       `json:"brands"`
}

// NameSlug is a category or brand suggestion.
type NameSlug struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// HomeData is the storefront home page content. Banners and tiles are stored and passed through untouched.
type HomeData struct {
	Banners       []any        `json:"banners"`
	CategoryTiles []any        `json:"categoryTiles"`
	Deals         HomeDeals    `json:"deals"`
	Rows          []ProductRow `json:"rows"`
	Brands        []Brand      `json:"brands"`
}

// HomeDeals is the "deals of the day" strip.
type HomeDeals struct {
	EndsAt string           `json:"endsAt"`
	Items  []ProductSummary `json:"items"`
}

// ProductRow is one named row of products on the home page.
type ProductRow struct {
	Key   string           `json:"key"`
	Title string           `json:"title"`
	Link  string           `json:"link,omitempty"`
	Items []ProductSummary `json:"items"`
}
