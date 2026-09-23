package httpapi

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// 一次都没跑过的库里没有"最近"这件事：零值 time.Time 会序列化成 0001-01-01，
// 面板照原样打印成"最近 1/1/1 08:05:43"，看着像一个真的日期。
func TestStatusHasNoTimestampsBeforeTheFirstRound(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Sources = nil
	app, err := pipeline.New(cfg, nil)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	t.Cleanup(func() { app.Close() })
	ts := httptest.NewServer(New(cfg, app, nil).Handler())
	t.Cleanup(ts.Close)

	_, body := get(t, ts, "/api/v1/status")
	store := mustJSON(t, body)["store"].(map[string]any)
	if store["deals_seen"] != float64(0) {
		t.Fatalf("the fixture is supposed to be a store that never ran, deals_seen=%v", store["deals_seen"])
	}
	for _, key := range []string{"oldest", "newest"} {
		if v := store[key]; v != "" {
			t.Errorf("store.%s = %v on an empty log; the panel would print a 0001-01-01 date as if it were real", key, v)
		}
	}

	// 接受侧：跑过一轮之后同一批字段必须是真时间戳，否则上面的"空"只是序列化坏了。
	seeded, _, _ := newTestServer(t)
	_, seededBody := get(t, seeded, "/api/v1/status")
	ran := mustJSON(t, seededBody)["store"].(map[string]any)
	if ran["deals_seen"] == float64(0) {
		t.Fatalf("the seeded server stored nothing, so it proves nothing about timestamps")
	}
	for _, key := range []string{"oldest", "newest"} {
		v, ok := ran[key].(string)
		if !ok {
			t.Fatalf("store.%s after a round = %T %v, want an RFC3339 string", key, ran[key], ran[key])
		}
		if _, err := time.Parse(time.RFC3339, v); err != nil {
			t.Errorf("store.%s = %q after a round: %v", key, v, err)
		}
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
	// 静默时长是"这个源多久没解析出过东西"。改版后返回 0 行的源不会报错，
	// 而面板原先只记得最近一轮 —— 重启之后就全忘了。
	idle, ok := s["idle_hours"].(float64)
	if !ok {
		t.Fatalf("idle_hours missing from %v", s)
	}
	if idle < 0 || idle > 1 {
		t.Errorf("a source that just ran should read as freshly heard, got %v", idle)
	}
	if hit, _ := s["last_hit"].(string); hit == "" {
		t.Errorf("last_hit should name the moment, got %v", s["last_hit"])
	}
}

// 面板上"多久没出声"必须比"最近一轮"更持久：改版后返回 0 行的源不报错，
// 而轮次记忆随重启消失。这里验的是渲染那段真的读了 idle_hours 并给出两种说法。
func TestDashboardShowsHowLongASourceHasBeenSilent(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp, body := get(t, ts, "/")
	defer resp.Body.Close()
	page := string(body)
	for _, want := range []string{"idle_hours", "上次出声", "从没解析出内容", "解析 "} {
		if !strings.Contains(page, want) {
			t.Errorf("the sources card should mention %q", want)
		}
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
	// 三态而不是两态：显式点过一次就把"跟随系统"永久毁掉了，傍晚系统转深色而面板不动。
	for _, tc := range []struct{ what, pat string }{
		{"回到跟随系统（撤销显式选择）", `removeItem\(['"]dh-theme`},
		{"系统换配色时当场跟着变", `addEventListener\(\s*['"]change`},
		{"夜间再点一次回到跟随系统", `dark\s*:\s*['"]auto['"]`},
		{"自动模式在落地时读系统偏好", `mode\s*===\s*['"]auto['"]\s*&&`},
		{"首屏与点击共用同一条判定", `dhIsLight\(dhThemeMode\(\)\)[\s\S]*dhIsLight\(mode\)`},
		{"按钮写的是当前所处模式，不是点下去会到哪", `textContent\s*=\s*ICON\[mode\]`},
	} {
		if matched, err := regexp.MatchString(tc.pat, html); err != nil {
			t.Errorf("bad pattern %s: %v", tc.pat, err)
		} else if !matched {
			t.Errorf("theme switch cannot %s; look for %q", tc.what, tc.pat)
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

// 面板把采集来的字符串塞进 innerHTML。挡在两处的东西各有一样：服务端 JSON 编码器把
// < > & 转成 \u003c 之类（Go 的默认，一旦被 SetEscapeHTML(false) 这种"性能优化"关掉，
// 面板就只剩自己那层），以及页面里 esc() 与 isHTTPS()。浏览器实测过一遍（投毒标题、
// 正文里的 <script>、javascript: 与 data: 链接：标题未被改写、0 个活的 img/svg、
// 0 个 javascript:/data: 锚点），但那条上不了构建机，所以这里钉住能被静态检查的两半。
func TestHostileStringsStayInertOnTheWayOut(t *testing.T) {
	hostile := `<img src=x onerror=document.title="PWNED"> 投毒标题`
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// 检材走真实的落盘格式：deals.jsonl 里的一行就是 store 原样读回来的那一条。
	line := `{"fingerprint":"xss1","url":"javascript:document.title=PWNED","title":"` +
		strings.ReplaceAll(hostile, `"`, `\"`) +
		`","source":"xss","category":"ai_free","score":90,"is_free":true,` +
		`"published_at":"2026-09-22T10:00:00Z","discovered_at":"2026-09-22T10:00:00Z","meta":{}}`
	if err := os.WriteFile(filepath.Join(dir, "deals.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Sources = nil
	app, err := pipeline.New(cfg, nil)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	t.Cleanup(func() { app.Close() })
	ts := httptest.NewServer(New(cfg, app, nil).Handler())
	t.Cleanup(ts.Close)

	resp, raw := get(t, ts, "/api/v1/deals?limit=5")
	resp.Body.Close()
	body := string(raw)
	if strings.Contains(body, "<img") {
		t.Errorf("the response carries a live <img - the encoder stopped escaping HTML:\n%s", body)
	}
	// 这条不查 `<img`（那正是要它被转义掉的），只查"投毒串确实进了载荷"，
	// 所以上面那条红的时候这条还是绿的 - 一条红只对应一件事。
	if !strings.Contains(body, `src=x onerror`) {
		t.Errorf("the hostile title never reached the payload, so this test checked nothing:\n%s", body)
	}
	// 编码器的转义是给浏览器的兜底，不是让 API 替面板洗数据：解码后必须还是原串，
	// 否则运营看到的标题已经被改过了。
	got := mustJSON(t, raw)["deals"].([]any)
	if len(got) != 1 {
		t.Fatalf("the fixture holds one deal, got %d", len(got))
	}
	if title := got[0].(map[string]any)["title"]; title != hostile {
		t.Errorf("title came back changed (%v) rather than merely escaped", title)
	}

	page := panelSource(t)
	if !strings.Contains(page, `/[&<>"]/g`) {
		t.Error(`esc() must replace all four of & < > " - the quote is what stops a breakout out of href="..."`)
	}
	hrefs := strings.Count(page, `href="' + `)
	guards := strings.Count(page, `isHTTPS(`)
	if hrefs < 3 {
		t.Fatalf("only %d concatenated hrefs read back; the panel changed shape and this check is now vacuous", hrefs)
	}
	if guards < hrefs {
		t.Errorf("%d hrefs are built from variables but only %d isHTTPS guards - a javascript: or data: URL can reach an anchor", hrefs, guards)
	}
}

func panelSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("web", "index.html"))
	if err != nil {
		t.Fatalf("read the panel: %v", err)
	}
	return string(b)
}

// The OpenClaw side of this project is three shell scripts and a skill document
// that a chat assistant reads. They talk to this package by URL string, so every
// "/api/v1/...?a=&b=" in them is a claim about a route and a query parameter this
// server implements - and until now nothing compared them against the route table
// (the same shape as the command-list and config-key gates: two places stating one
// fact). A parameter that quietly stops being read is the nasty case: the request
// still returns 200 and the assistant keeps answering with unfiltered data.
func TestOpenClawScriptsAndDocMatchTheRoutesTheyUse(t *testing.T) {
	src, err := os.ReadFile("httpapi.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := servedRoutes(t, string(src))
	if len(routes) < 5 {
		t.Fatalf("only %d routes were read back from the mux table - the parser is not seeing the code", len(routes))
	}
	// The deals route is the one with parameters; if the body scan cannot find
	// them, the parameter claims below would be checked against an empty set.
	if got := len(routes["/api/v1/deals"]); got < 5 {
		t.Fatalf("deals should read at least 5 query parameters, found %v", routes["/api/v1/deals"])
	}

	doc, err := os.ReadFile(filepath.Join("..", "..", "deploy", "openclaw", "deal-hunter.skill.md"))
	if err != nil {
		t.Fatal(err)
	}
	claimed := map[string]map[string]bool{}
	note := func(where, path string, params []string) {
		if claimed[path] == nil {
			claimed[path] = map[string]bool{}
		}
		for _, p := range params {
			if !routes[path][p] {
				t.Errorf("%s: %s asks for ?%s= but that route reads %v", where, path, p, keysOf(routes[path]))
			}
			claimed[path][p] = true
		}
		if _, ok := routes[path]; !ok {
			t.Errorf("%s: %s is not a route this server serves (%v)", where, path, keysOf(routes))
		}
	}

	for _, m := range regexp.MustCompile("`(GET )?(/api/v1/[a-z/]+)([?][a-z=&]+)?`").FindAllStringSubmatch(string(doc), -1) {
		note("技能说明", m[2], paramsOf(m[3]))
	}
	scripts, err := filepath.Glob(filepath.Join("..", "..", "deploy", "openclaw", "dealhunter-*.sh"))
	if err != nil || len(scripts) != 3 {
		t.Fatalf("expected the three query scripts, got %v (%v)", scripts, err)
	}
	for _, s := range scripts {
		body, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`/api/v1/[a-z/]+([?][A-Za-z0-9_=&$%{}.]*)?`).FindAllStringSubmatch(string(body), -1) {
			path := m[0]
			query := ""
			if i := strings.IndexByte(path, '?'); i >= 0 {
				query = path[i+1:]
				path = path[:i]
			}
			var names []string
			for _, seg := range strings.Split(query, "&") {
				if k, _, ok := strings.Cut(seg, "="); ok && k != "" {
					names = append(names, k)
				}
			}
			note(filepath.Base(s), path, names)
		}
	}

	// The other direction: an endpoint nobody is told about is an endpoint that
	// will be deleted by accident. /api/v1/openclaw/latest is the retired name of
	// /api/v1/digest, kept for the scripts already installed on the assistant -
	// it is documented by that comment, not by the table.
	for path := range routes {
		if !strings.HasPrefix(path, "/api/v1/") || path == "/api/v1/openclaw/latest" {
			continue
		}
		if !strings.Contains(string(doc), path) {
			t.Errorf("%s is served but the skill document never mentions it", path)
		}
	}
}

// servedRoutes reads the mux table out of this file's own source: path -> the set
// of query parameters that route's handler actually reads.
func servedRoutes(t *testing.T, src string) map[string]map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "httpapi.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]map[string]bool{}
	handlers := map[string]string{} // path -> method name
	for _, p := range file.Decls {
		fn, ok := p.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fn.Name.Name == "Handler" {
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "HandleFunc" {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok {
					return true
				}
				spec := strings.Trim(lit.Value, `"`)
				parts := strings.Fields(spec)
				if len(parts) != 2 {
					return true
				}
				recv, ok := call.Args[1].(*ast.SelectorExpr)
				if !ok {
					return true
				}
				handlers[parts[1]] = recv.Sel.Name
				return true
			})
		}
		body := map[string]bool{}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Get" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				return true
			}
			body[strings.Trim(lit.Value, `"`)] = true
			return true
		})
		for path, name := range handlers {
			if name == fn.Name.Name {
				routes[path] = body
			}
		}
	}
	return routes
}

func paramsOf(q string) []string {
	q = strings.TrimPrefix(q, "?")
	if q == "" {
		return nil
	}
	var out []string
	for _, seg := range strings.Split(q, "&") {
		if k, _, ok := strings.Cut(seg, "="); ok && k != "" {
			out = append(out, k)
		}
	}
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
