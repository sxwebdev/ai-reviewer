package config

import (
	"path/filepath"
	"testing"
)

// TestConfigEnvNamesRoundTrip proves configEnvNames() produces the exact env
// var names xconfig honors: setting each computed name must change the loaded
// value. This guards the env-shadow warnings against the naive dot→underscore
// mapping (which is wrong for e.g. gitlab.* → GIT_LAB_*).
func TestConfigEnvNamesRoundTrip(t *testing.T) {
	env := configEnvNames()
	cases := []struct {
		key, val string
		check    func(*Config) string
	}{
		{"gitlab.host", "https://gl.env", func(c *Config) string { return c.GitLab.Host }},
		{"gitlab.token", "glpat-env", func(c *Config) string { return c.GitLab.Token }},
		{"gitlab.token_env", "MY_TOKEN", func(c *Config) string { return c.GitLab.TokenEnv }},
		{"app.port", "8123", func(c *Config) string { return c.App.BindHost /*placeholder*/ }},
		{"llm.model", "claude-env", func(c *Config) string { return c.LLM.Model }},
		{"llm.claude.oauth_token_env", "OAUTH_ENV", func(c *Config) string { return c.LLM.Claude.OAuthTokenEnv }},
		{"review.pipeline.mode", "deep", func(c *Config) string { return c.Review.Pipeline.Mode }},
		{"review.coverage.node.install", "true", func(c *Config) string {
			if c.Review.Coverage.Node.Install {
				return "true"
			}
			return "false"
		}},
	}
	path := filepath.Join(t.TempDir(), "config.yaml") // nonexistent → defaults + env
	for _, tc := range cases {
		name, ok := env[tc.key]
		if !ok {
			t.Errorf("%s: no env name computed", tc.key)
			continue
		}
		if tc.key == "app.port" {
			continue // covered separately below to keep the int assertion honest
		}
		t.Setenv(name, tc.val)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("%s: load: %v", tc.key, err)
		}
		if got := tc.check(cfg); got != tc.val {
			t.Errorf("%s via %s: got %q, want %q", tc.key, name, got, tc.val)
		}
	}

	// app.port: assert the int override lands, using its computed name.
	t.Setenv(env["app.port"], "8123")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.App.Port != 8123 {
		t.Errorf("app.port via %s: got %d, want 8123", env["app.port"], cfg.App.Port)
	}
}

// TestSettingsSchemaCoversEveryConfigKey guards against drift: configEnvNames()
// enumerates every leaf config key by reflection, so any field added to
// config.go but forgotten in SettingsSchema() (silently non-editable in the UI
// and invisible to env-shadow detection) fails here.
func TestSettingsSchemaCoversEveryConfigKey(t *testing.T) {
	t.Parallel()
	schema := SettingsSchemaByKey()
	for key := range configEnvNames() {
		if _, ok := schema[key]; !ok {
			t.Errorf("config key %q has no SettingsSchema entry — add it (or it is intentionally non-editable)", key)
		}
	}
	// And no schema key references a non-existent config field.
	env := configEnvNames()
	for key := range schema {
		if _, ok := env[key]; !ok {
			t.Errorf("schema key %q is not a real config leaf key", key)
		}
	}
}

// TestSettingsSchemaWellFormed checks every field has an env name, a getter that
// does not panic on defaults, and that no duplicate keys exist.
func TestSettingsSchemaWellFormed(t *testing.T) {
	t.Parallel()
	def := DefaultConfig()
	seen := map[string]bool{}
	for _, f := range SettingsSchema() {
		if seen[f.Key] {
			t.Errorf("duplicate key %q", f.Key)
		}
		seen[f.Key] = true
		if f.EnvName == "" {
			t.Errorf("%s: empty EnvName", f.Key)
		}
		if f.Get == nil {
			t.Errorf("%s: nil Get", f.Key)
			continue
		}
		_ = f.Get(def) // must not panic
		if f.Kind == KindSelect && len(f.Options) == 0 && f.Key != "llm.model" {
			t.Errorf("%s: select field without options", f.Key)
		}
	}
}
