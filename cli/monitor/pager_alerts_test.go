package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	servermonitor "github.com/urnetwork/server/monitor"
	"github.com/urnetwork/server/pager"
)

// The class of a page in §19.8's table, for handler tests.
var pagerHandlerAlert = servermonitor.Alert{
	SignalKey:   "alpha",
	SignalID:    "probe/alpha",
	Severity:    servermonitor.SeverityPage,
	Class:       "postgres-primary-down",
	Target:      "primary",
	Environment: "synthetic",
	ObservedAt:  time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC),
}

func closePagerNotifier(t *testing.T, notifier *servermonitor.PagerAlertNotifier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	notifier.Close(ctx)
}

// Off by default: a diagnostic watcher started the old way must not page, and
// going live takes this flag as well as the vault key.
func TestParseMonitorOptionsPagerAlertsDefaultOff(t *testing.T) {
	opts, err := parseMonitorOptions(nil)
	if err != nil || opts.pagerAlerts {
		t.Fatalf("default pagerAlerts = %t, err = %v", opts.pagerAlerts, err)
	}
	opts, err = parseMonitorOptions([]string{"-pager-alerts"})
	if err != nil || !opts.pagerAlerts {
		t.Fatalf("-pager-alerts: pagerAlerts = %t, err = %v", opts.pagerAlerts, err)
	}
}

// Only a watcher that pages compares the pager webhook, and each webhook drops
// on its own flag.
func TestEffectiveSettingsGenerationComparesThePagerWebhookOnlyWhenPaging(t *testing.T) {
	const slackWebhook = "https://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/old"
	const oldWebhook = "https://pager.example/events/old"
	for _, c := range []struct {
		args      []string
		wantSlack bool
		wantStale bool
	}{
		{args: nil, wantSlack: true, wantStale: false},
		{args: []string{"-once"}, wantSlack: false, wantStale: false},
		{args: []string{"-pager-alerts"}, wantSlack: true, wantStale: true},
		{args: []string{"-pager-alerts", "-slack-alerts=false"}, wantSlack: false, wantStale: true},
		{args: []string{"-pager-alerts", "-once"}, wantSlack: false, wantStale: false},
	} {
		opts, err := parseMonitorOptions(c.args)
		if err != nil {
			t.Fatal(err)
		}
		current := servermonitor.SignalSettings{
			Environment: "synthetic",
			AlertNotifications: servermonitor.AlertNotificationSettings{
				SlackWebhookUrl: slackWebhook,
				PagerWebhookUrl: oldWebhook,
			},
		}
		loadEffective := func() (servermonitor.SignalSettings, error) {
			return applyMonitorSettingsOptions(current, opts)
		}
		startup, err := loadEffective()
		if err != nil {
			t.Fatal(err)
		}
		if keeping := startup.AlertNotifications.SlackWebhookUrl == slackWebhook; keeping != c.wantSlack {
			t.Errorf("%v: effective settings keep the Slack webhook = %t", c.args, keeping)
		}
		if paging := startup.AlertNotifications.PagerWebhookUrl == oldWebhook; paging != c.wantStale {
			t.Errorf("%v: effective settings keep the pager webhook = %t", c.args, paging)
		}
		check := servermonitor.NewSettingsGenerationCheck(loadEffective)
		current.AlertNotifications.PagerWebhookUrl = "https://pager.example/events/new"
		matched, err := check(context.Background(), startup)
		if err != nil || matched == c.wantStale {
			t.Errorf("%v: after a webhook rotation matched = %t, err = %v; want stale = %t", c.args, matched, err, c.wantStale)
		}
	}
}

// Fails closed: until a service is chosen, a configured endpoint off loopback
// builds no sink, and the reason names the question and never the endpoint.
func TestPagerNotifierSettingsBuildNoSinkOffLoopback(t *testing.T) {
	const secret = "syntheticPagerSecret0001"
	settings := servermonitor.SignalSettings{
		Environment:        "synthetic",
		AlertNotifications: servermonitor.AlertNotificationSettings{PagerWebhookUrl: "https://pager.example/events/" + secret},
	}
	notifierSettings := pagerNotifierSettings(settings, nil)
	if notifierSettings.Sink != nil {
		t.Fatal("an endpoint off loopback built a sink")
	}
	if !strings.Contains(notifierSettings.DisabledReason, "plan-OPS-pager-service") || strings.Contains(notifierSettings.DisabledReason, secret) {
		t.Fatalf("reason %q", notifierSettings.DisabledReason)
	}
	if notifierSettings.Warn != nil {
		t.Fatal("a watcher without Slack has a warn carrier")
	}
}

func TestPagerNotifierSettingsBuildNoSinkWithoutAnEndpoint(t *testing.T) {
	notifierSettings := pagerNotifierSettings(servermonitor.SignalSettings{Environment: "synthetic"}, nil)
	if notifierSettings.Sink != nil || notifierSettings.DisabledReason != pager.ErrNoWebhookUrl.Error() {
		t.Fatalf("sink %v, reason %q", notifierSettings.Sink, notifierSettings.DisabledReason)
	}
}

// The pager clears its warn on the count of the Slack notifier that carries it,
// not on a default the two happen to share.
func TestPagerNotifierSettingsTakeTheSlackNotifiersClearCount(t *testing.T) {
	slackSettings := servermonitor.DefaultSlackAlertNotifierSettings()
	slackSettings.ClearAfterCompleteRuns = 5
	slackSettings.Logf = func(string, ...any) {}
	slackNotifier := servermonitor.NewSlackAlertNotifier(slackSettings)
	notifierSettings := pagerNotifierSettings(servermonitor.SignalSettings{Environment: "synthetic"}, slackNotifier)
	if notifierSettings.Warn == nil || notifierSettings.WarnClearRuns != 5 {
		t.Fatalf("warn carrier set = %t, clear runs = %d", notifierSettings.Warn != nil, notifierSettings.WarnClearRuns)
	}
}

// A watcher asked to page that cannot says so on Slack.
func TestPagerWithoutASinkWarnsThroughTheSlackNotifier(t *testing.T) {
	notifier, closeAndPosts := newRecordedSlackNotifier(t)
	pagerSettings := pagerNotifierSettings(servermonitor.SignalSettings{Environment: "synthetic"}, notifier)
	pagerSettings.Logf = func(string, ...any) {}
	servermonitor.NewPagerAlertNotifier(pagerSettings)
	texts := closeAndPosts()
	// it comes at the watcher's start, so it rides in the start digest
	if len(texts) != 1 || !strings.Contains(texts[0], "[WARN] monitor/pager-sink · pager-sink-failing") {
		t.Fatalf("posted %q", texts)
	}
}

// The watcher's wiring end to end: every gated execution reaches the pager and
// the output, and the page reaches a loopback pager service.
func TestPagedAlertHandlerPagesBesideTheOutput(t *testing.T) {
	service := pager.NewFakeSink(pager.FakeSinkSettings{})
	endpoint := httptest.NewServer(service)
	defer endpoint.Close()
	settings := servermonitor.SignalSettings{
		Environment:        "synthetic",
		AlertNotifications: servermonitor.AlertNotificationSettings{PagerWebhookUrl: endpoint.URL + "/events"},
	}
	notifierSettings := pagerNotifierSettings(settings, nil)
	notifierSettings.Logf = func(string, ...any) {}
	notifier := servermonitor.NewPagerAlertNotifier(notifierSettings)
	var stdout bytes.Buffer
	handle := pagedAlertHandler(notifier, continuousAlertHandler(&stdout, alertFormatMarkdown, nil))
	if err := handle(context.Background(), slackHandlerSignal{}, servermonitor.SignalExecution{Alerts: servermonitor.Alerts{pagerHandlerAlert}}); err != nil {
		t.Fatal(err)
	}
	closePagerNotifier(t, notifier)
	notifications := service.Notifications()
	if len(notifications) != 1 || notifications[0].Person != pager.FakeFirstPerson || notifications[0].Page.Class != "postgres-primary-down" {
		t.Fatalf("notifications %+v", notifications)
	}
	if !strings.Contains(stdout.String(), "postgres-primary-down") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
}

// An output that fails cannot hold a page back, and its error still reaches the
// loop.
func TestPagedAlertHandlerPagesFirst(t *testing.T) {
	service := pager.NewFakeSink(pager.FakeSinkSettings{})
	notifier := servermonitor.NewPagerAlertNotifier(servermonitor.PagerAlertNotifierSettings{Sink: service, Logf: func(string, ...any) {}})
	refused := errors.New("synthetic output failure")
	handle := pagedAlertHandler(notifier, func(context.Context, servermonitor.Signal, servermonitor.SignalExecution) error {
		return refused
	})
	if err := handle(context.Background(), slackHandlerSignal{}, servermonitor.SignalExecution{Alerts: servermonitor.Alerts{pagerHandlerAlert}}); !errors.Is(err, refused) {
		t.Fatalf("err = %v", err)
	}
	closePagerNotifier(t, notifier)
	if got := len(service.Notifications()); got != 1 {
		t.Fatalf("%d notifications", got)
	}
}

// The drain at exit gives a stuck pager its deadline and no more.
func TestPagerDrainEndsAtItsDeadline(t *testing.T) {
	held := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		// the server notices a client disconnect only once the body is read
		_, _ = io.Copy(io.Discard, request.Body)
		select {
		case held <- struct{}{}:
		case <-request.Context().Done():
			return
		}
		<-request.Context().Done()
	}))
	defer endpoint.Close()
	settings := servermonitor.SignalSettings{
		Environment:        "synthetic",
		AlertNotifications: servermonitor.AlertNotificationSettings{PagerWebhookUrl: endpoint.URL + "/events"},
	}
	notifierSettings := pagerNotifierSettings(settings, nil)
	notifierSettings.Logf = func(string, ...any) {}
	notifier := servermonitor.NewPagerAlertNotifier(notifierSettings)
	notifier.Handle(slackHandlerSignal{}, servermonitor.SignalExecution{Alerts: servermonitor.Alerts{pagerHandlerAlert}})
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("the page never reached the endpoint")
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		drainNotifier(func() {}, notifier.Close, 0)
	}()
	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		t.Fatal("the drain outlived its deadline")
	}
}
