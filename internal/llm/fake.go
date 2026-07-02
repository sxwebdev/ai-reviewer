package llm

import (
	"context"
	"encoding/json"
	"sync"
)

// FakeClient is a scripted LLM client for tests. It is safe for concurrent use
// (the engine fans review passes out in parallel) and records every request so
// tests can assert on prompts, working dir, and agent-mode flags.
//
// Scripting: the *Fn hooks take precedence when set and may vary the response
// per request (e.g. per specialist pass, keyed on the system prompt); otherwise
// the static Response/JSON/AskText fields are served.
type FakeClient struct {
	mu sync.Mutex

	Response *ReviewResponse
	JSON     string
	JSONCost float64 // cost returned by CompleteJSON
	AskText  string
	Err      error

	ReviewFn func(req Request) (*ReviewResponse, error)
	JSONFn   func(req Request) (string, error)

	LastRequest Request
	Requests    []Request
	Calls       int
}

// NewFake returns a fake that returns the given review response.
func NewFake(resp *ReviewResponse) *FakeClient { return &FakeClient{Response: resp} }

func (f *FakeClient) record(req Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LastRequest = req
	f.Requests = append(f.Requests, req)
	f.Calls++
}

func (f *FakeClient) Review(ctx context.Context, req Request) (*ReviewResponse, error) {
	f.record(req)
	if f.ReviewFn != nil {
		return f.ReviewFn(req)
	}
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Response, nil
}

func (f *FakeClient) CompleteJSON(ctx context.Context, req Request, out any) (float64, error) {
	f.record(req)
	if f.JSONFn != nil {
		s, err := f.JSONFn(req)
		if err != nil {
			return 0, err
		}
		return f.JSONCost, json.Unmarshal([]byte(s), out)
	}
	if f.Err != nil {
		return 0, f.Err
	}
	return f.JSONCost, json.Unmarshal([]byte(f.JSON), out)
}

func (f *FakeClient) Ask(ctx context.Context, req Request) (string, error) {
	f.record(req)
	return f.AskText, f.Err
}

var _ Client = (*FakeClient)(nil)
