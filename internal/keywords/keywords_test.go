package keywords

import (
	"strings"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

func TestScanDetectsFreeOfferWithVendorAndProduct(t *testing.T) {
	offers := Default().Scan("智谱 GLM-5.3-flash 限时免费开放，API 调用 0 元")
	if len(offers) == 0 {
		t.Fatal("expected at least one offer")
	}
	var free *model.Offer
	for i := range offers {
		if offers[i].Kind == model.KindFree {
			free = &offers[i]
		}
	}
	if free == nil {
		t.Fatalf("no free offer in %+v", offers)
	}
	if free.Vendor != "智谱AI" {
		t.Errorf("vendor = %q, want 智谱AI", free.Vendor)
	}
	if !strings.Contains(strings.ToUpper(free.Product), "GLM") {
		t.Errorf("product = %q, want a GLM identifier", free.Product)
	}
	if free.Evidence == "" {
		t.Error("evidence snippet must not be empty")
	}
}

func TestScanHonoursWordBoundaries(t *testing.T) {
	if got := Default().Scan("The freedom of FreeBSD users is nice"); len(got) != 0 {
		t.Fatalf("substring matches must not count as offers, got %+v", got)
	}
	got := Default().Scan("Freemium plans, but the API is free")
	var found bool
	for _, o := range got {
		if o.Kind == model.KindFree && o.Matched == "free" {
			found = true
		}
	}
	if !found {
		t.Fatalf("standalone later occurrence missed: %+v", got)
	}
}

func TestScanDetectsCreditCouponAndTrial(t *testing.T) {
	cases := map[model.Kind]string{
		model.KindCredit: "注册即赠送 100 元代金券额度",
		model.KindCoupon: "评论区放出优惠码",
		model.KindTrial:  "新用户可免费体验",
	}
	for kind, text := range cases {
		offers := Default().Scan(text)
		var hit bool
		for _, o := range offers {
			if o.Kind == kind {
				hit = true
			}
		}
		if !hit {
			t.Errorf("kind %s not detected in %q (%+v)", kind, text, offers)
		}
	}
}

func TestDiscountPct(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"全场 5 折", 50},
		{"新品发布，限时 20% off", 20},
		{"100% off 庆祝上线", 100},
		{"没有任何价格信息的普通新闻", 0},
	}
	for _, c := range cases {
		if got := Default().DiscountPct(c.text); got != c.want {
			t.Errorf("DiscountPct(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

func TestVendorsAndProduct(t *testing.T) {
	d := Default()
	v := d.Vendors("DeepSeek 与 智谱 GLM 同时降价")
	if len(v) < 2 {
		t.Fatalf("expected at least 2 vendors, got %v", v)
	}
	for _, want := range []string{"DeepSeek", "智谱AI"} {
		var hit bool
		for _, got := range v {
			if got == want {
				hit = true
			}
		}
		if !hit {
			t.Errorf("vendor %s missing from %v", want, v)
		}
	}
	if p := d.Product("Qwen-3.8-Flash 发布"); !strings.EqualFold(p, "Qwen-3.8-Flash") {
		t.Errorf("Product = %q, want Qwen-3.8-Flash", p)
	}
	if d.IsModelNamed("普通新闻标题") {
		t.Error("plain text must not look like a model name")
	}
}

func TestIsNoise(t *testing.T) {
	d := Default()
	if !d.IsNoise("【招聘】资深后端工程师") {
		t.Error("job posting should be noise")
	}
	if d.IsNoise("智谱 GLM 限时免费") {
		t.Error("a real deal must not be noise")
	}
}

func TestExpiresAt(t *testing.T) {
	d := Default()
	got := d.ExpiresAt("活动截止 2026-10-01，过期不候")
	if got == nil {
		t.Fatal("expected a parsed deadline")
	}
	if got.Year() != 2026 || int(got.Month()) != 10 || got.Day() != 1 {
		t.Errorf("deadline = %s, want 2026-10-01", got.Format("2006-01-02"))
	}
	if d.ExpiresAt("没有时间信息") != nil {
		t.Error("expected no deadline")
	}
	en := d.ExpiresAt("Offer ends on September 30, 2026")
	if en == nil || !en.Equal(time.Date(2026, 9, 30, 23, 59, 59, 0, time.Local)) {
		t.Errorf("english deadline = %v", en)
	}
}

func TestScanEmptyAndDeterministic(t *testing.T) {
	d := Default()
	if len(d.Scan("   ")) != 0 {
		t.Error("blank text yields no offers")
	}
	text := "阿里云 Qwen3.8-Max 限时 5 折，新用户免费试用"
	a, b := d.Scan(text), d.Scan(text)
	if len(a) != len(b) {
		t.Fatalf("scan not deterministic: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("scan differs at %d: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestWindowStaysWithinBounds(t *testing.T) {
	long := strings.Repeat("很长的中文内容", 40) + " 免费 " + strings.Repeat("尾巴", 40)
	offers := Default().Scan(long)
	if len(offers) == 0 {
		t.Fatal("expected an offer in long text")
	}
	if n := len([]rune(offers[0].Evidence)); n > 90 {
		t.Errorf("evidence window too wide: %d runes", n)
	}
}

// 开抢时刻是消费券提醒唯一的前提。写法实测下来很杂：年月日齐全、只写月日、
// 12 小时制带"上午/中午/晚上"、以及全角冒号。锚点（公告发布日）用来补年份。
func TestStartsAtParsesAnnouncementWordings(t *testing.T) {
	anchor := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name, text string
		want       time.Time
	}{
		{"full date and hour", "第一轮自2026年6月18日上午9时开始发放",
			time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)},
		{"month-day borrows the year", "9月21日上午10:00开始发放，领完即止",
			time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)},
		{"noon", "发放时间：8月24日中午12:00",
			time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)},
		{"evening", "2月3日晚上8点开抢",
			time.Date(2026, 2, 3, 20, 0, 0, 0, time.UTC)},
		{"full-width colon", "10月1日10：00起",
			time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)},
		{"bare hour with 时", "每场9时整开始",
			time.Time{}},
	}
	d := Default()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := d.StartsAt(tc.text, anchor)
			if tc.want.IsZero() {
				if got != nil {
					t.Fatalf("must not guess a day from %q, got %v", tc.text, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("nil for %q", tc.text)
			}
			if !got.Equal(tc.want) {
				t.Errorf("%q -> %v, want %v", tc.text, got.UTC(), tc.want)
			}
		})
	}
}

// 猜错开抢时刻会推一条假提醒，那比不提醒糟得多，所以下面这些一律必须返回 nil。
func TestStartsAtRefusesToGuess(t *testing.T) {
	anchor := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	for _, text := range []string{
		"活动期间每日上午9:00发放一定数量，领完即止",
		"暂定近期发放，具体待定",
		"9月20日至9月25日 每日两轮",
		"上午10:00开抢",
		"9月31日10:00",
		"2026年13月2日10:00",
		"满100减30，先到先得",
		"",
	} {
		if got := Default().StartsAt(text, anchor); got != nil {
			t.Errorf("must not guess from %q, got %v", text, *got)
		}
	}
}

// ExpiresAt 与 StartsAt 同族，同样会被 time.Date 的进位骗过：核销期限写成
// "9月31日" 应该判为读不懂，而不是悄悄变成 10 月 1 日 —— 那会让一张早已结束的券
// 在日报里多活一天。
func TestExpiresAtRejectsImpossibleDates(t *testing.T) {
	if got := Default().ExpiresAt("有效期至 2026年9月31日"); got != nil {
		t.Errorf("impossible deadline must not resolve, got %v", *got)
	}
	if got := Default().ExpiresAt("有效期至 2026年10月15日"); got == nil {
		t.Error("a real deadline must still parse")
	}
}

// 消费券公告用「核销期限」表述截止，而现有前置词里没有它。不补上，spec 里
// 「券必须有 expires_at 才进日报」这条规则会把所有券一律挡在门外。
func TestExpiresAtReadsVoucherRedemptionDeadline(t *testing.T) {
	for _, text := range []string{"核销期限至2026年10月15日", "使用期限：2026年11月3日"} {
		if got := Default().ExpiresAt(text); got == nil {
			t.Errorf("no deadline parsed from %q", text)
		}
	}
}

// 这些句子是从当期真实公告里抄下来的写法（含 24:00 这种当小时用的截止表达），
// 不是我们臆造的语法。真实文本才是解析器的考题。
func TestStartsAtHandlesRealAnnouncementPhrasing(t *testing.T) {
	anchor := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	for _, text := range []string{
		"南昌赣超消费券领券时间为9月19日9:00至9月26日24:00，使用有效期至10月26日，逾期失效不予补发。",
		"领券窗口为9月19日上午9:00开放，至9月26日24:00关闭，领取后使用有效期至10月26日24:00。",
		"本次赣超体育消费券领券时间为2026年9月19日9:00起，至9月26日24:00截止，共计8天。",
	} {
		got := Default().StartsAt(text, anchor)
		if got == nil {
			t.Errorf("no start parsed from real wording: %q", text)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("%q -> %v, want %v", text, got.UTC(), want)
		}
	}

	// 「24:00」是截止表达，不是开抢时刻：这里必须拒绝而不是给出次日的 00:00。
	if got := Default().StartsAt("使用有效期至2026年10月26日24:00", anchor); got != nil {
		t.Errorf("a 24:00 deadline must not become a start, got %v", got)
	}
}
