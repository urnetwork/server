// Backend annotations leave aggregate response denominators unqualified.
package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Add annotations without changing any returned cohort or evaluation clock.
func providerCountAnnotatedFixture(t testing.TB, payload string, annotations map[string]any) string {
	t.Helper()
	var document map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &document) != nil {
		t.Fatal("invalid provider-count annotation fixture")
	}
	for name, value := range annotations {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal("invalid provider-count annotation value")
		}
		document[name] = encoded
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal("invalid provider-count annotation document")
	}
	return string(encoded)
}

// A missing failed cohort cannot be reconstructed from a surviving quiet one.
// The public Monitor must emit visibility, rather than a healthy Alert or a
// provider verdict, for either annotation channel, regardless of its text.
func TestProviderCountAnnotationsRefusePartialCohortAuthority(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	surviving := providerCountFixture(t, now, map[string]float64{"3-9": 30}, "false")
	complete := providerCountJoinedControlFixtures(t, surviving,
		providerCountDirectControlFixture(t, now, "group", map[string]float64{"0": 20}))
	alerts, err := NewWithSignals(providerCountSyntheticSettings(t, now, complete), NewProviderCountSignal()).Run(context.Background())
	if err != nil || len(alerts) != 1 || requireAlertClass(t, alerts, "provider-count-effective-empty").Severity != SeverityPage {
		t.Fatal("unannotated independent group failure lost its PAGE")
	}
	for _, annotations := range []map[string]any{
		{"warnings": []string{"synthetic-private-aggregate-warning"}},
		{"infos": []string{"synthetic-private-aggregate-info"}},
		{"warnings": []string{""}},
		{"infos": []string{""}},
		{"warnings": []string{"synthetic-private-aggregate-warning"}, "infos": []string{"synthetic-private-aggregate-info"}},
	} {
		signal := NewProviderCountSignal()
		settings := providerCountSyntheticSettings(t, now, providerCountAnnotatedFixture(t, surviving, annotations))
		alerts, err := NewWithSignals(settings, signal).Run(context.Background())
		if err == nil {
			t.Fatal("annotated surviving cohort supplied complete public execution authority")
		}
		requireAggregateVisibility(t, signal, alerts, observationErrorClassInvalidResponse)
		if strings.Contains(err.Error(), "synthetic-private-aggregate") || !strings.Contains(err.Error(), "invalid response annotations") {
			t.Fatal("annotation error lost its fixed private-free diagnostic")
		}
	}
}

// The annotation belongs to the whole result. Even an apparent positive ratio
// lacks an authoritative denominator; a separate clean response retains it.
func TestProviderCountAnnotationsCannotCertifyPositiveRatios(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, bands := range []map[string]float64{
		{"0": 16, "3-9": 4},
		{"1-2": 25, "3-9": 25},
		{"1-2": 10, "3-9": 10},
	} {
		payload := providerCountFixture(t, now, bands, "false")
		clean, err := NewProviderCountSignal().Run(context.Background(), providerCountSyntheticSettings(t, now, payload))
		if err != nil || len(clean) != 1 || isVisibilityAlert(clean[0]) {
			t.Fatal("clean independent threshold finding was suppressed")
		}
		for _, name := range []string{"warnings", "infos"} {
			signal := NewProviderCountSignal()
			annotated := providerCountAnnotatedFixture(t, payload, map[string]any{name: []string{"synthetic-private-aggregate"}})
			alerts, err := NewWithSignals(providerCountSyntheticSettings(t, now, annotated), signal).Run(context.Background())
			if err == nil {
				t.Fatal("annotated ratio established a provider verdict")
			}
			requireAggregateVisibility(t, signal, alerts, observationErrorClassInvalidResponse)
		}
	}
}

// Absent and empty annotation channels preserve deliberate quiet behavior.
// Public Run filters internal healthy findings, so quiet is an empty Alert set.
func TestProviderCountAnnotationsPreserveQuietSparseAndIntentControls(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, payload := range []string{
		providerCountFixture(t, now, map[string]float64{"3-9": 30}, "false"),
		providerCountFixture(t, now, map[string]float64{"0": 0}, "false"),
		providerCountFixture(t, now, map[string]float64{"1-2": 19.999}, "false"),
		providerCountDirectControlFixture(t, now, "direct", map[string]float64{"1-2": 30}),
		providerCountFixture(t, now, map[string]float64{"0": 100}, "true"),
	} {
		for _, annotations := range []map[string]any{
			{},
			{"warnings": nil, "infos": nil},
			{"warnings": []string{}, "infos": []string{}},
		} {
			settings := providerCountSyntheticSettings(t, now, providerCountAnnotatedFixture(t, payload, annotations))
			alerts, err := NewWithSignals(settings, NewProviderCountSignal()).Run(context.Background())
			if err != nil || len(alerts) != 0 {
				t.Fatal("empty annotations changed quiet, sparse, direct or ForceMinimum behavior")
			}
		}
	}
}

// A failed annotated execution must not supply either of the two complete
// recovery observations. Drive the actual signal, run loop and notifier state
// with cadence barriers; no scripted replacement signal or sleep proves this.
func TestProviderCountAnnotationsDoNotAdvanceIncidentRecovery(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	page := providerCountFixture(t, now, map[string]float64{"0": 20}, "false")
	quiet := providerCountFixture(t, now, map[string]float64{"3-9": 30}, "false")
	warning := providerCountAnnotatedFixture(t, quiet, map[string]any{"warnings": []string{"synthetic-private-aggregate-warning"}})
	info := providerCountAnnotatedFixture(t, quiet, map[string]any{"infos": []string{"synthetic-private-aggregate-info"}})
	payloads := []string{page, warning, info, quiet, warning, quiet, quiet}
	var run atomic.Int32
	settings := providerCountSyntheticSettings(t, now, quiet)
	settings.Source = &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		return payloads[int(run.Load())], nil
	}}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	signal := NewProviderCountSignal()
	key := slackIncidentKey(Alert{SignalID: signal.ID(), Class: "provider-count-effective-empty", Target: "api-fleet", Frame: "any/location/us/quality"})
	postSignalRunsThroughTheGate(t, signal, settings, len(payloads), func(next int) {
		if next > 0 {
			wantPresent, wantAbsent := next < 6, 0
			if next == 4 || next == 5 {
				wantAbsent = 1
			}
			notifier.stateLock.Lock()
			incident, present := notifier.keyIncidents[key]
			if present != wantPresent {
				notifier.stateLock.Unlock()
				t.Fatal("annotated execution cleared an incident before two complete observations")
			}
			if present {
				absent, owned := incident.signalKeyAbsentRuns[signal.Key()]
				if !owned || absent != wantAbsent {
					notifier.stateLock.Unlock()
					t.Fatal("annotated execution changed complete recovery credit")
				}
			}
			notifier.stateLock.Unlock()
			harness.advance(time.Minute)
		}
		run.Store(int32(next))
	}, notifier)
	closeSlackNotifier(t, notifier)
	posts := webhook.posted()
	identity := "provider-count-effective-empty · api-fleet"
	if countSlackPosts(posts, "[PAGE] raised", identity) != 1 || countSlackPosts(posts, "[CLEARED]", identity) != 1 {
		t.Fatal("annotation gap changed PAGE or complete-recovery notification count")
	}
	for _, post := range posts {
		if strings.Contains(post, "synthetic-private-aggregate") {
			t.Fatal("notifier exposed a private annotation")
		}
	}
}
