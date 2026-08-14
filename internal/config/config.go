// Package config defines the ai-reviewer configuration schema and loading.
//
// Sources, in increasing priority: YAML file(s) → environment (AI_REVIEWER_*) →
// Vault. See load.go.
//
// # Two invariants that silently break things if dropped
//
// Defaults do NOT come from xconfig's `default` struct tags. The defaults plugin
// runs after the file loader and fills every *zero* value, which flips an
// explicit `false` in the YAML back to a `true` default. So DefaultConfig() is
// the single source of truth for defaults and Load runs with
// WithSkipDefaults()/WithSkipCustomDefaults(). This is pinned by
// TestLoadPreservesExplicitFalse. Note the consequence: struct defaults of
// embedded third-party configs (logger.Config, ops.Config) are skipped too, so
// DefaultConfig() populates them explicitly.
//
// Every field an operator can set from outside carries an explicit `env:` tag.
// Without one, xconfig derives the name by word-splitting the Go field path
// ("GitLab" → Git+Lab, "OAuthToken" → O+Auth+Token), so gitlab.token would be
// AI_REVIEWER_GIT_LAB_TOKEN. An explicit tag short-circuits the derivation and
// only the prefix is prepended. The same names double as Vault keys, because
// xconfigvault reads Meta["env"]. TestEnvNamesRoundTrip asserts the documented
// names are the ones actually read.
package config

import (
	"net"
	"net/url"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
	"github.com/tkcrm/mx/launcher/ops"
	"github.com/tkcrm/mx/logger"
	"github.com/tkcrm/mx/transport/http_transport"
)

// EnvPrefix is prepended to every environment variable name (xconfig's
// WithEnvPrefix). It is exported so tests and diagnostics can name the exact
// variable an operator must set.
const EnvPrefix = "AI_REVIEWER"

// Config is the root configuration for the ai-reviewer team service.
type Config struct {
	Log      logger.Config  `yaml:"log"`
	Ops      ops.Config     `yaml:"ops"`
	Service  ServiceConfig  `yaml:"service"`
	Postgres PostgresConfig `yaml:"postgres"`
	Jobs     JobsConfig     `yaml:"jobs"`
	GitLab   GitLabConfig   `yaml:"gitlab"`
	Slack    SlackConfig    `yaml:"slack"`
	LLM      LLMConfig      `yaml:"llm"`
	Review   ReviewConfig   `yaml:"review"`
	Teams    []TeamConfig   `yaml:"teams" validate:"dive"`
}

// ServiceConfig holds the two global dry-run switches. Both default to false so
// a fresh deployment observes and reports without writing anything to GitLab or
// Slack until an operator opts in.
type ServiceConfig struct {
	SlackSendEnabled       bool `yaml:"slack_send_enabled" env:"SERVICE_SLACK_SEND_ENABLED" usage:"Actually deliver digests to Slack (off = dry-run, messages are only persisted)"`
	AIReviewPublishEnabled bool `yaml:"ai_review_publish_enabled" env:"SERVICE_AI_REVIEW_PUBLISH_ENABLED" usage:"Actually publish review findings to GitLab (off = dry-run)"`
}

// PostgresConfig is the operational store. Username/password are Vault-backed:
// in production they never appear in YAML or env.
type PostgresConfig struct {
	Host     string `yaml:"host" env:"POSTGRES_HOST" validate:"required" usage:"PostgreSQL host"`
	Port     string `yaml:"port" env:"POSTGRES_PORT" validate:"required" usage:"PostgreSQL port"`
	Database string `yaml:"database" env:"POSTGRES_DATABASE" validate:"required" usage:"PostgreSQL database name"`
	// Required, but checked in Validate() rather than by a `validate:"required"`
	// tag — see the secret-validation note there.
	Username Secret `yaml:"username" env:"POSTGRES_USERNAME" secret:"true" vault:"true" usage:"PostgreSQL user"`
	// Password is not `required`: local development and IAM/peer authentication
	// legitimately run without one. An empty password simply produces a DSN
	// without one.
	Password Secret `yaml:"password" env:"POSTGRES_PASSWORD" secret:"true" vault:"true" usage:"PostgreSQL password"`
	SSLMode  string `yaml:"ssl_mode" env:"POSTGRES_SSL_MODE" validate:"required,oneof=disable allow prefer require verify-ca verify-full" usage:"PostgreSQL sslmode"`
	// MigrateOnStart applies pending migrations at startup — both the
	// application's schema and River's own, in that order.
	//
	// On by default, and safe with any number of replicas: App.Migrate holds one
	// session advisory lock across BOTH migrators, so replicas starting together
	// serialise and every one after the first finds nothing pending. Turn it off
	// only if your deployment applies the schema somewhere else (an init
	// container, a release job) and you want a replica that refuses to start
	// rather than one that migrates.
	MigrateOnStart bool `yaml:"migrate_on_start" env:"POSTGRES_MIGRATE_ON_START" usage:"Apply pending migrations (application + River) on start"`
}

// DSN returns a pgx v5 compatible connection URL.
func (c PostgresConfig) DSN() string {
	user := url.User(c.Username.Unmask())
	if c.Password.IsSet() {
		user = url.UserPassword(c.Username.Unmask(), c.Password.Unmask())
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     user,
		Host:     net.JoinHostPort(c.Host, c.Port),
		Path:     c.Database,
		RawQuery: url.Values{"sslmode": {c.SSLMode}}.Encode(),
	}
	return u.String()
}

// JobsConfig tunes the River runtime.
type JobsConfig struct {
	// DrainTimeout is River's SoftStopTimeout: on shutdown in-flight jobs get
	// this long to finish before the rest are hard-cancelled. Keep it below the
	// jobs service ShutdownTimeout and the pod's terminationGracePeriodSeconds.
	DrainTimeout    time.Duration `yaml:"drain_timeout" env:"JOBS_DRAIN_TIMEOUT" usage:"Graceful drain window for in-flight jobs on shutdown"`
	Queues          QueuesConfig  `yaml:"queues"`
	CleanupInterval time.Duration `yaml:"cleanup_interval" env:"JOBS_CLEANUP_INTERVAL" usage:"Cadence of the retention/cleanup periodic job"`
}

// QueuesConfig sizes the River worker pools. There is deliberately no `review`
// entry: that pool is sized by review.max_parallel, so review concurrency has
// one knob rather than two that can drift apart.
type QueuesConfig struct {
	Default int `yaml:"default" env:"JOBS_QUEUES_DEFAULT" validate:"min=1" usage:"Worker pool size for the default queue (scan, digest, cleanup)"`
	Publish int `yaml:"publish" env:"JOBS_QUEUES_PUBLISH" validate:"min=1" usage:"Worker pool size for publishing findings (1 = serial, predictable for rate limits)"`
	Slack   int `yaml:"slack" env:"JOBS_QUEUES_SLACK" validate:"min=1" usage:"Worker pool size for Slack delivery"`
}

// GitLabConfig holds the service account's GitLab connection.
type GitLabConfig struct {
	BaseURL string `yaml:"base_url" env:"GITLAB_BASE_URL" validate:"required,url" usage:"GitLab base URL, e.g. https://gitlab.example.com"`
	// Required, but checked in Validate() — see the secret-validation note there.
	Token   Secret        `yaml:"token" env:"GITLAB_TOKEN" secret:"true" vault:"true" usage:"Service-account PAT (scope: api)"`
	Timeout time.Duration `yaml:"timeout" env:"GITLAB_TIMEOUT" usage:"Per-request timeout"`
	// GraphQLEnabled turns on the one-call-per-project reviewer review-state
	// query. When it is off (or unsupported by the instance) the service falls
	// back to the REST heuristic.
	GraphQLEnabled     bool          `yaml:"graphql_enabled" env:"GITLAB_GRAPHQL_ENABLED" usage:"Use GraphQL for exact reviewer review states"`
	MaxAttempts        int           `yaml:"max_attempts" env:"GITLAB_MAX_ATTEMPTS" validate:"min=1" usage:"Retry budget per request (429/5xx/transport)"`
	MaxRetryAfter      time.Duration `yaml:"max_retry_after" env:"GITLAB_MAX_RETRY_AFTER" usage:"Upper bound on an honoured Retry-After header"`
	InsecureSkipVerify bool          `yaml:"insecure_skip_verify" env:"GITLAB_INSECURE_SKIP_VERIFY" usage:"Skip TLS verification (self-managed only, explicit opt-in)"`
	CACertPath         string        `yaml:"ca_cert_path" env:"GITLAB_CA_CERT_PATH" usage:"Optional custom CA bundle path"`
}

// SlackConfig holds the digest delivery settings.
type SlackConfig struct {
	Token        Secret        `yaml:"token" env:"SLACK_TOKEN" secret:"true" vault:"true" usage:"Slack bot token (users:read, users:read.email, chat:write)"`
	DirectoryTTL time.Duration `yaml:"directory_ttl" env:"SLACK_DIRECTORY_TTL" usage:"In-process TTL of the users.list directory cache"`
	// UserMap is an optional gitlab_username → Slack user ID override. It never
	// touches the directory, so it keeps working while users.list is down.
	UserMap map[string]string `yaml:"user_map" env:"SLACK_USER_MAP" usage:"Override: gitlab_username -> Slack user ID"`
}

// LLMConfig selects and configures the LLM provider. claude-cli is the only
// implementation; the field exists so a second one can be added without a
// config break.
type LLMConfig struct {
	Provider string        `yaml:"provider" env:"LLM_PROVIDER" validate:"required,oneof=claude-cli" usage:"LLM provider: claude-cli"`
	Timeout  time.Duration `yaml:"timeout" env:"LLM_TIMEOUT" usage:"Overall timeout for one LLM call"`
	Claude   ClaudeConfig  `yaml:"claude"`
}

// ClaudeConfig configures the Claude CLI subprocess provider. Every flag is
// config-driven so the wrapper survives CLI version drift.
type ClaudeConfig struct {
	Bin            string           `yaml:"bin" env:"LLM_CLAUDE_BIN" validate:"required" usage:"Path to the claude binary"`
	Model          string           `yaml:"model" env:"LLM_CLAUDE_MODEL" usage:"Model alias or full id (sonnet, opus, claude-sonnet-5, ...)"`
	Auth           ClaudeAuthConfig `yaml:"auth"`
	PermissionMode string           `yaml:"permission_mode" env:"LLM_CLAUDE_PERMISSION_MODE" validate:"required" usage:"claude --permission-mode value"`
	AgentMode      bool             `yaml:"agent_mode" env:"LLM_CLAUDE_AGENT_MODE" usage:"Allow read-only repo inspection during review"`
	AllowedTools   []string         `yaml:"allowed_tools" env:"LLM_CLAUDE_ALLOWED_TOOLS" usage:"Tool permission rules granted in agent mode"`
	ExtraArgs      []string         `yaml:"extra_args" env:"LLM_CLAUDE_EXTRA_ARGS" usage:"Extra raw CLI args appended to every invocation"`
	// ExtraEnv is the operator's explicit opt-in for provider variables the auth
	// layer otherwise strips (CLAUDE_CODE_USE_BEDROCK and friends).
	ExtraEnv map[string]string `yaml:"extra_env" env:"LLM_CLAUDE_EXTRA_ENV" usage:"Extra environment variables passed to the claude subprocess"`
	// PassthroughEnv extends the subprocess environment allowlist with variables
	// that must keep the value they have in the service's own environment. The
	// default allowlist covers what a headless claude needs; this is the seam for
	// a deployment that needs more (an entry may end in '*').
	PassthroughEnv []string `yaml:"passthrough_env" env:"LLM_CLAUDE_PASSTHROUGH_ENV" usage:"Extra parent-environment variables the claude subprocess may inherit (trailing '*' allowed)"`
}

// ClaudeAuthConfig selects how the claude subprocess authenticates. The env tags
// are deliberately the bare variable names claude itself understands: prefixed,
// they read AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN / AI_REVIEWER_ANTHROPIC_API_KEY,
// which is what the *service* reads. The unprefixed variables are never part of
// the service's own environment — ClaudeAuth sets them only for the subprocess.
type ClaudeAuthConfig struct {
	Mode       llm.AuthMode `yaml:"mode" env:"LLM_CLAUDE_AUTH_MODE" validate:"required" usage:"existing-login | oauth-token | api-key"`
	OAuthToken Secret       `yaml:"oauth_token" env:"CLAUDE_CODE_OAUTH_TOKEN" secret:"true" vault:"true" usage:"Claude Code OAuth token minted by 'claude setup-token' (mode: oauth-token)"`
	APIKey     Secret       `yaml:"api_key" env:"ANTHROPIC_API_KEY" secret:"true" vault:"true" usage:"Anthropic API key (mode: api-key)"`
}

// ReviewConfig controls scanning cadence and review behaviour.
type ReviewConfig struct {
	ScanInterval time.Duration `yaml:"scan_interval" env:"REVIEW_SCAN_INTERVAL" usage:"How often every configured repository is scanned"`
	// MaxParallel is the single concurrency knob for reviews: it sizes the River
	// `review` queue pool.
	MaxParallel              int      `yaml:"max_parallel" env:"REVIEW_MAX_PARALLEL" validate:"min=1" usage:"Concurrent MR reviews across the service"`
	MaxComments              int      `yaml:"max_comments" env:"REVIEW_MAX_COMMENTS" validate:"min=1" usage:"Max findings published per review"`
	SeverityThreshold        string   `yaml:"severity_threshold" env:"REVIEW_SEVERITY_THRESHOLD" validate:"required,oneof=blocking high medium low nit" usage:"Drop findings below this severity"`
	PreferredCommentLanguage string   `yaml:"preferred_comment_language" env:"REVIEW_PREFERRED_COMMENT_LANGUAGE" validate:"required,oneof=en ru auto" usage:"Comment language: en | ru | auto"`
	WorkDir                  string   `yaml:"workdir" env:"REVIEW_WORKDIR" validate:"required" usage:"Ephemeral directory for mirrors and worktrees"`
	IgnoreGlobs              []string `yaml:"ignore_globs" env:"REVIEW_IGNORE_GLOBS" usage:"Globs excluded from context and from the LLM"`

	Pipeline PipelineConfig `yaml:"pipeline"`
	Context  ContextConfig  `yaml:"context"`
	Risk     RiskConfig     `yaml:"risk"`
	Coverage CoverageConfig `yaml:"coverage"`
}

// PipelineConfig controls the multi-pass review pipeline.
type PipelineConfig struct {
	Mode              string   `yaml:"mode" env:"REVIEW_PIPELINE_MODE" validate:"required,oneof=cheap standard deep custom" usage:"cheap | standard | deep | custom"`
	Passes            []string `yaml:"passes" env:"REVIEW_PIPELINE_PASSES" usage:"custom mode: pass names (general, correctness, concurrency, security, contracts)"`
	MaxParallel       int      `yaml:"max_parallel" env:"REVIEW_PIPELINE_MAX_PARALLEL" validate:"min=1" usage:"Concurrent LLM passes within one review"`
	VerifyMode        string   `yaml:"verify_mode" env:"REVIEW_PIPELINE_VERIFY_MODE" validate:"required,oneof=skeptic reflect off" usage:"skeptic | reflect | off"`
	VerifyMaxFindings int      `yaml:"verify_max_findings" env:"REVIEW_PIPELINE_VERIFY_MAX_FINDINGS" usage:"Max findings handed to the verification pass"`
	// Verifiers listed here must never execute repository code by default;
	// tsc and go_test do and stay an explicit opt-in.
	Verifiers    []string `yaml:"verifiers" env:"REVIEW_PIPELINE_VERIFIERS" usage:"Deterministic checks: go_build, go_vet, py_syntax, tsc, go_test"`
	Completeness string   `yaml:"completeness" env:"REVIEW_PIPELINE_COMPLETENESS" validate:"required,oneof=on off auto" usage:"Acceptance-criteria audit: on | off | auto"`
}

// ContextConfig bounds the enrichment context added to review prompts beyond
// the diffs themselves.
type ContextConfig struct {
	IncludeFullFiles   bool `yaml:"include_full_files" env:"REVIEW_CONTEXT_INCLUDE_FULL_FILES" usage:"Include changed files' content (full or windowed)"`
	MaxFileLines       int  `yaml:"max_file_lines" env:"REVIEW_CONTEXT_MAX_FILE_LINES" usage:"Files longer than this fall back to windows around hunks"`
	HunkWindowLines    int  `yaml:"hunk_window_lines" env:"REVIEW_CONTEXT_HUNK_WINDOW_LINES" usage:"Context lines around each hunk when windowing"`
	MaxTotalKB         int  `yaml:"max_total_kb" env:"REVIEW_CONTEXT_MAX_TOTAL_KB" usage:"Total budget (KB) for all enrichment sections"`
	IncludeCommits     bool `yaml:"include_commits" env:"REVIEW_CONTEXT_INCLUDE_COMMITS" usage:"Include the MR's commit messages"`
	IncludeDiscussions bool `yaml:"include_discussions" env:"REVIEW_CONTEXT_INCLUDE_DISCUSSIONS" usage:"Include existing discussion content"`
	MaxDiscussionKB    int  `yaml:"max_discussion_kb" env:"REVIEW_CONTEXT_MAX_DISCUSSION_KB" usage:"Budget (KB) for the discussions section"`
	PriorReview        bool `yaml:"prior_review" env:"REVIEW_CONTEXT_PRIOR_REVIEW" usage:"On re-review, include the previous review and the interdiff"`
	InterdiffMaxKB     int  `yaml:"interdiff_max_kb" env:"REVIEW_CONTEXT_INTERDIFF_MAX_KB" usage:"Budget (KB) for the interdiff section"`
}

// RiskConfig controls the deterministic risk score.
type RiskConfig struct {
	Enabled        bool     `yaml:"enabled" env:"REVIEW_RISK_ENABLED" usage:"Compute the deterministic risk score"`
	HistoryCommits int      `yaml:"history_commits" env:"REVIEW_RISK_HISTORY_COMMITS" validate:"min=0" usage:"Mirror commits scanned for churn / bug-fix factors"`
	SensitiveGlobs []string `yaml:"sensitive_globs" env:"REVIEW_RISK_SENSITIVE_GLOBS" usage:"Paths whose changes raise the risk score"`
}

// CoverageConfig controls changed-line test coverage measurement. Running it
// executes the reviewed repository's test code on a shared host, so it is off by
// default and an explicit opt-in.
type CoverageConfig struct {
	Enabled   bool          `yaml:"enabled" env:"REVIEW_COVERAGE_ENABLED" usage:"Run repo tests to measure changed-line coverage (executes repository code)"`
	Providers []string      `yaml:"providers" env:"REVIEW_COVERAGE_PROVIDERS" usage:"Coverage providers: go, node"`
	Timeout   time.Duration `yaml:"timeout" env:"REVIEW_COVERAGE_TIMEOUT" usage:"Per-provider test run timeout"`
	Node      NodeCoverage  `yaml:"node"`
}

// NodeCoverage holds node-specific coverage settings.
type NodeCoverage struct {
	Install bool `yaml:"install" env:"REVIEW_COVERAGE_NODE_INSTALL" usage:"Allow dependency install when node_modules is missing (runs lifecycle scripts)"`
}

// TeamConfig binds a set of repositories to a Slack channel. Leaf fields carry
// no `env:` tag on purpose: inside a slice element xconfig builds the name from
// the expanded path (AI_REVIEWER_TEAMS_0_NAME), and an explicit leaf tag would
// drop the index and collide across elements.
type TeamConfig struct {
	Name         string             `yaml:"name" validate:"required" usage:"Team name (unique, case-insensitive)"`
	SlackChannel string             `yaml:"slack_channel" validate:"required" usage:"Slack channel id the digest is posted to"`
	AIReview     TeamAIReviewConfig `yaml:"ai_review"`
	Repositories []string           `yaml:"repositories" validate:"required,min=1" usage:"GitLab project paths or numeric ids"`
}

// TeamAIReviewConfig toggles automated review for one team; the digest is
// unaffected by it.
type TeamAIReviewConfig struct {
	Enabled bool `yaml:"enabled" usage:"Run AI review for this team's repositories"`
}

// DefaultConfig returns a fully-populated Config. It is the single source of
// truth for defaults (see the package doc for why struct-tag defaults are not
// used); the file, env and Vault layers only override fields they actually
// contain.
func DefaultConfig() *Config {
	return &Config{
		Log: logger.Config{
			Format: logger.LoggerFormatJSON,
			Level:  logger.LogLevelInfo,
			Trace:  logger.LogLevelFatal,
		},
		// Mirrors mx's own `default:` tags, which WithSkipDefaults() suppresses.
		// The paths/ports are `validate:"required"` upstream, so leaving them
		// zero would fail validation rather than degrade quietly.
		Ops: ops.Config{
			Enabled: true,
			Network: "tcp",
			Metrics: ops.MetricsConfig{
				Enabled:   true,
				Path:      "/metrics",
				Port:      "10000",
				BasicAuth: http_transport.BasicAuthConfig{},
			},
			Healthy: ops.HealthCheckerConfig{
				Enabled:       true,
				Path:          "/healthy",
				Port:          "10000",
				LivenessPath:  "/livez",
				ReadinessPath: "/readyz",
			},
			Profiler: ops.ProfilerConfig{
				Path:         "/debug/pprof",
				Port:         "10000",
				WriteTimeout: 60,
			},
		},
		Service: ServiceConfig{
			SlackSendEnabled:       false, // dry-run until an operator opts in
			AIReviewPublishEnabled: false,
		},
		Postgres: PostgresConfig{
			Host:           "localhost",
			Port:           "5432",
			Database:       "ai_reviewer",
			SSLMode:        "require",
			MigrateOnStart: true, // serialised across replicas by App.Migrate's advisory lock
		},
		Jobs: JobsConfig{
			DrainTimeout:    60 * time.Second,
			Queues:          QueuesConfig{Default: 2, Publish: 1, Slack: 1},
			CleanupInterval: time.Hour,
		},
		GitLab: GitLabConfig{
			Timeout:        30 * time.Second,
			GraphQLEnabled: true,
			MaxAttempts:    4,
			MaxRetryAfter:  60 * time.Second,
		},
		Slack: SlackConfig{
			DirectoryTTL: 15 * time.Minute,
		},
		LLM: LLMConfig{
			Provider: "claude-cli",
			Timeout:  15 * time.Minute,
			Claude: ClaudeConfig{
				Bin:   "claude",
				Model: "sonnet",
				Auth: ClaudeAuthConfig{
					Mode: llm.AuthExistingLogin,
				},
				PermissionMode: "dontAsk",
				AgentMode:      true,
				// Read-only, and scoped to the worktree: llm.WorktreePlaceholder is
				// substituted per review with the checkout claude runs in.
				//
				// There are deliberately no Bash(git …) rules. An allow rule is a
				// prefix match, and `git diff` accepts both `--output=<path>` (an
				// arbitrary-file write, which manufactures a "clean build" verdict for
				// the deterministic verifiers and violates plan §1) and `--no-index
				// <any file>` (an arbitrary-file read that walks straight around the
				// path scope on the Read rule). `git show` and `git log` take the same
				// diff options. Reproduced against claude 2.1.222 with the shipped
				// flags: the write succeeded with permission_denials: []. History and
				// interdiff already reach the model through the prompt, built by Go
				// from the mirror, so the rules bought context we were paying for
				// twice.
				AllowedTools: []string{
					"Read(" + llm.WorktreePlaceholder + "/**)",
					"Grep(" + llm.WorktreePlaceholder + "/**)",
					"Glob(" + llm.WorktreePlaceholder + "/**)",
				},
			},
		},
		Review: ReviewConfig{
			ScanInterval:             5 * time.Minute,
			MaxParallel:              2,
			MaxComments:              12,
			SeverityThreshold:        "medium",
			PreferredCommentLanguage: "auto",
			WorkDir:                  "/work",
			IgnoreGlobs: []string{
				"vendor/**", "node_modules/**", "dist/**", "build/**",
				"*.generated.*", "*.pb.go", "*.min.js",
			},
			Pipeline: PipelineConfig{
				Mode:              "standard",
				MaxParallel:       2,
				VerifyMode:        "skeptic",
				VerifyMaxFindings: 24,
				Verifiers:         []string{"go_build", "go_vet", "py_syntax"},
				Completeness:      "auto",
			},
			Context: ContextConfig{
				IncludeFullFiles:   true,
				MaxFileLines:       500,
				HunkWindowLines:    60,
				MaxTotalKB:         256,
				IncludeCommits:     true,
				IncludeDiscussions: true,
				MaxDiscussionKB:    4,
				PriorReview:        true,
				InterdiffMaxKB:     32,
			},
			Risk: RiskConfig{
				Enabled:        true,
				HistoryCommits: 500,
				SensitiveGlobs: []string{
					"**/auth/**", "**/crypto/**", "**/security/**",
					"**/migrations/**", "**/*.sql",
					".gitlab-ci.yml", ".github/**", "Dockerfile*",
					"go.mod", "go.sum", "package.json", "requirements*.txt", "pyproject.toml",
					// Explicit lockfile names — a "*lock*" glob would also match
					// ordinary files like block.go or clock.ts.
					"package-lock.json", "yarn.lock", "pnpm-lock.yaml",
					"Cargo.lock", "Gemfile.lock", "poetry.lock", "composer.lock",
				},
			},
			Coverage: CoverageConfig{
				Enabled:   false, // executes repository test code — explicit opt-in
				Providers: []string{"go", "node"},
				Timeout:   5 * time.Minute,
			},
		},
	}
}
