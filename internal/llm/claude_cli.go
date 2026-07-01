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

// invoke runs the CLI and returns the parsed envelope.
func (c *ClaudeCLI) invoke(ctx context.Context, req Request) (*claudeEnvelope, error) {
	model := req.Model
	if model == "" {
		model = c.opts.Model
	}

	args := []string{"-p", "--output-format", "json", "--bare", "--permission-mode", c.opts.PermissionMode}
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
		errText := security.Mask(strings.TrimSpace(stderr.String()))
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("claude timed out after %s: %s", c.opts.Timeout, errText)
		}
		return nil, fmt.Errorf("claude failed: %w: %s", err, errText)
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
