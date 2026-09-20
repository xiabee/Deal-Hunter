package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/httpx"
	"github.com/xiabee/deal-hunter/internal/model"
	"github.com/xiabee/deal-hunter/internal/notify"
	"github.com/xiabee/deal-hunter/internal/pipeline"
)

const webhookToken = "https://open.feishu.invalid/open-apis/bot/v2/hook/supersecrettoken1234567"

type fetcher struct{ body []byte }

func (f *fetcher) Get(_ context.Context, u string, _ map[string]string) (*httpx.Response, error) {
	return &httpx.Response{Status: 200, FinalURL: u, Body: f.body}, nil
}

type recorder struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (r *recorder) Name() string { return "recorder" }
func (r *recorder) Send(_ context.Context, m notify.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, m)
	return nil
}

func newTestServer(t *testing.T) (*httptest.Server, *pipeline.App, *recorder) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "sources", "testdata", "rss_deals.xml"))
	if err != nil {
		t.Fatal(err)
	}
	drop := filepath.Join(t.TempDir(), "outreach")
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Server.Enabled = true
	cfg.Server.Bind = "127.0.0.1:0"
	cfg.Notify.Console = false
	cfg.Notify.OpenClaw.Enabled = true
	cfg.Notify.OpenClaw.SkillDir = drop
	cfg.Notify.Feishu.Enabled = true
	cfg.Notify.Feishu.WebhookURL = webhookToken
	cfg.Notify.Feishu.Secret = "TOPSECRETVALUE"
	cfg.Notify.Urgent.Enabled = false
	cfg.Sources = []config.Source{{Name: "rss", Kind: config.KindRSS, URL: "https://feeds.example/latest.rss", Trust: 8}}

	rec := &recorder{}
	app, err := pipeline.New(cfg, nil, pipeline.WithFetcher(&fetcher{body: body}), pipeline.WithNotifiers(rec))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	t.Cleanup(func() { app.Close() })
	if _, err := app.RunOnce(context.Background(), "test"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	ts := httptest.NewServer(New(cfg, app, nil).Handler())
	t.Cleanup(ts.Close)
	return ts, app, rec
}

func get(t *testing.T, ts *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func mustJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("not json: %v\n%s", err, b)
	}
	return out
}

func TestSecurityHeadersAndReadOnlySurface(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, body := get(t, ts, "/healthz")
	defer resp.Body.Close()
	for _, h := range []struct{ k, v string }{
		{"X-Content-Type-Options", "nosniff"},
		{"X-Frame-Options", "DENY"},
		{"Referrer-Policy", "no-referrer"},
		{"Cache-Control", "no-store"},
	} {
		if got := resp.Header.Get(h.k); got != h.v {
			t.Errorf("header %s = %q, want %q", h.k, got, h.v)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP missing: %q", csp)
	}
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("healthz = %d %s", resp.StatusCode, body)
	}
	// Nothing in the API may write.
	for _, path := range []string{"/healthz", "/api/v1/status", "/api/v1/deals", "/api/v1/digest"} {
		r, err := http.Post(ts.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", path, r.StatusCode)
		}
	}
	if r, _ := get(t, ts, "/api/v1/nonexistent"); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown route = %d, want 404", r.StatusCode)
	}
}

func TestStatusReportsOperationalState(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, body := get(t, ts, "/api/v1/status")
	defer resp.Body.Close()
	got := mustJSON(t, body)
	if got["bind"] == nil || got["version"] == nil {
		t.Fatalf("status incomplete: %v", got)
	}
	if got["network_scope"] != "loopback or Tailscale only" {
		t.Errorf("network scope missing: %v", got["network_scope"])
	}
	sources := got["sources"].(map[string]any)
	if sources["enabled"].(float64) != 1 {
		t.Errorf("sources = %v", sources)
	}
	store := got["store"].(map[string]any)
	if store["deals_seen"].(float64) == 0 {
		t.Error("store stats should be populated")
	}
	// The delivery tiles read these keys by name. A rename here would fail
	// silently in the browser, so it has to fail in the test instead.
	daily, ok := got["daily"].(map[string]any)
	if !ok || daily["at"] != "09:00" {
		t.Errorf("the briefing schedule is missing from status: %v", got["daily"])
	}
	if _, has := daily["sent_today"]; !has {
		t.Error("the panel cannot tell whether today's report already went out")
	}
	if _, has := store["live_in_briefing"]; !has {
		t.Error("the number of rows the briefing would carry should be reported")
	}
	ev, ok := got["event"].(map[string]any)
	if !ok {
		t.Error("the reminder channel must report its schedule: the panel reads it by name")
	}
	if _, has := ev["due_now"]; !has {
		t.Error("the panel cannot show how many events are waiting for a reminder")
	}
	for _, key := range []string{"due_opening", "due_expiry", "expiry_lead_minutes"} {
		if _, has := ev[key]; !has {
			t.Errorf("the status omits %s; an opening and a closing look identical without it", key)
		}
	}
	if _, has := ev["sent_today"]; !has {
		t.Error("the panel cannot show today's reminder budget")
	}
	if _, ok := got["urgent"].(map[string]any); !ok {
		t.Error("today's breakthrough budget is missing from status")
	}
	if _, present := store["pending_alerts"]; present {
		t.Error("pending_alerts belongs to the queue the daily model replaced")
	}
	if filters := got["filters"].(map[string]any); filters["alert_min_score"] != nil {
		t.Error("there is no per-round alert threshold any more")
	}
	lastRun, ok := got["last_run"].(map[string]any)
	if !ok || lastRun["trigger"] != "test" {
		t.Errorf("last_run missing: %v", got["last_run"])
	}
}

func TestNoCredentialEverLeavesTheProcess(t *testing.T) {
	ts, app, _ := newTestServer(t)
	drop := app.OutreachDir()
	if err := os.WriteFile(filepath.Join(drop, "deal-hunter-latest.md"), []byte("# 摘要\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	paths := []string{"/healthz", "/api/v1/status", "/api/v1/deals", "/api/v1/sources", "/api/v1/runs", "/api/v1/digest", "/"}
	for _, p := range paths {
		resp, body := get(t, ts, p)
		text := string(body)
		resp.Body.Close()
		for _, secret := range []string{"supersecrettoken1234567", "TOPSECRETVALUE"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaks the credential %q", p, secret)
			}
		}
	}
	// On-disk artifacts must be equally clean.
	filepath.Walk(app.Config().DataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(b), "supersecrettoken1234567") {
			t.Errorf("%s persists the webhook token", path)
		}
		return nil
	})
}

func TestDealsEndpointFiltering(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, body := get(t, ts, "/api/v1/deals")
	defer resp.Body.Close()
	all := mustJSON(t, body)
	total := int(all["count"].(float64))
	if total == 0 {
		t.Fatal("no deals stored")
	}

	resp2, body2 := get(t, ts, "/api/v1/deals?min=90")
	defer resp2.Body.Close()
	high := mustJSON(t, body2)
	if int(high["count"].(float64)) > total {
		t.Error("min filter widened the result set")
	}
	for _, d := range high["deals"].([]any) {
		if d.(map[string]any)["score"].(float64) < 90 {
			t.Errorf("score below the filter: %v", d)
		}
	}

	resp3, body3 := get(t, ts, "/api/v1/deals?q="+"GLM")
	defer resp3.Body.Close()
	filtered := mustJSON(t, body3)
	if int(filtered["count"].(float64)) == 0 {
		t.Error("search filter found nothing")
	}
	for _, raw := range filtered["deals"].([]any) {
		d := raw.(map[string]any)
		if !strings.Contains(strings.ToUpper(d["title"].(string)), "GLM") {
			t.Errorf("q filter not applied: %v", d["title"])
		}
	}

	resp4, body4 := get(t, ts, "/api/v1/deals?limit=1&source=rss")
	defer resp4.Body.Close()
	if c := mustJSON(t, body4)["count"].(float64); c != 1 {
		t.Errorf("limit=1 returned %v", c)
	}

	resp5, body5 := get(t, ts, "/api/v1/deals?category=nope")
	defer resp5.Body.Close()
	if c := mustJSON(t, body5)["count"].(float64); c != 0 {
		t.Errorf("impossible category returned %v", c)
	}
}

func TestSourcesEndpointSummarisesHealth(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, body := get(t, ts, "/api/v1/sources")
	defer resp.Body.Close()
	got := mustJSON(t, body)
	items := got["sources"].([]any)
	if len(items) != 1 {
		t.Fatalf("sources = %d", len(items))
	}
	s := items[0].(map[string]any)
	if s["name"] != "rss" || s["enabled"] != true {
		t.Errorf("source shape wrong: %v", s)
	}
	if s["last"] == nil {
		t.Error("the last round report should be attached")
	}
	if _, leaked := s["url"].(string); leaked && strings.Contains(s["url"].(string), "query=") {
		t.Errorf("query parameters should be masked: %v", s["url"])
	}
}

func TestDigestServesMarkdownForOpenClaw(t *testing.T) {
	ts, app, _ := newTestServer(t)
	// Before anything is delivered the endpoint must answer clearly.
	resp, body := get(t, ts, "/api/v1/digest")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d (%s)", resp.StatusCode, body)
	}
	resp.Body.Close()

	fd, err := notify.NewFileDrop(app.OutreachDir(), "deal-hunter")
	if err != nil {
		t.Fatal(err)
	}
	if err := fd.Send(context.Background(), notify.NewMessage(notify.KindDaily, "🌅 测试日报")); err != nil {
		t.Fatal(err)
	}
	resp2, body2 := get(t, ts, "/api/v1/digest")
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("status = %d", resp2.StatusCode)
	}
	if ct := resp2.Header.Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
		t.Errorf("content type = %q", ct)
	}
	if !strings.Contains(string(body2), "测试日报") {
		t.Errorf("markdown body = %s", body2)
	}
}

func TestDashboardIsServedInline(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, body := get(t, ts, "/")
	defer resp.Body.Close()
	html := string(body)
	if resp.StatusCode != 200 || !strings.Contains(html, "<!doctype html>") {
		t.Fatalf("dashboard not served: %d", resp.StatusCode)
	}
	if !strings.Contains(html, "/api/v1/status") {
		t.Error("dashboard should call the local API")
	}
	// No external asset requests: the page must work on an isolated network.
	for _, forbidden := range []string{"http://", "https://cdn", "fonts.googleapis", "//unpkg"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("dashboard references an external asset: %s", forbidden)
		}
	}
}

// The panel shows conclusions first: a deal is one line, and the reasoning waits
// behind an explicit disclosure. Run history and link quality come from endpoints
// that already exist, and the page stays keyboard- and motion-safe.
func TestDashboardLayersDetailAndHistory(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, body := get(t, ts, "/")
	defer resp.Body.Close()
	html := string(body)
	for _, want := range []string{
		"aria-expanded", // per-deal detail is folded, not dumped on screen
		"aria-controls", // ... and the toggle names the block it owns
		"body.hidden",   // collapsed until asked
		"/api/v1/runs",  // the run history the backend already serves
		"采集记录",          // ... labelled in the user's language
		"链接成色",          // how many finds reached a verified vendor page
		`<button`,       // filters must be focusable, not clickable spans
		":focus-visible",
		"prefers-reduced-motion",
		"max-width:640px",
		"最后同步",
		"显示更多",                      // a long list is paged, not dumped
		"opened.has(d.fingerprint)", // ... and paging keeps rows the reader opened
		`href="/favicon.ico"`,       // same-origin icon, so nothing 404s on load
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// A link nested in a <summary> is invalid HTML and swallows the toggle's
	// click, which is why the disclosure is a button over a hidden block.
	if strings.Contains(html, "<summary>") {
		t.Error("dashboard must not nest interactive content inside a <summary>")
	}
}

// A repost of an already reported announcement must not appear twice in the
// list, but it must stay reachable and counted.
func TestDealsHideFoldedRepostsUnlessAsked(t *testing.T) {
	ts, app, _ := newTestServer(t)
	dup := &model.Deal{
		URL: "https://other.test/glm-free", Title: "【公告】智谱 GLM-5.3-flash 限时免费开放",
		Source: "rss-clone", Category: model.CatAIFree, IsFree: true, Score: 70,
		DiscoveredAt: time.Now().UTC(), Meta: map[string]string{"dup_of": "original"},
	}
	if err := app.Store().Save(dup); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, ts, "/api/v1/deals?limit=100&min=0")
	if strings.Contains(string(body), "other.test/glm-free") {
		t.Error("a folded repost leaked into the default list")
	}
	_, body2 := get(t, ts, "/api/v1/deals?limit=100&min=0&dupes=1")
	if !strings.Contains(string(body2), "other.test/glm-free") {
		t.Error("dupes=1 should surface the repost")
	}
	if !strings.Contains(string(body2), `"folded_duplicates": 1`) {
		t.Errorf("the response should say how many were folded: %s", body2)
	}

	// A small page must not shrink the reported count of what was folded away.
	for i := 0; i < 3; i++ {
		extra := *dup
		extra.Fingerprint = ""
		extra.URL = "https://other.test/glm-free-" + string(rune('1'+i))
		extra.Meta = map[string]string{"dup_of": "original"}
		if err := app.Store().Save(&extra); err != nil {
			t.Fatal(err)
		}
	}
	_, body3 := get(t, ts, "/api/v1/deals?limit=2&min=0")
	if !strings.Contains(string(body3), `"folded_duplicates": 4`) {
		t.Errorf("folded count should cover the whole store: %s", body3)
	}
	if n := strings.Count(string(body3), `"fingerprint"`); n != 2 {
		t.Errorf("limit should still cap the page, got %d deals", n)
	}
	_, status := get(t, ts, "/api/v1/status")
	if !strings.Contains(string(status), `"duplicates": 4`) {
		t.Errorf("status should report the duplicate count: %s", status)
	}
}

func TestFaviconIsServedFromTheEmbed(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, body := get(t, ts, "/favicon.ico")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("favicon status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "image/svg") {
		t.Errorf("content type = %q", ct)
	}
	if !strings.Contains(string(body), "<svg") {
		t.Errorf("favicon body = %s", body)
	}
}

func TestRunsEndpoint(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, body := get(t, ts, "/api/v1/runs?limit=5")
	defer resp.Body.Close()
	got := mustJSON(t, body)
	runs, ok := got["runs"].([]any)
	if !ok || len(runs) == 0 {
		t.Fatalf("runs missing: %s", body)
	}
	first := runs[0].(map[string]any)
	if first["trigger"] != "test" {
		t.Errorf("run payload = %v", first)
	}
}

// 日间模式必须由门禁保证两件事：浅色块确实重定义了配色，而且文字在底色上读得清
// （WCAG AA 的 4.5:1）。配色写在 CSS 里，Go 测不到像素，但"忘了配浅色变量"和
// "浅底配浅字"这两类真事故是可以拦住的。
func TestDashboardHasAReadableLightThemeAndAWayToSwitch(t *testing.T) {
	ts, _, _ := newTestServer(t)
	_, body := get(t, ts, "/")
	html := string(body)

	for _, want := range []string{`data-theme`, `dh-theme`, `prefers-color-scheme`} {
		if !strings.Contains(html, want) {
			t.Errorf("theme switch missing %q", want)
		}
	}
	dark := cssVars(t, html, ":root")
	light := cssVars(t, html, "[data-theme=light]")
	for _, key := range []string{"--bg", "--panel", "--txt", "--dim", "--line"} {
		if _, ok := light[key]; !ok {
			t.Errorf("the light theme does not redefine %s; it would render half-dark", key)
		}
	}
	for name, vars := range map[string]map[string]string{"dark": dark, "light": light} {
		if r := contrast(vars["--txt"], vars["--bg"]); r < 4.5 {
			t.Errorf("%s theme: body text on background contrasts %.2f:1, want >= 4.5", name, r)
		}
	}
	// 白色半透明覆盖层留在浅色底上会直接看不见，必须全部收进变量。
	if n := strings.Count(html, "rgba(255,255,255"); n > 0 {
		t.Errorf("%d hardcoded white overlays left; move them behind --glass", n)
	}
}

// cssVars reads the "--x:#hex" assignments of one CSS block by name.
func cssVars(t *testing.T, html, block string) map[string]string {
	t.Helper()
	i := strings.Index(html, block)
	if i < 0 {
		t.Fatalf("css block %q not found in the panel", block)
	}
	rest := html[i+len(block):]
	end := strings.Index(rest, "}")
	if end < 0 {
		t.Fatalf("css block %q is unterminated", block)
	}
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`(--[a-z0-9]+)\s*:\s*(#[0-9a-fA-F]{6})`).FindAllStringSubmatch(rest[:end], -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatalf("no hex colours found in css block %q", block)
	}
	return out
}

// contrast is the WCAG 2.x ratio between two "#rrggbb" colours.
func contrast(fg, bg string) float64 {
	l1, ok1 := luminance(fg)
	l2, ok2 := luminance(bg)
	if !ok1 || !ok2 {
		return 0
	}
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

func luminance(hex string) (float64, bool) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, false
	}
	channels := []struct {
		at     int
		weight float64
	}{{1, 0.2126}, {3, 0.7152}, {5, 0.0722}}
	var out float64
	for _, c := range channels {
		v, err := strconv.ParseUint(hex[c.at:c.at+2], 16, 8)
		if err != nil {
			return 0, false
		}
		linear := float64(v) / 255
		if linear <= 0.03928 {
			linear /= 12.92
		} else {
			linear = math.Pow((linear+0.055)/1.055, 2.4)
		}
		out += linear * c.weight
	}
	return out, true
}

// 面板上"待提醒 3"这种混合计数回答不了读者的问题：下一次打扰是"要开抢了"还是
// "要作废了"。两个数必须分开显示。
func TestDashboardSplitsOpeningAndClosingReminders(t *testing.T) {
	ts, _, _ := newTestServer(t)
	_, body := get(t, ts, "/")
	html := string(body)
	for _, want := range []string{"due_opening", "due_expiry"} {
		if !strings.Contains(html, want) {
			t.Errorf("the panel never reads %s; it still shows one blended reminder count", want)
		}
	}
	// Read the whole expression that builds the hint: the labels sit on either
	// side of the numbers they describe, so slicing from one of them would drop
	// the other.
	hint := ""
	open, close := strings.Index(html, "due_opening"), strings.Index(html, "due_expiry")
	if open < 0 || close < 0 {
		hint = "（面板没有同时读取 due_opening 与 due_expiry）"
	} else {
		start := strings.LastIndex(html[:open], "\n")
		end := strings.Index(html[close:], ");")
		if start < 0 || end < 0 {
			hint = "（找不到 hint 表达式的边界）"
		} else {
			hint = html[start : close+end]
		}
	}
	if !strings.Contains(hint, "开抢") || !strings.Contains(hint, "到期") {
		t.Errorf("the hint must name both kinds, got %q", hint)
	}
}
