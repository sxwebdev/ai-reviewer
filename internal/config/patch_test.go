package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchFileOverTemplate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteDefaultFile(path); err != nil {
		t.Fatal(err)
	}

	err := PatchFile(path, map[string]string{
		"gitlab.host":          "https://gitlab.example.com",
		"gitlab.token":         `glpat-s3cr"et`,
		"gitlab.username":      "vasya",
		"review.pipeline.mode": "deep",
		"llm.model":            "opus",
	})
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Comments and unrelated keys survive.
	for _, want := range []string{
		"# ai-reviewer configuration",
		"# DANGER: keep false",
		`# token_env: "GITLAB_TOKEN"`,
		"vendor/**",
		"verifiers:",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("patched file lost %q", want)
		}
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitLab.Host != "https://gitlab.example.com" {
		t.Errorf("host = %q", cfg.GitLab.Host)
	}
	if cfg.GitLab.Token != `glpat-s3cr"et` {
		t.Errorf("token did not round-trip: %q", cfg.GitLab.Token)
	}
	if cfg.GitLab.Username != "vasya" {
		t.Errorf("username = %q", cfg.GitLab.Username)
	}
	if cfg.Review.Pipeline.Mode != "deep" {
		t.Errorf("pipeline mode = %q", cfg.Review.Pipeline.Mode)
	}
	if cfg.LLM.Model != "opus" {
		t.Errorf("model = %q", cfg.LLM.Model)
	}
}

func TestPatchFileCreatesMissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")

	if err := PatchFile(path, map[string]string{"gitlab.host": "https://gl.local"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# ai-reviewer configuration") {
		t.Error("created file is not based on the commented template")
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitLab.Host != "https://gl.local" {
		t.Errorf("host = %q", cfg.GitLab.Host)
	}
}

func TestPatchFileCreatesMissingKeys(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	minimal := "# keep me\ngitlab:\n  host: \"https://old.local\"\n"
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}

	err := PatchFile(path, map[string]string{
		"gitlab.username":      "vasya",
		"llm.model":            "haiku",
		"review.pipeline.mode": "cheap",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# keep me") {
		t.Error("existing comment lost")
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitLab.Host != "https://old.local" {
		t.Errorf("untouched key changed: host = %q", cfg.GitLab.Host)
	}
	if cfg.GitLab.Username != "vasya" {
		t.Errorf("username = %q", cfg.GitLab.Username)
	}
	if cfg.LLM.Model != "haiku" {
		t.Errorf("model = %q", cfg.LLM.Model)
	}
	if cfg.Review.Pipeline.Mode != "cheap" {
		t.Errorf("pipeline mode = %q", cfg.Review.Pipeline.Mode)
	}
}

func TestPatchFileEmptyFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PatchFile(path, map[string]string{"llm.model": "opus"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Model != "opus" {
		t.Errorf("model = %q", cfg.LLM.Model)
	}
}

func TestPatchFileListReplacesBlockSequence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteDefaultFile(path); err != nil {
		t.Fatal(err)
	}

	// ignore_globs exists as a block sequence in the template; verifiers too.
	// A missing list key (extra_args) exercises the merge-into-ancestor path.
	err := PatchFileMixed(path, map[string]string{
		"review.ignore_globs":       FormatYAMLList([]string{"a/**", "b/**"}),
		"review.pipeline.verifiers": FormatYAMLList([]string{"go_build", "go_test"}),
		"llm.claude.extra_args":     FormatYAMLList([]string{"--foo", "--bar=baz"}),
		"review.pipeline.passes":    FormatYAMLList(nil), // empty → []
	}, map[string]bool{
		"review.ignore_globs":       true,
		"review.pipeline.verifiers": true,
		"llm.claude.extra_args":     true,
		"review.pipeline.passes":    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Comments on unrelated keys survive the sequence replacement.
	if !strings.Contains(string(raw), "# ai-reviewer configuration") {
		t.Error("patched file lost header comment")
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Review.IgnoreGlobs, ","); got != "a/**,b/**" {
		t.Errorf("ignore_globs = %q", got)
	}
	if got := strings.Join(cfg.Review.Pipeline.Verifiers, ","); got != "go_build,go_test" {
		t.Errorf("verifiers = %q", got)
	}
	if got := strings.Join(cfg.LLM.Claude.ExtraArgs, ","); got != "--foo,--bar=baz" {
		t.Errorf("extra_args = %q", got)
	}
	if len(cfg.Review.Pipeline.Passes) != 0 {
		t.Errorf("passes = %v, want empty", cfg.Review.Pipeline.Passes)
	}
}

func TestPatchFileNoValues(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := PatchFile(path, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("no-op patch should not create the file")
	}
}
