package connect

import (
	"context"
	"testing"
	"time"
)

type cleanupContextKey struct{}

func TestConnectionCleanupContextIsDetachedAndBounded(t *testing.T) {
	parent, parentCancel := context.WithCancel(
		context.WithValue(context.Background(), cleanupContextKey{}, "preserved"),
	)
	parentCancel()

	start := time.Now()
	ctx, cancel := connectionCleanupContext(parent)
	defer cancel()

	if err := ctx.Err(); err != nil {
		t.Fatalf("cleanup inherited transport cancellation: %v", err)
	}
	if got := ctx.Value(cleanupContextKey{}); got != "preserved" {
		t.Fatalf("cleanup lost parent values: got %v", got)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("cleanup context has no deadline")
	}
	remaining := deadline.Sub(start)
	if remaining <= 0 || connectionCleanupTimeout+10*time.Millisecond < remaining {
		t.Fatalf("cleanup deadline outside expected bound: %s", remaining)
	}
}
