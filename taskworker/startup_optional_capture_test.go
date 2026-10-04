// A release join must preserve upstream optional capture isolation while the
// bounded dependency owner continues to acquire one real primary runtime.
package taskworker

import (
	"context"
	"testing"
	"time"
)

func TestTaskworkerStartupOptionalCaptureRefusalKeepsPrimaryOwner(t *testing.T) {
	taskworkerStartupTestEnvironment(t)
	// The actual config parser rejects this relative source before any I/O.
	t.Setenv("ARIN_SHADOW_CAPTURE_CONFIG", "synthetic-relative-capture.json")
	owner, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	runtime := &taskworkerLifecycleRuntime{drainStarted: make(chan struct{}), handbackDone: make(chan struct{})}
	reads, starts, metrics, waits := 0, 0, 0, 0
	result := startTaskworkerAfterReadiness(owner, owner, cancel, RunOptions{Port: 8080, Count: 1, BatchSize: 1, WorkloadProfile: WorkloadProfileSubnetOperator},
		func(context.Context) error { reads++; return nil },
		func(context.Context) func() { metrics++; return func() {} },
		func(context.Context, context.Context, context.CancelFunc, RunOptions) (taskworkerRuntime, error) {
			starts++
			return runtime, nil
		},
		func(context.Context, time.Duration) error { waits++; return nil },
	)
	if result.closeCapture != nil {
		result.closeCapture()
	}
	if result.err != nil || result.worker != runtime || reads != 1 || starts != 1 || metrics != 1 || waits != 0 || owner.Err() != nil {
		t.Fatal("optional capture refusal gated or restarted primary worker admission", result.err, reads, starts, metrics, waits)
	}
}
