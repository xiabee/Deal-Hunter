package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/model"
	"github.com/xiabee/deal-hunter/internal/official"
)

// Feishu pushes to a chat either through a group custom-bot webhook or through
// an application identity posting to im/v1/messages. Credentials come only from
// the environment and are never logged or echoed into payloads.
type Feishu struct {
	cfg     config.Feishu
	client  *http.Client
	tz      *time.Location
	apiBase string

	tokMu   sync.Mutex
	token   string
	tokenTo time.Time
}

// NewFeishu builds the backend; missing credentials are allowed so the binary
// can run and report exactly what is absent.
func NewFeishu(cfg config.Feishu) (*Feishu, error) {
	tz := time.Local
	if cfg.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Timezone); err == nil {
			tz = l
		} else {
			return nil, fmt.Errorf("feishu: bad timezone %q: %w", cfg.Timezone, err)
		}
	}
	base := strings.TrimRight(cfg.APIBase, "/")
	if base == "" {
		base = "https://open.feishu.cn"
	}
	return &Feishu{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}, tz: tz, apiBase: base}, nil
}

// Name implements Notifier.
func (f *Feishu) Name() string { return "feishu" }

// Mode reports the active route: "webhook", "app" or "". The webhook wins when
// both are configured, because it is scoped to a single group.
func (f *Feishu) Mode() string {
	switch {
	case URLLooksUsable(f.cfg.WebhookURL):
		return "webhook"
	case f.appReady():
		return "app"
	default:
		return ""
	}
}

// Ready reports whether any delivery route is usable. Remote endpoints must
// use HTTPS; plain HTTP is accepted only on loopback, so a local relay and the
// test suite can be exercised.
func (f *Feishu) Ready() bool { return f.Mode() != "" }

func (f *Feishu) appReady() bool {
	return strings.TrimSpace(f.cfg.AppID) != "" &&
		strings.TrimSpace(f.cfg.AppSecret) != "" &&
		strings.TrimSpace(f.cfg.ReceiveID) != ""
}

// ReceiveIDType reports the recipient kind, defaulting to open_id.
func (f *Feishu) ReceiveIDType() string {
	switch t := strings.TrimSpace(f.cfg.ReceiveIDType); t {
	case "user_id", "union_id", "email", "chat_id":
		return t
	default:
		return "open_id"
	}
}

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
	switch f.Mode() {
	case "webhook":
		return f.sendWebhook(ctx, m)
	case "app":
		return f.sendApp(ctx, m)
	default:
		return fmt.Errorf("feishu: no credentials configured (set %s, or %s + %s + %s for app delivery)",
			config.EnvFeishuWebhook, config.EnvFeishuAppID, config.EnvFeishuAppSect, config.EnvFeishuRecvID)
	}
}

func (f *Feishu) sendWebhook(ctx context.Context, m Message) error {
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
			if actions := buttons(&m.Deals[i]); len(actions) > 0 {
				elements = append(elements, map[string]any{
					"tag":     "action",
					"actions": actions,
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
	if badge := linkBadge(d.Meta[official.MetaLinkKind]); badge != "" {
		b.WriteString(badge + "\n")
	}
	if s := strings.TrimSpace(d.Summary); s != "" {
		b.WriteString("> " + truncateRunes(strings.ReplaceAll(s, "\n", " "), 260) + "\n")
	}
	if len(d.ScoreWhy) > 0 {
		b.WriteString("📈 " + strings.Join(d.ScoreWhy[:min(3, len(d.ScoreWhy))], "，"))
	}
	return b.String()
}

// buttons renders the link ladder: the verified official page leads, and the
// post we found it in stays one tap away so nothing is hidden.
func buttons(d *model.Deal) []map[string]any {
	best, original, kind := linkInfo(d)
	var out []map[string]any
	if strings.HasPrefix(best, "https://") {
		label, btnType := "查看原文 / 领取入口", "primary"
		if official.IsOfficialKind(kind) {
			label = "官方入口（已校验）"
		}
		out = append(out, map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "plain_text", "content": label},
			"url":  best,
			"type": btnType,
		})
	}
	if original != best && strings.HasPrefix(original, "https://") {
		out = append(out, map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "plain_text", "content": "原始出处"},
			"url":  original,
			"type": "default",
		})
	}
	return out
}

func digestLine(d *model.Deal) string {
	link, _, kind := linkInfo(d)
	if link == "" {
		link = "#"
	}
	suffix := ""
	if kind == official.KindThirdParty {
		suffix = " ⚠️"
	}
	return fmt.Sprintf("%s [%s](%s) — **%d**%s", Icon(d), truncateRunes(d.Title, 64), link, d.Score, suffix)
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

// apiResponse is the shared envelope of Feishu Open API replies.
type apiResponse struct {
	Code        int    `json:"code"`
	Msg         string `json:"msg"`
	TenantToken string `json:"tenant_access_token"`
	Expire      int64  `json:"expire"`
	Data        struct {
		MessageID string `json:"message_id"`
	} `json:"data"`
}

// sendApp delivers as the application itself via im/v1/messages, so no group
// bot has to be created. Cards fall back to plain text rather than dropping
// the finding.
func (f *Feishu) sendApp(ctx context.Context, m Message) error {
	tok, err := f.tenantToken(ctx)
	if err != nil {
		return err
	}
	card, err := json.Marshal(f.card(m))
	if err != nil {
		return fmt.Errorf("feishu: encode card: %w", err)
	}
	endpoint := f.apiBase + "/open-apis/im/v1/messages?receive_id_type=" + f.ReceiveIDType()

	content := string(card)
	for attempt, msgType := range []string{"interactive", "text"} {
		if attempt == 1 {
			content, _ = marshalText(m)
		}
		body, err := json.Marshal(map[string]any{
			"receive_id": f.cfg.ReceiveID,
			"msg_type":   msgType,
			"content":    content,
		})
		if err != nil {
			return fmt.Errorf("feishu: encode payload: %w", err)
		}
		resp, err := f.post(ctx, endpoint, tok, body)
		if err != nil {
			return err
		}
		if resp.Code == 0 {
			return nil
		}
		if msgType == "interactive" {
			continue
		}
		return fmt.Errorf("feishu: im/v1/messages code=%d msg=%s", resp.Code, resp.Msg)
	}
	return fmt.Errorf("feishu: im/v1/messages rejected")
}

func marshalText(m Message) (string, error) {
	b, err := json.Marshal(map[string]string{"text": strings.TrimSpace(m.Plain())})
	return string(b), err
}

// tenantToken mints (and caches) the tenant_access_token. The app secret is
// only ever present in the request body, never in a returned error or log line.
func (f *Feishu) tenantToken(ctx context.Context) (string, error) {
	f.tokMu.Lock()
	defer f.tokMu.Unlock()
	if f.token != "" && time.Now().Before(f.tokenTo) {
		return f.token, nil
	}
	body, err := json.Marshal(map[string]string{
		"app_id":     f.cfg.AppID,
		"app_secret": f.cfg.AppSecret,
	})
	if err != nil {
		return "", fmt.Errorf("feishu: encode auth request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		f.apiBase+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("feishu: auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	raw, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("feishu: token request failed: %w", err)
	}
	defer raw.Body.Close()
	var resp apiResponse
	if err := json.NewDecoder(io.LimitReader(raw.Body, 1<<20)).Decode(&resp); err != nil {
		return "", fmt.Errorf("feishu: undecodable token reply (HTTP %d)", raw.StatusCode)
	}
	if resp.Code != 0 || resp.TenantToken == "" {
		return "", fmt.Errorf("feishu: token code=%d msg=%s", resp.Code, resp.Msg)
	}
	ttl := time.Duration(resp.Expire) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	if ttl > 2*time.Minute {
		ttl -= 2 * time.Minute
	}
	f.token = resp.TenantToken
	f.tokenTo = time.Now().Add(ttl)
	return f.token, nil
}

func (f *Feishu) post(ctx context.Context, endpoint, bearer string, body []byte) (*apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("feishu: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+bearer)
	raw, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feishu: post failed: %w", err)
	}
	defer raw.Body.Close()
	var resp apiResponse
	if err := json.NewDecoder(io.LimitReader(raw.Body, 1<<20)).Decode(&resp); err != nil {
		return nil, fmt.Errorf("feishu: undecodable reply (HTTP %d)", raw.StatusCode)
	}
	if resp.Code == 0 && resp.Data.MessageID == "" {
		return &resp, fmt.Errorf("feishu: code=0 but no message id (HTTP %d)", raw.StatusCode)
	}
	return &resp, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
