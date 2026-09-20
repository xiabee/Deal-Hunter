// Package notify delivers findings. Delivery is deliberately pluggable: the
// Feishu custom bot pushes alerts directly, while the OpenClaw integration only
// drops files that the assistant reads over loopback HTTP.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
	"github.com/xiabee/deal-hunter/internal/official"
)

// Kind selects the message template.
type Kind string

const (
	// KindUrgent may interrupt the day's briefing; KindDaily is the one message
	// a day that every other finding waits for.
	KindUrgent Kind = "urgent"
	KindDaily  Kind = "daily"
	KindTest   Kind = "test"
	KindError  Kind = "error"
)

// Message is one delivery: the daily briefing, or a finding urgent enough to
// interrupt it.
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

// Send delivers m to all backends. A finding counts as delivered if ANY backend
// accepted it, so a broken secondary channel cannot make the same message go out
// twice — with one briefing a day, a duplicate is a worse failure than a plain
// one. An all-backend failure is reported so the schedule can retry; a partial
// one is only logged, which means the day's slot stays spent even if one channel
// missed the message.
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
	if m.Kind == KindDaily {
		for _, sec := range SplitByAge(m.Deals, m.CreatedAt).Sections() {
			b.WriteString("  " + sec.Title + "\n")
			for i := range sec.Deals {
				b.WriteString("    " + DailyLine(&sec.Deals[i], m.CreatedAt) + "\n")
			}
		}
		return b.String()
	}
	for i := range m.Deals {
		b.WriteString("  " + Line(&m.Deals[i]) + "\n")
	}
	return b.String()
}

// Line renders one deal as a single compact line: what it is, how sure we are,
// and where to go.
func Line(d *model.Deal) string {
	parts := []string{Icon(d), strconv.Itoa(d.Score)}
	if mark := LinkMark(d.Meta[official.MetaLinkKind]); mark != "" {
		parts = append(parts, mark)
	}
	parts = append(parts, truncateRunes(strings.ReplaceAll(d.Title, "\n", " "), 60))
	if best, _, _ := LinkFor(d); best != "" {
		parts = append(parts, best)
	}
	return strings.Join(parts, " ")
}

// DailyLine states the offer plus its lifespan: how long we have known about it
// and when it ends, which is what a morning briefing is actually for.
func DailyLine(d *model.Deal, now time.Time) string {
	return strings.Join([]string{Line(d), Lifespan(d, now)}, " · ")
}

// Daily sections of a briefing, split by whether the reader has already seen it.
// "Did I miss something" and "what is still open" are different questions, and a
// flat list answers neither.
type Daily struct{ Fresh, Ongoing []model.Deal }

// SplitByAge buckets deals into arrived-since-yesterday and already-known.
func SplitByAge(deals []model.Deal, now time.Time) Daily {
	var out Daily
	for i := range deals {
		if !deals[i].DiscoveredAt.IsZero() && now.Sub(deals[i].DiscoveredAt) < 24*time.Hour {
			out.Fresh = append(out.Fresh, deals[i])
			continue
		}
		out.Ongoing = append(out.Ongoing, deals[i])
	}
	return out
}

// DailySection is one labelled block of the morning briefing.
type DailySection struct {
	Title string
	Deals []model.Deal
}

// Sections names each bucket the way the card and the text fallback show them.
func (d Daily) Sections() []DailySection {
	var out []DailySection
	if len(d.Fresh) > 0 {
		out = append(out, DailySection{"🆕 今日新收录", d.Fresh})
	}
	if len(d.Ongoing) > 0 {
		out = append(out, DailySection{"⏳ 持续在效", d.Ongoing})
	}
	return out
}

// Lifespan states how long the offer has been on record and when it ends, which
// is the difference a morning briefing has to answer: is this still gettable,
// and how much of it have I already slept through.
func Lifespan(d *model.Deal, now time.Time) string {
	parts := []string{}
	if age := KnownFor(d.DiscoveredAt, now); age != "" {
		parts = append(parts, age)
	}
	if exp := d.Meta["expires_at"]; exp != "" {
		if t, err := time.Parse(time.RFC3339, exp); err == nil {
			parts = append(parts, "截止 "+t.Format("01-02"))
		}
	}
	return strings.Join(parts, " · ")
}

// KnownFor renders how long a finding has been on record.
func KnownFor(since, now time.Time) string {
	if since.IsZero() {
		return ""
	}
	d := now.Sub(since)
	switch {
	case d < time.Hour:
		return "刚收录"
	case d < 24*time.Hour:
		return fmt.Sprintf("已收录 %d 小时", int(d.Hours()))
	default:
		return fmt.Sprintf("已收录 %d 天", int(d.Hours()/24))
	}
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

// VendorSite is the vendor's own entry page, when we found and verified one. It
// is offered as a place to check, never as the link that carries the claim.
func VendorSite(d *model.Deal) string { return d.Meta[official.MetaVendorURL] }

// LinkBadge states in a few characters how far the link can be trusted, so a
// third-party retelling never looks like a confirmed vendor announcement.
func LinkBadge(kind string) string {
	switch kind {
	case official.KindAlreadyOfficial:
		return "✅ 厂商官方域名"
	case official.KindSearchVerified:
		return "✅ 官方页已校验"
	case official.KindThirdParty:
		return "⚠️ 第三方转述"
	}
	return ""
}

// LinkMark is the badge without the words, for lines that have no room for them.
func LinkMark(kind string) string {
	switch kind {
	case official.KindThirdParty:
		return "⚠️"
	case "":
		return ""
	}
	if official.IsOfficialKind(kind) {
		return "✅"
	}
	return ""
}

// Icon maps a category to an emoji used in cards and the briefing.
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

// Headline builds the card title for a single finding.
func Headline(d *model.Deal) string {
	if d.IsFree {
		return "🆓 免费 · " + truncateRunes(d.Title, 48)
	}
	if d.DiscountPct > 0 {
		return fmt.Sprintf("🏷️ %d%% off · %s", d.DiscountPct, truncateRunes(d.Title, 46))
	}
	return truncateRunes(d.Title, 52)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
