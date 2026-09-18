// Package httpx is the shared outbound fetcher: one browser-like client with a
// per-host rate limit, body size cap, bounded retries and a metadata-IP block.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Options tunes the client. Zero values fall back to sane defaults.
type Options struct {
	UserAgent    string
	Timeout      time.Duration
	MaxBody      int64 // bytes
	PerHostMin   time.Duration
	Retries      int
	AllowPrivate bool
}

// Response is a fetched document.
type Response struct {
	Status   int
	FinalURL string
	Body     []byte
	Header   http.Header
}

// Client serializes requests per host so feeds never see a burst from us.
type Client struct {
	h     *http.Client
	opts  Options
	mu    sync.Mutex
	gates map[string]*gate
}

type gate struct {
	mu   sync.Mutex
	last time.Time
}

// New builds a client.
func New(opts Options) *Client {
	if opts.UserAgent == "" {
		opts.UserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}
	if opts.MaxBody <= 0 {
		opts.MaxBody = 3 << 20
	}
	if opts.PerHostMin <= 0 {
		opts.PerHostMin = 800 * time.Millisecond
	}
	if opts.Retries < 0 {
		opts.Retries = 0
	}
	dial := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if err := opts.guardHost(host); err != nil {
				return nil, err
			}
			return dial.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Client{
		h:     &http.Client{Timeout: opts.Timeout, Transport: tr},
		opts:  opts,
		gates: map[string]*gate{},
	}
}

func (o Options) guardHost(host string) error {
	ip := net.ParseIP(host)
	if ip == nil {
		addrs, err := net.LookupIP(host)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", host, err)
		}
		for _, a := range addrs {
			if isBlockedIP(a, o.AllowPrivate) {
				return fmt.Errorf("host %s resolves to blocked address %s", host, a)
			}
		}
		return nil
	}
	if isBlockedIP(ip, o.AllowPrivate) {
		return fmt.Errorf("blocked address %s", ip)
	}
	return nil
}

// isBlockedIP always refuses link-local / cloud metadata endpoints, which no
// deal feed legitimately lives behind.
func isBlockedIP(ip net.IP, allowPrivate bool) bool {
	if ip == nil {
		return false
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.String() == "169.254.169.254" || ip.String() == "fd00:ec2::254" {
		return true
	}
	if allowPrivate {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// Get fetches url, applying the per-host gate and retrying transient failures.
func (c *Client) Get(ctx context.Context, rawURL string, hdr map[string]string) (*Response, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("httpx: parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("httpx: refusing scheme %q", u.Scheme)
	}
	var lastErr error
	for attempt := 0; attempt <= c.opts.Retries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * 1500 * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		resp, err := c.once(ctx, u.String(), hdr)
		if err == nil && resp.Status < 500 {
			return resp, nil
		}
		if err == nil {
			lastErr = fmt.Errorf("httpx: %s returned %d", u.Host, resp.Status)
			continue
		}
		lastErr = err
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) once(ctx context.Context, rawURL string, hdr map[string]string) (*Response, error) {
	if err := c.wait(ctx, rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("httpx: new request: %w", err)
	}
	req.Header.Set("User-Agent", c.opts.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/json, text/html;q=0.9, */*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.7")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.h.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpx: %w", unwrapURLError(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxBody))
	if err != nil {
		return nil, fmt.Errorf("httpx: read body: %w", err)
	}
	return &Response{Status: resp.StatusCode, FinalURL: resp.Request.URL.String(), Body: body, Header: resp.Header.Clone()}, nil
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, ue.URL, ue.Err)
	}
	return err
}

// wait serializes calls to the same host and enforces the minimum interval.
func (c *Client) wait(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	c.mu.Lock()
	g, ok := c.gates[u.Host]
	if !ok {
		g = &gate{}
		c.gates[u.Host] = g
	}
	c.mu.Unlock()

	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.IsZero() {
		if d := time.Until(g.last.Add(c.opts.PerHostMin)); d > 0 {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
			}
		}
	}
	g.last = time.Now()
	return nil
}
