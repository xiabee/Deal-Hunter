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

// evaluate scores d as of `now` (zero means the real current time). The clock is a
// parameter because freshness is part of the score: passing the item's own
// PublishedAt here would silently mean "brand new" and no stale test could fail.
func evaluate(d *model.Deal, trust int, official bool, now time.Time) Result {
	return Evaluate(Input{
		Deal:           d,
		Offers:         dict.Scan(d.TextBlob()),
		SourceTrust:    trust,
		OfficialDomain: official,
		Now:            now,
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
	now := time.Now()
	old := dealOf("大模型限时免费活动", "免费开放")
	old.PublishedAt = now.AddDate(0, 0, -90)
	fresh := dealOf("大模型限时免费活动", "免费开放")
	fresh.PublishedAt = now.Add(-time.Hour)
	if evaluate(old, 5, false, now).Score >= evaluate(fresh, 5, false, now).Score {
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

// 开抢时刻必须由评分层交出来，事件提醒才有触发依据。时区只能由调用方给：服务常跑
// UTC，而公告里的「上午10:00开抢」是北京时间，差 8 小时等于每条都提醒错。
func TestEvaluateParsesStartOnlyWhenToldWhichZone(t *testing.T) {
	shanghai := time.FixedZone("CST", 8*3600)
	d := dealOf("关于开展2026年洪城消费券发放的公告",
		"核销期限至2026年10月15日。第一轮9月21日上午10:00开始发放，领完即止。")
	d.PublishedAt = time.Date(2026, 9, 11, 8, 0, 0, 0, shanghai)
	base := Input{Deal: d, Offers: dict.Scan(d.TextBlob()),
		Now: time.Date(2026, 9, 11, 9, 0, 0, 0, shanghai)}

	withZone := base
	withZone.ReaderZone = shanghai
	res := Evaluate(withZone, dict)
	if res.Starts == nil {
		t.Fatal("the start moment in the body was not parsed")
	}
	want := time.Date(2026, 9, 21, 10, 0, 0, 0, shanghai)
	if !res.Starts.Equal(want) {
		t.Errorf("start = %v, want %v", res.Starts, want)
	}

	// 没有时区就没有可靠的解释方式，宁可不给。
	if got := Evaluate(base, dict).Starts; got != nil {
		t.Errorf("must not guess a zone, got %v", got)
	}
}

// 截止日也必须按读者的钟表来摆：服务跑 UTC 时，"核销期限至 10月15日" 若按主机时区
// 解释，等于让一张结束两天的券继续进日报。两个时区必须差出整 8 小时——这条断言不依赖
// 测试主机自己是哪个时区，所以实现退回 time.Local 时一定变红。
func TestEvaluateBuildsDeadlineInTheReadersZone(t *testing.T) {
	utc := time.UTC
	east := time.FixedZone("CST", 8*3600)
	d := dealOf("关于开展2026年洪城消费券发放的公告",
		"核销期限至2026年10月15日。第一轮9月21日上午10:00开始发放，领完即止。")
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, east)

	inEast := Input{Deal: d, Offers: dict.Scan(d.TextBlob()), Now: now, ReaderZone: east}
	inUTC := inEast
	inUTC.ReaderZone = utc
	resEast, resUTC := Evaluate(inEast, dict), Evaluate(inUTC, dict)
	if resEast.Expires == nil || resUTC.Expires == nil {
		t.Fatalf("both zones must yield a deadline: %v / %v", resEast.Expires, resUTC.Expires)
	}
	if got := resEast.Expires.Format("2006-01-02 15:04:05"); got != "2026-10-15 23:59:59" {
		t.Errorf("deadline read in the reader's zone = %s, want 2026-10-15 23:59:59", got)
	}
	if diff := resUTC.Expires.Sub(*resEast.Expires); diff != 8*time.Hour {
		t.Errorf("UTC vs UTC+8 differ by %s, want 8h — Evaluate is not passing the zone down", diff)
	}
}

// —— 新鲜度加分的边界 ——
// 这些台阶决定"三天前的免费公告"和"三十一天的免费公告"在日报里排不排得前面。
// 台阶本身是设计，漂一秒就换档才是 bug；30 天到 31 天之间那段没有加也没有罚，
// 也是有意的平台期，写下来免得被当成漏档。
func TestFreshnessTiersChangeOnlyAtTheirEdges(t *testing.T) {
	now := time.Now()
	score := func(age time.Duration) int {
		d := dealOf("某个模型限时免费", "免费额度")
		d.PublishedAt = now.Add(-age)
		res := evaluate(d, 0, false, now)
		return res.Score
	}
	fresh := score(time.Minute)
	cases := []struct {
		name string
		age  time.Duration
		want int
	}{
		{"正好 24 小时仍算新", 24 * time.Hour, fresh},
		{"24 小时零 1 秒降一档", 24*time.Hour + time.Second, fresh - 4},
		{"正好 72 小时仍是第二档", 72 * time.Hour, fresh - 4},
		{"72 小时零 1 秒再降一档", 72*time.Hour + time.Second, fresh - 8},
		{"正好 7 天仍是第三档", 7 * 24 * time.Hour, fresh - 8},
		{"7 天零 1 秒进入无加成的平台期", 7*24*time.Hour + time.Second, fresh - 12},
		{"30 天整仍是平台期", 30 * 24 * time.Hour, fresh - 12},
		{"30 天零 1 秒开始扣陈旧", 30*24*time.Hour + time.Second, fresh - 24},
		{"时间戳超前（时区偏差）按新算", -time.Hour, fresh},
	}
	for _, c := range cases {
		if got := score(c.age); got != c.want {
			t.Errorf("%s: score = %d, want %d (age %s)", c.name, got, c.want, c.age)
		}
	}
}

// 把所有加分项一起点着，也不能越过 100：读者看到的分数是同一个尺子，
// 插队门槛 90 才有意义。
func TestScoreCannotExceedOneHundred(t *testing.T) {
	d := dealOf("智谱 GLM-5.3-flash 限时免费开放，全场五折",
		"官方公告：免费开放且注册赠送百万 tokens 与代金券，另享五折折扣，API 调用 0 元")
	d.PublishedAt = time.Now().Add(-time.Hour)
	res := evaluate(d, 10, true, d.PublishedAt)
	if res.Reject != "" {
		t.Fatalf("unexpected reject: %s", res.Reject)
	}
	if res.Score < 90 {
		t.Fatalf("the fixture does not stack up (score %d), so the cap assertion would be vacuous", res.Score)
	}
	if res.Score > maxScore {
		t.Errorf("score %d exceeds the cap %d", res.Score, maxScore)
	}
}
