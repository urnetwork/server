// Synthetic catalog frames exercise the partition registry runway through the
// public signal. No database is read; partition_registry_native_test.go runs
// the real query.
package monitor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestPartitionRunwayRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "partition-runway" {
			continue
		}
		found++
		if signal.Number() != "2.4p" || signal.Cadence() != 15*time.Minute || signal.ID() != "pg/partition-runway" {
			t.Fatal("partition runway lost its catalog identity or cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4p partition runway is not registered")
	}
}

// A plain family has no runway; a family with its days ahead, and one at
// exactly its line, are healthy; a DEFAULT partition covers nothing, so it
// cannot stand in for missing ranges.
func TestPartitionRunwayHealthy(t *testing.T) {
	daily := partitionRegistryTestDailyFamily()
	weekly := partitionRegistryTestWeeklyFamily()
	families := []*server.PartitionFamily{daily, weekly}
	for _, test := range []struct {
		name   string
		source partitionRegistryTestSource
	}{
		{"plain", partitionRegistryTestPlain(families...)},
		{"ahead", partitionRegistryTestPlain(families...).partitioned(daily, 7, 3).partitioned(weekly, 13, 4)},
		{"at the line", partitionRegistryTestPlain(families...).partitioned(daily, 0, 2).partitioned(weekly, 0, 2)},
	} {
		if alerts := partitionRegistryTestRunway(t, families, test.source, nil); len(alerts) != 0 {
			t.Fatalf("%s: healthy runway alerted: %s %s", test.name, alerts[0].Class, alerts[0].Observed)
		}
	}
	// the legacy partition covers the weeks below its cutover
	source := partitionRegistryTestPlain(families...).partitioned(daily, 1, 3)
	current := weekly.Granularity.RangeStart(source.now)
	cutover := weekly.Granularity.AddRanges(current, 2)
	for _, table := range weekly.Tables {
		source.kinds[table] = "p"
		source.partitions[table] = []partitionRegistryTestPartition{{name: server.PartitionLegacyLeafName(table), bound: "FOR VALUES FROM (MINVALUE) TO ('" + server.ContractIdFloor(cutover).String() + "')"}}
		for i := 0; i < 3; i += 1 {
			source.partitions[table] = append(source.partitions[table], partitionRegistryTestLeaf(weekly, table, weekly.Granularity.AddRanges(cutover, i)))
		}
	}
	if alerts := partitionRegistryTestRunway(t, families, source, nil); len(alerts) != 0 {
		t.Fatalf("the legacy partition's weeks did not count: %s", alerts[0].Observed)
	}
}

// One range short of the line pages with the family and the thinnest table;
// a gap ends the count; an uncovered current range says inserts fail now; a
// DEFAULT partition does not hide it.
func TestPartitionRunwayFires(t *testing.T) {
	daily := partitionRegistryTestDailyFamily()
	weekly := partitionRegistryTestWeeklyFamily()
	families := []*server.PartitionFamily{daily, weekly}
	healthyWeekly := func(source partitionRegistryTestSource) partitionRegistryTestSource {
		return source.partitioned(weekly, 1, 4)
	}

	short := healthyWeekly(partitionRegistryTestPlain(families...).partitioned(daily, 1, 1))
	alert := requireAlertClass(t, partitionRegistryTestRunway(t, families, short, nil), "partition-runway")
	if alert.Severity != SeverityPage || alert.Sustain != 2 || alert.Target != "pg-1/synthetic_history-partitions" || alert.Frame != "synthetic_history" ||
		!strings.Contains(alert.Observed, "family=synthetic_history") || !strings.Contains(alert.Observed, "worst_future_ranges=1 ") ||
		!strings.Contains(alert.Observed, "minimum_future_ranges=2") {
		t.Fatalf("wrong short-runway alert: %s %s %s", alert.Target, alert.Frame, alert.Observed)
	}

	gapped := healthyWeekly(partitionRegistryTestPlain(families...).partitioned(daily, 1, 3))
	leaves := gapped.partitions["synthetic_history"]
	// drop tomorrow's day: the count ends at the current day
	gapped.partitions["synthetic_history"] = append(leaves[:2:2], leaves[3:]...)
	alert = requireAlertClass(t, partitionRegistryTestRunway(t, families, gapped, nil), "partition-runway")
	if !strings.Contains(alert.Observed, "worst_future_ranges=0 ") {
		t.Fatalf("a gap did not end the count: %s", alert.Observed)
	}

	// the child table lacks the current week, beside a DEFAULT partition
	uncovered := partitionRegistryTestPlain(families...).partitioned(daily, 1, 3).partitioned(weekly, 0, 4)
	child := uncovered.partitions["synthetic_weeks_child"]
	uncovered.partitions["synthetic_weeks_child"] = append(child[1:], partitionRegistryTestPartition{name: "synthetic_weeks_child_default", bound: "DEFAULT"})
	alert = requireAlertClass(t, partitionRegistryTestRunway(t, families, uncovered, nil), "partition-runway")
	if alert.Target != "pg-1/synthetic_weeks-partitions" || alert.Frame != "synthetic_weeks_child" || !strings.Contains(alert.Symptom, "failing now") ||
		!strings.Contains(alert.Observed, "current_range_uncovered=[synthetic_weeks_child]") {
		t.Fatalf("an uncovered current range was not the worst: %s %s %s", alert.Frame, alert.Symptom, alert.Observed)
	}
}

// A family whose first table is partitioned and another not is unknown.
func TestPartitionRunwayMixedLayoutIsUnknown(t *testing.T) {
	weekly := partitionRegistryTestWeeklyFamily()
	families := []*server.PartitionFamily{weekly}
	source := partitionRegistryTestPlain(families...).partitioned(weekly, 1, 4)
	source.kinds["synthetic_weeks_child"] = "r"
	source.partitions["synthetic_weeks_child"] = nil
	alerts := partitionRegistryTestRunway(t, families, source, nil)
	if len(alerts) != 1 || alerts[0].Class != "partition-runway-unobservable" || !strings.Contains(alerts[0].Observed, "reason=mixed-family-layout") {
		t.Fatalf("a mixed layout was judged: %+v", alerts)
	}
}

// An unknown runway pages while the family's last complete observation,
// inside the memory, was at its line; otherwise it warns, and a failed read
// keeps its source private.
func TestPartitionRunwayUnknownKeepsTheMeasuredRisk(t *testing.T) {
	daily := partitionRegistryTestDailyFamily()
	families := []*server.PartitionFamily{daily}
	for _, test := range []struct {
		name     string
		ahead    int
		elapsed  time.Duration
		severity Severity
	}{
		{name: "thin", ahead: 2, elapsed: time.Hour, severity: SeverityPage},
		{name: "normal", ahead: 3, elapsed: time.Hour, severity: SeverityWarn},
		{name: "forgotten", ahead: 2, elapsed: 8 * 24 * time.Hour, severity: SeverityWarn},
	} {
		settings := DefaultPartitionRunwaySettings()
		settings.Families = func() []*server.PartitionFamily { return families }
		signal := NewPartitionRunwaySignalWithSettings(settings)
		source := partitionRegistryTestPlain(families...).partitioned(daily, 1, test.ahead)
		if alerts := partitionRegistryTestRunway(t, families, source, signal); len(alerts) != 0 {
			t.Fatalf("%s: the measured run alerted", test.name)
		}
		failing := source
		failing.now = source.now.Add(test.elapsed)
		failing.err = errors.New("private-source-canary")
		alerts := partitionRegistryTestRunway(t, families, failing, signal)
		if len(alerts) != 1 || alerts[0].Class != "partition-runway-unobservable" || alerts[0].Severity != test.severity {
			t.Fatalf("%s: unknown runway severity is wrong: %+v", test.name, alerts)
		}
		requireAlertOmits(t, alerts[0], "private-source-canary")
	}
}

// No primary, or several, is unknown for every watched family; no watched
// family reads nothing and is healthy.
func TestPartitionRunwayInventoryAuthority(t *testing.T) {
	daily := partitionRegistryTestDailyFamily()
	families := []*server.PartitionFamily{daily}
	settings := partitionRegistryTestSettings(t, families, partitionRegistryTestPlain(families...))
	settings.Hosts = nil
	runwaySettings := DefaultPartitionRunwaySettings()
	runwaySettings.Families = func() []*server.PartitionFamily { return families }
	alerts, err := NewPartitionRunwaySignalWithSettings(runwaySettings).Run(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Class != "partition-runway-unobservable" || !strings.Contains(alerts[0].Observed, "reason=primary-inventory-unavailable-or-ambiguous") {
		t.Fatalf("a missing primary was judged: %+v", alerts)
	}
	none := DefaultPartitionRunwaySettings()
	none.Families = func() []*server.PartitionFamily { return nil }
	alerts, err = NewPartitionRunwaySignalWithSettings(none).Run(t.Context(), syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
		t.Fatal("no family should read nothing")
		return nil, nil
	}}))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("no family alerted: %v %+v", err, alerts)
	}
}
