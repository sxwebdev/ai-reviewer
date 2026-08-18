package linear_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/linear"
)

const testKey = "lin_api_secret_123456"

type request struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func newClient(t *testing.T, handler http.HandlerFunc, mutate ...func(*linear.Config)) (*linear.Client, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	cfg := linear.Config{Endpoint: server.URL, APIKey: testKey, MaxAttempts: 1}
	for _, fn := range mutate {
		fn(&cfg)
	}
	client, err := linear.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, &calls
}

func decodeRequest(t *testing.T, r *http.Request) request {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.Method)
	}
	if got := r.Header.Get("Authorization"); got != testKey {
		t.Errorf("Authorization = %q, want raw API key", got)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return req
}

func writeResponse(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestNewValidatesConnectionSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  linear.Config
	}{
		{name: "relative endpoint", cfg: linear.Config{Endpoint: "/graphql", APIKey: testKey}},
		{name: "missing key", cfg: linear.Config{Endpoint: linear.DefaultEndpoint}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := linear.New(tt.cfg); err == nil {
				t.Fatal("New succeeded, want error")
			}
		})
	}
}

func TestViewerAndTeam(t *testing.T) {
	t.Parallel()
	client, calls := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		switch {
		case strings.Contains(req.Query, "query Viewer"):
			writeResponse(t, w, `{"data":{"viewer":{"id":"u1","name":"Digest Bot","email":"bot@example.com"}}}`)
		case strings.Contains(req.Query, "query Team"):
			if req.Variables["id"] != "team-1" {
				t.Errorf("team id = %v", req.Variables["id"])
			}
			writeResponse(t, w, `{"data":{"team":{"id":"team-1","key":"PAY","name":"Payments","states":{"nodes":[{"id":"s1","name":"In Review"}]}}}}`)
		default:
			t.Errorf("unexpected query: %s", req.Query)
		}
	})

	viewer, err := client.Viewer(t.Context())
	if err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	if viewer.ID != "u1" || viewer.Name != "Digest Bot" {
		t.Errorf("viewer = %+v", viewer)
	}
	team, err := client.GetTeam(t.Context(), "team-1")
	if err != nil {
		t.Fatalf("GetTeam: %v", err)
	}
	if team.Key != "PAY" || len(team.States) != 1 || team.States[0].Name != linear.InReviewState {
		t.Errorf("team = %+v", team)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestListIssuesInReviewPaginatesFiltersAndDeduplicates(t *testing.T) {
	t.Parallel()
	var page atomic.Int64
	client, calls := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		if !strings.Contains(req.Query, `state: { name: { eqIgnoreCase: $stateName } }`) {
			t.Errorf("query has no server-side state filter:\n%s", req.Query)
		}
		if req.Variables["stateName"] != linear.InReviewState {
			t.Errorf("stateName = %v", req.Variables["stateName"])
		}
		ids, ok := req.Variables["teamIds"].([]any)
		if !ok || len(ids) != 2 || ids[0] != "t1" || ids[1] != "t2" {
			t.Errorf("teamIds = %#v", req.Variables["teamIds"])
		}
		if page.Add(1) == 1 {
			if req.Variables["after"] != nil {
				t.Errorf("first after = %v, want null", req.Variables["after"])
			}
			writeResponse(t, w, `{"data":{"issues":{"nodes":[`+
				`{"id":"i1","identifier":"PAY-1","title":"First","url":"https://linear/1","updatedAt":"2026-08-17T10:00:00Z","state":{"id":"s1","name":" in review "},"team":{"id":"t1","key":"PAY","name":"Payments"},"assignee":{"id":"u1","name":"Ann"}},`+
				`{"id":"bad","identifier":"PAY-2","title":"Wrong state","url":"https://linear/2","updatedAt":"2026-08-17T10:00:00Z","state":{"id":"s2","name":"Done"},"team":{"id":"t1","key":"PAY","name":"Payments"},"assignee":null}`+
				`],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}`)
			return
		}
		if req.Variables["after"] != "cursor-1" {
			t.Errorf("second after = %v", req.Variables["after"])
		}
		writeResponse(t, w, `{"data":{"issues":{"nodes":[`+
			`{"id":"i1","identifier":"PAY-1","title":"duplicate","url":"https://linear/1","updatedAt":"2026-08-17T10:00:00Z","state":{"id":"s1","name":"In Review"},"team":{"id":"t1","key":"PAY","name":"Payments"},"assignee":null},`+
			`{"id":"i3","identifier":"OPS-9","title":"Second","url":"https://linear/3","updatedAt":"2026-08-17T11:00:00Z","state":{"id":"s3","name":"IN REVIEW"},"team":{"id":"t2","key":"OPS","name":"Operations"},"assignee":null}`+
			`],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
	})

	issues, err := client.ListIssuesInReview(t.Context(), []string{"t1", "t2"})
	if err != nil {
		t.Fatalf("ListIssuesInReview: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("issues = %+v, want two", issues)
	}
	got := []string{issues[0].Identifier, issues[1].Identifier}
	if !slices.Equal(got, []string{"PAY-1", "OPS-9"}) {
		t.Errorf("issues = %v", got)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want two pages", calls.Load())
	}

	empty, err := client.ListIssuesInReview(t.Context(), nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty query = (%v, %v)", empty, err)
	}
	if calls.Load() != 2 {
		t.Errorf("an empty team list made a request; calls = %d", calls.Load())
	}
}

func TestListIssuesByNumbersBatchesCandidatesAndRestrictsTeams(t *testing.T) {
	t.Parallel()
	client, calls := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		if !strings.Contains(req.Query, `number: { in: $numbers }`) {
			t.Errorf("query has no number filter:\n%s", req.Query)
		}
		numbers, ok := req.Variables["numbers"].([]any)
		if !ok || !slices.Equal(numbers, []any{9.0, 184.0}) {
			t.Errorf("numbers = %#v", req.Variables["numbers"])
		}
		writeResponse(t, w, `{"data":{"issues":{"nodes":[`+
			`{"id":"i1","identifier":"CHAIN-184","number":184,"title":"Wrapper","url":"https://linear/184","updatedAt":"2026-08-17T10:00:00Z","state":{"id":"s1","name":"QA"},"team":{"id":"t1","key":"CHAIN","name":"Chain"},"assignee":null},`+
			`{"id":"foreign","identifier":"OPS-9","number":9,"title":"Foreign","url":"https://linear/9","updatedAt":"2026-08-17T10:00:00Z","state":{"id":"s2","name":"In Review"},"team":{"id":"t2","key":"OPS","name":"Ops"},"assignee":null}`+
			`],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
	})

	issues, err := client.ListIssuesByNumbers(t.Context(), []string{"t1"}, []int{9, 184})
	if err != nil {
		t.Fatalf("ListIssuesByNumbers: %v", err)
	}
	if len(issues) != 1 || issues[0].Identifier != "CHAIN-184" || issues[0].Number != 184 || issues[0].State.Name != "QA" {
		t.Errorf("issues = %+v", issues)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}

	empty, err := client.ListIssuesByNumbers(t.Context(), []string{"t1"}, nil)
	if err != nil || len(empty) != 0 || calls.Load() != 1 {
		t.Errorf("empty lookup = (%v, %v), calls=%d", empty, err, calls.Load())
	}
}

// Every field of Issue must be selected by every query that returns one.
//
// The struct used to carry Title, UpdatedAt and Assignee that neither query
// asked for: they decoded to the zero value on every real response while the
// fixtures here supplied them, so the tests could never notice, and a renderer
// reaching for issue.Title would have shipped an empty string. A field nothing
// populates is not a spare field, it is a trap.
func TestEveryIssueFieldIsSelectedByEveryQuery(t *testing.T) {
	t.Parallel()
	queries := map[string]string{}
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		name := "issues_in_review"
		if strings.Contains(req.Query, "IssuesByNumbers") {
			name = "issues_by_number"
		}
		queries[name] = req.Query
		writeResponse(t, w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
	})
	if _, err := client.ListIssuesInReview(t.Context(), []string{"t1"}); err != nil {
		t.Fatalf("ListIssuesInReview: %v", err)
	}
	if _, err := client.ListIssuesByNumbers(t.Context(), []string{"t1"}, []int{1}); err != nil {
		t.Fatalf("ListIssuesByNumbers: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("captured %d queries, want both", len(queries))
	}

	issue := reflect.TypeFor[linear.Issue]()
	for name, query := range queries {
		// Only the node selection matters; the filter mentions field names too.
		nodes := query[strings.Index(query, "nodes {"):]
		for i := range issue.NumField() {
			field := issue.Field(i).Tag.Get("json")
			if field == "" {
				t.Errorf("%s has no json tag", issue.Field(i).Name)
				continue
			}
			if !strings.Contains(nodes, field) {
				t.Errorf("query %s never selects Issue.%s (json %q), so it always decodes to the zero value",
					name, issue.Field(i).Name, field)
			}
		}
	}
}

// A null viewer is a well-formed 200. doctor's whole use of the call is to name
// the account behind the key, so accepting it printed "authenticated as " and
// passed green on a key that authenticates as nobody.
func TestViewerRejectsNullViewer(t *testing.T) {
	t.Parallel()
	client, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, `{"data":{"viewer":null}}`)
	})
	viewer, err := client.Viewer(t.Context())
	if err == nil || viewer != nil {
		t.Fatalf("Viewer = (%+v, %v), want an error", viewer, err)
	}
	if !strings.Contains(err.Error(), "no viewer") {
		t.Errorf("error = %v", err)
	}
}

// The candidate list is as long as the open merge requests make it, so the
// filter is chunked rather than sent whole.
func TestListIssuesByNumbersChunksLargeCandidateLists(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var batchSizes []int
	requested := map[float64]bool{}

	client, calls := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		numbers, ok := req.Variables["numbers"].([]any)
		if !ok {
			t.Fatalf("numbers = %#v", req.Variables["numbers"])
		}
		mu.Lock()
		batchSizes = append(batchSizes, len(numbers))
		for _, n := range numbers {
			requested[n.(float64)] = true
		}
		mu.Unlock()
		// The same issue is returned by every chunk: the merge must not duplicate it.
		writeResponse(t, w, `{"data":{"issues":{"nodes":[`+
			`{"id":"i1","identifier":"CHAIN-7","number":7,"title":"Shared","url":"https://linear/7","updatedAt":"2026-08-17T10:00:00Z","state":{"id":"s1","name":"QA"},"team":{"id":"t1","key":"CHAIN","name":"Chain"},"assignee":null}`+
			`],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
	})

	numbers := make([]int, 0, 250)
	for i := 1; i <= 250; i++ {
		numbers = append(numbers, i)
	}
	issues, err := client.ListIssuesByNumbers(t.Context(), []string{"t1"}, numbers)
	if err != nil {
		t.Fatalf("ListIssuesByNumbers: %v", err)
	}
	if len(issues) != 1 || issues[0].Identifier != "CHAIN-7" {
		t.Errorf("issues = %+v, want one deduplicated CHAIN-7", issues)
	}
	if calls.Load() != 3 || !slices.Equal(batchSizes, []int{100, 100, 50}) {
		t.Errorf("calls/batch sizes = %d/%v, want 3/[100 100 50]", calls.Load(), batchSizes)
	}
	// Chunking must not lose a candidate: every number still reaches Linear.
	if len(requested) != len(numbers) {
		t.Errorf("%d of %d candidates were sent", len(requested), len(numbers))
	}
}

func TestErrorsDoNotLeakKeyAndFatalResponsesDoNotRetry(t *testing.T) {
	t.Parallel()
	client, calls := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeResponse(t, w, fmt.Sprintf(`{"error":"echo %s"}`, testKey))
	}, func(c *linear.Config) { c.MaxAttempts = 3 })

	_, err := client.Viewer(t.Context())
	if err == nil {
		t.Fatal("Viewer succeeded")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("key leaked in error: %v", err)
	}
	var apiErr *linear.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("error = %T %v", err, err)
	}
	if calls.Load() != 1 {
		t.Errorf("401 calls = %d, want no retry", calls.Load())
	}
}

func TestGraphQLErrorIsFatalAndRedacted(t *testing.T) {
	t.Parallel()
	client, calls := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, fmt.Sprintf(`{"data":{"viewer":null},"errors":[{"message":"bad %s","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`, testKey))
	}, func(c *linear.Config) { c.MaxAttempts = 3 })

	_, err := client.Viewer(t.Context())
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("error = %v", err)
	}
	var apiErr *linear.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "AUTHENTICATION_ERROR" {
		t.Fatalf("error = %T %v", err, err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want no retry", calls.Load())
	}
}

func TestTransientFailureRetriesAndObserverSeesEveryAttempt(t *testing.T) {
	t.Parallel()
	var serverCalls atomic.Int64
	var observed atomic.Int64
	client, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if serverCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeResponse(t, w, `{"data":{"viewer":{"id":"u1","name":"Bot","email":""}}}`)
	}, func(c *linear.Config) {
		c.MaxAttempts = 2
		c.Observer = func(operation string, _ int, _ time.Duration, _ error) {
			if operation != "viewer" {
				t.Errorf("operation = %q", operation)
			}
			observed.Add(1)
		}
	})

	if _, err := client.Viewer(t.Context()); err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	if serverCalls.Load() != 2 || observed.Load() != 2 {
		t.Errorf("server calls/observations = %d/%d, want 2/2", serverCalls.Load(), observed.Load())
	}
}

func TestPaginationWithoutCursorFailsClosed(t *testing.T) {
	t.Parallel()
	client, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":null}}}}`)
	})
	_, err := client.ListIssuesInReview(t.Context(), []string{"t1"})
	if err == nil || !strings.Contains(err.Error(), "no end cursor") {
		t.Fatalf("error = %v", err)
	}
}

func TestInvalidDataIsReportedToObserver(t *testing.T) {
	t.Parallel()
	var observedErrors atomic.Int64
	client, calls := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, `{"data":{"viewer":"not-an-object"}}`)
	}, func(c *linear.Config) {
		c.Observer = func(_ string, _ int, _ time.Duration, err error) {
			if err != nil {
				observedErrors.Add(1)
			}
		}
	})

	_, err := client.Viewer(t.Context())
	if err == nil || !strings.Contains(err.Error(), "invalid data payload") {
		t.Fatalf("error = %v", err)
	}
	if calls.Load() != 1 || observedErrors.Load() != 1 {
		t.Errorf("calls/observed errors = %d/%d, want 1/1", calls.Load(), observedErrors.Load())
	}
}
