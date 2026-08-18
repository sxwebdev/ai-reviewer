// Package config defines the ai-reviewer configuration schema and loading.
//
// Sources, in increasing priority: YAML file(s) → environment (AI_REVIEWER_*) →
// Vault. See load.go.
//
// # Which tag does what
//
// `yaml:` is the one tag every field must carry. It names the key in the file,
// and xconfig's defaults plugin resolves it a second time to decide whether a
// field was *explicitly present* in the file: a field that was is never
// overwritten by its default, which is what keeps an explicit `false` in the
// YAML from being flipped back to a `true` default (plugins/defaults/rescan.go
// in xconfig). Drop the yaml tag and that protection goes with it.
//
// `default:` carries the value. Sources are applied in order file → defaults →
// env → Vault, so env and Vault still win over a default. The embedded
// third-party configs (logger.Config, ops.Config) bring their own `default:`
// tags and are deliberately not re-listed here — including the ops switches,
// which mx defaults to off: what the ops server exposes is the deployment's
// call, not this package's.
//
// `env:` is only for fields whose *derived* name is not the name we want.
// xconfig builds it by word-splitting the Go field path, which is right for
// almost everything (Review.SeverityThreshold → AI_REVIEWER_REVIEW_SEVERITY_-
// THRESHOLD) and wrong for acronyms: "GitLab" splits into Git+Lab, so every
// GitLabConfig field needs a tag, as do Review.WorkDir (REVIEW_WORK_DIR) and the
// two auth secrets whose names are deliberately Anthropic's own. Adding a tag
// that merely restates the derived name is noise. Note these names double as
// Vault keys — xconfigvault reads Meta["env"] — so changing one moves the secret.
package config

import (
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/sxwebdev/xconfig"
	"github.com/tkcrm/mx/launcher/ops"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
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
	Linear   LinearConfig   `yaml:"linear"`
	Slack    SlackConfig    `yaml:"slack"`
	LLM      LLMConfig      `yaml:"llm"`
	Review   ReviewConfig   `yaml:"review"`
	Teams    []TeamConfig   `yaml:"teams" validate:"dive"`
}

// ServiceConfig holds the two global dry-run switches. Both default to false so
// a fresh deployment observes and reports without writing anything to GitLab or
// Slack until an operator opts in.
type ServiceConfig struct {
	SlackSendEnabled       bool `yaml:"slack_send_enabled" usage:"Actually deliver digests to Slack (off = dry-run, messages are only persisted)"`
	AIReviewPublishEnabled bool `yaml:"ai_review_publish_enabled" usage:"Actually publish review findings to GitLab (off = dry-run)"`
}

// PostgresConfig is the operational store. Username/password are Vault-backed:
// in production they never appear in YAML or env.
type PostgresConfig struct {
	Host     string `yaml:"host" default:"localhost" validate:"required" usage:"PostgreSQL host"`
	Port     string `yaml:"port" default:"5432" validate:"required" usage:"PostgreSQL port"`
	Database string `yaml:"database" default:"ai_reviewer" validate:"required" usage:"PostgreSQL database name"`
	// Required, but checked in Validate() rather than by a `validate:"required"`
	// tag — see the secret-validation note there.
	Username Secret `yaml:"username" secret:"true" vault:"true" usage:"PostgreSQL user"`
	// Password is not `required`: local development and IAM/peer authentication
	// legitimately run without one. An empty password simply produces a DSN
	// without one.
	Password Secret `yaml:"password" secret:"true" vault:"true" usage:"PostgreSQL password"`
	SSLMode  string `yaml:"ssl_mode" default:"require" validate:"required,oneof=disable allow prefer require verify-ca verify-full" usage:"PostgreSQL sslmode"`
	// MigrateOnStart applies pending migrations at startup — both the
	// application's schema and River's own, in that order.
	//
	// On by default, and safe with any number of replicas: App.Migrate holds one
	// session advisory lock across BOTH migrators, so replicas starting together
	// serialise and every one after the first finds nothing pending. Turn it off
	// only if your deployment applies the schema somewhere else (an init
	// container, a release job) and you want a replica that refuses to start
	// rather than one that migrates.
	MigrateOnStart bool `yaml:"migrate_on_start" default:"true" usage:"Apply pending migrations (application + River) on start"`
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
	DrainTimeout    time.Duration `yaml:"drain_timeout" default:"60s" usage:"Graceful drain window for in-flight jobs on shutdown"`
	Queues          QueuesConfig  `yaml:"queues"`
	CleanupInterval time.Duration `yaml:"cleanup_interval" default:"1h" usage:"Cadence of the retention/cleanup periodic job"`
}

// QueuesConfig sizes the River worker pools. There is deliberately no `review`
// entry: that pool is sized by review.max_parallel, so review concurrency has
// one knob rather than two that can drift apart.
type QueuesConfig struct {
	Default int `yaml:"default" default:"2" validate:"min=1" usage:"Worker pool size for the default queue (scan, digest, cleanup)"`
	Publish int `yaml:"publish" default:"1" validate:"min=1" usage:"Worker pool size for publishing findings (1 = serial, predictable for rate limits)"`
	Slack   int `yaml:"slack" default:"1" validate:"min=1" usage:"Worker pool size for Slack delivery"`
}

// GitLabConfig holds the service account's GitLab connection.
//
// This is the one struct where every field keeps an explicit `env:` tag: the Go
// field name "GitLab" word-splits into Git+Lab, so the derived names would all
// read AI_REVIEWER_GIT_LAB_*. Tagging the parent field instead does not help —
// xconfig then snake-cases the leaf character by character and BaseURL becomes
// BASE_U_R_L.
type GitLabConfig struct {
	BaseURL string `yaml:"base_url" env:"GITLAB_BASE_URL" validate:"required,url" usage:"GitLab base URL, e.g. https://gitlab.example.com"`
	// Required, but checked in Validate() — see the secret-validation note there.
	Token   Secret        `yaml:"token" env:"GITLAB_TOKEN" secret:"true" vault:"true" usage:"Service-account PAT (scope: api)"`
	Timeout time.Duration `yaml:"timeout" env:"GITLAB_TIMEOUT" default:"30s" usage:"Per-request timeout"`
	// GraphQLEnabled turns on the one-call-per-project reviewer review-state
	// query. When it is off (or unsupported by the instance) the service falls
	// back to the REST heuristic.
	GraphQLEnabled     bool          `yaml:"graphql_enabled" env:"GITLAB_GRAPHQL_ENABLED" default:"true" usage:"Use GraphQL for exact reviewer review states"`
	MaxAttempts        int           `yaml:"max_attempts" env:"GITLAB_MAX_ATTEMPTS" default:"4" validate:"min=1" usage:"Retry budget per request (429/5xx/transport)"`
	MaxRetryAfter      time.Duration `yaml:"max_retry_after" env:"GITLAB_MAX_RETRY_AFTER" default:"60s" usage:"Upper bound on an honoured Retry-After header"`
	InsecureSkipVerify bool          `yaml:"insecure_skip_verify" env:"GITLAB_INSECURE_SKIP_VERIFY" usage:"Skip TLS verification (self-managed only, explicit opt-in)"`
	CACertPath         string        `yaml:"ca_cert_path" env:"GITLAB_CA_CERT_PATH" usage:"Optional custom CA bundle path"`
}

// LinearConfig holds the read-only Linear GraphQL connection used by digests.
// The feature is enabled per application team by teams[].linear_team_ids; an
// unused Linear block therefore needs no credential.
type LinearConfig struct {
	Endpoint      string        `yaml:"endpoint" default:"https://api.linear.app/graphql" validate:"required,url" usage:"Linear GraphQL endpoint"`
	APIKey        Secret        `yaml:"api_key" env:"LINEAR_API_KEY" secret:"true" vault:"true" usage:"Linear personal API key used for read-only digest queries"`
	Timeout       time.Duration `yaml:"timeout" default:"15s" usage:"Per-request timeout"`
	MaxAttempts   int           `yaml:"max_attempts" default:"4" validate:"min=1" usage:"Retry budget per request (rate limit/5xx/transport)"`
	MaxRetryAfter time.Duration `yaml:"max_retry_after" default:"60s" usage:"Upper bound on an honoured Retry-After header"`
}

// SlackConfig holds the digest delivery settings.
type SlackConfig struct {
	Token        Secret        `yaml:"token" secret:"true" vault:"true" usage:"Slack bot token (users:read, users:read.email, chat:write)"`
	DirectoryTTL time.Duration `yaml:"directory_ttl" default:"15m" usage:"In-process TTL of the users.list directory cache"`
	// UserMap is an optional gitlab_username → Slack identity override, in any of
	// three forms: a user id (U…/W…), an @handle or bare handle, or an email.
	// match.ParseOverride owns that grammar — doctor and the matcher both drive it,
	// so the map cannot mean one thing when it is checked and another when it is
	// used.
	//
	// Only the id form answers offline; it is therefore the form to reach for when
	// users.list is unavailable, and the only one that keeps working then. A handle
	// or an email is resolved against the directory like any other probe, and an
	// override that resolves to nothing is a configuration error rather than a
	// fallback to the name-matching ladder — `doctor` names the entry.
	UserMap map[string]string `yaml:"user_map" env:"SLACK_USER_MAP" usage:"Override: gitlab_username -> Slack id, @handle or email"`
}

// LLMConfig selects and configures the LLM provider. claude-cli is the only
// implementation; the field exists so a second one can be added without a
// config break.
type LLMConfig struct {
	Provider string        `yaml:"provider" default:"claude-cli" validate:"required,oneof=claude-cli" usage:"LLM provider: claude-cli"`
	Timeout  time.Duration `yaml:"timeout" default:"15m" usage:"Overall timeout for one LLM call"`
	Claude   ClaudeConfig  `yaml:"claude"`
}

// ClaudeConfig configures the Claude CLI subprocess provider. Every flag is
// config-driven so the wrapper survives CLI version drift.
type ClaudeConfig struct {
	Bin            string           `yaml:"bin" default:"claude" validate:"required" usage:"Path to the claude binary"`
	Model          string           `yaml:"model" default:"sonnet" usage:"Model alias or full id (sonnet, opus, claude-sonnet-5, ...)"`
	Auth           ClaudeAuthConfig `yaml:"auth"`
	PermissionMode string           `yaml:"permission_mode" default:"dontAsk" validate:"required" usage:"claude --permission-mode value"`
	AgentMode      bool             `yaml:"agent_mode" default:"true" usage:"Allow read-only repo inspection during review"`
	// The default is read-only and scoped to the worktree: llm.WorktreePlaceholder
	// ("${worktree}") is substituted per review with the checkout claude runs in.
	// It is spelled out here because a struct tag cannot reference the constant;
	// TestDefaultAllowedToolsAreReadOnlyAndWorktreeScoped compares the two.
	//
	// There are deliberately no Bash(git …) rules. An allow rule is a prefix
	// match, and `git diff` accepts both `--output=<path>` (an arbitrary-file
	// write, which manufactures a "clean build" verdict for the deterministic
	// verifiers and violates plan §1) and `--no-index <any file>` (an
	// arbitrary-file read that walks straight around the path scope on the Read
	// rule). `git show` and `git log` take the same diff options. Reproduced
	// against claude 2.1.222 with the shipped flags: the write succeeded with
	// permission_denials: []. History and interdiff already reach the model
	// through the prompt, built by Go from the mirror, so the rules bought context
	// we were paying for twice.
	AllowedTools []string `yaml:"allowed_tools" default:"Read(${worktree}/**),Grep(${worktree}/**),Glob(${worktree}/**)" usage:"Tool permission rules granted in agent mode"`
	ExtraArgs    []string `yaml:"extra_args" usage:"Extra raw CLI args appended to every invocation"`
	// ExtraEnv is the operator's explicit opt-in for provider variables the auth
	// layer otherwise strips (CLAUDE_CODE_USE_BEDROCK and friends).
	ExtraEnv map[string]string `yaml:"extra_env" usage:"Extra environment variables passed to the claude subprocess"`
	// PassthroughEnv extends the subprocess environment allowlist with variables
	// that must keep the value they have in the service's own environment. The
	// default allowlist covers what a headless claude needs; this is the seam for
	// a deployment that needs more (an entry may end in '*').
	PassthroughEnv []string `yaml:"passthrough_env" usage:"Extra parent-environment variables the claude subprocess may inherit (trailing '*' allowed)"`
}

// ClaudeAuthConfig selects how the claude subprocess authenticates. The env tags
// are deliberately the bare variable names claude itself understands: prefixed,
// they read AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN / AI_REVIEWER_ANTHROPIC_API_KEY,
// which is what the *service* reads. The unprefixed variables are never part of
// the service's own environment — ClaudeAuth sets them only for the subprocess.
type ClaudeAuthConfig struct {
	Mode       llm.AuthMode `yaml:"mode" default:"existing-login" validate:"required" usage:"existing-login | oauth-token | api-key"`
	OAuthToken Secret       `yaml:"oauth_token" env:"CLAUDE_CODE_OAUTH_TOKEN" secret:"true" vault:"true" usage:"Claude Code OAuth token minted by 'claude setup-token' (mode: oauth-token)"`
	APIKey     Secret       `yaml:"api_key" env:"ANTHROPIC_API_KEY" secret:"true" vault:"true" usage:"Anthropic API key (mode: api-key)"`
}

// ReviewConfig controls scanning cadence and review behaviour.
type ReviewConfig struct {
	ScanInterval time.Duration `yaml:"scan_interval" default:"5m" usage:"How often every configured repository is scanned"`
	// MaxParallel is the single concurrency knob for reviews: it sizes the River
	// `review` queue pool.
	MaxParallel              int    `yaml:"max_parallel" default:"2" validate:"min=1" usage:"Concurrent MR reviews across the service"`
	MaxComments              int    `yaml:"max_comments" default:"12" validate:"min=1" usage:"Max findings published per review"`
	SeverityThreshold        string `yaml:"severity_threshold" default:"medium" validate:"required,oneof=blocking high medium low nit" usage:"Drop findings below this severity"`
	PreferredCommentLanguage string `yaml:"preferred_comment_language" default:"auto" validate:"required,oneof=en ru auto" usage:"Comment language: en | ru | auto"`
	// The env tag stays: the derived name would be REVIEW_WORK_DIR.
	//
	// The default is relative on purpose, so a local `start` works with no config
	// at all. The image overrides it to the absolute /work it mounts a volume on
	// (Dockerfile), because inside the container a relative path would resolve
	// against WORKDIR and land in /work/data.
	WorkDir     string   `yaml:"workdir" env:"REVIEW_WORKDIR" default:"./data" validate:"required" usage:"Ephemeral directory for mirrors and worktrees"`
	IgnoreGlobs []string `yaml:"ignore_globs" default:"vendor/**,node_modules/**,dist/**,build/**,*.generated.*,*.pb.go,*.min.js" usage:"Globs excluded from context and from the LLM"`

	Pipeline PipelineConfig `yaml:"pipeline"`
	Context  ContextConfig  `yaml:"context"`
	Risk     RiskConfig     `yaml:"risk"`
	Coverage CoverageConfig `yaml:"coverage"`
}

// PipelineConfig controls the multi-pass review pipeline.
type PipelineConfig struct {
	Mode              string   `yaml:"mode" default:"standard" validate:"required,oneof=cheap standard deep custom" usage:"cheap | standard | deep | custom"`
	Passes            []string `yaml:"passes" usage:"custom mode: pass names (general, correctness, concurrency, security, contracts)"`
	MaxParallel       int      `yaml:"max_parallel" default:"2" validate:"min=1" usage:"Concurrent LLM passes within one review"`
	VerifyMode        string   `yaml:"verify_mode" default:"skeptic" validate:"required,oneof=skeptic reflect off" usage:"skeptic | reflect | off"`
	VerifyMaxFindings int      `yaml:"verify_max_findings" default:"24" usage:"Max findings handed to the verification pass"`
	// Verifiers listed here must never execute repository code by default;
	// tsc and go_test do and stay an explicit opt-in.
	Verifiers    []string `yaml:"verifiers" default:"go_build,go_vet,py_syntax" usage:"Deterministic checks: go_build, go_vet, py_syntax, tsc, go_test"`
	Completeness string   `yaml:"completeness" default:"auto" validate:"required,oneof=on off auto" usage:"Acceptance-criteria audit: on | off | auto"`
}

// ContextConfig bounds the enrichment context added to review prompts beyond
// the diffs themselves.
type ContextConfig struct {
	IncludeFullFiles   bool `yaml:"include_full_files" default:"true" usage:"Include changed files' content (full or windowed)"`
	MaxFileLines       int  `yaml:"max_file_lines" default:"500" usage:"Files longer than this fall back to windows around hunks"`
	HunkWindowLines    int  `yaml:"hunk_window_lines" default:"60" usage:"Context lines around each hunk when windowing"`
	MaxTotalKB         int  `yaml:"max_total_kb" default:"256" usage:"Total budget (KB) for all enrichment sections"`
	IncludeCommits     bool `yaml:"include_commits" default:"true" usage:"Include the MR's commit messages"`
	IncludeDiscussions bool `yaml:"include_discussions" default:"true" usage:"Include existing discussion content"`
	MaxDiscussionKB    int  `yaml:"max_discussion_kb" default:"4" usage:"Budget (KB) for the discussions section"`
	PriorReview        bool `yaml:"prior_review" default:"true" usage:"On re-review, include the previous review and the interdiff"`
	InterdiffMaxKB     int  `yaml:"interdiff_max_kb" default:"32" usage:"Budget (KB) for the interdiff section"`
}

// RiskConfig controls the deterministic risk score.
type RiskConfig struct {
	Enabled        bool `yaml:"enabled" default:"true" usage:"Compute the deterministic risk score"`
	HistoryCommits int  `yaml:"history_commits" default:"500" validate:"min=0" usage:"Mirror commits scanned for churn / bug-fix factors"`
	// The lockfiles are named explicitly: a "*lock*" glob would also match
	// ordinary files like block.go or clock.ts.
	SensitiveGlobs []string `yaml:"sensitive_globs" default:"**/auth/**,**/crypto/**,**/security/**,**/migrations/**,**/*.sql,.gitlab-ci.yml,.github/**,Dockerfile*,go.mod,go.sum,package.json,requirements*.txt,pyproject.toml,package-lock.json,yarn.lock,pnpm-lock.yaml,Cargo.lock,Gemfile.lock,poetry.lock,composer.lock" usage:"Paths whose changes raise the risk score"`
}

// CoverageConfig controls changed-line test coverage measurement. Running it
// executes the reviewed repository's test code on a shared host, so it is off by
// default and an explicit opt-in.
type CoverageConfig struct {
	Enabled   bool          `yaml:"enabled" usage:"Run repo tests to measure changed-line coverage (executes repository code)"`
	Providers []string      `yaml:"providers" default:"go,node" usage:"Coverage providers: go, node"`
	Timeout   time.Duration `yaml:"timeout" default:"5m" usage:"Per-provider test run timeout"`
	Node      NodeCoverage  `yaml:"node"`
}

// NodeCoverage holds node-specific coverage settings.
type NodeCoverage struct {
	Install bool `yaml:"install" usage:"Allow dependency install when node_modules is missing (runs lifecycle scripts)"`
}

// TeamConfig binds a set of repositories to a Slack channel. Leaf fields carry
// no `env:` tag on purpose: inside a slice element xconfig builds the name from
// the expanded path (AI_REVIEWER_TEAMS_0_NAME), and an explicit leaf tag would
// drop the index and collide across elements.
type TeamConfig struct {
	Name          string             `yaml:"name" validate:"required" usage:"Team name (unique, case-insensitive)"`
	SlackChannel  string             `yaml:"slack_channel" validate:"required" usage:"Slack channel id the digest is posted to"`
	AIReview      TeamAIReviewConfig `yaml:"ai_review"`
	LinearTeamIDs []string           `yaml:"linear_team_ids" usage:"Linear team UUIDs whose In Review issues are included in the digest"`
	Repositories  []string           `yaml:"repositories" validate:"required,min=1" usage:"GitLab project paths or numeric ids"`
}

// TeamAIReviewConfig toggles automated review for one team; the digest is
// unaffected by it.
type TeamAIReviewConfig struct {
	Enabled bool `yaml:"enabled" usage:"Run AI review for this team's repositories"`
}

// Default returns a Config with every default applied: this package's from the
// `default:` tags above, the embedded mx configs from their own. Nothing is
// hand-written here.
//
// Load takes it as the seed, and it is what the CLI hands `doctor` and
// `migrations` when there is no file to read. Applying the tag pass twice (once
// here, once inside Load) is harmless: defaults only ever fill a zero value.
//
// Note what this means for the ops server: mx defaults `ops.enabled`,
// `ops.metrics.enabled` and `ops.healthy.enabled` to false, so /livez, /readyz
// and /metrics exist only when the deployment asks for them. config.example.yaml
// turns them on and the compose file mounts it; a deployment configured purely
// through env sets AI_REVIEWER_OPS_* itself.
//
// The only way it can fail is a malformed `default:` tag in this file, which
// TestDefaultTagsAreWellFormed catches. It still returns an error rather than
// panicking, and it returns the config it did manage to build alongside it: a
// diagnostic like `doctor` has to keep running and report the problem, which it
// cannot do if the process is already gone.
func Default() (*Config, error) {
	c := &Config{}
	if _, err := xconfig.Load(c,
		xconfig.WithSkipFiles(),
		xconfig.WithSkipEnv(),
		xconfig.WithSkipFlags(),
	); err != nil {
		return c, fmt.Errorf("apply config default tags: %w", err)
	}
	return c, nil
}
