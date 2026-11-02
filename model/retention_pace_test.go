package model

import (
	"context"
	"testing"
	"time"
)

// Pure: an unpaced batch never waits; a paced one waits for its rate, with
// each batch held to the rate it is given; and a pace stops instead of
// waiting past its deadline or a canceled context.
func TestRetentionPace(t *testing.T) {
	ctx := context.Background()
	if !newRetentionPace().WaitAfter(ctx, 1000000, 0, time.Now()) {
		t.Fatal("an unpaced loop stopped")
	}

	pace := newRetentionPace()
	start := time.Now()
	if !pace.WaitAfter(ctx, 10, 100, start.Add(time.Minute)) {
		t.Fatal("a paced loop stopped inside its deadline")
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("10 rows at 100 a second waited %v, want about 100ms", elapsed)
	}
	// the next batch is held to its own, lower rate
	start = time.Now()
	if !pace.WaitAfter(ctx, 10, 50, start.Add(time.Minute)) {
		t.Fatal("a paced loop stopped inside its deadline")
	}
	if elapsed := time.Since(start); elapsed < 180*time.Millisecond {
		t.Fatalf("10 rows at 50 a second waited %v, want about 200ms", elapsed)
	}

	if newRetentionPace().WaitAfter(ctx, 1000, 100, time.Now().Add(time.Second)) {
		t.Fatal("a loop waited past its deadline")
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if newRetentionPace().WaitAfter(canceled, 10, 100, time.Now().Add(time.Minute)) {
		t.Fatal("a loop waited through a canceled context")
	}
}
