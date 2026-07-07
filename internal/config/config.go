// Package config defines the ai-reviewer configuration schema and loading.
//
// Configuration is loaded from a YAML file (default ~/.ai-reviewer/config.yaml)
// via the xconfig library, with environment-variable overrides (prefix
// AI_REVIEWER_). Defaults come from DefaultConfig() rather than xconfig's
// `default` struct tags: tag-based defaults reset any zero-valued field, which
// silently flips an explicit `false` back to a `true` default — so we start
// from a fully-populated struct and let the file/env only override present
// fields (see loader.go, WithSkipDefaults).
//
// Secrets (the GitLab token) are never stored in the file or the struct: they
// are resolved on demand from the environment variable named by the config, so
// they never leak into logs or a config dump.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the root configuration for ai-reviewer.
type Config struct {
	App     AppConfig     `yaml:"app"`
	GitLab  GitLabConfig  `yaml:"gitlab"`
	LLM     LLMConfig     `yaml:"llm"`
	Review  ReviewConfig  `yaml:"review"`
	Watch   WatchConfig   `yaml:"watch"`
	Index   IndexConfig   `yaml:"index"`
	Storage StorageConfig `yaml:"storage"`
}

// AppConfig holds process-wide/UI settings.
type AppConfig struct {
	DataDir     string `yaml:"data_dir" usage:"Base data directory"`
	BindHost    string `yaml:"bind_host" env:"BIND_HOST" usage:"Web UI bind host (localhost only by default)"`
	Port        int    `yaml:"port" env:"PORT" usage:"Web UI port (0 = random free port)"`
	OpsPort     int    `yaml:"ops_port" env:"OPS_PORT" usage:"mx ops (health/metrics) port (0 = disabled)"`
	OpenBrowser bool   `yaml:"open_browser" usage:"Open the browser on serve"`
	UI          string `yaml:"ui" usage:"Primary UI: web"`
}

// GitLabConfig holds GitLab connection settings. The token is read from the
// Token field, stored in config.yaml (which `init` writes with 0600 perms). If
// Token is empty it falls back to the environment variable named by TokenEnv
// (default GITLAB_TOKEN), so the older env-only workflow keeps working.
type GitLabConfig struct {
	Host               string        `yaml:"host" env:"GITLAB_HOST" usage:"GitLab base URL, e.g. https://gitlab.example.com"`
	Token              string        `yaml:"token" usage:"GitLab personal access token (scope: api), stored locally in config.yaml"`
	TokenEnv           string        `yaml:"token_env" usage:"Optional: read the token from this env var when 'token' is empty (default GITLAB_TOKEN)"`
	Username           string        `yaml:"username" env:"GITLAB_USERNAME" usage:"Your GitLab username (reviewer identity)"`
	Timeout            time.Duration `yaml:"timeout" usage:"Per-request timeout"`
	InsecureSkipVerify bool          `yaml:"insecure_skip_verify" usage:"Skip TLS verification (self-managed only, explicit opt-in)"`
	CACertPath         string        `yaml:"ca_cert_path" usage:"Optional custom CA bundle path"`
}

// LLMConfig selects and configures the LLM provider.
type LLMConfig struct {
	Provider string        `yaml:"provider" usage:"LLM provider: claude-cli | anthropic-api"`
	Model    string        `yaml:"model" usage:"Model name/alias"`
	Timeout  time.Duration `yaml:"timeout" usage:"Overall LLM call timeout"`
	Claude   ClaudeConfig  `yaml:"claude"`
}

// ClaudeConfig configures the Claude CLI subprocess provider.
type ClaudeConfig struct {
	Bin            string   `yaml:"bin" usage:"Path to the claude binary"`
	AuthMode       string   `yaml:"auth_mode" usage:"existing-login | oauth-token | api-key"`
	OAuthTokenEnv  string   `yaml:"oauth_token_env" usage:"Env var with Claude Code OAuth token"`
	APIKeyEnv      string   `yaml:"api_key_env" usage:"Env var with Anthropic API key"`
	PermissionMode string   `yaml:"permission_mode" usage:"claude --permission-mode value"`
	AgentMode      bool     `yaml:"agent_mode" usage:"Allow read-only repo inspection during review"`
	ReadOnly       bool     `yaml:"read_only" usage:"Deny all write/destructive tools"`
	AllowedTools   []string `yaml:"allowed_tools" usage:"Allowed tool permission rules"`
	ExtraArgs      []string `yaml:"extra_args" usage:"Extra raw CLI args appended to every invocation"`
}

// ReviewConfig controls review behaviour and safety defaults.
type ReviewConfig struct {
	DefaultMode              string   `yaml:"default_mode" usage:"full | changed-only"`
	MaxComments              int      `yaml:"max_comments" usage:"Max findings surfaced per review"`
	SeverityThreshold        string   `yaml:"severity_threshold" usage:"Drop findings below this severity"`
	CreateDrafts             bool     `yaml:"create_drafts" usage:"Auto-create GitLab draft notes (off by default)"`
	AutoReview               bool     `yaml:"auto_review" usage:"Watch-mode auto-runs review (local report only)"`
	AutoDraft                bool     `yaml:"auto_draft" usage:"Watch-mode may create drafts (explicit opt-in)"`
	AutoPublish              bool     `yaml:"auto_publish" usage:"DANGER: watch-mode may publish (hard-disabled default)"`
	FullRepoContext          bool     `yaml:"full_repo_context" usage:"Include relevant repo context beyond the diff"`
	AgentMode                bool     `yaml:"agent_mode" usage:"Enable agentic deep-analysis stage"`
	IncludeTests             bool     `yaml:"include_tests"`
	IncludeSecurity          bool     `yaml:"include_security"`
	IncludePerformance       bool     `yaml:"include_performance"`
	IncludeObservability     bool     `yaml:"include_observability"`
	IncludeStyle             bool     `yaml:"include_style"`
	PreferredCommentLanguage string   `yaml:"preferred_comment_language" usage:"ru | en | auto"`
	IgnoreGlobs              []string `yaml:"ignore_globs" usage:"Globs excluded from context/LLM"`

	Context  ContextConfig  `yaml:"context"`
	Pipeline PipelineConfig `yaml:"pipeline"`
	Risk     RiskConfig     `yaml:"risk"`
	Coverage CoverageConfig `yaml:"coverage"`
}

// PipelineConfig controls the multi-pass review pipeline.
type PipelineConfig struct {
	Mode              string   `yaml:"mode" usage:"cheap | standard | deep | custom"`
	Passes            []string `yaml:"passes" usage:"custom mode: pass names (general, correctness, concurrency, security, contracts)"`
	MaxParallel       int      `yaml:"max_parallel" usage:"Concurrent LLM review passes"`
	VerifyMode        string   `yaml:"verify_mode" usage:"skeptic | reflect | off"`
	VerifyMaxFindings int      `yaml:"verify_max_findings" usage:"Max findings sent to the skeptic pass"`
	Verifiers         []string `yaml:"verifiers" usage:"Deterministic checks: go_build, go_vet, py_syntax, tsc, go_test"`
	Completeness      string   `yaml:"completeness" usage:"on | off | auto (auto: on except cheap mode)"`
}

// RiskConfig controls the deterministic risk score.
type RiskConfig struct {
	Enabled        bool     `yaml:"enabled" usage:"Compute the deterministic risk score"`
	HistoryCommits int      `yaml:"history_commits" usage:"Mirror commits scanned for churn/bug-fix factors"`
	SensitiveGlobs []string `yaml:"sensitive_globs" usage:"Paths whose changes raise risk"`
}

// CoverageConfig controls changed-line test coverage measurement. Running it
// executes the repository's test code — explicit opt-in.
type CoverageConfig struct {
	Enabled   bool          `yaml:"enabled" usage:"Run repo tests to measure changed-line coverage (executes repository code)"`
	Providers []string      `yaml:"providers" usage:"Coverage providers: go, node"`
	Timeout   time.Duration `yaml:"timeout" usage:"Per-provider test run timeout"`
	Node      NodeCoverage  `yaml:"node"`
}

// NodeCoverage holds node-specific coverage settings.
type NodeCoverage struct {
	Install bool `yaml:"install" usage:"Allow dependency install (npm ci / pnpm / yarn) when node_modules is missing (runs lifecycle scripts)"`
}

// ContextConfig bounds the enrichment context added to review prompts beyond
// the diffs themselves.
type ContextConfig struct {
	IncludeFullFiles   bool `yaml:"include_full_files" usage:"Include changed files' content (full or windowed) in the prompt"`
	MaxFileLines       int  `yaml:"max_file_lines" usage:"Files longer than this fall back to windows around hunks"`
	HunkWindowLines    int  `yaml:"hunk_window_lines" usage:"Context lines around each hunk when windowing"`
	MaxTotalKB         int  `yaml:"max_total_kb" usage:"Total budget (KB) for all enrichment sections"`
	IncludeCommits     bool `yaml:"include_commits" usage:"Include the MR's commit messages in the prompt"`
	IncludeDiscussions bool `yaml:"include_discussions" usage:"Include existing discussion content in the prompt"`
	MaxDiscussionKB    int  `yaml:"max_discussion_kb" usage:"Budget (KB) for the discussions section"`
	PriorReview        bool `yaml:"prior_review" usage:"On re-review, include the previous review + interdiff"`
	InterdiffMaxKB     int  `yaml:"interdiff_max_kb" usage:"Budget (KB) for the interdiff section"`
	RelatedFiles       int  `yaml:"related_files" usage:"Max FTS-suggested related files listed as investigation leads (0 = off)"`
}

// WatchConfig controls the background daemon.
type WatchConfig struct {
	Enabled          bool          `yaml:"enabled"`
	Interval         time.Duration `yaml:"interval"`
	MaxParallel      int           `yaml:"max_parallel"`
	ReviewNewMRs     bool          `yaml:"review_new_mrs"`
	ReviewNewCommits bool          `yaml:"review_new_commits"`
}

// IndexConfig controls repository indexing features.
type IndexConfig struct {
	Enabled      bool `yaml:"enabled"`
	FTS          bool `yaml:"fts"`
	TreeSitter   bool `yaml:"tree_sitter"`
	LSP          bool `yaml:"lsp"`
	VectorSearch bool `yaml:"vector_search"`
}

// StorageConfig controls on-disk locations.
type StorageConfig struct {
	DBPath   string `yaml:"db_path" usage:"SQLite database path"`
	CacheDir string `yaml:"cache_dir" usage:"Git cache directory"`
}

// DefaultConfig returns a fully-populated Config with all defaults applied. It
// is the single source of truth for defaults; the loader overlays file and env
// values on top of it.
func DefaultConfig() *Config {
	return &Config{
		App: AppConfig{
			DataDir:     "~/.ai-reviewer",
			BindHost:    "127.0.0.1",
			Port:        0,
			OpsPort:     0,
			OpenBrowser: true,
			UI:          "web",
		},
		GitLab: GitLabConfig{
			TokenEnv: "GITLAB_TOKEN",
			Timeout:  30 * time.Second,
		},
		LLM: LLMConfig{
			Provider: "claude-cli",
			Model:    "sonnet",
			Timeout:  15 * time.Minute,
			Claude: ClaudeConfig{
				Bin:            "claude",
				AuthMode:       "existing-login",
				OAuthTokenEnv:  "CLAUDE_CODE_OAUTH_TOKEN",
				APIKeyEnv:      "ANTHROPIC_API_KEY",
				PermissionMode: "dontAsk",
				AgentMode:      true,
				ReadOnly:       true,
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git diff *)", "Bash(git log *)", "Bash(git show *)",
				},
			},
		},
		Review: ReviewConfig{
			DefaultMode:              "full",
			MaxComments:              12,
			SeverityThreshold:        "medium",
			CreateDrafts:             false,
			AutoReview:               true,
			AutoDraft:                false,
			AutoPublish:              false,
			FullRepoContext:          true,
			AgentMode:                true,
			IncludeTests:             true,
			IncludeSecurity:          true,
			IncludePerformance:       true,
			IncludeObservability:     true,
			IncludeStyle:             false,
			PreferredCommentLanguage: "auto",
			IgnoreGlobs: []string{
				"vendor/**", "node_modules/**", "dist/**", "build/**",
				"*.generated.*", "*.pb.go", "*.min.js",
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
				RelatedFiles:       5,
			},
			Pipeline: PipelineConfig{
				Mode:              "standard",
				MaxParallel:       2,
				VerifyMode:        "skeptic",
				VerifyMaxFindings: 24,
				Verifiers:         []string{"go_build", "go_vet", "py_syntax"},
				Completeness:      "auto",
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
		Watch: WatchConfig{
			Enabled:          true,
			Interval:         10 * time.Minute,
			MaxParallel:      2,
			ReviewNewMRs:     true,
			ReviewNewCommits: true,
		},
		Index: IndexConfig{
			Enabled: true,
			FTS:     true,
		},
		Storage: StorageConfig{
			DBPath:   "~/.ai-reviewer/state.db",
			CacheDir: "~/.ai-reviewer/cache",
		},
	}
}

// GitLabToken resolves the GitLab token. It prefers the token stored in the
// config file (GitLab.Token); if that is empty it falls back to the environment
// variable named by TokenEnv (default GITLAB_TOKEN).
func (c *Config) GitLabToken() string {
	if c.GitLab.Token != "" {
		return c.GitLab.Token
	}
	env := c.GitLab.TokenEnv
	if env == "" {
		env = "GITLAB_TOKEN"
	}
	return os.Getenv(env)
}

// GitLabConfigured reports whether the minimum GitLab settings are present: a
// host and a resolvable token. It is the single definition of "configured"
// shared by the web setup gate and the headless entrypoint guards.
func (c *Config) GitLabConfigured() bool {
	return c.GitLab.Host != "" && c.GitLabToken() != ""
}

// IsValidEnvName reports whether s is a syntactically valid POSIX environment
// variable name ([A-Za-z_][A-Za-z0-9_]*). It is used to detect when token_env
// was mistakenly set to a literal token (which contains '-'/'.') instead of an
// env var name.
func IsValidEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// ExpandPaths resolves ~ and makes storage paths absolute. Called after load.
func (c *Config) ExpandPaths() error {
	for _, p := range []*string{&c.App.DataDir, &c.Storage.DBPath, &c.Storage.CacheDir} {
		expanded, err := expandPath(*p)
		if err != nil {
			return err
		}
		*p = expanded
	}
	return nil
}

// Validate performs structural validation. It intentionally does NOT require
// gitlab.host/username so that `init` and `doctor` work on a fresh install;
// capability checks live in the doctor command and the services that need them.
func (c *Config) Validate() error {
	if c.App.Port < 0 || c.App.Port > 65535 {
		return fmt.Errorf("app.port out of range: %d", c.App.Port)
	}
	if c.App.BindHost == "" {
		return fmt.Errorf("app.bind_host must not be empty")
	}
	switch c.LLM.Provider {
	case "claude-cli", "anthropic-api":
	default:
		return fmt.Errorf("llm.provider must be claude-cli or anthropic-api, got %q", c.LLM.Provider)
	}
	switch c.Review.SeverityThreshold {
	case "blocking", "high", "medium", "low", "nit":
	default:
		return fmt.Errorf("review.severity_threshold invalid: %q", c.Review.SeverityThreshold)
	}
	switch c.Review.PreferredCommentLanguage {
	case "", "en", "ru", "auto":
	default:
		return fmt.Errorf("review.preferred_comment_language must be en, ru, or auto, got %q", c.Review.PreferredCommentLanguage)
	}
	if c.Review.Context.MaxFileLines < 0 || c.Review.Context.HunkWindowLines < 0 || c.Review.Context.MaxTotalKB < 0 {
		return fmt.Errorf("review.context sizes must not be negative")
	}
	switch c.Review.Pipeline.Mode {
	case "", "cheap", "standard", "deep", "custom":
	default:
		return fmt.Errorf("review.pipeline.mode must be cheap, standard, deep, or custom, got %q", c.Review.Pipeline.Mode)
	}
	switch c.Review.Pipeline.VerifyMode {
	case "", "skeptic", "reflect", "off":
	default:
		return fmt.Errorf("review.pipeline.verify_mode must be skeptic, reflect, or off, got %q", c.Review.Pipeline.VerifyMode)
	}
	switch c.Review.Pipeline.Completeness {
	case "", "on", "off", "auto":
	default:
		return fmt.Errorf("review.pipeline.completeness must be on, off, or auto, got %q", c.Review.Pipeline.Completeness)
	}
	if c.Review.Risk.HistoryCommits < 0 {
		return fmt.Errorf("review.risk.history_commits must not be negative")
	}
	if c.Review.Coverage.Timeout < 0 {
		return fmt.Errorf("review.coverage.timeout must not be negative")
	}
	if c.Storage.DBPath == "" {
		return fmt.Errorf("storage.db_path must not be empty")
	}
	return nil
}

// expandPath expands a leading ~ to the user's home directory and returns an
// absolute, cleaned path.
func expandPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		p = abs
	}
	return filepath.Clean(p), nil
}
