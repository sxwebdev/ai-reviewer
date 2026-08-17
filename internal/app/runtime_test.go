package app

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/tkcrm/mx/logger"
)

// quietLogger keeps the suite's output readable; nothing here asserts on logs.
func quietLogger() logger.ExtendedLogger {
	return logger.NewExtended(logger.WithConfig(logger.Config{
		Level: logger.LogLevelFatal, Format: logger.LoggerFormatJSON,
	}))
}

func testConfig(t *testing.T) *config.Config {
	cfg := defaultConfig(t)
	cfg.Teams = []config.TeamConfig{
		{
			Name: "Payments", SlackChannel: "C1",
			AIReview:     config.TeamAIReviewConfig{Enabled: true},
			Repositories: []string{"backend/payments", "backend/billing"},
		},
		{
			Name: "platform", SlackChannel: "C2",
			Repositories: []string{"platform/auth"},
		},
	}
	return cfg
}

func TestTeamsMapping(t *testing.T) {
	t.Parallel()
	got := Teams(testConfig(t))
	if len(got) != 2 {
		t.Fatalf("teams = %d, want 2", len(got))
	}
	if got[0].Name != "Payments" || got[0].SlackChannel != "C1" || !got[0].AIReview {
		t.Errorf("team = %+v", got[0])
	}
	// A team without ai_review.enabled must arrive with the switch off: the
	// digest still runs for it, automated review does not.
	if got[1].AIReview {
		t.Error("ai_review defaulted to on for a team that did not ask for it")
	}
	if !slices.Equal(got[0].Repositories, []string{"backend/payments", "backend/billing"}) {
		t.Errorf("repositories = %v", got[0].Repositories)
	}
	if len(Teams(defaultConfig(t))) != 0 {
		t.Error("a config with no teams must map to no teams")
	}
}

func TestTeamLookups(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)

	// Case-insensitive: config validation enforces uniqueness under the same
	// comparison, and a CLI flag is typed by a human.
	if team, ok := TeamByName(cfg, "PAYMENTS"); !ok || team.Name != "Payments" {
		t.Errorf("TeamByName(PAYMENTS) = %+v, %v", team, ok)
	}
	if _, ok := TeamByName(cfg, "retired"); ok {
		t.Error("an unknown team resolved")
	}

	if team, ok := TeamForRepository(cfg, "Backend/Billing"); !ok || team.Name != "Payments" {
		t.Errorf("TeamForRepository = %+v, %v", team, ok)
	}
	if _, ok := TeamForRepository(cfg, "other/repo"); ok {
		t.Error("an unconfigured repository resolved to a team")
	}
}

// TestJobsConfigSizesTheReviewQueueFromReviewMaxParallel pins the plan's "one
// knob" decision: jobs.queues deliberately has no `review` entry, because two
// settings for one pool drift apart and the one that loses is invisible.
func TestJobsConfigSizesTheReviewQueueFromReviewMaxParallel(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.Review.MaxParallel = 7
	cfg.Jobs.Queues = config.QueuesConfig{Default: 3, Publish: 2, Slack: 4}
	cfg.Service.AIReviewPublishEnabled = true
	cfg.Service.SlackSendEnabled = true

	a := &App{Config: cfg, Log: quietLogger()}
	got := a.jobsConfig()

	if got.ReviewWorkers != 7 {
		t.Errorf("ReviewWorkers = %d, want review.max_parallel (7)", got.ReviewWorkers)
	}
	if got.DefaultWorkers != 3 || got.PublishWorkers != 2 || got.SlackWorkers != 4 {
		t.Errorf("queue sizes = %+v", got)
	}
	if !got.PublishEnabled || !got.SlackSendEnabled {
		t.Errorf("the dry-run switches did not travel: %+v", got)
	}
	if got.ScanInterval != cfg.Review.ScanInterval || got.CleanupInterval != cfg.Jobs.CleanupInterval ||
		got.DrainTimeout != cfg.Jobs.DrainTimeout || got.WorkDir != cfg.Review.WorkDir {
		t.Errorf("jobs config = %+v", got)
	}
	if len(got.Teams) != 2 {
		t.Errorf("teams = %d, want 2", len(got.Teams))
	}
}

// TestPipelineFromConfigModes pins the presets: the mode is the only thing most
// operators set, and each one implies a different cost.
func TestPipelineFromConfigModes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode           string
		passes         int
		wantVerify     string
		completeness   string
		wantCompletion string
	}{
		{"cheap", 1, review.VerifyOff, "auto", review.CompletenessOff},
		{"standard", 2, "skeptic", "auto", review.CompletenessAuto},
		{"deep", 5, "skeptic", "auto", review.CompletenessAuto},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			rc := defaultConfig(t).Review
			rc.Pipeline.Mode = tc.mode
			rc.Pipeline.Completeness = tc.completeness

			got := pipelineFromConfig(rc)
			if len(got.Passes) != tc.passes {
				t.Errorf("passes = %v, want %d", got.Passes, tc.passes)
			}
			if got.VerifyMode != tc.wantVerify {
				t.Errorf("verify mode = %q, want %q", got.VerifyMode, tc.wantVerify)
			}
			if got.Completeness != tc.wantCompletion {
				t.Errorf("completeness = %q, want %q", got.Completeness, tc.wantCompletion)
			}
		})
	}

	// custom takes the configured list verbatim.
	rc := defaultConfig(t).Review
	rc.Pipeline.Mode = "custom"
	rc.Pipeline.Passes = []string{review.PassSecurity}
	if got := pipelineFromConfig(rc); !slices.Equal(got.Passes, []string{review.PassSecurity}) {
		t.Errorf("custom passes = %v", got.Passes)
	}

	// An explicit "on" survives cheap mode, which is the whole point of the
	// tri-state: the preset decides only when the operator did not.
	rc = defaultConfig(t).Review
	rc.Pipeline.Mode = "cheap"
	rc.Pipeline.Completeness = "on"
	if got := pipelineFromConfig(rc); got.Completeness != review.CompletenessOn {
		t.Errorf("completeness = %q, want an explicit on to survive cheap mode", got.Completeness)
	}
}

// TestContextBudgetConvertsKilobytes: the config is in KB and the engine counts
// bytes. Getting this wrong by 1024× either sends nothing or sends everything.
func TestContextBudgetConvertsKilobytes(t *testing.T) {
	t.Parallel()
	rc := defaultConfig(t).Review
	rc.Context.MaxTotalKB = 256
	rc.Context.MaxDiscussionKB = 4
	rc.Context.InterdiffMaxKB = 32

	got := contextBudgetFromConfig(rc)
	if got.MaxTotalBytes != 256<<10 || got.MaxDiscussionBytes != 4<<10 || got.MaxInterdiffBytes != 32<<10 {
		t.Errorf("budget = %+v", got)
	}
	if got.MaxFileLines != rc.Context.MaxFileLines || got.HunkWindowLines != rc.Context.HunkWindowLines {
		t.Errorf("line budgets were altered: %+v", got)
	}
}

func TestProfileFromConfig(t *testing.T) {
	t.Parallel()
	rc := defaultConfig(t).Review
	rc.PreferredCommentLanguage = "ru"
	rc.MaxComments = 3
	rc.SeverityThreshold = "blocking"

	got := profileFromConfig(rc)
	if got.Language != "ru" || got.MaxComments != 3 || got.SeverityThreshold != "blocking" {
		t.Errorf("profile = %+v", got)
	}

	// Empty/zero settings must leave the built-in defaults alone rather than
	// blanking them.
	def := profileFromConfig(config.ReviewConfig{})
	base := review.DefaultProfile()
	if def.Language != base.Language || def.MaxComments != base.MaxComments ||
		def.SeverityThreshold != base.SeverityThreshold {
		t.Errorf("an empty config overwrote the defaults: %+v", def)
	}
}

func TestRiskAndCoverageSettings(t *testing.T) {
	t.Parallel()
	rc := defaultConfig(t).Review
	risk := riskSettingsFromConfig(rc)
	if !risk.Enabled || risk.HistoryCommits != rc.Risk.HistoryCommits ||
		!slices.Equal(risk.SensitiveGlobs, rc.Risk.SensitiveGlobs) {
		t.Errorf("risk = %+v", risk)
	}

	cov := coverageSettingsFromConfig(rc)
	// Coverage executes the reviewed repository's test code, so it must stay an
	// explicit opt-in — a default of true here is a security regression.
	if cov.Enabled {
		t.Error("coverage defaulted to enabled; it runs untrusted repository code")
	}
	if cov.Options.Timeout != rc.Coverage.Timeout || cov.Options.NodeInstall != rc.Coverage.Node.Install {
		t.Errorf("coverage options = %+v", cov.Options)
	}
}

func TestCheckWorkdir(t *testing.T) {
	t.Parallel()

	t.Run("writable", func(t *testing.T) {
		t.Parallel()
		cfg := defaultConfig(t)
		cfg.Review.WorkDir = filepath.Join(t.TempDir(), "work")
		a := &App{Config: cfg, Log: quietLogger()}

		col := &checkCollector{}
		a.checkWorkdir(col)
		if len(col.checks) != 1 || col.checks[0].Status != StatusOK {
			t.Fatalf("checks = %+v", col.checks)
		}
		// The probe must leave nothing behind: an hourly doctor in a cron would
		// otherwise litter the directory the cleanup job sweeps.
		entries, err := os.ReadDir(cfg.Review.WorkDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("the probe left %d file(s) behind", len(entries))
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		cfg := defaultConfig(t)
		cfg.Review.WorkDir = ""
		a := &App{Config: cfg, Log: quietLogger()}

		col := &checkCollector{}
		a.checkWorkdir(col)
		if len(col.checks) != 1 || col.checks[0].Status != StatusFail {
			t.Fatalf("an empty workdir must fail: %+v", col.checks)
		}
	})

	t.Run("not writable", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root ignores the permission bits this case relies on")
		}
		root := t.TempDir()
		readonly := filepath.Join(root, "ro")
		if err := os.Mkdir(readonly, 0o500); err != nil {
			t.Fatal(err)
		}
		cfg := defaultConfig(t)
		cfg.Review.WorkDir = readonly
		a := &App{Config: cfg, Log: quietLogger()}

		col := &checkCollector{}
		a.checkWorkdir(col)
		if len(col.checks) != 1 || col.checks[0].Status != StatusFail {
			t.Fatalf("a read-only workdir must fail: %+v", col.checks)
		}
	})
}

func TestProjectKey(t *testing.T) {
	t.Parallel()
	if got := projectKey("42"); got != "42" {
		t.Errorf("a numeric id must pass through, got %q", got)
	}
	// A path has to be escaped or the slashes become extra API path segments.
	if got := projectKey("backend/payments"); got != "backend%2Fpayments" {
		t.Errorf("projectKey = %q", got)
	}
}

// TestPoolSizeCoversTheWorkerPools: a running review pins two connections
// besides the one it works on — the review advisory lock for its whole run, and
// the git mirror lock while it fetches. Leaving the pool at its default meant a
// raised review.max_parallel could starve the very queue it was raised for.
func TestPoolSizeCoversTheWorkerPools(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	cfg.Review.MaxParallel = 12
	cfg.Jobs.Queues.Default = 2
	cfg.Jobs.Queues.Publish = 1
	cfg.Jobs.Queues.Slack = 1

	a := &App{Config: cfg, Log: quietLogger()}
	// Three per review — work, the review advisory lock, the git mirror lock —
	// plus the three other pools, River's notifier, leader election and a margin.
	if got, want := a.poolSize(), int32(3*12+2+1+1+4); got != want {
		t.Errorf("poolSize = %d, want %d", got, want)
	}

	// The default configuration must not shrink the pool below what
	// postgres.New would have given it on its own.
	small := &App{Config: defaultConfig(t), Log: quietLogger()}
	if got := small.poolSize(); got != basePoolSize {
		t.Errorf("poolSize with the defaults = %d, want the %d floor", got, basePoolSize)
	}

	// And a mis-set worker count must not ask a stock PostgreSQL for more
	// connections than it has.
	huge := testConfig(t)
	huge.Review.MaxParallel = 10_000
	if got := (&App{Config: huge, Log: quietLogger()}).poolSize(); got != maxPoolSize {
		t.Errorf("poolSize = %d, want the %d cap", got, maxPoolSize)
	}
}
