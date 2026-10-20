package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// Removes only the business timestamp or replaces it with the source owner's
// invalid zero. Every Mimir evaluation timestamp and count stays complete.
func modernGateReadinessTestMissingClockPayload(t testing.TB, payload string, shape string) string {
	t.Helper()
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(payload), &response); err != nil {
		t.Fatal(err)
	}
	for index := range response.Data.Result {
		series := &response.Data.Result[index]
		if series.Metric["monitor_modern_gate_part"] != "counts_started" {
			continue
		}
		switch shape {
		case "missing":
			response.Data.Result = append(response.Data.Result[:index], response.Data.Result[index+1:]...)
		case "zero":
			series.Value[1] = json.RawMessage(`"0"`)
		default:
			t.Fatalf("unknown synthetic clock shape %q", shape)
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	t.Fatal("synthetic payload has no source clock to remove")
	return ""
}

// The reducer may not emit its healthy sentinel for either representation of
// missing source authority before or after the flip.
func TestModernGateReadinessMissingClockCannotEstablishHealth(t *testing.T) {
	for _, now := range []time.Time{
		modernGateReadinessTestStart.Add(-time.Hour),
		modernGateReadinessTestStart.Add(10 * time.Minute),
	} {
		for _, started := range []time.Time{{}, time.Unix(0, 0)} {
			observation := modernGateReadinessTestObservation(now, 90, 10, 90, 10)
			observation.countsStarted = started
			observation.bucketsBefore = map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}
			findings, err := evaluateModernGateReadiness(observation, now)
			if err == nil || len(findings) != 0 {
				t.Fatalf("at %s: missing source clock established health: findings %+v err %v", now, findings, err)
			}
		}
	}
}

// Fresh Mimir evaluation time cannot replace the absent or zero business
// clock, regardless of whether the numerical counts look healthy or fallen.
func TestModernGateReadinessSignalMissingClockIsUnknown(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(10 * time.Minute)
	before := start.Add(-time.Second)
	for _, shape := range []string{"missing", "zero"} {
		for _, quality := range []float64{90, 30} {
			current := modernGateReadinessTestPayload(t, now, modernGateReadinessTestProcesses(4, now, true, &start), now, modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: quality, model.RankModeSpeed: 90})
			payloads := map[int64]string{
				now.Unix():    modernGateReadinessTestMissingClockPayload(t, current, shape),
				before.Unix(): modernGateReadinessTestPayload(t, before, nil, before, nil, map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}),
			}
			alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, payloads))
			if err == nil || !strings.Contains(err.Error(), "no valid source start") || len(alerts) != 0 {
				t.Fatalf("%s quality=%g: missing business timestamp became a complete run: alerts %+v err %v", shape, quality, alerts, err)
			}
		}
	}
}

// The fleet has its own complete evidence even when the source clock and the
// independent historical read are unavailable. Neither loss hides its page.
func TestModernGateReadinessSignalMissingClockPreservesFleetPage(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(10 * time.Minute)
	before := start.Add(-time.Second)
	processes := modernGateReadinessTestProcesses(4, now, true, &start)
	processes[3].on, processes[3].start = nil, nil
	for _, shape := range []string{"missing", "zero"} {
		for _, historyAvailable := range []bool{false, true} {
			current := modernGateReadinessTestPayload(t, now, processes, now, modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: 30, model.RankModeSpeed: 90})
			payloads := map[int64]string{now.Unix(): modernGateReadinessTestMissingClockPayload(t, current, shape)}
			if historyAvailable {
				payloads[before.Unix()] = modernGateReadinessTestPayload(t, before, nil, before, nil, map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100})
			}
			alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, payloads))
			if err != nil || len(alerts) != 2 {
				t.Fatalf("%s history=%t: missing source clock discarded independent fleet evidence: alerts %+v err %v", shape, historyAvailable, alerts, err)
			}
			fleet := requireAlertClass(t, alerts, "modern-gate-fleet-disagrees")
			if fleet.Identity() != modernGateReadinessProbeId+"|modern-gate-fleet-disagrees|"+modernGateReadinessTarget+"|" || fleet.Severity != SeverityPage {
				t.Fatalf("%s history=%t: independent fleet page changed: %+v", shape, historyAvailable, fleet)
			}
			visibility := requireAlertClass(t, alerts, "cannot-observe")
			if visibility.Severity != SeverityWarn || !isVisibilityAlert(visibility) {
				t.Fatalf("%s history=%t: missing source clock lost unknown status: %+v", shape, historyAvailable, visibility)
			}
		}
	}
}

// Restoring a valid clock permits the unchanged current-count verdict. A
// missing sibling baseline still preserves the complete bucket's finding.
func TestModernGateReadinessSignalRestoredClockRequalifiesCounts(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(10 * time.Minute)
	before := start.Add(-time.Second)
	for _, quality := range []float64{90, 30} {
		for _, completeHistory := range []bool{false, true} {
			current := modernGateReadinessTestPayload(t, now, modernGateReadinessTestProcesses(4, now, true, &start), now, modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: quality, model.RankModeSpeed: 90})
			history := map[string]float64{model.RankModeQuality: 100}
			if completeHistory {
				history[model.RankModeSpeed] = 100
			}
			payloads := map[int64]string{
				now.Unix():    modernGateReadinessTestMissingClockPayload(t, current, "missing"),
				before.Unix(): modernGateReadinessTestPayload(t, before, nil, before, nil, history),
			}
			settings := modernGateReadinessTestSettings(t, now, payloads)
			if alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), settings); err == nil || len(alerts) != 0 {
				t.Fatalf("quality=%g history=%t: absent source clock was judged before restoration", quality, completeHistory)
			}
			payloads[now.Unix()] = current
			alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), settings)
			if quality == 90 {
				if len(alerts) != 0 || (err == nil) != completeHistory {
					t.Errorf("healthy quality history=%t: restored clock invented complete coverage: alerts %+v err %v", completeHistory, alerts, err)
				}
				continue
			}
			wantCount := 1
			if !completeHistory {
				wantCount++
			}
			if err != nil || len(alerts) != wantCount {
				t.Fatalf("fallen quality history=%t: restored clock lost the qualified bucket: alerts %+v err %v", completeHistory, alerts, err)
			}
			qualityAlert := requireAlertClass(t, alerts, "modern-gate-flip-drop")
			if qualityAlert.Severity != SeverityPage || qualityAlert.Frame != model.RankModeQuality {
				t.Errorf("restored clock changed the quality page: %+v", qualityAlert)
			}
			if !completeHistory && !isVisibilityAlert(requireAlertClass(t, alerts, "cannot-observe")) {
				t.Error("partial restored history lost its visibility finding")
			}
		}
	}
}

// The actual signal, cadence gate and notifier retain the bucket incident
// across absent and zero source clocks, including a partial fleet result.
// Only the two later complete healthy runs may clear it.
func TestModernGateReadinessNotifierMissingClockPreservesIncident(t *testing.T) {
	start := modernGateReadinessTestStart
	now := start.Add(10 * time.Minute)
	before := start.Add(-time.Second)
	for _, independentFleet := range []bool{false, true} {
		processes := modernGateReadinessTestProcesses(4, now, true, &start)
		current := func(quality float64) string {
			return modernGateReadinessTestPayload(t, now, processes, now, modernGateReadinessTestGuttedShadow(), map[string]float64{model.RankModeQuality: quality, model.RankModeSpeed: 90})
		}
		fallen, healthy := current(30), current(90)
		if independentFleet {
			processes[3].on, processes[3].start = nil, nil
		}
		missing := modernGateReadinessTestMissingClockPayload(t, current(90), "missing")
		zero := modernGateReadinessTestMissingClockPayload(t, current(90), "zero")
		runs := []string{fallen, missing, zero, healthy, healthy}
		payloads := map[int64]string{
			now.Unix():    fallen,
			before.Unix(): modernGateReadinessTestPayload(t, before, nil, before, nil, map[string]float64{model.RankModeQuality: 100, model.RankModeSpeed: 100}),
		}
		webhook, webhookUrl := newFakeSlackWebhook(t)
		harness := newSlackTestHarness()
		notifierSettings := harness.settings(webhookUrl)
		notifierSettings.ClearAfterCompleteRuns = 2
		notifier := NewSlackAlertNotifier(notifierSettings)
		defer closeSlackNotifier(t, notifier)
		incidentKey := modernGateReadinessProbeId + "|modern-gate-flip-drop|" + modernGateReadinessTarget
		postSignalRunsThroughTheGate(t, modernGateReadinessTestSignal(""), modernGateReadinessTestSettings(t, now, payloads), len(runs), func(index int) {
			if index == 3 || index == 4 {
				retained := func() bool {
					notifier.stateLock.Lock()
					defer notifier.stateLock.Unlock()
					_, ok := notifier.keyIncidents[incidentKey]
					return ok
				}()
				if !retained {
					t.Fatalf("independent fleet=%t run=%d: missing source clock cleared the bucket incident", independentFleet, index)
				}
			}
			payloads[now.Unix()] = runs[index]
		}, notifier)
		closeSlackNotifier(t, notifier)
		texts := webhook.posted()
		identity := modernGateReadinessProbeId + " · modern-gate-flip-drop · " + modernGateReadinessTarget
		if countSlackPosts(texts, "[PAGE] raised", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != 1 {
			t.Fatalf("independent fleet=%t: notifier did not retain then clear the bucket exactly once: %s", independentFleet, strings.Join(texts, "\n"))
		}
		if independentFleet {
			fleetIdentity := modernGateReadinessProbeId + " · modern-gate-fleet-disagrees · " + modernGateReadinessTarget
			if countSlackPosts(texts, "[PAGE] raised", fleetIdentity) != 1 || countSlackPosts(texts, "[CLEARED]", fleetIdentity) != 1 {
				t.Fatalf("independent fleet finding was not retained and later cleared: %s", strings.Join(texts, "\n"))
			}
		}
	}
}
