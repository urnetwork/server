// Source-only proposal: zero measured outcomes cannot clear an existing incident.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Public Run, the real cadence gate and notifier share one current population.
// The handled-run barrier proves both zero recovery credit and later clearing.
func TestEgressOutcomesRecoveryRequiresMeasuredOutcomes(t *testing.T) {
	for _, class := range []string{"egress-common-mode", "egress-mixed-failure", "egress-outcome-inconsistent", "egress-outcome-unknown"} {
		t.Run(class, func(t *testing.T) {
			var step atomic.Int32
			modes := []string{"incident", "incident", "unobserved", "unobserved", "healthy", "unobserved", "healthy", "healthy"}
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if !strings.Contains(query, "monitor-signal-2.23-egress-outcomes") {
					return nil, errors.New("egress recovery control reached an unrelated query")
				}
				snapshot := egressOutcomeSnapshot{eligible: 100, observed: 100, successes: 100, newestOutcomeAgeSeconds: 60, oldestOutcomeAgeSeconds: 120}
				switch modes[int(step.Load())] {
				case "incident":
					switch class {
					case "egress-common-mode":
						snapshot.successes, snapshot.failures, snapshot.tunnelFailed = 10, 90, 90
					case "egress-mixed-failure":
						snapshot.successes, snapshot.failures, snapshot.tunnelFailed, snapshot.contractFailed = 10, 90, 45, 45
					case "egress-outcome-inconsistent":
						snapshot.observed, snapshot.successes, snapshot.inconsistent = 99, 99, 1
					case "egress-outcome-unknown":
						snapshot.successes, snapshot.failures, snapshot.unknownFailure = 99, 1, 1
					}
				case "unobserved":
					snapshot = egressOutcomeSnapshot{eligible: 100, unobserved: 100, newestOutcomeAgeSeconds: -1, oldestOutcomeAgeSeconds: -1}
				}
				return []Row{syntheticEgressOutcomeRow(snapshot)}, nil
			}}
			settings := syntheticSettings(source)
			settings.Now = func() time.Time {
				return time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC).Add(time.Duration(step.Load()) * 5 * time.Minute)
			}
			webhook, webhookUrl := newFakeSlackWebhook(t)
			harness := newSlackTestHarness()
			notifierSettings := harness.settings(webhookUrl)
			notifierSettings.ClearAfterCompleteRuns = 2
			notifier := NewSlackAlertNotifier(notifierSettings)
			t.Cleanup(func() { closeSlackNotifier(t, notifier) })
			signal := NewEgressOutcomesSignal()
			incidentKey := signal.ID() + "|" + class + "|pg-1"
			assertIncident := func(wantPresent bool, wantCredit int) {
				t.Helper()
				notifier.stateLock.Lock()
				incident, present := notifier.keyIncidents[incidentKey]
				credit, owned := -1, false
				if present {
					credit, owned = incident.signalKeyAbsentRuns[signal.Key()]
				}
				notifier.stateLock.Unlock()
				if present != wantPresent || present && (!owned || credit != wantCredit) {
					t.Fatalf("egress recovery causal assertion: class=%s present=%t credit=%d; want present=%t credit=%d", class, present, credit, wantPresent, wantCredit)
				}
			}
			postSignalRunsThroughTheGate(t, signal, settings, len(modes), func(run int) {
				// Previous Handle completed before the next source frame is selected.
				if run >= 2 {
					wantCredit := 0
					if run == 5 || run == 7 {
						wantCredit = 1
					}
					assertIncident(true, wantCredit)
				}
				step.Store(int32(run))
				if run > 0 {
					harness.advance(5 * time.Minute)
				}
			}, notifier)
			closeSlackNotifier(t, notifier)
			assertIncident(false, 0)
			identity := "\n" + signal.ID() + " · " + class + " · pg-1\n"
			if countSlackPosts(webhook.posted(), "[CLEARED]", identity) != 1 {
				t.Fatal("measured recovery control did not produce exactly one clear")
			}
		})
	}
}

// Benign absence stays silent, with exact class holds; positives survive beside it.
func TestEgressOutcomesRecoveryHoldsAreSilentAndKeepIndependentPositives(t *testing.T) {
	for _, mode := range []string{"empty", "unobserved", "inconsistent", "partial-positive", "healthy"} {
		t.Run(mode, func(t *testing.T) {
			snapshot := egressOutcomeSnapshot{newestOutcomeAgeSeconds: -1, oldestOutcomeAgeSeconds: -1}
			switch mode {
			case "unobserved":
				snapshot.eligible, snapshot.unobserved = 100, 100
			case "inconsistent":
				snapshot.eligible, snapshot.inconsistent, snapshot.unobserved = 100, 1, 99
			case "partial-positive":
				snapshot = egressOutcomeSnapshot{eligible: 100, observed: 90, failures: 90, tunnelFailed: 90, unobserved: 10, newestOutcomeAgeSeconds: 60, oldestOutcomeAgeSeconds: 120}
			case "healthy":
				snapshot = egressOutcomeSnapshot{eligible: 100, observed: 100, successes: 100, newestOutcomeAgeSeconds: 60, oldestOutcomeAgeSeconds: 120}
			}
			observation := &signalClearHoldObservation{}
			ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, observation)
			alerts, err := NewEgressOutcomesSignal().Run(ctx, syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
				return []Row{syntheticEgressOutcomeRow(snapshot)}, nil
			}}))
			if err != nil {
				t.Fatal(err)
			}
			wantHolds, wantAlerts, wantClass := 4, 0, ""
			switch mode {
			case "inconsistent":
				wantHolds, wantAlerts, wantClass = 3, 1, "egress-outcome-inconsistent"
			case "partial-positive":
				wantHolds, wantAlerts, wantClass = 0, 1, "egress-common-mode"
			case "healthy":
				wantHolds = 0
			}
			holds := observation.snapshot(false)
			if len(holds) != wantHolds || len(alerts) != wantAlerts {
				t.Fatalf("holds=%d alerts=%d; want holds=%d alerts=%d", len(holds), len(alerts), wantHolds, wantAlerts)
			}
			seen := map[string]bool{}
			for _, hold := range holds {
				if hold.SignalId != "pg/egress-outcomes" || hold.Target != "pg-1" || seen[hold.Class] || hold.Class == wantClass {
					t.Fatal("silent hold changed incident scope or suppressed a positive")
				}
				seen[hold.Class] = true
			}
			if wantAlerts == 1 && (alerts[0].Class != wantClass || alerts[0].unobservable) {
				t.Fatal("recovery hold changed an independently measured positive")
			}
		})
	}
}

// Invalid numeric cells remain private; cancellation supplies neither alert nor hold.
func TestEgressOutcomesRecoveryPrivacyAndCancellation(t *testing.T) {
	const privateMarker = "synthetic-egress-recovery-private-cell"
	for _, mode := range []string{"malformed", "canceled-success"} {
		ctx, cancel := context.WithCancel(context.Background())
		observation := &signalClearHoldObservation{}
		ctx = context.WithValue(ctx, signalClearHoldContextKey{}, observation)
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			row := syntheticEgressOutcomeRow(egressOutcomeSnapshot{eligible: 100, unobserved: 100, newestOutcomeAgeSeconds: -1, oldestOutcomeAgeSeconds: -1})
			if mode == "malformed" {
				row[0] = privateMarker
			} else {
				cancel()
			}
			return []Row{row}, nil
		}}
		alerts, err := NewEgressOutcomesSignal().Run(ctx, syntheticSettings(source))
		cancel()
		if err == nil || len(alerts) != 0 || len(observation.snapshot(err != nil)) != 0 {
			t.Fatal("failed observation supplied alert or recovery hold")
		}
		if mode == "canceled-success" && !errors.Is(err, context.Canceled) {
			t.Fatal("ended parent lost its lifecycle cause")
		}
		encoded, encodeErr := json.Marshal(alerts)
		if encodeErr != nil || strings.Contains(string(encoded)+err.Error(), privateMarker) {
			t.Fatal("private malformed source entered public JSON or error")
		}
	}
}
