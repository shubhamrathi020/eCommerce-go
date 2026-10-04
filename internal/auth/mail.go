package auth

import (
	"sync"
	"time"

	"github.com/shubhamrathi020/eCommerce-go/internal/domain"
	"github.com/shubhamrathi020/eCommerce-go/internal/httpx"
)

// OutboxMail is one captured email.
type OutboxMail struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Link    string `json:"link,omitempty"`
	SentAt  string `json:"sentAt"`
}

const maxMail = 50

// Mail is the stand-in for real email delivery: mails are kept in memory and, outside production, readable at
// GET /dev/outbox. Links are never logged in production (they grant access).
type Mail struct {
	production bool
	mu         sync.Mutex
	outbox     []OutboxMail
}

// NewMail builds the dev mailer.
func NewMail(production bool) *Mail { return &Mail{production: production} }

// Send queues a mail.
func (m *Mail) Send(mail OutboxMail) {
	mail.SentAt = domain.ISO(time.Now())
	m.mu.Lock()
	m.outbox = append([]OutboxMail{mail}, m.outbox...)
	if len(m.outbox) > maxMail {
		m.outbox = m.outbox[:maxMail]
	}
	m.mu.Unlock()
	if m.production {
		httpx.Logger.Info(`{"event":"mail.queued","subject":"`+mail.Subject+`"}`, "logger", "Mail")
	} else {
		httpx.Logger.Info(`{"event":"mail.queued","subject":"`+mail.Subject+`","to":"`+mail.To+`","link":"`+mail.Link+`"}`, "logger", "Mail")
	}
}

// List returns the captured mails, newest first.
func (m *Mail) List() []OutboxMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]OutboxMail, len(m.outbox))
	copy(out, m.outbox)
	return out
}
