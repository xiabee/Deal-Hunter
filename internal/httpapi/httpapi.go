// Package httpapi serves read-only status and findings. It exists so the
// OpenClaw assistant (and the embedded dashboard) can read Deal-Hunter over
// loopback or Tailscale without ever touching its credentials.
package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xiabee/deal-hunter/internal/config"
	"github.com/xiabee/deal-hunter/internal/model"
	"github.com/xiabee/deal-hunter/internal/pipeline"
	"github.com/xiabee/deal-hunter/internal/redact"
	"github.com/xiabee/deal-hunter/internal/version"
)

//go:embed web
var webFS embed.FS

// Server exposes the read-only API and dashboard.
type Server struct {
	app     *pipeline.App
	cfg     *config.Config
	log     *slog.Logger
	started time.Time
	srv     *http.Server
	ln      net.Listener
}

// New wires the handler to an App.
func New(cfg *config.Config, app *pipeline.App, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{app: app, cfg: cfg, log: log, started: time.Now()}
}

// Handler builds the route table; exported so tests need no listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /favicon.ico", s.icon)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/v1/status", s.status)
	mux.HandleFunc("GET /api/v1/deals", s.deals)
	mux.HandleFunc("GET /api/v1/sources", s.sources)
	mux.HandleFunc("GET /api/v1/runs", s.runs)
	mux.HandleFunc("GET /api/v1/digest", s.digest)
	mux.HandleFunc("GET /api/v1/openclaw/latest", s.digest)
	return s.middleware(mux)
}

// Serve starts the listener and blocks until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if !s.cfg.Server.Enabled {
		<-ctx.Done()
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.Server.Bind)
	if err != nil {
		return fmt.Errorf("httpapi: listen %s: %w", s.cfg.Server.Bind, err)
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(ln) }()
	s.log.Info("httpapi: listening", "url", "http://"+ln.Addr().String(), "scope", "loopback/tailscale only")

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Addr reports the bound address, or "" before Serve.
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.cfg.Server.Bind
	}
	return s.ln.Addr().String()
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("httpapi: panic recovered", "path", r.URL.Path, "panic", rec)
				http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			}
		}()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        version.Version,
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
		"feishu_ready":   s.app.FeishuReady(),
		"backends":       s.app.Backends(),
	})
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	st := s.app.Store().Stats()
	interval := s.cfg.Interval.D()
	next := s.app.LastRun()
	var nextAt time.Time
	if next != nil {
		nextAt = next.FinishedAt.Add(interval)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        version.String(),
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
		"bind":           s.Addr(),
		"network_scope":  "loopback or Tailscale only",
		"interval":       interval.String(),
		"next_round_at":  nextAt.UTC(),
		"sources": map[string]any{
			"configured": len(s.cfg.Sources),
			"enabled":    len(s.cfg.EnabledSources()),
		},
		"filters": map[string]any{
			"min_score":       s.cfg.Filter.MinScore,
			"alert_min_score": s.cfg.Notify.Feishu.MinScore,
			"require_offer":   s.cfg.Filter.RequireOffer,
		},
		"notify": map[string]any{
			"backends":     s.app.Backends(),
			"feishu_ready": s.app.FeishuReady(),
			"feishu_mode":  s.app.FeishuMode(),
			"feishu_host":  s.feishuHost(),
			"relay_ready":  s.app.RelayReady(),
			"outreach_dir": s.app.OutreachDir(),
		},
		"store": map[string]any{
			"deals_seen":     st.DealsSeen,
			"duplicates":     st.Duplicates,
			"pushed":         st.PushedSeen,
			"cursors":        st.StateKeys,
			"size_kb":        st.DirSizeKB,
			"oldest":         st.OldestEntry,
			"newest":         st.NewestEntry,
			"pending_alerts": len(s.app.Store().Pending(s.cfg.Notify.Feishu.MinScore, time.Time{})),
		},
		"last_run": s.app.LastRun(),
		"daily":    s.app.Daily(time.Now().UTC()),
	})
}

func (s *Server) feishuHost() string {
	if s.app.FeishuReady() {
		return "configured"
	}
	return "missing " + config.EnvFeishuWebhook
}

func (s *Server) deals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 50, 500)
	minScore := atoiDefault(q.Get("min"), 0, 100)
	category := strings.TrimSpace(q.Get("category"))
	source := strings.TrimSpace(q.Get("source"))
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))
	// Reposts of something already reported are hidden unless asked for, so the
	// list and the alert never show the same announcement twice.
	wantDupes := q.Get("dupes") == "1"

	all := s.app.RecentDeals(2000)
	out := make([]model.Deal, 0, limit)
	folded := 0
	for _, d := range all {
		if d.Meta["dup_of"] != "" {
			folded++
			if !wantDupes {
				continue
			}
		}
		if d.Score < minScore {
			continue
		}
		if category != "" && d.Category != category {
			continue
		}
		if source != "" && d.Source != source {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(d.Title+" "+d.Summary), needle) {
			continue
		}
		if len(out) < limit {
			out = append(out, d)
		}
		// Keep scanning after the page is full: the folded count below has to
		// describe the whole store, not just what fit on this page.
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(out), "deals": out,
		"folded_duplicates": folded,
	})
}

func (s *Server) sources(w http.ResponseWriter, _ *http.Request) {
	byName := map[string]pipeline.SourceReport{}
	if run := s.app.LastRun(); run != nil {
		for _, rep := range run.Sources {
			byName[rep.Name] = rep
		}
	}
	out := make([]map[string]any, 0, len(s.cfg.Sources))
	for _, sc := range s.cfg.Sources {
		rep, ran := byName[sc.Name]
		item := map[string]any{
			"name":     sc.Name,
			"kind":     sc.Kind,
			"url":      redact.Query(sc.URL),
			"enabled":  !sc.Disabled,
			"trust":    sc.Trust,
			"official": len(sc.Sites) > 0,
		}
		if ran {
			item["last"] = rep
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "sources": out})
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 10, 24)
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.app.RecentRuns(limit)})
}

// digest serves the Markdown the OpenClaw assistant reads. Keeping it as text
// means the assistant can relay it to chat without parsing JSON.
func (s *Server) digest(w http.ResponseWriter, r *http.Request) {
	dir := s.app.OutreachDir()
	if dir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "openclaw drop disabled"})
		return
	}
	path := filepath.Join(dir, "deal-hunter-latest.md")
	// Contain the read to the drop directory even if config is odd.
	cleanDir, err := filepath.Abs(dir)
	if err != nil {
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil || !strings.HasPrefix(cleanPath, cleanDir+string(os.PathSeparator)) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	b, err := os.ReadFile(cleanPath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no digest yet", "path": filepath.Base(cleanPath)})
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write(b)
}

// icon serves the tab icon from the same embedded FS, so the page never makes a
// request that a public CDN would have to answer.
func (s *Server) icon(w http.ResponseWriter, _ *http.Request) {
	b, err := fs.ReadFile(webFS, "web/icon.svg")
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", s.started, strings.NewReader(string(b)))
}

func atoiDefault(raw string, def, max int) int {
	if strings.TrimSpace(raw) == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	if max > 0 && n > max {
		return max
	}
	return n
}
