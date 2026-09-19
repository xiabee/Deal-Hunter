// Package secretlint scans a source tree for credentials and private
// infrastructure details. It exists because this repository is published: the
// gate runs in CI on every change and as `dealhunter secretscan` locally.
package secretlint

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Finding is one lint hit.
type Finding struct {
	Rule    string `json:"rule"`
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: %s:%d: %s", f.Rule, f.Path, f.Line, f.Snippet)
}

// Options tune a scan.
type Options struct {
	Root         string
	SkipDirs     []string
	SkipFiles    []string
	MaxFileBytes int64
}

// DefaultOptions ignores build output and runtime state.
func DefaultOptions(root string) Options {
	return Options{
		Root:         root,
		SkipDirs:     []string{".git", "data", "outreach", "dist", "bin", "tmp", ".ci", "node_modules", "vendor"},
		SkipFiles:    []string{"go.sum"},
		MaxFileBytes: 2 << 20,
	}
}

// Rule is a named pattern.
type Rule struct {
	Name string
	RE   *regexp.Regexp
}

// IgnoreMarker lets an intentional occurrence opt out, with the reason on the
// same line.
const IgnoreMarker = "secretlint:ignore"

// binaryExt is a denylist: unknown extensions are scanned, because a new file
// type holding a credential must not be skipped by omission.
var binaryExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true, ".webp": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true, ".pdf": true,
	".zip": true, ".gz": true, ".tgz": true, ".tar": true, ".bz2": true, ".xz": true,
	".so": true, ".dll": true, ".dylib": true, ".exe": true, ".class": true, ".wasm": true,
	".mp4": true, ".mov": true, ".pyc": true, ".test": true, ".sum": true,
}

// Rules covers credentials, private-network addresses and host identity. The
// CGNAT rule is deliberate: Tailscale addresses are internal topology and must
// not leak into a public repository.
func Rules() []Rule {
	raw := []struct {
		name string
		re   string
	}{
		{"feishu_webhook_token", `open\.feishu\.cn/open-apis/bot/v2/hook/[0-9a-zA-Z\-]{16,}`},
		{"lark_webhook_token", `open\.larksuite\.com/open-apis/bot/v2/hook/[0-9a-zA-Z\-]{16,}`},
		{"github_token", `\bgh[pousr]_[A-Za-z0-9]{20,}\b`},
		{"tailscale_key", `\btskey-[A-Za-z0-9_\-]{10,}\b`},
		{"private_key_block", `-----BEGIN [A-Z ]*PRIVATE KEY-----`},
		{"aws_access_key", `\bAKIA[0-9A-Z]{16}\b`},
		{"gcp_api_key", `\bAIza[0-9A-Za-z_\-]{30,}\b`},
		{"slack_token", `\bxox[baprs]-[A-Za-z0-9\-]{10,}\b`},
		{"openai_style_key", `\bsk-[A-Za-z0-9_\-]{20,}\b`},
		{"huggingface_token", `\bhf_[A-Za-z0-9]{30,}\b`},
		{"gitlab_token", `\bglpat-[A-Za-z0-9_\-]{20,}\b`},
		{"stripe_key", `\b[spr]k_live_[A-Za-z0-9]{16,}\b`},
		{"sendgrid_key", `\bSG\.[A-Za-z0-9_\-]{20,}\.[A-Za-z0-9_\-]{20,}\b`},
		{"jwt", `\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`},
		{"credentials_in_url", `(?i)\b[a-z][a-z0-9+.\-]*://[^/\s:@]{1,64}:[^/\s@]{4,}@[a-z0-9.\-]+`},
		{"database_dsn", `(?i)\b(?:postgres|postgresql|mysql|mongodb(?:\+srv)?|redis)://[^\s:@/]{1,64}:[^\s@]{4,}@[^\s]+`},
		{"secret_assignment", `(?i)\b(?:authorization|x-api-key|api[_-]?key|access[_-]?key|client[_-]?secret|app[_-]?secret|webhook[_-]?secret|feishu[_-]?secret|sendkey)\s*"?\s*[=:]\s*["'][^"'\s]{12,}["']`},
		{"feishu_app_id", `cli_[a-z0-9]{10,}`},
		{"tailscale_cgnat_address", `\b100\.(?:6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.\d{1,3}\.\d{1,3}\b`},
		{"tailscale_magicdns", `\b[a-z0-9](?:[a-z0-9\-]{0,61}[a-z0-9])?\.ts\.net\b`},
		{"private_ipv4", `\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3})\b`},
		{"ssh_private_key_filename", `(?i)filename["']?\s*[:=]\s*["']?id_(?:rsa|ed25519|ecdsa)\b`},
		{"serverchan_key", `\bSCT[A-Za-z0-9]{24,}\b|\bsctp[A-Za-z0-9]{24,}\b`},
		{"dingtalk_webhook_token", `oapi\.dingtalk\.com/robot/send\?access_token=[0-9a-f]{20,}`},
		{"telegram_bot_token", `\b\d{8,10}:AA[A-Za-z0-9_\-]{30,}\b`},
	}
	out := make([]Rule, 0, len(raw))
	for _, r := range raw {
		out = append(out, Rule{Name: r.name, RE: regexp.MustCompile(r.re)})
	}
	return out
}

// entropyThreshold is the Shannon bits/char above which an opaque token is
// reported even when it matches no known shape.
const entropyThreshold = 4.35
const entropyMinLen = 32

var opaqueRe = regexp.MustCompile(`[A-Za-z0-9+/=_\-]{32,}`)

// Scan walks opts.Root and returns every finding, sorted by path.
func Scan(opts Options) ([]Finding, error) {
	root := opts.Root
	if root == "" {
		root = "."
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("secretlint: %s is not a directory", root)
	}
	rules := Rules()
	var findings []Finding

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if isDir(name, opts.SkipDirs) && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !eligible(path, name, opts) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if len(data) == 0 || strings.ContainsRune(string(data[:minInt(len(data), 512)]), '\x00') {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, IgnoreMarker) {
				continue
			}
			for _, r := range rules {
				if loc := r.RE.FindString(line); loc != "" {
					findings = append(findings, Finding{
						Rule: r.Name, Path: rel(root, path), Line: i + 1, Snippet: clip(loc),
					})
				}
			}
			for _, tok := range highEntropyTokens(line) {
				findings = append(findings, Finding{
					Rule: "high_entropy_token", Path: rel(root, path), Line: i + 1, Snippet: clip(tok),
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

func eligible(path, name string, opts Options) bool {
	for _, f := range opts.SkipFiles {
		if f == name {
			return false
		}
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		// Dotfiles such as .gitignore are matched on the whole name.
		ext = strings.ToLower(name)
	}
	if binaryExt[ext] {
		return false
	}
	return true
}

func isDir(name string, skip []string) bool {
	for _, s := range skip {
		if s == name {
			return true
		}
	}
	return false
}

func highEntropyTokens(line string) []string {
	var out []string
	for _, tok := range opaqueRe.FindAllString(line, -1) {
		if !hasLower(tok) || !hasUpper(tok) || !hasDigit(tok) {
			continue
		}
		if shannon(tok) > entropyThreshold {
			out = append(out, tok)
		}
	}
	return out
}

func hasLower(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'a' && s[i] <= 'z' {
			return true
		}
	}
	return false
}

func hasUpper(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return true
		}
	}
	return false
}

func hasDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return true
		}
	}
	return false
}

func shannon(s string) float64 {
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	var h float64
	n := float64(len(s))
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(r)
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 48 {
		return string(r[:20]) + "…" + string(r[len(r)-12:])
	}
	return s
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
