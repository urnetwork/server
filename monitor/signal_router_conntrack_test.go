package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

const syntheticRouterConntrack = "--count--\n100\n--max--\n1000\n--hash--\n256\n--stat--\nentries searched found new invalid ignore delete delete_list insert insert_failed drop early_drop\n00000064 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000001 00000000 00000000\n00000064 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000001 00000000 00000000"

func TestRouterConntrackFirstAndResetSamplesStayUnknown(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = syntheticRouterConntrack
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	now = now.Add(5 * time.Minute)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("stable zero-delta healthy sample alerts=%d error=%v", len(alerts), err)
	}
	source.body = strings.ReplaceAll(syntheticRouterConntrack, "00000001", "00000000")
	now = now.Add(5 * time.Minute)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	requireRouterPrivate(t, alerts)
}

func TestRouterConntrackCorroboratedDropsAndAppliedCapacity(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = syntheticRouterConntrack
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	if _, err := signal.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	source.body = strings.Replace(syntheticRouterConntrack, "00000001 00000000 00000000", "00000002 00000003 00000004", 1)
	now = now.Add(5 * time.Minute)
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "router-conntrack-drops")
	source.body = strings.Replace(syntheticRouterConntrack, "--max--\n1000", "--max--\n500", 1)
	alerts, err = NewRouterConntrackSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "router-conntrack-capacity")
	requireRouterPrivate(t, alerts)
}

func TestRouterConntrackInsertFailureAloneAndMissingIntentAreUnknown(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = syntheticRouterConntrack
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	if _, err := signal.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	source.body = strings.Replace(syntheticRouterConntrack, "00000001 00000000 00000000", "00000002 00000000 00000000", 1)
	now = now.Add(5 * time.Minute)
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	for _, alert := range alerts {
		if alert.Class == "router-conntrack-drops" {
			t.Fatal("duplicate insertion alone became a packet-drop claim")
		}
	}
	source.summary = strings.Replace(syntheticRouterSummary, `"explicit":true`, `"explicit":false`, 1)
	alerts, err = NewRouterConntrackSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
}

func TestRouterConntrackPartialDesiredFieldsRemainIndependent(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	for _, capacity := range []string{`"table_size":500,"hash_size":0`, `"table_size":0,"hash_size":128`} {
		source := newSyntheticRouterSource()
		source.body = syntheticRouterConntrack
		source.summary = strings.Replace(syntheticRouterSummary, `"table_size":1000,"hash_size":256`, capacity, 1)
		alerts, err := NewRouterConntrackSignal().Run(context.Background(), syntheticRouterSettings(source, &now))
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "router-conntrack-capacity")
		requireAlertClass(t, alerts, "cannot-observe")
	}
}

func TestRouterConntrackOccupancyNeverSumsPerCpuEntries(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	source.body = strings.Replace(syntheticRouterConntrack, "--max--\n1000", "--max--\n150", 1)
	source.summary = strings.Replace(syntheticRouterSummary, `"table_size":1000`, `"table_size":150`, 1)
	settings := syntheticRouterSettings(source, &now)
	signal := NewRouterConntrackSignal()
	if _, err := signal.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatal("repeated per-CPU entries inflated 100/150 live occupancy")
	}
}
