package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/model"
)

// Feishu pushes interactive cards to a group custom-bot webhook. The webhook
// URL and signing secret come only from the environment.
type Feishu struct {
	cfg    config.Feishu
	client *http.Client
	tz     *time.Location
}

// NewFeishu builds the backend; an empty webhook is allowed so the binary can
// run and report what is missing.
func NewFeishu(cfg config.Feishu) (*Feishu, error) {
	tz := time.Local
	if cfg.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Timezone); err == nil {
			tz = l
		} else {
			return nil, fmt.Errorf("feishu: bad timezone %q: %w", cfg.Timezone, err)
		}
	}
	return &Feishu{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}, tz: tz}, nil
}

// Name implements Notifier.
func (f *Feishu) Name() string { return "feishu" }

// Ready reports whether a usable webhook is configured. Remote endpoints must
// use HTTPS; plain HTTP is accepted only on loopback so a local relay (for
// example an OpenClaw sidecar) and the test suite can be exercised.
func (f *Feishu) Ready() bool { return URLLooksUsable(f.cfg.WebhookURL) }

// URLLooksUsable applies the HTTPS-or-loopback rule to a webhook URL.
func URLLooksUsable(raw string) bool {
	if strings.HasPrefix(raw, "https://") {
		return true
	}
	if !strings.HasPrefix(raw, "http://") {
		return false
	}
	hostPort := strings.TrimPrefix(raw, "http://")
	if i := strings.IndexAny(hostPort, "/?"); i >= 0 {
		hostPort = hostPort[:i]
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1" ||
		(err == nil && port != "" && (host == "127.0.0.1" || host == "localhost"))
}

// WebhookHost returns the destination host for status output; the token itself
// is never logged.
func (f *Feishu) WebhookHost() string {
	if !f.Ready() {
		return ""
	}
	if i := strings.Index(f.cfg.WebhookURL[8:], "/"); i > 0 {
		return f.cfg.WebhookURL[:8+i]
	}
	return f.cfg.WebhookURL
}

// QuietNow reports whether alerts should be held back at this hour.
func (f *Feishu) QuietNow(now time.Time) bool {
	if len(f.cfg.SilentHours) == 0 {
		return false
	}
	h := now.In(f.tz).Hour()
	for _, s := range f.cfg.SilentHours {
		if s == h {
			return true
		}
	}
	return false
}

// CardTemplate picks the header colour from the deal's shape.
func CardTemplate(m Message) string {
	switch m.Kind {
	case KindDigest:
		return "blue"
	case KindTest, KindError:
		return "grey"
	}
	if len(m.Deals) == 0 {
		return "turquoise"
	}
	d := m.Deals[0]
	switch {
	case d.Score >= 85:
		return "red"
	case d.IsFree:
		return "turquoise"
	case d.DiscountPct >= 50:
		return "orange"
	default:
		return "green"
	}
}

// Send implements Notifier.
func (f *Feishu) Send(ctx context.Context, m Message) error {
	if !f.Ready() {
		return fmt.Errorf("feishu: webhook not configured (set %s)", config.EnvFeishuWebhook)
	}
	payload := f.Render(m)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("feishu: encode card: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("feishu: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("feishu: post: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Code   int    `json:"code"`
		Msg    string `json:"msg"`
		Error  string `json:"error"`
		SubErr string `json:"sub_msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("feishu: undecodable reply (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || out.Code != 0 {
		return fmt.Errorf("feishu: HTTP %d code=%d msg=%s %s", resp.StatusCode, out.Code, out.Msg, out.SubErr)
	}
	return nil
}

// Render turns a Message into the webhook payload (exported for tests).
func (f *Feishu) Render(m Message) map[string]any {
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     f.card(m),
	}
	if f.cfg.Secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = Sign(f.cfg.Secret, ts)
	}
	return payload
}

// Sign reproduces the documented Feishu custom-bot signature: base64 of
// HMAC-SHA256 keyed by "<timestamp>\n<secret>" over an empty message.
func Sign(secret, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
	mac.Write([]byte{})
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (f *Feishu) card(m Message) map[string]any {
	elements := []map[string]any{}
	if m.Intro != "" {
		elements = append(elements, md("**"+m.Intro+"**"))
	}
	switch m.Kind {
	case KindDigest:
		for i := range m.Deals {
			elements = append(elements, md(digestLine(&m.Deals[i])))
		}
	case KindTest:
		elements = append(elements, md("链路自检成功：Deal-Hunter 已能写入该群。"))
	default:
		for i := range m.Deals {
			elements = append(elements, md(dealBlock(&m.Deals[i])))
			if url := m.Deals[i].URL; strings.HasPrefix(url, "https://") {
				elements = append(elements, map[string]any{
					"tag": "action",
					"actions": []map[string]any{{
						"tag":  "button",
						"text": map[string]any{"tag": "plain_text", "content": "查看原文 / 领取入口"},
						"url":  url,
						"type": "primary",
					}},
				})
			}
		}
	}
	elements = append(elements, map[string]any{
		"tag": "note",
		"elements": []map[string]any{{
			"tag": "lark_md",
			"content": fmt.Sprintf("Deal-Hunter · %s · 置信分 %s",
				m.CreatedAt.In(f.tz).Format("01-02 15:04"), topScore(m)),
		}},
	})
	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true, "enable_forward": true},
		"header": map[string]any{
			"template": CardTemplate(m),
			"title":    map[string]any{"tag": "plain_text", "content": f.title(m)},
		},
		"elements": elements,
	}
}

func (f *Feishu) title(m Message) string {
	if m.Title != "" {
		return m.Title
	}
	if m.Kind == KindDigest {
		return fmt.Sprintf("🧺 羊毛日报 · %d 条", len(m.Deals))
	}
	if len(m.Deals) > 0 {
		return Headline(&m.Deals[0])
	}
	return "Deal-Hunter 通知"
}

func md(content string) map[string]any {
	return map[string]any{
		"tag":  "div",
		"text": map[string]any{"tag": "lark_md", "content": content},
	}
}

func dealBlock(d *model.Deal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s **%s**\n", Icon(d), truncateRunes(d.Title, 100))
	meta := []string{fmt.Sprintf("置信分 **%d**", d.Score)}
	if len(d.Vendors) > 0 {
		meta = append(meta, "厂商 "+strings.Join(d.Vendors[:min(2, len(d.Vendors))], "/"))
	}
	if d.DiscountPct > 0 {
		meta = append(meta, fmt.Sprintf("降幅 %d%%", d.DiscountPct))
	}
	if len(d.Tags) > 0 {
		meta = append(meta, "#"+strings.Join(d.Tags[:min(3, len(d.Tags))], " #"))
	}
	b.WriteString(strings.Join(meta, " · ") + "\n")
	if s := strings.TrimSpace(d.Summary); s != "" {
		b.WriteString("> " + truncateRunes(strings.ReplaceAll(s, "\n", " "), 260) + "\n")
	}
	if len(d.ScoreWhy) > 0 {
		b.WriteString("📈 " + strings.Join(d.ScoreWhy[:min(3, len(d.ScoreWhy))], "，"))
	}
	return b.String()
}

func digestLine(d *model.Deal) string {
	link := d.URL
	if link == "" {
		link = "#"
	}
	return fmt.Sprintf("%s [%s](%s) — **%d**", Icon(d), truncateRunes(d.Title, 64), link, d.Score)
}

func topScore(m Message) string {
	if len(m.Deals) == 0 {
		return "-"
	}
	best := m.Deals[0].Score
	for _, d := range m.Deals {
		if d.Score > best {
			best = d.Score
		}
	}
	return strconv.Itoa(best)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
