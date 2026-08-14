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
	// overrides maps a normalised GitLab username to a Slack user ID
	// (slack.user_map).
	overrides map[string]string
}

// New builds a matcher over dir. userMap is the optional
// gitlab_username → SLACK_USER_ID override from the config; keys are
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
//  1. explicit user_map override;
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

	// 1. The override is an escape hatch, so it is answered without touching
	// the directory: it must keep working when users.list does not, and it
	// must not be second-guessed by the indexes.
	if id, ok := m.overrides[normalize(u.Username)]; ok {
		return Result{Status: Matched, SlackID: id, Display: fallback}, nil
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
