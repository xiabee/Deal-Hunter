// Package model defines the normalized deal record shared by sources, scoring,
// storage and notifiers.
package model

import (
	"crypto/sha256"
	"encoding/hex"
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
	CatUnknown  = "unknown"
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

// Kinds lists the distinct offer kinds in the deal.
func (d *Deal) Kinds() []Kind {
	seen := map[Kind]bool{}
	var out []Kind
	for _, o := range d.Offers {
		if !seen[o.Kind] {
			seen[o.Kind] = true
			out = append(out, o.Kind)
		}
	}
	return out
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
