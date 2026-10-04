// Package messaging is RabbitMQ and everything that rides on it: the transactional-outbox relay, the notification
// consumer with retry and dead-lettering, scheduled jobs and live order-tracking events.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/shubhamrathi020/eCommerce-go/internal/config"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// Rabbit is the one RabbitMQ connection the API uses. It declares the whole topology once at boot: exchange, retry
// queue, dead-letter exchange and queue, and the notifications queue, so every other service just publishes or
// consumes by name.
//
// Retry-with-backoff needs no delayed-message plugin: a message that failed is republished to the retry queue with a
// per-message expiration. That queue's own x-dead-letter-exchange points back at the events exchange, so once the TTL
// elapses RabbitMQ itself redelivers the message to the original queue. After the maximum attempts the message goes
// to the dead-letter queue instead, for the admin tool to inspect and replay.
type Rabbit struct {
	cfg       *config.Config
	conn      *amqp.Connection
	pub       *amqp.Channel
	cons      *amqp.Channel
	connected atomic.Bool
	pubMu     sync.Mutex
	consMu    sync.Mutex
}

// OpenRabbit connects and declares the topology.
func OpenRabbit(cfg *config.Config) (*Rabbit, error) {
	conn, err := amqp.Dial(cfg.RabbitMQURL)
	if err != nil {
		return nil, err
	}
	r := &Rabbit{cfg: cfg, conn: conn}
	if r.pub, err = conn.Channel(); err != nil {
		return nil, err
	}
	if err = r.pub.Confirm(false); err != nil {
		return nil, err
	}
	if r.cons, err = conn.Channel(); err != nil {
		return nil, err
	}
	if err = r.declare(); err != nil {
		return nil, err
	}
	r.connected.Store(true)
	go func() {
		if cerr := <-conn.NotifyClose(make(chan *amqp.Error, 1)); cerr != nil {
			r.connected.Store(false)
			httpx.Logger.Error("connection error: "+cerr.Error(), "logger", "Rabbit")
		} else {
			r.connected.Store(false)
		}
	}()
	return r, nil
}

func (r *Rabbit) name(suffix string) string { return r.cfg.MQPrefix + suffix }

// Names of the topology.
func (r *Rabbit) Events() string             { return r.name("events") }
func (r *Rabbit) DLX() string                { return r.name("dlx") }
func (r *Rabbit) DLQ() string                { return r.name("q.dlq") }
func (r *Rabbit) RetryQueue() string         { return r.name("q.retry") }
func (r *Rabbit) NotificationsQueue() string { return r.name("q.notifications") }

func (r *Rabbit) declare() error {
	ch := r.pub
	if err := ch.ExchangeDeclare(r.Events(), "topic", true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.ExchangeDeclare(r.DLX(), "topic", true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(r.DLQ(), true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.QueueBind(r.DLQ(), "#", r.DLX(), false, nil); err != nil {
		return err
	}
	// No queue-level TTL: each retry publish sets its own expiration for a per-attempt backoff instead.
	if _, err := ch.QueueDeclare(r.RetryQueue(), true, false, false, false, amqp.Table{"x-dead-letter-exchange": r.Events()}); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(r.NotificationsQueue(), true, false, false, false, nil); err != nil {
		return err
	}
	for _, key := range []string{"order.*", "payment.*", "cart.*"} {
		if err := ch.QueueBind(r.NotificationsQueue(), key, r.Events(), false, nil); err != nil {
			return err
		}
	}
	return nil
}

// Ping checks the connection's own last known state, deliberately not a round trip: a failing passive declare
// closes the channel, breaking real publishing just to answer a health probe.
func (r *Rabbit) Ping(ctx context.Context) error {
	if !r.connected.Load() || r.conn.IsClosed() {
		return errors.New("RabbitMQ connection is not open")
	}
	return nil
}

// Close shuts the connection.
func (r *Rabbit) Close() { _ = r.conn.Close() }

// PublishOptions are the optional parts of a publish.
type PublishOptions struct {
	MessageID  string
	Headers    amqp.Table
	Expiration time.Duration
}

func (r *Rabbit) publish(ctx context.Context, exchange, key string, payload any, opts PublishOptions) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	msg := amqp.Publishing{DeliveryMode: amqp.Persistent, ContentType: "application/json", Body: body, MessageId: opts.MessageID, Headers: opts.Headers}
	if opts.Expiration > 0 {
		msg.Expiration = fmt.Sprintf("%d", opts.Expiration.Milliseconds())
	}
	r.pubMu.Lock()
	conf, err := r.pub.PublishWithDeferredConfirmWithContext(ctx, exchange, key, false, false, msg)
	r.pubMu.Unlock()
	if err != nil {
		return err
	}
	// A publisher confirm: resolves only once the broker has acknowledged the message, which is what lets the outbox
	// relay mark a row published only when it is genuinely safe to.
	ok, err := conf.WaitContext(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("broker did not confirm the message")
	}
	return nil
}

// Publish sends to the events exchange with a publisher confirm.
func (r *Rabbit) Publish(ctx context.Context, routingKey string, payload any, opts PublishOptions) error {
	return r.publish(ctx, r.Events(), routingKey, payload, opts)
}

// PublishToQueue sends straight to one queue by name (the retry queue, or the dead-letter queue).
func (r *Rabbit) PublishToQueue(ctx context.Context, queue, routingKey string, payload any, opts PublishOptions) error {
	headers := amqp.Table{}
	for k, v := range opts.Headers {
		headers[k] = v
	}
	headers["x-original-routing-key"] = routingKey
	opts.Headers = headers
	return r.publish(ctx, "", queue, payload, opts)
}

// Consume starts a consumer; each delivery is handled concurrently up to the prefetch.
func (r *Rabbit) Consume(queue string, prefetch int, handle func(amqp.Delivery)) error {
	r.consMu.Lock()
	defer r.consMu.Unlock()
	if err := r.cons.Qos(prefetch, 0, false); err != nil {
		return err
	}
	deliveries, err := r.cons.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	go func() {
		for d := range deliveries {
			go handle(d)
		}
	}()
	return nil
}

// DeadLetter is one entry of the dead-letter queue.
type DeadLetter struct {
	RoutingKey string `json:"routingKey"`
	Body       any    `json:"body"`
	DeadAt     string `json:"deadAt,omitempty"`
	RetryCount int    `json:"retryCount"`
	LastError  string `json:"lastError,omitempty"`
}

func headerInt(h amqp.Table, key string) int {
	switch v := h[key].(type) {
	case int:
		return v
	case int8:
		return int(v)
	case int16:
		return int(v)
	case int32:
		return int(v)
	case int64:
		return int(v)
	case uint8:
		return int(v)
	case uint16:
		return int(v)
	case uint32:
		return int(v)
	case float32:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func routingKeyOf(d amqp.Delivery) string {
	if s, ok := d.Headers["x-original-routing-key"].(string); ok && s != "" {
		return s
	}
	return d.RoutingKey
}

// Peek is a non-destructive look at up to limit messages: each is pulled and immediately nacked back with requeue.
// A concurrent peek can reorder the queue slightly (basic.get has no dedicated browse mode): fine for an inspection
// tool, not for exactly-once processing.
func (r *Rabbit) Peek(queue string, limit int) ([]DeadLetter, error) {
	r.consMu.Lock()
	defer r.consMu.Unlock()
	out := []DeadLetter{}
	for i := 0; i < limit; i++ {
		d, ok, err := r.cons.Get(queue, false)
		if err != nil {
			return out, err
		}
		if !ok {
			break
		}
		var body any
		_ = json.Unmarshal(d.Body, &body)
		item := DeadLetter{RoutingKey: routingKeyOf(d), Body: body, RetryCount: headerInt(d.Headers, "x-retry-count")}
		if s, ok := d.Headers["x-last-error"].(string); ok {
			item.LastError = s
		}
		if !d.Timestamp.IsZero() {
			item.DeadAt = d.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		out = append(out, item)
		_ = d.Nack(false, true)
	}
	return out, nil
}

// PopOldest takes the single oldest message off the queue (permanently) for the caller to republish elsewhere.
func (r *Rabbit) PopOldest(queue string) (routingKey string, body any, found bool, err error) {
	r.consMu.Lock()
	defer r.consMu.Unlock()
	d, ok, err := r.cons.Get(queue, false)
	if err != nil || !ok {
		return "", nil, false, err
	}
	_ = d.Ack(false)
	_ = json.Unmarshal(d.Body, &body)
	return routingKeyOf(d), body, true, nil
}
