package sources

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

// HTML scrapes a server-rendered promo page. Deal-Hunter stays dependency free
// by splitting the document at block boundaries and keeping the text segments
// that survive the source keyword gate, rather than embedding a CSS selector
// engine.
type HTML struct{ base }

var (
	hrefRe     = regexp.MustCompile(`(?is)<a\b[^>]*?href\s*=\s*["']([^"']+)["']`)
	boundaryRe = regexp.MustCompile(`</(?:li|p|div|section|article|tr|h[1-6]|td|th|dd|dt|figcaption|span|a)>|<br\s*/?>`)
	// Inline scripts and styles would otherwise be scraped as "text".
	noiseBlockRe = regexp.MustCompile(`(?is)<(script|style|noscript|template)[^>]*>.*?</(script|style|noscript|template)>`)
	listDateRe   = regexp.MustCompile(`20\d{2}[-/]\d{1,2}[-/]\d{1,2}`)
)

// Fetch implements Source.
func (h *HTML) Fetch(ctx context.Context) ([]*model.Deal, error) {
	resp, err := h.fetch(ctx)
	if err != nil {
		return nil, err
	}
	body := noiseBlockRe.ReplaceAllString(decodeEntities(string(resp.Body)), " ")
	if cls := h.Cfg.Param("class_contains", ""); cls != "" {
		body = focusOnClass(body, cls)
	}
	segments := htmlSegments(body)

	minText := atoiOr(h.Cfg.Param("min_text_len", ""), 10)
	maxItems := h.Cfg.Limit
	if maxItems <= 0 {
		maxItems = 40
	}
	var out []*model.Deal
	seen := map[string]bool{}
	// carry is the deal made by the immediately preceding segment, or nil when
	// that segment produced nothing. A row's date may only attach to its own row:
	// letting it walk back to an earlier one would hand a filtered-out notice's
	// date to the item before it.
	var carry *model.Deal
	for _, seg := range segments {
		text := cleanText(seg.Text)
		if len([]rune(text)) < minText {
			carry = nil
			continue
		}
		key := strings.ToLower(text)
		if seen[key] {
			carry = nil
			continue
		}
		seen[key] = true
		link := seg.Href
		if link == "" {
			// A notice list marks each row up as <a>标题</a><i>日期</i>, and the
			// </a> boundary puts the date in the next segment. Hand it back to the
			// item it describes: without a publish date there is no anchor for the
			// "9月21日开抢" style of time that omits the year.
			if when, ok := rowDate(text); ok && carry != nil {
				carry.PublishedAt = when
				carry = nil
				continue
			}
			link = h.Cfg.URL
		}
		d := h.deal(truncate(text, 120), link, text, time.Time{})
		if d == nil {
			carry = nil
			continue
		}
		d.Tags = append(d.Tags, "html_scrape")
		out = append(out, d)
		carry = d
		if len(out) >= maxItems {
			break
		}
	}
	if h.Cfg.Param("follow_detail", "") == "1" {
		h.followDetails(ctx, out)
	}
	return out, nil
}

// followDetails re-reads each surviving row's own page, because on a notice list
// the useful facts (start time, redemption deadline) live in the announcement
// body rather than the row title. Only rows that already passed the keyword gate
// reach this, so a 20-row list costs a couple of extra requests rather than 20.
func (h *HTML) followDetails(ctx context.Context, deals []*model.Deal) {
	for _, d := range deals {
		if d.URL == "" || d.URL == h.Cfg.URL {
			continue
		}
		c, cancel := context.WithTimeout(ctx, h.Cfg.TimeoutOrDefault(h.Defaults.Timeout.D()))
		resp, err := h.HTTP.Get(c, d.URL, h.Cfg.Headers)
		cancel()
		if err != nil || resp == nil {
			h.logger().Warn("html: detail fetch failed", "source", h.Cfg.Name, "url", d.URL,
				"err", err)
			continue
		}
		if resp.Status < 200 || resp.Status > 299 {
			h.logger().Warn("html: detail fetch refused", "source", h.Cfg.Name, "url", d.URL,
				"status", resp.Status)
			continue
		}
		text := detailText(resp.Body)
		// A block list answers with a tiny "slow down" page under HTTP 200. Treat
		// that as no body at all and keep the row text, so the item never looks
		// like it was read when it was not.
		if minLen := atoiOr(h.Cfg.Param("detail_min_text_len", ""), 200); len([]rune(text)) < minLen {
			h.logger().Warn("html: detail body too thin to be the announcement",
				"source", h.Cfg.Name, "url", d.URL, "runes", len([]rune(text)))
			continue
		}
		// The body lands in Summary, which is persisted and rendered whole, so it
		// is capped after parsing-critical text at the head is guaranteed kept.
		d.Summary = truncate(text, atoiOr(h.Cfg.Param("detail_text_len", ""), 2000))
	}
}

// detailText flattens an article page to its visible text.
func detailText(body []byte) string {
	return cleanText(noiseBlockRe.ReplaceAllString(decodeEntities(string(body)), " "))
}

type htmlSegment struct {
	Text string
	Href string
}

// rowDate picks the publish date out of a list segment that carries nothing but
// a date, the way government notice lists mark up their rows. Anything longer is
// real content and must not be swallowed as a date.
func rowDate(text string) (time.Time, bool) {
	if len([]rune(text)) > 24 {
		return time.Time{}, false
	}
	m := listDateRe.FindString(text)
	if m == "" {
		return time.Time{}, false
	}
	when := parseAnyTime(strings.ReplaceAll(m, "/", "-"))
	if when.IsZero() {
		return time.Time{}, false
	}
	return when, true
}

// htmlSegments slices a page into text blocks at closing block-level tags,
// associating the first link found inside each block.
func htmlSegments(body string) []htmlSegment {
	var out []htmlSegment
	last := 0
	flush := func(chunk string) {
		text := strings.TrimSpace(stripTags(chunk))
		if text == "" {
			return
		}
		href := ""
		if m := hrefRe.FindStringSubmatch(chunk); m != nil {
			href = unescapeAttr(m[1])
		}
		out = append(out, htmlSegment{Text: text, Href: href})
	}
	for _, loc := range boundaryRe.FindAllStringIndex(body, -1) {
		flush(body[last:loc[0]])
		last = loc[1]
	}
	flush(body[last:])
	return out
}

// focusOnClass keeps only the windows that follow a class attribute
// containing needle, which limits scraping to the promo grid of a page.
func focusOnClass(body, needle string) string {
	lower := strings.ToLower(body)
	needle = strings.ToLower(needle)
	var kept []string
	for off := 0; ; {
		i := strings.Index(lower[off:], needle)
		if i < 0 {
			break
		}
		start := off + i
		end := start + 4000
		if end > len(body) {
			end = len(body)
		}
		kept = append(kept, body[start:end])
		off = start + len(needle)
		if off >= len(body) {
			break
		}
	}
	if len(kept) == 0 {
		return body
	}
	return strings.Join(kept, "\n")
}

func stripTags(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			inTag = true
			b.WriteByte(' ')
		case '>':
			inTag = false
		default:
			if !inTag {
				b.WriteByte(s[i])
			}
		}
	}
	return b.String()
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return def
}
