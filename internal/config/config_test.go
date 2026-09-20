package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAreValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default() must validate: %v", err)
	}
	if len(cfg.Sources) < 8 {
		t.Errorf("expected a useful default source set, got %d", len(cfg.Sources))
	}
	var autonomous int
	for _, s := range cfg.Sources {
		if s.Kind == KindOpenRouter || s.Kind == KindSearch || s.Kind == KindSnapshot || s.Kind == KindHTML {
			autonomous++
		}
	}
	if autonomous < 5 {
		t.Errorf("default set must lead with autonomous collectors, got %d", autonomous)
	}
	for _, s := range cfg.Sources {
		if s.Name == "" || s.URL == "" {
			t.Errorf("source %+v is incomplete", s)
		}
	}
}

func TestPublicBindIsRefused(t *testing.T) {
	for _, bind := range []string{"0.0.0.0:8765", ":8765", "*:8765", "8.8.8.8:8765"} {
		cfg := Default()
		cfg.Server.Bind = bind
		if err := cfg.Validate(); err == nil {
			t.Errorf("bind %q must be refused", bind)
		} else if !strings.Contains(err.Error(), "expose") && !strings.Contains(err.Error(), "public") {
			t.Errorf("bind %q: unclear error: %v", bind, err)
		}
	}
}

func TestLoopbackAndTailscaleBindsAreAllowed(t *testing.T) {
	for _, bind := range []string{
		"127.0.0.1:8765", "localhost:8765",
		"100.64.0.1:8765",       // secretlint:ignore RFC6598 documentation prefix, used as a fixture
		"100.100.100.100:8765",  // secretlint:ignore CGNAT fixture for the /10 range check
		"some-host.ts.net:8765", // secretlint:ignore magic dns suffix under test
	} {
		cfg := Default()
		cfg.Server.Bind = bind
		if err := cfg.Validate(); err != nil {
			t.Errorf("bind %q should be allowed: %v", bind, err)
		}
	}
}

func TestExplicitPublicOverrideStillNeedsOptIn(t *testing.T) {
	cfg := Default()
	cfg.Server.Bind = "0.0.0.0:8765"
	t.Setenv(EnvAllowPublic, "1")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("opt-in override should pass: %v", err)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv(EnvInterval, "45m")
	t.Setenv(EnvFeishuWebhook, "https://open.feishu.cn/open-apis/bot/v2/hook/redacted")
	t.Setenv(EnvFeishuSecret, "s3cr3t-value")
	t.Setenv(EnvFeishuEnabled, "false")
	t.Setenv(EnvDataDir, t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Interval.D() != 45*time.Minute {
		t.Errorf("interval = %s", cfg.Interval)
	}
	if cfg.Notify.Feishu.WebhookURL == "" || cfg.Notify.Feishu.Secret == "" {
		t.Error("secrets must come from the environment")
	}
	if cfg.Notify.Feishu.Enabled {
		t.Error("DH_FEISHU_ENABLED=false should disable the backend")
	}
}

// The morning briefing ships on: a feature nobody is told about in the README
// and nobody receives by default is the same as absent.
func TestDailyBriefingDefaultsAndEnv(t *testing.T) {
	d := Default().Notify.Daily
	if !d.Enabled || d.At != "09:00" || d.MinScore <= 0 || d.MaxItems <= 0 {
		t.Fatalf("unexpected defaults: %+v", d)
	}
	if Default().Timezone == "" {
		t.Fatal("the briefing slot is meaningless without a timezone")
	}

	t.Setenv(EnvDailyAt, "07:30")
	t.Setenv(EnvDailyEnabled, "false")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Notify.Daily.At != "07:30" {
		t.Errorf("at = %q", cfg.Notify.Daily.At)
	}
	if cfg.Notify.Daily.Enabled {
		t.Error("DH_DAILY_ENABLED=false must disable the briefing")
	}

	// A file may re-enable it and set the shape of the report.
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"notify":{"daily":{"enabled":true,"at":"08:15","min_score":55,"max_items":8}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDailyEnabled, "")
	t.Setenv(EnvDailyAt, "") // env wins over the file, so clear it to test the file
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := loaded.Notify.Daily
	if !got.Enabled || got.At != "08:15" || got.MinScore != 55 || got.MaxItems != 8 {
		t.Errorf("file config not applied: %+v", got)
	}
}

// The whole point of the delivery model is that a day produces one message.
// These defaults are that promise, so they are asserted as a contract: anything
// that loosens them has to say so here first.
func TestOneReportADayIsTheDefault(t *testing.T) {
	d := Default().Notify.Daily
	if !d.Enabled {
		t.Error("the briefing must be on by default, or nothing is ever delivered")
	}
	u := Default().Notify.Urgent
	if u.MaxPerDay != 1 {
		t.Errorf("breakthroughs per day = %d, want 1", u.MaxPerDay)
	}
	if u.MinScore <= d.MinScore {
		t.Errorf("the breakthrough gate (%d) must sit above what a briefing row needs (%d)",
			u.MinScore, d.MinScore)
	}
	if u.MinScore < 80 {
		t.Errorf("breakthrough gate %d is low enough to interrupt on ordinary finds", u.MinScore)
	}
	if !u.Enabled {
		t.Error("a free model that just went free should not wait for tomorrow")
	}
}

func TestUrgentGateEnv(t *testing.T) {
	t.Setenv(EnvUrgentMinScore, "97")
	t.Setenv(EnvUrgentEnabled, "false")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Notify.Urgent.MinScore != 97 {
		t.Errorf("min_score = %d", cfg.Notify.Urgent.MinScore)
	}
	if cfg.Notify.Urgent.Enabled {
		t.Error("DH_URGENT_ENABLED=false must retire the breakthrough channel")
	}
}

func TestSecretsCannotBeSmuggledThroughTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Even if someone writes a webhook into the file, the loader must ignore it.
	raw := `{"data_dir":"` + filepath.ToSlash(dir) + `","notify":{"feishu":{"webhook_url":"https://example.invalid/leak","secret":"leaked"}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Notify.Feishu.WebhookURL != "" || cfg.Notify.Feishu.Secret != "" {
		t.Fatalf("loader must never read credentials from disk, got %+v", cfg.Notify.Feishu)
	}
	// Round-tripping a config to disk must not emit secrets either.
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "webhook_url") || strings.Contains(string(b), "\"secret\"") {
		t.Errorf("serialized config leaked secret fields: %s", b)
	}
}

func TestPartialFileKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte(`{"interval":"5m","filter":{"max_age_hours":48}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Interval.D() != 5*time.Minute || cfg.Filter.MaxAgeHours != 48 {
		t.Errorf("overrides lost: %+v", cfg.Filter)
	}
	if len(cfg.Sources) != len(DefaultSources()) {
		t.Errorf("sources should fall back to defaults, got %d", len(cfg.Sources))
	}
	if cfg.Notify.Urgent.MinScore == 0 || cfg.Notify.Daily.At == "" {
		t.Error("unspecified notify fields must keep their defaults")
	}
}

func TestValidationRejectsBadConfigs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"interval", func(c *Config) { c.Interval = Duration(time.Second) }, "too aggressive"},
		{"dupes", func(c *Config) {
			c.Sources = []Source{{Name: "x", Kind: KindRSS, URL: "https://a/"}, {Name: "x", Kind: KindRSS, URL: "https://b/"}}
		}, "duplicate"},
		{"kind", func(c *Config) { c.Sources = []Source{{Name: "x", Kind: "magic", URL: "https://a/"}} }, "unknown kind"},
		{"url", func(c *Config) { c.Sources = []Source{{Name: "x", Kind: KindRSS, URL: "ftp://a/"}} }, "must be http"},
		{"loglevel", func(c *Config) { c.LogLevel = "verbose" }, "log_level"},
		{"trust", func(c *Config) { c.Sources = []Source{{Name: "x", Kind: KindRSS, URL: "https://a/", Trust: 99}} }, "trust"},
		{"urgent score", func(c *Config) { c.Notify.Urgent.MinScore = 101 }, "urgent.min_score"},
		// A briefing hour that does not parse would silently cost the day's only
		// message, so it has to fail at startup rather than at 09:00.
		{"daily at", func(c *Config) { c.Notify.Daily.At = "9am" }, "daily.at"},
		{"daily at single digit", func(c *Config) { c.Notify.Daily.At = "9:5" }, "daily.at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestEnvFileIsAppliedWithoutOverridingRealEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "secrets.env")
	body := "# comment\nDH_INTERVAL=45m\nDH_LOG_LEVEL=\"debug\"\nDH_FEISHU_WEBHOOK=https://example.invalid/hook/x\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvEnvFile, envFile)
	t.Setenv(EnvInterval, "5m") // real env must win over the file

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Interval.D() != 5*time.Minute {
		t.Errorf("process env should beat the env file, got %s", cfg.Interval)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("env file value ignored: %q", cfg.LogLevel)
	}
}

func TestDurationParsing(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"90s"`), &d); err != nil || d.D() != 90*time.Second {
		t.Errorf("string duration failed: %v %s", err, d)
	}
	if err := json.Unmarshal([]byte(`120`), &d); err != nil || d.D() != 2*time.Minute {
		t.Errorf("numeric seconds failed: %v %s", err, d)
	}
	if err := json.Unmarshal([]byte(`"nope"`), &d); err == nil {
		t.Error("invalid duration should error")
	}
}

func TestParamAndTimeoutHelpers(t *testing.T) {
	s := Source{Params: map[string]string{"a": "1"}, Timeout: Duration(3 * time.Second)}
	if s.Param("a", "x") != "1" || s.Param("b", "x") != "x" {
		t.Error("Param helper is wrong")
	}
	if s.TimeoutOrDefault(time.Second) != 3*time.Second {
		t.Error("source timeout should win")
	}
	if (Source{}).TimeoutOrDefault(0) != 20*time.Second {
		t.Error("missing timeout must fall back to 20s")
	}
}
