package taskworker

import (
	"context"
	"testing"
)

func TestRunRejectsInvalidInputsBeforeEnvironmentAccess(t *testing.T) {
	if err := Run(nil, RunOptions{Port: 1, Count: 1, BatchSize: 1}); err == nil {
		t.Fatal("nil context was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 0, Count: 1, BatchSize: 1}); err == nil {
		t.Fatal("zero port was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 1, Count: 0, BatchSize: 1}); err == nil {
		t.Fatal("zero worker count was accepted")
	}
	if err := Run(context.Background(), RunOptions{Port: 1, Count: 1, BatchSize: 0}); err == nil {
		t.Fatal("zero batch size was accepted")
	}
}
