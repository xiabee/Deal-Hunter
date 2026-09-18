// Package store persists discovered deals and per-source cursor state.
//
// The format is deliberately dependency free: an append-only JSONL log of
// deals plus an atomically rewritten state.json for source cursors. A personal
// single-node tool does not need an embedded database.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xiabee/deal-hunter/internal/model"
)

// Store is safe for concurrent use.
type Store struct {
	dir string

	mu      sync.Mutex
	seen    map[string]time.Time
	high    map[string]time.Time // pushed, or held for digest
	state   map[string]json.RawMessage
	log     *os.File
	scanned int
}

// Stats summarizes what the store holds.
type Stats struct {
	DealsSeen   int
	PushedSeen  int
	StateKeys   int
	OldestEntry time.Time
	NewestEntry time.Time
	DirSizeKB   int64
}

// Open prepares dir, loads the existing log and returns a writable store.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("store: empty dir")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
	}
	s := &Store{
		dir:   dir,
		seen:  map[string]time.Time{},
		high:  map[string]time.Time{},
		state: map[string]json.RawMessage{},
	}
	if err := s.loadState(); err != nil {
		return nil, err
	}
	if err := s.loadLog(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "deals.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("store: open log: %w", err)
	}
	s.log = f
	return s, nil
}

// Dir reports the on-disk location of the store.
func (s *Store) Dir() string { return s.dir }

func (s *Store) loadState() error {
	b, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read state: %w", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, &s.state); err != nil {
		return fmt.Errorf("store: decode state: %w", err)
	}
	return nil
}

func (s *Store) loadLog() error {
	f, err := os.Open(filepath.Join(s.dir, "deals.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: open log: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var d model.Deal
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			// A torn final line from a crash must not make the store unusable.
			continue
		}
		if d.Fingerprint == "" {
			continue
		}
		s.seen[d.Fingerprint] = d.DiscoveredAt
		if v, ok := d.Meta["pushed"]; ok && v == "true" {
			s.high[d.Fingerprint] = d.DiscoveredAt
		}
		s.scanned++
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("store: scan log: %w", err)
	}
	return nil
}

// Seen reports whether this fingerprint was already discovered.
func (s *Store) Seen(fp string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.seen[fp]
	return ok
}

// Pushed reports whether this fingerprint already reached the user.
func (s *Store) Pushed(fp string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.high[fp]
	return ok
}

// Save appends a deal to the log and records its fingerprint.
func (s *Store) Save(d *model.Deal) error {
	d.EnsureFingerprint()
	b, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("store: encode deal: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.log.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("store: append: %w", err)
	}
	s.seen[d.Fingerprint] = d.DiscoveredAt
	if v, ok := d.Meta["pushed"]; ok && v == "true" {
		s.high[d.Fingerprint] = d.DiscoveredAt
	}
	return nil
}

// MarkPushed records that a deal was delivered, rewriting its trailing log row
// so the flag survives a restart.
func (s *Store) MarkPushed(fp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.high[fp] = time.Now().UTC()
	if err := s.appendFlag(fp, "pushed", "true"); err != nil {
		return err
	}
	return nil
}

func (s *Store) appendFlag(fp, key, val string) error {
	for _, d := range s.recentLocked(5000) {
		if d.Fingerprint != fp {
			continue
		}
		if d.Meta == nil {
			d.Meta = map[string]string{}
		}
		d.Meta[key] = val
		b, err := json.Marshal(d)
		if err != nil {
			return err
		}
		_, err = s.log.Write(append(b, '\n'))
		return err
	}
	return nil
}

// Recent returns up to n most recently discovered deals, newest first.
func (s *Store) Recent(n int) []model.Deal { return s.recent(n, true) }

func (s *Store) recent(n int, lock bool) []model.Deal {
	if lock {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	return s.recentLocked(n)
}

func (s *Store) recentLocked(n int) []model.Deal {
	f, err := os.Open(filepath.Join(s.dir, "deals.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	// The log is append-only and MarkPushed rewrites a deal by appending a new
	// row, so collapse by fingerprint keeping the last occurrence anywhere in
	// the file - adjacency would leak duplicates.
	order := make([]string, 0, 256)
	latest := map[string]model.Deal{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var d model.Deal
		if json.Unmarshal([]byte(line), &d) != nil || d.Fingerprint == "" {
			continue
		}
		if _, seen := latest[d.Fingerprint]; !seen {
			order = append(order, d.Fingerprint)
		}
		latest[d.Fingerprint] = d
	}
	out := make([]model.Deal, 0, len(order))
	for _, fp := range order {
		out = append(out, latest[fp])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].DiscoveredAt.After(out[j].DiscoveredAt)
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// Pending returns deals that scored at or above min but were never pushed.
func (s *Store) Pending(min int, since time.Time) []model.Deal {
	var out []model.Deal
	for _, d := range s.Recent(20000) {
		if d.Score < min {
			continue
		}
		if !since.IsZero() && d.DiscoveredAt.Before(since) {
			continue
		}
		if s.Pushed(d.Fingerprint) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// GetState reads a source cursor.
func (s *Store) GetState(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.state[key]
	return append([]byte(nil), v...), ok
}

// PutState writes a source cursor; the map is flushed atomically.
func (s *Store) PutState(key string, val any) error {
	b, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("store: encode state %s: %w", key, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[key] = json.RawMessage(b)
	return s.flushStateLocked()
}

func (s *Store) flushStateLocked() error {
	tmp := filepath.Join(s.dir, "state.json.tmp")
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("store: open state tmp: %w", err)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s.state); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("store: write state: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("store: sync state: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: close state: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, "state.json")); err != nil {
		return fmt.Errorf("store: rename state: %w", err)
	}
	return nil
}

// Compact rewrites the log keeping one row per fingerprint, newest first.
func (s *Store) Compact(keepDays int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.recentLocked(0)
	cutoff := time.Now().UTC().Add(-time.Duration(keepDays) * 24 * time.Hour)
	var kept []model.Deal
	for _, d := range all {
		if keepDays > 0 && d.DiscoveredAt.Before(cutoff) && !s.high[d.Fingerprint].IsZero() {
			continue
		}
		if keepDays > 0 && d.DiscoveredAt.Before(cutoff) && d.Score < 60 {
			continue
		}
		kept = append(kept, d)
	}
	tmp := filepath.Join(s.dir, "deals.jsonl.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("store: open compact tmp: %w", err)
	}
	w := bufio.NewWriter(f)
	for _, d := range kept {
		b, err := json.Marshal(d)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		w.Write(b)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("store: write compact: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := s.log.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, "deals.jsonl")); err != nil {
		return fmt.Errorf("store: rename compact: %w", err)
	}
	s.seen = map[string]time.Time{}
	for _, d := range kept {
		s.seen[d.Fingerprint] = d.DiscoveredAt
	}
	l, err := os.OpenFile(filepath.Join(s.dir, "deals.jsonl"), os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("store: reopen log: %w", err)
	}
	s.log = l
	return nil
}

// Stats reports store contents for the health endpoint.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	st := Stats{DealsSeen: len(s.seen), PushedSeen: len(s.high), StateKeys: len(s.state)}
	for _, t := range s.seen {
		if st.OldestEntry.IsZero() || t.Before(st.OldestEntry) {
			st.OldestEntry = t
		}
		if t.After(st.NewestEntry) {
			st.NewestEntry = t
		}
	}
	s.mu.Unlock()
	_ = filepath.Walk(s.dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			st.DirSizeKB += info.Size() / 1024
		}
		return nil
	})
	return st
}

// Close flushes and releases the append handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return nil
	}
	err := s.log.Close()
	s.log = nil
	return err
}
