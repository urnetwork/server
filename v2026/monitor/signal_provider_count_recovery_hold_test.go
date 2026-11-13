// Unknown successful response populations cannot supply measured recovery credit.
package monitor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exact current response kinds; all values are synthetic and use existing public fixtures.
func providerCountRecoveryPayload(t testing.TB, now time.Time, mode string) string {
	t.Helper()
	switch mode {
	case "page":
		return providerCountFixture(t, now, map[string]float64{"0": 20}, "false")
	case "warn":
		return providerCountFixture(t, now, map[string]float64{"0": 25, "1-2": 10, "3-9": 15}, "false")
	case "unknown":
		return providerCountFixture(t, now, map[string]float64{"0": 19, "1-2": 11}, "false")
	case "sparse":
		return providerCountFixture(t, now, map[string]float64{"1-2": 19.999}, "false")
	case "idle":
		return providerCountFixture(t, now, map[string]float64{"0": 0}, "false")
	case "direct":
		return providerCountDirectControlFixture(t, now, "direct", map[string]float64{"1-2": 5})
	case "diagnostic":
		return providerCountFixture(t, now, map[string]float64{"0": 100}, "true")
	case "healthy":
		return providerCountFixture(t, now, map[string]float64{"3-9": 100}, "false")
	case "healthy+sparse":
		return providerCountJoinedControlFixtures(t,
			providerCountRecoveryPayload(t, now, "healthy"),
			providerCountDirectControlFixture(t, now, "group", map[string]float64{"1-2": 19.999}))
	case "healthy+unknown":
		return providerCountJoinedControlFixtures(t,
			providerCountRecoveryPayload(t, now, "healthy"),
			providerCountDirectControlFixture(t, now, "group", map[string]float64{"0": 19, "1-2": 11}))
	case "healthy+direct":
		return providerCountJoinedControlFixtures(t, providerCountRecoveryPayload(t, now, "healthy"), providerCountRecoveryPayload(t, now, "direct"))
	case "malformed":
		return "private-provider-recovery-canary"
	default:
		t.Fatal("unknown synthetic provider recovery mode")
		return ""
	}
}

// Public Run, the real cadence gate and notifier retain measured incidents through
// repeated unknown runs, reset interrupted recovery and clear after two healthy runs.
// Each unknown mode is the incident's own cohort without evidence (unknown,
// sparse, idle), its absence (direct, diagnostic) or a failed source; a sparse
// or unknown sibling beside a qualified own cohort is recovery
// (signal_provider_count_cohort_recovery_test.go).
func TestProviderCountUnknownResponsesRetainMeasuredRecovery(t *testing.T) {
	for _, class := range []string{"provider-count-effective-empty", "provider-count-small-list"} {
		for _, unknown := range []string{"unknown", "sparse", "idle", "direct", "diagnostic", "malformed", "unavailable"} {
			t.Run(class+"/"+unknown, func(t *testing.T) {
				now := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
				initial := "page"
				if class == "provider-count-small-list" {
					initial = "warn"
				}
				modes := []string{initial, unknown, unknown, "healthy", unknown, "healthy", "healthy"}
				var run atomic.Int32
				settings := providerCountSyntheticSettings(t, now, providerCountRecoveryPayload(t, now, "healthy"))
				settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
					mode := modes[int(run.Load())]
					if mode == "unavailable" {
						return "private-provider-recovery-canary", errors.New("private-provider-recovery-canary")
					}
					return providerCountRecoveryPayload(t, now, mode), nil
				}}
				webhook, url := newFakeSlackWebhook(t)
				harness := newSlackTestHarness()
				notifier := NewSlackAlertNotifier(harness.settings(url))
				t.Cleanup(func() { closeSlackNotifier(t, notifier) })
				signal := NewProviderCountSignal()
				key := slackIncidentKey(Alert{SignalID: signal.ID(), Class: class, Target: "api-fleet"})
				postSignalRunsThroughTheGate(t, signal, settings, len(modes), func(next int) {
					if next > 0 {
						wantCredit := 0
						if next == 4 || next == 6 {
							wantCredit = 1
						}
						notifier.stateLock.Lock()
						incident, present := notifier.keyIncidents[key]
						credit, owned := -1, false
						if present {
							credit, owned = incident.signalKeyAbsentRuns[signal.Key()]
						}
						notifier.stateLock.Unlock()
						// Keep running after OLD credit advancement so false clearing
						// is independently observed, not hidden by an early Fatal.
						if !present || !owned || credit != wantCredit {
							t.Error("provider-count recovery: unknown response supplied measured absence or clear credit")
						}
						harness.advance(time.Minute)
					}
					run.Store(int32(next))
				}, notifier)
				closeSlackNotifier(t, notifier)
				notifier.stateLock.Lock()
				_, present := notifier.keyIncidents[key]
				notifier.stateLock.Unlock()
				if present || countSlackPosts(webhook.posted(), "[CLEARED]", class+" · api-fleet") != 1 {
					t.Fatal("provider-count recovery: two qualified healthy runs did not clear exactly once")
				}
			})
		}
	}
}

// Successful unknown findings hold only measured provider-count identities and
// stay silent where they were silent; a sibling PAGE is still emitted. On a
// first execution only the response without any judgeable ordinary cohort
// holds: an unknown cohort that never reported a class holds nothing beside a
// sibling with the requests to judge.
func TestProviderCountUnknownHoldsAreScopedAndKeepPositiveSibling(t *testing.T) {
	now := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
	for _, mode := range []string{"unknown", "sparse", "idle", "direct", "diagnostic", "healthy", "healthy+direct", "positive-sibling"} {
		t.Run(mode, func(t *testing.T) {
			payload := ""
			if mode == "positive-sibling" {
				payload = providerCountJoinedControlFixtures(t, providerCountRecoveryPayload(t, now, "unknown"),
					providerCountDirectControlFixture(t, now, "group", map[string]float64{"0": 20}))
			} else {
				payload = providerCountRecoveryPayload(t, now, mode)
			}
			observation := &signalClearHoldObservation{}
			ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, observation)
			alerts, err := NewProviderCountSignal().Run(ctx, providerCountSyntheticSettings(t, now, payload))
			if err != nil {
				t.Fatal("provider-count hold control failed its public source contract")
			}
			holds := observation.snapshot(false)
			if mode == "healthy" || mode == "healthy+direct" {
				if len(holds) != 0 || len(alerts) != 0 {
					t.Fatal("qualified independent ordinary recovery acquired a hold")
				}
				return
			}
			if mode == "positive-sibling" {
				if len(holds) != 0 {
					t.Error("an unknown cohort that reported nothing held beside a judgeable sibling")
				}
			} else if len(holds) != 2 {
				t.Error("provider-count recovery: unknown response lost exact measured-class holds")
			} else {
				for index, class := range []string{"provider-count-effective-empty", "provider-count-small-list"} {
					if holds[index] != (SignalClassClearHold{SignalId: "mimir/provider-count", Class: class, Target: "api-fleet"}) {
						t.Error("provider-count recovery: hold escaped its probe class or target")
					}
				}
			}
			wantAlerts := 0
			if mode == "unknown" {
				wantAlerts = 1
			}
			if mode == "positive-sibling" {
				wantAlerts = 2
			}
			if len(alerts) != wantAlerts {
				t.Fatal("silent recovery hold created or suppressed a public finding")
			}
			if mode == "positive-sibling" {
				page := requireAlertClass(t, alerts, "provider-count-effective-empty")
				if page.Severity != SeverityPage {
					t.Fatal("unknown sibling suppressed measured PAGE")
				}
			}
		})
	}
}

// An ordinary >=50 provisional WARN is measured threshold movement, unlike
// unknown intent. It may clear the distinct effective-empty PAGE after two runs.
func TestProviderCountMeasuredWarnCanRecoverDistinctPage(t *testing.T) {
	now := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
	var run atomic.Int32
	modes := []string{"page", "warn", "warn"}
	settings := providerCountSyntheticSettings(t, now, providerCountRecoveryPayload(t, now, "healthy"))
	settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return providerCountRecoveryPayload(t, now, modes[int(run.Load())]), nil
	}}
	_, url := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(url))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	signal := NewProviderCountSignal()
	postSignalRunsThroughTheGate(t, signal, settings, len(modes), func(next int) {
		if next > 0 {
			harness.advance(time.Minute)
		}
		run.Store(int32(next))
	}, notifier)
	closeSlackNotifier(t, notifier)
	notifier.stateLock.Lock()
	_, pagePresent := notifier.keyIncidents[slackIncidentKey(Alert{SignalID: signal.ID(), Class: "provider-count-effective-empty", Target: "api-fleet"})]
	_, warnPresent := notifier.keyIncidents[slackIncidentKey(Alert{SignalID: signal.ID(), Class: "provider-count-small-list", Target: "api-fleet"})]
	notifier.stateLock.Unlock()
	if pagePresent || !warnPresent {
		t.Fatal("independent measured WARN no longer recovers the distinct PAGE")
	}
}

// Malformed, failed and retired source callbacks keep the existing no-recovery
// contract and cannot publish silent holds or source payload text after retirement.
func TestProviderCountUnknownRecoveryKeepsFailureAndCancellationFences(t *testing.T) {
	now := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
	for _, mode := range []string{"malformed", "unavailable", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observation := &signalClearHoldObservation{}
			ctx = context.WithValue(ctx, signalClearHoldContextKey{}, observation)
			settings := providerCountSyntheticSettings(t, now, providerCountRecoveryPayload(t, now, "healthy"))
			settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				if mode == "canceled" {
					cancel()
				}
				if mode == "malformed" {
					return "private-provider-recovery-canary", nil
				}
				return "private-provider-recovery-canary", errors.New("private-provider-recovery-canary")
			}}
			alerts, err := NewProviderCountSignal().Run(ctx, settings)
			if err == nil || len(alerts) != 0 || len(observation.snapshot(true)) != 0 {
				t.Fatal("failed or retired provider source supplied recovery authority")
			}
			if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("parent cancellation lost precedence")
			}
			if mode == "malformed" && strings.Contains(err.Error(), "private-provider-recovery-canary") {
				t.Fatal("malformed provider source exposed private content")
			}
		})
	}
}
