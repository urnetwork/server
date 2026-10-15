package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	servermonitor "github.com/urnetwork/server/monitor"
)

// A Signal with a fixed identity for handler tests.
type slackHandlerSignal struct{}

func (slackHandlerSignal) Number() string         { return "1.1" }
func (slackHandlerSignal) Key() string            { return "alpha" }
func (slackHandlerSignal) ID() string             { return "probe/alpha" }
func (slackHandlerSignal) Name() string           { return "Alpha" }
func (slackHandlerSignal) Cadence() time.Duration { return time.Minute }
func (slackHandlerSignal) Run(context.Context, servermonitor.SignalSettings) (servermonitor.Alerts, error) {
	return nil, nil
}

func TestParseMonitorOptionsSlackAlertsDefaultOn(t *testing.T) {
	opts, err := parseMonitorOptions(nil)
	if err != nil || !opts.slackAlerts {
		t.Fatalf("default slackAlerts = %t, err = %v", opts.slackAlerts, err)
	}
	opts, err = parseMonitorOptions([]string{"-slack-alerts=false"})
	if err != nil || opts.slackAlerts {
		t.Fatalf("-slack-alerts=false: slackAlerts = %t, err = %v", opts.slackAlerts, err)
	}
}

// A notifier posting to a recording webhook, and a function that closes it
// and returns what it posted.
func newRecordedSlackNotifier(t *testing.T) (*servermonitor.SlackAlertNotifier, func() []string) {
	var postsLock sync.Mutex
	posts := []string{}
	webhook := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		postsLock.Lock()
		posts = append(posts, payload.Text)
		postsLock.Unlock()
	}))
	t.Cleanup(webhook.Close)

	settings := servermonitor.DefaultSlackAlertNotifierSettings()
	settings.WebhookUrl = webhook.URL + "/services/T0SYNTHETIC/B0SYNTHETIC/synthetic"
	settings.Environment = "synthetic"
	settings.PostInterval = 0
	settings.SummaryInterval = time.Hour
	settings.Logf = func(string, ...any) {}
	notifier := servermonitor.NewSlackAlertNotifier(settings)
	return notifier, func() []string {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		notifier.Close(ctx)
		postsLock.Lock()
		defer postsLock.Unlock()
		return append([]string(nil), posts...)
	}
}

var slackHandlerAlert = servermonitor.Alert{
	SignalNumber: "1.1", SignalKey: "alpha", SignalID: "probe/alpha", SignalName: "Alpha",
	Severity: servermonitor.SeverityPage, Class: "slow", Target: "alpha-1", Environment: "synthetic",
	Symptom: "alpha slow",
}

// The continuous handler must give the notifier every execution, empty ones
// included, or an incident could never clear; stdout still gets only
// nonempty gated alerts.
func TestContinuousAlertHandlerNotifiesEmptyBatches(t *testing.T) {
	notifier, closeAndPosts := newRecordedSlackNotifier(t)
	var stdout bytes.Buffer
	handle := continuousAlertHandler(&stdout, alertFormatMarkdown, notifier)
	for _, alerts := range []servermonitor.Alerts{{slackHandlerAlert}, {}, {}} {
		if err := handle(context.Background(), slackHandlerSignal{}, servermonitor.SignalExecution{Alerts: alerts}); err != nil {
			t.Fatal(err)
		}
	}
	posts := closeAndPosts()

	if strings.Count(stdout.String(), "# Monitor alerts") != 1 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if len(posts) != 2 || !strings.HasPrefix(posts[0], "[PAGE] raised (synthetic): alpha slow") || !strings.HasPrefix(posts[1], "[CLEARED] PAGE (synthetic)") {
		t.Fatalf("posts = %q", posts)
	}
}

// The notifier gets the whole execution: a failed one, whose visibility alert
// the gate still holds, is no evidence of absence, so one complete run after
// it does not clear the incident.
func TestContinuousAlertHandlerForwardsFailedExecutions(t *testing.T) {
	notifier, closeAndPosts := newRecordedSlackNotifier(t)
	var stdout bytes.Buffer
	handle := continuousAlertHandler(&stdout, alertFormatMarkdown, notifier)
	cannotObserve := servermonitor.Alert{
		SignalNumber: "1.1", SignalKey: "alpha", SignalID: "monitor/visibility", SignalName: "Alpha",
		Severity: servermonitor.SeverityWarn, Class: "cannot-observe", Target: "probe/alpha", Sustain: 2,
	}
	for _, execution := range []servermonitor.SignalExecution{
		{Alerts: servermonitor.Alerts{slackHandlerAlert}},
		{HeldAlerts: servermonitor.Alerts{cannotObserve}, Failed: true},
		{},
	} {
		if err := handle(context.Background(), slackHandlerSignal{}, execution); err != nil {
			t.Fatal(err)
		}
	}
	posts := closeAndPosts()
	if len(posts) != 1 || !strings.HasPrefix(posts[0], "[PAGE] raised (synthetic): alpha slow") {
		t.Fatalf("posts = %q", posts)
	}
}

// With notifications off, the handler still writes alerts.
func TestContinuousAlertHandlerWithoutNotifier(t *testing.T) {
	var stdout bytes.Buffer
	handle := continuousAlertHandler(&stdout, alertFormatJSONL, nil)
	alert := servermonitor.Alert{SignalID: "probe/alpha", Severity: servermonitor.SeverityWarn, Class: "slow", Target: "alpha-1"}
	if err := handle(context.Background(), slackHandlerSignal{}, servermonitor.SignalExecution{Alerts: servermonitor.Alerts{alert}}); err != nil {
		t.Fatal(err)
	}
	if err := handle(context.Background(), slackHandlerSignal{}, servermonitor.SignalExecution{}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(stdout.String(), "\n") != 1 || !strings.Contains(stdout.String(), `"class":"slow"`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}
