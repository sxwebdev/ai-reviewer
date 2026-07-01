package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sxwebdev/ai-reviewer/internal/security"
)

// ClaudeOptions configures the Claude CLI provider. All flags are configurable
// so the wrapper survives CLI version drift and different auth modes.
type ClaudeOptions struct {
	Bin            string
	Model          string
	PermissionMode string
	Timeout        time.Duration
	ExtraArgs      []string
	Debug          bool
}

// ClaudeCLI runs the `claude` binary as a subprocess in headless JSON mode.
type ClaudeCLI struct {
	opts ClaudeOptions
	log  *slog.Logger
}

// NewClaudeCLI builds a Claude CLI client.
func NewClaudeCLI(opts ClaudeOptions, log *slog.Logger) *ClaudeCLI {
	if opts.Bin == "" {
		opts.Bin = "claude"
	}
	if opts.PermissionMode == "" {
		opts.PermissionMode = "dontAsk"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Minute
	}
	return &ClaudeCLI{opts: opts, log: log}
}

var _ Client = (*ClaudeCLI)(nil)

// claudeEnvelope is the `--output-format json` result envelope.
type claudeEnvelope struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

// errMaxStructuredOutputRetries is claude's envelope subtype when it cannot
// produce output satisfying --json-schema within its internal retry budget
// (common on long agent runs that keep wanting to call tools).
const errMaxStructuredOutputRetries = "error_max_structured_output_retries"

// invoke runs the CLI with one graceful fallback: if a schema-constrained call
// fails because claude could not satisfy --json-schema within its retry budget
// (errMaxStructuredOutputRetries), it retries once WITHOUT --json-schema and
// relies on our own extraction (pickJSON → ExtractJSONObject). The system prompt
// already demands strict JSON, so this drops enforcement, not correctness, and
// turns a hard failure into a recoverable one without a full job retry.
func (c *ClaudeCLI) invoke(ctx context.Context, req Request) (*claudeEnvelope, error) {
	env, err := c.runOnce(ctx, req)
	if err != nil && req.JSONSchema != "" && strings.Contains(err.Error(), errMaxStructuredOutputRetries) {
		c.log.Warn("claude could not satisfy --json-schema; retrying once without it and extracting JSON ourselves", "err", err)
		req.JSONSchema = ""
		return c.runOnce(ctx, req)
	}
	return env, err
}

// runOnce runs the CLI once and returns the parsed envelope.
func (c *ClaudeCLI) runOnce(ctx context.Context, req Request) (*claudeEnvelope, error) {
	model := req.Model
	if model == "" {
		model = c.opts.Model
	}

	// NOTE: do NOT add --bare here. It puts claude in "minimal mode" which skips
	// keychain reads, so a subscription/existing-login (stored in the macOS
	// keychain) resolves as "Not logged in". --output-format json already gives a
	// clean envelope, so --bare buys us nothing but broken auth.
	args := []string{"-p", "--output-format", "json", "--permission-mode", c.opts.PermissionMode}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.System != "" {
		args = append(args, "--append-system-prompt", req.System)
	}
	if req.JSONSchema != "" {
		args = append(args, "--json-schema", req.JSONSchema)
	}
	if req.AgentMode && len(req.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(req.AllowedTools, ","))
	}
	args = append(args, c.opts.ExtraArgs...)

	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.opts.Bin, args...)
	cmd.Dir = req.WorkDir
	cmd.Env = os.Environ() // inherit existing Claude Code login / token env
	cmd.Stdin = strings.NewReader(req.Prompt)
	// Run claude in its own process group and kill the whole group on timeout,
	// so tool subprocesses (git/bash in agent mode) don't survive as orphans.
	setProcessGroup(cmd)
	cmd.WaitDelay = 10 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	c.log.Debug("running claude", "bin", c.opts.Bin, "model", model, "workdir", req.WorkDir, "agent", req.AgentMode)
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("claude timed out after %s", c.opts.Timeout)
		}
		// On non-zero exit claude prints its error as a JSON envelope on stdout
		// (e.g. "Not logged in · Please run /login", invalid model, overload) and
		// leaves stderr empty, so surface stdout's result — not just stderr.
		detail := security.Mask(claudeErrDetail(stdout.Bytes(), stderr.String()))
		if hint := claudeErrHint(detail); hint != "" {
			return nil, fmt.Errorf("claude failed (%w): %s — %s", err, detail, hint)
		}
		return nil, fmt.Errorf("claude failed (%w): %s", err, detail)
	}

	var env claudeEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		// With --output-format json claude always emits a JSON envelope; if it
		// didn't, something went wrong (auth/overload/warning printed instead).
		// Do NOT treat that prose as a successful result.
		return nil, fmt.Errorf("claude did not return a JSON envelope: %s", security.Mask(truncate(stdout.String(), 500)))
	}
	if env.IsError {
		return nil, fmt.Errorf("claude returned an error result: %s", security.Mask(env.Result))
	}
	return &env, nil
}

// claudeErrDetail extracts the most useful human-readable text from a failed
// claude run: the envelope's result field if stdout parsed as JSON (claude puts
// its error there), otherwise raw stdout, then stderr.
func claudeErrDetail(stdout []byte, stderr string) string {
	var env claudeEnvelope
	if json.Unmarshal(stdout, &env) == nil {
		if s := strings.TrimSpace(env.Result); s != "" {
			return truncate(s, 500)
		}
		// is_error with no result text (e.g. error_max_structured_output_retries):
		// surface the subtype rather than dumping the whole JSON envelope.
		if env.IsError && env.Subtype != "" {
			return "error: " + env.Subtype
		}
	}
	if s := strings.TrimSpace(string(stdout)); s != "" {
		return truncate(s, 500)
	}
	if s := strings.TrimSpace(stderr); s != "" {
		return truncate(s, 500)
	}
	return "no output on stdout/stderr"
}

// claudeErrHint maps a known claude error message to an actionable hint, or "".
func claudeErrHint(detail string) string {
	switch {
	case strings.Contains(detail, "Not logged in"), strings.Contains(detail, "/login"):
		return "run `claude` once and `/login`, or set an auth token (claude setup-token → CLAUDE_CODE_OAUTH_TOKEN)"
	case strings.Contains(detail, errMaxStructuredOutputRetries):
		return "the model couldn't produce schema-valid output; the wrapper retries once without --json-schema — if it persists, try review.agent_mode: false"
	case strings.Contains(detail, "model"):
		return "check llm.model (use an alias like `opus`/`sonnet` or a full ID like `claude-opus-4-8`)"
	default:
		return ""
	}
}

// truncate shortens s to at most n bytes (rune-safe) for error messages.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// pickJSON returns the strict JSON payload: structured_output if present, else
// the first balanced object extracted from the result text.
func pickJSON(env *claudeEnvelope) (string, error) {
	if len(env.StructuredOutput) > 0 && string(env.StructuredOutput) != "null" {
		return string(env.StructuredOutput), nil
	}
	return ExtractJSONObject(env.Result)
}

// CompleteJSON runs the request and unmarshals strict JSON into out.
func (c *ClaudeCLI) CompleteJSON(ctx context.Context, req Request, out any) error {
	env, err := c.invoke(ctx, req)
	if err != nil {
		return err
	}
	jsonStr, err := pickJSON(env)
	if err != nil {
		return fmt.Errorf("extract JSON from claude output: %w", err)
	}
	if err := json.Unmarshal([]byte(jsonStr), out); err != nil {
		return fmt.Errorf("unmarshal claude JSON: %w", err)
	}
	return nil
}

// Review runs a review with the strict schema and returns the parsed response.
func (c *ClaudeCLI) Review(ctx context.Context, req Request) (*ReviewResponse, error) {
	if req.JSONSchema == "" {
		req.JSONSchema = ReviewJSONSchema
	}
	env, err := c.invoke(ctx, req)
	if err != nil {
		return nil, err
	}
	jsonStr, err := pickJSON(env)
	if err != nil {
		return nil, fmt.Errorf("extract review JSON: %w", err)
	}
	var resp ReviewResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal review JSON: %w", err)
	}
	resp.Raw = jsonStr
	resp.CostUSD = env.TotalCostUSD
	return &resp, nil
}

// Ask runs a free-form prompt and returns the text answer.
func (c *ClaudeCLI) Ask(ctx context.Context, req Request) (string, error) {
	env, err := c.invoke(ctx, req)
	if err != nil {
		return "", err
	}
	return env.Result, nil
}
