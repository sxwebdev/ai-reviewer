package app

import (
	"context"
	"os"
	"os/signal"
	"sync"

	"github.com/tkcrm/mx/launcher"
	"github.com/tkcrm/mx/logger"
)

// forceExitCode is the status a forced exit reports. Non-zero on purpose: the
// shutdown did not finish, in-flight work was abandoned where it stood, and a
// supervisor must not read that as a clean stop. It is the code mx's own
// force-exit paths use, so the process has one "was killed" code rather than two.
const forceExitCode = 1

// ShutdownContext returns a context cancelled by the first shutdown signal and
// forces the process to exit when a second one arrives.
//
// This process has exactly one signal owner, and it is this function. That is not
// a style preference. os/signal delivers a signal to EVERY registered channel,
// and the first registration anywhere in the process also removes the default
// disposition — from that moment SIGINT no longer kills anything by itself. So a
// second handler does not add a fallback, it takes away the only one there was:
// mx's launcher arms its own "send signal again to force exit" watcher solely in
// the branch where its channel won the race against our context cancellation, and
// when ours won instead nothing was armed while the terminal's Ctrl-C had already
// stopped working. Measured on the first live run: thirteen Ctrl-C during a
// 60-second drain, no effect at all. launcher.WithSignal(false) in Start is the
// other half of this rule.
func ShutdownContext(parent context.Context, log logger.Logger) (context.Context, context.CancelFunc) {
	return shutdownContext(parent, log, signal.Notify, signal.Stop, func() { os.Exit(forceExitCode) })
}

// shutdownContext is ShutdownContext with the three process-global operations
// injected, so a test can drive it without signalling — or exiting — the test
// binary.
func shutdownContext(
	parent context.Context,
	log logger.Logger,
	notify func(chan<- os.Signal, ...os.Signal),
	stopNotify func(chan<- os.Signal),
	exit func(),
) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	// Two slots: the signal that starts the shutdown, and the one that forces the
	// exit. A larger buffer would only queue signals nobody ever reads — which is
	// precisely the failure this function exists to remove.
	ch := make(chan os.Signal, 2)
	notify(ch, launcher.ShutdownSiganl()...)

	release := make(chan struct{})
	go func() {
		// Restoring the default disposition on the way out matters: after this
		// returns, a signal kills the process again instead of vanishing.
		defer stopNotify(ch)

		select {
		case sig := <-ch:
			log.Infow("shutdown signal received: draining, send it again to exit immediately",
				"signal", sig.String())
			cancel()
		case <-ctx.Done():
			// The parent was cancelled, or the returned stop func ran. Nothing to
			// force; the caller is already on its way out.
			return
		case <-release:
			return
		}

		select {
		case sig := <-ch:
			log.Warnw("second shutdown signal received: exiting now, in-flight work is abandoned",
				"signal", sig.String())
			exit()
		case <-release:
		}
	}()

	var once sync.Once
	return ctx, func() {
		// Idempotent: main defers this and the CLI may call it too, and closing a
		// closed channel would take the process down with a panic on the way out.
		once.Do(func() { close(release) })
		cancel()
	}
}
