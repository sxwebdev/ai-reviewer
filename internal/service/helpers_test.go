package service

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/linear"
	"github.com/sxwebdev/ai-reviewer/internal/llm"
	"github.com/sxwebdev/ai-reviewer/internal/match"
	"github.com/sxwebdev/ai-reviewer/internal/models"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

// Fixed clock so backoff arithmetic and digest dates are deterministic.
var testNow = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// sampleDiff adds one line (new line 3) to a four-line file. It is the smallest
// diff a finding can be anchored to, which keeps the fixtures readable.
const sampleDiff = "@@ -1,3 +1,4 @@\n package main\n \n+func added() {}\n // tail\n"

const (
	testProjectID = int64(7)
	testMRIID     = int64(481)
	testHeadSHA   = "aaaa111"
	testTeam      = "payments"
)

func testProject() *gitlab.Project {
	return &gitlab.Project{
		ID:                testProjectID,
		PathWithNamespace: "backend/payments",
		DefaultBranch:     "main",
		WebURL:            "https://gitlab.example.com/backend/payments",
	}
}

func testTeamConfig() domain.Team {
	return domain.Team{
		Name:         testTeam,
		SlackChannel: "C123",
		AIReview:     true,
		Repositories: []string{"backend/payments"},
	}
}

// mrOption mutates a fixture merge request.
type mrOption func(*gitlab.MergeRequest)

func testMR(iid int64, opts ...mrOption) *gitlab.MergeRequest {
	mr := &gitlab.MergeRequest{
		ID:           1000 + iid,
		IID:          iid,
		ProjectID:    testProjectID,
		Title:        "Add payment retries",
		State:        "opened",
		WebURL:       fmt.Sprintf("https://gitlab.example.com/backend/payments/-/merge_requests/%d", iid),
		Author:       gitlab.User{ID: 1, Username: "author", Name: "Ann Author"},
		SourceBranch: "feature",
		TargetBranch: "main",
		SHA:          testHeadSHA,
		DiffRefs: gitlab.DiffRefs{
			BaseSHA: "base000", HeadSHA: testHeadSHA, StartSHA: "start00",
		},
		MergeStatus:         "can_be_merged",
		DetailedMergeStatus: "mergeable",
		CreatedAt:           "2026-08-10T09:00:00.000Z",
		UpdatedAt:           "2026-08-13T09:00:00.000Z",
	}
	for _, o := range opts {
		o(mr)
	}
	return mr
}

func withHeadSHA(sha string) mrOption {
	return func(mr *gitlab.MergeRequest) {
		mr.SHA = sha
		mr.DiffRefs.HeadSHA = sha
	}
}

func withReviewers(users ...gitlab.User) mrOption {
	return func(mr *gitlab.MergeRequest) { mr.Reviewers = users }
}

func withDraft() mrOption {
	return func(mr *gitlab.MergeRequest) { mr.Draft = true }
}

func withState(state string) mrOption {
	return func(mr *gitlab.MergeRequest) { mr.State = state }
}

func withHeadPipeline(p *gitlab.Pipeline) mrOption {
	return func(mr *gitlab.MergeRequest) { mr.HeadPipeline = p }
}

func withConflicts(has bool, detailed string) mrOption {
	return func(mr *gitlab.MergeRequest) {
		mr.HasConflicts = has
		mr.DetailedMergeStatus = detailed
	}
}

// fakeKey mirrors the key gitlab.FakeClient stores per-MR fixtures under.
func fakeKey(pk string, iid int64) string { return fmt.Sprintf("%s/%d", pk, iid) }

// seedGitLab registers a project and a merge request under every project key
// the service may address them by: the numeric id (used once the project is
// resolved) and the URL-encoded path (used to resolve it).
func seedGitLab(f *gitlab.FakeClient, proj *gitlab.Project, mrs ...*gitlab.MergeRequest) {
	numeric := strconv.FormatInt(proj.ID, 10)
	escaped := url.PathEscape(proj.PathWithNamespace)
	f.Projects[numeric] = proj
	f.Projects[escaped] = proj

	var open []gitlab.MergeRequest
	for _, mr := range mrs {
		open = append(open, *mr)
		for _, pk := range []string{numeric, escaped} {
			k := fakeKey(pk, mr.IID)
			f.MRs[k] = mr
			if _, ok := f.Diffs[k]; !ok {
				f.Diffs[k] = []gitlab.MergeRequestDiff{
					{OldPath: "main.go", NewPath: "main.go", Diff: sampleDiff},
				}
			}
			if _, ok := f.Versions[k]; !ok {
				f.Versions[k] = []gitlab.MergeRequestVersion{
					{ID: 1, HeadCommitSHA: mr.DiffRefs.HeadSHA, CreatedAt: "2026-08-13T09:00:00.000Z"},
				}
			}
		}
	}
	for _, pk := range []string{numeric, escaped} {
		f.OpenMRs[pk] = open
	}
}

// setDiscussions installs discussions under both project keys.
func setDiscussions(f *gitlab.FakeClient, proj *gitlab.Project, iid int64, d []gitlab.Discussion) {
	for _, pk := range []string{strconv.FormatInt(proj.ID, 10), url.PathEscape(proj.PathWithNamespace)} {
		f.Discussions[fakeKey(pk, iid)] = d
	}
}

// setDiffs installs raw diffs under both project keys.
func setDiffs(f *gitlab.FakeClient, proj *gitlab.Project, iid int64, d []gitlab.MergeRequestDiff) {
	for _, pk := range []string{strconv.FormatInt(proj.ID, 10), url.PathEscape(proj.PathWithNamespace)} {
		f.Diffs[fakeKey(pk, iid)] = d
	}
}

// setPipelines installs the /pipelines fallback fixture under both keys.
func setPipelines(f *gitlab.FakeClient, proj *gitlab.Project, iid int64, p []gitlab.Pipeline) {
	for _, pk := range []string{strconv.FormatInt(proj.ID, 10), url.PathEscape(proj.PathWithNamespace)} {
		f.Pipelines[fakeKey(pk, iid)] = p
	}
}

// setApprovals installs approvals under both keys.
func setApprovals(f *gitlab.FakeClient, proj *gitlab.Project, iid int64, a *gitlab.Approvals) {
	for _, pk := range []string{strconv.FormatInt(proj.ID, 10), url.PathEscape(proj.PathWithNamespace)} {
		f.Approvals[fakeKey(pk, iid)] = a
	}
}

// setVersions installs diff versions under both keys.
func setVersions(f *gitlab.FakeClient, proj *gitlab.Project, iid int64, v []gitlab.MergeRequestVersion) {
	for _, pk := range []string{strconv.FormatInt(proj.ID, 10), url.PathEscape(proj.PathWithNamespace)} {
		f.Versions[fakeKey(pk, iid)] = v
	}
}

// countingGitLab wraps the fake so tests can count calls to one endpoint and
// inject per-call failures without reimplementing the whole API.
type countingGitLab struct {
	*gitlab.FakeClient

	pipelineCalls   map[int64]int
	discussionCalls int
	versionCalls    int
	approvalCalls   int
	openMRCalls     int

	failProject   map[string]error
	failMR        map[int64]error
	failDiffs     error
	failOpenMRs   error
	failApprovals error
}

func newCountingGitLab(f *gitlab.FakeClient) *countingGitLab {
	return &countingGitLab{
		FakeClient:    f,
		pipelineCalls: map[int64]int{},
		failProject:   map[string]error{},
		failMR:        map[int64]error{},
	}
}

func (c *countingGitLab) GetProject(ctx context.Context, pk string) (*gitlab.Project, error) {
	if err, ok := c.failProject[pk]; ok {
		return nil, err
	}
	return c.FakeClient.GetProject(ctx, pk)
}

func (c *countingGitLab) ListOpenMRs(ctx context.Context, pk string) ([]gitlab.MergeRequest, error) {
	c.openMRCalls++
	if c.failOpenMRs != nil {
		return nil, c.failOpenMRs
	}
	return c.FakeClient.ListOpenMRs(ctx, pk)
}

func (c *countingGitLab) GetMR(ctx context.Context, pk string, iid int64) (*gitlab.MergeRequest, error) {
	if err, ok := c.failMR[iid]; ok {
		return nil, err
	}
	return c.FakeClient.GetMR(ctx, pk, iid)
}

func (c *countingGitLab) ListMRDiffs(ctx context.Context, pk string, iid int64) ([]gitlab.MergeRequestDiff, error) {
	if c.failDiffs != nil {
		return nil, c.failDiffs
	}
	return c.FakeClient.ListMRDiffs(ctx, pk, iid)
}

func (c *countingGitLab) ListMRPipelines(ctx context.Context, pk string, iid int64) ([]gitlab.Pipeline, error) {
	c.pipelineCalls[iid]++
	return c.FakeClient.ListMRPipelines(ctx, pk, iid)
}

func (c *countingGitLab) ListMRDiscussions(ctx context.Context, pk string, iid int64) ([]gitlab.Discussion, error) {
	c.discussionCalls++
	return c.FakeClient.ListMRDiscussions(ctx, pk, iid)
}

func (c *countingGitLab) ListMRVersions(ctx context.Context, pk string, iid int64) ([]gitlab.MergeRequestVersion, error) {
	c.versionCalls++
	return c.FakeClient.ListMRVersions(ctx, pk, iid)
}

func (c *countingGitLab) GetMRApprovals(ctx context.Context, pk string, iid int64) (*gitlab.Approvals, error) {
	c.approvalCalls++
	if c.failApprovals != nil {
		return nil, c.failApprovals
	}
	return c.FakeClient.GetMRApprovals(ctx, pk, iid)
}

var _ gitlab.API = (*countingGitLab)(nil)

// stubMatcher resolves GitLab users without a Slack workspace.
type stubMatcher struct {
	results map[string]match.Result
	err     error
}

func (m stubMatcher) Match(_ context.Context, u match.GitLabUser) (match.Result, error) {
	if m.err != nil {
		return match.Result{}, m.err
	}
	if r, ok := m.results[u.Username]; ok {
		return r, nil
	}
	return match.Result{Status: match.NotFound, Display: match.Fallback(u)}, nil
}

// recordingSlack captures chat.postMessage and response-URL calls.
type recordingSlack struct {
	posts []slack.PostMessageRequest
	err   error
	ts    string

	// responses are the delayed command answers, in order, with the response URL
	// each was posted to — the pair is what a test needs to tell an ephemeral
	// answer from one the whole channel sees.
	responses    []recordedResponse
	responseErr  error
	responseErrs []error // per-call overrides, consumed in order
}

type recordedResponse struct {
	URL string
	Msg slack.ResponseMessage
}

type fakeLinear struct {
	issues          []linear.Issue
	issuesByNumbers []linear.Issue
	err             error
	lookupErr       error
	teamErr         error
	// teamStates overrides the board a GetTeam call reports. Nil means
	// testWorkflowStates, i.e. an ordinary Linear board — the default is a *live*
	// readiness gate on purpose, so a test has to opt out of it rather than
	// accidentally exercise a dormant one.
	teamStates []linear.WorkflowState
	// teamStatesByID gives each Linear team its own board, which is the only way
	// to exercise two teams that put In Review in different places.
	teamStatesByID map[string][]linear.WorkflowState
	calls          int
	lookupCalls    int
	teamCalls      int
	teamIDs        []string
	numbers        []int
}

// testWorkflowStates is a plain Linear board: two columns before In Review, one
// after. Positions are spaced the way Linear's own floats are, and every state
// carries the type the ordering actually depends on.
func testWorkflowStates() []linear.WorkflowState {
	return []linear.WorkflowState{
		{ID: "st-backlog", Name: "Backlog", Type: "backlog", Position: 0},
		{ID: "st-progress", Name: "In Progress", Type: "started", Position: 1024},
		{ID: "st-review", Name: linear.InReviewState, Type: "started", Position: 2048},
		{ID: "st-done", Name: "Done", Type: "completed", Position: 4096},
	}
}

// testState is one column of testWorkflowStates, so a fixture issue carries the
// id, type and position a real one does. A state built by name alone resolves to
// linear.StageUnknown, which silently disables the readiness gate — a fixture
// that does that tests the fail-open path while claiming to test the board.
func testState(t *testing.T, name string) linear.WorkflowState {
	t.Helper()
	for _, st := range testWorkflowStates() {
		if strings.EqualFold(strings.TrimSpace(st.Name), strings.TrimSpace(name)) {
			return st
		}
	}
	t.Fatalf("no workflow state named %q in the fixture board", name)
	return linear.WorkflowState{}
}

func (f *fakeLinear) GetTeam(_ context.Context, id string) (*linear.Team, error) {
	f.teamCalls++
	if f.teamErr != nil {
		return nil, f.teamErr
	}
	states := f.teamStates
	if byID, ok := f.teamStatesByID[id]; ok {
		states = byID
	}
	if states == nil {
		states = testWorkflowStates()
	}
	return &linear.Team{ID: id, Key: "CHAIN", Name: "Chain", States: states}, nil
}

func (f *fakeLinear) ListIssuesByNumbers(_ context.Context, teamIDs []string, numbers []int) ([]linear.Issue, error) {
	f.lookupCalls++
	f.teamIDs = append([]string(nil), teamIDs...)
	f.numbers = append([]int(nil), numbers...)
	return append([]linear.Issue(nil), f.issuesByNumbers...), f.lookupErr
}

func (f *fakeLinear) ListIssuesInReview(_ context.Context, teamIDs []string) ([]linear.Issue, error) {
	f.calls++
	f.teamIDs = append([]string(nil), teamIDs...)
	return append([]linear.Issue(nil), f.issues...), f.err
}

func (r *recordingSlack) PostMessage(_ context.Context, req slack.PostMessageRequest) (*slack.PostMessageResult, error) {
	r.posts = append(r.posts, req)
	if r.err != nil {
		return nil, r.err
	}
	ts := r.ts
	if ts == "" {
		ts = "1700000000.000100"
	}
	return &slack.PostMessageResult{Channel: req.Channel, TS: ts}, nil
}

func (r *recordingSlack) Respond(_ context.Context, responseURL string, msg slack.ResponseMessage) error {
	r.responses = append(r.responses, recordedResponse{URL: responseURL, Msg: msg})
	if len(r.responseErrs) > 0 {
		err := r.responseErrs[0]
		r.responseErrs = r.responseErrs[1:]
		return err
	}
	return r.responseErr
}

// harness wires a Service over fakes. Store and pool are nil unless the test
// asked for a database.
type harness struct {
	t       *testing.T
	gl      *countingGitLab
	fake    *gitlab.FakeClient
	gql     *gitlab.FakeGraphQL
	llm     *llm.FakeClient
	slack   *recordingSlack
	matcher match.UserMatcher
	linear  *fakeLinear
	st      *store.Store
	pool    *pgxpool.Pool
	svc     *Service
	cfg     Config
}

// defaultTestConfig keeps the fixtures small: no risk scoring, no coverage, no
// file contexts, no commit fetch — the review path under test is the pipeline
// and the persistence, not the prompt builders.
func defaultTestConfig() Config {
	return Config{
		Host:             "https://gitlab.example.com",
		ReviewerUsername: "ai-reviewer",
		ToolVersion:      "test",
		PipelineName:     "standard",
		Teams:            []domain.Team{testTeamConfig()},
		Profile:          review.DefaultProfile(),
		Context: review.ContextBudget{
			IncludeDiscussions: true,
			MaxDiscussionBytes: 4 << 10,
			IncludePriorReview: true,
		},
		Now: func() time.Time { return testNow },
	}
}

type harnessOption func(*harness)

// withDB attaches a real Postgres store; the test skips without a DSN.
func withDB(h *harness) {
	h.st, h.pool = storetest.Store(h.t)
}

func withConfig(fn func(*Config)) harnessOption {
	return func(h *harness) { fn(&h.cfg) }
}

// withoutLinear builds the service with no Linear collaborator at all, the
// shape a team carrying linear_team_ids sees when the process decided no client
// was needed.
func withoutLinear(h *harness) { h.linear = nil }

func withLLM(resp *llm.ReviewResponse) harnessOption {
	return func(h *harness) { h.llm.Response = resp }
}

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	fake := gitlab.NewFake()
	h := &harness{
		t:       t,
		fake:    fake,
		gl:      newCountingGitLab(fake),
		gql:     &gitlab.FakeGraphQL{},
		llm:     &llm.FakeClient{Response: &llm.ReviewResponse{Summary: "nothing to report"}},
		slack:   &recordingSlack{},
		matcher: stubMatcher{},
		linear:  &fakeLinear{},
		cfg:     defaultTestConfig(),
	}
	for _, o := range opts {
		o(h)
	}

	log := logger.ForTests(t)
	deps := Deps{
		GitLab:  h.gl,
		GraphQL: h.gql,
		Slack:   h.slack,
		Matcher: h.matcher,
		Store:   h.st,
		Engine:  review.NewEngine(h.llm, log),
		Log:     log,
	}
	// Assigned conditionally for the same reason app.linearAPI exists: a nil
	// *fakeLinear stored in the interface is a typed nil, and withoutLinear could
	// not reach the not-configured path through one.
	if h.linear != nil {
		deps.Linear = h.linear
	}
	if h.st == nil {
		// Tests that only exercise the snapshot loader need no database.
		h.svc = newService(deps, h.cfg)
		return h
	}
	svc, err := New(deps, h.cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.svc = svc
	return h
}

// reviewResponse builds a one-finding LLM response anchored to sampleDiff's
// added line.
func reviewResponse(title string) *llm.ReviewResponse {
	return &llm.ReviewResponse{
		Summary:               "one problem found",
		RiskLevel:             "medium",
		OverallRecommendation: "comment",
		Findings: []llm.Finding{{
			Severity: "high",
			Category: "correctness",
			FilePath: "main.go",
			LineKind: "new",
			Line:     3,
			Title:    title,
			Body:     "The added function never returns an error.",
		}},
	}
}

// multiFindingResponse builds one finding per title, all anchored to
// sampleDiff's added line. Titles differ, so the fingerprints do.
func multiFindingResponse(titles ...string) *llm.ReviewResponse {
	resp := &llm.ReviewResponse{
		Summary:               "several problems found",
		RiskLevel:             "medium",
		OverallRecommendation: "request_changes",
	}
	for _, title := range titles {
		resp.Findings = append(resp.Findings, llm.Finding{
			Severity: "high",
			Category: "correctness",
			FilePath: "main.go",
			LineKind: "new",
			Line:     3,
			Title:    title,
			Body:     "body of " + title,
		})
	}
	return resp
}

// mrFindingFixture is a persisted finding as the publisher sees it.
var mrFindingFixture = models.MrFinding{
	Fingerprint: "9f2c1ab3",
	Severity:    "high",
	Category:    "correctness",
	FilePath:    "main.go",
	Title:       "leaks a connection",
	Body:        "The connection is never closed.",
}

// fingerprintOf is the fingerprint the engine will compute for a finding
// produced by reviewResponse.
func fingerprintOf(title string) string {
	return review.Fingerprint(testProjectID, testMRIID, "main.go", "correctness", title)
}

// note builds a discussion note fixture.
func note(id int64, body string) gitlab.Note {
	return gitlab.Note{ID: id, Body: body, Author: gitlab.User{ID: 9, Username: "ai-reviewer"}}
}
