package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/config"
)

// defaultConfig is config.Default() for tests: the only error it can return is a
// malformed `default:` tag in the schema, which stops the test rather than
// becoming a branch.
func defaultConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	return c
}

// TestDoctorReportsInsecureTLS: gitlab.insecure_skip_verify disables
// certificate verification on every GitLab call, and those calls all carry the
// PAT in a header. Nothing else in the process mentions it — no startup
// warning, no other check — so a flag flipped once during a self-managed
// rollout stays on invisibly and the token travels over a MITM-able channel.
//
// It lives in the config-level checks rather than the service probes so
// `doctor --local`, which is what an operator runs when GitLab is unreachable,
// still reports it.
func TestDoctorReportsInsecureTLS(t *testing.T) {
	t.Parallel()

	find := func(t *testing.T, checks []DoctorCheck) DoctorCheck {
		t.Helper()
		for _, c := range checks {
			if c.Name == "gitlab tls" {
				return c
			}
		}
		t.Fatalf("no gitlab tls check: %+v", checks)
		return DoctorCheck{}
	}

	t.Run("on", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t)
		cfg.GitLab.InsecureSkipVerify = true

		got := find(t, Doctor(t.Context(), DoctorInput{Config: cfg}))
		if got.Status != StatusWarn {
			t.Errorf("status = %v (%s), want a warning", got.Status, got.Detail)
		}
		// The line has to say what is unsafe, or it is just another OK-looking row.
		for _, want := range []string{"insecure_skip_verify", "NOT verified", "token"} {
			if !strings.Contains(got.Detail, want) {
				t.Errorf("detail does not mention %q: %s", want, got.Detail)
			}
		}
	})

	t.Run("off", func(t *testing.T) {
		t.Parallel()
		got := find(t, Doctor(t.Context(), DoctorInput{Config: testConfig(t)}))
		if got.Status != StatusOK {
			t.Errorf("status = %v (%s), want ok", got.Status, got.Detail)
		}
	})

	t.Run("a private CA is reported as what it is", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t)
		cfg.GitLab.CACertPath = "/etc/ssl/corp.pem"

		got := find(t, Doctor(t.Context(), DoctorInput{Config: cfg}))
		if got.Status != StatusOK || !strings.Contains(got.Detail, "/etc/ssl/corp.pem") {
			t.Errorf("check = %+v, want an ok line naming the CA", got)
		}
	})
}

// TestDoctorMarksChecksThatRanAgainstDefaults: when the config does not load,
// the CLI hands Doctor defaultConfig(t) so the environment checks still run —
// that fallback is right, a broken config must not hide a missing `git`. What
// is not right is presenting the resulting verdicts as facts about the
// operator's file: "claude auth: existing-login" reads as a finding even when
// the config that could not be loaded asks for oauth-token.
func TestDoctorMarksChecksThatRanAgainstDefaults(t *testing.T) {
	t.Parallel()

	// Exactly what internal/cli does on a load failure.
	checks := Doctor(t.Context(), DoctorInput{
		Config:    defaultConfig(t),
		ConfigErr: errors.New("gitlab.base_url is empty"),
	})

	byName := map[string]DoctorCheck{}
	for _, c := range checks {
		byName[c.Name] = c
	}

	if got := byName["config"]; got.Status != StatusFail {
		t.Errorf("config = %v, want fail", got.Status)
	}

	// Said once, in words, next to the failure that caused it.
	note, ok := byName["config source"]
	if !ok {
		t.Fatalf("nothing says the report fell back to defaults: %+v", checks)
	}
	if note.Status != StatusWarn || !strings.Contains(note.Detail, "defaults") {
		t.Errorf("config source = %+v, want a warning that names the fallback", note)
	}

	// And marked on every line that read configuration, so a reader skimming
	// the list cannot mistake one for a fact about their setup.
	// "digest schedule" is on this list because the schedule became configuration:
	// with the config unloadable the line prints the built-in times, and unmarked
	// that reads as the operator's own schedule.
	for _, name := range []string{"gitlab tls", "claude cli", "claude auth", "digest schedule"} {
		c, ok := byName[name]
		if !ok {
			continue // claude auth is absent when the binary is not installed
		}
		if !strings.HasPrefix(c.Detail, defaultsPrefix) {
			t.Errorf("%q is derived from config but is not marked: %s", name, c.Detail)
		}
	}
	// Machine facts must NOT be marked: git is on PATH or it is not, whatever
	// the config says.
	if c := byName["git"]; strings.HasPrefix(c.Detail, defaultsPrefix) {
		t.Errorf("git is not a config-derived check: %s", c.Detail)
	}
	// Every name asserted above must actually exist, or the assertion is vacuous:
	// a map miss yields the zero DoctorCheck and HasPrefix("", …) is false whatever
	// the code does. This list held "timezone" — a check this tree no longer emits —
	// so it could not fail, and it was hiding that its successor prints a built-in
	// schedule unmarked.
	for _, name := range []string{"config", "config source", "git", "digest schedule"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("no %q check in the report, so the assertions about it are vacuous: %+v", name, checks)
		}
	}
}

// TestDoctorDoesNotMarkAValidConfig: the marker must appear only when the
// verdicts really did come from the defaults, or it becomes noise everyone
// learns to ignore.
func TestDoctorDoesNotMarkAValidConfig(t *testing.T) {
	t.Parallel()

	checks := Doctor(t.Context(), DoctorInput{Config: testConfig(t)})
	for _, c := range checks {
		if c.Name == "config source" {
			t.Errorf("a valid config produced a fallback warning: %+v", c)
		}
		if strings.HasPrefix(c.Detail, defaultsPrefix) {
			t.Errorf("%q was marked as default-derived on a valid config: %s", c.Name, c.Detail)
		}
	}
}

// TestDoctorPrintsTheSkippedDays: the schedule line answers "when does ours
// arrive". Printed without its exceptions it still answers that and quietly
// makes the other question — "why did nothing arrive today" — unanswerable.
func TestDoctorPrintsTheSkippedDays(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	cfg.Digest.SkipWeekdays = []string{"sat", "sun"}
	cfg.Digest.SkipDates = []string{"01-01"}
	// The second team keeps its own rhythm, which is what makes the per-team
	// resolution visible in the report rather than inferred from one global line.
	cfg.Teams[1].Digest = config.TeamDigestConfig{SkipWeekdays: []string{"sun"}}

	lines := digestScheduleLines(t, cfg)
	if len(lines) != 2 {
		t.Fatalf("digest schedule lines = %d, want one per team: %+v", len(lines), lines)
	}
	if !strings.Contains(lines[0].Detail, "Saturday, Sunday") || !strings.Contains(lines[0].Detail, "01-01") {
		t.Errorf("the inherited skip list is missing: %s", lines[0].Detail)
	}
	if strings.Contains(lines[1].Detail, "Saturday") {
		t.Errorf("team 2 overrides the weekdays and must not show Saturday: %s", lines[1].Detail)
	}
	if !strings.Contains(lines[1].Detail, "01-01") {
		t.Errorf("team 2 inherits the dates and must still show them: %s", lines[1].Detail)
	}
	for _, l := range lines {
		if l.Status != StatusOK {
			t.Errorf("an ordinary skip list must not degrade the check: %+v", l)
		}
	}
}

// TestDoctorWarnsWhenEveryWeekdayIsSkipped: it is a legitimate way to say "this
// team wants no scheduled digest" — there is no other switch — but it is the one
// skip list whose consequence is total, so it is stated rather than left to be
// worked out from seven names.
func TestDoctorWarnsWhenEveryWeekdayIsSkipped(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	cfg.Teams = cfg.Teams[:1]
	cfg.Teams[0].Digest = config.TeamDigestConfig{
		SkipWeekdays: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
	}

	lines := digestScheduleLines(t, cfg)
	if len(lines) != 1 {
		t.Fatalf("digest schedule lines = %d, want 1: %+v", len(lines), lines)
	}
	if lines[0].Status != StatusWarn {
		t.Errorf("status = %v, want a warning: %+v", lines[0].Status, lines[0])
	}
	if !strings.Contains(lines[0].Detail, "no digest is ever sent") {
		t.Errorf("the consequence is not stated: %s", lines[0].Detail)
	}
}

// TestDoctorSaysNothingAboutSkipsWhenThereAreNone: a clause on every line is a
// clause nobody reads.
func TestDoctorSaysNothingAboutSkipsWhenThereAreNone(t *testing.T) {
	t.Parallel()

	for _, l := range digestScheduleLines(t, testConfig(t)) {
		if strings.Contains(l.Detail, "except") {
			t.Errorf("an unconfigured schedule mentions exceptions: %s", l.Detail)
		}
	}
}

// digestScheduleLines runs the config-only checks and returns the schedule ones.
func digestScheduleLines(t *testing.T, cfg *config.Config) []DoctorCheck {
	t.Helper()
	var out []DoctorCheck
	for _, c := range Doctor(t.Context(), DoctorInput{Config: cfg}) {
		if c.Name == "digest schedule" {
			out = append(out, c)
		}
	}
	return out
}
