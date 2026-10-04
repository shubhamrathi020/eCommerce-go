package commerce

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// Handler serves cart, checkout, orders, payments and the Razorpay webhook.
type Handler struct {
	cfg      *config.Config
	guards   *httpx.Guards
	cart     *Cart
	orders   *Orders
	payments *Payments
	razorpay *Razorpay
	admin    *AdminOrders
	limit    func(string) func(httpx.HandlerFunc) httpx.HandlerFunc
	watch    func(ctx *http.Request, orderID string) (<-chan string, func())
}

// NewHandler builds the handler. limit returns a named Redis rate-limit bucket; watch opens a live order stream.
func NewHandler(cfg *config.Config, guards *httpx.Guards, cart *Cart, orders *Orders, payments *Payments, razorpay *Razorpay, admin *AdminOrders,
	limit func(string) func(httpx.HandlerFunc) httpx.HandlerFunc, watch func(r *http.Request, orderID string) (<-chan string, func())) *Handler {
	return &Handler{cfg: cfg, guards: guards, cart: cart, orders: orders, payments: payments, razorpay: razorpay, admin: admin, limit: limit, watch: watch}
}

// owner is the signed-in user, else the guest identified by the cart cookie.
func ownerOf(r *http.Request) Owner {
	if u := httpx.User(r); u != nil {
		return Owner{UserID: u.ID}
	}
	return Owner{GuestToken: GuestToken(r)}
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	opt := h.guards.Optional
	csrf := httpx.CSRF
	route := func(pattern string, fn httpx.HandlerFunc, mws ...func(httpx.HandlerFunc) httpx.HandlerFunc) {
		mux.HandleFunc(pattern, httpx.Wrap(httpx.Chain(fn, append([]func(httpx.HandlerFunc) httpx.HandlerFunc{opt}, mws...)...)))
	}

	route("GET /cart", h.getCart)
	route("POST /cart/items", h.addItem, csrf)
	route("PUT /cart/items/{variantId}", h.setQuantity, csrf)
	route("DELETE /cart/items/{variantId}", h.removeItem, csrf)
	route("POST /cart/coupon", h.applyCoupon, csrf, h.limit("coupon"))
	route("DELETE /cart/coupon", h.removeCoupon, csrf)
	route("PUT /cart/shipping-method", h.setShipping, csrf)
	route("DELETE /cart", h.clearCart, csrf)

	route("GET /checkout/shipping-options/{pincode}", h.shippingOptions)
	route("GET /checkout/payment-options/{pincode}", h.paymentOptions)

	route("POST /orders", h.placeOrder, csrf, h.limit("checkout"))
	route("GET /orders/{id}/stream", h.stream)
	route("GET /orders/{id}", h.getOrder)
	route("GET /orders", h.listOrders)
	route("POST /orders/{id}/cancel", h.cancelOrder, csrf)

	route("POST /orders/{id}/payment/initiate", h.initiatePayment, csrf)
	route("POST /orders/{id}/payment/confirm", h.confirmPayment, csrf)
	route("POST /orders/{id}/payment/fail", h.failPayment, csrf)
	mux.HandleFunc("POST /webhooks/razorpay", httpx.Wrap(h.webhook))

	read, refund := h.guards.Require("order:read:any"), h.guards.Require("order:refund")
	mux.HandleFunc("GET /admin/orders", httpx.Wrap(httpx.Chain(h.adminList, read)))
	mux.HandleFunc("GET /admin/orders/{id}", httpx.Wrap(httpx.Chain(h.adminGet, read)))
	mux.HandleFunc("PATCH /admin/orders/{id}/status", httpx.Wrap(httpx.Chain(h.adminAdvance, refund)))
	mux.HandleFunc("POST /admin/orders/{id}/notes", httpx.Wrap(httpx.Chain(h.adminNote, read)))
}

func (h *Handler) ownerKey(w http.ResponseWriter, r *http.Request) string {
	userID := ""
	if u := httpx.User(r); u != nil {
		userID = u.ID
	}
	return OwnerKey(userID, EnsureGuestToken(w, r, h.cfg.Production))
}

func (h *Handler) getCart(w http.ResponseWriter, r *http.Request) error {
	c, err := h.cart.Get(r.Context(), h.ownerKey(w, r))
	return respond(w, http.StatusOK, c, err)
}

func respond(w http.ResponseWriter, status int, v any, err error) error {
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, status, v)
	return nil
}

func (h *Handler) addItem(w http.ResponseWriter, r *http.Request) error {
	key := h.ownerKey(w, r)
	body := httpx.ReadObject(r, "variantId", "quantity")
	variantID, qty := body.Str("variantId", 0), body.Int("quantity", 1)
	if err := body.Err(); err != nil {
		return err
	}
	c, err := h.cart.Add(r.Context(), key, variantID, qty)
	return respond(w, http.StatusCreated, c, err)
}

func (h *Handler) setQuantity(w http.ResponseWriter, r *http.Request) error {
	key := h.ownerKey(w, r)
	body := httpx.ReadObject(r, "quantity")
	qty := body.Int("quantity", 0)
	if err := body.Err(); err != nil {
		return err
	}
	c, err := h.cart.SetQuantity(r.Context(), key, r.PathValue("variantId"), qty)
	return respond(w, http.StatusOK, c, err)
}

func (h *Handler) removeItem(w http.ResponseWriter, r *http.Request) error {
	c, err := h.cart.Remove(r.Context(), h.ownerKey(w, r), r.PathValue("variantId"))
	return respond(w, http.StatusOK, c, err)
}

func (h *Handler) applyCoupon(w http.ResponseWriter, r *http.Request) error {
	key := h.ownerKey(w, r)
	body := httpx.ReadObject(r, "code")
	code := body.Str("code", 0)
	if err := body.Err(); err != nil {
		return err
	}
	c, err := h.cart.ApplyCoupon(r.Context(), key, code)
	return respond(w, http.StatusCreated, c, err)
}

func (h *Handler) removeCoupon(w http.ResponseWriter, r *http.Request) error {
	c, err := h.cart.RemoveCoupon(r.Context(), h.ownerKey(w, r))
	return respond(w, http.StatusOK, c, err)
}

func (h *Handler) setShipping(w http.ResponseWriter, r *http.Request) error {
	key := h.ownerKey(w, r)
	body := httpx.ReadObject(r, "method")
	method := body.Enum("method", "standard", "express")
	if err := body.Err(); err != nil {
		return err
	}
	c, err := h.cart.SetShippingMethod(r.Context(), key, domain.ShippingMethodID(method))
	return respond(w, http.StatusOK, c, err)
}

func (h *Handler) clearCart(w http.ResponseWriter, r *http.Request) error {
	c, err := h.cart.Clear(r.Context(), h.ownerKey(w, r))
	return respond(w, http.StatusOK, c, err)
}

func (h *Handler) shippingOptions(w http.ResponseWriter, r *http.Request) error {
	pincode := r.PathValue("pincode")
	if err := assertDeliverable(pincode); err != nil {
		return err
	}
	cart, err := h.cart.Get(r.Context(), h.ownerKey(w, r))
	if err != nil {
		return err
	}
	base := domain.ComputeServiceability(pincode, time.Now())
	afterDiscount := cart.Totals.Subtotal.Amount - cart.Totals.CouponDiscount.Amount
	free := cart.Coupon != nil && cart.Coupon.FreeShipping
	standardDays := 3
	if base.EstimatedDays != nil {
		standardDays = *base.EstimatedDays
	}
	expressDays := max(1, standardDays-2)
	on := func(days int) string { return domain.ISO(time.Now().Add(time.Duration(days) * 24 * time.Hour)) }
	express := int64(domain.ExpressShipping)
	if free {
		express = 0
	}
	httpx.WriteJSON(w, http.StatusOK, []domain.ShippingOption{
		{ID: "standard", Label: "Standard delivery", Price: domain.INRInt(domain.ShippingFee("standard", afterDiscount, free)), EstimatedDays: standardDays, EstimatedDate: on(standardDays)},
		{ID: "express", Label: "Express delivery", Price: domain.INRInt(express), EstimatedDays: expressDays, EstimatedDate: on(expressDays)},
	})
	return nil
}

func (h *Handler) paymentOptions(w http.ResponseWriter, r *http.Request) error {
	pincode := r.PathValue("pincode")
	if err := assertDeliverable(pincode); err != nil {
		return err
	}
	cart, err := h.cart.Get(r.Context(), h.ownerKey(w, r))
	if err != nil {
		return err
	}
	enabled, reason := domain.CODEligibility(pincode, cart.Totals.Total.Amount)
	httpx.WriteJSON(w, http.StatusOK, []domain.PaymentOption{
		{Method: "razorpay", Label: "Pay online (UPI, cards, net banking, wallets)", Enabled: true},
		{Method: "cod", Label: "Cash on delivery", Enabled: enabled, Reason: reason},
	})
	return nil
}

func (h *Handler) placeOrder(w http.ResponseWriter, r *http.Request) error {
	guestToken := EnsureGuestToken(w, r, h.cfg.Production)
	user := httpx.User(r)
	userID := ""
	if user != nil {
		userID = user.ID
	}
	body := httpx.ReadObject(r, "idempotencyKey", "contact", "address", "paymentMethod")
	req := PlaceRequest{IdempotencyKey: body.Str("idempotencyKey", 100)}
	contact := body.Object("contact", "name", "email", "phone")
	req.Contact = domain.ContactDetails{Name: contact.Str("name", 120), Email: contact.Str("email", 200), Phone: contact.Str("phone", 20)}
	addr := body.Object("address", "line1", "line2", "city", "state", "pincode")
	req.Address.Line1 = addr.Str("line1", 200)
	req.Address.Line2, _ = addr.OptStr("line2", 200)
	req.Address.City, req.Address.State, req.Address.Pincode = addr.Str("city", 100), addr.Str("state", 100), addr.Str("pincode", 10)
	req.PaymentMethod = body.Enum("paymentMethod", "razorpay", "cod")
	body.Absorb(contact)
	body.Absorb(addr)
	if err := body.Err(); err != nil {
		return err
	}
	order, err := h.orders.Place(r.Context(), OwnerKey(userID, guestToken), Owner{UserID: userID, GuestToken: guestToken}, req)
	return respond(w, http.StatusCreated, order, err)
}

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) error {
	o, err := h.orders.Get(r.Context(), r.PathValue("id"), ownerOf(r))
	return respond(w, http.StatusOK, o, err)
}

func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request) error {
	list, err := h.orders.List(r.Context(), ownerOf(r))
	return respond(w, http.StatusOK, list, err)
}

func (h *Handler) cancelOrder(w http.ResponseWriter, r *http.Request) error {
	o, err := h.orders.Cancel(r.Context(), r.PathValue("id"), ownerOf(r))
	return respond(w, http.StatusOK, o, err)
}

// stream is real-time order tracking over Server-Sent Events. Ownership is checked before the stream opens (a
// non-owner gets the normal 404), and the current status is sent first so the page has something to show at once.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) error {
	current, err := h.orders.Get(r.Context(), r.PathValue("id"), ownerOf(r))
	if err != nil {
		return err
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return apperr.New(apperr.Unknown, "streaming unsupported")
	}
	events, stop := h.watch(r, current.ID)
	defer stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	first, _ := json.Marshal(current)
	fmt.Fprintf(w, "data: %s\n\n", first)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case msg, open := <-events:
			if !open {
				return nil
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

func (h *Handler) initiatePayment(w http.ResponseWriter, r *http.Request) error {
	s, err := h.payments.Initiate(r.Context(), r.PathValue("id"), ownerOf(r))
	return respond(w, http.StatusCreated, s, err)
}

func (h *Handler) confirmPayment(w http.ResponseWriter, r *http.Request) error {
	body := httpx.ReadObject(r, "providerPaymentId", "providerOrderId", "signature")
	res := PaymentResult{ProviderPaymentID: body.Str("providerPaymentId", 0), ProviderOrderID: body.Str("providerOrderId", 0), Signature: body.Str("signature", 0)}
	if err := body.Err(); err != nil {
		return err
	}
	o, err := h.payments.Confirm(r.Context(), r.PathValue("id"), ownerOf(r), res)
	return respond(w, http.StatusCreated, o, err)
}

func (h *Handler) failPayment(w http.ResponseWriter, r *http.Request) error {
	body := httpx.ReadObject(r, "reason")
	reason := body.Str("reason", 0)
	if err := body.Err(); err != nil {
		return err
	}
	o, err := h.payments.Fail(r.Context(), r.PathValue("id"), ownerOf(r), reason)
	return respond(w, http.StatusOK, o, err)
}

// webhook is called by Razorpay directly (never through a browser), so there is no session or CSRF concern: the
// signature, verified against the untouched raw body, is the only authentication. It answers 200 either way, so an
// attacker learns nothing about what a valid signature looks like.
func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 100*1024))
	signature := r.Header.Get("x-razorpay-signature")
	if err != nil || signature == "" || len(raw) == 0 || !h.razorpay.VerifyWebhookSignature(raw, signature) {
		httpx.Logger.Warn("Rejected a webhook call with a missing or invalid signature.", "logger", "RazorpayWebhook")
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}
	h.payments.HandleWebhook(r.Context(), raw)
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) error {
	get := func(k string) string { v, _ := httpx.Query(r, k); return v }
	status := get("status")
	if !in(status, "pending_payment", "confirmed", "packed", "shipped", "delivered", "cancelled") {
		status = ""
	}
	method := get("paymentMethod")
	if !in(method, "razorpay", "cod") {
		method = ""
	}
	out, err := h.admin.List(r.Context(), AdminOrderQuery{
		Q: get("q"), Status: status, PaymentMethod: method, From: get("from"), To: get("to"),
		Page: httpx.QueryInt(r, "page", 1), PageSize: min(200, httpx.QueryInt(r, "pageSize", 20)),
	})
	return respond(w, http.StatusOK, out, err)
}

func in(v string, options ...string) bool {
	for _, o := range options {
		if v == o {
			return true
		}
	}
	return false
}

func (h *Handler) adminGet(w http.ResponseWriter, r *http.Request) error {
	o, err := h.admin.Get(r.Context(), r.PathValue("id"))
	return respond(w, http.StatusOK, o, err)
}

func (h *Handler) adminAdvance(w http.ResponseWriter, r *http.Request) error {
	user, _ := httpx.MustUser(r)
	body := httpx.ReadObject(r, "status")
	status := body.Enum("status", "pending_payment", "confirmed", "packed", "shipped", "delivered", "cancelled")
	if err := body.Err(); err != nil {
		return err
	}
	o, err := h.admin.Advance(r.Context(), r.PathValue("id"), status, user.ID)
	return respond(w, http.StatusOK, o, err)
}

func (h *Handler) adminNote(w http.ResponseWriter, r *http.Request) error {
	user, _ := httpx.MustUser(r)
	body := httpx.ReadObject(r, "text")
	text := body.Str("text", 2000)
	if err := body.Err(); err != nil {
		return err
	}
	o, err := h.admin.AddNote(r.Context(), r.PathValue("id"), user.ID, text)
	return respond(w, http.StatusCreated, o, err)
}
