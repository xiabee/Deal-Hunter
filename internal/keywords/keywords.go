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
		// The bridge between a lead-in and the date is a few non-digit characters,
		// because real announcements say 免费使用期限直接顺延至2026年8月31日 and
		// 限时免费开放至2026年8月31日, not just 截止日期: …
		{regexp.MustCompile(`(?:截止|截至|有效期至|核销期限|使用期限|结束时间|活动至|到期|开放至|持续至|顺延至|延期至|延长至|免费至|限免至)[^0-9\n]{0,6}?(20\d{2})\s*[-/.年]\s*(\d{1,2})\s*[-/.月]\s*(\d{1,2})`), "ymd"},
		{regexp.MustCompile(`(?i)(?:until|ends?\s+(?:on|by)|deadline|valid\s+(?:thru|through|until))\s+([a-z]{3,9})\.?\s+(\d{1,2})[, ]+(\d{4})`), "mdy"},
		{regexp.MustCompile(`(?i)(20\d{2})-(\d{1,2})-(\d{1,2})\s*(?:前结束|截止|到期|失效|过期)`), "ymd"},
		// …and the suffix form carries the year, so "需在 2026 年 12 月 31 日前注册"
		// is a stated deadline rather than a guess. A bare "8月28日前" stays unread.
		{regexp.MustCompile(`(?i)(20\d{2})\s*[-/.年]\s*(\d{1,2})\s*[-/.月]\s*(\d{1,2})\s*日?\s*(?:之?前)`), "ymd"},
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

// StartsAt reports the moment an announcement says something opens.
//
// A bare "9月21日" borrows its year from the anchor instead of rolling to next
// year when that lands in the past: a guessed future instant can fire a reminder
// for something that is already over, while a parsed-but-past one can never fire
// at all. Guessing low is the only safe direction here.
func (d *Dict) StartsAt(text string, anchor time.Time) *time.Time {
	m := startsAtRe.FindStringSubmatch(strings.ReplaceAll(text, "：", ":"))
	if m == nil {
		return nil
	}
	year := anchor.Year()
	if m[1] != "" {
		year, _ = strconv.Atoi(m[1])
	}
	mon, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	hour, _ := strconv.Atoi(m[5])
	minute := 0
	if m[6] != "" {
		minute, _ = strconv.Atoi(m[6])
	}
	switch m[4] {
	case "下午", "晚上":
		if hour < 12 {
			hour += 12
		}
	case "中午":
		if hour < 11 {
			hour += 12
		}
	}
	if year < 2000 || year > 2100 || mon < 1 || mon > 12 || day < 1 || day > 31 ||
		hour > 23 || minute > 59 {
		return nil
	}
	// The anchor carries the zone the announcement's clock belongs to, so the
	// result is right even though the service itself usually runs in UTC.
	t := time.Date(year, time.Month(mon), day, hour, minute, 0, 0, anchor.Location())
	// time.Date happily rolls 9月31日 over into October. An announcement with an
	// impossible date is a misread, and a reminder on the wrong day is worse than
	// no reminder at all.
	if t.Day() != day || int(t.Month()) != mon {
		return nil
	}
	return &t
}

// startsAtRe matches the date-and-hour spellings seen in government voucher
// announcements. The month and day are required: a bare "9时" gives no way to
// tell which day opens, and a reminder for the wrong day is worse than none.
var startsAtRe = regexp.MustCompile(
	`(?:(\d{4})年)?(\d{1,2})月(\d{1,2})日?[^0-9]{0,8}?(上午|中午|下午|晚上|凌晨)?[^0-9]{0,4}?(\d{1,2})\s*(?:时|点|:)\s*(\d{1,2})?`)

// ExpiresAt parses a deadline mentioned in the text, if one is present.
func (d *Dict) ExpiresAt(text string, loc *time.Location) *time.Time {
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
		// The deadline is a wall clock the reader reads, and the service usually
		// runs in UTC; nil means "whatever this host's clock is" for callers that
		// have no configured zone.
		if loc == nil {
			loc = time.Local
		}
		t := time.Date(year, time.Month(mon), day, 23, 59, 59, 0, loc)
		// time.Date rolls 9月31日 into October. An unreadable deadline must not
		// become a later one, or a finished offer keeps a day of airtime in the
		// briefing.
		if t.Day() != day || int(t.Month()) != mon {
			continue
		}
		return &t
	}
	// A range announces its own end: "2026年9月18日10:00至2026年9月30日23:59" carries
	// no 截止/有效期 lead-in, so the rules above miss it and the row would look like an
	// event that never said when it closes.
	if t, ok := rangeEnd(text, loc); ok {
		return &t
	}
	return nil
}

// A range is written by joining two complete dates, so it is read with two small
// regexes rather than one with optional groups: Go numbers submatches by
// participation, and an optional clock silently shifts m[2]/m[3] out from under the
// month and day.
var (
	rangeLeadRe  = regexp.MustCompile(`20\d{2}[-/.年]\s*\d{1,2}[-/.月]\s*\d{1,2}`)
	rangeTailRe  = regexp.MustCompile(`(?:至|到|~|—|–)\s*(20\d{2})[-/.年]\s*(\d{1,2})[-/.月]\s*(\d{1,2})`)
	rangeClockRe = regexp.MustCompile(`^\s*(\d{1,2})\s*[:时点]\s*(\d{1,2})?`)
)

func rangeEnd(text string, loc *time.Location) (time.Time, bool) {
	se := rangeTailRe.FindStringSubmatchIndex(text)
	if se == nil {
		return time.Time{}, false
	}
	// The joiner must connect two dates. "有效期至2026年9月30日" also contains 至, but
	// there is no date before it — that form belongs to the lead-in rules above.
	if !rangeLeadRe.MatchString(text[:se[0]]) {
		return time.Time{}, false
	}
	year, _ := strconv.Atoi(text[se[2]:se[3]])
	mon, _ := strconv.Atoi(text[se[4]:se[5]])
	day, _ := strconv.Atoi(text[se[6]:se[7]])
	hour, minute, sec := 23, 59, 59
	if cm := rangeClockRe.FindStringSubmatch(text[se[7]:]); cm != nil {
		h, err := strconv.Atoi(cm[1])
		if err != nil || h > 23 {
			return time.Time{}, false
		}
		hour, sec = h, 0
		minute = 0
		if cm[2] != "" {
			minute, _ = strconv.Atoi(cm[2])
			if minute > 59 {
				return time.Time{}, false
			}
		}
	}
	if year < 2000 || year > 2100 || mon < 1 || mon > 12 || day < 1 || day > 31 {
		return time.Time{}, false
	}
	if loc == nil {
		loc = time.Local
	}
	t := time.Date(year, time.Month(mon), day, hour, minute, sec, 0, loc)
	if t.Day() != day || int(t.Month()) != mon {
		return time.Time{}, false
	}
	return t, true
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
