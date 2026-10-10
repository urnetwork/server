// Readout controls preserve raw loaded semantics while refusing incompatible
// latency/delivery attribution without discarding unrelated campaign metrics.
package main

import (
	"strings"
	"testing"
)

// Missing, explicit legacy, settled, and unsupported fields retain identity.
func TestLoadedProbeVersionReadoutParsing(t *testing.T) {
	for _, test := range []struct {
		field   string
		version int
	}{
		{field: "", version: 0},
		{field: `,"loaded_probe_measurement_version":0`, version: 0},
		{field: `,"loaded_probe_measurement_version":2`, version: 2},
		{field: `,"loaded_probe_measurement_version":3`, version: -1},
		{field: `,"loaded_probe_measurement_version":2.5`, version: -1},
		{field: `,"loaded_probe_measurement_version":"2"`, version: -1},
	} {
		record, ok := parseRecord(`{"record_type":"run","correct":true,"tunneled":{"loaded_probe_attempt_count":2,"loaded_probe_success_count":1,"loaded_latency":{"p95_nanoseconds":1000000}` + test.field + `}}`)
		if !ok || record.loadedVersion != test.version || record.loadedAttempt != 2 || record.loadedSuccess != 1 || record.loadedP95 != 1 {
			t.Fatalf("field=%s parsed=%+v ok=%t", test.field, record, ok)
		}
	}
}

// A mixed cell cannot publish a numeric aggregate or comparison of loaded data.
func TestLoadedProbeVersionReadoutRejectsMixedCell(t *testing.T) {
	records := []runRecord{
		{arm: "stock", workload: "latency-under-load", correct: true, loadedAttempt: 2, loadedSuccess: 1, loadedP95: 10, goodput: 1},
		{arm: "stock", workload: "latency-under-load", correct: true, loadedVersion: 2, loadedAttempt: 2, loadedSuccess: 2, loadedP95: 5, goodput: 3},
	}
	cell := summarize(records)["stock"][cellKey{workload: "latency-under-load"}]
	if !cell.loadedVersionInvalid || cell.correct != 2 || len(cell.goodputs) != 2 || cell.goodputs[0] != 1 || cell.goodputs[1] != 3 ||
		loadedDelta(cell, cell) != "incompatible versions" || deliveredDelta(cell, cell) != "incompatible versions" {
		t.Fatalf("mixed cell lost its guard or unrelated observations: %+v", cell)
	}
	report := renderReport("synthetic.example", records, "stock")
	if !strings.Contains(report, "incompatible versions") || !strings.Contains(report, "legacy bulk cutoff") {
		t.Fatalf("mixed readout lacked explicit semantics: %s", report)
	}
}

// Candidate-minus-control refuses only loaded metrics when versions differ.
func TestLoadedProbeVersionReadoutRejectsCrossVersionAttribution(t *testing.T) {
	records := []runRecord{
		{arm: "stock", workload: "latency-under-load", correct: true, loadedAttempt: 2, loadedSuccess: 1, loadedP95: 10, goodput: 1},
		{arm: "candidate", workload: "latency-under-load", correct: true, loadedVersion: 2, loadedAttempt: 2, loadedSuccess: 2, loadedP95: 5, goodput: 3},
	}
	summary := summarize(records)
	key := cellKey{workload: "latency-under-load"}
	base, candidate := summary["stock"][key], summary["candidate"][key]
	if loadedDelta(candidate, base) != "incompatible versions" || deliveredDelta(candidate, base) != "incompatible versions" ||
		median(candidate.goodputs)-median(base.goodputs) != 2 {
		t.Fatalf("cross-version attribution lost scope: base=%+v candidate=%+v", base, candidate)
	}
	report := renderReport("synthetic.example", records, "stock")
	if strings.Count(report, "incompatible versions") != 2 || !strings.Contains(report, "5 (v2)") || !strings.Contains(report, "10 (v0)") ||
		!strings.Contains(report, "+2.0") {
		t.Fatalf("version mismatch was not isolated to loaded attribution: %s", report)
	}
}

// Legacy and settled arms remain comparable only within their own semantics.
func TestLoadedProbeVersionReadoutKeepsCompatibleAttribution(t *testing.T) {
	for _, version := range []int{0, 2, -1} {
		records := []runRecord{
			{arm: "stock", workload: "latency-under-load", correct: true, loadedVersion: version, loadedAttempt: 2, loadedSuccess: 1, loadedP95: 10},
			{arm: "candidate", workload: "latency-under-load", correct: true, loadedVersion: version, loadedAttempt: 2, loadedSuccess: 2, loadedP95: 5},
		}
		summary := summarize(records)
		key := cellKey{workload: "latency-under-load"}
		base, candidate := summary["stock"][key], summary["candidate"][key]
		wantLatency, wantDelivery := "-5", "+50.0"
		if version == -1 {
			wantLatency, wantDelivery = "incompatible versions", "incompatible versions"
		}
		if loadedDelta(candidate, base) != wantLatency || deliveredDelta(candidate, base) != wantDelivery {
			t.Fatalf("version=%d latency=%s delivery=%s", version, loadedDelta(candidate, base), deliveredDelta(candidate, base))
		}
	}
}
