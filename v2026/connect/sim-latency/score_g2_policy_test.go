// Policy-specific reducers and authenticated replay preserve version-one
// scoring while making the prospective minimum-work contract explicit.
package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Re-signs only owned synthetic sidecars after changing their policy identity.
func setScoreFixtureG2Policy(t *testing.T, fixture *scoreFixture, policy model.CompetitionG2VolumePolicy) {
	t.Helper()
	version, err := model.CompetitionScorerVersionForG2Policy(policy)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runStats.G2VolumePolicy, fixture.runStats.ScorerVersion = policy, version
	writeScoreJSON(t, scoreSidecarForCSV(fixture.run), fixture.runStats)
	if err := writeFinalMarker(fixture.marker, scoreSidecarForCSV(fixture.run), fixture.runStats); err != nil {
		t.Fatal(err)
	}
	fixture.baselineData.G2VolumePolicy, fixture.baselineData.ScorerVersion = policy, version
	writeScoreJSON(t, fixture.baseline, fixture.baselineData)
}

// Both counts have inclusive floors; removing an upper bound must not remove
// the other floor, introduce a zero-work pass, or alter another gate.
func TestScoreG2MinimumWorkBoundariesAndLegacyUpperBounds(t *testing.T) {
	baseline := baselineAggregate{ScoreAggregateDiagnostics: ScoreAggregateDiagnostics{RequestCount: 100, ReceivedBytes: 1000}}
	cases := []struct {
		name                    string
		requests, receivedBytes float64
		legacy, minimumWork     bool
	}{
		{name: "exact floor", requests: 95, receivedBytes: 950, legacy: true, minimumWork: true},
		{name: "request below floor", requests: math.Nextafter(95, 0), receivedBytes: 950},
		{name: "byte below floor", requests: 95, receivedBytes: math.Nextafter(950, 0)},
		{name: "exact historical upper", requests: 105, receivedBytes: 1050, legacy: true, minimumWork: true},
		{name: "request above historical upper", requests: math.Nextafter(105, math.Inf(1)), receivedBytes: 1000, minimumWork: true},
		{name: "byte above historical upper", requests: 100, receivedBytes: math.Nextafter(1050, math.Inf(1)), minimumWork: true},
		{name: "both much larger", requests: 1000000, receivedBytes: 10000000, minimumWork: true},
		{name: "large requests cannot hide missing bytes", requests: 1000000, receivedBytes: 949},
		{name: "large bytes cannot hide missing requests", requests: 94, receivedBytes: 10000000},
		{name: "zero requests", requests: 0, receivedBytes: 1000},
		{name: "zero bytes", requests: 100, receivedBytes: 0},
		{name: "nonfinite requests", requests: math.Inf(1), receivedBytes: 1000},
		{name: "nonfinite bytes", requests: 100, receivedBytes: math.NaN()},
	}
	for _, c := range cases {
		candidate := ScoreAggregateDiagnostics{RequestCount: c.requests, ReceivedBytes: c.receivedBytes}
		legacy := buildScoreGates(baseline, candidate, nil, model.CompetitionG2VolumePolicyLegacy)
		minimumWork := buildScoreGates(baseline, candidate, nil, model.CompetitionG2VolumePolicyMinimumWork)
		if legacy["G2_volume"].Passed != c.legacy || minimumWork["G2_volume"].Passed != c.minimumWork {
			t.Errorf("%s: legacy=%t minimum-work=%t", c.name, legacy["G2_volume"].Passed, minimumWork["G2_volume"].Passed)
		}
		for name, gate := range legacy {
			if name != "G2_volume" && !reflect.DeepEqual(gate, minimumWork[name]) {
				t.Errorf("%s: policy changed unrelated gate %s", c.name, name)
			}
		}
		if _, present := minimumWork["G2_volume"].Details["request_max"]; present {
			t.Error("minimum-work diagnostics retained a request upper bound")
		}
		if _, present := minimumWork["G2_volume"].Details["bytes_max"]; present {
			t.Error("minimum-work diagnostics retained a byte upper bound")
		}
	}
}

// The policy remains a median-to-median contract, not a new per-replicate gate.
func TestScoreG2MinimumWorkUsesIndependentMedians(t *testing.T) {
	baseline := aggregateBaseline([]ScoreBaselineReplicate{
		{RequestCount: 1, ReceivedBytes: 100000},
		{RequestCount: 100, ReceivedBytes: 1000},
		{RequestCount: 10000, ReceivedBytes: 1},
	})
	candidate := aggregateCandidate([]candidateReplicate{
		{diagnostics: ScoreReplicateDiagnostics{RequestCount: 1, ReceivedBytes: 95000}},
		{diagnostics: ScoreReplicateDiagnostics{RequestCount: 95, ReceivedBytes: 950}},
		{diagnostics: ScoreReplicateDiagnostics{RequestCount: 9500, ReceivedBytes: 1}},
	})
	gate := buildScoreGates(baseline, candidate, nil, model.CompetitionG2VolumePolicyMinimumWork)["G2_volume"]
	if !gate.Passed || gate.Details["request_min"] != 95.0 || gate.Details["bytes_min"] != 950.0 {
		t.Fatalf("independent medians were not retained: %+v", gate)
	}
}

// Invalid policy or reference data cannot turn an absent floor into a pass.
func TestScoreG2RejectsInvalidPolicyAndZeroBaseline(t *testing.T) {
	positive := ScoreAggregateDiagnostics{RequestCount: 100, ReceivedBytes: 1000}
	if buildScoreGates(baselineAggregate{ScoreAggregateDiagnostics: positive}, positive, nil, "unknown/1")["G2_volume"].Passed {
		t.Fatal("unknown g2 policy passed")
	}
	for _, baseline := range []ScoreAggregateDiagnostics{{RequestCount: 0, ReceivedBytes: 1000}, {RequestCount: 100, ReceivedBytes: 0}, {RequestCount: math.NaN(), ReceivedBytes: 1000}} {
		if buildScoreGates(baselineAggregate{ScoreAggregateDiagnostics: baseline}, positive, nil, model.CompetitionG2VolumePolicyMinimumWork)["G2_volume"].Passed {
			t.Fatal("invalid baseline passed minimum-work gate")
		}
	}
}

// Full artifact replay proves the larger workload is admitted only by the new
// version; no live workload or measurement is needed by this regression.
func TestScoreG2MinimumWorkAuthenticatedReplay(t *testing.T) {
	options := defaultScoreFixtureOptions()
	options.rowCount, options.accountingBytes = 200, 20000
	fixture := newScoreFixture(t, options)
	legacy := Score(fixture.inputs)
	if legacy.EvalError != nil || legacy.Gates["G2_volume"].Passed || legacy.Placeable {
		t.Fatalf("legacy upper bounds changed: %+v", legacy)
	}
	setScoreFixtureG2Policy(t, fixture, model.CompetitionG2VolumePolicyMinimumWork)
	minimumWork := Score(fixture.inputs)
	if minimumWork.EvalError != nil || !minimumWork.Gates["G2_volume"].Passed || !minimumWork.Placeable ||
		minimumWork.Diagnostics.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork ||
		minimumWork.Diagnostics.ScorerVersion != model.CompetitionMinimumWorkScorerVersion {
		t.Fatalf("new policy failed authenticated larger-work replay: %+v", minimumWork)
	}
	first, err := marshalScoreResult(minimumWork)
	if err != nil {
		t.Fatal(err)
	}
	second, err := marshalScoreResult(Score(fixture.inputs))
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("identical immutable artifacts did not replay byte-identically", err)
	}
}

// The builder binds the same policy as every run; a CLI knob alone cannot
// relabel old measurements or bless an unsupported policy.
func TestBuildScoreBaselineCarriesG2PolicyAndRejectsMismatches(t *testing.T) {
	fixture := newScoreFixture(t, baselineScoreFixtureOptions())
	inputs := baselineInputsFromFixture(fixture)
	inputs.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
	if _, err := BuildScoreBaseline(inputs); err == nil {
		t.Fatal("minimum-work builder accepted version-one runs")
	}
	setScoreFixtureG2Policy(t, fixture, model.CompetitionG2VolumePolicyMinimumWork)
	baseline, err := BuildScoreBaseline(inputs)
	if err != nil || baseline.G2VolumePolicy != inputs.G2VolumePolicy || baseline.ScorerVersion != model.CompetitionMinimumWorkScorerVersion {
		t.Fatalf("builder lost the frozen new policy: %+v %v", baseline, err)
	}
	inputs.G2VolumePolicy = "unknown/1"
	if _, err := BuildScoreBaseline(inputs); err == nil {
		t.Fatal("unknown builder policy accepted")
	}
}

// Baseline, run and completion marker are separate authenticated boundaries.
func TestScoreG2PolicyMismatchFailsClosed(t *testing.T) {
	for _, boundary := range []string{"baseline policy", "baseline version", "run policy", "marker policy"} {
		fixture := newScoreFixture(t, defaultScoreFixtureOptions())
		setScoreFixtureG2Policy(t, fixture, model.CompetitionG2VolumePolicyMinimumWork)
		switch boundary {
		case "baseline policy":
			fixture.baselineData.G2VolumePolicy = "unknown/1"
			writeScoreJSON(t, fixture.baseline, fixture.baselineData)
		case "baseline version":
			fixture.baselineData.ScorerVersion = officialScorerVersion
			writeScoreJSON(t, fixture.baseline, fixture.baselineData)
		case "run policy":
			fixture.runStats.G2VolumePolicy = ""
			writeScoreJSON(t, scoreSidecarForCSV(fixture.run), fixture.runStats)
		case "marker policy":
			var marker runFinalMarker
			content, err := os.ReadFile(fixture.marker)
			if err != nil || json.Unmarshal(content, &marker) != nil {
				t.Fatal("cannot read synthetic marker", err)
			}
			marker.G2VolumePolicy = ""
			writeScoreJSON(t, fixture.marker, marker)
		}
		result := Score(fixture.inputs)
		if result.EvalError == nil || result.EvalError.Kind != "infrastructure" || result.Placeable {
			t.Errorf("%s mismatch accepted: %+v", boundary, result)
		}
	}
}

// The new optional fields do not perturb historical canonical artifact bytes.
func TestScoreG2LegacyCodecBytesAndReplay(t *testing.T) {
	baseline, err := json.Marshal(ScoreBaselineManifest{})
	if err != nil || string(baseline) != `{"score_schema":0,"kind":"","scorer_version":"","round_id":"","config_sha256":"","request_timeout_ms":0,"run_flags":null,"takeover_margin":0,"replicates":null}` {
		t.Fatal("legacy baseline codec changed", string(baseline), err)
	}
	marker, err := json.Marshal(runFinalMarker{})
	if err != nil || string(marker) != `{"schema":0,"kind":"","score_schema":0,"scorer_version":"","evaluation_id":"","run_manifest_sha256":"","run_manifest_bytes":0,"completed_unix_ms":0}` {
		t.Fatal("legacy marker codec changed", string(marker), err)
	}
	fixture := newScoreFixture(t, defaultScoreFixtureOptions())
	for _, path := range []string{fixture.baseline, scoreSidecarForCSV(fixture.run), fixture.marker} {
		content, err := os.ReadFile(path)
		if err != nil || bytes.Contains(content, []byte("g2_volume_policy")) {
			t.Fatal("legacy artifact acquired a policy field", path, err)
		}
	}
	result := Score(fixture.inputs)
	content, err := marshalScoreResult(result)
	if err != nil || result.EvalError != nil || bytes.Contains(content, []byte("g2_volume_policy")) || bytes.Contains(content, []byte("scorer_version")) {
		t.Fatal("legacy score output acquired policy/version diagnostics", err)
	}
	if len(result.Gates["G2_volume"].Details) != 6 || result.Gates["G2_volume"].Details["request_max"] != 105.0 || result.Gates["G2_volume"].Details["bytes_max"] != 10500.0 {
		t.Fatal("legacy bound diagnostics changed")
	}
}
