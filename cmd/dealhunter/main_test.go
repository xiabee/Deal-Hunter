package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/model"
)

func run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	code := cli(context.Background(), args, &buf, &buf)
	return code, buf.String()
}

// dispatchCommandNames reads the command names straight out of main.go's dispatch
// table - the only source of truth for "what is a command".
func dispatchCommandNames(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || id.Name != "dispatch" {
			return true
		}
		lit, ok := as.Rhs[0].(*ast.CompositeLit)
		if !ok {
			t.Fatal("dispatch is not a composite literal")
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			k, ok := kv.Key.(*ast.BasicLit)
			if !ok {
				continue
			}
			out[strings.Trim(k.Value, `"`)] = true
		}
		return false
	})
	if len(out) < 10 {
		t.Fatalf("the dispatch table was not read back (got %d names)", len(out))
	}
	return out
}

// listedCommands picks "  name   描述" out of a command list block.
func listedCommands() func(string) map[string]bool {
	re := regexp.MustCompile(`^  ([a-z][a-z-]{2,})\s{2,}[^ ]`)
	return func(text string) map[string]bool {
		out := map[string]bool{}
		for _, line := range strings.Split(text, "\n") {
			if m := re.FindStringSubmatch(line); m != nil {
				out[m[1]] = true
			}
		}
		return out
	}
}

// 同一件事写在三个地方：dispatch 表（真来源）、usage 文本、README 的命令清单。
// 手抄的清单会绿着空转 —— 漏一个命令只有人翻 help 才发现。这里按真来源对表：
// 从 AST 取 dispatch 的键，再要求 usage 与 README 各列出**同一批**名字，两个方向都查。
func TestCommandListsAgreeWithTheDispatchTable(t *testing.T) {
	cmds := dispatchCommandNames(t)
	inList := listedCommands()
	usageList := inList(usage)

	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readmeList := inList(string(readme))

	for name := range cmds {
		if !usageList[name] {
			t.Errorf("命令 %q 在 dispatch 表里，但 usage 没写它", name)
		}
		if !readmeList[name] {
			t.Errorf("命令 %q 在 dispatch 表里，但 README 的命令清单没写它", name)
		}
	}
	// 反方向：文档里不能有一个跑不通的名字。
	for name := range usageList {
		if !cmds[name] {
			t.Errorf("usage documents %q, which is not a command", name)
		}
	}
	for name := range readmeList {
		if !cmds[name] {
			t.Errorf("README documents %q, which is not a command", name)
		}
	}
}

// 文档与门禁脚本里写的 CLI 片段也是主张：一个不存在的旗标会让下一次会话照着跑一遍
// 然后怀疑产品。这里把 main.go 里每个 FlagSet 声明的旗标读回来，再要求文档里
// `dealhunter <命令> -旗标` 的每一个都真的存在（全局旗标也算）。
func TestDocumentedCommandLineFlagsAreReal(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	declared := map[string]map[string]bool{} // FlagSet 名 -> 旗标名
	lines := strings.Split(string(src), "\n")
	setRe := regexp.MustCompile(`flag\.NewFlagSet\("([a-z-]+)"`)
	flagRe := regexp.MustCompile(`\.(?:Bool|String|Int|Duration)\("([a-z][a-z-]*)"`)
	for i, line := range lines {
		set := setRe.FindStringSubmatch(line)
		if set == nil {
			continue
		}
		name := set[1]
		if declared[name] == nil {
			declared[name] = map[string]bool{}
		}
		// 往后扫到下一个函数头为止：这一段就是这个 FlagSet 的声明。
		for j := i + 1; j < len(lines) && !strings.HasPrefix(lines[j], "func "); j++ {
			if f := flagRe.FindStringSubmatch(lines[j]); f != nil {
				declared[name][f[1]] = true
			}
		}
	}
	if len(declared) < 8 {
		t.Fatalf("only %d FlagSets read back from main.go: %v", len(declared), declared)
	}
	if len(declared["global"]) == 0 {
		t.Fatal("the global FlagSet was not read back, so every per-command check would pass vacuously")
	}
	cmds := dispatchCommandNames(t)

	paths := []string{"../../README.md", "../../docs/STATUS.md", "../../docs/ROADMAP.md",
		"../../scripts/ci-local.sh", "../../deploy/install.sh"}
	checked, problems := 0, 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			i := strings.LastIndex(line, "dealhunter")
			if i < 0 {
				i = strings.LastIndex(line, "deal-hunter")
			}
			if i < 0 {
				continue
			}
			tail := line[i:]
			for _, cut := range []string{"|", ">", ")", "\""} {
				if j := strings.Index(tail, cut); j >= 0 {
					tail = tail[:j]
				}
			}
			cmd := ""
			for _, tok := range strings.Fields(tail) {
				if cmds[tok] {
					cmd = tok
					break
				}
			}
			if cmd == "" {
				continue
			}
			for _, f := range regexp.MustCompile(` -([a-z][a-z-]*)`).FindAllStringSubmatch(tail, -1) {
				flag := f[1]
				checked++
				t.Logf("checked: %s -> dealhunter %s -%s", filepath.Base(p), cmd, flag)
				if declared[cmd][flag] || declared["global"][flag] {
					continue
				}
				problems++
				t.Errorf("%s: `dealhunter %s -%s` 这个旗标不存在（该命令声明的：%v）", p, cmd, flag, keys(declared[cmd]))
			}
		}
	}
	if checked < 5 {
		t.Fatalf("只有 %d 处文档旗标被检查到，这个测试基本等于没跑", checked)
	}
	t.Logf("checked %d documented flag usages across %d files, %d unknown", checked, len(paths), problems)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

// probe 是「信源准入」那道题的答题入口。它曾在自己文件里另抄一份评分公式，于是表格里
// 的分数是生产永远不会给出的数——准入判断读的是一个假数，而且没有任何一处会报警。
// 现在它调用排程用的同一个 pipeline.Screen。对表走的是真路径：真 HTTP（httptest 起的
// 本地服务）、真采集、真入库，然后要求同一批字节在两边的读数逐条相同：probe 表格里的
// 分数必须等于 once 写进 deals.jsonl 的分数，被挡下的行必须带着原因出现在表里、且不在库里。
func TestProbeAgreesWithTheRoundOnTheSameRows(t *testing.T) {
	body := rssFeed(t,
		rssItem("https://feed.test/free", "智谱 GLM-5.3-flash 限时免费开放", "官方公告：面向所有用户免费开放，API 调用 0 元。"),
		rssItem("https://feed.test/job", "【招聘】后端工程师一名", "招聘免费内推"),
		rssItem("https://feed.test/news", "本周例会议程", "讨论季度安排与值班"),
		rssItem("https://feed.test/old", "某模型限时开放", "免费开放，活动截止 2020-01-01"),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfgPath := writeProbeConfig(t, dir, srv.URL+"/feed.xml")
	data := filepath.Join(dir, "data")

	code, out := run(t, "-config", cfgPath, "-data", data, "once")
	if code != 0 {
		t.Fatalf("once: code=%d out=%s", code, out)
	}
	stored := storedDeals(t, data)
	// 阳性对照：这一轮必须真的往库里写了东西，否则下面"分数对得上"是在拿空表对空表。
	if len(stored) != 1 {
		t.Fatalf("exactly the live free offer should reach the store, got %d rows: %+v", len(stored), stored)
	}

	before := append([]byte(nil), readFileBytes(t, filepath.Join(data, "deals.jsonl"))...)
	beforeState := append([]byte(nil), readFileBytes(t, filepath.Join(data, "state.json"))...)
	code, out = run(t, "-config", cfgPath, "-data", data, "probe", "-source", "fixture")
	if code != 0 {
		t.Fatalf("probe: code=%d out=%s", code, out)
	}
	if got := readFileBytes(t, filepath.Join(data, "deals.jsonl")); string(got) != string(before) {
		t.Error("probe wrote to the deal log")
	}
	if got := readFileBytes(t, filepath.Join(data, "state.json")); string(got) != string(beforeState) {
		t.Error("probe advanced a cursor: probing must not consume the day's findings")
	}

	rows := probeRows(t, out)
	if len(rows) != 4 {
		t.Fatalf("probe should list every row it fetched, refused ones included, got:\n%s", out)
	}
	live := rows["https://feed.test/free"]
	if live[0] != "收" {
		t.Errorf("the live offer should be admitted, got %q", live[0])
	}
	if want := strconv.Itoa(stored["https://feed.test/free"]); live[1] != want {
		t.Errorf("probe printed score %s where the round stored %s", live[1], want)
	}
	for url, reason := range map[string]string{
		"https://feed.test/job":  "噪音词",
		"https://feed.test/news": "无优惠命中词",
		"https://feed.test/old":  "截止已过",
	} {
		row := rows[url]
		if row[0] != reason {
			t.Errorf("%s: refused for %q, probe says %q", url, reason, row[0])
		}
		if row[1] != "-" {
			t.Errorf("%s: a refused row has no score to show, got %q", url, row[1])
		}
	}
	// 被挡下的一行仍然要带出它读到的时刻："解析出了一个已经过去的截止"与
	// "这一页根本没有日期"对准入是两种结论。
	if !strings.Contains(strings.Join(rows["https://feed.test/old"], " "), "截止 ") {
		t.Errorf("the expired row should still show the deadline that was read: %v", rows["https://feed.test/old"])
	}
	if !strings.Contains(out, "其中 1 条会入库") {
		t.Errorf("the tally should say one row is admitted:\n%s", out)
	}
}

// 分数的算法只准有一处。CLI 当年抄过一份，此后再没跟上过；这条闸拦的是"在命令里再算
// 一遍"，不是某个手抄的期望值。反空转：同一个遍历必须还能数到 .Score 的**读**——
// 字段被改名时读写会一起归零，那时要红的是这一半。
func TestTheCLINeverComputesAScoreItself(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var writes, reads int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			as, isAssign := n.(*ast.AssignStmt)
			if isAssign {
				for _, lhs := range as.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Score" {
						writes++
					}
				}
				return true
			}
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Score" {
				reads++
			}
			return true
		})
	}
	if writes != 0 {
		t.Errorf("the CLI assigns to a Score %d times: the scorer in internal/scoring is the only one allowed to", writes)
	}
	if reads == 0 {
		t.Errorf("no read of any .Score found in %d files - the walk is not seeing the code it claims to check", len(files))
	}
}

// officialDomainBonus is scoring's +10 for a link matching a source's own
// `sites`. Spelled out here because the point of the test is that a candidate has
// not been granted it yet.
const officialDomainBonus = 10

func atoiScore(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("%q is not a score", s)
	}
	return n
}

func rssItem(url, title, summary string) string {
	return "<item><title>" + title + "</title><link>" + url + "</link><description>" + summary + "</description></item>"
}

// rssFeed stamps every item with a pubDate two hours old: inside the 24h freshness
// band now and for the whole time a test may take, whereas a fixed date would walk
// across a scoring band (or the max-age cutoff) depending on when CI ran.
func rssFeed(t *testing.T, items ...string) string {
	t.Helper()
	date := time.Now().UTC().Add(-2 * time.Hour).Format(http.TimeFormat)
	out := `<?xml version="1.0"?><rss version="2.0"><channel><title>准入测试源</title>`
	for _, it := range items {
		out += strings.TrimSuffix(it, "</item>") + "<pubDate>" + date + "</pubDate></item>"
	}
	return out + "</channel></rss>"
}

func writeProbeConfig(t *testing.T, dir, url string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	body := `{"timezone":"Asia/Shanghai","server":{"enabled":false},` +
		`"notify":{"console":false,"daily":{"enabled":false},"urgent":{"enabled":false},"event":{"enabled":false}},` +
		`"http":{"allow_private_hosts":true},` +
		`"sources":[{"name":"fixture","kind":"rss","url":"` + url + `","trust":8,"sites":["example.com"],"limit":10}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// storedDeals reads the log the round wrote, keyed by URL, valued by the score stored.
func storedDeals(t *testing.T, data string) map[string]int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(data, "deals.jsonl"))
	if err != nil {
		t.Fatalf("the round wrote no deal log: %v", err)
	}
	out := map[string]int{}
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var d model.Deal
		if err := json.Unmarshal(line, &d); err != nil {
			t.Fatalf("parse stored row %q: %v", line, err)
		}
		out[d.URL] = d.Score
	}
	return out
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// probeRows keys each table row by its link (the last field on the line).
func probeRows(t *testing.T, out string) map[string][]string {
	t.Helper()
	rows := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasPrefix(f[len(f)-1], "https://") {
			continue
		}
		rows[f[len(f)-1]] = f
	}
	return rows
}

// notify-test 是 README 与 install.sh 的"下一步"里都写着的那条命令：装完的人靠它
// 判断推送链路通不通。它此前一行都没被执行过，而它最该说对的是两种相反的场合——
// 只配了 console 时要成功（那是真送达，不是空话），一个通道都没有时必须失败。
// 后者尤其要紧：全部关掉时构造就应当拒绝（"no notification backend enabled"），
// 而不是带着零个出口跑一次自检、再对着空集合说"已送达所有通道"。
func TestNotifyTestReachesTheConsoleAndRefusesAnEmptyFanOut(t *testing.T) {
	dir := t.TempDir()
	code, out := run(t, "-data", dir, "notify-test")
	if code != 0 {
		t.Fatalf("with the default console backend the self-test must pass: code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "console") {
		t.Errorf("the channels it will use should be named before sending: %s", out)
	}
	if !strings.Contains(out, "✓") {
		t.Errorf("a delivered self-test should say so: %s", out)
	}

	quiet := filepath.Join(dir, "no-backends.json")
	if err := os.WriteFile(quiet, []byte(`{"server":{"enabled":false},"notify":{"console":false,`+
		`"feishu":{"enabled":false},"openclaw":{"enabled":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out = run(t, "-config", quiet, "-data", dir, "notify-test")
	// The refusal comes from construction, not from the send: an operator who turns
	// every channel off gets told the configuration is empty rather than a self-test
	// that "succeeded" against nothing.
	if code == 0 || !strings.Contains(out, "no notification backend enabled") {
		t.Fatalf("with every channel off it must refuse to start: code=%d out=%s", code, out)
	}
	if strings.Contains(out, "已送达") {
		t.Errorf("it must not claim delivery to zero channels: %s", out)
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

// doctor 会把飞书目的地打在终端上 —— 那行只能是主机名。API 侧同一条承诺早就有测试
// （httpapi 的 TestNoCredentialEverLeavesTheProcess），CLI 这一条一直没有：
// 截断逻辑写在 doctor 里，改坏了没有任何门禁会发现。
func TestDoctorPrintsOnlyTheHostOfAWebhook(t *testing.T) {
	t.Setenv(config.EnvFeishuWebhook, "https://open.feishu.invalid/open-apis/bot/v2/hook/supersecrettoken1234567")
	t.Setenv(config.EnvFeishuSecret, "TOPSECRETVALUE")
	t.Setenv(config.EnvFeishuEnabled, "true")
	code, out := run(t, "-data", t.TempDir(), "doctor", "-net=false")
	if code != 0 {
		t.Fatalf("doctor: code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "open.feishu.invalid") {
		t.Errorf("the host should be shown, that is the whole point of the row:\n%s", out)
	}
	for _, secret := range []string{"supersecrettoken1234567", "TOPSECRETVALUE", "/open-apis/bot/"} {
		if strings.Contains(out, secret) {
			t.Errorf("doctor leaked %q into its output:\n%s", secret, out)
		}
	}
}

// 解析器改进之后，历史行只有 reparse 会去读第二次 —— 而"该不该跑一次 reparse"
// 这件事原本只能靠人记得。doctor 从此自己报漂移，且报的数必须与 reparse 的数一致。
func TestDoctorReportsReparseDrift(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, `{"fingerprint":"d1","url":"https://nc.test/one","title":"消费券第一批",`+
		`"summary":"领取后核销截止2026年9月10日","source":"nc","category":"voucher","score":78,`+
		`"is_free":true,"published_at":"2026-08-05T09:00:00+08:00",`+
		`"discovered_at":"2026-08-05T09:30:00+08:00","meta":{}}`)
	seed(t, dir, `{"fingerprint":"d2","url":"https://nc.test/two","title":"没有时刻的一条",`+
		`"summary":"长期有效","source":"nc","category":"voucher","score":70,`+
		`"is_free":true,"published_at":"2026-08-05T09:00:00+08:00",`+
		`"discovered_at":"2026-08-05T09:30:00+08:00","meta":{}}`)

	row := func() string {
		t.Helper()
		code, out := run(t, "-data", dir, "doctor", "-net=false")
		if code != 0 {
			t.Fatalf("doctor: code=%d out=%s", code, out)
		}
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[1] == "reparse" {
				return line
			}
		}
		t.Fatalf("no reparse drift row in:\n%s", out)
		return ""
	}

	// 阳性对照：reparse 自己看到的行数必须与 doctor 报的数一致，否则其中一个是空转。
	_, dry := run(t, "-data", dir, "reparse")
	if !strings.Contains(dry, "以上 1 行可补出时刻") {
		t.Fatalf("reparse should list exactly the one stale row:\n%s", dry)
	}
	line := row()
	if !strings.HasPrefix(line, "!") || !strings.Contains(line, "1 行") {
		t.Errorf("one unread row should warn with that count: %s", line)
	}
	if strings.Contains(line, "d2") || strings.Contains(line, "0 行") {
		t.Errorf("the row with nothing to read must not be counted: %s", line)
	}

	if code, out := run(t, "-data", dir, "reparse", "-write"); code != 0 {
		t.Fatalf("write: code=%d out=%s", code, out)
	}
	after := row()
	if !strings.HasPrefix(after, "✓") {
		t.Errorf("after -write the drift should be gone: %s", after)
	}
	if !strings.Contains(after, "2 行") {
		t.Errorf("the ok row should still say how many rows were checked: %s", after)
	}
}

// 压缩这件事必须单独报一行：排程曾经数的是**进程内**轮次（每 48 轮），天天部署的机器
// 永远凑不满，而"代码里有排程"看起来一切正常。现在改成每轮检查、以盘上记号节流，
// 这一行仍然要存在——它回答的是"上一次真剪过是什么时候"。
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

// dailyRow pulls doctor's `daily` line out, and reports the status glyph with it.
func dailyRow(t *testing.T, dir string, extra ...string) (string, string) {
	t.Helper()
	code, out := run(t, append([]string{"-data", dir, "doctor", "-net=false"}, extra...)...)
	if code != 0 {
		t.Fatalf("doctor failed: code=%d out=%s", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "daily" {
			return f[0], line
		}
	}
	t.Fatalf("doctor printed no daily row:\n%s", out)
	return "", ""
}

// writeDailyConfig aims the briefing slot at an hour either side of this instant, so
// "the slot has passed" and "it has not" are both states the test constructs rather
// than ones it hopes the wall clock happens to be in.
func writeDailyConfig(t *testing.T, dir, at string, enabled bool) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	body := `{"timezone":"UTC","server":{"enabled":false},"notify":{"daily":{"enabled":` +
		strbool(enabled) + `,"at":"` + at + `"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func strbool(b bool) string { return map[bool]string{true: "true", false: "false"}[b] }

// slotPassed recomputes, from the same "HH:MM" the config got, whether today's slot is
// behind us. The test needs its own copy because "an hour from now" as a *clock* string
// wraps across midnight, and a leg whose expectation depends on what hour CI happened
// to run at is not an expectation.
func slotPassed(t *testing.T, at string, now time.Time) bool {
	t.Helper()
	var hh, mm int
	if _, err := fmt.Sscanf(at, "%d:%d", &hh, &mm); err != nil {
		t.Fatalf("bad at %q: %v", at, err)
	}
	n := now.UTC()
	return n.After(time.Date(n.Year(), n.Month(), n.Day(), hh, mm, 0, 0, time.UTC))
}

func TestDoctorReportsTheDailyBriefing(t *testing.T) {
	// 00:00 and 23:59 put the two branches a fixed distance from any wall clock, and
	// each leg still asserts against its own computation rather than a guess.
	const passedAt, aheadAt = "00:00", "23:59"
	now := time.Now().UTC()

	t.Run("槽位两侧各报一次", func(t *testing.T) {
		for _, at := range []string{passedAt, aheadAt} {
			dir := t.TempDir()
			cfg := writeDailyConfig(t, dir, at, true)
			glyph, row := dailyRow(t, dir, "-config", cfg)
			wantPassed := slotPassed(t, at, now)
			if wantPassed {
				if glyph != "!" || !strings.Contains(row, "没发出去") {
					t.Fatalf("slot %s has passed with nothing sent; must warn: %s", at, row)
				}
				for _, want := range []string{"不会补发", "deal-hunter daily"} {
					if !strings.Contains(row, want) {
						t.Errorf("the missed-slot row must name %q: %s", want, row)
					}
				}
			} else if glyph != "✓" || !strings.Contains(row, "还没到点") {
				t.Fatalf("slot %s is ahead; must be calm: %s", at, row)
			}
			_, dry := run(t, "-data", dir, "-config", cfg, "daily", "-dry")
			if got := strings.Contains(dry, "今日已发过"); got {
				t.Fatalf("-dry on a store that never sent says it did: %s", dry)
			}
			if !strings.Contains(dry, "今日还没发") {
				t.Fatalf("doctor and `daily -dry` disagree about today: %s", dry)
			}
		}
	})

	t.Run("今天已经发过", func(t *testing.T) {
		dir := t.TempDir()
		cfg := writeDailyConfig(t, dir, passedAt, true)
		// The exact bytes the store writes on a real send (measured once against
		// production; the writer itself is pinned by TestAppDailyIsBriefingState).
		if err := os.WriteFile(filepath.Join(dir, "state.json"),
			[]byte(`{"daily:last_sent":"`+time.Now().UTC().Format(time.RFC3339)+`"}`), 0o600); err != nil {
			t.Fatalf("write state: %v", err)
		}
		glyph, row := dailyRow(t, dir, "-config", cfg)
		if glyph != "✓" || !strings.Contains(row, "今天这份") {
			t.Fatalf("a spent slot must be reported as spent: %s", row)
		}
		if strings.Contains(row, "没发出去") {
			t.Fatalf("a day that already sent must not also be called missed: %s", row)
		}
		_, dry := run(t, "-data", dir, "-config", cfg, "daily", "-dry")
		if !strings.Contains(dry, "今日已发过") {
			t.Fatalf("doctor and `daily -dry` disagree about today: %s", dry)
		}
	})

	t.Run("日报被关掉", func(t *testing.T) {
		dir := t.TempDir()
		cfg := writeDailyConfig(t, dir, passedAt, false)
		glyph, row := dailyRow(t, dir, "-config", cfg)
		if glyph != "!" || !strings.Contains(row, "不会来") {
			t.Fatalf("disabling the product's only scheduled message must warn: %s", row)
		}
	})
}

// `dealhunter daily -dry` 是"今天这一份日报长什么样"唯一的人工入口，而覆盖率扫出来
// 它一直是 0%：没有任何自动化执行过这条命令。不带 -dry 的那一半故意不在这里跑 —— 实测
// 它会真的发送、把 daily:last_sent 写进 state.json，还会往落盘目录写文件。
func TestDailyDryRun(t *testing.T) {
	dir := t.TempDir()
	// 六行里应当只剩三行，三个被挡掉的理由各不相同：分数差 1、时刻已过、重复行。
	// 所以"刚好 3 条"不是靠某一行凑出来的，任何一道门失效都会改变数目。
	row := func(fp, title string, score int, meta string) string {
		return `{"fingerprint":"` + fp + `","url":"https://dry.test/` + fp +
			`","title":"` + title + `","source":"dry","category":"ai_free","score":` +
			fmt.Sprintf("%d", score) + `,"is_free":true,` +
			`"published_at":"2026-09-21T10:00:00Z","discovered_at":"2026-09-21T10:00:00Z","meta":` + meta + `}`
	}
	seed(t, dir, row("L1", "DryAlpha 每月免费额度", 80, `{}`))
	seed(t, dir, row("L2", "DryBeta 到年底", 60, `{"expires_at":"2099-12-31T00:00:00Z"}`))
	seed(t, dir, row("L3", "DryGamma 正卡在门槛上", 45, `{}`))
	seed(t, dir, row("X1", "XrayLow 差一分进不来", 44, `{}`))
	seed(t, dir, row("X2", "XrayExpired 时刻已过", 70, `{"expires_at":"2020-01-01T00:00:00Z"}`))
	seed(t, dir, row("X3", "XrayDup 是重复行", 90, `{"dup_of":"L1"}`))

	code, out := run(t, "-data", dir, "daily", "-dry")
	if code != 0 {
		t.Fatalf("daily -dry should succeed offline: code=%d out=%s", code, out)
	}
	m := regexp.MustCompile(`当前在效 (\d+) 条`).FindStringSubmatch(out)
	if m == nil || m[1] != "3" {
		t.Fatalf("the briefing should carry exactly the three claimable rows:\n%s", out)
	}
	shown := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "https://dry.test/") {
			shown++
		}
	}
	if shown != 3 {
		t.Fatalf("3 rows counted but %d rendered:\n%s", shown, out)
	}
	for _, want := range []string{"DryAlpha", "DryBeta", "DryGamma"} {
		if !strings.Contains(out, want) {
			t.Errorf("live row %q missing from the briefing:\n%s", want, out)
		}
	}
	for _, no := range []string{"XrayLow", "XrayExpired", "XrayDup"} {
		if strings.Contains(out, no) {
			t.Errorf("filtered row %q leaked into the briefing:\n%s", no, out)
		}
	}
	// 报头的两个数是运维判断"什么时候会发、按哪个时区算"的依据。
	if !strings.Contains(out, "09:00") || !strings.Contains(out, "Asia/Shanghai") {
		t.Errorf("the header should name the configured slot and zone:\n%s", out)
	}
	if !strings.Contains(out, "今日还没发") {
		t.Fatalf("a fresh store must report the slot as unused:\n%s", out)
	}
	// -dry 的全部意义：看完不等于用掉。写进 state.json 就是今天再也没有第二份。
	if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil && strings.Contains(string(b), "daily:last_sent") {
		t.Fatalf("-dry spent today's slot: %s", b)
	}

	// 反向的一半：名额真的用掉过的时候，必须说"已发过"。时刻取今天（配置时区）的正午，
	// 免得测试正好跨过午夜就凭毫秒决定成败。
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	now := time.Now().In(loc)
	at := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, loc)
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"daily:last_sent":"`+at.UTC().Format(time.RFC3339)+`"}`), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	_, out = run(t, "-data", dir, "daily", "-dry")
	if !strings.Contains(out, "今日已发过") {
		t.Fatalf("a briefing already sent today must be reported as spent:\n%s", out)
	}
	// 同一份库、同一个 -dry，条数不能因为发过就变。
	if m := regexp.MustCompile(`当前在效 (\d+) 条`).FindStringSubmatch(out); m == nil || m[1] != "3" {
		t.Fatalf("the dry preview must not shrink after the slot is spent:\n%s", out)
	}

	// 空库那一条：0 条也要说清楚，而不是打印一个光秃秃的表头。
	empty := t.TempDir()
	code, out = run(t, "-data", empty, "daily", "-dry")
	if code != 0 {
		t.Fatalf("an empty store is not an error for -dry: code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "当前在效 0 条") || !strings.Contains(out, "空报") {
		t.Fatalf("a day with nothing live must say so:\n%s", out)
	}
}

// The delivery row answers "这个源到底有没有用", which the silence row cannot: a
// source that parses its list page every round is 出声 even when nothing it parsed
// ever reached the store. Zero delivery is not automatically broken (a city's
// voucher calendar can be empty for weeks), so the observation record decides
// whether this is a note or a warning - the same discipline as source silence.
func TestDoctorReportsDelivery(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"sources":[
		{"name":"delivers","kind":"rss","url":"https://x.test/a.rss"},
		{"name":"parses-nothing-usable","kind":"rss","url":"https://x.test/b.rss"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	seed(t, data, `{"fingerprint":"d1","url":"https://x.test/one","title":"一条真的交付过",`+
		`"source":"delivers","category":"ai_free","score":72,"is_free":true,`+
		`"published_at":"2026-09-20T09:00:00+08:00","discovered_at":"2026-09-20T09:30:00+08:00","meta":{}}`)

	row := func(since time.Duration) string {
		t.Helper()
		state := `{"src:last_hit":{"delivers":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) +
			`","parses-nothing-usable":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) +
			`"},"src:last_hit_since":"` + time.Now().Add(-since).UTC().Format(time.RFC3339) + `"}`
		if err := os.WriteFile(filepath.Join(data, "state.json"), []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := run(t, "-config", cfgPath, "-data", data, "doctor", "-net=false")
		if code != 0 {
			t.Fatalf("doctor: code=%d out=%s", code, out)
		}
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[1] == "delivery" {
				return line
			}
		}
		t.Fatalf("no delivery row in:\n%s", out)
		return ""
	}

	fresh := row(2 * time.Hour)
	if !strings.HasPrefix(fresh, "✓") {
		t.Errorf("two hours of observation is not enough to call a quiet source broken: %s", fresh)
	}
	if !strings.Contains(fresh, "1 个源") || strings.Contains(fresh, "delivers（") {
		t.Errorf("the count should name one empty source and never the delivered one: %s", fresh)
	}

	aged := row(8 * 24 * time.Hour)
	if !strings.HasPrefix(aged, "!") {
		t.Errorf("after a week of watching, zero delivery is worth a look: %s", aged)
	}
	if !strings.Contains(aged, "parses-nothing-usable") || strings.Contains(aged, "delivers、") {
		t.Errorf("the aged row must name the empty source only: %s", aged)
	}
	if !strings.Contains(aged, "probe") {
		t.Errorf("the warning should say what to run next: %s", aged)
	}
}

// 准入那道题问的是"这一栏要不要接进来"，而被测的那个地址按定义还没在配置里。以前
// 唯一的办法是去生产机的 /etc 里加一条 extra_sources 再删掉 - 于是一条没通过的候选
// 会在生产配置里留一会儿。现在 `-url` 直接测。
func TestProbeCandidateSourceNeedsNoConfig(t *testing.T) {
	body := rssFeed(t,
		rssItem("https://feed.test/free", "智谱 GLM-5.3-flash 限时免费开放", "官方公告：面向所有用户免费开放，API 调用 0 元。"),
		rssItem("https://feed.test/news", "本周例会议程", "讨论季度安排与值班"),
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	// 一个空的 sources 清单：候选源不属于任何配置，这条命令也不该因此罢工。
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"timezone":"Asia/Shanghai","server":{"enabled":false},`+
		`"notify":{"console":false},"http":{"allow_private_hosts":true},"sources":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	code, out := run(t, "-config", cfgPath, "-data", data, "probe",
		"-url", srv.URL+"/feed.xml", "-kind", "rss", "-trust", "3")
	if code != 0 {
		t.Fatalf("a candidate must be probeable without a config entry: code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "候选源") || !strings.Contains(out, "(rss)") {
		t.Errorf("the header should say what it actually probed: %s", out)
	}
	rows := probeRows(t, out)
	if len(rows) != 2 {
		t.Fatalf("both rows should be listed with their verdicts, got:\n%s", out)
	}
	if rows["https://feed.test/free"][0] != "收" {
		t.Errorf("the live offer should read as admitted: %v", rows["https://feed.test/free"])
	}
	if rows["https://feed.test/free"][1] == "-" {
		t.Errorf("an admitted candidate row must carry a score: %v", rows["https://feed.test/free"])
	}
	if rows["https://feed.test/news"][0] != "无优惠命中词" {
		t.Errorf("the plain news row should be refused for the same reason a round would: %v", rows["https://feed.test/news"])
	}
	// The candidate has no `sites`, so nothing from it may earn the official-domain
	// bonus: admission is exactly the moment that bonus has not been granted yet.
	// The witness is the same bytes through the configured path with a domain
	// somebody did vouch for - the two scores must differ by exactly that bonus,
	// which is a pair, not a magic number. Trust is deliberately low: at 8 both
	// sides of the pair reached the scorer's 100 ceiling, and the delta came out 9
	// - the clamp, not the bonus, would have been what the test measured.
	vouched := filepath.Join(dir, "vouched.json")
	if err := os.WriteFile(vouched, []byte(`{"timezone":"Asia/Shanghai","server":{"enabled":false},`+
		`"notify":{"console":false},"http":{"allow_private_hosts":true},"sources":[`+
		`{"name":"vouched","kind":"rss","url":"`+srv.URL+`/feed.xml","trust":3,"sites":["feed.test"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, vOut := run(t, "-config", vouched, "-data", filepath.Join(dir, "data2"), "probe", "-source", "vouched")
	cand := probeRows(t, out)["https://feed.test/free"]
	sites := probeRows(t, vOut)["https://feed.test/free"]
	if len(cand) < 2 || len(sites) < 2 || sites[0] != "收" {
		t.Fatalf("both probes must admit the same row to compare scores: candidate=%v vouched=%v", cand, sites)
	}
	if delta := atoiScore(t, sites[1]) - atoiScore(t, cand[1]); delta != officialDomainBonus {
		t.Errorf("an unvouched candidate should sit exactly %d below the same row from a vouched domain, got %d",
			officialDomainBonus, delta)
	}
}

// 两个入口同时给（或都不给）时必须拒绝，而不是猜一个：猜错的那次探测会给出一个
// 看着像结论的表，而它测的是另一个地址。
func TestProbeNeedsExactlyOneOfSourceAndURL(t *testing.T) {
	dir := t.TempDir()
	code, out := run(t, "-data", dir, "probe")
	if code != 2 || !strings.Contains(out, "-source") || !strings.Contains(out, "-url") {
		t.Fatalf("giving neither should list both options: code=%d out=%s", code, out)
	}
	code, out = run(t, "-data", dir, "probe", "-source", "openrouter-free-models", "-url", "https://x.test/feed")
	if code != 2 || !strings.Contains(out, "只能给一个") {
		t.Fatalf("giving both must be refused: code=%d out=%s", code, out)
	}
}
