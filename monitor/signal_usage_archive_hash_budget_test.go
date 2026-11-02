// Synthetic catalog frames exercise the usage archive hash budget through its
// public Run. No database is read.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Main's settings on 2026-11-01: work_mem 239674 kB and hash_mem_multiplier
// 16 hold about 61.4M rows of 64 bytes.
const (
	usageArchiveTestWorkMemKb  = "239674"
	usageArchiveTestMultiplier = "16"
)

type usageArchiveTestFrame struct {
	host       string
	clock      time.Time
	recovery   string
	workMemKb  string
	unit       string
	multiplier string
	kind       string
	parentRows string
	partitions map[string]float64
	omitEnd    bool
}

func newUsageArchiveTestFrame(partitions map[string]float64) usageArchiveTestFrame {
	return usageArchiveTestFrame{
		host: "pg-1", clock: syntheticSettings(nil).Now(), recovery: "0",
		workMemKb: usageArchiveTestWorkMemKb, unit: "kB", multiplier: usageArchiveTestMultiplier,
		kind: "p", parentRows: "0", partitions: partitions,
	}
}

func (self usageArchiveTestFrame) rows() []Row {
	rows := []Row{
		{"meta", self.host, strconv.FormatInt(self.clock.UnixMicro(), 10), self.recovery, ""},
		{"settings", self.workMemKb, self.unit, self.multiplier, ""},
		{"parent", self.kind, self.parentRows, "", ""},
	}
	names := []string{}
	for name := range self.partitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rows = append(rows, Row{"partition", name, fmt.Sprintf("%.0f", self.partitions[name]), "", ""})
	}
	if !self.omitEnd {
		rows = append(rows, Row{"end", strconv.Itoa(len(names)), "", "", ""})
	}
	return rows
}

func runUsageArchiveHashBudget(t *testing.T, rows []Row, sourceErr error) []Alert {
	t.Helper()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{"st_provider_usage_archive", "pg_inherits", "reltuples", "work_mem", "hash_mem_multiplier", "monitor_primary_hostname", "pg_is_in_recovery"} {
			if !strings.Contains(query, want) {
				return nil, fmt.Errorf("usage archive hash budget query missing %q", want)
			}
		}
		return rows, sourceErr
	}}
	alerts, err := NewUsageArchiveHashBudgetSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// Main's archive on 2026-11-01: its largest week, 2026-06-01, held about 5.79M
// rows of 20.35M.
func usageArchiveTestMainPartitions() map[string]float64 {
	return map[string]float64{
		"st_provider_usage_archive_w20260601": 5789626,
		"st_provider_usage_archive_w20260608": 232985,
		"st_provider_usage_archive_w20261026": 3100000,
		"st_provider_usage_archive_default":   0,
	}
}

func TestUsageArchiveHashBudgetRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "usage-archive-hash-budget" {
			continue
		}
		found++
		if signal.Number() != "2.4s" || signal.Cadence() != time.Hour || signal.ID() != "pg/usage-archive-hash-budget" {
			t.Fatal("the usage archive hash budget lost its catalog identity or cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4s usage archive hash budget is not registered")
	}
}

// Main's archive fits: twice its largest week is under half of what the hash
// holds; a week at exactly the line is healthy too.
func TestUsageArchiveHashBudgetHealthy(t *testing.T) {
	if alerts := runUsageArchiveHashBudget(t, newUsageArchiveTestFrame(usageArchiveTestMainPartitions()).rows(), nil); len(alerts) != 0 {
		t.Fatalf("Main's archive alerted: %s %s", alerts[0].Class, alerts[0].Observed)
	}
	// 239674 kB * 1024 * 16 / 64 = 61,356,544 rows; half of it is two weeks of 15,339,136
	atLine := map[string]float64{"st_provider_usage_archive_w20261026": 15339136}
	if alerts := runUsageArchiveHashBudget(t, newUsageArchiveTestFrame(atLine).rows(), nil); len(alerts) != 0 {
		t.Fatalf("a week at the line alerted: %s", alerts[0].Observed)
	}
}

// A week past the line, and a plain archive whose whole table a window can
// join.
func TestUsageArchiveHashBudgetOverBudget(t *testing.T) {
	partitions := usageArchiveTestMainPartitions()
	partitions["st_provider_usage_archive_w20261102"] = 15339137
	alert := requireAlertClass(t, runUsageArchiveHashBudget(t, newUsageArchiveTestFrame(partitions).rows(), nil), "usage-archive-hash-budget")
	for _, want := range []string{
		"largest_week=st_provider_usage_archive_w20261102",
		"window_estimate=30678274",
		"hash_rows=61356544",
		"work_mem_kb=239674",
		"hash_mem_multiplier=16",
		"archive_rows=24461748",
		"Never delete archive rows by hand",
		"SIGNALS.md §2.4s",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("over budget alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if alert.SignalNumber != "2.4s" || alert.Sustain != 1 {
		t.Fatalf("wrong over budget identity: %+v", alert)
	}

	plain := newUsageArchiveTestFrame(nil)
	plain.kind, plain.parentRows = "r", "40000000"
	alert = requireAlertClass(t, runUsageArchiveHashBudget(t, plain.rows(), nil), "usage-archive-hash-budget")
	if !strings.Contains(alert.Observed, "largest_week=st_provider_usage_archive ") || !strings.Contains(alert.Observed, "window_estimate=40000000") {
		t.Fatalf("a plain archive's whole table is not the window: %s", alert.Observed)
	}
}

// A frame that cannot be trusted is unknown, never a fit.
func TestUsageArchiveHashBudgetUnobservable(t *testing.T) {
	frame := func(change func(*usageArchiveTestFrame)) []Row {
		value := newUsageArchiveTestFrame(usageArchiveTestMainPartitions())
		change(&value)
		return value.rows()
	}
	for name, test := range map[string]struct {
		rows   []Row
		err    error
		reason string
	}{
		"another host":     {rows: frame(func(f *usageArchiveTestFrame) { f.host = "pg-2" }), reason: "source-identity-or-role-unavailable"},
		"a replica":        {rows: frame(func(f *usageArchiveTestFrame) { f.recovery = "1" }), reason: "source-identity-or-role-unavailable"},
		"a skewed clock":   {rows: frame(func(f *usageArchiveTestFrame) { f.clock = f.clock.Add(time.Minute) }), reason: "source-clock-unavailable"},
		"a work_mem in MB": {rows: frame(func(f *usageArchiveTestFrame) { f.unit = "MB" }), reason: "invalid-settings"},
		"no multiplier":    {rows: frame(func(f *usageArchiveTestFrame) { f.multiplier = "" }), reason: "invalid-settings"},
		"no archive":       {rows: frame(func(f *usageArchiveTestFrame) { f.kind = "" }), reason: "archive-missing"},
		"no end":           {rows: frame(func(f *usageArchiveTestFrame) { f.omitEnd = true }), reason: "incomplete-frame"},
		"no partitions": {rows: frame(func(f *usageArchiveTestFrame) {
			f.partitions = nil
		}), reason: "partitioned-archive-without-partitions"},
		"an unreadable source": {rows: nil, err: errors.New("synthetic catalog failure"), reason: "catalog-source-unavailable"},
	} {
		alerts := runUsageArchiveHashBudget(t, test.rows, test.err)
		alert := requireAlertClass(t, alerts, "usage-archive-hash-budget-unobservable")
		if alert.Sustain != 2 || !strings.Contains(alert.Observed, "reason="+test.reason) || len(alerts) != 1 {
			t.Fatalf("%s: wrong unobservable alert, want %q: %+v", name, test.reason, alerts)
		}
	}
}
