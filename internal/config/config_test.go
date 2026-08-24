package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/tkcrm/mx/logger"
)

// quietLog is the bootstrap logger Load wants; nothing below fatal is emitted,
// so test output stays clean.
func quietLog() logger.Logger {
	return logger.New(logger.WithConfig(logger.Config{
		Level:  logger.LogLevelFatal,
		Format: logger.LoggerFormatJSON,
	}))
}

// defaultConfig is Default() for tests. The error can only be a malformed
// `default:` tag, which is a mistake in the schema rather than a condition a test
// should branch on — so it stops the test instead.
func defaultConfig(t *testing.T) *Config {
	t.Helper()
	c, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	return c
}

// writeConfig writes yml to a temp file and returns its path.
func writeConfig(t *testing.T, yml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// minimalYAML is a config that passes validation, so a test can add exactly the
// one thing it is about.
const minimalYAML = `
gitlab:
  base_url: https://gitlab.example.com
  token: glpat-test
slack:
  token: xoxb-test
postgres:
  username: ai_reviewer
teams:
  - name: payments
    slack_channel: C012345678
    ai_review: { enabled: true }
    repositories: [backend/payments]
`

func loadFile(t *testing.T, path string) (*Config, error) {
	t.Helper()
	cfg := defaultConfig(t)
	res, err := Load(t.Context(), quietLog(), cfg, []string{path})
	if res != nil {
		t.Cleanup(res.Cleanup)
	}
	return cfg, err
}

// TestDefaultTagsAreWellFormed is what replaced a panic in Default().
//
// A `default:"5x"` on a duration, or a value xconfig cannot parse into the
// field's type, is a mistake in the schema above and nowhere else — it cannot
// depend on the environment. Default() reports it as an error so no code path has
// to panic, and this is the test that makes sure nobody has to see that error at
// runtime.
func TestDefaultTagsAreWellFormed(t *testing.T) {
	t.Parallel()
	if _, err := Default(); err != nil {
		t.Fatalf("a default: tag in the schema does not parse: %v", err)
	}
}

// TestDefaultSafetySwitches pins the defaults that are decisions rather than
// values: every one of them is a choice about what the service is allowed to do
// to the outside world before an operator says anything.
//
// It deliberately does not restate ordinary numbers and timeouts — those are the
// `default:` tags themselves, and asserting them here only duplicates the
// schema.
func TestDefaultSafetySwitches(t *testing.T) {
	t.Parallel()
	c := defaultConfig(t)

	// Nothing reaches GitLab or Slack until an operator opts in.
	if c.Service.SlackSendEnabled || c.Service.AIReviewPublishEnabled {
		t.Error("service dry-run switches must default to false")
	}
	if c.Review.Coverage.Enabled {
		t.Error("review.coverage.enabled must default to false (executes repository code)")
	}
	if c.Review.Coverage.Node.Install {
		t.Error("review.coverage.node.install must default to false (runs lifecycle scripts)")
	}
	// Verifiers that execute repository code (tsc, go_test) must stay out.
	for _, v := range c.Review.Pipeline.Verifiers {
		switch v {
		case "go_build", "go_vet", "py_syntax":
		default:
			t.Errorf("default verifier %q executes repository code; keep it an opt-in", v)
		}
	}
	// On by default: the service brings its own schema up, application migrations
	// then River's. What makes that safe with N replicas is App.Migrate holding
	// one advisory lock across both migrators — if that lock ever goes away, this
	// default is what turns the loss into two replicas racing `CREATE TABLE
	// river_job` on a fresh install.
	if !c.Postgres.MigrateOnStart {
		t.Error("postgres.migrate_on_start must default to true; the service migrates itself on start")
	}
	if c.LLM.Claude.Auth.Mode != llm.AuthExistingLogin {
		t.Errorf("claude auth mode = %q, want existing-login: any other mode needs a credential nobody supplied", c.LLM.Claude.Auth.Mode)
	}
	// /debug/pprof belongs in this list and nowhere else: the ops port carries no
	// authentication, so an on-by-default profiler hands heap and goroutine dumps
	// to anything that can reach the pod. It was previously asserted only against
	// config.example.yaml (TestExampleConfigLoads) — a file an env-only deployment
	// never reads, which is exactly the deployment that would be exposed. mx ships
	// it off; what is pinned here is that this package never turns it on, whether
	// by restating the embedded ops config with a `default:` of its own or by
	// seeding Default() by hand.
	if c.Ops.Profiler.Enabled {
		t.Error("ops.profiler.enabled must default to false: /debug/pprof on the ops port has no authentication in front of it")
	}
	// The other ops switches are deliberately NOT asserted, and the asymmetry is
	// the point: whether /livez, /readyz and /metrics are exposed is the
	// deployment's call (mx ships all three off, config.example.yaml turns them on,
	// an env-only deployment sets AI_REVIEWER_OPS_* itself — docs/configuration.md
	// lists them). A profiler is not that kind of choice.
}

// TestDefaultAllowedToolsAreReadOnlyAndWorktreeScoped pins plan §1's
// "Claude CLI does not write to the repository" at the only place it is
// actually enforced.
//
// Two shipped defaults were the defect. `Bash(git diff *)` is a prefix match and
// `git diff` accepts `--output=<path>` (an arbitrary-file write, which lets a
// "clean build" verdict be manufactured for the deterministic verifiers) and
// `--no-index <any file>` (an arbitrary-file read that walks around any path
// scope on Read). Reproduced against claude 2.1.222 with the shipped flags: the
// write succeeded with `permission_denials: []`. And unscoped `Read`/`Grep`/
// `Glob` made the worktree a working directory rather than a boundary.
// The digest schedule is a product decision, and this is now the only place in
// the tree it is written down: the scheduler takes whatever config hands it, so a
// silent edit to the default tag would change what every team is promised with
// nothing else failing. Pinned as literals for that reason, not derived.
func TestDefaultDigestSchedule(t *testing.T) {
	t.Parallel()
	c := defaultConfig(t)
	if got := c.Digest.Slots; !slices.Equal(got, []string{"09:00", "14:00", "17:30"}) {
		t.Errorf("digest.slots default = %v, want [09:00 14:00 17:30]", got)
	}
	if got := c.Digest.Timezone; got != "Europe/Moscow" {
		t.Errorf("digest.timezone default = %q, want Europe/Moscow", got)
	}
	// The per-team block must NOT be defaulted: a default there fills every team
	// and makes "inherit" indistinguishable from "set to the same value", which is
	// the distinction the whole override rests on.
	c.Teams = []TeamConfig{{Name: "payments"}}
	d := c.Teams[0].Digest
	if len(d.Slots) != 0 || d.Timezone != "" || len(d.SkipWeekdays) != 0 || len(d.SkipDates) != 0 {
		t.Errorf("teams[].digest was defaulted (%+v); inheritance can no longer be expressed", d)
	}
}

// Every half inherits independently, because the reasons to override are
// independent: a team in another country keeps the company's slot times, a team
// with an unusual rhythm keeps the company's zone, and a team that works
// Saturdays keeps the company's holiday list while dropping the weekend rule.
func TestDigestScheduleInheritance(t *testing.T) {
	t.Parallel()
	c := defaultConfig(t)
	c.Digest.Timezone = "Europe/Moscow"
	c.Digest.Slots = []string{"09:00", "17:30"}
	c.Digest.SkipWeekdays = []string{"sat", "sun"}
	c.Digest.SkipDates = []string{"01-01"}

	for _, tc := range []struct {
		name         string
		team         TeamDigestConfig
		wantSlot     []string
		wantTZ       string
		wantWeekdays []string
		wantDates    []string
	}{
		{name: "inherits both", wantSlot: []string{"09:00", "17:30"}, wantTZ: "Europe/Moscow"},
		{
			name:     "own slots, inherited zone",
			team:     TeamDigestConfig{Slots: []string{"11:00"}},
			wantSlot: []string{"11:00"}, wantTZ: "Europe/Moscow",
		},
		{
			name:     "own zone, inherited slots",
			team:     TeamDigestConfig{Timezone: "Asia/Tbilisi"},
			wantSlot: []string{"09:00", "17:30"}, wantTZ: "Asia/Tbilisi",
		},
		{
			name:     "own both",
			team:     TeamDigestConfig{Timezone: "Europe/Lisbon", Slots: []string{"10:00", "18:00"}},
			wantSlot: []string{"10:00", "18:00"}, wantTZ: "Europe/Lisbon",
		},
		{
			name:     "blank zone inherits rather than becoming UTC",
			team:     TeamDigestConfig{Timezone: "   "},
			wantSlot: []string{"09:00", "17:30"}, wantTZ: "Europe/Moscow",
		},
		{
			// The case the independence is for: a support team works weekends but
			// keeps the company holidays.
			name:         "own weekdays, inherited dates",
			team:         TeamDigestConfig{SkipWeekdays: []string{"sun"}},
			wantSlot:     []string{"09:00", "17:30"},
			wantTZ:       "Europe/Moscow",
			wantWeekdays: []string{"sun"},
			wantDates:    []string{"01-01"},
		},
		{
			name:         "own dates, inherited weekdays",
			team:         TeamDigestConfig{SkipDates: []string{"2026-05-09"}},
			wantSlot:     []string{"09:00", "17:30"},
			wantTZ:       "Europe/Moscow",
			wantWeekdays: []string{"sat", "sun"},
			wantDates:    []string{"2026-05-09"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Every case that does not say otherwise inherits the global skip lists.
			if tc.wantWeekdays == nil {
				tc.wantWeekdays = []string{"sat", "sun"}
			}
			if tc.wantDates == nil {
				tc.wantDates = []string{"01-01"}
			}
			got := c.DigestScheduleFor(TeamConfig{Name: "payments", Digest: tc.team})
			if !slices.Equal(got.Slots, tc.wantSlot) || got.Timezone != tc.wantTZ {
				t.Errorf("= (%v, %q), want (%v, %q)", got.Slots, got.Timezone, tc.wantSlot, tc.wantTZ)
			}
			if !slices.Equal(got.SkipWeekdays, tc.wantWeekdays) || !slices.Equal(got.SkipDates, tc.wantDates) {
				t.Errorf("skip = (%v, %v), want (%v, %v)",
					got.SkipWeekdays, got.SkipDates, tc.wantWeekdays, tc.wantDates)
			}
		})
	}
}

// TestValidateDigestSkipDays: a misspelled weekday or a date in the wrong shape
// does not stop the service — it just fails to skip the day, and nobody notices
// until a digest lands on a holiday. Refusing it at load time is the only moment
// anybody sees it.
func TestValidateDigestSkipDays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		global   DigestConfig
		team     TeamDigestConfig
		wantPart string
	}{
		{
			name:     "global weekday",
			global:   DigestConfig{SkipWeekdays: []string{"funday"}},
			wantPart: "digest: skip weekday",
		},
		{
			name:     "global date",
			global:   DigestConfig{SkipDates: []string{"01.01"}},
			wantPart: "digest: skip date",
		},
		{
			// Reported against the team, whichever side of the inheritance the bad
			// value came from: "teams[payments].digest" is what an operator greps.
			name:     "team weekday",
			team:     TeamDigestConfig{SkipWeekdays: []string{"вс"}},
			wantPart: "teams[payments].digest: skip weekday",
		},
		{
			name:     "team date",
			team:     TeamDigestConfig{SkipDates: []string{"2026-1-1"}},
			wantPart: "teams[payments].digest: skip date",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := defaultConfig(t)
			c.GitLab.BaseURL = "https://gitlab.example.com"
			c.GitLab.Token = "glpat-x"
			c.Postgres.Username = "ai_reviewer"
			c.Slack.Token = "xoxb-x"
			c.Digest.SkipWeekdays = tc.global.SkipWeekdays
			c.Digest.SkipDates = tc.global.SkipDates
			c.Teams = []TeamConfig{{
				Name: "payments", SlackChannel: "C012345678",
				Repositories: []string{"a/b"}, Digest: tc.team,
			}}

			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantPart) {
				t.Fatalf("Validate() = %v, want it to name %q", err, tc.wantPart)
			}
		})
	}

	// The values that work must keep working, including all seven weekdays: there
	// is no other way to say "this team wants no scheduled digest", so it is a
	// configuration doctor warns about rather than one the loader refuses.
	t.Run("valid values load", func(t *testing.T) {
		t.Parallel()
		c := defaultConfig(t)
		c.GitLab.BaseURL = "https://gitlab.example.com"
		c.GitLab.Token = "glpat-x"
		c.Postgres.Username = "ai_reviewer"
		c.Slack.Token = "xoxb-x"
		c.Digest.SkipWeekdays = []string{"sat", "Sunday"}
		c.Digest.SkipDates = []string{"01-01", "2026-05-09"}
		c.Teams = []TeamConfig{{
			Name: "payments", SlackChannel: "C012345678", Repositories: []string{"a/b"},
			Digest: TeamDigestConfig{SkipWeekdays: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}},
		}}
		if err := c.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// A schedule the service cannot name has to be refused at load time. Left to the
// first firing it is invisible: the service starts clean, every check passes and
// no digest is ever sent.
func TestValidateDigestSlots(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		slots []string
		want  string
	}{
		{name: "empty", slots: []string{}, want: "at least one digest slot"},
		{name: "not a time", slots: []string{"morning"}, want: "must be HH:MM"},
		// time.Parse accepts "9:00"; accepting it would file the same slot under two
		// spellings, because Clock.String only ever writes the padded form.
		{name: "unpadded", slots: []string{"9:00"}, want: "zero-padded"},
		{name: "out of range", slots: []string{"24:00"}, want: "must be HH:MM"},
		{name: "duplicate", slots: []string{"09:00", "09:00"}, want: "listed twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := digestBase(t)
			c.Digest.Slots = tc.slots
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted digest.slots = %v", tc.slots)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "digest.slots") {
				t.Errorf("error = %v, want it to name digest.slots and %q", err, tc.want)
			}
		})
	}

	// And the ordinary case still passes, in any order.
	c := digestBase(t)
	c.Digest.Slots = []string{"17:30", "09:00"}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate rejected a valid unsorted schedule: %v", err)
	}
}

// A per-team override is validated too, and the failure names the team: an
// operator reading it has to know whose digest is broken, not merely that some
// slot list is.
func TestValidateTeamDigestOverride(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		team TeamDigestConfig
		want string
	}{
		{name: "bad slot", team: TeamDigestConfig{Slots: []string{"9:00"}}, want: "teams[payments].digest.slots"},
		{name: "duplicate slot", team: TeamDigestConfig{Slots: []string{"09:00", "09:00"}}, want: "teams[payments].digest.slots"},
		{name: "unknown zone", team: TeamDigestConfig{Timezone: "Mars/Olympus"}, want: "teams[payments].digest.timezone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := digestBase(t)
			c.Teams[0].Digest = tc.team
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted teams[].digest = %+v", tc.team)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %s", err, tc.want)
			}
		})
	}

	t.Run("valid override passes", func(t *testing.T) {
		t.Parallel()
		c := digestBase(t)
		c.Teams[0].Digest = TeamDigestConfig{Timezone: "Europe/Lisbon", Slots: []string{"10:00", "18:00"}}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate rejected a valid override: %v", err)
		}
	})

	// The global block is checked even when every team overrides it: an unusable
	// default is a trap for the next team added, not dead config.
	t.Run("unused global default is still validated", func(t *testing.T) {
		t.Parallel()
		c := digestBase(t)
		c.Digest.Timezone = "Mars/Olympus"
		c.Teams[0].Digest = TeamDigestConfig{Timezone: "Europe/Lisbon", Slots: []string{"10:00"}}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "digest.timezone") {
			t.Errorf("error = %v, want the unused global timezone reported", err)
		}
	})
}

func TestValidateDigestTimezone(t *testing.T) {
	t.Parallel()
	for _, tz := range []string{"", "   ", "Mars/Olympus", "MSK+3"} {
		c := digestBase(t)
		c.Digest.Timezone = tz
		if err := c.Validate(); err == nil {
			t.Errorf("Validate accepted digest.timezone = %q", tz)
		}
	}
	// A zone the embedded tzdata really carries, in a different offset from the
	// default, so the test proves loading rather than string comparison.
	c := digestBase(t)
	c.Digest.Timezone = "America/Sao_Paulo"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate rejected a real IANA zone: %v", err)
	}
}

// digestBase is a config that passes Validate, so a slot test's only failure is
// the slot list.
func digestBase(t *testing.T) *Config {
	t.Helper()
	c := defaultConfig(t)
	c.GitLab.BaseURL = "https://gitlab.example.com"
	c.GitLab.Token = "glpat-x"
	c.Slack.Token = "xoxb-x"
	c.Postgres.Username = "ai_reviewer"
	c.Teams = []TeamConfig{{
		Name: "payments", SlackChannel: "C012345678", Repositories: []string{"a/b"},
	}}
	return c
}

func TestDefaultAllowedToolsAreReadOnlyAndWorktreeScoped(t *testing.T) {
	tools := defaultConfig(t).LLM.Claude.AllowedTools
	if len(tools) == 0 {
		t.Fatal("allowed_tools default is empty")
	}
	for _, rule := range tools {
		name, args, ok := strings.Cut(strings.TrimSuffix(rule, ")"), "(")
		if !ok {
			t.Errorf("rule %q has no path scope; the worktree would not be a boundary", rule)
			continue
		}
		switch name {
		case "Read", "Grep", "Glob":
		default:
			// Bash in particular: every git subcommand that takes diff options is
			// both a write and an arbitrary-read primitive.
			t.Errorf("rule %q grants %q; only the read-only tools may be granted by default", rule, name)
		}
		if !strings.Contains(args, llm.WorktreePlaceholder) {
			t.Errorf("rule %q is not scoped to %s", rule, llm.WorktreePlaceholder)
		}
	}
}

// TestExampleConfigLoads pins config.example.yaml against the schema: an unknown
// or renamed key fails the load (WithDisallowUnknownFields), and the file must
// still describe a valid deployment once the secrets it deliberately leaves
// empty are supplied.
func TestExampleConfigLoads(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config.example.yaml is missing: %v", err)
	}
	t.Setenv("AI_REVIEWER_GITLAB_TOKEN", "glpat-example")
	t.Setenv("AI_REVIEWER_SLACK_TOKEN", "xoxb-example")
	t.Setenv("AI_REVIEWER_POSTGRES_USERNAME", "ai_reviewer")

	c, err := loadFile(t, path)
	if err != nil {
		t.Fatalf("config.example.yaml does not load: %v", err)
	}
	if len(c.Teams) != 2 {
		t.Errorf("teams = %d, want the 2 example teams", len(c.Teams))
	}
	if c.Service.SlackSendEnabled || c.Service.AIReviewPublishEnabled {
		t.Error("the example must ship with both dry-run switches off")
	}
	// The publish switch is not a cost switch: with it off, reviews still run and
	// still cost a few dollars per merge request, they just post nothing. An example
	// that enables reviewing therefore starts spending on its first scan pass — 25
	// open merge requests on the live project it was first pointed at.
	for _, team := range c.Teams {
		if team.AIReview.Enabled {
			t.Errorf("team %q enables ai_review: the example must not spend money on its first scan", team.Name)
		}
	}
	// mx ships the ops server off and this package adds no default of its own, so
	// the example file is the only thing that turns /livez, /readyz and /metrics
	// on — and docker-compose.yml healthchecks /livez against exactly this file.
	// If the block is ever dropped from the example, the documented install path
	// comes up with no listener and the container never reports healthy.
	if !c.Ops.Enabled || !c.Ops.Healthy.Enabled || !c.Ops.Metrics.Enabled {
		t.Errorf("the example must enable the ops server, health checker and metrics: %+v", c.Ops)
	}
	if c.Ops.Profiler.Enabled {
		t.Error("the example must leave /debug/pprof off: the ops port carries no authentication")
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	path := writeConfig(t, minimalYAML+"watch:\n  enabled: true\n")
	if _, err := loadFile(t, path); err == nil {
		t.Fatal("a removed config section must be rejected, not silently ignored")
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	c := defaultConfig(t)
	c.GitLab.BaseURL = "gitlab.example.com" // not absolute
	c.GitLab.Token = ""
	c.Review.ScanInterval = 10 * time.Second
	c.Review.MaxComments = 0
	c.Teams = nil

	err := c.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"gitlab.base_url", "gitlab.token", "review.scan_interval",
		"review.max_comments", "teams is empty",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error is missing %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "problem(s)") {
		t.Errorf("errors should be rendered as a numbered list:\n%s", msg)
	}
}

// rawTagError is go-playground's own rendering. It names a Go field path, not a
// config key, and can never name the env variable or the Vault key — so it must
// not be what an operator sees.
const rawTagError = "failed on the 'required' tag"

// TestMissingSecretsAreReportedInOperatorVocabulary drives the whole load path,
// not Validate() in isolation: the defect it pins is one of *ordering*. xconfig
// runs Validate() before the tag validator, so a required secret with no domain
// check here falls through to the tag and the operator gets
//
//	load config: Key: 'Config.Postgres.Username' Error:Field validation for 'Username' failed on the 'required' tag
//
// which is exactly what someone starting from config.example.yaml hit first.
func TestMissingSecretsAreReportedInOperatorVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name string
		yml  string
		want []string
	}{
		{
			name: "postgres.username",
			yml: `
gitlab:
  base_url: https://gitlab.example.com
  token: glpat-test
slack:
  token: xoxb-test
teams:
  - name: payments
    slack_channel: C012345678
    ai_review: { enabled: true }
    repositories: [backend/payments]
`,
			want: []string{
				"postgres.username is empty",
				"AI_REVIEWER_POSTGRES_USERNAME",
				"Vault key",
			},
		},
		{
			name: "gitlab.token",
			yml: `
gitlab:
  base_url: https://gitlab.example.com
slack:
  token: xoxb-test
postgres:
  username: ai_reviewer
teams:
  - name: payments
    slack_channel: C012345678
    ai_review: { enabled: true }
    repositories: [backend/payments]
`,
			want: []string{
				"gitlab.token is empty",
				"AI_REVIEWER_GITLAB_TOKEN",
				"Vault key",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFile(t, writeConfig(t, tc.yml))
			if err == nil {
				t.Fatal("a missing required secret must fail the load")
			}
			msg := err.Error()
			if strings.Contains(msg, rawTagError) {
				t.Errorf("the operator got go-playground's raw rendering:\n%s", msg)
			}
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("error is missing %q:\n%s", want, msg)
				}
			}
			// The aggregated numbered list is deliberate; a tag failure would
			// short-circuit it.
			if !strings.Contains(msg, "problem(s)") {
				t.Errorf("the problem was not reported in the aggregated list:\n%s", msg)
			}
		})
	}
}

// A missing secret must not stop the other checks from being reported: fixing a
// config one error per restart is the feedback loop joinConfigErrors exists to
// avoid, and a tag failure would produce exactly that.
func TestMissingSecretsStillAggregateWithEverythingElse(t *testing.T) {
	c := defaultConfig(t)
	c.GitLab.BaseURL = "https://gitlab.example.com"
	c.GitLab.Token = ""
	c.Postgres.Username = ""
	c.Review.MaxComments = 0
	c.Teams = nil

	err := c.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"postgres.username is empty", "gitlab.token is empty",
		"review.max_comments", "teams is empty", "4 problem(s)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error is missing %q:\n%s", want, msg)
		}
	}
}

// postgres.password is deliberately optional — IAM and peer authentication run
// without one — so it must not acquire a required check by symmetry with the
// username.
func TestPostgresPasswordIsOptional(t *testing.T) {
	c := defaultConfig(t)
	c.GitLab.BaseURL = "https://gitlab.example.com"
	c.GitLab.Token = "glpat-test"
	c.Slack.Token = "xoxb-test"
	c.Postgres.Username = "ai_reviewer"
	c.Postgres.Password = ""
	c.Teams = []TeamConfig{{
		Name: "payments", SlackChannel: "C012345678", Repositories: []string{"backend/payments"},
	}}

	if err := c.Validate(); err != nil {
		t.Errorf("an empty postgres.password must be valid: %v", err)
	}
}

// The Vault bootstrap fails earlier than anything else — before the main config
// is read — so its message is the first thing an operator setting Vault up sees.
// It is loaded from a separate source set with no YAML keys, so its checks name
// the (unprefixed) environment variables directly.
func TestVaultBootstrapNamesItsVariables(t *testing.T) {
	setVault := func(t *testing.T, kv map[string]string) {
		t.Helper()
		// Every VAULT_* the bootstrap reads is set explicitly, so a developer
		// machine with its own Vault environment cannot change the outcome.
		for _, k := range []string{
			"VAULT_ENABLED", "VAULT_ADDR", "VAULT_SECRET_PATH", "VAULT_AUTH_KIND",
			"VAULT_TOKEN", "VAULT_KUBE_ROLE", "VAULT_KUBE_JWT_PATH", "VAULT_KUBE_MOUNT_PATH",
		} {
			t.Setenv(k, kv[k])
		}
	}

	t.Run("token auth with no token", func(t *testing.T) {
		setVault(t, map[string]string{
			"VAULT_ENABLED":     "true",
			"VAULT_ADDR":        "https://vault.example.com",
			"VAULT_SECRET_PATH": "secret/data/ai-reviewer",
			"VAULT_AUTH_KIND":   "token",
		})

		_, err := loadFile(t, writeConfig(t, minimalYAML))
		if err == nil {
			t.Fatal("vault token auth with no token must fail the load")
		}
		msg := err.Error()
		if strings.Contains(msg, rawTagError) {
			t.Errorf("the operator got go-playground's raw rendering:\n%s", msg)
		}
		if !strings.Contains(msg, "VAULT_TOKEN is empty but VAULT_AUTH_KIND=token requires it") {
			t.Errorf("error does not name VAULT_TOKEN and why it is needed:\n%s", msg)
		}
	})

	t.Run("every bootstrap problem at once", func(t *testing.T) {
		setVault(t, map[string]string{"VAULT_ENABLED": "true", "VAULT_AUTH_KIND": "token"})

		_, err := loadFile(t, writeConfig(t, minimalYAML))
		if err == nil {
			t.Fatal("want an error")
		}
		msg := err.Error()
		for _, want := range []string{"VAULT_ADDR", "VAULT_SECRET_PATH", "VAULT_TOKEN", "3 problem(s)"} {
			if !strings.Contains(msg, want) {
				t.Errorf("aggregated bootstrap error is missing %q:\n%s", want, msg)
			}
		}
	})

	t.Run("disabled vault needs nothing", func(t *testing.T) {
		setVault(t, map[string]string{"VAULT_ENABLED": "false"})
		if _, err := loadFile(t, writeConfig(t, minimalYAML)); err != nil {
			t.Errorf("a disabled Vault must not require its variables: %v", err)
		}
	})
}

// TestSecretsAreNotValidatedByTag is the structural guard behind the rule in
// Validate's doc comment. Re-adding `validate:"required"` to a Secret would
// compile, pass every behavioural test whose field also has a domain check, and
// silently regress the message for any field that does not — which is how
// postgres.username came to greet operators with a Go field path.
func TestSecretsAreNotValidatedByTag(t *testing.T) {
	t.Parallel()
	secretType := reflect.TypeFor[Secret]()

	var walk func(rt reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(rt reflect.Type, path string, seen map[reflect.Type]bool) {
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for f := range rt.Fields() {
			f := f
			if f.Type == secretType {
				if v, ok := f.Tag.Lookup("validate"); ok && strings.Contains(v, "required") {
					t.Errorf("%s.%s is a Secret with validate:%q — the tag validator cannot name the "+
						"env variable or the Vault key, and it short-circuits the aggregated list; "+
						"check it in Validate() instead", path, f.Name, v)
				}
				continue
			}
			walk(f.Type, path+"."+f.Name, seen)
		}
	}
	walk(reflect.TypeFor[Config](), "Config", map[reflect.Type]bool{})
	walk(reflect.TypeFor[VaultConfig](), "VaultConfig", map[reflect.Type]bool{})
}

func TestValidateTeams(t *testing.T) {
	base := func() *Config {
		c := defaultConfig(t)
		c.GitLab.BaseURL = "https://gitlab.example.com"
		c.GitLab.Token = "glpat-x"
		c.Slack.Token = "xoxb-x"
		c.Postgres.Username = "ai_reviewer"
		return c
	}

	tests := []struct {
		name  string
		teams []TeamConfig
		want  string
	}{
		{
			name: "ok",
			teams: []TeamConfig{
				{Name: "payments", SlackChannel: "C012345678", Repositories: []string{"backend/payments", "42"}},
			},
		},
		{
			name: "duplicate names differing only in case",
			teams: []TeamConfig{
				{Name: "payments", SlackChannel: "C012345678", Repositories: []string{"a/b"}},
				{Name: "Payments", SlackChannel: "C012345679", Repositories: []string{"c/d"}},
			},
			want: "duplicate team name",
		},
		{
			name: "repository claimed twice",
			teams: []TeamConfig{
				{Name: "payments", SlackChannel: "C012345678", Repositories: []string{"a/b"}},
				{Name: "platform", SlackChannel: "C012345679", Repositories: []string{"a/b"}},
			},
			want: `claimed by both team "payments" and team "platform"`,
		},
		{
			name:  "empty repositories",
			teams: []TeamConfig{{Name: "payments", SlackChannel: "C012345678"}},
			want:  "repositories is empty",
		},
		{
			name:  "channel name instead of id",
			teams: []TeamConfig{{Name: "payments", SlackChannel: "#payments", Repositories: []string{"a/b"}}},
			want:  "not a Slack channel id",
		},
		{
			name:  "repository is neither a path nor an id",
			teams: []TeamConfig{{Name: "payments", SlackChannel: "C012345678", Repositories: []string{"not a repo!"}}},
			want:  "neither a GitLab path",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			c.Teams = tc.teams
			err := c.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want == "":
			case err == nil:
				t.Fatalf("want an error containing %q, got nil", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateLinear(t *testing.T) {
	const (
		linearTeamA = "9cfb482a-81e3-4154-b5b9-2c805e70a02d"
		linearTeamB = "6f9568f2-3e89-4f54-9ab2-76d14bc90f3e"
	)
	base := func() *Config {
		c := defaultConfig(t)
		c.GitLab.BaseURL = "https://gitlab.example.com"
		c.GitLab.Token = "glpat-x"
		c.Slack.Token = "xoxb-x"
		c.Postgres.Username = "ai_reviewer"
		c.Teams = []TeamConfig{{
			Name: "payments", SlackChannel: "C012345678", Repositories: []string{"a/b"},
			LinearTeamIDs: []string{linearTeamA},
		}}
		return c
	}

	t.Run("configured", func(t *testing.T) {
		c := base()
		c.Linear.APIKey = "lin_api_key"
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name:   "missing key",
			mutate: func(*Config) {},
			want:   "AI_REVIEWER_LINEAR_API_KEY",
		},
		{
			name: "invalid uuid",
			mutate: func(c *Config) {
				c.Linear.APIKey = "lin_api_key"
				c.Teams[0].LinearTeamIDs = []string{"PAY"}
			},
			want: "is not a UUID",
		},
		{
			name: "team claimed twice",
			mutate: func(c *Config) {
				c.Linear.APIKey = "lin_api_key"
				c.Teams = append(c.Teams, TeamConfig{
					Name: "platform", SlackChannel: "C987654321", Repositories: []string{"c/d"},
					LinearTeamIDs: []string{linearTeamA, linearTeamB},
				})
			},
			want: "claimed by both team",
		},
		{
			name: "insecure endpoint",
			mutate: func(c *Config) {
				c.Linear.APIKey = "lin_api_key"
				c.Linear.Endpoint = "http://linear.example/graphql"
			},
			want: "absolute HTTPS URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want %q", err, tt.want)
			}
		})
	}

	t.Run("unused block needs no key", func(t *testing.T) {
		c := base()
		c.Teams[0].LinearTeamIDs = nil
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
}

func TestLinearAPIKeyEnvironmentName(t *testing.T) {
	t.Setenv("AI_REVIEWER_LINEAR_API_KEY", "lin_api_from_env")
	path := writeConfig(t, minimalYAML+`
linear:
  endpoint: https://api.linear.app/graphql
`)
	cfg, err := loadFile(t, path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Linear.APIKey.Unmask(); got != "lin_api_from_env" {
		t.Errorf("linear.api_key = %q, want env value", got)
	}
}

func TestValidateClaudeAuthMode(t *testing.T) {
	base := func() *Config {
		c := defaultConfig(t)
		c.GitLab.BaseURL = "https://gitlab.example.com"
		c.GitLab.Token = "glpat-x"
		c.Slack.Token = "xoxb-x"
		c.Postgres.Username = "ai_reviewer"
		c.Teams = []TeamConfig{{Name: "t", SlackChannel: "C012345678", Repositories: []string{"a/b"}}}
		return c
	}

	t.Run("unknown mode", func(t *testing.T) {
		c := base()
		c.LLM.Claude.Auth.Mode = "bogus"
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "llm.claude.auth.mode") {
			t.Fatalf("want a mode error, got %v", err)
		}
	})
	t.Run("oauth-token without a token", func(t *testing.T) {
		c := base()
		c.LLM.Claude.Auth.Mode = llm.AuthOAuthToken
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "requires oauth_token") {
			t.Fatalf("want a missing-secret error, got %v", err)
		}
	})
	t.Run("existing-login with a stray secret", func(t *testing.T) {
		c := base()
		c.LLM.Claude.Auth.APIKey = "sk-ant-stray"
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "takes no secret") {
			t.Fatalf("want a stray-secret error, got %v", err)
		}
	})
	t.Run("api-key with its key", func(t *testing.T) {
		c := base()
		c.LLM.Claude.Auth.Mode = llm.AuthAPIKey
		c.LLM.Claude.Auth.APIKey = "sk-ant-real"
		if err := c.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestValidateSlackTokenRequiredWhenChannelsConfigured(t *testing.T) {
	c := defaultConfig(t)
	c.GitLab.BaseURL = "https://gitlab.example.com"
	c.GitLab.Token = "glpat-x"
	c.Postgres.Username = "ai_reviewer"
	c.Teams = []TeamConfig{{Name: "t", SlackChannel: "C012345678", Repositories: []string{"a/b"}}}

	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "slack.token is empty") {
		t.Fatalf("a configured channel with no token must fail, got %v", err)
	}
	c.Slack.Token = "xoxb-x"
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidateSlackCommands covers the in-chat commands' configuration. Each
// case is a way to end up with a service that connects to Slack and then answers
// nothing, which is the failure mode the checks exist for: from a channel, a
// misconfigured command and an ignored one look identical.
func TestValidateSlackCommands(t *testing.T) {
	base := func() *Config {
		c := defaultConfig(t)
		c.GitLab.BaseURL = "https://gitlab.example.com"
		c.GitLab.Token = "glpat-x"
		c.Postgres.Username = "ai_reviewer"
		c.Slack.Token = "xoxb-x"
		c.Teams = []TeamConfig{{Name: "t", SlackChannel: "C012345678", Repositories: []string{"a/b"}}}
		return c
	}

	t.Run("the defaults are usable", func(t *testing.T) {
		c := base()
		c.Slack.AppToken = "xapp-1-A0-0-secret"
		if err := c.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.Slack.Commands.Team != "/all" || c.Slack.Commands.Mine != "/my" {
			t.Errorf("defaults = %q / %q, want /all and /my", c.Slack.Commands.Team, c.Slack.Commands.Mine)
		}
	})

	// The app token opens the socket and cannot answer on it: every command
	// resolves people through the directory and renders mentions, both the bot
	// token's work.
	t.Run("the app token needs the bot token", func(t *testing.T) {
		c := base()
		c.Slack.Token = ""
		c.Slack.AppToken = "xapp-1-A0-0-secret"
		c.Teams = nil
		c.Service.SlackSendEnabled = false
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "slack.app_token is set but slack.token is empty") {
			t.Fatalf("want the pair enforced, got %v", err)
		}
	})

	t.Run("a name that is not a command", func(t *testing.T) {
		c := base()
		c.Slack.AppToken = "xapp-1-A0-0-secret"
		c.Slack.Commands.Team = "all"
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "slack.commands.team") {
			t.Fatalf("want a missing slash rejected, got %v", err)
		}
	})

	// Both names the same would force the dispatcher to pick one, and picking the
	// team digest posts one person's queue to the channel.
	t.Run("the two commands must differ", func(t *testing.T) {
		c := base()
		c.Slack.AppToken = "xapp-1-A0-0-secret"
		c.Slack.Commands.Team = "/mr"
		c.Slack.Commands.Mine = "/MR"
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "they must differ") {
			t.Fatalf("want identical command names rejected, got %v", err)
		}
	})

	// Without an app token nothing listens, so the names are inert and must not
	// fail a deployment that never set them.
	t.Run("names are not checked without the token", func(t *testing.T) {
		c := base()
		c.Slack.Commands.Team = "nonsense"
		c.Slack.Commands.Mine = "nonsense"
		if err := c.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestSecretNeverLeaks(t *testing.T) {
	s := Secret("glpat-supersecret")
	if got := s.Unmask(); got != "glpat-supersecret" {
		t.Errorf("Unmask() = %q", got)
	}
	if !s.IsSet() || Secret("").IsSet() {
		t.Error("IsSet is wrong")
	}
	for name, got := range map[string]string{
		"String":   s.String(),
		"GoString": s.GoString(),
		"%v":       fmtSprintf("%v", s),
		"%s":       fmtSprintf("%s", s),
		"%#v":      fmtSprintf("%#v", s),
	} {
		if got != redactedPlaceholder {
			t.Errorf("%s = %q, want %q", name, got, redactedPlaceholder)
		}
	}
	j, err := json.Marshal(struct{ T Secret }{s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(j), "supersecret") {
		t.Errorf("JSON leaked the secret: %s", j)
	}
	y, err := s.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	if y != redactedPlaceholder {
		t.Errorf("MarshalYAML = %v", y)
	}
	// A whole Config formatted with %+v must not leak either — that is how a
	// config dump reaches a log line.
	c := defaultConfig(t)
	c.GitLab.Token = "glpat-supersecret"
	if strings.Contains(fmtSprintf("%+v", *c), "supersecret") {
		t.Error("formatting the Config leaked a secret")
	}
}

// TestRegisterSecretsCoversEverySecretField walks the schema for `Secret`
// fields, fills each with a distinctive value, and asserts the redactor masks
// all of them.
//
// It is written reflectively on purpose: the list in RegisterSecrets is
// hand-maintained, and the failure mode of forgetting an entry is silent —
// the field is still masked in a config dump and still read from Vault, so it
// looks handled, while any library that puts it in an error prints it in the
// clear. Postgres.Username was exactly that, for long enough that a comment in
// doctor justified another omission by claiming this one was covered.
func TestRegisterSecretsCoversEverySecretField(t *testing.T) {
	c := defaultConfig(t)
	secretType := reflect.TypeFor[Secret]()

	// path → the value planted there, so a failure names the field.
	planted := map[string]string{}
	var plant func(v reflect.Value, path string)
	plant = func(v reflect.Value, path string) {
		if v.Type() == secretType {
			// Distinctive and long enough to clear the redactor's minimum length;
			// nothing else in the process can contain it.
			val := "registered-secret-probe-" + strings.ReplaceAll(path, ".", "-")
			v.SetString(val)
			planted[path] = val
			return
		}
		if v.Kind() != reflect.Struct {
			return
		}
		for i := range v.NumField() {
			if !v.Field(i).CanSet() {
				continue
			}
			plant(v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	}
	plant(reflect.ValueOf(c).Elem(), "Config")

	if len(planted) < 6 {
		t.Fatalf("the walk found only %d Secret fields (%v); it is not covering the schema", len(planted), planted)
	}

	c.RegisterSecrets()
	for path, val := range planted {
		if security.Mask(val) == val {
			t.Errorf("%s is a Secret but is not registered with the redactor; add it to RegisterSecrets", path)
		}
	}
}

// The Vault token is the root of trust for every other secret, and it is loaded
// from a different source set than the rest of the schema — so it needs its own
// registration and its own guard.
func TestVaultConfigRegistersItsToken(t *testing.T) {
	const token = "hvs.registeredvaulttokenprobe"
	VaultConfig{Token: token}.RegisterSecrets()
	if security.Mask(token) == token {
		t.Error("VAULT_TOKEN is not registered with the redactor")
	}
}

func TestPostgresDSN(t *testing.T) {
	c := PostgresConfig{Host: "db", Port: "5432", Database: "ai_reviewer", Username: "u", Password: "p", SSLMode: "require"}
	if got, want := c.DSN(), "postgres://u:p@db:5432/ai_reviewer?sslmode=require"; got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
	// No password (trust/peer auth) must not produce a stray ":@".
	c.Password = ""
	if got, want := c.DSN(), "postgres://u@db:5432/ai_reviewer?sslmode=require"; got != want {
		t.Errorf("DSN() without password = %q, want %q", got, want)
	}
}

func fmtSprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// TestEveryFieldHasAYAMLTag guards the tag the whole defaulting scheme rests on.
//
// The yaml tag does two jobs. It names the key in the file, and xconfig resolves
// it a second time to decide whether a field was *explicitly present* there — a
// field that was is never overwritten by its `default:`. A field without the tag
// falls back to the lower-cased Go name for that second lookup, so the presence
// check quietly stops matching and a configured `false` starts being refilled
// with `true` again. That failure is invisible in a diff, hence this check.
//
// Third-party embedded configs (logger.Config, ops.Config) are exempt: we do not
// own their tags. VaultConfig is not walked at all — it has no file keys by
// design, being read from the unprefixed environment before anything else.
func TestEveryFieldHasAYAMLTag(t *testing.T) {
	t.Parallel()
	pkg := reflect.TypeFor[Config]().PkgPath()

	var walk func(rt reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(rt reflect.Type, path string, seen map[reflect.Type]bool) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || rt.PkgPath() != pkg || seen[rt] {
			return
		}
		seen[rt] = true
		for f := range rt.Fields() {
			if !f.IsExported() {
				continue
			}
			if tag, ok := f.Tag.Lookup("yaml"); !ok || tag == "" {
				t.Errorf("%s.%s has no yaml tag; its default would stop respecting an explicit value in the file",
					path, f.Name)
			}
			walk(f.Type, path+"."+f.Name, seen)
		}
	}
	walk(reflect.TypeFor[Config](), "Config", map[reflect.Type]bool{})
}
