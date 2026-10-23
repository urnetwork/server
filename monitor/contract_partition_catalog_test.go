// Synthetic catalog frames for the contract partition signals (SIGNALS.md
// §2.4h-§2.4j). No database is read.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

type contractPartitionTestPartition struct {
	name  string
	bound string
}

// One catalog frame as the primary returns it.
type contractPartitionTestSource struct {
	now        time.Time
	kinds      map[string]string
	staged     map[string]string
	bounds     map[string]string
	partitions map[string][]contractPartitionTestPartition
	err        error
	mutate     func([]Row) []Row
}

func contractPartitionTestId(at time.Time) string {
	return server.ContractIdFloor(at).String()
}

// The weekly partition the maintenance task creates for weekStart.
func contractPartitionTestWeekly(table string, weekStart time.Time) contractPartitionTestPartition {
	return contractPartitionTestPartition{
		name: server.ContractPartitionName(table, weekStart),
		bound: fmt.Sprintf("FOR VALUES FROM ('%s') TO ('%s')",
			contractPartitionTestId(weekStart), contractPartitionTestId(weekStart.Add(contractPartitionWeek))),
	}
}

// The legacy partition of a cutover at cutoverWeek.
func contractPartitionTestLegacy(table string, cutoverWeek time.Time) contractPartitionTestPartition {
	return contractPartitionTestPartition{
		name:  server.ContractLegacyPartitionName(table),
		bound: fmt.Sprintf("FOR VALUES FROM (MINVALUE) TO ('%s')", contractPartitionTestId(cutoverWeek)),
	}
}

// Plain contract tables at the synthetic clock.
func contractPartitionTestPlain() contractPartitionTestSource {
	source := contractPartitionTestSource{
		now:        syntheticSettings(nil).Now(),
		kinds:      map[string]string{},
		staged:     map[string]string{},
		bounds:     map[string]string{},
		partitions: map[string][]contractPartitionTestPartition{},
	}
	for _, table := range server.ContractPartitionTables {
		source.kinds[table] = "r"
	}
	return source
}

// Tables swapped at cutoverWeek with weekly partitions from it through the
// given number of weeks after the current week.
func contractPartitionTestPartitioned(cutoverWeek time.Time, aheadWeeks int) contractPartitionTestSource {
	source := contractPartitionTestPlain()
	currentWeek := server.ContractPartitionWeekStart(source.now)
	for _, table := range server.ContractPartitionTables {
		source.kinds[table] = "p"
		source.partitions[table] = append(source.partitions[table], contractPartitionTestLegacy(table, cutoverWeek))
		for week := cutoverWeek; !week.After(currentWeek.Add(time.Duration(aheadWeeks) * contractPartitionWeek)); week = week.Add(contractPartitionWeek) {
			source.partitions[table] = append(source.partitions[table], contractPartitionTestWeekly(table, week))
		}
	}
	return source
}

func (self contractPartitionTestSource) rows() []Row {
	rows := []Row{{"meta", "pg-1", retentionTestMicros(self.now), "0", "", "", ""}}
	count := 0
	for _, table := range server.ContractPartitionTables {
		rows = append(rows, Row{"table", table, self.kinds[table], self.staged[table], self.bounds[table], "", ""})
	}
	for _, table := range server.ContractPartitionTables {
		for _, partition := range self.partitions[table] {
			rows = append(rows, Row{"partition", table, partition.name, partition.bound, "", "", ""})
			count++
		}
	}
	rows = append(rows, Row{"end", strconv.Itoa(len(server.ContractPartitionTables)), strconv.Itoa(count), "", "", "", ""})
	if self.mutate != nil {
		rows = self.mutate(rows)
	}
	return rows
}

func contractPartitionTestSettings(t *testing.T, source contractPartitionTestSource) SignalSettings {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{"hostname -s", "pg_is_in_recovery()", "pg_inherits", "pg_get_expr(child.relpartbound, child.oid)", "'_legacy_bound'", "|| '_p'", "('transfer_contract'),('transfer_escrow')", "LIMIT 4097"} {
			if !strings.Contains(query, want) {
				t.Fatalf("contract partition catalog read lost %q", want)
			}
		}
		return source.rows(), source.err
	}})
	at := source.now
	settings.Now = func() time.Time { return at }
	return settings
}

func contractPartitionTestRun(t *testing.T, signal Signal, source contractPartitionTestSource) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), contractPartitionTestSettings(t, source))
	if err != nil {
		t.Fatal("contract partition run failed")
	}
	return alerts
}

// Bounds parse into id ranges; names count as the scheme's only with their
// own bounds.
func TestContractPartitionRangeParsing(t *testing.T) {
	week := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	weekly := contractPartitionTestWeekly("transfer_contract", week)
	legacy := contractPartitionTestLegacy("transfer_contract", week)
	for _, test := range []struct {
		name, partition, bound string
		ok, scheme             bool
		covers                 []time.Time
		misses                 []time.Time
	}{
		{name: "weekly", partition: weekly.name, bound: weekly.bound, ok: true, scheme: true, covers: []time.Time{week}, misses: []time.Time{week.Add(-contractPartitionWeek), week.Add(contractPartitionWeek)}},
		{name: "legacy", partition: legacy.name, bound: legacy.bound, ok: true, scheme: true, covers: []time.Time{week.Add(-contractPartitionWeek), week.Add(-52 * contractPartitionWeek)}, misses: []time.Time{week}},
		{name: "weekly named for another week", partition: server.ContractPartitionName("transfer_contract", week.Add(contractPartitionWeek)), bound: weekly.bound, ok: true, covers: []time.Time{week}},
		{name: "another table's name", partition: server.ContractPartitionName("transfer_escrow", week), bound: weekly.bound, ok: true, covers: []time.Time{week}},
		{name: "open above", partition: "transfer_contract_rest", bound: fmt.Sprintf("FOR VALUES FROM ('%s') TO (MAXVALUE)", contractPartitionTestId(week)), ok: true, covers: []time.Time{week, week.Add(520 * contractPartitionWeek)}, misses: []time.Time{week.Add(-contractPartitionWeek)}},
		{name: "default", partition: "transfer_contract_default", bound: "DEFAULT", ok: true, covers: []time.Time{week}},
		{name: "two weeks", partition: "transfer_contract_pair", bound: fmt.Sprintf("FOR VALUES FROM ('%s') TO ('%s')", contractPartitionTestId(week), contractPartitionTestId(week.Add(2*contractPartitionWeek))), ok: true, covers: []time.Time{week, week.Add(contractPartitionWeek)}},
		{name: "partial week", partition: "transfer_contract_part", bound: fmt.Sprintf("FOR VALUES FROM ('%s') TO ('%s')", contractPartitionTestId(week), contractPartitionTestId(week.Add(contractPartitionWeek-time.Millisecond))), ok: true, misses: []time.Time{week}},
		{name: "reversed", partition: weekly.name, bound: fmt.Sprintf("FOR VALUES FROM ('%s') TO ('%s')", contractPartitionTestId(week.Add(contractPartitionWeek)), contractPartitionTestId(week))},
		{name: "list bound", partition: weekly.name, bound: "FOR VALUES IN ('a')"},
		{name: "not a uuid", partition: weekly.name, bound: "FOR VALUES FROM ('xyz') TO ('abc')"},
		{name: "empty", partition: weekly.name, bound: ""},
	} {
		partition, ok := parseContractPartitionRange("transfer_contract", test.partition, test.bound)
		if ok != test.ok || ok && partition.scheme != test.scheme {
			t.Fatalf("%s: ok=%t scheme=%t, want ok=%t scheme=%t", test.name, ok, partition.scheme, test.ok, test.scheme)
		}
		for _, at := range test.covers {
			if !partition.coversWeek(at) {
				t.Fatalf("%s does not cover the week of %s", test.name, at.Format(time.DateOnly))
			}
		}
		for _, at := range test.misses {
			if partition.coversWeek(at) {
				t.Fatalf("%s covers the week of %s", test.name, at.Format(time.DateOnly))
			}
		}
	}
	if label := (contractPartitionRange{name: "Private Source Canary"}).label("transfer_contract"); label != "unnamed-partition-of-transfer_contract" {
		t.Fatal("an unscheme partition name was rendered")
	}
}

// The prepared bound check names its week in its uuid.
func TestContractPartitionLegacyBoundWeek(t *testing.T) {
	week := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for _, definition := range []string{
		fmt.Sprintf("CHECK ((contract_id < '%s'::uuid))", contractPartitionTestId(week)),
		fmt.Sprintf("CHECK ((contract_id < '%s'::uuid)) NOT VALID", contractPartitionTestId(week)),
	} {
		if parsed, ok := parseContractLegacyBoundWeek(definition); !ok || !parsed.Equal(week) {
			t.Fatalf("bound %q read as %s %t", definition, parsed, ok)
		}
	}
	for _, definition := range []string{"", "CHECK ((contract_id > '00000000-0000-0000-0000-000000000000'::uuid))", "CHECK ((outcome IS NOT NULL))"} {
		if _, ok := parseContractLegacyBoundWeek(definition); ok {
			t.Fatalf("unexpected bound %q parsed", definition)
		}
	}
}

// Every frame defect leaves all three signals unknown, never healthy.
func TestContractPartitionFramesStayUnknown(t *testing.T) {
	currentWeek := server.ContractPartitionWeekStart(syntheticSettings(nil).Now())
	for _, test := range []struct {
		name   string
		mutate func(*contractPartitionTestSource)
		reason string
	}{
		{name: "source error", mutate: func(s *contractPartitionTestSource) { s.err = errors.New("exit status 3: private-source-canary") }, reason: "catalog-source-unavailable"},
		{name: "wrong host", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row { rows[0][1] = "pg-2"; return rows }
		}, reason: "source-identity-or-role-unavailable"},
		{name: "replica", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row { rows[0][3] = "1"; return rows }
		}, reason: "source-identity-or-role-unavailable"},
		{name: "clock", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row { rows[0][2] = retentionTestMicros(s.now.Add(time.Minute)); return rows }
		}, reason: "source-clock-unavailable"},
		{name: "bad clock", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row { rows[0][2] = "-5"; return rows }
		}, reason: "invalid-source-clock"},
		{name: "missing end", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row { return rows[:len(rows)-1] }
		}, reason: "incomplete-frame"},
		{name: "partition count", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row { return append(rows[:7], rows[8:]...) }
		}, reason: "incomplete-frame"},
		{name: "missing table", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row {
				rows = append(rows[:1], rows[2:]...)
				rows[len(rows)-1][1] = "5"
				return rows
			}
		}, reason: "incomplete-frame"},
		{name: "foreign table", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row {
				rows[1][1] = "private-source-canary"
				return rows
			}
		}, reason: "incomplete-frame"},
		{name: "bad kind", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row { rows[1][2] = "v"; return rows }
		}, reason: "invalid-table-row"},
		{name: "unknown frame", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row {
				return append(rows, Row{"private-source-canary", "", "", "", "", "", ""})
			}
		}, reason: "unexpected-frame-kind"},
		{name: "narrow frame", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row { rows[0] = rows[0][:6]; return rows }
		}, reason: "invalid-frame-width"},
		{name: "overbound cell", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row { rows[1][4] = strings.Repeat("x", 300); return rows }
		}, reason: "overbound-cell"},
		{name: "bad bound", mutate: func(s *contractPartitionTestSource) {
			s.partitions["transfer_escrow"][1].bound = "FOR VALUES FROM ('private-source-canary') TO (MAXVALUE)"
		}, reason: "invalid-partition-bound"},
		{name: "unattached parent", mutate: func(s *contractPartitionTestSource) {
			s.mutate = func(rows []Row) []Row { rows[8][1] = "private-source-canary"; return rows }
		}, reason: "invalid-partition-row"},
	} {
		source := contractPartitionTestPartitioned(currentWeek, 4)
		test.mutate(&source)
		for _, signal := range []Signal{NewContractPartitionRunwaySignal(), NewContractPartitionRetentionBlockedSignal(), NewContractPartitionCutoverPendingSignal()} {
			alerts := contractPartitionTestRun(t, signal, source)
			if len(alerts) != 1 || !alerts[0].unobservable && !isVisibilityAlertClass(alerts[0].Class) ||
				!strings.HasSuffix(alerts[0].Class, "-unobservable") || alerts[0].Target != "pg-1/contract-partitions" ||
				!strings.Contains(alerts[0].Observed, "reason="+test.reason) {
				t.Fatalf("%s: %s gave the wrong unknown: %d alerts", test.name, signal.Key(), len(alerts))
			}
			requireAlertOmits(t, alerts[0], "private-source-canary")
		}
	}

	// over-cap census
	settings := DefaultContractPartitionRunwaySettings()
	settings.Catalog.MaxPartitions = 10
	source := contractPartitionTestPartitioned(currentWeek, 4)
	alerts, err := NewContractPartitionRunwaySignalWithSettings(settings).Run(context.Background(), syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if !strings.Contains(query, "LIMIT 11") {
			t.Fatal("the census bound did not reach the read")
		}
		return source.rows(), nil
	}}))
	if err != nil || len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "reason=partition-census-overbound") {
		t.Fatal("an over-cap census was judged")
	}
}

// Missing or ambiguous primaries never read a database.
func TestContractPartitionInventoryAuthority(t *testing.T) {
	for _, mode := range []string{"missing", "ambiguous"} {
		for _, signal := range []Signal{NewContractPartitionRunwaySignal(), NewContractPartitionRetentionBlockedSignal(), NewContractPartitionCutoverPendingSignal()} {
			settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
				t.Fatal("contract partition signal read without one primary")
				return nil, nil
			}})
			if mode == "missing" {
				settings.Hosts = settings.Hosts[1:]
			} else {
				settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-2", Roles: []string{"pg-primary"}})
			}
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "primary-inventory-unavailable-or-ambiguous") ||
				!strings.HasSuffix(alerts[0].Class, "-unobservable") {
				t.Fatalf("%s primary: %s certified its check", mode, signal.Key())
			}
		}
	}
}

// Cancellation before or during the read returns lifecycle with the original
// cause and publishes nothing.
func TestContractPartitionCancellationKeepsOriginalCause(t *testing.T) {
	for _, signal := range []Signal{NewContractPartitionRunwaySignal(), NewContractPartitionRetentionBlockedSignal(), NewContractPartitionCutoverPendingSignal()} {
		for _, stage := range []string{"before", "read"} {
			ctx, cancel := context.WithCancel(context.Background())
			cause := errors.New("private-source-canary")
			settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
				if stage == "before" {
					t.Fatal("canceled work read the database")
				}
				cancel()
				return nil, cause
			}})
			if stage == "before" {
				cancel()
			}
			alerts, err := signal.Run(ctx, settings)
			cancel()
			if len(alerts) != 0 || !errors.Is(err, context.Canceled) || (stage == "read" && !errors.Is(err, cause)) {
				t.Fatalf("%s %s: canceled run lost its causes or published findings", signal.Key(), stage)
			}
			if strings.Contains(err.Error(), "private-source-canary") {
				t.Fatal("lifecycle error exposed its private source")
			}
		}
	}
}
