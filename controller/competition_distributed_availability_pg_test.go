// Reconcile-only admission is exercised against the real append's job/slot
// guards in a disposable database, never by widening an existing job's lease.
package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestCompetitionDistributedOccupiedPhysicalHostCannotStartFreshClock(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		fresh := fixture.addJob(tb, server.Id{15: 4}, fixture.clock.Now(), "physical-pending")
		selected := fixture.selected(0)
		selected.executorReconcileOnly = true
		if job, err := fixture.store.Claim(tb.Context(), selected, "synthetic-busy-host", testWorkerImageDigest()); err != nil || job != nil {
			tb.Fatal("occupied physical host started a previously queued job", err)
		}
		other, err := fixture.store.Claim(tb.Context(), fixture.selected(1), "synthetic-free-host", testWorkerImageDigest())
		if err != nil || other == nil || other.JobId != fixture.sourceJob.JobId {
			tb.Fatal("occupied sibling blocked another host's original FIFO claim", err)
		}
		if job, err := fixture.store.Claim(tb.Context(), selected, "synthetic-busy-host", testWorkerImageDigest()); err != nil || job != nil {
			tb.Fatal("occupied host borrowed its sibling's active execution", err)
		}
		pending, err := fixture.store.GetJob(tb.Context(), fixture.settings, fresh.JobId, &Principal{Id: "synthetic-operator", Role: "operator"})
		if err != nil || pending == nil || pending.StartedAt != nil || pending.AttemptCount != 0 || pending.ExecutionDeadline != nil {
			tb.Fatal("physical wait consumed a queued submission's clock or attempt", err)
		}
		selected.executorReconcileOnly = false
		first, err := fixture.store.Claim(tb.Context(), selected, "synthetic-now-free-host", testWorkerImageDigest())
		if err != nil || first == nil || first.JobId != fresh.JobId {
			tb.Fatal("positive physical admission could not start the pending FIFO job", err)
		}
		selected.executorReconcileOnly = true
		fixture.clock.Advance(2 * time.Minute)
		reconciled, err := fixture.store.Claim(tb.Context(), selected, "synthetic-successor-host", testWorkerImageDigest())
		if err != nil || reconciled == nil || reconciled.JobId != first.JobId || reconciled.AttemptCount != first.AttemptCount ||
			!reconciled.StartedAt.Equal(*first.StartedAt) || !reconciled.ExecutionDeadline.Equal(*first.ExecutionDeadline) || reconciled.SlotFencingToken <= first.SlotFencingToken {
			tb.Fatal("busy physical host could not reconcile exactly its old execution", err)
		}
	})
}
