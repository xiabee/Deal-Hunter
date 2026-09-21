// Package scoring turns a raw finding plus keyword evidence into a 0-100
// value score with an explainable breakdown.
package scoring

import (
	"fmt"
	"time"

	"github.com/xiabee/deal-hunter/internal/keywords"
	"github.com/xiabee/deal-hunter/internal/model"
)

// Input carries everything needed to judge one deal.
type Input struct {
	Deal           *model.Deal
	Offers         []model.Offer
	SourceTrust    int // 0-10, from config
	OfficialDomain bool
	Now            time.Time
	// ReaderZone is the zone an announcement's clock belongs to — both the opening
	// moment and the deadline are wall clocks the reader reads. nil falls back to
	// the host's zone and disables start parsing.
	ReaderZone *time.Location
}

// Result is the scoring verdict. Reject is non-empty when the deal must not
// reach the user at all.
type Result struct {
	Score       int
	Why         []string
	Category    string
	IsFree      bool
	DiscountPct int
	Expires     *time.Time
	Starts      *time.Time
	Vendors     []string
	Product     string
	Reject      string
}

// Weights and thresholds used by Evaluate.
const (
	baseFree     = 45
	baseCredit   = 30
	baseDiscount = 24
	baseTrial    = 20
	baseCoupon   = 16
	baseUnknown  = 4

	bonusVendorKnown  = 10
	bonusModelNamed   = 12
	bonusFresh24h     = 12
	bonusFresh72h     = 8
	bonusFresh7d      = 4
	bonusOfficial     = 10
	bonusDeepDiscount = 12
	bonusMidDiscount  = 8
	bonusShallow      = 4
	bonusMultiOffer   = 4

	penaltyStale = 12
	penaltyNoise = 60
	maxScore     = 100
)

// Evaluate scores the deal. It mutates nothing outside the returned Result.
func Evaluate(in Input, dict *keywords.Dict) Result {
	text := in.Deal.TextBlob()
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	res := Result{Category: model.CatUnknown}
	if dict.IsNoise(text) {
		res.Reject = "noise"
		res.Why = append(res.Why, fmt.Sprintf("-%d 命中噪音词", penaltyNoise))
		return res
	}
	if res.DiscountPct = dict.DiscountPct(text); res.DiscountPct > 0 {
		res.Score += discountBonus(res.DiscountPct)
		res.Why = append(res.Why, fmt.Sprintf("+%d 折扣 %d%%", discountBonus(res.DiscountPct), res.DiscountPct))
	}
	// An announcement states its times in the reader's zone, and a date written
	// without a year belongs to the year the announcement was published — only the
	// caller knows either. A service running in UTC must still remind for 10:00
	// Beijing and read "至9月10日" as this year's.
	anchor := now
	if !in.Deal.PublishedAt.IsZero() {
		anchor = in.Deal.PublishedAt
	}
	if in.ReaderZone != nil {
		anchor = anchor.In(in.ReaderZone)
	}
	res.Expires = dict.ExpiresAt(text, anchor)
	if res.Expires != nil && res.Expires.Before(now) {
		res.Reject = "expired"
		res.Why = append(res.Why, "活动已于 "+res.Expires.Format("2006-01-02")+" 截止")
		return res
	}
	if in.ReaderZone != nil {
		res.Starts = dict.StartsAt(text, anchor)
	}

	seenKind := map[model.Kind]bool{}
	for _, o := range in.Offers {
		seenKind[o.Kind] = true
	}
	if seenKind[model.KindFree] {
		res.IsFree = true
		res.Score += baseFree
		res.Why = append(res.Why, fmt.Sprintf("+%d 免费类 offer", baseFree))
	}
	for _, k := range []struct {
		kind model.Kind
		val  int
	}{
		{model.KindCredit, baseCredit}, {model.KindDiscount, baseDiscount},
		{model.KindTrial, baseTrial}, {model.KindCoupon, baseCoupon},
	} {
		if seenKind[k.kind] && !seenKind[model.KindFree] {
			res.Score += k.val
			res.Why = append(res.Why, fmt.Sprintf("+%d %s 类 offer", k.val, k.kind))
			break
		}
	}
	if !res.IsFree && res.DiscountPct > 0 && res.Score < baseDiscount {
		res.Score += baseDiscount - res.Score
		res.Why = append(res.Why, fmt.Sprintf("+%d 明确降价", baseDiscount))
	}

	res.Vendors = dict.Vendors(text)
	res.Product = dict.Product(text)
	if len(res.Vendors) > 0 {
		res.Score += bonusVendorKnown
		res.Why = append(res.Why, fmt.Sprintf("+%d 已知厂商 %s", bonusVendorKnown, res.Vendors[0]))
	}
	if res.Product != "" {
		res.Score += bonusModelNamed
		res.Why = append(res.Why, fmt.Sprintf("+%d 指名产品/模型 %s", bonusModelNamed, res.Product))
	}
	if in.OfficialDomain {
		res.Score += bonusOfficial
		res.Why = append(res.Why, fmt.Sprintf("+%d 厂商官方源", bonusOfficial))
	}
	if trust := clamp(in.SourceTrust, 0, 10); trust > 0 {
		res.Score += trust
		res.Why = append(res.Why, fmt.Sprintf("+%d 信息源可信度", trust))
	}
	if len(in.Offers) > 1 {
		res.Score += bonusMultiOffer
		res.Why = append(res.Why, fmt.Sprintf("+%d 多重优惠", bonusMultiOffer))
	}

	switch {
	case res.DiscountPct >= 50:
		res.Category = model.CatDiscount
	case res.IsFree && res.Product != "":
		res.Category = model.CatAIFree
	case res.IsFree:
		res.Category = model.CatAIFree
	case len(in.Offers) > 0:
		res.Category = model.CatDiscount
	}
	if in.Deal.Category != "" && in.Deal.Category != model.CatUnknown {
		res.Category = in.Deal.Category
	}

	age := time.Duration(0)
	hasAge := false
	if pub := in.Deal.PublishedAt; !pub.IsZero() {
		age = now.Sub(pub)
		hasAge = true
	}
	if hasAge {
		switch {
		case age < 0:
			// Future-dated (timezone skew): treat as fresh, never penalize.
			res.Score += bonusFresh24h
			res.Why = append(res.Why, fmt.Sprintf("+%d 时间戳超前按新算", bonusFresh24h))
		case age <= 24*time.Hour:
			res.Score += bonusFresh24h
			res.Why = append(res.Why, fmt.Sprintf("+%d 24 小时内", bonusFresh24h))
		case age <= 72*time.Hour:
			res.Score += bonusFresh72h
			res.Why = append(res.Why, fmt.Sprintf("+%d 3 天内", bonusFresh72h))
		case age <= 7*24*time.Hour:
			res.Score += bonusFresh7d
			res.Why = append(res.Why, fmt.Sprintf("+%d 7 天内", bonusFresh7d))
		case age > 30*24*time.Hour:
			res.Score -= penaltyStale
			res.Why = append(res.Why, fmt.Sprintf("-%d 超过 30 天", penaltyStale))
		}
	}

	res.Score = clamp(res.Score, 0, maxScore)
	if res.Score == 0 {
		res.Reject = "low_signal"
	}
	return res
}

func discountPct(pct int) int { return pct }

func discountBonus(pct int) int {
	switch {
	case pct >= 50:
		return bonusDeepDiscount
	case pct >= 30:
		return bonusMidDiscount
	case pct >= 10:
		return bonusShallow
	default:
		return 0
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
