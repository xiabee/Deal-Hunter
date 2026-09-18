package secretlint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// opaqueToken builds a genuinely high-entropy value at runtime. Written as one
// literal it would (correctly) be flagged in this repository by its own gate.
func opaqueToken() string {
	return "aB3d" + "E5fG" + "hI7j" + "K9lM" + "nO2p" + "Q4rS" + "6tU8" + "vW1x"
}

// The planted values are assembled at runtime so this test file does not itself
// contain anything the scanner would (correctly) flag.
func TestDetectsPlantedSecrets(t *testing.T) {
	tok := opaqueToken()
	gh := "ghp_" + strings.Repeat("Zx9Q", 6)
	tsKey := "tskey-" + strings.Repeat("Auth", 5) + "-suffix"
	ak := "AKIA" + strings.Repeat("ABCD", 4)
	jwt := strings.Join([]string{"eyJ" + strings.Repeat("hb", 8), strings.Repeat("pd", 12), strings.Repeat("sg", 12)}, ".")
	begin := "-----BEGIN" + " RSA PRIVATE KEY-----"
	files := map[string]string{
		"notify/push.go":     "package notify\nconst hook = \"https://open.feishu.cn/open-apis/bot/v2/hook/" + tok + "\"\n",
		"ci/deploy.sh":       "export GITHUB_TOKEN=" + gh + "\nexport TS_KEY=" + tsKey + "\n",
		"idcloud/aws.env":    "AWS_ACCESS_KEY_ID=" + ak + "\n",
		"docs/session.md":    "bearer token: " + jwt + "\n",
		"keys/deploy.pem":    begin + "\nMIIBVw\n" + "-----END" + " RSA PRIVATE KEY-----\n",
		"db/url.txt":         "postgres://svc:" + strings.Repeat("Po9s", 4) + "@db.internal:5432/app\n",
		"redis/url.txt":      "redis://user:" + strings.Repeat("Se3r", 4) + "@10.1.2." + "3:6379/0\n",
		"net/topology.md":    "the relay sits at 100.96.24." + "7 behind " + "node.example" + ".ts.net\n",
		"assign/config.json": "{\"feishu_secret\": \"" + strings.Repeat("Qw7E", 6) + "\"}\n",
	}
	root := writeTree(t, files)
	findings, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	rules := map[string]int{}
	for _, f := range findings {
		rules[f.Rule]++
	}
	for _, want := range []string{
		"feishu_webhook_token", "github_token", "tailscale_key", "aws_access_key",
		"jwt", "private_key_block", "database_dsn", "credentials_in_url",
		"tailscale_cgnat_address", "tailscale_magicdns", "secret_assignment", "high_entropy_token",
	} {
		if rules[want] == 0 {
			t.Errorf("rule %s never fired; findings=%d rules=%v", want, len(findings), rules)
		}
	}
}

func TestIgnoreMarkerSuppressesALine(t *testing.T) {
	tok := strings.Repeat("a1B2", 8)
	root := writeTree(t, map[string]string{
		"a.go": "const a = \"https://open.feishu.cn/open-apis/bot/v2/hook/" + tok + "\" // secretlint:ignore fake test value\n",
		"b.go": "const b = \"https://open.feishu.cn/open-apis/bot/v2/hook/" + tok + "\"\n",
	})
	findings, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !strings.HasSuffix(findings[0].Path, "b.go") {
		t.Fatalf("expected only the unmarked hit, got %+v", findings)
	}
}

func TestBenignTreeIsClean(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.go": `package main

import "fmt"

func main() {
	fmt.Println("hello") // 127.0.0.1 is fine to mention as a bind address
}
`,
		"README.md": "# Tool\n\nConfigure DH_FEISHU_WEBHOOK in your environment; never commit it.\n",
		"data.json": "{\"items\": [1, 2, 3]}\n",
	})
	findings, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("benign tree reported findings: %+v", findings)
	}
}

func TestSkipDirsAndBinaryFiles(t *testing.T) {
	tok := strings.Repeat("a1B2", 8)
	root := writeTree(t, map[string]string{
		"data/leak.txt":  "https://open.feishu.cn/open-apis/bot/v2/hook/" + tok,
		"outreach/x.txt": "https://open.feishu.cn/open-apis/bot/v2/hook/" + tok,
		"keep/pic.png":   "binary\x00data",
		"keep/real.go":   "https://open.feishu.cn/open-apis/bot/v2/hook/" + tok,
	})
	findings, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Path, "real.go") {
		t.Fatalf("skip rules not applied: %+v", findings)
	}
}

func TestEntropyIgnoresHexDigests(t *testing.T) {
	// Lowercase hex has an entropy ceiling of 4 bits/char and must not trip the
	// detector, otherwise every dependency checksum would look like a secret.
	root := writeTree(t, map[string]string{
		"go.sum-ish.txt": "module v1.2.3 h1:" + strings.Repeat("0123456789abcdef", 4) + "=\n",
	})
	findings, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Rule == "high_entropy_token" {
			t.Fatalf("hex digest flagged as a secret: %+v", findings)
		}
	}
}

func TestScanRejectsMissingRoot(t *testing.T) {
	if _, err := Scan(Options{Root: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("expected an error for a non-existent directory")
	}
}

// TestRepositoryIsOpenSourceClean is the release gate: nothing in this
// repository may carry a credential, a Tailscale address or a private hostname.
func TestRepositoryIsOpenSourceClean(t *testing.T) {
	root := filepath.Join("..", "..")
	findings, err := Scan(DefaultOptions(root))
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	if len(findings) > 0 {
		var b strings.Builder
		for i, f := range findings {
			if i >= 20 {
				b.WriteString("…\n")
				break
			}
			b.WriteString(f.String() + "\n")
		}
		t.Fatalf("repository must not contain secrets or private topology:\n%s", b.String())
	}
}
