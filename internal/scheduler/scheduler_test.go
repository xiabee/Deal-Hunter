package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/httpx"
	"github.com/xiabee/deal-hunter/internal/model"
	"github.com/xiabee/deal-hunter/internal/pipeline"
	"github.com/xiabee/deal-hunter/internal/store"
)

type stubFetcher struct{ body []byte }

func (f *stubFetcher) Get(_ context.Context, u string, _ map[string]string) (*httpx.Response, error) {
	return &httpx.Response{Status: 200, FinalURL: u, Body: f.body}, nil
}

var stubFeed = []byte(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>t</title>
<item><title>某云模型限时免费一周</title><link>https://example.test/a</link>
<guid>https://example.test/a</guid><pubDate>Sat, 20 Sep 2026 09:00:00 GMT</pubDate>
<description>新模型免费用一周</description></item></channel></rss>`)

func newApp(t *testing.T) (*pipeline.App, *config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Notify.Console = false
	cfg.Notify.Daily.Enabled = false
	cfg.Notify.Urgent.Enabled = false
	cfg.Notify.Event.Enabled = false
	cfg.Sources = []config.Source{{Name: "rss", Kind: config.KindRSS,
		URL: "https://example.test/a.rss", Trust: 8}}
	app, err := pipeline.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
		pipeline.WithFetcher(&stubFetcher{body: stubFeed}))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	t.Cleanup(func() { app.Close() })
	return app, cfg
}

// seed writes rows through the store's own writer, so the sweep is judged on
// exactly what a real round leaves behind.
func seed(t *testing.T, app *pipeline.App, daysOld int, score int, title string) *model.Deal {
	t.Helper()
	d := &model.Deal{
		Title: title, URL: "https://example.test/" + title, Score: score,
		Source: "rss", Category: model.CatAIFree,
		DiscoveredAt: time.Now().UTC().AddDate(0, 0, -daysOld).Truncate(time.Second),
	}
	if err := app.Store().Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return d
}

func logLines(t *testing.T, app *pipeline.App) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(app.Store().Dir(), "deals.jsonl"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

func markerOnDisk(t *testing.T, app *pipeline.App) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(app.Store().Dir(), "state.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("read state: %v", err)
	}
	return strings.Contains(string(b), store.StateLastCompact)
}

func setMarker(t *testing.T, app *pipeline.App, at time.Time) {
	t.Helper()
	if err := app.Store().PutState(store.StateLastCompact, at.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("PutState: %v", err)
	}
}

func TestSweepPrunesOldLowScoreRows(t *testing.T) {
	app, cfg := newApp(t)
	old := seed(t, app, 90, 30, "九十天前的低分条目")
	fresh := seed(t, app, 1, 92, "昨天的条目")

	(&Loop{App: app, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).compact()

	after := logLines(t, app)
	if strings.Contains(after, old.Fingerprint) {
		t.Errorf("a 90-day-old row scoring 30 should have been pruned:\n%s", after)
	}
	if !strings.Contains(after, fresh.Fingerprint) {
		t.Errorf("the sweep must not touch recent rows:\n%s", after)
	}
	if !markerOnDisk(t, app) {
		t.Error("the sweep wrote no marker, so the next round would sweep again")
	}
}

// 7 天是这事的唯一节流：改成每轮都检查之后，"刚剪过"必须让后面几十轮什么都不做。
func TestSweepWaitsSevenDaysBetweenRuns(t *testing.T) {
	app, cfg := newApp(t)
	old := seed(t, app, 90, 30, "九十天前的低分条目")
	setMarker(t, app, time.Now().Add(-24*time.Hour))

	(&Loop{App: app, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).compact()

	if got := logLines(t, app); !strings.Contains(got, old.Fingerprint) {
		t.Errorf("only 24h since the last sweep, the row should still be there:\n%s", got)
	}
}

// 生产上真正的缺陷：触发器数的是**进程内**轮次（48 轮），而每天部署一次的机器
// 永远凑不满 —— 记号已经在盘上了，判据却还在内存里。
func TestSweepIsNotResetByARestart(t *testing.T) {
	app, cfg := newApp(t)
	old := seed(t, app, 90, 30, "九十天前的低分条目")
	cfg.Interval = config.Duration(24 * time.Hour) // 一轮之后就该一直卡在等待里

	loop := &Loop{App: app, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()

	deadline := time.After(10 * time.Second)
	for {
		if markerOnDisk(t, app) {
			break
		}
		select {
		case <-done:
			t.Fatal("Run returned before the first sweep happened")
		case <-deadline:
			t.Fatalf("no sweep within the first round after a start; "+
				"the old row is still there: %v", strings.Contains(logLines(t, app), old.Fingerprint))
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
	if got := logLines(t, app); strings.Contains(got, old.Fingerprint) {
		t.Errorf("marker says it swept but the row is still in the log:\n%s", got)
	}
}

// 记号读不出来时必须当作"该剪"，而不是当作"刚剪过"——否则一个坏字节就把剪枝永久关掉。
func TestSweepIgnoresAnUnreadableMarker(t *testing.T) {
	app, cfg := newApp(t)
	old := seed(t, app, 90, 30, "九十天前的低分条目")
	if err := app.Store().PutState(store.StateLastCompact, "not-a-time"); err != nil {
		t.Fatalf("PutState: %v", err)
	}

	(&Loop{App: app, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).compact()

	if got := logLines(t, app); strings.Contains(got, old.Fingerprint) {
		t.Errorf("an unreadable marker must not disable the sweep:\n%s", got)
	}
	b, ok := app.Store().GetState(store.StateLastCompact)
	if !ok {
		t.Fatal("the sweep left no marker behind")
	}
	var s string
	if json.Unmarshal(b, &s) != nil {
		t.Fatalf("marker not replaced: %s", b)
	}
	if _, perr := time.Parse(time.RFC3339, s); perr != nil {
		t.Errorf("marker should now hold a real timestamp, got %q", s)
	}
}
