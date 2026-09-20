package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/httpx"
	"github.com/xiabee/deal-hunter/internal/model"
)

// stubFetcher keeps every collector test offline and deterministic.
type stubFetcher struct {
	body   []byte
	status int
	err    error
	url    string
	header map[string]string
	calls  int
	byURL  map[string][]byte
}

func newStub(name string, body []byte) *stubFetcher {
	return &stubFetcher{body: body, byURL: map[string][]byte{name: body}}
}

func (s *stubFetcher) Get(_ context.Context, u string, hdr map[string]string) (*httpx.Response, error) {
	s.calls++
	s.url = u
	s.header = hdr
	if s.err != nil {
		return nil, s.err
	}
	body := s.body
	if byURL, ok := s.byURL[u]; ok {
		body = byURL
	}
	st := s.status
	if st == 0 {
		st = 200
	}
	return &httpx.Response{Status: st, FinalURL: u, Body: append([]byte(nil), body...), Header: http.Header{}}, nil
}

type fakeState struct{ m map[string]json.RawMessage }

func (f *fakeState) GetState(k string) ([]byte, bool) {
	v, ok := f.m[k]
	return append([]byte(nil), v...), ok
}
func (f *fakeState) PutState(k string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f.m[k] = b
	return nil
}
func newFakeState() *fakeState { return &fakeState{m: map[string]json.RawMessage{}} }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func mustSource(t *testing.T, d Deps) Source {
	t.Helper()
	s, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func titles(deals []*model.Deal) string {
	var out []string
	for _, d := range deals {
		out = append(out, d.Title)
	}
	return strings.Join(out, " | ")
}

func TestRSSParsesItemsLinksDatesAndEntities(t *testing.T) {
	cfg := config.Source{Name: "rss-test", Kind: config.KindRSS, URL: "https://www.example.com/latest.rss", Trust: 8}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: newStub("rss", fixture(t, "rss_deals.xml")), State: newFakeState()})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deals) != 5 {
		t.Fatalf("expected 5 items, got %d: %s", len(deals), titles(deals))
	}
	first := deals[0]
	if !strings.Contains(first.Title, "GLM-5.3-flash") {
		t.Errorf("title = %q", first.Title)
	}
	if first.URL != "https://www.example.com/news/glm-free" {
		t.Errorf("url = %q", first.URL)
	}
	if first.PublishedAt.UTC().Format("2006-01-02") != "2026-09-14" {
		t.Errorf("pubDate parsed as %s", first.PublishedAt)
	}
	if first.Source != "rss-test" || first.Meta["host"] != "www.example.com" {
		t.Errorf("meta not populated: %+v", first.Meta)
	}
	if !strings.Contains(deals[1].Summary, "首购五折 & ") {
		t.Errorf("XML entity not decoded: %q", deals[1].Summary)
	}
	if deals[1].URL != "https://www.example.com/promo/qwen" {
		t.Errorf("CDATA link not handled: %q", deals[1].URL)
	}
	if deals[1].PublishedAt.IsZero() {
		t.Error("dc:date should be understood")
	}
	if deals[2].URL != "https://www.example.com/jobs/1" {
		t.Errorf("relative link not resolved against the feed URL: %q", deals[2].URL)
	}
	if deals[3].Title == "" || !strings.Contains(deals[3].Title, "云盘") {
		t.Errorf("titleless item should fall back to the summary: %q", deals[3].Title)
	}
	if !strings.Contains(deals[4].Summary, "活动截止") {
		t.Errorf("content:encoded not used: %q", deals[4].Summary)
	}
	seen := map[string]bool{}
	for _, d := range deals {
		if d.Fingerprint == "" || seen[d.Fingerprint] {
			t.Fatalf("fingerprints must be present and unique: %+v", deals)
		}
		seen[d.Fingerprint] = true
	}
}

func TestRSSKeywordAllowAndDenyFilters(t *testing.T) {
	cfg := config.Source{
		Name: "kw", Kind: config.KindRSS, URL: "https://www.example.com/latest.rss",
		Keywords: []string{"免费"}, Deny: []string{"招聘"},
	}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: newStub("rss", fixture(t, "rss_deals.xml"))})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range deals {
		if strings.Contains(d.Title, "招聘") {
			t.Errorf("deny keyword leaked: %q", d.Title)
		}
		if !strings.Contains(d.Title+d.Summary, "免费") {
			t.Errorf("allow keyword not enforced: %q", d.Title)
		}
	}
	if len(deals) == 0 || len(deals) >= 5 {
		t.Errorf("expected a filtered subset, got %d", len(deals))
	}
}

func TestRSSLimitIsApplied(t *testing.T) {
	cfg := config.Source{Name: "lim", Kind: config.KindRSS, URL: "https://www.example.com/x.rss", Limit: 2}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: newStub("rss", fixture(t, "rss_deals.xml"))})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(deals) != 2 {
		t.Fatalf("limit not applied: %d", len(deals))
	}
}

func TestAtomFeedParsing(t *testing.T) {
	cfg := config.Source{Name: "atom", Kind: config.KindRSS, URL: "https://www.example.com/atom", Trust: 5}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: newStub("atom", fixture(t, "atom_deals.xml"))})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deals) != 2 {
		t.Fatalf("expected 2 entries, got %d: %s", len(deals), titles(deals))
	}
	if deals[0].URL != "https://www.example.com/deepseek-grant" {
		t.Errorf("alternate link not selected: %q", deals[0].URL)
	}
	if deals[0].PublishedAt.UTC().Format(time.RFC3339) != "2026-09-16T02:00:00Z" {
		t.Errorf("published = %s", deals[0].PublishedAt)
	}
	if len(deals[0].Tags) == 0 || deals[0].Tags[0] != "羊毛" {
		t.Errorf("atom category term not carried into tags: %+v", deals[0].Tags)
	}
	if !strings.Contains(deals[1].Summary, "普通新闻内容") {
		t.Errorf("xhtml content not decoded: %q", deals[1].Summary)
	}
}

func TestMalformedFeedFallsBackToTolerantParser(t *testing.T) {
	broken := []byte(`<?xml version="1.0"?><rss><channel><item>
		<title>腾讯云 & 阿里云联合特惠：轻量服务器 3 折</title>
		<link>https://www.example.com/deal</link>
		<description>裸 & 符号会让严格解析器失败。</description>
	</item></channel></rss>`)
	cfg := config.Source{Name: "broken", Kind: config.KindRSS, URL: "https://www.example.com/x.rss"}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: newStub("broken", broken)})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("tolerant parser should recover the item: %v", err)
	}
	if len(deals) != 1 || !strings.Contains(deals[0].Title, "3 折") {
		t.Fatalf("unexpected recovery: %+v", deals)
	}
}

func TestHTMLScraperSkipsScriptsAndResolvesLinks(t *testing.T) {
	cfg := config.Source{
		Name: "html", Kind: config.KindHTML, URL: "https://www.example.com/benefit",
		Keywords: []string{"免费", "折"}, Sites: []string{"example.com"},
	}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: newStub("html", fixture(t, "promo_page.html"))})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deals) == 0 {
		t.Fatal("expected promo segments")
	}
	all := titles(deals)
	if strings.Contains(all, "不应该被抓到") {
		t.Errorf("inline script text must not be scraped: %s", all)
	}
	var sawGLM, sawQwen, sawFree bool
	for _, d := range deals {
		switch {
		case strings.Contains(d.Title, "GLM-5.3-flash"):
			sawGLM = true
			if d.URL != "https://www.example.com/benefit/glm" {
				t.Errorf("relative promo link = %q", d.URL)
			}
		case strings.Contains(d.Title, "Qwen3.8-Max"):
			sawQwen = true
			if d.URL != "https://www.example.com/benefit/qwen" {
				t.Errorf("absolute link rewritten: %q", d.URL)
			}
		case strings.Contains(d.Title, "免费体验"):
			sawFree = true
			if d.URL != cfg.URL {
				t.Errorf("linkless segment should fall back to the page URL, got %q", d.URL)
			}
		}
	}
	if !sawGLM || !sawQwen || !sawFree {
		t.Errorf("missing expected promo items (glm=%v qwen=%v free=%v): %s", sawGLM, sawQwen, sawFree, all)
	}
	for _, d := range deals {
		if strings.Contains(d.Title, "普通导航文字") {
			t.Errorf("keyword gate not applied: %q", d.Title)
		}
	}
}

func TestJSONKindMapsDeclaredPaths(t *testing.T) {
	cfg := config.Source{
		Name: "json", Kind: config.KindJSON, URL: "https://api.example.com/v1/deals",
		Params: map[string]string{
			"items_path": "data.items", "title_path": "name", "url_path": "homepage",
			"summary_path": "summary", "time_path": "released", "id_path": "id",
			"url_prefix": "https://devbox.example.com",
		},
		Keywords: []string{"免费"},
	}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: newStub("json", fixture(t, "generic_api.json"))})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deals) != 1 {
		t.Fatalf("keyword gate failed, got %d: %s", len(deals), titles(deals))
	}
	d := deals[0]
	if !strings.HasPrefix(d.URL, "https://devbox.example.com/students") {
		t.Errorf("url_prefix not applied: %q", d.URL)
	}
	if d.Meta["external_id"] != "db-1" {
		t.Errorf("external id = %q", d.Meta["external_id"])
	}
	if d.PublishedAt.UTC().Format("2006-01-02") != "2026-09-10" {
		t.Errorf("released = %s", d.PublishedAt)
	}
}

func TestHNKindBuildsFallbackLinksAndStats(t *testing.T) {
	cfg := config.Source{
		Name: "hn", Kind: config.KindHN, URL: "https://hn.algolia.com/api/v1/search",
		Params: map[string]string{"query": "free credits", "tags": "story", "numericFilters": "points>10"},
		Limit:  20,
	}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: newStub("hn", fixture(t, "hn_hits.json"))})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deals) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(deals))
	}
	if !strings.Contains(deals[0].URL, "foo.example.com") {
		t.Errorf("external url lost: %q", deals[0].URL)
	}
	if !strings.Contains(deals[1].URL, "news.ycombinator.com/item?id=41000002") {
		t.Errorf("missing url should fall back to the HN item: %q", deals[1].URL)
	}
	if !strings.Contains(deals[0].Summary, "HN 128 分") {
		t.Errorf("points not surfaced: %q", deals[0].Summary)
	}
	// The outgoing request must carry the declared query parameters.
	captured := &stubFetcher{body: fixture(t, "hn_hits.json")}
	src2 := mustSource(t, Deps{Cfg: cfg, HTTP: captured})
	if _, err := src2.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"query=free+credits", "tags=story", "hitsPerPage=20"} {
		if !strings.Contains(captured.url, want) {
			t.Errorf("request %q missing %q", captured.url, want)
		}
	}
}

func TestSearchRotatesQueriesAndUnwrapsRedirects(t *testing.T) {
	cfg := config.Source{
		Name: "search", Kind: config.KindSearch, URL: "https://lite.duckduckgo.com/lite/",
		Params: map[string]string{"queries": "免费额度;free credits;GPU 赠送"},
		Trust:  9, Limit: 10,
	}
	state := newFakeState()
	body := fixture(t, "ddg_lite.html")

	first := &stubFetcher{body: body}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: first, State: state})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(first.url, urlEscape("免费额度")) {
		t.Errorf("first round should use query 1: %q", first.url)
	}
	if len(deals) == 0 {
		t.Fatal("expected search results")
	}
	var sawHelp bool
	for _, d := range deals {
		if strings.Contains(d.URL, "uddg=") || strings.Contains(d.URL, "duckduckgo.com") {
			t.Errorf("proxy href not unwrapped: %q", d.URL)
		}
		if strings.HasPrefix(d.URL, "//") {
			t.Errorf("protocol-relative href not normalized: %q", d.URL)
		}
		if d.URL == "https://help.example.com/model-studio/new-free-quota" {
			sawHelp = true
			if !strings.Contains(d.Summary, "阿里云百炼") {
				t.Errorf("snippet not associated: %q", d.Summary)
			}
			if d.Meta["query"] != "免费额度" {
				t.Errorf("query provenance missing: %v", d.Meta)
			}
		}
	}
	if !sawHelp {
		t.Errorf("expected the first organic hit, got %s", titles(deals))
	}

	// Second round must advance the rotation cursor.
	second := &stubFetcher{body: body}
	src2 := mustSource(t, Deps{Cfg: cfg, HTTP: second, State: state})
	if _, err := src2.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.url, "free%20credits") && !strings.Contains(second.url, "free+credits") {
		t.Errorf("query rotation failed: %q", second.url)
	}

	// A configured site filter is appended to the query.
	siteCfg := cfg
	siteCfg.Params = map[string]string{"queries": "免费额度", "site": "help.example.com"}
	third := &stubFetcher{body: body}
	src3 := mustSource(t, Deps{Cfg: siteCfg, HTTP: third, State: newFakeState()})
	if _, err := src3.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(third.url, "site%3Ahelp.example.com") {
		t.Errorf("site filter missing: %q", third.url)
	}
}

func TestSearchWithoutQueriesOrResultsIsHandled(t *testing.T) {
	src, err := New(Deps{Cfg: config.Source{Name: "s", Kind: config.KindSearch, URL: "https://lite.duckduckgo.com/lite/"},
		HTTP: &stubFetcher{body: []byte("<html>no results</html>")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Fetch(context.Background()); err == nil {
		t.Error("missing params.queries must be a config error")
	}
	cfg := config.Source{Name: "s2", Kind: config.KindSearch, URL: "https://lite.duckduckgo.com/lite/",
		Params: map[string]string{"queries": "test"}}
	src2 := mustSource(t, Deps{Cfg: cfg, HTTP: &stubFetcher{body: []byte("<html>no results</html>")}})
	deals, err := src2.Fetch(context.Background())
	if err != nil {
		t.Fatalf("an empty result page is not an error: %v", err)
	}
	if len(deals) != 0 {
		t.Errorf("expected no deals, got %d", len(deals))
	}
}

func TestSnapshotSeedsBaselineThenReportsOnlyNewFacts(t *testing.T) {
	cfg := config.Source{Name: "snap", Kind: config.KindSnapshot, URL: "https://www.example.com/benefit",
		Params: map[string]string{"mode": "html"}, Keywords: []string{"免费", "折"}}
	state := newFakeState()
	base := string(fixture(t, "promo_page.html"))

	seed := mustSource(t, Deps{Cfg: cfg, HTTP: &stubFetcher{body: []byte(base)}, State: state})
	deals, err := seed.Fetch(context.Background())
	if err != nil {
		t.Fatalf("baseline seed: %v", err)
	}
	if len(deals) != 0 {
		t.Fatalf("first run must only store a baseline, got %d deals", len(deals))
	}
	if _, ok := state.GetState("snapshot:facts:snap"); !ok {
		t.Fatal("baseline cursor missing")
	}

	again := mustSource(t, Deps{Cfg: cfg, HTTP: &stubFetcher{body: []byte(base)}, State: state})
	if deals, err := again.Fetch(context.Background()); err != nil || len(deals) != 0 {
		t.Fatalf("unchanged page must report nothing, deals=%d err=%v", len(deals), err)
	}

	changed := base + "\n<p>新上架：GLM-5.3-flash 免费额度 100 万 tokens 限时领取</p>\n"
	third := mustSource(t, Deps{Cfg: cfg, HTTP: &stubFetcher{body: []byte(changed)}, State: state})
	deals, err = third.Fetch(context.Background())
	if err != nil {
		t.Fatalf("changed page: %v", err)
	}
	if len(deals) != 1 || !strings.Contains(deals[0].Title, "GLM-5.3-flash") {
		t.Fatalf("expected exactly the new fact, got %s", titles(deals))
	}
	if deals[0].Meta["detected_by"] != "self_snapshot" {
		t.Errorf("provenance missing: %v", deals[0].Meta)
	}

	// Probe mode has no cursor: facts are reported so operators can see them.
	probe := mustSource(t, Deps{Cfg: cfg, HTTP: &stubFetcher{body: []byte(base)}})
	if deals, err := probe.Fetch(context.Background()); err != nil || len(deals) == 0 {
		t.Fatalf("probe mode should surface facts, deals=%d err=%v", len(deals), err)
	}
}

func TestSnapshotReportsErrorWhenPageHasNoPriceFacts(t *testing.T) {
	cfg := config.Source{Name: "empty", Kind: config.KindSnapshot, URL: "https://www.example.com/plain"}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: &stubFetcher{body: []byte("<html><body><p>这里没有任何价格信息</p></body></html>")}})
	if _, err := src.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "no price facts") {
		t.Fatalf("expected a clear diagnostic, got %v", err)
	}
}

func TestOpenRouterSeedsThenDetectsNewlyFreeModels(t *testing.T) {
	frozen := time.Unix(1800000000, 0).UTC()
	cfg := config.Source{Name: "or", Kind: config.KindOpenRouter, URL: "https://openrouter.ai/api/v1/models",
		Trust: 10, Params: map[string]string{"first_run_grace_days": "14"}}
	state := newFakeState()
	nowFunc := func() time.Time { return frozen }

	run1 := mustSource(t, Deps{Cfg: cfg, HTTP: newStub("or", fixture(t, "openrouter_baseline.json")), State: state, Now: nowFunc})
	deals, err := run1.Fetch(context.Background())
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if len(deals) != 2 {
		t.Fatalf("expected 2 recently created free models, got %d: %s", len(deals), titles(deals))
	}
	if !strings.Contains(deals[0].Title, "GLM-5.3 Flash") {
		t.Errorf("newest first ordering broken: %q", deals[0].Title)
	}
	for _, d := range deals {
		if d.Meta["baseline"] != "true" || !strings.Contains(d.Title, "免费") {
			t.Errorf("unexpected deal shape: %+v", d)
		}
		if d.URL == "" || !strings.Contains(d.URL, "openrouter.ai/") {
			t.Errorf("model link = %q", d.URL)
		}
	}
	var ids []string
	if b, ok := state.GetState(StateKeyFreeSet); ok {
		json.Unmarshal(b, &ids)
	}
	if len(ids) != 3 {
		t.Fatalf("cursor should hold all 3 free models, got %v", ids)
	}

	run2 := mustSource(t, Deps{Cfg: cfg, HTTP: newStub("or", fixture(t, "openrouter_updated.json")), State: state, Now: nowFunc})
	deals, err = run2.Fetch(context.Background())
	if err != nil {
		t.Fatalf("diff run: %v", err)
	}
	if len(deals) != 2 {
		t.Fatalf("expected the flipped model plus the new launch, got %d: %s", len(deals), titles(deals))
	}
	if !strings.Contains(deals[0].Title, "Nova Lite") {
		t.Errorf("newest-first ordering: %q", deals[0].Title)
	}
	if !strings.Contains(deals[1].Title, "Paid Model") {
		t.Errorf("a model that flipped to free must be reported: %q", deals[1].Title)
	}
	for _, d := range deals {
		if d.Meta["baseline"] == "true" {
			t.Error("diff results must not be marked as baseline")
		}
		if !strings.Contains(d.Summary, "此前为付费档") && !strings.Contains(d.Summary, "新增免费") {
			t.Errorf("summary should explain the trigger: %q", d.Summary)
		}
	}
}

func TestOpenRouterTreatsMissingPricingAsPaid(t *testing.T) {
	body := []byte(`{"data":[{"id":"a/b","name":"B","created":1799900000,"pricing":{"prompt":"","completion":""}}]}`)
	cfg := config.Source{Name: "or2", Kind: config.KindOpenRouter, URL: "https://openrouter.ai/api/v1/models"}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: &stubFetcher{body: body}, State: newFakeState(),
		Now: func() time.Time { return time.Unix(1800000000, 0).UTC() }})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(deals) != 0 {
		t.Fatalf("unparseable prices must never be reported as free: %s", titles(deals))
	}
}

func TestFetchFailuresSurfaceWithSourceNames(t *testing.T) {
	cfg := config.Source{Name: "flaky", Kind: config.KindRSS, URL: "https://www.example.com/x.rss"}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: &stubFetcher{status: 500, body: []byte("err")}})
	_, err := src.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "flaky") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should name the source and status: %v", err)
	}
	networkErr := mustSource(t, Deps{Cfg: cfg, HTTP: &stubFetcher{err: fmt.Errorf("dial tcp: refused")}})
	if _, err := networkErr.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("network error wrapped incorrectly: %v", err)
	}
}

func TestRegistryValidation(t *testing.T) {
	if _, err := New(Deps{Cfg: config.Source{Name: "x", Kind: "nope"}}); err == nil {
		t.Error("unknown kind must be rejected")
	}
	if _, err := New(Deps{Cfg: config.Source{Name: "x", Kind: config.KindRSS}}); err == nil {
		t.Error("a missing fetcher must be rejected")
	}
	for _, kind := range []string{config.KindRSS, config.KindHTML, config.KindJSON, config.KindHN, config.KindSearch, config.KindSnapshot, config.KindOpenRouter} {
		s, err := New(Deps{Cfg: config.Source{Name: "k-" + kind, Kind: kind, URL: "https://example.com/x"}, HTTP: &stubFetcher{body: []byte("<html></html>")}})
		if err != nil {
			t.Fatalf("kind %s: %v", kind, err)
		}
		if s.Name() != "k-"+kind || s.Kind() != kind {
			t.Errorf("kind %s identity wrong", kind)
		}
	}
}

func TestUserAgentAndHeadersAreSent(t *testing.T) {
	cfg := config.Source{Name: "hdr", Kind: config.KindRSS, URL: "https://www.example.com/x.rss",
		Headers: map[string]string{"X-Test": "1"}}
	stub := &stubFetcher{body: fixture(t, "rss_deals.xml")}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: stub})
	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stub.header["X-Test"] != "1" {
		t.Errorf("per-source headers dropped: %v", stub.header)
	}
}

func urlEscape(s string) string { return url.QueryEscape(s) }

// 政府门户的通知公告行普遍写成 <a>标题</a><i class="time">2026-09-11</i>，而 </a>
// 正好是段落边界，于是日期落进了下一段。没有发布日，正文里「9月21日10:00开抢」这类
// 不写年份的时刻就没有推断锚点，所以日期段必须挂回前一条。
func TestHTMLListRowDateAttachesToPreviousItem(t *testing.T) {
	list := []byte(`<html><body><ul>
<li><a href="https://nc.test/tzgg/202609/a.shtml">关于开展2026年洪城消费券发放的公告</a><i class="time">2026-09-11</i></li>
<li><a href="https://nc.test/tzgg/202608/b.shtml">关于举办业务培训的通知</a><i class="time">2026-08-02</i></li>
</ul></body></html>`)
	cfg := config.Source{Name: "nc-tzgg", Kind: config.KindHTML,
		URL: "https://nc.test/ncsswj/tzgg/index.shtml", Keywords: []string{"消费券"}}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: newStub("nc-tzgg", list)})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deals) != 1 {
		t.Fatalf("only the voucher row passes the gate, got: %s", titles(deals))
	}
	want := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	if !deals[0].PublishedAt.Equal(want) {
		t.Errorf("published = %v, want %v", deals[0].PublishedAt, want)
	}
}

// 开抢时间写在公告正文里，列表行只有标题。follow_detail 让采集器在关键词门之后再抓
// 一次正文 —— 顺序是要守住的：过不了门的行一次请求都不该发出，否则一轮 20 条列表
// 就变成 20 次外连去 hammer 政务站。
func TestHTMLFollowDetailFetchesOnlyGatedRows(t *testing.T) {
	const detail = "https://nc.test/tzgg/202609/a.shtml"
	list := []byte(`<html><body><ul>
<li><a href="` + detail + `">关于开展2026年洪城消费券发放的公告</a><i class="time">2026-09-11</i></li>
<li><a href="https://nc.test/tzgg/202608/b.shtml">关于举办业务培训的通知</a><i class="time">2026-08-02</i></li>
</ul></body></html>`)
	// 正文长度按真实公告取：实测徐汇 427 字、兴城 1.6 KB、高台 3.5 KB，都远在
	// detail_min_text_len=200 之上；写一条短夹具会让它被"频控页"守卫误杀。
	body := []byte(`<html><head><title>公告</title></head><body><div class="content">` +
		`现将2026年洪城消费券发放事项公告如下：一、发放时间：第一轮9月21日上午10:00开始发放，` +
		`第二轮10月1日上午10:00开始发放，领完即止。二、面额与用途：满100元减30元、满50元减15元，` +
		`限本市指定商超、餐饮主体使用。三、核销期限：自领取之日起至2026年10月15日止，逾期未使用自动作废。` +
		`四、发放平台：云闪付APP，进入"洪城消费券"活动页面定位本市后领取，每人每轮限领一份。` +
		`五、其他事项：活动期间请合理安排时间，如页面提示已领完则说明本轮额度已发放完毕。` +
		`</div></body></html>`)
	stub := &stubFetcher{body: list, byURL: map[string][]byte{detail: body}}
	cfg := config.Source{Name: "nc-tzgg", Kind: config.KindHTML,
		URL: "https://nc.test/ncsswj/tzgg/index.shtml", Keywords: []string{"消费券"},
		Params: map[string]string{"follow_detail": "1"}}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: stub})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deals) != 1 {
		t.Fatalf("gate should keep only the voucher row: %s", titles(deals))
	}
	if stub.calls != 2 {
		t.Errorf("want 1 list + 1 detail fetch, got %d", stub.calls)
	}
	if !strings.Contains(deals[0].Summary, "9月21日上午10:00") {
		t.Errorf("summary must come from the announcement body, got %q", deals[0].Summary)
	}
}

// 政务站会用 HTTP 200 返回一个"频繁访问"拦截页（实测遇到过）。这种假成功不能把列表行
// 已有的文本换成拦截页文字 —— 否则事件解析读到的是垃圾，而条目看起来却像抓到了正文。
func TestHTMLFollowDetailKeepsRowTextWhenBodyIsTooShort(t *testing.T) {
	const detail = "https://nc.test/tzgg/202609/a.shtml"
	list := []byte(`<html><body><ul>
<li><a href="` + detail + `">关于开展2026年洪城消费券发放的公告</a><i class="time">2026-09-11</i></li>
</ul></body></html>`)
	throttle := []byte(`<html><head><title>提示</title></head><body><p>频繁访问，请稍后再试。</p></body></html>`)
	stub := &stubFetcher{body: list, byURL: map[string][]byte{detail: throttle}}
	cfg := config.Source{Name: "nc-tzgg", Kind: config.KindHTML,
		URL: "https://nc.test/ncsswj/tzgg/index.shtml", Keywords: []string{"消费券"},
		Params: map[string]string{"follow_detail": "1"}}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: stub})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deals) != 1 {
		t.Fatalf("one gated row expected, got %s", titles(deals))
	}
	if strings.Contains(deals[0].Summary, "频繁访问") {
		t.Errorf("a throttle page must not become the body: %q", deals[0].Summary)
	}
	if !strings.Contains(deals[0].Summary, "消费券") {
		t.Errorf("the row text should survive: %q", deals[0].Summary)
	}
}

// 一段没有链接的文本会退回列表页自己的 URL。这时候再"二跳"就是把自己重抓一遍，
// 白涨一倍外连还什么新信息都拿不到。
func TestHTMLFollowDetailNeverRefetchesTheListPage(t *testing.T) {
	list := []byte(`<html><body>
<p>本页专题：洪城消费券发放安排汇总，点击各期公告查看。</p>
</body></html>`)
	stub := newStub("nc-tzgg", list)
	cfg := config.Source{Name: "nc-tzgg", Kind: config.KindHTML,
		URL: "https://nc.test/ncsswj/tzgg/index.shtml", Keywords: []string{"消费券"},
		Params: map[string]string{"follow_detail": "1"}}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: stub})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deals) != 1 {
		t.Fatalf("the summary paragraph should pass the gate, got %s", titles(deals))
	}
	if deals[0].URL != cfg.URL {
		t.Fatalf("linkless row should keep the list URL, got %q", deals[0].URL)
	}
	if stub.calls != 1 {
		t.Errorf("must not refetch the list page as its own detail, got %d calls", stub.calls)
	}
}

// 详情正文直接进 Summary，而 Summary 会写进 JSONL 并被面板整段渲染。实测政务与公众
// 号页面能到 MB 级，所以必须有上限；截断留在解析之后，保证开头的发放时间不会被切掉。
func TestHTMLFollowDetailCapsBodySize(t *testing.T) {
	const detail = "https://nc.test/tzgg/202609/a.shtml"
	list := []byte(`<html><body><ul>
<li><a href="` + detail + `">关于开展2026年洪城消费券发放的公告</a><i class="time">2026-09-11</i></li>
</ul></body></html>`)
	verbose := "现将2026年洪城消费券发放事项公告如下：9月21日上午10:00开始发放。" +
		strings.Repeat("本市指定商超餐饮主体均可参与，具体名单见附件说明。", 400)
	body := []byte("<html><body><div>" + verbose + "</div></body></html>")
	stub := &stubFetcher{body: list, byURL: map[string][]byte{detail: []byte(body)}}
	cfg := config.Source{Name: "nc-tzgg", Kind: config.KindHTML,
		URL: "https://nc.test/ncsswj/tzgg/index.shtml", Keywords: []string{"消费券"},
		Params: map[string]string{"follow_detail": "1"}}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: stub})
	deals, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(deals) != 1 {
		t.Fatalf("one gated row expected, got %s", titles(deals))
	}
	got := []rune(deals[0].Summary)
	if len(got) > 2000 {
		t.Errorf("body must be capped for storage, got %d runes", len(got))
	}
	if !strings.Contains(deals[0].Summary, "9月21日上午10:00") {
		t.Error("the cap must keep the beginning, where the start time is stated")
	}
}

// DuckDuckGo 认定出口 IP 像机器人时，回的是 HTTP 200 + 一张人机验证页：既没有结果也
// 没有错误。采集器原本把它归进"今天没搜到"，于是面板上看起来像羊毛断了 —— 2026-09-21
// 从生产机实测到就是这个状态（同一时刻另一台机器搜得到）。验证页必须单独报出来。
func TestSearchReportsTheHumanCheckPageInsteadOfZeroHits(t *testing.T) {
	cfg := config.Source{
		Name: "search-cn", Kind: config.KindSearch, URL: "https://lite.duckduckgo.com/lite/",
		Params: map[string]string{"queries": "免费额度"}, Trust: 9, Limit: 10,
	}
	src := mustSource(t, Deps{Cfg: cfg, HTTP: &stubFetcher{body: fixture(t, "ddg_anomaly.html")}, State: newFakeState()})
	_, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("a challenge page must not look like a successful empty round")
	}
	if !strings.Contains(err.Error(), "human") && !strings.Contains(err.Error(), "challenge") {
		t.Errorf("the error must name what happened, got %v", err)
	}
	if strings.Contains(err.Error(), "duckduckgo.com/anomaly.js") {
		t.Errorf("the error must not echo the challenge URL with its tokens: %v", err)
	}

	// 反面对照：真实结果页不能被这条判定误伤。
	ok := mustSource(t, Deps{Cfg: cfg, HTTP: &stubFetcher{body: fixture(t, "ddg_lite.html")}, State: newFakeState()})
	deals, err := ok.Fetch(context.Background())
	if err != nil {
		t.Fatalf("a normal results page must not be reported as a challenge: %v", err)
	}
	if len(deals) == 0 {
		t.Error("expected the normal fixture to still yield deals")
	}
}
