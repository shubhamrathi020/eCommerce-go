// Command api is the shop's Go backend. It serves the same HTTP API as the NestJS server, against the same
// PostgreSQL, MongoDB, Meilisearch, Redis and RabbitMQ.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/server"
)

func main() {
	// `api -healthcheck` asks the running server's /healthz and exits 0 or 1: the container HEALTHCHECK, since the
	// runtime image has no shell or wget.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := server.New(ctx)
	if err != nil {
		return err
	}
	defer app.Close()

	srv := &http.Server{Addr: ":" + app.Config.Port, Handler: app.Handler, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		fmt.Printf("API listening on port %s\n", app.Config.Port)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
	return nil
}

func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3333"
	}
	client := http.Client{Timeout: 3 * time.Second}
	res, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil || res.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
