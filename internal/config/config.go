// Package config loads Deal-Hunter settings from a JSON file with overrides
// from the process environment.
//
// Secrets are never read from the config file: only DH_* environment variables
// (or an env file pointed at by DH_ENV_FILE) can supply them, so a committed
// config can never leak a webhook token.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Known source kinds.
const (
	KindRSS        = "rss"
	KindHTML       = "html"
	KindJSON       = "json"
	KindOpenRouter = "openrouter"
	KindHN         = "hn"
	// KindSearch runs our own queries against an HTML search endpoint.
	KindSearch = "search"
	// KindSnapshot diffs a page or API against its own previous scrape.
	KindSnapshot = "snapshot"
)

// Duration accepts either a Go duration string ("20m") or a number of seconds.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var num json.Number
	if err := json.Unmarshal(b, &num); err == nil {
		if f, err := num.Float64(); err == nil {
			*d = Duration(time.Duration(f * float64(time.Second)))
			return nil
		}
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string or seconds: %s", string(b))
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// Source describes one collector.
type Source struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	URL      string            `json:"url"`
	Disabled bool              `json:"disabled,omitempty"`
	Trust    int               `json:"trust,omitempty"`
	Category string            `json:"category,omitempty"`
	Limit    int               `json:"limit,omitempty"`
	Timeout  Duration          `json:"timeout,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	Keywords []string          `json:"keywords,omitempty"`
	Deny     []string          `json:"deny,omitempty"`
	Sites    []string          `json:"sites,omitempty"` // official vendor domains for scoring
}

// Feishu configures direct delivery. Two credential sets work: a group
// custom-bot webhook, or an application identity (app_id/app_secret) that posts
// to a user or chat through the Open API. All four values come from the
// environment only, never from the config file.
type Feishu struct {
	Enabled            bool   `json:"enabled"`
	MinScore           int    `json:"min_score"`
	SilentHours        []int  `json:"silent_hours,omitempty"`
	Timezone           string `json:"timezone,omitempty"`
	AtAll              bool   `json:"at_all,omitempty"`
	MaxPerRun          int    `json:"max_per_run,omitempty"`
	DeduplicateMinutes int    `json:"dedupe_minutes,omitempty"`
	APIBase            string `json:"api_base,omitempty"`
	ReceiveIDType      string `json:"receive_id_type,omitempty"`

	WebhookURL string `json:"-"`
	Secret     string `json:"-"`
	AppID      string `json:"-"`
	AppSecret  string `json:"-"`
	ReceiveID  string `json:"-"`
}

// OpenClaw configures the assistant-side integration. Deal-Hunter never reads
// OpenClaw credentials: it either drops files that the assistant pulls over
// loopback HTTP, or shells out to the operator-installed `openclaw message
// send` command with a fixed argv.
type OpenClaw struct {
	Enabled     bool     `json:"enabled"`
	SkillDir    string   `json:"skill_dir,omitempty"`
	APIPath     string   `json:"api_path,omitempty"`
	Description string   `json:"description,omitempty"`
	Relay       bool     `json:"relay,omitempty"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	Channel     string   `json:"channel,omitempty"`
	Timeout     Duration `json:"timeout,omitempty"`
	Target      string   `json:"-"`
}

// Digest batches sub-threshold findings into one periodic summary.
type Digest struct {
	Enabled  bool     `json:"enabled"`
	Every    Duration `json:"every,omitempty"`
	MinScore int      `json:"min_score,omitempty"`
	At       string   `json:"at,omitempty"` // "08:30" local; overrides Every
	MaxItems int      `json:"max_items,omitempty"`
}

// Notify groups all delivery backends.
type Notify struct {
	Feishu   Feishu   `json:"feishu"`
	OpenClaw OpenClaw `json:"openclaw"`
	Digest   Digest   `json:"digest"`
	Console  bool     `json:"console"`
}

// Server is the read-only local API used by OpenClaw scripts.
type Server struct {
	Enabled bool   `json:"enabled"`
	Bind    string `json:"bind"`
}

// HTTP tunes the outbound fetcher.
type HTTP struct {
	UserAgent         string   `json:"user_agent,omitempty"`
	Timeout           Duration `json:"timeout,omitempty"`
	MaxBodyKB         int      `json:"max_body_kb,omitempty"`
	PerHostMili       int      `json:"per_host_min_interval_ms,omitempty"`
	Retries           int      `json:"retries,omitempty"`
	AllowPrivateHosts bool     `json:"allow_private_hosts,omitempty"`
}

// Filter decides what is worth storing and pushing.
type Filter struct {
	MinScore        int      `json:"min_score"`
	MaxAgeHours     int      `json:"max_age_hours,omitempty"`
	RequireOffer    bool     `json:"require_offer"`
	DenyKeywords    []string `json:"deny_keywords,omitempty"`
	AllowKeywords   []string `json:"allow_keywords,omitempty"`
	RequireKeywords []string `json:"require_keywords,omitempty"`
}

// Config is the whole application configuration.
type Config struct {
	DataDir  string   `json:"data_dir"`
	LogLevel string   `json:"log_level"`
	Interval Duration `json:"interval"`
	Jitter   Duration `json:"jitter,omitempty"`
	Timezone string   `json:"timezone,omitempty"`
	Filter   Filter   `json:"filter"`
	HTTP     HTTP     `json:"http"`
	Server   Server   `json:"server"`
	Notify   Notify   `json:"notify"`
	Sources  []Source `json:"sources"`
}

// Env is the set of environment variables honoured by the loader.
const (
	EnvDataDir       = "DH_DATA_DIR"
	EnvConfig        = "DH_CONFIG"
	EnvEnvFile       = "DH_ENV_FILE"
	EnvInterval      = "DH_INTERVAL"
	EnvMinScore      = "DH_MIN_SCORE"
	EnvLogLevel      = "DH_LOG_LEVEL"
	EnvFeishuWebhook = "DH_FEISHU_WEBHOOK"
	EnvFeishuSecret  = "DH_FEISHU_SECRET"
	EnvFeishuEnabled = "DH_FEISHU_ENABLED"
	// Application-identity delivery (no group bot required).
	EnvFeishuAppID    = "DH_FEISHU_APP_ID"
	EnvFeishuAppSect  = "DH_FEISHU_APP_SECRET"
	EnvFeishuRecvID   = "DH_FEISHU_RECEIVE_ID"
	EnvFeishuRecvType = "DH_FEISHU_RECEIVE_ID_TYPE"
	EnvFeishuAPIBase  = "DH_FEISHU_API_BASE"
	EnvServerBind     = "DH_SERVER_BIND"
	EnvServerEnabled  = "DH_SERVER_ENABLED"
	EnvAllowPublic    = "DH_ALLOW_PUBLIC_BIND"
	EnvDigestEnabled  = "DH_DIGEST_ENABLED"
	// OpenClaw relay: the destination chat id stays out of the config file.
	EnvOpenClawTarget = "DH_OPENCLAW_TARGET"
	EnvOpenClawCmd    = "DH_OPENCLAW_COMMAND"
	EnvOpenClawChan   = "DH_OPENCLAW_CHANNEL"
	EnvOpenClawRelay  = "DH_OPENCLAW_RELAY"
)

// Default returns a ready-to-run configuration using sources verified to be
// reachable from the 部署主机 network.
func Default() *Config {
	return &Config{
		DataDir:  "./data",
		LogLevel: "info",
		Interval: Duration(30 * time.Minute),
		Jitter:   Duration(3 * time.Minute),
		Timezone: "Asia/Shanghai",
		HTTP: HTTP{
			UserAgent:   "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36 DealHunter/1.0",
			Timeout:     Duration(20 * time.Second),
			MaxBodyKB:   3072,
			PerHostMili: 800,
			Retries:     2,
		},
		Server: Server{Enabled: true, Bind: "127.0.0.1:8765"},
		Filter: Filter{
			MinScore:     55,
			MaxAgeHours:  24 * 14,
			RequireOffer: true,
		},
		Notify: Notify{
			Feishu:   Feishu{Enabled: true, MinScore: 62, MaxPerRun: 6, DeduplicateMinutes: 90, Timezone: "Asia/Shanghai"},
			OpenClaw: OpenClaw{Enabled: true, SkillDir: "", APIPath: "/api/v1/", Description: "Deal-Hunter 羊毛情报"},
			Digest:   Digest{Enabled: true, Every: Duration(6 * time.Hour), MinScore: 45, MaxItems: 12},
			Console:  true,
		},
		Sources: DefaultSources(),
	}
}

// DefaultSources lists the collectors, ordered by autonomy: first what
// Deal-Hunter interrogates itself (model catalogues, our own search queries,
// our own price snapshots), then third-party aggregators that only add
// coverage and therefore carry lower trust. Every URL was verified reachable
// from the deployment host.
func DefaultSources() []Source {
	return []Source{
		// ---------- 自主探测：不依赖任何第三方聚合 ----------
		{
			Name: "openrouter-free-models", Kind: KindOpenRouter,
			URL:    "https://openrouter.ai/api/v1/models",
			Params: map[string]string{"max_new": "8"},
			Trust:  10, Category: "ai_free", Sites: []string{"openrouter.ai"},
		},
		{
			Name: "search-ai-free-cn", Kind: KindSearch,
			URL: "https://lite.duckduckgo.com/lite/", Trust: 9, Limit: 12,
			Params: map[string]string{
				"kl": "cn-zh",
				"queries": "大模型 API 免费额度 领取;限时免费 大模型 官方公告;" +
					"国产大模型 永久免费 token;AI 编程助手 免费 领取 额度;" +
					"模型 免费 开放 试用 官方;白嫖 AI API 免费额度 汇总",
			},
		},
		{
			Name: "search-ai-free-en", Kind: KindSearch,
			URL: "https://lite.duckduckgo.com/lite/", Trust: 9, Limit: 12,
			Params: map[string]string{
				"queries": "LLM API free credits announcement;model now free inference pricing;" +
					"free tier coding assistant limited time;zero cost API credits developer;" +
					"free GPU hours research grant",
			},
		},
		{
			Name: "search-cloud-cn", Kind: KindSearch,
			URL: "https://lite.duckduckgo.com/lite/", Trust: 8, Limit: 12,
			Params: map[string]string{
				"kl": "cn-zh",
				"queries": "云服务器 首购 特惠 元 一年;对象存储 免费额度 GB;" +
					"CDN 免费流量包 领取;轻量应用服务器 秒杀 特惠;云数据库 免费试用 新用户",
			},
		},
		{
			Name: "search-devtool-free", Kind: KindSearch,
			URL: "https://lite.duckduckgo.com/lite/", Trust: 8, Limit: 12,
			Params: map[string]string{
				"kl": "cn-zh",
				"queries": "学生 免费 授权 开发工具 申请;开源 许可证 免费 开发者 领取;" +
					"SaaS 免费版 优惠码 升级;Copilot JetBrains 免费 学生 教师",
			},
		},
		{
			Name: "aliyun-benefit", Kind: KindHTML,
			URL: "https://www.aliyun.com/benefit", Trust: 9,
			Sites: []string{"aliyun.com"}, Limit: 40,
			Keywords: []string{"免费", "试用", "额度", "折", "特惠"},
		},
		{
			Name: "copilot-plans-snapshot", Kind: KindSnapshot,
			URL: "https://github.com/features/copilot/plans", Trust: 8, Limit: 8,
			Sites:  []string{"github.com"},
			Params: map[string]string{"mode": "html", "focus": "free"},
		},
		// ---------- 聚合信息流：补充覆盖，权重更低 ----------
		{
			Name: "linux-do-latest", Kind: KindRSS,
			URL: "https://linux.do/latest.rss", Limit: 60, Trust: 6,
			Deny: []string{"招聘", "相亲", "车主", "婚礼", "举报"},
		},
		{
			Name: "aihot-all", Kind: KindRSS,
			URL: "https://aihot.news/feed/all.xml", Limit: 80, Trust: 6,
			Sites: []string{"aihot.news"},
		},
		{
			Name: "lowendtalk-latest", Kind: KindRSS,
			URL: "https://lowendtalk.com/discussions.rss", Limit: 80, Trust: 6,
			Deny: []string{"测评", "评测大赛"},
		},
		{
			Name: "qbitai-feed", Kind: KindRSS,
			URL: "https://www.qbitai.com/feed", Limit: 30, Trust: 5,
			Keywords: []string{"免费", "限时", "折", "开源", "发布", "试用", "额度"},
		},
		{
			Name: "v2ex-share", Kind: KindRSS,
			URL: "https://www.v2ex.com/index.xml", Limit: 60, Trust: 4,
		},
		{
			Name: "sspai-feed", Kind: KindRSS,
			URL: "https://sspai.com/feed", Limit: 40, Trust: 4,
		},
		{
			Name: "hn-free-api", Kind: KindHN,
			URL:    "https://hn.algolia.com/api/v1/search",
			Params: map[string]string{"query": "free API credits", "tags": "story", "numericFilters": "points>10"},
			Limit:  20, Trust: 5,
		},
		{
			Name: "hn-ai-promo", Kind: KindHN,
			URL:    "https://hn.algolia.com/api/v1/search",
			Params: map[string]string{"query": "model free tier launch", "tags": "story", "numericFilters": "points>20"},
			Limit:  20, Trust: 5,
		},
	}
}

// Load reads a config file (optional), applies env overrides and validates.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		path = os.Getenv(EnvConfig)
	}
	if envFile := os.Getenv(EnvEnvFile); envFile != "" {
		if err := loadEnvFile(envFile); err != nil {
			return nil, err
		}
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err == nil {
			// Defaults stay in place for absent keys, so the file may be partial.
			if err := json.Unmarshal(b, cfg); err != nil {
				return nil, fmt.Errorf("config: parse %s: %w", path, err)
			}
			if len(cfg.Sources) == 0 {
				cfg.Sources = DefaultSources()
			}
		}
	}
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyEnv() {
	setStr := func(env *string, key string) {
		if v := os.Getenv(key); v != "" {
			*env = v
		}
	}
	setStr(&c.DataDir, EnvDataDir)
	setStr(&c.LogLevel, EnvLogLevel)
	setStr(&c.Server.Bind, EnvServerBind)
	setStr(&c.Notify.Feishu.WebhookURL, EnvFeishuWebhook)
	setStr(&c.Notify.Feishu.Secret, EnvFeishuSecret)
	setStr(&c.Notify.Feishu.AppID, EnvFeishuAppID)
	setStr(&c.Notify.Feishu.AppSecret, EnvFeishuAppSect)
	setStr(&c.Notify.Feishu.ReceiveID, EnvFeishuRecvID)
	setStr(&c.Notify.Feishu.ReceiveIDType, EnvFeishuRecvType)
	setStr(&c.Notify.Feishu.APIBase, EnvFeishuAPIBase)
	setStr(&c.Notify.OpenClaw.Target, EnvOpenClawTarget)
	setStr(&c.Notify.OpenClaw.Command, EnvOpenClawCmd)
	setStr(&c.Notify.OpenClaw.Channel, EnvOpenClawChan)
	if v := os.Getenv(EnvOpenClawRelay); v != "" {
		c.Notify.OpenClaw.Relay = isTruthy(v)
	}
	if v := os.Getenv(EnvInterval); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Interval = Duration(d)
		}
	}
	if v := os.Getenv(EnvMinScore); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Filter.MinScore = n
			c.Notify.Feishu.MinScore = n
		}
	}
	if v := os.Getenv(EnvFeishuEnabled); v != "" {
		c.Notify.Feishu.Enabled = isTruthy(v)
	}
	if v := os.Getenv(EnvServerEnabled); v != "" {
		c.Server.Enabled = isTruthy(v)
	}
	if v := os.Getenv(EnvDigestEnabled); v != "" {
		c.Notify.Digest.Enabled = isTruthy(v)
	}
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// loadEnvFile applies KEY=VALUE lines without failing on comments. Values may
// be quoted. Existing process env wins so systemd EnvironmentFile stays last.
func loadEnvFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: env file %s: %w", path, err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("config: env file %s line %d: expected KEY=VALUE", path, i+1)
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); exists {
			continue
		}
		os.Setenv(k, v)
	}
	return nil
}

// Validate rejects configs that could leak a service to the public internet.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("config: data_dir is required")
	}
	if !filepath.IsAbs(c.DataDir) {
		abs, err := filepath.Abs(c.DataDir)
		if err == nil {
			c.DataDir = abs
		}
	}
	if c.Interval.D() < time.Minute {
		return fmt.Errorf("config: interval %s is too aggressive; use >= 1m", c.Interval)
	}
	if c.Filter.MinScore < 0 || c.Filter.MinScore > 100 {
		return fmt.Errorf("config: filter.min_score %d out of 0..100", c.Filter.MinScore)
	}
	if c.Notify.Feishu.MinScore < 0 || c.Notify.Feishu.MinScore > 100 {
		return fmt.Errorf("config: notify.feishu.min_score %d out of 0..100", c.Notify.Feishu.MinScore)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error", "":
	default:
		return fmt.Errorf("config: log_level %q must be debug|info|warn|error", c.LogLevel)
	}
	if c.Server.Enabled {
		if err := c.checkBind(c.Server.Bind); err != nil {
			return err
		}
	}
	names := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		s.Name = strings.TrimSpace(s.Name)
		s.Kind = strings.ToLower(strings.TrimSpace(s.Kind))
		if s.Name == "" {
			return fmt.Errorf("config: sources[%d]: name is required", i)
		}
		if names[s.Name] {
			return fmt.Errorf("config: duplicate source name %q", s.Name)
		}
		names[s.Name] = true
		switch s.Kind {
		case KindRSS, KindHTML, KindJSON, KindOpenRouter, KindHN, KindSearch, KindSnapshot:
		default:
			return fmt.Errorf("config: source %q: unknown kind %q", s.Name, s.Kind)
		}
		if !strings.HasPrefix(s.URL, "https://") && !strings.HasPrefix(s.URL, "http://") {
			return fmt.Errorf("config: source %q: url %q must be http(s)", s.Name, s.URL)
		}
		if s.Trust < 0 || s.Trust > 10 {
			return fmt.Errorf("config: source %q: trust %d out of 0..10", s.Name, s.Trust)
		}
	}
	return nil
}

// checkBind enforces the "never exposed to the internet" rule: loopback and
// Tailscale CGNAT (100.64/10) addresses are allowed, anything else needs an
// explicit opt-in.
func (c *Config) checkBind(bind string) error {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return fmt.Errorf("config: server.bind %q: %w", bind, err)
	}
	if port == "" {
		return fmt.Errorf("config: server.bind %q: missing port", bind)
	}
	if host == "" || host == "*" || host == "0.0.0.0" || host == "::" {
		if os.Getenv(EnvAllowPublic) == "1" {
			return nil
		}
		return fmt.Errorf("config: server.bind %q would expose the API on every interface; "+
			"bind to 127.0.0.1 or a Tailscale CGNAT (100.64/10) address (set %s=1 to override)", bind, EnvAllowPublic)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return nil
		}
		// Tailscale allocates from the RFC 6598 CGNAT block 100.64/10, so the
		// whole second-octet range must be accepted, not just 100.64.x.x.
		if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return nil
		}
		if ip.IsPrivate() || c.HTTP.AllowPrivateHosts {
			return nil
		}
		return fmt.Errorf("config: server.bind %q is a public address; refusing", host)
	}
	if host == "localhost" || strings.HasSuffix(host, ".ts.net") {
		return nil
	}
	if !c.HTTP.AllowPrivateHosts {
		return fmt.Errorf("config: server.bind host %q is not an IP; set http.allow_private_hosts to bind a named interface", host)
	}
	return nil
}

// EnabledSources returns the collectors that are not disabled.
func (c *Config) EnabledSources() []Source {
	out := make([]Source, 0, len(c.Sources))
	for _, s := range c.Sources {
		if !s.Disabled {
			out = append(out, s)
		}
	}
	return out
}

// Param reads a source parameter with a fallback.
func (s Source) Param(key, def string) string {
	if v, ok := s.Params[key]; ok && v != "" {
		return v
	}
	return def
}

// TimeoutOrDefault resolves the per-source fetch timeout.
func (s Source) TimeoutOrDefault(fallback time.Duration) time.Duration {
	if s.Timeout.D() > 0 {
		return s.Timeout.D()
	}
	if fallback <= 0 {
		return 20 * time.Second
	}
	return fallback
}
