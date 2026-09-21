package pipeline

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

// set replaces one canned body, so a test can make the next round see news.
func (c *cannedFetcher) set(u string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byURL == nil {
		c.byURL = map[string][]byte{}
	}
	c.byURL[u] = body
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

func (s *spyNotifier) byKind(k notify.Kind) []notify.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []notify.Message
	for _, m := range s.msgs {
		if m.Kind == k {
			out = append(out, m)
		}
	}
	return out
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

func titlesOf(deals []model.Deal) []string {
	out := make([]string, 0, len(deals))
	for _, d := range deals {
		out = append(out, d.Title)
	}
	return out
}

func testApp(t *testing.T, f sources.Fetcher, spy *spyNotifier, mutate func(*config.Config)) *App {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Server.Enabled = false
	cfg.Notify.Console = false
	cfg.Notify.OpenClaw.Enabled = false
	cfg.Notify.Feishu.Enabled = false
	// A round interrupts nothing at this gate. Tests that want a breakthrough
	// lower it, so "the briefing is the only scheduled message" stays the
	// default every other case runs under.
	cfg.Notify.Urgent.MinScore = 101
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

// nextFreeModelFeed is the same source one round later with one extra event: a
// different link and a different model name, so the collector sees a new finding
// rather than a repost of one it already has.
func nextFreeModelFeed(t *testing.T) []byte {
	b := feedBody(t)
	b = bytes.ReplaceAll(b, []byte("GLM-5.3-flash"), []byte("GLM-5.4-flash"))
	return bytes.Replace(b, []byte("news/glm-free"), []byte("news/glm-5-4-free"), 1)
}

// The daily model's core promise: a round that finds something good still sends
// nothing. Findings wait for the briefing unless they clear the urgent gate.
func TestRunOnceStoresFindingsWithoutSending(t *testing.T) {
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
	if spy.count() != 0 || run.Pushed != 0 {
		t.Fatalf("a round must not send below the urgent gate: messages=%d pushed=%d", spy.count(), run.Pushed)
	}

	var sawGLM bool
	for _, d := range app.RecentDeals(50) {
		if !strings.Contains(d.Title, "GLM-5.3-flash") {
			continue
		}
		sawGLM = true
		if !d.IsFree || len(d.Offers) == 0 || len(d.ScoreWhy) == 0 {
			t.Errorf("deal stored without annotation: %+v", d)
		}
		if d.Score < 60 {
			t.Errorf("a free named model should score high, got %d", d.Score)
		}
	}
	if !sawGLM {
		t.Error("the free GLM item should be in the store")
	}
	if len(run.Sources) != 1 || run.Sources[0].Found != 5 || run.Sources[0].Err != "" {
		t.Errorf("source report wrong: %+v", run.Sources)
	}
	if run.Trigger != "unit" || run.FinishedAt.Before(run.StartedAt) {
		t.Errorf("run metadata wrong: %+v", run)
	}

	// A second round must not re-store the same findings.
	run2, err := app.RunOnce(context.Background(), "unit2")
	if err != nil {
		t.Fatal(err)
	}
	if run2.NewDeals != 0 || spy.count() != 0 {
		t.Fatalf("dedup failed: %+v spy=%d", run2, spy.count())
	}
	if f.count(feedURL) != 2 {
		t.Errorf("expected 2 fetches, got %d", f.count(feedURL))
	}
}

// A finding above the urgent gate interrupts at once, and the day's single
// interrupt is then spent: the next big thing waits for the briefing rather than
// turning the breakthrough channel back into the old firehose. Nothing is lost
// by waiting, because the briefing re-reads the whole store.
func TestUrgentFindingInterruptsOncePerDay(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) { c.Notify.Urgent.MinScore = 60 })
	now := time.Now()

	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if spy.count() != 1 {
		t.Fatalf("one breakthrough expected, got %d messages", spy.count())
	}
	msg := spy.all()[0]
	if msg.Kind != notify.KindUrgent {
		t.Errorf("kind = %s", msg.Kind)
	}
	if !strings.Contains(strings.Join(titlesOf(msg.Deals), "|"), "GLM-5.3-flash") {
		t.Errorf("the free model should be the interrupt: %+v", msg.Deals)
	}
	if got := app.Urgent(now).SentToday; got != 1 {
		t.Fatalf("today's budget should read 1 sent, got %d", got)
	}

	spy.reset(nil)
	f.set(feedURL, nextFreeModelFeed(t))
	run2, err := app.RunOnce(context.Background(), "unit2")
	if err != nil {
		t.Fatal(err)
	}
	if run2.NewDeals == 0 {
		t.Fatal("the second feed should bring a new finding")
	}
	if run2.Pushed != 0 || spy.count() != 0 {
		t.Fatalf("the day already spent its interrupt: pushed=%d messages=%d", run2.Pushed, spy.count())
	}
	if run2.UrgentHeld == 0 {
		t.Error("the waiting finding must be reported as held, not quietly dropped")
	}
	var waits bool
	for _, d := range app.DailyPreview(now) {
		if strings.Contains(d.Title, "GLM-5.4-flash") {
			waits = true
		}
	}
	if !waits {
		t.Error("a held finding must show up in the next briefing")
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
	// The gate is lowered so this round interrupts: link resolution over a batch
	// happens for the findings about to be reported, which is now the briefing's
	// own rows or an urgent interrupt.
	app := testApp(t, f, spy, func(c *config.Config) { c.Notify.Urgent.MinScore = 60 })
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

	// The dashboard and the briefing read from the store, so the resolved link has
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
// that interrupt, so a badge in the panel means the same thing as one in the
// briefing.
func TestCheapLabelsCoverRowsThatNeverInterrupt(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, nil)
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
		c.Notify.Urgent.MinScore = 60
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
		t.Error("a slot that passed before this process started watching is not backfilled")
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
	if !strings.Contains(m.Intro, "新增 0 · 持续 1") {
		t.Errorf("the intro should separate new from still-open: %q", m.Intro)
	}
	if !strings.Contains(notify.DailyLine(&m.Deals[0], at), "已收录 3 天") {
		t.Error("the briefing must say how long the offer has been known")
	}
	// A snapshot is not a delivery: listing an offer in the briefing must not
	// mark it delivered, or a later breakthrough would skip it as already sent.
	for _, d := range app.RecentDeals(10) {
		if d.Meta["pushed"] == "true" {
			t.Errorf("briefing must not mark deals pushed: %+v", d)
		}
	}
	if app.DailyDue(at.Add(time.Hour)) {
		t.Error("must not fire twice on the same day")
	}
	if !app.DailyDue(at.Add(24 * time.Hour)) {
		t.Error("must be due again the next day")
	}
}

func TestBreakthroughCardIsCappedAtMaxItems(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) {
		c.Notify.Urgent.MinScore = 55
		c.Notify.Urgent.MaxItems = 1
	})
	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatal(err)
	}
	if spy.count() != 1 {
		t.Fatalf("one breakthrough message expected, got %d", spy.count())
	}
	if n := len(spy.all()[0].Deals); n != 1 {
		t.Errorf("max_items not applied, %d deals in the message", n)
	}
}

// A breakthrough that fails to deliver must not spend the day's budget, and the
// findings must not vanish with it: they are still live, so the briefing reports
// them. That fallback is what makes "wait for the daily" a safe default.
func TestFailedBreakthroughIsNotChargedAndTheBriefingCarriesIt(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	spy := &spyNotifier{err: errors.New("webhook refused")}
	app := testApp(t, f, spy, func(c *config.Config) { c.Notify.Urgent.MinScore = 60 })
	now := time.Now()

	run, err := app.RunOnce(context.Background(), "unit")
	if err == nil || !strings.Contains(err.Error(), "webhook refused") {
		t.Fatalf("the delivery error must surface: %v", err)
	}
	if run.Pushed != 0 {
		t.Errorf("a failed delivery must not count as sent: %+v", run)
	}
	if got := app.Urgent(now).SentToday; got != 0 {
		t.Errorf("a failed breakthrough must not spend the budget, sent_today=%d", got)
	}

	spy.reset(nil)
	if err := app.SendDaily(context.Background(), now); err != nil {
		t.Fatalf("SendDaily: %v", err)
	}
	msgs := spy.all()
	if len(msgs) != 1 {
		t.Fatalf("the briefing should deliver once, got %d", len(msgs))
	}
	if msgs[0].Kind != notify.KindDaily {
		t.Errorf("kind = %s", msgs[0].Kind)
	}
	if !strings.Contains(strings.Join(titlesOf(msgs[0].Deals), "|"), "GLM-5.3-flash") {
		t.Errorf("the finding the breakthrough could not deliver must reach the briefing: %+v", msgs[0].Deals)
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

// What an operator actually gets after install.sh: a brand-new data directory,
// nothing poked into its state by the test. A briefing that only ever becomes
// due once something has already sent it is a feature that does not exist, and
// the earlier "wait for the next slot" fix left it exactly that way.
func TestFreshInstallSendsItsFirstBriefing(t *testing.T) {
	spy := &spyNotifier{}
	// Two minutes out, truncated to the minute, so the slot always lands after
	// the moment this process armed itself.
	slot := time.Now().UTC().Add(2 * time.Minute)
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	app := testApp(t, f, spy, func(c *config.Config) {
		c.Timezone = "UTC"
		c.Notify.Daily.At = slot.Format("15:04")
		// The round exists only to arm the schedule; collecting fixture rows would
		// put them in the briefing this test checks the payload of.
		c.Sources = nil
	})
	seed := &model.Deal{URL: "https://a.test/free", Title: "智谱 GLM-5.3-flash 限时免费",
		Source: "rss", Score: 88, IsFree: true, DiscoveredAt: time.Now().UTC().Add(-time.Hour)}
	seed.EnsureFingerprint()
	if err := app.Store().Save(seed); err != nil {
		t.Fatal(err)
	}
	// The schedule is armed by the first round, not by construction: read-only
	// commands build an App too and must not write state.
	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	spy.reset(nil)

	if app.DailyDue(time.Now().UTC()) {
		t.Error("the briefing must not be due before its slot")
	}
	due := slot.Add(time.Minute)
	if !app.DailyDue(due) {
		t.Fatal("a fresh install must become due at its first slot")
	}
	if err := app.SendDaily(context.Background(), due); err != nil {
		t.Fatalf("SendDaily: %v", err)
	}
	if spy.count() != 1 {
		t.Fatalf("exactly one briefing expected, got %d", spy.count())
	}
	if got := spy.all()[0].Deals; len(got) != 1 || got[0].Title != seed.Title {
		t.Errorf("briefing payload wrong: %+v", got)
	}
	if app.DailyDue(due.Add(time.Hour)) {
		t.Error("must not fire twice on the same day")
	}
	if !app.DailyDue(due.Add(24 * time.Hour)) {
		t.Error("must be due again the next day")
	}

	off := testApp(t, &cannedFetcher{}, &spyNotifier{}, func(c *config.Config) { c.Notify.Daily.Enabled = false })
	if off.DailyDue(time.Now().AddDate(0, 0, 5)) {
		t.Error("a disabled briefing is never due")
	}
}

// A day with nothing live still sends one message and still consumes the slot:
// "nothing today" answers whether the radar is running, and re-firing the empty
// report every round for the rest of the day is exactly the noise the daily
// model exists to avoid.
func TestBriefingSendsEvenWhenNothingIsLive(t *testing.T) {
	spy := &spyNotifier{}
	app := testApp(t, &cannedFetcher{}, spy, nil)
	now := time.Now()

	if err := app.SendDaily(context.Background(), now); err != nil {
		t.Fatalf("SendDaily: %v", err)
	}
	if spy.count() != 1 {
		t.Fatalf("an empty day must still produce one briefing, got %d", spy.count())
	}
	msg := spy.all()[0]
	if len(msg.Deals) != 0 {
		t.Errorf("nothing is live, so nothing should be listed: %+v", msg.Deals)
	}
	if !strings.Contains(msg.Title, "没有在效") {
		t.Errorf("the title should say the day is empty, got %q", msg.Title)
	}
	if !app.Daily(now).SentToday {
		t.Error("an empty briefing must still consume today's slot")
	}
}

// The briefing is what the user acts on, so its links are checked when it is
// built. A round that interrupted nothing must still send verified pages.
func TestBriefingVerifiesItsOwnLinks(t *testing.T) {
	const found = "https://open.bigmodel.cn/pricing/glm-free"
	searchBody := []byte("<html><body><a href=\"" + found +
		"\" class='result-link'>GLM-5.3-flash 免费额度 - 智谱开放平台</a></body></html>")
	f := &cannedFetcher{
		byURL: map[string][]byte{
			feedURL: feedBody(t),
			found:   []byte("<html>GLM 免费额度</html>"),
		},
		byPrefix: map[string][]byte{"https://lite.duckduckgo.com/lite/": searchBody},
	}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, nil)
	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if spy.count() != 0 {
		t.Fatalf("the round should have sent nothing, got %d", spy.count())
	}
	if err := app.SendDaily(context.Background(), time.Now()); err != nil {
		t.Fatalf("SendDaily: %v", err)
	}

	msgs := spy.all()
	if len(msgs) != 1 {
		t.Fatalf("one briefing expected, got %d", len(msgs))
	}
	var inCard bool
	for _, d := range msgs[0].Deals {
		if strings.Contains(d.Title, "GLM-5.3-flash") && d.Meta[official.MetaOfficialURL] == found {
			inCard = true
		}
	}
	if !inCard {
		t.Fatalf("the briefing row should carry the verified vendor page: %+v", msgs[0].Deals)
	}
	// The panel reads the store, so the verdict has to be persisted too.
	var inStore bool
	for _, d := range app.RecentDeals(50) {
		if strings.Contains(d.Title, "GLM-5.3-flash") && d.Meta[official.MetaOfficialURL] == found {
			inStore = true
		}
	}
	if !inStore {
		t.Error("the resolved link must be written back, not just sent")
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

// 公告里写明的开抢时刻必须落到入库记录上，否则事件提醒永远看不到它。而且必须按配置
// 时区解释：服务常年跑 UTC，「上午10:00」是北京时间，差 8 小时等于每条都提醒错。
func TestRunOnceRecordsStartMomentInReadersZone(t *testing.T) {
	const voucherFeed = "https://nc.test/tzgg/voucher.rss"
	body, err := os.ReadFile(filepath.Join("..", "sources", "testdata", "voucher_notice.xml"))
	if err != nil {
		t.Fatal(err)
	}
	f := &cannedFetcher{byURL: map[string][]byte{voucherFeed: body}}
	app := testApp(t, f, &spyNotifier{}, func(c *config.Config) {
		c.Timezone = "Asia/Shanghai"
		c.Sources = []config.Source{{Name: "nc-voucher", Kind: config.KindRSS,
			URL: voucherFeed, Trust: 9, Category: model.CatVoucher}}
	})
	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var raw string
	var cat string
	for _, d := range app.RecentDeals(20) {
		if strings.Contains(d.Title, "洪城消费券") {
			raw, cat = d.Meta["starts_at"], d.Category
		}
	}
	if raw == "" {
		t.Fatal("no starts_at recorded; category was " + cat)
	}
	got, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("starts_at not RFC3339: %q", raw)
	}
	want := time.Date(2026, 9, 21, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if !got.Equal(want) {
		t.Errorf("starts_at = %v, want %v", got, want)
	}
	if cat != model.CatVoucher {
		t.Errorf("category = %q, want %q", cat, model.CatVoucher)
	}
}

// voucherZone is the zone the notice is written in: an announcement states its
// times in the reader's zone, never the server's, and the app under test is
// configured for Asia/Shanghai while the CI builder runs in UTC.
var voucherZone = time.FixedZone("CST", 8*3600)

// voucherFeed renders a notice whose opening time is `opens`, so the tests can
// place an event inside or outside the reminder window regardless of when they run.
func voucherFeed(opens time.Time) []byte {
	local := opens.In(voucherZone)
	return []byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><item>
<title>关于开展2026年洪城消费券发放的公告</title>
<link>https://nc.test/tzgg/202609/a.shtml</link>
<description>满100元减30元。本轮` + local.Format("1月2日15:04") + `开始发放，领完即止。核销期限至` +
		local.AddDate(0, 0, 20).Format("2006年1月2日") + `。</description>
<pubDate>` + opens.Add(-72*time.Hour).Format(time.RFC1123Z) + `</pubDate>
</item></channel></rss>`)
}

func voucherApp(t *testing.T, f sources.Fetcher, spy *spyNotifier, opens time.Time) *App {
	return testApp(t, f, spy, func(c *config.Config) {
		c.Timezone = "Asia/Shanghai"
		c.Notify.Event.MinScore = 50
		c.Notify.Event.Lead = config.Duration(45 * time.Minute)
		c.Notify.Event.LateGrace = config.Duration(15 * time.Minute)
		c.Sources = []config.Source{{Name: "nc-voucher", Kind: config.KindRSS,
			URL: feedURL, Trust: 9, Category: model.CatVoucher}}
	})
}

// 提前 30 分钟开抢的公告必须提醒一次，而且只提醒一次 —— 第二轮再发一遍就是骚扰。
func TestDatedEventRemindsOnceInsideItsWindow(t *testing.T) {
	opens := time.Now().Add(30 * time.Minute)
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: voucherFeed(opens)}}
	spy := &spyNotifier{}
	app := voucherApp(t, f, spy, opens)

	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	msgs := spy.all()
	if len(msgs) != 1 {
		t.Fatalf("one reminder expected, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Kind != notify.KindEvent {
		t.Errorf("kind = %s, want %s", msgs[0].Kind, notify.KindEvent)
	}
	if !strings.Contains(msgs[0].Title, "开抢") {
		t.Errorf("the title should name the moment, got %q", msgs[0].Title)
	}

	spy.reset(nil)
	if _, err := app.RunOnce(context.Background(), "unit2"); err != nil {
		t.Fatal(err)
	}
	if spy.count() != 0 {
		t.Errorf("must not remind twice, got %d", spy.count())
	}
}

// 窗口之外保持安静：提前三天发布的公告不该今天就提醒，而开抢时刻过去 15 分钟宽限之后
// 也不该再提 —— 那时提醒已经帮不上忙，只剩噪音。
func TestDatedEventStaysSilentOutsideItsWindow(t *testing.T) {
	for name, opens := range map[string]time.Time{
		"days early": time.Now().Add(72 * time.Hour),
		"long past":  time.Now().Add(-2 * time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			f := &cannedFetcher{byURL: map[string][]byte{feedURL: voucherFeed(opens)}}
			spy := &spyNotifier{}
			app := voucherApp(t, f, spy, opens)
			if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if spy.count() != 0 {
				t.Errorf("no reminder expected when the window has not opened or already closed, got %d", spy.count())
			}
		})
	}
}

// 两条事件挤进同一张卡时，抬头只放得下一个时刻，所以每一条都要在正文里带上自己的开抢时间。
func TestEventReminderCarriesEachMoment(t *testing.T) {
	now := time.Now()
	item := func(n int, opens time.Time) string {
		local := opens.In(voucherZone)
		return `<item><title>洪城消费券第` + strconv.Itoa(n) + `轮</title>
<link>https://nc.test/tzgg/202609/` + strconv.Itoa(n) + `.shtml</link>
<description>满100元减30元。本轮` + local.Format("1月2日15:04") + `开始发放，领完即止。
核销期限至` + local.AddDate(0, 0, 20).Format("2006年1月2日") + `。</description>
<pubDate>` + opens.Add(-24*time.Hour).Format(time.RFC1123Z) + `</pubDate></item>`
	}
	feed := []byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>` +
		item(1, now.Add(20*time.Minute)) + item(2, now.Add(40*time.Minute)) + `</channel></rss>`)
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feed}}
	spy := &spyNotifier{}
	app := voucherApp(t, f, spy, now.Add(20*time.Minute))
	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	msgs := spy.all()
	if len(msgs) != 1 || len(msgs[0].Deals) != 2 {
		t.Fatalf("one card with both rounds expected, got %+v", msgs)
	}
	if n := strings.Count(msgs[0].Plain(), "开抢"); n < 2 {
		t.Errorf("each row should state its own moment, found %d in:\n%s", n, msgs[0].Plain())
	}
}

// blockingFetcher parks a round mid-fetch until the test releases it, standing in
// for a slow upstream.
type blockingFetcher struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (b *blockingFetcher) Get(context.Context, string, map[string]string) (*httpx.Response, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return &httpx.Response{Status: 200, Body: []byte("<rss><channel></channel></rss>")}, nil
}

// A round may legitimately take a while: fifteen sources over a flaky link, up to
// the interval budget. The read-only panel must not be hostage to it — /api/v1/status
// answers with LastRun(), and a lock held across the round makes the dashboard hang.
func TestRoundHistoryReadersDoNotWaitForAnInFlightRound(t *testing.T) {
	f := &blockingFetcher{release: make(chan struct{}), entered: make(chan struct{})}
	app := testApp(t, f, &spyNotifier{}, nil)

	done := make(chan struct{})
	go func() {
		_, _ = app.RunOnce(context.Background(), "unit")
		close(done)
	}()
	<-f.entered // the round is now parked inside a fetch

	replies := make(chan int, 2)
	go func() { app.LastRun(); replies <- 1 }()
	go func() { replies <- len(app.RecentRuns(8)) }()

	for i := 0; i < 2; i++ {
		select {
		case <-replies:
		case <-time.After(3 * time.Second):
			close(f.release)
			<-done
			t.Fatal("a reader waited on the in-flight round; the panel would hang")
		}
	}
	close(f.release)
	<-done
}

// 提醒靠"到时候再看"，可运维的一方需要能问：现在在跟踪哪几场、几点开抢、提醒过没有。
// 只看 state.json 不叫可观察。
func TestUpcomingEventsReportsTheReminderState(t *testing.T) {
	opens := time.Now().Add(10 * time.Hour)
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: voucherFeed(opens)}}
	spy := &spyNotifier{}
	app := voucherApp(t, f, spy, opens)
	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	tracked := app.UpcomingEvents(time.Now())
	if len(tracked) != 1 {
		t.Fatalf("the announced event should be tracked, got %+v", tracked)
	}
	got := tracked[0]
	if got.Reminded || got.InWindow {
		t.Errorf("10 hours out is outside the window and un-reminded: %+v", got)
	}
	if got.StartsAt.IsZero() {
		t.Error("the start moment should be reported")
	}
	if !strings.Contains(got.Title, "洪城消费券") {
		t.Errorf("title = %q", got.Title)
	}
}

// expiryApp seeds one stored voucher whose deadline is `due` and collects nothing,
// so whatever the round sends is the deadline's doing alone.
func expiryApp(t *testing.T, due time.Time, lead time.Duration) (*App, *spyNotifier) {
	t.Helper()
	// The RSS collector refuses an empty channel, so serve one row that the
	// offer gate drops: what the round sends is then only the deadline's doing.
	empty := []byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><item>
<title>本周行业新闻汇总</title><link>https://nc.test/news/1</link>
<description>技术动态与产品发布，不涉及价格。</description>
<pubDate>` + time.Now().Add(-time.Hour).Format(time.RFC1123Z) + `</pubDate>
</item></channel></rss>`)
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: empty}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) {
		c.Timezone = "Asia/Shanghai"
		c.Notify.Event.MinScore = 50
		c.Notify.Event.ExpiryLead = config.Duration(lead)
	})
	d := &model.Deal{
		Title: "洪城消费券第三批", URL: "https://nc.test/tzgg/202609/d.shtml",
		Score: 78, IsFree: true, Category: model.CatVoucher,
		Offers:      []model.Offer{{Kind: model.KindFree}},
		PublishedAt: time.Now().Add(-24 * time.Hour), DiscoveredAt: time.Now().Add(-24 * time.Hour),
		Meta: map[string]string{"expires_at": due.Format(time.RFC3339)},
	}
	d.EnsureFingerprint()
	if err := app.st.Save(d); err != nil {
		t.Fatal(err)
	}
	return app, spy
}

// 日报是早上发的，"今天 23:59 作废"那句话在日报里读到时已经过了十几个小时。
// 所以到期前 3 小时要再开口一次，而且只开口一次。
func TestDeadlineInsideTheExpiryLeadRemindsOnce(t *testing.T) {
	app, spy := expiryApp(t, time.Now().Add(2*time.Hour), 3*time.Hour)
	for i := 0; i < 2; i++ {
		if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	msgs := spy.byKind(notify.KindEvent)
	if len(msgs) != 1 {
		t.Fatalf("one deadline reminder expected, got %d: %+v", len(msgs), msgs)
	}
	if len(msgs[0].Deals) != 1 || msgs[0].Deals[0].Title != "洪城消费券第三批" {
		t.Errorf("the reminder must carry the row whose deadline is near: %+v", msgs[0].Deals)
	}
}

// 窗口之外必须闭嘴：还有 5 天到期就提醒，等于把日报再发一遍。
func TestDeadlineOutsideTheExpiryLeadStaysSilent(t *testing.T) {
	for name, due := range map[string]time.Time{
		"still far":    time.Now().Add(5 * 24 * time.Hour),
		"already gone": time.Now().Add(-30 * time.Minute),
	} {
		t.Run(name, func(t *testing.T) {
			app, spy := expiryApp(t, due, 3*time.Hour)
			if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if n := len(spy.byKind(notify.KindEvent)); n != 0 {
				t.Errorf("no deadline reminder expected, got %d", n)
			}
		})
	}
}

// 提醒卡片的标题必须自己说清楚是"要开了"还是"要没了"：两种时刻要做的动作相反。
func TestDeadlineReminderTitleSaysWhatIsEnding(t *testing.T) {
	app, spy := expiryApp(t, time.Now().Add(2*time.Hour), 3*time.Hour)
	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	msgs := spy.byKind(notify.KindEvent)
	if len(msgs) != 1 {
		t.Fatalf("one reminder expected, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Title, "截止") {
		t.Errorf("title should name the closing moment, got %q", msgs[0].Title)
	}
}

// 两条都临近时按"还有多久发生"排序，而且一张卡最多 max_items 行；剩下的不是丢掉，
// 是等下一轮 / 明天，因为标记只在真的发出去之后才写。
func TestDueEventsOrderOpeningBeforeLaterDeadline(t *testing.T) {
	empty := []byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><item>
<title>本周行业新闻汇总</title><link>https://nc.test/news/1</link>
<description>技术动态与产品发布，不涉及价格。</description>
<pubDate>` + time.Now().Add(-time.Hour).Format(time.RFC1123Z) + `</pubDate>
</item></channel></rss>`)
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: empty}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) {
		c.Timezone = "Asia/Shanghai"
		c.Notify.Event.MinScore = 50
		c.Notify.Event.ExpiryLead = config.Duration(3 * time.Hour)
		c.Notify.Event.MaxItems = 2
	})
	save := func(fp, title string, meta map[string]string) {
		d := &model.Deal{Fingerprint: fp, Title: title, URL: "https://nc.test/" + fp, Score: 78,
			IsFree: true, Category: model.CatVoucher, Offers: []model.Offer{{Kind: model.KindFree}},
			PublishedAt: time.Now().Add(-time.Hour), DiscoveredAt: time.Now().Add(-time.Hour), Meta: meta}
		if err := app.st.Save(d); err != nil {
			t.Fatal(err)
		}
	}
	save("late", "傍晚截止的券", map[string]string{"expires_at": time.Now().Add(2 * time.Hour).Format(time.RFC3339)})
	save("soon", "半小时后开抢的券", map[string]string{"starts_at": time.Now().Add(30 * time.Minute).Format(time.RFC3339)})
	save("later", "两小时后才开抢的券", map[string]string{"starts_at": time.Now().Add(2 * time.Hour).Format(time.RFC3339)})

	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	msgs := spy.byKind(notify.KindEvent)
	if len(msgs) != 1 {
		t.Fatalf("one card expected, got %d", len(msgs))
	}
	if len(msgs[0].Deals) != 2 {
		t.Fatalf("max_items must cap the card at 2 rows, got %d", len(msgs[0].Deals))
	}
	if msgs[0].Deals[0].Fingerprint != "soon" {
		t.Errorf("the nearest moment should lead the card, got %s then %s",
			msgs[0].Deals[0].Fingerprint, msgs[0].Deals[1].Fingerprint)
	}
	if !strings.Contains(msgs[0].Intro, "开抢") || !strings.Contains(msgs[0].Intro, "截止") {
		t.Errorf("mixed rows must each name their own moment: %q", msgs[0].Intro)
	}
	// 第三行等下一轮：没发出去就不该留下"已提醒"的标记。
	if _, reminded := app.stateTime(eventRemindedPrefix + "later"); reminded {
		t.Error("a row that never left must not be marked as reminded")
	}
}

// 运维问"在跟踪哪些"时，只有截止时刻、从没写开抢时刻的行也算在跟踪 —— 否则
// "为什么没有提醒"这个问题只能靠猜。
func TestUpcomingEventsIncludesDeadlineOnlyRows(t *testing.T) {
	app, _ := expiryApp(t, time.Now().Add(2*time.Hour), 3*time.Hour)
	tracked := app.UpcomingEvents(time.Now())
	if len(tracked) != 1 {
		t.Fatalf("a row with only a deadline is still tracked, got %+v", tracked)
	}
	got := tracked[0]
	if got.Due != "截止" || got.ClosesAt.IsZero() {
		t.Errorf("the row must report its closing moment, got %+v", got)
	}
	if !got.InWindow {
		t.Error("two hours out with a 3h lead is inside the window")
	}
}

// 生产真实丢过一条：linux.do 的免费额度公告用「起至止」区间写有效期，没有"截止"字样。
// 解析不到截止时刻时，它算"没说何时结束的限时事件"，按 M3 的口径整条不进日报 —— 一条
// 89 分、还有十天有效期的免费额度，读者在日报里根本看不到。
func TestBriefingKeepsADatedEventWrittenAsARange(t *testing.T) {
	body := "9月30日前免费获取Qwen3.8-Flash，0.0×积分。新加坡时间：2026年9月18日10:00至2026年9月30日23:59"
	feed := []byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><item>
<title>9月30日前免费获取Qwen3.8-Flash</title><link>https://linux.test/t/2926925</link>
<description>` + body + `</description>
<pubDate>` + time.Now().Add(-time.Hour).Format(time.RFC1123Z) + `</pubDate>
</item></channel></rss>`)
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feed}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) {
		c.Timezone = "Asia/Shanghai"
		c.Sources = []config.Source{{Name: "linux-do", Kind: config.KindRSS, URL: feedURL, Trust: 8}}
	})
	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows := app.st.Live(app.cfg.Notify.Daily.MinScore, time.Now(), 45)
	if len(rows) != 1 {
		t.Fatalf("the range-worded offer must reach the briefing, got %d rows", len(rows))
	}
	if v := rows[0].Meta["expires_at"]; v == "" {
		t.Error("no deadline was read out of the range")
	} else if end, err := time.Parse(time.RFC3339, v); err != nil || end.Format("01-02") != "09-30" {
		t.Errorf("expires_at = %s, want 09-30", v)
	}
}

// 只读命令（events / doctor / probe）也要先 New 一个 App。如果构造过程就写 state.json，
// 那么运维"看一眼"就会换掉状态文件的所有者：以 root 跑一次 `sudo dealhunter events`，
// 服务下次要写状态时直接 permission denied 崩溃循环 —— 生产真实发生过（2026-09-20）。
func TestNewWritesNoState(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Server.Enabled = false
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Errorf("New() wrote the state file (err=%v); a read-only command must not dirty it", err)
	}
}

// 但日报的"本进程从何时起开始守排程"必须仍然被记住 —— 那是新装上日报永不触发的死锁修复。
// 把它挪到第一轮采集开始时落盘。
func TestFirstRoundArmsTheBriefingSchedule(t *testing.T) {
	f := &cannedFetcher{byURL: map[string][]byte{feedURL: feedBody(t)}}
	spy := &spyNotifier{}
	app := testApp(t, f, spy, func(c *config.Config) {
		c.Notify.Daily.At = "00:00"
	})
	if _, err := app.RunOnce(context.Background(), "unit"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok := app.stateTime(dailyWatched); !ok {
		t.Error("a real round must arm the briefing schedule, or a fresh install never sends")
	}
}
