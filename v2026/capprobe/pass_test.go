// A pass's concurrency, admission, failure and metrics.
package capprobe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A pass measures at most concurrency providers at once and starts the rest
// as tunnels close: eight modern providers, three at a time, each three
// seconds, so the pass takes three rounds and never holds a fourth tunnel.
func TestPassMeasuresAtMostConcurrencyAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore(8)
		fleet := newFakeFleet()
		for _, clientId := range store.clientIds {
			fleet.providers[clientId] = fakeModernProvider()
		}
		runner := newFakeRunner(t, fakeSettings(3, 8, 8, 1), store, fleet)
		startedAt := time.Now()
		_, result, err := runner.run(t.Context(), Sweep{})
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, fleet.maxOpenTunnels(), 3)
		connect.AssertEqual(t, fleet.openTunnels(), 0)
		connect.AssertEqual(t, result.Started, 8)
		connect.AssertEqual(t, result.Recorded, 8)
		connect.AssertEqual(t, time.Since(startedAt), 9*time.Second)
	})
}

// A provider starts only while the pass's deadline leaves a whole
// measurement, its teardown and the write: with room for exactly one round,
// the first two start and the rest are left for the next pass, which resumes
// after the last one started. Every tunnel opened is closed.
func TestPassStartsNoMeasurementItCannotFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore(6)
		ids := store.clientIds
		fleet := newFakeFleet()
		for _, clientId := range ids {
			fleet.providers[clientId] = fakeModernProvider()
		}
		settings := fakeSettings(2, 6, 6, 1)
		runner := newFakeRunner(t, settings, store, fleet)
		ctx, cancel := context.WithTimeout(t.Context(), settings.admissionReserve()+time.Second)
		defer cancel()
		next, result, err := runner.run(ctx, Sweep{})
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Started, 2)
		connect.AssertEqual(t, sortedIds(fleet.openedProviderIds()), ids[:2])
		connect.AssertEqual(t, next.Cursor, ids[1])
		connect.AssertEqual(t, result.SweepCompleted, false)
		connect.AssertEqual(t, fleet.openTunnels(), 0)
	})
}

// A failed evidence write stops further starts and fails the pass once the
// started measurements finish, leaving the sweep where it was so the retry
// selects the same page; the providers recorded are no longer due then.
func TestPassFailsOnAFailedWriteAndLeavesTheSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore(4)
		ids := store.clientIds
		fleet := newFakeFleet()
		for _, clientId := range ids {
			fleet.providers[clientId] = fakeModernProvider()
		}
		writeErr := errors.New("synthetic write failure")
		store.recordErr[ids[0]] = writeErr
		// the second provider measures past the first's failed write
		slow := fakeModernProvider()
		slow.sealAt = 10 * time.Second
		fleet.providers[ids[1]] = slow
		runner := newFakeRunner(t, fakeSettings(2, 4, 4, 1), store, fleet)
		sweep := Sweep{Cursor: server.Id{}, StartedAt: time.Now()}
		failedRecords := testutil.ToFloat64(providersTotal.WithLabelValues(outcomeRecordFailed))
		next, result, err := runner.run(t.Context(), sweep)
		if !errors.Is(err, writeErr) {
			t.Fatalf("the pass returned %v, want the write failure", err)
		}
		connect.AssertEqual(t, next, sweep)
		connect.AssertEqual(t, result.Started, 2)
		connect.AssertEqual(t, store.recordedIds(), []server.Id{ids[1]})
		connect.AssertEqual(t, testutil.ToFloat64(providersTotal.WithLabelValues(outcomeRecordFailed))-failedRecords, float64(1))
	})
}

// A pass over providers that never acknowledge records nothing: no write is
// even attempted, and every one counts as no_ack with its readings.
func TestPassRecordsNothingUntested(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore(3)
		fleet := newFakeFleet()
		runner := newFakeRunner(t, fakeSettings(3, 3, 3, 1), store, fleet)
		noAck := testutil.ToFloat64(providersTotal.WithLabelValues(outcomeNoAck))
		untestedWindows := testutil.ToFloat64(readingsTotal.WithLabelValues(capabilityWindow, string(windowNotTested)))
		_, result, err := runner.run(t.Context(), Sweep{})
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Started, 3)
		connect.AssertEqual(t, result.Recorded, 0)
		connect.AssertEqual(t, len(store.recordedIds()), 0)
		connect.AssertEqual(t, len(store.evidence), 0)
		connect.AssertEqual(t, testutil.ToFloat64(providersTotal.WithLabelValues(outcomeNoAck))-noAck, float64(3))
		connect.AssertEqual(t, testutil.ToFloat64(readingsTotal.WithLabelValues(capabilityWindow, string(windowNotTested)))-untestedWindows, float64(3))
	})
}

// One pass's metrics: the pass's result, each selection stage, each
// measurement's outcome and readings, its duration, the sweep it completed,
// and nothing left in flight.
func TestPassMetrics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore(4)
		ids := store.clientIds
		store.notPublic[ids[3]] = true
		store.ineligible[ids[2]] = true
		fleet := newFakeFleet()
		fleet.providers[ids[0]] = fakeModernProvider()
		fleet.providers[ids[1]] = fakeLegacyProvider()
		runner := newFakeRunner(t, fakeSettings(2, 4, 8, 1), store, fleet)
		read := func() map[string]float64 {
			return map[string]float64{
				"measured":            testutil.ToFloat64(passesTotal.WithLabelValues(PassResultMeasured)),
				"scanned":             testutil.ToFloat64(selectionTotal.WithLabelValues(selectionScanned)),
				"due":                 testutil.ToFloat64(selectionTotal.WithLabelValues(selectionDue)),
				"eligible":            testutil.ToFloat64(selectionTotal.WithLabelValues(selectionEligible)),
				"started":             testutil.ToFloat64(selectionTotal.WithLabelValues(selectionStarted)),
				"unguarded":           testutil.ToFloat64(selectionTotal.WithLabelValues(selectionUnguarded)),
				"modern":              testutil.ToFloat64(providersTotal.WithLabelValues(outcomeRecordedModern)),
				"not post-quantum":    testutil.ToFloat64(providersTotal.WithLabelValues(outcomeRecordedNotPostQuantum)),
				"sealed post-quantum": testutil.ToFloat64(readingsTotal.WithLabelValues(capabilityE2e, string(e2ePostQuantumSealed))),
				"no responder":        testutil.ToFloat64(readingsTotal.WithLabelValues(capabilityE2e, string(e2eNoResponder))),
				"resizable":           testutil.ToFloat64(readingsTotal.WithLabelValues(capabilityWindow, string(windowResizable))),
				"fixed":               testutil.ToFloat64(readingsTotal.WithLabelValues(capabilityWindow, string(windowFixed))),
				"sweeps":              testutil.ToFloat64(sweepsTotal),
				"in flight":           testutil.ToFloat64(inFlight),
				"measurements":        float64(measureSampleCount()),
			}
		}
		before := read()
		_, result, err := runner.run(t.Context(), Sweep{})
		if err != nil || !result.SweepCompleted {
			t.Fatalf("result %+v err %v", result, err)
		}
		after := read()
		for name, delta := range map[string]float64{
			"measured": 1, "scanned": 4, "due": 3, "eligible": 2, "started": 2, "unguarded": 0,
			"modern": 1, "not post-quantum": 1, "sealed post-quantum": 1, "no responder": 1,
			"resizable": 1, "fixed": 1, "sweeps": 1, "in flight": 0, "measurements": 2,
		} {
			if after[name]-before[name] != delta {
				t.Errorf("%s moved by %v, want %v", name, after[name]-before[name], delta)
			}
		}
		connect.AssertEqual(t, testutil.ToFloat64(lastPassTimestampSeconds), float64(time.Now().Unix()))
		connect.AssertEqual(t, testutil.ToFloat64(lastSweepSeconds), float64(61))
	})
}

// Without a published url-success guard every country is exempt, so a pass
// measures providers the score export keeps out of online for url success
// (FP2-MODERN.md). A pass that judged any eligibility read without the guard
// counts once in the selection's unguarded stage and logs one line, however
// many of its reads lacked it, a selection that fails after such a read
// included; a pass whose reads all had the guard counts and logs nothing.
// Each pass here reads twice: its first chunk holds no eligible provider.
func TestPassCountsAndLogsAnUnguardedSelectionOnce(t *testing.T) {
	for _, c := range []struct {
		name string
		// whether each eligibility read lacks the guard
		unguardedReads []bool
		// the second read fails, and the selection with it
		secondReadFails bool
		unguarded       int
	}{
		{name: "both reads without the guard", unguardedReads: []bool{true, true}, unguarded: 1},
		{name: "the first read without the guard", unguardedReads: []bool{true, false}, unguarded: 1},
		{name: "the second read without the guard", unguardedReads: []bool{false, true}, unguarded: 1},
		{name: "both reads with the guard", unguardedReads: []bool{false, false}, unguarded: 0},
		{name: "a failed selection after a read without the guard", unguardedReads: []bool{true}, secondReadFails: true, unguarded: 1},
	} {
		synctest.Test(t, func(t *testing.T) {
			store := newFakeStore(200)
			ids := store.clientIds
			for _, clientId := range ids[:minEligibilityChunk] {
				store.ineligible[clientId] = true
			}
			fleet := newFakeFleet()
			for _, clientId := range ids {
				fleet.providers[clientId] = fakeModernProvider()
			}
			runner := newFakeRunner(t, fakeSettings(2, 2, 200, 1), store, fleet)
			logs := []string{}
			runner.infof = func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			}
			reads := 0
			runner.filterEligible = func(ctx context.Context, clientIds []server.Id) (model.ProviderCapabilityProbeEligibility, error) {
				reads += 1
				if c.secondReadFails && reads == 2 {
					return model.ProviderCapabilityProbeEligibility{}, errors.New("synthetic eligibility read failure")
				}
				eligibility, err := store.filterEligible(ctx, clientIds)
				eligibility.Unguarded = c.unguardedReads[reads-1]
				return eligibility, err
			}
			unguarded := testutil.ToFloat64(selectionTotal.WithLabelValues(selectionUnguarded))
			_, result, err := runner.run(t.Context(), Sweep{})
			if (err != nil) != c.secondReadFails {
				t.Fatalf("%s: the pass returned %+v, %v", c.name, result, err)
			}
			connect.AssertEqual(t, reads, 2)
			if delta := testutil.ToFloat64(selectionTotal.WithLabelValues(selectionUnguarded)) - unguarded; delta != float64(c.unguarded) {
				t.Errorf("%s: the unguarded stage moved by %v, want %d", c.name, delta, c.unguarded)
			}
			if len(logs) != c.unguarded {
				t.Fatalf("%s: logs %q, want %d", c.name, logs, c.unguarded)
			}
			for _, line := range logs {
				if !strings.Contains(line, "without a published url-success guard") {
					t.Errorf("%s: log %q does not say what the pass lacked", c.name, line)
				}
			}
		})
	}
}

// The measurement histogram's observation count.
func measureSampleCount() uint64 {
	metric := &dto.Metric{}
	if err := measureSeconds.Write(metric); err != nil {
		panic(err)
	}
	return metric.GetHistogram().GetSampleCount()
}

// Every bounded label set is published from the start, zero included.
func TestMetricsPublishEveryLabelSet(t *testing.T) {
	connect.AssertEqual(t, testutil.CollectAndCount(passesTotal), len(passResults))
	connect.AssertEqual(t, testutil.CollectAndCount(selectionTotal), len(selectionStages))
	connect.AssertEqual(t, testutil.CollectAndCount(providersTotal), len(outcomes))
	connect.AssertEqual(t, testutil.CollectAndCount(readingsTotal), len(e2eReadings)+len(windowReadings))
	connect.AssertEqual(t, testutil.CollectAndCount(readinessFailuresTotal), len(readinessSteps))
}

// Only a pass with providers to measure needs tunnels, and so a prober
// shard: one that waits for its sweep's next start, or finds nothing due and
// eligible, finishes without any and still moves the sweep; one with
// providers and no tunnels fails rather than skip them.
func TestOnlyAPassWithProvidersNeedsTunnels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore(3)
		for _, clientId := range store.clientIds {
			store.ineligible[clientId] = true
		}
		selecting := newFakeRunner(t, fakeSettings(1, 2, 2, 1), store, newFakeFleet())
		selecting.open = nil

		waiting, err := selecting.begin(t.Context(), Sweep{NextSweepAt: time.Now().Add(time.Hour)})
		if err != nil || waiting.NeedsTunnels() {
			t.Fatalf("a waiting pass needs tunnels: %v", err)
		}
		_, result, err := selecting.finish(t.Context(), waiting)
		if err != nil || !result.Waiting {
			t.Fatalf("a waiting pass finished %+v, %v", result, err)
		}

		empty, err := selecting.begin(t.Context(), Sweep{})
		if err != nil || empty.NeedsTunnels() {
			t.Fatalf("a pass with nothing eligible needs tunnels: %v", err)
		}
		next, result, err := selecting.finish(t.Context(), empty)
		if err != nil || result.Started != 0 || result.Scanned != 2 || next.Cursor != store.clientIds[1] {
			t.Fatalf("an empty pass finished %+v at %+v, %v", result, next, err)
		}

		store.ineligible[store.clientIds[2]] = false
		measuring, err := selecting.begin(t.Context(), next)
		if err != nil || !measuring.NeedsTunnels() {
			t.Fatalf("a pass with a provider needs no tunnels: %v", err)
		}
		if _, _, err := selecting.finish(t.Context(), measuring); err == nil {
			t.Fatal("a pass with a provider finished without tunnels")
		}
	})
}

// A pass with settings that switch the probe off, or do not validate, does
// not run.
func TestPassRefusesDisabledOrInvalidSettings(t *testing.T) {
	store := newFakeStore(1)
	fleet := newFakeFleet()
	disabled := DefaultSettings()
	disabled.Enabled = false
	invalid := DefaultSettings()
	invalid.Enabled = true
	invalid.Concurrency = 0
	for _, settings := range []Settings{disabled, invalid} {
		runner := &passRunner{settings: settings, readPage: store.readPage, filterEligible: store.filterEligible, open: fleet.openTunnel, record: store.record, now: time.Now, after: time.After}
		if _, _, err := runner.run(context.Background(), Sweep{}); err == nil {
			t.Errorf("a pass ran with %+v", settings)
		}
	}
	connect.AssertEqual(t, store.pageReads, 0)
}
