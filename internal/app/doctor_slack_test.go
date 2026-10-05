package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/match"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// stubLister stands in for the Slack client. checkSlackDirectory takes
// slack.UserLister precisely so this is possible: checkSlack itself builds a
// client against the real API and cannot be pointed elsewhere.
type stubLister struct {
	users []slack.User
	err   error
}

func (s stubLister) ListUsers(context.Context) ([]slack.User, error) { return s.users, s.err }

func slackUser(id, handle, email string) slack.User {
	u := slack.User{ID: id, Name: handle, RealName: strings.ToUpper(handle)}
	u.Profile.Email = email
	u.Profile.DisplayName = handle
	return u
}

func findCheck(t *testing.T, checks []DoctorCheck, name string) DoctorCheck {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q was not reported; got %+v", name, checks)
	return DoctorCheck{}
}

// TestDoctorSlackDirectoryScopes is the gap this check was added for: the digest
// went out naming everybody without a mention because the token lacked
// users:read, and doctor was green — it never called users.list at all.
func TestDoctorSlackDirectoryScopes(t *testing.T) {
	t.Parallel()

	t.Run("missing scope names the scope", func(t *testing.T) {
		t.Parallel()
		col := &checkCollector{}
		lister := stubLister{err: &slack.APIError{
			Method: "users.list", Code: "missing_scope",
			Needed: "users:read", Provided: "chat:write,channels:read",
		}}
		checkSlackDirectory(t.Context(), col, lister, nil)

		got := findCheck(t, col.checks, "slack directory")
		if got.Status != StatusFail {
			t.Errorf("status = %q, want fail", got.Status)
		}
		if !strings.Contains(got.Detail, "users:read") {
			t.Errorf("detail = %q, want the needed scope named", got.Detail)
		}
	})

	// Slack answers 200 with the email field simply absent when users:read.email
	// is missing, so an empty-email directory is the only signal available. Email
	// is the matcher's one exact identifier.
	t.Run("no emails warns about users:read.email", func(t *testing.T) {
		t.Parallel()
		col := &checkCollector{}
		checkSlackDirectory(t.Context(), col, stubLister{users: []slack.User{
			slackUser("U1", "ann", ""),
			slackUser("U2", "bob", ""),
		}}, nil)

		got := findCheck(t, col.checks, "slack directory")
		if got.Status != StatusWarn {
			t.Errorf("status = %q, want warn", got.Status)
		}
		if !strings.Contains(got.Detail, "users:read.email") {
			t.Errorf("detail = %q, want the email scope named", got.Detail)
		}
	})

	t.Run("healthy directory reports counts", func(t *testing.T) {
		t.Parallel()
		col := &checkCollector{}
		checkSlackDirectory(t.Context(), col, stubLister{users: []slack.User{
			slackUser("U1", "ann", "ann@acme.io"),
			slackUser("U2", "bob", ""),
		}}, nil)

		got := findCheck(t, col.checks, "slack directory")
		if got.Status != StatusOK {
			t.Errorf("status = %q (%s), want ok", got.Status, got.Detail)
		}
		if !strings.Contains(got.Detail, "2 active members, 1 with an email") {
			t.Errorf("detail = %q", got.Detail)
		}
	})

	// A deactivated account is filtered out of the directory, which is why
	// resolving an entry against it proves the account is live.
	t.Run("only deactivated members is a failure", func(t *testing.T) {
		t.Parallel()
		dead := slackUser("U9", "ghost", "ghost@acme.io")
		dead.Deleted = true
		col := &checkCollector{}
		checkSlackDirectory(t.Context(), col, stubLister{users: []slack.User{dead}}, nil)

		if got := findCheck(t, col.checks, "slack directory"); got.Status != StatusFail {
			t.Errorf("status = %q (%s), want fail", got.Status, got.Detail)
		}
	})
}

// TestDoctorUserMapResolution: a broken user_map entry is invisible at runtime —
// the person is named without a ping, exactly as if no entry existed, which is
// the thing the entry was added to fix. This check is the only place it surfaces.
func TestDoctorUserMapResolution(t *testing.T) {
	t.Parallel()

	users := []slack.User{
		slackUser("UANN000001", "ann", "ann@acme.io"),
		slackUser("UBOB000001", "bob", "bob@acme.io"),
		slackUser("UBOB000002", "bob", "bob2@acme.io"), // same handle as UBOB000001
		// wendy's handle starts with the letter a Slack id starts with. Case is the
		// only thing that separates the two forms.
		slackUser("UWEN000001", "wendy", "wendy@acme.io"),
	}

	cases := []struct {
		name       string
		userMap    map[string]string
		wantStatus CheckStatus
		wantDetail string
	}{
		{
			name:       "id, handle and email all resolve",
			userMap:    map[string]string{"a": "UANN000001", "b": "@ann", "c": "ann@acme.io"},
			wantStatus: StatusOK,
			wantDetail: "UANN000001",
		},
		{
			// The regression: doctor shares the matcher's id predicate, and that
			// predicate upper-cased the value first — so a bare u/w-initial handle
			// was looked up by *id* and reported as "no active Slack account has
			// this user id" for a handle somebody actually holds.
			name:       "bare w-initial handle resolves as a handle",
			userMap:    map[string]string{"a": "wendy"},
			wantStatus: StatusOK,
			wantDetail: "UWEN000001",
		},
		{
			name:       "w-initial handle with an @ resolves as a handle",
			userMap:    map[string]string{"a": "@wendy"},
			wantStatus: StatusOK,
			wantDetail: "UWEN000001",
		},
		{
			// Mixed case is neither an id nor a Slack-spelled handle; the handle
			// index normalises, so it still resolves.
			name:       "mixed-case handle resolves as a handle",
			userMap:    map[string]string{"a": "Wendy"},
			wantStatus: StatusOK,
			wantDetail: "UWEN000001",
		},
		{
			// An operator typing a colleague's handle in caps. The id pattern's old
			// two-character minimum read every one of these as an id, so doctor
			// reported "no active Slack account has this user id" for a live handle.
			name:       "all-caps handle resolves as a handle",
			userMap:    map[string]string{"a": "WENDY"},
			wantStatus: StatusOK,
			wantDetail: "UWEN000001",
		},
		{
			// Too short to be any Slack id that has ever existed.
			name:       "a value too short to be an id is a handle",
			userMap:    map[string]string{"a": "U42"},
			wantStatus: StatusFail,
			wantDetail: "no active Slack account has this handle",
		},
		{
			// And a u-initial handle nobody holds must be reported as a *handle*
			// miss, so the operator looks for the right thing.
			name:       "unknown u-initial handle is reported as a handle",
			userMap:    map[string]string{"a": "uma"},
			wantStatus: StatusFail,
			wantDetail: "no active Slack account has this handle",
		},
		{
			// A blank value is what the *runtime* discards: match.New drops the
			// entry and the person is matched by the ordinary ladder, so the
			// deployment is not broken and doctor must not fail the run over it.
			// Still worth a warning — somebody meant to type an id.
			name:       "a blank value is ignored, not a failure",
			userMap:    map[string]string{"a": "   "},
			wantStatus: StatusWarn,
			wantDetail: "ignored",
		},
		{
			// A blank value alongside a good one keeps the good one reported.
			name:       "a blank value next to a resolved one still warns",
			userMap:    map[string]string{"a": "", "b": "@ann"},
			wantStatus: StatusWarn,
			wantDetail: "UANN000001",
		},
		{
			name:       "unknown id",
			userMap:    map[string]string{"a": "UNOBODY001"},
			wantStatus: StatusFail,
			wantDetail: "user id",
		},
		{
			name:       "unknown handle",
			userMap:    map[string]string{"a": "@ghost"},
			wantStatus: StatusFail,
			wantDetail: "handle",
		},
		{
			name:       "unknown email points at the scope",
			userMap:    map[string]string{"a": "nobody@acme.io"},
			wantStatus: StatusFail,
			wantDetail: "users:read.email",
		},
		{
			// Never resolved by picking one, same rule as the matcher.
			name:       "ambiguous handle asks for the id",
			userMap:    map[string]string{"a": "bob"},
			wantStatus: StatusFail,
			wantDetail: "use the Slack user id",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			col := &checkCollector{}
			checkSlackDirectory(t.Context(), col, stubLister{users: users}, c.userMap)

			got := findCheck(t, col.checks, "slack user_map")
			if got.Status != c.wantStatus {
				t.Errorf("status = %q, want %q (detail: %s)", got.Status, c.wantStatus, got.Detail)
			}
			if !strings.Contains(got.Detail, c.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, c.wantDetail)
			}
		})
	}

	t.Run("no map, no check", func(t *testing.T) {
		t.Parallel()
		col := &checkCollector{}
		checkSlackDirectory(t.Context(), col, stubLister{users: users}, nil)
		for _, c := range col.checks {
			if c.Name == "slack user_map" {
				t.Errorf("an empty user_map must not report a check: %+v", c)
			}
		}
	})
}

// TestDoctorAndMatcherAgreeOnUserMapGrammar drives both readers of
// slack.user_map over one directory and one list of values.
//
// It exists because doctor used to carry its own copy of the override grammar —
// the @-stripping, the "an @ elsewhere means email" rule, the ambiguity policy —
// sharing only the id predicate with the matcher. Two copies of a grammar drift,
// and the direction of drift is the whole problem: doctor's job is to answer "will
// these entries work at 09:00?", so a doctor that classifies a value differently
// from the matcher can bless a map that mentions nobody, or fail a map that is
// fine. Both sides now drive match.ParseOverride, and this is what holds them
// there.
func TestDoctorAndMatcherAgreeOnUserMapGrammar(t *testing.T) {
	t.Parallel()

	users := []slack.User{
		slackUser("UANN000001", "ann", "ann@acme.io"),
		slackUser("UWEN000001", "wendy", "wendy@acme.io"),
		slackUser("UBOB000001", "bob", "bob@acme.io"),
		slackUser("UBOB000002", "bob", "bob2@acme.io"), // same handle as UBOB000001
		// The GitLab user every case below is resolved for. It matters only for the
		// blank-value rows, where the override is dropped and the ordinary ladder
		// runs — which is exactly why a blank value is not a broken deployment.
		slackUser("UJSM000001", "jsmith", "jsmith@acme.io"),
	}

	// The grammar is written out here rather than asked of the code: the point is
	// that both sides agree with *this table*, not merely with each other.
	cases := []struct {
		name  string
		value string
		// form is the classification both sides must reach, asserted against
		// ParseOverride itself so the table cannot quietly describe something else.
		form match.OverrideForm
		// wantDoctor is doctor's verdict, which is not always the matcher's — see
		// the two asymmetries below, both deliberate and both in the safe direction.
		wantDoctor CheckStatus
		// wantID is the account the matcher must end up mentioning, or "" when the
		// value resolves to nothing or to more than one account.
		wantID string
	}{
		{
			name: "id", value: "UANN000001", form: match.OverrideID,
			wantDoctor: StatusOK, wantID: "UANN000001",
		},
		{
			name: "unknown id", value: "UNOBODY001", form: match.OverrideID,
			wantDoctor: StatusFail,
		},
		{
			name: "handle with @", value: "@ann", form: match.OverrideHandle,
			wantDoctor: StatusOK, wantID: "UANN000001",
		},
		{
			name: "bare handle", value: "ann", form: match.OverrideHandle,
			wantDoctor: StatusOK, wantID: "UANN000001",
		},
		// u/w-initial handles: the values that separate "id" from "starts with the
		// letter an id starts with".
		{
			name: "u-w-initial handle", value: "wendy", form: match.OverrideHandle,
			wantDoctor: StatusOK, wantID: "UWEN000001",
		},
		{
			name: "u-w-initial handle with @", value: "@wendy", form: match.OverrideHandle,
			wantDoctor: StatusOK, wantID: "UWEN000001",
		},
		{
			name: "mixed-case handle", value: "Wendy", form: match.OverrideHandle,
			wantDoctor: StatusOK, wantID: "UWEN000001",
		},
		{
			// The class the old two-character id minimum swallowed whole.
			name: "all-caps handle", value: "WENDY", form: match.OverrideHandle,
			wantDoctor: StatusOK, wantID: "UWEN000001",
		},
		{
			// Shorter than any Slack id that has ever existed, so: a handle.
			name: "too short to be an id", value: "U42", form: match.OverrideHandle,
			wantDoctor: StatusFail,
		},
		{
			name: "unknown handle", value: "uma", form: match.OverrideHandle,
			wantDoctor: StatusFail,
		},
		{
			// Two accounts share it: never resolved by picking one.
			name: "ambiguous handle", value: "bob", form: match.OverrideHandle,
			wantDoctor: StatusFail,
		},
		{
			name: "email", value: "ann@acme.io", form: match.OverrideEmail,
			wantDoctor: StatusOK, wantID: "UANN000001",
		},
		{
			name: "unknown email", value: "nobody@acme.io", form: match.OverrideEmail,
			wantDoctor: StatusFail,
		},
		{
			// The second asymmetry: the runtime *discards* a blank override and
			// matches the person normally, so the deployment works and doctor may
			// only warn. It used to fail, which failed the whole `doctor` run — and
			// any deploy gate reading its exit code — over a no-op.
			name: "blank value", value: "", form: match.OverrideEmpty,
			wantDoctor: StatusWarn, wantID: "UJSM000001",
		},
		{
			name: "whitespace value", value: "   ", form: match.OverrideEmpty,
			wantDoctor: StatusWarn, wantID: "UJSM000001",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := match.ParseOverride(c.value).Form; got != c.form {
				t.Fatalf("ParseOverride(%q).Form = %s, want %s — this table describes a "+
					"grammar the code does not implement", c.value, got, c.form)
			}

			userMap := map[string]string{"jsmith": c.value}

			col := &checkCollector{}
			checkSlackDirectory(t.Context(), col, stubLister{users: users}, userMap)
			doctor := findCheck(t, col.checks, "slack user_map")

			// The matcher reads the same users through the adapter the digest uses,
			// so any disagreement below is a disagreement about the grammar and
			// nothing else.
			m := match.New(
				slack.MatchDirectory(slack.NewDirectory(stubLister{users: users}, slack.DirectoryConfig{})),
				userMap,
			)
			got, err := m.Match(t.Context(), match.GitLabUser{Username: "jsmith", Name: "John Smith"})
			if err != nil {
				t.Fatalf("Match: %v", err)
			}

			if doctor.Status != c.wantDoctor {
				t.Errorf("doctor status = %q, want %q (detail: %s)", doctor.Status, c.wantDoctor, doctor.Detail)
			}
			// Whatever doctor says, it may never exit non-zero for a configuration the
			// runtime handles: a Fail has to mean the matcher will not mention anybody.
			if doctor.Status == StatusFail && got.Status == match.Matched && c.form != match.OverrideID {
				t.Errorf("doctor failed (%s) but the matcher mentions %q: doctor's exit code may only "+
					"diverge from the runtime when the deployment is genuinely broken", doctor.Detail, got.SlackID)
			}
			if c.wantID != "" && doctor.Status == StatusOK && !strings.Contains(doctor.Detail, c.wantID) {
				t.Errorf("doctor detail = %q, want it to name %s", doctor.Detail, c.wantID)
			}

			if c.form == match.OverrideID {
				// The first asymmetry: an id is taken at face value so the override
				// still works when users.list does not, which makes doctor the only
				// side that ever verifies one. Everything else about the
				// classification still has to match.
				if got.Status != match.Matched || got.SlackID != c.value {
					t.Errorf("matcher = %+v, want the id answered offline as %q", got, c.value)
				}
				return
			}

			switch {
			case c.wantID != "":
				if got.Status != match.Matched || got.SlackID != c.wantID {
					t.Errorf("matcher = %+v, want matched %s — doctor said %q (%s)",
						got, c.wantID, doctor.Status, doctor.Detail)
				}
			default:
				if got.Status == match.Matched {
					t.Errorf("matcher matched %q where doctor failed (%s): a value doctor cannot verify "+
						"must not become a mention", got.SlackID, doctor.Detail)
				}
			}
		})
	}
}

// TestCheckWorkdirFollowsAgentMode covers the mode dimension TestCheckWorkdir
// (runtime_test.go, which runs on the defaults and therefore always with agent
// mode on) does not.
//
// `start` gates the workdir through checkAgentWorkdir, which skips the check
// entirely when llm.claude.agent_mode is off, because with it off no mirror is
// cloned and no worktree is created. doctor failed regardless, so a deliberately
// diff-only deployment on a read-only mount ran correctly while `doctor` exited
// non-zero — blocking any deploy gate built on that exit code. A diagnostic that
// contradicts the process it diagnoses is worse than no diagnostic.
//
// It lives in this file because it is the only internal/app test file this change
// owns; TestCheckWorkdir stays where it is.
func TestCheckWorkdirFollowsAgentMode(t *testing.T) {
	t.Parallel()

	// A directory that exists and rejects writes — the read-only mount, without
	// needing one.
	readonlyDir := func(t *testing.T) string {
		t.Helper()
		if os.Geteuid() == 0 {
			t.Skip("root ignores the permission bits this case relies on")
		}
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	check := func(t *testing.T, agentMode bool, dir string) DoctorCheck {
		t.Helper()
		cfg := defaultConfig(t)
		cfg.LLM.Claude.AgentMode = agentMode
		cfg.Review.WorkDir = dir
		col := &checkCollector{}
		(&App{Config: cfg, Log: quietLogger()}).checkWorkdir(col)
		if len(col.checks) != 1 {
			t.Fatalf("checks = %+v, want exactly one", col.checks)
		}
		return col.checks[0]
	}

	t.Run("agent mode off tolerates a read-only workdir", func(t *testing.T) {
		t.Parallel()
		got := check(t, false, readonlyDir(t))
		if got.Status != StatusWarn {
			t.Errorf("status = %q (%s), want warn: start runs fine in this configuration", got.Status, got.Detail)
		}
		// The message has to say *why* it is only a warning, or the operator reads
		// a warning about an unwritable directory and goes fixing the mount.
		for _, want := range []string{"llm.claude.agent_mode is off", "diff"} {
			if !strings.Contains(got.Detail, want) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, want)
			}
		}
	})

	t.Run("agent mode off tolerates an unset workdir", func(t *testing.T) {
		t.Parallel()
		got := check(t, false, "")
		if got.Status != StatusWarn {
			t.Errorf("status = %q (%s), want warn", got.Status, got.Detail)
		}
		if !strings.Contains(got.Detail, "llm.claude.agent_mode is off") {
			t.Errorf("detail = %q, want it to name the mode that makes this survivable", got.Detail)
		}
	})

	// The other half: agreeing with start must not become "never fail". With agent
	// mode on an unusable workdir means every review silently degrades to a
	// diff-only reading at full price, which is what start refuses to do.
	t.Run("agent mode on still fails", func(t *testing.T) {
		t.Parallel()
		if got := check(t, true, readonlyDir(t)); got.Status != StatusFail {
			t.Errorf("status = %q (%s), want fail", got.Status, got.Detail)
		}
	})

	t.Run("agent mode off with a writable workdir still says reviews are diff-only", func(t *testing.T) {
		t.Parallel()
		got := check(t, false, filepath.Join(t.TempDir(), "work"))
		if got.Status != StatusWarn {
			t.Errorf("status = %q (%s), want warn", got.Status, got.Detail)
		}
		if !strings.Contains(got.Detail, "llm.claude.agent_mode is off") {
			t.Errorf("detail = %q", got.Detail)
		}
	})
}
