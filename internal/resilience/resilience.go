// Package resilience wraps calls to external dependencies (Razorpay, Meilisearch) in a timeout, a retry and a
// circuit breaker, so a struggling dependency fails fast and clearly instead of hanging every request behind it.
package resilience

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
)

// ErrCircuitOpen is returned without attempting the call while the circuit is open.
var ErrCircuitOpen = errors.New("circuit is open")

// ErrTimeout is returned when a call does not settle in time.
var ErrTimeout = errors.New("timed out")

// Options configure a breaker.
type Options struct {
	FailureThreshold int
	Cooldown         time.Duration
	Timeout          time.Duration
}

// Stats is a breaker's current state for the admin resilience endpoint and the metrics gauge.
type Stats struct {
	Name                string `json:"name"`
	State               string `json:"state"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	LastError           string `json:"lastError,omitempty"`
	OpenedAt            string `json:"openedAt,omitempty"`
}

// Breaker is a textbook circuit breaker: closed (normal) -> open after too many consecutive failures (calls are
// rejected immediately) -> after the cooldown, half-open (exactly one probe is allowed; success closes it again,
// failure reopens it for another full cooldown).
type Breaker struct {
	name string
	opts Options

	mu          sync.Mutex
	state       string
	failures    int
	lastError   string
	openedAt    time.Time
	probeFlight bool
}

// NewBreaker builds a closed breaker.
func NewBreaker(name string, opts Options) *Breaker {
	return &Breaker{name: name, opts: opts, state: "closed"}
}

// Stats reports the breaker's state.
func (b *Breaker) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Stats{Name: b.name, State: b.state, ConsecutiveFailures: b.failures, LastError: b.lastError}
	if b.state == "open" {
		s.OpenedAt = domain.ISO(b.openedAt)
	}
	return s
}

// Exec runs fn under the breaker, racing it against the timeout.
func (b *Breaker) Exec(ctx context.Context, fn func(context.Context) error) error {
	b.mu.Lock()
	if b.state == "open" {
		if time.Since(b.openedAt) < b.opts.Cooldown || b.probeFlight {
			b.mu.Unlock()
			return fmt.Errorf("circuit %q: %w", b.name, ErrCircuitOpen)
		}
		// Cooldown elapsed: let exactly one caller probe; everyone else is still rejected until it settles.
		b.state = "half_open"
		b.probeFlight = true
	}
	b.mu.Unlock()

	tctx, cancel := context.WithTimeout(ctx, b.opts.Timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(tctx) }()
	var err error
	select {
	case err = <-done:
	case <-tctx.Done():
		err = fmt.Errorf("%q timed out after %s: %w", b.name, b.opts.Timeout, ErrTimeout)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeFlight = false
	if err == nil {
		b.failures = 0
		b.lastError = ""
		b.state = "closed"
		return nil
	}
	b.failures++
	b.lastError = err.Error()
	if b.state == "half_open" || b.failures >= b.opts.FailureThreshold {
		b.state = "open"
		b.openedAt = time.Now()
	}
	return err
}

// Retry runs fn up to attempts times with exponential backoff, for a call that may have failed on a transient
// blip. It is used inside one breaker attempt, so a blip counts as one failure toward the breaker, not several.
func Retry(ctx context.Context, attempts int, base time.Duration, fn func(context.Context) error) error {
	var last error
	for i := 0; i < attempts; i++ {
		if last = fn(ctx); last == nil {
			return nil
		}
		if i < attempts-1 {
			select {
			case <-time.After(base * time.Duration(1<<i)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return last
}
