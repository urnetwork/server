// Lost acknowledgements and parent restarts recognize only an already-committed
// expired failure. No retry can resume an intermediate lease or consume FIFO.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The reply is lost only after both the terminal row and its events commit.
func TestExpiredControlBridgeLostCommitAcknowledgementReadsTerminal(t *testing.T) {
	worker, evaluator, store, transport, record, _ := newCompetitionExpiredBridgeFixture(t)
	store.claimAckError = errors.New("synthetic commit acknowledgement lost")
	occupied, err := worker.reconcileExpiredControl(t.Context(), evaluator)
	if !errors.Is(err, ErrAutomationHeld) || !occupied || store.claims != 1 || store.completions != 1 || store.job != nil || store.terminal == nil ||
		store.terminal.SlotFencingToken != record.SlotFencingToken+1 || transport.calls != 1 || transport.forbidden != 0 || store.queued != 5 {
		t.Fatal("lost acknowledgement reissued work or stranded an intermediate lease", occupied, err)
	}
}

// The short observation and transaction-local lease are historical evidence,
// not an expiring permission to acknowledge a durable terminal outcome.
func TestExpiredControlBridgeRestartAfterLeaseExpiryIsIdempotent(t *testing.T) {
	worker, evaluator, store, transport, record, _ := newCompetitionExpiredBridgeFixture(t)
	if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err != nil {
		t.Fatal(err)
	}
	store.now = store.now.Add(3 * time.Minute)
	worker.workerId = "synthetic-restarted-bridge-worker"
	lease := *worker.settings.automationLease
	lease.OwnerId, lease.FencingToken = "synthetic-restarted-bridge-parent", lease.FencingToken+1
	worker.settings.automationLease = &lease
	store.liveSupervisorOwner, store.liveSupervisorFence = lease.OwnerId, lease.FencingToken
	occupied, err := worker.reconcileExpiredControl(t.Context(), evaluator)
	if !errors.Is(err, ErrAutomationHeld) || !occupied || store.claims != 1 || store.completions != 1 || transport.calls != 1 || store.queued != 5 ||
		store.terminal.SlotFencingToken != record.SlotFencingToken+1 || !store.terminal.ExecutionDeadline.Equal(record.Identity.DeadlineAt) {
		t.Fatal("current held parent could not acknowledge the exact historical terminal receipt", occupied, err)
	}
}

// A later valid operator timeout change cannot retroactively invalidate the
// exact bounds under which an already-terminal transaction was authenticated.
func TestExpiredControlBridgeTerminalProofRetainsHistoricalBounds(t *testing.T) {
	worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
	evaluator.now = func() time.Time { return store.now }
	transport.after = func() { store.now = store.now.Add(90 * time.Second) }
	if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err != nil {
		t.Fatal(err)
	}
	worker.settings.distributedCoordinator.Config.Settings.RequestTimeout = 80 * time.Second
	worker.settings.distributedCoordinator.Config.Settings.ExistingWitnessMaximumAge = 2 * time.Second
	if occupied, err := worker.reconcileExpiredControl(t.Context(), evaluator); !occupied || !errors.Is(err, ErrAutomationHeld) ||
		store.claims != 1 || store.completions != 1 || transport.calls != 1 || store.queued != 5 {
		t.Fatal("current settings retimed a completed historical observation", occupied, err)
	}
}

// A rollback or unavailable acknowledgement with no durable event cannot be
// assumed committed and cannot provoke a second claim in the same operation.
func TestExpiredControlBridgeUnknownCommitWithoutEventNeverRetries(t *testing.T) {
	worker, evaluator, store, transport, record, _ := newCompetitionExpiredBridgeFixture(t)
	lost := errors.New("synthetic uncertain rolled-back commit")
	store.claimRollbackError = lost
	occupied, err := worker.reconcileExpiredControl(t.Context(), evaluator)
	if !occupied || !errors.Is(err, lost) || store.claims != 1 || store.completions != 0 || len(store.eventContent) != 0 ||
		store.job == nil || store.job.SlotFencingToken != record.SlotFencingToken || transport.calls != 1 || store.queued != 5 {
		t.Fatal("missing receipt was treated as a committed or repeatable claim", occupied, err)
	}
}

// An implementation using two commits could leave precisely this state. The
// terminal-only resolver must refuse it even with a genuine old signed status.
func TestExpiredControlBridgeIntermediateRunningReceiptNeverResumes(t *testing.T) {
	worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
	if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err != nil {
		t.Fatal(err)
	}
	copy := *store.terminal
	copy.State, copy.LeaseOwner, copy.CompletedAt = "running", store.eventActor, nil
	expired := store.now.Add(-time.Second)
	copy.LeaseExpiresAt = &expired
	store.job, store.terminal, store.slotRetained = &copy, nil, true
	if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); !errors.Is(err, ErrLeaseLost) || store.claims != 1 || store.completions != 1 || transport.calls != 1 || store.queued != 5 {
		t.Fatal("intermediate expired lease was resumed as bridge authority", err)
	}
}

// Historical validity never replaces the current exact held cut or a live
// parent. The already-terminal state stays untouched when either changes.
func TestExpiredControlBridgeTerminalReadbackRequiresCurrentAuthority(t *testing.T) {
	for _, mutate := range []func(*Worker, *competitionExpiredBridgeStore){
		func(_ *Worker, store *competitionExpiredBridgeStore) { store.held = false },
		func(_ *Worker, store *competitionExpiredBridgeStore) { store.revision++ },
		func(worker *Worker, _ *competitionExpiredBridgeStore) { worker.settings.automationLease.FencingToken++ },
		func(_ *Worker, store *competitionExpiredBridgeStore) { store.slotRetained = true },
	} {
		worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
		if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err != nil {
			t.Fatal(err)
		}
		mutate(worker, store)
		if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); !errors.Is(err, ErrLeaseLost) || store.claims != 1 || store.completions != 1 || transport.calls != 1 || store.queued != 5 {
			t.Fatal("stale current authority acknowledged or changed terminal custody", err)
		}
	}
}

// Recompute the event's own checksum to prove each independent identity and
// signature comparison, rather than relying only on hash mismatch rejection.
func TestExpiredControlBridgeTerminalReceiptRejectsRehashedMismatch(t *testing.T) {
	for _, mutate := range []func(*competitionExpiredControlClaimEvent){
		func(event *competitionExpiredControlClaimEvent) {
			event.ExpiredControlBridge.RecordSha256 = strings.Repeat("b", 64)
		},
		func(event *competitionExpiredControlClaimEvent) {
			event.ExpiredControlBridge.ApprovalSha256 = strings.Repeat("b", 64)
		},
		func(event *competitionExpiredControlClaimEvent) { event.ExpiredControlBridge.OldFence++ },
		func(event *competitionExpiredControlClaimEvent) { event.ExpiredControlBridge.NewFence++ },
		func(event *competitionExpiredControlClaimEvent) { event.ExpiredControlBridge.NewLeaseOwner += ".other" },
		func(event *competitionExpiredControlClaimEvent) { event.ExpiredControlBridge.EvaluationHoldRevision++ },
		func(event *competitionExpiredControlClaimEvent) { event.ExpiredControlBridge.SupervisorFence = 0 },
		func(event *competitionExpiredControlClaimEvent) {
			event.ExpiredControlBridge.RpcStartedAt = event.ExpiredControlBridge.RpcReceivedAt.Add(time.Second)
		},
		func(event *competitionExpiredControlClaimEvent) { event.ExpiredControlBridge.RpcTimeout = 0 },
		func(event *competitionExpiredControlClaimEvent) {
			event.ExpiredControlBridge.WitnessMaximumAge = event.ExpiredControlBridge.RpcTimeout + time.Second
		},
		func(event *competitionExpiredControlClaimEvent) {
			event.ExpiredControlBridge.WitnessExpiresAt = event.ExpiredControlBridge.RpcReceivedAt
		},
		func(event *competitionExpiredControlClaimEvent) { event.ExpiredControlBridge.Status.State = "running" },
		func(event *competitionExpiredControlClaimEvent) { event.ExecutorFencingToken++ },
	} {
		worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
		if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err != nil {
			t.Fatal(err)
		}
		var event competitionExpiredControlClaimEvent
		if err := json.Unmarshal(store.eventContent, &event); err != nil {
			t.Fatal(err)
		}
		mutate(&event)
		content, err := json.Marshal(map[string]any{"attempt": event.Attempt, "worker_image_digest": event.WorkerImageDigest,
			"executor_host_id": event.ExecutorHostId, "executor_slot_id": event.ExecutorSlotId, "executor_fencing_token": event.ExecutorFencingToken,
			"execution_deadline_at": event.ExecutionDeadlineAt, "expired_control_bridge": event.ExpiredControlBridge.payload()})
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		store.eventContent, store.eventSha256 = content, hex.EncodeToString(digest[:])
		if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); !errors.Is(err, ErrLeaseLost) || store.claims != 1 || store.completions != 1 || transport.calls != 1 || store.queued != 5 {
			t.Fatal("rehashed incompatible event adopted historical terminal authority", err)
		}
	}
}

// Neither a missing failed event nor arbitrary extra artifact keys can make
// a terminal row a positive acknowledgement of the bridge's exact outcome.
func TestExpiredControlBridgeTerminalPublicationRemainsClosed(t *testing.T) {
	for _, content := range [][]byte{nil, []byte(`{"attempt":1,"api_image_digest":"synthetic","worker_image_digest":"synthetic","artifact_manifest_sha256":"synthetic"}`)} {
		worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
		if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		store.terminalContent, store.terminalSha256 = content, hex.EncodeToString(digest[:])
		if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); !errors.Is(err, ErrLeaseLost) || store.claims != 1 || transport.calls != 1 || store.queued != 5 {
			t.Fatal("missing or nonclosed terminal publication was acknowledged", err)
		}
	}
}
