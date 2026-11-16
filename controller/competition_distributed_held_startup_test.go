// The real distributed child must start its truthful idle owner while claims
// are held, rather than skipping preflight into deadline-only reconciliation.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The wrong outer branch announces itself without a sleep or timeout oracle.
type competitionHeldStartupStore struct {
	*competitionIntakeHeldWorkerStore
	expiredOnly chan struct{}
}

// No held startup may turn into a deadline-only claim before host preflight.
func (self *competitionHeldStartupStore) Claim(ctx context.Context, settings *Settings, workerId, imageDigest string) (*queuedJob, error) {
	if settings.executorReconcileExpiredOnly {
		self.expiredOnly <- struct{}{}
		return nil, ErrAutomationHeld
	}
	return self.competitionIntakeHeldWorkerStore.Claim(ctx, settings, workerId, imageDigest)
}

// Positive barriers exercise the composed coordinator entry and actual worker
// heartbeat loop. The old outer condition deterministically takes the tripwire.
func TestCompetitionDistributedHeldStartupStartsHostHeartbeat(t *testing.T) {
	settings, source, _ := newAutomationStagingApiFixture(t)
	coordinator := defaultCompetitionDistributedCoordinatorSettings()
	coordinator.PollInterval = time.Hour
	settings.distributedCoordinator = &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{Settings: coordinator}}
	store := &competitionHeldStartupStore{
		competitionIntakeHeldWorkerStore: &competitionIntakeHeldWorkerStore{
			automationStagingApiStore: source,
			sealHeld:                  true,
			observed:                  make(chan struct{}, 8),
			registered:                make(chan struct{}, 4),
		},
		expiredOnly: make(chan struct{}, 1),
	}
	evaluator := &fakeEvaluator{check: passingHostCheck(settings)}
	worker, err := newWorkerWithImageDigest(settings, store, evaluator, "synthetic-held-coordinator", testWorkerImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	worker.pollTickForTest = make(chan time.Time)
	heartbeat := make(chan time.Time)
	worker.hostTickForTest = heartbeat
	agent := &competitionAgentEvaluator{host: CompetitionExecutorHostPolicy{HostId: "executor.example"}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- runCompetitionExecutorWorker(ctx, worker, agent)
	}()
	defer func() {
		cancel()
		for range done {
		}
	}()
	select {
	case <-store.registered:
	case <-store.expiredOnly:
		t.Fatal("grading hold skipped host preflight into deadline-only reconciliation")
	case err := <-done:
		t.Fatal("held coordinator exited before host preflight", err)
	}
	select {
	case heartbeat <- time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC):
	case err := <-done:
		t.Fatal("held coordinator exited before its heartbeat", err)
	}
	select {
	case <-store.registered:
	case err := <-done:
		t.Fatal("held coordinator did not acknowledge a fresh host check", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("held coordinator did not join on cancellation", err)
	}
	if evaluator.calls != 0 || store.claims != 0 {
		t.Fatal("held coordinator admitted evaluation work", evaluator.calls, store.claims)
	}
}
