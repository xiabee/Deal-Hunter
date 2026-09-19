package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		`"discovered_at":"2026-09-19T01:02:03Z","meta":{"link_kind":"vendor_entry",` +
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
