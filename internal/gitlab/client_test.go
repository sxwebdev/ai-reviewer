package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{Host: srv.URL, Token: "glpat-testtoken1234567890", Timeout: 5 * time.Second, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientSendsTokenAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "glpat-testtoken1234567890" {
			t.Errorf("missing/invalid token header: %q", got)
		}
		if r.URL.Path != "/api/v4/user" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(User{ID: 7, Username: "alice"})
	}))
	defer srv.Close()

	u, err := newTestClient(t, srv).CurrentUser(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "alice" || u.ID != 7 {
		t.Errorf("got %+v", u)
	}
}

func TestClientPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch page {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			_ = json.NewEncoder(w).Encode([]MergeRequest{{IID: 1}, {IID: 2}})
		case "2":
			w.Header().Set("X-Next-Page", "")
			_ = json.NewEncoder(w).Encode([]MergeRequest{{IID: 3}})
		default:
			t.Errorf("unexpected page %q", page)
		}
	}))
	defer srv.Close()

	mrs, err := newTestClient(t, srv).ListOpenMRs(t.Context(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(mrs) != 3 {
		t.Fatalf("want 3 MRs across pages, got %d", len(mrs))
	}
}

// TestClientPaginationIsBounded covers the endpoint that never stops handing
// out a cursor. Every page is appended into one slice and a GitLab response can
// be 32 MiB, so an unbounded follow walks a worker's RSS into the pod limit and
// OOMKills the replica mid-review. The failure must be an error, not a
// truncation: a short list that looks complete would silently stop reviewing
// everything past the cut.
func TestClientPaginationIsBounded(t *testing.T) {
	var pages atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pages.Add(1)
		w.Header().Set("X-Next-Page", strconv.Itoa(p+1)) // always one more
		_ = json.NewEncoder(w).Encode([]MergeRequest{{IID: int64(p)}})
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).ListOpenMRs(t.Context(), "1")
	if err == nil {
		t.Fatal("an endless cursor was followed to completion")
	}
	if !strings.Contains(err.Error(), "pagination did not terminate") {
		t.Errorf("err = %v, want it to name the pagination bound", err)
	}
	if got := pages.Load(); got != maxListPages {
		t.Errorf("requested %d pages, want exactly %d", got, maxListPages)
	}
}

func TestClientNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 Not found"}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).GetMR(t.Context(), "1", 999)
	if !IsNotFound(err) {
		t.Errorf("expected IsNotFound, got %v", err)
	}
}

func TestClientRetriesOn500(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(User{ID: 1, Username: "ok"})
	}))
	defer srv.Close()

	u, err := newTestClient(t, srv).CurrentUser(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "ok" {
		t.Errorf("got %+v", u)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("expected 3 attempts (2 failures + success), got %d", n)
	}
}

func TestClientDoesNotRetry4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad"}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).GetMR(t.Context(), "1", 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("4xx must not retry, got %d calls", n)
	}
}

func TestRetryAfterDelay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		header string
		want   time.Duration
		wantOK bool
	}{
		{name: "absent", header: "", wantOK: false},
		{name: "delay seconds", header: "120", want: 2 * time.Minute, wantOK: true},
		{name: "padded delay seconds", header: "  5 ", want: 5 * time.Second, wantOK: true},
		{name: "zero seconds", header: "0", wantOK: false},
		{name: "negative seconds", header: "-3", wantOK: false},
		{name: "http date", header: "Thu, 13 Aug 2026 09:01:30 GMT", want: 90 * time.Second, wantOK: true},
		{name: "http date in the past", header: "Thu, 13 Aug 2026 08:00:00 GMT", wantOK: false},
		{name: "garbage", header: "soon please", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := retryAfterDelay(tc.header, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (delay %s)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Errorf("delay = %s, want %s", got, tc.want)
			}
		})
	}
}

// A 429 must be paced by the server's own Retry-After rather than by the fixed
// backoff ladder, which starts at 500ms and would hammer a rate limiter.
func TestClientHonoursRetryAfterOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(User{ID: 1, Username: "ok"})
	}))
	defer srv.Close()

	start := time.Now()
	u, err := newTestClient(t, srv).CurrentUser(t.Context())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "ok" {
		t.Errorf("got %+v", u)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("expected 2 attempts (429 + success), got %d", n)
	}
	if elapsed < time.Second {
		t.Errorf("retried after %s, want at least the 1s Retry-After", elapsed)
	}
}

// Retry-After above MaxRetryAfter must not park a worker for the full period.
func TestClientCapsRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c, err := New(Config{
		Host: srv.URL, Token: "t", Timeout: 5 * time.Second,
		MaxAttempts: 2, MaxRetryAfter: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.CurrentUser(t.Context()); err == nil {
		t.Fatal("expected the rate limit to surface as an error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s: Retry-After was not capped", elapsed)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("expected 2 attempts, got %d", n)
	}
}

func TestClientObserverSeesEveryAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode([]MergeRequestDiff{})
	}))
	defer srv.Close()

	type observation struct {
		endpoint, method string
		status           int
		failed           bool
	}
	var (
		mu   sync.Mutex
		seen []observation
	)
	c, err := New(Config{
		Host: srv.URL, Token: "t", Timeout: 5 * time.Second, MaxAttempts: 3,
		Observer: func(endpoint, method string, status int, err error) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, observation{endpoint, method, status, err != nil})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListMRDiffs(t.Context(), "group%2Frepo", 7); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("want one observation per attempt, got %d: %+v", len(seen), seen)
	}
	// The label must be a template, not the concrete path: one time series per
	// endpoint, not per merge request.
	const want = "/projects/:key/merge_requests/:id/diffs"
	for i, o := range seen {
		if o.endpoint != want {
			t.Errorf("observation %d endpoint = %q, want %q", i, o.endpoint, want)
		}
		if o.method != "GET" {
			t.Errorf("observation %d method = %q", i, o.method)
		}
	}
	if seen[0].status != 500 || !seen[0].failed {
		t.Errorf("first observation should report the failure: %+v", seen[0])
	}
	if seen[1].status != 200 || seen[1].failed {
		t.Errorf("second observation should report success: %+v", seen[1])
	}
}

func TestEndpointLabel(t *testing.T) {
	t.Parallel()
	tests := []struct{ path, want string }{
		{"/user", "/user"},
		{"/merge_requests", "/merge_requests"},
		{"/projects/42", "/projects/:key"},
		{"/projects/group%2Fsub%2Frepo/merge_requests", "/projects/:key/merge_requests"},
		{"/projects/42/merge_requests/7/discussions", "/projects/:key/merge_requests/:id/discussions"},
		{"/projects/42/merge_requests/7/approvals", "/projects/:key/merge_requests/:id/approvals"},
		{"/projects/42/merge_requests/7/draft_notes/13/publish", "/projects/:key/merge_requests/:id/draft_notes/:id/publish"},
		{"/projects/42/repository/files/cmd%2Fmain.go/raw", "/projects/:key/repository/files/:path/raw"},
		{"/graphql", "/graphql"},
	}
	for _, tc := range tests {
		if got := endpointLabel(tc.path); got != tc.want {
			t.Errorf("endpointLabel(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
