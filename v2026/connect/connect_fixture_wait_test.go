// Bounded fixture polling keeps a stalled protocol phase observable even when
// the package runs with -timeout=0. Synthetic time pins the liveness boundary.
package connect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// Counts, bytes and frame types belong to one queue observation, not to a
// promise that asynchronous protocol producers have stopped.
type testConnectQueueSnapshot struct {
	itemCount    int
	byteCount    ByteCount
	messageTypes []protocol.MessageType
}

// The receive observations use the sequence ids from the corresponding send
// observations; keep the complete sample together through validation.
type testConnectQueuesSnapshot struct {
	sequenceIdA connect.Id
	sequenceIdB connect.Id
	resendA     testConnectQueueSnapshot
	resendB     testConnectQueueSnapshot
	receiveA    testConnectQueueSnapshot
	receiveB    testConnectQueueSnapshot
}

// Wait for encryption controls to drain, preserving unexpected application
// items for the caller's strict count, byte and frame-type assertions.
func waitForTestConnectControlDrain(
	ctx context.Context,
	timeout time.Duration,
	readSnapshot func() testConnectQueuesSnapshot,
) (testConnectQueuesSnapshot, error) {
	var snapshot testConnectQueuesSnapshot
	err := TestingWaitForConnectCondition(ctx, timeout, 10*time.Second, func(context.Context) (bool, string) {
		snapshot = readSnapshot()
		count := 0
		for _, queue := range []testConnectQueueSnapshot{snapshot.resendA, snapshot.resendB, snapshot.receiveA, snapshot.receiveB} {
			for _, messageType := range queue.messageTypes {
				if messageType == protocol.MessageType_TransferEncryptedControl {
					count++
				}
			}
		}
		return count == 0, fmt.Sprintf(
			"pending controls=%d sequence_a=%s resend_a=%v receive_b=%v sequence_b=%s resend_b=%v receive_a=%v",
			count, snapshot.sequenceIdA, snapshot.resendA.messageTypes, snapshot.receiveB.messageTypes,
			snapshot.sequenceIdB, snapshot.resendB.messageTypes, snapshot.receiveA.messageTypes,
		)
	})
	// Identity-proof resends continue after establishment. Validate the sample
	// that passed the gate, not a later read after a new control was enqueued.
	return snapshot, err
}

// Poll one condition within a single deadline; changing diagnostics never
// renew the budget. Checks must return promptly or honor their supplied context.
func TestingWaitForConnectCondition(
	ctx context.Context,
	timeout time.Duration,
	interval time.Duration,
	check func(context.Context) (bool, string),
) error {
	waitCtx, waitCancel := context.WithTimeout(ctx, timeout)
	defer waitCancel()
	detail := "condition not checked"
	for {
		if err := waitCtx.Err(); err != nil {
			return fmt.Errorf("%s: %w", detail, err)
		}
		var ready bool
		ready, detail = check(waitCtx)
		if err := waitCtx.Err(); err != nil {
			return fmt.Errorf("%s: %w", detail, err)
		}
		if ready {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("%s: %w", detail, waitCtx.Err())
		case <-time.After(interval):
		}
	}
}

// A new proof can arrive immediately after a successful drain observation.
// Script that exact transition for each queue; validating a second observation
// would report a false failure despite all application messages being acked.
func TestConnectFixtureControlDrainKeepsSuccessfulSnapshot(t *testing.T) {
	for queueIndex := range 4 {
		reads := 0
		snapshot, err := waitForTestConnectControlDrain(context.Background(), time.Minute, func() testConnectQueuesSnapshot {
			reads++
			var next testConnectQueuesSnapshot
			if 1 < reads {
				queues := []*testConnectQueueSnapshot{&next.resendA, &next.resendB, &next.receiveA, &next.receiveB}
				*queues[queueIndex] = testConnectQueueSnapshot{
					itemCount: 1, byteCount: 64, messageTypes: []protocol.MessageType{protocol.MessageType_TransferEncryptedControl},
				}
			}
			return next
		})
		if err != nil {
			t.Fatalf("queue %d: drained observation failed: %v", queueIndex, err)
		}
		queues := []testConnectQueueSnapshot{snapshot.resendA, snapshot.resendB, snapshot.receiveA, snapshot.receiveB}
		if reads != 1 || queues[queueIndex].itemCount != 0 || queues[queueIndex].byteCount != 0 || queues[queueIndex].messageTypes != nil {
			t.Errorf("queue %d: successful drain was replaced by a later control: reads=%d snapshot=%+v", queueIndex, reads, queues[queueIndex])
		}
	}
}

// Every send and receive queue participates in the gate. Virtual time advances
// through explicit queued-control states, then observes the final drained set.
func TestConnectFixtureControlDrainWaitsForEveryQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reads := 0
		startTime := time.Now()
		snapshot, err := waitForTestConnectControlDrain(context.Background(), time.Minute, func() testConnectQueuesSnapshot {
			var next testConnectQueuesSnapshot
			if reads < 4 {
				queues := []*testConnectQueueSnapshot{&next.resendA, &next.resendB, &next.receiveA, &next.receiveB}
				*queues[reads] = testConnectQueueSnapshot{
					itemCount: 1, byteCount: 64, messageTypes: []protocol.MessageType{protocol.MessageType_TransferEncryptedControl},
				}
			}
			reads++
			return next
		})
		if err != nil || reads != 5 || time.Since(startTime) != 40*time.Second {
			t.Fatalf("queue gate missed a pending control: err=%v reads=%d elapsed=%s", err, reads, time.Since(startTime))
		}
		for _, queue := range []testConnectQueueSnapshot{snapshot.resendA, snapshot.resendB, snapshot.receiveA, snapshot.receiveB} {
			if queue.itemCount != 0 || queue.byteCount != 0 || queue.messageTypes != nil {
				t.Fatalf("drained observation retained a control: %+v", queue)
			}
		}
	})
}

// Settling controls must not erase residual application data or inconsistent
// queue accounting that the caller still needs to reject.
func TestConnectFixtureControlDrainPreservesUnexpectedData(t *testing.T) {
	want := testConnectQueueSnapshot{
		itemCount: 2, byteCount: 128, messageTypes: []protocol.MessageType{protocol.MessageType_TestSimpleMessage},
	}
	snapshot, err := waitForTestConnectControlDrain(context.Background(), time.Minute, func() testConnectQueuesSnapshot {
		return testConnectQueuesSnapshot{resendA: want}
	})
	if err != nil || snapshot.resendA.itemCount != want.itemCount || snapshot.resendA.byteCount != want.byteCount ||
		len(snapshot.resendA.messageTypes) != 1 || snapshot.resendA.messageTypes[0] != want.messageTypes[0] {
		t.Fatalf("control gate hid unexpected application data: snapshot=%+v err=%v", snapshot.resendA, err)
	}
}

// A permanently queued control retains the existing absolute deadline and its
// final observation, rather than returning a stale successful snapshot.
func TestConnectFixtureControlDrainDeadlinePreservesPendingSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startTime := time.Now()
		snapshot, err := waitForTestConnectControlDrain(context.Background(), time.Minute, func() testConnectQueuesSnapshot {
			return testConnectQueuesSnapshot{receiveB: testConnectQueueSnapshot{
				itemCount: 1, byteCount: 64, messageTypes: []protocol.MessageType{protocol.MessageType_TransferEncryptedControl},
			}}
		})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(startTime) != time.Minute ||
			!strings.Contains(err.Error(), "receive_b=[TransferEncryptedControl]") {
			t.Fatalf("pending controls lost their deadline or diagnostic: elapsed=%s err=%v", time.Since(startTime), err)
		}
		if snapshot.receiveB.itemCount != 1 || snapshot.receiveB.byteCount != 64 || len(snapshot.receiveB.messageTypes) != 1 {
			t.Fatalf("deadline lost the pending observation: %+v", snapshot.receiveB)
		}
	})
}

// The original control settle loop could hold two queued controls forever.
// Fake time reaches the phase deadline without a scheduler-dependent timeout.
func TestConnectFixtureWaitStalledControlsReachDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startTime := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		checks := 0
		err := TestingWaitForConnectCondition(ctx, time.Minute, 10*time.Second, func(ctx context.Context) (bool, string) {
			checks++
			return false, "pending controls=2 resend_a=1 resend_b=1"
		})
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "pending controls=2") {
			t.Fatalf("stalled controls lost their deadline or diagnostics: %v", err)
		}
		// The final poll and context timer share a timestamp; either can wake
		// first, but neither may extend the absolute phase budget.
		if elapsed := time.Since(startTime); elapsed != time.Minute || checks < 6 || 7 < checks {
			t.Fatalf("settle deadline was renewed: elapsed=%s checks=%d", elapsed, checks)
		}
	})
}

// Parent cancellation interrupts the polling wait, including an hour-long poll
// interval; an explicit first-check barrier forces cancellation while pending.
func TestConnectFixtureWaitCancellationInterruptsPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		checked := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- TestingWaitForConnectCondition(ctx, 2*time.Hour, time.Hour, func(context.Context) (bool, string) {
				close(checked)
				return false, "controls still pending"
			})
		}()
		<-checked
		synctest.Wait()
		cancelTime := time.Now()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("pending wait ignored cancellation: %v", err)
		}
		if elapsed := time.Since(cancelTime); elapsed != 0 {
			t.Fatalf("cancellation waited for the polling interval: %s", elapsed)
		}
	})
}

// Readiness is checked immediately and does not pay the polling interval.
func TestConnectFixtureWaitReadyReturnsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startTime := time.Now()
		err := TestingWaitForConnectCondition(context.Background(), time.Minute, time.Hour, func(context.Context) (bool, string) {
			return true, "queues empty"
		})
		if err != nil || time.Since(startTime) != 0 {
			t.Fatalf("ready condition waited: err=%v elapsed=%s", err, time.Since(startTime))
		}
	})
}

// An explicit state transition drains a previously nonempty condition; the
// waiter must neither report premature success nor retain stale diagnostics.
func TestConnectFixtureWaitDrainsBeforeSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ready atomic.Bool
		checked := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			done <- TestingWaitForConnectCondition(context.Background(), time.Minute, time.Second, func(context.Context) (bool, string) {
				if ready.Load() {
					return true, "queues empty"
				}
				checked <- struct{}{}
				return false, "controls pending"
			})
		}()
		<-checked
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("wait ended before the queues drained: %v", err)
		default:
		}
		ready.Store(true)
		time.Sleep(time.Second)
		if err := <-done; err != nil {
			t.Fatalf("drained queues did not settle: %v", err)
		}
	})
}

// A canceled lifecycle cannot be reclassified as success by an empty snapshot.
func TestConnectFixtureWaitCanceledContextDoesNotCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := TestingWaitForConnectCondition(ctx, time.Minute, time.Second, func(context.Context) (bool, string) {
		t.Fatal("checked a condition after lifecycle cancellation")
		return true, "queues empty"
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lifecycle reported success: %v", err)
	}
}

// A check that finishes at its deadline must fail even if its last read is ready.
func TestConnectFixtureWaitCheckUsesPhaseContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startTime := time.Now()
		err := TestingWaitForConnectCondition(context.Background(), time.Minute, time.Second, func(ctx context.Context) (bool, string) {
			<-ctx.Done()
			return true, "check completed after deadline"
		})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(startTime) != time.Minute {
			t.Fatalf("check escaped its phase deadline: err=%v elapsed=%s", err, time.Since(startTime))
		}
	})
}

// A shorter owner deadline wins even when neither phase nor poll timer is due.
func TestConnectFixtureWaitPreservesEarlierDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		startTime := time.Now()
		err := TestingWaitForConnectCondition(ctx, time.Minute, time.Hour, func(context.Context) (bool, string) {
			return false, "pending"
		})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(startTime) != 5*time.Second {
			t.Fatalf("phase extended its owner deadline: err=%v elapsed=%s", err, time.Since(startTime))
		}
	})
}
