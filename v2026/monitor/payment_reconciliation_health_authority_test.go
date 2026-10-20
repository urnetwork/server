// Health findings require a complete, consistent aggregate whose integer
// domains match the query; positive ages retain their magnitude in seconds.
package monitor

import (
	"fmt"
	"testing"
	"time"
)

// Invalid health stays unknown both alone and beside independently complete
// repair and terminal positives through the existing three-query adapter.
func requirePaymentHealthAuthorityUnknown(t *testing.T, name string, health []Row) {
	t.Helper()
	for _, withAudits := range []bool{false, true} {
		var repairs []Row
		terminal := []Row{{"0", "0", "0", "0", "-1"}}
		wantAlerts := 1
		if withAudits {
			repairs = []Row{{"x402", "credited", "1", "60"}}
			terminal = []Row{{"1", "1", "1", "0", "60"}}
			wantAlerts = 3
		}
		alerts := paymentReconciliationPartialRun(t, health, repairs, terminal, nil)
		if len(alerts) != wantAlerts {
			t.Fatalf("%s (audits=%t): invalid health became healthy, produced a health incident or lost independent audits", name, withAudits)
		}
		requirePaymentReconciliationPartialVisibility(t, alerts, "payment-reconciliation/health", observationErrorClassInvalidResponse)
		if withAudits {
			repair := requireAlertClass(t, alerts, "payment-reconciliation-repair")
			terminalAlert := requireAlertClass(t, alerts, "payment-reconciliation-credit-unfulfillable")
			if repair.Target != "x402" || repair.Severity != SeverityWarn || repair.Frame != "action=credited" ||
				terminalAlert.Target != "stripe" || terminalAlert.Severity != SeverityPage {
				t.Fatalf("%s: invalid health changed complete sibling audit authority", name)
			}
		}
	}
}

// Keep global fields equal while violating their domains so a tuple mismatch
// cannot accidentally supply the negative-count or invalid-age regression.
func TestPaymentReconciliationHealthRejectsInvalidDomains(t *testing.T) {
	for _, test := range []struct {
		name   string
		column int
		value  string
		shared bool
	}{
		{name: "negative error count", column: 4, value: "-1"},
		{name: "negative skipped count", column: 3, value: "-1"},
		{name: "heartbeat below absent sentinel", column: 1, value: "-2", shared: true},
		{name: "watermark below absent sentinel", column: 2, value: "-2"},
		{name: "latest event below absent sentinel", column: 5, value: "-2"},
		{name: "negative task count", column: 6, value: "-1", shared: true},
		{name: "negative task error count", column: 7, value: "-1", shared: true},
		{name: "negative claimed task count", column: 8, value: "-1", shared: true},
		{name: "private malformed numeric cell", column: 4, value: "synthetic-private-cell"},
		{name: "integer outside source range", column: 4, value: "9223372036854775808"},
	} {
		health := paymentReconciliationPartialHealthRows("600")
		for index := range health {
			if test.shared || health[index][0] == "stripe" {
				health[index][test.column] = test.value
			}
		}
		requirePaymentHealthAuthorityUnknown(t, test.name, health)
	}
}

// Each store can contradict each shared field. Reversing delivery order must
// not let the reducer pick one row as a privileged source for global state.
func TestPaymentReconciliationHealthRejectsConflictingSharedTuples(t *testing.T) {
	for _, field := range []struct {
		name   string
		column int
		value  string
	}{
		{name: "heartbeat", column: 1, value: "10801"},
		{name: "task count", column: 6, value: "2"},
		{name: "task error count", column: 7, value: "1"},
		{name: "claimed task count", column: 8, value: "1"},
	} {
		for _, storeIndex := range []int{3, 0, 1, 2, 4} {
			for _, reversed := range []bool{false, true} {
				health := paymentReconciliationPartialHealthRows("600")
				store := health[storeIndex][0]
				health[storeIndex][field.column] = field.value
				if reversed {
					for left, right := 0, len(health)-1; left < right; left, right = left+1, right-1 {
						health[left], health[right] = health[right], health[left]
					}
				}
				name := fmt.Sprintf("%s %s (reversed=%t)", store, field.name, reversed)
				requirePaymentHealthAuthorityUnknown(t, name, health)
			}
		}
	}
}

// Both filtered task counts are subsets of the total, but they can overlap;
// the same task may be claimed while retaining a reschedule error.
func TestPaymentReconciliationHealthTaskSubsetAuthority(t *testing.T) {
	for _, test := range []struct {
		name     string
		count    string
		errors   string
		claimed  string
		invalid  bool
		wantTask bool
	}{
		{name: "error without task", count: "0", errors: "1", claimed: "0", invalid: true},
		{name: "claim without task", count: "0", errors: "0", claimed: "1", invalid: true},
		{name: "errors exceed total", count: "1", errors: "2", claimed: "0", invalid: true},
		{name: "claims exceed total", count: "1", errors: "0", claimed: "2", invalid: true},
		{name: "one idle task", count: "1", errors: "0", claimed: "0"},
		{name: "one claimed task", count: "1", errors: "0", claimed: "1"},
		{name: "missing chain", count: "0", errors: "0", claimed: "0", wantTask: true},
		{name: "errored task also claimed", count: "1", errors: "1", claimed: "1", wantTask: true},
		{name: "duplicate tasks with overlapping subsets", count: "2", errors: "2", claimed: "2", wantTask: true},
	} {
		health := paymentReconciliationPartialHealthRows("600")
		for index := range health {
			health[index][6], health[index][7], health[index][8] = test.count, test.errors, test.claimed
		}
		if test.invalid {
			requirePaymentHealthAuthorityUnknown(t, test.name, health)
			continue
		}
		alerts := paymentReconciliationPartialRun(t, health, nil, []Row{{"0", "0", "0", "0", "-1"}}, nil)
		if !test.wantTask {
			if len(alerts) != 0 {
				t.Fatalf("%s: valid singleton task became an incident or unknown", test.name)
			}
			continue
		}
		if len(alerts) != 1 {
			t.Fatalf("%s: proven task failure lost its independent warning", test.name)
		}
		alert := requireAlertClass(t, alerts, "payment-reconciliation-task")
		if alert.Severity != SeverityWarn || alert.Sustain != 2 || isVisibilityAlert(alert) {
			t.Fatalf("%s: valid task failure was reclassified or changed sustain", test.name)
		}
	}
}

// Age thresholds remain strict and -1 remains the existing absent sentinel.
// A large positive integer cannot wrap into a fresh nanosecond duration.
// Stale heartbeat findings retain visibility policy; watermark findings do not.
func TestPaymentReconciliationHealthAgeBoundaries(t *testing.T) {
	for _, test := range []struct {
		name           string
		column         int
		value          string
		wantClass      string
		wantSeverity   Severity
		wantVisibility bool
	}{
		{name: "fresh heartbeat", column: 1, value: "0"},
		{name: "heartbeat below boundary", column: 1, value: "8999"},
		{name: "heartbeat at boundary", column: 1, value: "9000"},
		{name: "heartbeat above boundary", column: 1, value: "9001", wantClass: "payment-reconciliation-stale", wantSeverity: SeverityPage, wantVisibility: true},
		{name: "absent heartbeat", column: 1, value: "-1", wantClass: "payment-reconciliation-stale", wantSeverity: SeverityPage, wantVisibility: true},
		{name: "largest positive heartbeat age", column: 1, value: "9223372036854775807", wantClass: "payment-reconciliation-stale", wantSeverity: SeverityPage, wantVisibility: true},
		{name: "fresh watermark", column: 2, value: "0"},
		{name: "watermark below warn boundary", column: 2, value: "10799"},
		{name: "watermark at warn boundary", column: 2, value: "10800"},
		{name: "watermark above warn boundary", column: 2, value: "10801", wantClass: "payment-reconciliation-watermark-stale", wantSeverity: SeverityWarn},
		{name: "watermark below page boundary", column: 2, value: "21599", wantClass: "payment-reconciliation-watermark-stale", wantSeverity: SeverityWarn},
		{name: "watermark at page boundary", column: 2, value: "21600", wantClass: "payment-reconciliation-watermark-stale", wantSeverity: SeverityWarn},
		{name: "watermark above page boundary", column: 2, value: "21601", wantClass: "payment-reconciliation-watermark-stale", wantSeverity: SeverityPage},
		{name: "absent watermark", column: 2, value: "-1", wantClass: "payment-reconciliation-watermark-stale", wantSeverity: SeverityPage},
		{name: "largest positive watermark age", column: 2, value: "9223372036854775807", wantClass: "payment-reconciliation-watermark-stale", wantSeverity: SeverityPage},
		{name: "absent latest store event", column: 5, value: "-1"},
		{name: "fresh latest store event", column: 5, value: "0"},
		{name: "large latest store event age", column: 5, value: "9223372036854775807"},
	} {
		health := paymentReconciliationPartialHealthRows("600")
		for index := range health {
			if test.column == 1 || health[index][0] == "stripe" {
				health[index][test.column] = test.value
			}
		}
		alerts := paymentReconciliationPartialRun(t, health, nil, []Row{{"0", "0", "0", "0", "-1"}}, nil)
		if test.wantClass == "" {
			if len(alerts) != 0 {
				t.Fatalf("%s: valid fresh or informational age became an incident or unknown", test.name)
			}
			continue
		}
		if len(alerts) != 1 || alerts[0].Class != test.wantClass {
			t.Fatalf("%s: stale or absent age did not reach its owning incident", test.name)
		}
		alert := requireAlertClass(t, alerts, test.wantClass)
		if alert.Severity != test.wantSeverity || alert.Sustain != 1 || isVisibilityAlert(alert) != test.wantVisibility {
			t.Fatalf("%s: strict age threshold changed severity, sustain or authority", test.name)
		}
		if test.column == 2 && alert.Target != "stripe" {
			t.Fatalf("%s: store age was attributed to a sibling", test.name)
		}
	}
}

// Invalid health is incomplete even before its visibility warning matures.
// Exercise the actual cadence gate and notifier reducer without a sender.
func TestPaymentReconciliationInvalidHealthCannotClearIncident(t *testing.T) {
	signal := NewPaymentReconciliationSignal()
	gate := newCadenceAlertGate()
	notifier := &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	for run := 0; run < 5; run++ {
		health := paymentReconciliationPartialHealthRows("600")
		switch run {
		case 0:
			health = paymentReconciliationPartialHealthRows("10801")
		case 1:
			health[3][4] = "-1"
		case 2:
			health[3][1] = "10801"
		}
		alerts := paymentReconciliationPartialRun(t, health, nil, []Row{{"0", "0", "0", "0", "-1"}}, nil)
		ready, held := gate.split(signal, alerts)
		if run == 1 && (len(ready) != 0 || len(held) != 1 || !isVisibilityAlert(held[0])) {
			t.Fatal("invalid health did not reach the first held visibility boundary")
		}
		if run == 2 && (len(ready) != 1 || !isVisibilityAlert(ready[0]) || len(held) != 0) {
			t.Fatal("contradictory shared health did not retain sustained visibility unknown")
		}
		now := syntheticSettings(nil).Now().Add(time.Duration(run) * 5 * time.Minute)
		notifier.stateLock.Lock()
		transitions := notifier.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
		notifier.stateLock.Unlock()
		raised, cleared := 0, 0
		for _, transition := range transitions {
			if transition.alert.Class != "payment-reconciliation-stale" {
				continue
			}
			if transition.kind == slackRaised {
				raised++
			}
			if transition.kind == slackCleared {
				cleared++
			}
		}
		if run == 0 {
			if raised != 1 || cleared != 0 {
				t.Fatal("complete stale health did not open exactly one incident")
			}
		} else if run < 4 {
			if raised != 0 || cleared != 0 {
				t.Fatalf("run %d: invalid source or first complete recovery changed the health incident", run)
			}
		} else if raised != 0 || cleared != 1 {
			t.Fatal("two complete healthy observations did not clear the incident exactly once")
		}
	}
}
