package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// Under the 15-minute cadence floor the active probes' second round starts
// near 30 minutes, so the start window stretches to three floors; without a
// floor it keeps the 30-minute default.
func TestSlackNotifierStartWindowCoversTwoRoundsOfTheCadenceFloor(t *testing.T) {
	for _, c := range []struct {
		args       []string
		wantWindow time.Duration
	}{
		{args: nil, wantWindow: 30 * time.Minute},
		{args: []string{"-min-probe-cadence=15m"}, wantWindow: 45 * time.Minute},
	} {
		opts, err := parseMonitorOptions(c.args)
		if err != nil {
			t.Fatal(err)
		}
		if window := slackNotifierSettings(servermonitor.SignalSettings{}, opts).StartWindow; window != c.wantWindow {
			t.Errorf("%v: start window = %s, want %s", c.args, window, c.wantWindow)
		}
	}
}

// Only a watcher that posts compares the webhook. One started with
// -slack-alerts=false, or a -once run, stays fresh across a webhook rotation;
// a posting watcher goes stale.
func TestEffectiveSettingsGenerationComparesTheWebhookOnlyWhenPosting(t *testing.T) {
	const oldWebhook = "https://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/old"
	for _, c := range []struct {
		args      []string
		wantStale bool
	}{
		{args: []string{"-slack-alerts=false"}, wantStale: false},
		{args: []string{"-once"}, wantStale: false},
		{args: nil, wantStale: true},
	} {
		opts, err := parseMonitorOptions(c.args)
		if err != nil {
			t.Fatal(err)
		}
		current := servermonitor.SignalSettings{
			Environment:        "synthetic",
			AlertNotifications: servermonitor.AlertNotificationSettings{SlackWebhookUrl: oldWebhook},
		}
		loadEffective := func() (servermonitor.SignalSettings, error) {
			return applyMonitorSettingsOptions(current, opts)
		}
		startup, err := loadEffective()
		if err != nil {
			t.Fatal(err)
		}
		if posting := startup.AlertNotifications.SlackWebhookUrl == oldWebhook; posting != c.wantStale {
			t.Errorf("%v: effective settings keep the webhook = %t", c.args, posting)
		}
		check := servermonitor.NewSettingsGenerationCheck(loadEffective)
		current.AlertNotifications.SlackWebhookUrl = "https://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/new"
		matched, err := check(context.Background(), startup)
		if err != nil || matched == c.wantStale {
			t.Errorf("%v: after a webhook rotation matched = %t, err = %v; want stale = %t", c.args, matched, err, c.wantStale)
		}
	}
}

// The drain at exit releases signal handling before it waits on the
// notifier, so a second interrupt or SIGTERM during the drain ends the
// process. The notifier's one post is held at the webhook to keep Close
// waiting.
func TestSlackNotifierDrainReleasesSignalsBeforeItWaits(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	webhook := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		select {
		case arrived <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	defer webhook.Close()
	settings := servermonitor.DefaultSlackAlertNotifierSettings()
	settings.WebhookUrl = webhook.URL + "/services/T0SYNTHETIC/B0SYNTHETIC/synthetic"
	settings.StartDigestHold = 0
	settings.PostInterval = 0
	// the held post outlasts the wait below, so only a released signal
	// handling can end that wait
	settings.PostTimeout = 10 * time.Minute
	settings.Logf = func(string, ...any) {}
	notifier := servermonitor.NewSlackAlertNotifier(settings)
	handle := continuousAlertHandler(&bytes.Buffer{}, alertFormatMarkdown, notifier)
	if err := handle(context.Background(), slackHandlerSignal{}, servermonitor.SignalExecution{Alerts: servermonitor.Alerts{slackHandlerAlert}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the notifier never posted")
	}

	stopped := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		drainSlackNotifier(func() { close(stopped) }, notifier.Close, 10*time.Minute)
	}()
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Error("signal handling was still captured while the notifier drained")
	}
	close(release)
	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		t.Fatal("the drain did not finish")
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
	// the first, empty run closes the start window, so the raise posts on its own
	for _, alerts := range []servermonitor.Alerts{{}, {slackHandlerAlert}, {}, {}} {
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

// The CLI's notifier folds a new watcher's first raises into one start digest
// instead of posting each.
func TestContinuousAlertHandlerFoldsFirstRaisesIntoTheStartDigest(t *testing.T) {
	notifier, closeAndPosts := newRecordedSlackNotifier(t)
	var stdout bytes.Buffer
	handle := continuousAlertHandler(&stdout, alertFormatMarkdown, notifier)
	second := slackHandlerAlert
	second.Target = "alpha-2"
	if err := handle(context.Background(), slackHandlerSignal{}, servermonitor.SignalExecution{Alerts: servermonitor.Alerts{slackHandlerAlert, second}}); err != nil {
		t.Fatal(err)
	}
	posts := closeAndPosts()
	if len(posts) != 1 || !strings.HasPrefix(posts[0], "[ACTIVE AT START] (synthetic) 2 incident(s)") {
		t.Fatalf("posts = %q", posts)
	}
}

// The notifier gets the whole execution: a failed one, whose visibility alert
// the gate still holds, is no evidence of absence, so one complete run after
// it does not clear the incident. The first, empty run closes the start
// window.
func TestContinuousAlertHandlerForwardsFailedExecutions(t *testing.T) {
	notifier, closeAndPosts := newRecordedSlackNotifier(t)
	var stdout bytes.Buffer
	handle := continuousAlertHandler(&stdout, alertFormatMarkdown, notifier)
	cannotObserve := servermonitor.Alert{
		SignalNumber: "1.1", SignalKey: "alpha", SignalID: "monitor/visibility", SignalName: "Alpha",
		Severity: servermonitor.SeverityWarn, Class: "cannot-observe", Target: "probe/alpha", Sustain: 2,
	}
	for _, execution := range []servermonitor.SignalExecution{
		{},
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
