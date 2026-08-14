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
	rt, err := a.Runtime(ctx)
	if err != nil {
		return err
	}

	ln := launcher.New(
		launcher.WithName(AppName),
		launcher.WithVersion(version.String()),
		launcher.WithLogger(a.Log),
		launcher.WithContext(ctx),
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
