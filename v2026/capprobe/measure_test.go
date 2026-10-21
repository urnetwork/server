// One provider's measurement: when it stops, what it reads and records, and
// that its tunnel is torn down once.
package capprobe

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// One provider's measurement end to end on the bubble's clock: when it
// stops, what it reads, what it would record, and that its tunnel is torn
// down exactly once.
func measureFakeProvider(t *testing.T, ctx context.Context, provider fakeProvider) (measurement, *fakeTunnel) {
	t.Helper()
	store := newFakeStore(1)
	fleet := newFakeFleet()
	providerId := store.clientIds[0]
	fleet.providers[providerId] = provider
	runner := newFakeRunner(t, DefaultSettings(), store, fleet)
	result := runner.measure(ctx, providerId)
	if len(fleet.opened) != 1 {
		t.Fatalf("opened %d tunnels, want 1", len(fleet.opened))
	}
	tunnel := fleet.opened[0]
	if closes := tunnel.closes(); closes != 1 {
		t.Fatalf("closed the tunnel %d times, want once", closes)
	}
	return result, tunnel
}

// A modern provider's measurement ends at the first read after its session
// seals: acknowledged at one second, negotiated at two, sealed at three.
func TestModernProviderMeasurementEndsAtTheSeal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		result, _ := measureFakeProvider(t, t.Context(), fakeModernProvider())
		if result.elapsed != 3*time.Second {
			t.Fatalf("measured for %s, want 3s", result.elapsed)
		}
		if result.reading != (reading{e2e: e2ePostQuantumSealed, window: windowResizable}) || !result.recordable {
			t.Fatalf("read %+v recordable %t", result.reading, result.recordable)
		}
		evidence := result.evidence
		if !evidence.E2eNegotiated || !evidence.PostQuantum || evidence.KeyExchange != "X25519MLKEM768" ||
			!evidence.WindowAdvertised || evidence.MaxReceiveWindowByteCount != 4*1024*1024 {
			t.Fatalf("evidence %+v", evidence)
		}
		if result.outcome() != outcomeRecordedModern {
			t.Fatalf("outcome %s", result.outcome())
		}
	})
}

// A provider without the responder is read only once its ClientHello has
// gone unanswered for the whole establishment window after its first
// acknowledgement: acknowledged at one second, so at 61, never at 60.
func TestNoResponderIsReadOnlyAfterTheEstablishmentWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		result, _ := measureFakeProvider(t, t.Context(), fakeLegacyProvider())
		if result.elapsed != 61*time.Second {
			t.Fatalf("measured for %s, want 61s", result.elapsed)
		}
		if result.reading != (reading{e2e: e2eNoResponder, window: windowFixed}) || !result.recordable {
			t.Fatalf("read %+v recordable %t", result.reading, result.recordable)
		}
		if result.evidence.E2eNegotiated || result.evidence.PostQuantum || result.evidence.WindowAdvertised || result.evidence.KeyExchange != "" {
			t.Fatalf("evidence %+v", result.evidence)
		}
		if result.outcome() != outcomeRecordedNotPostQuantum {
			t.Fatalf("outcome %s", result.outcome())
		}
	})
}

// A provider that never acknowledges is measured until the measurement's
// timeout and records nothing: silence is not evidence.
func TestSilentProviderRecordsNothingAtTheTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		result, _ := measureFakeProvider(t, t.Context(), fakeSilentProvider())
		if result.elapsed != DefaultSettings().measureTimeout() {
			t.Fatalf("measured for %s, want the %s timeout", result.elapsed, DefaultSettings().measureTimeout())
		}
		if result.recordable || result.reading.window != windowNotTested || result.outcome() != outcomeNoAck {
			t.Fatalf("read %+v recordable %t outcome %s", result.reading, result.recordable, result.outcome())
		}
	})
}

// A provider that acknowledges only late, so its ClientHello is still within
// the establishment window at the timeout, holds its slot no longer than the
// timeout and records nothing, though its window was read.
func TestPendingSessionIsBoundedByTheTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		provider := fakeLegacyProvider()
		provider.ackAt = 100 * time.Second
		provider.window = true
		result, _ := measureFakeProvider(t, t.Context(), provider)
		if result.elapsed != DefaultSettings().measureTimeout() {
			t.Fatalf("measured for %s, want the %s timeout", result.elapsed, DefaultSettings().measureTimeout())
		}
		if result.recordable || result.reading != (reading{e2e: e2ePending, window: windowResizable}) || result.outcome() != outcomeE2ePending {
			t.Fatalf("read %+v recordable %t outcome %s", result.reading, result.recordable, result.outcome())
		}
	})
}

// A session that negotiates and never seals, the shape of a provider that
// cannot resolve the probe's key, stands as unauthenticated after the seal
// grace: negotiated at two seconds, so read at twelve. The model records it
// as negotiated with its group.
func TestUnsealedSessionStandsAfterTheSealGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		provider := fakeModernProvider()
		provider.sealAt = fakeNever
		result, _ := measureFakeProvider(t, t.Context(), provider)
		if want := 2*time.Second + DefaultSettings().sealGrace(); result.elapsed != want {
			t.Fatalf("measured for %s, want %s", result.elapsed, want)
		}
		if result.reading != (reading{e2e: e2ePostQuantumUnsealed, window: windowResizable}) || !result.recordable ||
			!result.evidence.E2eNegotiated || !result.evidence.PostQuantum {
			t.Fatalf("read %+v evidence %+v", result.reading, result.evidence)
		}
	})
}

// A tunnel that loses its provider ends the measurement at once, and says
// so.
func TestLostTunnelEndsTheMeasurement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore(1)
		fleet := newFakeFleet()
		providerId := store.clientIds[0]
		fleet.providers[providerId] = fakeSilentProvider()
		runner := newFakeRunner(t, DefaultSettings(), store, fleet)
		done := make(chan measurement)
		go func() { done <- runner.measure(t.Context(), providerId) }()
		synctest.Wait()
		time.Sleep(5 * time.Second)
		tunnel := fleet.tunnel(0)
		close(tunnel.lostDone)
		result := <-done
		if result.elapsed != 5*time.Second || result.recordable || result.outcome() != outcomeTunnelLost {
			t.Fatalf("measured for %s, recordable %t, outcome %s", result.elapsed, result.recordable, result.outcome())
		}
		if tunnel.closes() != 1 {
			t.Fatal("the lost tunnel was not torn down once")
		}
	})
}

// A tunnel lost by the final read, its path gone or its window client
// replaced (review F6), concludes no unanswered ClientHello. Acknowledged at
// one second, with its ClientHello accepted at 2.5, a provider's
// establishment window runs out at 62.5 s, between the read at 62 s and a
// loss at 62.7 s: the window may be a replaced client's, so the measurement
// ends at the loss with nothing recorded. A session negotiated by then still
// stands.
func TestLostTunnelConcludesNoUnansweredClientHello(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		unanswered := fakeLegacyProvider()
		unanswered.attemptAt = 2500 * time.Millisecond
		unsealed := fakeModernProvider()
		unsealed.sealAt = fakeNever
		for _, c := range []struct {
			name       string
			provider   fakeProvider
			lostAt     time.Duration
			reading    reading
			recordable bool
			outcome    string
		}{
			{name: "unanswered", provider: unanswered, lostAt: 62700 * time.Millisecond, reading: reading{e2e: e2ePending, window: windowFixed}, outcome: outcomeTunnelLost},
			{name: "negotiated", provider: unsealed, lostAt: 5 * time.Second, reading: reading{e2e: e2ePostQuantumUnsealed, window: windowResizable}, recordable: true, outcome: outcomeRecordedModern},
		} {
			store := newFakeStore(1)
			fleet := newFakeFleet()
			providerId := store.clientIds[0]
			fleet.providers[providerId] = c.provider
			runner := newFakeRunner(t, DefaultSettings(), store, fleet)
			done := make(chan measurement)
			go func() { done <- runner.measure(t.Context(), providerId) }()
			synctest.Wait()
			time.Sleep(c.lostAt)
			close(fleet.tunnel(0).lostDone)
			result := <-done
			if result.elapsed != c.lostAt || !result.lost || result.reading != c.reading || result.recordable != c.recordable || result.outcome() != c.outcome {
				t.Errorf("%s: measured for %s, lost %t, read %+v, recordable %t, outcome %s; want %s, %+v, %t, %s",
					c.name, result.elapsed, result.lost, result.reading, result.recordable, result.outcome(), c.lostAt, c.reading, c.recordable, c.outcome)
			}
		}
	})
}

// The pass's end cancels a measurement: an inconclusive one is canceled and
// records nothing, a conclusive one still stands. Both tear down.
func TestCanceledMeasurementKeepsOnlyAConclusiveReading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, c := range []struct {
			provider   fakeProvider
			recordable bool
			outcome    string
		}{
			{provider: fakeSilentProvider(), recordable: false, outcome: outcomeCanceled},
			// negotiated unsealed at two seconds, still in its seal grace at
			// the cancel
			{provider: func() fakeProvider {
				provider := fakeModernProvider()
				provider.sealAt = fakeNever
				return provider
			}(), recordable: true, outcome: outcomeRecordedModern},
		} {
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				time.Sleep(5 * time.Second)
				cancel()
			}()
			result, _ := measureFakeProvider(t, ctx, c.provider)
			if result.elapsed != 5*time.Second || result.recordable != c.recordable || result.outcome() != c.outcome {
				t.Errorf("measured for %s, recordable %t, outcome %s; want 5s, %t, %s", result.elapsed, result.recordable, result.outcome(), c.recordable, c.outcome)
			}
			cancel()
		}
	})
}

// A tunnel that cannot be opened is a local failure with no reading and
// nothing to tear down. A tunnel is opened under the pass's context, never
// one the measurement's timeout ends, so its teardown can still retire it.
func TestTunnelFailureAndTunnelContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore(2)
		fleet := newFakeFleet()
		failing, opening := store.clientIds[0], store.clientIds[1]
		fleet.openErr[failing] = errors.New("synthetic tunnel failure")
		fleet.providers[opening] = fakeModernProvider()
		runner := newFakeRunner(t, DefaultSettings(), store, fleet)
		result := runner.measure(t.Context(), failing)
		if result.opened || result.recordable || result.outcome() != outcomeTunnelFailed || result.openErr == nil {
			t.Fatalf("a failed open measured %+v", result)
		}
		runner.measure(t.Context(), opening)
		for _, ctx := range fleet.openContexts {
			if _, bounded := ctx.Deadline(); bounded {
				t.Fatal("a tunnel was opened under a bounded measurement context")
			}
		}
	})
}
