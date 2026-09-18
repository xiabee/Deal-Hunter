package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/xiabee/deal-hunter/internal/model"
)

// Snapshot diffs one upstream against its own previous scrape and reports only
// the price facts that newly appeared. This is the autonomous counterpart to a
// feed: nobody has to publish the deal for us to notice it.
type Snapshot struct{ base }

const stateSnapshotPrefix = "snapshot:facts:"

// priceFactRe keeps lines that actually talk about money, quota or discounts.
var priceFactRe = regexp.MustCompile(`(?i)(?:[¥$￥]\s*\d|\d+(?:\.\d+)?\s*(?:元|折|%|/1k|/1m|tokens?|credits?)|免费|限时|赠送|试用|白送|no cost|free|credits|off|discount)`)

// Fetch implements Source.
func (s *Snapshot) Fetch(ctx context.Context) ([]*model.Deal, error) {
	resp, err := s.fetch(ctx)
	if err != nil {
		return nil, err
	}
	mode := s.Cfg.Param("mode", "auto")
	if mode == "auto" {
		mode = detectMode(resp.Body, resp.Header.Get("Content-Type"), resp.FinalURL)
	}
	facts := s.facts(string(resp.Body), mode)
	if len(facts) == 0 {
		return nil, fmt.Errorf("source %s: snapshot found no price facts (mode=%s)", s.Cfg.Name, mode)
	}

	key := stateSnapshotPrefix + s.Cfg.Name
	prev := map[string]bool{}
	firstRun := true
	if s.State != nil {
		if b, ok := s.State.GetState(key); ok {
			var ids []string
			if json.Unmarshal(b, &ids) == nil {
				firstRun = false
				for _, id := range ids {
					prev[id] = true
				}
			}
		}
	}
	current := make([]string, 0, len(facts))
	for _, f := range facts {
		current = append(current, f.key)
	}
	sort.Strings(current)
	if s.State != nil {
		if err := s.State.PutState(key, current); err != nil {
			s.logger().Warn("snapshot: cursor save failed", "source", s.Cfg.Name, "err", err)
		}
	}
	// With no cursor to write to (probe mode) every fact is treated as new, so
	// `dealhunter probe` can show what the collector sees.
	if firstRun && s.State != nil {
		s.logger().Info("snapshot: baseline stored", "source", s.Cfg.Name, "facts", len(facts))
		return nil, nil
	}

	var out []*model.Deal
	for _, f := range facts {
		if prev[f.key] {
			continue
		}
		d := s.deal(truncate(f.text, 120), f.url, f.text, zeroTime())
		if d == nil {
			continue
		}
		d.Tags = append(d.Tags, "snapshot", "price_change")
		d.Meta["detected_by"] = "self_snapshot"
		d.Meta["fact_key"] = f.key
		if f.url == "" {
			d.Meta["host"] = hostOf(s.Cfg.URL)
		}
		out = append(out, d)
	}
	if len(out) > 0 {
		s.logger().Info("snapshot: new price facts detected", "source", s.Cfg.Name, "new", len(out), "tracked", len(facts))
	}
	return limit(out, s.Cfg.Limit), nil
}

type fact struct {
	key  string
	text string
	url  string
}

// facts extracts candidate price statements from a page or API payload.
func (s *Snapshot) facts(body, mode string) []fact {
	var lines []string
	switch mode {
	case "json":
		lines = flattenJSON(body)
	default:
		for _, seg := range htmlSegments(noiseBlockRe.ReplaceAllString(body, " ")) {
			if t := cleanText(seg.Text); t != "" {
				lines = append(lines, t+"\x00"+seg.Href)
			}
		}
	}
	focus := s.Cfg.Param("focus", "")
	max := limitOrDefault(s.Cfg.Limit, 30)

	seen := map[string]bool{}
	var out []fact
	for _, ln := range lines {
		text, href := ln, ""
		if i := strings.Index(ln, "\x00"); i >= 0 {
			text, href = ln[:i], ln[i+1:]
		}
		if !priceFactRe.MatchString(text) {
			continue
		}
		if focus != "" && !strings.Contains(strings.ToLower(text), strings.ToLower(focus)) {
			continue
		}
		if !s.want(text) {
			continue
		}
		if href != "" {
			if abs, err := resolveURL(s.Cfg.URL, href); err == nil {
				href = abs
			}
		}
		norm := strings.ToLower(strings.Join(strings.Fields(text), " "))
		if len([]rune(norm)) < 8 || seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, fact{key: hashKey(norm), text: truncate(text, 400), url: href})
		if len(out) >= max {
			break
		}
	}
	return out
}

// flattenJSON turns an API payload into "path=value" lines so nested price
// fields become comparable facts.
func flattenJSON(body string) []string {
	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return nil
	}
	var out []string
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				walk(path+"."+k, val)
			}
		case []any:
			for i, val := range t {
				walk(fmt.Sprintf("%s[%d]", path, i), val)
			}
		default:
			s := fmt.Sprint(t)
			if s != "" && s != "<nil>" {
				out = append(out, path+"="+s)
			}
		}
	}
	walk("", root)
	return out
}

func detectMode(body []byte, contentType, finalURL string) string {
	trimmed := strings.TrimSpace(string(body[:minInt(len(body), 200)]))
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return "json"
	}
	if strings.Contains(strings.ToLower(contentType), "json") {
		return "json"
	}
	if strings.Contains(strings.ToLower(finalURL), ".json") {
		return "json"
	}
	return "html"
}

func minInt(n, m int) int {
	if n < m {
		return n
	}
	return m
}
