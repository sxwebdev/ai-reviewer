package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newTestServer builds a Server with stubbed Deps and returns it with its
// routed handler. Requests must come from loopback with the session cookie.
func newTestServer(t *testing.T, deps Deps) (*Server, http.Handler) {
	t.Helper()
	s, err := New(deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.token = "test-token"
	h, err := s.routes()
	if err != nil {
		t.Fatal(err)
	}
	return s, h
}

func doReq(h http.Handler, method, target string, form url.Values, hx bool) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	r.RemoteAddr = "127.0.0.1:34567"
	r.AddCookie(&http.Cookie{Name: "ai_session", Value: "test-token"})
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if hx {
		r.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func gatedDeps() Deps {
	return Deps{
		NeedsSetup: func() bool { return true },
		SetupStatus: func() SetupStatus {
			return SetupStatus{ClaudeFound: true, ClaudeDetail: "/usr/local/bin/claude", TokenEnvName: "GITLAB_TOKEN"}
		},
	}
}

func TestSetupGateRedirects(t *testing.T) {
	t.Parallel()
	_, h := newTestServer(t, gatedDeps())

	for _, target := range []string{"/", "/jobs", "/settings", "/mr/1"} {
		w := doReq(h, http.MethodGet, target, nil, false)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
			t.Errorf("GET %s = %d loc %q, want 303 /setup", target, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestSetupGateAllowsSetupStaticHealth(t *testing.T) {
	t.Parallel()
	_, h := newTestServer(t, gatedDeps())

	if w := doReq(h, http.MethodGet, "/setup", nil, false); w.Code != http.StatusOK {
		t.Errorf("GET /setup = %d, want 200", w.Code)
	}
	if w := doReq(h, http.MethodGet, "/static/app.css", nil, false); w.Code != http.StatusOK {
		t.Errorf("GET /static/app.css = %d, want 200", w.Code)
	}
	if w := doReq(h, http.MethodGet, "/health", nil, false); w.Code != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", w.Code)
	}
}

func TestSetupGateHXRedirect(t *testing.T) {
	t.Parallel()
	_, h := newTestServer(t, gatedDeps())

	w := doReq(h, http.MethodGet, "/mr/1/review-section", nil, true)
	if got := w.Header().Get("HX-Redirect"); got != "/setup" {
		t.Errorf("HX-Redirect = %q, want /setup", got)
	}
}

func TestSetupPageRedirectsHomeWhenGateOpen(t *testing.T) {
	t.Parallel()
	deps := gatedDeps()
	deps.NeedsSetup = func() bool { return false }
	_, h := newTestServer(t, deps)

	w := doReq(h, http.MethodGet, "/setup", nil, false)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Errorf("GET /setup = %d loc %q, want 303 /", w.Code, w.Header().Get("Location"))
	}
}

func TestSetupSubmitSuccess(t *testing.T) {
	t.Parallel()
	var applied struct{ host, username, token string }
	needsSetup := true
	deps := gatedDeps()
	deps.NeedsSetup = func() bool { return needsSetup }
	deps.ValidateGitLab = func(_ context.Context, host, token string) (string, error) { return "alice", nil }
	deps.ApplySetup = func(_ context.Context, host, username, token string) error {
		applied.host, applied.username, applied.token = host, username, token
		needsSetup = false // the gate opens once setup is applied
		return nil
	}
	_, h := newTestServer(t, deps)

	form := url.Values{"host": {"https://gl.local"}, "token": {"glpat-secret-123"}}
	w := doReq(h, http.MethodPost, "/setup", form, false)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("POST /setup = %d loc %q, want 303 /", w.Code, w.Header().Get("Location"))
	}
	if applied.host != "https://gl.local" || applied.username != "alice" || applied.token != "glpat-secret-123" {
		t.Errorf("ApplySetup got %+v", applied)
	}
}

// TestSetupSubmitClaudeStillMissing: GitLab settings save fine but the claude
// CLI is absent — the handler must redirect back to /setup with an explicit
// message, not bounce silently off the gate.
func TestSetupSubmitClaudeStillMissing(t *testing.T) {
	t.Parallel()
	deps := gatedDeps() // NeedsSetup stays true
	deps.ValidateGitLab = func(context.Context, string, string) (string, error) { return "alice", nil }
	deps.ApplySetup = func(context.Context, string, string, string) error { return nil }
	_, h := newTestServer(t, deps)

	form := url.Values{"host": {"https://gl.local"}, "token": {"glpat-x-123"}}
	w := doReq(h, http.MethodPost, "/setup", form, false)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
		t.Fatalf("POST /setup = %d loc %q, want 303 /setup", w.Code, w.Header().Get("Location"))
	}
	flash := ""
	for _, c := range w.Result().Cookies() {
		if c.Name == "flash" {
			flash = c.Value
		}
	}
	if !strings.Contains(flash, "warn") {
		t.Errorf("expected warn flash cookie, got %q", flash)
	}
	// The setup page renders the flash after the redirect.
	r := httptest.NewRequest(http.MethodGet, "/setup", nil)
	r.RemoteAddr = "127.0.0.1:34567"
	r.AddCookie(&http.Cookie{Name: "ai_session", Value: "test-token"})
	r.AddCookie(&http.Cookie{Name: "flash", Value: "warn|GitLab%20settings%20saved."})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !strings.Contains(rec.Body.String(), "GitLab settings saved.") {
		t.Error("setup page does not render the flash message")
	}
}

func TestSetupSubmitValidationFailure(t *testing.T) {
	t.Parallel()
	applyCalled := false
	deps := gatedDeps()
	deps.ValidateGitLab = func(context.Context, string, string) (string, error) {
		return "", errors.New("status 401")
	}
	deps.ApplySetup = func(context.Context, string, string, string) error {
		applyCalled = true
		return nil
	}
	_, h := newTestServer(t, deps)

	form := url.Values{"host": {"https://gl.local"}, "token": {"glpat-secret-123"}}
	w := doReq(h, http.MethodPost, "/setup", form, false)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /setup = %d, want 200 (re-rendered form)", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "GitLab validation failed") {
		t.Error("error message missing from response")
	}
	if strings.Contains(body, "glpat-secret-123") {
		t.Error("token echoed back in the response body")
	}
	if applyCalled {
		t.Error("ApplySetup called despite validation failure")
	}
}

func TestSetupSubmitRequiresToken(t *testing.T) {
	t.Parallel()
	deps := gatedDeps()
	validateCalled := false
	deps.ValidateGitLab = func(context.Context, string, string) (string, error) {
		validateCalled = true
		return "alice", nil
	}
	_, h := newTestServer(t, deps)

	w := doReq(h, http.MethodPost, "/setup", url.Values{"host": {"https://gl.local"}}, false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "token is required") {
		t.Errorf("expected inline token-required error, got %d", w.Code)
	}
	if validateCalled {
		t.Error("ValidateGitLab called without a token")
	}
}

func TestSetupSubmitEnvTokenAllowed(t *testing.T) {
	t.Parallel()
	deps := gatedDeps()
	st := deps.SetupStatus()
	st.TokenFromEnv = true
	deps.SetupStatus = func() SetupStatus { return st }
	var appliedToken string
	deps.ValidateGitLab = func(_ context.Context, _, token string) (string, error) {
		if token != "" {
			t.Errorf("expected empty token pass-through, got %q", token)
		}
		return "alice", nil
	}
	deps.ApplySetup = func(_ context.Context, _, _, token string) error {
		appliedToken = token
		return nil
	}
	_, h := newTestServer(t, deps)

	w := doReq(h, http.MethodPost, "/setup", url.Values{"host": {"https://gl.local"}}, false)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /setup = %d, want 303", w.Code)
	}
	if appliedToken != "" {
		t.Errorf("env-token flow must not persist a token, got %q", appliedToken)
	}
}

func TestApplyConfigHeaderSwitch(t *testing.T) {
	t.Parallel()
	var got map[string]string
	deps := gatedDeps()
	deps.NeedsSetup = func() bool { return false }
	deps.ApplyConfig = func(_ context.Context, values map[string]string) (ApplyResult, error) {
		got = values
		return ApplyResult{}, nil
	}
	_, h := newTestServer(t, deps)

	// Header switch: plain form POST → redirect.
	w := doReq(h, http.MethodPost, "/settings/apply", url.Values{"pipeline_mode": {"deep"}}, false)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/apply = %d, want 303", w.Code)
	}
	if got["review.pipeline.mode"] != "deep" || len(got) != 1 {
		t.Errorf("header-switch apply got %v", got)
	}

	w = doReq(h, http.MethodPost, "/settings/apply", url.Values{"llm_model": {"opus"}}, false)
	if w.Code != http.StatusSeeOther || got["llm.model"] != "opus" {
		t.Errorf("model apply: code %d, got %v", w.Code, got)
	}
}

func TestSettingsPageRenders(t *testing.T) {
	t.Parallel()
	deps := gatedDeps()
	deps.NeedsSetup = func() bool { return false }
	deps.UI = func() UIConfig { return UIConfig{} }
	deps.SettingsView = func() SettingsView {
		return SettingsView{Sections: []SettingsSection{{
			Name:      "Review",
			HasDanger: true,
			Fields: []SettingsFieldView{
				{Key: "review.max_comments", Label: "Max comments", Kind: "int", Value: "12", Help: "cap"},
				{Key: "review.severity_threshold", Label: "Severity", Kind: "select", Value: "medium", Options: []string{"low", "medium", "high"}},
				{Key: "review.auto_publish", Label: "Auto publish", Kind: "bool", Value: "false", Danger: true},
				{Key: "review.ignore_globs", Label: "Ignore globs", Kind: "list", Value: "a/**\nb/**"},
				{Key: "gitlab.token", Label: "Token", Kind: "password", Secret: true},
			},
		}}}
	}
	_, h := newTestServer(t, deps)

	w := doReq(h, http.MethodGet, "/settings", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`name="cfg:review.max_comments"`,
		`name="cfg:review.severity_threshold"`,
		`name="cfg:review.auto_publish"`,
		`name="cfg:review.ignore_globs"`,
		`name="cfg:gitlab.token"`,
		`badge--danger`, // danger field badge
		"a/**\nb/**",    // list textarea value
		`hx-confirm=`,   // danger section confirm guard (htmx-native, not a bypassable onsubmit)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}

func TestApplyConfigSettingsFormHX(t *testing.T) {
	t.Parallel()
	var got map[string]string
	deps := gatedDeps()
	deps.NeedsSetup = func() bool { return false }
	deps.ApplyConfig = func(_ context.Context, values map[string]string) (ApplyResult, error) {
		got = values
		return ApplyResult{RestartRequired: true}, nil
	}
	deps.SettingsView = func() SettingsView { return SettingsView{} }
	_, h := newTestServer(t, deps)

	// Settings form: cfg:-prefixed fields, htmx request → inline banner fragment.
	form := url.Values{
		"cfg:review.max_comments": {"20"},
		"cfg:review.ignore_globs": {"a/**\nb/**"},
		"ignore-me":               {"button"},
	}
	w := doReq(h, http.MethodPost, "/settings/apply", form, true)
	if w.Code != http.StatusOK {
		t.Fatalf("hx apply = %d, want 200", w.Code)
	}
	if got["review.max_comments"] != "20" || got["review.ignore_globs"] != "a/**\nb/**" {
		t.Errorf("settings-form apply got %v", got)
	}
	if _, ok := got["ignore-me"]; ok {
		t.Error("non cfg: field leaked into apply values")
	}
	if !strings.Contains(w.Body.String(), "Restart") {
		t.Errorf("restart-required banner missing: %s", w.Body.String())
	}
}
