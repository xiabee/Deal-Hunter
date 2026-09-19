package store

import (
	"os"
	"path/filepath"
	"strings"
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

func TestTitleIndexFoldsRepostsAndKeepsThemOutOfTheDigest(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	first := &model.Deal{URL: "https://linux.do/t/1", Title: "[分享创造] 做了一个家用 WMS", Score: 71, DiscoveredAt: time.Now().UTC()}
	repost := &model.Deal{URL: "https://www.v2ex.com/t/2", Title: "做了一个家用 WMS", Score: 68, DiscoveredAt: time.Now().UTC()}
	if err := st.Save(first); err != nil {
		t.Fatal(err)
	}
	if fp, ok := st.TitleClash(first.Title, first.Fingerprint); ok {
		t.Fatalf("the first finding must not clash with itself: %s", fp)
	}
	fp, ok := st.TitleClash(repost.Title, repost.Fingerprint)
	if !ok || fp != first.Fingerprint {
		t.Fatalf("repost should clash with the first finding, got %q,%v", fp, ok)
	}

	// A repeat is still recorded, but tagged, and never resurfaces in a digest.
	repost.Meta = map[string]string{"dup_of": first.Fingerprint}
	if err := st.Save(repost); err != nil {
		t.Fatal(err)
	}
	for _, d := range st.Pending(50, time.Time{}) {
		if d.Fingerprint == repost.Fingerprint {
			t.Error("a repost must not be queued for the digest")
		}
	}
	if got := st.Stats().Duplicates; got != 1 {
		t.Errorf("Duplicates = %d, want 1", got)
	}

	// The index is rebuilt from disk, so a restart keeps the fold.
	dir := st.Dir()
	st.Close()
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got, ok := again.TitleClash(repost.Title, "other"); !ok || got != first.Fingerprint {
		t.Errorf("title index lost after reopen: %q,%v", got, ok)
	}
	if got := again.Stats().Duplicates; got != 1 {
		t.Errorf("Duplicates after reopen = %d, want 1", got)
	}
}

// Records written before the fold existed carry no dup_of tag; they must still
// be folded when read, or the list keeps showing the same announcement twice
// until those rows age out on their own.
func TestRepostsWithoutTagsAreFoldedWhenLoaded(t *testing.T) {
	dir := t.TempDir()
	rows := []string{
		`{"fingerprint":"a1","url":"https://linux.do/t/1","title":"智谱 GLM-5.3-flash 限时免费开放","source":"a","score":80,"discovered_at":"2026-09-19T01:00:00Z"}`,
		`{"fingerprint":"b2","url":"https://www.v2ex.com/t/2","title":"【公告】智谱 GLM-5.3-flash 限时免费开放！","source":"b","score":74,"discovered_at":"2026-09-19T02:00:00Z"}`,
	}
	if err := os.WriteFile(filepath.Join(dir, "deals.jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got := map[string]model.Deal{}
	for _, d := range s.Recent(10) {
		got[d.Fingerprint] = d
	}
	if len(got) != 2 {
		t.Fatalf("both rows should load, got %d", len(got))
	}
	if got["b2"].Meta["dup_of"] != "a1" {
		t.Errorf("the later repost should be folded to a1, got %q", got["b2"].Meta["dup_of"])
	}
	if got["a1"].Meta["dup_of"] != "" {
		t.Error("the first finding must stay visible")
	}
	if st := s.Stats(); st.Duplicates != 1 {
		t.Errorf("Duplicates = %d, want 1", st.Duplicates)
	}
	for _, d := range s.Pending(50, time.Time{}) {
		if d.Fingerprint == "b2" {
			t.Error("a folded legacy repost must not reach the digest")
		}
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
