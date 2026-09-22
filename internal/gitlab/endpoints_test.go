package gitlab

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// jsonServer answers every request with the body registered for its path.
// Lookups use the escaped path: GitLab project keys are url-encoded paths and
// the %2F separators must survive to the wire.
func jsonServer(t *testing.T, bodies map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.EscapedPath()]
		if !ok {
			t.Errorf("unexpected path %q", r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The team service classifies conflicts and pipelines from the MR detail
// response, so those fields must survive decoding.
func TestGetMRDecodesMergeabilityAndHeadPipeline(t *testing.T) {
	srv := jsonServer(t, map[string]string{
		"/api/v4/projects/group%2Frepo/merge_requests/7": `{
			"id": 100, "iid": 7, "project_id": 42, "state": "opened",
			"sha": "abc123",
			"has_conflicts": true,
			"merge_status": "cannot_be_merged",
			"detailed_merge_status": "conflict",
			"reviewers": [{"id": 3, "username": "bob"}],
			"assignees": [{"id": 4, "username": "carol"}],
			"head_pipeline": {
				"id": 9001, "status": "failed", "sha": "abc123", "ref": "feature/x",
				"source": "merge_request_event",
				"web_url": "https://gitlab.example.com/group/repo/-/pipelines/9001",
				"created_at": "2026-08-13T09:00:00Z", "updated_at": "2026-08-13T09:05:00Z"
			}
		}`,
	})

	mr, err := newTestClient(t, srv).GetMR(t.Context(), "group%2Frepo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !mr.HasConflicts {
		t.Error("has_conflicts not decoded")
	}
	if mr.MergeStatus != "cannot_be_merged" {
		t.Errorf("merge_status = %q", mr.MergeStatus)
	}
	if mr.DetailedMergeStatus != "conflict" {
		t.Errorf("detailed_merge_status = %q", mr.DetailedMergeStatus)
	}
	if len(mr.Assignees) != 1 || mr.Assignees[0].Username != "carol" {
		t.Errorf("assignees = %+v", mr.Assignees)
	}
	if mr.HeadPipeline == nil {
		t.Fatal("head_pipeline not decoded")
	}
	if mr.HeadPipeline.Status != "failed" || mr.HeadPipeline.SHA != "abc123" {
		t.Errorf("head_pipeline = %+v", mr.HeadPipeline)
	}
	if mr.HeadPipeline.Ref != "feature/x" || mr.HeadPipeline.Source != "merge_request_event" {
		t.Errorf("pipeline ref/source = %+v", mr.HeadPipeline)
	}
	if mr.HeadPipeline.CreatedAt == "" || mr.HeadPipeline.UpdatedAt == "" {
		t.Errorf("pipeline timestamps = %+v", mr.HeadPipeline)
	}
}

// A token without pipeline visibility gets no head_pipeline at all: nil must
// mean "unknown", never "no pipeline".
func TestGetMRWithoutHeadPipeline(t *testing.T) {
	srv := jsonServer(t, map[string]string{
		"/api/v4/projects/1/merge_requests/2": `{"iid": 2, "state": "opened", "merge_status": "unchecked"}`,
	})
	mr, err := newTestClient(t, srv).GetMR(t.Context(), "1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if mr.HeadPipeline != nil {
		t.Errorf("head_pipeline = %+v, want nil", mr.HeadPipeline)
	}
	if mr.HasConflicts {
		t.Error("has_conflicts must default to false when mergeability is unchecked")
	}
}

func TestListOpenMRsQueriesProjectScope(t *testing.T) {
	var gotPath, gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotState = r.URL.Query().Get("state")
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		_, _ = w.Write([]byte(`[{"iid": 1}, {"iid": 2}]`))
	}))
	defer srv.Close()

	mrs, err := newTestClient(t, srv).ListOpenMRs(t.Context(), "group%2Frepo")
	if err != nil {
		t.Fatal(err)
	}
	if len(mrs) != 2 {
		t.Fatalf("got %d MRs, want 2", len(mrs))
	}
	if gotPath != "/api/v4/projects/group%2Frepo/merge_requests" {
		t.Errorf("path = %q", gotPath)
	}
	if gotState != "opened" {
		t.Errorf("state = %q, want opened", gotState)
	}
}

func TestGetMRApprovals(t *testing.T) {
	srv := jsonServer(t, map[string]string{
		"/api/v4/projects/42/merge_requests/7/approvals": `{
			"id": 100, "iid": 7, "project_id": 42,
			"approved": true, "approvals_required": 2, "approvals_left": 0,
			"approved_by": [
				{"user": {"id": 3, "username": "bob", "name": "Bob"}},
				{"user": {"id": 4, "username": "carol", "name": "Carol"}}
			]
		}`,
	})

	a, err := newTestClient(t, srv).GetMRApprovals(t.Context(), "42", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Approved || a.ApprovalsRequired != 2 || a.ApprovalsLeft != 0 {
		t.Errorf("approvals = %+v", a)
	}
	users := a.Approvers()
	if len(users) != 2 || users[0].Username != "bob" || users[1].Username != "carol" {
		t.Errorf("approvers = %+v", users)
	}
}

func TestListMRVersionsCarriesPushTime(t *testing.T) {
	srv := jsonServer(t, map[string]string{
		"/api/v4/projects/42/merge_requests/7/versions": `[
			{"id": 2, "head_commit_sha": "bbb", "base_commit_sha": "base", "start_commit_sha": "start", "created_at": "2026-08-13T09:12:44Z"},
			{"id": 1, "head_commit_sha": "aaa", "base_commit_sha": "base", "start_commit_sha": "start", "created_at": "2026-08-12T10:00:00Z"}
		]`,
	})

	vs, err := newTestClient(t, srv).ListMRVersions(t.Context(), "42", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 {
		t.Fatalf("got %d versions", len(vs))
	}
	// GitLab returns versions newest first; the first one is the last push.
	got, err := time.Parse(time.RFC3339, vs[0].CreatedAt)
	if err != nil {
		t.Fatalf("created_at not RFC3339: %v", err)
	}
	if want := time.Date(2026, 8, 13, 9, 12, 44, 0, time.UTC); !got.Equal(want) {
		t.Errorf("last push = %s, want %s", got, want)
	}
}

// The read endpoints the team service runs on. What matters is that the path,
// the query and the decoding are pinned — a wrong path or a dropped field only
// shows up against a live GitLab.
func TestReadEndpoints(t *testing.T) {
	tests := []struct {
		name      string
		respBody  string
		wantPath  string
		wantQuery map[string]string
		call      func(t *testing.T, c *Client)
	}{
		{
			name: "GetProject",
			respBody: `{"id":42,"path_with_namespace":"group/repo","default_branch":"main",
				"http_url_to_repo":"https://gitlab.example.com/group/repo.git",
				"web_url":"https://gitlab.example.com/group/repo"}`,
			wantPath: "/api/v4/projects/group%2Frepo",
			call: func(t *testing.T, c *Client) {
				p, err := c.GetProject(t.Context(), "group%2Frepo")
				if err != nil {
					t.Fatal(err)
				}
				if p.ID != 42 || p.PathWithNamespace != "group/repo" || p.DefaultBranch != "main" {
					t.Errorf("project = %+v", p)
				}
				if p.HTTPURLToRepo == "" || p.WebURL == "" {
					t.Errorf("clone/web url not decoded: %+v", p)
				}
			},
		},
		{
			// §8: the one-shot fallback for MRs whose head_pipeline the token
			// may not see. If this breaks, the digest silently stops reporting
			// failed pipelines on exactly the instances that need the fallback.
			name: "ListMRPipelines",
			respBody: `[{"id":9001,"status":"failed","sha":"abc123","ref":"feature/x",
				"source":"merge_request_event",
				"web_url":"https://gitlab.example.com/group/repo/-/pipelines/9001",
				"created_at":"2026-08-13T09:00:00Z","updated_at":"2026-08-13T09:05:00Z"},
				{"id":9000,"status":"success","sha":"old999"}]`,
			wantPath:  "/api/v4/projects/42/merge_requests/7/pipelines",
			wantQuery: map[string]string{"per_page": "100"},
			call: func(t *testing.T, c *Client) {
				ps, err := c.ListMRPipelines(t.Context(), "42", 7)
				if err != nil {
					t.Fatal(err)
				}
				if len(ps) != 2 {
					t.Fatalf("got %d pipelines, want 2", len(ps))
				}
				if ps[0].Status != "failed" || ps[0].SHA != "abc123" {
					t.Errorf("pipeline = %+v", ps[0])
				}
				// The digest links straight to the failed pipeline, not the MR.
				if ps[0].WebURL != "https://gitlab.example.com/group/repo/-/pipelines/9001" {
					t.Errorf("web_url = %q", ps[0].WebURL)
				}
				if ps[0].Ref != "feature/x" || ps[0].Source != "merge_request_event" {
					t.Errorf("ref/source = %+v", ps[0])
				}
			},
		},
		{
			name: "ListMRCommits",
			respBody: `[{"id":"abc123def","short_id":"abc123d","title":"fix: leak",
				"message":"fix: leak\n\nlong body","author_name":"Bob","created_at":"2026-08-13T09:00:00Z"}]`,
			wantPath:  "/api/v4/projects/42/merge_requests/7/commits",
			wantQuery: map[string]string{"per_page": "100"},
			call: func(t *testing.T, c *Client) {
				cs, err := c.ListMRCommits(t.Context(), "42", 7)
				if err != nil {
					t.Fatal(err)
				}
				if len(cs) != 1 {
					t.Fatalf("got %d commits, want 1", len(cs))
				}
				if cs[0].ID != "abc123def" || cs[0].ShortID != "abc123d" {
					t.Errorf("commit = %+v", cs[0])
				}
				if cs[0].Title != "fix: leak" || cs[0].AuthorName != "Bob" {
					t.Errorf("commit = %+v", cs[0])
				}
			},
		},
		{
			// The only endpoint that bypasses do(): the body is file content,
			// not JSON, and the file path is escaped into the URL path.
			name:      "GetRawFile",
			respBody:  "package main\n\nfunc main() {}\n",
			wantPath:  "/api/v4/projects/42/repository/files/internal%2Fapp%2Fmain.go/raw",
			wantQuery: map[string]string{"ref": "abc123"},
			call: func(t *testing.T, c *Client) {
				content, err := c.GetRawFile(t.Context(), "42", "internal/app/main.go", "abc123")
				if err != nil {
					t.Fatal(err)
				}
				if string(content) != "package main\n\nfunc main() {}\n" {
					t.Errorf("content = %q", content)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				gotPath  string
				gotQuery url.Values
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.EscapedPath()
				gotQuery = r.URL.Query()
				_, _ = w.Write([]byte(tc.respBody))
			}))
			defer srv.Close()

			tc.call(t, newTestClient(t, srv))

			if gotPath != tc.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tc.wantPath)
			}
			for k, want := range tc.wantQuery {
				if got := gotQuery.Get(k); got != want {
					t.Errorf("query %s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestListMRDiscussionsDecodesThreadFields(t *testing.T) {
	srv := jsonServer(t, map[string]string{
		"/api/v4/projects/42/merge_requests/7/discussions": `[
			{"id": "d1", "individual_note": true, "notes": [
				{"id": 1, "body": "just a comment", "resolvable": false, "resolved": false,
				 "created_at": "2026-08-13T08:00:00Z", "author": {"id": 5, "username": "dave"}}
			]},
			{"id": "d2", "individual_note": false, "notes": [
				{"id": 2, "body": "please fix", "resolvable": true, "resolved": false,
				 "created_at": "2026-08-13T09:00:00Z", "author": {"id": 3, "username": "bob"}},
				{"id": 3, "body": "ok", "resolvable": true, "resolved": false,
				 "created_at": "2026-08-13T09:30:00Z", "author": {"id": 9, "username": "author"}}
			]}
		]`,
	})

	ds, err := newTestClient(t, srv).ListMRDiscussions(t.Context(), "42", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 {
		t.Fatalf("got %d discussions", len(ds))
	}
	if !ds[0].IndividualNote {
		t.Error("individual_note not decoded on the standalone comment")
	}
	if ds[0].Notes[0].Resolvable {
		t.Error("a plain comment must not be resolvable")
	}
	if ds[1].IndividualNote {
		t.Error("a thread must not be flagged individual_note")
	}
	if !ds[1].Notes[0].Resolvable || ds[1].Notes[0].Resolved {
		t.Errorf("thread head = %+v, want resolvable and unresolved", ds[1].Notes[0])
	}
	if ds[1].Notes[1].CreatedAt != "2026-08-13T09:30:00Z" {
		t.Errorf("note created_at = %q", ds[1].Notes[1].CreatedAt)
	}
	if ds[1].Notes[0].Author.Username != "bob" {
		t.Errorf("note author = %+v", ds[1].Notes[0].Author)
	}
}
