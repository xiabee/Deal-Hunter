// Package model defines the normalized deal record shared by sources, scoring,
// storage and notifiers.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Kind classifies what is actually being offered.
type Kind string

const (
	KindFree     Kind = "free"
	KindDiscount Kind = "discount"
	KindTrial    Kind = "trial"
	KindCredit   Kind = "credit"
	KindCoupon   Kind = "coupon"
	KindUnknown  Kind = "unknown"
)

// Category buckets drive message templates and routing.
const (
	CatAIFree   = "ai_free"
	CatDiscount = "discount"
	CatResource = "resource"
	// CatVoucher marks a locally issued consumption voucher: a dated event rather than a
	// standing free tier, which changes what "still live" means.
	CatVoucher = "voucher"
	CatUnknown = "unknown"
)

// Offer is one keyword-evidenced offer found in a deal.
type Offer struct {
	Kind     Kind   `json:"kind"`
	Matched  string `json:"matched,omitempty"`
	Vendor   string `json:"vendor,omitempty"`
	Product  string `json:"product,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

// Deal is a normalized, deduplicated finding about a price advantage.
type Deal struct {
	Fingerprint  string            `json:"fingerprint"`
	URL          string            `json:"url"`
	Title        string            `json:"title"`
	Summary      string            `json:"summary,omitempty"`
	Source       string            `json:"source"`
	Category     string            `json:"category"`
	Offers       []Offer           `json:"offers,omitempty"`
	Vendors      []string          `json:"vendors,omitempty"`
	Tags         []string          `json:"tags,omitempty"`
	IsFree       bool              `json:"is_free"`
	DiscountPct  int               `json:"discount_pct,omitempty"`
	Score        int               `json:"score"`
	ScoreWhy     []string          `json:"score_why,omitempty"`
	PublishedAt  time.Time         `json:"published_at,omitempty"`
	DiscoveredAt time.Time         `json:"discovered_at"`
	Meta         map[string]string `json:"meta,omitempty"`
}

// EnsureFingerprint derives a stable identity from the cleaned URL, falling
// back to source+title so items without a link still deduplicate.
func (d *Deal) EnsureFingerprint() {
	if d.Fingerprint != "" {
		return
	}
	key := NormalizeURL(d.URL)
	if key == "" {
		key = d.Source + "|" + strings.ToLower(strings.TrimSpace(d.Title))
	}
	sum := sha256.Sum256([]byte(key))
	d.Fingerprint = hex.EncodeToString(sum[:16])
}

// NormalizeURL strips scheme, case, tracking parameters and the trailing slash
// so cosmetic URL differences collapse onto one fingerprint.
func NormalizeURL(u string) string {
	s := strings.TrimSpace(u)
	if s == "" {
		return ""
	}
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.ReplaceAll(s, "www.", "")
	trimmedParams := s
	if i := strings.Index(s, "?"); i >= 0 {
		base := s[:i]
		var keep []string
		for _, p := range strings.Split(s[i+1:], "&") {
			k, _, _ := strings.Cut(p, "=")
			if k == "" || isTrackingParam(strings.ToLower(k)) {
				continue
			}
			keep = append(keep, p)
		}
		if len(keep) == 0 {
			trimmedParams = base
		} else {
			trimmedParams = base + "?" + strings.Join(keep, "&")
		}
	}
	trimmedParams = strings.TrimSuffix(trimmedParams, "/")
	// A fragment is an anchor inside the same page: v2ex.com/t/1#reply4 and
	// v2ex.com/t/1 are one offer, and listing both reads as a broken panel.
	if i := strings.Index(trimmedParams, "#"); i >= 0 {
		trimmedParams = trimmedParams[:i]
	}
	return strings.ToLower(trimmedParams)
}

func isTrackingParam(k string) bool {
	for _, p := range []string{"utm_", "spm", "from", "ref", "share_", "scene", "clickid", "msclkid", "gclid", "force_", "abtest", "callback", "timestamp", "sign"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// dedupNoise drops the punctuation and board prefixes that make two copies of
// the same announcement look like different titles.
var dedupNoise = regexp.MustCompile(`[^\p{Han}\p{L}\p{N}]+`)

// dedupPrefix matches a short leading label terminated by a colon.
var dedupPrefix = regexp.MustCompile(`^[^：:]{1,6}[：:]`)

// DedupKey is the near-duplicate signature of a finding: the same announcement
// reposted across feeds differs only in punctuation, board tag and whitespace.
// An empty key means the title carries too little to judge by, so it never
// deduplicates.
func DedupKey(title string) string {
	s := strings.TrimSpace(title)
	if s == "" {
		return ""
	}
	// Leading "[板块] 标题" / "【公告】标题" / "(推广) 标题" wrappers.
	for {
		t := strings.TrimLeft(s, " \t")
		var cut bool
		for _, pair := range [][2]string{{"[", "]"}, {"【", "】"}, {"(", ")"}, {"（", "）"}} {
			if strings.HasPrefix(t, pair[0]) {
				if i := strings.Index(t, pair[1]); i > 0 && i < 24 {
					t, cut = strings.TrimSpace(t[i+len(pair[1]):]), true
				}
			}
		}
		if !cut {
			s = t
			break
		}
		s = t
	}
	// "推广：xxx" / "公告: xxx" carry the same board tag without brackets.
	for i := 0; i < 2; i++ {
		m := dedupPrefix.FindStringIndex(s)
		if m == nil || m[0] != 0 {
			break
		}
		s = strings.TrimSpace(s[m[1]:])
	}
	key := strings.ToLower(dedupNoise.ReplaceAllString(s, " "))
	key = strings.Join(strings.Fields(key), " ")
	if len([]rune(key)) < 8 {
		return ""
	}
	r := []rune(key)
	if len(r) > 80 {
		r = r[:80]
	}
	return strings.TrimSpace(string(r))
}

// doubtAboutOffer names a question or a gripe about somebody else's pricing,
// which is nothing the reader can claim this morning. 请教 is only a request when
// it does not follow 申: "免费申请教育许可证" and "额度申请教程" are 申请+教程, the
// single most common false positive this filter produced on real traffic.
var doubtAboutOffer = regexp.MustCompile(`吐槽|求助|是不是|有没有|该选|如何评价|值不值|翻车|失望|求佬?解答|求指导|(^|[^申])请教|求推荐|帮我看看`)

// endsInQuestion reports whether the title is phrased as a question or a guess.
func endsInQuestion(title string) bool {
	t := strings.TrimRight(strings.TrimSpace(title), " ！!")
	for _, suffix := range []string{"?", "？", "吗", "呢", "吧", "么"} {
		if strings.HasSuffix(t, suffix) {
			return true
		}
	}
	return false
}

// Claimable reports whether the finding describes an offer that can still be
// taken: it carries a free/discount/trial/credit/coupon signal and is not a
// question or a gripe about somebody else's pricing.
func (d *Deal) Claimable() bool {
	if endsInQuestion(d.Title) || doubtAboutOffer.MatchString(d.Title) {
		return false
	}
	if d.IsFree || d.DiscountPct > 0 {
		return true
	}
	for _, o := range d.Offers {
		switch o.Kind {
		case KindFree, KindDiscount, KindTrial, KindCredit, KindCoupon:
			return true
		}
	}
	return false
}

// SortOffersAndTags normalizes slice order so identical deals serialize the same.
func (d *Deal) SortOffersAndTags() {
	sort.SliceStable(d.Offers, func(i, j int) bool {
		if d.Offers[i].Kind != d.Offers[j].Kind {
			return d.Offers[i].Kind < d.Offers[j].Kind
		}
		return d.Offers[i].Matched < d.Offers[j].Matched
	})
	sort.Strings(d.Tags)
	sort.Strings(d.Vendors)
}

// TextBlob is everything textual about the deal, used by keyword and scoring passes.
func (d *Deal) TextBlob() string {
	parts := []string{d.Title, d.Summary}
	for _, o := range d.Offers {
		parts = append(parts, o.Evidence)
	}
	return strings.Join(parts, "\n")
}

// IsDatedEvent reports whether this finding is a one-off event with a moment of
// its own rather than a standing offer. Government voucher notices stay listed
// forever and often never say when they end, so "no deadline mentioned" cannot
// mean "still claimable" for them.
func (d *Deal) IsDatedEvent() bool {
	return d.Category == CatVoucher || d.Meta["starts_at"] != ""
}
