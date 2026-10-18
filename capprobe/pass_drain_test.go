// Buffered negative evidence spends one flush grace, even when the pass is
// canceled while the first negotiated reading is being stored.
package capprobe

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Serial measurements force several waves of negatives to finish before the
// last provider negotiates and releases the complete accumulated backlog.
func newReleasedEvidencePass(t *testing.T) (*passRunner, *fakeStore, *fakeFleet) {
	t.Helper()
	store := newFakeStore(5)
	fleet := newFakeFleet()
	for _, clientId := range store.clientIds {
		fleet.providers[clientId] = fakeLegacyProvider()
	}
	fleet.providers[store.clientIds[len(store.clientIds)-1]] = fakeModernProvider()
	return newFakeRunner(t, fakeSettings(1, 5, 5, 1), store, fleet), store, fleet
}

// A successful first write consumes part of the grace; the following stalled
// write gets only its remainder. A fresh per-item timeout would extend it.
func TestPassReleasedEvidenceSharesCancellationGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, store, fleet := newReleasedEvidencePass(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered := make(chan struct{})
		release := make(chan struct{})
		calls := 0
		var deadline time.Time
		runner.record = func(recordCtx context.Context, clientId server.Id, evidence model.ProviderModernEvidence, confirmed bool) (bool, error) {
			calls++
			if calls == 1 {
				deadline, _ = recordCtx.Deadline()
				close(entered)
				<-release
				time.Sleep(6 * time.Second)
				return store.record(recordCtx, clientId, evidence, confirmed)
			}
			if nextDeadline, _ := recordCtx.Deadline(); !nextDeadline.Equal(deadline) {
				t.Errorf("released evidence restarted its grace: %s, want %s", nextDeadline, deadline)
			}
			<-recordCtx.Done()
			return false, recordCtx.Err()
		}
		sweep := Sweep{StartedAt: time.Now()}
		done := make(chan struct{})
		var next Sweep
		var result *PassResult
		var err error
		go func() {
			next, result, err = runner.run(ctx, sweep)
			close(done)
		}()
		<-entered
		canceledAt := time.Now()
		cancel()
		close(release)
		<-done
		if !errors.Is(err, context.DeadlineExceeded) || calls != 2 || result.Recorded != 1 {
			t.Fatalf("drain calls=%d result=%+v err=%v, want one success then one timed-out write", calls, result, err)
		}
		if elapsed := time.Since(canceledAt); elapsed != recordTimeout {
			t.Fatalf("cancellation drain took %s, want one %s grace", elapsed, recordTimeout)
		}
		if next.Cursor != sweep.Cursor || !next.StartedAt.Equal(sweep.StartedAt) || fleet.openTunnels() != 0 {
			t.Fatal("failed drain advanced past unwritten evidence or retained tunnels", next)
		}
	})
}

// The first write failure ends the released backlog immediately, preserving
// the original error and leaving every unrecorded provider for the retry.
func TestPassReleasedEvidenceStopsAfterWriteFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, _, _ := newReleasedEvidencePass(t)
		writeErr := errors.New("synthetic released-evidence write failure")
		calls := 0
		runner.record = func(context.Context, server.Id, model.ProviderModernEvidence, bool) (bool, error) {
			calls++
			return false, writeErr
		}
		sweep := Sweep{StartedAt: time.Now()}
		next, result, err := runner.run(t.Context(), sweep)
		if calls != 1 || !errors.Is(err, writeErr) || result.Recorded != 0 || next.Cursor != sweep.Cursor {
			t.Fatalf("failed flush continued: calls=%d next=%+v result=%+v err=%v", calls, next, result, err)
		}
	})
}

// A commit can succeed just as its context expires. The next item must fail
// admission locally, without invoking a store that might ignore cancellation.
func TestPassReleasedEvidenceDoesNotStartAfterGraceExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, store, _ := newReleasedEvidencePass(t)
		calls := 0
		runner.record = func(recordCtx context.Context, clientId server.Id, evidence model.ProviderModernEvidence, confirmed bool) (bool, error) {
			calls++
			<-recordCtx.Done()
			return store.record(recordCtx, clientId, evidence, confirmed)
		}
		_, result, err := runner.run(t.Context(), Sweep{})
		if calls != 1 || !errors.Is(err, context.DeadlineExceeded) || result.Recorded != 1 || result.SweepCompleted {
			t.Fatalf("expired flush started another write or skipped replay: calls=%d result=%+v err=%v", calls, result, err)
		}
	})
}

// Cancellation does not discard a released batch that fits in its one grace.
func TestPassReleasedEvidenceCompletesInsideSharedGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, store, fleet := newReleasedEvidencePass(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		runner.record = func(recordCtx context.Context, clientId server.Id, evidence model.ProviderModernEvidence, confirmed bool) (bool, error) {
			calls++
			if calls == 1 {
				cancel()
			}
			time.Sleep(time.Second)
			if err := recordCtx.Err(); err != nil {
				return false, err
			}
			return store.record(recordCtx, clientId, evidence, confirmed)
		}
		_, result, err := runner.run(ctx, Sweep{})
		if err != nil || calls != len(store.clientIds) || result.Recorded != len(store.clientIds) || fleet.openTunnels() != 0 {
			t.Fatalf("bounded healthy drain lost evidence: calls=%d result=%+v err=%v", calls, result, err)
		}
	})
}
