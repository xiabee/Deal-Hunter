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
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Found counts rows that survived the source's own keyword gate; Parsed counts
	// everything the page yielded before that gate. Only Parsed distinguishes a
	// redesigned page from a day with nothing in scope.
	Found  int    `json:"found"`
	Parsed int    `json:"parsed"`
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
	Pushed     int            `json:"urgent_sent"`
	EventSent  int            `json:"event_sent"`
	EventHeld  int            `json:"event_held"`
	UrgentHeld int            `json:"urgent_held"`
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

	// roundMu serialises collection rounds, which can take minutes; mu guards
	// the run history only, so the read-only panel never waits on a round.
	roundMu sync.Mutex
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
	// Nothing here may write state: read-only commands (events, doctor, probe) build
	// an App too, and running one as root used to hand state.json to root and put the
	// service into a crash loop. The briefing schedule is armed by the first round.

	var backends []notify.Notifier
	if cfg.Notify.Feishu.Enabled {
		fs, err := notify.NewFeishu(cfg.Notify.Feishu, a.loc())
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
	cfg    config.Source
	deals  []*model.Deal
	err    error
	ms     int64
	parsed int
}

// RunOnce performs one full round and returns its report.
func (a *App) RunOnce(ctx context.Context, trigger string) (*Run, error) {
	a.roundMu.Lock()
	defer a.roundMu.Unlock()

	if trigger == "" {
		trigger = "manual"
	}
	run := &Run{StartedAt: time.Now().UTC(), Trigger: trigger}
	// A round is the earliest point where writing state is legitimate: see dailyWatched.
	if a.cfg.Notify.Daily.Enabled {
		if _, ok := a.stateTime(dailyWatched); !ok {
			if err := a.setStateTime(dailyWatched, run.StartedAt); err != nil {
				return run, err
			}
		}
	}
	srcCfgs := a.cfg.EnabledSources()
	outcomes := a.fetchAll(ctx, srcCfgs)

	byName := map[string]config.Source{}
	for _, s := range srcCfgs {
		byName[s.Name] = s
	}

	var interrupts []*model.Deal
	for _, oc := range outcomes {
		rep := SourceReport{Name: oc.cfg.Name, Kind: oc.cfg.Kind, Found: len(oc.deals),
			Parsed: oc.parsed, Ms: oc.ms}
		if oc.err != nil {
			// Upstream errors can echo a URL with credentials in the query.
			msg := redact.Text(oc.err.Error())
			rep.Err = msg
			run.Errors = append(run.Errors, msg)
			a.log.Warn("source failed", "source", oc.cfg.Name, "err", msg, "ms", oc.ms)
		}
		for _, d := range oc.deals {
			kept, urgent := a.judge(d, byName[oc.cfg.Name])
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
			if urgent {
				interrupts = append(interrupts, d)
			}
		}
		run.Sources = append(run.Sources, rep)
	}
	a.recordSourceHits(run.Sources, time.Now())

	// Resolve links to official, verified vendor pages first, then rank: a
	// community post that we could not tie back to the vendor is worth less than
	// a confirmed official page, and that must affect which few alerts fire.
	run.Verified = a.off.Apply(ctx, interrupts)
	for _, d := range interrupts {
		// Persist the resolved link, else the dashboard and the OpenClaw pull
		// would keep showing the community post the alert replaced.
		if err := a.st.Save(d); err != nil {
			a.log.Warn("persist resolved link", "err", err)
		}
		if official.IsOfficialKind(d.Meta[official.MetaLinkKind]) {
			continue
		}
		run.ThirdParty++
	}
	sort.SliceStable(interrupts, func(i, j int) bool {
		if interrupts[i].Score != interrupts[j].Score {
			return interrupts[i].Score > interrupts[j].Score
		}
		return interrupts[i].Title < interrupts[j].Title
	})

	var deliverErr error
	// Everything stored but not urgent enough to interrupt simply waits: the
	// briefing re-reads the whole live store, so waiting costs nothing but delay.
	if n := a.urgentMaxItems(); len(interrupts) > n {
		a.log.Info("interrupting findings capped", "have", len(interrupts), "max_items", n)
		run.UrgentHeld += len(interrupts) - n
		interrupts = interrupts[:n]
	}
	if len(interrupts) > 0 {
		// The budget counts messages, not rows: today's one breakthrough card
		// carries everything urgent found so far, and once it is out the day is
		// done interrupting.
		if a.urgentLeft(time.Now()) <= 0 {
			run.UrgentHeld = len(interrupts)
			interrupts = nil
			a.log.Info("urgent budget spent: waiting for the next briefing",
				"count", run.UrgentHeld, "max_per_day", a.urgentMax())
		}
	}
	if len(interrupts) > 0 {
		vals := make([]model.Deal, 0, len(interrupts))
		for _, d := range interrupts {
			vals = append(vals, *d)
		}
		now := time.Now()
		msg := notify.NewMessage(notify.KindUrgent, urgentTitle(vals), vals...)
		if err := a.nf.Send(ctx, msg); err != nil {
			// Nothing is charged to the budget and nothing is marked pushed, so
			// the next round retries the same findings.
			deliverErr = err
			run.Errors = append(run.Errors, err.Error())
			a.log.Error("breakthrough delivery failed; findings wait for the briefing", "err", err)
		} else {
			for _, d := range interrupts {
				run.Pushed++
				if err := a.st.MarkPushed(d.Fingerprint); err != nil {
					a.log.Warn("mark pushed", "err", err)
				}
			}
			if err := a.spendBudget(urgentCursor, now); err != nil {
				a.log.Warn("charge urgent budget", "err", err)
			}
		}
	}

	// Reminders scan the store rather than this round's findings, because an
	// announcement is usually published days before the thing it announces opens.
	sent, held, evErr := a.sendDueEvents(ctx, time.Now())
	run.EventSent, run.EventHeld = sent, held
	if evErr != nil {
		deliverErr = errors.Join(deliverErr, evErr)
	}

	run.FinishedAt = time.Now().UTC()
	a.publish(run)
	a.log.Info("round complete", "trigger", trigger, "sources", len(run.Sources),
		"new", run.NewDeals, "stored", run.Stored, "urgent_sent", run.Pushed,
		"urgent_held", run.UrgentHeld, "event_sent", run.EventSent, "event_held", run.EventHeld, "dupes", run.Dupes, "verified_official", run.Verified,
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

func urgentTitle(deals []model.Deal) string {
	if len(deals) == 1 {
		return notify.Headline(&deals[0])
	}
	return fmt.Sprintf("⚡ 值得立刻看 %d 条", len(deals))
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
			out[i] = fetchOutcome{cfg: sc, deals: deals, err: err,
				ms: time.Since(start).Milliseconds(), parsed: src.RawSeen()}
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
// urgent=true means it is strong enough to interrupt the day's briefing.
// Everything that does not need the state log lives in Screen, because probe
// has to answer the same question without a store to consult.
func (a *App) judge(d *model.Deal, sc config.Source) (kept, urgent bool) {
	d.EnsureFingerprint()
	if a.st.Seen(d.Fingerprint) {
		return false, false
	}
	now := time.Now()
	if Screen(a.cfg, a.dict, d, sc, now) != "" {
		return false, false
	}
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
	return true, a.cfg.Notify.Urgent.Enabled && d.Score >= a.cfg.Notify.Urgent.MinScore
}

// Screen is the store-free half of the decision: the reader's filters, the
// keyword evidence, the score, and whatever clocks the parser read out of the
// text. It fills the deal in exactly the way a round does, then returns the
// reason this row will not reach the reader - or "" when it will.
//
// probe reports this verdict because there is no second opinion to have: the
// question a source has to answer before it is admitted is "what would the
// reader see", and a formula copied next to the CLI starts disagreeing with the
// real one the day either of them changes. Two facts stay invisible here on
// purpose, since both need the state log: a row already seen in an earlier
// round, and a title already owned by an earlier row.
//
// The allow-list used to be checked after the title clash, so a row failing it
// was still recorded if its title happened to clash. It is a filter on what we
// care about, so it now applies to every row; no shipped config sets it.
func Screen(cfg *config.Config, dict *keywords.Dict, d *model.Deal, sc config.Source, now time.Time) string {
	f := cfg.Filter
	text := d.TextBlob()
	lower := strings.ToLower(text)
	for _, k := range f.DenyKeywords {
		if k != "" && strings.Contains(lower, strings.ToLower(k)) {
			return "命中拒绝词 " + k
		}
	}
	if len(f.RequireKeywords) > 0 && !anyContains(lower, f.RequireKeywords) {
		return "缺少要求词"
	}
	if f.MaxAgeHours > 0 && !d.PublishedAt.IsZero() {
		if age := now.Sub(d.PublishedAt); age > time.Duration(f.MaxAgeHours)*time.Hour {
			return fmt.Sprintf("发布已 %d 小时", int(age.Hours()))
		}
	}
	offers := dict.Scan(text)
	if len(offers) == 0 {
		offers = d.Offers
	}
	if f.RequireOffer && len(offers) == 0 {
		return "无优惠命中词"
	}
	res := scoring.Evaluate(scoring.Input{
		Deal:           d,
		Offers:         offers,
		SourceTrust:    sc.Trust,
		OfficialDomain: sources.IsOfficialURL(d.URL, sc.Sites),
		Now:            now,
		ReaderZone:     zoneOf(cfg),
	}, dict)
	// The clocks are recorded even when the row is dropped: "read a deadline that
	// has already passed" and "read nothing at all" are different answers about a
	// source, and only one of them is a reason not to admit it.
	if res.Expires != nil {
		d.Meta["expires_at"] = res.Expires.Format(time.RFC3339)
	}
	if res.Starts != nil {
		d.Meta["starts_at"] = res.Starts.Format(time.RFC3339)
	}
	if res.Reject != "" {
		return rejected(res.Reject)
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
	if len(offers) > 0 {
		var kinds []string
		for _, o := range offers {
			kinds = append(kinds, string(o.Kind))
		}
		d.Meta["offer_kinds"] = strings.Join(unique(kinds), ",")
	}
	d.SortOffersAndTags()
	d.EnsureFingerprint()
	if len(f.AllowKeywords) > 0 && !anyContains(lower, f.AllowKeywords) {
		return "不在允许词内"
	}
	return ""
}

// rejected names a scorer's refusal the way the operator reads it.
func rejected(reason string) string {
	switch reason {
	case "noise":
		return "噪音词"
	case "expired":
		return "截止已过"
	case "low_signal":
		return "信号太弱"
	}
	return reason
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

const dailyCursor = "daily:last_sent"

// dailyWatched marks when this process began honouring the briefing schedule.
// Without it an install that has never sent a briefing has nothing to compare
// the slot against, so it can never become due — only SendDaily writes
// dailyCursor, and only DailyDue lets us reach SendDaily.
const dailyWatched = "daily:watched_since"

// Per-day message budgets, one key each. Storing {local day, count} rather than
// "the last send" is what makes a ceiling of N a day mean N and not "one every
// N/2 hours" once sends accumulate, and the day rolls over with no cleanup.
const (
	urgentCursor = "urgent:sent"
	eventCursor  = "event:sent"
)

type dayCount struct {
	Date string `json:"date"` // local calendar day, as seen in the configured zone
	N    int    `json:"n"`    // messages already sent that day
}

// zoneOf resolves the configured timezone. The service often runs in UTC, so a
// "09:00" briefing must be computed in the user's zone, not the host's.
func zoneOf(cfg *config.Config) *time.Location {
	if cfg.Timezone != "" {
		if l, err := time.LoadLocation(cfg.Timezone); err == nil {
			return l
		}
	}
	return time.Local
}

func (a *App) loc() *time.Location { return zoneOf(a.cfg) }

// slotOf returns today's occurrence of "HH:MM" in loc.
func slotOf(loc *time.Location, now time.Time, at string) (time.Time, error) {
	hh, mm, err := config.ParseHHMM(at)
	if err != nil {
		return time.Time{}, err
	}
	n := now.In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), hh, mm, 0, 0, loc), nil
}

// slotAt returns today's occurrence of "HH:MM" in the configured zone.
func (a *App) slotAt(now time.Time, at string) (time.Time, error) {
	return slotOf(a.loc(), now, at)
}

// DailyDue reports whether the morning briefing is due.
func (a *App) DailyDue(now time.Time) bool {
	d := a.cfg.Notify.Daily
	if !d.Enabled {
		return false
	}
	sched, err := a.slotAt(now, d.At)
	if err != nil {
		a.log.Warn("daily.at is not HH:MM; the briefing is skipped", "value", d.At, "err", err)
		return false
	}
	gate, ok := a.stateTime(dailyCursor)
	if !ok {
		if gate, ok = a.stateTime(dailyWatched); !ok {
			return false
		}
	}
	// The briefing is due once its slot has arrived and the gate is behind it.
	// For an install that has never sent one the gate is when this process
	// started watching, so a slot that had already passed at startup is not
	// backfilled while the next one still fires.
	return !now.In(a.loc()).Before(sched) && gate.Before(sched)
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

// BriefingQuality reports how many rows the next briefing would carry, and of
// those how many have a link we tied back to the vendor. The dashboard gets this
// instead of reading deal metadata itself, because under the daily model the
// briefing's rows are the only links the user is actually shown.
func (a *App) BriefingQuality(now time.Time) (total, verified int) {
	live := a.DailyPreview(now)
	for i := range live {
		if official.IsOfficialKind(live[i].Meta[official.MetaLinkKind]) {
			verified++
		}
	}
	return len(live), verified
}

// SendDaily reports every offer that is still live, each with how long we have
// known it and when it ends. It never marks anything as pushed: this is a
// snapshot, not a delivery queue. A day with nothing live still sends one
// message — "nothing today" is the answer to "is the radar still running?", and
// consuming the slot is what keeps the schedule from re-firing all day.
func (a *App) SendDaily(ctx context.Context, now time.Time) error {
	live := a.DailyPreview(now)
	a.verifyForBriefing(ctx, live)
	msg := notify.NewMessage(notify.KindDaily, dailyTitle(len(live)), live...)
	// The intro answers the question the reader actually has at 09:00: is there
	// anything new, or is yesterday still on the table?
	daily := notify.SplitByAge(live, now)
	msg.Intro = fmt.Sprintf("%s · 新增 %d · 持续 %d",
		now.In(a.loc()).Format("01月02日"), len(daily.Fresh), len(daily.Ongoing))
	if err := a.nf.Send(ctx, msg); err != nil {
		return err
	}
	return a.setStateTime(dailyCursor, now.UTC())
}

func dailyTitle(n int) string {
	if n == 0 {
		return "🌅 羊毛日报 · 今天没有在效的"
	}
	return fmt.Sprintf("🌅 羊毛日报 · %d 条仍在效", n)
}

// verifyForBriefing resolves the rows about to be reported. The briefing is what
// the user acts on, so a link checked here is worth more than one checked for a
// random round: verdicts cache for a week and the pass is budget-capped, which
// keeps this a handful of requests once a day.
func (a *App) verifyForBriefing(ctx context.Context, live []model.Deal) {
	ptrs := make([]*model.Deal, 0, len(live))
	for i := range live {
		ptrs = append(ptrs, &live[i])
	}
	if len(ptrs) == 0 {
		return
	}
	before := make(map[string]int, len(ptrs))
	for _, d := range ptrs {
		before[d.Fingerprint] = d.Score
	}
	a.off.Apply(ctx, ptrs)
	for _, d := range ptrs {
		if d.Score == before[d.Fingerprint] {
			continue
		}
		if err := a.st.Save(d); err != nil {
			a.log.Warn("persist briefing link", "err", err)
		}
	}
	// Resolving a link can move the score, and the report reads best-first.
	sort.SliceStable(live, func(i, j int) bool {
		if live[i].Score != live[j].Score {
			return live[i].Score > live[j].Score
		}
		return live[i].Title < live[j].Title
	})
}

// DailyInfo describes the morning briefing for the status endpoint.
type DailyInfo struct {
	Enabled bool      `json:"enabled"`
	At      string    `json:"at"`
	Last    time.Time `json:"last_sent,omitempty"`
	Next    time.Time `json:"next_due,omitempty"`
	// SlotPassed is today's slot already behind us. With SentToday false it means
	// the day's briefing was missed and will not be backfilled - DailyOnly fires
	// once the gate (the last send, or when this process started watching) falls
	// behind the slot, so a service that came up at noon does not send 09:00's.
	SlotPassed bool `json:"slot_passed"`
	SentToday  bool `json:"sent_today"`
}

// BriefingState is App.Daily without an App. doctor runs on a broken host and must not
// construct notifiers to answer a question, so the schedule math lives here and both
// surfaces read the same one - two implementations of "sent today" is how a health
// check starts lying.
func BriefingState(cfg *config.Config, st *store.Store, now time.Time) DailyInfo {
	d := cfg.Notify.Daily
	loc := zoneOf(cfg)
	info := DailyInfo{Enabled: d.Enabled, At: d.At}
	if t, ok := stateTimeOf(st, dailyCursor); ok {
		info.Last = t
		info.SentToday = dayKey(loc, t) == dayKey(loc, now)
	}
	slot, err := slotOf(loc, now, d.At)
	if err != nil {
		return info
	}
	info.SlotPassed = now.In(loc).After(slot)
	info.Next = slot
	if info.SlotPassed {
		info.Next = slot.AddDate(0, 0, 1)
	}
	return info
}

// Daily reports the briefing schedule and where it stands.
func (a *App) Daily(now time.Time) DailyInfo { return BriefingState(a.cfg, a.st, now) }

// UrgentInfo describes today's breakthrough budget for the status endpoint.
type UrgentInfo struct {
	Enabled   bool `json:"enabled"`
	MinScore  int  `json:"min_score"`
	MaxPerDay int  `json:"max_per_day"`
	SentToday int  `json:"sent_today"`
}

// Urgent reports how much of today's breakthrough budget is already spent, so
// the panel can answer "will I hear from this thing again today?".
func (a *App) Urgent(now time.Time) UrgentInfo {
	u := a.cfg.Notify.Urgent
	info := UrgentInfo{Enabled: u.Enabled, MinScore: u.MinScore, MaxPerDay: a.urgentMax()}
	if day, n := a.budgetSpent(urgentCursor); day == a.localDay(now) {
		info.SentToday = n
	}
	return info
}

// urgentMax is the configured per-day breakthrough ceiling, at least one.
// urgentMax returns the configured daily interrupt budget as written, including 0.
// The default of one comes from config.Default(), so an absent key is already
// covered and an explicit zero must not be rounded up.
func (a *App) urgentMax() int { return a.cfg.Notify.Urgent.MaxPerDay }

// urgentMaxItems caps how many rows one breakthrough card carries.
func (a *App) urgentMaxItems() int {
	if n := a.cfg.Notify.Urgent.MaxItems; n > 0 {
		return n
	}
	return 5
}

// urgentLeft reports how many breakthrough messages may still go out today.
func (a *App) urgentLeft(now time.Time) int {
	if !a.cfg.Notify.Urgent.Enabled {
		return 0
	}
	return a.budgetLeft(urgentCursor, a.urgentMax(), now)

}

// budgetSpent reads the {local day, messages sent} pair recorded under key.
func (a *App) budgetSpent(key string) (string, int) {
	b, ok := a.st.GetState(key)
	if !ok {
		return "", 0
	}
	var u dayCount
	if json.Unmarshal(b, &u) != nil {
		return "", 0
	}
	return u.Date, u.N
}

// budgetLeft reports how many messages under key may still go out today.
// budgetLeft reports how many messages are left for the reader's local day.
// Zero means zero: a reader who writes max_per_day: 0 is asking not to be
// interrupted, and silently rounding that up to one message is the difference
// between "off" and "one surprise a day".
func (a *App) budgetLeft(key string, max int, now time.Time) int {
	if max < 0 {
		max = 0
	}
	if day, n := a.budgetSpent(key); day == a.localDay(now) {
		if left := max - n; left > 0 {
			return left
		}
		return 0
	}
	return max
}

// spendBudget charges one message under key to the local day it went out.
func (a *App) spendBudget(key string, now time.Time) error {
	day := a.localDay(now)
	_, spent := a.budgetSpent(key)
	return a.st.PutState(key, dayCount{Date: day, N: spent + 1})
}

// dayKey is the calendar day in the configured zone, which is what "once a day"
// has to mean for a service that usually runs in UTC.
func dayKey(loc *time.Location, now time.Time) string {
	return now.In(loc).Format("2006-01-02")
}

func (a *App) localDay(now time.Time) string {
	return dayKey(a.loc(), now)
}

// stateTime reads an RFC3339 timestamp stored under key.
// srcLastHit records, per source, the last moment its page parsed into at least one
// row. The failure this exists for is the quiet one: a page gets redesigned, the
// collector returns nothing, nothing errors, and the panel - which only remembers the
// last round, lost on restart - keeps looking green.
const srcLastHit = "src:last_hit"

// srcLastHitSince dates the record itself. Without it "这个源从没解析出过内容" would
// be read as "this source has never worked", when the honest reading on a host that
// started collecting an hour ago is simply "no evidence yet".
const srcLastHitSince = "src:last_hit_since"

// SourceIdle says how long a source has gone without producing a row. Last is the
// zero time when the source has never done so - a different complaint from "quiet
// lately", and never encoded as a negative Idle: a reader whose clock falls a few
// seconds ahead of the writer's would otherwise read a healthy source as broken.
type SourceIdle struct {
	Name string    `json:"name"`
	Last time.Time `json:"last"`
	Idle time.Duration
}

func (a *App) recordSourceHits(reports []SourceReport, now time.Time) {
	// The epoch is written by the first round that runs, hit or not: it is when the
	// evidence starts, not when evidence was first seen.
	if _, ok := a.stateTime(srcLastHitSince); !ok {
		if err := a.setStateTime(srcLastHitSince, now.UTC()); err != nil {
			a.log.Warn("record source record epoch", "err", err)
		}
	}
	hits := map[string]string{}
	if b, ok := a.st.GetState(srcLastHit); ok {
		_ = json.Unmarshal(b, &hits)
	}
	stamp := now.UTC().Format(time.RFC3339)
	changed := false
	for _, r := range reports {
		// Parsed, not Found: a source whose page still yields rows but none in
		// scope today is healthy, and alarming on it would be a false alarm on
		// exactly the quiet days this product is least interesting.
		if r.Parsed == 0 || hits[r.Name] == stamp {
			continue
		}
		hits[r.Name] = stamp
		changed = true
	}
	if !changed {
		return
	}
	if err := a.st.PutState(srcLastHit, hits); err != nil {
		a.log.Warn("record source hits", "err", err)
	}
}

// SourceRecordStart reports when this host began keeping per-source hit evidence.
// Callers need it to tell "quiet for days" apart from "we have only watched for an hour".
func SourceRecordStart(st *store.Store) (time.Time, bool) {
	b, ok := st.GetState(srcLastHitSince)
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

// SourceIdleness lists every enabled source, most silence first, so the worst one is
// always the first line an operator sees.
func (a *App) SourceIdleness(now time.Time) []SourceIdle {
	return SourceIdleness(a.cfg, a.st, now)
}

// SourceIdleness is the read side of the same record, available without an App so
// read-only commands (doctor) report exactly what the panel does.
func SourceIdleness(cfg *config.Config, st *store.Store, now time.Time) []SourceIdle {
	hits := map[string]string{}
	if b, ok := st.GetState(srcLastHit); ok {
		_ = json.Unmarshal(b, &hits)
	}
	enabled := cfg.EnabledSources()
	out := make([]SourceIdle, 0, len(enabled))
	for _, s := range enabled {
		si := SourceIdle{Name: s.Name}
		if t, err := time.Parse(time.RFC3339, hits[s.Name]); err == nil && !t.IsZero() {
			si.Last = t
			si.Idle = now.Sub(t)
			if si.Idle < 0 {
				si.Idle = 0 // a reader that started before the last round finished
			}
		} else {
			si.Idle = -1 // never: signalled by the zero Last, kept for sort ordering
		}
		out = append(out, si)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Last.IsZero() != out[j].Last.IsZero() {
			return out[i].Last.IsZero()
		}
		return out[i].Idle > out[j].Idle
	})
	return out
}

// SourceDelivery counts the rows currently in the log that came from each enabled
// source. This is the number the silence row cannot see: a list page can keep
// parsing 143 rows a round - which is what "出声" records - while every one of
// them is refused downstream (no offer keywords, a detail body that is only a
// WeChat placeholder), and nothing is ever delivered. Zero is not automatically
// broken: a city's voucher calendar really can be empty for weeks. It is a fact
// worth one line, because finding out by hand takes four commands.
func SourceDelivery(cfg *config.Config, st *store.Store) map[string]int {
	// n=0 means uncapped here, and it matters: a capped read would undercount the
	// quiet sources, which is the only thing this function can get wrong.
	counts := map[string]int{}
	for _, d := range st.Recent(0) {
		counts[d.Source]++
	}
	out := map[string]int{}
	for _, s := range cfg.EnabledSources() {
		out[s.Name] = counts[s.Name]
	}
	return out
}

// stateTimeOf reads an RFC3339 timestamp stored under key, without needing an App -
// see BriefingState for why doctor wants that.
func stateTimeOf(st *store.Store, key string) (time.Time, bool) {
	b, ok := st.GetState(key)
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

func (a *App) stateTime(key string) (time.Time, bool) { return stateTimeOf(a.st, key) }

func (a *App) setStateTime(key string, t time.Time) error {
	return a.st.PutState(key, t.Format(time.RFC3339))
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

// eventRemindedPrefix keys the one-reminder-per-event mark; it holds the moment
// the reminder went out, which is also what makes the mark survive a restart.
const eventRemindedPrefix = "event:reminded:"

// EventInfo describes the reminder channel for the status endpoint.
type EventInfo struct {
	Enabled   bool `json:"enabled"`
	LeadM     int  `json:"lead_minutes"`
	GraceM    int  `json:"late_grace_minutes"`
	ExpiryM   int  `json:"expiry_lead_minutes"`
	MinScore  int  `json:"min_score"`
	MaxPerDay int  `json:"max_per_day"`
	SentToday int  `json:"sent_today"`
	Upcoming  int  `json:"due_now"`
	// The two counts are split on purpose: one blended number cannot answer
	// "is the next interruption an opening or a closing?".
	DueOpening int `json:"due_opening"`
	DueExpiry  int `json:"due_expiry"`
}

// Event reports the reminder schedule and how many tracked events are waiting.
func (a *App) Event(now time.Time) EventInfo {
	e := a.cfg.Notify.Event
	info := EventInfo{Enabled: e.Enabled, LeadM: int(e.Lead.D() / time.Minute),
		GraceM: int(e.LateGrace.D() / time.Minute), ExpiryM: int(e.ExpiryLead.D() / time.Minute),
		MinScore: e.MinScore, MaxPerDay: e.MaxPerDay}
	if day, n := a.budgetSpent(eventCursor); day == a.localDay(now) {
		info.SentToday = n
	}
	due := a.dueEvents(now)
	info.Upcoming = len(due)
	for _, d := range due {
		if d.word == "截止" {
			info.DueExpiry++
		} else {
			info.DueOpening++
		}
	}
	return info
}

// dueEvent is one stored row the reminder channel should speak up about, and
// which of its two clocks made it due. A voucher announces both a start and an
// end, and both are worth a message — the morning report mentions "closes Friday"
// fourteen hours before it is actionable.
type dueEvent struct {
	deal model.Deal
	when time.Time
	word string // 开抢 or 截止
}

// eventsDue returns the un-reminded rows whose opening or closing moment is
// inside its own window. Both anchors can match the same row; the nearer one
// wins, because that is what the reader still has time to act on.
func (a *App) dueEvents(now time.Time) []dueEvent {
	e := a.cfg.Notify.Event
	if !e.Enabled {
		return nil
	}
	var candidates []dueEvent
	if d := e.ExpiryLead.D(); d > 0 {
		for _, row := range a.st.Expiring(now, now.Add(d), e.MinScore) {
			when, ok := closingInstant(row)
			// A deadline that is exactly now leaves nothing to do, so the expiry
			// side asks for strictly the future. The opening side forgives lateness
			// because a start ten minutes ago is still claimable.
			if !ok || !when.After(now) {
				continue
			}
			candidates = append(candidates, dueEvent{deal: row, when: when, word: "截止"})
		}
	}
	for _, row := range a.st.Upcoming(now.Add(-e.LateGrace.D()), now.Add(e.Lead.D()), e.MinScore) {
		if when, ok := metaInstant(row); ok {
			candidates = append(candidates, dueEvent{deal: row, when: when, word: "开抢"})
		}
	}
	nearest := map[string]dueEvent{}
	for _, c := range candidates {
		if cur, seen := nearest[c.deal.Fingerprint]; !seen || c.when.Before(cur.when) {
			nearest[c.deal.Fingerprint] = c
		}
	}
	out := make([]dueEvent, 0, len(nearest))
	for fp, c := range nearest {
		if _, done := a.stateTime(eventRemindedPrefix + fp); done {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].when.Equal(out[j].when) {
			return out[i].when.Before(out[j].when)
		}
		return out[i].deal.Title < out[j].deal.Title
	})
	return out
}

// sendDueEvents reminds once per dated event. A row that cannot be delivered
// stays unmarked, so the next round retries it; the budget is only charged for a
// message that actually left.
func (a *App) sendDueEvents(ctx context.Context, now time.Time) (sent, held int, err error) {
	due := a.dueEvents(now)
	if len(due) == 0 {
		return 0, 0, nil
	}
	if n := a.cfg.Notify.Event.MaxItems; n > 0 && len(due) > n {
		held += len(due) - n
		due = due[:n]
	}
	if a.budgetLeft(eventCursor, a.cfg.Notify.Event.MaxPerDay, now) <= 0 {
		a.log.Info("event budget spent: reminders wait for the next day",
			"waiting", len(due), "max_per_day", a.cfg.Notify.Event.MaxPerDay)
		return 0, len(due), nil
	}
	msg := notify.NewMessage(notify.KindEvent, a.eventTitle(due, now), dealsOf(due)...)
	if len(due) > 1 {
		// A shared title can only carry one moment, so each row states its own.
		lines := make([]string, 0, len(due))
		for _, d := range due {
			lines = append(lines, "· "+a.formatWhen(d.when)+" "+d.word+" · "+truncateRunes(d.deal.Title, 30))
		}
		msg.Intro = strings.Join(lines, "\n")
	}
	if err := a.nf.Send(ctx, msg); err != nil {
		a.log.Error("event reminder delivery failed; the next round retries", "err", err)
		return 0, len(due), err
	}
	for _, d := range due {
		if err := a.setStateTime(eventRemindedPrefix+d.deal.Fingerprint, now.UTC()); err != nil {
			a.log.Warn("record reminder", "err", err)
		}
	}
	if err := a.spendBudget(eventCursor, now); err != nil {
		a.log.Warn("charge event budget", "err", err)
	}
	return len(due), held, nil
}

func dealsOf(due []dueEvent) []model.Deal {
	out := make([]model.Deal, 0, len(due))
	for _, d := range due {
		out = append(out, d.deal)
	}
	return out
}

// eventTitle leads with the moment, because that is the whole reason the reader
// opened the message.
func (a *App) eventTitle(due []dueEvent, now time.Time) string {
	if len(due) > 1 {
		return fmt.Sprintf("⏰ %d 项临近%s", len(due), due[0].word)
	}
	d := due[0]
	return "⏰ " + a.formatWhen(d.when) + " " + d.word + " · " + truncateRunes(d.deal.Title, 40)
}

// formatWhen renders a moment in the reader's zone.
func (a *App) formatWhen(when time.Time) string {
	return when.In(a.loc()).Format("01月02日 15:04")
}

// closingInstant reads the deadline the scorer recorded, if there was one.
func closingInstant(d model.Deal) (time.Time, bool) {
	v := d.Meta["expires_at"]
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// metaInstant reads the start moment the scorer recorded.
func metaInstant(d model.Deal) (time.Time, bool) {
	v := d.Meta["starts_at"]
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// publish records a finished round where readers can see it. The Run is fully
// populated before this point and never mutated afterwards, so the history lock is
// held for a pointer swap rather than across a collection round.
func (a *App) publish(run *Run) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastRun = run
	a.runs = append(a.runs, run)
	if n := 24; len(a.runs) > n {
		a.runs = a.runs[len(a.runs)-n:]
	}
}

// EventItem is one tracked dated event, for the CLI and the status endpoint.
type EventItem struct {
	Title    string    `json:"title"`
	URL      string    `json:"url"`
	Source   string    `json:"source"`
	Score    int       `json:"score"`
	StartsAt time.Time `json:"starts_at"`
	ClosesAt time.Time `json:"closes_at,omitempty"`
	// Due names the moment the reminder would speak about: 开抢, 截止, or empty
	// while both are still outside their windows.
	Due      string    `json:"due"`
	Moment   time.Time `json:"moment,omitempty"`
	Reminded bool      `json:"reminded"`
	InWindow bool      `json:"in_window"`
}

// UpcomingEvents lists tracked dated events from three days ago to a month out,
// soonest first, each with whether it has been reminded about yet. Events below
// the reminder gate are included deliberately: "why is nothing firing?" is the
// question an operator asks, and a silent omission cannot answer it. A row that
// only ever announced a deadline is tracked too — that is the other half of the
// channel.
func (a *App) UpcomingEvents(now time.Time) []EventItem {
	e := a.cfg.Notify.Event
	from, through := now.Add(-72*time.Hour), now.AddDate(0, 0, 30)
	rows := a.st.Upcoming(from, through, 0)
	if e.Enabled && e.ExpiryLead.D() > 0 {
		rows = append(rows, a.st.Expiring(from, through, 0)...)
	}
	type moment struct {
		item EventItem
		when time.Time
	}
	picked := map[string]moment{}
	for _, d := range rows {
		start, hasStart := metaInstant(d)
		end, hasEnd := closingInstant(d)
		var (
			best moment
			ok   bool
		)
		if hasStart {
			best = moment{item: EventItem{Title: d.Title, URL: d.URL, Source: d.Source,
				Score: d.Score, StartsAt: start}, when: start}
			ok = true
		}
		// The nearer clock is the one worth reporting; a row can announce both.
		if hasEnd && (!ok || best.when.After(end)) {
			best = moment{item: EventItem{Title: d.Title, URL: d.URL, Source: d.Source,
				Score: d.Score, StartsAt: start, ClosesAt: end}, when: end}
			ok = true
		}
		if !ok {
			continue
		}
		best.item.Due = "开抢"
		if !best.item.ClosesAt.IsZero() && best.when.Equal(end) {
			best.item.Due = "截止"
		}
		if cur, seen := picked[d.Fingerprint]; !seen || best.when.Before(cur.when) {
			picked[d.Fingerprint] = best
		}
	}
	out := make([]EventItem, 0, len(picked))
	for fp, m := range picked {
		_, reminded := a.stateTime(eventRemindedPrefix + fp)
		m.item.Moment = m.when
		m.item.Reminded = reminded
		m.item.InWindow = a.inWindow(e, m.when, m.item.Due, now)
		out = append(out, m.item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Moment.Equal(out[j].Moment) {
			return out[i].Moment.Before(out[j].Moment)
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// inWindow reports whether a moment is inside the reminder window for its own
// kind: an opening may still be forgiven a little lateness, a deadline that has
// passed is not a reminder.
func (a *App) inWindow(e config.Event, when time.Time, word string, now time.Time) bool {
	if !e.Enabled {
		return false
	}
	if word == "截止" {
		d := e.ExpiryLead.D()
		return d > 0 && when.After(now) && !when.After(now.Add(d))
	}
	return !when.Before(now.Add(-e.LateGrace.D())) && !when.After(now.Add(e.Lead.D()))
}
