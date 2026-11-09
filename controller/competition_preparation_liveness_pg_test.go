// An independent source transaction must not serialize the watchdog's reads.
package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Keep the real advisory lock held until the monitor independently observes
// the already-activated fixture and refuses it for the correct lifecycle reason.
func TestCompetitionPreparationLivenessPgObservesLifecycleWithScopeHeld(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newCompetitionAutomationFixture(t)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-preparation-monitor", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := fixture.store.CurrentSourceActivation(ctx, fixture.settings)
		if err != nil {
			t.Fatal(err)
		}
		held, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		var releaseOnce sync.Once
		unlock := func() { releaseOnce.Do(func() { close(release) }) }
		defer func() {
			unlock()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		go func() {
			done <- captureDatabaseError(func() {
				server.MaintenanceTx(ctx, func(tx server.PgTx) {
					automationScopeLockInTx(ctx, tx, fixture.settings)
					close(held)
					select {
					case <-release:
					case <-ctx.Done():
						panic(ctx.Err())
					}
				}, server.OptNoRetry())
			})
		}()
		select {
		case <-held:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		err = checkCompetitionPreparationLiveness(ctx, fixture.settings, CompetitionPreparationRequest{Operation: "propagate_and_qualify", Intent: *intent, Lease: *lease})
		if !errors.Is(err, ErrConflict) {
			t.Fatal("monitor waited on source scope or missed activation", err)
		}
	})
}
