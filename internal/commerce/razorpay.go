package commerce

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
	"github.com/shubhamrathi020/eCommerce-go/internal/resilience"
)

const razorpayAPI = "https://api.razorpay.com/v1"

// Razorpay is the real payment integration, test mode only and gated on your own keys. With no keys set the API
// still starts and cash on delivery still works; online payment refuses with a clear message.
//
// Every provider call is timed out, retried once on a transient blip and counted by a circuit breaker: after three
// consecutive failures the breaker opens for 30 seconds and calls fail fast. Cash on delivery is a separate code
// path, so a Razorpay outage never blocks placing an order.
type Razorpay struct {
	cfg     *config.Config
	client  *http.Client
	breaker *resilience.Breaker
	baseURL string
}

// NewRazorpay builds the client.
func NewRazorpay(cfg *config.Config) *Razorpay {
	return &Razorpay{
		cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}, baseURL: razorpayAPI,
		breaker: resilience.NewBreaker("razorpay", resilience.Options{FailureThreshold: 3, Cooldown: 30 * time.Second, Timeout: 8 * time.Second}),
	}
}

// Enabled reports whether Razorpay keys are configured.
func (r *Razorpay) Enabled() bool { return r.cfg.RazorpayEnabled }

// BreakerStats reports the breaker state.
func (r *Razorpay) BreakerStats() resilience.Stats { return r.breaker.Stats() }

func (r *Razorpay) require() error {
	if !r.cfg.RazorpayEnabled {
		return apperr.New(apperr.Validation, "Online payment is not set up on this server yet. Please choose cash on delivery, or ask the site owner to add Razorpay test keys.")
	}
	return nil
}

func (r *Razorpay) guarded(ctx context.Context, fn func(context.Context) error) error {
	err := r.breaker.Exec(ctx, func(c context.Context) error { return resilience.Retry(c, 2, 300*time.Millisecond, fn) })
	if err != nil {
		httpx.Logger.Warn("razorpay call failed: "+err.Error(), "logger", "RazorpayService")
		return apperr.New(apperr.Network, "Online payment is temporarily unavailable. Please choose cash on delivery, or try again in a minute.")
	}
	return nil
}

func (r *Razorpay) post(ctx context.Context, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(r.cfg.RazorpayKeyID, r.cfg.RazorpayKeySecret)
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("razorpay %s: %d %s", path, res.StatusCode, string(data))
	}
	return json.Unmarshal(data, out)
}

// CreateOrder creates the provider-side order the checkout widget needs; receipt is our own order id.
func (r *Razorpay) CreateOrder(ctx context.Context, amountPaise int64, receipt string) (string, error) {
	if err := r.require(); err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	err := r.guarded(ctx, func(c context.Context) error {
		return r.post(c, "/orders", map[string]any{"amount": amountPaise, "currency": "INR", "receipt": receipt}, &out)
	})
	return out.ID, err
}

func safeEqual(expectedHex, actualHex string) bool {
	a, err1 := hex.DecodeString(expectedHex)
	b, err2 := hex.DecodeString(actualHex)
	return err1 == nil && err2 == nil && hmac.Equal(a, b)
}

// VerifyPaymentSignature checks the signature the checkout widget hands back: HMAC-SHA256 of
// "<order_id>|<payment_id>" keyed with the account secret. The secret never reaches the browser.
func (r *Razorpay) VerifyPaymentSignature(providerOrderID, providerPaymentID, signature string) bool {
	if r.cfg.RazorpayKeySecret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(r.cfg.RazorpayKeySecret))
	mac.Write([]byte(providerOrderID + "|" + providerPaymentID))
	return safeEqual(hex.EncodeToString(mac.Sum(nil)), signature)
}

// VerifyWebhookSignature checks the header Razorpay sends: HMAC-SHA256 of the raw body keyed with the webhook secret.
func (r *Razorpay) VerifyWebhookSignature(rawBody []byte, signature string) bool {
	if r.cfg.RazorpayWebhookSecret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(r.cfg.RazorpayWebhookSecret))
	mac.Write(rawBody)
	return safeEqual(hex.EncodeToString(mac.Sum(nil)), signature)
}

// Refund refunds a captured payment; amountPaise 0 refunds in full.
func (r *Razorpay) Refund(ctx context.Context, providerPaymentID string, amountPaise int64) error {
	if err := r.require(); err != nil {
		return err
	}
	body := map[string]any{}
	if amountPaise > 0 {
		body["amount"] = amountPaise
	}
	return r.guarded(ctx, func(c context.Context) error {
		var out map[string]any
		return r.post(c, "/payments/"+providerPaymentID+"/refund", body, &out)
	})
}
