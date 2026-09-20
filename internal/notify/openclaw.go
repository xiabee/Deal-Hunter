package notify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/model"
	"github.com/xiabee/deal-hunter/internal/redact"
)

// OpenClawRelay delivers through an existing OpenClaw installation, i.e.
// "let the assistant that is already wired to my chat send it". The relay is a
// fixed argv invocation (never a shell), so no operator-supplied message can
// become a command.
type OpenClawRelay struct {
	command string
	prefix  []string
	channel string
	target  string
	timeout time.Duration
}

// NewOpenClawRelay builds the relay from config.
func NewOpenClawRelay(cfg config.OpenClaw) (*OpenClawRelay, error) {
	cmdName := strings.TrimSpace(cfg.Command)
	if cmdName == "" {
		cmdName = "openclaw"
	}
	channel := strings.TrimSpace(cfg.Channel)
	if channel == "" {
		channel = "feishu"
	}
	timeout := cfg.Timeout.D()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &OpenClawRelay{
		command: cmdName,
		prefix:  append([]string(nil), cfg.Args...),
		channel: channel,
		target:  strings.TrimSpace(cfg.Target),
		timeout: timeout,
	}, nil
}

// Name implements Notifier.
func (o *OpenClawRelay) Name() string { return "openclaw-relay" }

// Ready reports whether a destination is known.
func (o *OpenClawRelay) Ready() bool { return o.target != "" }

// Args returns the full argv for a message (exported for tests and --dry-run).
func (o *OpenClawRelay) Args(m Message) []string {
	args := append([]string(nil), o.prefix...)
	args = append(args, "message", "send",
		"--channel", o.channel,
		"--target", o.target,
		"--message", o.text(m),
	)
	return args
}

// Send implements Notifier.
func (o *OpenClawRelay) Send(ctx context.Context, m Message) error {
	if o.target == "" {
		return fmt.Errorf("openclaw relay: no destination (set %s)", config.EnvOpenClawTarget)
	}
	c, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	cmd := exec.CommandContext(c, o.command, o.Args(m)...) // #nosec G204 -- fixed argv, no shell
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("openclaw relay: %w: %s", err, redact.Text(clipOutput(out.String())))
	}
	return nil
}

func (o *OpenClawRelay) text(m Message) string {
	var b strings.Builder
	b.WriteString(m.Title)
	if m.Intro != "" {
		b.WriteString("\n" + m.Intro)
	}
	if m.Kind == KindDaily {
		// Separating what is new from what is merely still open is the whole
		// point of the briefing, so the relay must not flatten it back into one
		// list the reader has to re-scan every morning.
		for _, sec := range SplitByAge(m.Deals, m.CreatedAt).Sections() {
			b.WriteString("\n\n" + sec.Title)
			for i := range sec.Deals {
				writeRelayDeal(&b, &sec.Deals[i])
			}
		}
		return b.String()
	}
	for i := range m.Deals {
		writeRelayDeal(&b, &m.Deals[i])
	}
	return b.String()
}

func writeRelayDeal(b *strings.Builder, d *model.Deal) {
	b.WriteString("\n\n" + Icon(d) + " " + d.Title + "  [" + fmt.Sprint(d.Score) + "]")
	if d.Summary != "" {
		b.WriteString("\n" + truncateRunes(strings.ReplaceAll(d.Summary, "\n", " "), 200))
	}
	if d.URL != "" {
		b.WriteString("\n" + d.URL)
	}
}

func clipOutput(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		return s[:300] + " … " + s[len(s)-250:]
	}
	return s
}
