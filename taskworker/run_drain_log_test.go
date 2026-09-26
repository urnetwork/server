package taskworker

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// A blocked journal writer must not gate task drain, final handback or the
// serving-context cancellation that makes the old container exit.
func TestRunShutdownDoesNotWaitForLogSink(t *testing.T) {
	for key, value := range map[string]string{
		"WARP_ENV": "test", "WARP_VERSION": "2026.1.1+1",
		"WARP_HOST": "fixture.example", "WARP_SERVICE": "taskworker", "WARP_BLOCK": "g1",
		"WARP_HOST_IPV4": "192.0.2.10", "WARP_HOST_IPV6": "", "WARP_PORTS": "8080:8080",
	} {
		t.Setenv(key, value)
	}
	t.Cleanup(router.SetWarpStatusReady)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	releaseLog := make(chan struct{})
	defer close(releaseLog)
	logEntered := make(chan struct{}, 4)
	runtime := &taskworkerLifecycleRuntime{
		drainStarted: make(chan struct{}),
		handbackDone: make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		done <- runWithDependenciesAndDrainLogger(
			ctx,
			RunOptions{Port: 8080, Count: 1, BatchSize: 1},
			func(context.Context) error { return nil },
			func(context.Context) func() { return func() {} },
			func(runCtx context.Context, _ string, _ http.Handler, _ bool, _ server.HttpServerOptions) error {
				cancel()
				<-runCtx.Done()
				return nil
			},
			func(context.Context, context.CancelFunc, RunOptions) taskworkerRuntime { return runtime },
			func(string, ...any) {
				logEntered <- struct{}{}
				<-releaseLog
			},
		)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("taskworker shutdown waited for a blocked log sink")
	}
	select {
	case <-logEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic blocked shutdown log was never exercised")
	}
	if runtime.stage.Load() != 2 {
		t.Fatal("drain and final handback did not complete")
	}
}
