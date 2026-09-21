package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	code := cli(context.Background(), args, &buf, &buf)
	return code, buf.String()
}

func TestVersionAndHelp(t *testing.T) {
	code, out := run(t, "version")
	if code != 0 || !strings.Contains(out, "deal-hunter") {
		t.Fatalf("version: code=%d out=%s", code, out)
	}
	code, out = run(t, "help")
	if code != 0 || !strings.Contains(out, "secretscan") {
		t.Fatalf("help should document every command: code=%d", code)
	}
	// 这两条是后加的：漏在 help 里的命令不会被任何门禁发现，只能靠人记得查。
	for _, want := range []string{"events", "reparse"} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not document %s", want)
		}
	}
	code, out = run(t)
	if code != 2 || !strings.Contains(out, "用法") {
		t.Fatalf("bare invocation should print usage and exit 2, got %d", code)
	}
}

func TestUnknownCommandFails(t *testing.T) {
	code, out := run(t, "frobnicate")
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(out, "未知命令") {
		t.Errorf("output = %s", out)
	}
}

func TestSourcesCommandListsDefaults(t *testing.T) {
	code, out := run(t, "sources")
	if code != 0 {
		t.Fatalf("code = %d out = %s", code, out)
	}
	for _, want := range []string{"openrouter-free-models", "search-ai-free-cn", "copilot-plans-snapshot", "aliyun-benefit"} {
		if !strings.Contains(out, want) {
			t.Errorf("default source %s missing from the listing", want)
		}
	}
}

func TestProbeRejectsUnknownSource(t *testing.T) {
	code, out := run(t, "-data", t.TempDir(), "probe", "-source", "does-not-exist")
	if code == 0 || !strings.Contains(out, "未找到") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestDoctorWithoutNetworkPasses(t *testing.T) {
	code, out := run(t, "-data", t.TempDir(), "doctor", "-net=false")
	if code != 0 {
		t.Fatalf("doctor should pass with defaults: code=%d out=%s", code, out)
	}
	for _, want := range []string{"data_dir", "feishu", "server_bind", "体检通过"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
	// The webhook env var name must be suggested when it is missing.
	if !strings.Contains(out, "DH_FEISHU_WEBHOOK") {
		t.Errorf("doctor should name the missing env var:\n%s", out)
	}
}

func TestSecretScanGateExitsNonZero(t *testing.T) {
	clean := t.TempDir()
	if err := os.WriteFile(filepath.Join(clean, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := run(t, "secretscan", "-C", clean); code != 0 {
		t.Fatalf("clean dir should pass: code=%d out=%s", code, out)
	}

	dirty := t.TempDir()
	secret := "https://open.feishu.cn/open-apis/bot/v2/hook/" + strings.Repeat("a1B2", 8)
	if err := os.WriteFile(filepath.Join(dirty, "leak.txt"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, "secretscan", "-C", dirty)
	if code != 1 {
		t.Fatalf("dirty dir must fail the gate, code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "feishu_webhook_token") || !strings.Contains(out, "secretlint:ignore") {
		t.Errorf("output should name the rule and the opt-out:\n%s", out)
	}
}

func TestCompactAndDealsOnEmptyStore(t *testing.T) {
	dir := t.TempDir()
	if code, out := run(t, "-data", dir, "compact", "-days", "30"); code != 0 {
		t.Fatalf("compact: code=%d out=%s", code, out)
	}
	code, out := run(t, "-data", dir, "deals")
	if code != 0 || !strings.Contains(out, "暂无记录") {
		t.Fatalf("deals on an empty store: code=%d out=%s", code, out)
	}
}

func TestDealsPresentsTheVerifiedOfficialLink(t *testing.T) {
	// A stored deal carries the resolver's verdict in meta; the operator must see
	// the same link the Feishu card points at.
	const row = `{"fingerprint":"abc","url":"https://www.v2ex.com/t/1","title":"智谱 GLM-5.3-flash 免费",` +
		`"source":"rss","category":"ai_free","score":88,"is_free":true,` +
		`"discovered_at":"2026-09-19T01:02:03Z","meta":{"link_kind":"search_verified",` +
		`"official_url":"https://open.bigmodel.cn/pricing","original_url":"https://www.v2ex.com/t/1"}}` + "\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deals.jsonl"), []byte(row), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, "-data", dir, "deals")
	if code != 0 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "open.bigmodel.cn/pricing") || !strings.Contains(out, "✅") {
		t.Errorf("the verified vendor page should be listed:\n%s", out)
	}
	if strings.Contains(out, "v2ex.com/t/1") {
		t.Errorf("the replaced community post should not be the presented link:\n%s", out)
	}
}

func TestBadConfigIsReportedNotPanicking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"interval":"nope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, "-config", path, "sources")
	if code != 2 || !strings.Contains(out, "配置错误") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestNormalizeGlobalsHoistsFlagsAfterCommand(t *testing.T) {
	got := normalizeGlobals([]string{"once", "-config", "x.json", "extra"})
	if got[0] != "-config" || got[1] != "x.json" {
		t.Errorf("global flag not hoisted: %v", got)
	}
	if got[len(got)-1] != "extra" || !strings.Contains(strings.Join(got, " "), "once") {
		t.Errorf("subcommand args mangled: %v", got)
	}
	withEq := normalizeGlobals([]string{"-data=/tmp/x", "doctor"})
	if withEq[0] != "-data=/tmp/x" || withEq[1] != "doctor" {
		t.Errorf("-flag=value form broken: %v", withEq)
	}
}

func TestTruncateHelper(t *testing.T) {
	if got := truncate("一二三四五六七八九十", 5); got != "一二三四…" || len([]rune(got)) != 5 {
		t.Errorf("rune truncation = %q", got)
	}
	if truncate("abc", 10) != "abc" {
		t.Error("short strings must pass through")
	}
	if truncate("abc", 1) != "" {
		t.Error("degenerate width should return empty")
	}
}

// 体检查配置那一行必须把三种消息都说清楚：读者靠它判断"今天还会不会有消息、在等
// 哪几场"。漏掉提醒通道，就等于第三个通道在健康检查里不存在。
func TestDoctorReportsAllThreeMessageTypes(t *testing.T) {
	code, out := run(t, "-data", t.TempDir(), "doctor", "-net=false")
	if code != 0 {
		t.Fatalf("doctor should pass with defaults: code=%d out=%s", code, out)
	}
	for _, want := range []string{"日报=", "突破=", "提醒=", "到期前"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor config row missing %q:\n%s", want, out)
		}
	}
}

// 没有跟踪任何事件时也要给出明确答案，而不是空输出或报错 —— 运维看的是"雷达在不在盯"。
func TestEventsCommandRunsOnAnEmptyStore(t *testing.T) {
	if code, out := run(t, "-data", t.TempDir(), "events"); code != 0 || !strings.Contains(out, "没有") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

// 只写了核销期限、没写开抢时刻的券同样是跟踪对象。表格若仍拿"开抢时刻"这一列去判断
// 状态，这样的行会被标成"已过窗口"，运维就看不出提醒到底在等什么。
func TestEventsCommandListsDeadlineOnlyRows(t *testing.T) {
	due := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	const tmpl = `{"fingerprint":"d1","url":"https://nc.test/d","title":"洪城消费券第三批",` +
		`"source":"nc","category":"voucher","score":78,"is_free":true,"published_at":"%s",` +
		`"discovered_at":"%s","meta":{"expires_at":"%s"}}` + "\n"
	now := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deals.jsonl"),
		[]byte(fmt.Sprintf(tmpl, now, now, due)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, "-data", dir, "events")
	if code != 0 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "截止") || !strings.Contains(out, "窗口内待提醒") {
		t.Errorf("the deadline row should be listed as waiting inside its window:\n%s", out)
	}
	if strings.Contains(out, "已过窗口") {
		t.Errorf("a deadline-only row is not a missed opening:\n%s", out)
	}
}

// 解析器修好之后，游标已经推进的信息源不会再回到旧公告上，那些行就永远带着修复前读出的
// （其实是"没有"）时刻。reparse 的职责就是把当前解析器的结论补回已入库的行。
func TestReparseFillsTheDeadlineOlderRowsMissed(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, `{"fingerprint":"o1","url":"https://nc.test/one","title":"消费券第三批",`+
		`"summary":"领取后核销截止2026年9月10日","source":"nc","category":"voucher","score":78,`+
		`"is_free":true,"published_at":"2026-08-05T09:00:00+08:00",`+
		`"discovered_at":"2026-08-05T09:30:00+08:00","meta":{}}`)
	// 不带年份的写法只有拿公告自己的发布日当锚点才解得开：拿"今天"当锚点的话，
	// 9月10日要么已过期要么落在 45 天窗口外，就会被正确地拒收——那正是修复追溯不到的样子。
	seed(t, dir, `{"fingerprint":"o4","url":"https://nc.test/four","title":"文旅券补发",`+
		`"summary":"本批核销截止9月10日","source":"nc","category":"voucher","score":66,`+
		`"is_free":true,"published_at":"2026-08-05T09:00:00+08:00",`+
		`"discovered_at":"2026-08-05T09:30:00+08:00","meta":{}}`)
	// 折叠掉的副本既不进日报也不进提醒，重读它只是制造噪音。
	seed(t, dir, `{"fingerprint":"o3","url":"https://nc.test/three","title":"重复的第三条",`+
		`"summary":"核销截止2026年9月12日","source":"nc","category":"voucher","score":60,`+
		`"is_free":true,"published_at":"2026-08-05T09:00:00+08:00",`+
		`"discovered_at":"2026-08-05T09:30:00+08:00",`+
		`"meta":{"dup_of":"o1"}}`)

	code, out := run(t, "-data", dir, "reparse")
	if code != 0 {
		t.Fatalf("dry run: code=%d out=%s", code, out)
	}
	// 默认只报告，不动库：这条命令会在跑着的服务旁边执行，不能顺手改写生产数据。
	if !strings.Contains(readLog(t, dir), `"meta":{}`) {
		t.Errorf("dry run must not rewrite rows:\n%s", readLog(t, dir))
	}
	if !strings.Contains(out, "2026-09-10") {
		t.Errorf("dry run should report the deadline it would add:\n%s", out)
	}

	if code, out := run(t, "-data", dir, "reparse", "-write"); code != 0 {
		t.Fatalf("write: code=%d out=%s", code, out)
	}
	got := rowMeta(t, dir, "o1")
	// 时刻按公告自身发布日补年份，落在读者时钟的墙钟上，不是主机的 UTC。
	if want := "2026-09-10T23:59:59+08:00"; got["expires_at"] != want {
		t.Errorf("expires_at = %q, want %q", got["expires_at"], want)
	}
	if score := rowScore(t, dir, "o1"); score != 78 {
		t.Errorf("reparse must not re-rank rows, score went %d -> %d", 78, score)
	}
	if got := rowMeta(t, dir, "o4")["expires_at"]; got != "2026-09-10T23:59:59+08:00" {
		t.Errorf("yearless deadline should resolve off the row's own publish date, got %q", got)
	}
	if got := rowMeta(t, dir, "o3")["expires_at"]; got != "" {
		t.Errorf("a folded duplicate should be left alone, got %q", got)
	}
}

// 抹掉一个已存的截止比漏补更糟：Live 只把"写明且已过"的行请出去，读不出时刻的
// 限时活动就永久留在日报里。所以解析器这一轮读不出时，旧值必须原样留着。
func TestReparseNeverErasesAStatedDeadline(t *testing.T) {
	dir := t.TempDir()
	stored := "2026-11-30T23:59:59+08:00"
	seed(t, dir, `{"fingerprint":"o2","url":"https://nc.test/two","title":"一个没有任何日期的标题",`+
		`"source":"nc","category":"voucher","score":70,"is_free":true,`+
		`"published_at":"2026-08-05T09:00:00+08:00",`+
		`"discovered_at":"2026-08-05T09:30:00+08:00",`+
		`"meta":{"expires_at":"`+stored+`"}}`)

	if code, out := run(t, "-data", dir, "reparse", "-write"); code != 0 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if got := rowMeta(t, dir, "o2")["expires_at"]; got != stored {
		t.Errorf("a deadline the parser can no longer read must survive: got %q want %q", got, stored)
	}
}

// compactionMarker returns the 结果 column of the compaction row (! warn, ✓ ok, x fail).
func compactionMarker(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "compaction") {
			return strings.Fields(line)[0]
		}
	}
	t.Fatalf("no compaction row in:\n%s", out)
	return ""
}

// 备份是同盘之外唯一的恢复路径，而它跑在 systemd timer 上 —— 失败只进 journal，
// 没人翻日志就等于"一直在备份"。deploy/backup.sh 成功后在数据目录留一个标记，
// doctor 替它说话。
func TestDoctorReportsBackupFreshness(t *testing.T) {
	dir := t.TempDir()
	_, out := run(t, "-data", dir, "doctor", "-net=false")
	if compactionMarker(t, out) == "" {
		t.Fatal("compaction row should exist")
	}
	marker, detail := backupMarker(t, out)
	if marker != "!" || !strings.Contains(detail, "没有") {
		t.Errorf("a host that never left a backup stamp should warn: %q %q", marker, detail)
	}
	// 没有标记有两种成因，两种都得给出下一步 —— 只说"去 enable 定时器"会在
	// 已经启用定时器的机器上把人引向错误的方向。
	for _, want := range []string{"enable --now", "journalctl"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the missing-stamp hint should name %q: %q", want, detail)
		}
	}

	write := func(age time.Duration) {
		t.Helper()
		stamp := time.Now().UTC().Add(-age).Format("2006-01-02T15:04:05Z")
		if err := os.WriteFile(filepath.Join(dir, "backup.stamp"),
			[]byte(stamp+" deal-hunter-x.tar.gz\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	write(2 * time.Hour)
	_, out = run(t, "-data", dir, "doctor", "-net=false")
	if marker, _ := backupMarker(t, out); marker != "✓" {
		t.Errorf("a 2-hour-old backup should pass, marker = %q", marker)
	}

	write(9 * 24 * time.Hour)
	_, out = run(t, "-data", dir, "doctor", "-net=false")
	if marker, detail := backupMarker(t, out); marker != "!" || !strings.Contains(detail, "超过") {
		t.Errorf("a 9-day-old backup against a nightly timer should warn: %q %q", marker, detail)
	}

	if err := os.WriteFile(filepath.Join(dir, "backup.stamp"), []byte("胡说八道\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, out = run(t, "-data", dir, "doctor", "-net=false")
	if marker, _ := backupMarker(t, out); marker == "✓" {
		t.Error("an unreadable stamp must not be reported as a healthy backup")
	}

	// 空文件与读不出是两条分支：没有这条，那个防 panic 的守卫就是断言照不到的死代码。
	if err := os.WriteFile(filepath.Join(dir, "backup.stamp"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, "-data", dir, "doctor", "-net=false")
	if marker, _ := backupMarker(t, out); marker == "✓" {
		t.Error("an empty stamp must not be reported as a healthy backup")
	}
	if code != 0 {
		t.Errorf("doctor should still complete on a broken stamp, code=%d out=%s", code, out)
	}
}

// backupMarker returns the 结果 column and the detail of the backup row.
func backupMarker(t *testing.T, out string) (string, string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "backup" {
			return f[0], line
		}
	}
	t.Fatalf("no backup row in:\n%s", out)
	return "", ""
}

// 排程里的"每 48 轮"是进程内计数，天天部署的机器上永远凑不满，所以压缩这件事必须单独
// 报一行，不能因为"代码里有排程"就当作在跑。
func TestDoctorReportsCompactionRecency(t *testing.T) {
	dir := t.TempDir()
	code, out := run(t, "-data", dir, "doctor", "-net=false")
	if code != 0 {
		t.Fatalf("doctor: code=%d out=%s", code, out)
	}
	if got := compactionMarker(t, out); got != "!" {
		t.Errorf("a store that never compacted should warn, marker = %q", got)
	}
	if !strings.Contains(out, "从未压缩") {
		t.Errorf("and say so:\n%s", out)
	}

	stale := time.Now().UTC().AddDate(0, 0, -51).Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"maint:last_compact":"`+stale+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out = run(t, "-data", dir, "doctor", "-net=false")
	if got := compactionMarker(t, out); got != "!" {
		t.Errorf("a 51-day-old compaction should warn, marker = %q", got)
	}
	if !strings.Contains(out, "超过 7 天的排程") {
		t.Errorf("and name the schedule it missed:\n%s", out)
	}

	fresh := time.Now().UTC().AddDate(0, 0, -2).Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"maint:last_compact":"`+fresh+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out = run(t, "-data", dir, "doctor", "-net=false")
	if got := compactionMarker(t, out); got != "✓" {
		t.Errorf("a recent compaction should pass, marker = %q\n%s", got, out)
	}
}

// 手动 compact 与排程 compact 是同一件事，必须留同一个记号：不然刚压缩过的机器在
// doctor 里仍然写着"从未压缩过"，而排程器也会在下一次凑满轮数时白跑一遍。
func TestManualCompactRecordsTheSameMarkerAsTheSchedule(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, `{"fingerprint":"m1","url":"https://x.test/1","title":"昨天收录的一条","source":"s",`+
		`"category":"ai_free","score":70,"is_free":true,`+
		`"discovered_at":"`+time.Now().AddDate(0, 0, -3).UTC().Format(time.RFC3339)+`"}`)
	if code, out := run(t, "-data", dir, "compact"); code != 0 {
		t.Fatalf("compact: code=%d out=%s", code, out)
	}
	_, out := run(t, "-data", dir, "doctor", "-net=false")
	if marker, detail := compactionRow(t, out); marker == "!" && strings.Contains(detail, "从未压缩") {
		t.Errorf("a manual compact should count as a compaction:\n%s", out)
	}
}

// compactionRow returns the marker and the whole line for the compaction check.
func compactionRow(t *testing.T, out string) (string, string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "compaction" {
			return f[0], line
		}
	}
	t.Fatalf("no compaction row in:\n%s", out)
	return "", ""
}

// 改版后的页面返回 0 行时什么都不报错，而面板只记得最近一轮。doctor 因此要能说出
// "哪个信源多久没解析出过东西"，且这条读数在重启之后仍然成立。
func TestDoctorReportsSourceSilence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// 两个信源写进配置文件，doctor 才有"全部正常"这个可达的结论。
	if err := os.WriteFile(cfgPath, []byte(`{"sources":[
		{"name":"talkative","kind":"rss","url":"https://x.test/a.rss"},
		{"name":"muted","kind":"rss","url":"https://x.test/b.rss"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	both := func(a, b time.Duration) string {
		return `{"src:last_hit":{` +
			`"talkative":"` + time.Now().Add(-a).UTC().Format(time.RFC3339) + `",` +
			`"muted":"` + time.Now().Add(-b).UTC().Format(time.RFC3339) + `"}}`
	}
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	runDoctor := func() string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(data, "state.json"), []byte(both(time.Hour, time.Hour)), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := run(t, "-config", cfgPath, "-data", data, "doctor", "-net=false")
		if code != 0 {
			t.Fatalf("doctor: code=%d out=%s", code, out)
		}
		return out
	}
	field := func(out, name string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[1] == name {
				return line
			}
		}
		t.Fatalf("no %q row in:\n%s", name, out)
		return ""
	}

	if line := field(runDoctor(), "source"); !strings.HasPrefix(line, "✓") {
		t.Errorf("two sources that both parsed an hour ago should pass:\n%s", line)
	}

	// 一个安静 48 小时：只有它被点名，另一个不该被牵连。
	if err := os.WriteFile(filepath.Join(data, "state.json"),
		[]byte(both(time.Hour, 48*time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, outTmp := run(t, "-config", cfgPath, "-data", data, "doctor", "-net=false")
	line := field(outTmp, "source")
	if !strings.HasPrefix(line, "!") || !strings.Contains(line, "muted") {
		t.Errorf("the silent source should be named in a warn: %s", line)
	}
	if strings.Contains(line, "talkative") {
		t.Errorf("a source that spoke an hour ago must not be dragged in: %s", line)
	}

	// 一条记录都没有 = 观察刚开始，不能算衰减：那会把"我们刚装上"报成"它坏了"。
	if err := os.WriteFile(filepath.Join(data, "state.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, outTmp2 := run(t, "-config", cfgPath, "-data", data, "doctor", "-net=false")
	line = field(outTmp2, "source")
	if !strings.HasPrefix(line, "✓") || !strings.Contains(line, "观察") {
		t.Errorf("an empty record should read as still-watching, not as decay: %s", line)
	}

	// 记录本身已有两天、某个源从始至终没出过声 —— 这才是可以判的"从没出声"。
	aged := `{"src:last_hit":{"talkative":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `"},` +
		`"src:last_hit_since":"` + time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(data, "state.json"), []byte(aged), 0o600); err != nil {
		t.Fatal(err)
	}
	_, outTmp3 := run(t, "-config", cfgPath, "-data", data, "doctor", "-net=false")
	line = field(outTmp3, "source")
	if !strings.HasPrefix(line, "!") || !strings.Contains(line, "从记录开始起") {
		t.Errorf("a source silent since a two-day-old record should warn: %s", line)
	}
	if strings.Contains(line, "talkative（") {
		t.Errorf("the source that spoke an hour ago must not be named: %s", line)
	}
}

// seed writes one JSONL row into a fresh data directory.
func seed(t *testing.T, dir, line string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "deals.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func readLog(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "deals.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// lastRow returns the fields of the newest occurrence of a fingerprint: the log is
// append-only, so a rewrite is a later line, not an edit.
func lastRow(t *testing.T, dir, fp string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(readLog(t, dir), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		if row["fingerprint"] == fp {
			found = row
		}
	}
	if found == nil {
		t.Fatalf("no row for %s in %s", fp, dir)
	}
	return found
}

func rowMeta(t *testing.T, dir, fp string) map[string]string {
	t.Helper()
	out := map[string]string{}
	m, _ := lastRow(t, dir, fp)["meta"].(map[string]any)
	for k, v := range m {
		out[k], _ = v.(string)
	}
	return out
}

func rowScore(t *testing.T, dir, fp string) int {
	t.Helper()
	n, _ := lastRow(t, dir, fp)["score"].(float64)
	return int(n)
}
