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
	t.Setenv(EnvMinScore, "77")
	t.Setenv(EnvInterval, "45m")
	t.Setenv(EnvFeishuWebhook, "https://open.feishu.cn/open-apis/bot/v2/hook/redacted")
	t.Setenv(EnvFeishuSecret, "s3cr3t-value")
	t.Setenv(EnvFeishuEnabled, "false")
	t.Setenv(EnvDataDir, t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Filter.MinScore != 77 || cfg.Notify.Feishu.MinScore != 77 {
		t.Errorf("min score not applied: %d/%d", cfg.Filter.MinScore, cfg.Notify.Feishu.MinScore)
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
	if err := os.WriteFile(path, []byte(`{"interval":"5m","filter":{"min_score":70}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Interval.D() != 5*time.Minute || cfg.Filter.MinScore != 70 {
		t.Errorf("overrides lost: %+v", cfg.Filter)
	}
	if len(cfg.Sources) != len(DefaultSources()) {
		t.Errorf("sources should fall back to defaults, got %d", len(cfg.Sources))
	}
	if cfg.Notify.Feishu.MinScore == 0 {
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
		{"minscore", func(c *Config) { c.Filter.MinScore = 500 }, "min_score"},
		{"dupes", func(c *Config) {
			c.Sources = []Source{{Name: "x", Kind: KindRSS, URL: "https://a/"}, {Name: "x", Kind: KindRSS, URL: "https://b/"}}
		}, "duplicate"},
		{"kind", func(c *Config) { c.Sources = []Source{{Name: "x", Kind: "magic", URL: "https://a/"}} }, "unknown kind"},
		{"url", func(c *Config) { c.Sources = []Source{{Name: "x", Kind: KindRSS, URL: "ftp://a/"}} }, "must be http"},
		{"loglevel", func(c *Config) { c.LogLevel = "verbose" }, "log_level"},
		{"trust", func(c *Config) { c.Sources = []Source{{Name: "x", Kind: KindRSS, URL: "https://a/", Trust: 99}} }, "trust"},
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
	body := "# comment\nDH_MIN_SCORE=81\nDH_LOG_LEVEL=\"debug\"\nDH_FEISHU_WEBHOOK=https://example.invalid/hook/x\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvEnvFile, envFile)
	t.Setenv(EnvMinScore, "64") // real env must win over the file

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Filter.MinScore != 64 {
		t.Errorf("process env should beat the env file, got %d", cfg.Filter.MinScore)
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
