package slack

import (
	"context"

	"github.com/sxwebdev/ai-reviewer/internal/match"
)

// MatchDirectory adapts the workspace directory to match.Directory.
//
// The adapter lives here, not in internal/match, so the matcher stays free of
// Slack transport code and can be tested with a plain in-memory fake.
func MatchDirectory(d *Directory) match.Directory { return matchDirectory{d: d} }

type matchDirectory struct{ d *Directory }

// Lookup resolves one index probe, loading the workspace directory on first
// use. Keys arrive normalised from the matcher; Snapshot normalises again,
// which is idempotent and keeps both sides honest.
func (m matchDirectory) Lookup(ctx context.Context, f match.Field, key string) ([]match.SlackUser, error) {
	snap, err := m.d.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	var users []User
	switch f {
	case match.FieldEmail:
		if u, ok := snap.ByEmail(key); ok {
			users = []User{u}
		}
	case match.FieldHandle:
		users = snap.ByHandle(key)
	case match.FieldDisplayName:
		users = snap.ByDisplayName(key)
	case match.FieldRealName:
		users = snap.ByRealName(key)
	}
	out := make([]match.SlackUser, 0, len(users))
	for _, u := range users {
		out = append(out, match.SlackUser{
			ID:          u.ID,
			Handle:      u.Name,
			DisplayName: u.Profile.DisplayName,
			RealName:    u.realName(),
			Email:       u.Profile.Email,
		})
	}
	return out, nil
}

// MentionFrom turns a match result into the Mention the digest renders.
func MentionFrom(r match.Result) Mention {
	switch r.Status {
	case match.Matched:
		return Mention{SlackID: r.SlackID, Display: r.Display}
	case match.Ambiguous:
		return Mention{Display: r.Display, Ambiguous: true}
	default:
		return Mention{Display: r.Display}
	}
}
