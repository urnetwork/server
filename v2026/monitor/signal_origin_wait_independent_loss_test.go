// Notification-family authority is independent of the wait denominator.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Keep the real synthetic query checks and mutate only the returned vector.
func originWaitChangedSettings(t *testing.T, now time.Time, fixture originWaitFixture, change func(*mimirInstantResponse)) SignalSettings {
	t.Helper()
	settings := originWaitSyntheticSettings(t, now, fixture)
	original := settings.Source
	settings.Source = &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		output, err := original.Host(context.Background(), host, command)
		if err != nil {
			return "", err
		}
		var response mimirInstantResponse
		if json.Unmarshal([]byte(output), &response) != nil {
			t.Fatal("invalid origin-wait fixture")
		}
		change(&response)
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal("invalid changed origin-wait fixture")
		}
		return string(encoded), nil
	}}
	return settings
}

// Removing a row never manufactures a measured zero for its family.
func originWaitDropMetric(response *mimirInstantResponse, kind, label string) {
	rows := response.Data.Result[:0]
	for _, row := range response.Data.Result {
		if row.Metric["monitor_metric"] == kind && (kind == "request" || row.Metric["source"] == label || row.Metric["event"] == label) {
			continue
		}
		rows = append(rows, row)
	}
	response.Data.Result = rows
}

// The loss family is complete, although each unrelated family can be missing.
// Neither database amplification nor any healthy sentinel gets partial credit.
func TestOriginWaitIndependentLossSurvivesMissingWaitFamily(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	for _, missing := range []struct{ kind, label string }{
		{kind: "lookup", label: "initial"},
		{kind: "wake", label: "event"},
		{kind: "request"},
	} {
		settings := originWaitChangedSettings(t, now, originWaitFixture{
			lookup: map[string]float64{"initial": 100, "fallback": 350, "deadline": 50}, requests: 100,
			notification: map[string]float64{"queue_full": 0.02},
		}, func(response *mimirInstantResponse) { originWaitDropMetric(response, missing.kind, missing.label) })
		alerts, err := NewOriginWaitSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 2 {
			t.Fatal("missing wait family erased independent complete notification loss")
		}
		unknown := requireAlertClass(t, alerts, "origin-wait-unobservable")
		loss := requireAlertClass(t, alerts, "origin-notification-loss")
		if !isVisibilityAlert(unknown) || isVisibilityAlert(loss) || loss.Severity != SeverityWarn || loss.Sustain != 2 ||
			requireAlertClassCount(alerts, "origin-wait-db-amplification") != 0 ||
			!strings.Contains(loss.Observed, "notification_family=complete other_metric_families=incomplete") {
			t.Fatal("partial response changed independent-loss identity or supplied unrelated authority")
		}
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		findings, err := (originWaitProbe{}).check(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range findings {
			if finding.healthy {
				t.Fatal("partial response supplied an internal healthy sentinel")
			}
		}
	}
}

// Missing or invalid notification data, including duplicate returned cells,
// cannot borrow a complete sibling or become observed loss/healthy delivery.
func TestOriginWaitIndependentLossRejectsUnqualifiedNotificationFamily(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	for _, mode := range []string{"missing", "stale", "duplicate", "negative", "nonfinite", "extra-label"} {
		settings := originWaitChangedSettings(t, now, originWaitFixture{
			notification: map[string]float64{"queue_full": 1},
		}, func(response *mimirInstantResponse) {
			originWaitDropMetric(response, "wake", "event")
			if mode == "missing" {
				originWaitDropMetric(response, "notification", "published")
				return
			}
			for index, row := range response.Data.Result {
				if row.Metric["monitor_metric"] != "notification" || row.Metric["event"] != "queue_full" {
					continue
				}
				switch mode {
				case "stale":
					response.Data.Result[index].Value[0] = json.RawMessage(strconv.FormatInt(now.Add(-2*time.Minute).Unix(), 10))
				case "duplicate":
					response.Data.Result = append(response.Data.Result, row)
				case "negative":
					response.Data.Result[index].Value[1] = json.RawMessage(`"-1"`)
				case "nonfinite":
					response.Data.Result[index].Value[1] = json.RawMessage(`"NaN"`)
				case "extra-label":
					response.Data.Result[index].Metric["private_id"] = "synthetic-private-notification"
				}
				return
			}
		})
		alerts, err := NewOriginWaitSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || alerts[0].Class != "origin-wait-unobservable" || !isVisibilityAlert(alerts[0]) {
			t.Fatal("unqualified notification family supplied product or healthy authority")
		}
		encoded, err := json.Marshal(alerts)
		if err != nil || strings.Contains(string(encoded), "synthetic-private-notification") {
			t.Fatal("private notification label entered public JSON")
		}
		requireAlertOmits(t, alerts[0], "synthetic-private-notification")
	}
}

// Keep the existing inclusive one/minute threshold. An incomplete quiet frame
// stays visibility-only, while a complete quiet frame remains quiet publicly.
func TestOriginWaitIndependentLossKeepsThresholdAndQuietAuthority(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	for _, partial := range []bool{false, true} {
		for _, rate := range []float64{0, 0.01, 1.0 / 60} {
			settings := originWaitChangedSettings(t, now, originWaitFixture{}, func(response *mimirInstantResponse) {
				if partial {
					originWaitDropMetric(response, "wake", "event")
				}
				for index, row := range response.Data.Result {
					if row.Metric["monitor_metric"] == "notification" && row.Metric["event"] == "queue_full" {
						response.Data.Result[index].Value[1] = json.RawMessage(strconv.Quote(strconv.FormatFloat(rate, 'g', -1, 64)))
					}
				}
			})
			alerts, err := NewOriginWaitSignal().Run(context.Background(), settings)
			wantLoss := rate == 1.0/60
			wantCount := 0
			if partial {
				wantCount++
			}
			if wantLoss {
				wantCount++
			}
			if err != nil || len(alerts) != wantCount || (requireAlertClassCount(alerts, "origin-notification-loss") == 1) != wantLoss {
				t.Fatal("notification equality/quiet or partial visibility policy changed")
			}
		}
	}
}

// A source that observes its ended parent returns lifecycle error, not partial
// data. Generic after-success parent policy remains owned by the shared runner.
func TestOriginWaitIndependentLossDoesNotRetainCanceledSourceData(t *testing.T) {
	settings := syntheticSettings(nil)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		cancel()
		return "", parent.Err()
	}}
	alerts, err := NewOriginWaitSignal().Run(parent, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatal("ended source supplied notification data")
	}
}

// Exercise real sustain and recovery gates: partial loss is retained; partial
// quiet cannot clear it, and complete quiet observations alone earn recovery.
func TestOriginWaitIndependentLossRetainsIncidentThroughPartialQuiet(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	positive := originWaitFixture{notification: map[string]float64{"queue_full": 0.02}}
	completeLoss := originWaitSyntheticSettings(t, now, positive)
	partialLoss := originWaitChangedSettings(t, now, positive, func(response *mimirInstantResponse) { originWaitDropMetric(response, "wake", "event") })
	quiet := originWaitSyntheticSettings(t, now, originWaitFixture{})
	partialQuiet := originWaitChangedSettings(t, now, originWaitFixture{}, func(response *mimirInstantResponse) { originWaitDropMetric(response, "wake", "event") })
	sources := []SignalSource{completeLoss.Source, completeLoss.Source, partialLoss.Source, partialLoss.Source, partialQuiet.Source, partialQuiet.Source, quiet.Source, partialQuiet.Source, quiet.Source, quiet.Source}
	var run atomic.Int32
	settings := completeLoss
	settings.Source = &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		return sources[int(run.Load())].Host(context.Background(), host, command)
	}}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	signal := NewOriginWaitSignal()
	key := slackIncidentKey(Alert{SignalID: signal.ID(), Class: "origin-notification-loss", Target: "synthetic/api-fleet"})
	postSignalRunsThroughTheGate(t, signal, settings, len(sources), func(next int) {
		if next >= 2 {
			notifier.stateLock.Lock()
			incident, present := notifier.keyIncidents[key]
			if !present {
				notifier.stateLock.Unlock()
				t.Fatal("partial notification authority cleared or re-raised incident")
			}
			if present {
				wantAbsent := 0
				if next == 7 || next == 9 {
					wantAbsent = 1
				}
				if incident.signalKeyAbsentRuns[signal.Key()] != wantAbsent {
					notifier.stateLock.Unlock()
					t.Fatal("partial quiet advanced notification recovery")
				}
			}
			notifier.stateLock.Unlock()
		}
		if next > 0 {
			harness.advance(time.Minute)
		}
		run.Store(int32(next))
	}, notifier)
	notifier.stateLock.Lock()
	_, present := notifier.keyIncidents[key]
	notifier.stateLock.Unlock()
	if present {
		t.Fatal("two complete observations after partial quiet did not clear incident")
	}
	closeSlackNotifier(t, notifier)
	posts := webhook.posted()
	identity := "origin-notification-loss · synthetic/api-fleet"
	if countSlackPosts(posts, "[WARN] raised", identity) != 1 || countSlackPosts(posts, "[CLEARED]", identity) != 1 {
		t.Fatal("independent loss or complete recovery changed notification count")
	}
}
