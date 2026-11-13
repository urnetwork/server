// Fixture-only lifecycle controls distinguish canceled test work from the
// independent join watchdog. Positive completion remains reusable and no
// running worker is considered joined merely because cancellation was sent.
package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
)

// Go cancels t.Context before cleanup. The real cleanup edge must leave the
// watchdog live even after repeated result reads and explicit prior cleanup.
func TestCompetitionIdleHostHeartbeatFixtureResultCleanupKeepsCompletedOutcome(t *testing.T) {
	fixture := newCompetitionIdleTestFixture(t, nil, nil)
	entered := make(chan struct{})
	fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	fixture.start()
	competitionIdleReceive(t, fixture, fixture.store.registrations)
	competitionIdleReceive(t, fixture, entered)
	fixture.cancel()
	for index := 0; index < 2; index++ {
		if !errors.Is(fixture.result(t), context.Canceled) {
			t.Fatal("repeated result read lost the original worker cancellation")
		}
	}
	if err := fixture.stopAndJoin(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !errors.Is(t.Context().Err(), context.Canceled) {
			t.Error("regression did not exercise the actual canceled test cleanup edge")
		}
		if fixture.watch.Err() != nil {
			t.Error("fixture join watchdog inherited test cleanup cancellation")
		}
		if err := fixture.stopAndJoin(); err != nil {
			t.Error(err)
			return
		}
		if !errors.Is(fixture.result(t), context.Canceled) || !fixture.joined.Load() {
			t.Error("cleanup consumed or replaced the completed worker outcome")
		}
	})
}

// No result read or body cancellation precedes this genuine cleanup-only path.
// The canceled test stops the worker and cleanup still physically joins it.
func TestCompetitionIdleHostHeartbeatFixtureCleanupOnlyJoinsCanceledTestWorker(t *testing.T) {
	fixture := newCompetitionIdleTestFixture(t, nil, nil)
	entered, claimReturned := make(chan struct{}), make(chan struct{})
	fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) {
		defer close(claimReturned)
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	fixture.start()
	competitionIdleReceive(t, fixture, fixture.store.registrations)
	competitionIdleReceive(t, fixture, entered)
	t.Cleanup(func() {
		if !errors.Is(t.Context().Err(), context.Canceled) || fixture.watch.Err() != nil {
			t.Error("cleanup-only join did not retain its independent watchdog")
		}
		if err := fixture.stopAndJoin(); err != nil {
			t.Error(err)
			return
		}
		select {
		case <-claimReturned:
		default:
			t.Error("cleanup returned before the exact blocked claim returned")
		}
		if !fixture.joined.Load() || !errors.Is(fixture.result(t), context.Canceled) {
			t.Error("cleanup-only path lost physical completion or the original error")
		}
	})
}

// Cancellation has a positive acknowledgement, but the actual claim is held
// until release. Quiescence, not elapsed time, proves cleanup has not joined.
func TestCompetitionIdleHostHeartbeatFixtureCleanupWaitsForPhysicalWorkerReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionIdleTestFixture(t, nil, nil)
		entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil, ctx.Err()
		}
		fixture.start()
		competitionIdleReceive(t, fixture, fixture.store.registrations)
		competitionIdleReceive(t, fixture, entered)
		joinReturned := make(chan struct{})
		var joinErr error
		go func() {
			defer close(joinReturned)
			joinErr = fixture.stopAndJoin()
		}()
		t.Cleanup(func() {
			releaseOnce.Do(func() { close(release) })
			competitionIdleReceive(t, fixture, joinReturned)
		})
		competitionIdleReceive(t, fixture, canceled)
		synctest.Wait()
		select {
		case <-joinReturned:
			t.Fatal("fixture cleanup treated sent cancellation as physical completion")
		default:
		}
		if fixture.joined.Load() {
			t.Fatal("fixture published completion while the exact claim was still held")
		}
		releaseOnce.Do(func() { close(release) })
		competitionIdleReceive(t, fixture, joinReturned)
		if joinErr != nil || !errors.Is(fixture.result(t), context.Canceled) {
			t.Fatal("physical release failed to complete the retained join", joinErr)
		}
	})
}

// A closed completion latch is stronger evidence than an already-canceled
// watchdog; two ready select cases must never randomly reject a proven join.
func TestCompetitionIdleHostHeartbeatFixtureCompletedWitnessWinsCanceledWatchdog(t *testing.T) {
	fixture := newCompetitionIdleTestFixture(t, nil, nil)
	entered := make(chan struct{})
	fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	fixture.start()
	competitionIdleReceive(t, fixture, fixture.store.registrations)
	competitionIdleReceive(t, fixture, entered)
	fixture.cancel()
	if !errors.Is(fixture.result(t), context.Canceled) {
		t.Fatal("synthetic worker did not first establish physical completion")
	}
	watch, cancelWatch := context.WithCancel(context.Background())
	cancelWatch()
	if err := fixture.awaitJoined(watch); err != nil {
		t.Fatal("an expired watchdog overruled an already-closed completion latch", err)
	}
}

// An expired watchdog still fails closed for a genuinely held child. It cannot
// cancel or finish unrelated work, and later explicit release remains joinable.
func TestCompetitionIdleHostHeartbeatFixtureExpiredWatchdogCannotCreditRunningWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionIdleTestFixture(t, nil, nil)
		entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil, ctx.Err()
		}
		fixture.start()
		competitionIdleReceive(t, fixture, fixture.store.registrations)
		competitionIdleReceive(t, fixture, entered)
		watch, cancelWatch := context.WithCancel(context.Background())
		cancelWatch()
		if err := fixture.awaitJoined(watch); !errors.Is(err, context.Canceled) || fixture.joined.Load() || fixture.ctx.Err() != nil {
			t.Fatal("watchdog cancellation fabricated worker completion or canceled its work", err)
		}
		fixture.cancel()
		competitionIdleReceive(t, fixture, canceled)
		synctest.Wait()
		if fixture.joined.Load() {
			t.Fatal("expired watchdog let the later worker cancellation escape physical join")
		}
		releaseOnce.Do(func() { close(release) })
		if err := fixture.stopAndJoin(); err != nil || !errors.Is(fixture.result(t), context.Canceled) {
			t.Fatal("held worker did not remain joinable after explicit release", err)
		}
	})
}
