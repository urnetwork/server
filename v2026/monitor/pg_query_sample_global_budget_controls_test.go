// These controls distinguish lawful global-slot contention from a scheduling
// defect. They make no production contact and do not change the owner policy.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

// All four real global permits remain owned until an explicit release at 23s.
// The real sampler then reaches the actual native guard with only 17s left.
// An empty executable path prevents contact even if that guard regresses.
// This is a passing reproduction of the current finite-owner contract, not a
// red regression or proof that the production holders were entitled to wait.
func TestPgGlobalBudgetLawfulLateGrantReproducesNativeRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		signal, calls := pgRunSlotTimingFixture(t)
		signal.env.runner.(*runner).runSSH = runSSHCommand
		acquires, releases := 0, 0
		signal.env.cfg.sharedSshAdmission = &sshAdmissionPhaseFixture{acquireCommand: func(context.Context, string) (func() error, error) {
			acquires++
			return func() error { releases++; return nil }, nil
		}}
		slots := newRunSlotPool(runLoopMaxConcurrentSignals)
		elapsed := pgRunSlotTimingTurn(t, t.Context(), slots, signal, 23*time.Second)
		receipt := pgRunSlotReadTiming(t, signal.env)
		proof := receipt.TransportObservation
		_, raw := pgTransportReadControlReceipt(t, signal.env)
		var decision struct {
			Transport struct {
				Remaining *int64 `json:"start_refusal_remaining_budget_ns"`
				Exit      *int   `json:"native_exit_code"`
			} `json:"transport_observation"`
		}
		if json.Unmarshal(raw, &decision) != nil {
			t.Fatal("fixture did not retain the actual native-start decision")
		}
		if elapsed != 23*time.Second || *calls != 0 || receipt.Complete || receipt.Samples != 0 || receipt.Reason != "source-unavailable" || receipt.SourceFailure == nil || receipt.SourceFailure.Phase != "local_start_budget" || receipt.SourceFailure.Cause != "below_mandatory_waits" {
			t.Fatal("lawful late global grant did not reach the mandatory-wait refusal")
		}
		if proof.RunSlotWaitMs == nil || *proof.RunSlotWaitMs != 23000 || proof.RunSlotGrantedRemainingBudgetMs == nil || *proof.RunSlotGrantedRemainingBudgetMs != 17000 || proof.RemainingBudgetMs != 17000 || decision.Transport.Remaining == nil || *decision.Transport.Remaining != (17*time.Second).Nanoseconds() {
			t.Fatal("global wait was not charged to the same original forty-second owner")
		}
		if proof.Phase != "ssh_start" || proof.Spawn != "not_attempted" || proof.RemotePhase != "unknown" || proof.SpawnRemainingBudgetMs != nil || proof.Joined || proof.OwnerDeadlineExceeded || decision.Transport.Exit != nil {
			t.Fatal("local refusal invented native execution, remote work or owner expiration")
		}
		if acquires != 1 || releases != 1 || slots.active != 0 || len(slots.waiting) != 0 {
			t.Fatal("refusal leaked or fabricated an owned global/shared grant")
		}
		host := signal.env.cfg.hostByRole("pg-primary")
		if held := len(signal.env.runner.(*runner).remoteCommands.hostSlots(host.addr(signal.env.cfg.addressMode))); held != 0 {
			t.Fatal("refusal retained its local host grant")
		}
		// The spent attempt must not obtain another source turn before the
		// terminal fifteen-minute floor, even when the global pool is now free.
		if _, err := signal.Run(t.Context(), syntheticSettings(nil)); err != nil || acquires != 1 {
			t.Fatal("local refusal loosened the durable no-early-retry boundary")
		}
		if pgQuerySampleBudget != 40*time.Second || pgSampleMinimumWaitBudget != 22*time.Second || pgQuerySampleCadence != 15*time.Minute || runLoopMaxConcurrentSignals != 4 || maxConcurrentRemoteCommandsPerHost != 2 || sshAdmissionGlobalLimit != 4 || sshAdmissionHostLimit != 2 {
			t.Fatal("control changed the observation work, owner, cadence or capacity contract")
		}
	})
}

// A released slot at 15s leaves 25s, so the unchanged 22s synthetic source can
// complete inside the original owner. The injected source has no native child
// authority; it exercises the real guard, parser, cadence and receipt path.
func TestPgGlobalBudgetTimelyGrantCompletesWithoutOwnerRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signal, calls := pgRunSlotTimingFixture(t)
		runner := signal.env.runner.(*runner)
		source := runner.runSSH
		runner.runSSH = func(ctx context.Context, args []string, input string) (string, string, error) {
			if err := pgSampleTransportFromContext(ctx).admitNativeStart(ctx); err != nil {
				return "", "", err
			}
			return source(ctx, args, input)
		}
		slots := newRunSlotPool(runLoopMaxConcurrentSignals)
		elapsed := pgRunSlotTimingTurn(t, t.Context(), slots, signal, 15*time.Second)
		receipt := pgRunSlotReadTiming(t, signal.env)
		proof := receipt.TransportObservation
		if elapsed != 37*time.Second || *calls != 1 || !receipt.Complete || receipt.Samples != 12 || receipt.SourceFailure != nil || proof.OwnerDeadlineExceeded || proof.RemainingBudgetMs != 25000 {
			t.Fatal("timely lawful release did not complete the unchanged mandatory waits")
		}
		if proof.RunSlotWaitMs == nil || *proof.RunSlotWaitMs != 15000 || proof.RunSlotGrantedRemainingBudgetMs == nil || *proof.RunSlotGrantedRemainingBudgetMs != 25000 || proof.Spawn != "unknown" || proof.Joined || proof.SpawnRemainingBudgetMs != nil {
			t.Fatal("healthy synthetic source renewed the owner or invented native authority")
		}
		if slots.active != 0 || len(slots.waiting) != 0 {
			t.Fatal("completed sample retained a global grant")
		}
	})
}

// A shorter inherited owner is still authoritative. Granting at 9s cannot
// create 40 fresh seconds or replace its remaining 21s with the normal owner.
func TestPgGlobalBudgetEarlierParentSurvivesEveryAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		signal, _ := pgRunSlotTimingFixture(t)
		signal.env.runner.(*runner).runSSH = runSSHCommand
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		slots := newRunSlotPool(runLoopMaxConcurrentSignals)
		elapsed := pgRunSlotTimingTurn(t, ctx, slots, signal, 9*time.Second)
		receipt := pgRunSlotReadTiming(t, signal.env)
		proof := receipt.TransportObservation
		if elapsed != 9*time.Second || receipt.Complete || receipt.SourceFailure == nil || receipt.SourceFailure.Phase != "local_start_budget" || proof.RemainingBudgetMs != 21000 || proof.RunSlotGrantedRemainingBudgetMs == nil || *proof.RunSlotGrantedRemainingBudgetMs != 21000 || proof.Spawn != "not_attempted" || proof.OwnerDeadlineExceeded {
			t.Fatal("a subordinate admission replaced or renewed the earlier parent owner")
		}
	})
}

// Prompt global admission does not protect the source from a later shared
// wait. Keep that distinct cause charged to the same original deadline.
func TestPgGlobalBudgetSharedWaitCannotBorrowNewExecutionTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		signal, _ := pgRunSlotTimingFixture(t)
		signal.env.runner.(*runner).runSSH = runSSHCommand
		releases := 0
		signal.env.cfg.sharedSshAdmission = &sshAdmissionPhaseFixture{acquireCommand: func(ctx context.Context, _ string) (func() error, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(14 * time.Second):
				return func() error { releases++; return nil }, nil
			}
		}}
		slots := newRunSlotPool(runLoopMaxConcurrentSignals)
		elapsed := pgRunSlotTimingTurn(t, t.Context(), slots, signal, 5*time.Second)
		receipt := pgRunSlotReadTiming(t, signal.env)
		proof := receipt.TransportObservation
		_, timing := readPgLocalTiming(t, signal.env)
		requirePgLocalSpan(t, timing, "shared_admission", 14*time.Second)
		if elapsed != 19*time.Second || receipt.Complete || receipt.SourceFailure == nil || receipt.SourceFailure.Phase != "local_start_budget" || proof.RunSlotWaitMs == nil || *proof.RunSlotWaitMs != 5000 || proof.RunSlotGrantedRemainingBudgetMs == nil || *proof.RunSlotGrantedRemainingBudgetMs != 35000 || proof.Spawn != "not_attempted" || proof.OwnerDeadlineExceeded || releases != 1 {
			t.Fatal("shared wait was reset, leaked or misattributed to global admission")
		}
	})
}

// Cancel a durably queued request while all four prior owners remain live.
// It must neither release their grants nor invent a database attempt receipt.
func TestPgGlobalBudgetCanceledQueuePreservesPriorOwners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signal, calls := pgRunSlotTimingFixture(t)
		slots := newRunSlotPool(runLoopMaxConcurrentSignals)
		for range runLoopMaxConcurrentSignals {
			release, err := slots.acquire(t.Context(), false)
			if err != nil {
				t.Fatal("fixture could not retain the four actual permits")
			}
			defer release()
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := runSignalInSlot(ctx, slots, signal, syntheticSettings(nil))
			done <- err
		}()
		synctest.Wait()
		if slots.active != 4 || len(slots.waiting) != 1 {
			t.Fatal("fixture did not reach the saturated global queue")
		}
		cancel()
		err := <-done
		var admission *runSlotAdmissionError
		if !errors.Is(err, context.Canceled) || !errors.As(err, &admission) || *calls != 0 || slots.active != 4 || len(slots.waiting) != 0 {
			t.Fatal("canceled waiter changed another owner's permit or reached source work")
		}
		if _, err := os.Stat(filepath.Join(signal.env.cfg.stateDir, "pg-query-sample")); !os.IsNotExist(err) {
			t.Fatal("unadmitted cancellation invented durable sampler state")
		}
	})
}
