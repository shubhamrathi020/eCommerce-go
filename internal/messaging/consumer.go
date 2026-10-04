package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/shubhamrathi020/eCommerce-go/internal/auth"
	"github.com/shubhamrathi020/eCommerce-go/internal/cache"
	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// Attempt 1/2/3 wait 2s/8s/20s in the retry queue before coming back; the 4th failure goes to the DLQ.
var retryDelays = []time.Duration{2 * time.Second, 8 * time.Second, 20 * time.Second}

const dedupeTTL = 7 * 24 * time.Hour

// Consumer is the one consumer of the notifications queue: it turns an outbox event into an email through the dev
// mailer. Swapping in a real provider later is a change to the mailer alone; this consumer, the outbox and the
// retry/DLQ plumbing do not know the current "provider" is an in-memory dev outbox.
//
// Idempotency: each event's own outbox-row id is the AMQP messageId, and a Redis SET NX on that id is the dedupe
// check: a message redelivered after a crash between "send the mail" and "ack" (or one replayed from the dead-letter
// queue after having succeeded once) is a no-op the second time, not a second email.
type Consumer struct {
	rabbit *Rabbit
	mail   *auth.Mail
	rdb    *cache.Redis
}

// NewConsumer builds the consumer.
func NewConsumer(rabbit *Rabbit, mail *auth.Mail, rdb *cache.Redis) *Consumer {
	return &Consumer{rabbit: rabbit, mail: mail, rdb: rdb}
}

// Start begins consuming.
func (c *Consumer) Start() error {
	return c.rabbit.Consume(c.rabbit.NotificationsQueue(), 5, c.handle)
}

func (c *Consumer) handle(d amqp.Delivery) {
	ctx := context.Background()
	if d.MessageId != "" {
		isNew, err := c.rdb.Client.SetNX(ctx, c.rdb.Key("dedupe:"+d.MessageId), "1", dedupeTTL).Result()
		if err == nil && !isNew {
			_ = d.Ack(false)
			return
		}
	}
	if err := c.deliver(d.RoutingKey, d.Body); err != nil {
		c.retryOrDeadLetter(ctx, d, err)
		return
	}
	_ = d.Ack(false)
}

func (c *Consumer) retryOrDeadLetter(ctx context.Context, d amqp.Delivery, cause error) {
	retryCount := headerInt(d.Headers, "x-retry-count")
	var body any
	_ = json.Unmarshal(d.Body, &body)
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if retryCount < len(retryDelays) {
		httpx.Logger.Warn(fmt.Sprintf("%s failed (attempt %d): %s; retrying in %s", d.RoutingKey, retryCount+1, cause, retryDelays[retryCount]), "logger", "NotificationConsumer")
		_ = c.rabbit.PublishToQueue(ctx, c.rabbit.RetryQueue(), d.RoutingKey, body, PublishOptions{Headers: amqp.Table{"x-retry-count": int32(retryCount + 1), "x-last-error": msg}, Expiration: retryDelays[retryCount]})
	} else {
		httpx.Logger.Error(fmt.Sprintf("%s failed permanently after %d retries: %s; moving to dead-letter queue", d.RoutingKey, retryCount, cause), "logger", "NotificationConsumer")
		_ = c.rabbit.PublishToQueue(ctx, c.rabbit.DLQ(), d.RoutingKey, body, PublishOptions{Headers: amqp.Table{"x-retry-count": int32(retryCount), "x-last-error": msg}})
	}
	_ = d.Ack(false)
}

type payload struct {
	OrderID   string       `json:"orderId"`
	Email     string       `json:"email"`
	Name      string       `json:"name"`
	Total     domain.Money `json:"total"`
	Amount    domain.Money `json:"amount"`
	ItemCount int          `json:"itemCount"`
	COD       bool         `json:"cod"`
	ByStaff   bool         `json:"byStaff"`
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (c *Consumer) deliver(routingKey string, raw []byte) error {
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	link := "/orders/" + p.OrderID
	switch routingKey {
	case "order.placed":
		paid := ""
		if p.COD {
			paid = " to be paid on delivery"
		}
		c.mail.Send(auth.OutboxMail{To: p.Email, Subject: "Order confirmed: " + p.OrderID, Link: link,
			Body: fmt.Sprintf("Hi %s, we've received your order %s (%d item%s, %s)%s. We'll email you again once it ships.", p.Name, p.OrderID, p.ItemCount, plural(p.ItemCount), domain.FormatMoney(p.Total), paid)})
	case "order.cancelled":
		by := ""
		if p.ByStaff {
			by = " by our team"
		}
		c.mail.Send(auth.OutboxMail{To: p.Email, Subject: "Order cancelled: " + p.OrderID, Link: link,
			Body: fmt.Sprintf("Hi %s, your order %s has been cancelled%s. Any payment taken will be refunded.", p.Name, p.OrderID, by)})
	case "order.expired":
		c.mail.Send(auth.OutboxMail{To: p.Email, Subject: "Order cancelled: " + p.OrderID, Link: link,
			Body: fmt.Sprintf("Hi %s, we cancelled order %s because payment wasn't completed in time. No charge was made; please place it again if you'd still like it.", p.Name, p.OrderID)})
	case "payment.confirmed":
		c.mail.Send(auth.OutboxMail{To: p.Email, Subject: "Payment received for order " + p.OrderID, Link: link,
			Body: fmt.Sprintf("Hi %s, we've received your payment of %s for order %s.", p.Name, domain.FormatMoney(p.Amount), p.OrderID)})
	case "payment.refunded":
		c.mail.Send(auth.OutboxMail{To: p.Email, Subject: "Refund issued for order " + p.OrderID, Link: link,
			Body: fmt.Sprintf("Hi %s, we've issued a refund of %s for order %s. It should reach your original payment method within a few business days.", p.Name, domain.FormatMoney(p.Amount), p.OrderID)})
	case "cart.abandoned":
		c.mail.Send(auth.OutboxMail{To: p.Email, Subject: "You left something in your cart", Link: "/cart",
			Body: fmt.Sprintf("Hi %s, you still have %d item%s waiting in your cart. Come back and finish checking out whenever you're ready.", p.Name, p.ItemCount, plural(p.ItemCount))})
	default:
		httpx.Logger.Warn(fmt.Sprintf(`no handler for routing key "%s"; dropping (acked)`, routingKey), "logger", "NotificationConsumer")
	}
	return nil
}
