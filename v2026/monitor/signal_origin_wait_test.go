package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type originWaitFixture struct {
	lookup, wake, notification map[string]float64
	requests                   float64
	omitNotification           string
	extraLabel                 string
}

func originWaitSyntheticSettings(t *testing.T, now time.Time, fixture originWaitFixture) SignalSettings {
	t.Helper()
	result := []map[string]any{}
	add := func(kind, labelName, label string, rate float64) {
		metric := map[string]string{"monitor_metric": kind}
		if labelName != "" {
			metric[labelName] = label
		}
		if fixture.extraLabel != "" && label == fixture.extraLabel {
			metric["private_id"] = "synthetic-private"
		}
		result = append(result, map[string]any{
			"metric": metric,
			"value":  []any{float64(now.Unix()), fmt.Sprintf("%.6f", rate)},
		})
	}
	for _, source := range originWaitSources {
		add("lookup", "source", source, fixture.lookup[source])
	}
	for _, source := range originWakeSources {
		add("wake", "source", source, fixture.wake[source])
	}
	for _, event := range originNotificationEvents {
		if event != fixture.omitNotification {
			add("notification", "event", event, fixture.notification[event])
		}
	}
	add("request", "", "", fixture.requests)
	payload, err := json.Marshal(map[string]any{
		"status": "success", "data": map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			"urnetwork_connect_companion_origin_lookups_total",
			"urnetwork_connect_companion_origin_wait_wakes_total",
			"urnetwork_contract_origin_notifications_total",
			"urnetwork_connect_companion_origin_lookups_per_request_count",
			"timestamp%28", "%5B5m%5D", "%22synthetic%22",
		} {
			if !strings.Contains(command, want) {
				t.Fatalf("origin-wait Mimir query omitted %q", want)
			}
		}
		if host.Name != "metrics-1" {
			t.Fatalf("unexpected gateway %q", host.Name)
		}
		return string(payload), nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings
}

func TestOriginWaitSignalDetectsDatabaseAmplification(t *testing.T) {
	now := time.Date(2026, 9, 26, 23, 0, 0, 0, time.UTC)
	settings := originWaitSyntheticSettings(t, now, originWaitFixture{
		lookup:   map[string]float64{"initial": 100, "fallback": 350, "deadline": 50, "event": 0},
		requests: 100,
	})
	alerts, err := NewOriginWaitSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "origin-wait-db-amplification")
	if requireAlertClassCount(alerts, "origin-notification-loss") != 0 ||
		requireAlertClassCount(alerts, "origin-wait-unobservable") != 0 ||
		!strings.Contains(alert.Markdown(), "fallback_plus_deadline_lookups_per_minute=24000.0") ||
		!strings.Contains(alert.Markdown(), "lookups_per_request=5.00") {
		t.Fatalf("wrong wait amplification finding: %+v", alerts)
	}
}

func TestOriginWaitSignalDetectsNotificationLoss(t *testing.T) {
	now := time.Date(2026, 9, 26, 23, 1, 0, 0, time.UTC)
	settings := originWaitSyntheticSettings(t, now, originWaitFixture{
		lookup:   map[string]float64{"initial": 100, "event": 50},
		requests: 100,
		notification: map[string]float64{
			"queue_full": 0.5, "publish_failed": 0.25, "subscription_failed": 0.25,
		},
	})
	alerts, err := NewOriginWaitSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "origin-notification-loss")
	if requireAlertClassCount(alerts, "origin-wait-db-amplification") != 0 ||
		!strings.Contains(alert.Markdown(), "loss_events_per_minute=60.0") ||
		!strings.Contains(alert.Markdown(), "queue_full_per_minute=30.0") {
		t.Fatalf("wrong notification finding: %+v", alerts)
	}
}

func TestOriginWaitSignalMissingChildIsVisibilityFailure(t *testing.T) {
	now := time.Date(2026, 9, 26, 23, 2, 0, 0, time.UTC)
	settings := originWaitSyntheticSettings(t, now, originWaitFixture{omitNotification: "published"})
	alerts, err := NewOriginWaitSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if requireAlertClassCount(alerts, "origin-wait-unobservable") != 1 ||
		!strings.Contains(requireAlertClass(t, alerts, "origin-wait-unobservable").Markdown(), "visibility_reason=incomplete_metric_family") {
		t.Fatalf("missing counter child was treated as zero: %+v", alerts)
	}
}

func TestOriginWaitSignalRejectsUnboundedMetricLabel(t *testing.T) {
	now := time.Date(2026, 9, 26, 23, 3, 0, 0, time.UTC)
	settings := originWaitSyntheticSettings(t, now, originWaitFixture{extraLabel: "published"})
	alerts, err := NewOriginWaitSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "origin-wait-unobservable").Markdown()
	if !strings.Contains(markdown, "visibility_reason=unknown_metric_label") || strings.Contains(markdown, "synthetic-private") {
		t.Fatal("unbounded label leaked into origin wait alert")
	}
}
