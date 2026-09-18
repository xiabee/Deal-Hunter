package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiabee/deal-hunter/internal/model"
)

// JSON reads an arbitrary JSON API. Mapping is declared in source params so a
// new endpoint needs no code: items_path locates the array, the *_path keys
// read fields of each element with dot notation.
type JSON struct{ base }

// Fetch implements Source.
func (j *JSON) Fetch(ctx context.Context) ([]*model.Deal, error) {
	resp, err := j.fetch(ctx)
	if err != nil {
		return nil, err
	}
	var root any
	if err := json.Unmarshal(resp.Body, &root); err != nil {
		return nil, fmt.Errorf("source %s: bad json: %w", j.Cfg.Name, err)
	}
	itemsPath := j.Cfg.Param("items_path", "")
	node := root
	if itemsPath != "" {
		node = jsonPath(root, strings.Split(itemsPath, "."))
	}
	arr, ok := node.([]any)
	if !ok {
		if m, isMap := node.(map[string]any); isMap && len(m) > 0 {
			arr = []any{m}
		} else {
			return nil, fmt.Errorf("source %s: items_path %q is not an array", j.Cfg.Name, itemsPath)
		}
	}
	var out []*model.Deal
	for _, raw := range arr {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		title := jsonStr(item, j.Cfg.Param("title_path", "title"))
		summary := jsonStr(item, j.Cfg.Param("summary_path", "description"))
		if title == "" {
			title = truncate(cleanText(summary), 120)
		}
		link := jsonStr(item, j.Cfg.Param("url_path", "url"))
		if prefix := j.Cfg.Param("url_prefix", ""); prefix != "" && link != "" && !strings.HasPrefix(link, "http") {
			link = strings.TrimSuffix(prefix, "/") + "/" + strings.TrimPrefix(link, "/")
		}
		pub := parseAnyTime(jsonStr(item, j.Cfg.Param("time_path", "published_at")))
		d := j.deal(title, link, truncate(cleanText(summary), 500), pub)
		if d == nil {
			continue
		}
		if id := jsonStr(item, j.Cfg.Param("id_path", "id")); id != "" {
			d.Meta["external_id"] = truncate(id, 160)
		}
		d.Tags = append(d.Tags, "json_api")
		out = append(out, d)
	}
	return limit(out, j.Cfg.Limit), nil
}

// jsonPath walks a dot-separated path through decoded JSON.
func jsonPath(v any, path []string) any {
	cur := v
	for _, seg := range path {
		if seg = strings.TrimSpace(seg); seg == "" {
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		if cur, ok = m[seg]; !ok {
			return nil
		}
	}
	return cur
}

// jsonStr renders a JSON scalar as a string, ignoring containers.
func jsonStr(item map[string]any, path string) string {
	if path == "" || item == nil {
		return ""
	}
	switch t := jsonPath(item, strings.Split(path, ".")).(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

func intVal(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
