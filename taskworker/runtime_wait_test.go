package taskworker

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// The release precheck of 2026-10-15: under -race,
// TestCapabilityProbeChainReturnsAfterAStaleTaskworkerEndsIt reset the st
// config while the stats collector of the runtime it had started still read
// it, since nothing could join that collector. Wait joins every goroutine the
// runtime started once its lifetime ends, for either profile: it returns, and
// the st config written after it is ordered after their reads, so the race
// detector reports nothing.
func TestTaskworkerRuntimeWaitJoinsItsObservers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
			lifetime, cancelLifetime := context.WithCancel(ctx)
			runtime, err := startTaskworkerRuntime(ctx, lifetime, cancelLifetime, RunOptions{Port: 8080, Count: 0, BatchSize: 1, WorkloadProfile: profile})
			if err != nil {
				cancelLifetime()
				t.Fatalf("%q runtime: %v", profile, err)
			}
			runtime.Drain()
			runtime.WaitFinalHandback()
			cancelLifetime()
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				runtime.(*taskworkerRuntimeOwner).Wait()
			}()
			select {
			case <-joined:
			case <-ctx.Done():
				t.Fatalf("%q runtime's goroutines did not return after its lifetime ended", profile)
			}
			controller.SetStConfig(&controller.StConfig{Enabled: false})
		}
	})
}
