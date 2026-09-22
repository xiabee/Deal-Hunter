package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

func deal(title, url string, score int) *model.Deal {
	return &model.Deal{
		Title: title, URL: url, Score: score,
		Source: "unit-test", Category: model.CatAIFree,
		DiscoveredAt: time.Now().UTC().Truncate(time.Second),
	}
}

func TestOpenRoundTripAndDedup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d := deal("智谱 GLM-5.3-flash 限时免费", "https://example.com/a", 88)
	if s.Seen(d.Fingerprint) {
		t.Fatal("fresh store must not know the item")
	}
	if err := s.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !s.Seen(d.Fingerprint) {
		t.Fatal("item should be seen after Save")
	}
	if err := s.Save(deal("same", "https://example.com/a", 10)); err != nil {
		t.Fatalf("Save dup: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if !reopened.Seen(d.Fingerprint) {
		t.Fatal("dedup state must survive a restart")
	}
	if got := reopened.Recent(10); len(got) != 1 {
		t.Fatalf("expected 1 collapsed deal, got %d", len(got))
	}
}

func TestTitleIndexFoldsRepostsOutOfTheBriefing(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	first := &model.Deal{URL: "https://linux.do/t/1", Title: "[分享创造] 做了一个家用 WMS", Score: 71, IsFree: true, DiscoveredAt: time.Now().UTC()}
	repost := &model.Deal{URL: "https://www.v2ex.com/t/2", Title: "做了一个家用 WMS", Score: 68, IsFree: true, DiscoveredAt: time.Now().UTC()}
	if err := st.Save(first); err != nil {
		t.Fatal(err)
	}
	if fp, ok := st.TitleClash(first.Title, first.Fingerprint); ok {
		t.Fatalf("the first finding must not clash with itself: %s", fp)
	}
	fp, ok := st.TitleClash(repost.Title, repost.Fingerprint)
	if !ok || fp != first.Fingerprint {
		t.Fatalf("repost should clash with the first finding, got %q,%v", fp, ok)
	}

	// A repeat is still recorded, but tagged, and never reaches the briefing.
	repost.Meta = map[string]string{"dup_of": first.Fingerprint}
	if err := st.Save(repost); err != nil {
		t.Fatal(err)
	}
	live := st.Live(50, time.Now().UTC(), 0)
	if len(live) != 1 || live[0].Fingerprint != first.Fingerprint {
		t.Errorf("only the first finding should be reported, got %+v", live)
	}
	if got := st.Stats().Duplicates; got != 1 {
		t.Errorf("Duplicates = %d, want 1", got)
	}

	// The index is rebuilt from disk, so a restart keeps the fold.
	dir := st.Dir()
	st.Close()
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got, ok := again.TitleClash(repost.Title, "other"); !ok || got != first.Fingerprint {
		t.Errorf("title index lost after reopen: %q,%v", got, ok)
	}
	if got := again.Stats().Duplicates; got != 1 {
		t.Errorf("Duplicates after reopen = %d, want 1", got)
	}
}

// Records written before the fold existed carry no dup_of tag; they must still
// be folded when read, or the list keeps showing the same announcement twice
// until those rows age out on their own.
func TestRepostsWithoutTagsAreFoldedWhenLoaded(t *testing.T) {
	dir := t.TempDir()
	rows := []string{
		`{"fingerprint":"a1","url":"https://linux.do/t/1","title":"智谱 GLM-5.3-flash 限时免费开放","source":"a","score":80,"is_free":true,"discovered_at":"2026-09-19T01:00:00Z"}`,
		`{"fingerprint":"b2","url":"https://www.v2ex.com/t/2","title":"【公告】智谱 GLM-5.3-flash 限时免费开放！","source":"b","score":74,"is_free":true,"discovered_at":"2026-09-19T02:00:00Z"}`,
	}
	if err := os.WriteFile(filepath.Join(dir, "deals.jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got := map[string]model.Deal{}
	for _, d := range s.Recent(10) {
		got[d.Fingerprint] = d
	}
	if len(got) != 2 {
		t.Fatalf("both rows should load, got %d", len(got))
	}
	if got["b2"].Meta["dup_of"] != "a1" {
		t.Errorf("the later repost should be folded to a1, got %q", got["b2"].Meta["dup_of"])
	}
	if got["a1"].Meta["dup_of"] != "" {
		t.Error("the first finding must stay visible")
	}
	if st := s.Stats(); st.Duplicates != 1 {
		t.Errorf("Duplicates = %d, want 1", st.Duplicates)
	}
	for _, d := range s.Live(50, time.Now().UTC(), 0) {
		if d.Fingerprint == "b2" {
			t.Error("a folded legacy repost must not reach the briefing")
		}
	}
}

// The morning report must show what can still be claimed today: no expired
// offers, no repeats, best first.
func TestLiveSkipsExpiredRepeatsAndRanksByScore(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()

	mk := func(fp, url, title string, score int, meta map[string]string) *model.Deal {
		return &model.Deal{Fingerprint: fp, URL: url, Title: title, Score: score,
			IsFree: true, DiscoveredAt: now.Add(-48 * time.Hour), Meta: meta}
	}
	fresh := mk("f1", "https://a.test/free", "GLM 免费额度", 88, map[string]string{})
	expired := mk("e1", "https://b.test/free", "上周的活动", 90,
		map[string]string{"expires_at": now.Add(-24 * time.Hour).Format(time.RFC3339)})
	untilTomorrow := mk("u1", "https://c.test/free", "明天截止", 60,
		map[string]string{"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339)})
	repeat := mk("r1", "https://d.test/free", "GLM 免费额度", 80, map[string]string{"dup_of": "f1"})
	low := mk("l1", "https://e.test/free", "不值一提", 20, map[string]string{})
	noOffer := &model.Deal{Fingerprint: "n1", URL: "https://f.test/news", Title: "一篇讲价格的文章",
		Score: 77, DiscoveredAt: now, Offers: []model.Offer{{Kind: model.KindUnknown}}}

	for _, d := range []*model.Deal{fresh, expired, untilTomorrow, repeat, low, noOffer} {
		if err := st.Save(d); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, d := range st.Live(45, now, 10) {
		got = append(got, d.Fingerprint)
	}
	// e1 expired, r1 is a repeat, l1 scored too low, n1 is news rather than an offer.
	want := []string{"f1", "u1"}
	if len(got) != len(want) {
		t.Fatalf("Live() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Live()[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	if len(st.Live(45, now, 1)) != 1 {
		t.Error("limit must truncate the ranking")
	}
}

func TestTornLastLineIsIgnored(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(deal("good", "https://example.com/good", 70)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Simulate a crash mid-write.
	f, _ := os.OpenFile(filepath.Join(dir, "deals.jsonl"), os.O_APPEND|os.O_WRONLY, 0o640)
	f.WriteString(`{"fingerprint":"broken","title":"cut off`)
	f.Close()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("store must survive a torn line: %v", err)
	}
	defer reopened.Close()
	if got := reopened.Recent(10); len(got) != 1 || got[0].Title != "good" {
		t.Fatalf("unexpected recovery: %+v", got)
	}
}

func TestStateCursorRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok := s.GetState("missing"); ok {
		t.Fatal("unknown key should be absent")
	}
	ids := []string{"a:free", "b:free"}
	if err := s.PutState("cursor", ids); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	b, ok := s.GetState("cursor")
	if !ok {
		t.Fatal("key should exist")
	}
	if string(b) != `["a:free","b:free"]` {
		t.Fatalf("unexpected payload: %s", b)
	}
}

// The breakthrough channel is the only thing that marks a finding delivered, and
// a day's budget is cheap next to reporting the same offer twice. So the flag has
// to survive a restart, not just live in memory.
func TestMarkPushedSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	high := deal("高价值羊毛", "https://example.com/high", 90)
	low := deal("小羊毛", "https://example.com/low", 50)
	for _, d := range []*model.Deal{high, low} {
		if err := s.Save(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkPushed(high.Fingerprint); err != nil {
		t.Fatalf("MarkPushed: %v", err)
	}
	if got := s.Stats().PushedSeen; got != 1 {
		t.Fatalf("PushedSeen = %d, want 1", got)
	}
	s.Close()

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var pushed, plain int
	for _, d := range again.Recent(10) {
		switch d.Meta["pushed"] {
		case "true":
			pushed++
			if d.Fingerprint != high.Fingerprint {
				t.Errorf("the wrong record carries the pushed flag: %+v", d)
			}
		default:
			plain++
		}
	}
	if pushed != 1 || plain != 1 {
		t.Errorf("after reopen pushed=%d plain=%d, want 1 and 1", pushed, plain)
	}
	if got := again.Stats().PushedSeen; got != 1 {
		t.Errorf("PushedSeen after reopen = %d, want 1", got)
	}
}

func TestCompactCollapsesAndKeepsLatest(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d1 := deal("one", "https://example.com/1", 60)
	d2 := deal("two", "https://example.com/2", 70)
	for _, d := range []*model.Deal{d1, d2, d1} {
		if err := s.Save(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkPushed(d1.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(30); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := s.Save(deal("three", "https://example.com/3", 80)); err != nil {
		t.Fatalf("Save after compact must still work: %v", err)
	}
	var kept bool
	for _, d := range s.Recent(10) {
		if d.Fingerprint == d1.Fingerprint && d.Meta["pushed"] == "true" {
			kept = true
		}
	}
	if !kept {
		t.Error("compact must preserve the pushed marker")
	}
	if got := len(s.Recent(10)); got != 3 {
		t.Errorf("expected 3 deals after compact, got %d", got)
	}
	if st := s.Stats(); st.DealsSeen != 3 {
		t.Errorf("stats DealsSeen = %d, want 3", st.DealsSeen)
	}
}

// 官方入口判定的缓存键按 fingerprint 存，读的时候判 7 天 TTL，却没人删：Compact 把行丢掉
// 之后那些键成了孤儿，而 state.json 每写一次就整体重写一遍。生产 4 天攒了 64 个。
func TestCompactDropsCacheKeysOfDroppedRows(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	old := deal("很久以前的低分发现", "https://example.com/old", 40)
	old.DiscoveredAt = time.Now().UTC().AddDate(0, 0, -100)
	fresh := deal("昨天的发现", "https://example.com/new", 80)
	for _, d := range []*model.Deal{old, fresh} {
		if err := s.Save(d); err != nil {
			t.Fatal(err)
		}
	}
	// 盘点通道的旧键也是同样处境：没人读、没人清。
	for k, v := range map[string]any{
		"official:deal:" + old.Fingerprint:   map[string]string{"kind": "vendor_entry"},
		"official:deal:" + fresh.Fingerprint: map[string]string{"kind": "third_party"},
		"official:verify:https://x/pricing":  map[string]string{"kind": "ok"},
		"digest:last_sent":                   time.Now().UTC().Format(time.RFC3339),
		"search:cursor:search-ai-free-cn":    146,
		"daily:last_sent":                    time.Now().UTC().Format(time.RFC3339),
	} {
		if err := s.PutState(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Compact(30); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if _, ok := s.GetState("official:deal:" + old.Fingerprint); ok {
		t.Error("the verdict of a dropped row must go with it")
	}
	if _, ok := s.GetState("official:deal:" + fresh.Fingerprint); !ok {
		t.Error("a verdict for a surviving row must stay")
	}
	if _, ok := s.GetState("official:verify:https://x/pricing"); !ok {
		t.Error("verify results are shared across rows and outlive any single one")
	}
	if _, ok := s.GetState("digest:last_sent"); ok {
		t.Error("the retired 盘点通道 key has no reader")
	}
	for _, k := range []string{"search:cursor:search-ai-free-cn", "daily:last_sent"} {
		if _, ok := s.GetState(k); !ok {
			t.Errorf("%s must survive compaction", k)
		}
	}

	// 重开一次才算证明删除落到了盘上：只改内存的话上面每一条也会是绿的。
	dir := s.Dir()
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, ok := reopened.GetState("official:deal:" + old.Fingerprint); ok {
		t.Error("the pruned key came back after a reopen — the state file was not rewritten")
	}
	if _, ok := reopened.GetState("official:deal:" + fresh.Fingerprint); !ok {
		t.Error("the surviving verdict must still be on disk")
	}
}

// 服务进程常驻，运维偶尔在同一份数据目录上跑 CLI —— 两边各持一份完整的状态图。
// 若 PutState 只写自己那份，后刷盘的一方就把对方刚写的键抹了（生产真实丢过
// maint:last_compact，doctor 因此谎报"从未压缩过"）。
func TestConcurrentStoresDoNotEraceEachOthersKeys(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := a.PutState("key:a", "from the first handle"); err != nil {
		t.Fatal(err)
	}
	if err := b.PutState("key:b", "from the second handle"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.GetState("key:a"); !ok {
		t.Error("the second handle's write erased the first one's key")
	}
	if _, ok := b.GetState("key:a"); !ok {
		t.Error("the second handle should see what the first one wrote")
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, k := range []string{"key:a", "key:b"} {
		if _, ok := reopened.GetState(k); !ok {
			t.Errorf("%s is missing on disk after both writes", k)
		}
	}

	// 合并不能反过来踩掉本次要写的值：两边都有同一个键时，以这次写入为准
	// （游标只会往前走，用磁盘上的旧值覆盖新值等于退回重抓）。
	if err := b.PutState("dup:key", "the older write"); err != nil {
		t.Fatal(err)
	}
	if err := a.PutState("dup:key", "the newer write"); err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	blob, ok := fresh.GetState("dup:key")
	if !ok || string(blob) != `"the newer write"` {
		t.Errorf("the later write should win, got %q (ok=%v)", string(blob), ok)
	}

	// 陈旧内存不许赢磁盘：a 手里还留着它自己早先写过的值，而 b 已经把它更新过一轮。
	// 以磁盘为底才对 —— 反过来（只补自己缺的键）会让 a 下次写任何键时把旧值刷回去。
	if err := a.PutState("stale:key", "from a, then superseded"); err != nil {
		t.Fatal(err)
	}
	if err := b.PutState("stale:key", "from b, the current value"); err != nil {
		t.Fatal(err)
	}
	if err := a.PutState("something:else", "an unrelated write"); err != nil {
		t.Fatal(err)
	}
	after, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	if v, _ := after.GetState("stale:key"); string(v) != `"from b, the current value"` {
		t.Errorf("a's stale copy rolled the key back: %q", string(v))
	}
}

// 退役通道的键没有任何读者，但只要它还在某个进程的内存里，那次写入就会把它带回磁盘：
// 生产上 `dealhunter compact` 剪掉 digest:last_sent 之后，服务下一次写游标又把它写回来了。
// 所以在装载时就丢掉它，剪枝才真正有意义。
func TestOpenDropsRetiredStateNamespaces(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"digest:last_sent":"2026-09-20T01:25:57Z","search:cursor:search-ai-free-cn":146,"daily:last_sent":"2026-09-22T01:03:23Z"}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok := s.GetState("digest:last_sent"); ok {
		t.Error("the retired digest namespace should be gone on load")
	}
	for _, k := range []string{"search:cursor:search-ai-free-cn", "daily:last_sent"} {
		if _, ok := s.GetState(k); !ok {
			t.Errorf("%s should survive", k)
		}
	}
	// 一次无关的写入不许把它写回来 —— 而且要看**文件**：只查内存视图的话，
	// "装载时丢掉、下一次写入又从磁盘合并回来"这种自相矛盾会一路绿灯（生产就是这么露的）。
	if err := s.PutState("maint:probe", "x"); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "digest:last_sent") {
		t.Errorf("the merge-on-write pulled the retired key back onto disk:\n%s", string(onDisk))
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, ok := again.GetState("digest:last_sent"); ok {
		t.Error("a write resurrected the retired key")
	}
	if _, ok := again.GetState("maint:probe"); !ok {
		t.Error("the unrelated write should still be there")
	}
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open("   "); err == nil {
		t.Fatal("expected an error for an empty dir")
	}
}

// 政府公告永不下架，也常常不标结束（实测：高台 6 月的端午券公告到 9 月仍挂在栏目里）。
// 所以「没有 expires_at = 仍然有效」对限时事件类是错的，日报会反复播报一张早发完的券。
// 长期额度类必须保持原规则不变。
func TestLiveRequiresADeadlineForDatedEvents(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	mk := func(fp, title, category string, meta map[string]string) *model.Deal {
		return &model.Deal{Fingerprint: fp, URL: "https://nc.test/" + fp, Title: title, Score: 85,
			IsFree: true, Category: category, DiscoveredAt: now.Add(-30 * 24 * time.Hour), Meta: meta}
	}
	rows := []*model.Deal{
		mk("v1", "洪城消费券发放公告", model.CatVoucher, map[string]string{}),
		mk("v2", "洪城消费券第二轮", model.CatVoucher,
			map[string]string{"starts_at": now.Add(-2 * time.Hour).Format(time.RFC3339)}),
		mk("v3", "洪城消费券核销中", model.CatVoucher,
			map[string]string{"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339)}),
		mk("t1", "GLM 免费额度", model.CatAIFree, map[string]string{}),
	}
	for _, d := range rows {
		if err := st.Save(d); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	for _, d := range st.Live(50, now, 10) {
		got[d.Fingerprint] = true
	}
	if got["v1"] || got["v2"] {
		t.Errorf("a dated event with no stated end must leave the report: %v", got)
	}
	if !got["v3"] {
		t.Error("a dated event with a future deadline belongs in the report")
	}
	if !got["t1"] {
		t.Error("standing free tiers must keep the old rule")
	}
}

// 到期侧的取数与开抢侧同形状：只看还在窗口内的截止时刻，最近的排前面。它必须
// 独立于 Live —— 日报里"今天截止"的那一行，和提醒"三小时后截止"的那一行，读的是
// 同一个库但不同的条件。
func TestExpiringReturnsRowsInsideTheWindowSoonestFirst(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()

	mk := func(fp string, score int, at time.Time, meta map[string]string) *model.Deal {
		d := &model.Deal{Fingerprint: fp, URL: "https://a.test/" + fp, Title: fp + " 消费券",
			Score: score, IsFree: true, Category: model.CatVoucher,
			PublishedAt: now.Add(-48 * time.Hour), DiscoveredAt: now.Add(-48 * time.Hour), Meta: meta}
		if !at.IsZero() {
			d.Meta["expires_at"] = at.Format(time.RFC3339)
		}
		return d
	}
	soon := mk("s1", 70, now.Add(2*time.Hour), map[string]string{})
	sooner := mk("s2", 70, now.Add(40*time.Minute), map[string]string{})
	outOfWindow := mk("o1", 90, now.Add(20*time.Hour), map[string]string{})
	alreadyGone := mk("g1", 95, now.Add(-1*time.Hour), map[string]string{})
	noDeadline := mk("n1", 95, time.Time{}, map[string]string{})
	repeat := mk("r1", 95, now.Add(30*time.Minute), map[string]string{"dup_of": "s1"})
	low := mk("l1", 10, now.Add(30*time.Minute), map[string]string{})

	for _, d := range []*model.Deal{soon, sooner, outOfWindow, alreadyGone, noDeadline, repeat, low} {
		if err := st.Save(d); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	for _, d := range st.Expiring(now, now.Add(3*time.Hour), 45) {
		got = append(got, d.Fingerprint)
	}
	want := []string{"s2", "s1"}
	if len(got) != len(want) {
		t.Fatalf("Expiring() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Expiring()[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}
