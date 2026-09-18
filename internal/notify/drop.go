package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

// FileDrop writes findings as Markdown and JSON into a directory. This is the
// OpenClaw-side contract: Deal-Hunter only produces files, and the assistant
// reads them over the loopback HTTP API, so no OpenClaw credential is ever
// touched by this program.
type FileDrop struct {
	dir    string
	prefix string
}

// NewFileDrop prepares dir (0750) and returns the backend.
func NewFileDrop(dir, prefix string) (*FileDrop, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("filedrop: dir is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("filedrop: mkdir %s: %w", dir, err)
	}
	if prefix == "" {
		prefix = "deal-hunter"
	}
	return &FileDrop{dir: dir, prefix: prefix}, nil
}

// Name implements Notifier.
func (f *FileDrop) Name() string { return "openclaw-drop" }

// Dir reports where files land.
func (f *FileDrop) Dir() string { return f.dir }

// Send writes latest.md, latest.json and an append-only daily log.
func (f *FileDrop) Send(_ context.Context, m Message) error {
	md := renderMarkdown(m)
	if err := f.write(f.prefix+"-latest.md", []byte(md)); err != nil {
		return err
	}
	js, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("filedrop: encode json: %w", err)
	}
	if err := f.write(f.prefix+"-latest.json", js); err != nil {
		return err
	}
	path := filepath.Join(f.dir, f.prefix+"-"+time.Now().UTC().Format("20060102")+".jsonl")
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("filedrop: open %s: %w", path, err)
	}
	defer fh.Close()
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := fh.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("filedrop: append: %w", err)
	}
	return nil
}

func (f *FileDrop) write(name string, b []byte) error {
	tmp := filepath.Join(f.dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return fmt.Errorf("filedrop: write %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(f.dir, name)); err != nil {
		return fmt.Errorf("filedrop: rename %s: %w", name, err)
	}
	return nil
}

func renderMarkdown(m Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", m.Title)
	if m.Intro != "" {
		fmt.Fprintf(&b, "%s\n\n", m.Intro)
	}
	fmt.Fprintf(&b, "_生成时间 %s · 共 %d 条_\n\n", m.CreatedAt.Format(time.RFC3339), len(m.Deals))
	for i := range m.Deals {
		d := &m.Deals[i]
		fmt.Fprintf(&b, "## %s %s\n\n", Icon(d), d.Title)
		fmt.Fprintf(&b, "- 置信分：**%d** · 来源：`%s`\n", d.Score, d.Source)
		if len(d.Vendors) > 0 {
			fmt.Fprintf(&b, "- 厂商：%s\n", strings.Join(d.Vendors, "、"))
		}
		if d.URL != "" {
			fmt.Fprintf(&b, "- 链接：%s\n", d.URL)
		}
		if !d.PublishedAt.IsZero() {
			fmt.Fprintf(&b, "- 发布：%s\n", d.PublishedAt.Format("2006-01-02 15:04 MST"))
		}
		if s := strings.TrimSpace(d.Summary); s != "" {
			fmt.Fprintf(&b, "\n> %s\n", strings.ReplaceAll(s, "\n", " "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Console writes messages to a writer, used for dev runs and the CI gate.
type Console struct{ out io.Writer }

// NewConsole returns a console backend on stdout.
func NewConsole() *Console { return &Console{out: os.Stdout} }

// NewConsoleWriter returns a console backend bound to an arbitrary writer.
func NewConsoleWriter(w io.Writer) *Console { return &Console{out: w} }

// Name implements Notifier.
func (c *Console) Name() string { return "console" }

// Send implements Notifier.
func (c *Console) Send(_ context.Context, m Message) error {
	_, err := fmt.Fprint(c.out, m.Plain())
	return err
}

// DealsOf exposes the deals carried by a message for assertions in tests.
func DealsOf(m Message) []model.Deal { return m.Deals }
