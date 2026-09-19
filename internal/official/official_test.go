package official

import (
	"strings"
	"testing"
)

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"https://open.bigmodel.cn/pricing": "open.bigmodel.cn",
		"http://User@Host.EXE:8447/a?b=1":  "host.exe",
		"https://[fd00::1]:18789/x":        "[fd00::1]",
		"www.v2ex.com":                     "www.v2ex.com",
		"":                                 "",
		"   https://a.test/#frag":          "a.test",
		// Synthetic userinfo URL: proves credentials never survive into a host
		// comparison.
		"https://someone:demo@example.test:443?x=1": "example.test", // secretlint:ignore
	}
	for raw, want := range cases {
		if got := HostOf(raw); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestVendorForDomainPrefersLongestMatch(t *testing.T) {
	cases := []struct {
		host, want string
		ok         bool
	}{
		{"bigmodel.cn", "智谱AI", true},
		{"open.bigmodel.cn", "智谱AI", true},
		{"pan.baidu.com", "百度网盘", true}, // not 百度智能云, which also owns baidu.com
		{"cloud.baidu.com", "百度智能云", true},
		{"linux.do", "", false},
		{"evil-bigmodel.cn", "", false}, // a hyphen is not a subdomain boundary
		{"bigmodel.cn.evil.test", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := VendorForDomain(c.host)
		if ok != c.ok || got != c.want {
			t.Errorf("VendorForDomain(%q) = %q,%v want %q,%v", c.host, got, ok, c.want, c.ok)
		}
	}
}

func TestClassifyReportsOfficialHost(t *testing.T) {
	v := Classify("https://platform.moonshot.cn/docs/pricing", "")
	if !v.Official || v.Vendor != "月之暗面" {
		t.Fatalf("classify = %+v", v)
	}
	v = Classify("https://www.v2ex.com/t/123", "智谱AI")
	if v.Official {
		t.Errorf("a community post must not classify as official: %+v", v)
	}
	if v.Vendor != "智谱AI" {
		t.Errorf("vendor hint should survive an unofficial host: %+v", v)
	}
}

func TestCanonicalForFollowsOfferKind(t *testing.T) {
	if u, ok := CanonicalFor("智谱AI", []string{"free"}); !ok || !strings.Contains(u, "bigmodel.cn") {
		t.Fatalf("canonical for 智谱AI/free = %q,%v", u, ok)
	}
	if u, ok := CanonicalFor("OpenRouter", []string{"discount"}); !ok || !strings.Contains(u, "openrouter.ai") {
		t.Fatalf("canonical for OpenRouter/discount = %q,%v", u, ok)
	}
	if _, ok := CanonicalFor("不明厂商", []string{"free"}); ok {
		t.Fatal("an unknown vendor must not yield a canonical page")
	}
	// Every curated page must be https: a plain-HTTP "official" link is a
	// credential leak waiting to happen on a hotel network.
	for _, v := range vendors {
		for k, u := range v.Canonical {
			if !strings.HasPrefix(u, "https://") {
				t.Errorf("%s/%s = %q: canonical pages must be https", v.Name, k, u)
			}
			if owner, ok := VendorForDomain(u); !ok || owner != v.Name {
				t.Errorf("%s/%s = %q is not on a domain owned by that vendor (%q,%v)", v.Name, k, u, owner, ok)
			}
		}
	}
}

func TestSearchQueryScopesToVendorDomain(t *testing.T) {
	q, domain, ok := SearchQuery("智谱AI", "GLM-5.3-flash", true)
	if !ok {
		t.Fatal("known vendor should produce a query")
	}
	if !strings.Contains(q, "site:"+domain) {
		t.Errorf("query %q must be scoped to %q", q, domain)
	}
	if !strings.Contains(q, "GLM-5.3-flash") || !strings.Contains(q, "免费") {
		t.Errorf("query should carry the model and the offer: %q", q)
	}
	if _, _, ok := SearchQuery("不明厂商", "x", true); ok {
		t.Fatal("an unknown vendor has no domain to search inside")
	}
	if q, _, _ := SearchQuery("DeepSeek", "", false); !strings.Contains(q, "价格") {
		t.Errorf("a paid offer should search pricing words: %q", q)
	}
}

func TestKeywordsFromTitleDropsNoise(t *testing.T) {
	got := KeywordsFromTitle("最新 官方 GLM-5.3-flash 免费额度 领取 https://x.com/a", 3)
	if strings.Contains(got, "https") || strings.Contains(got, "官方") || strings.Contains(got, "最新") {
		t.Errorf("stopwords and links should be dropped: %q", got)
	}
	if !strings.Contains(got, "GLM-5.3-flash") {
		t.Errorf("the model name is the searchable part: %q", got)
	}
	if len(strings.Fields(got)) > 3 {
		t.Errorf("max tokens not honoured: %q", got)
	}
	if KeywordsFromTitle("最新 今日 宣布", 4) != "" {
		t.Error("a title with nothing searchable should yield no query")
	}
}
