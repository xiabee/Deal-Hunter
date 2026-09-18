package scoring

import (
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/keywords"
	"github.com/xiabee/deal-hunter/internal/model"
)

var dict = keywords.Default()

func dealOf(title, summary string) *model.Deal {
	return &model.Deal{Title: title, Summary: summary, URL: "https://example.com/x", Source: "test"}
}

func evaluate(d *model.Deal, trust int, official bool, pub time.Time) Result {
	return Evaluate(Input{
		Deal:           d,
		Offers:         dict.Scan(d.TextBlob()),
		SourceTrust:    trust,
		OfficialDomain: official,
		Now:            time.Now(),
	}, dict)
}

func TestFreshOfficialFreeModelScoresHigh(t *testing.T) {
	d := dealOf("智谱 GLM-5.3-flash 限时免费开放", "官方公告：API 调用 0 元，注册即送额度")
	d.PublishedAt = time.Now().Add(-time.Hour)
	res := evaluate(d, 10, true, d.PublishedAt)
	if res.Reject != "" {
		t.Fatalf("unexpected reject: %s", res.Reject)
	}
	if res.Score < 90 {
		t.Errorf("score = %d, want >= 90 (%v)", res.Score, res.Why)
	}
	if !res.IsFree {
		t.Error("IsFree should be true")
	}
	if res.Category != model.CatAIFree {
		t.Errorf("category = %s, want %s", res.Category, model.CatAIFree)
	}
	if len(res.Why) < 4 {
		t.Errorf("expected an explainable breakdown, got %v", res.Why)
	}
}

func TestScoreIsCappedAt100(t *testing.T) {
	d := dealOf("永久免费：GLM-5.3-flash 全面开放，新用户赠送额度并五折特惠", "免费 免费 赠送 试用 优惠券")
	d.PublishedAt = time.Now().Add(-time.Minute)
	if res := evaluate(d, 10, true, d.PublishedAt); res.Score > 100 {
		t.Errorf("score %d exceeds cap", res.Score)
	}
}

func TestNoiseAndExpiryAreRejected(t *testing.T) {
	noise := dealOf("【招聘】资深后端工程师", "年薪 40w")
	if res := evaluate(noise, 5, false, time.Time{}); res.Reject != "noise" {
		t.Errorf("noise reject = %q, want \"noise\"", res.Reject)
	}
	expired := dealOf("云盘限时免费活动", "活动截止 2020-01-01，之前已失效")
	res := evaluate(expired, 8, false, time.Time{})
	if res.Reject != "expired" {
		t.Fatalf("expired reject = %q, want \"expired\" (%v)", res.Reject, res.Why)
	}
	if res.Expires == nil {
		t.Error("Expires should be parsed before rejection")
	}
}

func TestPlainNewsHasNoSignal(t *testing.T) {
	d := dealOf("本周科技早报", "行业观察与评论，无价格变化")
	res := evaluate(d, 0, false, time.Time{})
	if res.Reject != "low_signal" {
		t.Errorf("reject = %q, want low_signal (score %d, why %v)", res.Reject, res.Score, res.Why)
	}
}

func TestDeepDiscountBeatsShallow(t *testing.T) {
	deep := dealOf("对象存储 80% off 年度特惠", "限时 80% off")
	shallow := dealOf("对象存储 10% off 年度特惠", "限时 10% off")
	a, b := evaluate(deep, 5, false, time.Time{}), evaluate(shallow, 5, false, time.Time{})
	if a.Score <= b.Score {
		t.Errorf("deep discount %d should beat shallow %d", a.Score, b.Score)
	}
	if a.DiscountPct != 80 {
		t.Errorf("DiscountPct = %d, want 80", a.DiscountPct)
	}
	if a.Category != model.CatDiscount {
		t.Errorf("category = %s, want discount", a.Category)
	}
}

func TestStaleItemIsPenalized(t *testing.T) {
	old := dealOf("大模型限时免费活动", "免费开放")
	old.PublishedAt = time.Now().AddDate(0, 0, -90)
	fresh := dealOf("大模型限时免费活动", "免费开放")
	fresh.PublishedAt = time.Now().Add(-time.Hour)
	if evaluate(old, 5, false, old.PublishedAt).Score >= evaluate(fresh, 5, false, fresh.PublishedAt).Score {
		t.Error("a 90 day old item must not score as high as a fresh one")
	}
}

func TestFutureTimestampIsNotPenalized(t *testing.T) {
	// Timezone skew on upstream feeds is common and must not bury a real deal.
	d := dealOf("Qwen3.8-Max 免费开放", "官方公告 0 元")
	d.PublishedAt = time.Now().Add(3 * time.Hour)
	res := evaluate(d, 8, true, d.PublishedAt)
	if res.Reject != "" {
		t.Fatalf("future-dated item rejected: %s", res.Reject)
	}
	if res.Score < 80 {
		t.Errorf("score = %d, want >= 80 (%v)", res.Score, res.Why)
	}
}

func TestExplicitCategoryWins(t *testing.T) {
	d := dealOf("云主机特惠", "首购一折")
	d.Category = model.CatResource
	res := evaluate(d, 5, false, time.Time{})
	if res.Category != model.CatResource {
		t.Errorf("category = %s, want the source-declared value", res.Category)
	}
}

func TestZeroTrustAndNoOffersCannotLeakAlerts(t *testing.T) {
	d := dealOf("普通的行业新闻报道", "内容涉及技术与产品，不涉及价格")
	res := evaluate(d, 0, false, time.Time{})
	if res.Score >= 60 {
		t.Errorf("scoreless item scored %d", res.Score)
	}
}
