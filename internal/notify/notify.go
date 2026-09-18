// Package notify delivers findings. Delivery is deliberately pluggable: the
// Feishu custom bot pushes alerts directly, while the OpenClaw integration only
// drops files that the assistant reads over loopback HTTP.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

// Kind selects the message template.
type Kind string

const (
	KindAlert  Kind = "alert"
	KindDigest Kind = "digest"
	KindTest   Kind = "test"
	KindError  Kind = "error"
)

// Message is one delivery: a single deal alert or a batched digest.
type Message struct {
	Kind      Kind
	Title     string
	Intro     string
	Deals     []model.Deal
	CreatedAt time.Time
}

// NewMessage builds a message with a timestamp.
func NewMessage(k Kind, title string, deals ...model.Deal) Message {
	return Message{Kind: k, Title: title, Deals: deals, CreatedAt: time.Now()}
}

// Notifier delivers one message.
type Notifier interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// FanOut sends to every backend and reports the joined failures, so a dead
// webhook cannot stop the collector loop.
type FanOut struct {
	items []Notifier
	log   *slog.Logger
}

// NewFanOut wraps the given notifiers.
func NewFanOut(log *slog.Logger, items ...Notifier) *FanOut {
	if log == nil {
		log = slog.Default()
	}
	return &FanOut{items: items, log: log}
}

// Names lists the configured backends.
func (f *FanOut) Names() []string {
	out := make([]string, 0, len(f.items))
	for _, n := range f.items {
		out = append(out, n.Name())
	}
	return out
}

// Len reports how many backends are wired up.
func (f *FanOut) Len() int { return len(f.items) }

// Send delivers m to all backends.
func (f *FanOut) Send(ctx context.Context, m Message) error {
	if len(f.items) == 0 {
		return errors.New("notify: no backends configured")
	}
	var errs []error
	for _, n := range f.items {
		c, cancel := context.WithTimeout(ctx, 25*time.Second)
		err := n.Send(c, m)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n.Name(), err))
			f.log.Warn("notify: delivery failed", "backend", n.Name(), "kind", m.Kind, "err", err)
			continue
		}
		f.log.Info("notify: delivered", "backend", n.Name(), "kind", m.Kind, "items", len(m.Deals))
	}
	return errors.Join(errs...)
}

// Plain renders a human readable form used by the console and file backends.
func (m Message) Plain() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s", strings.ToUpper(string(m.Kind)), m.Title)
	if m.Intro != "" {
		b.WriteString(" — " + m.Intro)
	}
	b.WriteString("\n")
	for i := range m.Deals {
		b.WriteString("  " + Line(&m.Deals[i]) + "\n")
	}
	return b.String()
}

// Line renders one deal as a single compact line.
func Line(d *model.Deal) string {
	parts := []string{fmt.Sprintf("%s%d", Icon(d), d.Score)}
	if len(d.Vendors) > 0 {
		parts = append(parts, d.Vendors[0])
	}
	parts = append(parts, d.Title)
	if d.URL != "" {
		parts = append(parts, d.URL)
	}
	return strings.Join(parts, " | ")
}

// Icon maps a category to an emoji used in cards and digests.
func Icon(d *model.Deal) string {
	if d.IsFree {
		return "🆓"
	}
	switch d.Category {
	case model.CatAIFree:
		return "🤖"
	case model.CatDiscount:
		return "🏷️"
	case model.CatResource:
		return "📦"
	}
	return "🔎"
}

// Headline builds the card/digest title for a single-deal alert.
func Headline(d *model.Deal) string {
	if d.IsFree {
		return "免费羊毛 · " + truncateRunes(d.Title, 60)
	}
	if d.DiscountPct > 0 {
		return fmt.Sprintf("%d%% 折扣 · %s", d.DiscountPct, truncateRunes(d.Title, 56))
	}
	return truncateRunes(d.Title, 64)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
