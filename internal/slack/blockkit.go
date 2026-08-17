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

// The floors a Builder works to when a caller shrinks the limits, which is what
// tests do to exercise splitting without a giant fixture. They are part of the
// contract rather than a detail, because they are the limit the builder then
// promises to honour: below them a part could not hold a header plus one block,
// or a section a head plus its content. Whatever limit a builder is working to,
// it may neither exceed it nor drop an entry to stay inside it.
const (
	// MinBlocksPerMessage is the smallest per-message block budget: a header and
	// one body block, or pagination could not make progress.
	MinBlocksPerMessage = 2
	// MinSectionChars is the smallest section the builder will work in. Small
	// enough to be a stress test — at 16 runes nothing renders legibly — and
	// large enough that the head bound and the content half both stay positive.
	MinSectionChars = 16
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
	// Project is the single project every listed MR belongs to, if there is one.
	// When set, the header names it and the rows drop the per-row prefix — on a
	// one-repository team that prefix was repeated on every line for no
	// information. Empty when the digest spans several projects, and then each row
	// carries its own.
	Project string
	// People is one entry per person who owes something, ordered by the caller.
	//
	// One entry, not two lists. The digest used to be a "Reviews needed" section
	// grouped by reviewer plus an "Author actions" section grouped by author, so a
	// person appeared in both and a merge request appeared once per reviewer: a
	// real run listed 25 merge requests in about 100 lines across two messages.
	// Grouping by person and asking "what does this person owe" answers the only
	// question a reader has.
	People []PersonDigest
	// FailedRepos is how many repositories could not be inspected during the
	// run. When non-zero the digest carries a partial-data warning rather
	// than pretending it saw everything.
	FailedRepos int
}

// PersonDigest is everything one person owes: reviews they have not delivered,
// and their own MRs that need work.
type PersonDigest struct {
	Person   Mention
	ToReview []ReviewItem
	Own      []AuthorItem
}

// Total is how many merge requests this person is on the hook for. It orders the
// digest and drives the head line.
func (p PersonDigest) Total() int { return len(p.ToReview) + len(p.Own) }

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

// ReviewItem is one MR awaiting review.
type ReviewItem struct {
	// Project is the short project label, e.g. "payments". Rendered only when
	// DigestData.Project is empty, i.e. the digest spans more than one.
	Project string
	IID     int64
	Title   string
	WebURL  string
	// Waiting is how long the MR has been waiting. Zero omits the suffix.
	//
	// There is deliberately no Author here. It cost a second line on every entry
	// ("by <mention> · waiting 4d") to tell a reviewer something the merge request
	// itself says, and on a real run that was 36 extra lines.
	Waiting time.Duration
}

// AuthorItem is one MR needing action from its author. Rows render in a fixed
// order — threads, conflicts, pipeline — so the digest reads the same way
// every day.
type AuthorItem struct {
	Project string
	IID     int64
	Title   string
	WebURL  string
	// ChangesRequestedBy are the reviewers whose "Request changes" verdict still
	// stands. Rendered first: it outranks a thread count, and it used to be
	// invisible — the author saw "3 unresolved threads" and no mention that
	// somebody had formally asked for changes.
	ChangesRequestedBy []Mention
	UnresolvedThreads  int
	MergeConflicts     bool
	PipelineFailed     bool
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
	body := b.blocks(d)
	if len(body) == 0 {
		return nil
	}
	// One block per part is spent on the header.
	parts := paginate(body, b.maxBlocks()-1)

	msgs := make([]Message, 0, len(parts))
	for i, blocks := range parts {
		title := "MR Digest — " + d.Team
		// The project name moves into the title on a single-project digest, which
		// is what buys every row the right to omit it.
		if p := strings.TrimSpace(d.Project); p != "" && !strings.EqualFold(p, d.Team) {
			title += " · " + p
		}
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
	return max(b.MaxBlocks, MinBlocksPerMessage)
}

func (b Builder) maxSectionChars() int {
	if b.MaxSectionChars <= 0 {
		return MaxSectionChars
	}
	return max(b.MaxSectionChars, MinSectionChars)
}

// blocks flattens a digest into the block sequence, before pagination.
//
// Every block is a body block: what makes a part readable on its own is the
// person head, which b.sections repeats on each continuation block, so there is
// nothing left for pagination to re-emit at a part boundary.
func (b Builder) blocks(d DigestData) []Block {
	var out []Block
	if d.FailedRepos > 0 {
		out = append(out, sectionBlock(partialLine(d.FailedRepos)))
	}

	for _, p := range d.People {
		if p.Total() == 0 {
			continue
		}
		out = append(out, b.sections(personHead(p), b.personEntries(p, d.Project == ""))...)
	}
	return out
}

// reviewDetailRows is how many of a person's pending reviews get a full row —
// marker, id, age and title. The rest are compressed into the tail, which keeps
// the marker and the id and drops the age and the title.
//
// Three, because the point of the detail rows is "start here": they are the
// oldest, and a reader who has time for more can open the tail. Listing all
// fifteen in full is what made the digest two messages long.
const reviewDetailRows = 3

// personHead is the one line that says what this person owes, e.g.
// "*@alice* · review 15 · yours 3".
func personHead(p PersonDigest) string {
	head := "*" + p.Person.render() + "*"
	if n := len(p.ToReview); n > 0 {
		head += fmt.Sprintf(" · review %d", n)
	}
	if n := len(p.Own); n > 0 {
		head += fmt.Sprintf(" · yours %d", n)
	}
	return head
}

// personEntries renders one person's rows: the oldest reviews in full, the rest
// compressed, then their own merge requests.
//
// withProject is true only for a multi-project digest; on a single-project one
// the header carries the name instead of every row repeating it.
func (b Builder) personEntries(p PersonDigest, withProject bool) []string {
	var entries []string

	detailed := min(len(p.ToReview), reviewDetailRows)
	for _, mr := range p.ToReview[:detailed] {
		entries = append(entries, reviewEntry(mr, withProject))
	}
	entries = append(entries, tailEntries(p.ToReview[detailed:], withProject, b.tailBudget())...)

	for _, mr := range p.Own {
		entries = append(entries, authorEntry(mr, withProject))
	}
	return entries
}

// tailIndent aligns the compressed tail under the detail rows above it.
const tailIndent = "   "

// tailBudget bounds one tail entry to half a section, which is exactly the
// content half sections reserves for an entry.
//
// What that buys, stated so it can be checked: sections bounds every head form
// to limit - limit/2 - 1 runes, so an entry is always offered at least limit/2 —
// and a tail entry within its budget therefore never reaches the truncating
// branch, at any limit, equality included since the branch triggers on >. The
// bound has to cover the continuation head, 14 runes longer than the plain one,
// and it is why bounding the plain head to a quarter is not the reason: a quarter
// plus 14 only leaves limit/2 from 57 runes up, so that reading held by luck.
//
// The one tail entry still cuttable is a chunk holding a single merge request
// whose rendered link alone exceeds half a section, because tailEntries takes its
// first item however long or it would make no progress. At 3000 that needs a
// 1500-rune row; it is reachable only in a test that shrinks the limit below 200.
func (b Builder) tailBudget() int { return b.maxSectionChars() / 2 }

// tailEntries renders the merge requests beyond reviewDetailRows as compressed
// entries of at most budget runes each.
//
// Several entries, not one line. A reviewer with 60 pending merge requests
// rendered a single ~3400-rune entry, which is longer than a section can hold,
// and sections can only shorten a single oversized entry by cutting it: roughly
// twenty merge requests disappeared while the "+57 more" count still promised
// them. Chunking gives sections a boundary to split on, and splitting loses
// nothing.
func tailEntries(tail []ReviewItem, withProject bool, budget int) []string {
	if len(tail) == 0 {
		return nil
	}

	var (
		entries []string
		cur     strings.Builder
		n       int // runes in cur
		count   int // items in cur
	)
	// A continuation entry carries only the indent. The count belongs to the tail
	// as a whole, so repeating it would read as a second, smaller tail.
	start := func(head string) {
		cur.Reset()
		cur.WriteString(head)
		n = runeLen(head)
		count = 0
	}
	start(fmt.Sprintf("%s+%d more:", tailIndent, len(tail)))
	for _, mr := range tail {
		item := reviewRef(mr, withProject)
		// An entry always takes its first item, however long: an entry that could
		// stay empty would make no progress and loop.
		if count > 0 && n+1+runeLen(item) > budget {
			entries = append(entries, cur.String())
			start(tailIndent)
		}
		cur.WriteByte(' ')
		cur.WriteString(item)
		n += 1 + runeLen(item)
		count++
	}
	return append(entries, cur.String())
}

// sections renders one group as section blocks, splitting at MR boundaries
// when the text limit is reached. Continuation blocks repeat the group head so
// a part that starts mid-group still says whose MRs these are.
func (b Builder) sections(head string, entries []string) []Block {
	limit := b.maxSectionChars()

	// Reserve half the section for content and bound *both* head forms to what is
	// left. Bounding only the base head was a bug with teeth: " _(continued)_" is
	// appended after that bound, so at a small limit a continuation block opened
	// 18 runes into a 16-rune section, the entry after it was offered limit-n-1 =
	// -3, truncateRunes returned "" for the non-positive budget, and the merge
	// request was gone — from a section that was itself over the limit it had been
	// given. At 3000 the same arithmetic leaves 2235 runes of slack, which is why
	// only a shrunken Builder ever showed it.
	//
	// headRoom = limit - limit/2 - 1 leaves at least limit/2 runes for the entry
	// after the newline, at every limit, which is also the promise tailBudget
	// relies on.
	headRoom := limit - limit/2 - 1
	// A quarter is the head's own share, and it is never more than headRoom for
	// any limit at or above the floor.
	head = truncateRunes(head, limit/4)
	cont := truncateRunes(head+continuedSuffix, headRoom)

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
			start(cont)
		}
		if n+1+runeLen(e) > limit {
			// A single entry that cannot fit an empty block: cut it with a visible
			// ellipsis rather than dropping the MR. The budget is at least limit/2
			// by construction — see headRoom — so the cut always leaves a line.
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

// paginate splits blocks into parts of at most budget blocks each.
func paginate(blocks []Block, budget int) [][]Block {
	var (
		parts [][]Block
		cur   []Block
	)
	for _, blk := range blocks {
		if len(cur) > 0 && len(cur)+1 > budget {
			parts = append(parts, cur)
			cur = nil
		}
		cur = append(cur, blk)
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

// Age markers. Nothing is filtered on them — every merge request is still
// listed; they only let a reader see at a glance which end of the list is
// urgent, which a bare "waiting 4d" on every row does not.
const (
	ageAlarmAfter = 7 * 24 * time.Hour
	ageWarnAfter  = 2 * 24 * time.Hour
)

func ageMarker(d time.Duration) string {
	switch {
	case d >= ageAlarmAfter:
		return "🔴 "
	case d >= ageWarnAfter:
		return "🟡 "
	default:
		return "▫️ "
	}
}

// reviewRef identifies one merge request: age marker, link, and the project
// label when the digest spans several projects.
//
// Shared by the detail rows and the compressed tail so the two cannot drift
// apart. The tail once rendered the bare link, which lost both halves of the
// identification at once: two !1404s from different repositories read
// identically, and so did a 110-day-old merge request and a 2-day-old one.
func reviewRef(mr ReviewItem, withProject bool) string {
	var b strings.Builder
	b.WriteString(ageMarker(mr.Waiting))
	b.WriteString(link(mr.WebURL, mrRef(mr.IID)))
	if withProject {
		if p := escape(strings.TrimSpace(mr.Project)); p != "" {
			b.WriteString(" _" + p + "_")
		}
	}
	return b.String()
}

// reviewEntry renders one pending review as a single line:
// "🔴 !1370 17d — CHAIN-182 optimize the Metabase wallet lookup".
func reviewEntry(mr ReviewItem, withProject bool) string {
	var b strings.Builder
	b.WriteString(reviewRef(mr, withProject))
	if mr.Waiting > 0 {
		b.WriteByte(' ')
		b.WriteString(humanDuration(mr.Waiting))
	}
	if t := shortTitle(mr.Title); t != "" {
		b.WriteString(" — ")
		b.WriteString(t)
	}
	return b.String()
}

// authorEntry renders one of the author's own merge requests on a single line,
// with the flags in a fixed order — changes requested, threads, conflicts,
// pipeline — so the digest reads the same way every day.
func authorEntry(mr AuthorItem, withProject bool) string {
	var b strings.Builder
	b.WriteString("🛠 ")
	b.WriteString(link(mr.WebURL, mrRef(mr.IID)))
	if withProject {
		if p := escape(strings.TrimSpace(mr.Project)); p != "" {
			b.WriteString(" _" + p + "_")
		}
	}

	var flags []string
	if len(mr.ChangesRequestedBy) > 0 {
		names := make([]string, 0, len(mr.ChangesRequestedBy))
		for _, m := range mr.ChangesRequestedBy {
			names = append(names, m.render())
		}
		flags = append(flags, "🔁 changes requested by "+strings.Join(names, ", "))
	}
	if mr.UnresolvedThreads > 0 {
		flags = append(flags, fmt.Sprintf("💬 %d %s", mr.UnresolvedThreads,
			plural(mr.UnresolvedThreads, "thread", "threads")))
	}
	if mr.MergeConflicts {
		flags = append(flags, "⚠️ conflicts")
	}
	if mr.PipelineFailed {
		flags = append(flags, "❌ "+link(mr.PipelineWebURL, "pipeline"))
	}
	if len(flags) > 0 {
		b.WriteString(" · ")
		b.WriteString(strings.Join(flags, " · "))
	}
	if t := shortTitle(mr.Title); t != "" {
		b.WriteString(" — ")
		b.WriteString(t)
	}
	return b.String()
}

// maxTitleRunes bounds a title so an entry stays one line in Slack. Titles here
// are commit-message length — the observed worst case was 104 characters, which
// wrapped to three lines and undid the whole point of one row per merge request.
const maxTitleRunes = 55

// shortTitle trims a title to maxTitleRunes at a word boundary. The leading
// ticket key survives by construction, since it comes first.
//
// Cut first, escape second, and mark the cut exactly once. Escaping first made
// the cut land inside an entity and left a literal "&amp" in the row; going
// through truncateRunes added an ellipsis this function then added again, so a
// single long word rendered "xxxx……"; and the word-boundary test compared a
// byte index against a rune budget, which is the same number only in ASCII. For
// Cyrillic the index runs at twice the rune count, so the guard accepted
// boundaries it exists to reject and cut Russian titles a fifth short or worse.
func shortTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if runeLen(s) <= maxTitleRunes {
		return escape(s)
	}

	// One rune of the budget belongs to the ellipsis.
	cut := []rune(s)[:maxTitleRunes-1]
	// Prefer the last word boundary, unless that throws away most of the line.
	for i := len(cut) - 1; i >= 0; i-- {
		if cut[i] != ' ' {
			continue
		}
		if i > len(cut)/2 {
			cut = cut[:i]
		}
		break
	}
	return escape(strings.TrimRight(string(cut), " ,.;:-")) + "…"
}

// mrRef is the "!481" a row links on.
func mrRef(iid int64) string { return "!" + strconv.FormatInt(iid, 10) }

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
	// Unreachable through Builder: every caller derives its limit from a floored
	// budget — maxSectionChars (≥ MinSectionChars) or the constant maxHeaderChars —
	// and sections now bounds both head forms so an entry is always offered at
	// least limit/2. The guard stays because slicing below zero would panic, and
	// because it is what turned the head-bound bug into a silently dropped merge
	// request rather than a crash: returning "" is the safer failure, not a safe one.
	if maxRunes <= 0 {
		return ""
	}
	if runeLen(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes-1]) + "…"
}
