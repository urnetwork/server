// These controls separate read completion from response handoff and retain
// strict deadlines, bounded ownership, and terminal reader joins.
package perfvar

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A retained completion protects only its identity, not its validity or budget.
func TestLoadedLatencyProbeRetainedCompletionBoundaries(t *testing.T) {
	for _, test := range []struct {
		name        string
		readDelay   time.Duration
		success     bool
		deadline    bool
		duplicate   bool
		unknown     bool
		corrupt     bool
		missingTime bool
		zeroOffer   bool
		beforeOffer bool
	}{
		{name: "timely", readDelay: 200 * time.Millisecond, success: true},
		{name: "deadline", readDelay: time.Second, deadline: true},
		{name: "late", readDelay: time.Second + time.Nanosecond, deadline: true},
		{name: "duplicate", readDelay: 200 * time.Millisecond, success: true, duplicate: true},
		{name: "unknown", readDelay: 200 * time.Millisecond, unknown: true, deadline: true},
		{name: "corrupt", readDelay: 200 * time.Millisecond, corrupt: true},
		{name: "missing-time", readDelay: 200 * time.Millisecond, missingTime: true},
		{name: "missing-time-zero-offer", missingTime: true, zeroOffer: true},
		{name: "before-offer", readDelay: 200 * time.Millisecond, beforeOffer: true},
	} {
		synctest.Test(t, func(t *testing.T) {
			const sequence = latencyProbeLoadedStartSequence
			state := newLoadedLatencyProbeState(time.Second)
			sendTime := time.Now()
			if test.zeroOffer {
				sendTime = time.Time{}
			}
			state.attempt(sequence, sendTime, nil)
			var packet [32]byte
			responseSequence := sequence
			if test.unknown {
				responseSequence += 1
			}
			copy(packet[:], latencyProbeTestPacket(responseSequence))
			if test.corrupt {
				packet[31] = 1
			}
			time.Sleep(test.readDelay)
			response := state.completeRead(packet)
			var duplicate loadedLatencyProbeResponse
			if test.duplicate {
				duplicate = state.completeRead(packet)
			}
			if test.missingTime {
				response.receiveTime = time.Time{}
			} else if test.beforeOffer {
				response.receiveTime = sendTime.Add(-time.Nanosecond)
			}
			time.Sleep(2 * time.Second)
			state.expire(time.Now())
			state.receive(response.packet, response.receiveTime)
			if test.duplicate {
				state.receive(duplicate.packet, duplicate.receiveTime)
			}
			state.expire(time.Now())
			state.finish()
			wantSuccess := 0
			if test.success {
				wantSuccess = 1
			}
			if state.samples.attemptCount != 1 || len(state.samples.latencies) != wantSuccess ||
				state.samples.failureCount != 1-wantSuccess ||
				len(state.pending) != 0 || len(state.completedSequenceCounts) != 0 {
				t.Fatalf("case=%s completion ownership or accounting: samples=%+v pending=%v completed=%v",
					test.name, state.samples, state.pending, state.completedSequenceCounts)
			}
			if test.success {
				if state.samples.firstFailure != nil || state.samples.latencies[0] != test.readDelay {
					t.Fatalf("case=%s timely completion changed: %+v", test.name, state.samples)
				}
			} else if state.samples.firstFailure == nil ||
				(test.deadline && !errors.Is(state.samples.firstFailure, context.DeadlineExceeded)) {
				t.Fatalf("case=%s invalid completion lacked its failure: %+v", test.name, state.samples)
			}
		})
	}
}

// Expired offers without completed reads must not accumulate for the phase.
func TestLoadedLatencyProbeUnansweredOffersStayBounded(t *testing.T) {
	state := newLoadedLatencyProbeState(time.Second)
	startTime := time.Unix(100, 0)
	for index := 0; index < 1000; index += 1 {
		sendTime := startTime.Add(time.Duration(index) * 10 * time.Millisecond)
		state.attempt(latencyProbeLoadedStartSequence+uint64(index), sendTime, nil)
		state.expire(sendTime)
		if 100 < len(state.pending) || len(state.completedSequenceCounts) != 0 {
			t.Fatalf("unanswered offers retained beyond their budget: pending=%d completed=%d",
				len(state.pending), len(state.completedSequenceCounts))
		}
	}
	state.finish()
	if state.samples.attemptCount != 1000 || state.samples.failureCount != 1000 ||
		len(state.samples.latencies) != 0 || len(state.pending) != 0 {
		t.Fatalf("unanswered offer accounting: %+v pending=%d", state.samples, len(state.pending))
	}
}

// A phase already stopped offers nothing and starts no reader ownership.
func TestLoadedLatencyProbeStoppedBeforeStart(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		name := "workload"
		if cancelContext {
			name = "cancel"
		}
		ctx, cancel := context.WithCancel(t.Context())
		workloadDone := make(chan struct{})
		if cancelContext {
			cancel()
		} else {
			close(workloadDone)
		}
		connection := &latencyProbeScriptConn{
			reads: []latencyProbeScriptRead{{packet: latencyProbeTestPacket(latencyProbeLoadedStartSequence)}},
		}
		samples := runLoadedLatencyProbes(ctx, connection, latencyProbeLoadedStartSequence,
			time.Second, time.Millisecond, workloadDone, nil)
		cancel()
		if samples.attemptCount != 0 || samples.failureCount != 0 || len(samples.latencies) != 0 ||
			samples.firstFailure != nil || len(connection.writes) != 0 || len(connection.reads) != 1 {
			t.Fatalf("case=%s stopped phase started work: samples=%+v writes=%d reads=%d",
				name, samples, len(connection.writes), len(connection.reads))
		}
	}
}

// Cancellation joins both an unbuffered handoff and a full response channel;
// duplicates release every completion slot without creating extra successes.
func TestLoadedLatencyProbeCompletionJoinsFullHandoff(t *testing.T) {
	for _, test := range []struct {
		name       string
		unbuffered bool
		cancel     bool
	}{
		{name: "buffered-workload"},
		{name: "unbuffered-workload", unbuffered: true},
		{name: "buffered-cancel", cancel: true},
		{name: "unbuffered-cancel", unbuffered: true, cancel: true},
	} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			const sequence = latencyProbeLoadedStartSequence
			responseCount := 65
			if test.unbuffered {
				responseCount = 1
			}
			reads := make([]latencyProbeScriptRead, responseCount)
			for index := range reads {
				reads[index] = latencyProbeScriptRead{packet: latencyProbeTestPacket(sequence)}
			}
			writeObserved := make(chan struct{})
			backlogReady := make(chan struct{})
			releaseOwner := make(chan struct{})
			readerInterrupted := make(chan struct{})
			workloadDone := make(chan struct{})
			var writeOnce, interruptOnce sync.Once
			readCount := 0
			connection := &latencyProbeScriptConn{
				reads: reads,
				beforeReadHook: func() {
					<-writeObserved
					if readCount == responseCount {
						<-readerInterrupted
					}
				},
				afterWriteHook: func() { writeOnce.Do(func() { close(writeObserved) }) },
				setReadDeadlineHook: func(deadline time.Time) {
					if !time.Now().Before(deadline) {
						interruptOnce.Do(func() { close(readerInterrupted) })
					}
				},
			}
			completion := make(chan latencyProbeSamples, 1)
			go func() {
				completion <- runLoadedLatencyProbes(ctx, connection, sequence, time.Second, time.Hour,
					workloadDone, &loadedLatencyProbeTestSettings{
						afterAttemptHook: func(int) { <-releaseOwner },
						afterResponseReadHook: func() {
							readCount += 1
							if readCount == responseCount {
								close(backlogReady)
							}
						},
						unbufferedResponseHandoff: test.unbuffered,
					})
			}()
			<-backlogReady
			synctest.Wait()
			time.Sleep(2 * time.Second)
			if test.cancel {
				cancel()
			} else {
				close(workloadDone)
			}
			close(releaseOwner)
			samples := <-completion
			if samples.attemptCount != 1 || len(samples.latencies) != 1 || samples.failureCount != 0 ||
				samples.firstFailure != nil || readCount != responseCount || len(connection.reads) != 0 {
				t.Fatalf("case=%s terminal backlog did not join exactly once: samples=%+v read=%d remaining=%d",
					test.name, samples, readCount, len(connection.reads))
			}
		})
	}
}
