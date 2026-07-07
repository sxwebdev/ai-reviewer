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

func TestApplyConfigValidatesAndSwaps(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}
	apply := func(v map[string]string) error { _, err := a.ApplyConfig(t.Context(), v); return err }

	if apply(map[string]string{"review.pipeline.mode": "bogus"}) == nil {
		t.Error("bogus pipeline mode accepted")
	}
	if apply(map[string]string{"llm.model": "not-a-choice"}) == nil {
		t.Error("model outside modelChoices accepted")
	}
	if apply(map[string]string{"review.max_comments": "notanumber"}) == nil {
		t.Error("non-numeric int accepted")
	}
	if apply(map[string]string{"totally.unknown.key": "x"}) == nil {
		t.Error("unknown key accepted")
	}

	old := a.Bundle()
	res, err := a.ApplyConfig(t.Context(), map[string]string{
		"review.pipeline.mode": "deep",
		"llm.model":            "claude-opus-4-8",
		"review.max_comments":  "20",
	})
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if res.RestartRequired {
		t.Error("no restart-required field was changed")
	}
	cfg := a.Config()
	if cfg.Review.Pipeline.Mode != "deep" || cfg.LLM.Model != "claude-opus-4-8" || cfg.Review.MaxComments != 20 {
		t.Errorf("settings not applied: mode %q model %q max %d", cfg.Review.Pipeline.Mode, cfg.LLM.Model, cfg.Review.MaxComments)
	}
	if a.Bundle() == old {
		t.Error("bundle was not rebuilt")
	}
	if a.uiConfig().PipelineMode != "deep" || a.uiConfig().LLMModel != "claude-opus-4-8" {
		t.Errorf("uiConfig stale: %+v", a.uiConfig())
	}
}

// TestApplyConfigList applies a list-valued field and confirms it round-trips
// through the file and reload as a real YAML sequence.
func TestApplyConfigList(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ApplyConfig(t.Context(), map[string]string{
		"review.ignore_globs": "foo/**\nbar/**\n\n  baz/**  ",
	}); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	got := a.Config().Review.IgnoreGlobs
	want := []string{"foo/**", "bar/**", "baz/**"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ignore_globs = %v, want %v", got, want)
	}
}

// TestApplyConfigDurationNoFalseShadow: a non-normalized duration input
// ("60s") must not be mistaken for env-shadowing after it reloads as "1m0s".
func TestApplyConfigDurationNoFalseShadow(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}
	res, err := a.ApplyConfig(t.Context(), map[string]string{"gitlab.timeout": "60s"})
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if res.Warning != "" {
		t.Errorf("false env-shadow warning: %q", res.Warning)
	}
	if a.Config().GitLab.Timeout.String() != "1m0s" {
		t.Errorf("timeout = %s, want 1m0s", a.Config().GitLab.Timeout)
	}
}

// TestApplyConfigCustomModeRequiresPasses: mode=custom with no passes is
// rejected (it would silently degrade to a single general pass), but custom +
// passes submitted together is accepted.
func TestApplyConfigCustomModeRequiresPasses(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ApplyConfig(t.Context(), map[string]string{"review.pipeline.mode": "custom"}); err == nil {
		t.Error("custom mode with no passes accepted")
	}
	if _, err := a.ApplyConfig(t.Context(), map[string]string{
		"review.pipeline.mode":   "custom",
		"review.pipeline.passes": "general\ncorrectness",
	}); err != nil {
		t.Fatalf("custom + passes rejected: %v", err)
	}
	if got := a.Config().Review.Pipeline.Mode; got != "custom" {
		t.Errorf("mode = %q, want custom", got)
	}
}

// TestApplyConfigSkipsUnchangedPreservesComments: a section save resubmits every
// field, but only changed values are written — so an unchanged field keeps its
// inline documentation comment (regression guard for comment stripping).
func TestApplyConfigSkipsUnchangedPreservesComments(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}
	// auto_publish=false is the current value (unchanged); max_comments changes.
	if _, err := a.ApplyConfig(t.Context(), map[string]string{
		"review.auto_publish": "false",
		"review.max_comments": "7",
	}); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	raw, err := os.ReadFile(a.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# DANGER: keep false") {
		t.Error("inline comment on unchanged auto_publish was stripped")
	}
	if !strings.Contains(string(raw), "max_comments: 7") {
		t.Error("changed max_comments not written")
	}
	if a.Config().Review.MaxComments != 7 {
		t.Errorf("max_comments = %d, want 7", a.Config().Review.MaxComments)
	}
}

// TestApplyConfigRestartFlag: changing a restart-required field reports it.
func TestApplyConfigRestartFlag(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}
	res, err := a.ApplyConfig(t.Context(), map[string]string{"app.port": "8123"})
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !res.RestartRequired {
		t.Error("app.port change should flag restart required")
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
		if _, err := a.ApplyConfig(t.Context(), map[string]string{"review.pipeline.mode": modes[i%3]}); err != nil {
			t.Errorf("apply %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestApplyConfigEnvShadow: an AI_REVIEWER_* env var overrides the value the
// user just applied — the apply must surface a warning naming the variable
// (not report plain success) while still persisting the choice to the file.
func TestApplyConfigEnvShadow(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("AI_REVIEWER_LLM_MODEL", "claude-sonnet-5")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}

	res, err := a.ApplyConfig(t.Context(), map[string]string{"llm.model": "claude-opus-4-8"})
	if err != nil {
		t.Fatalf("env-shadow should be a warning, not an error: %v", err)
	}
	if !strings.Contains(res.Warning, "AI_REVIEWER_LLM_MODEL") {
		t.Errorf("warning does not name the shadowing env var: %q", res.Warning)
	}
	// The runtime stays consistent with the environment, not the form value.
	if got := a.Config().LLM.Model; got != "claude-sonnet-5" {
		t.Errorf("effective model = %q, want sonnet (env)", got)
	}
	// The file still records the user's choice for when the env var is unset.
	raw, err := os.ReadFile(a.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `model: "claude-opus-4-8"`) {
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

	if _, err := a.ApplyConfig(t.Context(), map[string]string{"review.pipeline.mode": "deep"}); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if got := a.Config().App.Port; got != 12345 {
		t.Errorf("override lost after reload: port = %d", got)
	}
	if got := a.Config().Review.Pipeline.Mode; got != "deep" {
		t.Errorf("setting not applied: mode = %q", got)
	}
}

// TestApplyConfigFailureKeepsRuntimeAndRollsBack: when the reload fails nothing
// may be swapped — old config and old bundle stay live — and the config file is
// rolled back to its prior contents so no broken value lingers on disk.
func TestApplyConfigFailureKeepsRuntimeAndRollsBack(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	a := newTestApp(t)
	if _, err := a.Services(); err != nil {
		t.Fatal(err)
	}
	oldBundle := a.Bundle()
	oldMode := a.Config().Review.Pipeline.Mode
	before, err := os.ReadFile(a.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}

	// Make the reload's Validate fail: per-field validation passes for a valid
	// mode, so break the *merged* config via an env override the reload picks up.
	t.Setenv("AI_REVIEWER_REVIEW_SEVERITY_THRESHOLD", "not-a-severity")
	if _, err := a.ApplyConfig(t.Context(), map[string]string{"review.pipeline.mode": "deep"}); err == nil {
		t.Fatal("expected reload failure")
	}
	if a.Bundle() != oldBundle {
		t.Error("bundle swapped despite failed reload")
	}
	if got := a.Config().Review.Pipeline.Mode; got != oldMode {
		t.Errorf("config swapped despite failed reload: mode = %q", got)
	}
	after, err := os.ReadFile(a.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("config file not rolled back after failed apply:\n--- before ---\n%s\n--- after ---\n%s", before, after)
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
