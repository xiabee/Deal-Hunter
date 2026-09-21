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

// anchorAt builds the clock a finding carries into ExpiresAt / StartsAt: any
// instant inside the zone under test.
func anchorAt(loc *time.Location) time.Time {
	return time.Date(2026, 6, 15, 9, 0, 0, 0, loc)
}

func TestExpiresAt(t *testing.T) {
	d := Default()
	got := d.ExpiresAt("活动截止 2026-10-01，过期不候", time.Time{})
	if got == nil {
		t.Fatal("expected a parsed deadline")
	}
	if got.Year() != 2026 || int(got.Month()) != 10 || got.Day() != 1 {
		t.Errorf("deadline = %s, want 2026-10-01", got.Format("2006-01-02"))
	}
	if d.ExpiresAt("没有时间信息", time.Time{}) != nil {
		t.Error("expected no deadline")
	}
	en := d.ExpiresAt("Offer ends on September 30, 2026", time.Time{})
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

// 服务常年跑在 UTC 主机上，而公告里的"截止 9月26日"是读者时区的钟表。用
// time.Local 构造会把 23:59 变成 UTC 的 23:59，也就是北京的次日 07:59 —— 一张
// 已经结束的券会在日报里多活 8 小时。所以两个时区必须给出相差整 8 小时的时刻，
// 这条断言在任何主机上都成立（若实现改用 time.Local，两者会相等）。
func TestExpiresAtBuildsDeadlineInTheReadersZone(t *testing.T) {
	const text = "活动截止：2026年9月26日"
	east := time.FixedZone("UTC+8", 8*3600)
	inUTC := Default().ExpiresAt(text, anchorAt(time.UTC))
	inEast := Default().ExpiresAt(text, anchorAt(east))
	if inUTC == nil || inEast == nil {
		t.Fatalf("both zones must resolve the deadline: %v / %v", inUTC, inEast)
	}
	if got := inEast.Format("2006-01-02 15:04:05"); got != "2026-09-26 23:59:59" {
		t.Errorf("deadline in the reader's own zone = %s, want 2026-09-26 23:59:59", got)
	}
	if diff := inUTC.Sub(*inEast); diff != 8*time.Hour {
		t.Errorf("same wall clock in UTC vs UTC+8 differs by %s, want 8h (the zone is being ignored)", diff)
	}
}

// ExpiresAt 与 StartsAt 同族，同样会被 time.Date 的进位骗过：核销期限写成
// "9月31日" 应该判为读不懂，而不是悄悄变成 10 月 1 日 —— 那会让一张早已结束的券
// 在日报里多活一天。
func TestExpiresAtRejectsImpossibleDates(t *testing.T) {
	if got := Default().ExpiresAt("有效期至 2026年9月31日", time.Time{}); got != nil {
		t.Errorf("impossible deadline must not resolve, got %v", *got)
	}
	if got := Default().ExpiresAt("有效期至 2026年10月15日", time.Time{}); got == nil {
		t.Error("a real deadline must still parse")
	}
}

// 消费券公告用「核销期限」表述截止，而现有前置词里没有它。不补上，spec 里
// 「券必须有 expires_at 才进日报」这条规则会把所有券一律挡在门外。
func TestExpiresAtReadsVoucherRedemptionDeadline(t *testing.T) {
	for _, text := range []string{"核销期限至2026年10月15日", "使用期限：2026年11月3日"} {
		if got := Default().ExpiresAt(text, time.Time{}); got == nil {
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

// 生产里真实丢过一条：linux.do 的「新加坡时间：2026年9月18日10:00至2026年9月30日23:59」
// 是"起止区间"写法，没有"截止/有效期至"这类前置词。前半句被 starts_at 认走，后半句
// 没人认，于是这条限时事件在日报口径里变成"没说何时结束"，整条被丢掉 —— 一份明明还
// 有十天有效期、89 分的免费额度，读者在日报里看不到。
func TestExpiresAtReadsExplicitDateRange(t *testing.T) {
	text := "新加坡时间：2026年9月18日10:00至2026年9月30日23:59"
	zone := time.FixedZone("CST", 8*3600)
	got := Default().ExpiresAt(text, anchorAt(zone))
	if got == nil {
		t.Fatalf("the end of an explicit range is a deadline: %q", text)
	}
	if s := got.Format("2006-01-02 15:04"); s != "2026-09-30 23:59" {
		t.Errorf("range end = %s, want 2026-09-30 23:59", s)
	}

	// 没有小时的时候仍按"当天过完"算，与既有规则一致。
	dayOnly := Default().ExpiresAt("活动时间：2026年9月1日至2026年9月20日", anchorAt(zone))
	if dayOnly == nil {
		t.Fatal("a day-only range must still yield a deadline")
	}
	if s := dayOnly.Format("2006-01-02 15:04"); s != "2026-09-20 23:59" {
		t.Errorf("day-only range end = %s, want 2026-09-20 23:59", s)
	}
}

// 从生产历史库里回灌出来的两种真实写法（不是编的语法）：
//
//	"需在 2026 年 12 月 31 日前注册"            —— 年在前、"前"在后，现有规则只认 ISO 横线写法
//	"限时免费开放至2026年8月31日" / "使用期限直接顺延至2026年8月31日" / "活动持续至 2026 年 9 月 30 日"
//
// —— 前置词与日期之间夹着动词，且"至"跟在动词后，原规则的 `\s*[:：至]?\s*` 跨不过去。
// 这两类年份都写明白了，不属于"猜日期"，读不出来就是白丢一条有期限的羊毛。
func TestExpiresAtReadsRealPhrasingFromTheCorpus(t *testing.T) {
	east := time.FixedZone("CST", 8*3600)
	cases := []struct {
		text, want string
	}{
		{"活动需在各平台完成认证，需在 2026 年 12 月 31 日前注册", "2026-12-31 23:59"},
		{"现通过 CodeBuddy 平台限时免费开放至2026年8月31日，用户可零成本体验", "2026-08-31 23:59"},
		{"将混元Hy3大模型免费调用权益再度延期，免费使用期限直接顺延至2026年8月31日", "2026-08-31 23:59"},
		{"模型在 Qoder 平台限时免费开放，活动持续至 2026 年 9 月 30 日 23:59:59（UTC+8）", "2026-09-30 23:59"},
	}
	for _, c := range cases {
		got := Default().ExpiresAt(c.text, anchorAt(east))
		if got == nil {
			t.Errorf("no deadline read from %q", c.text)
			continue
		}
		if s := got.Format("2006-01-02 15:04"); s != c.want {
			t.Errorf("%q -> %s, want %s", c.text, s, c.want)
		}
	}
}

// 反过来：这些同样来自真实语料，但不该被当成截止 —— 它们是文章发布日/数据时效，
// 或者是"截至某月"这种没有日的说法。把它们读成截止会让一条早已过期的羊毛继续占日报。
func TestExpiresStillRefusesPublicationDates(t *testing.T) {
	east := time.FixedZone("CST", 8*3600)
	for _, text := range []string{
		"2026 年 9 月 Ai 免费额度日历｜限时羊毛、长期额度与三个坑",
		"本文基于 2025-09-02 的最新官网与权威测评，一口气梳理 6 款",
		"2026 年免费 AI 大模型 API 清单：官方文档核实版（截至 2026-08）",
		"数据时效：2026 年 7 月，具体以各平台官网为准",
		"网站编辑 2026-07-12 11:40:02 腾讯云代码助手免费额度",
	} {
		if got := Default().ExpiresAt(text, anchorAt(east)); got != nil {
			t.Errorf("must not read a deadline out of %q, got %s", text, got.Format(time.RFC3339))
		}
	}
}

// 不写年份的截止（"限时免费至9月10日"、"截止至1月5日"）是语料里剩下的最大一类。
// 锚点是发布日期：它的钟决定时刻怎么解释，它的年决定"9月10日"是哪一年。
// 规则刻意保守：同一年的读法还没过期才接受；只有临近年底时才允许跨年滚一年，
// 且滚完必须在锚点之后 45 天以内 —— 9 月的文章里一个不写年的"8月28日"，
// 意思是"已经过了"，不是"明年8月28日"。
func TestExpiresAtResolvesYearlessDeadlinesFromTheAnchor(t *testing.T) {
	east := time.FixedZone("CST", 8*3600)

	cases := []struct {
		name   string
		anchor time.Time
		text   string
		want   string
		expect bool
	}{
		{
			name:   "同年仍在未来",
			anchor: time.Date(2026, 8, 4, 9, 0, 0, 0, east), // 文章发布于 8 月 4 日
			text:   "腾讯Hy3限时免费体验，CodeBuddy、WorkBuddy同步上线，限时免费至9月10日，支持API调用",
			want:   "2026-09-10 23:59", expect: true,
		},
		{
			name:   "九月看到八月二十八，是已经过了",
			anchor: time.Date(2026, 9, 21, 9, 0, 0, 0, east),
			text:   "CDN流量包免费领取活动，分享赢速干T恤，截止至8月28日。",
			expect: false,
		},
		{
			name:   "八月初看到八月二十八，还来得及",
			anchor: time.Date(2026, 8, 4, 9, 0, 0, 0, east),
			text:   "CDN流量包免费领取活动，分享赢速干T恤，截止至8月28日。",
			want:   "2026-08-28 23:59", expect: true,
		},
		{
			name:   "年底附近允许滚一年",
			anchor: time.Date(2026, 12, 30, 9, 0, 0, 0, east),
			text:   "本期限量领取，报名从速，截止至1月5日。",
			want:   "2027-01-05 23:59", expect: true,
		},
		{
			name:   "滚一年太远就不读",
			anchor: time.Date(2026, 12, 30, 9, 0, 0, 0, east),
			text:   "年度盘点里的长期额度，截止至6月1日。",
			expect: false,
		},
	}
	for _, c := range cases {
		got := Default().ExpiresAt(c.text, c.anchor)
		if !c.expect {
			if got != nil {
				t.Errorf("%s: must stay unread, got %s", c.name, got.Format(time.RFC3339))
			}
			continue
		}
		if got == nil {
			t.Errorf("%s: no deadline read from %q", c.name, c.text)
			continue
		}
		if s := got.In(east).Format("2006-01-02 15:04"); s != c.want {
			t.Errorf("%s: %s, want %s", c.name, s, c.want)
		}
	}
}

// 年份明写的老规矩不变：锚点只是提供钟点，不参与补年。
func TestYearlessRuleDoesNotTouchExplicitYears(t *testing.T) {
	east := time.FixedZone("CST", 8*3600)
	late := time.Date(2026, 12, 30, 9, 0, 0, 0, east)
	got := Default().ExpiresAt("核销期限至2026年10月15日", late)
	if got == nil {
		t.Fatal("an explicit year must still parse regardless of the anchor")
	}
	if s := got.In(east).Format("2006-01-02"); s != "2026-10-15" {
		t.Errorf("explicit year rewritten by the anchor: %s", s)
	}
	if Default().ExpiresAt("没有时间信息", late) != nil {
		t.Error("still no deadline in text that has none")
	}
}

// 语料里的真实一句："截止到9月20日凌晨5:40，我翻了好久论坛"。日期读对了、时刻丢掉，
// 这张券就在日报里多活 18 小时 —— 既然原话写了点，就没有理由按"当天过完"算。
// 反过来，没写点的（核销期限至2026年10月15日）继续按 23:59:59 收尾。
func TestExpiresAtHonoursAStatedClockTime(t *testing.T) {
	east := time.FixedZone("CST", 8*3600)
	published := time.Date(2026, 9, 19, 10, 0, 0, 0, east)
	cases := []struct {
		text, want string
	}{
		{"社区里有人晒单，试用活动截止到9月20日凌晨5:40，我翻了好久论坛", "2026-09-20 05:40"},
		{"本期活动截至 2026 年 9 月 30 日 23:59:59（UTC+8）", "2026-09-30 23:59"},
		{"限时免费开放至2026年8月31日18:00，之后恢复计价", "2026-08-31 18:00"},
		{"核销期限至2026年10月15日，逾期作废", "2026-10-15 23:59"},
	}
	for _, c := range cases {
		got := Default().ExpiresAt(c.text, published)
		if got == nil {
			t.Errorf("no deadline read from %q", c.text)
			continue
		}
		if s := got.In(east).Format("2006-01-02 15:04"); s != c.want {
			t.Errorf("%q -> %s, want %s", c.text, s, c.want)
		}
	}

	// 逗号之后是另一句话："10:00 开抢"不能被借来当截止时刻。
	borrowed := Default().ExpiresAt("本期券限时免费至9月25日，10:00 开抢，先到先得", published)
	if borrowed == nil {
		t.Fatal("the date itself must still be readable")
	}
	if s := borrowed.In(east).Format("2006-01-02 15:04"); s != "2026-09-25 23:59" {
		t.Errorf("borrowed a clock from the next clause: %s", s)
	}
}
