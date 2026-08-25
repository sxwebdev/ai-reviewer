package gitlab

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// capturedRequest is what the fake GitLab saw.
type capturedRequest struct {
	method string
	path   string
	raw    []byte
	body   map[string]any
}

// captureServer records the one request it receives and replies with respBody.
func captureServer(t *testing.T, respBody string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.EscapedPath()
		got.raw, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(got.raw, &got.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// The publication path posts one discussion per finding. Position serialization
// is the thing that can only fail against a live GitLab: a wrong shape means
// every inline finding 400s. models.go documents the rules — added line sends
// new_line only, removed line old_line only, context line both — and *int +
// omitempty is what enforces them, so pin all three here.
func TestCreateDiscussionPositionWireShape(t *testing.T) {
	line := func(n int) *int { return &n }

	tests := []struct {
		name        string
		pos         *Position
		wantNewLine any // nil means "key must be absent"
		wantOldLine any
	}{
		{
			name:        "added line sends new_line only",
			pos:         &Position{NewLine: line(42)},
			wantNewLine: float64(42),
		},
		{
			name:        "removed line sends old_line only",
			pos:         &Position{OldLine: line(17)},
			wantOldLine: float64(17),
		},
		{
			name:        "context line sends both",
			pos:         &Position{OldLine: line(17), NewLine: line(42)},
			wantOldLine: float64(17),
			wantNewLine: float64(42),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pos := *tc.pos
			pos.BaseSHA, pos.HeadSHA, pos.StartSHA = "base1", "head2", "start3"
			pos.PositionType = "text"
			pos.OldPath, pos.NewPath = "internal/a.go", "internal/a.go"

			srv, got := captureServer(t, `{"id":"disc-1","notes":[{"id":5}]}`)
			d, err := newTestClient(t, srv).CreateDiscussion(t.Context(), "group%2Frepo", 7, "**[high/correctness] Leak**", &pos)
			if err != nil {
				t.Fatal(err)
			}
			if d.ID != "disc-1" {
				t.Errorf("response not decoded: %+v", d)
			}
			if got.method != http.MethodPost {
				t.Errorf("method = %q, want POST", got.method)
			}
			if want := "/api/v4/projects/group%2Frepo/merge_requests/7/discussions"; got.path != want {
				t.Errorf("path = %q, want %q", got.path, want)
			}
			if got.body["body"] != "**[high/correctness] Leak**" {
				t.Errorf("body = %v", got.body["body"])
			}

			p, ok := got.body["position"].(map[string]any)
			if !ok {
				t.Fatalf("position missing or not an object: %s", got.raw)
			}
			for k, want := range map[string]any{
				"position_type": "text",
				"base_sha":      "base1",
				"head_sha":      "head2",
				"start_sha":     "start3",
				"old_path":      "internal/a.go",
				"new_path":      "internal/a.go",
			} {
				if p[k] != want {
					t.Errorf("position[%q] = %v, want %v", k, p[k], want)
				}
			}
			for k, want := range map[string]any{"new_line": tc.wantNewLine, "old_line": tc.wantOldLine} {
				v, present := p[k]
				if want == nil {
					if present {
						t.Errorf("position[%q] = %v, want the key to be omitted", k, v)
					}
					continue
				}
				if !present {
					t.Errorf("position[%q] missing, want %v", k, want)
				} else if v != want {
					t.Errorf("position[%q] = %v, want %v", k, v, want)
				}
			}
		})
	}
}

// The validator's fallback ladder ends at an overview note with no position.
// The key must be absent, not null: GitLab 400s on a null position.
func TestCreateDiscussionWithoutPositionOmitsTheKey(t *testing.T) {
	srv, got := captureServer(t, `{"id":"disc-2"}`)
	if _, err := newTestClient(t, srv).CreateDiscussion(t.Context(), "42", 7, "overview", nil); err != nil {
		t.Fatal(err)
	}
	if _, present := got.body["position"]; present {
		t.Errorf("position must be omitted entirely for a non-positioned discussion, got: %s", got.raw)
	}
	if got.body["body"] != "overview" {
		t.Errorf("body = %v", got.body["body"])
	}
}

// The summary note carrying the review marker is published last; it is a plain
// note with no position at all.
func TestCreateMRNote(t *testing.T) {
	srv, got := captureServer(t, `{"id":991,"body":"summary","created_at":"2026-08-13T09:12:44Z"}`)

	body := "🤖 **AI review** — 2 finding(s)\n\n" + ReviewMarker{HeadSHA: "abc"}.Render()
	n, err := newTestClient(t, srv).CreateMRNote(t.Context(), "group%2Frepo", 7, body)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != 991 || n.Body != "summary" {
		t.Errorf("note not decoded from the response: %+v", n)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if want := "/api/v4/projects/group%2Frepo/merge_requests/7/notes"; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if got.body["body"] != body {
		t.Errorf("body = %v, want the marker-bearing summary", got.body["body"])
	}
	if len(got.body) != 1 {
		t.Errorf("a plain note must send only {\"body\":…}, got: %s", got.raw)
	}
	// The marker must survive the round trip through the request body.
	if _, ok := ParseReviewMarker(got.body["body"].(string)); !ok {
		t.Error("review marker did not survive into the request body")
	}
}
