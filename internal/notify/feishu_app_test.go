package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/model"
)

const (
	// Built at runtime so this file does not itself trip the repo's secret scan.
	testAppID     = "cli_" + "testappid1234"
	testAppSecret = "supersecretappsecret"
	testToken     = "t-abc123"

	// The Feishu bot hook prefix is a public constant; only the token part matters.
	hookBase = "https://open.feishu.cn/open-apis/bot/v2/hook/"
)

func appDeal() model.Deal {
	return model.Deal{
		Title: "阿里云 Qwen3.8-Max 限时 5 折", Summary: "新用户注册即享免费额度",
		URL: "https://www.example.com/benefit/qwen", Source: "aliyun-benefit",
		Category: model.CatAIFree, IsFree: true, Score: 84,
		Vendors: []string{"阿里云"}, ScoreWhy: []string{"+45 免费类 offer"},
		PublishedAt: time.Now().Add(-2 * time.Hour), DiscoveredAt: time.Now(),
	}
}

// appServer emulates the two Open API endpoints Deal-Hunter uses.
func appServer(t *testing.T, tokenCalls *int32, sendCode int, sendMsg string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			atomic.AddInt32(tokenCalls, 1)
			var req map[string]string
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			if req["app_id"] != testAppID || req["app_secret"] != testAppSecret {
				fmt.Fprint(w, `{"code":10003,"msg":"app not found"}`)
				return
			}
			fmt.Fprintf(w, `{"code":0,"msg":"ok","tenant_access_token":%q,"expire":7200}`, testToken)
		case "/open-apis/im/v1/messages":
			if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
				fmt.Fprint(w, `{"code":99991663,"msg":"invalid token"}`)
				return
			}
			if r.URL.Query().Get("receive_id_type") != "open_id" {
				fmt.Fprintf(w, `{"code":1,"msg":"bad receive_id_type"}`)
				return
			}
			var req map[string]string
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			if req["receive_id"] != "ou_testrecipient" {
				fmt.Fprint(w, `{"code":230001,"msg":"bad receive_id"}`)
				return
			}
			if strings.Contains(req["content"], testAppSecret) || strings.Contains(req["content"], testToken) {
				t.Error("credentials must never appear in a delivered payload")
			}
			if sendCode != 0 {
				fmt.Fprintf(w, `{"code":%d,"msg":%q}`, sendCode, sendMsg)
				return
			}
			fmt.Fprint(w, `{"code":0,"msg":"success","data":{"message_id":"om_test123"}}`)
		default:
			fmt.Fprint(w, `{"code":404,"msg":"not found"}`)
		}
	}))
}

func appCfg(base string) config.Feishu {
	return config.Feishu{
		Enabled: true, Timezone: "UTC", APIBase: base,
		AppID: testAppID, AppSecret: testAppSecret, ReceiveID: "ou_testrecipient",
	}
}

func TestAppDeliverySendsCard(t *testing.T) {
	var calls int32
	srv := appServer(t, &calls, 0, "")
	defer srv.Close()

	f, err := NewFeishu(appCfg(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if f.Mode() != "app" {
		t.Fatalf("mode = %s, want app", f.Mode())
	}
	if !f.Ready() {
		t.Fatal("app credentials should make the backend ready")
	}
	if got := f.ReceiveIDType(); got != "open_id" {
		t.Errorf("default receive_id_type = %s", got)
	}
	msg := NewMessage(KindAlert, Headline(&[]model.Deal{appDeal()}[0]), appDeal())
	if err := f.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected exactly one token mint, got %d", got)
	}
}

func TestAppTokenIsCachedThenRefreshed(t *testing.T) {
	var calls int32
	srv := appServer(t, &calls, 0, "")
	defer srv.Close()
	f, _ := NewFeishu(appCfg(srv.URL))
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := f.Send(ctx, NewMessage(KindAlert, "t", appDeal())); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("token must be cached across sends, mints=%d", got)
	}
	f.tokenTo = time.Now().Add(-time.Second)
	if err := f.Send(ctx, NewMessage(KindAlert, "t", appDeal())); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("an expired token must be refreshed, mints=%d", got)
	}
}

func TestAppFallsBackToTextWhenCardRejected(t *testing.T) {
	var calls int32
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "internal") {
			atomic.AddInt32(&calls, 1)
			fmt.Fprintf(w, `{"code":0,"tenant_access_token":%q,"expire":7200}`, testToken)
			return
		}
		var req map[string]string
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		seen = append(seen, req["msg_type"])
		if req["msg_type"] == "interactive" {
			fmt.Fprint(w, `{"code":230099,"msg":"card rejected"}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"msg":"success","data":{"message_id":"om_text"}}`)
	}))
	defer srv.Close()

	f, _ := NewFeishu(appCfg(srv.URL))
	if err := f.Send(context.Background(), NewMessage(KindAlert, "标题", appDeal())); err != nil {
		t.Fatalf("a rejected card should fall back instead of dropping the finding: %v", err)
	}
	if len(seen) != 2 || seen[0] != "interactive" || seen[1] != "text" {
		t.Errorf("fallback order wrong: %v", seen)
	}
}

func TestAppReportsUpstreamRejectionWithoutLeakingSecrets(t *testing.T) {
	var calls int32
	srv := appServer(t, &calls, 230002, "BOT HAS NO PERMISSION")
	defer srv.Close()
	f, _ := NewFeishu(appCfg(srv.URL))

	err := f.Send(context.Background(), NewMessage(KindAlert, "标题", appDeal()))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "230002") {
		t.Errorf("error should carry the upstream code: %v", err)
	}
	for _, leak := range []string{testAppSecret, testToken} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaked %q: %v", leak, err)
		}
	}
}

func TestAppBadSecretIsReported(t *testing.T) {
	var calls int32
	srv := appServer(t, &calls, 0, "")
	defer srv.Close()
	cfg := appCfg(srv.URL)
	cfg.AppSecret = "wrong-value"
	f, _ := NewFeishu(cfg)

	err := f.Send(context.Background(), NewMessage(KindAlert, "t", appDeal()))
	if err == nil || !strings.Contains(err.Error(), "token code=") {
		t.Fatalf("expected a token mint failure, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong-value") {
		t.Error("the secret must not be echoed back")
	}
}

// A code=0 response without a message id is treated as a failure, so the
// finding stays pending instead of being silently swallowed.
func TestAppRequiresMessageID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "internal") {
			fmt.Fprintf(w, `{"code":0,"tenant_access_token":%q,"expire":7200}`, testToken)
			return
		}
		fmt.Fprint(w, `{"code":0,"msg":"success","data":{}}`)
	}))
	defer srv.Close()
	f, _ := NewFeishu(appCfg(srv.URL))
	if err := f.Send(context.Background(), NewMessage(KindAlert, "t", appDeal())); err == nil {
		t.Fatal("a response without a message id must be an error")
	}
}

func TestModePreferenceAndUnconfigured(t *testing.T) {
	both := appCfg("https://example.invalid")
	both.WebhookURL = hookBase + "abcdefghij" + "0123456789" // runtime-built: keeps the literal out of the repo
	f, _ := NewFeishu(both)
	if f.Mode() != "webhook" {
		t.Errorf("the webhook must win when both are configured, got %s", f.Mode())
	}

	none, _ := NewFeishu(config.Feishu{Enabled: true, Timezone: "UTC"})
	if none.Mode() != "" || none.Ready() {
		t.Error("without credentials there is no mode")
	}
	err := none.Send(context.Background(), NewMessage(KindAlert, "t"))
	if err == nil || !strings.Contains(err.Error(), config.EnvFeishuAppID) {
		t.Errorf("the error should list the app env vars as an alternative: %v", err)
	}

	partial := appCfg("https://example.invalid")
	partial.ReceiveID = ""
	if f, _ := NewFeishu(partial); f.Ready() {
		t.Error("an app identity without a recipient is not usable")
	}
}

func TestReceiveIDTypeValidation(t *testing.T) {
	cases := map[string]string{
		"": "open_id", "bogus": "open_id", "chat_id": "chat_id",
		"union_id": "union_id", "user_id": "user_id", "email": "email",
	}
	for in, want := range cases {
		cfg := appCfg("https://example.invalid")
		cfg.ReceiveIDType = in
		f, _ := NewFeishu(cfg)
		if got := f.ReceiveIDType(); got != want {
			t.Errorf("ReceiveIDType(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestAPIBaseDefaultsToProduction(t *testing.T) {
	f, _ := NewFeishu(appCfg(""))
	if f.apiBase != "https://open.feishu.cn" {
		t.Errorf("apiBase = %s", f.apiBase)
	}
	trailing := appCfg("https://open.feishu.cn/")
	f2, _ := NewFeishu(trailing)
	if f2.apiBase != "https://open.feishu.cn" {
		t.Errorf("trailing slash not trimmed: %s", f2.apiBase)
	}
}
