package official

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

// fakeProbe records which URLs it was asked about; anything absent does not answer.
type fakeProbe struct {
	answers map[string]string // url -> final url after redirects
	calls   map[string]int
}

func (p *fakeProbe) OK(_ context.Context, u string) (string, bool) {
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[u]++
	final, ok := p.answers[u]
	if !ok {
		return "", false
	}
	if final == "" {
		final = u
	}
	return final, true
}

func (p *fakeProbe) total() int {
	n := 0
	for _, c := range p.calls {
		n += c
	}
	return n
}

// fakeSearch answers every query from one scripted list.
type fakeSearch struct {
	hits  []string
	err   error
	calls []string
}

func (f *fakeSearch) Search(_ context.Context, q string) ([]string, error) {
	f.calls = append(f.calls, q)
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

// fakeCache is the slice of store.Store the resolver uses.
type fakeCache struct {
	m  map[string][]byte
	ws int
}

func (c *fakeCache) GetState(key string) ([]byte, bool) {
	b, ok := c.m[key]
	return b, ok
}

func (c *fakeCache) PutState(key string, v any) error {
	if c.m == nil {
		c.m = map[string][]byte{}
	}
	b, err := json.Marshal(v)
	c.m[key] = b
	c.ws++
	return err
}

func testDeal(url, vendor string, kinds ...model.Kind) *model.Deal {
	d := &model.Deal{
		URL: url, Title: vendor + " 免费额度开放", Source: "test", Score: 70,
		IsFree: true, DiscoveredAt: time.Now().UTC(),
	}
	if vendor != "" {
		d.Vendors = []string{vendor}
	}
	for _, k := range kinds {
		d.Offers = append(d.Offers, model.Offer{Kind: k, Vendor: vendor})
	}
	d.EnsureFingerprint()
	return d
}

func newTestResolver(probe Prober, s Searcher) (*Resolver, *fakeCache) {
	c := &fakeCache{}
	r := NewResolver(probe, s, c, nil)
	r.SetBudget(10)
	return r, c
}

func TestAlreadyOfficialNeedsNoNetwork(t *testing.T) {
	probe := &fakeProbe{}
	r, _ := newTestResolver(probe, &fakeSearch{})
	d := testDeal("https://platform.moonshot.cn/docs/pricing", "月之暗面", model.KindFree)
	if r.Apply(context.Background(), []*model.Deal{d}) != 1 {
		t.Fatal("a vendor's own domain is official by definition")
	}
	if d.Meta[MetaLinkKind] != KindAlreadyOfficial {
		t.Errorf("link_kind = %q", d.Meta[MetaLinkKind])
	}
	if d.Meta[MetaOriginalURL] != d.URL {
		t.Error("original_url should still record where it came from")
	}
	if probe.total() != 0 {
		t.Errorf("rung 1 must not make requests, got %d", probe.total())
	}
}

func TestUnknownVendorIsLeftAlone(t *testing.T) {
	probe := &fakeProbe{}
	sr := &fakeSearch{}
	r, _ := newTestResolver(probe, sr)
	d := testDeal("https://linux.do/t/x", "")
	if r.Apply(context.Background(), []*model.Deal{d}) != 0 {
		t.Fatal("nothing was verified")
	}
	if d.Meta[MetaLinkKind] != KindThirdParty || d.Meta[MetaResolvedBy] != "no-vendor" {
		t.Errorf("meta = %v", d.Meta)
	}
	if probe.total() != 0 || len(sr.calls) != 0 {
		t.Error("an unattributable deal must not spend lookups")
	}
	if d.Score != 70 {
		t.Errorf("score should be untouched without evidence, got %d", d.Score)
	}
}

// A curated entry page says where the vendor lives, not that this offer exists.
// It is attached as a side door and must never be presented as the found offer.
func TestEntryPageIsASideDoorNotAClaim(t *testing.T) {
	canonical := "https://open.bigmodel.cn/pricing"
	probe := &fakeProbe{answers: map[string]string{canonical: canonical}}
	r, cache := newTestResolver(probe, &fakeSearch{})
	d := testDeal("https://www.v2ex.com/t/123", "智谱AI", model.KindFree)
	if r.Apply(context.Background(), []*model.Deal{d}) != 0 {
		t.Fatal("an entry page is not a verified offer")
	}
	if d.Meta[MetaLinkKind] != KindThirdParty {
		t.Errorf("link_kind = %q, want third_party", d.Meta[MetaLinkKind])
	}
	if d.Meta[MetaOfficialURL] != d.URL {
		t.Errorf("the presented link must stay the post itself: %q", d.Meta[MetaOfficialURL])
	}
	if d.Meta[MetaVendorURL] != canonical {
		t.Errorf("the vendor's front door should still be offered: %q", d.Meta[MetaVendorURL])
	}
	if probe.calls[canonical] != 1 {
		t.Errorf("the entry page should be probed exactly once, got %d", probe.calls[canonical])
	}
	if d.Score != 64 {
		t.Errorf("score = %d, want 70-6 (unverified third-party)", d.Score)
	}
	if !strings.Contains(strings.Join(d.ScoreWhy, "|"), "仅第三方来源") {
		t.Errorf("the penalty must be explained: %v", d.ScoreWhy)
	}
	if _, ok := cache.m["official:deal:"+d.Fingerprint]; !ok {
		t.Error("verdict should be cached for later rounds")
	}
}

func TestSearchOnlyAcceptsVendorOwnedHosts(t *testing.T) {
	target := "https://platform.moonshot.cn/pricing/free"
	probe := &fakeProbe{answers: map[string]string{target: target}}
	sr := &fakeSearch{hits: []string{
		"https://linux.do/t/1",            // a retelling, not the vendor
		"https://moonshot.cn.evil.test/x", // looks similar, is not owned
		target,
		"https://platform.moonshot.cn/never-reached",
	}}
	r, _ := newTestResolver(probe, sr)
	d := testDeal("https://linux.do/t/1", "月之暗面", model.KindFree)
	if r.Apply(context.Background(), []*model.Deal{d}) != 1 {
		t.Fatal("expected the vendor page to be found")
	}
	if d.Meta[MetaLinkKind] != KindSearchVerified || d.Meta[MetaOfficialURL] != target {
		t.Errorf("meta = %v", d.Meta)
	}
	if probe.calls["https://linux.do/t/1"] != 0 {
		t.Error("a third-party hit must be rejected before it is probed")
	}
	if probe.calls["https://moonshot.cn.evil.test/x"] != 0 {
		t.Error("suffix look-alikes must never be presented as official")
	}
	if _, ok := probe.calls["https://platform.moonshot.cn/never-reached"]; ok {
		t.Error("resolution stops at the first verified page")
	}
	for _, q := range sr.calls {
		if !strings.Contains(q, "site:") {
			t.Errorf("queries must be scoped to the vendor domain: %q", q)
		}
	}
}

func TestVerdictIsReusedNextRound(t *testing.T) {
	target := "https://open.bigmodel.cn/pricing/glm-free"
	probe := &fakeProbe{answers: map[string]string{target: target}}
	sr := &fakeSearch{hits: []string{target}}
	r, _ := newTestResolver(probe, sr)
	first := testDeal("https://www.v2ex.com/t/1", "智谱AI", model.KindFree)
	if r.Apply(context.Background(), []*model.Deal{first}) != 1 {
		t.Fatal("first round should verify via the vendor's own site")
	}
	if first.Meta[MetaLinkKind] != KindSearchVerified {
		t.Fatalf("link_kind = %q", first.Meta[MetaLinkKind])
	}
	afterProbe, afterSearch := probe.total(), len(sr.calls)
	second := testDeal(first.URL, "智谱AI", model.KindFree)
	second.Title = "同一个活动的后续报道"
	if r.Apply(context.Background(), []*model.Deal{second}) != 1 {
		t.Fatal("cached verdict should still count as verified")
	}
	if probe.total() != afterProbe || len(sr.calls) != afterSearch {
		t.Errorf("a cached deal must not touch the network (probes +%d, queries +%d)",
			probe.total()-afterProbe, len(sr.calls)-afterSearch)
	}
	if second.Meta[MetaLinkKind] != KindSearchVerified {
		t.Errorf("link_kind = %q", second.Meta[MetaLinkKind])
	}
}

func TestDeadEntryPageIsNotOffered(t *testing.T) {
	// The curated page 404s and search finds nothing on the vendor's domain:
	// the deal must stay labelled third-party rather than link to a dead page.
	probe := &fakeProbe{}
	r, cache := newTestResolver(probe, &fakeSearch{})
	d := testDeal("https://www.v2ex.com/t/2", "智谱AI", model.KindFree)
	if r.Apply(context.Background(), []*model.Deal{d}) != 0 {
		t.Fatal("nothing was verified")
	}
	if d.Meta[MetaOfficialURL] != d.URL {
		t.Errorf("official_url should fall back to the post itself: %q", d.Meta[MetaOfficialURL])
	}
	if d.Meta[MetaVendorURL] != "" {
		t.Errorf("a page that does not answer must not be offered as the vendor's site: %q", d.Meta[MetaVendorURL])
	}
	if d.Score != 64 {
		t.Errorf("score = %d, want 70-6", d.Score)
	}
	var v verdict
	if b, ok := cache.m["official:deal:"+d.Fingerprint]; !ok || json.Unmarshal(b, &v) != nil || v.Kind != KindThirdParty {
		t.Error("negative verdicts are cached too, so a dead page is not retried every round")
	}
}

// A pricing page that redirects into somebody else's promotion is the exact
// failure this package exists to prevent: the destination must still be owned
// by the vendor before we present it.
func TestRedirectOffAllowlistIsRejected(t *testing.T) {
	canonical := "https://open.bigmodel.cn/pricing"
	target := "https://open.bigmodel.cn/activity/glm"
	probe := &fakeProbe{answers: map[string]string{
		canonical: "https://ads.example.net/landing",
		target:    "https://sponsor.example.org/deal",
	}}
	r, _ := newTestResolver(probe, &fakeSearch{hits: []string{target}})
	d := testDeal("https://www.v2ex.com/t/8", "智谱AI", model.KindFree)
	if r.Apply(context.Background(), []*model.Deal{d}) != 0 {
		t.Fatal("neither page stayed on the vendor's domains")
	}
	if d.Meta[MetaOfficialURL] != d.URL {
		t.Errorf("must not present a redirected third-party page: %q", d.Meta[MetaOfficialURL])
	}
	if d.Meta[MetaVendorURL] != "" {
		t.Errorf("vendor_url must be a vendor-owned host, got %q", d.Meta[MetaVendorURL])
	}
}

func TestSearchOutageIsNotPunished(t *testing.T) {
	r, _ := newTestResolver(&fakeProbe{}, &fakeSearch{err: errors.New("duckduckgo: 503")})
	d := testDeal("https://www.v2ex.com/t/3", "智谱AI", model.KindFree)
	if r.Apply(context.Background(), []*model.Deal{d}) != 0 {
		t.Fatal("nothing was verified")
	}
	if d.Meta[MetaResolvedBy] != "search-unavailable" {
		t.Errorf("resolved_by = %q", d.Meta[MetaResolvedBy])
	}
	if d.Score != 70 {
		t.Errorf("an outage must not silently downgrade findings: %d", d.Score)
	}
}

func TestBudgetCapsLookups(t *testing.T) {
	probe := &fakeProbe{}
	sr := &fakeSearch{hits: []string{"https://open.bigmodel.cn/a"}}
	r, _ := newTestResolver(probe, sr)
	r.SetBudget(2)
	deals := []*model.Deal{
		testDeal("https://www.v2ex.com/t/a", "智谱AI", model.KindFree),
		testDeal("https://www.v2ex.com/t/b", "智谱AI", model.KindFree),
	}
	r.Apply(context.Background(), deals)
	if len(sr.calls) != 1 {
		t.Errorf("the budget must bound searches too, got %d", len(sr.calls))
	}
	if probe.total() != 1 {
		t.Errorf("only the entry page may be probed, got %d probes", probe.total())
	}
	if deals[1].Meta[MetaResolvedBy] != "budget" {
		t.Errorf("second deal should hit the budget: %v", deals[1].Meta)
	}
	if deals[1].Score != 70 {
		t.Error("running out of budget is our limit, not the deal's fault")
	}
}

func TestMissingDepsDegradeWithoutRewriting(t *testing.T) {
	r, _ := newTestResolver(nil, nil)
	d := testDeal("https://www.v2ex.com/t/4", "智谱AI", model.KindFree)
	if r.Apply(context.Background(), []*model.Deal{d}) != 0 {
		t.Fatal("no probe, no search: nothing can be verified")
	}
	if d.Meta[MetaLinkKind] != KindThirdParty || d.Meta[MetaOfficialURL] != d.URL {
		t.Errorf("meta = %v", d.Meta)
	}
	if d.Meta[MetaResolvedBy] != "no-search" {
		t.Errorf("resolved_by = %q", d.Meta[MetaResolvedBy])
	}
}

func TestLabelCheapOnlyUsesFreeVerdicts(t *testing.T) {
	owned := testDeal("https://www.volcengine.com/activity/ai", "火山引擎", model.KindFree)
	owned.Score = 50
	LabelCheap(owned)
	if owned.Meta[MetaLinkKind] != KindAlreadyOfficial || owned.Meta[MetaResolvedBy] != "domain" {
		t.Errorf("a vendor-owned host needs no lookup: %v", owned.Meta)
	}
	if owned.Meta[MetaOfficialURL] != owned.URL || owned.Meta[MetaOriginalURL] != owned.URL {
		t.Errorf("meta = %v", owned.Meta)
	}
	if owned.Score != 50 {
		t.Errorf("the cheap pass must not score, got %d", owned.Score)
	}

	unowned := testDeal("https://blog.example.net/someone-says-free", "", model.KindFree)
	LabelCheap(unowned)
	if unowned.Meta[MetaLinkKind] != KindThirdParty || unowned.Meta[MetaResolvedBy] != "no-vendor" {
		t.Errorf("a deal with no vendor is a retelling by definition: %v", unowned.Meta)
	}

	// A known vendor on someone else's host stays unlabelled until the ladder
	// actually checks it: an absent badge must mean "not verified", never a
	// conclusion we did not earn.
	waiting := testDeal("https://www.v2ex.com/t/9", "智谱AI", model.KindFree)
	LabelCheap(waiting)
	if _, ok := waiting.Meta[MetaLinkKind]; ok {
		t.Errorf("must not guess before looking: %v", waiting.Meta)
	}

	waiting.Meta[MetaLinkKind] = KindSearchVerified
	LabelCheap(waiting)
	if waiting.Meta[MetaLinkKind] != KindSearchVerified {
		t.Error("an existing verdict must never be downgraded")
	}
}

func TestScoreStaysInRange(t *testing.T) {
	r, _ := newTestResolver(&fakeProbe{}, &fakeSearch{})
	top := testDeal("https://www.v2ex.com/t/5", "智谱AI", model.KindFree)
	top.Score = 100
	r.Apply(context.Background(), []*model.Deal{top})
	if top.Score > 100 {
		t.Errorf("score clamped: %d", top.Score)
	}
	low := testDeal("https://www.v2ex.com/t/6", "智谱AI", model.KindFree)
	low.Score = 3
	r.Apply(context.Background(), []*model.Deal{low})
	if low.Score != 0 {
		t.Errorf("score must not go negative: %d", low.Score)
	}
}

func TestExpiredCacheIsRechecked(t *testing.T) {
	cache := &fakeCache{m: map[string][]byte{}}
	stale := verdict{URL: "https://open.bigmodel.cn/old", Kind: KindSearchVerified, By: "search",
		At: time.Now().Add(-8 * 24 * time.Hour)}
	b, _ := json.Marshal(stale)
	fresh := "https://open.bigmodel.cn/pricing/glm-free"
	probe := &fakeProbe{answers: map[string]string{
		"https://open.bigmodel.cn/pricing": "https://open.bigmodel.cn/pricing",
		fresh:                              fresh,
	}}
	r := NewResolver(probe, &fakeSearch{hits: []string{fresh}}, cache, nil)
	d := testDeal("https://www.v2ex.com/t/7", "智谱AI", model.KindFree)
	cache.m["official:deal:"+d.Fingerprint] = b
	if r.Apply(context.Background(), []*model.Deal{d}) != 1 {
		t.Fatal("a stale verdict must not be trusted")
	}
	if d.Meta[MetaOfficialURL] != fresh {
		t.Errorf("should have re-resolved to the page found now, got %q", d.Meta[MetaOfficialURL])
	}
}
