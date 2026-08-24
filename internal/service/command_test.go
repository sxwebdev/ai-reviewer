package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/match"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

const testResponseURL = "https://hooks.slack.com/commands/T1/B2/C3"

// commandHarness is the digest harness with both people resolved to Slack
// accounts, which is what a command needs: the caller is identified by their
// Slack id and nothing else.
func commandHarness(t *testing.T) *harness {
	t.Helper()
	return digestHarness(t, func(h *harness) {
		h.matcher = stubMatcher{results: map[string]match.Result{
			"reviewer": {Status: match.Matched, SlackID: "U42", Display: "Rita"},
			"author":   {Status: match.Matched, SlackID: "U01", Display: "Ann"},
		}}
	})
}

// commandText joins everything one command posted, so an assertion can ask what
// the caller ended up seeing.
func commandText(h *harness) string {
	var b strings.Builder
	for _, r := range h.slack.responses {
		b.WriteString(r.Msg.Text)
		for _, blk := range r.Msg.Blocks {
			if blk.Text != nil {
				b.WriteString("\n")
				b.WriteString(blk.Text.Text)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// TestSlackCommandTeamAnswersTheChannel: /all posts the digest everybody
// already gets, to the channel it was asked in.
func TestSlackCommandTeamAnswersTheChannel(t *testing.T) {
	h := commandHarness(t)

	err := h.svc.RunSlackCommand(t.Context(), SlackCommandRequest{
		Team: testTeamConfig(), Scope: ScopeTeam,
		SlackUserID: "U42", ResponseURL: testResponseURL,
	})
	if err != nil {
		t.Fatalf("RunSlackCommand: %v", err)
	}
	if len(h.slack.responses) != 1 {
		t.Fatalf("responses = %d, want one", len(h.slack.responses))
	}
	got := h.slack.responses[0]
	if got.URL != testResponseURL {
		t.Errorf("posted to %q, want the command's own response URL", got.URL)
	}
	if got.Msg.ResponseType != slack.ResponseInChannel {
		t.Errorf("response_type = %q, want %q — the team digest is for the channel",
			got.Msg.ResponseType, slack.ResponseInChannel)
	}
	text := commandText(h)
	for _, want := range []string{"<@U42>", "<@U01>", "!481"} {
		if !strings.Contains(text, want) {
			t.Errorf("the answer is missing %q:\n%s", want, text)
		}
	}
	// A command is a question. Answering it must not consume the slot the
	// scheduled run needs.
	assertNoDigestRows(t, h)
	if len(h.slack.posts) != 0 {
		t.Errorf("chat.postMessage was called %d times; a command answers through its response URL",
			len(h.slack.posts))
	}
}

// TestSlackCommandMineIsPrivateAndFiltered: the personal digest goes to the
// caller only, and carries nobody else's rows.
func TestSlackCommandMineIsPrivateAndFiltered(t *testing.T) {
	h := commandHarness(t)

	err := h.svc.RunSlackCommand(t.Context(), SlackCommandRequest{
		Team: testTeamConfig(), Scope: ScopeMine,
		SlackUserID: "U42", ResponseURL: testResponseURL,
	})
	if err != nil {
		t.Fatalf("RunSlackCommand: %v", err)
	}
	if len(h.slack.responses) != 1 {
		t.Fatalf("responses = %d, want one", len(h.slack.responses))
	}
	// Ephemeral is Slack's default and the zero value here: a personal queue
	// posted to the channel is the failure this scope exists to avoid.
	if got := h.slack.responses[0].Msg.ResponseType; got != "" {
		t.Errorf("response_type = %q, want ephemeral", got)
	}
	text := commandText(h)
	if !strings.Contains(text, "<@U42>") {
		t.Errorf("the caller's own rows are missing:\n%s", text)
	}
	if strings.Contains(text, "<@U01>") {
		t.Errorf("another person's rows leaked into a personal answer:\n%s", text)
	}
	assertNoDigestRows(t, h)
}

// TestSlackCommandMineNamesBothReasonsForAnEmptyAnswer: the service cannot tell
// "nothing is waiting on you" from "your Slack account is not mapped to your
// GitLab one" — an unmatched person is simply absent from the digest. Reporting
// only the first would tell a reviewer with three merge requests waiting that
// they are clear.
func TestSlackCommandMineNamesBothReasonsForAnEmptyAnswer(t *testing.T) {
	h := commandHarness(t)

	err := h.svc.RunSlackCommand(t.Context(), SlackCommandRequest{
		Team: testTeamConfig(), Scope: ScopeMine,
		SlackUserID: "U404", ResponseURL: testResponseURL,
	})
	if err != nil {
		t.Fatalf("RunSlackCommand: %v", err)
	}
	if len(h.slack.responses) != 1 {
		t.Fatalf("responses = %d, want one notice", len(h.slack.responses))
	}
	text := h.slack.responses[0].Msg.Text
	if !strings.Contains(text, "Nothing is waiting on you") {
		t.Errorf("notice = %q, want the healthy reading first", text)
	}
	if !strings.Contains(text, "user_map") {
		t.Errorf("notice = %q, want the unmatched-account reading named too", text)
	}
	if h.slack.responses[0].Msg.ResponseType != "" {
		t.Error("a personal notice must stay ephemeral")
	}
}

// TestSlackCommandOnAQuietTeamSaysSo: an empty digest is a legitimate answer and
// silence is not — from Slack, a command that posts nothing is indistinguishable
// from one that failed.
func TestSlackCommandOnAQuietTeamSaysSo(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	proj := testProject()
	// One merge request that needs nothing from anybody: no reviewers, no
	// threads, no conflicts, no pipeline.
	seedGitLab(h.fake, proj, testMR(testMRIID))

	err := h.svc.RunSlackCommand(t.Context(), SlackCommandRequest{
		Team: testTeamConfig(), Scope: ScopeTeam,
		SlackUserID: "U42", ResponseURL: testResponseURL,
	})
	if err != nil {
		t.Fatalf("RunSlackCommand: %v", err)
	}
	if len(h.slack.responses) != 1 {
		t.Fatalf("responses = %d, want one notice", len(h.slack.responses))
	}
	if got := h.slack.responses[0].Msg.Text; !strings.Contains(got, "Nothing to report") {
		t.Errorf("notice = %q, want an explicit empty answer", got)
	}
}

// TestSlackCommandReportsABuildFailureToTheCaller: from a Slack channel a failed
// command and a slow one look identical, so the failure has to arrive as a
// message and not only as an error on the queue.
func TestSlackCommandReportsABuildFailureToTheCaller(t *testing.T) {
	h := commandHarness(t)
	h.gl.failOpenMRs = errors.New("gitlab is down")

	err := h.svc.RunSlackCommand(t.Context(), SlackCommandRequest{
		Team: testTeamConfig(), Scope: ScopeTeam,
		SlackUserID: "U42", ResponseURL: testResponseURL,
	})
	if err == nil {
		t.Fatal("a command whose digest could not be built must report an error")
	}
	if len(h.slack.responses) != 1 {
		t.Fatalf("responses = %d, want the caller told", len(h.slack.responses))
	}
	if got := h.slack.responses[0].Msg.Text; !strings.Contains(got, "Could not build") {
		t.Errorf("notice = %q, want it to say the digest could not be built", got)
	}
}

// TestSlackCommandRejectsAnUnknownScope: the scope arrives from a durable job
// argument, so a value this version does not recognise is representable —
// and must be refused rather than silently treated as one of the two.
func TestSlackCommandRejectsAnUnknownScope(t *testing.T) {
	h := commandHarness(t)

	err := h.svc.RunSlackCommand(t.Context(), SlackCommandRequest{
		Team: testTeamConfig(), Scope: CommandScope("everyone"),
		SlackUserID: "U42", ResponseURL: testResponseURL,
	})
	if err == nil {
		t.Fatal("an unknown scope must be refused")
	}
	if len(h.slack.responses) != 0 {
		t.Errorf("nothing may be posted for a scope the service does not understand: %+v", h.slack.responses)
	}
}

// TestSlackCommandTruncatesWithinSlacksBudget: a response URL takes five
// messages. A digest longer than that is cut — and says so, because "your
// digest" and "most of your digest" have to be distinguishable.
func TestSlackCommandTruncatesWithinSlacksBudget(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) { c.SlackSendEnabled = true }))
	h.matcher = stubMatcher{results: map[string]match.Result{
		"reviewer": {Status: match.Matched, SlackID: "U42", Display: "Rita"},
	}}

	proj := testProject()
	// Every merge request gets its own reviewer, so the digest is one person
	// block per merge request and pagination has something to split.
	var mrs []*gitlab.MergeRequest
	for i := range 300 {
		iid := int64(1000 + i)
		reviewer := gitlab.User{ID: int64(1000 + i), Username: "r" + string(rune('a'+i%26)) + string(rune('a'+i/26))}
		mrs = append(mrs, testMR(iid, withReviewers(reviewer)))
	}
	seedGitLab(h.fake, proj, mrs...)

	err := h.svc.RunSlackCommand(t.Context(), SlackCommandRequest{
		Team: testTeamConfig(), Scope: ScopeTeam,
		SlackUserID: "U42", ResponseURL: testResponseURL,
	})
	if err != nil {
		t.Fatalf("RunSlackCommand: %v", err)
	}
	if n := len(h.slack.responses); n != slack.MaxCommandResponses {
		t.Fatalf("responses = %d, want Slack's cap of %d", n, slack.MaxCommandResponses)
	}
	last := h.slack.responses[len(h.slack.responses)-1].Msg
	if !strings.Contains(last.Text, "not shown") {
		t.Errorf("the cut must be visible; last message = %q", last.Text)
	}
}

// TestSlackCommandTeamResolvesByChannel: a team owns exactly one channel, so
// the channel is the whole answer to "whose digest".
func TestSlackCommandTeamResolvesByChannel(t *testing.T) {
	t.Parallel()

	payments := domain.Team{Name: "payments", SlackChannel: "C123"}
	checkout := domain.Team{Name: "checkout", SlackChannel: "C999"}
	h := newHarness(t, withConfig(func(c *Config) { c.Teams = []domain.Team{payments, checkout} }))

	if team, ok := h.svc.SlackCommandTeam("C999"); !ok || team.Name != "checkout" {
		t.Errorf("SlackCommandTeam(C999) = %q, %t; want checkout", team.Name, ok)
	}
	// A channel nobody owns, on a multi-team deployment: refused rather than
	// answered with somebody else's queue.
	if team, ok := h.svc.SlackCommandTeam("D-dm-with-the-bot"); ok {
		t.Errorf("SlackCommandTeam(unknown) = %q, want a refusal on a multi-team deployment", team.Name)
	}
}

// TestSlackCommandTeamFallsBackOnASingleTeam: with one team configured there is
// nothing else a command could mean, so a DM works too.
func TestSlackCommandTeamFallsBackOnASingleTeam(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withConfig(func(c *Config) {
		c.Teams = []domain.Team{{Name: "payments", SlackChannel: "C123"}}
	}))
	if team, ok := h.svc.SlackCommandTeam("D-dm-with-the-bot"); !ok || team.Name != "payments" {
		t.Errorf("SlackCommandTeam(dm) = %q, %t; want the only configured team", team.Name, ok)
	}
	if _, ok := h.svc.SlackCommandTeam(""); !ok {
		t.Error("an empty channel must still resolve on a one-team deployment")
	}
}

// assertNoDigestRows is the contract a command shares with `digest --dry-run`:
// it answers a question and records nothing.
func assertNoDigestRows(t *testing.T, h *harness) {
	t.Helper()
	for _, table := range []string{"digest_runs", "digest_messages"} {
		var n int
		if err := h.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s has %d row(s); answering a command must persist nothing", table, n)
		}
	}
}
