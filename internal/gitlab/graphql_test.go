package gitlab

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestGraphQL(t *testing.T, srv *httptest.Server) *GraphQLClient {
	t.Helper()
	g, err := NewGraphQL(Config{Host: srv.URL, Token: "glpat-testtoken1234567890", Timeout: 5 * time.Second, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// graphQLServer replies with body and captures the decoded request.
func graphQLServer(t *testing.T, status int, body string, got *map[string]any, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		if r.URL.Path != "/api/graphql" {
			t.Errorf("path = %q, want /api/graphql", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if tok := r.Header.Get("PRIVATE-TOKEN"); tok != "glpat-testtoken1234567890" {
			t.Errorf("token header = %q", tok)
		}
		if got != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGraphQLReviewStates(t *testing.T) {
	var got map[string]any
	var calls atomic.Int32
	srv := graphQLServer(t, http.StatusOK, `{"data":{"project":{"mergeRequests":{"nodes":[
		{"iid":"7","reviewers":{"nodes":[
			{"username":"bob","mergeRequestInteraction":{"reviewState":"REVIEWED","approved":false,"reviewed":true}},
			{"username":"carol","mergeRequestInteraction":{"reviewState":"UNREVIEWED","approved":false,"reviewed":false}}
		]}},
		{"iid":"9","reviewers":{"nodes":[
			{"username":"dave","mergeRequestInteraction":{"reviewState":"REQUESTED_CHANGES","approved":false,"reviewed":true}}
		]}}
	]}}}}`, &got, &calls)

	states, err := newTestGraphQL(t, srv).ReviewStates(t.Context(), "group/repo", []int64{7, 9})
	if err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("one POST per project per run, got %d", n)
	}

	// The whole batch travels in one request.
	vars, _ := got["variables"].(map[string]any)
	if vars["fullPath"] != "group/repo" {
		t.Errorf("fullPath = %v", vars["fullPath"])
	}
	iids, _ := vars["iids"].([]any)
	if len(iids) != 2 || iids[0] != "7" || iids[1] != "9" {
		t.Errorf("iids = %v, want the string form GitLab expects", iids)
	}

	if len(states) != 2 {
		t.Fatalf("got %d MRs, want 2: %+v", len(states), states)
	}
	if len(states[7]) != 2 {
		t.Fatalf("MR 7 reviewers = %+v", states[7])
	}
	if states[7][0].Username != "bob" || states[7][0].State != ReviewStateReviewed || !states[7][0].Reviewed {
		t.Errorf("bob = %+v", states[7][0])
	}
	if states[7][1].State != ReviewStateUnreviewed {
		t.Errorf("carol = %+v", states[7][1])
	}
	if states[9][0].State != ReviewStateRequestedChanges {
		t.Errorf("dave = %+v", states[9][0])
	}
}

// Every degradation mode must be recognizable with one errors.Is check, so the
// caller can log once per run and fall back to the REST heuristic.
func TestGraphQLFallbackSignals(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "graphql errors array",
			status: http.StatusOK,
			body:   `{"errors":[{"message":"Field 'reviewState' doesn't exist on type 'MergeRequestInteraction'"}],"data":{"project":null}}`,
		},
		{
			name:   "reviewState missing from the schema response",
			status: http.StatusOK,
			body: `{"data":{"project":{"mergeRequests":{"nodes":[
				{"iid":"7","reviewers":{"nodes":[{"username":"bob","mergeRequestInteraction":{"approved":false,"reviewed":false}}]}}
			]}}}}`,
		},
		{
			name:   "mergeRequestInteraction null",
			status: http.StatusOK,
			body: `{"data":{"project":{"mergeRequests":{"nodes":[
				{"iid":"7","reviewers":{"nodes":[{"username":"bob","mergeRequestInteraction":null}]}}
			]}}}}`,
		},
		{
			name:   "project not visible",
			status: http.StatusOK,
			body:   `{"data":{"project":null}}`,
		},
		{
			name:   "graphql endpoint absent",
			status: http.StatusNotFound,
			body:   `{"message":"404 Not Found"}`,
		},
		{
			name:   "token rejected",
			status: http.StatusUnauthorized,
			body:   `{"message":"401 Unauthorized"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := graphQLServer(t, tc.status, tc.body, nil, nil)
			states, err := newTestGraphQL(t, srv).ReviewStates(t.Context(), "group/repo", []int64{7})
			if err == nil {
				t.Fatalf("want a fallback signal, got states %+v", states)
			}
			if !errors.Is(err, ErrGraphQLUnsupported) {
				t.Errorf("errors.Is(err, ErrGraphQLUnsupported) = false for %v", err)
			}
			if states != nil {
				t.Errorf("states must be nil on the fallback path, got %+v", states)
			}
		})
	}
}

// An MR without reviewers is not a degradation: absent from the map, no error.
func TestGraphQLNoReviewersIsNotAFailure(t *testing.T) {
	srv := graphQLServer(t, http.StatusOK,
		`{"data":{"project":{"mergeRequests":{"nodes":[{"iid":"7","reviewers":{"nodes":[]}}]}}}}`, nil, nil)

	states, err := newTestGraphQL(t, srv).ReviewStates(t.Context(), "group/repo", []int64{7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("states = %+v, want empty", states)
	}
}

// No iids means nothing to ask about: no request at all.
func TestGraphQLEmptyBatchSkipsRequest(t *testing.T) {
	var calls atomic.Int32
	srv := graphQLServer(t, http.StatusOK, `{"data":{"project":null}}`, nil, &calls)

	states, err := newTestGraphQL(t, srv).ReviewStates(t.Context(), "group/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Errorf("states = %+v", states)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("made %d requests for an empty batch", n)
	}
}

// The GraphQL client hangs off the same host/token/transport as REST.
func TestClientGraphQLSharesTransport(t *testing.T) {
	srv := graphQLServer(t, http.StatusOK,
		`{"data":{"project":{"mergeRequests":{"nodes":[{"iid":"1","reviewers":{"nodes":[{"username":"bob","mergeRequestInteraction":{"reviewState":"APPROVED","approved":true,"reviewed":true}}]}}]}}}}`,
		nil, nil)

	states, err := newTestClient(t, srv).GraphQL().ReviewStates(t.Context(), "group/repo", []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	if states[1][0].State != ReviewStateApproved || !states[1][0].Approved {
		t.Errorf("states = %+v", states)
	}
}

func TestFakeGraphQLFallback(t *testing.T) {
	f := &FakeGraphQL{Err: ErrGraphQLUnsupported}
	if _, err := f.ReviewStates(t.Context(), "group/repo", []int64{1}); !errors.Is(err, ErrGraphQLUnsupported) {
		t.Errorf("err = %v", err)
	}
	if f.CallCount() != 1 {
		t.Errorf("calls = %d", f.CallCount())
	}
}
