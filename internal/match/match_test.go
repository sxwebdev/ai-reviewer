package match_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/match"
)

// fakeDirectory is an in-memory workspace index. It counts lookups so tests can
// assert how far down the ladder a match went.
type fakeDirectory struct {
	idx    map[match.Field]map[string][]match.SlackUser
	probes []string
	err    error
}

func newFakeDirectory(users ...match.SlackUser) *fakeDirectory {
	f := &fakeDirectory{idx: map[match.Field]map[string][]match.SlackUser{}}
	add := func(field match.Field, key string, u match.SlackUser) {
		key = normalize(key)
		if key == "" {
			return
		}
		if f.idx[field] == nil {
			f.idx[field] = map[string][]match.SlackUser{}
		}
		f.idx[field][key] = append(f.idx[field][key], u)
	}
	for _, u := range users {
		add(match.FieldEmail, u.Email, u)
		add(match.FieldHandle, u.Handle, u)
		add(match.FieldDisplayName, u.DisplayName, u)
		add(match.FieldRealName, u.RealName, u)
	}
	return f
}

func (f *fakeDirectory) Lookup(_ context.Context, field match.Field, key string) ([]match.SlackUser, error) {
	f.probes = append(f.probes, field.String()+"="+key)
	if f.err != nil {
		return nil, f.err
	}
	return f.idx[field][key], nil
}

func normalize(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

var (
	ann = match.SlackUser{ID: "U1", Handle: "ann", DisplayName: "Ann", RealName: "Ann Lee", Email: "ann@acme.io"}
	bob = match.SlackUser{ID: "U2", Handle: "bob", DisplayName: "Bobby", RealName: "Bob Ray", Email: "bob@acme.io"}
	// bob2 collides with bob on both display and real name.
	bob2 = match.SlackUser{ID: "U3", Handle: "bob-2", DisplayName: "Bobby", RealName: "Bob Ray", Email: "bob2@acme.io"}
)

func TestMatchLadder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		user       match.GitLabUser
		userMap    map[string]string
		want       match.Result
		wantProbes []string
	}{
		{
			name: "exact email",
			user: match.GitLabUser{Username: "nobody", Name: "No Body", Email: "ann@acme.io"},
			want: match.Result{Status: match.Matched, SlackID: "U1", Display: "Ann"},
			// The email hit ends the ladder: no name probes follow.
			wantProbes: []string{"email=ann@acme.io"},
		},
		{
			name:       "email is normalised",
			user:       match.GitLabUser{Username: "nobody", Email: "  ANN@Acme.IO "},
			want:       match.Result{Status: match.Matched, SlackID: "U1", Display: "Ann"},
			wantProbes: []string{"email=ann@acme.io"},
		},
		{
			name:       "username against handle",
			user:       match.GitLabUser{Username: "Ann", Name: "Someone Else"},
			want:       match.Result{Status: match.Matched, SlackID: "U1", Display: "Ann"},
			wantProbes: []string{"handle=ann"},
		},
		{
			name:       "username against display name",
			user:       match.GitLabUser{Username: "Bobby"},
			want:       match.Result{Status: match.Ambiguous, Display: "@Bobby"},
			wantProbes: []string{"handle=bobby", "display_name=bobby"},
		},
		{
			name:       "username against real name",
			user:       match.GitLabUser{Username: "Ann  Lee"},
			want:       match.Result{Status: match.Matched, SlackID: "U1", Display: "Ann"},
			wantProbes: []string{"handle=ann lee", "display_name=ann lee", "real_name=ann lee"},
		},
		{
			name: "gitlab name against real name",
			user: match.GitLabUser{Username: "unknown-handle", Name: "ANN LEE"},
			want: match.Result{Status: match.Matched, SlackID: "U1", Display: "Ann"},
			wantProbes: []string{
				"handle=unknown-handle", "display_name=unknown-handle", "real_name=unknown-handle",
				"real_name=ann lee",
			},
		},
		{
			name: "gitlab name against display name",
			user: match.GitLabUser{Username: "unknown-handle", Name: "ann"},
			want: match.Result{Status: match.Matched, SlackID: "U1", Display: "Ann"},
			wantProbes: []string{
				"handle=unknown-handle", "display_name=unknown-handle", "real_name=unknown-handle",
				"real_name=ann", "display_name=ann",
			},
		},
		{
			name: "not found",
			user: match.GitLabUser{Username: "john", Name: "John Smith", Email: "john@other.io"},
			want: match.Result{Status: match.NotFound, Display: "John Smith (@john)"},
			wantProbes: []string{
				"email=john@other.io",
				"handle=john", "display_name=john", "real_name=john",
				"real_name=john smith", "display_name=john smith",
			},
		},
		{
			name:       "nothing to probe",
			user:       match.GitLabUser{},
			want:       match.Result{Status: match.NotFound, Display: "unknown user"},
			wantProbes: nil,
		},
		{
			name:    "user_map override wins over email",
			user:    match.GitLabUser{Username: "bob", Name: "Bob Ray", Email: "ann@acme.io"},
			userMap: map[string]string{"bob": "U999"},
			want:    match.Result{Status: match.Matched, SlackID: "U999", Display: "Bob Ray (@bob)"},
			// The escape hatch never touches the directory, so it keeps
			// working when users.list does not.
			wantProbes: nil,
		},
		{
			name:    "user_map keys are normalised",
			user:    match.GitLabUser{Username: "Bob", Name: "Bob Ray"},
			userMap: map[string]string{" BOB ": " U999 "},
			// The display keeps the GitLab spelling; only the lookup key is normalised.
			want:       match.Result{Status: match.Matched, SlackID: "U999", Display: "Bob Ray (@Bob)"},
			wantProbes: nil,
		},
		{
			name:       "blank user_map entries are ignored",
			user:       match.GitLabUser{Username: "ann"},
			userMap:    map[string]string{"ann": "  ", "": "U5"},
			want:       match.Result{Status: match.Matched, SlackID: "U1", Display: "Ann"},
			wantProbes: []string{"handle=ann"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := newFakeDirectory(ann, bob, bob2)
			got, err := match.New(dir, tt.userMap).Match(t.Context(), tt.user)
			if err != nil {
				t.Fatalf("Match: %v", err)
			}
			if got != tt.want {
				t.Errorf("Match = %+v, want %+v", got, tt.want)
			}
			if !equal(dir.probes, tt.wantProbes) {
				t.Errorf("probes = %v, want %v", dir.probes, tt.wantProbes)
			}
		})
	}
}

// TestAmbiguousNeverPicks is the rule that matters most: a wrong mention is
// worse than a missing one.
func TestAmbiguousNeverPicks(t *testing.T) {
	t.Parallel()

	dir := newFakeDirectory(ann, bob, bob2)
	m := match.New(dir, nil)
	u := match.GitLabUser{Username: "bobby", Name: "Bob Ray"}

	for i := range 200 {
		got, err := m.Match(t.Context(), u)
		if err != nil {
			t.Fatalf("Match: %v", err)
		}
		if got.Status != match.Ambiguous {
			t.Fatalf("run %d: status = %v, want Ambiguous", i, got.Status)
		}
		if got.SlackID != "" {
			t.Fatalf("run %d: picked %q from two candidates", i, got.SlackID)
		}
		if got.Display != "Bob Ray (@bobby)" {
			t.Fatalf("run %d: display = %q", i, got.Display)
		}
	}
}

func TestDuplicateCandidatesAreOnePerson(t *testing.T) {
	t.Parallel()

	// A directory that returns the same person twice must not look ambiguous.
	dir := newFakeDirectory(ann, ann)
	got, err := match.New(dir, nil).Match(t.Context(), match.GitLabUser{Username: "ann"})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got.Status != match.Matched || got.SlackID != "U1" {
		t.Fatalf("Match = %+v, want a single match on U1", got)
	}
}

func TestLookupErrorIsReported(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("users.list is down")
	dir := newFakeDirectory(ann)
	dir.err = wantErr

	_, err := match.New(dir, nil).Match(t.Context(), match.GitLabUser{Username: "ann", Email: "ann@acme.io"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "email") {
		t.Errorf("error should name the failed probe: %v", err)
	}
}

func TestOverrideSurvivesADeadDirectory(t *testing.T) {
	t.Parallel()

	dir := newFakeDirectory(ann)
	dir.err = errors.New("users.list is down")

	got, err := match.New(dir, map[string]string{"john": "U42"}).
		Match(t.Context(), match.GitLabUser{Username: "john", Name: "John Smith"})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got.Status != match.Matched || got.SlackID != "U42" {
		t.Fatalf("Match = %+v, want the override to win", got)
	}
}

func TestMatchedDisplayFallsBackThroughSlackFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		user match.SlackUser
		want string
	}{
		{"display name", match.SlackUser{ID: "U1", Handle: "ann", DisplayName: "Ann", RealName: "Ann Lee"}, "Ann"},
		{"real name", match.SlackUser{ID: "U1", Handle: "ann", RealName: "Ann Lee"}, "Ann Lee"},
		{"handle", match.SlackUser{ID: "U1", Handle: "ann"}, "ann"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := newFakeDirectory(tt.user)
			got, err := match.New(dir, nil).Match(t.Context(), match.GitLabUser{Username: "ann"})
			if err != nil {
				t.Fatalf("Match: %v", err)
			}
			if got.Display != tt.want {
				t.Errorf("Display = %q, want %q", got.Display, tt.want)
			}
		})
	}

	// A directory entry with an ID and nothing else still matches; the digest
	// mentions it by ID anyway.
	dir := &fakeDirectory{idx: map[match.Field]map[string][]match.SlackUser{
		match.FieldHandle: {"ann": {{ID: "U1"}}},
	}}
	got, err := match.New(dir, nil).Match(t.Context(), match.GitLabUser{Username: "ann", Name: "Ann Lee"})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got.Display != "Ann Lee (@ann)" {
		t.Errorf("Display = %q, want the GitLab fallback", got.Display)
	}
}

func TestFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		user match.GitLabUser
		want string
	}{
		{"both", match.GitLabUser{Username: "john", Name: "John Smith"}, "John Smith (@john)"},
		{"username only", match.GitLabUser{Username: "john"}, "@john"},
		{"name only", match.GitLabUser{Name: "John Smith"}, "John Smith"},
		{"neither", match.GitLabUser{Email: "john@acme.io"}, "unknown user"},
		{"trimmed", match.GitLabUser{Username: " john ", Name: " John Smith "}, "John Smith (@john)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := match.Fallback(tt.user); got != tt.want {
				t.Errorf("Fallback = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStringers(t *testing.T) {
	t.Parallel()

	statuses := map[match.Status]string{
		match.Matched: "matched", match.Ambiguous: "ambiguous", match.NotFound: "not_found",
		match.Status(9): "status(9)",
	}
	for s, want := range statuses {
		if got := s.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", int(s), got, want)
		}
	}
	fields := map[match.Field]string{
		match.FieldEmail: "email", match.FieldHandle: "handle",
		match.FieldDisplayName: "display_name", match.FieldRealName: "real_name",
		match.Field(9): "field(9)",
	}
	for f, want := range fields {
		if got := f.String(); got != want {
			t.Errorf("Field(%d).String() = %q, want %q", int(f), got, want)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
