// Synthetic catalog frames exercise the pending contract partition cutover
// through the public signal. No database is read;
// contract_partition_native_test.go prepares a real cutover.
package monitor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestContractPartitionCutoverPendingRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "contract-partition-cutover-pending" {
			continue
		}
		found++
		if signal.Number() != "2.4j" || signal.Cadence() != 15*time.Minute || signal.ID() != "pg/contract-partition-cutover" {
			t.Fatal("contract partition cutover lost its catalog identity or cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4j contract partition cutover is not registered")
	}
}

// Plain tables prepared for a week that starts in until, with the bound on
// every table.
func contractPartitionTestPrepared(until time.Duration, notValid bool) contractPartitionTestSource {
	source := contractPartitionTestPlain()
	week := source.now.Add(until)
	for _, table := range server.ContractPartitionTables {
		source.staged[table] = "p"
		source.bounds[table] = fmt.Sprintf("CHECK ((contract_id < '%s'::uuid))", contractPartitionTestId(week))
		if notValid {
			source.bounds[table] += " NOT VALID"
		}
	}
	return source
}

// Unprepared, finalized, far, staged-only and window-edge states are healthy.
func TestContractPartitionCutoverPendingHealthy(t *testing.T) {
	currentWeek := server.ContractPartitionWeekStart(syntheticSettings(nil).Now())
	stagedOnly := contractPartitionTestPlain()
	for _, table := range server.ContractPartitionTables {
		stagedOnly.staged[table] = "p"
	}
	for _, test := range []struct {
		name   string
		source contractPartitionTestSource
	}{
		{name: "plain", source: contractPartitionTestPlain()},
		{name: "finalized", source: contractPartitionTestPartitioned(currentWeek, 4)},
		{name: "days away", source: contractPartitionTestPrepared(5*24*time.Hour, false)},
		{name: "at the window", source: contractPartitionTestPrepared(24*time.Hour, true)},
		{name: "staged without a bound", source: stagedOnly},
	} {
		if alerts := contractPartitionTestRun(t, NewContractPartitionCutoverPendingSignal(), test.source); len(alerts) != 0 {
			t.Fatalf("%s: healthy cutover state alerted: %s", test.name, alerts[0].Observed)
		}
	}
}

// Inside the window it warns with the hours to the week and to the task's
// abandonment; past the abandonment line and past the week it says so.
func TestContractPartitionCutoverPendingFires(t *testing.T) {
	for _, test := range []struct {
		name     string
		until    time.Duration
		observed string
		wording  string
	}{
		{name: "inside the window", until: 20 * time.Hour, observed: "hours_until_cutover=20.0 hours_until_abandon=14.0", wording: "14.0 hours remain until then"},
		{name: "past abandonment", until: 5 * time.Hour, observed: "hours_until_cutover=5.0 hours_until_abandon=-1.0", wording: "The abandonment margin passed 1.0 hours ago"},
		{name: "past the week", until: -2 * time.Hour, observed: "hours_until_cutover=-2.0 hours_until_abandon=-8.0", wording: "new contracts are refused now"},
	} {
		source := contractPartitionTestPrepared(test.until, test.name == "inside the window")
		alert := requireAlertClass(t, contractPartitionTestRun(t, NewContractPartitionCutoverPendingSignal(), source), "contract-partition-cutover-pending")
		if alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.Target != "pg-1/contract-partitions" ||
			!strings.Contains(alert.Observed, test.observed) || !strings.Contains(alert.Context, test.wording) ||
			!strings.Contains(alert.Observed, "bounded_tables=[transfer_contract transfer_escrow contract_close transfer_escrow_sweep contract_close_report contract_extender]") ||
			!strings.Contains(alert.Observed, "distinct_bound_weeks=1") {
			t.Fatalf("%s: wrong cutover alert: %s / %s", test.name, alert.Observed, alert.Context)
		}
		if test.until < 0 && !strings.Contains(alert.Symptom, "refuses every contract id of that week") {
			t.Fatalf("%s: a passed week was not called out: %s", test.name, alert.Symptom)
		}
	}

	// tables prepared for different weeks: the earliest decides
	mixed := contractPartitionTestPrepared(5*24*time.Hour, false)
	mixed.bounds["contract_extender"] = fmt.Sprintf("CHECK ((contract_id < '%s'::uuid))", contractPartitionTestId(mixed.now.Add(10*time.Hour)))
	alert := requireAlertClass(t, contractPartitionTestRun(t, NewContractPartitionCutoverPendingSignal(), mixed), "contract-partition-cutover-pending")
	if !strings.Contains(alert.Observed, "hours_until_cutover=10.0 ") || !strings.Contains(alert.Observed, "distinct_bound_weeks=2") {
		t.Fatalf("mixed bounds did not take the earliest week: %s", alert.Observed)
	}
}

// A bound check that is not the prepared shape is unknown.
func TestContractPartitionCutoverPendingInvalidBoundIsUnknown(t *testing.T) {
	source := contractPartitionTestPrepared(20*time.Hour, false)
	source.bounds["contract_close"] = "CHECK ((contract_id <> 'private-source-canary'::uuid))"
	alerts := contractPartitionTestRun(t, NewContractPartitionCutoverPendingSignal(), source)
	if len(alerts) != 1 || alerts[0].Class != "contract-partition-cutover-unobservable" || !strings.Contains(alerts[0].Observed, "reason=invalid-legacy-bound") {
		t.Fatal("an unexpected bound check was judged")
	}
	requireAlertOmits(t, alerts[0], "private-source-canary")
}
