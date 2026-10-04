package catalog

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

var sortKeys = []string{"relevance", "featured", "price-asc", "price-desc", "newest", "rating", "discount"}
var reviewSorts = []string{"recent", "helpful", "high", "low"}

var popularSearches = []string{"smartphone", "sneakers", "t-shirt", "laptop", "headphones", "yoga mat", "air fryer", "watch"}

const maxQueryLength = 100

// parseFilters reads `filters` as a JSON object of string arrays; anything else is treated as "no filters".
func parseFilters(raw string) map[string][]string {
	out := map[string][]string{}
	if raw == "" {
		return out
	}
	var parsed map[string]any
	if err := jsonUnmarshal(raw, &parsed); err != nil || parsed == nil {
		return out
	}
	for k, v := range parsed {
		arr, ok := v.([]any)
		if !ok {
			continue
		}
		list := []string{}
		for _, x := range arr {
			if s, isStr := x.(string); isStr {
				list = append(list, s)
			}
		}
		out[k] = list
	}
	return out
}

func inList(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Handler serves the shopper-facing /catalog and /search routes.
type Handler struct {
	svc     *Service
	mongo   *Mongo
	search  *Search
	limiter func(httpx.HandlerFunc) httpx.HandlerFunc
}

// NewHandler builds the handler; searchLimit is the rate-limit bucket guarding /search/suggest.
func NewHandler(svc *Service, m *Mongo, s *Search, searchLimit func(httpx.HandlerFunc) httpx.HandlerFunc) *Handler {
	return &Handler{svc: svc, mongo: m, search: s, limiter: searchLimit}
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /catalog/home", httpx.Wrap(h.home))
	mux.HandleFunc("GET /catalog/categories/tree", httpx.Wrap(h.categoryTree))
	mux.HandleFunc("GET /catalog/listing", httpx.Wrap(h.listing))
	mux.HandleFunc("GET /catalog/products/{slug}", httpx.Wrap(h.product))
	mux.HandleFunc("POST /catalog/products/by-ids", httpx.Wrap(h.productsByIDs))
	mux.HandleFunc("POST /catalog/products/summaries", httpx.Wrap(h.summaries))
	mux.HandleFunc("GET /catalog/products/{id}/related", httpx.Wrap(h.related))
	mux.HandleFunc("GET /catalog/products/{id}/bought-together", httpx.Wrap(h.boughtTogether))
	mux.HandleFunc("GET /catalog/products/{id}/reviews", httpx.Wrap(h.reviews))
	mux.HandleFunc("GET /catalog/serviceability/{pincode}", httpx.Wrap(h.serviceability))
	mux.HandleFunc("GET /search/suggest", httpx.Wrap(httpx.Chain(h.suggest, h.limiter)))
	mux.HandleFunc("GET /search/popular", httpx.Wrap(h.popular))
}

func (h *Handler) home(w http.ResponseWriter, r *http.Request) error {
	v, err := h.svc.Home(r.Context())
	if err != nil {
		return err
	}
	httpx.CacheJSON(w, r, 60, v)
	return nil
}

func (h *Handler) categoryTree(w http.ResponseWriter, r *http.Request) error {
	v, err := h.svc.CategoryTree(r.Context())
	if err != nil {
		return err
	}
	httpx.CacheJSON(w, r, 300, v)
	return nil
}

func (h *Handler) listing(w http.ResponseWriter, r *http.Request) error {
	get := func(k string) string { v, _ := httpx.Query(r, k); return v }
	sortKey := get("sort")
	if !inList(sortKeys, sortKey) {
		sortKey = "relevance"
	}
	q := domain.ListingQuery{
		CategorySlug: get("categorySlug"), BrandSlug: get("brandSlug"), CollectionSlug: get("collectionSlug"), Q: get("q"),
		Filters: parseFilters(get("filters")), Sort: domain.SortKey(sortKey), Page: httpx.QueryInt(r, "page", 1),
		PageSize: min(100, httpx.QueryInt(r, "pageSize", 24)),
	}
	if _, ok := httpx.Query(r, "priceMin"); ok {
		v := int64(httpx.QueryInt(r, "priceMin", 0))
		q.PriceMin = &v
	}
	if _, ok := httpx.Query(r, "priceMax"); ok {
		v := int64(httpx.QueryInt(r, "priceMax", 0))
		q.PriceMax = &v
	}
	res, err := h.svc.Listing(r.Context(), q)
	if err != nil {
		return err
	}
	httpx.CacheJSON(w, r, 30, res)
	return nil
}

func (h *Handler) product(w http.ResponseWriter, r *http.Request) error {
	res, err := h.svc.Product(r.Context(), r.PathValue("slug"))
	if err != nil {
		return err
	}
	httpx.CacheJSON(w, r, 120, res)
	return nil
}

func idsFrom(r *http.Request) []string {
	ids := []string{}
	if arr, ok := httpx.Lenient(r)["ids"].([]any); ok {
		for _, x := range arr {
			if s, isStr := x.(string); isStr {
				ids = append(ids, s)
			}
		}
	}
	return ids
}

func (h *Handler) productsByIDs(w http.ResponseWriter, r *http.Request) error {
	out, err := h.svc.ProductsByIDs(r.Context(), idsFrom(r))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
	return nil
}

func (h *Handler) summaries(w http.ResponseWriter, r *http.Request) error {
	out, err := h.svc.SummariesByIDs(r.Context(), idsFrom(r))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
	return nil
}

func (h *Handler) related(w http.ResponseWriter, r *http.Request) error {
	out, err := h.svc.Related(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *Handler) boughtTogether(w http.ResponseWriter, r *http.Request) error {
	out, err := h.svc.BoughtTogether(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *Handler) reviews(w http.ResponseWriter, r *http.Request) error {
	sortKey, _ := httpx.Query(r, "sort")
	if !inList(reviewSorts, sortKey) {
		sortKey = "recent"
	}
	out, err := h.svc.Reviews(r.Context(), r.PathValue("id"), sortKey, httpx.QueryInt(r, "page", 1), min(50, httpx.QueryInt(r, "pageSize", 10)))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *Handler) serviceability(w http.ResponseWriter, r *http.Request) error {
	out, err := h.svc.Serviceability(r.PathValue("pincode"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *Handler) suggest(w http.ResponseWriter, r *http.Request) error {
	raw, _ := httpx.Query(r, "q")
	if rs := []rune(raw); len(rs) > maxQueryLength {
		raw = string(rs[:maxQueryLength])
	}
	q := strings.TrimSpace(raw)
	if q == "" {
		httpx.WriteJSON(w, http.StatusOK, domain.SearchSuggestions{Queries: popularSearches[:5], Products: []domain.ProductSummary{}, Categories: []domain.NameSlug{}, Brands: []domain.NameSlug{}})
		return nil
	}
	ctx := r.Context()
	// Product suggestions need Meilisearch; category/brand suggestions are plain Mongo reads. A search outage
	// degrades to an empty product list rather than failing the whole dropdown.
	ids, err := h.search.SuggestProducts(ctx, q, 4)
	if err != nil {
		ids = []string{}
	}
	regex := bson.M{"$regex": regexp.QuoteMeta(q), "$options": "i"}
	var cats []domain.CategoryDoc
	if cur, err := h.mongo.Categories().Find(ctx, bson.M{"name": regex}, optionsLimit(3)); err == nil {
		_ = cur.All(ctx, &cats)
	}
	var brands []domain.BrandDoc
	if cur, err := h.mongo.Brands().Find(ctx, bson.M{"name": regex}, optionsLimit(3)); err == nil {
		_ = cur.All(ctx, &brands)
	}
	products, err := h.svc.SummariesByIDs(ctx, ids)
	if err != nil {
		return err
	}
	queries := []string{}
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			queries = append(queries, s)
		}
	}
	for _, p := range popularSearches {
		if strings.HasPrefix(strings.ToLower(p), strings.ToLower(q)) {
			add(p)
		}
	}
	for _, c := range cats {
		add(c.Name)
	}
	for _, b := range brands {
		add(b.Name)
	}
	if len(queries) > 5 {
		queries = queries[:5]
	}
	catOut, brandOut := []domain.NameSlug{}, []domain.NameSlug{}
	for _, c := range cats {
		catOut = append(catOut, domain.NameSlug{Slug: c.Slug, Name: c.Name})
	}
	for _, b := range brands {
		brandOut = append(brandOut, domain.NameSlug{Slug: b.Slug, Name: b.Name})
	}
	httpx.WriteJSON(w, http.StatusOK, domain.SearchSuggestions{Queries: queries, Products: products, Categories: catOut, Brands: brandOut})
	return nil
}

func (h *Handler) popular(w http.ResponseWriter, r *http.Request) error {
	httpx.WriteJSON(w, http.StatusOK, popularSearches)
	return nil
}

var _ = apperr.Validation
var _ = time.Second

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
