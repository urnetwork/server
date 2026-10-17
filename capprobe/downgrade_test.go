// The guards that keep a passing fault from taking modern providers out of
// Quality and Speed: downgrades held until a later sweep confirms them, and
// the breaker of a pass that negotiates nothing.
package capprobe

import (
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Runs passes of sweep until it completes, and returns the sweep after it
// and the results of its passes.
func runFakeSweep(t *testing.T, runner *passRunner, sweep Sweep) (Sweep, []*PassResult) {
	t.Helper()
	results := []*PassResult{}
	for range 8 {
		next, result, err := runner.run(t.Context(), sweep)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
		sweep = next
		if result.SweepCompleted {
			return sweep, results
		}
	}
	t.Fatalf("the sweep did not complete: %+v", results)
	return sweep, results
}

// The sum of one count over a sweep's passes.
func sumPassResults(results []*PassResult, count func(*PassResult) int) int {
	sum := 0
	for _, result := range results {
		sum += count(result)
	}
	return sum
}

// Root cause (review F8, hysteresis): one negative reading replaced a
// provider's modern evidence, so one transient failure, a provider
// restarting or a fault on the probe's side, took it out of Quality and
// Speed once the gate is on. A negative reading over fresh modern evidence is
// held back and carried by the sweep, the provider stays due, and only a
// negative reading in a later sweep confirms and records the downgrade. A
// modern reading in a later sweep retires the held one instead.
func TestOneNegativeReadingDoesNotDowngradeFreshModernEvidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, laterReadsModern := range []bool{false, true} {
			store := newFakeStore(2)
			ids := store.clientIds
			downgrading, modern := ids[0], ids[1]
			modernEvidence := model.ProviderModernEvidence{
				E2eNegotiated:             true,
				KeyExchange:               "X25519MLKEM768",
				PostQuantum:               true,
				WindowAdvertised:          true,
				MaxReceiveWindowByteCount: 4 * 1024 * 1024,
				// due for a refresh, and fresh
				ObservedAt: time.Now().Add(-80 * time.Hour),
			}
			store.evidence[downgrading] = modernEvidence
			fleet := newFakeFleet()
			fleet.providers[downgrading] = fakeLegacyProvider()
			// a provider that negotiates in every sweep, so no pass trips the
			// breaker: its evidence is due again a sweep later
			fleet.providers[modern] = fakeModernProvider()
			settings := fakeSettings(2, 2, 2, 1)
			settings.ModernRefreshHours = 1
			runner := newFakeRunner(t, settings, store, fleet)
			heldBefore := testutil.ToFloat64(downgradesTotal.WithLabelValues(downgradeHeld))
			confirmedBefore := testutil.ToFloat64(downgradesTotal.WithLabelValues(downgradeConfirmed))
			heldOutcomesBefore := testutil.ToFloat64(providersTotal.WithLabelValues(outcomeDowngradeHeld))

			sweep, results := runFakeSweep(t, runner, Sweep{})
			if !reflect.DeepEqual(store.evidence[downgrading], modernEvidence) {
				t.Fatalf("one negative reading replaced fresh modern evidence with %+v", store.evidence[downgrading])
			}
			heldDowngrade := HeldDowngrade{ClientId: downgrading, SweepStartedAt: sweep.NextSweepAt.Add(-settings.minSweepInterval())}
			connect.AssertEqual(t, sweep.HeldDowngrades, []HeldDowngrade{heldDowngrade})
			connect.AssertEqual(t, sumPassResults(results, func(result *PassResult) int { return result.DowngradesHeld }), 1)
			connect.AssertEqual(t, testutil.ToFloat64(downgradesTotal.WithLabelValues(downgradeHeld))-heldBefore, float64(1))
			connect.AssertEqual(t, testutil.ToFloat64(providersTotal.WithLabelValues(outcomeDowngradeHeld))-heldOutcomesBefore, float64(1))

			// the next sweep measures the provider again
			if laterReadsModern {
				fleet.providers[downgrading] = fakeModernProvider()
			}
			time.Sleep(sweep.NextSweepAt.Sub(time.Now()))
			sweep, results = runFakeSweep(t, runner, sweep)
			connect.AssertEqual(t, len(sweep.HeldDowngrades), 0)
			if laterReadsModern {
				if modernOutcomeOf(store.evidence[downgrading]) != model.ProviderModernOutcomeModern || !store.evidence[downgrading].ObservedAt.After(modernEvidence.ObservedAt) {
					t.Fatalf("a modern reading did not refresh the held provider's evidence: %+v", store.evidence[downgrading])
				}
				connect.AssertEqual(t, testutil.ToFloat64(downgradesTotal.WithLabelValues(downgradeConfirmed))-confirmedBefore, float64(0))
			} else {
				if outcome := modernOutcomeOf(store.evidence[downgrading]); outcome != model.ProviderModernOutcomeNotPostQuantum {
					t.Fatalf("a confirmed downgrade recorded %s", outcome)
				}
				connect.AssertEqual(t, sumPassResults(results, func(result *PassResult) int { return result.DowngradesConfirmed }), 1)
				connect.AssertEqual(t, testutil.ToFloat64(downgradesTotal.WithLabelValues(downgradeConfirmed))-confirmedBefore, float64(1))
			}
			connect.AssertEqual(t, len(store.held), 1)
		}
	})
}

// Root cause (review F8, breaker): a fault on the probe's own side, its
// sessions or its responder handling broken, shows as every provider
// without a session, and each pass recorded those negatives fleet-wide. A
// pass that negotiates no end-to-end session with any provider records none
// of its negative readings and counts the breaker's trip; the providers stay
// due. Negative readings that finish before the pass's first negotiation
// wait for it, and are recorded once it comes.
func TestAPassThatNegotiatesNothingWithholdsItsNegatives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore(3)
		fleet := newFakeFleet()
		for _, clientId := range store.clientIds {
			fleet.providers[clientId] = fakeLegacyProvider()
		}
		runner := newFakeRunner(t, fakeSettings(3, 3, 3, 1), store, fleet)
		tripsBefore := testutil.ToFloat64(breakerTripsTotal)
		withheldBefore := testutil.ToFloat64(providersTotal.WithLabelValues(outcomeNegativeWithheld))
		_, result, err := runner.run(t.Context(), Sweep{})
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Started, 3)
		connect.AssertEqual(t, result.Recorded, 0)
		connect.AssertEqual(t, result.NegativesWithheld, 3)
		connect.AssertEqual(t, len(store.recordedIds()), 0)
		connect.AssertEqual(t, testutil.ToFloat64(breakerTripsTotal)-tripsBefore, float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(providersTotal.WithLabelValues(outcomeNegativeWithheld))-withheldBefore, float64(3))

		// two negatives finish at 61 s and wait for the session a third
		// provider, first acknowledged at 31 s, negotiates at 65 s; then all
		// three are recorded
		store = newFakeStore(3)
		fleet = newFakeFleet()
		lateModern := fakeModernProvider()
		lateModern.attemptAt = 30 * time.Second
		lateModern.ackAt = 31 * time.Second
		lateModern.negotiateAt = 65 * time.Second
		lateModern.sealAt = 66 * time.Second
		fleet.providers[store.clientIds[0]] = fakeLegacyProvider()
		fleet.providers[store.clientIds[1]] = lateModern
		fleet.providers[store.clientIds[2]] = fakeLegacyProvider()
		runner = newFakeRunner(t, fakeSettings(3, 3, 3, 1), store, fleet)
		tripsBefore = testutil.ToFloat64(breakerTripsTotal)
		_, result, err = runner.run(t.Context(), Sweep{})
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.Recorded, 3)
		connect.AssertEqual(t, result.NegativesWithheld, 0)
		connect.AssertEqual(t, sortedIds(store.recordedIds()), store.clientIds)
		connect.AssertEqual(t, testutil.ToFloat64(breakerTripsTotal)-tripsBefore, float64(0))
		for _, clientId := range []server.Id{store.clientIds[0], store.clientIds[2]} {
			if outcome := modernOutcomeOf(store.evidence[clientId]); outcome != model.ProviderModernOutcomeNotPostQuantum {
				t.Errorf("a released negative recorded %s", outcome)
			}
		}
	})
}

// The held downgrades a pass carries: those held within the staleness
// window, each confirmed only by a later sweep, one entry per provider, at
// most maxHeldDowngrades, and retired once recorded.
func TestHeldDowngradesAreCarriedConfirmedAndRetired(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	earlier, stale, current := server.NewId(), server.NewId(), server.NewId()
	sweep := Sweep{
		StartedAt: now,
		HeldDowngrades: []HeldDowngrade{
			{ClientId: earlier, SweepStartedAt: now.Add(-6 * time.Hour)},
			{ClientId: stale, SweepStartedAt: now.Add(-model.ProviderModernEvidenceMaxAge)},
			{ClientId: current, SweepStartedAt: now},
		},
	}
	held := newHeldDowngradeSet(sweep, now)
	connect.AssertEqual(t, held.carried(), []HeldDowngrade{sweep.HeldDowngrades[0], sweep.HeldDowngrades[2]})
	connect.AssertEqual(t, held.confirms(earlier), true)
	connect.AssertEqual(t, held.confirms(current), false)
	connect.AssertEqual(t, held.confirms(stale), false)
	connect.AssertEqual(t, held.confirms(server.NewId()), false)

	// holding a carried one again keeps its sweep
	held.hold(earlier)
	connect.AssertEqual(t, held.carried(), []HeldDowngrade{sweep.HeldDowngrades[0], sweep.HeldDowngrades[2]})
	held.retire(earlier)
	held.retire(current)
	if held.carried() != nil {
		t.Fatalf("retired downgrades are still carried: %+v", held.carried())
	}

	for range maxHeldDowngrades + 1 {
		held.hold(server.NewId())
	}
	connect.AssertEqual(t, len(held.carried()), maxHeldDowngrades)
}

// Root cause (re-review finding 5): a pass whose evidence write failed
// returned the sweep it started with, held downgrades included. A hold the
// pass retired, answering a modern reading it recorded, came back with the
// task's retry, and that provider, fresh now, was not measured again in the
// sweep; a later sweep's one negative reading then confirmed its downgrade
// at once. A failed pass keeps the sweep's place, so the retry selects the
// same page, and carries the held downgrades as the pass left them: the
// retired hold is gone and a negative reading it held is carried. Its write
// failures come back as one error, not a join, which the task's retry could
// not carry the sweep with.
func TestAFailedPassCarriesItsHeldDowngrades(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeStore(4)
		ids := store.clientIds
		answered, failing, alsoFailing, downgrading := ids[0], ids[1], ids[2], ids[3]
		modernEvidence := model.ProviderModernEvidence{
			E2eNegotiated:             true,
			KeyExchange:               "X25519MLKEM768",
			PostQuantum:               true,
			WindowAdvertised:          true,
			MaxReceiveWindowByteCount: 4 * 1024 * 1024,
			// due for a refresh, and fresh
			ObservedAt: time.Now().Add(-80 * time.Hour),
		}
		store.evidence[answered] = modernEvidence
		store.evidence[downgrading] = modernEvidence
		writeErr := errors.New("synthetic evidence write failure")
		store.recordErr[failing] = writeErr
		store.recordErr[alsoFailing] = writeErr
		fleet := newFakeFleet()
		fleet.providers[answered] = fakeModernProvider()
		fleet.providers[failing] = fakeModernProvider()
		fleet.providers[alsoFailing] = fakeModernProvider()
		fleet.providers[downgrading] = fakeLegacyProvider()
		runner := newFakeRunner(t, fakeSettings(4, 4, 4, 1), store, fleet)
		// a sweep under way, holding a downgrade of answered from an earlier
		// sweep
		sweep := Sweep{
			StartedAt:      time.Now().Add(-time.Hour),
			HeldDowngrades: []HeldDowngrade{{ClientId: answered, SweepStartedAt: time.Now().Add(-7 * time.Hour)}},
		}

		next, result, err := runner.run(t.Context(), sweep)
		if !errors.Is(err, writeErr) {
			t.Fatalf("the pass returned %v, want its write failure", err)
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			t.Fatalf("the pass's write failures came back joined: %v", err)
		}
		if result == nil || result.Started != 4 || result.Recorded != 1 || result.DowngradesHeld != 1 {
			t.Fatalf("the failed pass counted %+v", result)
		}
		if modernOutcomeOf(store.evidence[answered]) != model.ProviderModernOutcomeModern || !store.evidence[answered].ObservedAt.After(modernEvidence.ObservedAt) {
			t.Fatalf("the modern reading was not recorded: %+v", store.evidence[answered])
		}
		connect.AssertEqual(t, next, Sweep{
			StartedAt:      sweep.StartedAt,
			HeldDowngrades: []HeldDowngrade{{ClientId: downgrading, SweepStartedAt: sweep.StartedAt}},
		})
	})
}
