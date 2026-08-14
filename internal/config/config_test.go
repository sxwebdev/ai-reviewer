package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/tkcrm/mx/logger"
)

// quietLog is the bootstrap logger Load wants; nothing below fatal is emitted,
// so test output stays clean.
func quietLog() logger.Logger {
	return logger.New(logger.WithConfig(logger.Config{
		Level:  logger.LogLevelFatal,
		Format: logger.LoggerFormatJSON,
	}))
}

// writeConfig writes yml to a temp file and returns its path.
func writeConfig(t *testing.T, yml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// minimalYAML is a config that passes validation, so a test can add exactly the
// one thing it is about.
const minimalYAML = `
gitlab:
  base_url: https://gitlab.example.com
  token: glpat-test
slack:
  token: xoxb-test
postgres:
  username: ai_reviewer
teams:
  - name: payments
    slack_channel: C012345678
    ai_review: { enabled: true }
    repositories: [backend/payments]
`

func loadFile(t *testing.T, path string) (*Config, error) {
	t.Helper()
	cfg := DefaultConfig()
	res, err := Load(t.Context(), quietLog(), cfg, []string{path})
	if res != nil {
		t.Cleanup(res.Cleanup)
	}
	return cfg, err
}

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	// Safety defaults: nothing reaches GitLab or Slack until an operator opts in.
	if c.Service.SlackSendEnabled || c.Service.AIReviewPublishEnabled {
		t.Error("service dry-run switches must default to false")
	}
	if c.Review.Coverage.Enabled {
		t.Error("review.coverage.enabled must default to false (executes repository code)")
	}
	// On by default: the service brings its own schema up, application migrations
	// then River's. What makes that safe with N replicas is App.Migrate holding
	// one advisory lock across both migrators — if that lock ever goes away, this
	// default is what turns the loss into two replicas racing `CREATE TABLE
	// river_job` on a fresh install.
	if !c.Postgres.MigrateOnStart {
		t.Error("postgres.migrate_on_start must default to true; the service migrates itself on start")
	}
	// The ops port has no ingress restriction in the shipped NetworkPolicy
	// (it is egress-only, because probe source addresses are CNI-specific), so
	// a profiler on by default would serve heap and goroutine dumps to anything
	// that can reach the pod. If this ever flips, README's endpoint table and
	// CLAUDE.md both describe it as opt-in and would become wrong.
	if c.Ops.Profiler.Enabled {
		t.Error("ops.profiler.enabled must default to false; /debug/pprof is an explicit opt-in")
	}
	if c.LLM.Claude.Auth.Mode != llm.AuthExistingLogin {
		t.Errorf("claude auth mode = %q, want existing-login", c.LLM.Claude.Auth.Mode)
	}
	if c.Review.MaxComments != 12 {
		t.Errorf("MaxComments = %d, want 12", c.Review.MaxComments)
	}
	if c.GitLab.Timeout != 30*time.Second {
		t.Errorf("GitLab.Timeout = %v, want 30s", c.GitLab.Timeout)
	}
	// WithSkipDefaults() also suppresses mx's own struct-tag defaults, so
	// DefaultConfig must populate the fields mx marks `validate:"required"`.
	if c.Ops.Network == "" || c.Ops.Metrics.Path == "" || c.Ops.Metrics.Port == "" ||
		c.Ops.Healthy.Path == "" || c.Ops.Healthy.Port == "" ||
		c.Ops.Profiler.Path == "" || c.Ops.Profiler.Port == "" {
		t.Errorf("ops defaults incomplete, tag validation would fail: %+v", c.Ops)
	}
	if !c.Log.Format.Valid() || !c.Log.Level.Valid() || !c.Log.Trace.Valid() {
		t.Errorf("logger defaults incomplete: %+v", c.Log)
	}
}

// TestDefaultAllowedToolsAreReadOnlyAndWorktreeScoped pins plan §1's
// "Claude CLI does not write to the repository" at the only place it is
// actually enforced.
//
// Two shipped defaults were the defect. `Bash(git diff *)` is a prefix match and
// `git diff` accepts `--output=<path>` (an arbitrary-file write, which lets a
// "clean build" verdict be manufactured for the deterministic verifiers) and
// `--no-index <any file>` (an arbitrary-file read that walks around any path
// scope on Read). Reproduced against claude 2.1.222 with the shipped flags: the
// write succeeded with `permission_denials: []`. And unscoped `Read`/`Grep`/
// `Glob` made the worktree a working directory rather than a boundary.
func TestDefaultAllowedToolsAreReadOnlyAndWorktreeScoped(t *testing.T) {
	tools := DefaultConfig().LLM.Claude.AllowedTools
	if len(tools) == 0 {
		t.Fatal("allowed_tools default is empty")
	}
	for _, rule := range tools {
		name, args, ok := strings.Cut(strings.TrimSuffix(rule, ")"), "(")
		if !ok {
			t.Errorf("rule %q has no path scope; the worktree would not be a boundary", rule)
			continue
		}
		switch name {
		case "Read", "Grep", "Glob":
		default:
			// Bash in particular: every git subcommand that takes diff options is
			// both a write and an arbitrary-read primitive.
			t.Errorf("rule %q grants %q; only the read-only tools may be granted by default", rule, name)
		}
		if !strings.Contains(args, llm.WorktreePlaceholder) {
			t.Errorf("rule %q is not scoped to %s", rule, llm.WorktreePlaceholder)
		}
	}
}

// TestLoadPreservesExplicitFalse is the key regression: xconfig's defaults
// plugin runs after the file loader and fills every zero value, which would flip
// an explicitly configured `false` back to a `true` default. Load must run with
// WithSkipDefaults() and take its defaults from DefaultConfig() instead.
func TestLoadPreservesExplicitFalse(t *testing.T) {
	path := writeConfig(t, `
gitlab:
  base_url: https://gitlab.example.com
  token: glpat-test
  graphql_enabled: false
slack:
  token: xoxb-test
postgres:
  username: ai_reviewer
  migrate_on_start: false
review:
  max_comments: 3
  risk:
    enabled: false
  context:
    include_full_files: false
    prior_review: false
llm:
  claude:
    agent_mode: false
teams:
  - name: payments
    slack_channel: C012345678
    ai_review: { enabled: true }
    repositories: [backend/payments]
`)
	c, err := loadFile(t, path)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]bool{
		// Opting out of self-migration is the case where this bug would hurt
		// most: a deployment that applies the schema elsewhere would silently
		// get replicas migrating anyway.
		"postgres.migrate_on_start":         c.Postgres.MigrateOnStart,
		"gitlab.graphql_enabled":            c.GitLab.GraphQLEnabled,
		"review.risk.enabled":               c.Review.Risk.Enabled,
		"review.context.include_full_files": c.Review.Context.IncludeFullFiles,
		"review.context.prior_review":       c.Review.Context.PriorReview,
		"llm.claude.agent_mode":             c.LLM.Claude.AgentMode,
	} {
		if got {
			t.Errorf("%s: explicit false in the file was reset to the default true", name)
		}
	}
	if c.Review.MaxComments != 3 {
		t.Errorf("MaxComments = %d, want 3", c.Review.MaxComments)
	}
	// A field absent from the file keeps its default.
	if c.Review.SeverityThreshold != "medium" {
		t.Errorf("SeverityThreshold = %q, want default medium", c.Review.SeverityThreshold)
	}
	if !c.Review.Context.IncludeCommits {
		t.Error("include_commits was absent from the file and must keep its default true")
	}
}

// TestEnvNamesRoundTrip proves the env variable names documented in config.go
// and config.example.yaml are the ones xconfig actually reads. Without explicit
// `env:` tags xconfig derives them by word-splitting the Go field path, which
// turns gitlab.token into AI_REVIEWER_GIT_LAB_TOKEN — and, because xconfigvault
// reads the same metadata, would put the secret in Vault under that name too.
func TestEnvNamesRoundTrip(t *testing.T) {
	cases := []struct {
		env   string
		value string
		got   func(*Config) string
	}{
		{"AI_REVIEWER_GITLAB_BASE_URL", "https://gl.env", func(c *Config) string { return c.GitLab.BaseURL }},
		{"AI_REVIEWER_GITLAB_TOKEN", "glpat-env", func(c *Config) string { return c.GitLab.Token.Unmask() }},
		{"AI_REVIEWER_SLACK_TOKEN", "xoxb-env", func(c *Config) string { return c.Slack.Token.Unmask() }},
		{"AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-env", func(c *Config) string { return c.LLM.Claude.Auth.OAuthToken.Unmask() }},
		{"AI_REVIEWER_ANTHROPIC_API_KEY", "sk-ant-api-env", func(c *Config) string { return c.LLM.Claude.Auth.APIKey.Unmask() }},
		{"AI_REVIEWER_LLM_CLAUDE_AUTH_MODE", "api-key", func(c *Config) string { return string(c.LLM.Claude.Auth.Mode) }},
		{"AI_REVIEWER_LLM_CLAUDE_BIN", "/usr/local/bin/claude", func(c *Config) string { return c.LLM.Claude.Bin }},
		{"AI_REVIEWER_LOG_LEVEL", "debug", func(c *Config) string { return string(c.Log.Level) }},
		{"AI_REVIEWER_OPS_METRICS_PORT", "10002", func(c *Config) string { return c.Ops.Metrics.Port }},
		{"AI_REVIEWER_POSTGRES_HOST", "pg.env", func(c *Config) string { return c.Postgres.Host }},
		{"AI_REVIEWER_POSTGRES_USERNAME", "pguser", func(c *Config) string { return c.Postgres.Username.Unmask() }},
		{"AI_REVIEWER_POSTGRES_PASSWORD", "pgpass", func(c *Config) string { return c.Postgres.Password.Unmask() }},
		{"AI_REVIEWER_JOBS_QUEUES_DEFAULT", "7", func(c *Config) string { return itoa(c.Jobs.Queues.Default) }},
		{"AI_REVIEWER_REVIEW_PIPELINE_MODE", "deep", func(c *Config) string { return c.Review.Pipeline.Mode }},
		{"AI_REVIEWER_REVIEW_WORKDIR", "/tmp/work", func(c *Config) string { return c.Review.WorkDir }},
		{"AI_REVIEWER_SERVICE_SLACK_SEND_ENABLED", "true", func(c *Config) string { return btoa(c.Service.SlackSendEnabled) }},
		{"AI_REVIEWER_REVIEW_COVERAGE_NODE_INSTALL", "true", func(c *Config) string { return btoa(c.Review.Coverage.Node.Install) }},
		// Slice-of-struct: teams are configurable entirely through env in k8s.
		{"AI_REVIEWER_TEAMS_0_NAME", "platform", func(c *Config) string { return c.Teams[0].Name }},
		{"AI_REVIEWER_TEAMS_0_SLACK_CHANNEL", "C987654321", func(c *Config) string { return c.Teams[0].SlackChannel }},
		{"AI_REVIEWER_TEAMS_0_REPOSITORIES", "platform/auth", func(c *Config) string { return strings.Join(c.Teams[0].Repositories, ",") }},
		{"AI_REVIEWER_TEAMS_0_AI_REVIEW_ENABLED", "true", func(c *Config) string { return btoa(c.Teams[0].AIReview.Enabled) }},
	}

	// The whole set is applied at once: a name that silently shadows another
	// (the derivation footgun) shows up as a mismatch on the losing field.
	for _, tc := range cases {
		t.Setenv(tc.env, tc.value)
	}
	// api-key mode needs its secret, which the table sets; the base file only
	// supplies what the env cases do not.
	c, err := loadFile(t, writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, tc := range cases {
		if got := tc.got(c); got != tc.value {
			t.Errorf("%s: got %q, want %q", tc.env, got, tc.value)
		}
	}
}

// TestExampleConfigLoads pins config.example.yaml against the schema: an unknown
// or renamed key fails the load (WithDisallowUnknownFields), and the file must
// still describe a valid deployment once the secrets it deliberately leaves
// empty are supplied.
func TestExampleConfigLoads(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config.example.yaml is missing: %v", err)
	}
	t.Setenv("AI_REVIEWER_GITLAB_TOKEN", "glpat-example")
	t.Setenv("AI_REVIEWER_SLACK_TOKEN", "xoxb-example")
	t.Setenv("AI_REVIEWER_POSTGRES_USERNAME", "ai_reviewer")

	c, err := loadFile(t, path)
	if err != nil {
		t.Fatalf("config.example.yaml does not load: %v", err)
	}
	if len(c.Teams) != 2 {
		t.Errorf("teams = %d, want the 2 example teams", len(c.Teams))
	}
	if c.Service.SlackSendEnabled || c.Service.AIReviewPublishEnabled {
		t.Error("the example must ship with both dry-run switches off")
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	path := writeConfig(t, minimalYAML+"watch:\n  enabled: true\n")
	if _, err := loadFile(t, path); err == nil {
		t.Fatal("a removed config section must be rejected, not silently ignored")
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	c := DefaultConfig()
	c.GitLab.BaseURL = "gitlab.example.com" // not absolute
	c.GitLab.Token = ""
	c.Review.ScanInterval = 10 * time.Second
	c.Review.MaxComments = 0
	c.Teams = nil

	err := c.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"gitlab.base_url", "gitlab.token", "review.scan_interval",
		"review.max_comments", "teams is empty",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error is missing %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "problem(s)") {
		t.Errorf("errors should be rendered as a numbered list:\n%s", msg)
	}
}

// rawTagError is go-playground's own rendering. It names a Go field path, not a
// config key, and can never name the env variable or the Vault key — so it must
// not be what an operator sees.
const rawTagError = "failed on the 'required' tag"

// TestMissingSecretsAreReportedInOperatorVocabulary drives the whole load path,
// not Validate() in isolation: the defect it pins is one of *ordering*. xconfig
// runs Validate() before the tag validator, so a required secret with no domain
// check here falls through to the tag and the operator gets
//
//	load config: Key: 'Config.Postgres.Username' Error:Field validation for 'Username' failed on the 'required' tag
//
// which is exactly what someone starting from config.example.yaml hit first.
func TestMissingSecretsAreReportedInOperatorVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name string
		yml  string
		want []string
	}{
		{
			name: "postgres.username",
			yml: `
gitlab:
  base_url: https://gitlab.example.com
  token: glpat-test
slack:
  token: xoxb-test
teams:
  - name: payments
    slack_channel: C012345678
    ai_review: { enabled: true }
    repositories: [backend/payments]
`,
			want: []string{
				"postgres.username is empty",
				"AI_REVIEWER_POSTGRES_USERNAME",
				"Vault key",
			},
		},
		{
			name: "gitlab.token",
			yml: `
gitlab:
  base_url: https://gitlab.example.com
slack:
  token: xoxb-test
postgres:
  username: ai_reviewer
teams:
  - name: payments
    slack_channel: C012345678
    ai_review: { enabled: true }
    repositories: [backend/payments]
`,
			want: []string{
				"gitlab.token is empty",
				"AI_REVIEWER_GITLAB_TOKEN",
				"Vault key",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFile(t, writeConfig(t, tc.yml))
			if err == nil {
				t.Fatal("a missing required secret must fail the load")
			}
			msg := err.Error()
			if strings.Contains(msg, rawTagError) {
				t.Errorf("the operator got go-playground's raw rendering:\n%s", msg)
			}
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("error is missing %q:\n%s", want, msg)
				}
			}
			// The aggregated numbered list is deliberate; a tag failure would
			// short-circuit it.
			if !strings.Contains(msg, "problem(s)") {
				t.Errorf("the problem was not reported in the aggregated list:\n%s", msg)
			}
		})
	}
}

// A missing secret must not stop the other checks from being reported: fixing a
// config one error per restart is the feedback loop joinConfigErrors exists to
// avoid, and a tag failure would produce exactly that.
func TestMissingSecretsStillAggregateWithEverythingElse(t *testing.T) {
	c := DefaultConfig()
	c.GitLab.BaseURL = "https://gitlab.example.com"
	c.GitLab.Token = ""
	c.Postgres.Username = ""
	c.Review.MaxComments = 0
	c.Teams = nil

	err := c.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"postgres.username is empty", "gitlab.token is empty",
		"review.max_comments", "teams is empty", "4 problem(s)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error is missing %q:\n%s", want, msg)
		}
	}
}

// postgres.password is deliberately optional — IAM and peer authentication run
// without one — so it must not acquire a required check by symmetry with the
// username.
func TestPostgresPasswordIsOptional(t *testing.T) {
	c := DefaultConfig()
	c.GitLab.BaseURL = "https://gitlab.example.com"
	c.GitLab.Token = "glpat-test"
	c.Slack.Token = "xoxb-test"
	c.Postgres.Username = "ai_reviewer"
	c.Postgres.Password = ""
	c.Teams = []TeamConfig{{
		Name: "payments", SlackChannel: "C012345678", Repositories: []string{"backend/payments"},
	}}

	if err := c.Validate(); err != nil {
		t.Errorf("an empty postgres.password must be valid: %v", err)
	}
}

// The Vault bootstrap fails earlier than anything else — before the main config
// is read — so its message is the first thing an operator setting Vault up sees.
// It is loaded from a separate source set with no YAML keys, so its checks name
// the (unprefixed) environment variables directly.
func TestVaultBootstrapNamesItsVariables(t *testing.T) {
	setVault := func(t *testing.T, kv map[string]string) {
		t.Helper()
		// Every VAULT_* the bootstrap reads is set explicitly, so a developer
		// machine with its own Vault environment cannot change the outcome.
		for _, k := range []string{
			"VAULT_ENABLED", "VAULT_ADDR", "VAULT_SECRET_PATH", "VAULT_AUTH_KIND",
			"VAULT_TOKEN", "VAULT_KUBE_ROLE", "VAULT_KUBE_JWT_PATH", "VAULT_KUBE_MOUNT_PATH",
		} {
			t.Setenv(k, kv[k])
		}
	}

	t.Run("token auth with no token", func(t *testing.T) {
		setVault(t, map[string]string{
			"VAULT_ENABLED":     "true",
			"VAULT_ADDR":        "https://vault.example.com",
			"VAULT_SECRET_PATH": "secret/data/ai-reviewer",
			"VAULT_AUTH_KIND":   "token",
		})

		_, err := loadFile(t, writeConfig(t, minimalYAML))
		if err == nil {
			t.Fatal("vault token auth with no token must fail the load")
		}
		msg := err.Error()
		if strings.Contains(msg, rawTagError) {
			t.Errorf("the operator got go-playground's raw rendering:\n%s", msg)
		}
		if !strings.Contains(msg, "VAULT_TOKEN is empty but VAULT_AUTH_KIND=token requires it") {
			t.Errorf("error does not name VAULT_TOKEN and why it is needed:\n%s", msg)
		}
	})

	t.Run("every bootstrap problem at once", func(t *testing.T) {
		setVault(t, map[string]string{"VAULT_ENABLED": "true", "VAULT_AUTH_KIND": "token"})

		_, err := loadFile(t, writeConfig(t, minimalYAML))
		if err == nil {
			t.Fatal("want an error")
		}
		msg := err.Error()
		for _, want := range []string{"VAULT_ADDR", "VAULT_SECRET_PATH", "VAULT_TOKEN", "3 problem(s)"} {
			if !strings.Contains(msg, want) {
				t.Errorf("aggregated bootstrap error is missing %q:\n%s", want, msg)
			}
		}
	})

	t.Run("disabled vault needs nothing", func(t *testing.T) {
		setVault(t, map[string]string{"VAULT_ENABLED": "false"})
		if _, err := loadFile(t, writeConfig(t, minimalYAML)); err != nil {
			t.Errorf("a disabled Vault must not require its variables: %v", err)
		}
	})
}

// TestSecretsAreNotValidatedByTag is the structural guard behind the rule in
// Validate's doc comment. Re-adding `validate:"required"` to a Secret would
// compile, pass every behavioural test whose field also has a domain check, and
// silently regress the message for any field that does not — which is how
// postgres.username came to greet operators with a Go field path.
func TestSecretsAreNotValidatedByTag(t *testing.T) {
	t.Parallel()
	secretType := reflect.TypeFor[Secret]()

	var walk func(rt reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(rt reflect.Type, path string, seen map[reflect.Type]bool) {
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for f := range rt.Fields() {
			f := f
			if f.Type == secretType {
				if v, ok := f.Tag.Lookup("validate"); ok && strings.Contains(v, "required") {
					t.Errorf("%s.%s is a Secret with validate:%q — the tag validator cannot name the "+
						"env variable or the Vault key, and it short-circuits the aggregated list; "+
						"check it in Validate() instead", path, f.Name, v)
				}
				continue
			}
			walk(f.Type, path+"."+f.Name, seen)
		}
	}
	walk(reflect.TypeFor[Config](), "Config", map[reflect.Type]bool{})
	walk(reflect.TypeFor[VaultConfig](), "VaultConfig", map[reflect.Type]bool{})
}

func TestValidateTeams(t *testing.T) {
	base := func() *Config {
		c := DefaultConfig()
		c.GitLab.BaseURL = "https://gitlab.example.com"
		c.GitLab.Token = "glpat-x"
		c.Slack.Token = "xoxb-x"
		c.Postgres.Username = "ai_reviewer"
		return c
	}

	tests := []struct {
		name  string
		teams []TeamConfig
		want  string
	}{
		{
			name: "ok",
			teams: []TeamConfig{
				{Name: "payments", SlackChannel: "C012345678", Repositories: []string{"backend/payments", "42"}},
			},
		},
		{
			name: "duplicate names differing only in case",
			teams: []TeamConfig{
				{Name: "payments", SlackChannel: "C012345678", Repositories: []string{"a/b"}},
				{Name: "Payments", SlackChannel: "C012345679", Repositories: []string{"c/d"}},
			},
			want: "duplicate team name",
		},
		{
			name: "repository claimed twice",
			teams: []TeamConfig{
				{Name: "payments", SlackChannel: "C012345678", Repositories: []string{"a/b"}},
				{Name: "platform", SlackChannel: "C012345679", Repositories: []string{"a/b"}},
			},
			want: `claimed by both team "payments" and team "platform"`,
		},
		{
			name:  "empty repositories",
			teams: []TeamConfig{{Name: "payments", SlackChannel: "C012345678"}},
			want:  "repositories is empty",
		},
		{
			name:  "channel name instead of id",
			teams: []TeamConfig{{Name: "payments", SlackChannel: "#payments", Repositories: []string{"a/b"}}},
			want:  "not a Slack channel id",
		},
		{
			name:  "repository is neither a path nor an id",
			teams: []TeamConfig{{Name: "payments", SlackChannel: "C012345678", Repositories: []string{"not a repo!"}}},
			want:  "neither a GitLab path",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			c.Teams = tc.teams
			err := c.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want == "":
			case err == nil:
				t.Fatalf("want an error containing %q, got nil", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateClaudeAuthMode(t *testing.T) {
	base := func() *Config {
		c := DefaultConfig()
		c.GitLab.BaseURL = "https://gitlab.example.com"
		c.GitLab.Token = "glpat-x"
		c.Slack.Token = "xoxb-x"
		c.Postgres.Username = "ai_reviewer"
		c.Teams = []TeamConfig{{Name: "t", SlackChannel: "C012345678", Repositories: []string{"a/b"}}}
		return c
	}

	t.Run("unknown mode", func(t *testing.T) {
		c := base()
		c.LLM.Claude.Auth.Mode = "bogus"
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "llm.claude.auth.mode") {
			t.Fatalf("want a mode error, got %v", err)
		}
	})
	t.Run("oauth-token without a token", func(t *testing.T) {
		c := base()
		c.LLM.Claude.Auth.Mode = llm.AuthOAuthToken
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "requires oauth_token") {
			t.Fatalf("want a missing-secret error, got %v", err)
		}
	})
	t.Run("existing-login with a stray secret", func(t *testing.T) {
		c := base()
		c.LLM.Claude.Auth.APIKey = "sk-ant-stray"
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "takes no secret") {
			t.Fatalf("want a stray-secret error, got %v", err)
		}
	})
	t.Run("api-key with its key", func(t *testing.T) {
		c := base()
		c.LLM.Claude.Auth.Mode = llm.AuthAPIKey
		c.LLM.Claude.Auth.APIKey = "sk-ant-real"
		if err := c.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestValidateSlackTokenRequiredWhenChannelsConfigured(t *testing.T) {
	c := DefaultConfig()
	c.GitLab.BaseURL = "https://gitlab.example.com"
	c.GitLab.Token = "glpat-x"
	c.Postgres.Username = "ai_reviewer"
	c.Teams = []TeamConfig{{Name: "t", SlackChannel: "C012345678", Repositories: []string{"a/b"}}}

	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "slack.token is empty") {
		t.Fatalf("a configured channel with no token must fail, got %v", err)
	}
	c.Slack.Token = "xoxb-x"
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSecretNeverLeaks(t *testing.T) {
	s := Secret("glpat-supersecret")
	if got := s.Unmask(); got != "glpat-supersecret" {
		t.Errorf("Unmask() = %q", got)
	}
	if !s.IsSet() || Secret("").IsSet() {
		t.Error("IsSet is wrong")
	}
	for name, got := range map[string]string{
		"String":   s.String(),
		"GoString": s.GoString(),
		"%v":       fmtSprintf("%v", s),
		"%s":       fmtSprintf("%s", s),
		"%#v":      fmtSprintf("%#v", s),
	} {
		if got != redactedPlaceholder {
			t.Errorf("%s = %q, want %q", name, got, redactedPlaceholder)
		}
	}
	j, err := json.Marshal(struct{ T Secret }{s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(j), "supersecret") {
		t.Errorf("JSON leaked the secret: %s", j)
	}
	y, err := s.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	if y != redactedPlaceholder {
		t.Errorf("MarshalYAML = %v", y)
	}
	// A whole Config formatted with %+v must not leak either — that is how a
	// config dump reaches a log line.
	c := DefaultConfig()
	c.GitLab.Token = "glpat-supersecret"
	if strings.Contains(fmtSprintf("%+v", *c), "supersecret") {
		t.Error("formatting the Config leaked a secret")
	}
}

// TestRegisterSecretsCoversEverySecretField walks the schema for `Secret`
// fields, fills each with a distinctive value, and asserts the redactor masks
// all of them.
//
// It is written reflectively on purpose: the list in RegisterSecrets is
// hand-maintained, and the failure mode of forgetting an entry is silent —
// the field is still masked in a config dump and still read from Vault, so it
// looks handled, while any library that puts it in an error prints it in the
// clear. Postgres.Username was exactly that, for long enough that a comment in
// doctor justified another omission by claiming this one was covered.
func TestRegisterSecretsCoversEverySecretField(t *testing.T) {
	c := DefaultConfig()
	secretType := reflect.TypeFor[Secret]()

	// path → the value planted there, so a failure names the field.
	planted := map[string]string{}
	var plant func(v reflect.Value, path string)
	plant = func(v reflect.Value, path string) {
		if v.Type() == secretType {
			// Distinctive and long enough to clear the redactor's minimum length;
			// nothing else in the process can contain it.
			val := "registered-secret-probe-" + strings.ReplaceAll(path, ".", "-")
			v.SetString(val)
			planted[path] = val
			return
		}
		if v.Kind() != reflect.Struct {
			return
		}
		for i := range v.NumField() {
			if !v.Field(i).CanSet() {
				continue
			}
			plant(v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	}
	plant(reflect.ValueOf(c).Elem(), "Config")

	if len(planted) < 6 {
		t.Fatalf("the walk found only %d Secret fields (%v); it is not covering the schema", len(planted), planted)
	}

	c.RegisterSecrets()
	for path, val := range planted {
		if security.Mask(val) == val {
			t.Errorf("%s is a Secret but is not registered with the redactor; add it to RegisterSecrets", path)
		}
	}
}

// The Vault token is the root of trust for every other secret, and it is loaded
// from a different source set than the rest of the schema — so it needs its own
// registration and its own guard.
func TestVaultConfigRegistersItsToken(t *testing.T) {
	const token = "hvs.registeredvaulttokenprobe"
	VaultConfig{Token: token}.RegisterSecrets()
	if security.Mask(token) == token {
		t.Error("VAULT_TOKEN is not registered with the redactor")
	}
}

func TestPostgresDSN(t *testing.T) {
	c := PostgresConfig{Host: "db", Port: "5432", Database: "ai_reviewer", Username: "u", Password: "p", SSLMode: "require"}
	if got, want := c.DSN(), "postgres://u:p@db:5432/ai_reviewer?sslmode=require"; got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
	// No password (trust/peer auth) must not produce a stray ":@".
	c.Password = ""
	if got, want := c.DSN(), "postgres://u@db:5432/ai_reviewer?sslmode=require"; got != want {
		t.Errorf("DSN() without password = %q, want %q", got, want)
	}
}

// Small helpers so the env round-trip table can compare everything as strings.
func itoa(v int) string  { return strconv.Itoa(v) }
func btoa(v bool) string { return strconv.FormatBool(v) }

func fmtSprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// TestSchemaHasNoTagDefaults pins the other half of the explicit-false
// invariant. Load runs with WithSkipDefaults(), so a `default:` tag on one of
// our fields would be dead weight; and if that option were ever dropped, a
// `default:"true"` tag would resurrect the footgun the whole DefaultConfig()
// arrangement exists to avoid — xconfig's defaults plugin runs after the file
// loader and overwrites every zero value, turning a configured `false` back
// into `true`. Defaults belong in DefaultConfig(), full stop.
//
// Third-party embedded configs (logger.Config, ops.Config) are exempt: we do
// not own their tags, which is exactly why DefaultConfig() restates their
// values.
func TestSchemaHasNoTagDefaults(t *testing.T) {
	t.Parallel()
	pkg := reflect.TypeFor[Config]().PkgPath()

	var walk func(t reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(rt reflect.Type, path string, seen map[reflect.Type]bool) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || rt.PkgPath() != pkg || seen[rt] {
			return
		}
		seen[rt] = true
		for f := range rt.Fields() {
			f := f
			if v, ok := f.Tag.Lookup("default"); ok {
				t.Errorf("%s.%s has default:%q — move it to DefaultConfig()", path, f.Name, v)
			}
			walk(f.Type, path+"."+f.Name, seen)
		}
	}
	walk(reflect.TypeFor[Config](), "Config", map[reflect.Type]bool{})
}
