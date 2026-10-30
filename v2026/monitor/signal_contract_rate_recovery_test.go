// Public contract observations feed the real sustain gate and notifier.
// All counts, clocks, baseline files and prior incidents are private fixtures.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const contractRateRecoveryCanary = "PRIVATE_CONTRACT_RATE_RECOVERY"

// The public adapter sees the real child context and exactly one scalar read.
type contractRateRecoverySource struct {
	*syntheticSource
	rows   []Row
	retire context.CancelFunc
	calls  int
}

// Return one synthetic scalar through the public source API.
func (self *contractRateRecoverySource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	self.calls++
	if self.retire != nil {
		self.retire()
		self.retire = nil
	}
	return self.rows, nil
}

// Capture only this execution's holds; failed runs retain the run loop's
// existing visibility/error path and cannot manufacture a successful hold.
func contractRateRecoveryExecution(t *testing.T, signal Signal, settings SignalSettings, ctx context.Context) (SignalExecution, error) {
	t.Helper()
	observation := &signalClearHoldObservation{}
	ctx = context.WithValue(ctx, signalClearHoldContextKey{}, observation)
	alerts, err := signal.Run(ctx, settings)
	if err != nil {
		alerts = append(alerts, visibilityAlert(settings, signal, err))
	}
	encoded, encodeErr := json.Marshal(alerts)
	if encodeErr != nil {
		t.Fatal("public recovery JSON rendering failed")
	}
	public := AlertsMarkdown(alerts) + string(encoded)
	if err != nil {
		public += err.Error()
	}
	if strings.Contains(public, contractRateRecoveryCanary) {
		t.Fatal("private recovery source entered public output")
	}
	return SignalExecution{Alerts: alerts, ClearHolds: observation.snapshot(err != nil), Failed: err != nil}, err
}

// This uses the same gate/notifier consumption as the execution loop, without
// a webhook sender, network or replacing the public signal's producer/reducer.
func contractRateRecoveryDeliver(signal Signal, gate *cadenceAlertGate, notifier *SlackAlertNotifier, execution SignalExecution) []slackTransition {
	execution.Alerts, execution.HeldAlerts = gate.split(signal, execution.Alerts)
	notifier.stateLock.Lock()
	defer notifier.stateLock.Unlock()
	return notifier.observeWithLock(time.Now(), signal.Key(), execution)
}

// A real learned-rate incident cannot recover when that comparison loses
// eligible history; two complete qualified healthy readings still clear it.
func TestContractRateRecoveryInsufficientBaselineKeepsIncident(t *testing.T) {
	for _, mode := range []string{"missing-store", "empty-store", "future-only", "expired-history"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, err := newBaselineStore(t.TempDir())
				if err != nil {
					t.Fatal("private baseline setup failed")
				}
				for i := 30; i > 0; i-- {
					store.record(contractRateMetric, time.Now().Add(-time.Duration(i)*time.Minute), 6000)
				}
				source := &contractRateRecoverySource{syntheticSource: &syntheticSource{}, rows: []Row{{"2000"}}}
				settings := syntheticSettings(source)
				settings.Now = time.Now
				settings.runtime = &signalRuntime{baseline: store}
				signal := NewContractRateSignal()
				gate := newCadenceAlertGate()
				notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2}, keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{}}
				raised, cleared := 0, 0
				tick := func() SignalExecution {
					execution, err := contractRateRecoveryExecution(t, signal, settings, context.Background())
					if err != nil {
						t.Fatal("valid count failed")
					}
					for _, transition := range contractRateRecoveryDeliver(signal, gate, notifier, execution) {
						if transition.alert.Class == "contracts-collapse" {
							if transition.kind == slackRaised {
								raised++
							}
							if transition.kind == slackCleared {
								cleared++
							}
						}
					}
					time.Sleep(time.Minute)
					return execution
				}
				for i := 0; i < 3; i++ {
					execution := tick()
					if len(execution.Alerts) != 1 || execution.Alerts[0].Class != "contracts-collapse" || len(execution.ClearHolds) != 0 {
						t.Fatal("prior measured learned incident was not preserved")
					}
				}
				if raised != 1 || cleared != 0 {
					t.Fatal("prior incident did not open through actual sustain gate")
				}
				switch mode {
				case "missing-store":
					settings.runtime = &signalRuntime{}
				case "empty-store", "future-only":
					next, err := newBaselineStore(t.TempDir())
					if err != nil {
						t.Fatal("private replacement baseline failed")
					}
					if mode == "future-only" {
						for i := 0; i < 30; i++ {
							next.record(contractRateMetric, time.Now().Add(time.Hour+time.Duration(i)*time.Minute), 6000)
						}
					}
					settings.runtime = &signalRuntime{baseline: next}
				case "expired-history":
					time.Sleep(61 * time.Minute)
				}
				for i := 0; i < 2; i++ {
					execution := tick()
					if len(execution.Alerts) != 0 || len(execution.ClearHolds) != 1 || execution.ClearHolds[0] != (SignalClassClearHold{SignalId: "pg/contracts-collapse", Class: "contracts-collapse", Target: "pg-1"}) {
						t.Error("insufficient baseline did not retain the exact silent class/target hold")
					}
					if cleared != 0 {
						t.Error("successful inconclusive counts cleared the prior relative-rate incident")
					}
				}
				healthy, err := newBaselineStore(t.TempDir())
				if err != nil {
					t.Fatal("private healthy baseline failed")
				}
				for i := 30; i > 0; i-- {
					healthy.record(contractRateMetric, time.Now().Add(-time.Duration(i)*time.Minute), 6000)
				}
				settings.runtime = &signalRuntime{baseline: healthy}
				source.rows = []Row{{"6000"}}
				for i := 0; i < 2; i++ {
					execution := tick()
					if len(execution.Alerts) != 0 || len(execution.ClearHolds) != 0 {
						t.Error("qualified in-band comparison did not permit healthy recovery")
					}
					if i == 0 && cleared != 0 {
						t.Error("one healthy run cleared prior incident")
					}
				}
				if raised != 1 || cleared != 1 {
					t.Error("two qualified healthy runs did not supply sole recovery")
				}
				if source.calls != 7 {
					t.Fatal("recovery fixture changed one read per execution")
				}
			})
		})
	}
}

// Static outage evidence, target identity and an independent positive remain
// authoritative while the learned arm is deliberately unavailable.
func TestContractRateRecoveryHoldPreservesPositiveAndTargetScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, row := range []struct {
			count    string
			positive bool
		}{{"0", true}, {"999", true}, {"1000", false}, {"9000", false}} {
			source := &contractRateRecoverySource{syntheticSource: &syntheticSource{}, rows: []Row{{row.count}}}
			settings := syntheticSettings(source)
			settings.Now = time.Now
			settings.runtime = &signalRuntime{}
			for i := range settings.Hosts {
				if settings.Hosts[i].Name == "pg-1" {
					settings.Hosts[i].Name = "pg-target-control"
				}
			}
			execution, err := contractRateRecoveryExecution(t, NewContractRateSignal(), settings, context.Background())
			if err != nil || source.calls != 1 {
				t.Fatal("valid scalar changed source authority")
			}
			if row.positive {
				if len(execution.Alerts) != 1 || execution.Alerts[0].Class != "contracts-collapse" || execution.Alerts[0].Severity != SeverityPage || execution.Alerts[0].Sustain != 3 || len(execution.ClearHolds) != 0 {
					t.Error("unlearned baseline erased independent static outage")
				}
			} else {
				if len(execution.Alerts) != 0 || len(execution.ClearHolds) != 1 || execution.ClearHolds[0] != (SignalClassClearHold{SignalId: "pg/contracts-collapse", Class: "contracts-collapse", Target: "pg-target-control"}) {
					t.Error("unavailable baseline hold escaped its exact target")
				}
			}
		}
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "SELECT count(*) FROM transfer_contract") {
				return []Row{{"2000"}}, nil
			}
			return []Row{{"2", "45", "1", "1", "1", "1", "1", "1", "1", "1", "0", "0"}}, nil
		}}
		settings := syntheticSettings(source)
		settings.Now = time.Now
		settings.runtime = &signalRuntime{}
		observation := &signalClearHoldObservation{}
		ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, observation)
		alerts, err := NewWithSignals(settings, NewContractRateSignal(), NewRetentionFanoutSignal()).Run(ctx)
		if err != nil || len(alerts) != 1 || alerts[0].Class != "retention-fanout" {
			t.Fatal("silent contract hold erased independent measured retention positive")
		}
		holds := observation.snapshot(false)
		if len(holds) != 1 || holds[0].Class != "contracts-collapse" {
			t.Error("contract hold acquired sibling class authority")
		}
	})
}

// Errors and cancellation protect a real opened incident through the existing
// failed-execution path; they are not converted into successful silent holds.
func TestContractRateRecoveryMalformedAndCanceledKeepIncident(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := newBaselineStore(t.TempDir())
		if err != nil {
			t.Fatal("private baseline setup failed")
		}
		for i := 30; i > 0; i-- {
			store.record(contractRateMetric, time.Now().Add(-time.Duration(i)*time.Minute), 6000)
		}
		source := &contractRateRecoverySource{syntheticSource: &syntheticSource{}, rows: []Row{{"2000"}}}
		settings := syntheticSettings(source)
		settings.Now = time.Now
		settings.runtime = &signalRuntime{baseline: store}
		signal := NewContractRateSignal()
		gate := newCadenceAlertGate()
		notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2}, keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{}}
		raised, cleared := 0, 0
		deliver := func(execution SignalExecution) {
			for _, transition := range contractRateRecoveryDeliver(signal, gate, notifier, execution) {
				if transition.alert.Class == "contracts-collapse" {
					if transition.kind == slackRaised {
						raised++
					}
					if transition.kind == slackCleared {
						cleared++
					}
				}
			}
			time.Sleep(time.Minute)
		}
		for i := 0; i < 3; i++ {
			execution, err := contractRateRecoveryExecution(t, signal, settings, context.Background())
			if err != nil {
				t.Fatal("prior source failed")
			}
			deliver(execution)
		}
		if raised != 1 {
			t.Fatal("prior incident failed to open")
		}
		for _, mode := range []string{"malformed", "canceled"} {
			for i := 0; i < 2; i++ {
				ctx := context.Background()
				var cancel context.CancelFunc
				if mode == "malformed" {
					source.rows = []Row{{contractRateRecoveryCanary}}
				} else {
					source.rows = []Row{{"2000"}}
					ctx, cancel = context.WithCancel(ctx)
					source.retire = cancel
				}
				execution, runErr := contractRateRecoveryExecution(t, signal, settings, ctx)
				if cancel != nil {
					cancel()
				}
				if runErr == nil || !execution.Failed || len(execution.ClearHolds) != 0 {
					t.Error("invalid or canceled source became successful hold/healthy")
				}
				if mode == "canceled" && !errors.Is(runErr, context.Canceled) {
					t.Error("canceled source lost parent cause")
				}
				deliver(execution)
				if cleared != 0 {
					t.Error("invalid or canceled observation cleared prior incident")
				}
			}
		}
	})
}
