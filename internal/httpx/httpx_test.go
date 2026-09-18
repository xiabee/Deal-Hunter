package httpx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPerHostSerialization(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	c := New(Options{AllowPrivate: true, PerHostMin: 60 * time.Millisecond})
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.Get(context.Background(), srv.URL, nil); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 110*time.Millisecond {
		t.Errorf("per-host gate not applied: 3 requests took %s", elapsed)
	}
	if hits != 3 {
		t.Errorf("hits = %d", hits)
	}
}

func TestDifferentHostsAreNotGatedByEachOther(t *testing.T) {
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "a") }))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "b") }))
	defer b.Close()
	c := New(Options{AllowPrivate: true, PerHostMin: 500 * time.Millisecond})
	start := time.Now()
	if _, err := c.Get(context.Background(), a.URL, nil); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(context.Background(), b.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "b" {
		t.Errorf("body = %q", resp.Body)
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Error("distinct hosts must not wait on each other")
	}
}

func TestRetriesOnServerError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, "recovered")
	}))
	defer srv.Close()
	c := New(Options{AllowPrivate: true, Retries: 2, PerHostMin: time.Millisecond})
	resp, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	if string(resp.Body) != "recovered" || calls != 3 {
		t.Errorf("calls=%d body=%q", calls, resp.Body)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := New(Options{AllowPrivate: true, Retries: 3, PerHostMin: time.Millisecond})
	resp, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatalf("404 is a response, not a transport error: %v", err)
	}
	if resp.Status != 404 || calls != 1 {
		t.Errorf("calls=%d status=%d", calls, resp.Status)
	}
}

func TestBlockedTargets(t *testing.T) {
	c := New(Options{}) // private/loopback blocked by default
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://127.0.0.1:9/",                      // loopback
		"http://10.0.0.1/",                         // secretlint:ignore RFC1918 example that proves the SSRF block
		"http://[fd00::1]/",                        // unique local
	} {
		if _, err := c.Get(context.Background(), raw, nil); err == nil {
			t.Errorf("request to %s should be blocked", raw)
		}
	}
	// Metadata must stay blocked even when an operator opens private ranges.
	open := New(Options{AllowPrivate: true})
	if _, err := open.Get(context.Background(), "http://169.254.169.254/", nil); err == nil {
		t.Error("link-local/metadata addresses are never allowed")
	}
}

func TestPrivateTargetsAllowedWhenConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer srv.Close()
	strict := New(Options{})
	if _, err := strict.Get(context.Background(), srv.URL, nil); err == nil {
		t.Fatal("httptest binds loopback, which is blocked by default")
	}
	lenient := New(Options{AllowPrivate: true})
	if _, err := lenient.Get(context.Background(), srv.URL, nil); err != nil {
		t.Fatalf("AllowPrivate should permit loopback: %v", err)
	}
}

func TestSchemeRestrictions(t *testing.T) {
	c := New(Options{})
	for _, raw := range []string{"file:///etc/passwd", "gopher://x/", "http://"} {
		if _, err := c.Get(context.Background(), raw, nil); err == nil {
			t.Errorf("%q must be refused", raw)
		}
	}
}

func TestBodyIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, strings.Repeat("A", 5000))
	}))
	defer srv.Close()
	c := New(Options{AllowPrivate: true, MaxBody: 100})
	resp, err := c.Get(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Body) != 100 {
		t.Errorf("body length = %d, want the 100 byte cap", len(resp.Body))
	}
}

func TestHeadersAreSent(t *testing.T) {
	var ua, accept, custom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		accept = r.Header.Get("Accept")
		custom = r.Header.Get("X-Trace")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	c := New(Options{AllowPrivate: true, UserAgent: "DealHunterTest/1.0"})
	if _, err := c.Get(context.Background(), srv.URL, map[string]string{"X-Trace": "abc"}); err != nil {
		t.Fatal(err)
	}
	if ua != "DealHunterTest/1.0" {
		t.Errorf("user agent = %q", ua)
	}
	if !strings.Contains(accept, "application/json") {
		t.Errorf("accept = %q", accept)
	}
	if custom != "abc" {
		t.Errorf("custom header = %q", custom)
	}
}

func TestDefaultOptionsAreApplied(t *testing.T) {
	c := New(Options{})
	if c.opts.Timeout != 20*time.Second || c.opts.MaxBody != 3<<20 || c.opts.PerHostMin != 800*time.Millisecond {
		t.Errorf("defaults not applied: %+v", c.opts)
	}
	if !strings.Contains(c.opts.UserAgent, "Mozilla") {
		t.Errorf("default UA should look browser-like: %q", c.opts.UserAgent)
	}
}

func TestContextCancellationStopsRetries(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(Options{AllowPrivate: true, Retries: 5, PerHostMin: time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, srv.URL, nil); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if calls > 3 {
		t.Errorf("cancelled context should stop retrying quickly, calls=%d", calls)
	}
}

func TestFinalURLRecordsRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/final") {
			fmt.Fprint(w, "landed")
			return
		}
		http.Redirect(w, r, "/final", http.StatusFound)
	}))
	defer srv.Close()
	c := New(Options{AllowPrivate: true})
	resp, err := c.Get(context.Background(), srv.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(resp.FinalURL, "/final") || string(resp.Body) != "landed" {
		t.Errorf("redirect not followed: %q %q", resp.FinalURL, resp.Body)
	}
}
