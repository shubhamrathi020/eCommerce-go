package catalog

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/cache"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// Admin types: back-office product management (the frontend's AdminProductApi, backed by the real catalog store).

// AdminProductRow is one line of the admin product list.
type AdminProductRow struct {
	ID           string          `json:"id"`
	Slug         string          `json:"slug"`
	Title        string          `json:"title"`
	BrandName    string          `json:"brandName"`
	CategoryName string          `json:"categoryName"`
	Status       string          `json:"status"`
	PriceMin     domain.Money    `json:"priceMin"`
	StockTotal   int64           `json:"stockTotal"`
	VariantCount int             `json:"variantCount"`
	Image        domain.ImageRef `json:"image"`
	UpdatedAt    string          `json:"updatedAt"`
}

// AdminVariantInput is one variant in the admin form.
type AdminVariantInput struct {
	ID      string            `json:"id,omitempty"`
	SKU     string            `json:"sku"`
	Options map[string]string `json:"options"`
	Price   float64           `json:"price"`
	MRP     *float64          `json:"mrp,omitempty"`
	Stock   int64             `json:"stock"`

	optionKeys []string
}

// AdminProductInput is the admin product form.
type AdminProductInput struct {
	Title       string
	BrandName   string
	CategoryID  string
	Description string
	Highlights  []string
	Tags        []string
	Status      string
	Variants    []AdminVariantInput
}

// AdminProductDetail is a product as the admin form edits it.
type AdminProductDetail struct {
	ID           string              `json:"id"`
	Slug         string              `json:"slug"`
	Title        string              `json:"title"`
	BrandName    string              `json:"brandName"`
	CategoryID   string              `json:"categoryId"`
	CategoryName string              `json:"categoryName"`
	Description  string              `json:"description"`
	Highlights   []string            `json:"highlights"`
	Tags         []string            `json:"tags"`
	Status       string              `json:"status"`
	VariantAxes  []string            `json:"variantAxes"`
	UpdatedAt    string              `json:"updatedAt"`
	Variants     []AdminVariantInput `json:"variants"`
}

// Paged is one page of a list.
type Paged[T any] struct {
	Items    []T `json:"items"`
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"pageSize"`
}

// AdminCategoryOption is a leaf category choice.
type AdminCategoryOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AdminProductQuery narrows the admin list.
type AdminProductQuery struct {
	Q        string
	Status   string
	Sort     string
	Dir      string
	Page     int
	PageSize int
}

// Admin is the back-office catalog service.
type Admin struct {
	mongo  *Mongo
	search *Search
	cache  *cache.Cache
}

// NewAdmin builds the admin catalog service.
func NewAdmin(m *Mongo, s *Search, c *cache.Cache) *Admin {
	return &Admin{mongo: m, search: s, cache: c}
}

func inr(amount float64) domain.Money { return domain.INR(amount) }

var (
	ampRE   = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	paraRE  = regexp.MustCompile(`\n\s*\n`)
	closeRE = regexp.MustCompile(`</p>\s*<p>`)
	tagRE   = regexp.MustCompile(`<[^>]+>`)
	slugRE  = regexp.MustCompile(`[^a-z0-9]+`)
)

func paragraphs(text string) string {
	var b strings.Builder
	for _, p := range paraRE.Split(text, -1) {
		if p = strings.TrimSpace(p); p != "" {
			b.WriteString("<p>" + ampRE.Replace(p) + "</p>")
		}
	}
	return b.String()
}

func plainText(html string) string {
	s := closeRE.ReplaceAllString(html, "\n\n")
	s = tagRE.ReplaceAllString(s, "")
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&amp;", "&").Replace(s)
}

func slugify(s string) string {
	return strings.Trim(slugRE.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func nowISO() string { return domain.ISO(time.Now()) }

func base36(n int64) string { return strconv.FormatInt(n, 36) }

func validateInput(in AdminProductInput, categoryIDs map[string]bool, others []domain.Variant) error {
	fields := map[string]string{}
	if strings.TrimSpace(in.Title) == "" {
		fields["title"] = "Title is required"
	} else if len([]rune(in.Title)) > 120 {
		fields["title"] = "Title is too long (120 characters at most)"
	}
	if strings.TrimSpace(in.BrandName) == "" {
		fields["brandName"] = "Brand is required"
	}
	if !categoryIDs[in.CategoryID] {
		fields["categoryId"] = "Choose a category"
	}
	if len([]rune(in.Description)) > 2000 {
		fields["description"] = "Description is too long (2,000 characters at most)"
	}
	if len(in.Variants) == 0 {
		fields["variants"] = "Add at least one variant"
	}
	skus := map[string]bool{}
	for i, v := range in.Variants {
		sku := strings.TrimSpace(v.SKU)
		key := func(f string) string { return fmt.Sprintf("variants.%d.%s", i, f) }
		if sku == "" {
			fields[key("sku")] = "SKU is required"
		} else {
			dup := skus[sku]
			for _, o := range others {
				if o.SKU == sku && o.ID != v.ID {
					dup = true
				}
			}
			if dup {
				fields[key("sku")] = "SKU must be unique"
			}
		}
		skus[sku] = true
		if !(v.Price > 0) {
			fields[key("price")] = "Price must be greater than zero"
		}
		if v.MRP != nil && *v.MRP < v.Price {
			fields[key("mrp")] = "MRP cannot be lower than the price"
		}
		if v.Stock < 0 {
			fields[key("stock")] = "Stock must be a whole number, 0 or more"
		}
	}
	return apperr.AssertNoFieldErrors(fields)
}

func buildVariants(productID string, in AdminProductInput, existing []domain.Variant) []domain.Variant {
	out := make([]domain.Variant, len(in.Variants))
	for i, v := range in.Variants {
		var previous *domain.Variant
		for j := range existing {
			if v.ID != "" && existing[j].ID == v.ID {
				previous = &existing[j]
			}
		}
		id := fmt.Sprintf("%s-v%s%d", productID, base36(time.Now().UnixMilli()), i)
		if previous != nil {
			id = previous.ID
		}
		nv := domain.Variant{ID: id, SKU: strings.TrimSpace(v.SKU), Options: v.Options, Price: inr(v.Price), Stock: v.Stock}
		if v.MRP != nil && *v.MRP > v.Price {
			m := inr(*v.MRP)
			nv.MRP = &m
		}
		if previous != nil && previous.Images != nil {
			nv.Images = previous.Images
		}
		out[i] = nv
	}
	return out
}

func minPrice(vs []domain.Variant) int64 {
	m := vs[0].Price.Amount
	for _, v := range vs {
		if v.Price.Amount < m {
			m = v.Price.Amount
		}
	}
	return m
}

func totalStockOf(vs []domain.Variant) int64 {
	var s int64
	for _, v := range vs {
		s += v.Stock
	}
	return s
}

func toRow(d *domain.ProductDoc) AdminProductRow {
	row := AdminProductRow{
		ID: d.ID, Slug: d.Slug, Title: d.Title, BrandName: d.BrandName, CategoryName: d.CategoryPath[len(d.CategoryPath)-1].Name,
		Status: d.Status, PriceMin: domain.INRInt(minPrice(d.Variants)), StockTotal: totalStockOf(d.Variants), VariantCount: len(d.Variants), UpdatedAt: d.UpdatedAt,
	}
	if len(d.Images) > 0 {
		row.Image = d.Images[0]
	}
	return row
}

func toDetail(d *domain.ProductDoc) AdminProductDetail {
	variants := make([]AdminVariantInput, len(d.Variants))
	for i, v := range d.Variants {
		vi := AdminVariantInput{ID: v.ID, SKU: v.SKU, Options: v.Options, Price: float64(v.Price.Amount), Stock: v.Stock}
		if v.MRP != nil {
			m := float64(v.MRP.Amount)
			vi.MRP = &m
		}
		variants[i] = vi
	}
	p := Normalize(d.Product)
	return AdminProductDetail{
		ID: d.ID, Slug: d.Slug, Title: d.Title, BrandName: d.BrandName, CategoryID: d.CategoryID, CategoryName: d.CategoryPath[len(d.CategoryPath)-1].Name,
		Description: plainText(d.Description), Highlights: p.Highlights, Tags: p.Tags, Status: d.Status, VariantAxes: p.VariantAxes, UpdatedAt: d.UpdatedAt, Variants: variants,
	}
}

func (a *Admin) syncIndex(ctx context.Context, d *domain.ProductDoc) {
	if d.Status == "published" {
		p := Normalize(d.Product)
		_ = a.search.IndexProducts(ctx, []map[string]any{SearchDoc(&p)})
	} else {
		_ = a.search.DeleteProduct(ctx, d.ID)
	}
	// Every cached view of this product, plus the broad home/listing bucket: a status or price change can move
	// this product in or out of listings the cache has no per-entry record of.
	a.cache.InvalidateTag(ctx, "product:"+d.ID)
	a.cache.InvalidateTag(ctx, listingsTag)
}

func (a *Admin) allDocs(ctx context.Context, filter any) ([]domain.ProductDoc, error) {
	var docs []domain.ProductDoc
	cur, err := a.mongo.Products().Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	err = cur.All(ctx, &docs)
	return docs, err
}

// List is one sorted, filtered page of products.
func (a *Admin) List(ctx context.Context, q AdminProductQuery) (*Paged[AdminProductRow], error) {
	filter := bson.M{}
	if q.Status != "" {
		filter["status"] = q.Status
	}
	rows, err := a.allDocs(ctx, filter)
	if err != nil {
		return nil, err
	}
	if s := strings.ToLower(strings.TrimSpace(q.Q)); s != "" {
		kept := rows[:0]
		for _, d := range rows {
			skus := []string{}
			for _, v := range d.Variants {
				skus = append(skus, v.SKU)
			}
			if strings.Contains(strings.ToLower(d.Title+" "+d.BrandName+" "+strings.Join(skus, " ")), s) {
				kept = append(kept, d)
			}
		}
		rows = kept
	}
	cmp := func(x, y *domain.ProductDoc) int {
		switch q.Sort {
		case "title":
			return localeCompare(x.Title, y.Title)
		case "price":
			return int(minPrice(x.Variants) - minPrice(y.Variants))
		case "stock":
			return int(totalStockOf(x.Variants) - totalStockOf(y.Variants))
		default:
			return strings.Compare(x.UpdatedAt, y.UpdatedAt)
		}
	}
	dir := -1
	if q.Dir == "asc" {
		dir = 1
	}
	sort.SliceStable(rows, func(i, j int) bool {
		c := cmp(&rows[i], &rows[j])
		if c == 0 {
			c = strings.Compare(rows[i].ID, rows[j].ID)
		}
		return c*dir < 0
	})
	pageSize := max(1, q.PageSize)
	pages := max(1, (len(rows)+pageSize-1)/pageSize)
	page := min(max(1, q.Page), pages)
	items := []AdminProductRow{}
	for i := (page - 1) * pageSize; i < min(page*pageSize, len(rows)); i++ {
		items = append(items, toRow(&rows[i]))
	}
	return &Paged[AdminProductRow]{Items: items, Total: len(rows), Page: page, PageSize: pageSize}, nil
}

// Categories are the leaf categories a product can be filed under.
func (a *Admin) Categories(ctx context.Context) ([]AdminCategoryOption, error) {
	var docs []domain.CategoryDoc
	cur, err := a.mongo.Categories().Find(ctx, bson.M{"parentId": bson.M{"$exists": true}})
	if err != nil {
		return nil, err
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]AdminCategoryOption, len(docs))
	for i, c := range docs {
		out[i] = AdminCategoryOption{ID: c.ID, Name: c.Name}
	}
	return out, nil
}

func (a *Admin) find(ctx context.Context, id string) (*domain.ProductDoc, error) {
	var doc domain.ProductDoc
	if err := a.mongo.Products().FindOne(ctx, bson.M{"id": id}).Decode(&doc); err != nil {
		return nil, apperr.New(apperr.NotFound, "Product not found")
	}
	return &doc, nil
}

// Get is one product for the admin form.
func (a *Admin) Get(ctx context.Context, id string) (*AdminProductDetail, error) {
	d, err := a.find(ctx, id)
	if err != nil {
		return nil, err
	}
	out := toDetail(d)
	return &out, nil
}

func (a *Admin) leafIDs(ctx context.Context) (map[string]bool, []domain.CategoryDoc, error) {
	var cats []domain.CategoryDoc
	cur, err := a.mongo.Categories().Find(ctx, bson.M{})
	if err != nil {
		return nil, nil, err
	}
	if err := cur.All(ctx, &cats); err != nil {
		return nil, nil, err
	}
	leaf := map[string]bool{}
	for _, c := range cats {
		if c.ParentID != "" {
			leaf[c.ID] = true
		}
	}
	return leaf, cats, nil
}

// Create adds a product.
func (a *Admin) Create(ctx context.Context, in AdminProductInput) (*AdminProductDetail, error) {
	leafIDs, cats, err := a.leafIDs(ctx)
	if err != nil {
		return nil, err
	}
	existing, err := a.allDocs(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	var existingVariants []domain.Variant
	for _, d := range existing {
		existingVariants = append(existingVariants, d.Variants...)
	}
	if err := validateInput(in, leafIDs, existingVariants); err != nil {
		return nil, err
	}
	var leaf, parent *domain.CategoryDoc
	for i := range cats {
		if cats[i].ID == in.CategoryID {
			leaf = &cats[i]
		}
	}
	if leaf != nil {
		for i := range cats {
			if cats[i].ID == leaf.ParentID {
				parent = &cats[i]
			}
		}
	}
	if leaf == nil || parent == nil {
		return nil, apperr.WithFields(apperr.Validation, "Choose a category", map[string]string{"categoryId": "Choose a category"})
	}
	// No media upload pipeline yet: a same-category product's images stand in until real uploads exist.
	var donor *domain.ProductDoc
	for i := range existing {
		if existing[i].CategoryID == leaf.ID {
			donor = &existing[i]
			break
		}
	}
	if donor == nil && len(existing) > 0 {
		donor = &existing[0]
	}
	if donor == nil {
		return nil, apperr.New(apperr.Conflict, "Catalog is empty; seed it before creating products.")
	}
	id := "p-new-" + base36(time.Now().UnixMilli())
	axes := in.Variants[0].optionKeys
	if axes == nil {
		axes = []string{}
	}
	doc := domain.ProductDoc{
		DocID: id, Status: in.Status, UpdatedAt: nowISO(),
		Product: domain.Product{
			ID: id, Slug: fmt.Sprintf("%s-%s", slugify(in.Title), id[len(id)-4:]), Title: strings.TrimSpace(in.Title),
			BrandID: "brand-" + slugify(in.BrandName), BrandName: strings.TrimSpace(in.BrandName), CategoryID: leaf.ID,
			CategoryPath: []domain.CategoryRef{{ID: parent.ID, Slug: parent.Slug, Name: parent.Name}, {ID: leaf.ID, Slug: leaf.Slug, Name: leaf.Name}},
			Description:  paragraphs(in.Description), Highlights: nonNil(in.Highlights), Images: donor.Images, Attributes: map[string]any{},
			VariantAxes: axes, Variants: buildVariants(id, in, nil), Rating: domain.RatingSummary{Distribution: [5]int64{}},
			Tags: nonNil(in.Tags), CreatedAt: nowISO(),
		},
	}
	if _, err := a.mongo.Products().InsertOne(ctx, doc); err != nil {
		return nil, err
	}
	a.syncIndex(ctx, &doc)
	out := toDetail(&doc)
	return &out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Update changes a product.
func (a *Admin) Update(ctx context.Context, id string, in AdminProductInput) (*AdminProductDetail, error) {
	existing, err := a.find(ctx, id)
	if err != nil {
		return nil, err
	}
	leafIDs, _, err := a.leafIDs(ctx)
	if err != nil {
		return nil, err
	}
	others, err := a.allDocs(ctx, bson.M{"id": bson.M{"$ne": id}})
	if err != nil {
		return nil, err
	}
	var otherVariants []domain.Variant
	for _, d := range others {
		otherVariants = append(otherVariants, d.Variants...)
	}
	if err := validateInput(in, leafIDs, otherVariants); err != nil {
		return nil, err
	}
	updated := *existing
	updated.Title = strings.TrimSpace(in.Title)
	updated.BrandName = strings.TrimSpace(in.BrandName)
	updated.Description = paragraphs(in.Description)
	updated.Highlights = nonNil(in.Highlights)
	updated.Tags = nonNil(in.Tags)
	updated.Variants = buildVariants(existing.ID, in, existing.Variants)
	updated.Status = in.Status
	updated.UpdatedAt = nowISO()
	if _, err := a.mongo.Products().ReplaceOne(ctx, bson.M{"id": id}, updated); err != nil {
		return nil, err
	}
	a.syncIndex(ctx, &updated)
	out := toDetail(&updated)
	return &out, nil
}

// BulkSetStatus sets the status of several products at once, skipping unknown ids.
func (a *Admin) BulkSetStatus(ctx context.Context, ids []string, status string) (int, error) {
	targets, err := a.allDocs(ctx, bson.M{"id": bson.M{"$in": ids}})
	if err != nil || len(targets) == 0 {
		return 0, err
	}
	found := make([]string, len(targets))
	for i, t := range targets {
		found[i] = t.ID
	}
	now := nowISO()
	if _, err := a.mongo.Products().UpdateMany(ctx, bson.M{"id": bson.M{"$in": found}}, bson.M{"$set": bson.M{"status": status, "updatedAt": now}}); err != nil {
		return 0, err
	}
	for i := range targets {
		targets[i].Status, targets[i].UpdatedAt = status, now
		a.syncIndex(ctx, &targets[i])
	}
	return len(targets), nil
}

// BulkDeleteDrafts deletes only drafts, never a published product.
func (a *Admin) BulkDeleteDrafts(ctx context.Context, ids []string) (int, error) {
	drafts, err := a.allDocs(ctx, bson.M{"id": bson.M{"$in": ids}, "status": "draft"})
	if err != nil || len(drafts) == 0 {
		return 0, err
	}
	found := make([]string, len(drafts))
	for i, d := range drafts {
		found[i] = d.ID
	}
	if _, err := a.mongo.Products().DeleteMany(ctx, bson.M{"id": bson.M{"$in": found}}); err != nil {
		return 0, err
	}
	for _, d := range drafts {
		_ = a.search.DeleteProduct(ctx, d.ID)
		a.cache.InvalidateTag(ctx, "product:"+d.ID)
	}
	a.cache.InvalidateTag(ctx, listingsTag)
	return len(drafts), nil
}

// AdminHandler serves /admin/products.
type AdminHandler struct {
	svc    *Admin
	guards *httpx.Guards
}

// NewAdminHandler builds the handler.
func NewAdminHandler(svc *Admin, guards *httpx.Guards) *AdminHandler {
	return &AdminHandler{svc: svc, guards: guards}
}

var statuses = []string{"draft", "published", "archived"}

// Register mounts the routes.
func (h *AdminHandler) Register(mux *http.ServeMux) {
	read, write := h.guards.Require("product:read"), h.guards.Require("product:write")
	mux.HandleFunc("GET /admin/products", httpx.Wrap(httpx.Chain(h.list, read)))
	mux.HandleFunc("GET /admin/products/categories", httpx.Wrap(httpx.Chain(h.categories, read)))
	mux.HandleFunc("GET /admin/products/{id}", httpx.Wrap(httpx.Chain(h.get, read)))
	mux.HandleFunc("POST /admin/products", httpx.Wrap(httpx.Chain(h.create, write)))
	mux.HandleFunc("PUT /admin/products/{id}", httpx.Wrap(httpx.Chain(h.update, write)))
	mux.HandleFunc("PATCH /admin/products/bulk-status", httpx.Wrap(httpx.Chain(h.bulkStatus, write)))
	mux.HandleFunc("PATCH /admin/products/bulk-delete-drafts", httpx.Wrap(httpx.Chain(h.bulkDelete, write)))
}

func (h *AdminHandler) list(w http.ResponseWriter, r *http.Request) error {
	get := func(k string) string { v, _ := httpx.Query(r, k); return v }
	sortKey := get("sort")
	if !inList([]string{"updated", "title", "price", "stock"}, sortKey) {
		sortKey = "updated"
	}
	status := get("status")
	if !inList(statuses, status) {
		status = ""
	}
	dir := "desc"
	if get("dir") == "asc" {
		dir = "asc"
	}
	out, err := h.svc.List(r.Context(), AdminProductQuery{Q: get("q"), Status: status, Sort: sortKey, Dir: dir, Page: httpx.QueryInt(r, "page", 1), PageSize: min(200, httpx.QueryInt(r, "pageSize", 20))})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *AdminHandler) categories(w http.ResponseWriter, r *http.Request) error {
	out, err := h.svc.Categories(r.Context())
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *AdminHandler) get(w http.ResponseWriter, r *http.Request) error {
	out, err := h.svc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func readProductInput(r *http.Request) (AdminProductInput, error) {
	body := httpx.ReadObject(r, "title", "brandName", "categoryId", "description", "highlights", "tags", "status", "variants")
	in := AdminProductInput{
		Title: body.Str("title", 0), BrandName: body.Str("brandName", 0), CategoryID: body.Str("categoryId", 0), Description: body.Str("description", 0),
		Highlights: body.Strings("highlights"), Tags: body.Strings("tags"), Status: body.Enum("status", statuses...),
	}
	for _, raw := range body.Array("variants") {
		v := httpx.ObjectFrom(raw, "id", "sku", "options", "price", "mrp", "stock")
		item := AdminVariantInput{}
		item.ID, _ = v.OptStr("id", 0)
		item.SKU = v.Str("sku", 0)
		item.optionKeys, item.Options = v.OrderedStringMap("options")
		item.Price = v.Num("price", 0)
		if mrp, ok := v.OptNum("mrp", 0); ok {
			item.MRP = &mrp
		}
		item.Stock = v.Int("stock", 0)
		body.Absorb(v)
		in.Variants = append(in.Variants, item)
	}
	return in, body.Err()
}

func (h *AdminHandler) create(w http.ResponseWriter, r *http.Request) error {
	in, err := readProductInput(r)
	if err != nil {
		return err
	}
	out, err := h.svc.Create(r.Context(), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
	return nil
}

func (h *AdminHandler) update(w http.ResponseWriter, r *http.Request) error {
	in, err := readProductInput(r)
	if err != nil {
		return err
	}
	out, err := h.svc.Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *AdminHandler) bulkStatus(w http.ResponseWriter, r *http.Request) error {
	body := httpx.ReadObject(r, "ids", "status")
	ids := body.Strings("ids")
	status := body.Enum("status", statuses...)
	if err := body.Err(); err != nil {
		return err
	}
	n, err := h.svc.BulkSetStatus(r.Context(), ids, status)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"count": n})
	return nil
}

func (h *AdminHandler) bulkDelete(w http.ResponseWriter, r *http.Request) error {
	body := httpx.ReadObject(r, "ids")
	ids := body.Strings("ids")
	if err := body.Err(); err != nil {
		return err
	}
	n, err := h.svc.BulkDeleteDrafts(r.Context(), ids)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"count": n})
	return nil
}
