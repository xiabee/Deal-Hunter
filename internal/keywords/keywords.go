// Package keywords detects price-advantage language (Chinese and English) and
// the vendors / model versions it refers to.
package keywords

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

// Dict is the detection dictionary. Use Default; the zero value matches nothing.
type Dict struct {
	groups     []group
	noise      []string
	vendors    []vendorNeedle
	product    *regexp.Regexp
	chineseZhe *regexp.Regexp
	percentOff *regexp.Regexp
	offAfter   *regexp.Regexp
	expiry     []expiryRule
}

type group struct {
	kind model.Kind
	list []string
}

type vendorNeedle struct {
	needle    string
	canonical string
}

// Default returns the built-in dictionary, tuned for AI/cloud offers plus
// general consumer discount chatter.
func Default() *Dict {
	d := &Dict{
		groups: []group{
			{model.KindFree, []string{
				"免费", "限时免费", "免费领", "免费用", "0元", "零元", "1元", "一元", "白嫖", "免单",
				"免费额度", "免费试用", "免费送", "无限免费", "永久免费", "免费开放", "全部免费", "开放试用",
				"free", "for free", "free tier", "free access", "free credits", "free quota", "no charge",
				"no cost", "$0", "at no cost", "100% off", "unlimited free", "zero cost", "no fee",
				"costs nothing", "free for", "now free",
			}},
			{model.KindDiscount, []string{
				"折扣", "打折", "半价", "立减", "直降", "特惠", "特价", "秒杀", "促销", "满减", "降价",
				"一折", "二折", "三折", "四折", "五折", "六折", "七折", "八折", "九折",
				"骨折价", "白菜价", "抄底价", "限时特惠", "大促", "补贴价",
				"discount", "% off", "sale", "promo", "promotion", "bogo", "buy one",
				"price drop", "reduced price", "flash sale", "half price",
			}},
			{model.KindCredit, []string{
				"赠送", "送额度", "代金券", "体验金", "充值送", "礼包", "免费领取", "签到", "积分兑换", "补贴",
				"credits", "signup bonus", "starter credit", "free credit", "rebate", "cashback",
			}},
			{model.KindTrial, []string{
				"试用", "免费体验", "公测", "内测", "抢先体验",
				"free trial", "trial", "beta access", "early access", "preview access",
			}},
			{model.KindCoupon, []string{
				"优惠券", "兑换码", "优惠码", "折扣码", "邀请码", "券码", "卡密",
				"coupon", "promo code", "voucher", "discount code",
			}},
		},
		noise: []string{
			"招聘", "求职", "相亲", "二手房", "租房", "贷款", "博彩", "彩票", "投资有风险",
			"代购", "微商", "演唱会门票", "hiring", "job offer", "recruit",
		},
		product:    regexp.MustCompile(`(?i)\b(?:glm|chatglm|qwen|deepseek|kimi|moonshot|doubao|ernie|hunyuan|spark|yi|step|minimax|hailuo|claude|gpt|gemini|mistral|grok|llama|groq|o)[-_ ]?v?\d+(?:\.\d+)*(?:[-.][a-z0-9]{1,12})*`),
		chineseZhe: regexp.MustCompile(`([1-9])\s*折`),
		percentOff: regexp.MustCompile(`(?i)(\d{1,3})\s*%\s*(?:off|折扣|减免|降价)`),
		offAfter:   regexp.MustCompile(`(?i)(?:off|discount|折扣)\D{0,12}?(\d{1,3})\s*%`),
	}
	for needle, canonical := range map[string]string{
		"智谱": "智谱AI", "glm": "智谱AI", "chatglm": "智谱AI", "bigmodel": "智谱AI",
		"通义": "阿里云", "百炼": "阿里云", "qwen": "阿里云", "aliyun": "阿里云", "alibaba cloud": "阿里云",
		"deepseek": "DeepSeek",
		"豆包":       "火山引擎", "火山引擎": "火山引擎", "doubao": "火山引擎", "volcengine": "火山引擎", "ark": "火山引擎",
		"kimi": "月之暗面", "moonshot": "月之暗面",
		"minimax": "MiniMax", "hailuo": "MiniMax",
		"文心": "百度智能云", "ernie": "百度智能云", "千帆": "百度智能云",
		"混元": "腾讯云", "hunyuan": "腾讯云",
		"星火": "科大讯飞", "spark llm": "科大讯飞",
		"阶跃": "阶跃星辰", "stepfun": "阶跃星辰",
		"天工": "昆仑万维", "skysense": "昆仑万维",
		"siliconflow": "硅基流动", "硅基流动": "硅基流动",
		"openrouter": "OpenRouter", "ollama": "Ollama",
		"anthropic": "Anthropic", "claude": "Anthropic",
		"openai": "OpenAI", "chatgpt": "OpenAI",
		"gemini": "Google", "vertex ai": "Google", "colab": "Google",
		"mistral": "Mistral", "le chat": "Mistral",
		"grok": "xAI", "xai": "xAI",
		"llama": "Meta", "groq": "Groq", "together ai": "Together", "fireworks ai": "Fireworks",
		"nvidia nim": "NVIDIA", "ngc": "NVIDIA",
		"aws": "AWS", "bedrock": "AWS", "azure": "Azure", "gcp": "GCP",
		"cloudflare": "Cloudflare", "vercel": "Vercel", "netlify": "Netlify",
		"github": "GitHub", "copilot": "GitHub", "codespaces": "GitHub", "jetbrains": "JetBrains",
		"oracle": "Oracle", "vultr": "Vultr", "hetzner": "Hetzner", "racknerd": "RackNerd",
		"京东": "京东", "淘宝": "淘宝", "拼多多": "拼多多", "美团": "美团", "饿了么": "饿了么",
		"网易云音乐": "网易云音乐", "b站": "B站", "哔哩哔哩": "B站", "夸克": "夸克", "quark": "夸克",
		"迅雷": "迅雷", "wps": "WPS", "百度网盘": "百度网盘", "阿里云盘": "阿里云盘",
		"notion": "Notion", "figma": "Figma", "canva": "Canva", "1password": "1Password",
		"huggingface": "Hugging Face", "modelscope": "魔搭", "飞书": "飞书", "钉钉": "钉钉",
	} {
		d.vendors = append(d.vendors, vendorNeedle{needle: needle, canonical: canonical})
	}
	sort.Slice(d.vendors, func(i, j int) bool { return d.vendors[i].needle < d.vendors[j].needle })
	d.expiry = []expiryRule{
		{regexp.MustCompile(`(?:截止|截至|有效期至|结束时间|活动至|到期)\s*[:：至]?\s*(20\d{2})[-/.年]\s*(\d{1,2})[-/.月]\s*(\d{1,2})`), "ymd"},
		{regexp.MustCompile(`(?i)(?:until|ends?\s+(?:on|by)|deadline|valid\s+(?:thru|through|until))\s+([a-z]{3,9})\.?\s+(\d{1,2})[, ]+(\d{4})`), "mdy"},
		{regexp.MustCompile(`(?i)(20\d{2})-(\d{1,2})-(\d{1,2})\s*(?:前结束|截止|到期|失效|过期)`), "ymd"},
	}
	return d
}

// expiryRule pairs a deadline pattern with the order its three capture groups
// appear in, since Chinese dates are year-first and English dates are not.
type expiryRule struct {
	re    *regexp.Regexp
	order string
}

// Scan returns every offer detected in the text, each with an evidence snippet.
func (d *Dict) Scan(text string) []model.Offer {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lower := strings.ToLower(text)
	vendors := d.Vendors(text)
	product := d.Product(text)
	var out []model.Offer
	for _, g := range d.groups {
		for _, needle := range g.list {
			lc := strings.ToLower(needle)
			idx := boundedIndex(lower, lc)
			if idx < 0 {
				continue
			}
			out = append(out, model.Offer{
				Kind:     g.kind,
				Matched:  needle,
				Vendor:   firstOrEmpty(vendors),
				Product:  product,
				Evidence: window(text, idx),
			})
		}
	}
	return out
}

// boundedIndex returns the first occurrence of needle that stands alone as a
// word for ASCII needles; later occurrences are considered when an earlier one
// is merely a substring such as "free" inside "freedom".
func boundedIndex(lower, needle string) int {
	if needle == "" {
		return -1
	}
	if !isASCII(needle) {
		return strings.Index(lower, needle)
	}
	for off := 0; ; {
		i := strings.Index(lower[off:], needle)
		if i < 0 {
			return -1
		}
		at := off + i
		if wordBounded(lower, at, at+len(needle)) {
			return at
		}
		off = at + 1
		if off >= len(lower) {
			return -1
		}
	}
}

// IsNoise reports whether the text matches a topic that must never be pushed.
func (d *Dict) IsNoise(text string) bool {
	lower := strings.ToLower(text)
	for _, n := range d.noise {
		if strings.Contains(lower, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

// Vendors returns canonical vendor names mentioned in the text.
func (d *Dict) Vendors(text string) []string {
	lower := strings.ToLower(text)
	seen := map[string]bool{}
	var out []string
	for _, v := range d.vendors {
		if !seen[v.canonical] && strings.Contains(lower, v.needle) {
			seen[v.canonical] = true
			out = append(out, v.canonical)
		}
	}
	return out
}

// Product returns the first detected model / product identifier, e.g. GLM-5.3-flash.
func (d *Dict) Product(text string) string {
	return strings.TrimSpace(d.product.FindString(text))
}

// IsModelNamed reports whether the text names a specific model version.
func (d *Dict) IsModelNamed(text string) bool { return d.Product(text) != "" }

// DiscountPct infers the size of a price cut from "N折" or "N% off" phrasing.
func (d *Dict) DiscountPct(text string) int {
	best := 0
	if m := d.chineseZhe.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 1 && n <= 9 {
			best = (10 - n) * 10
		}
	}
	for _, re := range []*regexp.Regexp{d.percentOff, d.offAfter} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if len(m) < 2 {
				continue
			}
			n, err := strconv.Atoi(m[1])
			if err != nil || n < 5 || n > 100 {
				continue
			}
			if n > best {
				best = n
			}
		}
	}
	return best
}

// ExpiresAt parses a deadline mentioned in the text, if one is present.
func (d *Dict) ExpiresAt(text string) *time.Time {
	for _, rule := range d.expiry {
		m := rule.re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		var year, mon, day int
		switch rule.order {
		case "mdy":
			n, ok := monthNumber(m[1])
			if !ok {
				continue
			}
			mon = n
			day, _ = strconv.Atoi(m[2])
			year, _ = strconv.Atoi(m[3])
		default:
			year, _ = strconv.Atoi(m[1])
			n, ok := monthNumber(m[2])
			if !ok {
				continue
			}
			mon = n
			day, _ = strconv.Atoi(m[3])
		}
		if year < 2000 || year > 2100 || mon < 1 || mon > 12 || day < 1 || day > 31 {
			continue
		}
		t := time.Date(year, time.Month(mon), day, 23, 59, 59, 0, time.Local)
		return &t
	}
	return nil
}

func monthNumber(s string) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, n >= 1 && n <= 12
	}
	names := map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}
	if len(s) >= 3 {
		n, ok := names[strings.ToLower(s[:3])]
		return n, ok
	}
	return 0, false
}

func firstOrEmpty(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}

func wordBounded(text string, start, end int) bool {
	if start > 0 && isWordByte(text[start-1]) {
		return false
	}
	if end < len(text) && isWordByte(text[end]) {
		return false
	}
	return true
}

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// window returns a rune-safe context around a byte offset for display.
func window(text string, byteIdx int) string {
	runes := []rune(text)
	consumed, pos := 0, 0
	for i, r := range runes {
		if consumed >= byteIdx {
			pos = i
			break
		}
		consumed += len(string(r))
	}
	const span = 80
	lo := pos - 16
	if lo < 0 {
		lo = 0
	}
	hi := lo + span
	if hi > len(runes) {
		hi = len(runes)
		lo = hi - span
		if lo < 0 {
			lo = 0
		}
	}
	return strings.Join(strings.Fields(string(runes[lo:hi])), " ")
}
