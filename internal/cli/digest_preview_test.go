package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// previewFixture is one team's preview with a resolved mention and an
// unresolved one, which is the pair every rendering rule here turns on.
func previewFixture() *service.DigestPreview {
	data := slack.DigestData{
		Team: "payments", Project: "payments",
		LinearEnabled: true, LinearInReviewCount: 3,
		People: []slack.PersonDigest{
			{
				Person:   slack.Mention{SlackID: "U42", Display: "Rita Reviewer"},
				ToReview: []slack.ReviewItem{{IID: 481, Title: "Add payment retries", WebURL: "https://gl/481"}},
			},
			{
				Person: slack.Mention{Display: "Ann Author (@ann)"},
				Own: []slack.AuthorItem{{
					IID: 475, Title: "Cache invalidation", WebURL: "https://gl/475",
					ChangesRequestedBy: []slack.Mention{{SlackID: "U9", Display: "Bob Bobson"}},
					UnresolvedThreads:  2,
				}},
			},
		},
	}
	return &service.DigestPreview{
		Team: domain.Team{Name: "payments", SlackChannel: "#dev-review", Repositories: []string{"backend/payments"}},
		Data: data, Messages: slack.BuildDigest(data),
		MRCount: 2, LinearIssueCount: 3,
	}
}

func preview(t *testing.T, p *service.DigestPreview, asJSON bool) string {
	t.Helper()
	return previewWithSkip(t, p, "", asJSON)
}

func previewWithSkip(t *testing.T, p *service.DigestPreview, skipped string, asJSON bool) string {
	t.Helper()
	var b strings.Builder
	if err := printDigestPreview(&b, p, skipped, asJSON); err != nil {
		t.Fatalf("printDigestPreview: %v", err)
	}
	return b.String()
}

// TestPreviewNamesEveryMention: Slack draws "<@U42>" as a person, a terminal
// draws it as an id, and a preview nobody can read is not a preview. The names
// come from the digest's own data, so the substitution can never invent one.
func TestPreviewNamesEveryMention(t *testing.T) {
	t.Parallel()

	out := preview(t, previewFixture(), false)
	for _, want := range []string{"@Rita Reviewer", "@Bob Bobson", "Ann Author (@ann)"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing from the preview:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<@U") {
		t.Errorf("a raw Slack id survived into the rendered preview:\n%s", out)
	}
	// The dry run says so on every run: it is the whole reason somebody is
	// allowed to run this against production without thinking twice.
	if !strings.Contains(out, "nothing was written to the database and nothing was sent to Slack") {
		t.Errorf("the preview does not say it changed nothing:\n%s", out)
	}
	if !strings.Contains(out, "#dev-review") {
		t.Errorf("the preview does not name the channel it would have posted to:\n%s", out)
	}
}

// TestPreviewJSONIsTheUntouchedPayload: --json exists for the reader checking
// what Slack would receive, so it may not carry the display-name substitution
// the rendered form applies.
func TestPreviewJSONIsTheUntouchedPayload(t *testing.T) {
	t.Parallel()

	out := preview(t, previewFixture(), true)
	if !strings.Contains(out, "<@U42>") {
		t.Errorf("--json must print the payload Slack gets, mentions included:\n%s", out)
	}
	if strings.Contains(out, "@Rita Reviewer") {
		t.Errorf("--json must not rewrite the payload:\n%s", out)
	}

	var req slack.PostMessageRequest
	body := out[strings.Index(out, "{"):]
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("the printed payload is not valid JSON: %v\n%s", err, out)
	}
	if req.Channel != "#dev-review" {
		t.Errorf("channel = %q, want the team's own", req.Channel)
	}
	if len(req.Blocks) == 0 {
		t.Error("the payload carries no blocks")
	}
}

// TestPreviewOfAnEmptyDigestSaysSo: no messages is a legitimate outcome — nobody
// owes anything — and printing a bare header for it reads like a failure.
func TestPreviewOfAnEmptyDigestSaysSo(t *testing.T) {
	t.Parallel()

	out := preview(t, &service.DigestPreview{
		Team: domain.Team{Name: "payments", SlackChannel: "#dev-review"},
	}, false)
	if !strings.Contains(out, "nothing to report") {
		t.Errorf("an empty digest must say so:\n%s", out)
	}
}

// TestPreviewReportsDegradedSources: the message body says "partial data", never
// which source failed. A preview is where somebody is trying to find out.
func TestPreviewReportsDegradedSources(t *testing.T) {
	t.Parallel()

	p := previewFixture()
	p.FailedRepos = 1
	p.LinearErr = errors.New("not reachable")

	out := preview(t, p, false)
	for _, want := range []string{"1 of 1 repositories could not be inspected", "linear: not reachable"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing from the preview:\n%s", want, out)
		}
	}
}

// TestPreviewDistinguishesAnUnconfiguredLinear: a team without linear_team_ids
// has no count, and printing 0 claims a board that was never read.
func TestPreviewDistinguishesAnUnconfiguredLinear(t *testing.T) {
	t.Parallel()

	p := previewFixture()
	p.Data.LinearEnabled = false
	p.LinearIssueCount = 0
	if out := preview(t, p, false); !strings.Contains(out, "Linear In Review: n/a") {
		t.Errorf("an unread board must not render as zero:\n%s", out)
	}
}

// TestPreviewRendersLinksTheWaySlackDraws: a row is two characters of label
// behind ninety of URL, and the wording this command exists to check is what
// gets pushed off the screen. The URLs are still one --json away.
func TestPreviewRendersLinksTheWaySlackDraws(t *testing.T) {
	t.Parallel()

	out := preview(t, previewFixture(), false)
	if strings.Contains(out, "https://gl/481") {
		t.Errorf("the rendered preview still carries raw link markup:\n%s", out)
	}
	for _, want := range []string{"review !481", "your MR !475"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing — the label must survive the link:\n%s", want, out)
		}
	}
}

// TestPlainTextLeavesEscapedMarkupAlone: the builder escapes "<" so Slack does
// not read a title as markup. Unescaping before the link pass would hand it
// exactly that.
func TestPlainTextLeavesEscapedMarkupAlone(t *testing.T) {
	t.Parallel()

	got := plainText("&lt;https://evil|click&gt; and <https://gl/1|!1> and Ann &amp; Bob",
		strings.NewReplacer())
	want := "<https://evil|click> and !1 and Ann & Bob"
	if got != want {
		t.Errorf("plainText = %q, want %q", got, want)
	}
}

// TestPlainTextKeepsABareURL: the digest builds no bare links today, but a
// dropped label must leave the URL rather than an empty gap.
func TestPlainTextKeepsABareURL(t *testing.T) {
	t.Parallel()

	if got := plainText("see <https://gl/1>", strings.NewReplacer()); got != "see https://gl/1" {
		t.Errorf("plainText = %q, want the URL kept", got)
	}
}

// TestPreviewSaysWhenTheDayIsSkipped: "why is the channel quiet today" is the
// question a preview is run to answer, and a digest that reads perfectly normal
// while the schedule was never going to fire answers it wrongly.
func TestPreviewSaysWhenTheDayIsSkipped(t *testing.T) {
	t.Parallel()

	const note = "Saturday 2026-08-22 is a skipped day (weekdays Saturday, Sunday): " +
		"the scheduled digest would not be sent"
	out := previewWithSkip(t, previewFixture(), note, false)
	if !strings.Contains(out, note) {
		t.Errorf("the preview does not mention the skipped day:\n%s", out)
	}
	// An ordinary day says nothing: a note on every run is a note nobody reads.
	if strings.Contains(preview(t, previewFixture(), false), "skipped day") {
		t.Error("an ordinary day must not carry a skip note")
	}
}
