// The reading contract of connect's per-peer evidence, and that a capability
// that was not tested is never recorded.
package capprobe

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// The reading of connect's per-peer evidence (DESIGNNOTES §3.13): which
// observations are evidence about the provider, and which say nothing and
// are never recorded.

// A fixed instant, so no test reads the clock.
var readingTestNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

const readingTestTlsTimeout = 60 * time.Second

// The observation of a provider that answered, with the given seal, group and
// window, first attempted and first acknowledged long ago.
func readingTestNegotiated(sealed bool, postQuantum bool, windowAdvertised bool) connect.PeerCapabilityObservation {
	observation := connect.PeerCapabilityObservation{
		PeerId:              connect.NewId(),
		E2eAttempted:        true,
		E2eFirstAttemptTime: readingTestNow.Add(-5 * time.Minute),
		E2eNegotiated:       true,
		E2eSealed:           sealed,
		KeyExchange:         "X25519",
		AckObserved:         true,
		WindowAdvertised:    windowAdvertised,
	}
	if postQuantum {
		observation.KeyExchange = "X25519MLKEM768"
		observation.PostQuantum = true
	}
	if windowAdvertised {
		observation.MaxReceiveWindowByteCount = 4 * 1024 * 1024
	}
	return observation
}

// Each row of the reading contract: PQE authenticated is a seal with a
// post-quantum group, unauthenticated a negotiation with one, classical one
// without; no responder is an attempt unanswered after the establishment
// window; not tested is no attempt. Windows read yes with an advertisement,
// no with acknowledgements that never carried one, untested without either.
func TestReadingContract(t *testing.T) {
	longAgo := readingTestNow.Add(-5 * time.Minute)
	unanswered := connect.PeerCapabilityObservation{
		E2eAttempted:        true,
		E2eFirstAttemptTime: longAgo,
		AckObserved:         true,
	}
	for _, c := range []struct {
		name        string
		observation connect.PeerCapabilityObservation
		found       bool
		ackSeenAt   time.Time
		e2e         e2eReading
		window      windowReading
	}{
		{name: "pqe authenticated, window yes", observation: readingTestNegotiated(true, true, true), found: true, ackSeenAt: longAgo, e2e: e2ePostQuantumSealed, window: windowResizable},
		{name: "pqe unauthenticated, window yes", observation: readingTestNegotiated(false, true, true), found: true, ackSeenAt: longAgo, e2e: e2ePostQuantumUnsealed, window: windowResizable},
		{name: "classical sealed, window no", observation: readingTestNegotiated(true, false, false), found: true, ackSeenAt: longAgo, e2e: e2eClassicalSealed, window: windowFixed},
		{name: "classical unsealed, window yes", observation: readingTestNegotiated(false, false, true), found: true, ackSeenAt: longAgo, e2e: e2eClassicalUnsealed, window: windowResizable},
		{name: "no responder after the window", observation: unanswered, found: true, ackSeenAt: longAgo, e2e: e2eNoResponder, window: windowFixed},
		{name: "not tested", observation: connect.PeerCapabilityObservation{AckObserved: true, WindowAdvertised: true}, found: true, ackSeenAt: longAgo, e2e: e2eNotTested, window: windowResizable},
		{name: "window not tested", observation: connect.PeerCapabilityObservation{E2eAttempted: true, E2eFirstAttemptTime: longAgo}, found: true, e2e: e2ePending, window: windowNotTested},
		{name: "no record", found: false, ackSeenAt: longAgo, e2e: e2eNotTested, window: windowNotTested},
	} {
		read := readObservation(c.observation, c.found, readingTestNow, c.ackSeenAt, readingTestTlsTimeout)
		if read.e2e != c.e2e || read.window != c.window {
			t.Errorf("%s: read %s/%s, want %s/%s", c.name, read.e2e, read.window, c.e2e, c.window)
		}
	}
}

// An unanswered ClientHello is no responder exactly once the establishment
// window has passed since both the first attempt and the first observed
// acknowledgement, and pending a nanosecond before either; without an
// acknowledgement it stays pending however long it has waited.
func TestNoResponderWaitsTheWholeEstablishmentWindow(t *testing.T) {
	attempt := readingTestNow.Add(-readingTestTlsTimeout)
	observation := connect.PeerCapabilityObservation{E2eAttempted: true, E2eFirstAttemptTime: attempt, AckObserved: true}
	for _, c := range []struct {
		name      string
		attempt   time.Time
		ackSeenAt time.Time
		want      e2eReading
	}{
		{name: "both exactly a window ago", attempt: attempt, ackSeenAt: attempt, want: e2eNoResponder},
		{name: "attempt a nanosecond short", attempt: attempt.Add(time.Nanosecond), ackSeenAt: attempt, want: e2ePending},
		{name: "acknowledgement a nanosecond short", attempt: attempt, ackSeenAt: attempt.Add(time.Nanosecond), want: e2ePending},
		// an earlier client's ClientHello does not shorten the window of the
		// client whose pings were acknowledged
		{name: "attempt long before the first acknowledgement", attempt: readingTestNow.Add(-time.Hour), ackSeenAt: readingTestNow.Add(-time.Second), want: e2ePending},
		{name: "never acknowledged", attempt: readingTestNow.Add(-time.Hour), ackSeenAt: time.Time{}, want: e2ePending},
	} {
		observation.E2eFirstAttemptTime = c.attempt
		if read := readObservation(observation, true, readingTestNow, c.ackSeenAt, readingTestTlsTimeout); read.e2e != c.want {
			t.Errorf("%s: read %s, want %s", c.name, read.e2e, c.want)
		}
	}
}

// Over every combination of the observation's flags, found or not, inside
// or past the establishment window, with or without a seen acknowledgement:
// a capability that was not tested is never recorded, as a negative or
// otherwise, and what is recorded says only what the provider did.
func TestUntestedCapabilitiesAreNeverRecorded(t *testing.T) {
	recordedCount := 0
	negativeCount := 0
	for flags := range 1 << 6 {
		for _, found := range []bool{false, true} {
			for _, attemptAge := range []time.Duration{time.Second, readingTestTlsTimeout, 2 * readingTestTlsTimeout} {
				for _, ackAge := range []time.Duration{-1, time.Second, 2 * readingTestTlsTimeout} {
					observation := connect.PeerCapabilityObservation{
						E2eAttempted:        flags&1 != 0,
						E2eNegotiated:       flags&2 != 0,
						E2eSealed:           flags&4 != 0,
						PostQuantum:         flags&8 != 0,
						AckObserved:         flags&16 != 0,
						WindowAdvertised:    flags&32 != 0,
						E2eFirstAttemptTime: readingTestNow.Add(-attemptAge),
						KeyExchange:         "X25519MLKEM768",
					}
					if observation.WindowAdvertised {
						observation.MaxReceiveWindowByteCount = 1024 * 1024
					}
					ackSeenAt := time.Time{}
					if 0 <= ackAge {
						ackSeenAt = readingTestNow.Add(-ackAge)
					}
					read := readObservation(observation, found, readingTestNow, ackSeenAt, readingTestTlsTimeout)
					evidence, recorded := read.evidence(observation, readingTestNow)
					acknowledged := found && (observation.AckObserved || observation.WindowAdvertised)
					negotiated := found && (observation.E2eNegotiated || observation.E2eSealed)
					answeredOrTimedOut := negotiated || (found && observation.E2eAttempted && !ackSeenAt.IsZero() &&
						!readingTestNow.Before(observation.E2eFirstAttemptTime.Add(readingTestTlsTimeout)) &&
						!readingTestNow.Before(ackSeenAt.Add(readingTestTlsTimeout)))
					if recorded != (acknowledged && answeredOrTimedOut) {
						t.Fatalf("flags %06b found %t attempt %s ack %s: recorded %t, want %t (read %+v)", flags, found, attemptAge, ackAge, recorded, acknowledged && answeredOrTimedOut, read)
					}
					if !recorded {
						if evidence != (model.ProviderModernEvidence{}) {
							t.Fatalf("flags %06b: an unrecorded reading carries evidence %+v", flags, evidence)
						}
						continue
					}
					recordedCount += 1
					if !evidence.E2eNegotiated {
						negativeCount += 1
					}
					if evidence.E2eNegotiated != negotiated ||
						evidence.PostQuantum != (negotiated && observation.PostQuantum) ||
						evidence.WindowAdvertised != observation.WindowAdvertised ||
						(evidence.KeyExchange != "") != negotiated ||
						(evidence.MaxReceiveWindowByteCount != 0) != observation.WindowAdvertised ||
						!evidence.ObservedAt.Equal(readingTestNow) {
						t.Fatalf("flags %06b: recorded %+v from %+v", flags, evidence, observation)
					}
				}
			}
		}
	}
	// the table reaches every kind of record
	if recordedCount == 0 || negativeCount == 0 || negativeCount == recordedCount {
		t.Fatalf("recorded %d readings, %d of them without a session", recordedCount, negativeCount)
	}
}

// A measurement stops once the reading is conclusive, except that a
// negotiated session that has not sealed has the seal grace, counted from
// when the measurement first saw it negotiate, to seal.
func TestReadingCompletesAfterTheSealGrace(t *testing.T) {
	grace := 10 * time.Second
	unsealed := reading{e2e: e2ePostQuantumUnsealed, window: windowResizable}
	negotiatedAt := readingTestNow.Add(-grace)
	if !unsealed.complete(readingTestNow, negotiatedAt, grace) {
		t.Error("an unsealed session did not stand at the end of its grace")
	}
	if unsealed.complete(readingTestNow.Add(-time.Nanosecond), negotiatedAt, grace) {
		t.Error("an unsealed session stood before its grace ended")
	}
	if unsealed.complete(readingTestNow, time.Time{}, grace) {
		t.Error("an unsealed session stood without a negotiation time")
	}
	for _, complete := range []reading{
		{e2e: e2ePostQuantumSealed, window: windowResizable},
		{e2e: e2eClassicalSealed, window: windowFixed},
		{e2e: e2eNoResponder, window: windowFixed},
	} {
		if !complete.complete(readingTestNow, time.Time{}, grace) {
			t.Errorf("%+v did not complete at once", complete)
		}
	}
	for _, incomplete := range []reading{
		{e2e: e2ePostQuantumSealed, window: windowNotTested},
		{e2e: e2ePending, window: windowResizable},
		{e2e: e2eNotTested, window: windowFixed},
	} {
		if incomplete.complete(readingTestNow, negotiatedAt, 0) {
			t.Errorf("%+v completed", incomplete)
		}
	}
}
