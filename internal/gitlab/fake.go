package gitlab

import (
	"context"
	"fmt"
	"sync"
)

// FakeClient is an in-memory API implementation for tests. It serves fixture
// data and records write operations so tests can assert safety invariants
// (e.g. nothing is published without an explicit call).
type FakeClient struct {
	mu sync.Mutex

	Me          *User
	OpenMRs     map[string][]MergeRequest // projectKey -> open MRs
	Projects    map[string]*Project
	MRs         map[string]*MergeRequest
	Diffs       map[string][]MergeRequestDiff
	Versions    map[string][]MergeRequestVersion
	Discussions map[string][]Discussion
	Pipelines   map[string][]Pipeline
	Commits     map[string][]Commit
	Approvals   map[string]*Approvals
	RawFiles    map[string][]byte // "path@ref" -> content

	// Recorded writes.
	CreatedNotes       []string
	CreatedDiscussions []string
	// WriteLog records write calls in order ("discussion", "note", …) so tests
	// can assert publication ordering — the summary note carrying the review
	// marker must always be last.
	WriteLog []string

	nextNoteID int64
}

// NewFake returns an empty fake with initialized maps.
func NewFake() *FakeClient {
	return &FakeClient{
		OpenMRs:     map[string][]MergeRequest{},
		Projects:    map[string]*Project{},
		MRs:         map[string]*MergeRequest{},
		Diffs:       map[string][]MergeRequestDiff{},
		Versions:    map[string][]MergeRequestVersion{},
		Discussions: map[string][]Discussion{},
		Pipelines:   map[string][]Pipeline{},
		Commits:     map[string][]Commit{},
		Approvals:   map[string]*Approvals{},
		RawFiles:    map[string][]byte{},
	}
}

func key(projectKey string, iid int64) string { return fmt.Sprintf("%s/%d", projectKey, iid) }

func (f *FakeClient) CurrentUser(ctx context.Context) (*User, error) {
	if f.Me == nil {
		return &User{ID: 1, Username: "me"}, nil
	}
	return f.Me, nil
}

func (f *FakeClient) ListOpenMRs(ctx context.Context, projectKey string) ([]MergeRequest, error) {
	return f.OpenMRs[projectKey], nil
}

func (f *FakeClient) GetProject(ctx context.Context, projectKey string) (*Project, error) {
	if p, ok := f.Projects[projectKey]; ok {
		return p, nil
	}
	return nil, &APIError{Status: 404, Path: "/projects/" + projectKey}
}

func (f *FakeClient) GetMR(ctx context.Context, projectKey string, iid int64) (*MergeRequest, error) {
	if m, ok := f.MRs[key(projectKey, iid)]; ok {
		return m, nil
	}
	return nil, &APIError{Status: 404, Path: mrPath(projectKey, iid, "")}
}

func (f *FakeClient) ListMRDiffs(ctx context.Context, projectKey string, iid int64) ([]MergeRequestDiff, error) {
	return f.Diffs[key(projectKey, iid)], nil
}

func (f *FakeClient) ListMRVersions(ctx context.Context, projectKey string, iid int64) ([]MergeRequestVersion, error) {
	return f.Versions[key(projectKey, iid)], nil
}

func (f *FakeClient) ListMRDiscussions(ctx context.Context, projectKey string, iid int64) ([]Discussion, error) {
	return f.Discussions[key(projectKey, iid)], nil
}

func (f *FakeClient) ListMRPipelines(ctx context.Context, projectKey string, iid int64) ([]Pipeline, error) {
	return f.Pipelines[key(projectKey, iid)], nil
}

func (f *FakeClient) ListMRCommits(ctx context.Context, projectKey string, iid int64) ([]Commit, error) {
	return f.Commits[key(projectKey, iid)], nil
}

func (f *FakeClient) GetMRApprovals(ctx context.Context, projectKey string, iid int64) (*Approvals, error) {
	if a, ok := f.Approvals[key(projectKey, iid)]; ok {
		return a, nil
	}
	return &Approvals{IID: iid}, nil
}

func (f *FakeClient) GetRawFile(ctx context.Context, projectKey, filePath, ref string) ([]byte, error) {
	if c, ok := f.RawFiles[filePath+"@"+ref]; ok {
		return c, nil
	}
	return nil, &APIError{Status: 404, Path: "/projects/" + projectKey + "/repository/files/" + filePath}
}

func (f *FakeClient) CreateMRNote(ctx context.Context, projectKey string, iid int64, body string) (*Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreatedNotes = append(f.CreatedNotes, body)
	f.WriteLog = append(f.WriteLog, "note")
	f.nextNoteID++
	return &Note{ID: f.nextNoteID, Body: body}, nil
}

func (f *FakeClient) CreateDiscussion(ctx context.Context, projectKey string, iid int64, body string, pos *Position) (*Discussion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreatedDiscussions = append(f.CreatedDiscussions, body)
	f.WriteLog = append(f.WriteLog, "discussion")
	f.nextNoteID++
	return &Discussion{
		ID:    fmt.Sprintf("disc-%d", len(f.CreatedDiscussions)),
		Notes: []Note{{ID: f.nextNoteID, Body: body, Position: pos, Resolvable: pos != nil}},
	}, nil
}

var _ API = (*FakeClient)(nil)

// FakeGraphQL is an in-memory GraphQLAPI for tests, including the degradation
// path: set Err to gitlab.ErrGraphQLUnsupported to exercise the REST fallback.
type FakeGraphQL struct {
	mu     sync.Mutex
	States map[int64][]ReviewerState
	Err    error
	Calls  int
}

var _ GraphQLAPI = (*FakeGraphQL)(nil)

func (f *FakeGraphQL) ReviewStates(ctx context.Context, fullPath string, iids []int64) (map[int64][]ReviewerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if f.Err != nil {
		return nil, f.Err
	}
	out := make(map[int64][]ReviewerState, len(iids))
	for _, iid := range iids {
		if s, ok := f.States[iid]; ok {
			out[iid] = s
		}
	}
	return out, nil
}

// CallCount reports how many times ReviewStates was called; one call per
// project per run is the budget.
func (f *FakeGraphQL) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls
}
