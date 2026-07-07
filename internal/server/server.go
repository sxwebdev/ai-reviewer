// Package server implements the local web UI: a localhost-only HTTP server that
// renders HTMX/Alpine pages backed by the shared service layer.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/ui"
)

// UIConfig carries the config bits the UI displays (Settings page, header
// switches) and uses to label cost (subscription auth reports notional cost).
type UIConfig struct {
	Host               string
	LLMModel           string
	CommentLanguage    string
	SeverityThreshold  string
	MaxComments        int
	AgentMode          bool // review.agent_mode toggle state (header switch)
	AgentModeEffective bool // review.agent_mode AND llm.claude.agent_mode — what actually enables skills
	SubscriptionAuth   bool // existing-login/oauth → reported cost is notional

	// Header switches: current review depth (pipeline mode) and model, plus the
	// choices offered in the selects.
	PipelineMode  string
	PipelineModes []string
	Models        []ModelChoice
}

// ModelChoice is one entry in the model select: ID is the value passed to the
// claude CLI (--model), Label is the human-friendly name shown in the dropdown.
type ModelChoice struct {
	ID    string
	Label string
}

// SetupStatus feeds the setup page: current prefills and environment checks.
type SetupStatus struct {
	Host         string
	Username     string
	TokenFromEnv bool   // a token is already resolvable via the token_env fallback
	TokenEnvName string // name of that env var (never its value)
	ClaudeFound  bool
	ClaudeDetail string // resolved path, or a doctor-style "not found" message
}

// HealthCheck is one environment/config check result for the Health page. It
// mirrors app.DoctorCheck without importing app (which would be an import cycle).
type HealthCheck struct {
	Name   string
	Status string // ok|warn|fail
	Detail string
}

// HealthFunc runs the doctor checks. It is injected by the app layer so the
// server can surface health without importing app.
type HealthFunc func(context.Context) []HealthCheck

// Deps carries everything the app layer injects. Function fields keep every
// value hot-reloadable (the app can swap config and services at runtime) and
// keep this package importable without internal/app (no cycle).
type Deps struct {
	// Bundle returns the current service bundle.
	Bundle func() *service.Bundle
	// UI returns the current display config (re-read per request so header
	// switches reflect hot-applied changes).
	UI func() UIConfig
	// Health runs the doctor checks. May be nil (the panel reports nothing).
	Health HealthFunc
	// NeedsSetup reports whether the setup gate should hide the interface.
	NeedsSetup func() bool
	// SetupStatus returns prefills + environment checks for the setup page.
	SetupStatus func() SetupStatus
	// ValidateGitLab checks host+token against the live API and returns the
	// authenticated username.
	ValidateGitLab func(ctx context.Context, host, token string) (string, error)
	// ApplySetup persists the GitLab settings and rebuilds services in-process.
	// An empty token keeps the env-provided one (never written to disk).
	ApplySetup func(ctx context.Context, host, username, token string) error
	// ApplySettings persists whitelisted config keys (header switches) and
	// rebuilds services in-process.
	ApplySettings func(ctx context.Context, values map[string]string) error
}

// Server is the local web UI.
type Server struct {
	deps  Deps
	log   *slog.Logger
	tmpl  map[string]*template.Template
	token string
}

// New builds the server and parses templates.
func New(deps Deps, log *slog.Logger) (*Server, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{deps: deps, log: log, tmpl: tmpl}, nil
}

// svc returns the current service bundle (hot-swappable by the app layer).
func (s *Server) svc() *service.Bundle { return s.deps.Bundle() }

// ui returns the current display config.
func (s *Server) ui() UIConfig {
	if s.deps.UI == nil {
		return UIConfig{}
	}
	return s.deps.UI()
}

var funcMap = template.FuncMap{
	"shortSHA": func(s string) string {
		if len(s) > 8 {
			return s[:8]
		}
		return s
	},
	"fmtTime": func(ms int64) string {
		if ms == 0 {
			return ""
		}
		return time.UnixMilli(ms).Format("2006-01-02 15:04")
	},
	// ago renders a compact relative age like "5d", "3h", "2w" from a unix-ms
	// timestamp — how long ago something happened.
	"ago": func(ms int64) string {
		if ms <= 0 {
			return ""
		}
		d := time.Since(time.UnixMilli(ms))
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%dm", int(d.Minutes()))
		case d < 24*time.Hour:
			return fmt.Sprintf("%dh", int(d.Hours()))
		case d < 7*24*time.Hour:
			return fmt.Sprintf("%dd", int(d.Hours())/24)
		case d < 30*24*time.Hour:
			return fmt.Sprintf("%dw", int(d.Hours())/(24*7))
		default:
			return fmt.Sprintf("%dmo", int(d.Hours())/(24*30))
		}
	},
}

// parseTemplates builds one template set per page (base + page), so each page's
// {{define "content"}} does not collide with another's.
func parseTemplates() (map[string]*template.Template, error) {
	pages := []string{"dashboard", "mr", "jobs", "memory", "settings"}
	out := make(map[string]*template.Template, len(pages)+1)
	for _, p := range pages {
		t, err := template.New("base.gohtml").Funcs(funcMap).
			ParseFS(ui.FS, "templates/base.gohtml", "templates/"+p+".gohtml")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", p, err)
		}
		out[p] = t
	}
	// The setup page is a standalone document (no base layout: the interface
	// stays hidden until setup completes).
	t, err := template.New("setup.gohtml").Funcs(funcMap).ParseFS(ui.FS, "templates/setup.gohtml")
	if err != nil {
		return nil, fmt.Errorf("parse template setup: %w", err)
	}
	out["setup"] = t
	return out, nil
}

// renderSetup executes the standalone setup document.
func (s *Server) renderSetup(w http.ResponseWriter, data any) {
	s.renderRoot(w, "setup", "setup.gohtml", data)
}

// render executes a base-composed page template into the response.
func (s *Server) render(w http.ResponseWriter, page string, data any) {
	s.renderRoot(w, page, "base.gohtml", data)
}

// renderRoot executes a page's template set starting at the given root
// template (base layout for normal pages, the standalone document for setup).
func (s *Server) renderRoot(w http.ResponseWriter, page, root string, data any) {
	t, ok := s.tmpl[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, root, data); err != nil {
		s.log.Error("render failed", "page", page, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// isHX reports whether the request came from htmx (a partial swap is expected).
func isHX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// renderPartial executes a named block from a page's template set (for htmx
// fragment swaps) into the response.
func (s *Server) renderPartial(w http.ResponseWriter, page, name string, data any) {
	t, ok := s.tmpl[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render partial failed", "page", page, "name", name, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// routes builds the handler with static files and localhost+token auth.
func (s *Server) routes() (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("POST /sync", s.handleSync)
	mux.HandleFunc("GET /mr/{id}", s.handleMR)
	mux.HandleFunc("POST /mr/{id}/review", s.handleRunReview)
	mux.HandleFunc("GET /mr/{id}/review-section", s.handleReviewSection)
	mux.HandleFunc("POST /finding/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /finding/{id}/reject", s.handleReject)
	mux.HandleFunc("POST /finding/{id}/edit", s.handleEdit)
	mux.HandleFunc("POST /review/{id}/approve-all", s.handleApproveAll)
	mux.HandleFunc("POST /review/{id}/drafts", s.handleCreateDrafts)
	mux.HandleFunc("POST /review/{id}/publish", s.handlePublish)
	mux.HandleFunc("GET /jobs", s.handleJobs)
	mux.HandleFunc("POST /jobs/{id}/retry", s.handleRetryJob)
	mux.HandleFunc("GET /memory", s.handleMemory)
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /setup", s.handleSetup)
	mux.HandleFunc("POST /setup", s.handleSetupSubmit)
	mux.HandleFunc("GET /setup/check", s.handleSetupCheck)
	mux.HandleFunc("POST /settings/apply", s.handleApplySettings)

	staticFS, err := fs.Sub(ui.FS, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))

	return s.auth(s.setupGate(mux)), nil
}

// Run binds to host:port (port 0 = random), prints the URL, optionally opens the
// browser, and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context, host string, port int, open bool) error {
	s.token = randomToken()
	handler, err := s.routes()
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	actual := ln.Addr().(*net.TCPAddr)
	url := fmt.Sprintf("http://%s:%d/?token=%s", host, actual.Port, s.token)

	fmt.Printf("\n  ai-reviewer web UI:\n  %s\n\n", url)
	if open {
		openBrowser(url)
	}

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// openBrowser best-effort opens url in the default browser.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	args = append(args, url)
	_ = exec.Command(cmd, args...).Start()
}
