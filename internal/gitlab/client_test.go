package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	mrs, err := newTestClient(t, srv).ListReviewerMRs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(mrs) != 3 {
		t.Fatalf("want 3 MRs across pages, got %d", len(mrs))
	}
}

func TestClientDraftNotePositionJSON(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(DraftNote{ID: 99})
	}))
	defer srv.Close()

	newLine := 42
	pos := &Position{
		BaseSHA: "b", HeadSHA: "h", StartSHA: "s",
		PositionType: "text", OldPath: "f.go", NewPath: "f.go", NewLine: &newLine,
	}
	if _, err := newTestClient(t, srv).CreateDraftNote(t.Context(), "1", 5, "hello", pos); err != nil {
		t.Fatal(err)
	}
	p, ok := body["position"].(map[string]any)
	if !ok {
		t.Fatalf("position missing in body: %v", body)
	}
	if p["position_type"] != "text" || p["new_path"] != "f.go" {
		t.Errorf("position fields wrong: %v", p)
	}
	if p["new_line"] != float64(42) {
		t.Errorf("new_line = %v, want 42", p["new_line"])
	}
	if _, hasOld := p["old_line"]; hasOld {
		t.Errorf("old_line should be omitted for an added line: %v", p)
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
