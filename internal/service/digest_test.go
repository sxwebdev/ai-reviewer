package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/linear"
	"github.com/sxwebdev/ai-reviewer/internal/match"
	"github.com/sxwebdev/ai-reviewer/internal/metrics"
	"github.com/sxwebdev/ai-reviewer/internal/models"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

var digestDay = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

// mustGather reads the default registry, which is the only way to tell an absent
// series from a zero one — testutil.ToFloat64 creates the series it reads.
func mustGather(t *testing.T) []*dto.MetricFamily {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	return families
}

func TestTruncateDay(t *testing.T) {
	t.Parallel()
	got := truncateDay(time.Date(2026, 8, 13, 16, 30, 0, 0, time.FixedZone("MSK", 3*3600)))
	want := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("truncateDay = %v, want %v — the caller already chose the calendar day", got, want)
	}
}

func TestProjectLabel(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"backend/payments":   "payments",
		"group/sub/checkout": "checkout",
		"payments":           "payments",
		"trailing/":          "trailing/",
		"":                   "",
	}
	for in, want := range cases {
		if got := projectLabel(in); got != want {
			t.Errorf("projectLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDigestStatuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		send        bool
		failedRepos int
		wantRun     string
		wantMessage string
	}{
		{"clean run", true, 0, DigestBuilt, MessagePending},
		{"partial data", true, 2, DigestPartial, MessagePending},
		// "nothing was sent" is the fact an operator has to see first; the
		// partial-data warning still renders inside the message.
		{"dry run outranks partial", false, 2, DigestDryRun, MessageDryRun},
		{"dry run", false, 0, DigestDryRun, MessageDryRun},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			run, msg := digestStatuses(c.send, c.failedRepos)
			if run != c.wantRun || msg != c.wantMessage {
				t.Errorf("digestStatuses = (%q, %q), want (%q, %q)", run, msg, c.wantRun, c.wantMessage)
			}
		})
	}
}

// digestHarness seeds one team with one repository whose MR needs both a review
// and an author action.
func digestHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	base := []harnessOption{withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true })}
	h := newHarness(t, append(base, opts...)...)

	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
	mr := testMR(testMRIID,
		withReviewers(reviewer),
		withConflicts(true, "conflict"),
		withHeadPipeline(&gitlab.Pipeline{ID: 5, SHA: testHeadSHA, Status: "failed", WebURL: "https://pipelines/5"}),
	)
	seedGitLab(h.fake, proj, mr)
	setDiscussions(h.fake, proj, testMRIID, []gitlab.Discussion{
		{ID: "open", Notes: []gitlab.Note{{ID: 1, Resolvable: true, Author: gitlab.User{ID: 1, Username: "author"}}}},
	})
	return h
}

func (h *harness) digestMessages(t *testing.T, runID uuid.UUID) []*models.DigestMessage {
	t.Helper()
	msgs, err := h.st.DigestMessage().ListByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	return msgs
}

func TestBuildDigestPersistsRunAndParts(t *testing.T) {
	h := digestHarness(t, func(h *harness) {
		h.matcher = stubMatcher{results: map[string]match.Result{
			"reviewer": {Status: match.Matched, SlackID: "U42", Display: "Rita"},
			"author":   {Status: match.Matched, SlackID: "U01", Display: "Ann"},
		}}
	})

	out, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestBuilt {
		t.Errorf("status = %q, want %q", out.Status, DigestBuilt)
	}
	if out.MRCount != 1 {
		t.Errorf("mr count = %d, want 1", out.MRCount)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(out.Messages))
	}
	// BuildDigest never posts; delivery is a separate job.
	if len(h.slack.posts) != 0 {
		t.Errorf("BuildDigest called chat.postMessage %d times", len(h.slack.posts))
	}

	msgs := h.digestMessages(t, out.RunID)
	if len(msgs) != 1 || msgs[0].Status != MessagePending || msgs[0].Channel != "C123" {
		t.Fatalf("persisted message = %+v, want one pending part for C123", msgs[0])
	}

	var m slack.Message
	if err := json.Unmarshal(msgs[0].Payload, &m); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	rendered := renderedText(m)
	// One row per merge request now, with the author's flags inline.
	for _, want := range []string{
		"<@U42>", "<@U01>", "!481", "💬 resolve 1 thread", "⚠️ fix merge conflicts", "https://pipelines/5",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("digest is missing %q:\n%s", want, rendered)
		}
	}
}

func TestBuildDigestShowsOnlyLinearInReviewCount(t *testing.T) {
	h := digestHarness(t)
	team := testTeamConfig()
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	h.linear.issues = []linear.Issue{
		{
			ID: "i2", Identifier: "OPS-9", Number: 9, URL: "https://linear.app/ops/OPS-9",
			State: linear.WorkflowState{Name: linear.InReviewState}, Team: linear.Team{Key: "OPS"},
		},
		{
			ID: "i1", Identifier: "PAY-2", Number: 2, URL: "https://linear.app/pay/PAY-2",
			State: linear.WorkflowState{Name: linear.InReviewState}, Team: linear.Team{Key: "PAY"},
		},
	}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.LinearIssueCount != 2 {
		t.Fatalf("linear issue count = %d, want 2", out.LinearIssueCount)
	}
	if h.linear.calls != 1 || !slices.Equal(h.linear.teamIDs, team.LinearTeamIDs) {
		t.Errorf("Linear calls/team ids = %d/%v", h.linear.calls, h.linear.teamIDs)
	}

	msgs := h.digestMessages(t, out.RunID)
	var message slack.Message
	if err := json.Unmarshal(msgs[0].Payload, &message); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	text := renderedText(message)
	// Rita is the seeded GitLab reviewer, not a Linear assignee: the two sections
	// coexist, and only the Linear one is collapsed to a count.
	for _, want := range []string{"Linear · In Review: 2", "Rita Reviewer"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest is missing %q:\n%s", want, text)
		}
	}
	// Titles are no longer expressible here — linear.Issue does not carry one, so
	// no query can fetch one and no renderer can print one. The identifiers are
	// the remaining way a per-issue row could leak in.
	for _, unwanted := range []string{"OPS-9", "PAY-2"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("digest still renders Linear issue %q as a separate row:\n%s", unwanted, text)
		}
	}

	run, err := h.st.DigestRun().GetByID(t.Context(), out.RunID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if run.LinearIssueCount != 2 {
		t.Errorf("persisted linear issue count = %d", run.LinearIssueCount)
	}
}

func TestBuildDigestApprovedLinkedInReviewNudgesAuthorInsteadOfReviewers(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
	mr := testMR(testMRIID, withReviewers(reviewer))
	mr.Title = "CHAIN-184 Scheduler wrapper"
	seedGitLab(h.fake, proj, mr)
	h.gql.States = map[int64][]gitlab.ReviewerState{
		testMRIID: {{Username: reviewer.Username, State: gitlab.ReviewStateUnreviewed}},
	}
	setApprovals(h.fake, proj, testMRIID, &gitlab.Approvals{ApprovedBy: []gitlab.ApprovedBy{{
		User: gitlab.User{ID: 77, Username: "approver", Name: "Alice Approver"},
	}}})
	issue := linear.Issue{
		ID: "linear-184", Identifier: "CHAIN-184", Number: 184,
		URL:   "https://linear.app/CHAIN-184",
		State: testState(t, linear.InReviewState),
		Team:  linear.Team{ID: team.LinearTeamIDs[0], Key: "CHAIN"},
	}
	h.linear.issues = []linear.Issue{issue}
	h.linear.issuesByNumbers = []linear.Issue{issue}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.MRCount != 1 || out.LinearIssueCount != 1 {
		t.Errorf("outcome = %+v, want one author action and one Linear issue", out)
	}
	if h.linear.lookupCalls != 1 || !slices.Equal(h.linear.numbers, []int{184}) {
		t.Errorf("Linear lookup calls/numbers = %d/%v, want 1/[184]", h.linear.lookupCalls, h.linear.numbers)
	}

	msgs := h.digestMessages(t, out.RunID)
	var message slack.Message
	if err := json.Unmarshal(msgs[0].Payload, &message); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	text := renderedText(message)
	for _, want := range []string{"Linear · In Review: 1", "Ann Author", "move <https://linear.app/CHAIN-184|CHAIN-184> forward in Linear"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Rita Reviewer") || strings.Contains(text, "review 1") {
		t.Errorf("approved linked MR still asks its remaining reviewer:\n%s", text)
	}
}

// The readiness gate, end to end. This is the report that started it: an open MR
// with an unreviewed reviewer whose Linear card never left In Progress was pushed
// to three reviewers, while the one person who could fix it — the author — was
// told nothing.
func TestBuildDigestBeforeInReviewParksTheMRWithItsAuthor(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
	mr := testMR(testMRIID, withReviewers(reviewer))
	mr.Title = "CHAIN-184 Scheduler wrapper"
	seedGitLab(h.fake, proj, mr)
	h.gql.States = map[int64][]gitlab.ReviewerState{
		testMRIID: {{Username: reviewer.Username, State: gitlab.ReviewStateUnreviewed}},
	}
	issue := linear.Issue{
		ID: "linear-184", Identifier: "CHAIN-184", Number: 184,
		URL:   "https://linear.app/CHAIN-184",
		State: testState(t, "In Progress"),
		Team:  linear.Team{ID: team.LinearTeamIDs[0], Key: "CHAIN"},
	}
	h.linear.issuesByNumbers = []linear.Issue{issue}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestBuilt {
		t.Errorf("status = %q, want %q — the board being behind is not a failure", out.Status, DigestBuilt)
	}
	if h.linear.teamCalls != 1 {
		t.Errorf("GetTeam calls = %d, want one per configured Linear team", h.linear.teamCalls)
	}

	text := renderedText(decodeMessage(t, h.digestMessages(t, out.RunID)[0]))
	if strings.Contains(text, "Rita Reviewer") {
		t.Errorf("a card that never reached In Review still asked its reviewer:\n%s", text)
	}
	// And it is not lost either: the author owns it, and the row says which column
	// the card is actually in.
	for _, want := range []string{"Ann Author", "move <https://linear.app/CHAIN-184|CHAIN-184> to In Review", "(now In Progress)"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest is missing %q:\n%s", want, text)
		}
	}
	if out.MRCount != 1 {
		t.Errorf("mr count = %d, want the merge request still counted once", out.MRCount)
	}
}

// A board whose column order cannot be established must change nothing: the gate
// switches off, reviewers are notified exactly as before, and the digest says so
// rather than quietly behaving differently.
func TestBuildDigestUnreadableWorkflowKeepsNotifyingReviewers(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
	mr := testMR(testMRIID, withReviewers(reviewer))
	mr.Title = "CHAIN-184 Scheduler wrapper"
	seedGitLab(h.fake, proj, mr)
	h.gql.States = map[int64][]gitlab.ReviewerState{
		testMRIID: {{Username: reviewer.Username, State: gitlab.ReviewStateUnreviewed}},
	}
	// No In Review column at all, so linear.NewWorkflow refuses the board.
	h.linear.teamStates = []linear.WorkflowState{
		{ID: "st-progress", Name: "In Progress", Type: "started", Position: 1024},
	}
	h.linear.issuesByNumbers = []linear.Issue{{
		ID: "linear-184", Identifier: "CHAIN-184", Number: 184,
		URL:   "https://linear.app/CHAIN-184",
		State: linear.WorkflowState{ID: "st-progress", Name: "In Progress", Type: "started", Position: 1024},
		Team:  linear.Team{ID: team.LinearTeamIDs[0], Key: "CHAIN"},
	}}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestPartial {
		t.Errorf("status = %q, want %q", out.Status, DigestPartial)
	}
	text := renderedText(decodeMessage(t, h.digestMessages(t, out.RunID)[0]))
	if !strings.Contains(text, "Rita Reviewer") {
		t.Errorf("an unreadable board silenced a reviewer:\n%s", text)
	}
	// The warning has to name *this* degradation, and to claim only what happened:
	// this merge request was linked to the unorderable board and did keep notifying
	// its reviewer. "Linear issue links could not be resolved" would be false — the
	// link is right there in the row above.
	if !strings.Contains(text, "a Linear board could not be ordered; merge requests linked to it were not graded against it") {
		t.Errorf("digest does not explain the disabled gate:\n%s", text)
	}
	if strings.Contains(text, "issue links could not be resolved") {
		t.Errorf("digest blames the wrong half of Linear:\n%s", text)
	}
}

// GitLab refusing GET /approvals is the outage that made the whole Linear
// completion rule inert without a word anywhere. The reviewer keeps being asked —
// which is the safe direction — and nothing pretends the review is finished.
func TestBuildDigestUnreadableApprovalsNeverReadAsUnapproved(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	// Its own team label: the gauge asserted below lives on the default registry,
	// and every other test in this package publishes under testTeam.
	team.Name = "payments-blind-approvals"
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
	mr := testMR(testMRIID, withReviewers(reviewer))
	mr.Title = "CHAIN-184 Scheduler wrapper"
	seedGitLab(h.fake, proj, mr)
	h.gql.States = map[int64][]gitlab.ReviewerState{
		testMRIID: {{Username: reviewer.Username, State: gitlab.ReviewStateUnreviewed}},
	}
	// The approval exists in GitLab; the service is simply not allowed to read it.
	setApprovals(h.fake, proj, testMRIID, &gitlab.Approvals{ApprovedBy: []gitlab.ApprovedBy{{
		User: gitlab.User{ID: 77, Username: "approver", Name: "Alice Approver"},
	}}})
	h.gl.failApprovals = &gitlab.APIError{Status: 403, Path: "/approvals"}
	issue := linear.Issue{
		ID: "linear-184", Identifier: "CHAIN-184", Number: 184,
		URL:   "https://linear.app/CHAIN-184",
		State: testState(t, linear.InReviewState),
		Team:  linear.Team{ID: team.LinearTeamIDs[0], Key: "CHAIN"},
	}
	h.linear.issuesByNumbers = []linear.Issue{issue}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	text := renderedText(decodeMessage(t, h.digestMessages(t, out.RunID)[0]))
	if !strings.Contains(text, "Rita Reviewer") {
		t.Errorf("unknown approvals were read as an approval and silenced the reviewer:\n%s", text)
	}
	if strings.Contains(text, "forward in Linear") {
		t.Errorf("unknown approvals were read as a finished review:\n%s", text)
	}
	// The rendering assertions above hold with or without ApprovalsKnown — an
	// unreadable endpoint leaves ApprovedBy empty either way, so on their own they
	// pin the *pre-fix* behaviour. This is the assertion only the flag can satisfy:
	// the difference between "nobody approved" and "we could not ask" has to be
	// observable somewhere, and this gauge is where.
	if got := testutil.ToFloat64(
		metrics.MergeRequestsWithUnknownApprovals.WithLabelValues(team.Name),
	); got != 1 {
		t.Errorf("unknown-approvals gauge = %v, want 1 — the outage is invisible", got)
	}
}

// A service team may map several Linear teams, and each orders its own columns.
// Grading an issue against another team's In Review position is how a card that
// *is* ready reads as unready — and the merge request then silently stops
// reaching its reviewers, which is the exact failure the gate is supposed to fix.
func TestBuildDigestGradesEachIssueAgainstItsOwnLinearTeam(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	const chainTeam, opsTeam = "team-chain", "team-ops"
	team.LinearTeamIDs = []string{chainTeam, opsTeam}
	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
	first := testMR(101, withReviewers(reviewer))
	first.Title = "CHAIN-1 chain side"
	second := testMR(102, withReviewers(reviewer))
	second.Title = "OPS-2 ops side"
	seedGitLab(h.fake, proj, first, second)
	h.gql.States = map[int64][]gitlab.ReviewerState{
		101: {{Username: reviewer.Username, State: gitlab.ReviewStateUnreviewed}},
		102: {{Username: reviewer.Username, State: gitlab.ReviewStateUnreviewed}},
	}

	// One column id, opposite verdicts. Sharing the id across the two boards is
	// what makes "which board was consulted" observable *in both directions*: with
	// distinct ids a wrong-board lookup merely misses and fails open, which is
	// indistinguishable from the correct answer on the reviewer side, and a test
	// built that way passes while the grading is hard-wired to one team.
	chainTesting := linear.WorkflowState{ID: "testing", Name: "Testing", Type: "started", Position: 300}
	opsTesting := linear.WorkflowState{ID: "testing", Name: "Testing", Type: "started", Position: 100}
	h.linear.teamStatesByID = map[string][]linear.WorkflowState{
		chainTeam: {
			{ID: "c-review", Name: linear.InReviewState, Type: "started", Position: 200},
			chainTesting,
		},
		opsTeam: {
			opsTesting,
			{ID: "o-review", Name: linear.InReviewState, Type: "started", Position: 200},
		},
	}
	h.linear.issuesByNumbers = []linear.Issue{
		{
			ID: "i1", Identifier: "CHAIN-1", Number: 1, URL: "https://linear.app/CHAIN-1",
			State: chainTesting, Team: linear.Team{ID: chainTeam, Key: "CHAIN"},
		},
		{
			ID: "i2", Identifier: "OPS-2", Number: 2, URL: "https://linear.app/OPS-2",
			State: opsTesting, Team: linear.Team{ID: opsTeam, Key: "OPS"},
		},
	}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if h.linear.teamCalls != 2 {
		t.Errorf("GetTeam calls = %d, want one per configured Linear team", h.linear.teamCalls)
	}
	text := renderedText(decodeMessage(t, h.digestMessages(t, out.RunID)[0]))
	// Chain's card is past review on Chain's board, so its reviewer is asked and it
	// gets no author row.
	if !strings.Contains(text, "Rita Reviewer") || !strings.Contains(text, "!101") {
		t.Errorf("the merge request past its own board's In Review lost its reviewer:\n%s", text)
	}
	if strings.Contains(text, "CHAIN-1> to In Review") {
		t.Errorf("chain's card was graded against another team's board:\n%s", text)
	}
	// Ops's identically-keyed card is behind review on Ops's board, so its author is
	// asked instead.
	if !strings.Contains(text, "move <https://linear.app/OPS-2|OPS-2> to In Review") {
		t.Errorf("the merge request behind its own board's In Review was not parked with its author:\n%s", text)
	}
}

// One unusable board must not cost the other team its gate, and the digest still
// reports the degradation.
func TestBuildDigestKeepsWorkingWorkflowsWhenOneTeamIsBroken(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	const goodTeam, badTeam = "team-good", "team-bad"
	team.LinearTeamIDs = []string{badTeam, goodTeam}
	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
	mr := testMR(testMRIID, withReviewers(reviewer))
	mr.Title = "CHAIN-1 wrapper"
	seedGitLab(h.fake, proj, mr)
	h.gql.States = map[int64][]gitlab.ReviewerState{
		testMRIID: {{Username: reviewer.Username, State: gitlab.ReviewStateUnreviewed}},
	}
	h.linear.teamStatesByID = map[string][]linear.WorkflowState{
		badTeam:  {{ID: "b1", Name: "Doing", Type: "started", Position: 1}}, // no In Review
		goodTeam: testWorkflowStates(),
	}
	h.linear.issuesByNumbers = []linear.Issue{{
		ID: "i1", Identifier: "CHAIN-1", Number: 1, URL: "https://linear.app/CHAIN-1",
		State: testState(t, "In Progress"), Team: linear.Team{ID: goodTeam, Key: "CHAIN"},
	}}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestPartial {
		t.Errorf("status = %q, want %q — one broken board is a degradation", out.Status, DigestPartial)
	}
	text := renderedText(decodeMessage(t, h.digestMessages(t, out.RunID)[0]))
	if !strings.Contains(text, "move <https://linear.app/CHAIN-1|CHAIN-1> to In Review") {
		t.Errorf("the readable board lost its gate because another team's was broken:\n%s", text)
	}
	// And the warning may not deny what the row above it just did. The digest-wide
	// "every linked merge request was treated as ready for review" was printed here,
	// directly contradicting the gated row: no merge request in this digest was
	// linked to the board that failed.
	if !strings.Contains(text, "no merge request in this digest was linked to it") {
		t.Errorf("warning does not describe the partial failure:\n%s", text)
	}
	if strings.Contains(text, "were not graded against it") {
		t.Errorf("warning claims an effect on merge requests that were not affected:\n%s", text)
	}
}

// The uncovered corner where "nothing to look up" and "the board is unreadable"
// meet: with no merge request naming a Linear id the link set is complete and
// empty, so the digest must not report the links as the thing that failed.
func TestBuildDigestUnorderableBoardWithNoLinkedMergeRequests(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
	seedGitLab(h.fake, proj, testMR(testMRIID, withReviewers(reviewer))) // title names no issue
	h.gql.States = map[int64][]gitlab.ReviewerState{
		testMRIID: {{Username: reviewer.Username, State: gitlab.ReviewStateUnreviewed}},
	}
	h.linear.teamStates = []linear.WorkflowState{
		{ID: "st-progress", Name: "In Progress", Type: "started", Position: 1024},
	}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if h.linear.lookupCalls != 0 {
		t.Errorf("issue lookup calls = %d, want none — no merge request named an issue", h.linear.lookupCalls)
	}
	text := renderedText(decodeMessage(t, h.digestMessages(t, out.RunID)[0]))
	if strings.Contains(text, "issue links could not be resolved") {
		t.Errorf("an empty link set was reported as a failed lookup:\n%s", text)
	}
	if !strings.Contains(text, "no merge request in this digest was linked to it") {
		t.Errorf("warning does not name the unorderable board:\n%s", text)
	}
}

// The not-ready gauge must go *absent* when the column order is unreadable, not to
// zero. Every card then grades unknown, so the count computes to zero while the
// merge requests are still parked exactly where they were — and the reviewers they
// fell back to show up as a jump in waiting_human_review, so an operator reads the
// pair as work moving forward.
// Both halves have to be known, and there are two ways to lose one: the board's
// column order, and the per-merge-request link lookup. Either leaves the count a
// hard zero — "nothing is stuck before In Review" — while the merge requests stay
// exactly where they were, so both must take the series away instead.
func TestBuildDigestClearsTheNotReadyGaugeWheneverTheGateCouldNotRun(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(*harness)
	}{
		{
			name: "board has no In Review column",
			break_: func(h *harness) {
				h.linear.teamStates = []linear.WorkflowState{
					{ID: "st-progress", Name: "In Progress", Type: "started", Position: 1024},
				}
			},
		},
		{
			// gateKnown is decided before this call, so it stays true: the guard has
			// to consult linksKnown as well or the same false zero returns by the
			// other door.
			name:   "issue link lookup fails",
			break_: func(h *harness) { h.linear.lookupErr = errors.New("Linear lookup unavailable") },
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
			team := testTeamConfig()
			// Distinct per subtest: the gauge lives on the default registry.
			team.Name = fmt.Sprintf("payments-gauge-gate-%d", i)
			team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
			proj := testProject()
			reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
			mr := testMR(testMRIID, withReviewers(reviewer))
			mr.Title = "CHAIN-184 Scheduler wrapper"
			seedGitLab(h.fake, proj, mr)
			h.gql.States = map[int64][]gitlab.ReviewerState{
				testMRIID: {{Username: reviewer.Username, State: gitlab.ReviewStateUnreviewed}},
			}
			h.linear.issuesByNumbers = []linear.Issue{{
				ID: "linear-184", Identifier: "CHAIN-184", Number: 184,
				URL: "https://linear.app/CHAIN-184", State: testState(t, "In Progress"),
				Team: linear.Team{ID: team.LinearTeamIDs[0], Key: "CHAIN"},
			}}

			// First a healthy build, so the series exists and is non-zero.
			if _, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0); err != nil {
				t.Fatalf("BuildDigest: %v", err)
			}
			if got := testutil.ToFloat64(
				metrics.MergeRequestsLinearNotReady.WithLabelValues(team.Name),
			); got != 1 {
				t.Fatalf("not-ready gauge = %v, want 1", got)
			}

			tt.break_(h)
			if _, err := h.svc.BuildDigest(t.Context(), team, "14:00", digestDay, 0); err != nil {
				t.Fatalf("BuildDigest: %v", err)
			}
			assertSeriesAbsent(t, "merge_requests_linear_not_ready_total", team.Name)
		})
	}
}

// assertSeriesAbsent fails when a gauge still publishes a series for team. It reads
// the registry directly because testutil.ToFloat64 *creates* the series it reads,
// so it can never tell an absent one from a zero.
func assertSeriesAbsent(t *testing.T, metric, team string) {
	t.Helper()
	for _, m := range mustGather(t) {
		if m.GetName() != metric {
			continue
		}
		for _, series := range m.GetMetric() {
			for _, label := range series.GetLabel() {
				if label.GetValue() == team {
					t.Errorf("%s is still published at %v for %q; it must be absent, not zero",
						metric, series.GetGauge().GetValue(), team)
				}
			}
		}
	}
}

// An ambiguous match switches the gate off, and that has to be visible from
// outside the process. The log line alone answers "our card is in Backlog, why were
// three reviewers still pinged?" only for somebody already reading logs.
func TestBuildDigestAmbiguousLinearMatchIsCountedAndFailsOpen(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	team.Name = "payments-ambiguous"
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
	mr := testMR(testMRIID, withReviewers(reviewer))
	// The first identifier names a superseded card; the branch names the live one.
	mr.Title = "CHAIN-1 superseded by CHAIN-2 work"
	mr.SourceBranch = "feature/CHAIN-2-impl"
	seedGitLab(h.fake, proj, mr)
	h.gql.States = map[int64][]gitlab.ReviewerState{
		testMRIID: {{Username: reviewer.Username, State: gitlab.ReviewStateUnreviewed}},
	}
	linearTeam := linear.Team{ID: team.LinearTeamIDs[0], Key: "CHAIN"}
	h.linear.issuesByNumbers = []linear.Issue{
		{
			ID: "l1", Identifier: "CHAIN-1", Number: 1, URL: "https://linear.app/CHAIN-1",
			State: testState(t, "Backlog"), Team: linearTeam,
		},
		{
			ID: "l2", Identifier: "CHAIN-2", Number: 2, URL: "https://linear.app/CHAIN-2",
			State: testState(t, linear.InReviewState), Team: linearTeam,
		},
	}

	before := testutil.ToFloat64(metrics.LinearGateAmbiguousTotal.WithLabelValues(team.Name))
	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if got := testutil.ToFloat64(
		metrics.LinearGateAmbiguousTotal.WithLabelValues(team.Name),
	); got != before+1 {
		t.Errorf("ambiguity counter = %v, want %v", got, before+1)
	}

	text := renderedText(decodeMessage(t, h.digestMessages(t, out.RunID)[0]))
	// Fail open: the Backlog card must not silence the reviewer, because the merge
	// request also names a card that is properly in review.
	if !strings.Contains(text, "Rita Reviewer") {
		t.Errorf("an ambiguous match silenced the reviewer:\n%s", text)
	}
	if strings.Contains(text, "to In Review") {
		t.Errorf("an ambiguous match produced a board nudge:\n%s", text)
	}
}

// A canceled card, end to end.// A canceled card, end to end. Nothing above linear.Workflow exercised it, so the
// claim that naming the column tells the author whether to move the card or close
// the merge request was asserted nowhere.
func TestBuildDigestCanceledCardParksTheMRWithItsAuthor(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
	mr := testMR(testMRIID, withReviewers(reviewer))
	mr.Title = "CHAIN-184 Scheduler wrapper"
	seedGitLab(h.fake, proj, mr)
	h.gql.States = map[int64][]gitlab.ReviewerState{
		testMRIID: {{Username: reviewer.Username, State: gitlab.ReviewStateUnreviewed}},
	}
	canceled := linear.WorkflowState{ID: "st-canceled", Name: "Canceled", Type: "canceled", Position: 8192}
	h.linear.teamStates = append(testWorkflowStates(), canceled)
	h.linear.issuesByNumbers = []linear.Issue{{
		ID: "linear-184", Identifier: "CHAIN-184", Number: 184,
		URL: "https://linear.app/CHAIN-184", State: canceled,
		Team: linear.Team{ID: team.LinearTeamIDs[0], Key: "CHAIN"},
	}}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestBuilt {
		t.Errorf("status = %q, want %q", out.Status, DigestBuilt)
	}
	text := renderedText(decodeMessage(t, h.digestMessages(t, out.RunID)[0]))
	if strings.Contains(text, "Rita Reviewer") {
		t.Errorf("a canceled card still asked its reviewer:\n%s", text)
	}
	// The column is named, which is the difference between "move the card" and
	// "close the merge request".
	if !strings.Contains(text, "(now Canceled)") {
		t.Errorf("the author row does not name the column:\n%s", text)
	}
}

func TestBuildDigestLinearFailureIsPartial(t *testing.T) {
	h := digestHarness(t)
	team := testTeamConfig()
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	h.linear.err = errors.New("Linear unavailable")

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestPartial || out.LinearIssueCount != 0 {
		t.Errorf("outcome = %+v, want partial with no Linear issues", out)
	}
	msgs := h.digestMessages(t, out.RunID)
	var message slack.Message
	if err := json.Unmarshal(msgs[0].Payload, &message); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if text := renderedText(message); !strings.Contains(text, "Linear could not be inspected") {
		t.Errorf("partial warning is missing:\n%s", text)
	}
}

// The batch lookup is the *link* half of Linear. Failing it must fail the
// reviewer gate open, and must not throw away the board total the first call
// already delivered: rendering a known 8 as no count, persisting it as 0 and
// leaving the gauge stale is the false zero §7 forbids.
func TestBuildDigestLinearLinkLookupFailureKeepsGitLabReviewersAndTheCount(t *testing.T) {
	h := digestHarness(t)
	team := testTeamConfig()
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	for _, mr := range h.fake.MRs {
		mr.Title = "CHAIN-481 Add payment retries"
	}
	h.linear.issues = []linear.Issue{{
		ID: "i1", Identifier: "PAY-4", URL: "https://linear/PAY-4",
		State: linear.WorkflowState{Name: linear.InReviewState}, Team: linear.Team{Key: "PAY"},
	}}
	h.linear.lookupErr = errors.New("Linear lookup unavailable")

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestPartial || h.linear.lookupCalls != 1 {
		t.Errorf("outcome/lookup calls = %+v/%d, want partial/1", out, h.linear.lookupCalls)
	}
	if out.LinearIssueCount != 1 {
		t.Errorf("linear issue count = %d, want the count the board already returned", out.LinearIssueCount)
	}
	run, err := h.st.DigestRun().GetByID(t.Context(), out.RunID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if run.LinearIssueCount != 1 {
		t.Errorf("persisted linear issue count = %d, want 1", run.LinearIssueCount)
	}

	msgs := h.digestMessages(t, out.RunID)
	var message slack.Message
	if err := json.Unmarshal(msgs[0].Payload, &message); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	text := renderedText(message)
	for _, want := range []string{"Rita Reviewer", "Linear · In Review: 1", "issue links could not be resolved"} {
		if !strings.Contains(text, want) {
			t.Errorf("fail-open digest is missing %q:\n%s", want, text)
		}
	}
}

// A team configured for Linear against a service that has no client is a
// configuration mistake, and the one thing it may not do is take the digest
// worker down: the guard has to be reachable, which it is not when a typed nil
// is stored in the interface.
func TestBuildDigestWithoutALinearClientDegradesInsteadOfPanicking(t *testing.T) {
	h := digestHarness(t, withoutLinear)
	team := testTeamConfig()
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestPartial || out.LinearIssueCount != 0 {
		t.Errorf("outcome = %+v, want partial with no Linear count", out)
	}
	msgs := h.digestMessages(t, out.RunID)
	var message slack.Message
	if err := json.Unmarshal(msgs[0].Payload, &message); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	// Fail-open: the GitLab half is still reported in full.
	text := renderedText(message)
	for _, want := range []string{"Rita Reviewer", "Linear could not be inspected"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest is missing %q:\n%s", want, text)
		}
	}
}

func TestBuildDigestUsesLinearWhenAllGitLabRepositoriesFail(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	team.Repositories = []string{"backend/gone"}
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	h.gl.failProject[url.PathEscape("backend/gone")] = errors.New("GitLab unavailable")
	h.linear.issues = []linear.Issue{{
		ID: "i1", Identifier: "PAY-4", Number: 4, URL: "https://linear/PAY-4",
		State: linear.WorkflowState{Name: linear.InReviewState}, Team: linear.Team{Key: "PAY"},
	}}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestPartial || out.MRCount != 0 || out.LinearIssueCount != 1 {
		t.Errorf("outcome = %+v", out)
	}
	if len(out.Messages) == 0 {
		t.Fatal("available Linear data was not persisted for delivery")
	}
}

// Both sources report into digest_source_errors_total, and they report from a
// run that never got as far as building anything too — a total outage is the
// failure an operator most wants counted, and it was the one path that skipped
// the counter entirely. The Linear gauge must go absent rather than keep the
// last good number.
func TestBuildDigestCountsBothSourcesAndClearsTheLinearGaugeOnFailure(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	team.Name = "metrics-" + t.Name()
	team.Repositories = []string{"backend/gone"}
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	h.gl.failProject[url.PathEscape("backend/gone")] = errors.New("GitLab unavailable")

	// A healthy build first, so the gauge holds a number worth losing.
	h.linear.issues = []linear.Issue{{
		ID: "i1", Identifier: "PAY-4", Number: 4, URL: "https://linear/PAY-4",
		State: linear.WorkflowState{Name: linear.InReviewState}, Team: linear.Team{Key: "PAY"},
	}}
	if _, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0); err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if got := testutil.ToFloat64(metrics.LinearIssuesInReview.WithLabelValues(team.Name)); got != 1 {
		t.Fatalf("linear gauge = %v, want the healthy count 1", got)
	}
	seriesWithGauge := testutil.CollectAndCount(metrics.LinearIssuesInReview)

	// That build already lost its one repository, so the GitLab source is counted
	// on an ordinary partial run too, not only on a total outage.
	sourceErrors := func(source string) float64 {
		return testutil.ToFloat64(metrics.DigestSourceErrorsTotal.WithLabelValues(team.Name, source))
	}
	if got := sourceErrors(metrics.SourceGitLab); got != 1 {
		t.Fatalf("digest_source_errors_total{source=gitlab} = %v after a partial build, want 1", got)
	}
	beforeGitLab, beforeLinear := sourceErrors(metrics.SourceGitLab), sourceErrors(metrics.SourceLinear)

	// Now lose Linear as well. The run fails outright, and that is exactly when
	// the counters have to have been written already — the early return used to
	// skip them.
	h.linear.err = errors.New("Linear unavailable")
	if _, err := h.svc.BuildDigest(t.Context(), team, "17:30", digestDay, 0); err == nil {
		t.Fatal("BuildDigest succeeded with every configured source unavailable")
	}

	for _, tc := range []struct {
		source string
		before float64
	}{{metrics.SourceGitLab, beforeGitLab}, {metrics.SourceLinear, beforeLinear}} {
		if got := sourceErrors(tc.source); got != tc.before+1 {
			t.Errorf("digest_source_errors_total{source=%q} = %v, want %v: the failed run did not count it",
				tc.source, got, tc.before+1)
		}
	}
	if got := testutil.CollectAndCount(metrics.LinearIssuesInReview); got != seriesWithGauge-1 {
		t.Errorf("linear gauge series = %d, want %d: an unknown count is still exported", got, seriesWithGauge-1)
	}
}

func TestBuildDigestFailsWhenGitLabAndLinearAreUnavailable(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	team.Repositories = []string{"backend/gone"}
	team.LinearTeamIDs = []string{"9cfb482a-81e3-4154-b5b9-2c805e70a02d"}
	h.gl.failProject[url.PathEscape("backend/gone")] = errors.New("GitLab unavailable")
	h.linear.err = errors.New("Linear unavailable")

	if _, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0); err == nil {
		t.Fatal("BuildDigest succeeded with every configured source unavailable")
	}
}

// renderedText concatenates every section of a message for substring checks.
// decodeMessage unwraps one persisted part into the Block Kit message it holds.
func decodeMessage(t *testing.T, m *models.DigestMessage) slack.Message {
	t.Helper()
	var message slack.Message
	if err := json.Unmarshal(m.Payload, &message); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return message
}

func renderedText(m slack.Message) string {
	var b strings.Builder
	b.WriteString(m.Text)
	for _, blk := range m.Blocks {
		if blk.Text != nil {
			b.WriteString("\n")
			b.WriteString(blk.Text.Text)
		}
	}
	return b.String()
}

// TestBuildDigestDryRunNeverSchedulesDelivery is the §13.5 bullet: everything
// except the network call happens and can be inspected, and no delivery job may
// be created.
func TestBuildDigestDryRunNeverSchedulesDelivery(t *testing.T) {
	h := digestHarness(t, withConfig(func(c *Config) { c.SlackSendEnabled = false }))

	out, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestDryRun {
		t.Errorf("status = %q, want %q", out.Status, DigestDryRun)
	}
	if len(out.Messages) != 0 {
		t.Errorf("messages = %v, want none: a dry run may not enqueue delivery", out.Messages)
	}
	if len(h.slack.posts) != 0 {
		t.Errorf("chat.postMessage was called %d times in a dry run", len(h.slack.posts))
	}

	msgs := h.digestMessages(t, out.RunID)
	if len(msgs) != 1 {
		t.Fatalf("persisted messages = %d, want the payload kept for inspection", len(msgs))
	}
	if msgs[0].Status != MessageDryRun {
		t.Errorf("message status = %q, want %q", msgs[0].Status, MessageDryRun)
	}
}

// TestBuildDigestPartialData covers the §17 partial-failure bullet verbatim:
// ten repositories, one of them answering 500, the other nine processed — the
// run marked partial and the warning rendered in the message.
//
// The status is read back out of digest_runs rather than off the in-memory
// outcome: the §6.6 journal is what an operator greps to find which slots were
// built on incomplete data, and a version that returned "partial" while writing
// "built" would pass an in-memory assertion.
func TestBuildDigestPartialData(t *testing.T) {
	h := digestHarness(t)
	team := testTeamConfig()

	proj := testProject()
	// Nine readable repositories, all serving the same fixture MR, plus one that
	// answers 500. The fixture project is registered under each path so the
	// snapshot cache does not collapse them into one.
	team.Repositories = nil
	for i := range 9 {
		path := fmt.Sprintf("backend/repo%d", i)
		h.fake.Projects[url.PathEscape(path)] = proj
		team.Repositories = append(team.Repositories, path)
	}
	team.Repositories = append(team.Repositories, "backend/broken")
	h.gl.failProject[url.PathEscape("backend/broken")] = &gitlab.APIError{
		Status: 500, Method: "GET", Path: "/projects/backend%2Fbroken",
	}

	out, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestPartial {
		t.Errorf("status = %q, want %q", out.Status, DigestPartial)
	}
	if got := h.digestRunStatus(t, out.RunID); got != DigestPartial {
		t.Errorf("digest_runs.status = %q, want %q — the journal must record the degradation", got, DigestPartial)
	}
	if out.MRCount != 1 {
		t.Errorf("mr count = %d, want the nine readable repositories' MR to survive", out.MRCount)
	}
	msgs := h.digestMessages(t, out.RunID)
	if len(msgs) == 0 {
		t.Fatal("a partial digest must still be sent")
	}
	var m slack.Message
	if err := json.Unmarshal(msgs[0].Payload, &m); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	text := renderedText(m)
	if !strings.Contains(text, "Partial data: failed to inspect 1 repository.") {
		t.Errorf("the partial-data warning is missing or miscounted:\n%s", text)
	}
}

func (h *harness) digestRunStatus(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	var status string
	if err := h.pool.QueryRow(t.Context(),
		`SELECT status FROM digest_runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read digest run: %v", err)
	}
	return status
}

func TestBuildDigestFailsWhenNoRepositoryCouldBeInspected(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	team := testTeamConfig()
	team.Repositories = []string{"backend/gone"}

	_, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0)
	if err == nil {
		t.Fatal("a run that learned nothing at all must report an error so the job retries")
	}

	var status string
	if err := h.pool.QueryRow(t.Context(),
		`SELECT status FROM digest_runs WHERE team = $1 AND slot = '09:00'`, testTeam).Scan(&status); err != nil {
		t.Fatalf("read digest run: %v", err)
	}
	if status != DigestFailed {
		t.Errorf("status = %q, want %q so the slot's attempt is journalled", status, DigestFailed)
	}
}

// TestBuildDigestRetryAfterAFailureReusesTheRow keeps a retry from colliding
// with the unique index on (team, run_date, slot, attempt).
func TestBuildDigestRetryAfterAFailureReusesTheRow(t *testing.T) {
	h := digestHarness(t)
	team := testTeamConfig()
	team.Repositories = []string{"backend/gone"}

	if _, err := h.svc.BuildDigest(t.Context(), team, "09:00", digestDay, 0); err == nil {
		t.Fatal("the first attempt must fail")
	}

	// The repository comes back; the retry has to reuse the failed row.
	out, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if out.Status != DigestBuilt {
		t.Errorf("status = %q, want %q", out.Status, DigestBuilt)
	}
	if n := h.count(t, `SELECT count(*) FROM digest_runs WHERE team = $1 AND slot = '09:00'`, testTeam); n != 1 {
		t.Errorf("digest_runs rows = %d, want the failed row reused", n)
	}
}

// TestBuildDigestIsIdempotentForTheSameSlot pins that a retried job hands back
// the run it already built instead of assembling a second copy.
func TestBuildDigestIsIdempotentForTheSameSlot(t *testing.T) {
	h := digestHarness(t)

	first, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	second, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("second BuildDigest: %v", err)
	}
	if first.RunID != second.RunID {
		t.Errorf("run ids differ (%s vs %s); the slot was built twice", first.RunID, second.RunID)
	}
	if n := h.count(t, `SELECT count(*) FROM digest_messages`); n != len(first.Messages) {
		t.Errorf("digest_messages rows = %d, want %d", n, len(first.Messages))
	}
	if len(second.Messages) != len(first.Messages) {
		t.Errorf("the retry reported %d parts, want %d so delivery can be re-enqueued",
			len(second.Messages), len(first.Messages))
	}
}

// TestBuildDigestForceTakesTheNextAttempt is the §15 `digest --force` path: a
// deliberate repeat is a new run, not a relaxation of the unique index.
func TestBuildDigestForceTakesTheNextAttempt(t *testing.T) {
	h := digestHarness(t)

	first, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	forced, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 1)
	if err != nil {
		t.Fatalf("forced BuildDigest: %v", err)
	}
	if forced.RunID == first.RunID {
		t.Error("--force must create a new run, not reuse the scheduled one")
	}
	if n := h.count(t, `SELECT count(*) FROM digest_runs WHERE team = $1`, testTeam); n != 2 {
		t.Errorf("digest_runs rows = %d, want 2", n)
	}
}

// TestBuildDigestKeepsTeamsApart is the §17 teams bullet: one team's data must
// never leak into another's digest.
func TestBuildDigestKeepsTeamsApart(t *testing.T) {
	h := digestHarness(t)
	other := domain.Team{Name: "checkout", SlackChannel: "C999", AIReview: true, Repositories: []string{}}

	out, err := h.svc.BuildDigest(t.Context(), other, "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.MRCount != 0 {
		t.Errorf("mr count = %d, want 0: the other team owns no repositories", out.MRCount)
	}
	if len(out.Messages) != 0 {
		t.Errorf("messages = %d, want none for an empty digest", len(out.Messages))
	}
}

func TestBuildDigestNamesPeopleItCannotMention(t *testing.T) {
	h := digestHarness(t, func(h *harness) {
		h.matcher = stubMatcher{results: map[string]match.Result{
			"reviewer": {Status: match.Ambiguous, Display: "Rita Reviewer (@reviewer)"},
		}}
	})

	out, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	msgs := h.digestMessages(t, out.RunID)
	var m slack.Message
	if err := json.Unmarshal(msgs[0].Payload, &m); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	text := renderedText(m)
	if strings.Contains(text, "<@") {
		t.Errorf("an ambiguous match must never produce a mention:\n%s", text)
	}
	if !strings.Contains(text, "Rita Reviewer") {
		t.Errorf("the person must still be named:\n%s", text)
	}
}

// TestBuildDigestMeasuresWaitingFromTheLastPush pins §20.3: GitLab does not
// report when a reviewer was assigned, so "waiting" counts from the newest diff
// version — how long nobody reacted to the MR's current state.
func TestBuildDigestMeasuresWaitingFromTheLastPush(t *testing.T) {
	h := digestHarness(t, func(h *harness) {
		h.matcher = stubMatcher{results: map[string]match.Result{
			"reviewer": {Status: match.Matched, SlackID: "U42", Display: "Rita"},
		}}
	})
	setVersions(h.fake, testProject(), testMRIID, []gitlab.MergeRequestVersion{
		{ID: 1, CreatedAt: "2026-08-11T12:00:00.000Z"},
		{ID: 2, CreatedAt: "2026-08-12T18:00:00.000Z"}, // 18h before testNow
	})

	out, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	msgs := h.digestMessages(t, out.RunID)
	var m slack.Message
	if err := json.Unmarshal(msgs[0].Payload, &m); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if text := renderedText(m); !strings.Contains(text, "|!481> · waiting 18h") {
		t.Errorf("digest does not report the wait since the last push:\n%s", text)
	}
}

func TestBuildDigestSurvivesADeadSlackDirectory(t *testing.T) {
	h := digestHarness(t, func(h *harness) {
		h.matcher = stubMatcher{err: errors.New("users.list is down")}
	})

	out, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("a dead Slack directory must not cost the team its digest: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want the digest to go out unmentioned", len(out.Messages))
	}
}

func TestWaitingFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		push time.Time
		want time.Duration
	}{
		{"eighteen hours ago", testNow.Add(-18 * time.Hour), 18 * time.Hour},
		{"unknown push time", time.Time{}, 0},
		// Clock skew between GitLab and us must not render "waiting -3s".
		{"in the future", testNow.Add(time.Hour), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := waitingFor(domain.MergeRequestSnapshot{LastPushAt: c.push}, testNow)
			if got != c.want {
				t.Errorf("waitingFor = %v, want %v", got, c.want)
			}
		})
	}
}

// TestOrderedPeopleSortsByDisplayName pins the order the digest's people are
// assembled in: alphabetical by the name the reader sees, workload ignored.
//
// The display name and not the map key, because the key is the GitLab username
// and the digest renders Slack mentions — an order derived from a string the
// reader never sees looks like no order at all. Workload used to decide this;
// the cost was that a person's position moved every slot, so nobody could find
// their own block without reading the whole digest.
func TestOrderedPeopleSortsByDisplayName(t *testing.T) {
	t.Parallel()
	people := map[string]*slack.PersonDigest{
		// The busiest person, and last alphabetically: workload must not rescue them
		// to the top.
		"zz": {
			Person:   slack.Mention{SlackID: "U3", Display: "Zoe Zimmer"},
			ToReview: []slack.ReviewItem{{}, {}}, Own: []slack.AuthorItem{{}},
		},
		// Lower-cased before comparing, so casing does not split the alphabet in two.
		"bb": {Person: slack.Mention{SlackID: "U2", Display: "bob Brown"}, ToReview: []slack.ReviewItem{{}}},
		"aa": {Person: slack.Mention{SlackID: "U1", Display: "Anna Adams"}, Own: []slack.AuthorItem{{}}},
	}
	got := orderedPeople(people)
	want := []string{"aa", "bb", "zz"}
	if !slices.Equal(got, want) {
		t.Errorf("orderedPeople = %v, want %v (display name ascending)", got, want)
	}
}

// TestOrderedPeopleFallsBackToTheKey: Display is empty only if the matcher
// returned nothing usable, and an order that collapsed those people into one
// bucket would reshuffle them between runs — the map is a map.
func TestOrderedPeopleFallsBackToTheKey(t *testing.T) {
	t.Parallel()
	people := map[string]*slack.PersonDigest{
		"nobody-c": {Person: slack.Mention{SlackID: "U3"}, Own: []slack.AuthorItem{{}}},
		"nobody-a": {Person: slack.Mention{SlackID: "U1"}, Own: []slack.AuthorItem{{}}},
		"nobody-b": {Person: slack.Mention{SlackID: "U2"}, Own: []slack.AuthorItem{{}}},
	}
	want := []string{"nobody-a", "nobody-b", "nobody-c"}
	// Repeated, because a single pass cannot tell a stable order from a lucky one:
	// Go randomises map iteration and the slice starts life in that order.
	for range 20 {
		if got := orderedPeople(people); !slices.Equal(got, want) {
			t.Fatalf("orderedPeople = %v, want %v (key ascending when no name resolved)", got, want)
		}
	}
}

func TestUserKey(t *testing.T) {
	t.Parallel()
	if got := userKey(domain.User{Username: " Reviewer "}); got != "reviewer" {
		t.Errorf("userKey = %q, want %q", got, "reviewer")
	}
	if got := userKey(domain.User{ID: 7}); got != "id:7" {
		t.Errorf("userKey without a username = %q, want %q", got, "id:7")
	}
}

func TestTeamStateCountsEachMROnce(t *testing.T) {
	t.Parallel()
	snap := domain.MergeRequestSnapshot{
		MR:      domain.MergeRequest{State: "opened", IID: 1},
		HeadSHA: "aaa",
		Reviewers: []domain.Reviewer{
			{User: domain.User{ID: 1, Username: "a"}, State: domain.ReviewStateUnreviewed},
			{User: domain.User{ID: 2, Username: "b"}, State: domain.ReviewStateUnreviewed},
		},
		Discussions:  []domain.Discussion{{ID: "d", Notes: []domain.Note{{Resolvable: true}}}},
		Mergeability: domain.Mergeability{HasConflicts: true, DetailedStatus: "conflict", Known: true},
		Pipeline:     domain.Pipeline{SHA: "aaa", Status: "failed", Known: true},
	}
	got := teamState([]domain.MergeRequestSnapshot{snap}, linearDigestState{})
	if got.WaitingHumanReview != 1 {
		t.Errorf("WaitingHumanReview = %d, want 1 — two idle reviewers are still one MR",
			got.WaitingHumanReview)
	}
	if got.UnresolvedThreads != 1 || got.Conflicts != 1 || got.FailedPipeline != 1 {
		t.Errorf("team state = %+v, want every counter at 1", got)
	}
	// This fixture never loaded approvals, so "unknown" is the honest count.
	if got.ApprovalsUnknown != 1 {
		t.Errorf("ApprovalsUnknown = %d, want 1", got.ApprovalsUnknown)
	}
}

// The two gauges added with the readiness gate answer questions a dashboard
// could not ask before: "why is the review queue empty" and "are we blind to
// approvals". Both must be per merge request, and LinearNotReady must be the
// mutually exclusive counterpart of WaitingHumanReview.
func TestTeamStateCountsBoardGatedAndApprovalBlindMergeRequests(t *testing.T) {
	t.Parallel()
	mr := func(iid int64) domain.MergeRequest {
		return domain.MergeRequest{State: "opened", IID: iid}
	}
	reviewers := []domain.Reviewer{{User: domain.User{ID: 1, Username: "a"}, State: domain.ReviewStateUnreviewed}}
	gated := domain.MergeRequestSnapshot{
		Project: domain.Project{ID: 1}, MR: mr(1), Reviewers: reviewers, ApprovalsKnown: true,
	}
	waiting := domain.MergeRequestSnapshot{
		Project: domain.Project{ID: 1}, MR: mr(2), Reviewers: reviewers, ApprovalsKnown: true,
	}
	blind := domain.MergeRequestSnapshot{
		Project: domain.Project{ID: 1}, MR: mr(3), Reviewers: reviewers,
	}
	state := linearDigestState{enabled: true, linksByMR: map[string]linearLink{
		snapshotKey(1, 1): {issue: linear.Issue{Identifier: "CHAIN-1"}, stage: linear.StageBeforeReview},
		snapshotKey(1, 2): {issue: linear.Issue{Identifier: "CHAIN-2"}, stage: linear.StageReviewOrLater},
	}}

	got := teamState([]domain.MergeRequestSnapshot{gated, waiting, blind}, state)
	if got.LinearNotReady != 1 {
		t.Errorf("LinearNotReady = %d, want 1", got.LinearNotReady)
	}
	if got.WaitingHumanReview != 2 {
		t.Errorf("WaitingHumanReview = %d, want 2 — the gated one is not waiting on a reviewer", got.WaitingHumanReview)
	}
	if got.ApprovalsUnknown != 1 {
		t.Errorf("ApprovalsUnknown = %d, want 1", got.ApprovalsUnknown)
	}
}

func TestTeamStateReportsZerosSoGaugesCannotGoStale(t *testing.T) {
	t.Parallel()
	// The gauges are overwritten every pass; a team that drops to zero has to
	// report zero rather than keep its last non-zero value forever.
	if got := teamState(nil, linearDigestState{}); got != (metrics.TeamState{}) {
		t.Errorf("teamState(nil) = %+v, want every counter at 0", got)
	}
}

// TestBuildDigestDryRunOfAnEmptyTeamStillJournalsTheRun keeps the dry-run
// preview path honest when there is nothing to preview.
func TestBuildDigestDryRunOfAnEmptyTeamStillJournalsTheRun(t *testing.T) {
	h := newHarness(t, withDB)
	empty := domain.Team{Name: "empty", SlackChannel: "C000", Repositories: nil}

	out, err := h.svc.BuildDigest(t.Context(), empty, "17:30", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if out.Status != DigestDryRun {
		t.Errorf("status = %q, want %q", out.Status, DigestDryRun)
	}
	if len(out.Messages) != 0 || out.MRCount != 0 {
		t.Errorf("outcome = %+v, want an empty digest", out)
	}

	// Re-running the same slot returns the same run without rebuilding, and a
	// dry run still exposes no delivery jobs.
	again, err := h.svc.BuildDigest(t.Context(), empty, "17:30", digestDay, 0)
	if err != nil {
		t.Fatalf("second BuildDigest: %v", err)
	}
	if again.RunID != out.RunID {
		t.Errorf("run ids differ (%s vs %s)", again.RunID, out.RunID)
	}
	if len(again.Messages) != 0 {
		t.Errorf("messages = %v, want none for a dry run", again.Messages)
	}
}

// TestBuildDigestScheduledSlotIsIdempotentAfterAForcedRepeat pins the collision
// the attempt-addressed lookup closes.
//
// Asking for "the latest run of this slot" answers with the forced attempt 1,
// so a retry of the scheduled attempt 0 looks like a run that never happened
// and tries to insert a second attempt 0 — straight into
// digest_runs_slot_uniq. Addressing the attempt explicitly recognises it.
func TestBuildDigestScheduledSlotIsIdempotentAfterAForcedRepeat(t *testing.T) {
	h := digestHarness(t)

	scheduled, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("scheduled BuildDigest: %v", err)
	}
	forced, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 1)
	if err != nil {
		t.Fatalf("forced BuildDigest: %v", err)
	}
	if forced.RunID == scheduled.RunID {
		t.Fatal("--force must create its own run")
	}

	// The scheduled job is retried (a crash after the commit, a rescued River
	// job) while the forced run is the slot's newest.
	retried, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("retrying the scheduled run must not collide with the forced one: %v", err)
	}
	if retried.RunID != scheduled.RunID {
		t.Errorf("retry returned run %s, want the scheduled run %s", retried.RunID, scheduled.RunID)
	}
	if n := h.count(t, `SELECT count(*) FROM digest_runs WHERE team = $1 AND slot = '09:00'`, testTeam); n != 2 {
		t.Errorf("digest_runs rows = %d, want exactly one per attempt", n)
	}
}

// TestPreviewDigestWritesNothing is the whole contract of `digest --dry-run`:
// the same digest a build would produce, with no trace of it anywhere. A
// preview that filed a digest_runs row would consume the slot the scheduled run
// needs, and the unique index would then reject that run rather than the
// preview — the failure would land on the wrong side.
func TestPreviewDigestWritesNothing(t *testing.T) {
	h := digestHarness(t, func(h *harness) {
		h.matcher = stubMatcher{results: map[string]match.Result{
			"reviewer": {Status: match.Matched, SlackID: "U42", Display: "Rita"},
			"author":   {Status: match.Matched, SlackID: "U01", Display: "Ann"},
		}}
	})

	preview, err := h.svc.PreviewDigest(t.Context(), testTeamConfig())
	if err != nil {
		t.Fatalf("PreviewDigest: %v", err)
	}
	if len(preview.Messages) != 1 {
		t.Fatalf("messages = %d, want the digest rendered", len(preview.Messages))
	}
	if preview.MRCount != 1 {
		t.Errorf("mr count = %d, want 1", preview.MRCount)
	}
	// The data travels with the messages so a caller can map a Slack id back onto
	// the person it stands for; a terminal cannot render <@U42>.
	if len(preview.Data.People) != 2 {
		t.Errorf("people = %d, want the reviewer and the author", len(preview.Data.People))
	}
	text := renderedText(preview.Messages[0])
	for _, want := range []string{"<@U42>", "<@U01>", "!481"} {
		if !strings.Contains(text, want) {
			t.Errorf("preview is missing %q:\n%s", want, text)
		}
	}

	if len(h.slack.posts) != 0 {
		t.Errorf("chat.postMessage was called %d times by a preview", len(h.slack.posts))
	}
	for _, table := range []string{"digest_runs", "digest_messages"} {
		var n int
		if err := h.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s has %d row(s); a preview must persist nothing", table, n)
		}
	}
}

// TestPreviewDigestRendersWhatTheBuildWouldSend: the preview is only worth
// running if it is the same digest. Both paths render through renderDigest, and
// this is what keeps that true — a second renderer added for the CLI would
// eventually disagree with the one that ships.
func TestPreviewDigestRendersWhatTheBuildWouldSend(t *testing.T) {
	h := digestHarness(t)

	preview, err := h.svc.PreviewDigest(t.Context(), testTeamConfig())
	if err != nil {
		t.Fatalf("PreviewDigest: %v", err)
	}
	out, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	msgs := h.digestMessages(t, out.RunID)
	if len(msgs) != len(preview.Messages) {
		t.Fatalf("parts = %d built, %d previewed", len(msgs), len(preview.Messages))
	}
	for i, row := range msgs {
		var built slack.Message
		if err := json.Unmarshal(row.Payload, &built); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if got, want := renderedText(preview.Messages[i]), renderedText(built); got != want {
			t.Errorf("part %d differs:\npreview: %q\nbuilt:   %q", i+1, got, want)
		}
	}
}

// TestPreviewDigestReportsDegradedSources: a preview has no status column to
// record a partial run in, so it hands the failures back to the caller. Reading
// "3 merge requests" without knowing that half the repositories were unreachable
// is how a preview lies without saying anything false.
func TestPreviewDigestReportsDegradedSources(t *testing.T) {
	h := digestHarness(t)
	team := testTeamConfig()
	team.Repositories = append(team.Repositories, "backend/broken")
	h.gl.failProject[url.PathEscape("backend/broken")] = &gitlab.APIError{
		Status: 500, Method: "GET", Path: "/projects/backend%2Fbroken",
	}

	preview, err := h.svc.PreviewDigest(t.Context(), team)
	if err != nil {
		t.Fatalf("PreviewDigest: %v", err)
	}
	if preview.FailedRepos != 1 {
		t.Errorf("failed repos = %d, want 1", preview.FailedRepos)
	}
	if !strings.Contains(renderedText(preview.Messages[0]), "Partial data") {
		t.Errorf("the message itself must carry the partial-data warning:\n%s",
			renderedText(preview.Messages[0]))
	}
}

// TestPreviewDigestFailsWhenEverySourceDid: the one case where there is nothing
// to show. It is an error rather than an empty digest for the same reason
// BuildDigest refuses to record one — "nobody owes anything" and "we could not
// look" must not render identically.
func TestPreviewDigestFailsWhenEverySourceDid(t *testing.T) {
	h := digestHarness(t)
	h.gl.failOpenMRs = errors.New("gitlab is down")

	if _, err := h.svc.PreviewDigest(t.Context(), testTeamConfig()); err == nil {
		t.Fatal("a preview with no readable source must fail, not render an empty digest")
	}
}

// TestDigestNamesAnUntaggedMergeRequest: an MR whose author forgot to tag anyone
// is in no reviewer's queue, so before this rule it appeared in neither section
// of the digest — the one list of everything the team owes said nothing about
// work nobody had been handed.
func TestDigestNamesAnUntaggedMergeRequest(t *testing.T) {
	// The matcher goes in as an option: harness options run before the service is
	// built, and assigning h.matcher afterwards leaves the service holding the
	// default one.
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }),
		func(h *harness) {
			h.matcher = stubMatcher{results: map[string]match.Result{
				"author": {Status: match.Matched, SlackID: "U01", Display: "Ann"},
			}}
		})

	proj := testProject()
	// No reviewers, no approvals, and nothing else wrong with it: the row exists
	// only because nobody was asked.
	seedGitLab(h.fake, proj, testMR(testMRIID))

	p, err := h.svc.PreviewDigest(t.Context(), testTeamConfig())
	if err != nil {
		t.Fatalf("PreviewDigest: %v", err)
	}
	if p.MRCount != 1 {
		t.Fatalf("mr count = %d, want the untagged merge request counted", p.MRCount)
	}
	if len(p.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(p.Messages))
	}
	body := renderedText(p.Messages[0])
	for _, want := range []string{"add a reviewer", "your MR ", "<@U01>"} {
		if !strings.Contains(body, want) {
			t.Errorf("digest does not contain %q:\n%s", want, body)
		}
	}
}

// TestDigestStaysQuietAboutATaggedMergeRequest is the other half: the rule must
// not fire on an MR that simply has not been reviewed yet. That one is already
// in its reviewer's queue, and telling the author to add a reviewer would ask
// them to fix something that is not broken.
func TestDigestStaysQuietAboutATaggedMergeRequest(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }),
		func(h *harness) {
			h.matcher = stubMatcher{results: map[string]match.Result{
				"reviewer": {Status: match.Matched, SlackID: "U42", Display: "Rita"},
				"author":   {Status: match.Matched, SlackID: "U01", Display: "Ann"},
			}}
		})

	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer", Name: "Rita Reviewer"}
	seedGitLab(h.fake, proj, testMR(testMRIID, withReviewers(reviewer)))

	p, err := h.svc.PreviewDigest(t.Context(), testTeamConfig())
	if err != nil {
		t.Fatalf("PreviewDigest: %v", err)
	}
	if len(p.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(p.Messages))
	}
	if body := renderedText(p.Messages[0]); strings.Contains(body, "add a reviewer") {
		t.Errorf("a tagged merge request must not ask for a reviewer:\n%s", body)
	}
}

// TestBuildDigestReportsAReusedRun: every branch of BuildDigest returns a
// DigestOutcome and a nil error, so a caller cannot tell "assembled just now"
// from "found the one delivered three hours ago". DigestWorker's summary line
// reads on Reused; without it, it announced "digest built" directly under the
// service's own "already built for this slot; reusing it".
func TestBuildDigestReportsAReusedRun(t *testing.T) {
	h := digestHarness(t)

	first, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if first.Reused {
		t.Error("the first build of a slot assembled it; Reused must be false")
	}

	second, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest (again): %v", err)
	}
	if !second.Reused {
		t.Error("the second call found the run rather than building it; Reused must be true")
	}
	if second.RunID != first.RunID {
		t.Errorf("run id = %v, want the first run %v", second.RunID, first.RunID)
	}
	// BuiltAt is the field that makes the worker's line an answer: at 17:03,
	// "the 14:00 slot was assembled at 14:45". Zero on a fresh build, where the
	// log line's own timestamp already says it.
	if second.BuiltAt.IsZero() {
		t.Error("a reused run must carry when it was built")
	}
	if !first.BuiltAt.IsZero() {
		t.Errorf("BuiltAt = %v on a fresh build, want the zero time", first.BuiltAt)
	}
	// Parts comes from the row, not from Messages: Messages is deliberately empty
	// on a dry run, so counting it reported "parts 0" for a digest that has them.
	if first.Parts != 1 || second.Parts != 1 {
		t.Errorf("parts = %d then %d, want 1 both times", first.Parts, second.Parts)
	}
}

// TestBuildDigestReportsPartsForADryRun is the half the reuse test cannot see:
// a dry run persists its parts and leaves Messages empty on purpose, so a caller
// counting Messages reports a digest with no parts.
func TestBuildDigestReportsPartsForADryRun(t *testing.T) {
	h := digestHarness(t, withConfig(func(c *Config) { c.SlackSendEnabled = false }))

	out, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if len(out.Messages) != 0 {
		t.Fatalf("messages = %d, want none queued for a dry run", len(out.Messages))
	}
	if out.Parts != 1 {
		t.Errorf("parts = %d, want 1: the run has a part, it is simply not being delivered", out.Parts)
	}
}
