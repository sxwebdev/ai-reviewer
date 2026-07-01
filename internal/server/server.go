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

// Server is the local web UI.
type Server struct {
	svc   *service.Bundle
	log   *slog.Logger
	host  string // GitLab host, for display
	tmpl  map[string]*template.Template
	token string
}

// New builds the server and parses templates.
func New(svc *service.Bundle, gitlabHost string, log *slog.Logger) (*Server, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{svc: svc, log: log, host: gitlabHost, tmpl: tmpl}, nil
}

var funcMap = template.FuncMap{
	"shortSHA": func(s string) string {
		if len(s) > 8 {
			return s[:8]
		}
		return s
	},
}

// parseTemplates builds one template set per page (base + page), so each page's
// {{define "content"}} does not collide with another's.
func parseTemplates() (map[string]*template.Template, error) {
	pages := []string{"dashboard", "mr"}
	out := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t, err := template.New("base.gohtml").Funcs(funcMap).
			ParseFS(ui.FS, "templates/base.gohtml", "templates/"+p+".gohtml")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", p, err)
		}
		out[p] = t
	}
	return out, nil
}

// render executes a page template into the response.
func (s *Server) render(w http.ResponseWriter, page string, data any) {
	t, ok := s.tmpl[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base.gohtml", data); err != nil {
		s.log.Error("render failed", "page", page, "err", err)
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
	mux.HandleFunc("POST /finding/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /finding/{id}/reject", s.handleReject)
	mux.HandleFunc("POST /review/{id}/drafts", s.handleCreateDrafts)
	mux.HandleFunc("POST /review/{id}/publish", s.handlePublish)

	staticFS, err := fs.Sub(ui.FS, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))

	return s.auth(mux), nil
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
