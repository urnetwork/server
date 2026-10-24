// Synthetic catalog frames for the partition registry signals (SIGNALS.md
// §2.4p-§2.4q). No database is read; partition_registry_native_test.go runs
// the real query.
package monitor

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A daily family keyed by a utc timestamp, as provider_egress_health_history.
func partitionRegistryTestDailyFamily() *server.PartitionFamily {
	return &server.PartitionFamily{
		Name:                "synthetic_history",
		Description:         "a synthetic daily history",
		Tables:              []string{"synthetic_history"},
		KeyColumn:           "measured_at",
		KeyKind:             server.PartitionKeyTimestamp,
		Granularity:         server.PartitionGranularityDaily,
		AheadRanges:         3,
		Retention:           7 * 24 * time.Hour,
		MonitorRunwayRanges: 2,
	}
}

// A weekly family of two tables keyed by ids, with a kept legacy partition.
func partitionRegistryTestWeeklyFamily() *server.PartitionFamily {
	return &server.PartitionFamily{
		Name:                "synthetic_weeks",
		Description:         "a synthetic weekly pair",
		Tables:              []string{"synthetic_weeks_parent", "synthetic_weeks_child"},
		KeyColumn:           "report_id",
		KeyKind:             server.PartitionKeyId,
		Granularity:         server.PartitionGranularityWeekly,
		AheadRanges:         4,
		Retention:           91 * 24 * time.Hour,
		KeepLegacyPartition: true,
		DefaultPartition:    true,
	}
}

type partitionRegistryTestPartition struct {
	name  string
	bound string
	lower string
	upper string
}

// One catalog frame as the primary returns it.
type partitionRegistryTestSource struct {
	now        time.Time
	kinds      map[string]string
	partitions map[string][]partitionRegistryTestPartition
	err        error
	mutate     func([]Row) []Row
}

func partitionRegistryTestPlain(families ...*server.PartitionFamily) partitionRegistryTestSource {
	source := partitionRegistryTestSource{
		now:        syntheticSettings(nil).Now(),
		kinds:      map[string]string{},
		partitions: map[string][]partitionRegistryTestPartition{},
	}
	for _, family := range families {
		for _, table := range family.Tables {
			source.kinds[table] = "r"
		}
	}
	return source
}

// The partition the registry creates for the range starting at start.
func partitionRegistryTestLeaf(family *server.PartitionFamily, table string, start time.Time) partitionRegistryTestPartition {
	r := family.RangeAt(start)
	partition := partitionRegistryTestPartition{name: family.LeafName(table, r)}
	switch family.KeyKind {
	case server.PartitionKeyTimestamp:
		partition.bound = fmt.Sprintf("FOR VALUES FROM ('%s') TO ('%s')", r.Lower.Format("2006-01-02 15:04:05"), r.Upper.Format("2006-01-02 15:04:05"))
		partition.lower = r.Lower.Format("2006-01-02 15:04:05.000000")
		partition.upper = r.Upper.Format("2006-01-02 15:04:05.000000")
	default:
		partition.bound = fmt.Sprintf("FOR VALUES FROM ('%s') TO ('%s')", server.ContractIdFloor(r.Lower), server.ContractIdFloor(r.Upper))
	}
	return partition
}

// Every table of the family partitioned, with ranges from the one first
// ranges back through ahead after the current one.
func (self partitionRegistryTestSource) partitioned(family *server.PartitionFamily, first int, ahead int) partitionRegistryTestSource {
	current := family.Granularity.RangeStart(self.now)
	for _, table := range family.Tables {
		self.kinds[table] = "p"
		for i := -first; i <= ahead; i += 1 {
			self.partitions[table] = append(self.partitions[table], partitionRegistryTestLeaf(family, table, family.Granularity.AddRanges(current, i)))
		}
	}
	return self
}

func (self partitionRegistryTestSource) rows(families []*server.PartitionFamily) []Row {
	rows := []Row{{"meta", "pg-1", retentionTestMicros(self.now), "0", "", ""}}
	tables := 0
	count := 0
	for _, family := range families {
		for _, table := range family.Tables {
			rows = append(rows, Row{"table", table, self.kinds[table], "", "", ""})
			tables++
		}
	}
	for _, family := range families {
		for _, table := range family.Tables {
			for _, partition := range self.partitions[table] {
				rows = append(rows, Row{"partition", table, partition.name, partition.bound, partition.lower, partition.upper})
				count++
			}
		}
	}
	rows = append(rows, Row{"end", strconv.Itoa(tables), strconv.Itoa(count), "", "", ""})
	if self.mutate != nil {
		rows = self.mutate(rows)
	}
	return rows
}

func partitionRegistryTestSettings(t *testing.T, families []*server.PartitionFamily, source partitionRegistryTestSource) SignalSettings {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		wants := []string{"hostname -s", "pg_is_in_recovery()", "pg_inherits", "pg_get_expr(child.relpartbound, child.oid)", "LIMIT 4097", "'YYYY-MM-DD HH24:MI:SS.US'"}
		for _, family := range families {
			for _, table := range family.Tables {
				wants = append(wants, fmt.Sprintf("('%s',%t)", table, family.KeyKind == server.PartitionKeyTimestamp))
			}
		}
		for _, want := range wants {
			if !strings.Contains(query, want) {
				t.Fatalf("partition registry catalog read lost %q", want)
			}
		}
		return source.rows(families), source.err
	}})
	at := source.now
	settings.Now = func() time.Time { return at }
	return settings
}

func partitionRegistryTestRunway(t *testing.T, families []*server.PartitionFamily, source partitionRegistryTestSource, signal Signal) Alerts {
	t.Helper()
	if signal == nil {
		settings := DefaultPartitionRunwaySettings()
		settings.Families = func() []*server.PartitionFamily { return families }
		signal = NewPartitionRunwaySignalWithSettings(settings)
	}
	alerts, err := signal.Run(context.Background(), partitionRegistryTestSettings(t, families, source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func partitionRegistryTestRetention(t *testing.T, families []*server.PartitionFamily, source partitionRegistryTestSource) Alerts {
	t.Helper()
	settings := DefaultPartitionRetentionSettings()
	settings.Families = func() []*server.PartitionFamily { return families }
	alerts, err := NewPartitionRetentionBlockedSignalWithSettings(settings).Run(context.Background(), partitionRegistryTestSettings(t, families, source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// Every registered family is watched by exactly one runway probe and, when
// it drops ranges, one retention probe: the registry's own, or the dedicated
// one its declaration names, which must be a registered signal.
func TestPartitionRegistryProbesCoverEveryRegisteredFamily(t *testing.T) {
	registered := map[string]bool{}
	for _, signal := range NewSignals() {
		registered[signal.Number()] = true
	}
	for _, number := range []string{"2.4p", "2.4q"} {
		if !registered[number] {
			t.Fatalf("§%s is not registered", number)
		}
	}
	families := server.PartitionFamilies()
	names := []string{}
	for _, family := range families {
		names = append(names, family.Name)
	}
	for _, name := range []string{"client_reliability", "contract", "network_ping", "provider_egress_health_history"} {
		if !slices.Contains(names, name) {
			t.Fatalf("family %s is not registered in the monitor's binary: %v", name, names)
		}
	}
	runway := partitionRegistryRunwayFamilies()
	retention := partitionRegistryRetentionFamilies()
	for _, family := range families {
		watched := slices.Contains(runway, family)
		if watched == (family.RunwaySignal != "") {
			t.Errorf("%s: watched by the registry runway %t with a dedicated signal %q", family.Name, watched, family.RunwaySignal)
		}
		if family.RunwaySignal != "" && !registered[family.RunwaySignal] {
			t.Errorf("%s names runway signal §%s, which is not registered", family.Name, family.RunwaySignal)
		}
		if 0 < family.Retention {
			watched := slices.Contains(retention, family)
			if watched == (family.RetentionSignal != "") {
				t.Errorf("%s: watched by the registry retention %t with a dedicated signal %q", family.Name, watched, family.RetentionSignal)
			}
			if family.RetentionSignal != "" && !registered[family.RetentionSignal] {
				t.Errorf("%s names retention signal §%s, which is not registered", family.Name, family.RetentionSignal)
			}
		}
	}
	if !slices.ContainsFunc(runway, func(family *server.PartitionFamily) bool { return family.Name == "provider_egress_health_history" }) ||
		!slices.ContainsFunc(retention, func(family *server.PartitionFamily) bool { return family.Name == "client_reliability" }) {
		t.Fatal("the registry probes do not watch the families without dedicated probes")
	}
}

// The frame is bound by its end row, its identity, its clock and the
// families' tables; any gap is unknown, never a verdict.
func TestPartitionRegistryCatalogRefusesBrokenFrames(t *testing.T) {
	families := []*server.PartitionFamily{partitionRegistryTestDailyFamily()}
	for _, test := range []struct {
		name   string
		mutate func([]Row) []Row
		reason string
	}{
		{"short row", func(rows []Row) []Row { rows[1] = rows[1][:5]; return rows }, "invalid-frame-width"},
		{"wrong host", func(rows []Row) []Row { rows[0][1] = "pg-2"; return rows }, "source-identity-or-role-unavailable"},
		{"standby", func(rows []Row) []Row { rows[0][3] = "1"; return rows }, "source-identity-or-role-unavailable"},
		{"skewed clock", func(rows []Row) []Row {
			rows[0][2] = retentionTestMicros(syntheticSettings(nil).Now().Add(time.Hour))
			return rows
		}, "source-clock-unavailable"},
		{"missing end", func(rows []Row) []Row { return rows[:len(rows)-1] }, "incomplete-frame"},
		{"miscounted partitions", func(rows []Row) []Row { rows[len(rows)-1][2] = "99"; return rows }, "incomplete-frame"},
		{"unknown table", func(rows []Row) []Row { rows[1][1] = "synthetic_other"; return rows }, "invalid-table-row"},
		{"unparseable bound", func(rows []Row) []Row { rows[2][3] = "FOR VALUES IN ('x')"; return rows }, "invalid-partition-bound"},
	} {
		source := partitionRegistryTestPlain(families...).partitioned(families[0], 1, 3)
		source.mutate = test.mutate
		alerts := partitionRegistryTestRunway(t, families, source, nil)
		if len(alerts) != 1 || alerts[0].Class != "partition-runway-unobservable" || !strings.Contains(alerts[0].Observed, "reason="+test.reason) {
			t.Fatalf("%s: %+v", test.name, alerts)
		}
	}
}
