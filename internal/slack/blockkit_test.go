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

// sampleDigest is the digest from plan §13.3.
func sampleDigest() slack.DigestData {
	return slack.DigestData{
		Team: "Payments",
		ReviewsNeeded: []slack.ReviewerGroup{{
			Reviewer: slack.Mention{SlackID: "U123"},
			MRs: []slack.ReviewItem{
				{
					Project: "payments", IID: 481, Title: "Add payment retries",
					WebURL: "https://gl/payments/-/merge_requests/481",
					Author: slack.Mention{SlackID: "U456"}, Waiting: 18 * time.Hour,
				},
				{
					Project: "billing", IID: 932, Title: "Invoice export",
					WebURL: "https://gl/billing/-/merge_requests/932",
					Author: slack.Mention{SlackID: "U789"}, Waiting: 5 * time.Hour,
				},
			},
		}},
		AuthorActions: []slack.AuthorGroup{
			{
				Author: slack.Mention{SlackID: "U456"},
				MRs: []slack.AuthorItem{{
					Project: "payments", IID: 475, Title: "Cache invalidation",
					WebURL:            "https://gl/payments/-/merge_requests/475",
					UnresolvedThreads: 3, MergeConflicts: true, PipelineFailed: true,
					PipelineWebURL: "https://gl/payments/-/pipelines/9001",
				}},
			},
			{
				Author: slack.Mention{SlackID: "U789"},
				MRs: []slack.AuthorItem{{
					Project: "checkout", IID: 122, Title: "Search filters",
					WebURL:         "https://gl/checkout/-/merge_requests/122",
					PipelineFailed: true,
					PipelineWebURL: "https://gl/checkout/-/pipelines/42",
				}},
			},
		},
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

	want := []string{
		"📋 MR Digest — Payments",
		"*👀 Reviews needed*",
		"<@U123> — 2 MRs\n" +
			"• <https://gl/payments/-/merge_requests/481|payments !481 — Add payment retries>\n" +
			"  by <@U456> · waiting 18h\n" +
			"• <https://gl/billing/-/merge_requests/932|billing !932 — Invoice export>\n" +
			"  by <@U789> · waiting 5h",
		"*🛠 Author actions*",
		"<@U456>\n" +
			"• <https://gl/payments/-/merge_requests/475|payments !475 — Cache invalidation>\n" +
			"  💬 3 unresolved threads\n" +
			"  ⚠️ merge conflicts\n" +
			"  ❌ <https://gl/payments/-/pipelines/9001|pipeline failed>",
		"<@U789>\n" +
			"• <https://gl/checkout/-/merge_requests/122|checkout !122 — Search filters>\n" +
			"  ❌ <https://gl/checkout/-/pipelines/42|pipeline failed>",
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

func TestAuthorRowOrderIsFixed(t *testing.T) {
	t.Parallel()

	// Whatever the flags, the rows read threads → conflicts → pipeline so the
	// digest looks the same every day.
	d := slack.DigestData{
		Team: "payments",
		AuthorActions: []slack.AuthorGroup{{
			Author: slack.Mention{SlackID: "U1"},
			MRs: []slack.AuthorItem{{
				Project: "payments", IID: 1, Title: "T",
				UnresolvedThreads: 1, MergeConflicts: true, PipelineFailed: true,
			}},
		}},
	}
	got := blockTexts(t, slack.BuildDigest(d)[0].Blocks)
	body := got[len(got)-1]
	wantOrder := []string{"💬 1 unresolved thread", "⚠️ merge conflicts", "❌ pipeline failed"}
	prev := -1
	for _, row := range wantOrder {
		i := strings.Index(body, row)
		if i < 0 {
			t.Fatalf("row %q missing from:\n%s", row, body)
		}
		if i <= prev {
			t.Fatalf("row %q out of order in:\n%s", row, body)
		}
		prev = i
	}
	// No pipeline URL: the row is still there, just not a link.
	if strings.Contains(body, "|pipeline failed>") {
		t.Errorf("pipeline row should not be a link without a URL:\n%s", body)
	}
}

func TestAuthorRowsOmitWhatDoesNotApply(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{
		Team: "payments",
		AuthorActions: []slack.AuthorGroup{{
			Author: slack.Mention{SlackID: "U1"},
			MRs:    []slack.AuthorItem{{Project: "payments", IID: 1, PipelineFailed: true, PipelineWebURL: "https://gl/p/1"}},
		}},
	}
	got := blockTexts(t, slack.BuildDigest(d)[0].Blocks)
	body := got[len(got)-1]
	want := "<@U1>\n• payments !1\n  ❌ <https://gl/p/1|pipeline failed>"
	if body != want {
		t.Errorf("body =\n%q\nwant\n%q", body, want)
	}
}

func TestUnmatchedAndAmbiguousPeopleStillAppear(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{
		Team: "payments",
		ReviewsNeeded: []slack.ReviewerGroup{
			{
				Reviewer: slack.Mention{Display: "John Smith (@john)"},
				MRs: []slack.ReviewItem{{
					Project: "payments", IID: 1, Title: "T",
					Author: slack.Mention{Display: "Ann Lee (@ann)", Ambiguous: true},
				}},
			},
			{
				Reviewer: slack.Mention{}, // nothing known at all
				MRs:      []slack.ReviewItem{{Project: "payments", IID: 2, Title: "T2"}},
			},
		},
	}
	texts := strings.Join(blockTexts(t, slack.BuildDigest(d)[0].Blocks), "\n")
	if !strings.Contains(texts, "John Smith (@john) — 1 MR") {
		t.Errorf("unmatched reviewer missing:\n%s", texts)
	}
	if !strings.Contains(texts, "Ann Lee (@ann) ❓") {
		t.Errorf("ambiguous author is not marked:\n%s", texts)
	}
	if strings.Contains(texts, "<@>") {
		t.Errorf("empty mention rendered:\n%s", texts)
	}
	if !strings.Contains(texts, "unknown user") {
		t.Errorf("nameless person should still be listed:\n%s", texts)
	}
	// Both MRs are present — an unmatched user never drops a row.
	for _, ref := range []string{"payments !1", "payments !2"} {
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
		Team:          "payments",
		ReviewsNeeded: []slack.ReviewerGroup{{Reviewer: slack.Mention{SlackID: "U1"}}}, // no MRs
		AuthorActions: []slack.AuthorGroup{{Author: slack.Mention{SlackID: "U1"}}},
	}
	if msgs := slack.BuildDigest(empty); len(msgs) != 0 {
		t.Fatalf("messages = %d, want none", len(msgs))
	}
}

func TestEscapingKeepsMarkupOutOfText(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{
		Team: "payments",
		ReviewsNeeded: []slack.ReviewerGroup{{
			Reviewer: slack.Mention{Display: "A & B <script>"},
			MRs: []slack.ReviewItem{{
				Project: "pay<ments", IID: 7, Title: "Fix a > b && c",
				WebURL: "https://gl/p/-/mr/7?a=1|b=2",
				Author: slack.Mention{Display: "x>y"},
			}},
		}},
	}
	body := blockTexts(t, slack.BuildDigest(d)[0].Blocks)[2]
	for _, raw := range []string{"A & B <script>", "a > b && c", "pay<ments", "x>y"} {
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

func TestWaitingSuffix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wait time.Duration
		want string
	}{
		{"omitted when zero", 0, "  by <@U2>"},
		{"minutes", 45 * time.Minute, "  by <@U2> · waiting 45m"},
		{"rounds up to a minute", 20 * time.Second, "  by <@U2> · waiting 1m"},
		{"hours", 18 * time.Hour, "  by <@U2> · waiting 18h"},
		{"days past two", 72 * time.Hour, "  by <@U2> · waiting 3d"},
		{"still hours at 47", 47 * time.Hour, "  by <@U2> · waiting 47h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := slack.DigestData{
				Team: "t",
				ReviewsNeeded: []slack.ReviewerGroup{{
					Reviewer: slack.Mention{SlackID: "U1"},
					MRs: []slack.ReviewItem{{
						Project: "p", IID: 1, Title: "T",
						Author: slack.Mention{SlackID: "U2"}, Waiting: tt.wait,
					}},
				}},
			}
			body := blockTexts(t, slack.BuildDigest(d)[0].Blocks)[2]
			if !strings.HasSuffix(body, tt.want) {
				t.Errorf("body =\n%q\nwant suffix %q", body, tt.want)
			}
		})
	}
}

// TestSplitIntoNumberedPartsLosesNothing is the promise that a big digest is
// split, never silently truncated.
func TestSplitIntoNumberedPartsLosesNothing(t *testing.T) {
	t.Parallel()

	const (
		reviewers = 60
		authors   = 40
	)
	d := slack.DigestData{Team: "Payments", FailedRepos: 2}
	var wantRefs []string
	for i := range reviewers {
		g := slack.ReviewerGroup{Reviewer: slack.Mention{SlackID: fmt.Sprintf("UR%d", i)}}
		for j := range 3 {
			ref := fmt.Sprintf("rev%d-%d !%d%d", i, j, i, j)
			g.MRs = append(g.MRs, slack.ReviewItem{
				Project: fmt.Sprintf("rev%d-%d", i, j), IID: int64(i*10 + j),
				Title:  strings.Repeat("long title ", 20),
				WebURL: "https://gl/x", Author: slack.Mention{SlackID: "UA"},
			})
			_ = ref
			wantRefs = append(wantRefs, fmt.Sprintf("rev%d-%d !%d", i, j, i*10+j))
		}
		d.ReviewsNeeded = append(d.ReviewsNeeded, g)
	}
	for i := range authors {
		g := slack.AuthorGroup{Author: slack.Mention{SlackID: fmt.Sprintf("UA%d", i)}}
		for j := range 2 {
			g.MRs = append(g.MRs, slack.AuthorItem{
				Project: fmt.Sprintf("auth%d-%d", i, j), IID: int64(i*10 + j),
				Title:             strings.Repeat("another long title ", 15),
				UnresolvedThreads: 2, MergeConflicts: true, PipelineFailed: true,
			})
			wantRefs = append(wantRefs, fmt.Sprintf("auth%d-%d !%d", i, j, i*10+j))
		}
		d.AuthorActions = append(d.AuthorActions, g)
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
		assertWithinLimits(t, m)
		for _, s := range blockTexts(t, m.Blocks) {
			all.WriteString(s)
			all.WriteString("\n")
		}
	}

	joined := all.String()
	for _, ref := range wantRefs {
		if !strings.Contains(joined, ref) {
			t.Fatalf("MR %q lost in the split", ref)
		}
	}
	if strings.Contains(joined, "…") {
		t.Error("a part was truncated instead of split")
	}
	// Every part that carries reviewer or author rows names the section it
	// continues, so a part reads on its own.
	for _, m := range msgs {
		texts := blockTexts(t, m.Blocks)
		if len(texts) < 2 {
			continue
		}
		if !strings.Contains(strings.Join(texts, "\n"), "Reviews needed") &&
			!strings.Contains(strings.Join(texts, "\n"), "Author actions") {
			t.Errorf("part %d has no section heading:\n%s", m.Part, strings.Join(texts, "\n"))
		}
	}
}

// TestOneGroupSplitsAcrossSections covers a single reviewer with more MRs than
// fit in one 3000-character section.
func TestOneGroupSplitsAcrossSections(t *testing.T) {
	t.Parallel()

	g := slack.ReviewerGroup{Reviewer: slack.Mention{SlackID: "U1"}}
	const mrs = 40
	for i := range mrs {
		g.MRs = append(g.MRs, slack.ReviewItem{
			Project: "payments", IID: int64(i), Title: strings.Repeat("x", 100),
			WebURL: "https://gl/p/-/merge_requests/" + fmt.Sprint(i),
			Author: slack.Mention{SlackID: "U2"}, Waiting: time.Hour,
		})
	}
	msgs := slack.BuildDigest(slack.DigestData{Team: "t", ReviewsNeeded: []slack.ReviewerGroup{g}})

	var texts []string
	for _, m := range msgs {
		assertWithinLimits(t, m)
		texts = append(texts, blockTexts(t, m.Blocks)...)
	}
	joined := strings.Join(texts, "\n")
	for i := range mrs {
		if !strings.Contains(joined, fmt.Sprintf("payments !%d ", i)) {
			t.Fatalf("MR !%d lost", i)
		}
	}
	if !strings.Contains(joined, "<@U1> — 40 MRs _(continued)_") {
		t.Errorf("a continued group must repeat whose MRs these are:\n%s", joined)
	}
}

func TestLimitsHoldForAHugeDigest(t *testing.T) {
	t.Parallel()

	d := slack.DigestData{Team: strings.Repeat("very long team name ", 20)}
	for i := range 200 {
		d.ReviewsNeeded = append(d.ReviewsNeeded, slack.ReviewerGroup{
			Reviewer: slack.Mention{SlackID: fmt.Sprintf("U%d", i)},
			MRs: []slack.ReviewItem{{
				Project: "p", IID: int64(i), Title: strings.Repeat("t", 300),
				Author: slack.Mention{SlackID: "UA"},
			}},
		})
	}
	msgs := slack.BuildDigest(d)
	if len(msgs) < 4 {
		t.Fatalf("messages = %d, want several parts", len(msgs))
	}
	for _, m := range msgs {
		assertWithinLimits(t, m)
	}
}

// TestSingleOversizedEntryIsCutVisibly is the one place a row can be shortened:
// a single MR whose rendered text alone exceeds a section. It is marked, not
// dropped.
func TestSingleOversizedEntryIsCutVisibly(t *testing.T) {
	t.Parallel()

	b := slack.Builder{MaxSectionChars: 120}
	d := slack.DigestData{
		Team: "t",
		ReviewsNeeded: []slack.ReviewerGroup{{
			Reviewer: slack.Mention{SlackID: "U1"},
			MRs: []slack.ReviewItem{{
				Project: "payments", IID: 1, Title: strings.Repeat("y", 500),
				Author: slack.Mention{SlackID: "U2"},
			}},
		}},
	}
	msgs := b.Build(d)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	body := blockTexts(t, msgs[0].Blocks)[2]
	if utf8.RuneCountInString(body) > 120 {
		t.Errorf("section = %d runes, want ≤ 120", utf8.RuneCountInString(body))
	}
	if !strings.HasSuffix(body, "…") {
		t.Errorf("a shortened row must be marked:\n%s", body)
	}
	if !strings.Contains(body, "payments !1") {
		t.Errorf("the MR itself must survive:\n%s", body)
	}
}

func TestHeaderIsClippedToSlacksLimit(t *testing.T) {
	t.Parallel()

	msgs := slack.BuildDigest(slack.DigestData{
		Team:          strings.Repeat("n", 400),
		AuthorActions: []slack.AuthorGroup{{Author: slack.Mention{SlackID: "U1"}, MRs: []slack.AuthorItem{{Project: "p", IID: 1}}}},
	})
	header := msgs[0].Blocks[0].Text.Text
	if n := utf8.RuneCountInString(header); n > 150 {
		t.Errorf("header = %d runes, want ≤ 150", n)
	}
	if !strings.HasSuffix(header, "…") {
		t.Errorf("a clipped header must be marked: %q", header)
	}
}

func TestBuilderFloorsAbsurdLimits(t *testing.T) {
	t.Parallel()

	// A caller passing nonsense must not be able to make pagination spin.
	b := slack.Builder{MaxBlocks: 1, MaxSectionChars: 1}
	msgs := b.Build(sampleDigest())
	if len(msgs) == 0 {
		t.Fatal("want messages")
	}
	for _, m := range msgs {
		if len(m.Blocks) > 3 {
			t.Errorf("blocks = %d, want ≤ 3", len(m.Blocks))
		}
	}
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

// assertWithinLimits checks the two Block Kit limits the builder owns.
func assertWithinLimits(t *testing.T, m slack.Message) {
	t.Helper()
	if len(m.Blocks) > slack.MaxBlocksPerMessage {
		t.Errorf("part %d has %d blocks, want ≤ %d", m.Part, len(m.Blocks), slack.MaxBlocksPerMessage)
	}
	for i, b := range m.Blocks {
		if b.Text == nil {
			continue
		}
		if n := utf8.RuneCountInString(b.Text.Text); b.Type == "section" && n > slack.MaxSectionChars {
			t.Errorf("part %d block %d = %d chars, want ≤ %d", m.Part, i, n, slack.MaxSectionChars)
		}
	}
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
