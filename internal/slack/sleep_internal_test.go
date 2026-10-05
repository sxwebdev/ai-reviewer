package slack

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSleepCtx covers the real Retry-After wait, which every other test
// replaces with a recording stub.
func TestSleepCtx(t *testing.T) {
	t.Parallel()

	if err := sleepCtx(t.Context(), 0); err != nil {
		t.Errorf("sleepCtx(0) = %v, want nil", err)
	}

	start := time.Now()
	if err := sleepCtx(t.Context(), 20*time.Millisecond); err != nil {
		t.Errorf("sleepCtx = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("returned after %v, want at least 20ms", elapsed)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleepCtx on a cancelled context = %v, want context.Canceled", err)
	}
}
