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

// CardTemplate picks the header colour from the deal's shape.
func CardTemplate(m Message) string {
	switch m.Kind {
	case KindDaily:
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
	case KindDaily:
		for _, sec := range SplitByAge(m.Deals, m.CreatedAt).Sections() {
			elements = append(elements, md("**"+sec.Title+"**"))
			for i := range sec.Deals {
				elements = append(elements, md(dailyCardLine(&sec.Deals[i], m.CreatedAt)))
			}
		}
	case KindTest:
		elements = append(elements, md("链路自检成功：Deal-Hunter 已能写入该群。"))
	default:
		// One deal gets a short body plus buttons; several get one line each,
		// because the only question at 3am is "do I tap this".
		if len(m.Deals) == 1 {
			elements = append(elements, singleDealBody(&m.Deals[0])...)
			break
		}
		for i := range m.Deals {
			elements = append(elements, md(dealLine(&m.Deals[i])))
		}
	}
	elements = append(elements, map[string]any{
		"tag": "note",
		"elements": []map[string]any{{
			"tag":     "lark_md",
			"content": "Deal-Hunter · " + m.CreatedAt.In(f.tz).Format("01-02 15:04"),
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

// dealLine renders one deal as a single tappable line. The verdict is a mark,
// not a sentence: the full reasoning lives in the panel and the API.
func dealLine(d *model.Deal) string {
	link, _, kind := LinkFor(d)
	title := truncateRunes(strings.ReplaceAll(d.Title, "\n", " "), 42)
	if !strings.HasPrefix(link, "https://") {
		return fmt.Sprintf("%s %s · **%d**", Icon(d), title, d.Score)
	}
	return strings.TrimSpace(fmt.Sprintf("%s [%s](%s) · **%d** %s",
		Icon(d), title, link, d.Score, LinkMark(kind)))
}

// dailyCardLine is the briefing row: the same tappable line as an alert, plus
// how long the offer has been known and when it ends.
func dailyCardLine(d *model.Deal, now time.Time) string {
	line := dealLine(d)
	if life := Lifespan(d, now); life != "" {
		line += " · " + life
	}
	return line
}

// singleDealBody is the one-deal layout: score, where the offer came from, and
// at most one short quote.
func singleDealBody(d *model.Deal) []map[string]any {
	meta := []string{"置信分 **" + strconv.Itoa(d.Score) + "**"}
	if len(d.Vendors) > 0 && !strings.Contains(d.Title, d.Vendors[0]) {
		meta = append(meta, d.Vendors[0])
	}
	if d.DiscountPct > 0 {
		meta = append(meta, fmt.Sprintf("%d%% off", d.DiscountPct))
	}
	if badge := LinkBadge(d.Meta[official.MetaLinkKind]); badge != "" {
		meta = append(meta, badge)
	}
	out := []map[string]any{md(strings.Join(meta, " · "))}
	if s := strings.TrimSpace(d.Summary); s != "" {
		out = append(out, md("> "+truncateRunes(strings.ReplaceAll(s, "\n", " "), 90)))
	}
	if actions := buttons(d); len(actions) > 0 {
		out = append(out, map[string]any{"tag": "action", "actions": actions})
	}
	return out
}

func (f *Feishu) title(m Message) string {
	if m.Title != "" {
		return m.Title
	}
	if m.Kind == KindDaily {
		return fmt.Sprintf("🌅 羊毛日报 · %d 条仍在效", len(m.Deals))
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

// buttons renders the link ladder: the verified official page leads, the post we
// found it in stays one tap away, and the vendor's own front door is offered
// wherever the offer itself could not be confirmed.
func buttons(d *model.Deal) []map[string]any {
	best, original, kind := LinkFor(d)
	var out []map[string]any
	if strings.HasPrefix(best, "https://") {
		label := "查看原文"
		if official.IsOfficialKind(kind) {
			label = "官方入口"
		}
		out = append(out, button(label, best, "primary"))
	}
	if original != best && strings.HasPrefix(original, "https://") {
		out = append(out, button("原始出处", original, "default"))
	}
	if site := VendorSite(d); site != "" && site != best && site != original && strings.HasPrefix(site, "https://") {
		out = append(out, button("去官网核实", site, "default"))
	}
	return out
}

func button(label, url, btnType string) map[string]any {
	return map[string]any{
		"tag":  "button",
		"text": map[string]any{"tag": "plain_text", "content": label},
		"url":  url,
		"type": btnType,
	}
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
