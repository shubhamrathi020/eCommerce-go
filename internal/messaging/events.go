package messaging

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/shubhamrathi020/eCommerce-go/internal/cache"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// OrderEvents is real-time order tracking over Redis pub/sub, not RabbitMQ: a live, best-effort push to whichever
// browser tab has the tracking page open right now, not a durable event that must survive a broker restart. A tab
// that is not connected when a status changes simply sees the new status on its next GET /orders/:id.
type OrderEvents struct{ rdb *cache.Redis }

// NewOrderEvents builds the publisher.
func NewOrderEvents(rdb *cache.Redis) *OrderEvents { return &OrderEvents{rdb: rdb} }

func (e *OrderEvents) channel(orderID string) string { return e.rdb.Prefix + "order-events:" + orderID }

// Publish pushes an order's new state.
func (e *OrderEvents) Publish(ctx context.Context, order domain.Order) {
	b, err := json.Marshal(order)
	if err != nil {
		return
	}
	_ = e.rdb.Client.Publish(ctx, e.channel(order.ID), b).Err()
}

// Watch subscribes to an order's updates until stop is called or the request ends.
func (e *OrderEvents) Watch(r *http.Request, orderID string) (<-chan string, func()) {
	sub := e.rdb.Client.Subscribe(r.Context(), e.channel(orderID))
	out := make(chan string, 16)
	go func() {
		defer close(out)
		ch := sub.Channel()
		for msg := range ch {
			select {
			case out <- msg.Payload:
			default: // a slow reader misses an intermediate update rather than blocking everyone
			}
		}
	}()
	return out, func() { _ = sub.Close() }
}

// DeadLetterHandler serves the admin tool to inspect and replay messages that failed processing three times.
type DeadLetterHandler struct {
	rabbit *Rabbit
	guards *httpx.Guards
}

// NewDeadLetterHandler builds the handler.
func NewDeadLetterHandler(rabbit *Rabbit, guards *httpx.Guards) *DeadLetterHandler {
	return &DeadLetterHandler{rabbit: rabbit, guards: guards}
}

// Register mounts the routes.
func (h *DeadLetterHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/system/dead-letters", httpx.Wrap(httpx.Chain(h.list, h.guards.Require("system:read"))))
	mux.HandleFunc("POST /admin/system/dead-letters/replay", httpx.Wrap(httpx.Chain(h.replay, h.guards.Require("system:write"))))
}

func (h *DeadLetterHandler) list(w http.ResponseWriter, r *http.Request) error {
	limit := httpx.QueryInt(r, "limit", 20)
	if limit <= 0 {
		limit = 20
	}
	items, err := h.rabbit.Peek(h.rabbit.DLQ(), min(100, limit))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, items)
	return nil
}

// replay re-publishes the single oldest dead letter back onto the main exchange under its original routing key.
func (h *DeadLetterHandler) replay(w http.ResponseWriter, r *http.Request) error {
	key, body, found, err := h.rabbit.PopOldest(h.rabbit.DLQ())
	if err != nil {
		return err
	}
	if !found {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"replayed": false})
		return nil
	}
	if err := h.rabbit.Publish(r.Context(), key, body, PublishOptions{}); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"replayed": true, "routingKey": key})
	return nil
}
