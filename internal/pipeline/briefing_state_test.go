package pipeline

import (
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
)

// BriefingState is the single answer to "where does today's briefing stand" - the panel
// reads it through App.Daily, `dealhunter doctor` reads it directly. Fixed clocks here
// because the whole point is which side of the slot you are on; the wall clock cannot
// be part of a verdict.
func TestBriefingStateReadsTheSlotAndTheCursor(t *testing.T) {
	today := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		cursor  *time.Time // nil: never sent a briefing
		now     time.Time
		passed  bool
		sent    bool
		nextHMM string
		nextDay string
	}{
		{
			name: "到点之前", now: today.Add(8 * time.Hour),
			nextHMM: "09:00", nextDay: "09-23",
		},
		{
			name: "到点之后还没发过", now: today.Add(9*time.Hour + 30*time.Minute),
			passed:  true,
			nextHMM: "09:00", nextDay: "09-24",
		},
		{
			name:   "今天已经发过",
			cursor: ptr(today.Add(9 * time.Hour)), now: today.Add(20 * time.Hour),
			passed: true, sent: true,
			nextHMM: "09:00", nextDay: "09-24",
		},
		{
			name:   "昨天发过而今天这份漏了",
			cursor: ptr(today.Add(9*time.Hour - 24*time.Hour)), now: today.Add(20 * time.Hour),
			passed: true, sent: false,
			nextHMM: "09:00", nextDay: "09-24",
		},
		{
			// 昨天 23:00 那份不算今天：新的一天还没到点，两个字段都必须跟着换日。
			name:   "跨过本地零点后昨天那份就不算今天",
			cursor: ptr(today.Add(23 * time.Hour)), now: today.Add(25 * time.Hour),
			passed: false, sent: false,
			nextHMM: "09:00", nextDay: "09-24",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := testApp(t, &cannedFetcher{}, nil, func(c *config.Config) {
				c.Timezone = "UTC" // 让测试里的"本地"和断言里写的是同一个地方
				c.Notify.Daily.At = "09:00"
			})
			if tc.cursor != nil {
				if err := app.setStateTime(dailyCursor, *tc.cursor); err != nil {
					t.Fatalf("setStateTime: %v", err)
				}
			}
			got := BriefingState(app.cfg, app.st, tc.now)
			if !got.Enabled {
				t.Fatal("the briefing is enabled in this config")
			}
			if got.SlotPassed != tc.passed {
				t.Errorf("SlotPassed = %v, want %v (now=%s at=%s)", got.SlotPassed, tc.passed, tc.now.Format(time.RFC3339), got.At)
			}
			if got.SentToday != tc.sent {
				t.Errorf("SentToday = %v, want %v (cursor=%v)", got.SentToday, tc.sent, tc.cursor)
			}
			if got.Next.In(time.UTC).Format("01-02") != tc.nextDay || got.Next.In(time.UTC).Format("15:04") != tc.nextHMM {
				t.Errorf("Next = %s, want %s %s", got.Next.Format(time.RFC3339), tc.nextDay, tc.nextHMM)
			}
			if tc.cursor == nil {
				if !got.Last.IsZero() {
					t.Errorf("a store that never sent must report no last time, got %s", got.Last)
				}
				return
			}
			if !got.Last.Equal(*tc.cursor) {
				t.Errorf("Last = %s, want %s", got.Last.Format(time.RFC3339), tc.cursor.Format(time.RFC3339))
			}
		})
	}
}

// The panel and `doctor` must not be able to disagree, and both go through App.Daily:
// the method has to be the same computation, not a copy that can drift.
func TestAppDailyIsBriefingState(t *testing.T) {
	app := testApp(t, &cannedFetcher{}, nil, func(c *config.Config) {
		c.Timezone = "UTC"
		c.Notify.Daily.At = "09:00"
	})
	now := time.Date(2026, 9, 23, 20, 0, 0, 0, time.UTC)
	if err := app.setStateTime(dailyCursor, now.Add(-9*time.Hour)); err != nil {
		t.Fatalf("setStateTime: %v", err)
	}
	via := app.Daily(now)
	direct := BriefingState(app.cfg, app.st, now)
	if via != direct {
		t.Errorf("App.Daily and BriefingState disagree:\n via   %+v\n direct %+v", via, direct)
	}
}

func ptr[T any](v T) *T { return &v }
