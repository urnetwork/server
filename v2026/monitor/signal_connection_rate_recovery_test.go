// Counter warmup/reset/clock gaps stay silent without granting incident recovery.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Drive the public adapter and real cadence/notifier reducers with private fake SQL.
type connectionRateRecoveryHarness struct {
	signal     Signal
	probe      *pgConnectRateProbe
	settings   SignalSettings
	gate       *cadenceAlertGate
	notifier   *SlackAlertNotifier
	count      int64
	cell       string
	sourceErr  error
	cancelRead context.CancelFunc
}

// Seed only the learned band; actual public measured executions must raise incidents.
func newConnectionRateRecoveryHarness(t *testing.T, scoped bool) *connectionRateRecoveryHarness {
	t.Helper()
	h := &connectionRateRecoveryHarness{signal: NewConnectionRateSignal(), gate: newCadenceAlertGate(), count: 10000}
	h.probe = h.signal.(*signalAdapter).probe.(*pgConnectRateProbe)
	h.notifier = &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "WITH cohorts(label, start_time, end_time)") {
			return nil, nil // Optional cohort diagnosis never supplies the rate.
		}
		if !strings.Contains(query, connectionRateAggregateFixture().queryMarker) {
			t.Fatal("recovery control missed the actual connection counter query")
		}
		if h.cancelRead != nil {
			h.cancelRead()
		}
		if h.sourceErr != nil {
			return nil, h.sourceErr
		}
		if h.cell != "" {
			return []Row{{h.cell}}, nil
		}
		return []Row{{strconv.FormatInt(h.count, 10)}}, nil
	}}
	h.settings = syntheticSettings(source)
	h.settings.Now = time.Now
	h.settings.StateDir = t.TempDir()
	values := make([]float64, 30)
	for i := range values {
		values[i] = 1000
	}
	populateMetric(t, h.settings.StateDir, connectRateMetric, values...)
	if scoped {
		h.settings.Hosts = append(h.settings.Hosts, HostSettings{Name: "synthetic-recovery-excluded"})
		h.settings.ExcludedHosts = []string{"synthetic-recovery-excluded"}
	}
	return h
}

// Match the execution loop's public call, cadence split and failed-run hold policy.
func (self *connectionRateRecoveryHarness) run(ctx context.Context) (SignalExecution, []slackTransition, error) {
	observation := &signalClearHoldObservation{}
	ctx = context.WithValue(ctx, signalClearHoldContextKey{}, observation)
	alerts, err := self.signal.Run(ctx, self.settings)
	ready, held := self.gate.split(self.signal, alerts)
	execution := SignalExecution{Alerts: ready, HeldAlerts: held, Failed: err != nil, ClearHolds: observation.snapshot(err != nil)}
	self.notifier.stateLock.Lock()
	defer self.notifier.stateLock.Unlock()
	return execution, self.notifier.observeWithLock(time.Now(), self.signal.Key(), execution), err
}

// Raise through measured rates and the unchanged sustain, never direct incident injection.
func (self *connectionRateRecoveryHarness) arm(t *testing.T, class string) string {
	t.Helper()
	self.probe.initialized, self.probe.lastCount, self.probe.lastTime = true, self.count, time.Now().Add(-time.Minute)
	steps, delta := 5, int64(100)
	if class == "connects-storm" {
		steps, delta = 3, 3000
	}
	key := ""
	for i := 0; i < steps; i++ {
		if i != 0 {
			time.Sleep(time.Minute)
		}
		self.count += delta
		execution, transitions, err := self.run(context.Background())
		if err != nil || len(execution.ClearHolds) != 0 {
			t.Fatal("qualified measured positive acquired recovery uncertainty")
		}
		for _, transition := range transitions {
			if transition.kind == slackRaised && transition.alert.Class == class {
				key = slackIncidentKey(transition.alert)
			}
		}
	}
	if key == "" || self.notifier.keyIncidents[key] == nil {
		t.Fatal("public measured rate never armed the sustained incident")
	}
	return key
}

// Select only existing unmeasured branches; reset uses two genuinely decreasing counters.
func (self *connectionRateRecoveryHarness) inconclusive(kind string, index int) {
	time.Sleep(time.Minute)
	switch kind {
	case "warmup":
		self.probe.initialized = false
	case "reset":
		self.count = int64(1000 - 500*index)
	case "nonforward":
		self.probe.lastTime = time.Now().Add(time.Minute)
	}
}

// The OLD causal root fails when the second successful unmeasured call clears a real incident.
func TestConnectionRateRecoveryUnmeasuredCannotClearArmedIncident(t *testing.T) {
	for _, class := range []string{"connects-rate", "connects-storm"} {
		for _, kind := range []string{"warmup", "reset", "nonforward"} {
			for _, scoped := range []bool{false, true} {
				t.Run(class+"/"+kind+"/"+strconv.FormatBool(scoped), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						h := newConnectionRateRecoveryHarness(t, scoped)
						key := h.arm(t, class)
						for i := 0; i < 2; i++ {
							h.inconclusive(kind, i)
							execution, transitions, err := h.run(context.Background())
							if err != nil || len(execution.Alerts) != 0 || len(execution.HeldAlerts) != 0 {
								t.Fatal("counter uncertainty became a new incident or failure")
							}
							if h.notifier.keyIncidents[key] == nil {
								t.Fatal("two successful unmeasured counter samples falsely cleared the armed incident")
							}
							for _, transition := range transitions {
								if transition.kind == slackCleared && slackIncidentKey(transition.alert) == key {
									t.Fatal("unmeasured rate emitted recovery")
								}
							}
						}
						for i := 0; i < 2; i++ {
							time.Sleep(time.Minute)
							h.count = h.probe.lastCount + 1000
							execution, transitions, err := h.run(context.Background())
							if err != nil || len(execution.Alerts) != 0 || len(execution.HeldAlerts) != 0 || len(execution.ClearHolds) != 0 {
								t.Fatal("measured healthy paired rate failed to supply recovery")
							}
							if (h.notifier.keyIncidents[key] != nil) != (i == 0) {
								t.Fatal("recovery did not require exactly two measured healthy intervals")
							}
							cleared := 0
							for _, transition := range transitions {
								if transition.kind == slackCleared && slackIncidentKey(transition.alert) == key {
									cleared++
								}
							}
							if (i == 0 && cleared != 0) || (i == 1 && cleared != 1) {
								t.Fatal("healthy recovery transition count changed")
							}
						}
					})
				})
			}
		}
	}
}

// No uncertainty creates an alert, incident or reporter; only the two exact class identities are held.
func TestConnectionRateRecoveryUnmeasuredIsSilentWithoutIncident(t *testing.T) {
	for _, kind := range []string{"warmup", "reset", "nonforward"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newConnectionRateRecoveryHarness(t, false)
				h.probe.initialized, h.probe.lastCount, h.probe.lastTime = true, h.count, time.Now().Add(-time.Minute)
				h.inconclusive(kind, 0)
				execution, transitions, err := h.run(context.Background())
				if err != nil || len(execution.Alerts) != 0 || len(execution.HeldAlerts) != 0 || len(transitions) != 0 || len(h.notifier.keyIncidents) != 0 || len(execution.ClearHolds) != 2 {
					t.Fatal("silent availability changed incident enrollment or emitted an alert")
				}
				seen := map[string]bool{}
				for _, hold := range execution.ClearHolds {
					if hold.SignalId != h.signal.ID() || hold.Target != "pg-1" || (hold.Class != "connects-rate" && hold.Class != "connects-storm") || seen[hold.Class] {
						t.Fatal("counter hold escaped its exact probe/class/target identity")
					}
					seen[hold.Class] = true
				}
			})
		})
	}
}

// Measured independent low/storm findings and strict threshold equalities retain their original output.
func TestConnectionRateRecoveryMeasuredBandsDoNotAcquireHolds(t *testing.T) {
	for _, test := range []struct {
		rate    int64
		class   string
		sustain int
	}{
		{100, "connects-rate", 5}, {500, "", 0}, {1000, "", 0}, {2500, "", 0}, {3000, "connects-storm", 3},
	} {
		t.Run(strconv.FormatInt(test.rate, 10), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newConnectionRateRecoveryHarness(t, true)
				h.probe.initialized, h.probe.lastCount, h.probe.lastTime = true, h.count, time.Now().Add(-time.Minute)
				h.count += test.rate
				execution, _, err := h.run(context.Background())
				if err != nil || len(execution.ClearHolds) != 0 {
					t.Fatal("measured interval became inconclusive")
				}
				all := append(append(Alerts{}, execution.Alerts...), execution.HeldAlerts...)
				if test.class == "" {
					if len(all) != 0 {
						t.Fatal("strict measured equality became an incident")
					}
				} else {
					if len(all) != 1 || all[0].Class != test.class || all[0].Sustain != test.sustain || all[0].Target != "pg-1" {
						t.Fatal("measured independent positive lost identity or sustain")
					}
				}
			})
		})
	}
}

// Failed/malformed sources and ended parents cannot publish holds or grant complete absence.
func TestConnectionRateRecoveryMalformedAndCanceledCannotClear(t *testing.T) {
	for _, kind := range []string{"malformed", "source-error", "cancel-after-read"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newConnectionRateRecoveryHarness(t, true)
				key := h.arm(t, "connects-rate")
				for i := 0; i < 2; i++ {
					ctx, cancel := context.WithCancel(context.Background())
					switch kind {
					case "malformed":
						h.cell = "synthetic-private-recovery-cell"
					case "source-error":
						h.sourceErr = errors.New("synthetic-private-recovery-source")
					case "cancel-after-read":
						h.cancelRead = cancel
						h.probe.initialized = false
					}
					execution, transitions, err := h.run(ctx)
					cancel()
					if err == nil || !execution.Failed || len(execution.ClearHolds) != 0 || len(transitions) != 0 || h.notifier.keyIncidents[key] == nil {
						t.Fatal("failed source or canceled owner supplied recovery metadata")
					}
					if kind == "cancel-after-read" && !errors.Is(err, context.Canceled) {
						t.Fatal("parent cancellation lost lifecycle authority")
					}
				}
				h.cancelRead = nil
				if kind != "cancel-after-read" {
					alerts, err := NewWithSignals(h.settings, h.signal).Run(context.Background())
					// Monitor.Run retains source failure for callers alongside its visibility alert.
					if err == nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
						t.Fatal("failed source must return both its error and public visibility")
					}
					if !alerts[0].unobservable || alerts[0].SignalID != "monitor/visibility" || alerts[0].Target != h.signal.ID() {
						t.Fatal("failed source lost its exact visibility authority")
					}
					if kind == "source-error" && !errors.Is(err, h.sourceErr) {
						t.Fatal("monitor wrapper lost the original source error identity")
					}
					encoded, err := json.Marshal(alerts)
					if err != nil || strings.Contains(string(encoded)+alerts.Markdown(), "synthetic-private-recovery") {
						t.Fatal("private source reached public JSON or Markdown")
					}
				}
			})
		})
	}
}

// The shared adapter's existing accept policy also scopes these leaf-owned identities.
func TestConnectionRateRecoveryHoldsRespectAcceptScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newConnectionRateRecoveryHarness(t, false)
		h.signal.(*signalAdapter).accept = func(one finding) bool { return one.class == "connects-rate" }
		execution, _, err := h.run(context.Background())
		if err != nil || len(execution.ClearHolds) != 1 || execution.ClearHolds[0] != (SignalClassClearHold{SignalId: h.signal.ID(), Class: "connects-rate", Target: "pg-1"}) || len(h.notifier.keyIncidents) != 0 {
			t.Fatal("accept policy enrolled a sibling class or new incident")
		}
	})
}

// A qualified counter delta cannot replace missing learned relative authority.
func TestConnectionRateRecoveryMissingBaselineCannotClearArmedIncident(t *testing.T) {
	for _, class := range []string{"connects-rate", "connects-storm"} {
		for _, missing := range []string{"unavailable-store", "insufficient", "future-only", "expired-only"} {
			t.Run(class+"/"+missing, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					h := newConnectionRateRecoveryHarness(t, true)
					key := h.arm(t, class)
					if missing == "unavailable-store" {
						h.settings.runtime = &signalRuntime{baselineErr: errors.New("synthetic-private-baseline-unavailable")}
					} else {
						store, err := newBaselineStore(t.TempDir())
						if err != nil {
							t.Fatal(err)
						}
						for i := 0; i < 30; i++ {
							at := time.Now().Add(-time.Duration(i+1) * time.Minute)
							switch missing {
							case "insufficient":
								if i >= 27 {
									continue
								}
							case "future-only":
								at = time.Now().Add(time.Hour + time.Duration(i)*time.Minute)
							case "expired-only":
								at = time.Now().Add(-2*time.Hour - time.Duration(i)*time.Minute)
							}
							store.record(connectRateMetric, at, 1000)
						}
						h.settings.runtime = &signalRuntime{baseline: store}
					}
					for i := 0; i < 2; i++ {
						time.Sleep(time.Minute)
						h.count = h.probe.lastCount + 1000
						execution, transitions, err := h.run(context.Background())
						if err != nil || len(execution.Alerts) != 0 || len(execution.HeldAlerts) != 0 {
							t.Fatal("missing learned history manufactured a relative incident")
						}
						if h.notifier.keyIncidents[key] == nil {
							t.Fatal("two measured counters without eligible baseline falsely cleared the armed relative incident")
						}
						for _, transition := range transitions {
							if transition.kind == slackCleared && slackIncidentKey(transition.alert) == key {
								t.Fatal("unavailable baseline emitted relative recovery")
							}
						}
					}
					// Restore the unchanged eligible original history, not an invented static band.
					h.settings.runtime = nil
					for i := 0; i < 2; i++ {
						time.Sleep(time.Minute)
						h.count = h.probe.lastCount + 1000
						execution, transitions, err := h.run(context.Background())
						if err != nil || len(execution.ClearHolds) != 0 || len(execution.Alerts) != 0 || len(execution.HeldAlerts) != 0 {
							t.Fatal("restored measured learned authority did not permit recovery")
						}
						if (h.notifier.keyIncidents[key] != nil) != (i == 0) {
							t.Fatal("restored baseline recovery did not require two complete intervals")
						}
						cleared := 0
						for _, transition := range transitions {
							if transition.kind == slackCleared && slackIncidentKey(transition.alert) == key {
								cleared++
							}
						}
						if (i == 0 && cleared != 0) || (i == 1 && cleared != 1) {
							t.Fatal("restored authority did not clear exactly once")
						}
					}
				})
			})
		}
	}
}

// A known qualified low-volume median is intentional guard-off, not absent history.
func TestConnectionRateRecoveryQualifiedGuardOffRemainsHealthy(t *testing.T) {
	for _, test := range []struct {
		median float64
		rate   int64
		class  string
	}{
		{0, 3000, ""}, {499, 3000, ""}, {500, 1250, ""}, {500, 1251, "connects-storm"},
		{999, 100, ""}, {1000, 500, ""}, {1000, 499, "connects-rate"},
	} {
		t.Run(strconv.FormatFloat(test.median, 'f', 0, 64)+"/"+strconv.FormatInt(test.rate, 10), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newConnectionRateRecoveryHarness(t, false)
				store, err := newBaselineStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				for i := 1; i <= 30; i++ {
					store.record(connectRateMetric, time.Now().Add(-time.Duration(i)*time.Minute), test.median)
				}
				h.settings.runtime = &signalRuntime{baseline: store}
				h.probe.initialized, h.probe.lastCount, h.probe.lastTime = true, h.count, time.Now().Add(-time.Minute)
				h.count += test.rate
				execution, _, err := h.run(context.Background())
				if err != nil || len(execution.ClearHolds) != 0 {
					t.Fatal("known threshold conjunction acquired unavailable-baseline authority")
				}
				all := append(append(Alerts{}, execution.Alerts...), execution.HeldAlerts...)
				if test.class == "" {
					if len(all) != 0 {
						t.Fatal("qualified guard-off state became a relative incident")
					}
				} else if len(all) != 1 || all[0].Class != test.class {
					t.Fatal("qualified guard boundary lost independent positive")
				}
			})
		})
	}
}
