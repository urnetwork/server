package monitor

// Every api process has exported a resident_move_result frame partition
// since 2026-10-22 (65778f4d1). The parser marked a process with any unlisted
// partition invalid, so from then on every api slot read as unobservable and
// §2.19e could not raise pressure (CPUOPT3 D3).

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// An unlisted frame or phase partition is a diagnostic the signal does not
// read. A hot process that exports one still raises pressure, a quiet one
// stays quiet, and neither is reported unobservable.
func TestControlRoutePressureToleratesUnlistedPartitions(t *testing.T) {
	now := time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC)
	hot := controlRouteFixture(now, "synthetic-current")
	// the frame partitions production exports, and partitions no release
	// exports yet
	hot.values["frame:resident_move_result"] = 0
	hot.values["frame:synthetic_future_message"] = 7
	hot.values["phase:synthetic_future_phase"] = 3
	alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, hot))
	if len(alerts) != 1 {
		t.Fatalf("an unlisted partition changed the verdict: %+v", alerts)
	}
	alert := requireAlertClass(t, alerts, "control-route-pressure")
	if alert.Severity != Severity(tierPage) {
		t.Fatalf("pressure alert = %+v", alert)
	}
	for _, unwanted := range []string{"phase_frame_coverage=incomplete", "synthetic_future"} {
		if strings.Contains(alert.Markdown(), unwanted) {
			t.Fatalf("pressure alert carries %q: %s", unwanted, alert.Markdown())
		}
	}

	quiet := controlRouteFixture(now, "synthetic-current")
	quiet.values["canceled"] = 0
	quiet.values["frame:resident_move_result"] = 0
	quiet.values["frame:synthetic_future_message"] = 7
	if alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, quiet)); len(alerts) != 0 {
		t.Fatalf("a quiet process with an unlisted partition was not evaluated: %+v", alerts)
	}
}

// The tolerance covers only partition names. A frame series from another
// ingress, a cell outside the signal's own query vocabulary and a duplicated
// series still make the slot unobservable rather than evidence.
func TestControlRoutePressureStillRejectsForeignSeries(t *testing.T) {
	now := time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC)
	process := controlRouteFixture(now, "synthetic-current")
	valid := controlRouteFixtureJson(t, now, process)
	unknownCell := controlRouteFixture(now, "synthetic-current")
	unknownCell.values["synthetic_unknown_cell"] = 1
	for _, payload := range []string{
		strings.Replace(valid, `"ingress":"http"`, `"ingress":"internal"`, 1),
		controlRouteFixtureJson(t, now, unknownCell),
		controlRouteFixtureJson(t, now, process, process),
	} {
		alerts := runControlRouteFixture(t, now, payload)
		requireAlertClass(t, alerts, "control-route-pressure-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("a foreign series became evidence: %+v", alerts)
		}
	}
}

// The signal lists exactly the partitions a current api exports: the frame
// message labels of the controller and the phase labels of the router, both
// materialized at zero at startup. A label added to either fails here until
// the signal lists it, and a listed label no api exports would leave every
// slot's diagnostics incomplete.
func TestControlRouteMessagesCoverControllerLabels(t *testing.T) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	exported := map[string][]string{}
	for _, family := range families {
		var partition string
		switch family.GetName() {
		case "urnetwork_connect_control_frames_inflight":
			partition = "message"
		case "urnetwork_connect_control_http_phase_inflight":
			partition = "phase"
		default:
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if partition == "message" && labels["ingress"] != "http" {
				continue
			}
			exported[partition] = append(exported[partition], labels[partition])
		}
	}
	for _, check := range []struct {
		partition string
		listed    []string
	}{
		{partition: "message", listed: controlRouteMessages[:]},
		{partition: "phase", listed: controlRoutePhases[:]},
	} {
		if len(exported[check.partition]) == 0 {
			t.Fatalf("no exported %s partitions: the collector is not registered in this binary", check.partition)
		}
		for _, label := range exported[check.partition] {
			if !slices.Contains(check.listed, label) {
				t.Errorf("exported %s %q is not listed by the control route signal", check.partition, label)
			}
		}
		for _, label := range check.listed {
			if !slices.Contains(exported[check.partition], label) {
				t.Errorf("the control route signal lists %s %q, which no api exports", check.partition, label)
			}
		}
	}
}
