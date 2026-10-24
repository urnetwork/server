// Synthetic catalog frames exercise the partition registry retention through
// the public signal. No database is read; partition_registry_native_test.go
// runs the real query.
package monitor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestPartitionRetentionBlockedRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "partition-retention-blocked" {
			continue
		}
		found++
		if signal.Number() != "2.4q" || signal.Cadence() != time.Hour || signal.ID() != "pg/partition-retention" {
			t.Fatal("partition retention lost its catalog identity or cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4q partition retention is not registered")
	}
}

// A day survives its retention by the grace and no more: two days of grace
// for a daily family, so a day whose end is 9 days past is blocked and one
// whose end is just inside 9 days is not.
func TestPartitionRetentionHorizonEdge(t *testing.T) {
	daily := partitionRegistryTestDailyFamily()
	families := []*server.PartitionFamily{daily}
	source := partitionRegistryTestPlain(families...)
	current := daily.Granularity.RangeStart(source.now)
	// the clock sits exactly 9 days past the end of the day 10 days back
	source.now = current.AddDate(0, 0, -9)
	source = source.partitioned(daily, 0, 3)
	source.now = current
	edge := partitionRegistryTestLeaf(daily, "synthetic_history", current.AddDate(0, 0, -10))
	source.partitions["synthetic_history"] = append(source.partitions["synthetic_history"], edge)
	if alerts := partitionRegistryTestRetention(t, families, source); len(alerts) != 0 {
		t.Fatalf("a day exactly at its horizon plus grace was blocked: %s", alerts[0].Observed)
	}
	source.now = current.Add(time.Second)
	alert := requireAlertClass(t, partitionRegistryTestRetention(t, families, source), "partition-retention-blocked")
	if alert.Severity != SeverityWarn || alert.Target != "pg-1/synthetic_history-partitions" ||
		!strings.Contains(alert.Observed, "oldest_blocked="+daily.LeafName("synthetic_history", daily.RangeAt(current.AddDate(0, 0, -10)))) ||
		!strings.Contains(alert.Observed, "blocked_partitions=1 ") {
		t.Fatalf("wrong blocked alert: %s %s", alert.Target, alert.Observed)
	}
}

// The DEFAULT partition and a kept legacy partition are never blocked, a
// partition the registry did not name is not its to drop, and a plain family
// has nothing to drop.
func TestPartitionRetentionSkipsKeptPartitions(t *testing.T) {
	weekly := partitionRegistryTestWeeklyFamily()
	families := []*server.PartitionFamily{weekly}
	if alerts := partitionRegistryTestRetention(t, families, partitionRegistryTestPlain(families...)); len(alerts) != 0 {
		t.Fatal("a plain family alerted")
	}
	source := partitionRegistryTestPlain(families...).partitioned(weekly, 0, 4)
	ancient := source.now.AddDate(-2, 0, 0)
	for _, table := range weekly.Tables {
		source.partitions[table] = append(source.partitions[table],
			partitionRegistryTestPartition{name: server.PartitionLegacyLeafName(table), bound: "FOR VALUES FROM (MINVALUE) TO ('" + server.ContractIdFloor(weekly.Granularity.RangeStart(ancient)).String() + "')"},
			partitionRegistryTestPartition{name: server.PartitionDefaultLeafName(table), bound: "DEFAULT"},
			partitionRegistryTestPartition{name: table + "_by_hand", bound: "FOR VALUES FROM ('" + server.ContractIdFloor(ancient.AddDate(0, -1, 0)).String() + "') TO ('" + server.ContractIdFloor(ancient.AddDate(0, 0, -7)).String() + "')"},
		)
	}
	if alerts := partitionRegistryTestRetention(t, families, source); len(alerts) != 0 {
		t.Fatalf("kept or foreign partitions were blocked: %s", alerts[0].Observed)
	}
	// without KeepLegacyPartition the legacy partition retires like a range
	retiring := partitionRegistryTestWeeklyFamily()
	retiring.KeepLegacyPartition = false
	alert := requireAlertClass(t, partitionRegistryTestRetention(t, []*server.PartitionFamily{retiring}, source), "partition-retention-blocked")
	if !strings.Contains(alert.Observed, "oldest_blocked=synthetic_weeks_parent_legacy") || !strings.Contains(alert.Observed, "blocked_partitions=2 ") {
		t.Fatalf("the retiring legacy partition was not blocked: %s", alert.Observed)
	}
}

// A failed read is unknown for each watched family and keeps its source
// private.
func TestPartitionRetentionUnknown(t *testing.T) {
	daily := partitionRegistryTestDailyFamily()
	families := []*server.PartitionFamily{daily}
	source := partitionRegistryTestPlain(families...).partitioned(daily, 1, 3)
	source.err = errors.New("private-source-canary")
	alerts := partitionRegistryTestRetention(t, families, source)
	if len(alerts) != 1 || alerts[0].Class != "partition-retention-unobservable" || !strings.Contains(alerts[0].Observed, "family=synthetic_history reason=catalog-source-unavailable") {
		t.Fatalf("a failed read was judged: %+v", alerts)
	}
	requireAlertOmits(t, alerts[0], "private-source-canary")
}
