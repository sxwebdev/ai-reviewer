package app

import (
	"context"
	"time"

	"github.com/tkcrm/mx/launcher"

	"github.com/sxwebdev/ai-reviewer/internal/version"
)

// globalShutdownTimeout bounds the whole shutdown sequence. It must exceed the
// jobs service's own window (jobs.drain_timeout + 5s) or the launcher would cut
// the drain short from the outside.
const globalShutdownTimeout = 90 * time.Second

// Start runs the production process: Postgres, the River workers and the mx ops
// server (/livez, /readyz, /metrics, and /debug/pprof when ops.profiler.enabled
// is set — mx leaves it off by default).
//
// Startup order is expressed as a priority, not as a code order: Postgres is in
// priority group 1 so it is connected and reporting readiness before the jobs
// service starts claiming work. Shutdown is the reverse (LIFO), so the pool
// outlives the workers draining through it — the opposite order would fail
// every in-flight job at the last write.
func (a *App) Start(ctx context.Context) error {
	// Before anything is connected or registered: a workdir the reviews cannot use
	// does not fail them, it silently makes every one of them worse (see
	// checkAgentWorkdir). The check lives here rather than in Runtime on purpose —
	// `doctor` has to finish its checklist and print a verdict instead of dying on
	// the first problem, and `review --local` is a one-off debugging run where
	// degrading is acceptable.
	if err := checkAgentWorkdir(a.Config.LLM.Claude.AgentMode, a.Config.Review.WorkDir); err != nil {
		return err
	}

	a.logEffectiveMode()

	rt, err := a.Runtime(ctx)
	if err != nil {
		return err
	}

	ln := launcher.New(
		launcher.WithName(AppName),
		launcher.WithVersion(version.String()),
		launcher.WithLogger(a.Log),
		launcher.WithContext(ctx),
		// ShutdownContext owns the signals for the whole process (see the comment
		// there). Leaving mx's handler on as well is not redundancy: both channels
		// receive every signal, the launcher arms its force-exit watcher only when
		// its own channel wins the race, and the first registration has already
		// disabled the default disposition — so the losing case is a process that
		// cannot be interrupted at all.
		launcher.WithSignal(false),
		launcher.WithAppStartStopLog(true),
		launcher.WithGlobalShutdownTimeout(globalShutdownTimeout),
		launcher.WithRunnerServicesSequence(launcher.RunnerServicesSequenceLifo),
		launcher.WithOpsConfig(a.Config.Ops),
	)

	ln.ServicesRunner().Register(
		// Postgres is also an mx health checker, so registering it here is what
		// puts the pool's Ping behind /readyz. WithService is duck-typed and
		// picks that up from the same value.
		launcher.NewService(
			launcher.WithService(rt.Postgres),
			launcher.WithStartupPriority(1),
		),
		// The per-service shutdown default is 10s, which would cut River's
		// SoftStopTimeout (jobs.drain_timeout) short and hard-cancel jobs that
		// were about to finish. The margin covers the client's own teardown
		// after the last job returns.
		launcher.NewService(
			launcher.WithService(rt.Jobs),
			launcher.WithShutdownTimeout(a.Config.Jobs.DrainTimeout+5*time.Second),
		),
	)

	return ln.Run()
}

// logEffectiveMode states, once at startup, which switches decide whether this
// process spends money and whether it writes anywhere.
//
// It exists because the first live run raised a question the log could not answer:
// "why is it reviewing — I never enabled that?". Reviewing is per team
// (`teams[].ai_review.enabled`) while publishing is global
// (`service.ai_review_publish_enabled`), and the second being off does not stop the
// first: a dry run costs exactly the same tokens as a published one and produces
// nothing visible in GitLab. Both, plus the names of the teams they apply to, now
// appear in the first three lines of the log.
func (a *App) logEffectiveMode() {
	var reviewing, idle []string
	for _, t := range a.Config.Teams {
		if t.AIReview.Enabled {
			reviewing = append(reviewing, t.Name)
			continue
		}
		idle = append(idle, t.Name)
	}

	a.Log.Infow("effective mode",
		"ai_review_teams", reviewing,
		"digest_only_teams", idle,
		"publish_findings", a.Config.Service.AIReviewPublishEnabled,
		"send_to_slack", a.Config.Service.SlackSendEnabled,
		"agent_mode", a.Config.LLM.Claude.AgentMode,
		"workdir", a.Config.Review.WorkDir,
		"model", a.Config.LLM.Claude.Model,
	)
	if len(reviewing) > 0 && !a.Config.Service.AIReviewPublishEnabled {
		// Not a warning about a misconfiguration — dry-run first is the documented
		// way in. A warning about the bill: this combination pays for every review
		// and posts none of them.
		a.Log.Warnw("reviews will run but nothing will be published to GitLab",
			"teams", reviewing,
			"turn_publishing_on", "service.ai_review_publish_enabled",
			"turn_reviewing_off", "teams[].ai_review.enabled")
	}
}
