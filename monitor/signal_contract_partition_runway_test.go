// Synthetic catalog frames exercise the contract partition runway through the
// public signal. No database is read; contract_partition_native_test.go runs
// the real query.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestContractPartitionRunwayRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "contract-partition-runway" {
			continue
		}
		found++
		if signal.Number() != "2.4h" || signal.Cadence() != 15*time.Minute || signal.ID() != "pg/contract-partition-runway" {
			t.Fatal("contract partition runway lost its catalog identity or cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4h contract partition runway is not registered")
	}
}

// Plain tables have no runway; the partitioned layout with the task's three
// days ahead, and a cutover two days ahead whose earlier days still route to
// the legacy partition, are healthy.
func TestContractPartitionRunwayHealthy(t *testing.T) {
	currentDay := server.ContractPartitionDayStart(syntheticSettings(nil).Now())
	for _, test := range []struct {
		name   string
		source contractPartitionTestSource
	}{
		{name: "plain", source: contractPartitionTestPlain()},
		{name: "current-day cutover", source: contractPartitionTestPartitioned(currentDay, server.ContractPartitionAheadDays)},
		{name: "older cutover", source: contractPartitionTestPartitioned(currentDay.Add(-140*contractPartitionDay), server.ContractPartitionAheadDays)},
		{name: "cutover two days ahead", source: contractPartitionTestPartitioned(currentDay.Add(2*contractPartitionDay), 2+server.ContractPartitionAheadDays)},
		// the legacy partition alone covers the current day and the next
		{name: "cutover two days ahead with its first day only", source: contractPartitionTestPartitioned(currentDay.Add(2*contractPartitionDay), 2)},
		{name: "exactly two days", source: contractPartitionTestPartitioned(currentDay, 2)},
	} {
		if alerts := contractPartitionTestRun(t, NewContractPartitionRunwaySignal(), test.source); len(alerts) != 0 {
			t.Fatalf("%s: healthy runway alerted: %s %s", test.name, alerts[0].Class, alerts[0].Observed)
		}
	}
	// an open or default partition takes every later day
	for _, bound := range []string{"FOR VALUES FROM ('" + contractPartitionTestId(currentDay.Add(contractPartitionDay)) + "') TO (MAXVALUE)", "DEFAULT"} {
		source := contractPartitionTestPartitioned(currentDay, 0)
		for _, table := range server.ContractPartitionTables {
			source.partitions[table] = append(source.partitions[table], contractPartitionTestPartition{name: table + "_rest", bound: bound})
		}
		if alerts := contractPartitionTestRun(t, NewContractPartitionRunwaySignal(), source); len(alerts) != 0 {
			t.Fatalf("an open partition (%s) left a short runway: %s", bound, alerts[0].Observed)
		}
	}
}

// One table one day short pages; a gap ends the count; an uncovered current
// day says contract creation is failing now.
func TestContractPartitionRunwayFires(t *testing.T) {
	currentDay := server.ContractPartitionDayStart(syntheticSettings(nil).Now())
	short := contractPartitionTestPartitioned(currentDay, server.ContractPartitionAheadDays)
	short.partitions["contract_extender"] = short.partitions["contract_extender"][:3]
	alert := requireAlertClass(t, contractPartitionTestRun(t, NewContractPartitionRunwaySignal(), short), "contract-partition-runway")
	if alert.Severity != SeverityPage || alert.Sustain != 2 || alert.Target != "pg-1/contract-partitions" || alert.Frame != "contract_extender" ||
		!strings.Contains(alert.Observed, "current_day="+currentDay.Format(time.DateOnly)+" ") ||
		!strings.Contains(alert.Observed, "worst_table=contract_extender worst_future_days=1 worst_last_covered_day="+currentDay.Add(contractPartitionDay).Format(time.DateOnly)) ||
		!strings.Contains(alert.Observed, "future_days=[transfer_contract=3 transfer_escrow=3 contract_close=3 transfer_escrow_sweep=3 contract_close_report=3 contract_extender=1]") ||
		!strings.Contains(alert.Observed, "current_day_uncovered=[]") ||
		!strings.Contains(alert.Observed, "minimum_future_days=2") ||
		!strings.Contains(alert.Mechanism, "daily partitions 3 days ahead") {
		t.Fatalf("wrong short-runway alert: %s %s", alert.Frame, alert.Observed)
	}

	gap := contractPartitionTestPartitioned(currentDay, server.ContractPartitionAheadDays)
	daily := gap.partitions["transfer_escrow"]
	gap.partitions["transfer_escrow"] = append(daily[:3:3], daily[4:]...)
	alert = requireAlertClass(t, contractPartitionTestRun(t, NewContractPartitionRunwaySignal(), gap), "contract-partition-runway")
	if alert.Frame != "transfer_escrow" || !strings.Contains(alert.Observed, "worst_future_days=1 ") {
		t.Fatalf("a gap did not end the count: %s", alert.Observed)
	}

	// the legacy partition ends a day ago and the daily ones start tomorrow
	uncovered := contractPartitionTestPlain()
	for _, table := range server.ContractPartitionTables {
		uncovered.kinds[table] = "p"
		uncovered.partitions[table] = []contractPartitionTestPartition{contractPartitionTestLegacy(table, currentDay)}
		for day := 1; day <= server.ContractPartitionAheadDays+1; day++ {
			if table == "contract_close" {
				continue
			}
			uncovered.partitions[table] = append(uncovered.partitions[table], contractPartitionTestDaily(table, currentDay.Add(time.Duration(day)*contractPartitionDay)))
		}
	}
	alert = requireAlertClass(t, contractPartitionTestRun(t, NewContractPartitionRunwaySignal(), uncovered), "contract-partition-runway")
	if alert.Frame != "contract_close" || !strings.Contains(alert.Symptom, "contract creation is failing now") ||
		!strings.Contains(alert.Observed, "current_day_uncovered=[transfer_contract transfer_escrow contract_close transfer_escrow_sweep contract_close_report contract_extender]") {
		t.Fatalf("an uncovered current day was not the worst: %s %s", alert.Symptom, alert.Observed)
	}
}

// transfer_contract partitioned beside a plain contract table is not a layout
// the cutover produces; its runway is unknown.
func TestContractPartitionRunwayMixedLayoutIsUnknown(t *testing.T) {
	source := contractPartitionTestPartitioned(server.ContractPartitionDayStart(syntheticSettings(nil).Now()), server.ContractPartitionAheadDays)
	source.kinds["contract_extender"] = "r"
	source.partitions["contract_extender"] = nil
	alerts := contractPartitionTestRun(t, NewContractPartitionRunwaySignal(), source)
	if len(alerts) != 1 || alerts[0].Class != "contract-partition-runway-unobservable" || !strings.Contains(alerts[0].Observed, "reason=mixed-contract-layout") {
		t.Fatal("a mixed layout was judged")
	}
}

// An unknown runway pages while the last complete observation, inside the
// memory, was already at the line; otherwise it warns.
func TestContractPartitionRunwayUnknownKeepsTheMeasuredRisk(t *testing.T) {
	currentDay := server.ContractPartitionDayStart(syntheticSettings(nil).Now())
	for _, test := range []struct {
		name     string
		ahead    int
		elapsed  time.Duration
		severity Severity
	}{
		{name: "thin", ahead: 2, elapsed: time.Hour, severity: SeverityPage},
		{name: "normal", ahead: server.ContractPartitionAheadDays, elapsed: time.Hour, severity: SeverityWarn},
		{name: "forgotten", ahead: 2, elapsed: 8 * 24 * time.Hour, severity: SeverityWarn},
	} {
		signal := NewContractPartitionRunwaySignal()
		source := contractPartitionTestPartitioned(currentDay, test.ahead)
		if alerts := contractPartitionTestRun(t, signal, source); len(alerts) != 0 {
			t.Fatalf("%s: the measured run alerted", test.name)
		}
		failing := source
		failing.now = source.now.Add(test.elapsed)
		failing.err = errors.New("private-source-canary")
		alerts := contractPartitionTestRun(t, signal, failing)
		if len(alerts) != 1 || alerts[0].Class != "contract-partition-runway-unobservable" || alerts[0].Severity != test.severity {
			t.Fatalf("%s: unknown runway severity is wrong", test.name)
		}
		requireAlertOmits(t, alerts[0], "private-source-canary")
	}
	// with no measured run the unknown warns
	source := contractPartitionTestPartitioned(currentDay, 2)
	source.err = errors.New("private-source-canary")
	alerts, err := NewContractPartitionRunwaySignal().Run(context.Background(), contractPartitionTestSettings(t, source))
	if err != nil || len(alerts) != 1 || alerts[0].Severity != SeverityWarn || !strings.Contains(alerts[0].Observed, "last_complete_future_days=unknown") {
		t.Fatal("a first unknown did not warn")
	}
}
