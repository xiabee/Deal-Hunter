package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/xiabee/deal-hunter/internal/model"
)

// HN queries the public Hacker News Algolia search API, which needs no key and
// surfaces launch/promo posts that never reach a Chinese feed.
type HN struct{ base }

type hnResponse struct {
	Hits []hnHit `json:"hits"`
}

type hnHit struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Text        string `json:"story_text"`
	ObjectID    string `json:"objectID"`
	CreatedAt   string `json:"created_at"`
	Points      *int   `json:"points"`
	NumComments *int   `json:"num_comments"`
	Author      string `json:"author"`
}

// Fetch implements Source.
func (h *HN) Fetch(ctx context.Context) ([]*model.Deal, error) {
	q := h.Cfg.Param("query", "")
	if q == "" {
		return nil, fmt.Errorf("source %s: hn kind requires params.query", h.Cfg.Name)
	}
	endpoint := h.Cfg.URL
	if strings.Contains(endpoint, "?") {
		endpoint += "&"
	} else {
		endpoint += "?"
	}
	endpoint += "query=" + url.QueryEscape(q) + "&hitsPerPage=" + strconv.Itoa(limitOrDefault(h.Cfg.Limit, 20))
	if tags := h.Cfg.Param("tags", ""); tags != "" {
		endpoint += "&tags=" + url.QueryEscape(tags)
	}
	if nf := h.Cfg.Param("numericFilters", ""); nf != "" {
		endpoint += "&numericFilters=" + url.QueryEscape(nf)
	}

	c, cancel := context.WithTimeout(ctx, h.Cfg.TimeoutOrDefault(h.Defaults.Timeout.D()))
	defer cancel()
	resp, err := h.HTTP.Get(c, endpoint, h.Cfg.Headers)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", h.Cfg.Name, err)
	}
	if resp.Status < 200 || resp.Status > 299 {
		return nil, fmt.Errorf("source %s: hn returned HTTP %d", h.Cfg.Name, resp.Status)
	}
	var parsed hnResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return nil, fmt.Errorf("source %s: bad hn json: %w", h.Cfg.Name, err)
	}
	var out []*model.Deal
	for _, hit := range parsed.Hits {
		link := strings.TrimSpace(hit.URL)
		if link == "" {
			link = "https://news.ycombinator.com/item?id=" + hit.ObjectID
		}
		summary := cleanText(hit.Text)
		if summary == "" {
			summary = link
		}
		summary = fmt.Sprintf("HN %d 分 / %d 评论 · %s", intVal(hit.Points), intVal(hit.NumComments), summary)
		d := h.deal(hit.Title, link, summary, parseAnyTime(hit.CreatedAt))
		if d == nil {
			continue
		}
		d.Tags = append(d.Tags, "hackernews")
		d.Meta["hn_id"] = hit.ObjectID
		if hit.Author != "" {
			d.Meta["hn_author"] = hit.Author
		}
		out = append(out, d)
	}
	return limit(out, h.Cfg.Limit), nil
}

func limitOrDefault(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}
