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
	ReviewerMRs []MergeRequest
	Projects    map[string]*Project
	MRs         map[string]*MergeRequest
	Diffs       map[string][]MergeRequestDiff
	Versions    map[string][]MergeRequestVersion
	Discussions map[string][]Discussion
	Pipelines   map[string][]Pipeline
	Commits     map[string][]Commit
	RawFiles    map[string][]byte // "path@ref" -> content

	// Recorded writes.
	CreatedDrafts      []DraftNote
	CreatedNotes       []string
	CreatedDiscussions []string
	PublishedDraftIDs  []int64
	BulkPublished      int

	nextDraftID int64
}

// NewFake returns an empty fake with initialized maps.
func NewFake() *FakeClient {
	return &FakeClient{
		Projects:    map[string]*Project{},
		MRs:         map[string]*MergeRequest{},
		Diffs:       map[string][]MergeRequestDiff{},
		Versions:    map[string][]MergeRequestVersion{},
		Discussions: map[string][]Discussion{},
		Pipelines:   map[string][]Pipeline{},
		Commits:     map[string][]Commit{},
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

func (f *FakeClient) ListReviewerMRs(ctx context.Context) ([]MergeRequest, error) {
	return f.ReviewerMRs, nil
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

func (f *FakeClient) GetRawFile(ctx context.Context, projectKey, filePath, ref string) ([]byte, error) {
	if c, ok := f.RawFiles[filePath+"@"+ref]; ok {
		return c, nil
	}
	return nil, &APIError{Status: 404, Path: "/projects/" + projectKey + "/repository/files/" + filePath}
}

func (f *FakeClient) CreateDraftNote(ctx context.Context, projectKey string, iid int64, note string, pos *Position) (*DraftNote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextDraftID++
	dn := DraftNote{ID: f.nextDraftID, Note: note, Position: pos}
	f.CreatedDrafts = append(f.CreatedDrafts, dn)
	return &dn, nil
}

func (f *FakeClient) ListDraftNotes(ctx context.Context, projectKey string, iid int64) ([]DraftNote, error) {
	return f.CreatedDrafts, nil
}

func (f *FakeClient) PublishDraftNote(ctx context.Context, projectKey string, iid, draftID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PublishedDraftIDs = append(f.PublishedDraftIDs, draftID)
	return nil
}

func (f *FakeClient) BulkPublishDraftNotes(ctx context.Context, projectKey string, iid int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.BulkPublished++
	return nil
}

func (f *FakeClient) CreateMRNote(ctx context.Context, projectKey string, iid int64, body string) (*Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreatedNotes = append(f.CreatedNotes, body)
	return &Note{ID: int64(len(f.CreatedNotes)), Body: body}, nil
}

func (f *FakeClient) CreateDiscussion(ctx context.Context, projectKey string, iid int64, body string, pos *Position) (*Discussion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CreatedDiscussions = append(f.CreatedDiscussions, body)
	return &Discussion{ID: fmt.Sprintf("disc-%d", len(f.CreatedDiscussions))}, nil
}

var _ API = (*FakeClient)(nil)
