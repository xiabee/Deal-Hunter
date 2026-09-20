package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/model"
	"github.com/xiabee/deal-hunter/internal/official"
)

func sampleDeal() model.Deal {
	return model.Deal{
		Title: "智谱 GLM-5.3-flash 限时免费开放", Summary: "官方公告：API 调用 0 元。",
		URL: "https://example.com/glm-free", Source: "openrouter-free-models",
		Category: model.CatAIFree, IsFree: true, Score: 92, DiscountPct: 0,
		ScoreWhy: []string{"+45 免费类 offer", "+10 已知厂商 智谱AI"},
		Vendors:  []string{"智谱AI"}, Tags: []string{"ai_free"},
		PublishedAt: time.Now().Add(-time.Hour), DiscoveredAt: time.Now(),
	}
}

func TestFeishuDeliversSignedCard(t *testing.T) {
	var received map[string]any
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("payload is not valid json: %v (%s)", err, body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"msg":"success"}`)
	}))
	defer srv.Close()

	f, err := NewFeishu(config.Feishu{Enabled: true, WebhookURL: srv.URL, Secret: "unit-test-secret", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("NewFeishu: %v", err)
	}
	if !f.Ready() {
		t.Fatal("a loopback webhook must be accepted for local relays")
	}
	msg := NewMessage(KindUrgent, Headline(&[]model.Deal{sampleDeal()}[0]), sampleDeal())
	if err := f.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(contentType, "application/json") {
		t.Errorf("content type = %q", contentType)
	}
	if received["msg_type"] != "interactive" {
		t.Fatalf("msg_type = %v", received["msg_type"])
	}
	ts, _ := received["timestamp"].(string)
	sign, _ := received["sign"].(string)
	if ts == "" || sign == "" {
		t.Fatal("a signing secret must always produce timestamp+sign")
	}
	// Verify the signature the way Feishu does, independently of Sign().
	mac := hmac.New(sha256.New, []byte(ts+"\n"+"unit-test-secret"))
	mac.Write([]byte{})
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); sign != want {
		t.Errorf("signature mismatch: got %s want %s", sign, want)
	}
	card, ok := received["card"].(map[string]any)
	if !ok {
		t.Fatal("card missing")
	}
	header := card["header"].(map[string]any)
	if header["template"] == "" || header["template"] == "grey" {
		t.Errorf("a high value alert should not use the grey template: %v", header["template"])
	}
	title := header["title"].(map[string]any)["content"].(string)
	if !strings.Contains(title, "免费") {
		t.Errorf("card title = %q", title)
	}
	if got := len(cardElements(card)); got < 3 {
		t.Fatalf("expected body/action/note elements, got %d", got)
	}
	joined, _ := json.Marshal(card)
	for _, want := range []string{"GLM-5.3-flash", "https://example.com/glm-free", "92", "查看原文"} {
		if !strings.Contains(string(joined), want) {
			t.Errorf("card missing %q: %s", want, joined)
		}
	}
	if strings.Contains(string(joined), "unit-test-secret") {
		t.Error("the signing secret must never appear in the payload")
	}
}

// cardElements reads the element list from a freshly built card. Before JSON
// encoding the slice keeps its concrete type, so both shapes must be accepted.
func cardElements(card map[string]any) []map[string]any {
	switch v := card["elements"].(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, e := range v {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

func TestFeishuSurfacesUpstreamErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"code":19021,"msg":"sign match fail"}`)
	}))
	defer srv.Close()
	f, _ := NewFeishu(config.Feishu{WebhookURL: srv.URL, Timezone: "UTC"})
	err := f.Send(context.Background(), NewMessage(KindUrgent, "x", sampleDeal()))
	if err == nil || !strings.Contains(err.Error(), "19021") {
		t.Fatalf("expected the upstream code to be reported, got %v", err)
	}
}

func TestFeishuReportsHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()
	f, _ := NewFeishu(config.Feishu{WebhookURL: srv.URL, Timezone: "UTC"})
	if err := f.Send(context.Background(), NewMessage(KindUrgent, "x", sampleDeal())); err == nil {
		t.Fatal("expected an error on HTTP 500")
	}
}

func TestFeishuWithoutWebhookNamesTheEnvVar(t *testing.T) {
	f, _ := NewFeishu(config.Feishu{Enabled: true, Timezone: "UTC"})
	err := f.Send(context.Background(), NewMessage(KindUrgent, "x", sampleDeal()))
	if err == nil || !strings.Contains(err.Error(), config.EnvFeishuWebhook) {
		t.Fatalf("error should tell the operator which env var to set, got %v", err)
	}
}

func TestWebhookURLRules(t *testing.T) {
	cases := map[string]bool{
		"https://open.feishu.cn/open-apis/bot/v2/hook/abc": true,
		"http://127.0.0.1:8080/hook":                       true,
		"http://localhost:8080/hook":                       true,
		"http://attacker.example.com/hook":                 false,
		"ftp://example.com/hook":                           false,
		"":                                                 false,
	}
	for raw, want := range cases {
		if got := URLLooksUsable(raw); got != want {
			t.Errorf("URLLooksUsable(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestFeishuResolvesItsTimezoneOrRefusesToStart(t *testing.T) {
	if _, err := NewFeishu(config.Feishu{Timezone: "Asia/Shanghai"}); err != nil {
		t.Fatalf("a real zone must load: %v", err)
	}
	// The service usually runs in UTC while the card footer is read by someone
	// on +0800, so the zone is not decoration and a typo in it must not pass.
	if _, err := NewFeishu(config.Feishu{Timezone: "Mars/Valles"}); err == nil {
		t.Error("an invalid timezone must be rejected")
	}
}

func TestTemplateChoiceReflectsValue(t *testing.T) {
	cases := []struct {
		msg  Message
		want string
	}{
		{NewMessage(KindTest, "t", sampleDeal()), "grey"},
		{NewMessage(KindDaily, "d", sampleDeal()), "blue"},
		{NewMessage(KindUrgent, "a", sampleDeal()), "red"},
	}
	for _, c := range cases {
		if got := CardTemplate(c.msg); got != c.want {
			t.Errorf("template for %s = %s, want %s", c.msg.Kind, got, c.want)
		}
	}
}

func TestFileDropWritesArtifacts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "drop")
	fd, err := NewFileDrop(dir, "deal-hunter")
	if err != nil {
		t.Fatalf("NewFileDrop: %v", err)
	}
	if err := fd.Send(context.Background(), NewMessage(KindUrgent, "测试标题", sampleDeal())); err != nil {
		t.Fatalf("Send: %v", err)
	}
	md, err := os.ReadFile(filepath.Join(dir, "deal-hunter-latest.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"测试标题", "GLM-5.3-flash", "https://example.com/glm-free", "**92**"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	// The markdown summarises the message; it must not restate the whole record.
	if lines := strings.Count(strings.TrimSpace(string(md)), "\n"); lines > 5 {
		t.Errorf("markdown should stay a few lines, got %d:\n%s", lines+1, md)
	}
	for _, omit := range []string{"置信分：", "厂商：", "发布：", "评分理由"} {
		if strings.Contains(string(md), omit) {
			t.Errorf("markdown should not spell out %q", omit)
		}
	}
	var payload map[string]any
	js, err := os.ReadFile(filepath.Join(dir, "deal-hunter-latest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(js, &payload); err != nil {
		t.Fatalf("json artifact invalid: %v", err)
	}
	if payload["Kind"] != string(KindUrgent) {
		t.Errorf("json payload = %v", payload)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("expected md + json + daily jsonl, got %d files", len(entries))
	}
	if _, err := NewFileDrop("", "x"); err == nil {
		t.Error("an empty dir must be rejected")
	}
}

func TestFanOutTreatsPartialSuccessAsDelivered(t *testing.T) {
	ok := &recordingNotifier{}
	bad := &failingNotifier{err: errors.New("webhook down")}
	f := NewFanOut(nil, bad, ok)
	if f.Len() != 2 {
		t.Fatalf("Len = %d", f.Len())
	}
	// A broken secondary channel must not make the pipeline re-alert findings
	// that already reached the user through a healthy backend.
	err := f.Send(context.Background(), NewMessage(KindUrgent, "t", sampleDeal()))
	if err != nil {
		t.Fatalf("partial success should not be an error, got %v", err)
	}
	if ok.calls != 1 {
		t.Errorf("healthy backends must still receive the message, calls=%d", ok.calls)
	}
	if got := f.Names(); len(got) != 2 || got[0] != "failing" {
		t.Errorf("Names = %v", got)
	}
}

func TestFanOutErrorsOnlyWhenEveryBackendFails(t *testing.T) {
	f := NewFanOut(nil,
		&failingNotifier{err: errors.New("relay down")},
		&failingNotifier{err: errors.New("webhook down")})
	err := f.Send(context.Background(), NewMessage(KindUrgent, "t", sampleDeal()))
	if err == nil {
		t.Fatal("all-backend failure must be reported")
	}
	for _, want := range []string{"relay down", "webhook down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error missing %q: %v", want, err)
		}
	}
}

func TestFanOutWithoutBackendsErrors(t *testing.T) {
	if err := NewFanOut(nil).Send(context.Background(), NewMessage(KindUrgent, "t")); err == nil {
		t.Error("expected an error when nothing is configured")
	}
}

func TestConsoleRendersPlainText(t *testing.T) {
	var buf strings.Builder
	c := NewConsoleWriter(&buf)
	if err := c.Send(context.Background(), NewMessage(KindUrgent, "插队标题", sampleDeal())); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"[URGENT]", "插队标题", "92", "🆓", "https://example.com/glm-free"} {
		if !strings.Contains(out, want) {
			t.Errorf("console output missing %q:\n%s", want, out)
		}
	}
}

func TestHeadlineAndIconChoices(t *testing.T) {
	d := sampleDeal()
	if got := Headline(&d); !strings.Contains(got, "🆓 免费") {
		t.Errorf("Headline = %q", got)
	}
	paid := sampleDeal()
	paid.IsFree = false
	paid.DiscountPct = 70
	paid.Category = model.CatDiscount
	if got := Headline(&paid); !strings.Contains(got, "70% off") {
		t.Errorf("discount headline = %q", got)
	}
	if Icon(&paid) != "🏷️" {
		t.Errorf("icon for discount = %s", Icon(&paid))
	}
	res := sampleDeal()
	res.IsFree = false
	res.Category = model.CatResource
	if Icon(&res) != "📦" {
		t.Error("resource icon")
	}
}

type recordingNotifier struct {
	calls int
	last  Message
}

func (r *recordingNotifier) Name() string { return "recorder" }
func (r *recordingNotifier) Send(_ context.Context, m Message) error {
	r.calls++
	r.last = m
	return nil
}

type failingNotifier struct{ err error }

func (f *failingNotifier) Name() string                        { return "failing" }
func (f *failingNotifier) Send(context.Context, Message) error { return f.err }

// resolvedDeal is what a deal looks like after the link ladder found its vendor.
func resolvedDeal(kind, officialURL string) model.Deal {
	d := sampleDeal()
	d.Meta = map[string]string{
		official.MetaLinkKind:    kind,
		official.MetaOfficialURL: officialURL,
		official.MetaOriginalURL: d.URL,
	}
	return d
}

func TestCardPresentsVerifiedOfficialPageFirst(t *testing.T) {
	f, _ := NewFeishu(config.Feishu{Timezone: "UTC"})
	const officialURL = "https://open.bigmodel.cn/pricing"
	m := NewMessage(KindUrgent, "t", resolvedDeal(official.KindSearchVerified, officialURL))
	elements := cardElements(f.card(m))

	var joined strings.Builder
	for _, e := range elements {
		b, _ := json.Marshal(e)
		joined.Write(b)
	}
	body := joined.String()
	if !strings.Contains(body, "官方页已校验") {
		t.Errorf("the card must say the link was verified: %s", body)
	}

	var actions []map[string]any
	for _, e := range elements {
		if e["tag"] == "action" {
			raw, _ := json.Marshal(e["actions"])
			_ = json.Unmarshal(raw, &actions)
		}
	}
	if len(actions) != 2 {
		t.Fatalf("expected an official button plus the original post, got %d: %v", len(actions), actions)
	}
	if actions[0]["url"] != officialURL || actions[0]["type"] != "primary" {
		t.Errorf("the verified vendor page must be the primary action: %v", actions[0])
	}
	label, _ := actions[0]["text"].(map[string]any)["content"].(string)
	if !strings.Contains(label, "官方入口") {
		t.Errorf("primary button label = %q", label)
	}
	if actions[1]["url"] != "https://example.com/glm-free" {
		t.Errorf("the community post must stay reachable: %v", actions[1])
	}
}

func TestCardKeepsUnverifiedLinkHonest(t *testing.T) {
	f, _ := NewFeishu(config.Feishu{Timezone: "UTC"})
	d := sampleDeal() // no resolution metadata at all
	b, _ := json.Marshal(f.card(NewMessage(KindUrgent, "t", d)))
	body := string(b)
	if strings.Contains(body, "官方入口") {
		t.Error("an unresolved link must not be dressed up as official")
	}
	if !strings.Contains(body, "查看原文") {
		t.Error("the original post should still be offered")
	}
	third := resolvedDeal(official.KindThirdParty, d.URL)
	elements := cardElements(f.card(NewMessage(KindUrgent, "t", third)))
	var joined strings.Builder
	for _, e := range elements {
		b, _ := json.Marshal(e)
		joined.Write(b)
	}
	if !strings.Contains(joined.String(), "第三方转述") {
		t.Errorf("a third-party retelling must be labelled: %s", joined.String())
	}
}

// The vendor's front door is a place to check, so it must appear beside an
// unconfirmed link without ever posing as the found offer.
func TestCardOffersVendorSiteWithoutClaimingIt(t *testing.T) {
	f, _ := NewFeishu(config.Feishu{Timezone: "UTC"})
	d := resolvedDeal(official.KindThirdParty, "https://www.v2ex.com/t/1")
	d.URL = "https://www.v2ex.com/t/1"
	d.Meta[official.MetaOriginalURL] = d.URL
	d.Meta[official.MetaVendorURL] = "https://open.bigmodel.cn/pricing"
	elements := cardElements(f.card(NewMessage(KindUrgent, "t", d)))

	var actions []map[string]any
	for _, e := range elements {
		if e["tag"] == "action" {
			raw, _ := json.Marshal(e["actions"])
			_ = json.Unmarshal(raw, &actions)
		}
	}
	if len(actions) != 2 {
		t.Fatalf("expected the post plus the vendor's front door, got %v", actions)
	}
	if actions[0]["url"] != "https://www.v2ex.com/t/1" || actions[0]["type"] != "primary" {
		t.Errorf("an unverified find must still present its own source: %v", actions[0])
	}
	site, _ := actions[1]["text"].(map[string]any)["content"].(string)
	if actions[1]["url"] != "https://open.bigmodel.cn/pricing" || !strings.Contains(site, "官网") {
		t.Errorf("the entry page should be offered as a place to check: %v", actions[1])
	}
	primary, _ := actions[0]["text"].(map[string]any)["content"].(string)
	if primary != "查看原文" {
		t.Errorf("an unconfirmed find must be labelled by what it is, got %q", primary)
	}
}

// A batch alert must stay scannable on a lock screen: one line per deal, no
// per-deal buttons, and no runaway text.
func TestMultiDealAlertIsOneLinePerDeal(t *testing.T) {
	f, _ := NewFeishu(config.Feishu{Timezone: "UTC"})
	long := sampleDeal()
	long.Title = strings.Repeat("限", 90) + " 免费额度"
	long.Summary = strings.Repeat("很长的一段说明，", 30)
	msg := NewMessage(KindUrgent, "🧾 新羊毛 3 条", resolvedDeal(official.KindAlreadyOfficial, "https://openrouter.ai/x:free"), long, sampleDeal())
	divs, actions, total := 0, 0, 0
	for _, e := range cardElements(f.card(msg)) {
		switch e["tag"] {
		case "action":
			actions++
		case "div":
			divs++
			content := e["text"].(map[string]any)["content"].(string)
			if strings.Contains(content, "\n") {
				t.Errorf("a batch row must stay on one line: %q", content)
			}
			total += len([]rune(content))
		}
	}
	if divs != 3 {
		t.Errorf("expected exactly one row per deal, got %d", divs)
	}
	if actions != 0 {
		t.Errorf("buttons belong to the one-deal layout, got %d", actions)
	}
	if total > 300 {
		t.Errorf("the whole batch should stay brief, got %d characters", total)
	}
}

// The morning briefing is a snapshot of what is still claimable, so each line
// carries its lifespan and the card stays as tight as the digest.
func TestDailyBriefingCardShowsLifespan(t *testing.T) {
	f, _ := NewFeishu(config.Feishu{Timezone: "UTC"})
	now := time.Now().UTC()
	live := sampleDeal()
	live.DiscoveredAt = now.Add(-50 * time.Hour)
	live.Meta = map[string]string{"expires_at": now.Add(72 * time.Hour).Format(time.RFC3339)}
	gone := sampleDeal()
	gone.Title = "已经结束的"
	gone.Meta = map[string]string{"expires_at": now.Add(-time.Hour).Format(time.RFC3339)}

	m := NewMessage(KindDaily, "🌅 羊毛日报 · 2 条仍在效", live, gone)
	m.Intro = "09月19日 · 未过期会再次出现"
	if got := CardTemplate(m); got != "blue" {
		t.Errorf("template = %s", got)
	}
	lines := []string{}
	for _, e := range cardElements(f.card(m)) {
		if e["tag"] == "div" {
			if txt, ok := e["text"].(map[string]any); ok {
				lines = append(lines, txt["content"].(string))
			}
		}
	}
	body := strings.Join(lines, "\n")
	if !strings.Contains(body, "已收录 2 天") {
		t.Errorf("the briefing should say how long the offer has been known:\n%s", body)
	}
	if !strings.Contains(body, "截止 ") {
		t.Errorf("a stated deadline belongs in the briefing:\n%s", body)
	}
	if n := strings.Count(body, "]("); n != 2 {
		t.Errorf("one line per deal, no expanded blocks, got %d links:\n%s", n, body)
	}
	if !strings.Contains(m.Plain(), "已收录 2 天") {
		t.Errorf("the text fallback must carry the same facts:\n%s", m.Plain())
	}
}

// New since yesterday and merely still-open are different questions; the card
// and the text fallback must both say which is which.
func TestDailySplitsFreshFromOngoing(t *testing.T) {
	now := time.Now().UTC()
	fresh := sampleDeal()
	fresh.Title = "今天刚发现的免费额度"
	fresh.DiscoveredAt = now.Add(-2 * time.Hour)
	stale := sampleDeal()
	stale.Title = "上周开始的活动"
	stale.DiscoveredAt = now.Add(-72 * time.Hour)

	d := SplitByAge([]model.Deal{fresh, stale}, now)
	if len(d.Fresh) != 1 || len(d.Ongoing) != 1 {
		t.Fatalf("split = %d fresh / %d ongoing", len(d.Fresh), len(d.Ongoing))
	}
	secs := d.Sections()
	if len(secs) != 2 || secs[0].Title != "🆕 今日新收录" || secs[1].Title != "⏳ 持续在效" {
		t.Fatalf("sections = %+v", secs)
	}

	f, _ := NewFeishu(config.Feishu{Timezone: "UTC"})
	mixed := NewMessage(KindDaily, "t", fresh, stale)
	mixed.CreatedAt = now
	b, _ := json.Marshal(f.card(mixed))
	for _, want := range []string{"今日新收录", "持续在效", "今天刚发现的免费额度", "上周开始的活动"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("card missing %q:\n%s", want, b)
		}
	}
	// A briefing with nothing new must not advertise an empty section.
	onlyNew := NewMessage(KindDaily, "t", fresh)
	onlyNew.CreatedAt = now
	b2, _ := json.Marshal(f.card(onlyNew))
	if strings.Contains(string(b2), "持续在效") {
		t.Errorf("empty section rendered:\n%s", b2)
	}
	if !strings.Contains(onlyNew.Plain(), "🆕 今日新收录") {
		t.Errorf("text fallback lost its heading:\n%s", onlyNew.Plain())
	}
}

func TestUrgentAndPlainUseTheResolvedLink(t *testing.T) {
	const officialURL = "https://open.bigmodel.cn/pricing"
	d := resolvedDeal(official.KindSearchVerified, officialURL)
	line := Line(&d)
	if !strings.Contains(line, officialURL) {
		t.Errorf("plain line should carry the vendor link: %q", line)
	}
	if !strings.Contains(line, "✅") {
		t.Errorf("plain line should state the verdict: %q", line)
	}
	breakthrough := NewMessage(KindUrgent, "⚡ 值得立刻看", d)
	f, _ := NewFeishu(config.Feishu{Timezone: "UTC"})
	b, _ := json.Marshal(f.card(breakthrough))
	if !strings.Contains(string(b), officialURL) {
		t.Errorf("breakthrough rows should link the vendor page: %s", b)
	}
}

// 提醒在三个通道上都必须带上开抢时刻：飞书卡片走 Intro，控制台/文件走 Plain()，
// OpenClaw 中转走它自己的文本。少一条就是某类读者只看到"要开抢了"却不知道几点。
func TestEventMessageCarriesTheMomentOnEveryPath(t *testing.T) {
	when := time.Now().Add(20 * time.Minute).UTC().Format("01月02日 15:04")
	m := NewMessage(KindEvent, "⏰ 09月21日 10:00 开抢 · 洪城消费券", sampleDeal())
	m.Title = "⏰ " + when + " 开抢 · 洪城消费券"
	f, _ := NewFeishu(config.Feishu{Timezone: "UTC"})
	card, err := json.Marshal(f.card(m))
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"card": string(card), "plain": m.Plain()} {
		if !strings.Contains(out, when) {
			t.Errorf("%s lost the moment: %s", name, out)
		}
	}
	r, _ := NewOpenClawRelay(config.OpenClaw{Command: "openclaw", Target: "chat:1", Channel: "feishu"})
	if body := r.Args(m)[len(r.Args(m))-1]; !strings.Contains(body, when) {
		t.Errorf("relay text lost the moment: %s", body)
	}
}

// 一张卡里混排"要开抢的"和"要作废的"时，每条的时刻写在 Intro 的多行里。任何一条
// 渲染路径把 Intro 吞掉，读者就只知道"有两条要提醒"而不知道哪条今晚到期。
func TestClosingCardKeepsItsPerRowMomentsOnEveryPath(t *testing.T) {
	m := NewMessage(KindEvent, "⏰ 2 项临近截止", sampleDeal())
	m.Intro = "· 09月26日 23:59 截止 · 洪城消费券第三批\n· 09月27日 10:00 开抢 · 滕王阁消费券"
	f, _ := NewFeishu(config.Feishu{Timezone: "UTC"})
	card, err := json.Marshal(f.card(m))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := NewOpenClawRelay(config.OpenClaw{Command: "openclaw", Target: "chat:1", Channel: "feishu"})
	for name, out := range map[string]string{
		"card":  string(card),
		"plain": m.Plain(),
		"relay": r.Args(m)[len(r.Args(m))-1],
	} {
		if !strings.Contains(out, "截止") || !strings.Contains(out, "23:59") {
			t.Errorf("%s lost the closing moment: %s", name, out)
		}
	}
	dir := t.TempDir()
	d, err := NewFileDrop(dir, "deal-hunter")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "deal-hunter-latest.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "截止") {
		t.Errorf("drop file lost the closing moment: %s", body)
	}
}
