package config

import (
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// SettingKind is the input widget/type for a config field in the web UI. It
// also decides how the value is patched: KindText/KindPassword/KindSelect are
// written as quoted YAML strings; KindBool/KindInt/KindDuration/KindList are
// written raw (unquoted) — a quoted "true" or "[...]" would not unmarshal.
type SettingKind int

const (
	KindText SettingKind = iota
	KindPassword
	KindBool
	KindInt
	KindDuration
	KindSelect
	KindList
)

// String is the stable identifier used by templates to branch on widget type.
func (k SettingKind) String() string {
	switch k {
	case KindPassword:
		return "password"
	case KindBool:
		return "bool"
	case KindInt:
		return "int"
	case KindDuration:
		return "duration"
	case KindSelect:
		return "select"
	case KindList:
		return "list"
	default:
		return "text"
	}
}

// Raw reports whether the field is patched as a bare (unquoted) YAML scalar or
// sequence rather than a double-quoted string.
func (k SettingKind) Raw() bool {
	switch k {
	case KindBool, KindInt, KindDuration, KindList:
		return true
	default:
		return false
	}
}

// SettingField describes one editable config field: how to render it, validate
// it, patch it, and detect environment shadowing. The schema (SettingsSchema)
// is the single source of truth shared by the settings form, the apply/validate
// path, and env-shadow detection.
type SettingField struct {
	Key      string // dotted yaml path, e.g. "review.pipeline.mode"
	Section  string // display group, e.g. "GitLab", "Pipeline"
	Label    string
	Help     string // one-line description (from the struct usage tag where present)
	Kind     SettingKind
	Options  []string // choices for KindSelect
	Min, Max *int     // inclusive bounds for KindInt (nil = unbounded)
	Required bool     // KindText/KindPassword that must not be blank (blank rejected before write)
	Danger   bool     // safety-sensitive: publishing, executing repo code, disabling TLS/read-only
	Restart  bool     // takes effect only after a restart (listener/DB/worker pool fixed at boot)
	Secret   bool     // write only when non-empty; never echo the current value
	EnvName  string   // the AI_REVIEWER_* variable that overrides this key (for shadow warnings)

	// Get returns the current value as the string the form binds to: ints via
	// strconv, durations via Duration.String, bools as true/false, and lists as
	// newline-joined items.
	Get func(*Config) string
}

func intp(v int) *int { return &v }

// SettingsSchema returns the full, ordered list of editable config fields. It is
// pure: EnvName values are computed once from the Config struct's tags so they
// stay correct if fields are renamed or re-tagged.
func SettingsSchema() []SettingField {
	fields := []SettingField{
		// ---- GitLab ----
		{Key: "gitlab.host", Section: "GitLab", Label: "Host", Kind: KindText,
			Help: "GitLab base URL, e.g. https://gitlab.example.com",
			Get:  func(c *Config) string { return c.GitLab.Host }},
		{Key: "gitlab.token", Section: "GitLab", Label: "Token", Kind: KindPassword, Secret: true,
			Help: "Personal access token (scope: api), stored locally in config.yaml. Leave blank to keep the current one.",
			Get:  func(c *Config) string { return c.GitLab.Token }},
		{Key: "gitlab.token_env", Section: "GitLab", Label: "Token env var", Kind: KindText,
			Help: "Read the token from this env var when 'token' is empty (default GITLAB_TOKEN)",
			Get:  func(c *Config) string { return c.GitLab.TokenEnv }},
		{Key: "gitlab.username", Section: "GitLab", Label: "Username", Kind: KindText,
			Help: "Your GitLab username (reviewer identity)",
			Get:  func(c *Config) string { return c.GitLab.Username }},
		{Key: "gitlab.timeout", Section: "GitLab", Label: "Timeout", Kind: KindDuration,
			Help: "Per-request timeout, e.g. 30s",
			Get:  func(c *Config) string { return c.GitLab.Timeout.String() }},
		{Key: "gitlab.insecure_skip_verify", Section: "GitLab", Label: "Insecure skip TLS verify", Kind: KindBool, Danger: true,
			Help: "Skip TLS verification (self-managed only, explicit opt-in)",
			Get:  func(c *Config) string { return strconv.FormatBool(c.GitLab.InsecureSkipVerify) }},
		{Key: "gitlab.ca_cert_path", Section: "GitLab", Label: "CA cert path", Kind: KindText,
			Help: "Optional custom CA bundle path",
			Get:  func(c *Config) string { return c.GitLab.CACertPath }},

		// ---- LLM ----
		{Key: "llm.provider", Section: "LLM", Label: "Provider", Kind: KindSelect,
			Options: []string{"claude-cli", "anthropic-api"},
			Help:    "LLM provider",
			Get:     func(c *Config) string { return c.LLM.Provider }},
		{Key: "llm.model", Section: "LLM", Label: "Model", Kind: KindSelect,
			Help: "Model name/alias passed to the provider",
			Get:  func(c *Config) string { return c.LLM.Model }},
		{Key: "llm.timeout", Section: "LLM", Label: "Timeout", Kind: KindDuration,
			Help: "Overall LLM call timeout, e.g. 15m",
			Get:  func(c *Config) string { return c.LLM.Timeout.String() }},

		// ---- Claude CLI ----
		{Key: "llm.claude.bin", Section: "Claude CLI", Label: "Binary", Kind: KindText,
			Help: "Path to the claude binary",
			Get:  func(c *Config) string { return c.LLM.Claude.Bin }},
		{Key: "llm.claude.auth_mode", Section: "Claude CLI", Label: "Auth mode", Kind: KindSelect,
			Options: []string{"existing-login", "oauth-token", "api-key"},
			Help:    "How the claude CLI authenticates",
			Get:     func(c *Config) string { return c.LLM.Claude.AuthMode }},
		{Key: "llm.claude.oauth_token_env", Section: "Claude CLI", Label: "OAuth token env var", Kind: KindText,
			Help: "Env var with the Claude Code OAuth token",
			Get:  func(c *Config) string { return c.LLM.Claude.OAuthTokenEnv }},
		{Key: "llm.claude.api_key_env", Section: "Claude CLI", Label: "API key env var", Kind: KindText,
			Help: "Env var with the Anthropic API key",
			Get:  func(c *Config) string { return c.LLM.Claude.APIKeyEnv }},
		{Key: "llm.claude.permission_mode", Section: "Claude CLI", Label: "Permission mode", Kind: KindText,
			Help: "claude --permission-mode value (e.g. dontAsk)",
			Get:  func(c *Config) string { return c.LLM.Claude.PermissionMode }},
		{Key: "llm.claude.agent_mode", Section: "Claude CLI", Label: "Agent mode", Kind: KindBool,
			Help: "Allow read-only repo inspection during review",
			Get:  func(c *Config) string { return strconv.FormatBool(c.LLM.Claude.AgentMode) }},
		{Key: "llm.claude.read_only", Section: "Claude CLI", Label: "Read-only", Kind: KindBool, Danger: true,
			Help: "Deny all write/destructive tools. Turning this off lets the reviewer mutate the checkout.",
			Get:  func(c *Config) string { return strconv.FormatBool(c.LLM.Claude.ReadOnly) }},
		{Key: "llm.claude.allowed_tools", Section: "Claude CLI", Label: "Allowed tools", Kind: KindList,
			Help: "Allowed tool permission rules (one per line)",
			Get:  func(c *Config) string { return strings.Join(c.LLM.Claude.AllowedTools, "\n") }},
		{Key: "llm.claude.skill_tools", Section: "Claude CLI", Label: "Skill tools", Kind: KindList, Danger: true,
			Help: "Tool rules granted when a review selects skills (may include Bash — broader than the default set)",
			Get:  func(c *Config) string { return strings.Join(c.LLM.Claude.SkillTools, "\n") }},
		{Key: "llm.claude.extra_args", Section: "Claude CLI", Label: "Extra args", Kind: KindList,
			Help: "Extra raw CLI args appended to every invocation (one per line)",
			Get:  func(c *Config) string { return strings.Join(c.LLM.Claude.ExtraArgs, "\n") }},

		// ---- Review ----
		{Key: "review.default_mode", Section: "Review", Label: "Default mode", Kind: KindSelect,
			Options: []string{"full", "changed-only"},
			Help:    "Scope of the review",
			Get:     func(c *Config) string { return c.Review.DefaultMode }},
		{Key: "review.max_comments", Section: "Review", Label: "Max comments", Kind: KindInt, Min: intp(0),
			Help: "Max findings surfaced per review",
			Get:  func(c *Config) string { return strconv.Itoa(c.Review.MaxComments) }},
		{Key: "review.severity_threshold", Section: "Review", Label: "Severity threshold", Kind: KindSelect,
			Options: []string{"blocking", "high", "medium", "low", "nit"},
			Help:    "Drop findings below this severity",
			Get:     func(c *Config) string { return c.Review.SeverityThreshold }},
		{Key: "review.preferred_comment_language", Section: "Review", Label: "Comment language", Kind: KindSelect,
			Options: []string{"auto", "en", "ru"},
			Help:    "Language for posted comments (auto = match the MR description)",
			Get:     func(c *Config) string { return c.Review.PreferredCommentLanguage }},
		{Key: "review.create_drafts", Section: "Review", Label: "Create drafts", Kind: KindBool,
			Help: "Auto-create GitLab draft notes (off by default)",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.CreateDrafts) }},
		{Key: "review.auto_review", Section: "Review", Label: "Auto review", Kind: KindBool,
			Help: "Watch-mode auto-runs review (local report only)",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.AutoReview) }},
		{Key: "review.auto_draft", Section: "Review", Label: "Auto draft", Kind: KindBool, Danger: true,
			Help: "Watch-mode may create GitLab draft notes (explicit opt-in)",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.AutoDraft) }},
		{Key: "review.auto_publish", Section: "Review", Label: "Auto publish", Kind: KindBool, Danger: true,
			Help: "DANGER: watch-mode may publish comments to GitLab without manual approval",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.AutoPublish) }},
		{Key: "review.full_repo_context", Section: "Review", Label: "Full repo context", Kind: KindBool,
			Help: "Include relevant repo context beyond the diff",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.FullRepoContext) }},
		{Key: "review.agent_mode", Section: "Review", Label: "Agentic analysis", Kind: KindBool,
			Help: "Enable the agentic deep-analysis stage",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.AgentMode) }},
		{Key: "review.include_tests", Section: "Review", Label: "Include tests", Kind: KindBool,
			Help: "Surface test-related findings",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.IncludeTests) }},
		{Key: "review.include_security", Section: "Review", Label: "Include security", Kind: KindBool,
			Help: "Surface security findings",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.IncludeSecurity) }},
		{Key: "review.include_performance", Section: "Review", Label: "Include performance", Kind: KindBool,
			Help: "Surface performance findings",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.IncludePerformance) }},
		{Key: "review.include_observability", Section: "Review", Label: "Include observability", Kind: KindBool,
			Help: "Surface observability findings",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.IncludeObservability) }},
		{Key: "review.include_style", Section: "Review", Label: "Include style", Kind: KindBool,
			Help: "Surface style findings (off by default)",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.IncludeStyle) }},
		{Key: "review.ignore_globs", Section: "Review", Label: "Ignore globs", Kind: KindList,
			Help: "Globs excluded from context/LLM (one per line)",
			Get:  func(c *Config) string { return strings.Join(c.Review.IgnoreGlobs, "\n") }},

		// ---- Pipeline ----
		{Key: "review.pipeline.mode", Section: "Pipeline", Label: "Mode", Kind: KindSelect,
			Options: []string{"cheap", "standard", "deep", "custom"},
			Help:    "cheap = 1 pass | standard = 2 passes + skeptic | deep = 5 passes + skeptic | custom = passes below",
			Get:     func(c *Config) string { return c.Review.Pipeline.Mode }},
		{Key: "review.pipeline.passes", Section: "Pipeline", Label: "Custom passes", Kind: KindList,
			Help: "Custom mode passes (one per line): general, correctness, concurrency, security, contracts",
			Get:  func(c *Config) string { return strings.Join(c.Review.Pipeline.Passes, "\n") }},
		{Key: "review.pipeline.max_parallel", Section: "Pipeline", Label: "Max parallel passes", Kind: KindInt, Min: intp(1),
			Help: "Concurrent LLM review passes",
			Get:  func(c *Config) string { return strconv.Itoa(c.Review.Pipeline.MaxParallel) }},
		{Key: "review.pipeline.verify_mode", Section: "Pipeline", Label: "Verify mode", Kind: KindSelect,
			Options: []string{"skeptic", "reflect", "off"},
			Help:    "Verification pass that refutes findings against the checkout",
			Get:     func(c *Config) string { return c.Review.Pipeline.VerifyMode }},
		{Key: "review.pipeline.verify_max_findings", Section: "Pipeline", Label: "Verify max findings", Kind: KindInt, Min: intp(0),
			Help: "Max findings sent to the skeptic pass",
			Get:  func(c *Config) string { return strconv.Itoa(c.Review.Pipeline.VerifyMaxFindings) }},
		{Key: "review.pipeline.verifiers", Section: "Pipeline", Label: "Verifiers", Kind: KindList, Danger: true,
			Help: "Deterministic checks (one per line): go_build, go_vet, py_syntax (safe); tsc, go_test execute repo code",
			Get:  func(c *Config) string { return strings.Join(c.Review.Pipeline.Verifiers, "\n") }},
		{Key: "review.pipeline.completeness", Section: "Pipeline", Label: "Completeness", Kind: KindSelect,
			Options: []string{"on", "off", "auto"},
			Help:    "Acceptance-criteria audit (auto: on except cheap mode)",
			Get:     func(c *Config) string { return c.Review.Pipeline.Completeness }},

		// ---- Context ----
		{Key: "review.context.include_full_files", Section: "Context", Label: "Include full files", Kind: KindBool,
			Help: "Include changed files' content (whole file or windows around hunks)",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.Context.IncludeFullFiles) }},
		{Key: "review.context.max_file_lines", Section: "Context", Label: "Max file lines", Kind: KindInt, Min: intp(0),
			Help: "Files longer than this fall back to hunk windows",
			Get:  func(c *Config) string { return strconv.Itoa(c.Review.Context.MaxFileLines) }},
		{Key: "review.context.hunk_window_lines", Section: "Context", Label: "Hunk window lines", Kind: KindInt, Min: intp(0),
			Help: "Context lines around each hunk when windowing",
			Get:  func(c *Config) string { return strconv.Itoa(c.Review.Context.HunkWindowLines) }},
		{Key: "review.context.max_total_kb", Section: "Context", Label: "Max total KB", Kind: KindInt, Min: intp(0),
			Help: "Total budget (KB) for all enrichment sections",
			Get:  func(c *Config) string { return strconv.Itoa(c.Review.Context.MaxTotalKB) }},
		{Key: "review.context.include_commits", Section: "Context", Label: "Include commits", Kind: KindBool,
			Help: "Include the MR's commit messages in the prompt",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.Context.IncludeCommits) }},
		{Key: "review.context.include_discussions", Section: "Context", Label: "Include discussions", Kind: KindBool,
			Help: "Include existing discussion content in the prompt",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.Context.IncludeDiscussions) }},
		{Key: "review.context.max_discussion_kb", Section: "Context", Label: "Max discussion KB", Kind: KindInt, Min: intp(0),
			Help: "Budget (KB) for the discussions section",
			Get:  func(c *Config) string { return strconv.Itoa(c.Review.Context.MaxDiscussionKB) }},
		{Key: "review.context.prior_review", Section: "Context", Label: "Prior review", Kind: KindBool,
			Help: "On re-review, include the previous review + interdiff",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.Context.PriorReview) }},
		{Key: "review.context.interdiff_max_kb", Section: "Context", Label: "Interdiff max KB", Kind: KindInt, Min: intp(0),
			Help: "Budget (KB) for the interdiff section",
			Get:  func(c *Config) string { return strconv.Itoa(c.Review.Context.InterdiffMaxKB) }},
		{Key: "review.context.related_files", Section: "Context", Label: "Related files", Kind: KindInt, Min: intp(0),
			Help: "Max FTS-suggested related files listed as investigation leads (0 = off)",
			Get:  func(c *Config) string { return strconv.Itoa(c.Review.Context.RelatedFiles) }},

		// ---- Risk ----
		{Key: "review.risk.enabled", Section: "Risk", Label: "Enabled", Kind: KindBool,
			Help: "Compute the deterministic risk score",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.Risk.Enabled) }},
		{Key: "review.risk.history_commits", Section: "Risk", Label: "History commits", Kind: KindInt, Min: intp(0),
			Help: "Mirror commits scanned for churn/bug-fix factors",
			Get:  func(c *Config) string { return strconv.Itoa(c.Review.Risk.HistoryCommits) }},
		{Key: "review.risk.sensitive_globs", Section: "Risk", Label: "Sensitive globs", Kind: KindList,
			Help: "Paths whose changes raise risk (one per line)",
			Get:  func(c *Config) string { return strings.Join(c.Review.Risk.SensitiveGlobs, "\n") }},

		// ---- Coverage ----
		{Key: "review.coverage.enabled", Section: "Coverage", Label: "Enabled", Kind: KindBool, Danger: true,
			Help: "Run repo tests to measure changed-line coverage (executes repository code)",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.Coverage.Enabled) }},
		{Key: "review.coverage.providers", Section: "Coverage", Label: "Providers", Kind: KindList,
			Help: "Coverage providers (one per line): go, node",
			Get:  func(c *Config) string { return strings.Join(c.Review.Coverage.Providers, "\n") }},
		{Key: "review.coverage.timeout", Section: "Coverage", Label: "Timeout", Kind: KindDuration,
			Help: "Per-provider test run timeout, e.g. 5m",
			Get:  func(c *Config) string { return c.Review.Coverage.Timeout.String() }},
		{Key: "review.coverage.node.install", Section: "Coverage", Label: "Node install", Kind: KindBool, Danger: true,
			Help: "Allow dependency install (npm ci / pnpm / yarn) when node_modules is missing (runs lifecycle scripts)",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Review.Coverage.Node.Install) }},

		// ---- Watch ----
		{Key: "watch.enabled", Section: "Watch", Label: "Enabled", Kind: KindBool,
			Help: "Run the background watch daemon",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Watch.Enabled) }},
		{Key: "watch.interval", Section: "Watch", Label: "Interval", Kind: KindDuration,
			Help: "How often the daemon polls for changes, e.g. 10m",
			Get:  func(c *Config) string { return c.Watch.Interval.String() }},
		{Key: "watch.max_parallel", Section: "Watch", Label: "Max parallel", Kind: KindInt, Min: intp(1), Restart: true,
			Help: "Concurrent background review jobs (worker pool size — restart to resize)",
			Get:  func(c *Config) string { return strconv.Itoa(c.Watch.MaxParallel) }},
		{Key: "watch.review_new_mrs", Section: "Watch", Label: "Review new MRs", Kind: KindBool,
			Help: "Auto-review newly assigned merge requests",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Watch.ReviewNewMRs) }},
		{Key: "watch.review_new_commits", Section: "Watch", Label: "Review new commits", Kind: KindBool,
			Help: "Re-review when an MR's head SHA changes",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Watch.ReviewNewCommits) }},

		// ---- Index ----
		{Key: "index.enabled", Section: "Index", Label: "Enabled", Kind: KindBool,
			Help: "Index the repository for related-file suggestions",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Index.Enabled) }},
		{Key: "index.fts", Section: "Index", Label: "Full-text search", Kind: KindBool,
			Help: "Enable SQLite FTS5 full-text index",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Index.FTS) }},
		{Key: "index.tree_sitter", Section: "Index", Label: "Tree-sitter", Kind: KindBool,
			Help: "Enable tree-sitter symbol indexing (experimental)",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Index.TreeSitter) }},
		{Key: "index.lsp", Section: "Index", Label: "LSP", Kind: KindBool,
			Help: "Enable LSP-backed indexing (experimental)",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Index.LSP) }},
		{Key: "index.vector_search", Section: "Index", Label: "Vector search", Kind: KindBool,
			Help: "Enable vector search (experimental)",
			Get:  func(c *Config) string { return strconv.FormatBool(c.Index.VectorSearch) }},

		// ---- App (restart required) ----
		{Key: "app.data_dir", Section: "App", Label: "Data dir", Kind: KindText, Restart: true,
			Help: "Base data directory",
			Get:  func(c *Config) string { return c.App.DataDir }},
		{Key: "app.bind_host", Section: "App", Label: "Bind host", Kind: KindText, Required: true, Restart: true,
			Help: "Web UI bind host (localhost only by default)",
			Get:  func(c *Config) string { return c.App.BindHost }},
		{Key: "app.port", Section: "App", Label: "Port", Kind: KindInt, Min: intp(0), Max: intp(65535), Restart: true,
			Help: "Web UI port (0 = random free port)",
			Get:  func(c *Config) string { return strconv.Itoa(c.App.Port) }},
		{Key: "app.ops_port", Section: "App", Label: "Ops port", Kind: KindInt, Min: intp(0), Max: intp(65535), Restart: true,
			Help: "Ops (health/metrics) port (0 = disabled)",
			Get:  func(c *Config) string { return strconv.Itoa(c.App.OpsPort) }},
		{Key: "app.open_browser", Section: "App", Label: "Open browser", Kind: KindBool, Restart: true,
			Help: "Open the browser on serve",
			Get:  func(c *Config) string { return strconv.FormatBool(c.App.OpenBrowser) }},
		{Key: "app.ui", Section: "App", Label: "Primary UI", Kind: KindText, Restart: true,
			Help: "Primary UI: web",
			Get:  func(c *Config) string { return c.App.UI }},

		// ---- Storage (restart required) ----
		{Key: "storage.db_path", Section: "Storage", Label: "Database path", Kind: KindText, Required: true, Restart: true,
			Help: "SQLite database path",
			Get:  func(c *Config) string { return c.Storage.DBPath }},
		{Key: "storage.cache_dir", Section: "Storage", Label: "Cache dir", Kind: KindText, Restart: true,
			Help: "Git cache directory",
			Get:  func(c *Config) string { return c.Storage.CacheDir }},
	}

	env := configEnvNames()
	for i := range fields {
		fields[i].EnvName = env[fields[i].Key]
	}
	return fields
}

// SettingsSchemaByKey indexes SettingsSchema by dotted key for O(1) lookup in
// the apply path.
func SettingsSchemaByKey() map[string]SettingField {
	schema := SettingsSchema()
	m := make(map[string]SettingField, len(schema))
	for _, f := range schema {
		m[f.Key] = f
	}
	return m
}

// envPrefix is the environment override prefix wired in loader.go
// (xconfig.WithEnvPrefix). Kept in sync here so env-shadow warnings name the
// exact variable a user must unset.
const envPrefix = "AI_REVIEWER"

// configEnvNames computes, for every leaf config key, the AI_REVIEWER_*
// environment variable that overrides it — replicating xconfig's derivation
// (explicit `env:` tag wins verbatim; otherwise each Go field-name segment is
// word-split and joined with '_'). Reflecting over the struct keeps these names
// correct even when the naive dot→underscore mapping would be wrong (e.g.
// gitlab.token → AI_REVIEWER_GIT_LAB_TOKEN, because the Go field is "GitLab").
func configEnvNames() map[string]string {
	out := map[string]string{}
	walkConfigEnv(reflect.TypeOf(Config{}), "", nil, out)
	return out
}

func walkConfigEnv(t reflect.Type, yamlPrefix string, envWords []string, out map[string]string) {
	durationType := reflect.TypeOf(time.Duration(0))
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		yamlName := strings.Split(sf.Tag.Get("yaml"), ",")[0]
		if yamlName == "" || yamlName == "-" {
			continue
		}
		yamlKey := yamlName
		if yamlPrefix != "" {
			yamlKey = yamlPrefix + "." + yamlName
		}
		// An explicit env tag on a field short-circuits the whole name (prefix +
		// tag), matching xconfig's leaf/parent handling.
		var words []string
		if envTag := sf.Tag.Get("env"); envTag != "" {
			words = []string{envTag}
		} else {
			words = append(append([]string{}, envWords...), strings.ToUpper(strings.Join(splitNameByWords(sf.Name), "_")))
		}
		ft := sf.Type
		if ft.Kind() == reflect.Struct && ft != durationType {
			walkConfigEnv(ft, yamlKey, words, out)
			continue
		}
		out[yamlKey] = envPrefix + "_" + strings.Join(words, "_")
	}
}

// splitNameByWords splits a PascalCase/acronym identifier into its words,
// mirroring xconfig's utils.SplitNameByWords so derived env names match exactly
// (e.g. "GitLab" → ["Git","Lab"], "OAuthTokenEnv" → ["O","Auth","Token","Env"]).
func splitNameByWords(src string) []string {
	var runes [][]rune
	lastClass, class := 0, 0
	for _, r := range src {
		switch {
		case unicode.IsLower(r):
			class = 1
		case unicode.IsUpper(r):
			class = 2
		case unicode.IsDigit(r):
			class = 3
		default:
			class = 4
		}
		if class == lastClass || (lastClass == 2 && class == 3) {
			sz := len(runes) - 1
			runes[sz] = append(runes[sz], r)
		} else {
			runes = append(runes, []rune{r})
		}
		lastClass = class
	}
	// Pull a trailing uppercase into a following lowercase run: "PDFL","oader" → "PDF","Loader".
	for i := 0; i < len(runes)-1; i++ {
		if len(runes[i]) > 0 && len(runes[i+1]) > 0 &&
			unicode.IsUpper(runes[i][0]) && unicode.IsLower(runes[i+1][0]) {
			runes[i+1] = append([]rune{runes[i][len(runes[i])-1]}, runes[i+1]...)
			runes[i] = runes[i][:len(runes[i])-1]
		}
	}
	words := make([]string, 0, len(runes))
	for _, s := range runes {
		if len(s) > 0 {
			words = append(words, string(s))
		}
	}
	return words
}
