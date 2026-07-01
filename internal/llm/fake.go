package llm

import (
	"context"
	"encoding/json"
)

// FakeClient is a scripted LLM client for tests. It records the last request so
// tests can assert on prompts, working dir, and agent-mode flags.
type FakeClient struct {
	Response    *ReviewResponse
	JSON        string
	AskText     string
	Err         error
	LastRequest Request
	Calls       int
}

// NewFake returns a fake that returns the given review response.
func NewFake(resp *ReviewResponse) *FakeClient { return &FakeClient{Response: resp} }

func (f *FakeClient) Review(ctx context.Context, req Request) (*ReviewResponse, error) {
	f.LastRequest = req
	f.Calls++
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Response, nil
}

func (f *FakeClient) CompleteJSON(ctx context.Context, req Request, out any) error {
	f.LastRequest = req
	f.Calls++
	if f.Err != nil {
		return f.Err
	}
	return json.Unmarshal([]byte(f.JSON), out)
}

func (f *FakeClient) Ask(ctx context.Context, req Request) (string, error) {
	f.LastRequest = req
	f.Calls++
	return f.AskText, f.Err
}

var _ Client = (*FakeClient)(nil)
