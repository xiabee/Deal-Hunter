// Package official turns a third-party mention into a verified vendor link.
//
// The rule is deliberately strict: a link is only rewritten when ALL three
// conditions hold — the deal names a vendor we know, the candidate page sits on
// that vendor's own curated domain (before AND after redirects), and it answers
// HTTP 2xx. A curated entry page is attached as a place to check, never as proof
// that the offer exists: a wrong "official" link is worse than an honest
// community link.
package official

import (
	"regexp"
	"strings"
)

// Vendor is one recognised supplier and its official internet real estate.
type Vendor struct {
	Name      string
	Domains   []string
	Canonical map[string]string // category -> curated entry page
}

// vendors is curated, not scraped: only domains the vendor controls.
var vendors = []Vendor{
	{Name: "智谱AI", Domains: []string{"bigmodel.cn", "zhipuai.cn", "chatglm.cn"},
		Canonical: map[string]string{"pricing": "https://open.bigmodel.cn/pricing", "docs": "https://docs.bigmodel.cn"}},
	{Name: "阿里云", Domains: []string{"aliyun.com", "alibabacloud.com", "aliyun.dev"},
		Canonical: map[string]string{"pricing": "https://www.aliyun.com/price/detail", "benefit": "https://www.aliyun.com/benefit", "docs": "https://help.aliyun.com/zh/model-studio/"}},
	{Name: "DeepSeek", Domains: []string{"deepseek.com"},
		Canonical: map[string]string{"pricing": "https://api-docs.deepseek.com/quick_start/pricing", "docs": "https://api-docs.deepseek.com"}},
	{Name: "火山引擎", Domains: []string{"volcengine.com", "volces.com"},
		Canonical: map[string]string{"pricing": "https://www.volcengine.com/pricing", "activity": "https://www.volcengine.com/activity"}},
	{Name: "月之暗面", Domains: []string{"moonshot.cn", "kimi.com"},
		Canonical: map[string]string{"pricing": "https://platform.moonshot.cn/docs/pricing"}},
	{Name: "MiniMax", Domains: []string{"minimaxi.com", "minimaxi.chat", "minimax.io", "hailuoai.com"},
		Canonical: map[string]string{"pricing": "https://api.minimaxi.chat/document/pricing"}},
	{Name: "百度智能云", Domains: []string{"baidu.com", "qianfan.cloud.baidu.com", "cloud.baidu.com"},
		Canonical: map[string]string{"pricing": "https://cloud.baidu.com/product/price/wenxin_big_model"}},
	{Name: "腾讯云", Domains: []string{"tencentcloud.com", "tencent.com", "qq.com"},
		Canonical: map[string]string{"pricing": "https://cloud.tencent.com/document/product/1729/97620"}},
	{Name: "科大讯飞", Domains: []string{"xfyun.cn", "iflytek.com"},
		Canonical: map[string]string{"pricing": "https://www.xfyun.cn/doc/spark/Charge.html"}},
	{Name: "阶跃星辰", Domains: []string{"stepfun.com"}, Canonical: map[string]string{"pricing": "https://platform.stepfun.com/docs/pricing"}},
	{Name: "硅基流动", Domains: []string{"siliconflow.cn", "siliconflow.com"},
		Canonical: map[string]string{"pricing": "https://siliconflow.cn/pricing", "docs": "https://docs.siliconflow.cn"}},
	{Name: "OpenRouter", Domains: []string{"openrouter.ai"},
		Canonical: map[string]string{"models": "https://openrouter.ai/models?max_price=0", "pricing": "https://openrouter.ai/pricing"}},
	{Name: "OpenAI", Domains: []string{"openai.com", "platform.openai.com", "chatgpt.com"},
		Canonical: map[string]string{"pricing": "https://openai.com/api/pricing"}},
	{Name: "Anthropic", Domains: []string{"anthropic.com", "console.anthropic.com"},
		Canonical: map[string]string{"pricing": "https://www.anthropic.com/pricing"}},
	{Name: "Google", Domains: []string{"ai.google.dev", "google.com", "cloud.google.com", "colab.research.google.com"},
		Canonical: map[string]string{"pricing": "https://ai.google.dev/gemini-api/docs/pricing"}},
	{Name: "Mistral", Domains: []string{"mistral.ai"}, Canonical: map[string]string{"pricing": "https://mistral.ai/pricing"}},
	{Name: "xAI", Domains: []string{"x.ai", "grok.com"}, Canonical: map[string]string{"pricing": "https://x.ai/pricing"}},
	{Name: "Groq", Domains: []string{"groq.com"}, Canonical: map[string]string{"pricing": "https://groq.com/pricing"}},
	{Name: "NVIDIA", Domains: []string{"nvidia.com", "build.nvidia.com", "ngc.nvidia.com"},
		Canonical: map[string]string{"credits": "https://build.nvidia.com"}},
	{Name: "Hugging Face", Domains: []string{"huggingface.co", "hf.co"}, Canonical: map[string]string{"docs": "https://huggingface.co/docs"}},
	{Name: "GitHub", Domains: []string{"github.com", "github.blog", "githubassets.com"},
		Canonical: map[string]string{"plans": "https://github.com/features/copilot/plans", "education": "https://education.github.com/pack"}},
	{Name: "JetBrains", Domains: []string{"jetbrains.com"}, Canonical: map[string]string{"community": "https://www.jetbrains.com/products/#section_community"}},
	{Name: "AWS", Domains: []string{"aws.amazon.com", "amazon.com"}, Canonical: map[string]string{"free": "https://aws.amazon.com/free/"}},
	{Name: "Azure", Domains: []string{"azure.com", "microsoft.com"}, Canonical: map[string]string{"free": "https://azure.microsoft.com/en-us/free/"}},
	{Name: "Cloudflare", Domains: []string{"cloudflare.com", "pages.dev"}, Canonical: map[string]string{"plans": "https://www.cloudflare.com/plans/"}},
	{Name: "Vercel", Domains: []string{"vercel.com"}, Canonical: map[string]string{"plans": "https://vercel.com/pricing"}},
	{Name: "Oracle", Domains: []string{"oracle.com", "oraclecloud.com"}, Canonical: map[string]string{"free": "https://www.oracle.com/cloud/free/"}},
	{Name: "Hetzner", Domains: []string{"hetzner.com"}, Canonical: map[string]string{"pricing": "https://www.hetzner.com/cloud/"}},
	{Name: "Vultr", Domains: []string{"vultr.com"}, Canonical: map[string]string{"pricing": "https://www.vultr.com/pricing/"}},
	{Name: "夸克", Domains: []string{"quark.cn"}, Canonical: map[string]string{"activity": "https://www.quark.cn"}},
	{Name: "百度网盘", Domains: []string{"pan.baidu.com"}, Canonical: map[string]string{"activity": "https://pan.baidu.com"}},
	{Name: "迅雷", Domains: []string{"xunlei.com"}, Canonical: map[string]string{"activity": "https://vip.xunlei.com"}},
	{Name: "WPS", Domains: []string{"wps.cn", "kdocs.cn"}, Canonical: map[string]string{"activity": "https://www.wps.cn"}},
	{Name: "B站", Domains: []string{"bilibili.com"}, Canonical: map[string]string{"activity": "https://account.bilibili.com"}},
	{Name: "飞书", Domains: []string{"feishu.cn", "larksuite.com"}, Canonical: map[string]string{"pricing": "https://www.feishu.cn/price"}},
	{Name: "Notion", Domains: []string{"notion.so", "notion.com"}, Canonical: map[string]string{"plans": "https://www.notion.com/pricing"}},
	{Name: "Figma", Domains: []string{"figma.com"}, Canonical: map[string]string{"plans": "https://www.figma.com/pricing/"}},
}

// byVendor indexes the table by vendor name.
var byVendor = func() map[string]Vendor {
	m := make(map[string]Vendor, len(vendors))
	for _, v := range vendors {
		m[v.Name] = v
	}
	return m
}()

// Verdict is the outcome of classifying one URL.
type Verdict struct {
	Host       string `json:"host"`
	Vendor     string `json:"vendor,omitempty"`
	Official   bool   `json:"official"`
	Category   string `json:"category,omitempty"`
	Verified   bool   `json:"verified"`
	ResolvedBy string `json:"resolved_by,omitempty"`
}

// HostOf returns the lowercase hostname of an absolute URL.
func HostOf(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "]") {
		s = s[:i]
	}
	return strings.ToLower(s)
}

// VendorForDomain reports which vendor owns a host. The longest matching domain
// wins: pan.baidu.com belongs to 百度网盘, not to the 百度智能云 entry that also
// sits under baidu.com.
func VendorForDomain(host string) (string, bool) {
	host = HostOf(host)
	if host == "" {
		return "", false
	}
	best, bestLen := "", 0
	for _, v := range vendors {
		for _, d := range v.Domains {
			if (host == d || strings.HasSuffix(host, "."+d)) && len(d) > bestLen {
				best, bestLen = v.Name, len(d)
			}
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// Classify states whether a URL is on a vendor's own domain, and which vendor.
func Classify(rawURL, vendor string) Verdict {
	host := HostOf(rawURL)
	v := Verdict{Host: host}
	if owner, ok := VendorForDomain(host); ok {
		v.Official = true
		v.Vendor = owner
		return v
	}
	v.Vendor = vendor
	return v
}

// OfficialDomainsOf returns the vendor's official domains (empty if unknown).
func OfficialDomainsOf(vendor string) []string {
	if v, ok := byVendor[vendor]; ok {
		return append([]string(nil), v.Domains...)
	}
	return nil
}

// CanonicalFor returns a curated entry page for a vendor and one of the
// categories a deal can plausibly point at ("pricing", "benefit", "free",
// "activity", "plans", "community", "credits", "docs").
func CanonicalFor(vendor string, kinds []string) (string, bool) {
	v, ok := byVendor[vendor]
	if !ok {
		return "", false
	}
	for _, k := range kinds {
		switch k {
		case "free", "credit", "trial", "coupon":
			for _, cand := range []string{"free", "credits", "benefit", "activity", "plans", "pricing"} {
				if u, ok := v.Canonical[cand]; ok {
					return u, true
				}
			}
		case "discount":
			for _, cand := range []string{"pricing", "plans", "activity", "benefit"} {
				if u, ok := v.Canonical[cand]; ok {
					return u, true
				}
			}
		}
	}
	for _, cand := range []string{"pricing", "plans", "docs", "activity"} {
		if u, ok := v.Canonical[cand]; ok {
			return u, true
		}
	}
	return "", false
}

// SearchQuery builds a site-scoped query aimed at the vendor's own domain.
// Words are kept short: search endpoints throttle long CJK strings differently.
func SearchQuery(vendor, product string, isFree bool) (query string, domain string, ok bool) {
	domains := OfficialDomainsOf(vendor)
	if len(domains) == 0 {
		return "", "", false
	}
	term := "免费 额度"
	if !isFree {
		term = "价格 优惠"
	}
	if p := strings.TrimSpace(product); p != "" {
		term = p + " " + term
	}
	return term + " site:" + domains[0], domains[0], true
}

var cjkOrWord = regexp.MustCompile(`[\p{Han}]+|[A-Za-z0-9.\-]+`)

// KeywordsFromTitle pulls the most search-worthy tokens out of a title so a
// community post's own wording can drive the official-page lookup.
func KeywordsFromTitle(title string, max int) string {
	var parts []string
	for _, m := range cjkOrWord.FindAllString(title, -1) {
		if len(m) < 2 {
			continue
		}
		switch strings.ToLower(m) {
		case "最新", "今日", "刚刚", "宣布", "官方", "活动", "链接", "http", "https", "com", "www":
			continue
		}
		parts = append(parts, m)
		if len(parts) >= max {
			break
		}
	}
	return strings.Join(parts, " ")
}
