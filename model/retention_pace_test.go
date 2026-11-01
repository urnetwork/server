package model

import (
	"context"
	"testing"
	"time"
)

// Pure: an unpaced loop never waits; a paced one waits for its rate, and stops
// instead of waiting past its deadline or a canceled context.
func TestRetentionPace(t *testing.T) {
	ctx := context.Background()
	if !newRetentionPace(0).WaitAfter(ctx, 1000000, time.Now()) {
		t.Fatal("an unpaced loop stopped")
	}

	pace := newRetentionPace(100)
	start := time.Now()
	if !pace.WaitAfter(ctx, 10, start.Add(time.Minute)) {
		t.Fatal("a paced loop stopped inside its deadline")
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("10 rows at 100 a second waited %v, want about 100ms", elapsed)
	}

	if newRetentionPace(100).WaitAfter(ctx, 1000, time.Now().Add(time.Second)) {
		t.Fatal("a loop waited past its deadline")
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if newRetentionPace(100).WaitAfter(canceled, 10, time.Now().Add(time.Minute)) {
		t.Fatal("a loop waited through a canceled context")
	}
}
