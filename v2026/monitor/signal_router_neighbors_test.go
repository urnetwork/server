package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRouterNeighborsNeedFreshExactActiveFailurePair(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = "192.0.2.1 dev eth0 used 1/300/2 probes 6 FAILED"
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterNeighborsSignal()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	now = now.Add(5 * time.Minute)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "router-neighbor-active-failure")
	requireRouterPrivate(t, alerts)
	source.desired += "\n/* changed synthetic desired generation */\n"
	now = now.Add(5 * time.Minute)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
}

func TestRouterNeighborsColdStaleAndHistoricalProbesAreNotOutages(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	for _, body := range []string{"", "192.0.2.1 dev eth0 used 300/300/300 probes 6 FAILED", "192.0.2.1 dev eth0 used 1/300/2 probes 0 INCOMPLETE", "192.0.2.1 dev eth0 lladdr 02:00:00:00:00:01 used 1/300/2 probes 6 STALE", "192.0.2.1 dev eth1 used 1/300/2 probes 6 FAILED"} {
		source := newSyntheticRouterSource()
		source.body = body
		settings := syntheticRouterSettings(source, &now)
		signal := NewRouterNeighborsSignal()
		for sample := 0; sample < 2; sample++ {
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			requireAlertClass(t, alerts, "cannot-observe")
			for _, alert := range alerts {
				if alert.Class == "router-neighbor-active-failure" {
					t.Fatal("cold/stale/wrong-interface cache state was labeled an active outage")
				}
			}
			now = now.Add(5 * time.Minute)
		}
	}
}

func TestRouterNeighborsHealthyAndPartialAuthority(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = "192.0.2.1 dev eth0 lladdr 02:00:00:00:00:01 used 1/1/1 probes 1 REACHABLE"
	settings := syntheticRouterSettings(source, &now)
	alerts, err := NewRouterNeighborsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("healthy neighbor alerts=%d error=%v", len(alerts), err)
	}
	source.summary = strings.Replace(syntheticRouterSummary, `"topology":{"complete":true`, `"topology":{"complete":false`, 1)
	alerts, err = NewRouterNeighborsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
}

func TestRouterNeighborsKnownPathSurvivesUnenumeratedRaScope(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = "192.0.2.1 dev eth0 used 1/300/2 probes 6 FAILED"
	source.summary = strings.Replace(syntheticRouterSummary, `"topology":{"complete":true,"reason":""`, `"topology":{"complete":true,"reason":"derived-explicit-neighbors-only"`, 1)
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterNeighborsSignal()
	if _, err := signal.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "router-neighbor-active-failure")
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if unknown.Frame != "coverage" {
		t.Fatal("known explicit path failure erased dynamic neighbor census uncertainty")
	}
}

// Upstream main's print_cacheinfo does not terminate the updated age with a
// space; print_neigh prints probes or the state immediately after it.
// https://raw.githubusercontent.com/iproute2/iproute2/main/ip/ipneigh.c
// The v4.9.0 producer instead emits a leading space before probes and state.
// https://raw.githubusercontent.com/iproute2/iproute2/v4.9.0/ip/ipneigh.c
func TestRouterNeighborsModernConcatenatedProbesParser(t *testing.T) {
	for _, address := range []string{"192.0.2.1", "2001:db8::1"} {
		body := address + " dev eth0 lladdr 02:00:00:00:00:01  ref 1 used 4/0/0probes 1 REACHABLE \n"
		entries, err := parseRouterNeighbors(body)
		if err != nil {
			t.Error("the modern producer's adjacent updated-age/probes field was rejected")
			continue
		}
		entry, ok := entries[routerNeighborKey("eth0", address)]
		if !ok || len(entries) != 1 || !entry.stats || entry.used != 4 || entry.confirmed != 0 || entry.updated != 0 || entry.probes != 1 || entry.state != "REACHABLE" {
			t.Error("modern producer fields were not retained exactly")
		}
	}
}

func TestRouterNeighborsModernConcatenatedStateParser(t *testing.T) {
	// NDA_PROBES is optional; a missing probe counter is not a fabricated zero
	// activity proof. Confirmed REACHABLE remains positive cache evidence.
	body := "192.0.2.1 dev eth0 lladdr 02:00:00:00:00:01  used 4/0/0REACHABLE \n"
	entries, err := parseRouterNeighbors(body)
	if err != nil {
		t.Fatal("the modern producer's adjacent updated-age/state field was rejected")
	}
	entry, ok := entries[routerNeighborKey("eth0", "192.0.2.1")]
	if !ok || len(entries) != 1 || !entry.stats || entry.used != 4 || entry.confirmed != 0 || entry.updated != 0 || entry.probes != 0 || entry.state != "REACHABLE" {
		t.Fatal("optional-probe modern producer fields were not retained exactly")
	}
}

func TestRouterNeighborsModernConcatenatedHealthyRun(t *testing.T) {
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	for _, body := range []string{
		"192.0.2.1 dev eth0 lladdr 02:00:00:00:00:01  used 4/0/0probes 1 REACHABLE \n",
		"192.0.2.1 dev eth0 lladdr 02:00:00:00:00:01  used 4/0/0REACHABLE \n",
	} {
		source := newSyntheticRouterSource()
		source.body = body
		alerts, err := NewRouterNeighborsSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 0 {
			t.Error("valid modern producer output became public visibility failure instead of healthy exact-neighbor evidence")
		}
		requireRouterPrivate(t, alerts)
	}
}

func TestRouterNeighborsModernConcatenatedFailureRun(t *testing.T) {
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = "192.0.2.1 dev eth0  used 4/300/0probes 6 FAILED \n"
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterNeighborsSignal()
	first, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, first, "cannot-observe")
	now = now.Add(5 * time.Minute)
	second, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, second, "router-neighbor-active-failure")
	if !strings.Contains(alert.Observed, "active_failed_neighbors=1") || !strings.Contains(alert.Observed, "samples=2") {
		t.Fatal("modern output did not retain the exact active failure pair")
	}
	requireRouterPrivate(t, second)
}

func TestRouterNeighborsLegacySpacedProducerControl(t *testing.T) {
	body := "192.0.2.1 dev eth0 lladdr 02:00:00:00:00:01 router ref 1 used 4/0/0 probes 1 REACHABLE\n"
	entries, err := parseRouterNeighbors(body)
	if err != nil {
		t.Fatal("the established spaced producer shape was rejected")
	}
	entry := entries[routerNeighborKey("eth0", "192.0.2.1")]
	if len(entries) != 1 || !entry.stats || entry.used != 4 || entry.confirmed != 0 || entry.updated != 0 || entry.probes != 1 || entry.state != "REACHABLE" {
		t.Fatal("legacy producer fields changed")
	}
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = body
	alerts, err := NewRouterNeighborsSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
	if err != nil || len(alerts) != 0 {
		t.Fatal("the old-format healthy control did not remain healthy through the public signal")
	}
}

func TestRouterNeighborsConcatenationRejectsMalformedAndAmbiguousRows(t *testing.T) {
	for _, suffix := range []string{
		"used 4/0/0junkprobes 1 REACHABLE",
		"used 4/0/0probesXYZ 1 REACHABLE",
		"used 4/0/0probes -1 REACHABLE",
		"used 4/0/0probes 1 probes 2 REACHABLE",
		"used 4/0/0probes 1probes 2 REACHABLE",
		"used 4/0/0probes 1 REACHABLE unknown",
		"used 4/0/0REACHABLEjunk",
		"used 4/0/0REACHABLE STALE",
		"used 4/0/0probes 1 REACHABLE used 4/0/0",
		"used 4/0/18446744073709551616probes 1 REACHABLE",
		"used4/0/0probes 1 REACHABLE",
	} {
		if _, err := parseRouterNeighbors("192.0.2.1 dev eth0 " + suffix); err == nil {
			t.Error("malformed or ambiguous concatenation was silently normalized")
		}
	}
	duplicate := "192.0.2.1 dev eth0 used 4/0/0 probes 1 REACHABLE\n192.0.2.1 dev eth0 used 4/0/0 probes 1 REACHABLE\n"
	if _, err := parseRouterNeighbors(duplicate); err == nil {
		t.Fatal("duplicate exact neighbor rows were accepted")
	}
}
