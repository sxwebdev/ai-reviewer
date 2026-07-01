package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// defaultYAML is the documented starter config written by `ai-reviewer init`.
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
  token_env: "GITLAB_TOKEN"  # env var holding your PAT (scope: api)
  username: ""             # your GitLab username (reviewer identity)
  timeout: "30s"
  insecure_skip_verify: false

llm:
  provider: "claude-cli"
  model: "sonnet"
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
  preferred_comment_language: "auto"
  ignore_globs:
    - "vendor/**"
    - "node_modules/**"
    - "dist/**"
    - "build/**"
    - "*.generated.*"
    - "*.pb.go"
    - "*.min.js"

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
