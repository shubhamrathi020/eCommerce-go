package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
	"github.com/shubhamrathi020/eCommerce-go/internal/resilience"
)

// synonymGroups are interchangeable words, mirrored from the mock search engine so real search behaves the
// same way a shopper already saw in development.
var synonymGroups = [][]string{
	{"tee", "tshirt"}, {"mobile", "phone", "smartphone", "cellphone"}, {"laptop", "notebook", "ultrabook"}, {"tv", "television"},
	{"sofa", "couch"}, {"sneaker", "shoe", "trainer"}, {"earbud", "earphone", "headphone"}, {"bag", "backpack"}, {"kid", "child"}, {"jean", "denim"},
}

var sortField = map[domain.SortKey][]string{
	"featured": {"popularity:desc"}, "price-asc": {"priceMin:asc"}, "price-desc": {"priceMax:desc"},
	"newest": {"createdAtTs:desc"}, "rating": {"ratingAverage:desc"}, "discount": {"bestDiscount:desc"},
}

// FacetClause is a Meilisearch filter for one facet group and the field it counts.
type FacetClause struct{ Filter, Field string }

// SearchQuery is one listing search.
type SearchQuery struct {
	Q            string
	Scope        []string
	FacetFilters []FacetClause
	FacetFields  []string
	PriceFilter  string
	Sort         domain.SortKey
	Page         int
	PageSize     int
}

// SearchResult is what the listing needs from Meilisearch.
type SearchResult struct {
	IDs               []string
	Scores            map[string]float64
	Total             int
	FacetDistribution map[string]map[string]int
	PriceBounds       domain.PriceBounds
}

// Search talks to Meilisearch over its REST API. Shopper-facing reads go through a circuit breaker and one retry.
type Search struct {
	host      string
	key       string
	IndexName string
	client    *http.Client
	breaker   *resilience.Breaker
}

// NewSearch builds the search client.
func NewSearch(cfg *config.Config) *Search {
	return &Search{
		host: strings.TrimRight(cfg.MeiliURL, "/"), key: cfg.MeiliMasterKey, IndexName: "products" + cfg.MeiliIndexSuffix,
		client:  &http.Client{Timeout: 10 * time.Second},
		breaker: resilience.NewBreaker("meilisearch", resilience.Options{FailureThreshold: 3, Cooldown: 20 * time.Second, Timeout: 3 * time.Second}),
	}
}

// BreakerStats reports the breaker state.
func (s *Search) BreakerStats() resilience.Stats { return s.breaker.Stats() }

func (s *Search) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.host+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.key != "" {
		req.Header.Set("Authorization", "Bearer "+s.key)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("meilisearch %s %s: %d %s", method, path, res.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// guarded runs a shopper-facing read through the breaker with one retry. Every failure surfaces as the same
// friendly error, never the raw HTTP failure.
func (s *Search) guarded(ctx context.Context, fn func(context.Context) error) error {
	err := s.breaker.Exec(ctx, func(c context.Context) error { return resilience.Retry(c, 2, 150*time.Millisecond, fn) })
	if err != nil {
		httpx.Logger.Warn("meilisearch call failed: "+err.Error(), "logger", "SearchService")
		return apperr.New(apperr.Network, "Search is temporarily unavailable. Please try again shortly.")
	}
	return nil
}

// Health checks Meilisearch answers.
func (s *Search) Health(ctx context.Context) error {
	return s.do(ctx, http.MethodGet, "/health", nil, nil)
}

func (s *Search) waitForTask(ctx context.Context, body []byte) error {
	var task struct {
		TaskUID int64 `json:"taskUid"`
	}
	if err := json.Unmarshal(body, &task); err != nil {
		return err
	}
	for i := 0; i < 600; i++ {
		var st struct {
			Status string `json:"status"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := s.do(ctx, http.MethodGet, fmt.Sprintf("/tasks/%d", task.TaskUID), nil, &st); err != nil {
			return err
		}
		switch st.Status {
		case "succeeded":
			return nil
		case "failed", "canceled":
			return fmt.Errorf("meilisearch task %d %s: %s", task.TaskUID, st.Status, st.Error.Message)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("meilisearch task %d did not finish", task.TaskUID)
}

func (s *Search) doTask(ctx context.Context, method, path string, body any) error {
	var raw json.RawMessage
	if err := s.do(ctx, method, path, body, &raw); err != nil {
		return err
	}
	return s.waitForTask(ctx, raw)
}

// IndexProducts adds or replaces documents in the products index.
func (s *Search) IndexProducts(ctx context.Context, docs []map[string]any) error {
	if len(docs) == 0 {
		return nil
	}
	return s.doTask(ctx, http.MethodPost, "/indexes/"+s.IndexName+"/documents", docs)
}

// DeleteProduct removes a document from the index.
func (s *Search) DeleteProduct(ctx context.Context, id string) error {
	return s.doTask(ctx, http.MethodDelete, "/indexes/"+s.IndexName+"/documents/"+id, nil)
}

type searchResponse struct {
	Hits []struct {
		ID           string  `json:"id"`
		RankingScore float64 `json:"_rankingScore"`
	} `json:"hits"`
	TotalHits         *int                      `json:"totalHits"`
	FacetDistribution map[string]map[string]int `json:"facetDistribution"`
	FacetStats        map[string]struct {
		Min float64 `json:"min"`
		Max float64 `json:"max"`
	} `json:"facetStats"`
}

func (s *Search) query(ctx context.Context, q string, params map[string]any) (*searchResponse, error) {
	params["q"] = q
	var res searchResponse
	if err := s.do(ctx, http.MethodPost, "/indexes/"+s.IndexName+"/search", params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func joinAnd(parts []string) any {
	if len(parts) == 0 {
		return nil
	}
	return strings.Join(parts, " AND ")
}

// Search runs a listing search plus, for each active facet group, a recount with that group's own clause removed,
// so its option counts reflect "if you added this option" rather than "given you already selected it".
func (s *Search) Search(ctx context.Context, q SearchQuery) (*SearchResult, error) {
	var result *SearchResult
	err := s.guarded(ctx, func(c context.Context) error {
		all := append([]string{}, q.Scope...)
		for _, f := range q.FacetFilters {
			all = append(all, f.Filter)
		}
		if q.PriceFilter != "" {
			all = append(all, q.PriceFilter)
		}
		facets := append(append([]string{}, q.FacetFields...), "priceMin", "priceMax")
		params := map[string]any{"filter": joinAnd(all), "facets": facets, "page": max(1, q.Page), "hitsPerPage": max(1, q.PageSize), "showRankingScore": true}
		if sf := sortField[q.Sort]; sf != nil {
			params["sort"] = sf
		}
		main, err := s.query(c, q.Q, params)
		if err != nil {
			return err
		}
		distribution := map[string]map[string]int{}
		for k, v := range main.FacetDistribution {
			distribution[k] = v
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		var firstErr error
		for _, own := range q.FacetFilters {
			wg.Add(1)
			go func(own FacetClause) {
				defer wg.Done()
				without := []string{}
				for _, f := range all {
					if f != own.Filter {
						without = append(without, f)
					}
				}
				res, err := s.query(c, q.Q, map[string]any{"filter": joinAnd(without), "facets": []string{own.Field}, "limit": 0})
				mu.Lock()
				defer mu.Unlock()
				if err != nil && firstErr == nil {
					firstErr = err
				} else if err == nil {
					if d, ok := res.FacetDistribution[own.Field]; ok {
						distribution[own.Field] = d
					}
				}
			}(own)
		}
		wg.Wait()
		if firstErr != nil {
			return firstErr
		}
		r := &SearchResult{Scores: map[string]float64{}, FacetDistribution: distribution, IDs: []string{}}
		for _, h := range main.Hits {
			r.IDs = append(r.IDs, h.ID)
			r.Scores[h.ID] = h.RankingScore
		}
		r.Total = len(main.Hits)
		if main.TotalHits != nil {
			r.Total = *main.TotalHits
		}
		r.PriceBounds = domain.PriceBounds{Min: main.FacetStats["priceMin"].Min, Max: main.FacetStats["priceMax"].Max}
		result = r
		return nil
	})
	return result, err
}

// SuggestProducts returns the ids of the best matching products for the search box dropdown.
func (s *Search) SuggestProducts(ctx context.Context, q string, limit int) ([]string, error) {
	ids := []string{}
	err := s.guarded(ctx, func(c context.Context) error {
		res, err := s.query(c, q, map[string]any{"limit": limit})
		if err != nil {
			return err
		}
		for _, h := range res.Hits {
			ids = append(ids, h.ID)
		}
		return nil
	})
	return ids, err
}

// Count is the exact number of documents matching all the filters (used for the threshold facets).
func (s *Search) Count(ctx context.Context, filters []string) (int, error) {
	n := 0
	err := s.guarded(ctx, func(c context.Context) error {
		res, err := s.query(c, "", map[string]any{"filter": joinAnd(filters), "page": 1, "hitsPerPage": 1})
		if err != nil {
			return err
		}
		if res.TotalHits != nil {
			n = *res.TotalHits
		}
		return nil
	})
	return n, err
}

// EnsureIndex creates the index and its settings; safe to call every time.
func (s *Search) EnsureIndex(ctx context.Context, attrFields []string) error {
	_ = s.doTask(ctx, http.MethodPost, "/indexes", map[string]any{"uid": s.IndexName, "primaryKey": "id"}) // exists already is fine
	numeric := []string{"priceMin", "priceMax", "ratingAverage", "bestDiscount", "totalStock", "popularity", "createdAtTs"}
	synonyms := map[string][]string{}
	for _, group := range synonymGroups {
		for _, word := range group {
			for _, w := range group {
				if w != word {
					synonyms[word] = append(synonyms[word], w)
				}
			}
		}
	}
	return s.doTask(ctx, http.MethodPatch, "/indexes/"+s.IndexName+"/settings", map[string]any{
		"searchableAttributes": append([]string{"title", "brandName", "tags"}, attrFields...),
		"filterableAttributes": append(append([]string{"categoryId", "brandSlug", "tags"}, numeric...), attrFields...),
		"sortableAttributes":   numeric,
		"synonyms":             synonyms,
	})
}

// SearchDoc is the flattened, denormalised document written to the products index.
func SearchDoc(p *domain.Product) map[string]any {
	var stock int64
	cheapest, dearest := p.Variants[0].Price.Amount, p.Variants[0].Price.Amount
	for _, v := range p.Variants {
		stock += v.Stock
		if v.Price.Amount < cheapest {
			cheapest = v.Price.Amount
		}
		if v.Price.Amount > dearest {
			dearest = v.Price.Amount
		}
	}
	created, _ := time.Parse(time.RFC3339, p.CreatedAt)
	doc := map[string]any{
		"id": p.ID, "slug": p.Slug, "title": p.Title, "brandName": p.BrandName, "brandSlug": strings.TrimPrefix(p.BrandID, "brand-"),
		"categoryId": p.CategoryID, "tags": p.Tags, "priceMin": cheapest, "priceMax": dearest, "ratingAverage": p.Rating.Average,
		"bestDiscount": domain.BestDiscount(p), "totalStock": stock, "popularity": p.Popularity, "createdAtTs": created.UnixMilli(),
	}
	for key, values := range facetValuesOf(p) {
		doc["attr_"+key] = values
	}
	return doc
}

// facetValuesOf is every attribute and variant-axis value on a product, one string list per key.
func facetValuesOf(p *domain.Product) map[string][]string {
	out := map[string][]string{}
	for key, value := range p.Attributes {
		out[key] = []string{fmt.Sprint(value)}
	}
	for _, axis := range p.VariantAxes {
		seen := map[string]bool{}
		values := []string{}
		for _, v := range p.Variants {
			if val, ok := v.Options[axis]; ok && !seen[val] {
				seen[val] = true
				values = append(values, val)
			}
		}
		if len(values) > 0 {
			out[axis] = values
		}
	}
	return out
}
