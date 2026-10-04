package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/cache"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
)

var (
	ratingSteps   = []int{4, 3, 2, 1}
	discountSteps = []int{10, 25, 40}
)

// listingsTag is carried by every home/listing cache entry, so any catalog write can invalidate all of them at once.
const listingsTag = "catalog:listings"

// Service is the shopper-facing catalog.
type Service struct {
	mongo  *Mongo
	search *Search
	cache  *cache.Cache
}

// NewService builds the catalog service.
func NewService(m *Mongo, s *Search, c *cache.Cache) *Service {
	return &Service{mongo: m, search: s, cache: c}
}

// jsonStr quotes a string for a Meilisearch filter exactly like JSON.stringify does.
func jsonStr(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

func jsonList(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = jsonStr(v)
	}
	return strings.Join(parts, ",")
}

// stableString is deterministic JSON: object keys sorted recursively, so two logically identical queries
// produce the same cache key whatever order their fields were built in.
func stableString(v any) string {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = jsonStr(k) + ":" + stableString(x[k])
		}
		return "{" + strings.Join(parts, ",") + "}"
	case map[string][]string:
		m := map[string]any{}
		for k, vv := range x {
			m[k] = vv
		}
		return stableString(m)
	case []string:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = jsonStr(s)
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// localeCompare approximates String#localeCompare: alphabetical ignoring case first, lower case before upper.
func localeCompare(a, b string) int {
	if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return -strings.Compare(a, b)
}

func leafIDsUnder(category *domain.Category, categories []domain.Category) map[string]bool {
	ids := map[string]bool{}
	for _, c := range categories {
		if c.ParentID == category.ID {
			ids[c.ID] = true
		}
	}
	if len(ids) == 0 {
		ids[category.ID] = true
	}
	return ids
}

func (s *Service) allCategories(ctx context.Context) ([]domain.Category, error) {
	var docs []domain.CategoryDoc
	cur, err := s.mongo.Categories().Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]domain.Category, len(docs))
	for i := range docs {
		out[i] = docs[i].Category
	}
	return out, nil
}

// publishedProducts is every published product (admin bookkeeping fields stripped).
func (s *Service) publishedProducts(ctx context.Context) ([]domain.Product, error) {
	return s.productsWhere(ctx, bson.M{"status": "published"})
}

func (s *Service) productsWhere(ctx context.Context, filter any) ([]domain.Product, error) {
	var docs []domain.ProductDoc
	cur, err := s.mongo.Products().Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]domain.Product, len(docs))
	for i := range docs {
		out[i] = Normalize(docs[i].Product)
	}
	return out, nil
}

// Normalize makes sure arrays and maps that the contract says are always present are never null.
func Normalize(p domain.Product) domain.Product {
	if p.CategoryPath == nil {
		p.CategoryPath = []domain.CategoryRef{}
	}
	if p.Highlights == nil {
		p.Highlights = []string{}
	}
	if p.Images == nil {
		p.Images = []domain.ImageRef{}
	}
	if p.Attributes == nil {
		p.Attributes = map[string]any{}
	}
	if p.VariantAxes == nil {
		p.VariantAxes = []string{}
	}
	if p.Tags == nil {
		p.Tags = []string{}
	}
	for i := range p.Variants {
		if p.Variants[i].Options == nil {
			p.Variants[i].Options = map[string]string{}
		}
	}
	return p
}

// Home is the storefront home page.
func (s *Service) Home(ctx context.Context) (*domain.HomeData, error) {
	h, err := cache.GetOrSet(ctx, s.cache, "catalog:home", 60*time.Second, func(*domain.HomeData) []string { return []string{listingsTag} },
		func() (*domain.HomeData, error) { return s.homeUncached(ctx) })
	return h, err
}

func (s *Service) homeUncached(ctx context.Context) (*domain.HomeData, error) {
	products, err := s.publishedProducts(ctx)
	if err != nil {
		return nil, err
	}
	var brandDocs []domain.BrandDoc
	cur, err := s.mongo.Brands().Find(ctx, bson.M{}, optionsSortLimit("name", 12))
	if err != nil {
		return nil, err
	}
	if err := cur.All(ctx, &brandDocs); err != nil {
		return nil, err
	}
	var homeDoc struct {
		Banners       []any `json:"banners"`
		CategoryTiles []any `json:"categoryTiles"`
	}
	_ = s.mongo.Home().FindOne(ctx, bson.M{"_id": "home"}).Decode(&homeDoc)

	inStock := []domain.Product{}
	for i := range products {
		if st, _ := domain.StockStatusOf(&products[i]); st != domain.OutOfStock {
			inStock = append(inStock, products[i])
		}
	}
	summaries := func(list []domain.Product, n int) []domain.ProductSummary {
		out := []domain.ProductSummary{}
		for i := range list {
			if i >= n {
				break
			}
			out = append(out, domain.ToSummary(&list[i]))
		}
		return out
	}
	endOfDay := time.Now().UTC()
	endOfDay = time.Date(endOfDay.Year(), endOfDay.Month(), endOfDay.Day(), 23, 59, 59, 0, time.UTC)
	deals := append([]domain.Product(nil), inStock...)
	sort.SliceStable(deals, func(i, j int) bool { return domain.BestDiscount(&deals[j]) < domain.BestDiscount(&deals[i]) })
	var rated []domain.Product
	for _, p := range inStock {
		if p.Rating.Count >= 5 {
			rated = append(rated, p)
		}
	}
	brands := make([]domain.Brand, len(brandDocs))
	for i := range brandDocs {
		brands[i] = brandDocs[i].Brand
	}
	banners, tiles := homeDoc.Banners, homeDoc.CategoryTiles
	if banners == nil {
		banners = []any{}
	}
	if tiles == nil {
		tiles = []any{}
	}
	return &domain.HomeData{
		Banners:       banners,
		CategoryTiles: tiles,
		Deals:         domain.HomeDeals{EndsAt: domain.ISO(endOfDay), Items: summaries(deals, 10)},
		Rows: []domain.ProductRow{
			{Key: "featured", Title: "Featured for you", Items: summaries(domain.SortProducts(inStock, "featured", nil), 12)},
			{Key: "new", Title: "New arrivals", Link: "/collections/trending", Items: summaries(domain.SortProducts(inStock, "newest", nil), 12)},
			{Key: "best", Title: "Top rated", Items: summaries(domain.SortProducts(rated, "rating", nil), 12)},
		},
		Brands: brands,
	}, nil
}

// Listing is a category, brand, collection or search results page.
func (s *Service) Listing(ctx context.Context, q domain.ListingQuery) (*domain.ListingResult, error) {
	filters := map[string]any{}
	for k, v := range q.Filters {
		filters[k] = v
	}
	canon := map[string]any{"filters": filters, "sort": string(q.Sort), "page": q.Page, "pageSize": q.PageSize}
	if q.CategorySlug != "" {
		canon["categorySlug"] = q.CategorySlug
	}
	if q.BrandSlug != "" {
		canon["brandSlug"] = q.BrandSlug
	}
	if q.CollectionSlug != "" {
		canon["collectionSlug"] = q.CollectionSlug
	}
	if q.Q != "" {
		canon["q"] = q.Q
	}
	if q.PriceMin != nil {
		canon["priceMin"] = *q.PriceMin
	}
	if q.PriceMax != nil {
		canon["priceMax"] = *q.PriceMax
	}
	sum := sha256.Sum256([]byte(stableString(canon)))
	key := "catalog:listing:" + hex.EncodeToString(sum[:])
	return cache.GetOrSet(ctx, s.cache, key, 30*time.Second, func(*domain.ListingResult) []string { return []string{listingsTag} },
		func() (*domain.ListingResult, error) { return s.listingUncached(ctx, q) })
}

type facetGroup struct {
	key       string
	field     string
	threshold bool
	op        string
	build     func([]string) string
}

func (s *Service) listingUncached(ctx context.Context, q domain.ListingQuery) (*domain.ListingResult, error) {
	categories, err := s.allCategories(ctx)
	if err != nil {
		return nil, err
	}
	scope := []string{}
	heading := "All products"
	breadcrumb := []domain.CategoryRef{}
	var attributeDefs []domain.AttributeDef

	if q.CategorySlug != "" {
		var category *domain.Category
		for i := range categories {
			if categories[i].Slug == q.CategorySlug {
				category = &categories[i]
				break
			}
		}
		if category == nil {
			return nil, apperr.New(apperr.NotFound, "Category not found")
		}
		ids := []string{}
		for id := range leafIDsUnder(category, categories) {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		scope = append(scope, "categoryId IN ["+jsonList(ids)+"]")
		heading = category.Name
		if category.ParentID != "" {
			for i := range categories {
				if categories[i].ID == category.ParentID {
					breadcrumb = append(breadcrumb, domain.CategoryRef{ID: categories[i].ID, Slug: categories[i].Slug, Name: categories[i].Name})
				}
			}
		}
		breadcrumb = append(breadcrumb, domain.CategoryRef{ID: category.ID, Slug: category.Slug, Name: category.Name})
		for _, d := range category.AttributeDefs {
			if d.Filterable {
				attributeDefs = append(attributeDefs, d)
			}
		}
	}
	if q.BrandSlug != "" {
		var brand domain.BrandDoc
		if err := s.mongo.Brands().FindOne(ctx, bson.M{"slug": q.BrandSlug}).Decode(&brand); err != nil {
			return nil, apperr.New(apperr.NotFound, "Brand not found")
		}
		scope = append(scope, "brandSlug = "+jsonStr(q.BrandSlug))
		heading = brand.Name
	}
	if q.CollectionSlug != "" {
		var collection domain.CollectionDoc
		if err := s.mongo.Collections().FindOne(ctx, bson.M{"slug": q.CollectionSlug}).Decode(&collection); err != nil {
			return nil, apperr.New(apperr.NotFound, "Collection not found")
		}
		scope = append(scope, "tags = "+jsonStr(collection.Tag))
		heading = collection.Name
	}

	sel := q.Filters
	categorical := func(field string) func([]string) string {
		return func(v []string) string { return field + " IN [" + jsonList(v) + "]" }
	}
	groups := []facetGroup{
		{key: "brand", field: "brandSlug", build: categorical("brandSlug")},
		{key: "rating", field: "ratingAverage", threshold: true, op: ">="},
		{key: "discount", field: "bestDiscount", threshold: true, op: ">="},
		{key: "availability", field: "totalStock", threshold: true, op: ">="},
	}
	for _, d := range attributeDefs {
		groups = append(groups, facetGroup{key: d.Key, field: "attr_" + d.Key, build: categorical("attr_" + d.Key)})
	}

	facetFilters := []FacetClause{}
	for _, g := range groups {
		chosen := sel[g.key]
		if len(chosen) == 0 {
			continue
		}
		if !g.threshold {
			facetFilters = append(facetFilters, FacetClause{Filter: g.build(chosen), Field: g.field})
		} else {
			min := math.Inf(1)
			for _, c := range chosen {
				n, err := strconv.ParseFloat(strings.TrimSpace(c), 64)
				if err != nil {
					n = math.NaN()
				}
				if math.IsNaN(n) || n < min {
					min = n
					if math.IsNaN(n) {
						break
					}
				}
			}
			facetFilters = append(facetFilters, FacetClause{Filter: fmt.Sprintf("%s %s %s", g.field, g.op, formatNum(min)), Field: g.field})
		}
	}
	priceFilter := ""
	if q.PriceMin != nil || q.PriceMax != nil {
		lo, hi := int64(0), int64(9007199254740991)
		if q.PriceMin != nil {
			lo = *q.PriceMin
		}
		if q.PriceMax != nil {
			hi = *q.PriceMax
		}
		priceFilter = fmt.Sprintf("priceMin <= %d AND priceMax >= %d", hi, lo)
	}

	facetFields := []string{"brandSlug"}
	for _, d := range attributeDefs {
		facetFields = append(facetFields, "attr_"+d.Key)
	}
	sortKey := q.Sort
	if strings.TrimSpace(q.Q) != "" {
		sortKey = "relevance"
	}
	result, err := s.search.Search(ctx, SearchQuery{Q: q.Q, Scope: scope, FacetFilters: facetFilters, FacetFields: facetFields, PriceFilter: priceFilter, Sort: sortKey, Page: q.Page, PageSize: q.PageSize})
	if err != nil {
		return nil, err
	}
	title := heading
	if t := strings.TrimSpace(q.Q); t != "" {
		title = `Results for "` + t + `"`
	}

	exceptField := func(exclude string) []string {
		out := append([]string{}, scope...)
		for _, f := range facetFilters {
			if f.Field != exclude {
				out = append(out, f.Filter)
			}
		}
		if priceFilter != "" {
			out = append(out, priceFilter)
		}
		return out
	}

	// Brand facet.
	brandCounts := result.FacetDistribution["brandSlug"]
	slugs := make([]string, 0, len(brandCounts))
	for slug := range brandCounts {
		slugs = append(slugs, slug)
	}
	var brandDocs []domain.BrandDoc
	if len(slugs) > 0 {
		cur, err := s.mongo.Brands().Find(ctx, bson.M{"slug": bson.M{"$in": slugs}})
		if err != nil {
			return nil, err
		}
		if err := cur.All(ctx, &brandDocs); err != nil {
			return nil, err
		}
	}
	brandName := func(slug string) string {
		for _, b := range brandDocs {
			if b.Slug == slug {
				return b.Name
			}
		}
		return slug
	}
	brandOpts := []domain.FacetOption{}
	for slug, count := range brandCounts {
		brandOpts = append(brandOpts, domain.FacetOption{Value: slug, Label: brandName(slug), Count: count, Selected: contains(sel["brand"], slug)})
	}
	sort.SliceStable(brandOpts, func(i, j int) bool {
		if brandOpts[i].Count != brandOpts[j].Count {
			return brandOpts[i].Count > brandOpts[j].Count
		}
		return localeCompare(brandOpts[i].Label, brandOpts[j].Label) < 0
	})
	facets := []domain.Facet{{Key: "brand", Label: "Brand", Options: brandOpts}}

	for _, def := range attributeDefs {
		counts := result.FacetDistribution["attr_"+def.Key]
		opts := []domain.FacetOption{}
		anySelected := false
		for value, count := range counts {
			selected := contains(sel[def.Key], value)
			anySelected = anySelected || selected
			opts = append(opts, domain.FacetOption{Value: value, Label: value, Count: count, Selected: selected})
		}
		sort.SliceStable(opts, func(i, j int) bool {
			if len(def.Values) > 0 {
				return indexOf(def.Values, opts[i].Value) < indexOf(def.Values, opts[j].Value)
			}
			return localeCompare(opts[i].Label, opts[j].Label) < 0
		})
		if len(opts) > 1 || anySelected {
			facets = append(facets, domain.Facet{Key: def.Key, Label: def.Label, Options: opts})
		}
	}

	// Threshold facets: one exact count query per step, ignoring that group's own filter.
	thresholdCounts := func(field string, steps []int) ([]int, error) {
		base := exceptField(field)
		counts := make([]int, len(steps))
		var wg sync.WaitGroup
		var mu sync.Mutex
		var firstErr error
		for i, n := range steps {
			wg.Add(1)
			go func(i, n int) {
				defer wg.Done()
				c, err := s.search.Count(ctx, append(append([]string{}, base...), fmt.Sprintf("%s >= %d", field, n)))
				mu.Lock()
				defer mu.Unlock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				counts[i] = c
			}(i, n)
		}
		wg.Wait()
		return counts, firstErr
	}
	ratingCounts, err := thresholdCounts("ratingAverage", ratingSteps)
	if err != nil {
		return nil, err
	}
	ratingOpts := []domain.FacetOption{}
	for i, n := range ratingSteps {
		ratingOpts = append(ratingOpts, domain.FacetOption{Value: strconv.Itoa(n), Label: fmt.Sprintf("%d stars & up", n), Count: ratingCounts[i], Selected: contains(sel["rating"], strconv.Itoa(n))})
	}
	facets = append(facets, domain.Facet{Key: "rating", Label: "Customer rating", Options: ratingOpts})
	discountCounts, err := thresholdCounts("bestDiscount", discountSteps)
	if err != nil {
		return nil, err
	}
	discountOpts := []domain.FacetOption{}
	for i, n := range discountSteps {
		discountOpts = append(discountOpts, domain.FacetOption{Value: strconv.Itoa(n), Label: fmt.Sprintf("%d%% or more", n), Count: discountCounts[i], Selected: contains(sel["discount"], strconv.Itoa(n))})
	}
	facets = append(facets, domain.Facet{Key: "discount", Label: "Discount", Options: discountOpts})
	inStockCount, err := s.search.Count(ctx, append(exceptField("totalStock"), "totalStock >= 1"))
	if err != nil {
		return nil, err
	}
	facets = append(facets, domain.Facet{Key: "availability", Label: "Availability", Options: []domain.FacetOption{{Value: "in_stock", Label: "In stock only", Count: inStockCount, Selected: contains(sel["availability"], "in_stock")}}})

	pageSize := max(1, q.PageSize)
	pages := max(1, int(math.Ceil(float64(result.Total)/float64(pageSize))))
	page := min(max(1, q.Page), pages)

	items := []domain.ProductSummary{}
	if len(result.IDs) > 0 {
		products, err := s.productsWhere(ctx, bson.M{"id": bson.M{"$in": result.IDs}})
		if err != nil {
			return nil, err
		}
		byID := map[string]*domain.Product{}
		for i := range products {
			byID[products[i].ID] = &products[i]
		}
		for _, id := range result.IDs { // Meilisearch already returned ids in the right order; Mongo's $in does not preserve it.
			if p, ok := byID[id]; ok {
				items = append(items, domain.ToSummary(p))
			}
		}
	}
	return &domain.ListingResult{Items: items, Total: result.Total, Page: page, PageSize: pageSize, Facets: facets, PriceBounds: result.PriceBounds, Title: title, Breadcrumb: breadcrumb}, nil
}

func formatNum(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

// ProductResult is a product page's data.
type ProductResult struct {
	Product        domain.Product `json:"product"`
	RedirectedFrom string         `json:"redirectedFrom,omitempty"`
}

// Product is one product by slug, following an old slug if it moved.
func (s *Service) Product(ctx context.Context, slug string) (*ProductResult, error) {
	return cache.GetOrSet(ctx, s.cache, "catalog:product:"+slug, 120*time.Second,
		func(r *ProductResult) []string {
			tags := []string{listingsTag, "product:" + r.Product.ID}
			for _, v := range r.Product.Variants {
				tags = append(tags, "variant:"+v.ID)
			}
			return tags
		},
		func() (*ProductResult, error) { return s.productUncached(ctx, slug) })
}

func (s *Service) productUncached(ctx context.Context, slug string) (*ProductResult, error) {
	var doc domain.ProductDoc
	if err := s.mongo.Products().FindOne(ctx, bson.M{"slug": slug, "status": "published"}).Decode(&doc); err == nil {
		return &ProductResult{Product: Normalize(doc.Product)}, nil
	}
	if err := s.mongo.Products().FindOne(ctx, bson.M{"slugHistory": slug, "status": "published"}).Decode(&doc); err == nil {
		return &ProductResult{Product: Normalize(doc.Product), RedirectedFrom: slug}, nil
	}
	return nil, apperr.New(apperr.NotFound, "Product not found")
}

// ProductsByIDs returns published products in the requested order, skipping unknown ids.
func (s *Service) ProductsByIDs(ctx context.Context, ids []string) ([]domain.Product, error) {
	if len(ids) == 0 {
		return []domain.Product{}, nil
	}
	products, err := s.productsWhere(ctx, bson.M{"id": bson.M{"$in": ids}, "status": "published"})
	if err != nil {
		return nil, err
	}
	byID := map[string]domain.Product{}
	for _, p := range products {
		byID[p.ID] = p
	}
	out := []domain.Product{}
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// SummariesByIDs is ProductsByIDs as cards.
func (s *Service) SummariesByIDs(ctx context.Context, ids []string) ([]domain.ProductSummary, error) {
	products, err := s.ProductsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ProductSummary, len(products))
	for i := range products {
		out[i] = domain.ToSummary(&products[i])
	}
	return out, nil
}

func (s *Service) publishedByID(ctx context.Context, id string) (*domain.Product, error) {
	var doc domain.ProductDoc
	if err := s.mongo.Products().FindOne(ctx, bson.M{"id": id, "status": "published"}).Decode(&doc); err != nil {
		return nil, nil
	}
	p := Normalize(doc.Product)
	return &p, nil
}

// Related is up to eight similar products: the same leaf category first, then the same root category.
func (s *Service) Related(ctx context.Context, productID string) ([]domain.ProductSummary, error) {
	product, err := s.publishedByID(ctx, productID)
	if err != nil || product == nil {
		return []domain.ProductSummary{}, err
	}
	rootID := product.CategoryPath[0].ID
	candidates, err := s.productsWhere(ctx, bson.M{"id": bson.M{"$ne": productID}, "status": "published", "$or": bson.A{bson.M{"categoryId": product.CategoryID}, bson.M{"categoryPath.0.id": rootID}}})
	if err != nil {
		return nil, err
	}
	var sameLeaf, sameRoot []domain.Product
	for _, p := range candidates {
		if p.CategoryID == product.CategoryID {
			sameLeaf = append(sameLeaf, p)
		} else {
			sameRoot = append(sameRoot, p)
		}
	}
	if len(sameRoot) > 4 {
		sameRoot = sameRoot[:4]
	}
	sorted := domain.SortProducts(append(sameLeaf, sameRoot...), "featured", nil)
	out := []domain.ProductSummary{}
	for i := range sorted {
		if i >= 8 {
			break
		}
		out = append(out, domain.ToSummary(&sorted[i]))
	}
	return out, nil
}

// BoughtTogether is up to two in-stock products from the same root category but a different leaf.
func (s *Service) BoughtTogether(ctx context.Context, productID string) ([]domain.ProductSummary, error) {
	product, err := s.publishedByID(ctx, productID)
	if err != nil || product == nil {
		return []domain.ProductSummary{}, err
	}
	rootID := product.CategoryPath[0].ID
	others, err := s.productsWhere(ctx, bson.M{"categoryPath.0.id": rootID, "categoryId": bson.M{"$ne": product.CategoryID}, "status": "published"})
	if err != nil {
		return nil, err
	}
	inStock := []domain.Product{}
	for i := range others {
		if st, _ := domain.StockStatusOf(&others[i]); st != domain.OutOfStock {
			inStock = append(inStock, others[i])
		}
	}
	sorted := domain.SortProducts(inStock, "featured", nil)
	out := []domain.ProductSummary{}
	for i := range sorted {
		if i >= 2 {
			break
		}
		out = append(out, domain.ToSummary(&sorted[i]))
	}
	return out, nil
}

// Reviews is one sorted page of a product's reviews.
func (s *Service) Reviews(ctx context.Context, productID, sortKey string, page, pageSize int) (*domain.ReviewPage, error) {
	product, err := s.publishedByID(ctx, productID)
	if err != nil {
		return nil, err
	}
	if product == nil {
		return nil, apperr.New(apperr.NotFound, "Product not found")
	}
	var docs []domain.ReviewDoc
	cur, err := s.mongo.Reviews().Find(ctx, bson.M{"productId": productID})
	if err != nil {
		return nil, err
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	all := make([]domain.Review, len(docs))
	for i := range docs {
		all[i] = docs[i].Review
	}
	less := map[string]func(a, b domain.Review) int{
		"recent":  func(a, b domain.Review) int { return strings.Compare(b.CreatedAt, a.CreatedAt) },
		"helpful": func(a, b domain.Review) int { return b.Helpful - a.Helpful },
		"high":    func(a, b domain.Review) int { return b.Rating - a.Rating },
		"low":     func(a, b domain.Review) int { return a.Rating - b.Rating },
	}[sortKey]
	sort.SliceStable(all, func(i, j int) bool { return less(all[i], all[j]) < 0 })
	start := (max(1, page) - 1) * pageSize
	items := []domain.Review{}
	if start < len(all) {
		end := min(start+pageSize, len(all))
		items = all[start:end]
	}
	return &domain.ReviewPage{Items: items, Total: len(all), Summary: product.Rating}, nil
}

var pincodeOK = func(p string) bool {
	if len(p) != 6 || p[0] < '1' || p[0] > '9' {
		return false
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Serviceability says whether a pin code can be delivered to.
func (s *Service) Serviceability(pincode string) (*domain.Serviceability, error) {
	if !pincodeOK(pincode) {
		return nil, apperr.WithFields(apperr.Validation, "Enter a valid 6-digit pin code", map[string]string{"pincode": "Invalid pin code"})
	}
	r := domain.ComputeServiceability(pincode, time.Now())
	return &r, nil
}

// CategoryTree is the nested, ordered category tree.
func (s *Service) CategoryTree(ctx context.Context) ([]*domain.CategoryNode, error) {
	return cache.GetOrSet(ctx, s.cache, "catalog:category-tree", 300*time.Second, func([]*domain.CategoryNode) []string { return []string{listingsTag, "catalog:categories"} },
		func() ([]*domain.CategoryNode, error) { return s.categoryTreeUncached(ctx) })
}

func (s *Service) categoryTreeUncached(ctx context.Context) ([]*domain.CategoryNode, error) {
	flat, err := s.allCategories(ctx)
	if err != nil {
		return nil, err
	}
	nodes := map[string]*domain.CategoryNode{}
	order := []string{}
	for _, c := range flat {
		nodes[c.ID] = &domain.CategoryNode{Category: c, Children: []*domain.CategoryNode{}}
		order = append(order, c.ID)
	}
	roots := []*domain.CategoryNode{}
	for _, id := range order {
		n := nodes[id]
		if parent, ok := nodes[n.ParentID]; ok && n.ParentID != "" {
			parent.Children = append(parent.Children, n)
		} else {
			roots = append(roots, n)
		}
	}
	var sortNodes func([]*domain.CategoryNode)
	sortNodes = func(list []*domain.CategoryNode) {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Order < list[j].Order })
		for _, n := range list {
			sortNodes(n.Children)
		}
	}
	sortNodes(roots)
	return roots, nil
}
