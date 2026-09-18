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
