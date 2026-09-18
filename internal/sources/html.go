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
	for _, seg := range segments {
		text := cleanText(seg.Text)
		if len([]rune(text)) < minText {
			continue
		}
		key := strings.ToLower(text)
		if seen[key] {
			continue
		}
		seen[key] = true
		link := seg.Href
		if link == "" {
			link = h.Cfg.URL
		}
		d := h.deal(truncate(text, 120), link, text, time.Time{})
		if d == nil {
			continue
		}
		d.Tags = append(d.Tags, "html_scrape")
		out = append(out, d)
		if len(out) >= maxItems {
			break
		}
	}
	return out, nil
}

type htmlSegment struct {
	Text string
	Href string
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
