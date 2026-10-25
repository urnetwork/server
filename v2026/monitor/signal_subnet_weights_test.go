package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
)

func runSubnetWeights(t *testing.T, signal Signal, settings SignalSettings) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("weights run failed: %v", err)
	}
	return alerts
}

// Masked, capped, fresh rows with a pending commit inside its window: only
// the two fixed replay gaps remain, and the runtime metadata is read once.
func TestSubnetWeightsHealthySubnetHasOnlyReplayGaps(t *testing.T) {
	chain, _, settings := newSubnetWeightsFixture(t)
	signal := NewSubnetWeightsSignal()
	alerts := runSubnetWeights(t, signal, settings)
	requireSubnetWeightsOnlyReplayGaps(t, alerts)
	runSubnetWeights(t, signal, settings)
	if chain.metadataGet != 1 {
		t.Fatalf("runtime metadata fetched %d times for one spec version", chain.metadataGet)
	}
	requireSubnetReplayRedacted(t, alerts, subnetReplayTestSs58(t, subnetWeightTestEnrolled))
}

// Self weight, escrow weight, an unregistered recipient and a cap breach are
// each a proved row counterexample of an enrolled validator.
func TestSubnetWeightsEnrolledRowCounterexamplesPage(t *testing.T) {
	cases := []struct {
		frame string
		row   []subnetWeightEntry
	}{
		{frame: "self-weight", row: []subnetWeightEntry{{Uid: 0, Value: 100}, {Uid: 3, Value: 65535}}},
		{frame: "escrow-recipient", row: []subnetWeightEntry{{Uid: 7, Value: 100}, {Uid: 3, Value: 65535}}},
		{frame: "unregistered-recipient", row: []subnetWeightEntry{{Uid: 9, Value: 100}, {Uid: 3, Value: 65535}}},
		{frame: "cap-breach", row: []subnetWeightEntry{{Uid: 2, Value: 1000}, {Uid: 3, Value: 65535}}},
	}
	for _, c := range cases {
		chain, _, settings := newSubnetWeightsFixture(t)
		chain.set("Weights", c.row, subnetWeightLittleEndian16(25), subnetWeightLittleEndian16(0))
		alerts := runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
		alert := requireSubnetReplayAlert(t, alerts, "subnet-weight-vector", "subnet-weights/validator-1", c.frame)
		if alert.Severity != SeverityPage || alert.Sustain != 1 {
			t.Fatalf("%s severity=%s sustain=%d", c.frame, alert.Severity, alert.Sustain)
		}
	}
}

// Without an enrolled cap the cap is unknown, never a page.
func TestSubnetWeightsCapUnknownWithoutEnrollment(t *testing.T) {
	chain, _, settings := newSubnetWeightsFixture(t)
	settings.SubnetReplay.MaximumWeightLimitU16 = 0
	chain.set("Weights", []subnetWeightEntry{{Uid: 2, Value: 1000}, {Uid: 3, Value: 65535}}, subnetWeightLittleEndian16(25), subnetWeightLittleEndian16(0))
	alerts := runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-weights/coverage", "weight-cap")
	if len(subnetReplayAlerts(alerts, "subnet-weight-vector")) != 0 {
		t.Fatal("a cap was judged without the signed policy cap")
	}
}

// A census validator's row is not judged by the enrolled policy.
func TestSubnetWeightsCensusRowsAreNotPolicyJudged(t *testing.T) {
	chain, _, settings := newSubnetWeightsFixture(t)
	chain.set("Weights", []subnetWeightEntry{{Uid: 1, Value: 65535}}, subnetWeightLittleEndian16(25), subnetWeightLittleEndian16(1))
	alerts := runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	requireSubnetWeightsOnlyReplayGaps(t, alerts)
}

// No applied row and no pending commit for the stale window is a missed cycle.
func TestSubnetWeightsMissedCyclePages(t *testing.T) {
	chain, _, settings := newSubnetWeightsFixture(t)
	chain.set("LastUpdate", []types.U64{types.U64(chain.number - 400), types.U64(chain.number - 50), 0, 0, 0, 0, 0, 0}, subnetWeightLittleEndian16(25))
	chain.commits()
	alerts := runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-crv4-lifecycle", "subnet-weights/validator-1", "cycle")
	if alert.Severity != SeverityPage {
		t.Fatalf("missed cycle severity=%s", alert.Severity)
	}
}

// A revealable commit still pending pages for the enrolled validator and
// warns for a census validator; a missing drand round only warns.
func TestSubnetWeightsUnappliedRevealAndDrandWait(t *testing.T) {
	chain, _, settings := newSubnetWeightsFixture(t)
	chain.commits(subnetWeightTestCommit(subnetWeightTestEnrolled, chain.number-300, 900), subnetWeightTestCommit(subnetWeightTestCensus, chain.number-300, 900))
	alerts := runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	enrolled := requireSubnetReplayAlert(t, alerts, "subnet-crv4-lifecycle", "subnet-weights/validator-1", "reveal")
	census := requireSubnetReplayAlert(t, alerts, "subnet-crv4-lifecycle", "subnet-weights/census", "uid-1/reveal")
	if enrolled.Severity != SeverityPage || census.Severity != SeverityWarn {
		t.Fatalf("unapplied reveal enrolled=%s census=%s", enrolled.Severity, census.Severity)
	}

	chain, _, settings = newSubnetWeightsFixture(t)
	chain.commits(subnetWeightTestCommit(subnetWeightTestEnrolled, chain.number-300, 5000))
	alerts = runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	drand := requireSubnetReplayAlert(t, alerts, "subnet-crv4-lifecycle", "subnet-weights/validator-1", "reveal-drand")
	if drand.Severity != SeverityWarn {
		t.Fatalf("drand wait severity=%s", drand.Severity)
	}
}

// Two pending commits in one tempo window are a duplicate effect.
func TestSubnetWeightsDuplicateIntentPages(t *testing.T) {
	chain, _, settings := newSubnetWeightsFixture(t)
	chain.commits(subnetWeightTestCommit(subnetWeightTestEnrolled, chain.number-10, 1500), subnetWeightTestCommit(subnetWeightTestEnrolled, chain.number-5, 1501))
	alerts := runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-crv4-lifecycle", "subnet-weights/validator-1", "duplicate-intent")
}

// Collapsed trust of an active validator warns.
func TestSubnetWeightsTrustCollapseWarns(t *testing.T) {
	chain, _, settings := newSubnetWeightsFixture(t)
	chain.set("ValidatorTrust", []types.U16{100, 50000, 0, 0, 0, 0, 0, 0}, subnetWeightLittleEndian16(25))
	alerts := runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-consensus-divergence", "subnet-weights/validator-1", "validator-trust")
	if alert.Severity != SeverityWarn {
		t.Fatalf("trust collapse severity=%s", alert.Severity)
	}
}

// Zero dividends warn only after a full tempo of eligible observation.
func TestSubnetWeightsMissingDividendsAfterAFullTempo(t *testing.T) {
	chain, node, settings := newSubnetWeightsFixture(t)
	chain.set("Dividends", []types.U16{0, 200, 0, 0, 0, 0, 0, 0}, subnetWeightLittleEndian16(25))
	signal := NewSubnetWeightsSignal()
	first := runSubnetWeights(t, signal, settings)
	if len(subnetReplayAlerts(first, "subnet-validator-native-payout")) != 0 {
		t.Fatal("one zero observation alerted before a full tempo")
	}
	later := newHealthySubnetWeightChain(t, chain.number+100)
	later.set("Dividends", []types.U16{0, 200, 0, 0, 0, 0, 0, 0}, subnetWeightLittleEndian16(25))
	node.native = later.answer
	second := runSubnetWeights(t, signal, settings)
	requireSubnetReplayAlert(t, second, "subnet-validator-native-payout", "subnet-weights/validator-1", "dividends")
}

// Inconsistent vectors, trailing bytes, malformed metadata and an
// unregistered enrolled validator are unknown, never green.
func TestSubnetWeightsUnknownInputsNeverGreen(t *testing.T) {
	chain, _, settings := newSubnetWeightsFixture(t)
	chain.set("Dividends", []types.U16{100, 200}, subnetWeightLittleEndian16(25))
	alerts := runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	requireSubnetWeightsSourceReason(t, alerts, "metagraph-vectors-inconsistent")

	chain, _, settings = newSubnetWeightsFixture(t)
	key, _ := types.CreateStorageKey(chain.meta, "SubtensorModule", "Tempo", subnetWeightLittleEndian16(25))
	chain.storage[key.Hex()] = append(chain.storage[key.Hex()], 0x00)
	alerts = runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	requireSubnetWeightsSourceReason(t, alerts, "storage-value-trailing")

	chain, _, settings = newSubnetWeightsFixture(t)
	chain.raw = chain.raw[:len(chain.raw)/2]
	alerts = runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	requireSubnetWeightsSourceReason(t, alerts, "metadata-unsupported")

	chain, _, settings = newSubnetWeightsFixture(t)
	chain.remove("Uids", subnetWeightLittleEndian16(25), subnetWeightTestEnrolled[:])
	alerts = runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	permit := requireSubnetReplayAlert(t, alerts, "subnet-crv4-lifecycle", "subnet-weights/validator-1", "permit")
	if !permit.unobservable || permit.Severity != SeverityWarn {
		t.Fatalf("unregistered enrolled validator=%+v", permit)
	}

	_, _, settings = newSubnetWeightsFixture(t)
	settings.SubnetReplay.Validators = nil
	alerts = runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-weights/coverage", "validators")
}

func requireSubnetWeightsSourceReason(t *testing.T, alerts []Alert, reason string) {
	t.Helper()
	alert := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-weights/coverage", "source")
	if !strings.Contains(alert.Observed, "reason="+reason) {
		t.Fatalf("source gap=%q, want %s", alert.Observed, reason)
	}
	for _, other := range alerts {
		if other.Severity == SeverityPage {
			t.Fatalf("an unknown native input paged: %+v", other)
		}
	}
}

// A native checkpoint that stops being canonical withholds every verdict.
func TestSubnetWeightsNativeReorgWithholdsVerdicts(t *testing.T) {
	chain, node, settings := newSubnetWeightsFixture(t)
	chain.set("ValidatorTrust", []types.U16{100, 50000, 0, 0, 0, 0, 0, 0}, subnetWeightLittleEndian16(25))
	native := chain.answer
	node.native = func(method string, params []json.RawMessage) (any, *subnetReplayRpcFailure) {
		if method == "chain_getBlockHash" {
			var number uint64
			if json.Unmarshal(params[0], &number) == nil && number == chain.number {
				return subnetWeightTestNativeHash(number, 9).Hex(), nil
			}
		}
		return native(method, params)
	}
	alerts := runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	if len(subnetReplayAlerts(alerts, "subnet-consensus-divergence")) != 0 {
		t.Fatal("a verdict survived a non-canonical native checkpoint")
	}
	requireSubnetWeightsSourceReason(t, alerts, "checkpoint-not-canonical")
}

// Parent cancellation withholds every weights finding.
func TestSubnetWeightsCanceledRunWithholdsFindings(t *testing.T) {
	chain, node, settings := newSubnetWeightsFixture(t)
	chain.set("ValidatorTrust", []types.U16{100, 50000, 0, 0, 0, 0, 0, 0}, subnetWeightLittleEndian16(25))
	ctx, cancel := context.WithCancel(context.Background())
	node.beforeExchange = func(exchange int) {
		if exchange == 4 {
			cancel()
		}
	}
	alerts, err := NewSubnetWeightsSignal().Run(ctx, settings)
	if err == nil || len(alerts) != 0 {
		t.Fatalf("canceled weights run returned alerts=%d err=%v", len(alerts), err)
	}
}

func TestSubnetWeightsRegistrationMetadata(t *testing.T) {
	signal := NewSubnetWeightsSignal()
	if signal.Number() != "22.6" || signal.Key() != "subnet-weights" || signal.ID() != "subnet/weights" || signal.Cadence() <= 0 {
		t.Fatalf("unexpected metadata: %s %s %s", signal.Number(), signal.Key(), signal.ID())
	}
}

// A runtime whose item shape differs from the reviewed shape is refused as
// unsupported instead of being decoded with the wrong type.
func TestSubnetWeightsUnsupportedRuntimeShapeIsUnknown(t *testing.T) {
	chain, _, settings := newSubnetWeightsFixture(t)
	mutated := false
	for pallet := range chain.meta.AsMetadataV14.Pallets {
		storage := &chain.meta.AsMetadataV14.Pallets[pallet].Storage
		var otherValue types.Si1LookupTypeID
		found := false
		for _, item := range storage.Items {
			if string(item.Name) == "LastUpdate" {
				otherValue, found = item.Type.AsMap.Value, true
			}
		}
		for i := range storage.Items {
			if string(storage.Items[i].Name) == "Weights" && found {
				storage.Items[i].Type.AsMap.Value = otherValue
				mutated = true
			}
		}
	}
	if !mutated {
		t.Fatal("fixture did not find the weight item to mutate")
	}
	if reason := verifySubnetWeightShapes(chain.meta); reason != "storage-shape-unsupported" {
		t.Fatalf("mutated runtime shape reason=%q", reason)
	}
	raw, err := codec.Encode(chain.meta)
	if err != nil {
		t.Fatal(err)
	}
	chain.raw = raw
	alerts := runSubnetWeights(t, NewSubnetWeightsSignal(), settings)
	requireSubnetWeightsSourceReason(t, alerts, "storage-shape-unsupported")
}

// §22.10 acceptance fixtures for this family.
func init() {
	subnetFamilyAcceptanceCases["22.6"] = subnetFamilyAcceptanceCase{
		healthy: func(t *testing.T) (Signal, SignalSettings) {
			_, _, settings := newSubnetWeightsFixture(t)
			return NewSubnetWeightsSignal(), settings
		},
		violated: func(t *testing.T) (Signal, SignalSettings, func()) {
			chain, _, settings := newSubnetWeightsFixture(t)
			netuid, uid := subnetWeightLittleEndian16(25), subnetWeightLittleEndian16(0)
			chain.set("Weights", []subnetWeightEntry{{Uid: 0, Value: 100}, {Uid: 3, Value: 65535}}, netuid, uid)
			return NewSubnetWeightsSignal(), settings, func() {
				chain.set("Weights", []subnetWeightEntry{{Uid: 2, Value: 30000}, {Uid: 3, Value: 65535}}, netuid, uid)
			}
		},
		violatedClass: "subnet-weight-vector",
		unknown: func(t *testing.T) (Signal, SignalSettings) {
			chain, _, settings := newSubnetWeightsFixture(t)
			chain.set("Dividends", []types.U16{100, 200}, subnetWeightLittleEndian16(25))
			return NewSubnetWeightsSignal(), settings
		},
		canceled: func(t *testing.T) (Signal, SignalSettings, context.Context) {
			_, node, settings := newSubnetWeightsFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			node.beforeExchange = func(exchange int) {
				if exchange == 4 {
					cancel()
				}
			}
			return NewSubnetWeightsSignal(), settings, ctx
		},
		fixedGaps: []subnetFamilyAcceptanceGap{{class: "subnet-monitor-coverage", frame: "decision-replay"}, {class: "subnet-monitor-coverage", frame: "yuma-replay"}},
	}
}
