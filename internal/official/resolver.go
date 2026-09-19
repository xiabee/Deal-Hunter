package official

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

// Meta keys written by the resolver. They are surfaced in the card, the API and
// the dashboard, so "how did we arrive at this link" is always answerable.
const (
	MetaLinkKind    = "link_kind"
	MetaOfficialURL = "official_url"
	MetaOriginalURL = "original_url"
	MetaResolvedBy  = "resolved_by"
	MetaVendorURL   = "vendor_url"

	KindAlreadyOfficial = "official"
	KindSearchVerified  = "search_verified"
	KindThirdParty      = "third_party"
)

// Link kinds considered official (i.e. pointing at the vendor's own site).
var officialKinds = map[string]bool{
	KindAlreadyOfficial: true, KindSearchVerified: true,
}

// IsOfficialKind reports whether a link_kind value denotes a verified vendor link.
func IsOfficialKind(kind string) bool { return officialKinds[kind] }

// Score deltas applied by resolution. Verified vendor links are rewarded;
// unverifiable third-party chatter is discounted rather than hidden, so it can
// never outrank a confirmed official page.
const (
	bonusVerified     = 8
	penaltyThirdParty = 6
)

// Prober confirms a URL actually answers (after redirects).
type Prober interface {
	OK(ctx context.Context, rawURL string) (finalURL string, ok bool)
}

// Searcher runs a site-scoped query and returns result URLs in rank order.
type Searcher interface {
	Search(ctx context.Context, query string) ([]string, error)
}

// Cache stores verdicts; matches store.Store.
type Cache interface {
	GetState(key string) ([]byte, bool)
	PutState(key string, v any) error
}

// Resolver walks the link ladder for a batch of deals.
type Resolver struct {
	probe      Prober
	search     Searcher
	cache      Cache
	log        *slog.Logger
	ttl        time.Duration
	maxLookups int
}

// NewResolver builds a resolver. Any dependency may be nil, in which case the
// corresponding rung of the ladder is skipped.
func NewResolver(probe Prober, s Searcher, c Cache, log *slog.Logger) *Resolver {
	if log == nil {
		log = slog.Default()
	}
	return &Resolver{probe: probe, search: s, cache: c, log: log,
		ttl: 7 * 24 * time.Hour, maxLookups: 10}
}

// SetBudget caps how much verification work one round may do.
func (r *Resolver) SetBudget(n int) {
	if n > 0 {
		r.maxLookups = n
	}
}

type verdict struct {
	URL  string    `json:"url"`
	Kind string    `json:"kind"`
	By   string    `json:"by,omitempty"`
	At   time.Time `json:"at"`
}

// Apply resolves links for the given deals, mutating Score/ScoreWhy/Meta.
// It returns the number that ended up on a verified vendor link.
func (r *Resolver) Apply(ctx context.Context, deals []*model.Deal) int {
	lookups := 0
	resolved := 0
	for _, d := range deals {
		if ctx.Err() != nil {
			break
		}
		if r.applyOne(ctx, d, &lookups) {
			resolved++
		}
	}
	if lookups > 0 {
		r.log.Debug("official: link resolution pass", "lookups", lookups, "verified", resolved, "deals", len(deals))
	}
	return resolved
}

func (r *Resolver) applyOne(ctx context.Context, d *model.Deal, lookups *int) bool {
	if d.Meta == nil {
		d.Meta = map[string]string{}
	}
	vendor := primaryVendor(d)

	// Rung 1: already on the vendor's own domain. Free — no network.
	if v := Classify(d.URL, vendor); v.Official {
		setLabel(d, d.URL, KindAlreadyOfficial, "domain")
		r.bump(d, bonusVerified, "链接为厂商官方域名")
		return true
	}
	if vendor == "" {
		setLabel(d, d.URL, KindThirdParty, "no-vendor")
		return false
	}
	// The vendor's front door is attached to every verdict below: it is a place
	// to check, not evidence, so it never replaces the presented link.
	r.attachVendorSite(ctx, d, vendor, lookups)

	// Cached verdict from an earlier round.
	if v, ok := r.cached(d.Fingerprint); ok && time.Since(v.At) < r.ttl && v.URL != "" {
		setLabel(d, v.URL, v.Kind, v.By)
		if IsOfficialKind(v.Kind) {
			r.bump(d, bonusVerified, "已校验的官方链接（缓存）")
			return true
		}
		r.bump(d, -penaltyThirdParty, "仅第三方来源，未定位到官方页")
		return false
	}
	if *lookups >= r.maxLookups {
		setLabel(d, d.URL, KindThirdParty, "budget")
		return false
	}

	// Rung 2: search the vendor's own domain with *this deal's words* — the model
	// name, the offer wording — and verify the page answers. This is the only
	// rewrite that claims to have found the offer itself.
	if r.search == nil {
		r.unresolved(d, "no-search", 0)
		return false
	}
	q, ok := searchQueryFor(vendor, d)
	if !ok {
		r.unresolved(d, "no-query", penaltyThirdParty)
		return false
	}
	*lookups++
	hits, err := r.search.Search(ctx, q)
	if err != nil {
		// A search endpoint that is down says nothing about the deal. Discounting
		// it would let an outage quietly reorder the alerts, so label and move on.
		if !errors.Is(err, context.Canceled) {
			r.log.Debug("official: search failed", "vendor", vendor, "err", err)
		}
		r.unresolved(d, "search-unavailable", 0)
		return false
	}
	for _, h := range hits {
		if *lookups >= r.maxLookups {
			break
		}
		// Only a host the vendor owns counts: a third-party blog retelling the
		// offer is exactly what this rung exists to get away from.
		if owner, isVendor := VendorForDomain(h); !isVendor || owner != vendor {
			continue
		}
		*lookups++
		final, good := r.verify(ctx, h, vendor)
		if !good {
			continue
		}
		r.store(d.Fingerprint, final, KindSearchVerified, "search")
		setLabel(d, final, KindSearchVerified, "search")
		r.bump(d, bonusVerified, "在厂商站内检索到该 offer 并校验通过")
		return true
	}

	r.unresolved(d, "unresolved", penaltyThirdParty)
	return false
}

// attachVendorSite records the vendor's own entry page as a side link, verified
// to answer. An entry page proves where the vendor lives, not that this offer
// exists, so it earns no score and never becomes the presented URL.
func (r *Resolver) attachVendorSite(ctx context.Context, d *model.Deal, vendor string, lookups *int) {
	if d.Meta[MetaVendorURL] != "" {
		return
	}
	kinds := make([]string, 0, len(d.Offers))
	for _, o := range d.Offers {
		kinds = append(kinds, string(o.Kind))
	}
	u, ok := CanonicalFor(vendor, kinds)
	if !ok || u == d.URL || *lookups >= r.maxLookups {
		return
	}
	*lookups++
	// One probe per vendor page, then cached for the whole TTL, so this costs a
	// round at most one request per vendor.
	if final, good := r.verify(ctx, u, vendor); good {
		d.Meta[MetaVendorURL] = final
	}
}

// searchQueryFor aims a query at the vendor's own domain, preferring the model
// name and falling back to the wording of the post that surfaced the deal.
func searchQueryFor(vendor string, d *model.Deal) (string, bool) {
	if q, _, ok := SearchQuery(vendor, productHint(d), d.IsFree); ok {
		return q, true
	}
	domains := OfficialDomainsOf(vendor)
	kw := KeywordsFromTitle(d.Title, 4)
	if len(domains) == 0 || kw == "" {
		return "", false
	}
	return kw + " site:" + strings.Join(domains, " site:"), true
}

func (r *Resolver) unresolved(d *model.Deal, by string, penalty int) {
	r.store(d.Fingerprint, d.URL, KindThirdParty, by)
	setLabel(d, d.URL, KindThirdParty, by)
	if penalty > 0 {
		r.bump(d, -penalty, "仅第三方来源，未定位到官方页")
	}
}

func (r *Resolver) verify(ctx context.Context, u, vendor string) (string, bool) {
	if r.probe == nil {
		return "", false
	}
	key := "official:verify:" + u
	if b, ok := r.cacheGet(key); ok {
		var v verdict
		if json.Unmarshal(b, &v) == nil && time.Since(v.At) < r.ttl {
			return v.URL, v.Kind == "ok"
		}
	}
	final, good := r.probe.OK(ctx, u)
	if final == "" {
		final = u
	}
	// Redirects are where "official" links go wrong: the destination must still
	// belong to the vendor, or we would happily point at whatever a pricing page
	// decides to promote today.
	if good && vendor != "" {
		if owner, isVendor := VendorForDomain(final); !isVendor || owner != vendor {
			r.log.Debug("official: link left the vendor's own domains", "from", u, "to", final)
			good = false
		}
	}
	kind := "fail"
	if good {
		kind = "ok"
	}
	if r.cache != nil {
		_ = r.cache.PutState(key, verdict{URL: final, Kind: kind, At: time.Now().UTC()})
	}
	return final, good
}

func setLabel(d *model.Deal, officialURL, kind, by string) {
	if d.Meta == nil {
		d.Meta = map[string]string{}
	}
	if d.Meta[MetaOriginalURL] == "" {
		d.Meta[MetaOriginalURL] = d.URL
	}
	d.Meta[MetaLinkKind] = kind
	d.Meta[MetaResolvedBy] = by
	d.Meta[MetaOfficialURL] = officialURL
}

// LabelCheap records the verdicts that cost nothing: who owns this host, if
// anybody. It runs for every stored deal, not just the handful that reach the
// alert queue, so the panel and the digest never contradict the card. No
// network, no cache writes, and deliberately no score change — only the full
// ladder earns or loses points.
func LabelCheap(d *model.Deal) {
	if d.Meta == nil {
		d.Meta = map[string]string{}
	}
	if d.Meta[MetaLinkKind] != "" {
		return
	}
	vendor := primaryVendor(d)
	if v := Classify(d.URL, vendor); v.Official {
		setLabel(d, d.URL, KindAlreadyOfficial, "domain")
		return
	}
	// With no vendor named at all there is nothing to look up: this is a
	// retelling by definition. A known vendor stays unlabelled until verified.
	if vendor == "" {
		setLabel(d, d.URL, KindThirdParty, "no-vendor")
	}
}

func (r *Resolver) bump(d *model.Deal, delta int, why string) {
	if delta == 0 {
		return
	}
	d.Score += delta
	if d.Score < 0 {
		d.Score = 0
	}
	if d.Score > 100 {
		d.Score = 100
	}
	d.ScoreWhy = append(d.ScoreWhy, signed(delta)+" "+why)
}

func signed(n int) string {
	if n >= 0 {
		return "+" + itoa(n)
	}
	return "-" + itoa(-n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func (r *Resolver) cached(fp string) (verdict, bool) {
	b, ok := r.cacheGet("official:deal:" + fp)
	if !ok {
		return verdict{}, false
	}
	var v verdict
	if json.Unmarshal(b, &v) != nil {
		return verdict{}, false
	}
	return v, true
}

func (r *Resolver) cacheGet(key string) ([]byte, bool) {
	if r.cache == nil {
		return nil, false
	}
	return r.cache.GetState(key)
}

func (r *Resolver) store(fp, url, kind, by string) {
	if r.cache == nil {
		return
	}
	_ = r.cache.PutState("official:deal:"+fp, verdict{URL: url, Kind: kind, By: by, At: time.Now().UTC()})
}

func primaryVendor(d *model.Deal) string {
	if len(d.Vendors) > 0 {
		return d.Vendors[0]
	}
	for _, o := range d.Offers {
		if o.Vendor != "" {
			return o.Vendor
		}
	}
	return ""
}

func productHint(d *model.Deal) string {
	for _, t := range d.Tags {
		if v, ok := cutPrefix(t, "model:"); ok {
			return v
		}
	}
	for _, o := range d.Offers {
		if o.Product != "" {
			return o.Product
		}
	}
	if v, ok := cutPrefix(d.Meta["model_id"], ""); ok && v != "" && d.Meta["model_id"] != "" {
		return v
	}
	return ""
}

func cutPrefix(s, prefix string) (string, bool) {
	if prefix == "" {
		return s, s != ""
	}
	if strings.HasPrefix(s, prefix) {
		return strings.TrimPrefix(s, prefix), true
	}
	return "", false
}
