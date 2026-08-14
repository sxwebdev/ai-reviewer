package slack_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/match"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// usersListBody is a two-person workspace: Ann is findable by every index,
// and the two Bobs collide on display name.
const usersListBody = `{"ok":true,"members":[
	{"id":"U1","name":"ann","real_name":"Ann Lee",
	 "profile":{"email":"Ann@Acme.io","display_name":"Ann","real_name":"Ann Lee"}},
	{"id":"U2","name":"bob","profile":{"email":"bob@acme.io","display_name":"Bobby","real_name":"Bob Ray"}},
	{"id":"U3","name":"bob2","profile":{"email":"bob2@acme.io","display_name":"Bobby","real_name":"Bob Ray"}}
]}`

func newMatcher(t *testing.T, userMap map[string]string) (match.UserMatcher, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeJSON(t, w, usersListBody)
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv, slack.Config{})
	dir := slack.NewDirectory(c, slack.DirectoryConfig{})
	return match.New(slack.MatchDirectory(dir), userMap), &hits
}

func TestMatchThroughTheWorkspaceDirectory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		user match.GitLabUser
		want match.Result
	}{
		{
			name: "email, case-insensitively",
			user: match.GitLabUser{Username: "nobody", Email: "ANN@acme.io"},
			want: match.Result{Status: match.Matched, SlackID: "U1", Display: "Ann"},
		},
		{
			name: "handle",
			user: match.GitLabUser{Username: "ann"},
			want: match.Result{Status: match.Matched, SlackID: "U1", Display: "Ann"},
		},
		{
			name: "gitlab name against slack real name",
			user: match.GitLabUser{Username: "a.lee", Name: "ann lee"},
			want: match.Result{Status: match.Matched, SlackID: "U1", Display: "Ann"},
		},
		{
			name: "ambiguous display name",
			user: match.GitLabUser{Username: "bobby", Name: "Bob Ray"},
			want: match.Result{Status: match.Ambiguous, Display: "Bob Ray (@bobby)"},
		},
		{
			name: "not found",
			user: match.GitLabUser{Username: "john", Name: "John Smith"},
			want: match.Result{Status: match.NotFound, Display: "John Smith (@john)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m, _ := newMatcher(t, nil)
			got, err := m.Match(t.Context(), tt.user)
			if err != nil {
				t.Fatalf("Match: %v", err)
			}
			if got != tt.want {
				t.Errorf("Match = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestSecondMatchReusesTheDirectory is the caching promise: a digest matching
// dozens of people loads users.list once.
func TestSecondMatchReusesTheDirectory(t *testing.T) {
	t.Parallel()

	m, hits := newMatcher(t, nil)
	for _, u := range []match.GitLabUser{
		{Username: "ann"},
		{Username: "bob", Email: "bob@acme.io"},
		{Username: "john", Name: "John Smith"}, // walks the whole ladder
		{Username: "ann"},
	} {
		if _, err := m.Match(t.Context(), u); err != nil {
			t.Fatalf("Match(%+v): %v", u, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("users.list requests = %d, want 1", got)
	}
}

func TestOverrideDoesNotLoadTheDirectory(t *testing.T) {
	t.Parallel()

	m, hits := newMatcher(t, map[string]string{"john": "U42"})
	got, err := m.Match(t.Context(), match.GitLabUser{Username: "john", Name: "John Smith"})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got.SlackID != "U42" {
		t.Errorf("Match = %+v, want the override", got)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("users.list requests = %d, want 0", n)
	}
}

func TestDirectoryErrorReachesTheCaller(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	dir := slack.NewDirectory(c, slack.DirectoryConfig{})
	_, err := match.New(slack.MatchDirectory(dir), nil).Match(t.Context(), match.GitLabUser{Username: "ann"})
	if slack.ErrorCode(err) != "invalid_auth" {
		t.Fatalf("err = %v, want the Slack error to reach the caller", err)
	}
}

func TestUnknownFieldLooksUpNothing(t *testing.T) {
	t.Parallel()

	// Defensive: an unknown Field must return no candidates rather than
	// panicking or matching everyone.
	lister := &fakeLister{users: []slack.User{user("U1", "ann", "Ann", "Ann Lee", "ann@acme.io")}}
	dir := slack.MatchDirectory(slack.NewDirectory(lister, slack.DirectoryConfig{}))
	got, err := dir.Lookup(t.Context(), match.Field(99), "ann")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("candidates = %+v, want none", got)
	}
}

func TestMatchDirectoryMapsEveryField(t *testing.T) {
	t.Parallel()

	lister := &fakeLister{users: []slack.User{user("U1", "ann", "Ann", "Ann Lee", "ann@acme.io")}}
	dir := slack.MatchDirectory(slack.NewDirectory(lister, slack.DirectoryConfig{}))

	want := match.SlackUser{ID: "U1", Handle: "ann", DisplayName: "Ann", RealName: "Ann Lee", Email: "ann@acme.io"}
	for _, probe := range []struct {
		field match.Field
		key   string
	}{
		{match.FieldEmail, "ann@acme.io"},
		{match.FieldHandle, "ann"},
		{match.FieldDisplayName, "ann"},
		{match.FieldRealName, "ann lee"},
	} {
		got, err := dir.Lookup(t.Context(), probe.field, probe.key)
		if err != nil {
			t.Fatalf("Lookup(%v): %v", probe.field, err)
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("Lookup(%v) = %+v, want %+v", probe.field, got, want)
		}
	}
}

func TestMentionFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		res  match.Result
		want slack.Mention
	}{
		{
			name: "matched",
			res:  match.Result{Status: match.Matched, SlackID: "U1", Display: "Ann"},
			want: slack.Mention{SlackID: "U1", Display: "Ann"},
		},
		{
			name: "ambiguous keeps the text and the marker",
			res:  match.Result{Status: match.Ambiguous, Display: "Bob Ray (@bob)"},
			want: slack.Mention{Display: "Bob Ray (@bob)", Ambiguous: true},
		},
		{
			name: "not found is named, not mentioned",
			res:  match.Result{Status: match.NotFound, Display: "John Smith (@john)"},
			want: slack.Mention{Display: "John Smith (@john)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := slack.MentionFrom(tt.res); got != tt.want {
				t.Errorf("MentionFrom = %+v, want %+v", got, tt.want)
			}
		})
	}
}
