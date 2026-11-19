// Versioned result controls preserve legacy records while refusing only
// loaded metrics whose input deadlines have incompatible meanings.
package perfvar

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// New loaded samples explicitly identify their load-at-offer semantics.
func TestLoadedLatencyProbeVersionSerialization(t *testing.T) {
	result := workloadResult{UsefulByteCount: 64, Duration: time.Second, GoodputGigabits: 4}
	samples := latencyProbeSamples{latencies: []time.Duration{time.Millisecond}, attemptCount: 2, failureCount: 1}
	applyLatencyProbeSamples(&result, latencyProbeSamples{}, samples, latencyProbeSamples{})
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded workloadResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"loaded_probe_measurement_version":2`)) ||
		decoded.LoadedProbeMeasurementVersion != 2 || decoded.LoadedLatency.P95 != time.Millisecond ||
		decoded.LoadedProbeAttemptCount != 2 || decoded.LoadedProbeSuccessCount != 1 || decoded.LoadedProbeFailureCount != 1 ||
		decoded.UsefulByteCount != 64 || decoded.Duration != time.Second || decoded.GoodputGigabits != 4 || decoded.Latency.P95 != 0 {
		t.Fatalf("versioned serialization changed provenance or unrelated metrics: %s", encoded)
	}
}

// Absent and explicit zero versions remain legacy rather than being upgraded.
func TestLoadedLatencyProbeVersionKeepsLegacySerialization(t *testing.T) {
	for _, encoded := range []string{
		`{"loaded_latency":{"p95_nanoseconds":1000000},"loaded_probe_attempt_count":2,"loaded_probe_success_count":1}`,
		`{"loaded_probe_measurement_version":0,"loaded_latency":{"p95_nanoseconds":1000000},"loaded_probe_attempt_count":2,"loaded_probe_success_count":1}`,
	} {
		var decoded workloadResult
		if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
			t.Fatal(err)
		}
		roundTrip, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.LoadedProbeMeasurementVersion != 0 || decoded.LoadedLatency.P95 != time.Millisecond ||
			decoded.LoadedProbeAttemptCount != 2 || decoded.LoadedProbeSuccessCount != 1 ||
			bytes.Contains(roundTrip, []byte("loaded_probe_measurement_version")) {
			t.Fatalf("legacy observation was relabeled: %s", roundTrip)
		}
	}
}

// Mixed versions suppress loaded p95 without dropping useful-byte statistics.
func TestLoadedLatencyProbeVersionRejectsMixedAggregate(t *testing.T) {
	testLoadedLatencyProbeVersionAggregate(t, 0, 2, "mixed loaded-probe measurement versions")
}

// Unknown semantics cannot become a compatible cohort merely by matching.
func TestLoadedLatencyProbeVersionRejectsUnsupportedAggregate(t *testing.T) {
	testLoadedLatencyProbeVersionAggregate(t, 3, 3, "unsupported loaded-probe measurement version")
}

// Homogeneous settled cohorts retain their numeric loaded statistics.
func TestLoadedLatencyProbeVersionKeepsSettledAggregate(t *testing.T) {
	testLoadedLatencyProbeVersionAggregate(t, 2, 2, "")
}

// Homogeneous legacy cohorts remain readable without retrospective validation.
func TestLoadedLatencyProbeVersionKeepsLegacyAggregate(t *testing.T) {
	testLoadedLatencyProbeVersionAggregate(t, 0, 0, "")
}

// The same original records feed ordinary and shaped headroom-qualified paths.
func testLoadedLatencyProbeVersionAggregate(t *testing.T, firstVersion int, secondVersion int, wantReason string) {
	t.Helper()
	records := []perfvarRunRecord{
		{SchemaVersion: perfvarShapedLinkSchema, Correct: true, Tunneled: workloadResult{GoodputGigabits: 1,
			LoadedLatency: latencyDistribution{P95: time.Millisecond}, LoadedProbeAttemptCount: 1,
			LoadedProbeMeasurementVersion: firstVersion}},
		{SchemaVersion: perfvarShapedLinkSchema, Correct: true, Tunneled: workloadResult{GoodputGigabits: 3,
			LoadedLatency: latencyDistribution{P95: 3 * time.Millisecond}, LoadedProbeAttemptCount: 1,
			LoadedProbeMeasurementVersion: secondVersion}},
	}
	before, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	aggregate := aggregatePerfvarRuns(records)
	wantVersion := firstVersion
	wantLoaded := time.Millisecond
	if wantReason != "" {
		wantVersion = 0
		wantLoaded = 0
	}
	if aggregate.LoadedProbeMeasurementVersion != wantVersion || aggregate.LoadedProbeMeasurementInvalidReason != wantReason ||
		aggregate.LoadedP95Median != wantLoaded || aggregate.GoodputMedianGbps != 1 || aggregate.ValidRunCount != 2 ||
		aggregate.CorrectRunCount != 2 || aggregate.InvalidRunCount != 0 || aggregate.ShapedLink == nil {
		t.Fatalf("ordinary aggregate mixed loaded semantics or erased unrelated observations: %+v", aggregate)
	}
	shaped := aggregate.ShapedLink
	if shaped.LoadedProbeMeasurementVersion != wantVersion || shaped.LoadedProbeMeasurementInvalidReason != wantReason ||
		shaped.Metrics["loaded_latency_p95_median_nanoseconds"] != float64(wantLoaded) ||
		shaped.Metrics["goodput_median_gigabits_per_second"] != 1 || shaped.EligibleRunCount != 2 || shaped.CorrectRunCount != 2 {
		t.Fatalf("shaped aggregate mixed loaded semantics or erased unrelated observations: %+v", shaped)
	}
	after, err := json.Marshal(records)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("aggregation modified original provenance: err=%v before=%s after=%s", err, before, after)
	}
	encoded, err := json.Marshal(aggregate)
	if err != nil {
		t.Fatal(err)
	}
	var decoded perfvarAggregateRecord
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.ShapedLink == nil || decoded.LoadedProbeMeasurementVersion != wantVersion ||
		decoded.LoadedProbeMeasurementInvalidReason != wantReason || decoded.ShapedLink.LoadedProbeMeasurementInvalidReason != wantReason {
		t.Fatalf("aggregate semantics were lost in serialization: err=%v record=%s", err, encoded)
	}
}

// Underlay provenance and valid-run filtering use their actual aggregate scope.
func TestLoadedLatencyProbeVersionAggregateScope(t *testing.T) {
	for _, test := range []struct {
		name             string
		version          int
		underlayVersion  int
		underlayAttempts int
		invalidReason    string
		invalidKind      string
		wantOuterReason  string
		wantShapedReason string
	}{
		{name: "calibration-mismatch", version: 2, underlayAttempts: 1,
			wantOuterReason: "mixed loaded-probe measurement versions", wantShapedReason: "mixed loaded-probe measurement versions"},
		{name: "unsupported-underlay", version: 2, underlayVersion: 3, underlayAttempts: 1,
			wantOuterReason: "unsupported loaded-probe measurement version", wantShapedReason: "unsupported loaded-probe measurement version"},
		{name: "headroom-scope", invalidReason: perfvarHeadroomReason, invalidKind: perfvarHeadroomLimited,
			wantShapedReason: "mixed loaded-probe measurement versions"},
		{name: "invalid-scope", version: 3, invalidReason: "synthetic invalid fixture"},
	} {
		records := []perfvarRunRecord{
			{SchemaVersion: perfvarShapedLinkSchema, Correct: true, Tunneled: workloadResult{LoadedProbeMeasurementVersion: 2}},
			{SchemaVersion: perfvarShapedLinkSchema, Correct: true, InvalidReason: test.invalidReason, InvalidKind: test.invalidKind,
				Tunneled: workloadResult{LoadedProbeMeasurementVersion: test.version},
				Underlay: workloadResult{LoadedProbeMeasurementVersion: test.underlayVersion, LoadedProbeAttemptCount: test.underlayAttempts}},
		}
		aggregate := aggregatePerfvarRuns(records)
		if aggregate.LoadedProbeMeasurementInvalidReason != test.wantOuterReason ||
			aggregate.ShapedLink.LoadedProbeMeasurementInvalidReason != test.wantShapedReason {
			t.Fatalf("case=%s scope drift outer=%q shaped=%q", test.name,
				aggregate.LoadedProbeMeasurementInvalidReason, aggregate.ShapedLink.LoadedProbeMeasurementInvalidReason)
		}
	}
}
