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
	for _, name := range []string{"gitlab tls", "claude cli", "claude auth"} {
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
	if c := byName["timezone"]; strings.HasPrefix(c.Detail, defaultsPrefix) {
		t.Errorf("timezone is not a config-derived check: %s", c.Detail)
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
