package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

// OpenRouter watches the public model catalogue and reports models that just
// became zero-cost. The state cursor is the previous free-model ID set, so a
// price flip to zero produces exactly one alert - this is how a "GLM-5.3-flash
// is free now" event is caught the hour it happens.
type OpenRouter struct{ base }

// StateKeyFreeSet names the persisted cursor for the free-model differ.
const StateKeyFreeSet = "openrouter:free_models"

type orCatalog struct {
	Data []orModel `json:"data"`
}

type orModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Created       int64  `json:"created"`
	ContextLength int    `json:"context_length"`
	Pricing       struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
		Request    string `json:"request"`
		Image      string `json:"image"`
	} `json:"pricing"`
	Architecture struct {
		Modality string `json:"modality"`
	} `json:"architecture"`
}

// Fetch implements Source.
func (o *OpenRouter) Fetch(ctx context.Context) ([]*model.Deal, error) {
	resp, err := o.fetch(ctx)
	if err != nil {
		return nil, err
	}
	var catalog orCatalog
	if err := json.Unmarshal(resp.Body, &catalog); err != nil {
		return nil, fmt.Errorf("source %s: bad catalogue json: %w", o.Cfg.Name, err)
	}
	if len(catalog.Data) == 0 {
		return nil, fmt.Errorf("source %s: empty catalogue", o.Cfg.Name)
	}

	now := o.now()
	graceDays := atoiOr(o.Cfg.Param("first_run_grace_days", "14"), 14)
	maxNew := atoiOr(o.Cfg.Param("max_new", "10"), 10)

	var free []orModel
	freeIDs := make([]string, 0, len(catalog.Data))
	for _, m := range catalog.Data {
		if !isZeroPrice(m.Pricing.Prompt) || !isZeroPrice(m.Pricing.Completion) {
			continue
		}
		free = append(free, m)
		freeIDs = append(freeIDs, m.ID)
	}
	sort.Strings(freeIDs)

	prev := map[string]bool{}
	firstRun := true
	if o.State != nil {
		if b, ok := o.State.GetState(StateKeyFreeSet); ok {
			var ids []string
			if json.Unmarshal(b, &ids) == nil {
				firstRun = false
				for _, id := range ids {
					prev[id] = true
				}
			}
		}
	}

	var candidates []orModel
	for _, m := range free {
		if firstRun {
			// Seed run: only recently created models, so a first deploy does not
			// dump the whole free catalogue into chat.
			if m.Created > 0 && time.Unix(m.Created, 0).After(now.AddDate(0, 0, -graceDays)) {
				candidates = append(candidates, m)
			}
			continue
		}
		if !prev[m.ID] {
			candidates = append(candidates, m)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Created > candidates[j].Created })
	candidates = limit(candidates, maxNew)

	if o.State != nil {
		if err := o.State.PutState(StateKeyFreeSet, freeIDs); err != nil {
			o.logger().Warn("openrouter: persist cursor failed", "source", o.Cfg.Name, "err", err)
		}
	}

	var out []*model.Deal
	for _, m := range candidates {
		created := time.Time{}
		if m.Created > 0 {
			created = time.Unix(m.Created, 0).UTC()
		}
		summary := fmt.Sprintf(
			"输入/输出价格均为 0，上下文 %d tokens，模态 %s。%s",
			m.ContextLength, orDefault(m.Architecture.Modality, "text"), truncate(cleanText(m.Description), 220),
		)
		if !firstRun {
			summary = "OpenRouter 新增免费模型（此前为付费档）。" + summary
		} else {
			summary = "OpenRouter 免费模型（首轮基线）。" + summary
		}
		d := o.deal(fmt.Sprintf("OpenRouter 免费模型：%s（%s）", orDefault(m.Name, m.ID), m.ID),
			"https://openrouter.ai/"+m.ID, summary, created)
		if d == nil {
			continue
		}
		d.Category = model.CatAIFree
		d.IsFree = true
		d.Tags = append(d.Tags, "openrouter", "zero_price")
		d.Meta["model_id"] = m.ID
		d.Meta["context_length"] = strconv.Itoa(m.ContextLength)
		d.Meta["modality"] = m.Architecture.Modality
		d.Meta["price_prompt"] = m.Pricing.Prompt
		d.Meta["price_completion"] = m.Pricing.Completion
		if firstRun {
			d.Meta["baseline"] = "true"
		}
		out = append(out, d)
	}
	o.logger().Debug("openrouter: differ pass",
		"source", o.Cfg.Name, "free_total", len(freeIDs), "first_run", firstRun, "candidates", len(out))
	return out, nil
}

// isZeroPrice treats a missing or unparseable price as non-free, so a schema
// change cannot silently produce a flood of "free" alerts.
func isZeroPrice(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == 0
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
