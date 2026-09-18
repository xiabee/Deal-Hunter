package redact

import (
	"strings"
	"testing"
)

const hookPath = "https://open.feishu.cn/open-apis/bot/v2/hook/"

func TestURLMasksWebhookToken(t *testing.T) {
	secret := hookPath + "abcdef1234567890abcdef1234567890"
	got := URL(secret)
	if strings.Contains(got, "abcdef1234567890abcdef") {
		t.Fatalf("token leaked through URL(): %s", got)
	}
	if !strings.Contains(got, "open.feishu.cn") || !strings.Contains(got, "/hook/") {
		t.Errorf("useful context lost: %s", got)
	}
	if URL("") != "" {
		t.Error("empty input must stay empty")
	}
}

func TestURLMasksUserinfo(t *testing.T) {
	got := URL("https://admin:password123@mail.example.com/inbox") // secretlint:ignore synthetic credential used to prove masking
	if strings.Contains(got, "password123") {
		t.Errorf("password leaked: %s", got)
	}
	if !strings.Contains(got, "mail.example.com") {
		t.Errorf("host lost: %s", got)
	}
}

func TestTokenFingerprint(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"short":             "*****",
		"12345678":          "********",
		"abcdefghij":        "abcd…****ij",
		"   spacedtoken   ": "spac…****en",
	}
	for in, want := range cases {
		if got := Token(in); got != want {
			t.Errorf("Token(%q) = %q, want %q", in, got, want)
		}
	}
	if Token(strings.Repeat("x", 200)) == strings.Repeat("x", 200) {
		t.Error("long secrets must be shortened")
	}
}

func TestTextMasksAssignments(t *testing.T) {
	in := "upstream rejected: authorization: Bearer abcdef123456, api_key = \"super-secret-value\", webhook: " + hookPath + "deadbeefcafebabe"
	out := Text(in)
	for _, leak := range []string{"abcdef123456", "super-secret-value", "deadbeefcafebabe"} {
		if strings.Contains(out, leak) {
			t.Errorf("Text leaked %q: %s", leak, out)
		}
	}
	for _, keep := range []string{"upstream rejected", "Bearer"} {
		if !strings.Contains(out, keep) {
			t.Errorf("Text dropped %q: %s", keep, out)
		}
	}
}

func TestQueryKeepsKeysOnly(t *testing.T) {
	got := Query("https://api.example.com/v1/search?q=deal&token=abcdef123456")
	if strings.Contains(got, "abcdef123456") {
		t.Errorf("query value leaked: %s", got)
	}
	if !strings.Contains(got, "token=***") || !strings.Contains(got, "q=***") {
		t.Errorf("parameter names should remain visible: %s", got)
	}
	if Query("not a url") != "not a url" {
		t.Error("unparseable input should pass through unchanged")
	}
}
