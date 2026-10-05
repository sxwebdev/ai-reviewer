package match_test

import (
	"context"
	"errors"
	"slices"
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
	// uly and wen exist for one reason: their handles start with the two letters
	// a Slack user id starts with. Case is the only thing separating the two
	// forms, so these are the values that catch a classifier which folds it.
	uly = match.SlackUser{ID: "U7ULY", Handle: "ulyana", DisplayName: "Ulyana", RealName: "Ulyana Kim", Email: "uly@acme.io"}
	wen = match.SlackUser{ID: "W8WEN", Handle: "wendy", DisplayName: "Wendy", RealName: "Wendy Ho", Email: "wendy@acme.io"}
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
			userMap: map[string]string{"bob": "UOVERRIDE1"},
			want:    match.Result{Status: match.Matched, SlackID: "UOVERRIDE1", Display: "Bob Ray (@bob)"},
			// The escape hatch never touches the directory, so it keeps
			// working when users.list does not.
			wantProbes: nil,
		},
		{
			name:    "user_map keys are normalised",
			user:    match.GitLabUser{Username: "Bob", Name: "Bob Ray"},
			userMap: map[string]string{" BOB ": " UOVERRIDE1 "},
			// The display keeps the GitLab spelling; only the lookup key is normalised.
			want:       match.Result{Status: match.Matched, SlackID: "UOVERRIDE1", Display: "Bob Ray (@Bob)"},
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

	got, err := match.New(dir, map[string]string{"john": "UOFFLINE01"}).
		Match(t.Context(), match.GitLabUser{Username: "john", Name: "John Smith"})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got.Status != match.Matched || got.SlackID != "UOFFLINE01" {
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
	forms := map[match.OverrideForm]string{
		match.OverrideEmpty: "empty", match.OverrideID: "id",
		match.OverrideHandle: "handle", match.OverrideEmail: "email",
		match.OverrideForm(9): "override_form(9)",
	}
	for f, want := range forms {
		if got := f.String(); got != want {
			t.Errorf("OverrideForm(%d).String() = %q, want %q", int(f), got, want)
		}
	}
}

// TestParseOverride pins the override grammar in the one place that owns it.
// Both readers of slack.user_map — the matcher and `doctor` — are driven by this
// function, so a change here changes both, which is exactly the property that was
// missing when each classified values on its own.
func TestParseOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  match.Override
	}{
		{"slack id", "U012ABCDEF", match.Override{Form: match.OverrideID, Key: "U012ABCDEF"}},
		{"w-prefixed id", "W012ABCDEF", match.Override{Form: match.OverrideID, Key: "W012ABCDEF"}},
		{"the shortest real id", "U012AB3CD", match.Override{Form: match.OverrideID, Key: "U012AB3CD"}},
		{"surrounding space", "  U012AB3CD  ", match.Override{Form: match.OverrideID, Key: "U012AB3CD"}},
		// The length floor is Slack's documented shape — U/W plus eight — and these
		// two are the boundary. It used to be plus *two*, loosened in an earlier
		// session to accommodate "U42"-style test fixtures and then rationalised in
		// a comment as protecting ids from old workspaces; no such id exists.
		{"one short of an id", "U012AB3C", match.Override{Form: match.OverrideHandle, Key: "u012ab3c"}},
		{"a short U-value is not an id", "U42", match.Override{Form: match.OverrideHandle, Key: "u42"}},
		// Case is the discriminator above the floor, so these are handles too. Fold
		// the case first and every one becomes an "id" that mentions nobody.
		{"u-initial handle", "ulyana", match.Override{Form: match.OverrideHandle, Key: "ulyana"}},
		{"w-initial handle", "wendy", match.Override{Form: match.OverrideHandle, Key: "wendy"}},
		{"mixed case is not an id", "Wendy", match.Override{Form: match.OverrideHandle, Key: "wendy"}},
		{"an all-caps handle is not an id", "WENDY", match.Override{Form: match.OverrideHandle, Key: "wendy"}},
		// The residual false-positive class, stated honestly: an all-caps handle
		// long enough to be an id is still read as one. doctor is what catches it.
		{"an all-caps handle at id length", "UNDERWATER", match.Override{Form: match.OverrideID, Key: "UNDERWATER"}},
		{"three lower-case letters", "uma", match.Override{Form: match.OverrideHandle, Key: "uma"}},
		{"handle with a leading @", "@john.smith", match.Override{Form: match.OverrideHandle, Key: "john.smith"}},
		{"handle is normalised", " @  John ", match.Override{Form: match.OverrideHandle, Key: "john"}},
		// An @ anywhere but the front makes it an email, leading @ or not.
		{"email", "john@acme.io", match.Override{Form: match.OverrideEmail, Key: "john@acme.io"}},
		{"email is normalised", "  John@ACME.io ", match.Override{Form: match.OverrideEmail, Key: "john@acme.io"}},
		{"@-prefixed email", "@john@acme.io", match.Override{Form: match.OverrideEmail, Key: "john@acme.io"}},
		// An id-shaped local part is still an email, not an id.
		{"id-shaped email", "U012AB3CD@acme.io", match.Override{Form: match.OverrideEmail, Key: "u012ab3cd@acme.io"}},
		{"empty", "", match.Override{Form: match.OverrideEmpty}},
		{"blank", "   ", match.Override{Form: match.OverrideEmpty}},
		// A lone @ is not an email address and must not become a lookup for "".
		{"bare @", "@", match.Override{Form: match.OverrideHandle, Key: ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := match.ParseOverride(tt.value); got != tt.want {
				t.Errorf("ParseOverride(%q) = %+v, want %+v", tt.value, got, tt.want)
			}
		})
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

// TestOverrideAcceptsHandleAndEmail covers the reason the override became usable
// by hand: a Slack user id is the one identifier an operator cannot look up
// without the API, so the map takes the forms people actually know.
func TestOverrideAcceptsHandleAndEmail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		value      string
		wantStatus match.Status
		wantID     string
		wantProbes []string
		wantNote   string
	}{
		{
			name: "handle with a leading @", value: "@bob",
			wantStatus: match.Matched, wantID: "U2", wantProbes: []string{"handle=bob"},
		},
		{
			name: "bare handle", value: "bob",
			wantStatus: match.Matched, wantID: "U2", wantProbes: []string{"handle=bob"},
		},
		{
			name: "email", value: "ann@acme.io",
			wantStatus: match.Matched, wantID: "U1", wantProbes: []string{"email=ann@acme.io"},
		},
		{
			// Never silently retried through the name ladder: an override that does
			// not resolve is a configuration mistake, and a plausible-looking
			// substitute would bury it.
			name: "unresolvable handle stops here", value: "@ghost",
			wantStatus: match.NotFound, wantProbes: []string{"handle=ghost"},
			wantNote: "matched no active Slack account",
		},
		{
			// An all-caps U…/W… value of Slack's documented length is the id form,
			// and it is answered without touching the directory — the other half of
			// the discriminator the cases below rely on.
			name: "an id is still answered offline", value: "UANN000001",
			wantStatus: match.Matched, wantID: "UANN000001", wantProbes: nil,
		},
		{
			// The regression these three pin: the classifier upper-cased the value
			// before testing it against ^[UW][A-Z0-9]{2,}$, which destroyed the one
			// thing separating an id from a handle. {jsmith: wendy} became
			// Matched/"WENDY" offline — no lookup, no Note, no log line — and the
			// digest rendered <@WENDY>, an account that does not exist, while the
			// match metric counted a success.
			name: "u-initial bare handle is a handle", value: "ulyana",
			wantStatus: match.Matched, wantID: "U7ULY", wantProbes: []string{"handle=ulyana"},
		},
		{
			name: "w-initial handle with a leading @", value: "@wendy",
			wantStatus: match.Matched, wantID: "W8WEN", wantProbes: []string{"handle=wendy"},
		},
		{
			// Mixed case is neither form: Slack lower-cases handles and ids are
			// upper-case. It goes to the handle index, which normalises — so the
			// worst outcome is a reported miss, never a fabricated mention.
			name: "mixed case is not an id", value: "Wendy",
			wantStatus: match.Matched, wantID: "W8WEN", wantProbes: []string{"handle=wendy"},
		},
		{
			// The class the id pattern's old two-character minimum swallowed whole:
			// an operator typing a colleague's handle in caps. It is a handle, and
			// upper case is not what makes something an id — length is, above the
			// floor Slack actually documents.
			name: "an all-caps handle is a handle", value: "WENDY",
			wantStatus: match.Matched, wantID: "W8WEN", wantProbes: []string{"handle=wendy"},
		},
		{
			// Too short to be any Slack id that has ever existed, so it is a handle
			// — and an unheld one, which must be reported rather than mentioned.
			name: "a value too short to be an id stops here", value: "U42",
			wantStatus: match.NotFound, wantProbes: []string{"handle=u42"},
			wantNote: "matched no active Slack account",
		},
		{
			// A u-initial handle nobody holds has to be *reported*. Read as an id
			// it would have been mentioned instead, and nothing would have said so.
			name: "unresolvable u-initial handle stops here", value: "uma",
			wantStatus: match.NotFound, wantProbes: []string{"handle=uma"},
			wantNote: "matched no active Slack account",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := newFakeDirectory(ann, bob, uly, wen)
			got, err := match.New(dir, map[string]string{"john": c.value}).
				Match(t.Context(), match.GitLabUser{Username: "john", Name: "John Smith", Email: "john@acme.io"})
			if err != nil {
				t.Fatalf("Match: %v", err)
			}
			if got.Status != c.wantStatus || got.SlackID != c.wantID {
				t.Errorf("Match = %+v, want status %v id %q", got, c.wantStatus, c.wantID)
			}
			if !slices.Equal(dir.probes, c.wantProbes) {
				t.Errorf("probes = %v, want %v", dir.probes, c.wantProbes)
			}
			if c.wantNote != "" && !strings.Contains(got.Note, c.wantNote) {
				t.Errorf("Note = %q, want it to contain %q", got.Note, c.wantNote)
			}
		})
	}
}

// TestOverrideByHandleIsNeverAGuess: two accounts answering one handle-shaped
// override must not be resolved by picking one, same rule as the ladder.
func TestOverrideAmbiguousDisplayName(t *testing.T) {
	t.Parallel()

	dir := newFakeDirectory(bob, bob2)
	got, err := match.New(dir, map[string]string{"john": "bobby"}).
		Match(t.Context(), match.GitLabUser{Username: "john"})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	// "bobby" is a display name, not a handle, so it resolves to nothing by
	// handle — the override names an account that does not exist under that key.
	if got.Status != match.NotFound {
		t.Errorf("Match = %+v, want not_found for a value that is not a handle", got)
	}
	if !strings.Contains(got.Note, "user_map") {
		t.Errorf("Note = %q, want it to name the user_map entry", got.Note)
	}
}
