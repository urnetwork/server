package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The registered signal for the account message send counters. Before §19.3
// nothing watched them, so a channel whose every call was refused raised no
// warning: the failure log is info-level and rate limited.
func accountMessagesSignalForTest(t *testing.T) Signal {
	t.Helper()
	for _, signal := range NewSignals() {
		if signal.Key() == "account-messages" {
			return signal
		}
	}
	t.Fatal("no registered signal watches urnetwork_account_message_sends_total; a channel that delivers nothing raises no warning")
	return nil
}

// One synthetic per-service channel result over the probe's range.
type accountMessagesFixture struct {
	job     string
	channel string
	result  string
	value   float64
}

// A Mimir vector for the probe's query, sampled at sampledAt.
func accountMessagesFixtureJSON(t testing.TB, sampledAt time.Time, fixtures []accountMessagesFixture) string {
	t.Helper()
	result := []map[string]any{}
	for _, fixture := range fixtures {
		metric := map[string]string{"job": fixture.job, "channel": fixture.channel, "result": fixture.result}
		for key, value := range metric {
			if value == "" {
				delete(metric, key)
			}
		}
		result = append(result, map[string]any{
			"metric": metric,
			"value":  []any{float64(sampledAt.Unix()), fmt.Sprintf("%g", fixture.value)},
		})
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

var accountMessagesTestNow = time.Date(2026, 10, 18, 23, 30, 0, 0, time.UTC)

// Runs the signal over a Mimir payload, checking the query it sends.
func runAccountMessagesPayload(t *testing.T, payload string) Alerts {
	t.Helper()
	signal := accountMessagesSignalForTest(t)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			"urnetwork_account_message_sends_total",
			"sum+by+%28job%2Cchannel%2Cresult%29",
			"%5B24h%5D",
			"%22synthetic%22",
			"127.0.0.1:3100/prometheus/api/v1/query",
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("account messages query missing %q: %s", want, command)
			}
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return accountMessagesTestNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func runAccountMessagesFixture(t *testing.T, fixtures []accountMessagesFixture) Alerts {
	t.Helper()
	return runAccountMessagesPayload(t, accountMessagesFixtureJSON(t, accountMessagesTestNow, fixtures))
}

// Both channels deliver, with an isolated failure and a timeout: healthy.
func TestAccountMessagesSignalSyntheticHealthyChannels(t *testing.T) {
	alerts := runAccountMessagesFixture(t, []accountMessagesFixture{
		{job: "api", channel: "email", result: "sent", value: 137},
		{job: "api", channel: "email", result: "timeout", value: 1},
		{job: "taskworker", channel: "email", result: "sent", value: 1430},
		{job: "api", channel: "sms", result: "sent", value: 6},
		{job: "api", channel: "sms", result: "failed", value: 1},
	})
	if len(alerts) != 0 {
		t.Fatalf("healthy channels alerted: %+v", alerts)
	}
}

// The 2026-10-18 production shape: every SMS call refused for a week while
// email delivered. The SMS channel pages, naming each failing service, and
// email stays quiet.
func TestAccountMessagesSignalSyntheticChannelDeliversNothing(t *testing.T) {
	alerts := runAccountMessagesFixture(t, []accountMessagesFixture{
		{job: "api", channel: "email", result: "sent", value: 137},
		{job: "taskworker", channel: "email", result: "sent", value: 1430},
		{job: "alt", channel: "email", result: "sent", value: 0},
		{job: "api", channel: "sms", result: "failed", value: 4.1},
		{job: "taskworker", channel: "sms", result: "failed", value: 13},
	})
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want only the SMS page: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "account-message-channel-down")
	if alert.SignalNumber != "19.3" || alert.SignalKey != "account-messages" || alert.Severity != SeverityPage ||
		alert.Frame != "channel=sms" || alert.Sustain != 2 {
		t.Fatalf("wrong channel-down identity: %+v", alert)
	}
	for _, want := range []string{
		"account sms delivered nothing in the last 24h while 17 send(s) failed",
		"channel=sms range=24h sent=0 failed=17 timeout=0 failure_share=1.000 failed_by_job=taskworker=13/13,api=4/4",
		"sms-voice:SendTextMessage",
		"[aws]account <channel> send failed (<code>)",
		"SIGNALS.md §19.3",
	} {
		if !strings.Contains(alert.Symptom+alert.Observed+alert.Mechanism+alert.Context+alert.Action+alert.Playbook, want) {
			t.Errorf("channel-down alert missing %q: %+v", want, alert)
		}
	}
	var markdown strings.Builder
	if err := WriteAlertsMarkdown(&markdown, alerts); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"@", "+1"} {
		if strings.Contains(markdown.String(), forbidden) {
			t.Errorf("markdown carries a recipient-shaped value %q", forbidden)
		}
	}
}

// Timeouts count as failures: a channel whose calls all time out is down.
func TestAccountMessagesSignalSyntheticTimeoutsOnlyIsDown(t *testing.T) {
	alerts := runAccountMessagesFixture(t, []accountMessagesFixture{
		{job: "api", channel: "email", result: "timeout", value: 3},
	})
	alert := requireAlertClass(t, alerts, "account-message-channel-down")
	if alert.Frame != "channel=email" || !strings.Contains(alert.Observed, "sent=0 failed=0 timeout=3") {
		t.Fatalf("timeout-only channel: %+v", alert)
	}
}

// Fewer failures than the floor, with nothing delivered, can be one transient
// refusal on a rarely used channel: no page.
func TestAccountMessagesSignalSyntheticBelowFloorIsQuiet(t *testing.T) {
	alerts := runAccountMessagesFixture(t, []accountMessagesFixture{
		{job: "api", channel: "email", result: "sent", value: 30},
		{job: "api", channel: "sms", result: "failed", value: 2},
	})
	if len(alerts) != 0 {
		t.Fatalf("two failures below the floor alerted: %+v", alerts)
	}
}

// One delivered send ends the page at once, even with the week's failures
// still inside the range; too few attempts to judge a share stay quiet.
func TestAccountMessagesSignalSyntheticFirstSuccessClearsDown(t *testing.T) {
	alerts := runAccountMessagesFixture(t, []accountMessagesFixture{
		{job: "api", channel: "email", result: "sent", value: 30},
		{job: "api", channel: "sms", result: "failed", value: 17},
		{job: "api", channel: "sms", result: "sent", value: 1},
	})
	if len(alerts) != 0 {
		t.Fatalf("a delivering channel below the share's attempt floor alerted: %+v", alerts)
	}
}

// A channel that still delivers but fails a quarter of enough sends warns.
func TestAccountMessagesSignalSyntheticDegradedShareWarns(t *testing.T) {
	alerts := runAccountMessagesFixture(t, []accountMessagesFixture{
		{job: "api", channel: "email", result: "sent", value: 60},
		{job: "api", channel: "email", result: "failed", value: 15},
		{job: "taskworker", channel: "email", result: "timeout", value: 5},
	})
	alert := requireAlertClass(t, alerts, "account-message-channel-degraded")
	if alert.Severity != SeverityWarn || alert.Frame != "channel=email" {
		t.Fatalf("wrong degraded identity: %+v", alert)
	}
	for _, want := range []string{
		"account email failed 20 of 80 sends in the last 24h",
		"failure_share=0.250 failed_by_job=api=15/75,taskworker=5/5",
	} {
		if !strings.Contains(alert.Symptom+alert.Observed, want) {
			t.Errorf("degraded alert missing %q: %+v", want, alert)
		}
	}
}

// Just under the share does not warn.
func TestAccountMessagesSignalSyntheticDegradedShareEdge(t *testing.T) {
	alerts := runAccountMessagesFixture(t, []accountMessagesFixture{
		{job: "api", channel: "email", result: "sent", value: 61},
		{job: "api", channel: "email", result: "failed", value: 20},
	})
	if len(alerts) != 0 {
		t.Fatalf("a share under 25%% alerted: %+v", alerts)
	}
}

// Readings that cannot be trusted are unknown, never a healthy zero: no
// series, an unlabeled or unknown series, a stale sample and a negative value.
func TestAccountMessagesSignalSyntheticUnreadableFrameIsUnobservable(t *testing.T) {
	stale := accountMessagesTestNow.Add(-10 * time.Minute)
	cases := []struct {
		reason  string
		payload string
	}{
		{reason: "no_series", payload: accountMessagesFixtureJSON(t, accountMessagesTestNow, nil)},
		{reason: "unlabeled_series", payload: accountMessagesFixtureJSON(t, accountMessagesTestNow, []accountMessagesFixture{
			{job: "api", channel: "sms", value: 4},
		})},
		{reason: "unknown_channel_or_result", payload: accountMessagesFixtureJSON(t, accountMessagesTestNow, []accountMessagesFixture{
			{job: "api", channel: "push", result: "sent", value: 4},
		})},
		{reason: "unknown_channel_or_result", payload: accountMessagesFixtureJSON(t, accountMessagesTestNow, []accountMessagesFixture{
			{job: "api", channel: "sms", result: "bounced", value: 4},
		})},
		{reason: "invalid_or_stale_metric", payload: accountMessagesFixtureJSON(t, stale, []accountMessagesFixture{
			{job: "api", channel: "sms", result: "failed", value: 40},
		})},
		{reason: "invalid_or_stale_metric", payload: accountMessagesFixtureJSON(t, accountMessagesTestNow, []accountMessagesFixture{
			{job: "api", channel: "email", result: "sent", value: -1},
		})},
	}
	for _, c := range cases {
		alerts := runAccountMessagesPayload(t, c.payload)
		if len(alerts) != 1 {
			t.Errorf("%s: got %d alerts, want only the visibility warning: %+v", c.reason, len(alerts), alerts)
			continue
		}
		alert := requireAlertClass(t, alerts, "account-message-sends-unobservable")
		if alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "visibility_reason="+c.reason) {
			t.Errorf("%s: wrong visibility alert: %+v", c.reason, alert)
		}
	}
}

// A channel with no series had no send of any result since its processes
// started, so it cannot be failing; the other channel's series prove the
// counter is exported.
func TestAccountMessagesSignalSyntheticChannelWithoutSeriesIsQuiet(t *testing.T) {
	alerts := runAccountMessagesFixture(t, []accountMessagesFixture{
		{job: "api", channel: "email", result: "sent", value: 12},
	})
	if len(alerts) != 0 {
		t.Fatalf("a channel without sends alerted: %+v", alerts)
	}
}

// A Mimir answer that is not a successful vector is a probe failure, which the
// monitor run reports as a visibility alert, never a healthy result.
func TestAccountMessagesSignalSyntheticFailedQueryIsProbeFailure(t *testing.T) {
	signal := accountMessagesSignalForTest(t)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		return `{"status":"error","errorType":"bad_data","error":"synthetic"}`, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return accountMessagesTestNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := signal.Run(context.Background(), settings)
	if err == nil || !strings.Contains(err.Error(), "not a successful vector") || len(alerts) != 0 {
		t.Fatalf("failed query = %+v, %v; want a probe failure", alerts, err)
	}
}
