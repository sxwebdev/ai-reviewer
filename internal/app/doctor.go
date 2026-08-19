package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/llm"
	"github.com/sxwebdev/ai-reviewer/internal/scheduler"
	"github.com/sxwebdev/ai-reviewer/internal/security"
)

// CheckStatus is the outcome of a single doctor check.
type CheckStatus string

const (
	StatusOK   CheckStatus = "ok"
	StatusWarn CheckStatus = "warn"
	StatusFail CheckStatus = "fail"
)

// DoctorCheck is a single environment/configuration check result.
type DoctorCheck struct {
	Name   string
	Status CheckStatus
	Detail string
}

// DoctorInput is what the doctor inspects. Config is never nil — when the load
// failed it holds the defaults, so the environment checks still run and the
// operator gets the whole picture instead of one error.
type DoctorInput struct {
	Config    *config.Config
	ConfigErr error
}

// probeTimeout bounds each external probe. Doctor is interactive; a hung binary
// must not hold the whole report.
const probeTimeout = 15 * time.Second

// defaultsPrefix marks a check whose verdict came from the built-in defaults
// because the configuration could not be loaded. It is a literal (and part of
// the "config source" line's own wording) so the marker an operator sees and
// the marker the report explains cannot drift apart.
const defaultsPrefix = "[defaults] "

// Doctor runs the environment and configuration checks that are possible
// without the service layer. It never fails the process; the CLI derives the
// exit code from the results.
//
// The probes that need infrastructure — Postgres, migrations, the River schema,
// GitLab, Slack and the per-repository checks — live in doctor_service.go and
// are appended by the CLI unless --local is given.
func Doctor(ctx context.Context, in DoctorInput) []DoctorCheck {
	cfg := in.Config
	var defaultsErr error
	if cfg == nil {
		// Default returns what it built even on error, which is what lets the rest
		// of the checklist run: a diagnostic that gives up has nothing to report.
		cfg, defaultsErr = config.Default()
	}

	var checks []DoctorCheck
	add := func(name string, status CheckStatus, detail string) {
		checks = append(checks, DoctorCheck{Name: name, Status: status, Detail: detail})
	}
	if defaultsErr != nil {
		add("config schema", StatusFail, defaultsErr.Error())
	}

	// usingDefaults: the config did not load, so cfg holds config.Default() and
	// every verdict derived from it describes the defaults, not the operator's
	// file. Running those checks anyway is right — a broken config must not hide
	// a missing `git` or an expired login — but presenting "claude auth:
	// existing-login" as a fact when the config asks for oauth-token sends the
	// reader after the wrong problem. So the checks that read configuration are
	// marked, and the reason is stated once.
	usingDefaults := in.ConfigErr != nil
	fromConfig := func(name string, status CheckStatus, detail string) {
		if usingDefaults {
			detail = defaultsPrefix + detail
		}
		add(name, status, detail)
	}

	// config — reported first: every other check below reads it, so a failure
	// here explains why the rest ran against defaults.
	if in.ConfigErr != nil {
		add("config", StatusFail, security.Mask(in.ConfigErr.Error()))
		add("config source", StatusWarn,
			"the config could not be loaded, so the "+strings.TrimSpace(defaultsPrefix)+" checks below describe "+
				"the built-in defaults rather than your configuration. Fix the error above and re-run.")
	} else {
		add("config", StatusOK, fmt.Sprintf("valid; %d team(s)", len(cfg.Teams)))
	}

	// gitlab tls — a config fact, so it is reported here rather than in the
	// service probes: it must show up under `doctor --local` too.
	//
	// The flag disables certificate verification on every GitLab call, and those
	// calls all carry the PAT in a header. Nothing else in the process mentions
	// it: switched on once during a self-managed rollout, it stays on invisibly.
	switch {
	case cfg.GitLab.InsecureSkipVerify:
		fromConfig("gitlab tls", StatusWarn,
			"gitlab.insecure_skip_verify is on: server certificates are NOT verified on any GitLab call, "+
				"including the ones that send the token. Prefer gitlab.ca_cert_path for a private CA.")
	case cfg.GitLab.CACertPath != "":
		fromConfig("gitlab tls", StatusOK, "verified against "+cfg.GitLab.CACertPath)
	default:
		fromConfig("gitlab tls", StatusOK, "verified against the system roots")
	}

	// git — a property of the machine, not of the config, so it is reported
	// unmarked even when the config failed to load.
	if p, err := exec.LookPath("git"); err == nil {
		add("git", StatusOK, p)
	} else {
		add("git", StatusFail, "git not found in PATH")
	}

	// claude binary
	bin, err := exec.LookPath(cfg.LLM.Claude.Bin)
	if err != nil {
		fromConfig("claude cli", StatusFail, fmt.Sprintf("%q not found in PATH", cfg.LLM.Claude.Bin))
	} else {
		detail := bin
		if v, err := claudeVersion(ctx, bin); err == nil {
			detail = bin + " (" + v + ")"
		}
		fromConfig("claude cli", StatusOK, detail)
	}

	// claude auth — probed through the same ClaudeAuth the reviewer subprocess
	// uses, so the verdict reflects the environment reviews will actually run in
	// rather than the operator's shell.
	if err == nil {
		status, detail := claudeAuthCheck(ctx, bin, cfg)
		fromConfig("claude auth", status, detail)
	}

	// digest schedule — every team's, resolved against the global default. Both
	// halves are checked the way the scheduler will read them, so a stripped image
	// (no tzdata) or a typo'd slot fails here and not at the first firing. Printed
	// in full because "when does my digest arrive" has no other answer once the
	// schedule is per team.
	// fromConfig, not add: this check reads cfg.Digest and cfg.Teams, so with the
	// configuration unloadable it prints the *built-in* schedule. Unmarked, that
	// presents a default as the operator's own times — which is exactly what
	// defaultsPrefix exists to prevent. Its predecessor checked a Go constant and
	// was correctly unmarked; moving the schedule into configuration moved this
	// check across that line too.
	addDigestSchedule(cfg, fromConfig)

	return checks
}

// addDigestSchedule reports each team's effective digest schedule, or the first
// thing wrong with it.
//
// One line per team rather than one aggregate: the schedule is the answer to a
// question an operator asks per team ("when does ours arrive"), and an inherited
// value is indistinguishable from an overridden one in the config file alone.
func addDigestSchedule(cfg *config.Config, add func(string, CheckStatus, string)) {
	if len(cfg.Teams) == 0 {
		// The global block still has to be loadable: it is what the first team
		// added will inherit.
		if err := checkDigestSchedule(cfg.Digest.Slots, cfg.Digest.Timezone); err != nil {
			add("digest schedule", StatusFail, err.Error())
			return
		}
		add("digest schedule", StatusOK,
			fmt.Sprintf("%s %s (no teams configured)", cfg.Digest.Timezone, strings.Join(cfg.Digest.Slots, ", ")))
		return
	}
	for _, t := range cfg.Teams {
		slots, tz := cfg.DigestScheduleFor(t)
		if err := checkDigestSchedule(slots, tz); err != nil {
			add("digest schedule", StatusFail, fmt.Sprintf("%s: %s", t.Name, err))
			continue
		}
		add("digest schedule", StatusOK, fmt.Sprintf("%s → %s %s", t.Name, tz, strings.Join(slots, ", ")))
	}
}

func checkDigestSchedule(slots []string, timezone string) error {
	if _, err := scheduler.ParseClocks(slots); err != nil {
		return err
	}
	_, err := scheduler.LoadLocation(timezone)
	return err
}

// claudeAuthStatus is the subset of `claude auth status --json` the doctor
// reports. email and orgId are deliberately absent: they are neither actionable
// nor safe to print into a terminal that may end up in a ticket.
type claudeAuthStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	SubscriptionType string `json:"subscriptionType"`
}

// claudeAuthCheck runs `claude auth status --json` under the configured auth
// mode's environment.
func claudeAuthCheck(ctx context.Context, bin string, cfg *config.Config) (CheckStatus, string) {
	auth, err := llm.NewClaudeAuth(cfg.ClaudeAuthConfig())
	if err != nil {
		return StatusFail, security.Mask(err.Error())
	}
	env, err := auth.Env(os.Environ())
	if err != nil {
		return StatusFail, security.Mask(err.Error())
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "auth", "status", "--json")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		// A non-zero exit here is the normal "not logged in" signal for
		// existing-login; report the mode alongside it so the fix is obvious.
		return StatusFail, fmt.Sprintf("%s — %s", auth.Describe(), security.Mask(strings.TrimSpace(firstLine(string(out))+" "+err.Error())))
	}

	var st claudeAuthStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return StatusWarn, fmt.Sprintf("%s — could not parse `claude auth status --json`", auth.Describe())
	}
	if !st.LoggedIn {
		return StatusFail, fmt.Sprintf("%s — claude reports loggedIn=false", auth.Describe())
	}

	detail := auth.Describe() + " — loggedIn=true"
	if st.AuthMethod != "" {
		detail += ", authMethod=" + st.AuthMethod
	}
	if st.APIProvider != "" {
		detail += ", apiProvider=" + st.APIProvider
	}
	if st.SubscriptionType != "" {
		detail += ", subscriptionType=" + st.SubscriptionType
	}
	return StatusOK, detail
}

func claudeVersion(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", err
	}
	return firstLine(string(out)), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
