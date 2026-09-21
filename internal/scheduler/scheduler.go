// Package scheduler repeats collection rounds and sends the daily briefing.
package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/pipeline"
	"github.com/xiabee/deal-hunter/internal/store"
)

// Loop is the daemon driver.
type Loop struct {
	App       *pipeline.App
	Cfg       *config.Config
	Log       *slog.Logger
	SkipFirst bool
}

const (
	compactEveryRounds = 48
	compactKeepDays    = 60
)

// Run blocks until ctx is cancelled, doing one round immediately unless
// SkipFirst is set.
func (l *Loop) Run(ctx context.Context) error {
	rounds := 0
	if !l.SkipFirst {
		rounds++
		l.round(ctx, "startup")
	}
	for {
		delay := l.nextDelay()
		l.Log.Info("waiting for next round", "in", delay.Round(time.Second), "interval", l.Cfg.Interval.String())
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		if ctx.Err() != nil {
			return nil
		}
		rounds++
		l.round(ctx, "tick")
		if rounds%compactEveryRounds == 0 {
			l.compact()
		}
	}
}

func (l *Loop) round(ctx context.Context, trigger string) {
	run, err := l.App.RunOnce(ctx, trigger)
	if err != nil && ctx.Err() == nil {
		l.Log.Warn("round finished with errors", "trigger", trigger, "err", err)
	}
	if run == nil {
		return
	}
	now := time.Now()
	// The briefing is the day's only scheduled message, so a failed send stays
	// due and is retried on the next round instead of being written off.
	if l.App.DailyDue(now) {
		if err := l.App.SendDaily(ctx, now); err != nil && ctx.Err() == nil {
			l.Log.Warn("daily briefing failed", "err", err)
		}
	}
}

// nextDelay adds jitter so a restart storm cannot hammer the same feeds.
func (l *Loop) nextDelay() time.Duration {
	base := l.Cfg.Interval.D()
	if base <= 0 {
		base = 30 * time.Minute
	}
	j := l.Cfg.Jitter.D()
	if j <= 0 {
		return base
	}
	return base + time.Duration(rand.Int63n(int64(j)))
}

func (l *Loop) compact() {
	st := l.App.Store()
	if b, ok := st.GetState(store.StateLastCompact); ok {
		var s string
		if json.Unmarshal(b, &s) == nil {
			if t, err := time.Parse(time.RFC3339, s); err == nil && time.Since(t) < 7*24*time.Hour {
				return
			}
		}
	}
	if err := st.Compact(compactKeepDays); err != nil {
		l.Log.Warn("compact failed", "err", err)
		return
	}
	_ = st.PutState(store.StateLastCompact, time.Now().UTC().Format(time.RFC3339))
	l.Log.Info("store compacted", "keep_days", compactKeepDays)
}
