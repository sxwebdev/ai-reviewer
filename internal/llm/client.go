package llm

import "context"

// Request is the input to any LLM invocation.
type Request struct {
	System       string   // system prompt / instructions
	Prompt       string   // user prompt (fed via stdin for the CLI provider)
	Model        string   // model alias/name; empty uses the provider default
	WorkDir      string   // process working directory (agent mode reads the repo here)
	AgentMode    bool     // allow read-only repo inspection via tools
	AllowedTools []string // tool permission rules (agent mode)
	JSONSchema   string   // optional strict-output JSON schema
}

// Client is the provider-agnostic LLM interface. Implementations must never be
// allowed to write to the repo or the network beyond their model call.
type Client interface {
	// Review runs a code review and returns the strict ReviewResponse.
	Review(ctx context.Context, req Request) (*ReviewResponse, error)
	// CompleteJSON runs a prompt and unmarshals strict JSON output into out.
	CompleteJSON(ctx context.Context, req Request, out any) error
	// Ask runs a free-form prompt and returns the text answer.
	Ask(ctx context.Context, req Request) (string, error)
}
