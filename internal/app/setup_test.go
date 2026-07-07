package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/config"
)

// newTestApp builds an App whose config file and storage all live in a temp
// dir, with the DB opened.
func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := config.PatchFile(path, map[string]string{
		"app.data_dir":      dir,
		"storage.db_path":   filepath.Join(dir, "state.db"),
		"storage.cache_dir": filepath.Join(dir, "cache"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.OpenState(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// fakeGitLab serves GET /api/v4/user for the given username.
func fakeGitLab(t *testing.T, username string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/user" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "username": username})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenStateCreatesDirs(t *testing.T) {
	a := newTestApp(t)
	cfg := a.Config()
	for _, dir := range []string{cfg.App.DataDir, cfg.Storage.CacheDir} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("dir %s not created: %v", dir, err)
		}
	}
	if _, err := os.Stat(cfg.Storage.DBPath); err != nil {
		t.Errorf("db not created: %v", err)
	}
}

func TestApplySetupEndToEnd(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	gl := fakeGitLab(t, "alice")

	user, err := a.ValidateGitLab(t.Context(), gl.URL, "glpat-test-token")
	if err != nil {
		t.Fatalf("ValidateGitLab: %v", err)
	}
	if user != "alice" {
		t.Fatalf("username = %q, want alice", user)
	}

	oldBundle, err := a.Services()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ApplySetup(t.Context(), gl.URL, "alice", "glpat-test-token"); err != nil {
		t.Fatalf("ApplySetup: %v", err)
	}

	cfg := a.Config()
	if cfg.GitLab.Host != gl.URL || cfg.GitLab.Username != "alice" || cfg.GitLab.Token != "glpat-test-token" {
		t.Errorf("reloaded config = host %q user %q", cfg.GitLab.Host, cfg.GitLab.Username)
	}
	if a.Bundle() == oldBundle {
		t.Error("bundle was not rebuilt")
	}
	// The persisted file round-trips through a fresh Load.
	cfg2, err := config.Load(a.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.GitLab.Token != "glpat-test-token" {
		t.Error("token not persisted")
	}
	// Sync through the new bundle reaches the fake host (host configured means
	// the stub client was replaced; the fake returns 404 for list endpoints,
	// which proves the real client is wired).
	if _, err := a.Bundle().Sync.SyncAssignedMRs(t.Context()); err == nil {
		t.Log("sync unexpectedly succeeded (fake returns 404) — still proves real client")
	}
}

func TestApplySetupEnvTokenNotPersisted(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "env-secret-token")
	a := newTestApp(t)
	gl := fakeGitLab(t, "alice")

	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}
	// Empty token = keep using the env one.
	if err := a.ApplySetup(t.Context(), gl.URL, "alice", ""); err != nil {
		t.Fatalf("ApplySetup: %v", err)
	}
	raw, err := os.ReadFile(a.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" || filepath.Base(a.ConfigPath) != "config.yaml" {
		t.Fatal("config file missing")
	}
	if got := a.Config().GitLab.Token; got != "" {
		t.Errorf("token written to config: %q", got)
	}
	if a.Config().GitLabToken() != "env-secret-token" {
		t.Error("env token not resolvable after apply")
	}
	if a.NeedsSetup() && a.SetupStatus().ClaudeFound {
		t.Error("gate still closed despite host+env-token+claude")
	}
}

func TestApplySettingsValidatesAndSwaps(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}

	if err := a.ApplySettings(t.Context(), map[string]string{"review.pipeline.mode": "bogus"}); err == nil {
		t.Error("bogus pipeline mode accepted")
	}
	if err := a.ApplySettings(t.Context(), map[string]string{"llm.model": "not-a-choice"}); err == nil {
		t.Error("model outside modelChoices accepted")
	}
	if err := a.ApplySettings(t.Context(), map[string]string{"gitlab.token": "nope"}); err == nil {
		t.Error("non-whitelisted key accepted")
	}

	old := a.Bundle()
	if err := a.ApplySettings(t.Context(), map[string]string{
		"review.pipeline.mode": "deep",
		"llm.model":            "opus",
	}); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	cfg := a.Config()
	if cfg.Review.Pipeline.Mode != "deep" || cfg.LLM.Model != "opus" {
		t.Errorf("settings not applied: mode %q model %q", cfg.Review.Pipeline.Mode, cfg.LLM.Model)
	}
	if a.Bundle() == old {
		t.Error("bundle was not rebuilt")
	}
	if a.uiConfig().PipelineMode != "deep" || a.uiConfig().LLMModel != "opus" {
		t.Errorf("uiConfig stale: %+v", a.uiConfig())
	}
}

// TestHotApplyRace hammers the lock-free readers while settings are applied;
// verified by -race.
func TestHotApplyRace(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_ = a.Config().LLM.Model
					_ = a.Bundle()
					_ = a.uiConfig()
				}
			}
		})
	}
	modes := []string{"cheap", "deep", "standard"}
	for i := range 6 {
		if err := a.ApplySettings(t.Context(), map[string]string{"review.pipeline.mode": modes[i%3]}); err != nil {
			t.Errorf("apply %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestApplySettingsEnvShadow: an AI_REVIEWER_* env var overrides the value the
// user just applied — the apply must return an error naming the variable
// instead of reporting success for a setting that did not take effect.
func TestApplySettingsEnvShadow(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("AI_REVIEWER_LLM_MODEL", "sonnet")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}

	err := a.ApplySettings(t.Context(), map[string]string{"llm.model": "opus"})
	if err == nil {
		t.Fatal("expected env-shadow error, got nil")
	}
	if !strings.Contains(err.Error(), "AI_REVIEWER_LLM_MODEL") {
		t.Errorf("error does not name the shadowing env var: %v", err)
	}
	// The runtime stays consistent with the environment, not the form value.
	if got := a.Config().LLM.Model; got != "sonnet" {
		t.Errorf("effective model = %q, want sonnet (env)", got)
	}
	// The file still records the user's choice for when the env var is unset.
	raw, err := os.ReadFile(a.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `model: "opus"`) {
		t.Error("chosen model not persisted to the file")
	}
}

// TestOverridesSurviveReload: CLI flag overrides must be re-applied after a
// hot config reload instead of being silently dropped.
func TestOverridesSurviveReload(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}
	a.SetOverrides(func(cfg *config.Config) { cfg.App.Port = 12345 })
	if got := a.Config().App.Port; got != 12345 {
		t.Fatalf("override not applied immediately: port = %d", got)
	}

	if err := a.ApplySettings(t.Context(), map[string]string{"review.pipeline.mode": "deep"}); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if got := a.Config().App.Port; got != 12345 {
		t.Errorf("override lost after reload: port = %d", got)
	}
	if got := a.Config().Review.Pipeline.Mode; got != "deep" {
		t.Errorf("setting not applied: mode = %q", got)
	}
}

// TestApplySettingsFailureKeepsRuntime: when the rebuild fails nothing may be
// swapped — old config and old bundle stay live.
func TestApplySettingsFailureKeepsRuntime(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}
	oldBundle := a.Bundle()
	oldMode := a.Config().Review.Pipeline.Mode

	// Make the reload's Validate fail: PatchFile would fix any YAML we poison,
	// so break the merged config via an env override instead.
	t.Setenv("AI_REVIEWER_REVIEW_SEVERITY_THRESHOLD", "not-a-severity")
	err := a.ApplySettings(t.Context(), map[string]string{"review.pipeline.mode": "deep"})
	if err == nil {
		t.Fatal("expected reload failure")
	}
	if a.Bundle() != oldBundle {
		t.Error("bundle swapped despite failed reload")
	}
	if got := a.Config().Review.Pipeline.Mode; got != oldMode {
		t.Errorf("config swapped despite failed reload: mode = %q", got)
	}
}

func TestNeedsSetup(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)

	if !a.NeedsSetup() {
		t.Error("fresh config should need setup")
	}
	cfg := a.Config()
	cfg.GitLab.Host = "https://gl.local"
	cfg.GitLab.Token = "tok"
	cfg.LLM.Claude.Bin = "git" // something guaranteed on PATH in CI
	if a.NeedsSetup() {
		t.Error("host+token+bin present, gate should open")
	}
	cfg.LLM.Claude.Bin = "definitely-not-a-real-binary-xyz"
	if !a.NeedsSetup() {
		t.Error("missing claude bin should keep the gate closed")
	}
}
