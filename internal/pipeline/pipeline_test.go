package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/httpx"
	"github.com/xiabee/deal-hunter/internal/model"
	"github.com/xiabee/deal-hunter/internal/notify"
	"github.com/xiabee/deal-hunter/internal/official"
	"github.com/xiabee/deal-hunter/internal/sources"
)

// cannedFetcher serves fixtures so the whole pipeline is tested offline.
type cannedFetcher struct {
	byURL  map[string][]byte
	status map[string]int
	mu     sync.Mutex
	calls  map[string]int
}

func (c *cannedFetcher) Get(_ context.Context, u string, _ map[string]string) (*httpx.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[u]++
	body, ok := c.byURL[u]
	if !ok {
		return nil, errors.New("no canned body for " + u)
	}
	status := 200
	if s, ok := c.status[u]; ok {
		status = s
	}
	return &httpx.Response{Status: status, FinalURL: u, Body: body}, nil
}

func (c *cannedFetcher) count(u string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[u]
}

// countPrefix reports how many requests went to a host or path prefix, so a
// test can assert that a cheaper rung made the expensive one unnecessary.
func (c *cannedFetcher) countPrefix(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for u, k := range c.calls {
		if strings.HasPrefix(u, prefix) {
			n += k
		}
	}
	return n
}

type spyNotifier struct {
	mu   sync.Mutex
	msgs []notify.Message
	err  error
}

func (s *spyNotifier) Name() string { return "spy" }

func (s *spyNotifier) Send(_ context.Context, m notify.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *spyNotifier) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

func (s *spyNotifier) all() []notify.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notify.Message(nil), s.msgs...)
}

func (s *spyNotifier) reset(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = nil
	s.err = err
}

const feedURL = "https://feeds.example/latest.rss"

func feedBody(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "sources", "testdata", "rss_deals.xml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testApp(t *testing.T, f sources.Fetcher, spy *spyNotifier, mutate func(*config.Config)) *App {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Server.Enabled = false
	cfg.Notify.Console = false
	cfg.Notify.OpenClaw.Enabled = false
	cfg.Notify.Feishu.Enabled = false
	cfg.Notify.Feishu.MinScore = 60
	cfg.Notify.Feishu.MaxPerRun = 6
	cfg.Notify.Digest.Enabled = false
	cfg.Sources = []config.Source{{Name: "rss", Kind: config.KindRSS, URL: feedURL, Trust: 8}}
	if mutate != nil {
		mutate(cfg)
	}
	app, err := New(cfg, nil, WithFetcher(f), WithNotifiers(spy))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { app.Close() })
	return app
}

func TestRunOnceStoresScoresAndAlertsOnce(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, nil)

	run, err := app.RunOnce(context.Background(), "unit")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if run.Stored == 0 {
		t.Fatalf("nothing stored: %+v", run)
	}
	if run.Pushed == 0 || spy.count() != 1 {
		t.Fatalf("expected one alert message, pushed=%d spy=%d", run.Pushed, spy.count())
	}
	msg := spy.all()[0]
	if msg.Kind != notify.KindAlert {
		t.Errorf("kind = %s", msg.Kind)
	}
	var bestScore int
	var sawGLM bool
	for i := range msg.Deals {
		d := &msg.Deals[i]
		if d.Score > bestScore {
			bestScore = d.Score
		}
		if strings.Contains(d.Title, "GLM-5.3-flash") {
			sawGLM = true
			if !d.IsFree || len(d.Offers) == 0 || len(d.ScoreWhy) == 0 {
				t.Errorf("deal not annotated: %+v", d)
			}
		}
	}
	if !sawGLM {
		t.Errorf("the free GLM item should be alerted: %+v", msg.Deals)
	}
	if bestScore < 60 {
		t.Errorf("alert score %d below the configured threshold", bestScore)
	}
	if len(run.Sources) != 1 || run.Sources[0].Found != 5 || run.Sources[0].Err != "" {
		t.Errorf("source report wrong: %+v", run.Sources)
	}
	if run.Trigger != "unit" || run.FinishedAt.Before(run.StartedAt) {
		t.Errorf("run metadata wrong: %+v", run)
	}

	// A second round must not re-alert the same findings.
	spy.reset(nil)
	run2, err := app.RunOnce(context.Background(), "unit2")
	if err != nil {
		t.Fatal(err)
	}
	if run2.NewDeals != 0 || run2.Pushed != 0 || spy.count() != 0 {
		t.Fatalf("dedup failed: %+v spy=%d", run2, spy.count())
	}
	if f.count(feedURL) != 2 {
		t.Errorf("expected 2 fetches, got %d", f.count(feedURL))
	}
}

// The curated vendor entry page answers, so the community link in the feed must
// be replaced by the official one — in the alert, in the store and in the run
// report, without ever contacting a search endpoint.
func TestRunOnceRewritesLinksToOfficialPages(t *testing.T) {
	const canonical = "https://open.bigmodel.cn/pricing"
	f := &cannedFetcher{byURL: map[string][]byte{
		feedURL:                               feedBody(t),
		canonical:                             []byte("<html>智谱价格</html>"),
		"https://www.aliyun.com/price/detail": []byte("<html>阿里云价格</html>"),
	}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, nil)
	run, err := app.RunOnce(context.Background(), "unit")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if run.Verified == 0 {
		t.Fatalf("a curated vendor page answered, so something must be verified: %+v", run)
	}
	if f.count(canonical) == 0 {
		t.Error("the canonical page should have been probed")
	}
	if n := f.countPrefix("https://lite.duckduckgo.com"); n != 0 {
		t.Errorf("rung 2 succeeded, so search must not run (got %d queries)", n)
	}

	var alerted *model.Deal
	msgs := spy.all()
	if len(msgs) != 1 {
		t.Fatalf("expected one alert, got %d", len(msgs))
	}
	for i := range msgs[0].Deals {
		d := &msgs[0].Deals[i]
		if strings.Contains(d.Title, "GLM-5.3-flash") {
			alerted = d
		}
	}
	if alerted == nil {
		t.Fatal("the GLM deal was not alerted")
	}
	if got := alerted.Meta[official.MetaOfficialURL]; got != canonical {
		t.Errorf("alert link = %q, want %q", got, canonical)
	}
	if got := alerted.Meta[official.MetaLinkKind]; got != official.KindVendorEntry {
		t.Errorf("link_kind = %q", got)
	}
	if got := alerted.Meta[official.MetaOriginalURL]; got != "https://www.example.com/news/glm-free" {
		t.Errorf("the original post must be recorded, got %q", got)
	}

	// The dashboard and the digest read from the store, so the resolved link has
	// to be persisted, not just sent.
	var stored *model.Deal
	deals := app.RecentDeals(50)
	for i := range deals {
		if strings.Contains(deals[i].Title, "GLM-5.3-flash") {
			stored = &deals[i]
		}
	}
	if stored == nil {
		t.Fatal("deal missing from the store")
	}
	if got := stored.Meta[official.MetaOfficialURL]; got != canonical {
		t.Errorf("stored link = %q, want the verified official page", got)
	}
}

func TestAlertThresholdIsHonoured(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) { c.Notify.Feishu.MinScore = 101 })
	run, err := app.RunOnce(context.Background(), "unit")
	if err != nil {
		t.Fatal(err)
	}
	if run.Stored == 0 {
		t.Error("findings should still be recorded")
	}
	if run.Pushed != 0 || spy.count() != 0 {
		t.Errorf("nothing should be alerted above 101, pushed=%d", run.Pushed)
	}
}

func TestMaxPerRunCapsOneMessage(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) {
		c.Notify.Feishu.MinScore = 55
		c.Notify.Feishu.MaxPerRun = 1
	})
	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatal(err)
	}
	if spy.count() != 1 {
		t.Fatalf("expected a single batched message, got %d", spy.count())
	}
	if n := len(spy.all()[0].Deals); n != 1 {
		t.Errorf("MaxPerRun not applied, %d deals in the message", n)
	}
}

func TestQuietHoursHoldAlertsAndDigestRecovers(t *testing.T) {
	quietHour := time.Now().UTC().Hour()
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) {
		c.Notify.Feishu.Enabled = true
		c.Notify.Feishu.Timezone = "UTC"
		c.Notify.Feishu.SilentHours = []int{quietHour}
		c.Notify.Digest.Enabled = true
		c.Notify.Digest.MinScore = 45
	})
	run, err := app.RunOnce(context.Background(), "unit")
	if err != nil {
		t.Fatal(err)
	}
	if run.HeldQuiet == 0 || run.Pushed != 0 {
		t.Fatalf("quiet hours must hold alerts: %+v", run)
	}
	if spy.count() != 0 {
		t.Error("nothing may be delivered while quiet")
	}
	if err := app.SendDigest(context.Background()); err != nil {
		t.Fatalf("SendDigest: %v", err)
	}
	if spy.count() != 1 {
		t.Fatalf("digest should recover the held findings, got %d messages", spy.count())
	}
	msg := spy.all()[0]
	if msg.Kind != notify.KindDigest || len(msg.Deals) == 0 {
		t.Errorf("digest payload wrong: %s %d", msg.Kind, len(msg.Deals))
	}
	// After a digest the same items must not be batched twice.
	spy.reset(nil)
	if err := app.SendDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spy.count() != 0 {
		t.Error("digest must not repeat delivered findings")
	}
}

func TestDeliveryFailureLeavesFindingsPending(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	spy := &spyNotifier{err: errors.New("webhook refused")}
	app := testApp(t, f, spy, func(c *config.Config) {
		c.Notify.Digest.Enabled = true
		c.Notify.Digest.MinScore = 45
	})
	run, err := app.RunOnce(context.Background(), "unit")
	if err == nil || !strings.Contains(err.Error(), "webhook refused") {
		t.Fatalf("the delivery error must surface: %v", err)
	}
	if run.Pushed != 0 {
		t.Errorf("failed delivery must not count as pushed: %+v", run)
	}
	if pending := app.Store().Pending(60, time.Time{}); len(pending) == 0 {
		t.Error("failed findings should remain pending for the digest")
	}
	spy.reset(nil)
	if err := app.SendDigest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spy.count() != 1 {
		t.Fatal("the retry path should deliver them in the next digest")
	}
}

func TestSourceFailureIsIsolated(t *testing.T) {
	f := &cannedFetcher{
		byURL:  map[string][]byte{feedURL: feedBody(t), "https://feeds.example/broken.rss": []byte("<xml>")},
		status: map[string]int{"https://feeds.example/broken.rss": 503},
	}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) {
		c.Sources = append(c.Sources, config.Source{Name: "broken", Kind: config.KindRSS, URL: "https://feeds.example/broken.rss"})
	})
	run, err := app.RunOnce(context.Background(), "unit")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected the broken source to report, got %v", err)
	}
	if run.Stored == 0 {
		t.Error("the healthy source must still be processed")
	}
	var sawErr bool
	for _, s := range run.Sources {
		if s.Name == "broken" && s.Err != "" {
			sawErr = true
		}
	}
	if !sawErr {
		t.Errorf("broken source should carry an error in its report: %+v", run.Sources)
	}
}

func TestNoiseAndExpiryAreNeverStored(t *testing.T) {
	body := []byte(`<?xml version="1.0"?><rss><channel>
		<item><title>【招聘】后端工程师一名</title><link>https://a.example/1</link><description>招聘免费内推</description></item>
		<item><title>GLM-5.3-flash 免费活动</title><link>https://a.example/2</link><description>限时免费，活动截止 2020-01-01</description></item>
		<item><title>无关新闻</title><link>https://a.example/3</link><description>今天发布了新版本，修复若干问题</description></item>
		<item><title>Qwen3.8-Max 开放免费额度</title><link>https://a.example/4</link><description>新用户免费 100 万 tokens</description></item>
	</channel></rss>`)
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: body}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, nil)
	run, err := app.RunOnce(context.Background(), "unit")
	if err != nil {
		t.Fatal(err)
	}
	if run.Stored != 1 {
		t.Fatalf("only the live Qwen offer should survive, stored=%d", run.Stored)
	}
	deals := app.RecentDeals(10)
	if len(deals) != 1 || !strings.Contains(deals[0].Title, "Qwen3.8-Max") {
		t.Fatalf("unexpected stored set: %+v", deals)
	}
}

func TestRequireOfferFilterCanBeDisabled(t *testing.T) {
	body := []byte(`<rss><channel><item><title>纯新闻标题</title><link>https://n.example/1</link><description>没有任何优惠</description></item></channel></rss>`)
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: body}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) { c.Filter.RequireOffer = false })
	run, err := app.RunOnce(context.Background(), "unit")
	if err != nil {
		t.Fatal(err)
	}
	if run.Stored != 1 {
		t.Errorf("with RequireOffer off the item should be kept, stored=%d", run.Stored)
	}
	if spy.count() != 0 {
		t.Error("a no-signal item must never reach the alert threshold")
	}
}

func TestGlobalKeywordGates(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	withDeny := testApp(t, f, &spyNotifier{}, func(c *config.Config) {
		c.Filter.DenyKeywords = []string{"GLM"}
	})
	run, err := withDeny.RunOnce(context.Background(), "unit")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("nil run")
	}
	for _, d := range withDeny.RecentDeals(50) {
		if strings.Contains(d.Title, "GLM") {
			t.Errorf("deny keyword ignored: %q", d.Title)
		}
	}

	f2 := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	withRequire := testApp(t, f2, &spyNotifier{}, func(c *config.Config) {
		c.Filter.RequireKeywords = []string{"门票"}
	})
	if _, err := withRequire.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatal(err)
	}
	for _, d := range withRequire.RecentDeals(50) {
		if !strings.Contains(d.Title+d.Summary, "门票") {
			t.Errorf("require keywords not enforced: %q", d.Title)
		}
	}
}

func TestStaleItemsAreDroppedByAge(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	app := testApp(t, f, &spyNotifier{}, func(c *config.Config) { c.Filter.MaxAgeHours = 1 })
	run, err := app.RunOnce(context.Background(), "unit")
	if err != nil {
		t.Fatal(err)
	}
	if run.Stored != 0 {
		t.Errorf("everything in the fixture predates the last hour, stored=%d", run.Stored)
	}
}

func TestRunHistoryIsBounded(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	app := testApp(t, f, &spyNotifier{}, nil)
	for i := 0; i < 3; i++ {
		if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
			t.Fatal(err)
		}
	}
	if app.LastRun() == nil {
		t.Fatal("LastRun empty")
	}
	if got := len(app.RecentRuns(2)); got != 2 {
		t.Errorf("RecentRuns(2) = %d", got)
	}
	if got := len(app.RecentRuns(0)); got != 3 {
		t.Errorf("RecentRuns(0) should return everything, got %d", got)
	}
}

func TestDigestDueFollowsCursor(t *testing.T) {
	app := testApp(t, &cannedFetcher{}, &spyNotifier{}, func(c *config.Config) {
		c.Notify.Digest.Enabled = true
		c.Notify.Digest.Every = config.Duration(6 * time.Hour)
	})
	if !app.DigestDue(time.Now()) {
		t.Error("a never-run digest is due")
	}
	if err := app.setDigestCursor(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if app.DigestDue(time.Now()) {
		t.Error("digest should not be due immediately after sending")
	}
	if !app.DigestDue(time.Now().Add(7 * time.Hour)) {
		t.Error("digest should be due after the interval")
	}
	off := testApp(t, &cannedFetcher{}, &spyNotifier{}, func(c *config.Config) { c.Notify.Digest.Enabled = false })
	if off.DigestDue(time.Now().AddDate(5, 0, 0)) {
		t.Error("a disabled digest is never due")
	}
}

func TestDigestAtTimeSchedule(t *testing.T) {
	app := testApp(t, &cannedFetcher{}, &spyNotifier{}, func(c *config.Config) {
		c.Notify.Digest.Enabled = true
		c.Notify.Digest.At = "08:30"
	})
	if err := app.setDigestCursor(time.Date(2026, 9, 18, 8, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	if app.DigestDue(time.Date(2026, 9, 18, 8, 0, 30, 0, time.Local)) {
		t.Error("must not fire before the scheduled time")
	}
	if !app.DigestDue(time.Date(2026, 9, 18, 8, 31, 0, 0, time.Local)) {
		t.Error("must fire after the scheduled time")
	}
}

func TestNotifyTestReachesBackends(t *testing.T) {
	spy := &spyNotifier{}
	app := testApp(t, &cannedFetcher{}, spy, nil)
	if err := app.NotifyTest(context.Background()); err != nil {
		t.Fatalf("NotifyTest: %v", err)
	}
	if spy.count() != 1 || spy.all()[0].Kind != notify.KindTest {
		t.Fatalf("expected one self-test message, got %d", spy.count())
	}
}

func TestConstructorGuards(t *testing.T) {
	if _, err := New(nil, nil); err == nil {
		t.Error("nil config must be rejected")
	}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Notify.Console = false
	cfg.Notify.Feishu.Enabled = false
	cfg.Notify.OpenClaw.Enabled = false
	if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), "backend") {
		t.Errorf("an app with no backends must fail loudly: %v", err)
	}
}

func TestFeishuStatusHelpers(t *testing.T) {
	app := testApp(t, &cannedFetcher{}, &spyNotifier{}, func(c *config.Config) {
		c.Notify.Feishu.Enabled = true
		c.Notify.Feishu.Timezone = "UTC"
	})
	if app.FeishuReady() {
		t.Error("no webhook configured, so it cannot be ready")
	}
	if len(app.Backends()) == 0 {
		t.Error("injected backends should be reported")
	}
	if app.OutreachDir() != "" {
		t.Error("openclaw drop is disabled in this fixture")
	}
}

func TestSourcePanicsAreContained(t *testing.T) {
	// A collector that fails to construct must not crash the round.
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	app := testApp(t, f, &spyNotifier{}, func(c *config.Config) {
		c.Sources = append(c.Sources, config.Source{Name: "bogus", Kind: "magic", URL: "https://x.example/"})
	})
	run, err := app.RunOnce(context.Background(), "unit")
	if err == nil {
		t.Fatal("expected the bogus source to report an error")
	}
	if run.Stored == 0 {
		t.Error("the valid source must still be processed")
	}
}

var _ = notify.DealsOf
