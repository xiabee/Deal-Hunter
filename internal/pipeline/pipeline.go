// Package pipeline runs one collection round: fetch every source, normalize,
// deduplicate, score, then deliver what is worth the user's attention.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/httpx"
	"github.com/xiabee/deal-hunter/internal/keywords"
	"github.com/xiabee/deal-hunter/internal/model"
	"github.com/xiabee/deal-hunter/internal/notify"
	"github.com/xiabee/deal-hunter/internal/official"
	"github.com/xiabee/deal-hunter/internal/redact"
	"github.com/xiabee/deal-hunter/internal/scoring"
	"github.com/xiabee/deal-hunter/internal/sources"
	"github.com/xiabee/deal-hunter/internal/store"
)

// SourceReport is the per-source outcome of one round.
type SourceReport struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Found  int    `json:"found"`
	Stored int    `json:"stored"`
	Pushed int    `json:"pushed"`
	Ms     int64  `json:"ms"`
	Err    string `json:"err,omitempty"`
}

// Run summarizes one round.
type Run struct {
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Trigger    string         `json:"trigger"`
	NewDeals   int            `json:"new_deals"`
	Stored     int            `json:"stored"`
	Pushed     int            `json:"pushed"`
	HeldQuiet  int            `json:"held_quiet"`
	Dupes      int            `json:"dupes_folded"`
	Verified   int            `json:"verified_official"`
	ThirdParty int            `json:"third_party_links"`
	Sources    []SourceReport `json:"sources"`
	Errors     []string       `json:"errors,omitempty"`
}

// prober confirms a candidate official page really answers, so we never rewrite
// a link to a URL that 404s.
type prober struct{ cli sources.Fetcher }

func (p prober) OK(ctx context.Context, rawURL string) (string, bool) {
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := p.cli.Get(c, rawURL, nil)
	if err != nil || resp == nil {
		return "", false
	}
	return resp.FinalURL, resp.Status >= 200 && resp.Status < 400
}

func (r *Run) duration() time.Duration {
	if r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// App owns the long-lived collaborators for the collector.
type App struct {
	cfg   *config.Config
	st    *store.Store
	cli   sources.Fetcher
	dict  *keywords.Dict
	nf    *notify.FanOut
	fs    *notify.Feishu
	relay *notify.OpenClawRelay
	log   *slog.Logger
	drop  string
	off   *official.Resolver

	mu      sync.Mutex
	runs    []*Run
	lastRun *Run
}

// Option customizes an App; tests use it to inject a fetcher and backends.
type Option func(*App)

// WithFetcher replaces the outbound HTTP client.
func WithFetcher(f sources.Fetcher) Option {
	return func(a *App) {
		if f != nil {
			a.cli = f
		}
	}
}

// WithNotifiers replaces the delivery backends so tests can assert on what
// would have been pushed without touching the network.
func WithNotifiers(ns ...notify.Notifier) Option {
	return func(a *App) {
		if len(ns) > 0 {
			a.nf = notify.NewFanOut(a.log, ns...)
		}
	}
}

// New opens the store and wires the notifiers described by cfg.
func New(cfg *config.Config, log *slog.Logger, opts ...Option) (*App, error) {
	if cfg == nil {
		return nil, errors.New("pipeline: nil config")
	}
	if log == nil {
		log = slog.Default()
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	a := &App{
		cfg:  cfg,
		st:   st,
		cli:  newClient(cfg),
		dict: keywords.Default(),
		log:  log,
	}
	for _, opt := range opts {
		opt(a)
	}
	a.off = official.NewResolver(prober{cli: a.cli}, sources.HTTPSearcher{
		HTTP:     a.cli,
		Endpoint: "https://lite.duckduckgo.com/lite/",
		Headers:  map[string]string{"User-Agent": cfg.HTTP.UserAgent},
		Timeout:  25 * time.Second,
	}, st, log)
	a.off.SetBudget(cfg.Filter.MaxOfficialLookups)

	var backends []notify.Notifier
	if cfg.Notify.Feishu.Enabled {
		fs, err := notify.NewFeishu(cfg.Notify.Feishu)
		if err != nil {
			st.Close()
			return nil, err
		}
		a.fs = fs
		backends = append(backends, fs)
	}
	if cfg.Notify.OpenClaw.Enabled {
		dir := cfg.Notify.OpenClaw.SkillDir
		if dir == "" {
			dir = filepath.Join(cfg.DataDir, "outreach")
		}
		a.drop = dir
		fd, err := notify.NewFileDrop(dir, "deal-hunter")
		if err != nil {
			st.Close()
			return nil, err
		}
		backends = append(backends, fd)
		if cfg.Notify.OpenClaw.Relay {
			relay, err := notify.NewOpenClawRelay(cfg.Notify.OpenClaw)
			if err != nil {
				st.Close()
				return nil, err
			}
			a.relay = relay
			backends = append(backends, relay)
		}
	}
	if cfg.Notify.Console {
		backends = append(backends, notify.NewConsole())
	}
	if a.nf != nil {
		// Backends were injected with WithNotifiers; config-derived ones are skipped.
		return a, nil
	}
	if len(backends) == 0 {
		st.Close()
		return nil, errors.New("pipeline: no notification backend enabled")
	}
	a.nf = notify.NewFanOut(log, backends...)
	return a, nil
}

func newClient(cfg *config.Config) *httpx.Client {
	return httpx.New(httpx.Options{UserAgent: cfg.HTTP.UserAgent,
		Timeout:      cfg.HTTP.Timeout.D(),
		MaxBody:      int64(cfg.HTTP.MaxBodyKB) * 1024,
		PerHostMin:   time.Duration(cfg.HTTP.PerHostMili) * time.Millisecond,
		Retries:      cfg.HTTP.Retries,
		AllowPrivate: cfg.HTTP.AllowPrivateHosts,
	})
}

// Store exposes the persistence layer to the read-only API.
func (a *App) Store() *store.Store { return a.st }

// Config exposes the loaded configuration.
func (a *App) Config() *config.Config { return a.cfg }

// Backends lists delivery names.
func (a *App) Backends() []string { return a.nf.Names() }

// FeishuReady reports whether direct push is usable.
func (a *App) FeishuReady() bool { return a.fs != nil && a.fs.Ready() }

// OutreachDir is where the OpenClaw drop files land (empty when disabled).
func (a *App) OutreachDir() string { return a.drop }

// FeishuMode reports the active direct-delivery route: webhook, app or "".
func (a *App) FeishuMode() string {
	if a.fs == nil {
		return ""
	}
	return a.fs.Mode()
}

// RelayReady reports whether the OpenClaw message relay can deliver.
func (a *App) RelayReady() bool { return a.relay != nil && a.relay.Ready() }

// Close releases the store.
func (a *App) Close() error { return a.st.Close() }

type fetchOutcome struct {
	cfg   config.Source
	deals []*model.Deal
	err   error
	ms    int64
}

// RunOnce performs one full round and returns its report.
func (a *App) RunOnce(ctx context.Context, trigger string) (*Run, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if trigger == "" {
		trigger = "manual"
	}
	run := &Run{StartedAt: time.Now().UTC(), Trigger: trigger}
	srcCfgs := a.cfg.EnabledSources()
	outcomes := a.fetchAll(ctx, srcCfgs)

	byName := map[string]config.Source{}
	for _, s := range srcCfgs {
		byName[s.Name] = s
	}

	var candidates []*model.Deal
	for _, oc := range outcomes {
		rep := SourceReport{Name: oc.cfg.Name, Kind: oc.cfg.Kind, Found: len(oc.deals), Ms: oc.ms}
		if oc.err != nil {
			// Upstream errors can echo a URL with credentials in the query.
			msg := redact.Text(oc.err.Error())
			rep.Err = msg
			run.Errors = append(run.Errors, msg)
			a.log.Warn("source failed", "source", oc.cfg.Name, "err", msg, "ms", oc.ms)
		}
		for _, d := range oc.deals {
			kept, push := a.judge(d, byName[oc.cfg.Name])
			if !kept {
				continue
			}
			rep.Stored++
			run.NewDeals++
			if d.Meta["dup_of"] != "" {
				run.Dupes++
			}
			if err := a.st.Save(d); err != nil {
				run.Errors = append(run.Errors, err.Error())
				continue
			}
			run.Stored++
			if push {
				candidates = append(candidates, d)
			}
		}
		run.Sources = append(run.Sources, rep)
	}

	// Resolve links to official, verified vendor pages first, then rank: a
	// community post that we could not tie back to the vendor is worth less than
	// a confirmed official page, and that must affect which few alerts fire.
	run.Verified = a.off.Apply(ctx, candidates)
	for _, d := range candidates {
		// Persist the resolved link, else the dashboard, the digest and the
		// OpenClaw pull would keep showing the community post the alert replaced.
		if err := a.st.Save(d); err != nil {
			a.log.Warn("persist resolved link", "err", err)
		}
		if official.IsOfficialKind(d.Meta[official.MetaLinkKind]) {
			continue
		}
		run.ThirdParty++
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].Title < candidates[j].Title
	})
	if max := a.cfg.Notify.Feishu.MaxPerRun; max > 0 && len(candidates) > max {
		a.log.Info("alert candidates capped", "have", len(candidates), "max_per_run", max)
		candidates = candidates[:max]
	}

	var deliverErr error
	if len(candidates) > 0 {
		if a.fs != nil && a.fs.QuietNow(time.Now()) {
			run.HeldQuiet = len(candidates)
			a.log.Info("quiet hours: holding alerts for the digest", "count", len(candidates))
		} else {
			vals := make([]model.Deal, 0, len(candidates))
			for _, d := range candidates {
				vals = append(vals, *d)
			}
			msg := notify.NewMessage(notify.KindAlert, alertTitle(vals), vals...)
			if err := a.nf.Send(ctx, msg); err != nil {
				deliverErr = err
				run.Errors = append(run.Errors, err.Error())
				a.log.Error("delivery incomplete; findings stay pending for the digest", "err", err)
			} else {
				for _, d := range candidates {
					run.Pushed++
					if err := a.st.MarkPushed(d.Fingerprint); err != nil {
						a.log.Warn("mark pushed", "err", err)
					}
				}
			}
		}
	}

	run.FinishedAt = time.Now().UTC()
	a.lastRun = run
	a.runs = append(a.runs, run)
	if n := 24; len(a.runs) > n {
		a.runs = a.runs[len(a.runs)-n:]
	}
	a.log.Info("round complete", "trigger", trigger, "sources", len(run.Sources),
		"new", run.NewDeals, "stored", run.Stored, "pushed", run.Pushed,
		"held_quiet", run.HeldQuiet, "dupes", run.Dupes, "verified_official", run.Verified,
		"third_party", run.ThirdParty, "errors", len(run.Errors), "took", run.duration().Round(time.Millisecond))
	return run, errors.Join(append(a.collectSourceErrors(outcomes), deliverErr)...)
}

func (a *App) collectSourceErrors(ocs []fetchOutcome) []error {
	var errs []error
	for _, oc := range ocs {
		if oc.err != nil {
			errs = append(errs, oc.err)
		}
	}
	return errs
}

func alertTitle(deals []model.Deal) string {
	if len(deals) == 1 {
		return notify.Headline(&deals[0])
	}
	return fmt.Sprintf("🧾 新羊毛 %d 条", len(deals))
}

// fetchAll runs every collector concurrently; each has its own timeout and
// panic guard so one bad source cannot stall or crash the round.
func (a *App) fetchAll(ctx context.Context, srcCfgs []config.Source) []fetchOutcome {
	out := make([]fetchOutcome, len(srcCfgs))
	var wg sync.WaitGroup
	for i, sc := range srcCfgs {
		wg.Add(1)
		go func(i int, sc config.Source) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					out[i] = fetchOutcome{cfg: sc, err: fmt.Errorf("source %s panicked: %v", sc.Name, r)}
				}
			}()
			src, err := sources.New(sources.Deps{
				Cfg:      sc,
				HTTP:     a.cli,
				Defaults: a.cfg.HTTP,
				State:    a.st,
				Log:      a.log,
				Now:      time.Now,
			})
			if err != nil {
				out[i] = fetchOutcome{cfg: sc, err: err}
				return
			}
			perCall, cancel := context.WithTimeout(ctx, a.sourceBudget())
			defer cancel()
			start := time.Now()
			deals, err := src.Fetch(perCall)
			out[i] = fetchOutcome{cfg: sc, deals: deals, err: err, ms: time.Since(start).Milliseconds()}
		}(i, sc)
	}
	wg.Wait()
	return out
}

// sourceBudget is the round interval minus a margin, never below 30s.
func (a *App) sourceBudget() time.Duration {
	d := a.cfg.Interval.D() - 30*time.Second
	if d < 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// judge scores one candidate. kept=false means it never enters history;
// push=true marks it as alert-worthy.
func (a *App) judge(d *model.Deal, sc config.Source) (kept, push bool) {
	f := a.cfg.Filter
	d.EnsureFingerprint()
	if a.st.Seen(d.Fingerprint) {
		return false, false
	}
	text := d.TextBlob()
	lower := strings.ToLower(text)
	for _, k := range f.DenyKeywords {
		if k != "" && strings.Contains(lower, strings.ToLower(k)) {
			return false, false
		}
	}
	if len(f.RequireKeywords) > 0 && !anyContains(lower, f.RequireKeywords) {
		return false, false
	}
	if f.MaxAgeHours > 0 && !d.PublishedAt.IsZero() {
		if time.Since(d.PublishedAt) > time.Duration(f.MaxAgeHours)*time.Hour {
			return false, false
		}
	}
	offers := a.dict.Scan(text)
	if len(offers) == 0 {
		offers = d.Offers
	}
	if f.RequireOffer && len(offers) == 0 {
		return false, false
	}
	res := scoring.Evaluate(scoring.Input{
		Deal:           d,
		Offers:         offers,
		SourceTrust:    sc.Trust,
		OfficialDomain: sources.IsOfficialURL(d.URL, sc.Sites),
		Now:            time.Now(),
	}, a.dict)
	if res.Reject != "" {
		return false, false
	}
	d.Offers = offers
	d.Score = res.Score
	d.ScoreWhy = res.Why
	d.Category = res.Category
	d.IsFree = res.IsFree
	d.DiscountPct = res.DiscountPct
	d.Vendors = res.Vendors
	if len(d.Tags) > 0 && res.Product != "" {
		d.Tags = append(d.Tags, "model:"+res.Product)
	}
	if res.Expires != nil {
		d.Meta["expires_at"] = res.Expires.Format(time.RFC3339)
	}
	if len(offers) > 0 {
		var kinds []string
		for _, o := range offers {
			kinds = append(kinds, string(o.Kind))
		}
		d.Meta["offer_kinds"] = strings.Join(unique(kinds), ",")
	}
	d.SortOffersAndTags()
	d.EnsureFingerprint()
	// Mark the free verdicts now, while the record is still being written: an
	// unlabelled row later reads as "not checked", which is only true for deals
	// whose vendor we know but could not verify.
	official.LabelCheap(d)
	// The same announcement reposted by a second feed is recorded but never
	// pushed twice: the first finding seen owns the title.
	if fp, clash := a.st.TitleClash(d.Title, d.Fingerprint); clash {
		d.Meta["dup_of"] = fp
		return true, false
	}
	if len(f.AllowKeywords) > 0 && !anyContains(lower, f.AllowKeywords) {
		return false, false
	}
	return true, d.Score >= a.cfg.Notify.Feishu.MinScore
}

func anyContains(hay string, needles []string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(hay, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

func unique(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// LastRun returns the most recent round report, or nil.
func (a *App) LastRun() *Run {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastRun
}

// RecentRuns returns up to n stored round reports, oldest first.
func (a *App) RecentRuns(n int) []*Run {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Run, 0, len(a.runs))
	for _, r := range a.runs {
		cp := *r
		out = append(out, &cp)
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// RecentDeals returns the newest stored findings.
func (a *App) RecentDeals(n int) []model.Deal { return a.st.Recent(n) }

const digestCursor = "digest:last_sent"
const dailyCursor = "daily:last_sent"

// loc resolves the configured timezone. The service often runs in UTC, so a
// "09:00" briefing must be computed in the user's zone, not the host's.
func (a *App) loc() *time.Location {
	if a.cfg.Timezone != "" {
		if l, err := time.LoadLocation(a.cfg.Timezone); err == nil {
			return l
		}
	}
	return time.Local
}

// slotAt returns today's occurrence of "HH:MM" in the configured zone.
func (a *App) slotAt(now time.Time, at string) (time.Time, error) {
	hh, mm, err := parseHHMM(at)
	if err != nil {
		return time.Time{}, err
	}
	l := a.loc()
	n := now.In(l)
	return time.Date(n.Year(), n.Month(), n.Day(), hh, mm, 0, 0, l), nil
}

// DailyDue reports whether the morning briefing is due.
func (a *App) DailyDue(now time.Time) bool {
	d := a.cfg.Notify.Daily
	if !d.Enabled {
		return false
	}
	at := d.At
	if at == "" {
		at = "09:00"
	}
	sched, err := a.slotAt(now, at)
	if err != nil {
		a.log.Warn("daily.at is not HH:MM; the briefing is skipped", "value", at, "err", err)
		return false
	}
	last, ok := a.stateTime(dailyCursor)
	if !ok {
		// Never sent: wait for the next slot. Firing on first start would turn a
		// "09:00 briefing" into a random message at deploy time.
		return false
	}
	return !now.In(a.loc()).Before(sched) && last.Before(sched)
}

// DailyPreview applies the briefing's own limits and returns the live offers.
func (a *App) DailyPreview(now time.Time) []model.Deal {
	d := a.cfg.Notify.Daily
	minScore, maxItems := d.MinScore, d.MaxItems
	if minScore <= 0 {
		minScore = 45
	}
	if maxItems <= 0 {
		maxItems = 15
	}
	return a.st.Live(minScore, now.UTC(), maxItems)
}

// SendDaily reports every offer that is still live, each with how long we have
// known it and when it ends. It never marks anything as pushed: this is a
// snapshot, not a delivery queue.
func (a *App) SendDaily(ctx context.Context, now time.Time) error {
	live := a.DailyPreview(now)
	if len(live) == 0 {
		a.log.Info("daily: nothing live to report")
		return nil
	}
	msg := notify.NewMessage(notify.KindDaily,
		fmt.Sprintf("🌅 羊毛日报 · %d 条仍在效", len(live)), live...)
	msg.Intro = now.In(a.loc()).Format("01月02日") + " · 未过期会再次出现"
	if err := a.nf.Send(ctx, msg); err != nil {
		return err
	}
	return a.setStateTime(dailyCursor, now.UTC())
}

// DailyInfo describes the morning briefing for the status endpoint.
type DailyInfo struct {
	Enabled bool      `json:"enabled"`
	At      string    `json:"at"`
	Last    time.Time `json:"last_sent,omitempty"`
	Next    time.Time `json:"next_due,omitempty"`
}

// Daily reports the briefing schedule and where it stands.
func (a *App) Daily(now time.Time) DailyInfo {
	d := a.cfg.Notify.Daily
	at := d.At
	if at == "" {
		at = "09:00"
	}
	info := DailyInfo{Enabled: d.Enabled, At: at}
	if t, ok := a.stateTime(dailyCursor); ok {
		info.Last = t
	}
	if sched, err := a.slotAt(now, at); err == nil {
		next := sched
		if !now.In(a.loc()).Before(sched) {
			next = sched.AddDate(0, 0, 1)
		}
		info.Next = next
	}
	return info
}

// LiveDeals returns the offers still worth claiming, for the panel and CLI.
func (a *App) LiveDeals(minScore int, now time.Time, limit int) []model.Deal {
	return a.st.Live(minScore, now, limit)
}

// DigestDue reports whether a batched summary is due now.
func (a *App) DigestDue(now time.Time) bool {
	d := a.cfg.Notify.Digest
	if !d.Enabled {
		return false
	}
	last, ok := a.digestCursor()
	if !ok {
		return true
	}
	if at := d.At; at != "" {
		// A fixed daily slot is due once "now" has passed it and the cursor is
		// still pointing at an earlier time.
		if sched, err := a.slotAt(now, at); err == nil {
			return !now.In(a.loc()).Before(sched) && last.Before(sched)
		} else {
			a.log.Warn("digest.at is not HH:MM, falling back to the interval", "value", at, "err", err)
		}
	}
	every := d.Every.D()
	if every <= 0 {
		every = 6 * time.Hour
	}
	return now.Sub(last) >= every
}

// SendDigest pushes the pending sub-threshold findings as one batched message.
func (a *App) SendDigest(ctx context.Context) error {
	d := a.cfg.Notify.Digest
	minScore := d.MinScore
	if minScore <= 0 {
		minScore = 45
	}
	maxItems := d.MaxItems
	if maxItems <= 0 {
		maxItems = 12
	}
	last, ok := a.digestCursor()
	if !ok {
		last = time.Now().AddDate(0, 0, -3)
	}
	pending := a.st.Pending(minScore, last)
	if len(pending) == 0 {
		a.log.Debug("digest: nothing pending")
		return nil
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].Score > pending[j].Score })
	if len(pending) > maxItems {
		pending = pending[:maxItems]
	}
	msg := notify.NewMessage(notify.KindDigest,
		fmt.Sprintf("🧺 羊毛盘点 · %d 条", len(pending)), pending...)
	if err := a.nf.Send(ctx, msg); err != nil {
		return err
	}
	for _, p := range pending {
		if err := a.st.MarkPushed(p.Fingerprint); err != nil {
			a.log.Warn("digest mark pushed", "err", err)
		}
	}
	return a.setDigestCursor(time.Now().UTC())
}

// stateTime reads an RFC3339 timestamp stored under key.
func (a *App) stateTime(key string) (time.Time, bool) {
	b, ok := a.st.GetState(key)
	if !ok {
		return time.Time{}, false
	}
	var s string
	if json.Unmarshal(b, &s) != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func (a *App) setStateTime(key string, t time.Time) error {
	return a.st.PutState(key, t.Format(time.RFC3339))
}

func (a *App) digestCursor() (time.Time, bool) {
	b, ok := a.st.GetState(digestCursor)
	if !ok {
		return time.Time{}, false
	}
	var s string
	if json.Unmarshal(b, &s) != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func (a *App) setDigestCursor(t time.Time) error {
	return a.st.PutState(digestCursor, t.Format(time.RFC3339))
}

func parseHHMM(s string) (int, int, error) {
	var hh, mm int
	if _, err := fmt.Sscanf(s, "%d:%d", &hh, &mm); err != nil {
		return 0, 0, fmt.Errorf("parse %q as HH:MM: %w", s, err)
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf("time %q out of range", s)
	}
	return hh, mm, nil
}

// NotifyTest sends a self-check message through every configured backend.
func (a *App) NotifyTest(ctx context.Context) error {
	now := time.Now()
	probe := model.Deal{
		Title:        "链路自检：Deal-Hunter 已就绪",
		Summary:      "这是一条测试卡片，用来确认飞书推送与 OpenClaw 落地文件都正常。",
		Source:       "self-test",
		Category:     model.CatAIFree,
		IsFree:       true,
		Score:        99,
		ScoreWhy:     []string{"+45 免费类 offer", "+10 信息源可信度"},
		Vendors:      []string{"Deal-Hunter"},
		URL:          "https://github.com/xiabee/Deal-Hunter",
		DiscoveredAt: now,
	}
	probe.EnsureFingerprint()
	msg := notify.NewMessage(notify.KindTest, "✅ Deal-Hunter 自检", probe)
	msg.Intro = "推送链路连通性测试"
	return a.nf.Send(ctx, msg)
}
