// Synthetic catalog frames exercise contract partition retention through the
// public signal. No database is read; contract_partition_native_test.go runs
// the real query.
package monitor

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestContractPartitionRetentionBlockedRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "contract-partition-retention-blocked" {
			continue
		}
		found++
		if signal.Number() != "2.4i" || signal.Cadence() != time.Hour || signal.ID() != "pg/contract-partition-retention" {
			t.Fatal("contract partition retention lost its catalog identity or cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4i contract partition retention is not registered")
	}
	if DefaultContractPartitionRetentionSettings().Retention != server.ContractRetention {
		t.Fatal("the monitor's horizon is not the task's")
	}
}

// Plain tables and partitions inside the horizon plus grace are healthy; the
// line is exclusive.
func TestContractPartitionRetentionHealthy(t *testing.T) {
	now := syntheticSettings(nil).Now()
	currentDay := server.ContractPartitionDayStart(now)
	line := server.ContractRetention + 14*contractPartitionDay
	if settings := DefaultContractPartitionRetentionSettings(); settings.Retention+settings.Grace != line {
		t.Fatalf("the line is %s past a partition's end, want %s", settings.Retention+settings.Grace, line)
	}
	for _, test := range []struct {
		name   string
		source contractPartitionTestSource
	}{
		{name: "plain", source: contractPartitionTestPlain()},
		{name: "fresh cutover", source: contractPartitionTestPartitioned(currentDay, server.ContractPartitionAheadDays)},
		// every day since a cutover 105 days ago, its legacy partition
		// dropped: the oldest day ended 104 days ago, inside the 105-day line
		{name: "days inside the line", source: func() contractPartitionTestSource {
			source := contractPartitionTestPartitioned(currentDay.Add(-105*contractPartitionDay), server.ContractPartitionAheadDays)
			for _, table := range server.ContractPartitionTables {
				source.partitions[table] = source.partitions[table][1:]
			}
			return source
		}()},
		// the legacy partition's upper bound is exactly on the line
		{name: "legacy at the line", source: func() contractPartitionTestSource {
			source := contractPartitionTestPartitioned(currentDay, server.ContractPartitionAheadDays)
			for _, table := range server.ContractPartitionTables {
				source.partitions[table] = []contractPartitionTestPartition{contractPartitionTestLegacy(table, now.Add(-line))}
			}
			return source
		}()},
	} {
		if alerts := contractPartitionTestRun(t, NewContractPartitionRetentionBlockedSignal(), test.source); len(alerts) != 0 {
			t.Fatalf("%s: healthy retention alerted: %s", test.name, alerts[0].Observed)
		}
	}
}

// A legacy partition past the line, and a task that stopped dropping days.
func TestContractPartitionRetentionBlockedFires(t *testing.T) {
	now := syntheticSettings(nil).Now()
	currentDay := server.ContractPartitionDayStart(now)

	// cut over 120 days ago, every day since dropped as due: only the legacy
	// partition outlives the line
	legacy := contractPartitionTestPartitioned(currentDay.Add(-120*contractPartitionDay), server.ContractPartitionAheadDays)
	for _, table := range server.ContractPartitionTables {
		kept := []contractPartitionTestPartition{legacy.partitions[table][0]}
		for _, partition := range legacy.partitions[table][1:] {
			if _, dayStart, _, _ := server.ParseContractPartitionName(partition.name); !dayStart.Add(contractPartitionDay + server.ContractRetention).Before(now) {
				kept = append(kept, partition)
			}
		}
		legacy.partitions[table] = kept
	}
	alert := requireAlertClass(t, contractPartitionTestRun(t, NewContractPartitionRetentionBlockedSignal(), legacy), "contract-partition-retention-blocked")
	if alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.Target != "pg-1/contract-partitions" ||
		!strings.Contains(alert.Observed, "blocked_partitions=6 ") ||
		!strings.Contains(alert.Observed, "oldest_blocked=transfer_contract_legacy oldest_upper_bound="+currentDay.Add(-120*contractPartitionDay).Format(time.DateOnly)) ||
		!strings.Contains(alert.Observed, "blocked_by_table=[transfer_contract=1 transfer_escrow=1 contract_close=1 transfer_escrow_sweep=1 contract_close_report=1 contract_extender=1]") {
		t.Fatalf("wrong legacy retention alert: %s", alert.Observed)
	}

	// nothing dropped for 110 days: the legacy partition and five days per
	// table are past the line of 105 days, listed oldest first up to the bound
	stopped := contractPartitionTestPartitioned(currentDay.Add(-110*contractPartitionDay), server.ContractPartitionAheadDays)
	settings := DefaultContractPartitionRetentionSettings()
	settings.ListLimit = 3
	alert = requireAlertClass(t, contractPartitionTestRun(t, NewContractPartitionRetentionBlockedSignalWithSettings(settings), stopped), "contract-partition-retention-blocked")
	firstDayEnd := currentDay.Add(-109 * contractPartitionDay)
	if !strings.Contains(alert.Observed, "blocked_partitions=36 ") ||
		!strings.Contains(alert.Observed, "blocked_by_table=[transfer_contract=6 ") ||
		!strings.Contains(alert.Observed, "blocked_day_ends=["+currentDay.Add(-110*contractPartitionDay).Format(time.DateOnly)+" "+firstDayEnd.Format(time.DateOnly)+" "+firstDayEnd.Add(contractPartitionDay).Format(time.DateOnly)+" (+3)]") {
		t.Fatalf("wrong stopped-task retention alert: %s", alert.Observed)
	}

	// a partition with an unscheme name is still judged, by its bound, and
	// rendered without its name
	foreign := contractPartitionTestPartitioned(currentDay, server.ContractPartitionAheadDays)
	foreign.partitions["contract_extender"] = append(foreign.partitions["contract_extender"], contractPartitionTestPartition{
		name:  "Private Source Canary",
		bound: "FOR VALUES FROM ('" + contractPartitionTestId(currentDay.Add(-280*contractPartitionDay)) + "') TO ('" + contractPartitionTestId(currentDay.Add(-279*contractPartitionDay)) + "')",
	})
	alert = requireAlertClass(t, contractPartitionTestRun(t, NewContractPartitionRetentionBlockedSignal(), foreign), "contract-partition-retention-blocked")
	if !strings.Contains(alert.Observed, "oldest_blocked=unnamed-partition-of-contract_extender ") {
		t.Fatalf("an unscheme partition was not judged: %s", alert.Observed)
	}
	requireAlertOmits(t, alert, "Private Source Canary")
}
