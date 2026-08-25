package app

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

// signalStub stands in for os/signal: it hands back the channel the code under
// test registered, so a test can deliver signals without signalling the test
// binary — and records that the default disposition was restored.
type signalStub struct {
	ch      chan<- os.Signal
	stopped chan struct{}
}

func newSignalStub() *signalStub {
	return &signalStub{stopped: make(chan struct{}, 1)}
}

func (s *signalStub) notify(ch chan<- os.Signal, _ ...os.Signal) { s.ch = ch }
func (s *signalStub) stop(chan<- os.Signal)                      { s.stopped <- struct{}{} }

// send delivers one signal, failing the test rather than blocking forever if
// nothing is listening — a full channel is exactly the bug this file exists for.
func (s *signalStub) send(t *testing.T, sig os.Signal) {
	t.Helper()
	select {
	case s.ch <- sig:
	case <-time.After(2 * time.Second):
		t.Fatal("nobody is reading the signal channel")
	}
}

func waitFor(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestShutdownContextCancelsOnFirstSignal(t *testing.T) {
	stub := newSignalStub()
	exited := make(chan struct{}, 1)

	ctx, stop := shutdownContext(context.Background(), quietLogger(), stub.notify, stub.stop,
		func() { exited <- struct{}{} })
	defer stop()

	stub.send(t, syscall.SIGTERM)
	waitFor(t, "the context to be cancelled", ctx.Done())

	select {
	case <-exited:
		t.Fatal("the first signal must drain, not exit")
	case <-time.After(50 * time.Millisecond):
	}
}

// The whole point of owning the signals: after the first one the default
// disposition is gone, so if nothing acts on the second, the process can no
// longer be interrupted at all. That is what happened on the first live run —
// thirteen Ctrl-C, no effect.
func TestShutdownContextForcesExitOnSecondSignal(t *testing.T) {
	stub := newSignalStub()
	exited := make(chan struct{}, 1)

	ctx, stop := shutdownContext(context.Background(), quietLogger(), stub.notify, stub.stop,
		func() { exited <- struct{}{} })
	defer stop()

	stub.send(t, syscall.SIGINT)
	waitFor(t, "the context to be cancelled", ctx.Done())

	stub.send(t, syscall.SIGINT)
	waitFor(t, "the forced exit", exited)
}

func TestShutdownContextStopIsIdempotentAndRestoresTheDefault(t *testing.T) {
	stub := newSignalStub()
	exited := make(chan struct{}, 1)

	ctx, stop := shutdownContext(context.Background(), quietLogger(), stub.notify, stub.stop,
		func() { exited <- struct{}{} })

	// Twice, because main defers it and a caller may stop early: closing a closed
	// channel would take the process down on the way out.
	stop()
	stop()

	waitFor(t, "the context to be cancelled", ctx.Done())
	waitFor(t, "signal.Stop", stub.stopped)

	select {
	case <-exited:
		t.Fatal("stop() must not force an exit")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestShutdownContextFollowsItsParent(t *testing.T) {
	stub := newSignalStub()
	parent, cancelParent := context.WithCancel(context.Background())

	ctx, stop := shutdownContext(parent, quietLogger(), stub.notify, stub.stop,
		func() { t.Error("a cancelled parent is not a signal; nothing may be forced") })
	defer stop()

	cancelParent()
	waitFor(t, "the derived context to be cancelled", ctx.Done())
	waitFor(t, "signal.Stop", stub.stopped)
}
