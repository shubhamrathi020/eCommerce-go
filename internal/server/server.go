// Package server is the composition root: it builds every module, wires them together and returns the HTTP
// handler. It is the Go counterpart of the Nest AppModule.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/accounts"
	"github.com/shubhamrathi020/eCommerce-go/internal/addresses"
	"github.com/shubhamrathi020/eCommerce-go/internal/auth"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/health"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// App is the running application.
type App struct {
	Config  *config.Config
	Handler http.Handler
	closers []func()
}

// Close releases every connection.
func (a *App) Close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
}

// New loads the configuration, connects to every store and builds the handler.
func New(ctx context.Context) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return Build(ctx, cfg)
}

// Build wires the application for a given configuration (tests call this directly).
func Build(ctx context.Context, cfg *config.Config) (*App, error) {
	app := &App{Config: cfg}

	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	app.closers = append(app.closers, store.Close)

	guards := httpx.NewGuards(cfg)
	throttle := httpx.NewThrottle(cfg.RateLimit)

	mail := auth.NewMail(cfg.Production)
	passwords := &auth.Passwords{}
	tokens := auth.NewTokens(cfg, store)
	authService := auth.NewService(store, passwords, tokens, mail)

	mux := http.NewServeMux()
	auth.NewHandler(authService, cfg, guards, throttle, nil).Register(mux)
	accounts.NewHandler(accounts.NewService(store, passwords, tokens), cfg, guards).Register(mux)
	addresses.NewHandler(addresses.NewService(store), guards).Register(mux)
	health.NewHandler(cfg, mail, store.Ping).Register(mux)
	mux.HandleFunc("/", httpx.NotFound)

	var h http.Handler = mux
	h = throttle.Global(cfg.RateLimits.Global.Limit, time.Duration(cfg.RateLimits.Global.WindowSeconds)*time.Second)(h)
	h = httpx.CORS(cfg.CORSOrigins)(h)
	h = httpx.SecurityHeaders()(h)
	h = httpx.RequestContext(nil)(h)
	app.Handler = h
	return app, nil
}
