package slack_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// sampleDigest is a two-project digest: one person owing reviews, two owing work
// on their own merge requests, and one person on both hooks.
func sampleDigest() slack.DigestData {
	return slack.DigestData{
		Team: "Payments",
		People: []slack.PersonDigest{
			{
				Person: slack.Mention{SlackID: "U123"},
				ToReview: []slack.ReviewItem{
					{
						Project: "payments", IID: 481, Title: "Add payment retries",
						WebURL: "https://gl/payments/-/merge_requests/481", Waiting: 18 * time.Hour,
					},
					{
						Project: "billing", IID: 932, Title: "Invoice export",
						WebURL: "https://gl/billing/-/merge_requests/932", Waiting: 5 * time.Hour,
					},
				},
			},
			{
				Person: slack.Mention{SlackID: "U456"},
				Own: []slack.AuthorItem{{
					Project: "payments", IID: 475, Title: "Cache invalidation",
					WebURL:            "https://gl/payments/-/merge_requests/475",
					UnresolvedThreads: 3, MergeConflicts: true, PipelineFailed: true,
					PipelineWebURL: "https://gl/payments/-/pipelines/9001",
				}},
			},
			{
				Person: slack.Mention{SlackID: "U789"},
				Own: []slack.AuthorItem{{
					Project: "checkout", IID: 122, Title: "Search filters",
					WebURL:         "https://gl/checkout/-/merge_requests/122",
					PipelineFailed: true,
					PipelineWebURL: "https://gl/checkout/-/pipelines/42",
				}},
			},
		},
	}
}

func rendered(m slack.Message) string {
	var b strings.Builder
	for _, block := range m.Blocks {
		if block.Text != nil {
			b.WriteString(block.Text.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func TestLinearSummaryAndMoveAction(t *testing.T) {
	t.Parallel()
	d := slack.DigestData{
		Team:                "payments",
		LinearEnabled:       true,
		LinearInReviewCount: 8,
		People: []slack.PersonDigest{{
			Person: slack.Mention{Display: "Ann & Bob"},
			Own: []slack.AuthorItem{{
				IID: 42, Title: "PAY-42 <checkout>", WebURL: "https://gitlab/42",
				MoveLinear: true, LinearIdentifier: "PAY-42", LinearWebURL: "https://linear.app/PAY-42",
			}},
		}},
	}
	msgs := slack.BuildDigest(d)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	text := rendered(msgs[0])
	for _, want := range []string{
		"Linear · In Review: 8", "Ann &amp; Bob", "move <https://linear.app/PAY-42|PAY-42> forward in Linear",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("render is missing %q:\n%s", want, text)
		}
	}
}

// The row that keeps a board-gated merge request in the digest. It has to name
// the column, because "not in review" leaves the author unable to tell whether to
// move the card or close the merge request.
func TestLinearStartActionNamesTheCurrentColumn(t *testing.T) {
	t.Parallel()
	d := slack.DigestData{
		Team: "payments",
		People: []slack.PersonDigest{{
			Person: slack.Mention{Display: "Ann"},
			Own: []slack.AuthorItem{{
				IID: 42, Title: "PAY-42 checkout", WebURL: "https://gitlab/42",
				StartLinear: true, LinearIdentifier: "PAY-42",
				LinearWebURL: "https://linear.app/PAY-42", LinearState: "In Progress",
			}},
		}},
	}
	text := rendered(slack.BuildDigest(d)[0])
	if !strings.Contains(text, "move <https://linear.app/PAY-42|PAY-42> to In Review (now In Progress)") {
		t.Errorf("render is missing the start action:\n%s", text)
	}
}

// Both halves degrade rather than render an empty link or a dangling "(now )".
func TestLinearActionsSurviveMissingIdentifierAndState(t *testing.T) {
	t.Parallel()
	d := slack.DigestData{
		Team: "payments",
		People: []slack.PersonDigest{{
			Person: slack.Mention{Display: "Ann"},
			Own: []slack.AuthorItem{{
				IID: 42, Title: "checkout", WebURL: "https://gitlab/42", StartLinear: true,
			}},
		}},
	}
	text := rendered(slack.BuildDigest(d)[0])
	if !strings.Contains(text, "move task to In Review") {
		t.Errorf("render lost the action:\n%s", text)
	}
	if strings.Contains(text, "(now ") {
		t.Errorf("render left a dangling state clause:\n%s", text)
	}
}

// A card name is workspace-authored text on the same footing as a title, so it
// goes through the same escaping.
func TestLinearStateIsEscaped(t *testing.T) {
	t.Parallel()
	d := slack.DigestData{
		Team: "payments",
		People: []slack.PersonDigest{{
			Person: slack.Mention{Display: "Ann"},
			Own: []slack.AuthorItem{{
				IID: 42, Title: "checkout", WebURL: "https://gitlab/42",
				StartLinear: true, LinearIdentifier: "PAY-42", LinearState: "R&D <hold>",
			}},
		}},
	}
	text := rendered(slack.BuildDigest(d)[0])
	if !strings.Contains(text, "R&amp;D &lt;hold&gt;") {
		t.Errorf("state was not escaped:\n%s", text)
	}
}

func TestLinearHealthyZeroStillRendersSummary(t *testing.T) {
	t.Parallel()
	msgs := slack.BuildDigest(slack.DigestData{Team: "payments", LinearEnabled: true})
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want one", len(msgs))
	}
	if text := rendered(msgs[0]); !strings.Contains(text, "Linear · In Review: 0") {
		t.Errorf("healthy zero summary is missing:\n%s", text)
	}
}

func TestBuildDigestLayout(t *testing.T) {
	t.Parallel()

	msgs := slack.BuildDigest(sampleDigest())
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Part != 1 || m.Parts != 1 {
		t.Errorf("part = %d/%d, want 1/1", m.Part, m.Parts)
	}
	if m.Text != "MR Digest — Payments" {
		t.Errorf("fallback text = %q", m.Text)
	}

	// One block per person, and every row is one line: no "by <author>" second
	// line, no per-person section heading, no separate Reviews/Author sections.
	want := []string{
		"📋 MR Digest — Payments",
		// Both are hours old, so both are unmarked: the markers are for days.
		"*<@U123>* · to review 2\n" +
			"▫️ review <https://gl/payments/-/merge_requests/481|!481> _payments_ · waiting 18h — Add payment retries\n" +
			"▫️ review <https://gl/billing/-/merge_requests/932|!932> _billing_ · waiting 5h — Invoice export",
		"*<@U456>* · your MRs 1\n" +
			"🛠 your MR <https://gl/payments/-/merge_requests/475|!475> _payments_ · 💬 resolve 3 threads · " +
			"⚠️ fix merge conflicts · " +
			"❌ fix the failed <https://gl/payments/-/pipelines/9001|pipeline> — Cache invalidation",
		"*<@U789>* · your MRs 1\n" +
			"🛠 your MR <https://gl/checkout/-/merge_requests/122|!122> _checkout_ · " +
			"❌ fix the failed <https://gl/checkout/-/pipelines/42|pipeline> — Search filters",
	}
	got := blockTexts(t, m.Blocks)
	if len(got) != len(want) {
		t.Fatalf("blocks = %d, want %d:\n%s", len(got), len(want), strings.Join(got, "\n---\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d =\n%q\nwant\n%q", i, got[i], want[i])
		}
	}
	if m.Blocks[0].Type != "header" || m.Blocks[0].Text.Type != "plain_text" {
		t.Errorf("first block = %+v, want a plain_text header", m.Blocks[0])
	}
	for _, b := range m.Blocks[1:] {
		if b.Type != "section" || b.Text.Type != "mrkdwn" {
			t.Errorf("block = %+v, want an mrkdwn section", b)
		}
	}
}

// TestSinglePersonSeesBothHalvesTogether is the reason the two sections were
// merged: someone with reviews to deliver *and* their own MRs to fix used to
// appear twice, in two places, with no way to see their whole workload at once.
func TestSinglePersonSeesBothHalvesTogether(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{
		Team:    "payments",
		Project: "payments",
		People: []slack.PersonDigest{{
			Person:   slack.Mention{SlackID: "U1"},
			ToReview: []slack.ReviewItem{{IID: 10, Title: "CHAIN-1 a", WebURL: "https://gl/10", Waiting: 3 * 24 * time.Hour}},
			Own:      []slack.AuthorItem{{IID: 11, Title: "CHAIN-2 b", WebURL: "https://gl/11", UnresolvedThreads: 2}},
		}},
	}
	blocks := blockTexts(t, slack.BuildDigest(d)[0].Blocks)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want header + one person block:\n%s", len(blocks), strings.Join(blocks, "\n---\n"))
	}
	body := blocks[1]
	if !strings.HasPrefix(body, "*<@U1>* · to review 1 · your MRs 1\n") {
		t.Errorf("head must state both halves:\n%s", body)
	}
	for _, want := range []string{"|!10>", "|!11>", "💬 resolve 2 threads"} {
		if !strings.Contains(body, want) {
			t.Errorf("%q missing:\n%s", want, body)
		}
	}
}

// TestReviewTailIsListedNotDropped: only the oldest few reviews get a full row,
// but every remaining merge request is still linked. Compression must never lose
// one.
func TestReviewTailIsListedNotDropped(t *testing.T) {
	t.Parallel()

	const total = 15
	p := slack.PersonDigest{Person: slack.Mention{SlackID: "U1"}}
	for i := range total {
		p.ToReview = append(p.ToReview, slack.ReviewItem{
			IID:     int64(100 + i),
			Title:   fmt.Sprintf("CHAIN-%d something", i),
			WebURL:  fmt.Sprintf("https://gl/%d", 100+i),
			Waiting: time.Duration(total-i) * 24 * time.Hour, // caller sorts; oldest first here
		})
	}
	d := slack.DigestData{Team: "t", Project: "p", People: []slack.PersonDigest{p}}
	body := blockTexts(t, slack.BuildDigest(d)[0].Blocks)[1]

	if got := strings.Count(body, " — CHAIN-"); got != 3 {
		t.Errorf("detailed rows = %d, want 3 (the rest belong in the tail):\n%s", got, body)
	}
	if !strings.Contains(body, fmt.Sprintf("+%d more to review:", total-3)) {
		t.Errorf("tail count missing:\n%s", body)
	}
	for i := range total {
		if !strings.Contains(body, fmt.Sprintf("|!%d>", 100+i)) {
			t.Fatalf("!%d is not in the digest at all:\n%s", 100+i, body)
		}
	}
}

// TestBigReviewTailIsSplitNotTruncated: a reviewer with 60 pending merge
// requests renders a tail no single 3000-character section can hold. The tail
// has to be split across sections — cutting it with an ellipsis drops merge
// requests while the "+57 more" count still promises them, which is silent
// data loss dressed up as compression.
func TestBigReviewTailIsSplitNotTruncated(t *testing.T) {
	t.Parallel()

	// Realistic GitLab URLs: the tail is a list of links, so the URL length is
	// what actually blows the section limit.
	const total = 60
	p := slack.PersonDigest{Person: slack.Mention{SlackID: "U1"}}
	for i := range total {
		iid := int64(1400 + i)
		p.ToReview = append(p.ToReview, slack.ReviewItem{
			Project: "blockchain-api",
			IID:     iid,
			Title:   fmt.Sprintf("CHAIN-%d fix the lookup", i),
			WebURL: fmt.Sprintf(
				"https://gitlab.example.com/backend/blockchain-api/-/merge_requests/%d", iid),
			Waiting: time.Duration(total-i) * 24 * time.Hour,
		})
	}
	// No DigestData.Project: a multi-project digest, where the tail carries the
	// project label too and is therefore at its longest.
	msgs := slack.BuildDigest(slack.DigestData{Team: "t", People: []slack.PersonDigest{p}})

	var texts []string
	for _, m := range msgs {
		assertWithinLimits(t, slack.Builder{}, m)
		texts = append(texts, blockTexts(t, m.Blocks)...)
	}
	joined := strings.Join(texts, "\n")

	if !strings.Contains(joined, fmt.Sprintf("+%d more to review:", total-3)) {
		t.Errorf("tail count missing:\n%s", joined)
	}
	// Every title here is short, so the only thing that can produce an ellipsis
	// is a truncated section entry.
	for i, m := range msgs {
		for j, b := range m.Blocks {
			if b.Type == "section" && strings.Contains(b.Text.Text, "…") {
				t.Errorf("part %d block %d was truncated instead of split:\n%s", i+1, j, b.Text.Text)
			}
		}
	}
	for i := range total {
		if !strings.Contains(joined, fmt.Sprintf("|!%d>", 1400+i)) {
			t.Fatalf("!%d is not in the digest at all:\n%s", 1400+i, joined)
		}
	}
}

// TestTailStaysUnambiguous: the tail is compressed, not anonymous. It keeps the
// age marker — a 110-day-old merge request must not look like a 2-day-old one —
// and on a multi-project digest it keeps the project label, which is the only
// thing telling two !1404s from different repositories apart.
func TestTailStaysUnambiguous(t *testing.T) {
	t.Parallel()

	person := func() slack.PersonDigest {
		p := slack.PersonDigest{Person: slack.Mention{SlackID: "U1"}}
		for i := range 3 { // the detail rows, pushed aside
			p.ToReview = append(p.ToReview, slack.ReviewItem{
				Project: "alpha", IID: int64(i + 1), Title: "T",
				WebURL: fmt.Sprintf("https://gl/%d", i+1), Waiting: 200 * 24 * time.Hour,
			})
		}
		p.ToReview = append(p.ToReview,
			slack.ReviewItem{
				Project: "alpha", IID: 13, Title: "T", WebURL: "https://gl/13",
				Waiting: 110 * 24 * time.Hour,
			},
			slack.ReviewItem{
				Project: "beta", IID: 14, Title: "T", WebURL: "https://gl/14",
				Waiting: 2 * 24 * time.Hour,
			},
		)
		return p
	}

	tailOf := func(t *testing.T, d slack.DigestData) string {
		t.Helper()
		for _, line := range strings.Split(blockTexts(t, slack.BuildDigest(d)[0].Blocks)[1], "\n") {
			if strings.Contains(line, "+2 more to review:") {
				return line
			}
		}
		t.Fatalf("no tail line in the digest")
		return ""
	}

	multi := tailOf(t, slack.DigestData{Team: "t", People: []slack.PersonDigest{person()}})
	for _, want := range []string{"🔴 <https://gl/13|!13> _alpha_", "🟡 <https://gl/14|!14> _beta_"} {
		if !strings.Contains(multi, want) {
			t.Errorf("tail entry %q missing from:\n%s", want, multi)
		}
	}

	single := tailOf(t, slack.DigestData{
		Team: "t", Project: "alpha", People: []slack.PersonDigest{person()},
	})
	if strings.Contains(single, "_alpha_") {
		t.Errorf("the tail must not repeat the project the title carries:\n%s", single)
	}
	for _, want := range []string{"🔴 <https://gl/13|!13>", "🟡 <https://gl/14|!14>"} {
		if !strings.Contains(single, want) {
			t.Errorf("tail entry %q missing from:\n%s", want, single)
		}
	}
}

// TestProjectLabelOnlyWhenItDisambiguates: on a one-repository team the project
// was repeated on every single row. It stays on a multi-project digest, where it
// is the only thing telling two !1404s apart.
func TestProjectLabelOnlyWhenItDisambiguates(t *testing.T) {
	t.Parallel()

	person := slack.PersonDigest{
		Person:   slack.Mention{SlackID: "U1"},
		ToReview: []slack.ReviewItem{{Project: "blockchain-api", IID: 1, Title: "T", WebURL: "https://gl/1"}},
		Own:      []slack.AuthorItem{{Project: "blockchain-api", IID: 2, Title: "T2", WebURL: "https://gl/2"}},
	}

	single := slack.BuildDigest(slack.DigestData{
		Team: "team", Project: "blockchain-api", People: []slack.PersonDigest{person},
	})[0]
	if !strings.Contains(single.Text, "· blockchain-api") {
		t.Errorf("a single-project digest must name it in the title: %q", single.Text)
	}
	if body := blockTexts(t, single.Blocks)[1]; strings.Contains(body, "blockchain-api") {
		t.Errorf("rows must not repeat the project when the title carries it:\n%s", body)
	}

	multi := slack.BuildDigest(slack.DigestData{Team: "team", People: []slack.PersonDigest{person}})[0]
	if strings.Contains(multi.Text, "blockchain-api") {
		t.Errorf("a multi-project digest must not claim one project: %q", multi.Text)
	}
	if body := blockTexts(t, multi.Blocks)[1]; strings.Count(body, "_blockchain-api_") != 2 {
		t.Errorf("every row of a multi-project digest needs its project:\n%s", body)
	}
}

// TestAgeMarkers classify without filtering: nothing is hidden, but a reader can
// see which end of the list is old. "waiting 38m" and "waiting 110d" used to be
// rendered identically.
func TestAgeMarkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wait time.Duration
		want string
	}{
		{"fresh", 38 * time.Minute, "▫️ "},
		{"just under two days", 47 * time.Hour, "▫️ "},
		{"two days", 48 * time.Hour, "🟡 "},
		{"just under a week", 6 * 24 * time.Hour, "🟡 "},
		{"a week", 7 * 24 * time.Hour, "🔴 "},
		{"a hundred days", 100 * 24 * time.Hour, "🔴 "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := slack.DigestData{
				Team: "t", Project: "p",
				People: []slack.PersonDigest{{
					Person:   slack.Mention{SlackID: "U1"},
					ToReview: []slack.ReviewItem{{IID: 1, Title: "T", WebURL: "https://gl/1", Waiting: tt.wait}},
				}},
			}
			body := blockTexts(t, slack.BuildDigest(d)[0].Blocks)[1]
			row := strings.Split(body, "\n")[1]
			if !strings.HasPrefix(row, tt.want) {
				t.Errorf("row = %q, want it to start with %q", row, tt.want)
			}
		})
	}
}

// TestWaitingSuffix keeps the duration format, which now sits inside the row.
func TestWaitingSuffix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wait time.Duration
		want string
	}{
		{"omitted when zero", 0, "|!1> — T"},
		{"minutes", 45 * time.Minute, "|!1> · waiting 45m — T"},
		{"rounds up to a minute", 20 * time.Second, "|!1> · waiting 1m — T"},
		{"hours", 18 * time.Hour, "|!1> · waiting 18h — T"},
		{"days past two", 72 * time.Hour, "|!1> · waiting 3d — T"},
		{"still hours at 47", 47 * time.Hour, "|!1> · waiting 47h — T"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := slack.DigestData{
				Team: "t", Project: "p",
				People: []slack.PersonDigest{{
					Person:   slack.Mention{SlackID: "U1"},
					ToReview: []slack.ReviewItem{{IID: 1, Title: "T", WebURL: "https://gl/1", Waiting: tt.wait}},
				}},
			}
			body := blockTexts(t, slack.BuildDigest(d)[0].Blocks)[1]
			if !strings.HasSuffix(body, tt.want) {
				t.Errorf("body =\n%q\nwant suffix %q", body, tt.want)
			}
		})
	}
}

// TestLongTitleIsShortenedToOneLine: titles here are commit-message length. A
// 104-character one wrapped to three lines in Slack and undid one-row-per-MR.
func TestLongTitleIsShortenedToOneLine(t *testing.T) {
	t.Parallel()

	const long = "CHAIN-190 create one outbox message per transaction/transfer instead of batched payload with first-id aggregate_id"
	d := slack.DigestData{
		Team: "t", Project: "p",
		People: []slack.PersonDigest{{
			Person:   slack.Mention{SlackID: "U1"},
			ToReview: []slack.ReviewItem{{IID: 1, Title: long, WebURL: "https://gl/1"}},
		}},
	}
	row := strings.Split(blockTexts(t, slack.BuildDigest(d)[0].Blocks)[1], "\n")[1]

	title := row[strings.Index(row, "— ")+len("— "):]
	if n := utf8.RuneCountInString(title); n > 56 {
		t.Errorf("title = %d runes (%q), want it shortened", n, title)
	}
	if !strings.HasSuffix(title, "…") {
		t.Errorf("a shortened title must be marked: %q", title)
	}
	// The ticket key is what makes a shortened title still identifiable.
	if !strings.HasPrefix(title, "CHAIN-190 ") {
		t.Errorf("the ticket key must survive: %q", title)
	}
	// Cut at a word boundary, not mid-word.
	if strings.HasSuffix(strings.TrimSuffix(title, "…"), " ") {
		t.Errorf("trailing space before the ellipsis: %q", title)
	}
}

// TestShortenedTitleIsCutCleanly pins the three ways the cut used to go wrong:
// a second ellipsis on an already-ellipsised string, a cut landing inside an
// HTML entity because escaping ran first, and a byte index compared against a
// rune budget, which shortened Cyrillic titles far below their budget.
func TestShortenedTitleIsCutCleanly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		title    string
		minRunes int // a shortened title must still use most of its budget
	}{
		{
			// One long word: nothing to cut back to, so the old code ellipsised
			// truncateRunes' output a second time and rendered "xxxx……".
			name: "single long word", title: strings.Repeat("x", 80), minRunes: 40,
		},
		{
			// The "&" sits where the cut falls, so escaping before cutting left a
			// literal "&am" in the row.
			name:  "ampersand at the cut",
			title: strings.Repeat("a", 51) + "&" + strings.Repeat("b", 30), minRunes: 40,
		},
		{
			// Russian titles are normal here. The only word boundary is at rune 20,
			// well inside the half-budget guard, so the title must be cut mid-word
			// at the budget instead — the byte index made the guard accept it and
			// threw away two thirds of the line.
			name:  "cyrillic with an early word boundary",
			title: strings.Repeat("ф", 20) + " " + strings.Repeat("б", 60), minRunes: 40,
		},
		{
			name:  "cyrillic with a late word boundary",
			title: strings.Repeat("ф", 40) + " " + strings.Repeat("б", 40), minRunes: 40,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := slack.DigestData{
				Team: "t", Project: "p",
				People: []slack.PersonDigest{{
					Person:   slack.Mention{SlackID: "U1"},
					ToReview: []slack.ReviewItem{{IID: 1, Title: tt.title, WebURL: "https://gl/1"}},
				}},
			}
			row := strings.Split(blockTexts(t, slack.BuildDigest(d)[0].Blocks)[1], "\n")[1]
			parts := strings.SplitN(row, " — ", 2)
			if len(parts) != 2 {
				t.Fatalf("no title in the row: %q", row)
			}
			title := parts[1]

			// The budget bounds what Slack draws, and it draws "&amp;" as one
			// character, so the length is measured on the unescaped form.
			display := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(title)
			if n := utf8.RuneCountInString(display); n > 56 {
				t.Errorf("title = %d runes (%q), want it shortened", n, title)
			}
			if n := utf8.RuneCountInString(display); n < tt.minRunes {
				t.Errorf("title = %d runes (%q), want at least %d: the budget is there to be used",
					n, title, tt.minRunes)
			}
			if !strings.HasSuffix(title, "…") {
				t.Errorf("a shortened title must be marked: %q", title)
			}
			if strings.Contains(title, "……") {
				t.Errorf("the cut is marked once, not twice: %q", title)
			}
			// Escaping must happen after the cut, or the row carries half an entity.
			bare := strings.NewReplacer("&amp;", "", "&lt;", "", "&gt;", "").Replace(title)
			if strings.ContainsAny(bare, "&<>") {
				t.Errorf("cut through an escape sequence: %q", title)
			}
		})
	}
}

func TestAuthorFlagOrderIsFixed(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{
		Team: "payments", Project: "payments",
		People: []slack.PersonDigest{{
			Person: slack.Mention{SlackID: "U1"},
			Own: []slack.AuthorItem{{
				IID: 1, Title: "T", WebURL: "https://gl/1",
				ChangesRequestedBy: []slack.Mention{{SlackID: "U9"}, {Display: "Jane Doe (@jane)"}},
				UnresolvedThreads:  1, MergeConflicts: true, PipelineFailed: true,
				PipelineWebURL: "https://gl/p/9",
			}},
		}},
	}
	body := blockTexts(t, slack.BuildDigest(d)[0].Blocks)[1]

	// An unmatched reviewer is named without a ping, exactly like everywhere else.
	if !strings.Contains(body, "🔁 address changes requested by <@U9>, Jane Doe (@jane)") {
		t.Errorf("changes-requested flag missing or misrendered:\n%s", body)
	}
	wantOrder := []string{"🔁 address changes requested by", "💬 resolve 1 thread", "⚠️ fix merge conflicts", "❌ "}
	prev := -1
	for _, part := range wantOrder {
		i := strings.Index(body, part)
		if i < 0 {
			t.Fatalf("%q missing from:\n%s", part, body)
		}
		if i <= prev {
			t.Fatalf("%q out of order in:\n%s", part, body)
		}
		prev = i
	}
}

func TestAuthorFlagsOmitWhatDoesNotApply(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{
		Team: "payments", Project: "payments",
		People: []slack.PersonDigest{{
			Person: slack.Mention{SlackID: "U1"},
			Own:    []slack.AuthorItem{{IID: 1, PipelineFailed: true, PipelineWebURL: "https://gl/p/1"}},
		}},
	}
	body := blockTexts(t, slack.BuildDigest(d)[0].Blocks)[1]
	want := "*<@U1>* · your MRs 1\n🛠 your MR !1 · ❌ fix the failed <https://gl/p/1|pipeline>"
	if body != want {
		t.Errorf("body =\n%q\nwant\n%q", body, want)
	}
}

func TestUnmatchedAndAmbiguousPeopleStillAppear(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{
		Team: "payments", Project: "payments",
		People: []slack.PersonDigest{
			{
				Person:   slack.Mention{Display: "John Smith (@john)"},
				ToReview: []slack.ReviewItem{{IID: 1, Title: "T", WebURL: "https://gl/1"}},
				Own: []slack.AuthorItem{{
					IID: 3, Title: "T3", WebURL: "https://gl/3",
					ChangesRequestedBy: []slack.Mention{{Display: "Ann Lee (@ann)", Ambiguous: true}},
				}},
			},
			{
				Person:   slack.Mention{}, // nothing known at all
				ToReview: []slack.ReviewItem{{IID: 2, Title: "T2", WebURL: "https://gl/2"}},
			},
		},
	}
	texts := strings.Join(blockTexts(t, slack.BuildDigest(d)[0].Blocks), "\n")
	if !strings.Contains(texts, "*John Smith (@john)* · to review 1 · your MRs 1") {
		t.Errorf("unmatched person missing:\n%s", texts)
	}
	if !strings.Contains(texts, "Ann Lee (@ann) ❓") {
		t.Errorf("ambiguous person is not marked:\n%s", texts)
	}
	if strings.Contains(texts, "<@>") {
		t.Errorf("empty mention rendered:\n%s", texts)
	}
	if !strings.Contains(texts, "unknown user") {
		t.Errorf("nameless person should still be listed:\n%s", texts)
	}
	// Every MR is present — an unmatched user never drops a row.
	for _, ref := range []string{"|!1>", "|!2>", "|!3>"} {
		if !strings.Contains(texts, ref) {
			t.Errorf("%s missing:\n%s", ref, texts)
		}
	}
}

func TestPartialDataLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		failed int
		want   string
	}{
		{1, "⚠️ Partial data: failed to inspect 1 repository."},
		{3, "⚠️ Partial data: failed to inspect 3 repositories."},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.failed), func(t *testing.T) {
			t.Parallel()
			d := sampleDigest()
			d.FailedRepos = tt.failed
			texts := blockTexts(t, slack.BuildDigest(d)[0].Blocks)
			if texts[1] != tt.want {
				t.Errorf("second block = %q, want %q", texts[1], tt.want)
			}
		})
	}

	// A run that saw everything says nothing.
	texts := blockTexts(t, slack.BuildDigest(sampleDigest())[0].Blocks)
	if strings.Contains(strings.Join(texts, "\n"), "Partial data") {
		t.Error("clean run must not claim partial data")
	}
}

func TestPartialDataAloneStillProducesAMessage(t *testing.T) {
	t.Parallel()

	msgs := slack.BuildDigest(slack.DigestData{Team: "payments", FailedRepos: 2})
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
}

func TestEmptyDigestProducesNoMessages(t *testing.T) {
	t.Parallel()

	empty := slack.DigestData{
		Team: "payments",
		// A person with nothing owed is not a reason to post.
		People: []slack.PersonDigest{{Person: slack.Mention{SlackID: "U1"}}},
	}
	if msgs := slack.BuildDigest(empty); len(msgs) != 0 {
		t.Fatalf("messages = %d, want none", len(msgs))
	}
}

func TestEscapingKeepsMarkupOutOfText(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{
		Team: "payments",
		People: []slack.PersonDigest{{
			Person: slack.Mention{Display: "A & B <script>"},
			ToReview: []slack.ReviewItem{{
				Project: "pay<ments", IID: 7, Title: "Fix a > b && c",
				WebURL: "https://gl/p/-/mr/7?a=1|b=2",
			}},
		}},
	}
	body := blockTexts(t, slack.BuildDigest(d)[0].Blocks)[1]
	for _, raw := range []string{"A & B <script>", "a > b && c", "pay<ments"} {
		if strings.Contains(body, raw) {
			t.Errorf("unescaped %q in:\n%s", raw, body)
		}
	}
	if !strings.Contains(body, "Fix a &gt; b &amp;&amp; c") {
		t.Errorf("escaped title missing:\n%s", body)
	}
	// The pipe would end the link label early.
	if !strings.Contains(body, "https://gl/p/-/mr/7?a=1%7Cb=2|") {
		t.Errorf("link URL not sanitised:\n%s", body)
	}
}

// TestSplitIntoNumberedPartsLosesNothing is the promise that a big digest is
// split, never silently truncated.
func TestSplitIntoNumberedPartsLosesNothing(t *testing.T) {
	t.Parallel()

	// Enough people that the compressed layout still overflows several messages —
	// the compression is what makes a real team fit in one, so the split has to be
	// exercised at a scale beyond it.
	// Six reviews each, so every person also has a compressed tail: the tail is
	// where merge requests used to disappear.
	//
	// Titles stay well under the one-line budget on purpose. Nothing here may be
	// shortened, which makes any ellipsis in the output proof that a section was
	// truncated rather than split.
	const people = 200
	d := slack.DigestData{Team: "Payments", FailedRepos: 2}
	var wantRefs []string
	for i := range people {
		p := slack.PersonDigest{Person: slack.Mention{SlackID: fmt.Sprintf("UR%d", i)}}
		// One reviewer is badly behind, so a single person's tail is longer than a
		// section on its own. That is the case that used to be cut instead of split.
		reviews := 6
		if i == 0 {
			reviews = 60
		}
		for j := range reviews {
			iid := int64(i*1000 + j)
			p.ToReview = append(p.ToReview, slack.ReviewItem{
				Project: fmt.Sprintf("rev%d-%d", i, j), IID: iid,
				Title: fmt.Sprintf("CHAIN-%d review", j),
				// Real GitLab URLs: the URL is most of a compressed row, so a short
				// stand-in would hide the very overflow this test is about.
				WebURL:  fmt.Sprintf("https://gitlab.example.com/backend/rev%d/-/merge_requests/%d", i, iid),
				Waiting: time.Duration(j+1) * 24 * time.Hour,
			})
			wantRefs = append(wantRefs, fmt.Sprintf("|!%d>", iid))
		}
		for j := range 2 {
			iid := int64(i*1000 + 500 + j)
			p.Own = append(p.Own, slack.AuthorItem{
				Project: fmt.Sprintf("auth%d-%d", i, j), IID: iid,
				Title:             fmt.Sprintf("CHAIN-%d own", j),
				WebURL:            fmt.Sprintf("https://gitlab.example.com/backend/auth%d/-/merge_requests/%d", i, iid),
				UnresolvedThreads: 2, MergeConflicts: true,
			})
			wantRefs = append(wantRefs, fmt.Sprintf("|!%d>", iid))
		}
		d.People = append(d.People, p)
	}

	msgs := slack.BuildDigest(d)
	if len(msgs) < 3 {
		t.Fatalf("messages = %d, want the digest split into several parts", len(msgs))
	}

	var all strings.Builder
	for i, m := range msgs {
		if m.Part != i+1 || m.Parts != len(msgs) {
			t.Errorf("message %d numbered %d/%d", i, m.Part, m.Parts)
		}
		wantTitle := fmt.Sprintf("MR Digest — Payments (%d/%d)", i+1, len(msgs))
		if m.Text != wantTitle {
			t.Errorf("text = %q, want %q", m.Text, wantTitle)
		}
		if got := m.Blocks[0].Text.Text; got != "📋 "+wantTitle {
			t.Errorf("header = %q, want %q", got, "📋 "+wantTitle)
		}
		assertWithinLimits(t, slack.Builder{}, m)
		for _, s := range blockTexts(t, m.Blocks) {
			all.WriteString(s)
			all.WriteString("\n")
		}
		for j, b := range m.Blocks {
			if b.Type == "section" && strings.Contains(b.Text.Text, "…") {
				t.Errorf("part %d block %d was truncated instead of split:\n%s", i+1, j, b.Text.Text)
			}
		}
	}

	joined := all.String()
	for _, ref := range wantRefs {
		if !strings.Contains(joined, ref) {
			t.Fatalf("MR %q lost in the split", ref)
		}
	}
}

// TestOnePersonSplitsAcrossSections covers a single person with more merge
// requests than fit in one 3000-character section. It uses pending reviews
// rather than the person's own merge requests: reviews are the compressed side,
// so this is where a section boundary is hard to place.
func TestOnePersonSplitsAcrossSections(t *testing.T) {
	t.Parallel()

	p := slack.PersonDigest{Person: slack.Mention{SlackID: "U1"}}
	const reviews = 60
	for i := range reviews {
		p.ToReview = append(p.ToReview, slack.ReviewItem{
			Project: "payments", IID: int64(i), Title: strings.Repeat("x", 100),
			// A real GitLab URL, because the URL is most of a compressed row.
			WebURL:  "https://gitlab.example.com/payments/backend/-/merge_requests/" + fmt.Sprint(i),
			Waiting: time.Duration(reviews-i) * 24 * time.Hour,
		})
	}
	msgs := slack.BuildDigest(slack.DigestData{Team: "t", People: []slack.PersonDigest{p}})

	var texts []string
	for _, m := range msgs {
		assertWithinLimits(t, slack.Builder{}, m)
		texts = append(texts, blockTexts(t, m.Blocks)...)
	}
	joined := strings.Join(texts, "\n")
	for i := range reviews {
		if !strings.Contains(joined, fmt.Sprintf("|!%d>", i)) {
			t.Fatalf("MR !%d lost", i)
		}
	}
	if !strings.Contains(joined, "*<@U1>* · to review 60"+" _(continued)_") {
		t.Errorf("a continued person must repeat whose merge requests these are:\n%s", joined)
	}
}

func TestLimitsHoldForAHugeDigest(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{Team: strings.Repeat("very long team name ", 20)}
	for i := range 200 {
		d.People = append(d.People, slack.PersonDigest{
			Person: slack.Mention{SlackID: fmt.Sprintf("U%d", i)},
			ToReview: []slack.ReviewItem{{
				Project: "p", IID: int64(i), Title: strings.Repeat("t", 300),
			}},
		})
	}
	msgs := slack.BuildDigest(d)
	if len(msgs) < 4 {
		t.Fatalf("messages = %d, want several parts", len(msgs))
	}
	for _, m := range msgs {
		assertWithinLimits(t, slack.Builder{}, m)
	}
}

// TestSingleOversizedEntryIsCutVisibly is the one place a row can be shortened:
// a single entry whose rendered text alone exceeds a section. It is marked, not
// dropped.
func TestSingleOversizedEntryIsCutVisibly(t *testing.T) {
	t.Parallel()

	b := slack.Builder{MaxSectionChars: 120}
	d := slack.DigestData{
		Team: "t", Project: "p",
		People: []slack.PersonDigest{{
			Person: slack.Mention{SlackID: "U1"},
			Own: []slack.AuthorItem{{
				IID: 1, Title: "T",
				ChangesRequestedBy: []slack.Mention{{Display: strings.Repeat("y", 500)}},
			}},
		}},
	}
	msgs := b.Build(d)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	body := blockTexts(t, msgs[0].Blocks)[1]
	if utf8.RuneCountInString(body) > 120 {
		t.Errorf("section = %d runes, want ≤ 120", utf8.RuneCountInString(body))
	}
	if !strings.HasSuffix(body, "…") {
		t.Errorf("a shortened row must be marked:\n%s", body)
	}
	if !strings.Contains(body, "!1") {
		t.Errorf("the MR itself must survive:\n%s", body)
	}
}

func TestHeaderIsClippedToSlacksLimit(t *testing.T) {
	t.Parallel()

	msgs := slack.BuildDigest(slack.DigestData{
		Team: strings.Repeat("n", 400),
		People: []slack.PersonDigest{{
			Person: slack.Mention{SlackID: "U1"},
			Own:    []slack.AuthorItem{{Project: "p", IID: 1}},
		}},
	})
	header := msgs[0].Blocks[0].Text.Text
	if n := utf8.RuneCountInString(header); n > 150 {
		t.Errorf("header = %d runes, want ≤ 150", n)
	}
	if !strings.HasSuffix(header, "…") {
		t.Errorf("a clipped header must be marked: %q", header)
	}
}

// TestSectionsHonourAnyLimit sweeps the section limit. The two promises are owed
// to whatever limit the builder was handed, not only to Slack's generous 3000:
// no section may exceed it, and no entry may be cut to nothing to fit inside it.
//
// The bug this pins hid for a whole redesign because at 3000 the head arithmetic
// leaves 2235 runes of slack, and the only test that shrank the limit measured
// its sections against the 3000 constant.
func TestSectionsHonourAnyLimit(t *testing.T) {
	t.Parallel()

	// A pathological display name and a tail, so both head forms — plain and
	// " _(continued)_" — and both entry kinds are exercised at every limit.
	p := slack.PersonDigest{Person: slack.Mention{Display: strings.Repeat("Very Long Name ", 12)}}
	for i := range 20 {
		p.ToReview = append(p.ToReview, slack.ReviewItem{
			Project: "blockchain-api", IID: int64(1400 + i), Title: "CHAIN-7 fix the lookup",
			WebURL:  fmt.Sprintf("https://gitlab.example.com/backend/blockchain-api/-/merge_requests/%d", 1400+i),
			Waiting: time.Duration(i+1) * 24 * time.Hour,
		})
	}
	for i := range 2 {
		p.Own = append(p.Own, slack.AuthorItem{
			Project: "wallet", IID: int64(70 + i), Title: "CHAIN-8 фикс отправки уведомлений",
			WebURL:            fmt.Sprintf("https://gitlab.example.com/backend/wallet/-/merge_requests/%d", 70+i),
			UnresolvedThreads: 3, MergeConflicts: true,
		})
	}
	d := slack.DigestData{Team: "t", People: []slack.PersonDigest{p}}

	// 1 floors to MinSectionChars; 59/60/61 straddle the limit below which a
	// quarter of the section can no longer cover a continued head on its own.
	for _, limit := range []int{1, 16, 17, 23, 31, 59, 60, 61, 120, 301, 1000, 3000} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			t.Parallel()

			b := slack.Builder{MaxSectionChars: limit}
			msgs := b.Build(d)
			if len(msgs) == 0 {
				t.Fatal("want messages")
			}
			for _, m := range msgs {
				assertWithinLimits(t, b, m)
			}
			// Three detail rows, two own merge requests and at least one tail entry.
			if n := countEntryLines(t, msgs); n < 6 {
				t.Errorf("entry lines = %d, want at least 6", n)
			}
		})
	}
}

func TestBuilderFloorsAbsurdLimits(t *testing.T) {
	t.Parallel()

	// A caller passing nonsense must not be able to make pagination spin — and
	// must not be able to make a merge request disappear. The floor is the limit
	// the builder then works to, so it owes that limit the same two promises it
	// owes 3000: never exceed it, never drop an entry to stay inside it. It used
	// to do both here, because the continued head was bounded before its suffix
	// was appended: the entry after it got a negative budget and was cut to "".
	b := slack.Builder{MaxBlocks: 1, MaxSectionChars: 1}
	msgs := b.Build(sampleDigest())
	if len(msgs) == 0 {
		t.Fatal("want messages")
	}
	for _, m := range msgs {
		assertWithinLimits(t, b, m)
		if len(m.Blocks) > 3 {
			t.Errorf("blocks = %d, want ≤ 3", len(m.Blocks))
		}
	}
	// sampleDigest: two reviews for U123, one merge request each for U456 and U789.
	assertNoEntryLost(t, msgs, 4)
}

func TestMessagePostPayloadIsSerialisable(t *testing.T) {
	t.Parallel()

	m := slack.BuildDigest(sampleDigest())[0]
	raw, err := json.Marshal(m.Post("C123"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["channel"] != "C123" {
		t.Errorf("channel = %v", back["channel"])
	}
	blocks, ok := back["blocks"].([]any)
	if !ok || len(blocks) != len(m.Blocks) {
		t.Fatalf("blocks = %v", back["blocks"])
	}
	first, _ := blocks[0].(map[string]any)
	text, _ := first["text"].(map[string]any)
	if first["type"] != "header" || text["type"] != "plain_text" || text["emoji"] != true {
		t.Errorf("header block = %v", first)
	}
}

// assertWithinLimits checks the two Block Kit limits the builder owns, against
// the limits *this* builder works to rather than against Slack's constants.
//
// Measuring a shrunken Builder against MaxSectionChars (3000) made the helper
// blind to a whole class of bug: a section 19 runes long against its own 16-rune
// limit, with a merge request dropped to get there, sat inside a green test
// through an entire redesign. The floors are part of the limit, so they are
// applied here the same way the builder applies them.
func assertWithinLimits(t *testing.T, b slack.Builder, m slack.Message) {
	t.Helper()

	maxBlocks, maxChars := slack.MaxBlocksPerMessage, slack.MaxSectionChars
	if b.MaxBlocks > 0 {
		maxBlocks = max(b.MaxBlocks, slack.MinBlocksPerMessage)
	}
	if b.MaxSectionChars > 0 {
		maxChars = max(b.MaxSectionChars, slack.MinSectionChars)
	}

	if len(m.Blocks) > maxBlocks {
		t.Errorf("part %d has %d blocks, want ≤ %d", m.Part, len(m.Blocks), maxBlocks)
	}
	for i, blk := range m.Blocks {
		if blk.Text == nil {
			continue
		}
		if n := utf8.RuneCountInString(blk.Text.Text); blk.Type == "section" && n > maxChars {
			t.Errorf("part %d block %d = %d chars, want ≤ %d", m.Part, i, n, maxChars)
		}
	}
}

// assertNoEntryLost checks that every entry survived as its own non-empty line.
func assertNoEntryLost(t *testing.T, msgs []slack.Message, wantEntries int) {
	t.Helper()
	if got := countEntryLines(t, msgs); got != wantEntries {
		t.Errorf("entry lines = %d, want %d — one per merge request", got, wantEntries)
	}
}

// countEntryLines counts the entry lines across every section and fails on a
// blank one.
//
// Content assertions stop working at a shrunken limit — at 16 runes no line can
// carry a whole link — but "one line per entry, none of them blank" holds at
// every limit, and a blank line is exactly what a vanishing entry leaves behind:
// the entry was cut to nothing and the newline before it stayed.
func countEntryLines(t *testing.T, msgs []slack.Message) int {
	t.Helper()

	n := 0
	for _, m := range msgs {
		for _, blk := range m.Blocks {
			if blk.Type != "section" {
				continue
			}
			// The first line of a section is the group head; the rest are entries.
			for i, line := range strings.Split(blk.Text.Text, "\n") {
				if i == 0 {
					continue
				}
				if strings.TrimSpace(line) == "" {
					t.Errorf("part %d has a blank entry line — an entry was cut to nothing:\n%q",
						m.Part, blk.Text.Text)
				}
				n++
			}
		}
	}
	return n
}

func blockTexts(t *testing.T, blocks []slack.Block) []string {
	t.Helper()
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Text == nil {
			t.Fatalf("block %+v has no text", b)
		}
		out = append(out, b.Text.Text)
	}
	return out
}

// TestEveryRowSaysWhatToDo is the rule the icons alone broke: a reader must be
// able to act on a row without first learning what ▫️ and 🛠 mean. Every row
// opens with the action it is asking for, and every flag on it is an imperative
// — which is what "⚠️ conflicts" and "❌ pipeline" already were, and what
// nothing else was.
//
// Stated as a vocabulary over every segment rather than as a golden string, so
// a flag added later without a verb fails here rather than shipping as another
// icon nobody can read.
func TestEveryRowSaysWhatToDo(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{
		Team: "payments", Project: "payments",
		People: []slack.PersonDigest{{
			Person: slack.Mention{SlackID: "U1"},
			ToReview: []slack.ReviewItem{
				{IID: 10, Title: "CHAIN-1 a", WebURL: "https://gl/10", Waiting: 3 * 24 * time.Hour},
				{IID: 11, Title: "CHAIN-2 b", WebURL: "https://gl/11"},
			},
			Own: []slack.AuthorItem{
				{
					IID: 20, Title: "CHAIN-3 c", WebURL: "https://gl/20",
					ChangesRequestedBy: []slack.Mention{{SlackID: "U9"}},
					UnresolvedThreads:  2, MergeConflicts: true, PipelineFailed: true,
					PipelineWebURL: "https://gl/p/20",
				},
				{
					IID: 21, Title: "CHAIN-4 d", WebURL: "https://gl/21",
					MoveLinear: true, LinearIdentifier: "PAY-4", LinearWebURL: "https://linear.app/PAY-4",
				},
				{
					IID: 22, Title: "CHAIN-5 e", WebURL: "https://gl/22",
					StartLinear: true, LinearIdentifier: "PAY-5",
					LinearWebURL: "https://linear.app/PAY-5", LinearState: "In Progress",
				},
			},
		}},
	}

	// Every form a segment may take. The row openers name what the merge request
	// is to this person; the rest name what to do about it.
	openers := []string{"review ", "your MR "}
	imperatives := []string{
		"waiting ", "address ", "resolve ", "fix ", "move ",
	}
	hasAny := func(s string, prefixes []string) bool {
		for _, p := range prefixes {
			if strings.Contains(s, p) {
				return true
			}
		}
		return false
	}

	body := blockTexts(t, slack.BuildDigest(d)[0].Blocks)[1]
	lines := strings.Split(body, "\n")
	if len(lines) != 6 { // the person head, two reviews, three own merge requests
		t.Fatalf("lines = %d, want 6:\n%s", len(lines), body)
	}
	for _, line := range lines[1:] {
		segments := strings.Split(line, " · ")
		if !hasAny(segments[0], openers) {
			t.Errorf("row does not say what it is: %q", line)
		}
		for _, seg := range segments[1:] {
			if !hasAny(seg, imperatives) {
				t.Errorf("segment %q of row %q names no action", seg, line)
			}
		}
	}
}

// TestTailNamesItsActionOnce: the compressed tail is a list of ids under one
// line that already says what they are. Repeating "review" on every entry is
// exactly the noise the tail exists to remove.
func TestTailNamesItsActionOnce(t *testing.T) {
	t.Parallel()

	p := slack.PersonDigest{Person: slack.Mention{SlackID: "U1"}}
	for i := range 6 {
		p.ToReview = append(p.ToReview, slack.ReviewItem{
			IID: int64(i), Title: "T", WebURL: fmt.Sprintf("https://gl/%d", i),
			Waiting: time.Duration(6-i) * 24 * time.Hour,
		})
	}
	body := blockTexts(t, slack.BuildDigest(slack.DigestData{
		Team: "t", Project: "p", People: []slack.PersonDigest{p},
	})[0].Blocks)[1]

	var tail string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "more to review:") {
			tail = line
		}
	}
	if tail == "" {
		t.Fatalf("no tail line:\n%s", body)
	}
	if n := strings.Count(tail, "review"); n != 1 {
		t.Errorf("tail names the action %d times, want once:\n%s", n, tail)
	}
}

// TestOnlyPersonKeepsOneQueue is the personal command: the same digest, narrowed
// to the caller. It is a filter rather than a second classification precisely so
// the two can never answer differently.
func TestOnlyPersonKeepsOneQueue(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{
		Team: "payments", Project: "payments",
		LinearEnabled: true, LinearInReviewCount: 8,
		Warnings:    []string{"Partial data: Linear could not be inspected."},
		FailedRepos: 1,
		People: []slack.PersonDigest{
			{
				Person:   slack.Mention{SlackID: "U1", Display: "Ann"},
				ToReview: []slack.ReviewItem{{IID: 1, Title: "T1", WebURL: "https://gl/1"}},
				Own:      []slack.AuthorItem{{IID: 2, Title: "T2", WebURL: "https://gl/2", UnresolvedThreads: 1}},
			},
			{
				Person:   slack.Mention{SlackID: "U2", Display: "Bob"},
				ToReview: []slack.ReviewItem{{IID: 3, Title: "T3", WebURL: "https://gl/3"}},
			},
		},
	}

	mine, ok := d.OnlyPerson("U1")
	if !ok {
		t.Fatal("U1 is in the digest and must be found")
	}
	if len(mine.People) != 1 || mine.People[0].Person.SlackID != "U1" {
		t.Fatalf("people = %+v, want only U1", mine.People)
	}
	if len(mine.People[0].ToReview) != 1 || len(mine.People[0].Own) != 1 {
		t.Error("both halves of one person's workload must survive the filter")
	}
	// The board total is the team's, not the caller's: a personal answer opening
	// with "In Review: 8" invites reading it as their own.
	if mine.LinearEnabled || mine.LinearInReviewCount != 0 {
		t.Error("the Linear aggregate must not survive into a personal digest")
	}
	// A digest built on half the repositories is exactly as incomplete for one
	// person as for the team.
	if len(mine.Warnings) != 1 || mine.FailedRepos != 1 {
		t.Errorf("partial-data warnings must survive: warnings=%v failed=%d", mine.Warnings, mine.FailedRepos)
	}
	if mine.Team != d.Team || mine.Project != d.Project {
		t.Error("the header must still say whose digest this is")
	}

	body := blockTexts(t, slack.BuildDigest(mine)[0].Blocks)
	joined := strings.Join(body, "\n")
	if strings.Contains(joined, "|!3>") {
		t.Errorf("another person's merge request leaked into a personal digest:\n%s", joined)
	}
}

// TestOnlyPersonMissesQuietlyAndSafely: an unknown or empty id must produce
// nothing rather than everything. The dangerous failure is the other direction —
// a filter that fell through to the full digest would post the whole team's
// queue as an answer meant for one person.
func TestOnlyPersonMissesQuietlyAndSafely(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{
		Team: "payments",
		People: []slack.PersonDigest{
			{
				// Named but not matched to a Slack account: nobody can address them,
				// so no caller can be them either.
				Person:   slack.Mention{Display: "Ann Author (@ann)"},
				ToReview: []slack.ReviewItem{{IID: 1, Title: "T", WebURL: "https://gl/1"}},
			},
			{
				Person:   slack.Mention{SlackID: "U2"},
				ToReview: []slack.ReviewItem{{IID: 2, Title: "T", WebURL: "https://gl/2"}},
			},
		},
	}

	for _, id := range []string{"U404", "", "   ", "u2"} {
		got, ok := got2(d.OnlyPerson(id))
		if ok {
			t.Errorf("OnlyPerson(%q) matched %d person/people, want none", id, len(got.People))
		}
		if len(got.People) != 0 {
			t.Errorf("OnlyPerson(%q) returned %d people alongside false", id, len(got.People))
		}
	}
}

// got2 is a tiny helper so the two-value call above reads as one expression.
func got2(d slack.DigestData, ok bool) (slack.DigestData, bool) { return d, ok }
