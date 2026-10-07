package perfvar

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A disabled observer must not inspect either error or packet contents.
type unreadableLatencyProbeError struct{}

func (unreadableLatencyProbeError) Error() string { panic("disabled observer inspected error") }
func (unreadableLatencyProbeError) Is(error) bool { panic("disabled observer classified error") }
func (unreadableLatencyProbeError) As(any) bool   { panic("disabled observer classified error") }

func TestLatencyProbeObserverNilIsAllocationAndParseFree(t *testing.T) {
	if got := testing.AllocsPerRun(100, func() {
		observeLatencyProbe(nil, "test", 1, time.Time{}, time.Time{}, unreadableLatencyProbeError{})
		observeLatencyProbePacket(nil, "test", nil, time.Time{}, unreadableLatencyProbeError{})
	}); got != 0 {
		t.Fatalf("nil observer allocated %g objects", got)
	}
	var captured latencyProbeObservation
	observer := func(event latencyProbeObservation) { captured = event }
	if got := testing.AllocsPerRun(100, func() {
		observeLatencyProbe(observer, "test", 1, time.Time{}, time.Time{}, nil)
	}); got != 0 {
		t.Fatalf("metadata observation allocated %g objects", got)
	}
	if captured.Stage != "test" || captured.Sequence != 1 || captured.ObservedTime.IsZero() {
		t.Fatalf("metadata not published: %+v", captured)
	}
}

func TestLatencyProbeObserverPacketAndErrorMetadata(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		kind string
	}{
		{"success", nil, ""},
		{"canceled", context.Canceled, "canceled"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"short-write", io.ErrShortWrite, "short-write"},
		{"eof", io.EOF, "eof"},
		{"other", errors.New("not retained in metadata"), "error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var event latencyProbeObservation
			observer := func(value latencyProbeObservation) { event = value }
			sample := time.Unix(123, 456)
			packet := latencyProbeTestPacket(latencyProbeLoadedStartSequence + 9)
			observeLatencyProbePacket(observer, "echo-write", packet, sample, test.err)
			clear(packet)
			if event.Sequence != latencyProbeLoadedStartSequence+9 || event.Stage != "echo-write" ||
				event.SampleTime != sample || event.ErrorKind != test.kind {
				t.Fatalf("metadata=%+v", event)
			}
		})
	}
	for _, packet := range [][]byte{nil, make([]byte, 31), append(make([]byte, 31), 1)} {
		var stage string
		observeLatencyProbePacket(func(event latencyProbeObservation) { stage = event.Stage }, "echo-read", packet, time.Time{}, nil)
		if stage != "echo-read-malformed" {
			t.Fatalf("malformed packet stage=%q", stage)
		}
	}
}

func TestLatencyProbeObserverDistinguishesTimelyLateAndLost(t *testing.T) {
	for _, mode := range []string{"timely", "late", "lost", "incomplete", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var stages []string
				state := newLoadedLatencyProbeState(time.Second)
				state.observer = func(event latencyProbeObservation) { stages = append(stages, event.Stage) }
				const sequence = latencyProbeLoadedStartSequence
				state.attempt(sequence, time.Now(), nil)
				var response [32]byte
				copy(response[:], latencyProbeTestPacket(sequence))
				wantStage := ""
				switch mode {
				case "timely":
					time.Sleep(500 * time.Millisecond)
					state.receive(response, time.Now())
					wantStage = "accepted"
				case "late":
					time.Sleep(2 * time.Second)
					state.receive(response, time.Now())
					wantStage = "late"
				case "lost":
					time.Sleep(2 * time.Second)
					state.expire(time.Now())
					wantStage = "expired"
				case "incomplete":
					wantStage = "incomplete"
				case "corrupt":
					response[31] = 1
					state.receive(response, time.Now())
					wantStage = "malformed"
				}
				state.finish()
				found := false
				for _, stage := range stages {
					found = found || stage == wantStage
				}
				wantSuccess := 0
				if mode == "timely" {
					wantSuccess = 1
				}
				if !found || len(state.samples.latencies) != wantSuccess ||
					state.samples.failureCount != 1-wantSuccess || state.samples.attemptCount != 1 {
					t.Fatalf("mode=%s stages=%v samples=%+v", mode, stages, state.samples)
				}
			})
		})
	}
}

func TestLatencyProbeObserverRejectsShortRead(t *testing.T) {
	connection := &latencyProbeScriptConn{
		reads: []latencyProbeScriptRead{{packet: make([]byte, 31)}},
	}
	var events []latencyProbeObservation
	_, err := runLatencyProbeObserved(t.Context(), connection, 7, time.Second, func(event latencyProbeObservation) {
		events = append(events, event)
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short response error=%v", err)
	}
	if len(events) != 3 || events[2].Stage != "read-error" || events[2].ErrorKind != "eof" {
		t.Fatalf("short read events=%+v", events)
	}
}

func TestLatencyProbeObserverExcludesReadAfterBulkBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sequence = latencyProbeLoadedStartSequence
		readEntered := make(chan struct{})
		writeObserved := make(chan struct{})
		boundaryObserved := make(chan struct{})
		releaseRead := make(chan struct{})
		workloadDone := make(chan struct{})
		var readOnce, writeOnce sync.Once
		var mutex sync.Mutex
		var events []latencyProbeObservation
		observer := func(event latencyProbeObservation) {
			mutex.Lock()
			defer mutex.Unlock()
			events = append(events, event)
			if event.Stage == "loaded-boundary" {
				close(boundaryObserved)
			}
		}
		connection := &latencyProbeScriptConn{
			reads: []latencyProbeScriptRead{{packet: latencyProbeTestPacket(sequence)}},
			beforeReadHook: func() {
				readOnce.Do(func() { close(readEntered) })
				<-releaseRead
			},
			afterWriteHook: func() { writeOnce.Do(func() { close(writeObserved) }) },
		}
		completion := make(chan latencyProbeSamples, 1)
		go func() {
			completion <- runLoadedLatencyProbes(
				t.Context(), connection, sequence, time.Second, time.Hour, workloadDone,
				&loadedLatencyProbeTestSettings{observer: observer, unbufferedResponseHandoff: true},
			)
		}()
		<-readEntered
		<-writeObserved
		close(workloadDone)
		<-boundaryObserved
		synctest.Wait()
		time.Sleep(time.Nanosecond)
		close(releaseRead)
		samples := <-completion
		var boundary, discarded latencyProbeObservation
		for _, event := range events {
			switch event.Stage {
			case "loaded-boundary":
				boundary = event
			case "read-after-boundary":
				discarded = event
			case "accepted":
				t.Fatal("post-boundary read was accepted")
			}
		}
		if discarded.Sequence != sequence || !boundary.SampleTime.Before(discarded.SampleTime) ||
			samples.attemptCount != 1 || len(samples.latencies) != 0 || samples.failureCount != 1 ||
			!errors.Is(samples.firstFailure, errLoadedLatencyProbeIncomplete) {
			t.Fatalf("boundary=%+v discard=%+v samples=%+v", boundary, discarded, samples)
		}
	})
}

// This characterizes the existing accounting seam; it does not weaken the
// gate or claim it caused the PERF failure. The diagnostic must distinguish a
// timely complete read whose handoff loses its pending identity from a packet
// which was actually read after its deadline.
func TestLatencyProbeObserverRevealsTimelyReadExpiredBeforeHandoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sequence = latencyProbeLoadedStartSequence
		writeObserved := make(chan struct{})
		readObserved := make(chan struct{})
		releaseRead := make(chan struct{})
		workloadDone := make(chan struct{})
		var writeOnce sync.Once
		var readOnce sync.Once
		var mutex sync.Mutex
		var events []latencyProbeObservation
		observer := func(event latencyProbeObservation) {
			mutex.Lock()
			defer mutex.Unlock()
			events = append(events, event)
		}
		connection := &latencyProbeScriptConn{
			reads:          []latencyProbeScriptRead{{packet: latencyProbeTestPacket(sequence)}},
			beforeReadHook: func() { <-writeObserved },
			afterWriteHook: func() { writeOnce.Do(func() { close(writeObserved) }) },
		}
		completion := make(chan latencyProbeSamples, 1)
		go func() {
			completion <- runLoadedLatencyProbes(
				t.Context(), connection, sequence, time.Second, 100*time.Millisecond, workloadDone,
				&loadedLatencyProbeTestSettings{
					observer: observer,
					afterResponseReadHook: func() {
						readOnce.Do(func() { close(readObserved) })
						<-releaseRead
					},
				},
			)
		}()
		<-readObserved
		synctest.Wait()
		time.Sleep(1100 * time.Millisecond)
		synctest.Wait()
		close(workloadDone)
		synctest.Wait()
		close(releaseRead)
		samples := <-completion
		var offered, read, expired, unmatched latencyProbeObservation
		for _, event := range events {
			if event.Sequence != sequence {
				continue
			}
			switch event.Stage {
			case "offer":
				offered = event
			case "read-complete":
				read = event
			case "expired":
				expired = event
			case "unmatched":
				unmatched = event
			}
		}
		if read.Stage == "" || expired.Stage == "" || unmatched.Stage == "" ||
			!read.SampleTime.Before(offered.SampleTime.Add(time.Second)) ||
			unmatched.SampleTime != read.SampleTime || unmatched.ObservedTime.Before(expired.ObservedTime) {
			t.Fatalf("handoff evidence offer=%+v read=%+v expired=%+v unmatched=%+v", offered, read, expired, unmatched)
		}
		if len(samples.latencies) != 0 || samples.failureCount != samples.attemptCount {
			t.Fatalf("instrumentation changed the existing accounting: %+v", samples)
		}
	})
}
