// Package match resolves GitLab users to Slack accounts for the MR digest.
//
// It is deliberately free of both GitLab and Slack transport code: the caller
// adapts its own user type into GitLabUser and plugs a Directory over the Slack
// workspace index, so the matching rules can be tested without a server at
// either end.
package match

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Status is the outcome of a match.
type Status int

const (
	// NotFound means no Slack account corresponds to the GitLab user. The
	// digest still lists them, by name, without a mention.
	NotFound Status = iota
	// Matched means exactly one Slack account was identified.
	Matched
	// Ambiguous means several Slack accounts were equally good candidates.
	// No pick is made — a wrong mention is worse than a missing one.
	Ambiguous
)

func (s Status) String() string {
	switch s {
	case Matched:
		return "matched"
	case Ambiguous:
		return "ambiguous"
	case NotFound:
		return "not_found"
	default:
		return fmt.Sprintf("status(%d)", int(s))
	}
}

// GitLabUser is the input side of a match. It is defined here rather than
// imported so this package stays independent of the GitLab client.
type GitLabUser struct {
	Username string // GitLab @username
	Name     string // GitLab display name, e.g. "John Smith"
	Email    string // often empty: GitLab only exposes another user's email to an admin token
}

// Result is the outcome of matching one GitLab user.
type Result struct {
	Status Status
	// SlackID is the Slack user ID, set only when Status is Matched.
	SlackID string
	// Display is a human label: the Slack display name when matched, and
	// "John Smith (@john)" otherwise, so the digest can still name the
	// person it cannot ping.
	Display string
	// Note explains an outcome the caller should log, and is set only when the
	// explanation is not obvious from Status: today, a user_map entry that did not
	// resolve. A misconfigured override must not look like an ordinary unmatched
	// user, or nobody ever finds out the entry is wrong.
	Note string
}

// SlackUser is the subset of a Slack directory entry the matcher compares
// against.
type SlackUser struct {
	ID          string
	Handle      string // Slack `name`
	DisplayName string
	RealName    string
	Email       string
}

// Field names one of the indexes a Directory can be queried by.
type Field int

const (
	FieldEmail Field = iota
	FieldHandle
	FieldDisplayName
	FieldRealName
)

func (f Field) String() string {
	switch f {
	case FieldEmail:
		return "email"
	case FieldHandle:
		return "handle"
	case FieldDisplayName:
		return "display_name"
	case FieldRealName:
		return "real_name"
	default:
		return fmt.Sprintf("field(%d)", int(f))
	}
}

// Directory is the Slack workspace index the matcher reads. Keys arrive
// already normalised (trimmed, whitespace-collapsed, lowercased); an
// implementation may normalise again, the operation is idempotent.
//
// slack.MatchDirectory adapts the real workspace directory to this interface.
type Directory interface {
	Lookup(ctx context.Context, f Field, key string) ([]SlackUser, error)
}

// UserMatcher resolves a GitLab user to a Slack account.
type UserMatcher interface {
	Match(ctx context.Context, u GitLabUser) (Result, error)
}

// Matcher is the default UserMatcher.
type Matcher struct {
	dir Directory
	// overrides maps a normalised GitLab username to a Slack user id, @handle or
	// email (slack.user_map). ParseOverride decides which of the three a value
	// is; resolveOverride is what each form then costs.
	overrides map[string]string
}

// slackIDPattern recognises a Slack user id: U or W, then eight or more
// uppercase alphanumerics. Matching it is what keeps an id-valued override
// answerable without touching the directory.
//
// Both halves of the shape are load-bearing, and each was got wrong once:
//
// Length is Slack's documented format — a user id is nine characters or more
// (`U012AB3CD`), on every workspace, including the oldest. The floor was `{2,}`
// for a while, lowered purely so short test fixtures (`U42`, `U999`) would keep
// working and then justified in a comment as tolerating ids from old workspaces.
// No such id exists; the fixtures were the only reason, and a fixture is not a
// reason. That looseness classified *any* all-caps U/W-initial string of three
// characters or more as an id, which is precisely what an operator produces by
// typing a colleague's handle in capitals: `{jsmith: WENDY}` mentioned <@WENDY>.
//
// Case is the discriminator above that floor: Slack lower-cases the `name` it
// serves as a handle, so an all-caps value of id length is an id. Nothing may
// fold the case before matching — that too happened, and it made every
// u/w-initial handle an "id", so `{jsmith: wendy}` resolved to <@WENDY> offline
// with no lookup, no Note and no log line, while `slack_user_match_total` counted
// a success and `doctor` reported "no active Slack account has this user id" for
// a handle somebody actually holds.
//
// The residual false-positive class, stated plainly: an all-caps handle at least
// nine characters long (`UNDERWATER`) is still read as an id. It is rare, it
// produces a broken mention rather than a mention of the wrong person, and
// `doctor` resolves ids against the directory too — so a value that is not a real
// account is reported before any digest goes out. That last property is the one
// that makes the residue tolerable; do not remove it.
var slackIDPattern = regexp.MustCompile(`^[UW][A-Z0-9]{8,}$`)

// OverrideForm is the form one slack.user_map value takes. It decides whether
// resolving the value needs the Slack directory at all.
type OverrideForm int

const (
	// OverrideEmpty is a value that is blank once trimmed. It is named rather
	// than reported as a handle so callers can say "nothing was configured"
	// instead of looking up "".
	OverrideEmpty OverrideForm = iota
	// OverrideID is a Slack user id (U…/W…), the one form that resolves offline.
	OverrideID
	// OverrideHandle is a Slack @handle, with or without the @.
	OverrideHandle
	// OverrideEmail is an email address.
	OverrideEmail
)

func (f OverrideForm) String() string {
	switch f {
	case OverrideID:
		return "id"
	case OverrideHandle:
		return "handle"
	case OverrideEmail:
		return "email"
	case OverrideEmpty:
		return "empty"
	default:
		return fmt.Sprintf("override_form(%d)", int(f))
	}
}

// Override is one slack.user_map value, classified.
type Override struct {
	// Form decides how Key is meant to be used.
	Form OverrideForm
	// Key is what the lookup takes: the id verbatim for OverrideID — Slack ids
	// are case-sensitive and the pattern already required upper case — and the
	// normalised comparison form for a handle or an email.
	Key string
}

// ParseOverride classifies one slack.user_map value: id, handle, email or empty.
//
// This is the single owner of the override grammar, and it is exported and pure
// for one reason. Two callers must agree on it exactly — the matcher, which
// resolves entries when a digest is built, and `doctor`, whose whole purpose is to
// tell an operator beforehand whether those entries will resolve. `doctor` used to
// reimplement the @-stripping, the "an @ elsewhere means email" rule and the
// ambiguity policy, sharing only the id predicate; two copies of a grammar drift,
// and a `doctor` that classifies a value differently from the matcher blesses a
// map that mentions nobody. Neither side may re-derive any of this locally.
//
// It performs no I/O by design: what the Key resolves *to* is the caller's
// business, because the two callers ask different questions of different
// indexes — the matcher through Directory, doctor against a slack.Snapshot.
func ParseOverride(value string) Override {
	v := strings.TrimSpace(value)
	switch {
	case v == "":
		return Override{Form: OverrideEmpty}
	case slackIDPattern.MatchString(v):
		return Override{Form: OverrideID, Key: v}
	}
	// A leading @ is how people write handles; an @ anywhere else makes it an
	// email. Both are stripped to the bare key the indexes hold.
	key := strings.TrimPrefix(v, "@")
	if strings.Contains(key, "@") {
		return Override{Form: OverrideEmail, Key: normalize(key)}
	}
	return Override{Form: OverrideHandle, Key: normalize(key)}
}

// New builds a matcher over dir. userMap is the optional
// gitlab_username → Slack account override from the config; keys are
// normalised, so it tolerates however the operator typed them.
func New(dir Directory, userMap map[string]string) *Matcher {
	m := &Matcher{dir: dir, overrides: make(map[string]string, len(userMap))}
	for gitlabUser, slackID := range userMap {
		k := normalize(gitlabUser)
		v := strings.TrimSpace(slackID)
		if k == "" || v == "" {
			continue
		}
		m.overrides[k] = v
	}
	return m
}

var _ UserMatcher = (*Matcher)(nil)

// Match resolves u, in the fixed order of plan §13.2:
//
//  1. explicit user_map override (an id answers offline; a handle or email is
//     resolved, and a failure to resolve stops here rather than falling through);
//  2. normalised email;
//  3. GitLab username against Slack handle / display name / real name, then
//     GitLab name against real name / display name;
//  4. more than one candidate at any step → Ambiguous;
//  5. otherwise NotFound.
//
// The first probe that produces candidates decides. Falling through an
// ambiguous probe to a later, luckier one would make the outcome depend on the
// probe order in a way nobody can predict from the data.
func (m *Matcher) Match(ctx context.Context, u GitLabUser) (Result, error) {
	fallback := Fallback(u)

	// 1. The override wins outright — it is the escape hatch for whatever the
	// indexes get wrong.
	if v, ok := m.overrides[normalize(u.Username)]; ok {
		return m.resolveOverride(ctx, v, fallback)
	}

	probes := make([]struct {
		field Field
		key   string
	}, 0, 5)
	add := func(f Field, raw string) {
		if k := normalize(raw); k != "" {
			probes = append(probes, struct {
				field Field
				key   string
			}{f, k})
		}
	}
	// 2. Email is the only identifier both systems agree on exactly.
	add(FieldEmail, u.Email)
	// 3. Then names, most specific first.
	add(FieldHandle, u.Username)
	add(FieldDisplayName, u.Username)
	add(FieldRealName, u.Username)
	add(FieldRealName, u.Name)
	add(FieldDisplayName, u.Name)

	for _, p := range probes {
		candidates, err := m.dir.Lookup(ctx, p.field, p.key)
		if err != nil {
			return Result{}, fmt.Errorf("slack directory lookup by %s: %w", p.field, err)
		}
		candidates = dedupe(candidates)
		switch len(candidates) {
		case 0:
			continue
		case 1:
			c := candidates[0]
			return Result{Status: Matched, SlackID: c.ID, Display: label(c, fallback)}, nil
		default:
			// 4. Never a random pick.
			return Result{Status: Ambiguous, Display: fallback}, nil
		}
	}

	// 5.
	return Result{Status: NotFound, Display: fallback}, nil
}

// resolveOverride turns one user_map value into a Result.
//
// The form of the value — ParseOverride's verdict, never a local re-reading of
// it — decides whether the directory is consulted, and that is the whole design.
// A Slack id is answered offline, which is what makes the override an escape
// hatch worth having: the case it exists for is precisely the one where
// users.list is unavailable (a missing users:read scope, a rate limit, an
// outage), and an override that needed the directory would fail exactly then.
//
// Handles and emails are accepted because ids are the one identifier an operator
// cannot look up without the API — which made the escape hatch unusable by hand.
// They cost a directory read, and they are the forms a human actually knows.
//
// A value that does not resolve is NOT quietly retried through the normal probe
// ladder: an explicit override is a statement about a specific person, and
// silently substituting a name-similarity guess for it would hide the mistake
// behind a plausible mention. It reports NotFound with a Note the caller logs.
func (m *Matcher) resolveOverride(ctx context.Context, value, fallback string) (Result, error) {
	o := ParseOverride(value)

	var field Field
	switch o.Form {
	case OverrideID:
		// The offline answer, and the reason the escape hatch is worth having.
		// The id is taken at face value; `doctor` is the side that verifies it.
		return Result{Status: Matched, SlackID: o.Key, Display: fallback}, nil
	case OverrideHandle:
		field = FieldHandle
	case OverrideEmail:
		field = FieldEmail
	case OverrideEmpty:
		// New drops blank values, so this is unreachable through Match. Handled
		// anyway rather than probing the directory for "" — which matches
		// everybody or nobody depending on the index.
		return Result{Status: NotFound, Display: fallback, Note: "user_map entry is empty"}, nil
	default:
		return Result{}, fmt.Errorf("unclassifiable user_map entry %q (%s)", value, o.Form)
	}

	candidates, err := m.dir.Lookup(ctx, field, o.Key)
	if err != nil {
		return Result{}, fmt.Errorf("slack directory lookup by %s for user_map entry %q: %w", field, value, err)
	}
	switch candidates = dedupe(candidates); len(candidates) {
	case 1:
		c := candidates[0]
		return Result{Status: Matched, SlackID: c.ID, Display: label(c, fallback)}, nil
	case 0:
		return Result{
			Status:  NotFound,
			Display: fallback,
			Note:    fmt.Sprintf("user_map entry %q matched no active Slack account by %s", value, field),
		}, nil
	default:
		return Result{
			Status:  Ambiguous,
			Display: fallback,
			Note:    fmt.Sprintf("user_map entry %q matched %d Slack accounts by %s; use the Slack user id instead", value, len(candidates), field),
		}, nil
	}
}

// Fallback renders a GitLab user the way the digest shows someone it cannot
// mention: "John Smith (@john)".
func Fallback(u GitLabUser) string {
	name := strings.TrimSpace(u.Name)
	username := strings.TrimSpace(u.Username)
	switch {
	case name != "" && username != "":
		return name + " (@" + username + ")"
	case username != "":
		return "@" + username
	case name != "":
		return name
	default:
		return "unknown user"
	}
}

// label is the display text for a matched Slack account.
func label(c SlackUser, fallback string) string {
	for _, s := range []string{c.DisplayName, c.RealName, c.Handle} {
		if s := strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return fallback
}

// dedupe removes repeated Slack IDs. A directory that indexes the same person
// under two spellings of one key must not make them look like two people.
func dedupe(in []SlackUser) []SlackUser {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0:0]
	for _, u := range in {
		if _, ok := seen[u.ID]; ok {
			continue
		}
		seen[u.ID] = struct{}{}
		out = append(out, u)
	}
	return out
}

// normalize is the comparison form for every key: trim, collapse internal
// whitespace, lowercase. Nothing fuzzy is layered on top.
func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
