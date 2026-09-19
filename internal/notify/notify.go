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
	"github.com/xiabee/deal-hunter/internal/official"
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

// Send delivers m to all backends. Failure semantics matter: a finding counts
// as delivered if ANY backend accepted it, so a broken secondary channel (for
// example a relay awaiting credentials) cannot make alerts re-fire every round
// or pile up in the digest. Only an all-backend failure is reported.
func (f *FanOut) Send(ctx context.Context, m Message) error {
	if len(f.items) == 0 {
		return errors.New("notify: no backends configured")
	}
	var errs []error
	ok := 0
	for _, n := range f.items {
		c, cancel := context.WithTimeout(ctx, 25*time.Second)
		err := n.Send(c, m)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n.Name(), err))
			f.log.Warn("notify: delivery failed", "backend", n.Name(), "kind", m.Kind, "err", err)
			continue
		}
		ok++
		f.log.Info("notify: delivered", "backend", n.Name(), "kind", m.Kind, "items", len(m.Deals))
	}
	if ok > 0 {
		if len(errs) > 0 {
			f.log.Warn("notify: partially delivered", "ok", ok, "failed", len(errs),
				"backend_total", len(f.items), "err", errors.Join(errs...))
		}
		return nil
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
	if best, _, _ := LinkFor(d); best != "" {
		parts = append(parts, best)
	}
	if badge := LinkBadge(d.Meta[official.MetaLinkKind]); badge != "" {
		parts = append(parts, badge)
	}
	return strings.Join(parts, " | ")
}

// LinkFor returns the URL worth clicking, the post it came from, and how the
// link was classified. Deals collected before resolution simply point at
// themselves.
func LinkFor(d *model.Deal) (best, original, kind string) {
	kind = d.Meta[official.MetaLinkKind]
	best, original = d.URL, d.URL
	if u := d.Meta[official.MetaOfficialURL]; u != "" {
		best = u
	}
	if u := d.Meta[official.MetaOriginalURL]; u != "" {
		original = u
	}
	return best, original, kind
}

// LinkBadge tells the user how much to trust the link, so a third-party retelling
// never looks like a confirmed vendor announcement.
func LinkBadge(kind string) string {
	switch kind {
	case official.KindAlreadyOfficial:
		return "✅ 厂商官方域名"
	case official.KindVendorEntry:
		return "✅ 已定位并校验厂商入口页"
	case official.KindSearchVerified:
		return "✅ 已定位并校验厂商官方页"
	case official.KindThirdParty:
		return "⚠️ 第三方转述，未找到官方页"
	}
	return ""
}

// LinkMark is the compact form of LinkBadge for table output.
func LinkMark(kind string) string {
	switch kind {
	case official.KindThirdParty:
		return "⚠️ "
	case "":
		return ""
	}
	if official.IsOfficialKind(kind) {
		return "✅ "
	}
	return ""
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
