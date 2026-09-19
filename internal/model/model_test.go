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
