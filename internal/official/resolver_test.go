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

func TestCanonicalEntryPageRewritesLink(t *testing.T) {
	canonical := "https://open.bigmodel.cn/pricing"
	probe := &fakeProbe{answers: map[string]string{canonical: "https://open.bigmodel.cn/pricing/"}}
	r, cache := newTestResolver(probe, &fakeSearch{})
	d := testDeal("https://www.v2ex.com/t/123", "智谱AI", model.KindFree)
	if r.Apply(context.Background(), []*model.Deal{d}) != 1 {
		t.Fatal("the curated entry page answered, so this is verified")
	}
	if d.Meta[MetaLinkKind] != KindVendorEntry {
		t.Errorf("link_kind = %q", d.Meta[MetaLinkKind])
	}
	if d.Meta[MetaOfficialURL] != canonical+"/" {
		t.Errorf("should keep the redirect target actually served: %q", d.Meta[MetaOfficialURL])
	}
	if d.URL != "https://www.v2ex.com/t/123" {
		t.Error("the original post must never be overwritten, only the link we present")
	}
	if d.Score != 78 {
		t.Errorf("score = %d, want 70+8", d.Score)
	}
	if !strings.Contains(strings.Join(d.ScoreWhy, "|"), "厂商入口页") {
		t.Errorf("the bonus must be explained: %v", d.ScoreWhy)
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
	canonical := "https://open.bigmodel.cn/pricing"
	probe := &fakeProbe{answers: map[string]string{canonical: canonical}}
	sr := &fakeSearch{hits: []string{"https://never-queried.example/x"}}
	r, _ := newTestResolver(probe, sr)
	first := testDeal("https://www.v2ex.com/t/1", "智谱AI", model.KindFree)
	if r.Apply(context.Background(), []*model.Deal{first}) != 1 {
		t.Fatal("first round should verify")
	}
	after := probe.total()
	second := testDeal(first.URL, "智谱AI", model.KindFree)
	second.Title = "同一个活动的后续报道"
	second.EnsureFingerprint()
	if r.Apply(context.Background(), []*model.Deal{second}) != 1 {
		t.Fatal("cached verdict should still count as verified")
	}
	if probe.total() != after || len(sr.calls) != 0 {
		t.Errorf("cached deal must not touch the network (probes +%d, queries %d)", probe.total()-after, len(sr.calls))
	}
	if second.Meta[MetaLinkKind] != KindVendorEntry {
		t.Errorf("link_kind = %q", second.Meta[MetaLinkKind])
	}
}

func TestUnverifiableCanonicalPageIsNotUsed(t *testing.T) {
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
	if d.Score != 64 {
		t.Errorf("score = %d, want 70-6", d.Score)
	}
	var v verdict
	if b, ok := cache.m["official:deal:"+d.Fingerprint]; !ok || json.Unmarshal(b, &v) != nil || v.Kind != KindThirdParty {
		t.Error("negative verdicts are cached too, so a dead page is not retried every round")
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
	r, _ := newTestResolver(probe, &fakeSearch{hits: []string{"https://open.bigmodel.cn/a"}})
	r.SetBudget(1)
	deals := []*model.Deal{
		testDeal("https://www.v2ex.com/t/a", "智谱AI", model.KindFree),
		testDeal("https://www.v2ex.com/t/b", "智谱AI", model.KindFree),
	}
	r.Apply(context.Background(), deals)
	if deals[1].Meta[MetaResolvedBy] != "budget" {
		t.Errorf("second deal should hit the budget: %v", deals[1].Meta)
	}
	if deals[1].Score != 70 {
		t.Error("running out of budget is our limit, not the deal's fault")
	}
	if probe.total() > 1 {
		t.Errorf("budget exceeded: %d probes", probe.total())
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
	stale := verdict{URL: "https://open.bigmodel.cn/old", Kind: KindVendorEntry, By: "canonical",
		At: time.Now().Add(-8 * 24 * time.Hour)}
	b, _ := json.Marshal(stale)
	probe := &fakeProbe{answers: map[string]string{"https://open.bigmodel.cn/pricing": "https://open.bigmodel.cn/pricing"}}
	r := NewResolver(probe, &fakeSearch{}, cache, nil)
	d := testDeal("https://www.v2ex.com/t/7", "智谱AI", model.KindFree)
	cache.m["official:deal:"+d.Fingerprint] = b
	if r.Apply(context.Background(), []*model.Deal{d}) != 1 {
		t.Fatal("a stale verdict must not be trusted")
	}
	if d.Meta[MetaOfficialURL] == stale.URL {
		t.Error("the old cached page should have been re-resolved")
	}
}
