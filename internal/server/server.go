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
	"github.com/shubhamrathi020/eCommerce-go/internal/cache"
	"github.com/shubhamrathi020/eCommerce-go/internal/catalog"
	"github.com/shubhamrathi020/eCommerce-go/internal/commerce"
	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/health"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
	"github.com/shubhamrathi020/eCommerce-go/internal/messaging"
	"github.com/shubhamrathi020/eCommerce-go/internal/metrics"
	"github.com/shubhamrathi020/eCommerce-go/internal/resilience"
)

// App is the running application.
type App struct {
	Config  *config.Config
	Handler http.Handler
	closers []func()
	cancel  context.CancelFunc
}

// Close stops the background jobs and releases every connection.
func (a *App) Close() {
	if a.cancel != nil {
		a.cancel()
	}
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
	fail := func(err error) (*App, error) {
		app.Close()
		return nil, err
	}

	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fail(err)
	}
	app.closers = append(app.closers, store.Close)
	rdb, err := cache.OpenRedis(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	app.closers = append(app.closers, rdb.Close)
	mongo, err := catalog.OpenMongo(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	app.closers = append(app.closers, mongo.Close)
	rabbit, err := messaging.OpenRabbit(cfg)
	if err != nil {
		return fail(err)
	}
	app.closers = append(app.closers, rabbit.Close)

	guards := httpx.NewGuards(cfg)
	throttle := httpx.NewThrottle(cfg.RateLimit)
	appCache := cache.New(rdb)
	limiter := cache.NewRateLimiter(rdb, appCache, cfg)
	coupons := cache.NewCoupons(rdb)
	search := catalog.NewSearch(cfg)
	razorpay := commerce.NewRazorpay(cfg)

	mail := auth.NewMail(cfg.Production)
	passwords := &auth.Passwords{}
	tokens := auth.NewTokens(cfg, store)
	authService := auth.NewService(store, passwords, tokens, mail)

	events := messaging.NewOrderEvents(rdb)
	cart := commerce.NewCart(store, mongo)
	inventory := commerce.NewInventory(mongo, appCache)
	orders := commerce.NewOrders(store, cart, inventory, coupons, events)
	payments := commerce.NewPayments(store, orders, cart, inventory, razorpay, events, cfg)
	adminOrders := commerce.NewAdminOrders(store, orders, inventory, razorpay, coupons, events)

	breakers := func() []resilience.Stats { return []resilience.Stats{razorpay.BreakerStats(), search.BreakerStats()} }
	m := metrics.New(appCache, store, breakers)

	mux := http.NewServeMux()
	auth.NewHandler(authService, cfg, guards, throttle, cart).Register(mux)
	accounts.NewHandler(accounts.NewService(store, passwords, tokens), cfg, guards).Register(mux)
	addresses.NewHandler(addresses.NewService(store), guards).Register(mux)
	catalog.NewHandler(catalog.NewService(mongo, search, appCache), mongo, search, limiter.Bucket("search")).Register(mux)
	catalog.NewAdminHandler(catalog.NewAdmin(mongo, search, appCache), guards).Register(mux)
	commerce.NewHandler(cfg, guards, cart, orders, payments, razorpay, adminOrders, limiter.Bucket, events.Watch).Register(mux)
	cache.NewStatsHandler(appCache, guards, func() []any {
		out := []any{}
		for _, s := range breakers() {
			out = append(out, s)
		}
		return out
	}).Register(mux)
	messaging.NewDeadLetterHandler(rabbit, guards).Register(mux)
	health.NewHandler(cfg, mail, store.Ping, mongo.Ping, search.Health, rdb.Ping, rabbit.Ping).Register(mux)
	m.Register(mux)
	mux.HandleFunc("/", httpx.NotFound)

	// Background work: the outbox relay, the notification consumer and the scheduled jobs.
	bg, cancel := context.WithCancel(context.Background())
	app.cancel = cancel
	consumer := messaging.NewConsumer(rabbit, mail, rdb)
	if err := consumer.Start(); err != nil {
		return fail(err)
	}
	go messaging.NewRelay(store, rabbit, rdb).Run(bg)
	go messaging.NewScheduler(store, rdb, orders, cfg).Run(bg)

	var h http.Handler = mux
	h = throttle.Global(cfg.RateLimits.Global.Limit, time.Duration(cfg.RateLimits.Global.WindowSeconds)*time.Second)(h)
	h = httpx.CORS(cfg.CORSOrigins)(h)
	h = httpx.SecurityHeaders()(h)
	h = httpx.RequestContext(m.Observe)(h)
	app.Handler = h
	return app, nil
}
