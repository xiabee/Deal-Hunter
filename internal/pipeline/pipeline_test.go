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
	byURL    map[string][]byte
	byPrefix map[string][]byte
	status   map[string]int
	mu       sync.Mutex
	calls    map[string]int
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
		for p, b := range c.byPrefix {
			if strings.HasPrefix(u, p) {
				body, ok = b, true
				break
			}
		}
	}
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

// A community post is only rewritten when the vendor's own site turns up a page
// matching this deal's words and that page answers. The entry page is attached
// as a side door, and the store keeps every verdict so the panel agrees with the
// card.
func TestRunOnceRewritesLinksToOfficialPages(t *testing.T) {
	const (
		entry       = "https://open.bigmodel.cn/pricing"
		found       = "https://open.bigmodel.cn/pricing/glm-free"
		aliyunEntry = "https://www.aliyun.com/price/detail"
	)
	searchBody := []byte("<html><body>" +
		"<a href=\"https://linux.do/t/9\" class='result-link'>别人也在说</a>" +
		"<a href=\"" + found + "\" class='result-link'>GLM-5.3-flash 免费额度 - 智谱开放平台</a>" +
		"</body></html>")
	f := &cannedFetcher{
		byURL: map[string][]byte{
			feedURL:     feedBody(t),
			entry:       []byte("<html>智谱价格</html>"),
			found:       []byte("<html>GLM 免费额度</html>"),
			aliyunEntry: []byte("<html>阿里云价格</html>"),
		},
		byPrefix: map[string][]byte{"https://lite.duckduckgo.com/lite/": searchBody},
	}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, nil)
	run, err := app.RunOnce(context.Background(), "unit")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if run.Verified == 0 {
		t.Fatalf("a vendor page answered, so something must be verified: %+v", run)
	}
	if f.count(entry) != 0 {
		t.Error("a deal already pointing at a verified vendor page needs no side door")
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
	if got := alerted.Meta[official.MetaOfficialURL]; got != found {
		t.Errorf("alert link = %q, want %q", got, found)
	}
	if got := alerted.Meta[official.MetaLinkKind]; got != official.KindSearchVerified {
		t.Errorf("link_kind = %q, want %q", got, official.KindSearchVerified)
	}
	if got := alerted.Meta[official.MetaVendorURL]; got != "" {
		t.Errorf("no side door is needed once a vendor page is presented, got %q", got)
	}
	// A deal we could not confirm keeps its own link and merely gains the
	// vendor's front door as a place to check.
	var second *model.Deal
	for i := range msgs[0].Deals {
		if strings.Contains(msgs[0].Deals[i].Title, "Qwen") {
			second = &msgs[0].Deals[i]
		}
	}
	if second == nil {
		t.Fatal("the Qwen deal was not alerted")
	}
	if got := second.Meta[official.MetaLinkKind]; got != official.KindThirdParty {
		t.Errorf("link_kind = %q, want third_party", got)
	}
	if got := second.Meta[official.MetaOfficialURL]; got != second.URL {
		t.Errorf("the post itself must remain the presented link, got %q", got)
	}
	if got := second.Meta[official.MetaVendorURL]; got != aliyunEntry {
		t.Errorf("vendor_url = %q, want the entry page %q", got, aliyunEntry)
	}
	if got := alerted.Meta[official.MetaOriginalURL]; got != "https://www.example.com/news/glm-free" {
		t.Errorf("the original post must be recorded, got %q", got)
	}
	if !strings.Contains(notify.Line(alerted), found) {
		t.Error("the plain-text form must carry the same link as the card")
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
	if got := stored.Meta[official.MetaOfficialURL]; got != found {
		t.Errorf("stored link = %q, want the verified official page", got)
	}
}

// Verdicts that cost nothing apply to every stored deal, not just the handful
// that reach the alert queue, so a badge in the panel means the same thing as
// one in the card.
func TestCheapLabelsApplyBelowThreshold(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) { c.Notify.Feishu.MinScore = 101 })
	run, err := app.RunOnce(context.Background(), "unit")
	if err != nil {
		t.Fatal(err)
	}
	if spy.count() != 0 || run.Pushed != 0 || run.Stored == 0 {
		t.Fatalf("expected stored-but-not-pushed, got %+v spy=%d", run, spy.count())
	}
	byURL := map[string]model.Deal{}
	for _, d := range app.RecentDeals(50) {
		byURL[d.URL] = d
	}
	drive := byURL["https://www.example.com/drive"] // 「某云盘」 names no vendor
	if drive.Meta[official.MetaLinkKind] != official.KindThirdParty {
		t.Errorf("an unattributable find must be marked third party: %+v", drive)
	}
	glm := byURL["https://www.example.com/news/glm-free"]
	if _, labelled := glm.Meta[official.MetaLinkKind]; labelled {
		t.Errorf("a known vendor must not be judged without looking: %v", glm.Meta)
	}
}

// Two feeds carrying the same announcement must produce one alert, not two.
func TestRepostOfTheSameAnnouncementIsFolded(t *testing.T) {
	rss := func(link, title string) []byte {
		return []byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>` +
			`<item><title>` + title + `</title><link>` + link +
			`</link><description>官方公告：GLM-5.3-flash 面向所有用户免费开放，API 调用 0 元。</description>` +
			`<pubDate>Mon, 14 Sep 2026 08:00:00 GMT</pubDate></item></channel></rss>`)
	}
	const aURL = "https://feed-a.test/latest.rss"
	const bURL = "https://feed-b.test/latest.rss"
	f := &cannedFetcher{byURL: map[string][]byte{
		aURL: rss(aURL, "智谱 GLM-5.3-flash 限时免费开放"),
		bURL: rss(bURL, "【公告】智谱 GLM-5.3-flash 限时免费开放！"),
	}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) {
		c.Sources = []config.Source{
			{Name: "a", Kind: config.KindRSS, URL: aURL, Trust: 8},
			{Name: "b", Kind: config.KindRSS, URL: bURL, Trust: 5},
		}
	})
	run, err := app.RunOnce(context.Background(), "unit")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if run.Stored != 2 || run.Dupes != 1 {
		t.Fatalf("both reposts should be recorded, one folded: %+v", run)
	}
	msgs := spy.all()
	if len(msgs) != 1 || len(msgs[0].Deals) != 1 {
		t.Fatalf("exactly one alert with one deal, got %d messages: %+v", len(msgs), msgs)
	}
	if got := msgs[0].Deals[0].URL; !strings.Contains(got, "feed-a.test") {
		t.Errorf("the first finding seen should be the one pushed, got %s", got)
	}
	var folded bool
	for _, d := range app.RecentDeals(20) {
		if strings.Contains(d.URL, "feed-b.test") && d.Meta["dup_of"] == "" {
			t.Error("the repost should record which finding it repeats")
		}
		if d.Meta["dup_of"] != "" {
			folded = true
		}
	}
	if !folded {
		t.Error("no deal was tagged as a repeat")
	}
}

// The briefing is scheduled in the user's timezone, not the host's: the service
// runs in UTC while the reader is on Asia/Shanghai.
func TestDailyBriefingFollowsTheConfiguredZoneAndSendsOnce(t *testing.T) {
	spy := &spyNotifier{}
	app := testApp(t, &cannedFetcher{}, spy, nil)
	// 09:00 Beijing on a day the host clock (UTC) is somewhere else entirely.
	at := time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC)

	seed := []*model.Deal{
		{URL: "https://a.test/free", Title: "GLM 免费额度", Source: "rss", Score: 88, IsFree: true,
			DiscoveredAt: at.Add(-72 * time.Hour)},
		{URL: "https://b.test/act", Title: "上周截止的活动", Source: "rss", Score: 90, IsFree: true,
			DiscoveredAt: at.Add(-10 * time.Hour),
			Meta:         map[string]string{"expires_at": at.Add(-time.Hour).Format(time.RFC3339)}},
	}
	for _, d := range seed {
		d.EnsureFingerprint()
		if err := app.Store().Save(d); err != nil {
			t.Fatal(err)
		}
	}

	before := at.Add(-time.Minute) // 08:59 Beijing
	if app.DailyDue(before) || app.DailyDue(at) {
		t.Error("a briefing that was never sent must wait for its slot, not fire on first start")
	}
	// Pretend yesterday's briefing happened, so today's slot is genuinely due.
	if err := app.SendDaily(context.Background(), at.AddDate(0, 0, -1)); err != nil {
		t.Fatalf("seed SendDaily: %v", err)
	}
	spy.reset(nil)
	if app.DailyDue(before) {
		t.Error("08:59 Beijing is too early")
	}
	if !app.DailyDue(at) {
		t.Error("09:00 Beijing should be due")
	}

	if err := app.SendDaily(context.Background(), at); err != nil {
		t.Fatalf("SendDaily: %v", err)
	}
	msgs := spy.all()
	if len(msgs) != 1 {
		t.Fatalf("expected one briefing, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Kind != notify.KindDaily {
		t.Errorf("kind = %s", m.Kind)
	}
	if len(m.Deals) != 1 || !strings.Contains(m.Deals[0].Title, "GLM") {
		t.Fatalf("only the live offer belongs in the briefing: %+v", m.Deals)
	}
	if !strings.Contains(notify.DailyLine(&m.Deals[0], at), "已收录 3 天") {
		t.Error("the briefing must say how long the offer has been known")
	}
	// A snapshot does not consume the alert queue.
	if got := app.Store().Pending(45, time.Time{}); len(got) != 2 {
		t.Errorf("daily must not mark deals pushed, pending = %d", len(got))
	}
	if app.DailyDue(at.Add(time.Hour)) {
		t.Error("must not fire twice on the same day")
	}
	if !app.DailyDue(at.Add(24 * time.Hour)) {
		t.Error("must be due again the next day")
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
