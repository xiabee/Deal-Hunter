// Package sources turns upstream endpoints into candidate deals. Every
// collector implements Source and shares base helpers for fetching, URL
// resolution and per-source keyword filtering.
package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/httpx"
	"github.com/xiabee/deal-hunter/internal/model"
)

// Fetcher is the outbound HTTP surface, stubbed in tests.
type Fetcher interface {
	Get(ctx context.Context, rawURL string, hdr map[string]string) (*httpx.Response, error)
}

// StateStore persists cursors for stateful collectors such as the OpenRouter
// free-model differ.
type StateStore interface {
	GetState(key string) ([]byte, bool)
	PutState(key string, v any) error
}

// Deps wires one collector.
type Deps struct {
	Cfg      config.Source
	HTTP     Fetcher
	Defaults config.HTTP
	State    StateStore
	Log      *slog.Logger
	Now      func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) logger() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

// Source collects candidate deals from one upstream.
type Source interface {
	Name() string
	Kind() string
	Fetch(ctx context.Context) ([]*model.Deal, error)
}

// New builds the collector named by cfg.Kind.
func New(d Deps) (Source, error) {
	if d.HTTP == nil {
		return nil, fmt.Errorf("sources: %s: no fetcher", d.Cfg.Name)
	}
	switch strings.ToLower(d.Cfg.Kind) {
	case config.KindRSS:
		return &RSS{base{d}}, nil
	case config.KindHTML:
		return &HTML{base{d}}, nil
	case config.KindJSON:
		return &JSON{base{d}}, nil
	case config.KindHN:
		return &HN{base{d}}, nil
	case config.KindSearch:
		return &Search{base{d}}, nil
	case config.KindSnapshot:
		return &Snapshot{base{d}}, nil
	case config.KindOpenRouter:
		return &OpenRouter{base{d}}, nil
	default:
		return nil, fmt.Errorf("sources: %s: unknown kind %q", d.Cfg.Name, d.Cfg.Kind)
	}
}

type base struct{ Deps }

func (b base) Name() string { return b.Cfg.Name }
func (b base) Kind() string { return b.Cfg.Kind }

func (b base) fetch(ctx context.Context) (*httpx.Response, error) {
	c, cancel := context.WithTimeout(ctx, b.Cfg.TimeoutOrDefault(b.Defaults.Timeout.D()))
	defer cancel()
	resp, err := b.HTTP.Get(c, b.Cfg.URL, b.Cfg.Headers)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", b.Cfg.Name, err)
	}
	if resp.Status < 200 || resp.Status > 299 {
		return nil, fmt.Errorf("source %s: %s returned HTTP %d", b.Cfg.Name, resp.FinalURL, resp.Status)
	}
	return resp, nil
}

// deal normalizes one raw item and applies the source-level keyword gate. It
// returns nil when the item is out of scope, which keeps collectors dumb.
func (b base) deal(title, link, summary string, pub time.Time) *model.Deal {
	title = cleanText(title)
	if title == "" {
		return nil
	}
	summary = truncate(cleanText(summary), 700)
	abs, err := resolveURL(b.Cfg.URL, link)
	if err != nil {
		abs = strings.TrimSpace(link)
	}
	haystack := title + "\n" + summary
	if !b.want(haystack) {
		return nil
	}
	d := &model.Deal{
		URL:          abs,
		Title:        title,
		Summary:      summary,
		Source:       b.Cfg.Name,
		Category:     b.Cfg.Category,
		PublishedAt:  pub.UTC(),
		DiscoveredAt: b.now().UTC(),
		Meta:         map[string]string{"kind": b.Cfg.Kind, "url": b.Cfg.URL},
	}
	if abs != "" {
		d.Meta["host"] = hostOf(abs)
	}
	d.SortOffersAndTags()
	d.EnsureFingerprint()
	return d
}

// want applies the per-source allow/deny keyword lists.
func (b base) want(text string) bool {
	lower := strings.ToLower(text)
	for _, d := range b.Cfg.Deny {
		if d != "" && strings.Contains(lower, strings.ToLower(d)) {
			return false
		}
	}
	if len(b.Cfg.Keywords) == 0 {
		return true
	}
	for _, k := range b.Cfg.Keywords {
		if k != "" && strings.Contains(lower, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

// IsOfficial reports whether a result URL belongs to a vendor's own domain,
// which is a strong trust signal for scoring.
func (b base) IsOfficial(resultURL string) bool {
	if len(b.Cfg.Sites) == 0 {
		return false
	}
	h := hostOf(resultURL)
	if h == "" {
		h = hostOf(b.Cfg.URL)
	}
	for _, s := range b.Cfg.Sites {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && (h == s || strings.HasSuffix(h, "."+s)) {
			return true
		}
	}
	return false
}

// OfficialHosts exposes the vendor domain check to the pipeline.
func OfficialHosts(cfg config.Source) []string { return cfg.Sites }

// IsOfficialURL reports whether u lives on one of sites (exact or subdomain).
func IsOfficialURL(u string, sites []string) bool {
	h := hostOf(u)
	if h == "" {
		return false
	}
	for _, s := range sites {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && (h == s || strings.HasSuffix(h, "."+s)) {
			return true
		}
	}
	return false
}

var tagRe = regexp.MustCompile(`(?s)<[^>]*>`)
var wsRe = regexp.MustCompile(`[ \t\r\f\v]+`)

func cleanText(s string) string {
	s = decodeEntities(s)
	s = tagRe.ReplaceAllString(s, " ")
	s = strings.NewReplacer("\n", " ", "\t", " ").Replace(s)
	s = wsRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

var entityRe = regexp.MustCompile(`&(#\d+|#[xX][0-9a-fA-F]+|[a-zA-Z][a-zA-Z0-9]{1,9});`)
var namedEntities = map[string]string{
	"nbsp": " ", "amp": "&", "lt": "<", "gt": ">", "quot": `"`, "apos": "'",
	"hellip": "…", "mdash": "—", "ndash": "–", "rsquo": "’", "lsquo": "‘",
	"rdquo": "”", "ldquo": "“", "middot": "·", "bull": "•", "trade": "™",
	"reg": "®", "copy": "©", "plusmn": "±", "times": "×", "divide": "÷",
	"deg": "°", "laquo": "«", "raquo": "»", "darr": "↓", "uarr": "↑",
	"rarr": "→", "larr": "←", "harr": "↔", "euro": "€", "pound": "£",
	"curren": "¤", "yuml": "ÿ", "sect": "§", "para": "¶", "dagger": "†",
	"permil": "‰", "prime": "′", "lsqb": "[", "rsqb": "]", "brvbar": "¦",
	"shy": "",
}

// decodeEntities resolves the HTML entities that encoding/xml leaves alone,
// so feeds such as Discourse do not break tag stripping.
func decodeEntities(s string) string {
	return entityRe.ReplaceAllStringFunc(s, func(m string) string {
		body := m[1 : len(m)-1]
		switch {
		case body[0] == '#':
			base, digits := 10, body[1:]
			if len(digits) > 1 && (digits[0] == 'x' || digits[0] == 'X') {
				base, digits = 16, digits[1:]
			}
			n, err := strconv.ParseInt(digits, base, 32)
			if err != nil || n <= 0 {
				return m
			}
			return string(rune(n))
		default:
			if r, ok := namedEntities[strings.ToLower(body)]; ok {
				return r
			}
			return m
		}
	})
}

// decodeXMLUnsafeEntities resolves only the HTML named entities that XML does
// not define. The five predefined ones (amp, lt, gt, quot, apos) are left
// alone: a feed that escapes its own markup as &lt;div&gt; must keep it escaped
// until after the XML parse, or the content becomes real child nodes and the
// text is lost.
func decodeXMLUnsafeEntities(s string) string {
	return entityRe.ReplaceAllStringFunc(s, func(m string) string {
		body := m[1 : len(m)-1]
		if body[0] == '#' {
			return m // numeric references are valid XML; the decoder handles them
		}
		switch strings.ToLower(body) {
		case "amp", "lt", "gt", "quot", "apos":
			return m
		}
		if r, ok := namedEntities[strings.ToLower(body)]; ok {
			return r
		}
		return m
	})
}

// unescapeAttr handles attribute values.
func unescapeAttr(s string) string { return html.UnescapeString(s) }

func resolveURL(baseURL, ref string) (string, error) {
	ref = strings.TrimSpace(decodeEntities(ref))
	if ref == "" {
		return "", nil
	}
	if strings.HasPrefix(ref, "javascript:") || strings.HasPrefix(ref, "mailto:") {
		return "", fmt.Errorf("useless href scheme")
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	if u.IsAbs() {
		return ref, nil
	}
	b, err := url.Parse(baseURL)
	if err != nil {
		return ref, nil
	}
	return b.ResolveReference(u).String(), nil
}

func hostOf(u string) string {
	parsed, err := url.Parse(strings.TrimSpace(u))
	if err != nil || parsed.Host == "" {
		return ""
	}
	h := parsed.Hostname()
	if i := strings.LastIndex(h, "]"); i >= 0 {
		h = h[i+1:]
	}
	return strings.ToLower(h)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return ""
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// parseAnyTime accepts the date formats seen across feeds and APIs.
func parseAnyTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && len(s) >= 9 {
		if n > 1e12 { // milliseconds
			n /= 1000
		}
		return time.Unix(n, 0).UTC()
	}
	layouts := []string{
		time.RFC3339Nano, time.RFC3339,
		"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
		time.RFC1123, time.RFC1123Z,
		"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02",
		"2006/01/02", "01-02-2006",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	// "Fri, 01 Sep 2026 10:00:00 +0800" with padded day of month.
	if t, err := time.Parse("Mon, 02 Jan 2006 15:04:05 -0700", s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// zeroTime marks items whose upstream publishes no timestamp.
func zeroTime() time.Time { return time.Time{} }

// hashKey fingerprints a normalized price fact for snapshot diffing.
func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// limit caps a slice of items to the source's configured limit.
func limit[T any](items []T, n int) []T {
	if n > 0 && len(items) > n {
		return items[:n]
	}
	return items
}
