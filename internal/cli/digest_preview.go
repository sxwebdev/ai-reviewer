package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/app"
	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// previewDigests builds each team's digest in this process and prints it.
//
// The second exception to "the CLI enqueues, it does not do the work" (§15),
// and it earns it the same way `review --local` does: what is being inspected is
// the *content* of a digest, and a queued job puts that content in a
// digest_messages row and a log line rather than in front of the person asking.
// Nothing here writes: no digest_runs row, no digest_messages, no slack_send
// job, no chat.postMessage — so unlike `digest --force` it cannot consume a
// slot, and unlike `slack_send_enabled: false` it does not have to be
// configured, deployed and waited for.
func previewDigests(ctx context.Context, w io.Writer, a *app.App, teams []domain.Team, asJSON bool, now time.Time) error {
	// The full runtime, because a digest reads GitLab, Linear and the Slack
	// directory, and the service that knows how to do that is the one the
	// scheduled build uses. Postgres comes with it and stays unused by this path.
	rt, err := a.Runtime(ctx)
	if err != nil {
		return err
	}
	defer rt.Close(ctx)

	out := &printer{w: w}
	for i, team := range teams {
		if i > 0 {
			out.printf("\n")
		}
		// Whether the scheduled run would have happened today is half of what
		// somebody asking "why is the channel quiet" needs, and the preview is
		// exactly where they ask it.
		skipped := ""
		if schedule, err := teamSchedule(team); err == nil && schedule.Skipped(now) {
			skipped = fmt.Sprintf("%s is a skipped day (%s): the scheduled digest would not be sent",
				now.In(schedule.Location()).Format("Monday 2006-01-02"), schedule.Skip.Describe())
		}

		preview, err := rt.Service.PreviewDigest(ctx, team)
		if err != nil {
			// One team's dead GitLab must not cost the others their preview, for
			// the same reason it does not cost them their digest.
			out.printf("%s: %v\n", team.Name, err)
			continue
		}
		if err := printDigestPreview(w, preview, skipped, asJSON); err != nil {
			return err
		}
	}
	return out.err
}

// printDigestPreview writes one team's preview. skipped, when non-empty, says
// that today is a day this team's schedule does not fire on.
func printDigestPreview(w io.Writer, p *service.DigestPreview, skipped string, asJSON bool) error {
	out := &printer{w: w}
	out.printf("── %s → %s ──\n", p.Team.Name, channelLabel(p.Team.SlackChannel))
	out.printf("%d merge request(s) · Linear In Review: %s · %d part(s)\n",
		p.MRCount, linearCountLabel(p), len(p.Messages))
	if skipped != "" {
		out.printf("note: %s\n", skipped)
	}
	// Named here rather than left to the message body: the digest's own
	// partial-data line says a repository was missed, never which failure did it,
	// and a preview is exactly where somebody is trying to find out.
	if p.FailedRepos > 0 {
		out.printf("warning: %d of %d repositories could not be inspected\n",
			p.FailedRepos, len(p.Team.Repositories))
	}
	if p.LinearErr != nil {
		out.printf("warning: linear: %v\n", p.LinearErr)
	}
	out.printf("dry run: nothing was written to the database and nothing was sent to Slack\n")

	switch {
	case len(p.Messages) == 0:
		out.printf("\nnothing to report — no message would be sent\n")

	case asJSON:
		// HTML escaping off: it is what turns every mention and every link into
		// "\u003c@U42\u003e" on a screen somebody is reading. The JSON value is the
		// same one either way — this only decides which of two encodings of it is
		// printed, and the Slack client encodes its own request regardless.
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		for _, m := range p.Messages {
			out.printf("\n")
			if err := enc.Encode(m.Post(p.Team.SlackChannel)); err != nil {
				return fmt.Errorf("render part %d: %w", m.Part, err)
			}
		}

	default:
		names := mentionNames(p.Data)
		for _, m := range p.Messages {
			out.printf("\n[part %d/%d]\n", m.Part, m.Parts)
			for _, b := range m.Blocks {
				if b.Text == nil {
					continue
				}
				out.printf("%s\n", plainText(b.Text.Text, names))
			}
		}
	}
	return out.err
}

// printer writes a preview and keeps the first failure, so the body above reads
// as the sequence of lines it is rather than as an error check per line. The
// only realistic failure is a closed pipe — `| head` on a long digest — and
// reporting that once at the end is enough.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

// mrkdwnLink matches Slack's "<url|label>" and "<url>" link forms. Mentions are
// substituted before this runs, so "<@U42>" can never reach it.
var mrkdwnLink = regexp.MustCompile(`<([^<>|]*)\|([^<>]*)>|<(https?://[^<>|]*)>`)

// plainText renders one mrkdwn block the way Slack draws it.
//
// A preview is read, not parsed, and the two mrkdwn constructs the digest uses
// are exactly the two a terminal cannot draw: "<@U024BE7LH>" is an id rather
// than a person, and "<https://gitlab.example.com/group/sub/project/-/merge_requests/1395|!1395>"
// is ninety characters of URL in front of the two-character label Slack shows —
// on eleven rows that is the whole screen, and the wording nobody could then
// read is what this command exists to check. Emphasis markers stay: they are one
// character and they say which line is a heading.
//
// Nothing is invented. Every name comes from the digest's own resolved mentions,
// and `--json` prints the untouched payload — URLs, escapes and all — for anyone
// checking what Slack actually receives.
func plainText(text string, names *strings.Replacer) string {
	text = names.Replace(text)
	text = mrkdwnLink.ReplaceAllString(text, "$2$3")
	// Last, so a title that legitimately contains "<" — escaped to "&lt;" by the
	// builder, exactly so Slack does not read it as markup — cannot be turned
	// back into something the link pass would have eaten.
	return strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(text)
}

// mentionNames replaces every Slack mention the digest renders with the name it
// stands for.
//
// The mapping is the digest's own — the matcher already resolved both halves —
// so a preview can never invent a name for an id the message does not carry.
func mentionNames(d slack.DigestData) *strings.Replacer {
	var pairs []string
	add := func(m slack.Mention) {
		id, display := strings.TrimSpace(m.SlackID), strings.TrimSpace(m.Display)
		if id == "" || display == "" {
			return
		}
		pairs = append(pairs, "<@"+id+">", "@"+display)
	}
	for _, person := range d.People {
		add(person.Person)
		for _, own := range person.Own {
			for _, requester := range own.ChangesRequestedBy {
				add(requester)
			}
		}
	}
	return strings.NewReplacer(pairs...)
}

// linearCountLabel distinguishes a healthy zero from a source that said nothing.
func linearCountLabel(p *service.DigestPreview) string {
	if !p.Data.LinearEnabled {
		return "n/a"
	}
	return fmt.Sprint(p.LinearIssueCount)
}

// channelLabel names the destination the digest would have gone to.
func channelLabel(channel string) string {
	if c := strings.TrimSpace(channel); c != "" {
		return c
	}
	return "(no slack channel configured)"
}
