package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.App.BindHost != "127.0.0.1" {
		t.Errorf("BindHost = %q, want 127.0.0.1", c.App.BindHost)
	}
	if !c.Review.AutoReview {
		t.Error("AutoReview should default true")
	}
	if c.Review.AutoPublish {
		t.Error("AutoPublish must default false (safety)")
	}
	if c.Review.MaxComments != 12 {
		t.Errorf("MaxComments = %d, want 12", c.Review.MaxComments)
	}
	if c.GitLab.Timeout != 30*time.Second {
		t.Errorf("GitLab.Timeout = %v, want 30s", c.GitLab.Timeout)
	}
	if len(c.Review.IgnoreGlobs) == 0 {
		t.Error("IgnoreGlobs should be populated")
	}
}

// TestLoadPreservesExplicitFalse is the key regression: a user explicitly
// setting a default-true boolean to false must not be reset by a default.
func TestLoadPreservesExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
app:
  open_browser: false
review:
  auto_review: false
  max_comments: 3
storage:
  db_path: "` + dir + `/state.db"
  cache_dir: "` + dir + `/cache"
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.App.OpenBrowser {
		t.Error("open_browser=false in file was reset to true")
	}
	if c.Review.AutoReview {
		t.Error("auto_review=false in file was reset to true")
	}
	if c.Review.MaxComments != 3 {
		t.Errorf("MaxComments = %d, want 3", c.Review.MaxComments)
	}
	// A field absent from the file keeps its default.
	if c.Review.SeverityThreshold != "medium" {
		t.Errorf("SeverityThreshold = %q, want default medium", c.Review.SeverityThreshold)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("storage:\n  db_path: \""+dir+"/state.db\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_REVIEWER_GITLAB_HOST", "https://gitlab.test")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.GitLab.Host != "https://gitlab.test" {
		t.Errorf("GitLab.Host = %q, want env override", c.GitLab.Host)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("missing optional file should not error: %v", err)
	}
	if c.LLM.Provider != "claude-cli" {
		t.Errorf("Provider = %q, want claude-cli", c.LLM.Provider)
	}
}

func TestGitLabToken(t *testing.T) {
	c := DefaultConfig()
	c.GitLab.TokenEnv = "MY_TEST_TOKEN"
	t.Setenv("MY_TEST_TOKEN", "glpat-secret")
	if got := c.GitLabToken(); got != "glpat-secret" {
		t.Errorf("GitLabToken() = %q", got)
	}
}

func TestExpandPaths(t *testing.T) {
	c := DefaultConfig()
	if err := c.ExpandPaths(); err != nil {
		t.Fatal(err)
	}
	if len(c.Storage.DBPath) == 0 || c.Storage.DBPath[0] == '~' {
		t.Errorf("DBPath not expanded: %q", c.Storage.DBPath)
	}
	if !filepath.IsAbs(c.Storage.DBPath) {
		t.Errorf("DBPath should be absolute: %q", c.Storage.DBPath)
	}
}

func TestValidate(t *testing.T) {
	c := DefaultConfig()
	if err := c.Validate(); err != nil {
		t.Errorf("default config should validate: %v", err)
	}
	c.LLM.Provider = "bogus"
	if err := c.Validate(); err == nil {
		t.Error("bogus provider should fail validation")
	}
}
