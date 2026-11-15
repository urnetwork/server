package model

import (
	"context"
	"testing"
)

// V10 owns inline mirror publication and has no durable mirror task target.
func legacyTargetDrainMirrorOwners(t testing.TB, ctx context.Context, expected int) (int, int) {
	t.Helper()
	if expected != 0 || legacyTargetMirrorQueueCount(ctx) != 0 {
		t.Fatal("V10 profile unexpectedly contains durable mirror owners")
	}
	return 0, 0
}
