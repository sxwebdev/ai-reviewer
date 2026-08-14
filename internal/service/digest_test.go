package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/match"
	"github.com/sxwebdev/ai-reviewer/internal/metrics"
	"github.com/sxwebdev/ai-reviewer/internal/models"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

var digestDay = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

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
	for _, want := range []string{"<@U42>", "<@U01>", "payments", "!481", "unresolved", "merge conflicts", "pipeline failed", "https://pipelines/5"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("digest is missing %q:\n%s", want, rendered)
		}
	}
}

// renderedText concatenates every section of a message for substring checks.
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
	if text := renderedText(m); !strings.Contains(text, "waiting 18h") {
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
	got := teamState([]domain.MergeRequestSnapshot{snap})
	if got.WaitingHumanReview != 1 {
		t.Errorf("WaitingHumanReview = %d, want 1 — two idle reviewers are still one MR",
			got.WaitingHumanReview)
	}
	if got.UnresolvedThreads != 1 || got.Conflicts != 1 || got.FailedPipeline != 1 {
		t.Errorf("team state = %+v, want every counter at 1", got)
	}
}

func TestTeamStateReportsZerosSoGaugesCannotGoStale(t *testing.T) {
	t.Parallel()
	// The gauges are overwritten every pass; a team that drops to zero has to
	// report zero rather than keep its last non-zero value forever.
	if got := teamState(nil); got != (metrics.TeamState{}) {
		t.Errorf("teamState(nil) = %+v, want every counter at 0", got)
	}
}

// TestBuildDigestDryRunOfAnEmptyTeamStillJournalsTheRun keeps the dry-run
// preview path honest when there is nothing to preview.
func TestBuildDigestDryRunOfAnEmptyTeamStillJournalsTheRun(t *testing.T) {
	h := newHarness(t, withDB)
	empty := domain.Team{Name: "empty", SlackChannel: "C000", Repositories: nil}

	out, err := h.svc.BuildDigest(t.Context(), empty, "16:30", digestDay, 0)
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
	again, err := h.svc.BuildDigest(t.Context(), empty, "16:30", digestDay, 0)
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
