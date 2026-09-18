package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

func deal(title, url string, score int) *model.Deal {
	return &model.Deal{
		Title: title, URL: url, Score: score,
		Source: "unit-test", Category: model.CatAIFree,
		DiscoveredAt: time.Now().UTC().Truncate(time.Second),
	}
}

func TestOpenRoundTripAndDedup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d := deal("智谱 GLM-5.3-flash 限时免费", "https://example.com/a", 88)
	if s.Seen(d.Fingerprint) {
		t.Fatal("fresh store must not know the item")
	}
	if err := s.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !s.Seen(d.Fingerprint) {
		t.Fatal("item should be seen after Save")
	}
	if err := s.Save(deal("same", "https://example.com/a", 10)); err != nil {
		t.Fatalf("Save dup: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if !reopened.Seen(d.Fingerprint) {
		t.Fatal("dedup state must survive a restart")
	}
	if got := reopened.Recent(10); len(got) != 1 {
		t.Fatalf("expected 1 collapsed deal, got %d", len(got))
	}
}

func TestTornLastLineIsIgnored(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(deal("good", "https://example.com/good", 70)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Simulate a crash mid-write.
	f, _ := os.OpenFile(filepath.Join(dir, "deals.jsonl"), os.O_APPEND|os.O_WRONLY, 0o640)
	f.WriteString(`{"fingerprint":"broken","title":"cut off`)
	f.Close()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("store must survive a torn line: %v", err)
	}
	defer reopened.Close()
	if got := reopened.Recent(10); len(got) != 1 || got[0].Title != "good" {
		t.Fatalf("unexpected recovery: %+v", got)
	}
}

func TestStateCursorRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok := s.GetState("missing"); ok {
		t.Fatal("unknown key should be absent")
	}
	ids := []string{"a:free", "b:free"}
	if err := s.PutState("cursor", ids); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	b, ok := s.GetState("cursor")
	if !ok {
		t.Fatal("key should exist")
	}
	if string(b) != `["a:free","b:free"]` {
		t.Fatalf("unexpected payload: %s", b)
	}
}

func TestMarkPushedDrivesPending(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	high := deal("高价值羊毛", "https://example.com/high", 90)
	low := deal("小羊毛", "https://example.com/low", 50)
	for _, d := range []*model.Deal{high, low} {
		if err := s.Save(d); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Pending(60, time.Time{}); len(got) != 1 || got[0].Title != "高价值羊毛" {
		t.Fatalf("Pending(60) = %+v", got)
	}
	if err := s.MarkPushed(high.Fingerprint); err != nil {
		t.Fatalf("MarkPushed: %v", err)
	}
	if !s.Pushed(high.Fingerprint) {
		t.Fatal("should be marked pushed")
	}
	if got := s.Pending(60, time.Time{}); len(got) != 0 {
		t.Fatalf("pushed item must leave the pending queue: %+v", got)
	}
	// The pushed flag must be visible on the collapsed record.
	all := s.Recent(10)
	if len(all) != 2 {
		t.Fatalf("expected 2 collapsed records, got %d", len(all))
	}
	var found bool
	for _, d := range all {
		if d.Fingerprint == high.Fingerprint && d.Meta["pushed"] == "true" {
			found = true
		}
	}
	if !found {
		t.Error("pushed flag should be persisted on the deal record")
	}
}

func TestCompactCollapsesAndKeepsLatest(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d1 := deal("one", "https://example.com/1", 60)
	d2 := deal("two", "https://example.com/2", 70)
	for _, d := range []*model.Deal{d1, d2, d1} {
		if err := s.Save(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkPushed(d1.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(30); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := s.Save(deal("three", "https://example.com/3", 80)); err != nil {
		t.Fatalf("Save after compact must still work: %v", err)
	}
	if !s.Pushed(d1.Fingerprint) {
		t.Error("compact must preserve the pushed marker")
	}
	if got := len(s.Recent(10)); got != 3 {
		t.Errorf("expected 3 deals after compact, got %d", got)
	}
	if st := s.Stats(); st.DealsSeen != 3 {
		t.Errorf("stats DealsSeen = %d, want 3", st.DealsSeen)
	}
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open("   "); err == nil {
		t.Fatal("expected an error for an empty dir")
	}
}
