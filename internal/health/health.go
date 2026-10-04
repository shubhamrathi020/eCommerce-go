// Package health is liveness and readiness for Docker and Kubernetes probes, plus the development mail outbox.
package health

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/auth"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// Check pings one dependency.
type Check func(ctx context.Context) error

// Handler serves /healthz, /readyz and /dev/outbox.
type Handler struct {
	cfg    *config.Config
	mail   *auth.Mail
	checks []Check
}

// NewHandler builds the handler; checks are the dependencies /readyz waits for.
func NewHandler(cfg *config.Config, mail *auth.Mail, checks ...Check) *Handler {
	return &Handler{cfg: cfg, mail: mail, checks: checks}
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// The process is up. Never touches dependencies, so an outage does not restart every pod.
		httpx.WriteText(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", httpx.Wrap(h.readyz))
	mux.HandleFunc("GET /dev/outbox", httpx.Wrap(h.outbox))
}

// readyz is ready for traffic only when every store the API depends on answers.
func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	failed := make(chan error, len(h.checks))
	for _, c := range h.checks {
		wg.Add(1)
		go func(c Check) {
			defer wg.Done()
			if err := c(ctx); err != nil {
				failed <- err
			}
		}(c)
	}
	wg.Wait()
	close(failed)
	if len(failed) > 0 {
		return &httpx.HTTPError{Status: http.StatusServiceUnavailable, Message: "a dependency is unavailable"}
	}
	httpx.WriteText(w, http.StatusOK, "ok")
	return nil
}

// outbox lists the mails the API would have sent. 404 in production.
func (h *Handler) outbox(w http.ResponseWriter, r *http.Request) error {
	if h.cfg.Production {
		return &httpx.HTTPError{Status: http.StatusNotFound, Message: "Not found"}
	}
	httpx.WriteJSON(w, http.StatusOK, h.mail.List())
	return nil
}
