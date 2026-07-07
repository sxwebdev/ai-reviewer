package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// defaultYAML is the documented starter config, written automatically the
// first time settings are persisted (web UI setup) via PatchFile.
const defaultYAML = `# ai-reviewer configuration
# Docs: https://github.com/sxwebdev/ai-reviewer

app:
  data_dir: "~/.ai-reviewer"
  bind_host: "127.0.0.1"   # localhost only by default
  port: 0                  # 0 = random free port
  open_browser: true
  ui: "web"

gitlab:
  host: ""                 # e.g. https://gitlab.example.com
  token: ""                # your GitLab PAT (scope: api); stored in this file (kept at chmod 600)
  username: ""             # your GitLab username (reviewer identity)
  timeout: "30s"
  insecure_skip_verify: false
  # token_env: "GITLAB_TOKEN"  # optional fallback: read the token from this env var when 'token' is empty

llm:
  provider: "claude-cli"
  model: "claude-sonnet-5"   # claude-opus-4-8 | claude-sonnet-5 | claude-haiku-4-5-20251001 | claude-fable-5
  timeout: "15m"
  claude:
    bin: "claude"
    auth_mode: "existing-login"   # existing-login | oauth-token | api-key
    oauth_token_env: "CLAUDE_CODE_OAUTH_TOKEN"
    api_key_env: "ANTHROPIC_API_KEY"
    permission_mode: "dontAsk"
    agent_mode: true
    read_only: true
    allowed_tools:
      - "Read"
      - "Grep"
      - "Glob"
      - "Bash(git diff *)"
      - "Bash(git log *)"
    # Granted only when a review selects skills (skills may run Bash, etc.):
    skill_tools:
      - "Skill"
      - "Read"
      - "Grep"
      - "Glob"
      - "Bash"

review:
  default_mode: "full"
  max_comments: 12
  severity_threshold: "medium"
  create_drafts: false
  auto_review: true
  auto_draft: false
  auto_publish: false        # DANGER: keep false
  full_repo_context: true
  agent_mode: true
  include_tests: true
  include_security: true
  include_performance: true
  include_observability: true
  include_style: false
  preferred_comment_language: "auto"   # auto (match the MR description language) | en | ru
  ignore_globs:
    - "vendor/**"
    - "node_modules/**"
    - "dist/**"
    - "build/**"
    - "*.generated.*"
    - "*.pb.go"
    - "*.min.js"

  # Multi-pass review pipeline. Modes trade cost for depth:
  #   cheap    — 1 LLM call (single general pass, no verification)
  #   standard — general + correctness passes, then a skeptic verification pass (~3 calls)
  #   deep     — general + correctness + concurrency + security + contracts + skeptic (~6 calls);
  #              recommended when catching real bugs matters more than cost
  #   custom   — use the 'passes' list below
  pipeline:
    mode: "standard"
    # passes: ["general", "correctness", "concurrency", "security", "contracts"]  # custom mode only
    max_parallel: 2
    verify_mode: "skeptic"       # skeptic (refute findings against the checkout) | reflect | off
    verify_max_findings: 24
    # Deterministic verifiers. Default set never executes repository code:
    # go_build/go_vet use the system toolchain, py_syntax is a pure parse.
    # "tsc" (runs the repo's node_modules tsc) and "go_test" (runs repo tests)
    # execute repository code — add them explicitly if you accept that.
    verifiers: ["go_build", "go_vet", "py_syntax"]
    completeness: "auto"         # acceptance-criteria audit: on | off | auto (auto: on except cheap mode)

  # Deterministic risk score computed from git history and diff stats
  # (independent of the model's risk_level; shown side by side in the UI).
  risk:
    enabled: true
    history_commits: 500         # mirror commits scanned for churn/bug-fix factors
    sensitive_globs:
      - "**/auth/**"
      - "**/crypto/**"
      - "**/security/**"
      - "**/migrations/**"
      - "**/*.sql"
      - ".gitlab-ci.yml"
      - ".github/**"
      - "Dockerfile*"
      - "go.mod"
      - "go.sum"
      - "package.json"
      - "requirements*.txt"
      - "pyproject.toml"
      - "package-lock.json"
      - "yarn.lock"
      - "pnpm-lock.yaml"
      - "Cargo.lock"
      - "Gemfile.lock"
      - "poetry.lock"
      - "composer.lock"

  # Changed-line test coverage: runs the repository's OWN tests (go test,
  # vitest/jest) against the MR worktree and reports which added lines no test
  # executes. This executes repository code — explicit opt-in.
  coverage:
    enabled: false
    providers: ["go", "node"]
    timeout: "5m"                # per project root
    node:
      install: false             # allow npm ci / pnpm / yarn when node_modules is missing (runs lifecycle scripts)

  # Extra prompt context beyond the diffs (token budget knobs).
  context:
    include_full_files: true     # include changed files' content (whole file or windows around hunks)
    max_file_lines: 500          # longer files fall back to hunk windows
    hunk_window_lines: 60
    max_total_kb: 256            # shared budget for all enrichment sections
    include_commits: true        # commit messages carry intent the description often lacks
    include_discussions: true    # existing discussion content so the model does not repeat settled topics
    max_discussion_kb: 4
    prior_review: true           # on re-review: previous summary, findings, rejection reasons, interdiff
    interdiff_max_kb: 32
    related_files: 5             # FTS-suggested investigation leads in agent mode (0 = off)

watch:
  enabled: true
  interval: "10m"
  max_parallel: 2
  review_new_mrs: true
  review_new_commits: true

index:
  enabled: true
  fts: true
  tree_sitter: false
  lsp: false
  vector_search: false

storage:
  db_path: "~/.ai-reviewer/state.db"
  cache_dir: "~/.ai-reviewer/cache"
`

// WriteDefaultFile writes the documented default config to path, creating parent
// directories. It refuses to overwrite an existing file.
func WriteDefaultFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultYAML), 0o600)
}
