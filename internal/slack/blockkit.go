package slack

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Slack's Block Kit limits, enforced here so a large team never gets a
// silently truncated digest.
const (
	// MaxBlocksPerMessage is Slack's per-message block cap.
	MaxBlocksPerMessage = 50
	// MaxSectionChars is Slack's cap on a section's text.
	MaxSectionChars = 3000
	// maxHeaderChars is Slack's cap on a header block's text.
	maxHeaderChars = 150
)

// continuedSuffix marks a group that spilled into a second section block.
const continuedSuffix = " _(continued)_"

// ambiguousMarker follows a person the matcher could not resolve to a single
// Slack account. They are named, not mentioned, and the ambiguity is visible
// so somebody can fix it with slack.user_map.
const ambiguousMarker = " ❓ _(ambiguous Slack match)_"

// DigestData is the input to the Block Kit builder: a plain description of one
// team's digest, with every person already resolved to a Mention. It carries no
// GitLab or Slack API types — the digest service fills it.
type DigestData struct {
	// Team is the team name, shown in the title.
	Team string
	// ReviewsNeeded lists MRs waiting on each reviewer.
	ReviewsNeeded []ReviewerGroup
	// AuthorActions lists MRs whose author has something to do: unresolved
	// threads, merge conflicts or a failed pipeline.
	AuthorActions []AuthorGroup
	// FailedRepos is how many repositories could not be inspected during the
	// run. When non-zero the digest carries a partial-data warning rather
	// than pretending it saw everything.
	FailedRepos int
}

// Mention is one person as the digest renders them: a real Slack mention when
// the matcher resolved the account, plain text otherwise.
type Mention struct {
	// SlackID is set only for a resolved account.
	SlackID string
	// Display is the human label used when there is no ID,
	// e.g. "John Smith (@john)".
	Display string
	// Ambiguous marks a person with several equally good Slack candidates.
	Ambiguous bool
}

// ReviewerGroup is the set of MRs waiting on one reviewer.
type ReviewerGroup struct {
	Reviewer Mention
	MRs      []ReviewItem
}

// ReviewItem is one MR awaiting review.
type ReviewItem struct {
	// Project is the short project label, e.g. "payments".
	Project string
	IID     int64
	Title   string
	WebURL  string
	Author  Mention
	// Waiting is how long the MR has been waiting. Zero omits the suffix.
	Waiting time.Duration
}

// AuthorGroup is the set of MRs one author has to act on.
type AuthorGroup struct {
	Author Mention
	MRs    []AuthorItem
}

// AuthorItem is one MR needing action from its author. Rows render in a fixed
// order — threads, conflicts, pipeline — so the digest reads the same way
// every day.
type AuthorItem struct {
	Project           string
	IID               int64
	Title             string
	WebURL            string
	UnresolvedThreads int
	MergeConflicts    bool
	PipelineFailed    bool
	// PipelineWebURL links "pipeline failed" straight at the failed
	// pipeline. Empty renders the row without a link.
	PipelineWebURL string
}

// Message is one Slack post. A digest that exceeds the block or character
// limits becomes several numbered messages, each delivered by its own
// slack_send job.
type Message struct {
	Part   int // 1-based
	Parts  int // total parts
	Text   string
	Blocks []Block
}

// Post turns m into a chat.postMessage payload. Link previews are off: a
// digest is a list of links, and unfurling would bury it.
func (m Message) Post(channel string) PostMessageRequest {
	return PostMessageRequest{Channel: channel, Text: m.Text, Blocks: m.Blocks}
}

// Block is one Block Kit block. Only the three kinds the digest uses are
// modelled: header, section and divider.
type Block struct {
	Type string `json:"type"`
	Text *Text  `json:"text,omitempty"`
}

// Text is a Block Kit composition object.
type Text struct {
	Type  string `json:"type"` // "mrkdwn" or "plain_text"
	Text  string `json:"text"`
	Emoji *bool  `json:"emoji,omitempty"`
}

// Builder builds digest messages. The zero value uses Slack's real limits;
// tests shrink them to exercise splitting without a giant fixture.
type Builder struct {
	MaxBlocks       int
	MaxSectionChars int
}

// BuildDigest builds d into one or more messages using Slack's real limits.
func BuildDigest(d DigestData) []Message { return Builder{}.Build(d) }

// Build renders d. It returns no messages when there is nothing to report —
// the caller decides whether silence or a "nothing to do" note is right.
func (b Builder) Build(d DigestData) []Message {
	units := b.units(d)
	if len(units) == 0 {
		return nil
	}
	// One block per part is spent on the header.
	parts := paginate(units, b.maxBlocks()-1)

	msgs := make([]Message, 0, len(parts))
	for i, blocks := range parts {
		title := "MR Digest — " + d.Team
		if len(parts) > 1 {
			title += fmt.Sprintf(" (%d/%d)", i+1, len(parts))
		}
		withHeader := make([]Block, 0, len(blocks)+1)
		withHeader = append(withHeader, headerBlock("📋 "+title))
		withHeader = append(withHeader, blocks...)
		msgs = append(msgs, Message{
			Part:   i + 1,
			Parts:  len(parts),
			Text:   title,
			Blocks: withHeader,
		})
	}
	return msgs
}

func (b Builder) maxBlocks() int {
	if b.MaxBlocks <= 0 {
		return MaxBlocksPerMessage
	}
	// A part must fit a header, a section heading and one body block, or
	// pagination could not make progress.
	return max(b.MaxBlocks, 3)
}

func (b Builder) maxSectionChars() int {
	if b.MaxSectionChars <= 0 {
		return MaxSectionChars
	}
	return max(b.MaxSectionChars, 16)
}

// unit is one block plus whether it is a section heading. Headings are
// repeated at the top of a continuation part so every part reads on its own.
type unit struct {
	block   Block
	heading bool
}

// units flattens a digest into the block sequence, before pagination.
func (b Builder) units(d DigestData) []unit {
	var out []unit
	if d.FailedRepos > 0 {
		out = append(out, unit{block: sectionBlock(partialLine(d.FailedRepos))})
	}

	if groups := nonEmptyReviewGroups(d.ReviewsNeeded); len(groups) > 0 {
		out = append(out, unit{block: sectionBlock("*👀 Reviews needed*"), heading: true})
		for _, g := range groups {
			head := fmt.Sprintf("%s — %d %s", g.Reviewer.render(), len(g.MRs), plural(len(g.MRs), "MR", "MRs"))
			entries := make([]string, 0, len(g.MRs))
			for _, mr := range g.MRs {
				entries = append(entries, reviewEntry(mr))
			}
			for _, blk := range b.sections(head, entries) {
				out = append(out, unit{block: blk})
			}
		}
	}

	if groups := nonEmptyAuthorGroups(d.AuthorActions); len(groups) > 0 {
		out = append(out, unit{block: sectionBlock("*🛠 Author actions*"), heading: true})
		for _, g := range groups {
			entries := make([]string, 0, len(g.MRs))
			for _, mr := range g.MRs {
				entries = append(entries, authorEntry(mr))
			}
			for _, blk := range b.sections(g.Author.render(), entries) {
				out = append(out, unit{block: blk})
			}
		}
	}
	return out
}

func nonEmptyReviewGroups(in []ReviewerGroup) []ReviewerGroup {
	out := make([]ReviewerGroup, 0, len(in))
	for _, g := range in {
		if len(g.MRs) > 0 {
			out = append(out, g)
		}
	}
	return out
}

func nonEmptyAuthorGroups(in []AuthorGroup) []AuthorGroup {
	out := make([]AuthorGroup, 0, len(in))
	for _, g := range in {
		if len(g.MRs) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// sections renders one group as section blocks, splitting at MR boundaries
// when the text limit is reached. Continuation blocks repeat the group head so
// a part that starts mid-group still says whose MRs these are.
func (b Builder) sections(head string, entries []string) []Block {
	limit := b.maxSectionChars()
	// Bound the head so a pathological display name can never leave an
	// entry without room.
	head = truncateRunes(head, limit/4)

	var (
		blocks []Block
		cur    strings.Builder
		n      int // runes in cur
		count  int // entries in cur
	)
	start := func(h string) {
		cur.Reset()
		cur.WriteString(h)
		n = runeLen(h)
		count = 0
	}
	start(head)
	for _, e := range entries {
		if count > 0 && n+1+runeLen(e) > limit {
			blocks = append(blocks, sectionBlock(cur.String()))
			start(head + continuedSuffix)
		}
		if n+1+runeLen(e) > limit {
			// A single entry that cannot fit an empty block: cut it with a
			// visible ellipsis rather than dropping the MR.
			e = truncateRunes(e, limit-n-1)
		}
		cur.WriteString("\n")
		cur.WriteString(e)
		n += 1 + runeLen(e)
		count++
	}
	if count > 0 {
		blocks = append(blocks, sectionBlock(cur.String()))
	}
	return blocks
}

// paginate splits units into parts of at most budget blocks each, re-emitting
// the current section heading whenever a part boundary falls inside a section.
func paginate(units []unit, budget int) [][]Block {
	var (
		parts   [][]Block
		cur     []Block
		heading *Block
	)
	for _, u := range units {
		need := 1
		if u.heading {
			need = 2 // a heading stranded at the end of a part helps nobody
		}
		if len(cur) > 0 && len(cur)+need > budget {
			parts = append(parts, cur)
			cur = nil
			if heading != nil && !u.heading {
				cur = append(cur, *heading)
			}
		}
		if u.heading {
			h := u.block
			heading = &h
		}
		cur = append(cur, u.block)
	}
	if len(cur) > 0 {
		parts = append(parts, cur)
	}
	return parts
}

// render returns the mrkdwn for m. Someone the matcher could not resolve is
// still named — the digest goes out either way.
func (m Mention) render() string {
	if id := strings.TrimSpace(m.SlackID); id != "" {
		return "<@" + id + ">"
	}
	d := escape(strings.TrimSpace(m.Display))
	if d == "" {
		d = "unknown user"
	}
	if m.Ambiguous {
		return d + ambiguousMarker
	}
	return d
}

// reviewEntry renders one MR awaiting review.
func reviewEntry(mr ReviewItem) string {
	var b strings.Builder
	b.WriteString("• ")
	b.WriteString(link(mr.WebURL, mrLabel(mr.Project, mr.IID, mr.Title)))
	b.WriteString("\n  by ")
	b.WriteString(mr.Author.render())
	if mr.Waiting > 0 {
		b.WriteString(" · waiting ")
		b.WriteString(humanDuration(mr.Waiting))
	}
	return b.String()
}

// authorEntry renders one MR needing action from its author. Row order is
// fixed: threads, conflicts, pipeline.
func authorEntry(mr AuthorItem) string {
	var b strings.Builder
	b.WriteString("• ")
	b.WriteString(link(mr.WebURL, mrLabel(mr.Project, mr.IID, mr.Title)))
	if mr.UnresolvedThreads > 0 {
		fmt.Fprintf(&b, "\n  💬 %d unresolved %s", mr.UnresolvedThreads,
			plural(mr.UnresolvedThreads, "thread", "threads"))
	}
	if mr.MergeConflicts {
		b.WriteString("\n  ⚠️ merge conflicts")
	}
	if mr.PipelineFailed {
		b.WriteString("\n  ❌ ")
		b.WriteString(link(mr.PipelineWebURL, "pipeline failed"))
	}
	return b.String()
}

// mrLabel is "payments !481 — Add payment retries".
func mrLabel(project string, iid int64, title string) string {
	label := escape(strings.TrimSpace(project)) + " !" + strconv.FormatInt(iid, 10)
	if t := escape(strings.TrimSpace(title)); t != "" {
		label += " — " + t
	}
	return label
}

// partialLine warns that the run did not see every repository (§17).
func partialLine(n int) string {
	return fmt.Sprintf("⚠️ Partial data: failed to inspect %d %s.", n, plural(n, "repository", "repositories"))
}

func headerBlock(text string) Block {
	emoji := true
	return Block{Type: "header", Text: &Text{
		Type: "plain_text",
		// The header is a title, not content: Slack rejects anything longer
		// than 150 characters, so it is clipped rather than split.
		Text:  truncateRunes(collapseLines(text), maxHeaderChars),
		Emoji: &emoji,
	}}
}

func sectionBlock(text string) Block {
	return Block{Type: "section", Text: &Text{Type: "mrkdwn", Text: text}}
}

// link renders a Slack mrkdwn link. label must already be escaped.
func link(rawURL, label string) string {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return label
	}
	return "<" + escapeURL(u) + "|" + label + ">"
}

// escape applies Slack's rule for text: & < > must be escaped, or a title
// containing "<script>" or "a > b" breaks the message.
func escape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// escapeURL removes the three characters that would terminate a mrkdwn link
// early, plus any line break.
func escapeURL(u string) string {
	r := strings.NewReplacer(
		"<", "%3C",
		">", "%3E",
		"|", "%7C",
		"\n", "",
		"\r", "",
		" ", "%20",
	)
	return r.Replace(u)
}

// collapseLines flattens a string to a single line (headers are one line).
func collapseLines(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// humanDuration renders a waiting time the way a human reads it: minutes for
// the first hour, then hours, then days.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return strconv.Itoa(max(int(d/time.Minute), 1)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// truncateRunes cuts s to at most maxRunes runes, marking the cut with an
// ellipsis so a shortened line is never mistaken for the whole line.
func truncateRunes(s string, maxRunes int) string {
	// Unreachable through Builder — every caller passes a limit derived from
	// maxSectionChars, which floors at 16 — but slicing below zero would
	// panic, so the guard stays.
	if maxRunes <= 0 {
		return ""
	}
	if runeLen(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes-1]) + "…"
}
