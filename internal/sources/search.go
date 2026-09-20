package sources

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

// Search runs Deal-Hunter's own keyword queries against a consent-friendly HTML
// search endpoint. Discovery therefore does not depend on anybody else's
// aggregation: we ask the index directly, rotating queries round by round so a
// long-running deployment sweeps a large question space.
type Search struct{ base }

const stateSearchCursor = "search:cursor:"

var (
	// DuckDuckGo lite emits href before class.
	ddgHrefFirst  = regexp.MustCompile(`(?is)<a\b[^>]*?href=["']([^"']+)["'][^>]*?class=['"]result-link['"][^>]*>(.*?)</a>`)
	ddgClassFirst = regexp.MustCompile(`(?is)<a\b[^>]*?class=['"]result-link['"][^>]*?href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	ddgSnippet    = regexp.MustCompile(`(?is)class=['"]result-snippet['"][^>]*>(.*?)(?:</td>|</div>)`)
	// ddgChallengeRe marks DuckDuckGo's "bots use DuckDuckGo too" interstitial,
	// served with HTTP 200 and zero results.
	ddgChallengeRe = regexp.MustCompile(`(?i)anomaly-modal|/anomaly\.js|id=["']challenge-form["']|cc=botnet`)
)

// Fetch implements Source: one rotating query per round keeps the crawler polite.
func (sr *Search) Fetch(ctx context.Context) ([]*model.Deal, error) {
	queries := splitList(sr.Cfg.Param("queries", ""))
	if len(queries) == 0 {
		return nil, fmt.Errorf("source %s: search kind requires params.queries", sr.Cfg.Name)
	}
	idx := 0
	stateKey := stateSearchCursor + sr.Cfg.Name
	if sr.State != nil {
		if b, ok := sr.State.GetState(stateKey); ok {
			idx = atoiDefault(string(b), 0)
		}
	}
	q := queries[idx%len(queries)]
	if site := sr.Cfg.Param("site", ""); site != "" {
		q = q + " site:" + site
	}
	if sr.State != nil {
		if err := sr.State.PutState(stateKey, idx+1); err != nil {
			sr.logger().Warn("search: cursor save failed", "source", sr.Cfg.Name, "err", err)
		}
	}

	endpoint := sr.Cfg.Param("endpoint", "https://lite.duckduckgo.com/lite/")
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	full := endpoint + sep + "q=" + url.QueryEscape(q)
	if kl := sr.Cfg.Param("kl", ""); kl != "" {
		full += "&kl=" + url.QueryEscape(kl)
	}

	c, cancel := context.WithTimeout(ctx, sr.Cfg.TimeoutOrDefault(sr.Defaults.Timeout.D()))
	defer cancel()
	resp, err := sr.HTTP.Get(c, full, sr.Cfg.Headers)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", sr.Cfg.Name, err)
	}
	if resp.Status < 200 || resp.Status > 299 {
		return nil, fmt.Errorf("source %s: search returned HTTP %d for %q", sr.Cfg.Name, resp.Status, q)
	}
	hits := parseSearchResults(string(resp.Body))
	if len(hits) == 0 {
		// The engine answers 200 with a human-check page when it decides the egress
		// IP is automated. Silently reporting zero would read as "nothing to find",
		// which is the opposite diagnosis, so name it.
		if ddgChallengeRe.MatchString(string(resp.Body)) {
			return nil, fmt.Errorf("source %s: search engine demanded a human check for %q (no results this round)", sr.Cfg.Name, q)
		}
		sr.logger().Debug("search: no results", "source", sr.Cfg.Name, "query", q)
		return nil, nil
	}

	sr.logger().Debug("search: query executed", "source", sr.Cfg.Name, "query", q, "hits", len(hits))
	var out []*model.Deal
	seen := map[string]bool{}
	for _, h := range hits {
		if h.URL == "" || seen[h.URL] {
			continue
		}
		seen[h.URL] = true
		d := sr.deal(h.Title, h.URL, h.Snippet, zeroTime())
		if d == nil {
			continue
		}
		d.Tags = append(d.Tags, "search", "q:"+truncate(q, 40))
		d.Meta["query"] = q
		d.Meta["discovered_by"] = "own_search"
		out = append(out, d)
	}
	return limit(out, sr.Cfg.Limit), nil
}

type SearchHit struct {
	Title   string
	URL     string
	Snippet string
}

// parseSearchResults tolerates both attribute orders DuckDuckGo has shipped.
// ParseSearchResults exposes the tolerant result parser used by the search kind,
// so the official-link resolver can reuse it.
func ParseSearchResults(body string) []SearchHit {
	return parseSearchResults(body)
}

func parseSearchResults(body string) []SearchHit {
	blocks := ddgHrefFirst.FindAllStringSubmatch(body, -1)
	if len(blocks) == 0 {
		blocks = ddgClassFirst.FindAllStringSubmatch(body, -1)
	}
	var hits []SearchHit
	for _, b := range blocks {
		href := unwrapProxyHref(b[1])
		title := cleanText(b[2])
		if title == "" || href == "" {
			continue
		}
		snippet := ""
		if m := ddgSnippet.FindStringSubmatch(after(body, b[0])); m != nil {
			snippet = cleanText(m[1])
		}
		hits = append(hits, SearchHit{Title: title, URL: href, Snippet: snippet})
	}
	return hits
}

// after returns the document tail following a matched block, so a snippet can
// be located without a full HTML parser.
func after(body, match string) string {
	i := strings.Index(body, match)
	if i < 0 {
		return body
	}
	end := i + len(match)
	if end+1200 < len(body) {
		return body[end : end+1200]
	}
	return body[end:]
}

// unwrapProxyHref resolves DuckDuckGo's /l/?uddg=<encoded> redirector.
func unwrapProxyHref(raw string) string {
	raw = strings.TrimSpace(unescapeAttr(raw))
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if !strings.Contains(u.Path, "/l/") {
		return raw
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	return ""
}

func splitList(s string) []string {
	s = strings.NewReplacer(";", "\n").Replace(s)
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func atoiDefault(s string, def int) int {
	s = strings.TrimSpace(strings.Trim(s, `"`))
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}

// HTTPSearcher runs one ad-hoc query against the HTML search endpoint. It is
// used by the official-link resolver, not by a configured source.
type HTTPSearcher struct {
	HTTP     Fetcher
	Endpoint string
	Headers  map[string]string
	Timeout  time.Duration
}

// Search returns result URLs in rank order.
func (h HTTPSearcher) Search(ctx context.Context, query string) ([]string, error) {
	endpoint := h.Endpoint
	if endpoint == "" {
		endpoint = "https://lite.duckduckgo.com/lite/"
	}
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	full := endpoint + sep + "q=" + url.QueryEscape(query)
	to := h.Timeout
	if to <= 0 {
		to = 25 * time.Second
	}
	c, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	resp, err := h.HTTP.Get(c, full, h.Headers)
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		return nil, fmt.Errorf("search endpoint returned HTTP %d", resp.Status)
	}
	hits := ParseSearchResults(string(resp.Body))
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		if hit.URL != "" {
			out = append(out, hit.URL)
		}
	}
	return out, nil
}
