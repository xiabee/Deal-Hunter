package model

import "testing"

func TestNormalizeURLCollapsesCosmeticDifferences(t *testing.T) {
	cases := map[string]string{
		"https://www.V2EX.com/t/123":                     "v2ex.com/t/123",
		"http://v2ex.com/t/123/":                         "v2ex.com/t/123",
		"https://v2ex.com/t/123#reply4":                  "v2ex.com/t/123",
		"https://v2ex.com/t/123?utm_source=telegram&x=1": "v2ex.com/t/123?x=1",
		"https://v2ex.com/t/123?spm=a2c1&from=weibo":     "v2ex.com/t/123",
		"  https://site.test/promo/?ref=feed#anchor  ":   "site.test/promo",
		"https://site.test/a":                            "site.test/a",
	}
	for raw, want := range cases {
		if got := NormalizeURL(raw); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Two pages that differ in substance must not collapse onto one fingerprint,
// or a new offer would be silently swallowed as a duplicate.
func TestNormalizeURLKeepsRealDifferences(t *testing.T) {
	a := NormalizeURL("https://site.test/pricing?plan=pro")
	b := NormalizeURL("https://site.test/pricing?plan=free")
	if a == b {
		t.Fatalf("both normalized to %q", a)
	}
	if NormalizeURL("https://site.test/a") == NormalizeURL("https://site.test/b") {
		t.Fatal("different paths must stay distinct")
	}
}

func TestDedupKeyCollapsesRepostedTitles(t *testing.T) {
	same := [][2]string{
		{"[分享创造] 用 WorkBuddy vibe 了一个小游戏站", "用 WorkBuddy vibe 了一个小游戏站"},
		{"【招聘】资深后端工程师（base 上海）", "[招聘] 资深后端工程师 (base 上海)"},
		{"(推广) 芒果 AI 便宜量大的代餐", "推广：芒果 AI，便宜量大的代餐！"},
		{"OpenAI 开放 ChatGPT for Microsoft Word，免费用户可在 Word 内起草", "OpenAI 开放 ChatGPT for Microsoft Word — 免费用户可在 Word 内起草"},
	}
	for _, p := range same {
		a, b := DedupKey(p[0]), DedupKey(p[1])
		if a == "" || a != b {
			t.Errorf("dedup key differs: %q vs %q  (from %q / %q)", a, b, p[0], p[1])
		}
	}
	// Different offers must never collide.
	diff := [][2]string{
		{"GLM-5.3-flash 限时免费开放", "GLM-5.3 视觉模型限时免费开放"},
		{"阿里云 Qwen3.8-Max 新品发布 5 折", "阿里云 Qwen3.8-Max 新品发布 8 折"},
	}
	for _, p := range diff {
		if a, b := DedupKey(p[0]), DedupKey(p[1]); a != "" && a == b {
			t.Errorf("distinct offers collapsed: %q == %q", p[0], p[1])
		}
	}
	// Titles too thin to judge by must not deduplicate anything.
	for _, s := range []string{"", "   ", "免费", "[推广]", "666"} {
		if got := DedupKey(s); got != "" {
			t.Errorf("DedupKey(%q) = %q, want empty", s, got)
		}
	}
}

func TestEnsureFingerprintIsStableAndSourced(t *testing.T) {
	x := &Deal{URL: "https://Site.test/promo#top", Source: "rss", Title: "同一件事"}
	y := &Deal{URL: "http://www.site.test/promo/", Source: "rss", Title: "同一件事"}
	x.EnsureFingerprint()
	y.EnsureFingerprint()
	if x.Fingerprint != y.Fingerprint {
		t.Fatalf("cosmetic URL differences must dedupe: %s vs %s", x.Fingerprint, y.Fingerprint)
	}
	// Linkless findings still need a stable identity.
	a := &Deal{Source: "linux-do-latest", Title: "只有标题"}
	b := &Deal{Source: "linux-do-latest", Title: "只有标题"}
	a.EnsureFingerprint()
	b.EnsureFingerprint()
	if a.Fingerprint == "" || a.Fingerprint != b.Fingerprint {
		t.Fatalf("title fallback unstable: %q vs %q", a.Fingerprint, b.Fingerprint)
	}
}
