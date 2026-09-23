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

// mustBeBlocked asserts that the *guard* refused a request, not that the request
// merely failed. "err != nil" is not evidence of a block: a machine with no route
// to an address returns an error too, so a test written that way keeps passing
// after the rule it guards is deleted. The word "blocked" only appears in the
// guard's own messages.
func mustBeBlocked(t *testing.T, err error, rawURL string) {
	t.Helper()
	if err == nil {
		t.Fatalf("request to %s should be blocked", rawURL)
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("nothing blocked %s - it just failed: %v", rawURL, err)
	}
}

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
		_, err := c.Get(context.Background(), raw, nil)
		// The address, not just "an error": a refused connection also errors, and
		// reading that as "the guard worked" would let the test keep passing after
		// the guard was removed. See TestGuardRefusalsNameTheAddress.
		mustBeBlocked(t, err, raw)
	}
	// Metadata must stay blocked even when an operator opens private ranges.
	open := New(Options{AllowPrivate: true})
	_, err := open.Get(context.Background(), "http://169.254.169.254/", nil)
	mustBeBlocked(t, err, "http://169.254.169.254/")
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

// The guard runs where the connection is actually made, not where the URL was
// typed. That is the difference between guarding the request and guarding the
// hop - and a 302 is a hop this project really takes: the official-link
// re-check follows links it read out of announcement text, so a post can point
// the client anywhere it likes and the reply can then redirect again.
//
// The witness is the *reason*: a refused connection also fails, but it fails
// with "no route to host", so only the guard's own wording proves the guard is
// what stopped the second hop. And the handler counting one request proves the
// first hop was allowed - otherwise this would be a test of nothing.
func TestRedirectTargetIsGuardedTheWayTheRequestWas(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	c := New(Options{AllowPrivate: true})
	_, err := c.Get(context.Background(), srv.URL, nil)
	if err == nil {
		t.Fatal("a redirect into the cloud metadata address was followed")
	}
	if !strings.Contains(err.Error(), "blocked") || !strings.Contains(err.Error(), "169.254.169.254") {
		t.Errorf("the guard, not the network, must be what refused: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("the first hop must have been served exactly once, hits=%d", got)
	}
}

// Two literals in isBlockedIP had no test naming them: the IPv6 metadata
// endpoint, and IPv6 loopback (which is only refused while private ranges are
// closed - the operator who fetches a tailscale/loopback panel on purpose
// relies on that staying the rule).
func TestBlockedTargetsCoversTheIPv6Forms(t *testing.T) {
	strict := New(Options{})
	_, err := strict.Get(context.Background(), "http://[::1]:9/", nil)
	mustBeBlocked(t, err, "http://[::1]:9/ (IPv6 loopback)")

	open := New(Options{AllowPrivate: true})
	_, err = open.Get(context.Background(), "http://[fd00:ec2::254]/", nil)
	mustBeBlocked(t, err, "http://[fd00:ec2::254]/ (IPv6 cloud metadata)")
	if !strings.Contains(err.Error(), "fd00:ec2::254") {
		t.Errorf("the metadata rule must name the address it refused: %v", err)
	}
	// The positive control: with private ranges open, loopback does work - so a
	// pass above is the metadata rule, not a client that cannot reach ::1 at all.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer srv.Close()
	if _, err := open.Get(context.Background(), srv.URL, nil); err != nil {
		t.Fatalf("control: loopback fetch should succeed with AllowPrivate, got %v", err)
	}
}
