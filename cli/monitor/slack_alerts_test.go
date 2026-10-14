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

// The continuous handler must give the notifier every batch, empty ones
// included, or an incident could never clear; stdout still gets only
// nonempty batches.
func TestContinuousAlertHandlerNotifiesEmptyBatches(t *testing.T) {
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
	defer webhook.Close()

	settings := servermonitor.DefaultSlackAlertNotifierSettings()
	settings.WebhookUrl = webhook.URL + "/services/T0SYNTHETIC/B0SYNTHETIC/synthetic"
	settings.Environment = "synthetic"
	settings.PostInterval = 0
	settings.SummaryInterval = time.Hour
	settings.Logf = func(string, ...any) {}
	notifier := servermonitor.NewSlackAlertNotifier(settings)

	var stdout bytes.Buffer
	handle := continuousAlertHandler(&stdout, alertFormatMarkdown, notifier)
	alert := servermonitor.Alert{
		SignalNumber: "1.1", SignalKey: "alpha", SignalID: "probe/alpha", SignalName: "Alpha",
		Severity: servermonitor.SeverityPage, Class: "slow", Target: "alpha-1", Environment: "synthetic",
		Symptom: "alpha slow",
	}
	for _, alerts := range []servermonitor.Alerts{{alert}, {}, {}} {
		if err := handle(context.Background(), slackHandlerSignal{}, alerts); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	notifier.Close(ctx)

	if strings.Count(stdout.String(), "# Monitor alerts") != 1 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	postsLock.Lock()
	defer postsLock.Unlock()
	if len(posts) != 2 || !strings.HasPrefix(posts[0], "[PAGE] raised (synthetic): alpha slow") || !strings.HasPrefix(posts[1], "[CLEARED] PAGE (synthetic)") {
		t.Fatalf("posts = %q", posts)
	}
}

// With notifications off, the handler still writes alerts.
func TestContinuousAlertHandlerWithoutNotifier(t *testing.T) {
	var stdout bytes.Buffer
	handle := continuousAlertHandler(&stdout, alertFormatJSONL, nil)
	alert := servermonitor.Alert{SignalID: "probe/alpha", Severity: servermonitor.SeverityWarn, Class: "slow", Target: "alpha-1"}
	if err := handle(context.Background(), slackHandlerSignal{}, servermonitor.Alerts{alert}); err != nil {
		t.Fatal(err)
	}
	if err := handle(context.Background(), slackHandlerSignal{}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Count(stdout.String(), "\n") != 1 || !strings.Contains(stdout.String(), `"class":"slow"`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}
