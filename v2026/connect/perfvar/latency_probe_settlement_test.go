// These controls keep the load-at-offer boundary separate from each offered
// probe's unchanged deadline, using real pipe reads and reader cancellation.
package perfvar

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A reply remains owned by its load-time offer after the bulk completes.
func TestLoadedLatencyProbeSettlementAcceptsReplyAfterBulk(t *testing.T) {
	runLoadedLatencyProbeSettlementTest(t, "after-bulk")
}

// Stopping offers never renews a pending probe's original deadline.
func TestLoadedLatencyProbeSettlementKeepsOriginalDeadlines(t *testing.T) {
	runLoadedLatencyProbeSettlementTest(t, "deadline")
}

// Existing replies completed while the bulk is active remain successes.
func TestLoadedLatencyProbeSettlementKeepsBulkActiveReplies(t *testing.T) {
	runLoadedLatencyProbeSettlementTest(t, "before-bulk")
}

// Caller cancellation must join the pending reader without a settlement wait.
func TestLoadedLatencyProbeSettlementCancellationJoinsReader(t *testing.T) {
	runLoadedLatencyProbeSettlementTest(t, "cancel")
}

// Three real writes at 0/100/200 ms precede the 250 ms bulk boundary. The
// receiver owns their exact bytes; only its echo barrier changes between cases.
func runLoadedLatencyProbeSettlementTest(t *testing.T, mode string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		connection, peer := net.Pipe()
		startTime := time.Now()
		workloadDone := make(chan struct{})
		releaseReplies := make(chan struct{})
		thirdAttempt := make(chan struct{})
		peerDone := make(chan struct{})
		ownerDone := make(chan struct{})
		type settlementResult struct {
			samples latencyProbeSamples
			elapsed time.Duration
		}
		completion := make(chan settlementResult, 1)
		var packets [3][32]byte
		var peerErr error
		var stateLock sync.Mutex
		var events []latencyProbeObservation
		observer := func(event latencyProbeObservation) {
			stateLock.Lock()
			defer stateLock.Unlock()
			events = append(events, event)
		}
		go func() {
			defer close(peerDone)
			for index := range packets {
				if _, peerErr = io.ReadFull(peer, packets[index][:]); peerErr != nil {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-releaseReplies:
			}
			for index := range packets {
				if _, peerErr = peer.Write(packets[index][:]); peerErr != nil {
					return
				}
			}
		}()
		go func() {
			defer close(ownerDone)
			samples := runLoadedLatencyProbes(ctx, connection, latencyProbeLoadedStartSequence,
				time.Second, 100*time.Millisecond, workloadDone, &loadedLatencyProbeTestSettings{
					observer: observer,
					afterAttemptHook: func(count int) {
						if count == 3 {
							close(thirdAttempt)
						}
					},
				})
			completion <- settlementResult{samples: samples, elapsed: time.Since(startTime)}
		}()
		defer func() {
			cancel()
			_ = connection.Close()
			_ = peer.Close()
			<-ownerDone
			<-peerDone
		}()
		<-thirdAttempt
		synctest.Wait()
		if elapsed := time.Since(startTime); elapsed != 200*time.Millisecond {
			t.Fatalf("third offer at %s, want 200 ms", elapsed)
		}
		for index := range packets {
			if !bytes.Equal(packets[index][:], latencyProbeTestPacket(latencyProbeLoadedStartSequence+uint64(index))) {
				t.Fatalf("receiver did not own exact offer %d: %x", index, packets[index])
			}
		}
		if mode == "before-bulk" {
			time.Sleep(25 * time.Millisecond)
			close(releaseReplies)
			synctest.Wait()
			time.Sleep(25 * time.Millisecond)
		} else {
			time.Sleep(50 * time.Millisecond)
		}
		if mode == "cancel" {
			cancel()
		} else {
			close(workloadDone)
		}
		synctest.Wait()
		if mode == "after-bulk" {
			time.Sleep(100 * time.Millisecond)
			close(releaseReplies)
			synctest.Wait()
		}
		result := <-completion
		<-ownerDone
		stateLock.Lock()
		observations := append([]latencyProbeObservation(nil), events...)
		stateLock.Unlock()
		wantElapsed := 250 * time.Millisecond
		wantSuccess := 0
		var wantLatencies []time.Duration
		switch mode {
		case "after-bulk":
			wantElapsed = 350 * time.Millisecond
			wantSuccess = 3
			wantLatencies = []time.Duration{350 * time.Millisecond, 250 * time.Millisecond, 150 * time.Millisecond}
		case "before-bulk":
			wantSuccess = 3
			wantLatencies = []time.Duration{225 * time.Millisecond, 125 * time.Millisecond, 25 * time.Millisecond}
		case "deadline":
			wantElapsed = 1200 * time.Millisecond
		case "cancel":
		default:
			t.Fatalf("unknown settlement mode %q", mode)
		}
		t.Logf("mode=%s elapsed=%s attempts=%d success=%d failure=%d first=%v",
			mode, result.elapsed, result.samples.attemptCount, len(result.samples.latencies),
			result.samples.failureCount, result.samples.firstFailure)
		if result.elapsed != wantElapsed || result.samples.attemptCount != 3 ||
			len(result.samples.latencies) != wantSuccess || result.samples.failureCount != 3-wantSuccess {
			t.Fatalf("bulk completion prematurely settled or extended load-time offers: elapsed=%s want=%s samples=%+v",
				result.elapsed, wantElapsed, result.samples)
		}
		for index, wantLatency := range wantLatencies {
			if result.samples.latencies[index] != wantLatency {
				t.Fatalf("offer %d latency=%s want=%s", index, result.samples.latencies[index], wantLatency)
			}
		}
		if 0 < wantSuccess {
			<-peerDone
			if peerErr != nil || result.samples.firstFailure != nil {
				t.Fatalf("timely real echoes failed: peer=%v samples=%+v", peerErr, result.samples)
			}
		} else if result.samples.firstFailure == nil {
			t.Fatal("unanswered offers lost their failures")
		}
		if mode == "deadline" && !errors.Is(result.samples.firstFailure, context.DeadlineExceeded) {
			t.Fatalf("original expiry reason changed: %v", result.samples.firstFailure)
		}
		offerCount := 0
		boundaryCount := 0
		var expired [3]bool
		for _, event := range observations {
			switch event.Stage {
			case "offer":
				if event.SampleTime.Sub(startTime) != time.Duration(offerCount)*100*time.Millisecond || 3 <= offerCount {
					t.Fatalf("offered new demand after bulk completion: %+v", event)
				}
				offerCount += 1
			case "loaded-boundary":
				boundaryCount += 1
				if event.SampleTime.Sub(startTime) != 250*time.Millisecond {
					t.Fatalf("load-at-offer boundary moved to settlement: %+v", event)
				}
			case "expired":
				if event.Sequence < latencyProbeLoadedStartSequence ||
					latencyProbeLoadedStartSequence+3 <= event.Sequence ||
					expired[event.Sequence-latencyProbeLoadedStartSequence] {
					t.Fatalf("expiry did not belong to one outstanding offer: %+v", event)
				}
				expired[event.Sequence-latencyProbeLoadedStartSequence] = true
				wantDeadline := time.Second + time.Duration(event.Sequence-latencyProbeLoadedStartSequence)*100*time.Millisecond
				if event.SampleTime.Sub(startTime) != wantDeadline {
					t.Fatalf("offer's original deadline changed: %+v want=%s", event, wantDeadline)
				}
			}
		}
		if offerCount != 3 || boundaryCount != 1 {
			t.Fatalf("offer/boundary lineage=%d/%d want=3/1", offerCount, boundaryCount)
		}
		if mode == "deadline" && expired != [3]bool{true, true, true} {
			t.Fatalf("original deadlines did not settle all three identities: %v", expired)
		}
	})
}
