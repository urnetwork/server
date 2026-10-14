package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The secret part of a synthetic webhook path. Real webhooks carry their
// credential in the path, so it must never reach a log line.
const syntheticSlackSecret = "syntheticSlackSecret0001"

// A recording incoming webhook. Each accepted post's text is kept in order.
type fakeSlackWebhook struct {
	stateLock sync.Mutex
	texts     []string
	requests  int
	// status and body for every response; zero status answers 200 "ok"
	status int
	body   string
	// when set, the handler holds every request until it is canceled
	hold bool
	// receives once per answered request, after the response is written
	answered chan struct{}
}

func newFakeSlackWebhook(t *testing.T) (*fakeSlackWebhook, string) {
	webhook := &fakeSlackWebhook{answered: make(chan struct{}, 64)}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		webhook.stateLock.Lock()
		webhook.requests++
		hold, status, body := webhook.hold, webhook.status, webhook.body
		webhook.stateLock.Unlock()
		if hold {
			// the server notices a client disconnect only once the body is read
			_, _ = io.Copy(io.Discard, request.Body)
			<-request.Context().Done()
			return
		}
		defer func() {
			select {
			case webhook.answered <- struct{}{}:
			default:
			}
		}()
		var payload struct {
			Text        string `json:"text"`
			UnfurlLinks bool   `json:"unfurl_links"`
		}
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" ||
			json.NewDecoder(request.Body).Decode(&payload) != nil || payload.UnfurlLinks {
			response.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(response, "invalid_payload")
			return
		}
		if status != 0 {
			response.WriteHeader(status)
			_, _ = io.WriteString(response, body)
			return
		}
		webhook.stateLock.Lock()
		webhook.texts = append(webhook.texts, payload.Text)
		webhook.stateLock.Unlock()
		_, _ = io.WriteString(response, "ok")
	}))
	t.Cleanup(server.Close)
	return webhook, server.URL + "/services/T0SYNTHETIC/B0SYNTHETIC/" + syntheticSlackSecret
}

func (self *fakeSlackWebhook) posted() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.texts...)
}

func (self *fakeSlackWebhook) requestCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.requests
}

func (self *fakeSlackWebhook) respond(status int, body string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.status = status
	self.body = body
}

// A manually advanced clock and a log sink, shared by the notifier tests.
type slackTestHarness struct {
	stateLock sync.Mutex
	now       time.Time
	logs      []string
}

func newSlackTestHarness() *slackTestHarness {
	return &slackTestHarness{now: time.Date(2026, 10, 14, 18, 0, 0, 0, time.UTC)}
}

func (self *slackTestHarness) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *slackTestHarness) advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(duration)
}

func (self *slackTestHarness) Logf(format string, args ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.logs = append(self.logs, fmt.Sprintf(format, args...))
}

func (self *slackTestHarness) logLines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.logs...)
}

// Settings that never post or flush on a wall-clock schedule: the only
// summary flushes are explicit, and Close drains the queue.
func (self *slackTestHarness) settings(webhookUrl string) SlackAlertNotifierSettings {
	return SlackAlertNotifierSettings{
		WebhookUrl:      webhookUrl,
		Environment:     "synthetic",
		PostInterval:    0,
		PostTimeout:     5 * time.Second,
		SummaryInterval: time.Hour,
		Logf:            self.Logf,
		Now:             self.Now,
	}
}

func closeSlackNotifier(t *testing.T, notifier *SlackAlertNotifier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	notifier.Close(ctx)
	if ctx.Err() != nil {
		t.Fatal("notifier did not drain before the close deadline")
	}
}

var slackTestSignal = selectionSignal{number: "1.3", key: "active-pileup", id: "pg/active-pileup"}

func slackTestAlert(target string, frame string, severity Severity) Alert {
	return Alert{
		SignalNumber: slackTestSignal.number,
		SignalKey:    slackTestSignal.key,
		SignalID:     slackTestSignal.id,
		SignalName:   "PostgreSQL active backends",
		Severity:     severity,
		Class:        "active-pileup",
		Target:       target,
		Frame:        frame,
		Environment:  "synthetic",
		Symptom:      "Active client backends on " + target + " exceed the band",
		Playbook:     "SIGNALS.md 5.8",
	}
}

func requireSlackTexts(t *testing.T, got []string, wantPrefixes ...string) {
	t.Helper()
	if len(got) != len(wantPrefixes) {
		t.Fatalf("posted %d messages, want %d:\n%s", len(got), len(wantPrefixes), strings.Join(got, "\n---\n"))
	}
	for i, prefix := range wantPrefixes {
		if !strings.HasPrefix(got[i], prefix) {
			t.Errorf("message %d = %q, want prefix %q", i, got[i], prefix)
		}
	}
}

// One message per transition: a raise, nothing while the incident stays
// active (frame changes included), a clear after two consecutive complete
// runs without it, and a new raise when it returns.
func TestSlackAlertNotifierPostsOneMessagePerTransition(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))

	for _, alerts := range []Alerts{
		{slackTestAlert("pg-1", "query-a", SeverityWarn)},
		{slackTestAlert("pg-1", "query-a", SeverityWarn)},
		// the same incident with different attribution
		{slackTestAlert("pg-1", "query-b", SeverityWarn), slackTestAlert("pg-1", "query-c", SeverityWarn)},
		// one absent run, then present again: the absence count restarts
		{},
		{slackTestAlert("pg-1", "query-a", SeverityWarn)},
		{},
		{},
		{},
		{slackTestAlert("pg-1", "query-a", SeverityWarn)},
	} {
		notifier.Handle(slackTestSignal, alerts)
		harness.advance(time.Minute)
	}
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-1 exceed the band",
		"[CLEARED] WARN (synthetic): PostgreSQL active backends",
		"[WARN] raised (synthetic): Active client backends on pg-1 exceed the band",
	)
	if !strings.Contains(texts[0], "pg/active-pileup · active-pileup · pg-1") ||
		!strings.Contains(texts[0], "SIGNALS.md §1.3 · active-pileup · playbook SIGNALS.md 5.8") {
		t.Errorf("raise message lacks its identity or reference: %q", texts[0])
	}
	if strings.Contains(texts[0], "query-a") {
		t.Errorf("raise message carries the frame: %q", texts[0])
	}
	if !strings.Contains(texts[1], "Absent from 2 consecutive complete runs") || !strings.Contains(texts[1], "not proof of recovery") {
		t.Errorf("clear message overstates recovery: %q", texts[1])
	}
}

// A warn incident promoted to page severity is announced once; a later warn
// report does not post, and the clear names the highest severity announced.
func TestSlackAlertNotifierEscalatesOnceToPage(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))

	for _, alerts := range []Alerts{
		{slackTestAlert("pg-1", "", SeverityWarn)},
		{slackTestAlert("pg-1", "", SeverityPage)},
		{slackTestAlert("pg-1", "", SeverityPage)},
		{slackTestAlert("pg-1", "", SeverityWarn)},
		// a page report beside a warn one for the same incident counts as page
		{slackTestAlert("pg-2", "a", SeverityWarn), slackTestAlert("pg-2", "b", SeverityPage)},
		{},
		{},
	} {
		notifier.Handle(slackTestSignal, alerts)
	}
	closeSlackNotifier(t, notifier)

	requireSlackTexts(t, webhook.posted(),
		"[WARN] raised (synthetic): Active client backends on pg-1",
		"[PAGE] escalated from WARN (synthetic): Active client backends on pg-1",
		"[PAGE] raised (synthetic): Active client backends on pg-2",
		"[CLEARED] PAGE (synthetic)",
		"[CLEARED] PAGE (synthetic)",
	)
}

// A run that could not observe its source is not evidence of recovery: its
// absent incidents keep their state until complete runs stop reporting them.
func TestSlackAlertNotifierKeepsIncidentsThroughIncompleteRuns(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))

	cannotObserve := visibilityAlert(syntheticSettings(nil), slackTestSignal, errors.New("synthetic ssh timeout"))
	notifier.Handle(slackTestSignal, Alerts{slackTestAlert("pg-1", "", SeverityPage)})
	// three runs that could not observe: an early clear of pg-1 would
	// appear here, ahead of the visibility clear
	for i := 0; i < 3; i++ {
		notifier.Handle(slackTestSignal, Alerts{cannotObserve})
	}
	notifier.Handle(slackTestSignal, Alerts{})
	notifier.Handle(slackTestSignal, Alerts{})
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[PAGE] raised (synthetic): Active client backends on pg-1",
		"[WARN] raised (synthetic): Signal 1.3 (pg/active-pileup) could not run",
		"[CLEARED] WARN (synthetic)",
		"[CLEARED] PAGE (synthetic)",
	)
	if !strings.Contains(texts[2], "monitor/visibility · cannot-observe") || !strings.Contains(texts[3], "pg/active-pileup · active-pileup · pg-1") {
		t.Fatalf("clears = %q", texts[2:])
	}
}

// An incident two signals report clears only after both stop reporting it.
func TestSlackAlertNotifierClearsASharedIncidentAfterEveryReporter(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))

	otherSignal := selectionSignal{number: "3.1", key: "redis-memory", id: "redis/memory"}
	shared := func(signal selectionSignal) Alert {
		return Alert{
			SignalNumber: signal.number, SignalKey: signal.key, SignalID: "monitor/host-scope",
			Severity: SeverityWarn, Class: "monitor-host-scope-partial", Target: "monitor-host-scope",
			Frame: signal.key, Symptom: "Some host observations are excluded",
		}
	}
	notifier.Handle(slackTestSignal, Alerts{shared(slackTestSignal)})
	notifier.Handle(otherSignal, Alerts{shared(otherSignal)})
	// the first signal stops reporting: an early clear would be followed by a
	// second raise from the other signal
	for i := 0; i < 3; i++ {
		notifier.Handle(slackTestSignal, Alerts{})
		notifier.Handle(otherSignal, Alerts{shared(otherSignal)})
	}
	notifier.Handle(otherSignal, Alerts{})
	notifier.Handle(otherSignal, Alerts{})
	closeSlackNotifier(t, notifier)

	requireSlackTexts(t, webhook.posted(),
		"[WARN] raised (synthetic): Some host observations are excluded",
		"[CLEARED] WARN (synthetic)",
	)
}

// At most MaxMessagesPerMinute over a sliding minute. The rest are held and
// posted as one summary, ahead of later transitions, once the window allows.
func TestSlackAlertNotifierRateLimitsWithASummary(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	settings := harness.settings(webhookUrl)
	settings.MaxMessagesPerMinute = 3
	notifier := NewSlackAlertNotifier(settings)

	burst := Alerts{}
	for i := 1; i <= 5; i++ {
		burst = append(burst, slackTestAlert(fmt.Sprintf("pg-%d", i), "", SeverityWarn))
	}
	notifier.Handle(slackTestSignal, burst)
	notifier.Handle(slackTestSignal, burst)
	harness.advance(30 * time.Second)
	// still inside the minute of the first three posts
	notifier.Handle(slackTestSignal, append(append(Alerts{}, burst...), slackTestAlert("pg-6", "", SeverityPage)))
	notifier.flushSummary()
	harness.advance(31 * time.Second)
	// the window has room again: the periodic flush posts the summary
	notifier.flushSummary()
	harness.advance(time.Second)
	notifier.Handle(slackTestSignal, burst)
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-1",
		"[WARN] raised (synthetic): Active client backends on pg-2",
		"[WARN] raised (synthetic): Active client backends on pg-3",
		"[RATE LIMIT] (synthetic) 3 alert transition(s) were not posted one by one (at most 3 messages per minute):",
	)
	for _, want := range []string{
		"- [WARN] raised: pg/active-pileup · active-pileup · pg-4",
		"- [WARN] raised: pg/active-pileup · active-pileup · pg-5",
		"- [PAGE] raised: pg/active-pileup · active-pileup · pg-6",
	} {
		if !strings.Contains(texts[3], want) {
			t.Errorf("summary lacks %q: %q", want, texts[3])
		}
	}
}

// Transitions still held at shutdown are posted as a final summary.
func TestSlackAlertNotifierPostsHeldTransitionsAtClose(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	settings := harness.settings(webhookUrl)
	settings.MaxMessagesPerMinute = 1
	notifier := NewSlackAlertNotifier(settings)

	notifier.Handle(slackTestSignal, Alerts{slackTestAlert("pg-1", "", SeverityWarn), slackTestAlert("pg-2", "", SeverityWarn)})
	closeSlackNotifier(t, notifier)
	// closed: later batches are ignored
	notifier.Handle(slackTestSignal, Alerts{slackTestAlert("pg-3", "", SeverityWarn)})

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-1",
		"[RATE LIMIT] (synthetic) 1 alert transition(s)",
	)
	if !strings.Contains(texts[1], "pg-2") {
		t.Fatalf("final summary = %q", texts[1])
	}
}

// Without a usable webhook the notifier logs one line, never posts, and its
// methods do nothing. The line never quotes the configured value.
func TestSlackAlertNotifierIsDisabledWithoutAUsableWebhook(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	for _, c := range []struct {
		webhookUrl string
		reason     string
	}{
		{webhookUrl: "", reason: "alerts.slack_webhook_url is empty"},
		{webhookUrl: " \n", reason: "alerts.slack_webhook_url is empty"},
		{webhookUrl: "http://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/" + syntheticSlackSecret, reason: "is not an absolute https URL"},
		{webhookUrl: "https://user:" + syntheticSlackSecret + "@hooks.slack.example/services/x", reason: "is not an absolute https URL"},
		{webhookUrl: "%zz" + syntheticSlackSecret, reason: "is not an absolute https URL"},
	} {
		harness := newSlackTestHarness()
		notifier := NewSlackAlertNotifier(harness.settings(c.webhookUrl))
		notifier.Handle(slackTestSignal, Alerts{slackTestAlert("pg-1", "", SeverityPage)})
		notifier.Handle(slackTestSignal, Alerts{})
		closeSlackNotifier(t, notifier)
		logs := harness.logLines()
		if len(logs) != 1 || !strings.Contains(logs[0], "Slack alert notifications disabled") || !strings.Contains(logs[0], c.reason) {
			t.Errorf("webhook of length %d: logs = %q", len(c.webhookUrl), logs)
		}
		if strings.Contains(strings.Join(logs, "\n"), syntheticSlackSecret) {
			t.Errorf("disabled notice quotes the webhook: %q", logs)
		}
	}
	// the enabled path logs one line too, without the webhook
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	closeSlackNotifier(t, notifier)
	if logs := harness.logLines(); len(logs) != 1 || !strings.Contains(logs[0], "enabled") || strings.Contains(logs[0], webhookUrl) {
		t.Fatalf("enabled logs = %q", logs)
	}
	if webhook.requestCount() != 0 {
		t.Fatalf("disabled notifiers posted %d times", webhook.requestCount())
	}
}

// Every failure path logs a fixed class: net/http errors embed the request
// URL, and a response body could echo it. A failure run logs once, and the
// next success logs the recovery.
func TestSlackAlertNotifierNeverLogsTheWebhookUrl(t *testing.T) {
	alerts := Alerts{slackTestAlert("pg-1", "", SeverityPage)}
	run := func(t *testing.T, webhookUrl string, prepare func(*SlackAlertNotifierSettings)) []string {
		harness := newSlackTestHarness()
		settings := harness.settings(webhookUrl)
		if prepare != nil {
			prepare(&settings)
		}
		notifier := NewSlackAlertNotifier(settings)
		notifier.Handle(slackTestSignal, alerts)
		notifier.Handle(slackTestSignal, Alerts{})
		notifier.Handle(slackTestSignal, Alerts{})
		closeSlackNotifier(t, notifier)
		logs := harness.logLines()
		joined := strings.Join(logs, "\n")
		if strings.Contains(joined, webhookUrl) || strings.Contains(joined, syntheticSlackSecret) || strings.Contains(joined, "/services/") {
			t.Fatalf("logs quote the webhook: %q", logs)
		}
		return logs
	}
	requireFailure := func(t *testing.T, logs []string, class string) {
		t.Helper()
		failures := 0
		for _, line := range logs {
			if strings.Contains(line, "Slack alert post failed") {
				failures++
				if !strings.Contains(line, "("+class) {
					t.Errorf("failure line = %q, want class %q", line, class)
				}
			}
		}
		if failures != 1 {
			t.Errorf("failure lines = %d in %q", failures, logs)
		}
	}

	// Slack's reason token is kept; any other body is not
	webhook, webhookUrl := newFakeSlackWebhook(t)
	webhook.respond(http.StatusNotFound, "no_service")
	requireFailure(t, run(t, webhookUrl, nil), "http-404 no_service")
	webhook.respond(http.StatusInternalServerError, "rejected "+webhookUrl)
	requireFailure(t, run(t, webhookUrl, nil), "http-500")

	// a redirect is not followed, so alert text cannot reach another host
	redirectTarget, redirectTargetUrl := newFakeSlackWebhook(t)
	redirector := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, redirectTargetUrl, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	requireFailure(t, run(t, redirector.URL+"/services/T0SYNTHETIC/B0SYNTHETIC/"+syntheticSlackSecret, nil), "http-307")
	if redirectTarget.requestCount() != 0 {
		t.Fatal("the notifier followed a redirect")
	}

	// connection refused: the transport error text carries the URL
	refused := httptest.NewServer(http.NotFoundHandler())
	refusedUrl := refused.URL + "/services/T0SYNTHETIC/B0SYNTHETIC/" + syntheticSlackSecret
	refused.Close()
	requireFailure(t, run(t, refusedUrl, nil), "network")

	// a webhook that never answers times out
	held, heldUrl := newFakeSlackWebhook(t)
	held.stateLock.Lock()
	held.hold = true
	held.stateLock.Unlock()
	requireFailure(t, run(t, heldUrl, func(settings *SlackAlertNotifierSettings) {
		settings.PostTimeout = 50 * time.Millisecond
	}), "timeout")

	// failure then success: one failure line and one recovery line
	recovering, recoveringUrl := newFakeSlackWebhook(t)
	recovering.respond(http.StatusServiceUnavailable, "")
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(recoveringUrl))
	notifier.Handle(slackTestSignal, alerts)
	// barrier: the raise has been answered 503 before the webhook recovers
	select {
	case <-recovering.answered:
	case <-time.After(30 * time.Second):
		t.Fatal("the raise was never posted")
	}
	recovering.respond(0, "")
	notifier.Handle(slackTestSignal, Alerts{})
	notifier.Handle(slackTestSignal, Alerts{})
	closeSlackNotifier(t, notifier)
	logs := harness.logLines()
	if len(logs) != 3 || !strings.Contains(logs[1], "(http-503)") || !strings.Contains(logs[2], "recovered after 1 failed post(s)") {
		t.Fatalf("recovery logs = %q", logs)
	}
	if strings.Contains(strings.Join(logs, "\n"), syntheticSlackSecret) {
		t.Fatalf("recovery logs quote the webhook: %q", logs)
	}
}

// Alert text cannot mention a channel or render a link.
func TestSlackAlertMessagesEscapeSlackControlSequences(t *testing.T) {
	alert := slackTestAlert("pg-1", "", SeverityPage)
	alert.Symptom = "<!channel> & <https://phishing.example|click> on pg-1"
	text := slackTransitionText("synthetic", slackTransition{kind: slackRaised, alert: alert}, 2)
	if strings.Contains(text, "<!channel>") || strings.Contains(text, "<https://") ||
		!strings.Contains(text, "&lt;!channel&gt; &amp; &lt;https://phishing.example|click&gt;") {
		t.Fatalf("text = %q", text)
	}
	long := slackTestAlert(strings.Repeat("x", 400), "", SeverityWarn)
	if text := slackTransitionText("synthetic", slackTransition{kind: slackRaised, alert: long}, 2); 900 < len(text) {
		t.Fatalf("long alert produced a %d-byte message", len(text))
	}
}

// Every alert visibilityAlert can produce marks its batch incomplete, so a
// failed run never clears the signal's incidents.
func TestEveryVisibilityAlertMarksItsBatchIncomplete(t *testing.T) {
	settings := syntheticSettings(nil)
	pgSample := selectionSignal{number: "2.30", key: "pg-query-sample", id: "pg/query-sample"}
	for _, c := range []struct {
		signal Signal
		err    error
	}{
		{signal: slackTestSignal, err: errors.New("synthetic command timeout")},
		{signal: slackTestSignal, err: errors.New("host-a.example: kex_exchange_identification: read: Connection reset by peer")},
		{signal: slackTestSignal, err: newProviderVisibilityError(providerAuthenticationClass, "synthetic provider refusal")},
		{signal: pgSample, err: errors.New("synthetic sample failure")},
	} {
		alert := visibilityAlert(settings, c.signal, c.err)
		if !isVisibilityAlert(alert) {
			t.Errorf("visibility alert %s/%s does not mark its batch incomplete", alert.SignalID, alert.Class)
		}
	}
	if isVisibilityAlert(slackTestAlert("pg-1", "", SeverityPage)) {
		t.Fatal("an ordinary alert marked its batch incomplete")
	}
}

// The monitor.yml decode carries the webhook into the settings, and a
// malformed document is reported by line, never by quoting its values.
func TestMonitorYamlDecodesTheSlackWebhookWithoutQuotingIt(t *testing.T) {
	y, err := decodeMonitorYaml([]byte("ssh:\n  user: monitor\nalerts:\n  slack_webhook_url: https://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/" + syntheticSlackSecret + "\n"))
	if err != nil || y.Ssh.User != "monitor" || !strings.HasSuffix(y.Alerts.SlackWebhookUrl, syntheticSlackSecret) {
		t.Fatalf("decoded = %+v, err = %v", y.Alerts, err)
	}
	if y, err := decodeMonitorYaml([]byte("ssh:\n  user: monitor\n")); err != nil || y.Alerts.SlackWebhookUrl != "" {
		t.Fatalf("absent alerts block = %+v, err = %v", y.Alerts, err)
	}
	_, err = decodeMonitorYaml([]byte("ssh:\n  user: monitor\nalerts: https://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/" + syntheticSlackSecret + "\n"))
	if err == nil || strings.Contains(err.Error(), syntheticSlackSecret) || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("malformed document error = %v", err)
	}
}

// A changed webhook makes a running watcher's settings stale, so the
// settings-freshness signal asks for a new watcher instead of posting to a
// rotated-out webhook.
func TestSettingsGenerationSeesAChangedSlackWebhook(t *testing.T) {
	startup := syntheticSettings(nil)
	startup.AlertNotifications = AlertNotificationSettings{SlackWebhookUrl: "https://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/old"}
	current := startup
	check := NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
	if same, err := check(context.Background(), startup); err != nil || !same {
		t.Fatalf("unchanged webhook: same = %t, err = %v", same, err)
	}
	current.AlertNotifications = AlertNotificationSettings{SlackWebhookUrl: "https://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/new"}
	if same, err := check(context.Background(), startup); err != nil || same {
		t.Fatalf("changed webhook: same = %t, err = %v", same, err)
	}
}

// The production loader carries vault monitor.yml alerts.slack_webhook_url
// into the settings the CLI hands the notifier.
func TestLoadSignalSettingsCarriesTheSlackWebhook(t *testing.T) {
	t.Setenv("WARP_ENV", "synthetic")
	t.Setenv("WARP_HOME", t.TempDir())
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
	popMonitor := server.Vault.PushSimpleResource("monitor.yml", []byte("ssh:\n  user: monitor\nhosts:\n  - name: pg-1\n    overlay_ip: 192.0.2.10\n    roles: [pg-primary]\nalerts:\n  slack_webhook_url: \" https://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/"+syntheticSlackSecret+" \"\n"))
	defer popMonitor()
	popServices := server.Vault.PushSimpleResource("services.yml", []byte("domain: example.test\nversions:\n  - services:\n      api:\n        blocks:\n          - g1: 100\n"))
	defer popServices()

	settings, err := loadSignalSettingsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if settings.AlertNotifications.SlackWebhookUrl != "https://hooks.slack.example/services/T0SYNTHETIC/B0SYNTHETIC/"+syntheticSlackSecret {
		t.Fatal("loaded settings do not carry the trimmed webhook")
	}
}
