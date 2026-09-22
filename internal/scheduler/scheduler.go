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

const compactKeepDays = 60

// Run blocks until ctx is cancelled, doing one round immediately unless
// SkipFirst is set.
func (l *Loop) Run(ctx context.Context) error {
	if !l.SkipFirst {
		l.round(ctx, "startup")
		l.compact()
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
		l.round(ctx, "tick")
		l.compact()
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

// compact prunes the log at most once every seven days. The throttle is the
// marker on disk, not a round count: a host that gets redeployed daily used to
// reset the counter before it ever reached 48, so the log grew unbounded while
// the schedule looked configured.
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
	// Deleting rows is the one thing here that a restart cannot undo, so it waits
	// for proof that a backup actually succeeded recently. A host whose timer never
	// fired, or whose record is missing or unreadable, keeps its rows: not pruning
	// is recoverable, pruning without a net is not.
	if age, archive, err := store.BackupAge(st.Dir()); err != nil {
		l.Log.Warn("compaction skipped: no usable record of a successful backup", "err", err)
		return
	} else if age > store.BackupPatience {
		l.Log.Warn("compaction skipped: the last successful backup is too old to lean on",
			"backup_age_hours", int(age.Hours()), "archive", archive)
		return
	}
	if err := st.Compact(compactKeepDays); err != nil {
		l.Log.Warn("compact failed", "err", err)
		return
	}
	// A marker that fails to land means the next round prunes again, and - worse
	// for the operator - doctor keeps saying "never compacted" after a sweep that
	// really happened. Say so instead of swallowing it.
	if err := st.PutState(store.StateLastCompact, time.Now().UTC().Format(time.RFC3339)); err != nil {
		l.Log.Warn("compact marker not written", "err", err)
		return
	}
	l.Log.Info("store compacted", "keep_days", compactKeepDays)
}
