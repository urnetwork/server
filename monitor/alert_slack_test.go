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

// One scripted answer of the fake webhook. A zero status answers 200 "ok".
type fakeSlackResponse struct {
	status     int
	retryAfter string
	body       string
	// never answer: the client gives up at its post timeout
	hold bool
}

// A recording incoming webhook. Each accepted post's text is kept in order.
type fakeSlackWebhook struct {
	stateLock sync.Mutex
	texts     []string
	requests  int
	// answers for the next requests, in order, ahead of status and body
	script []fakeSlackResponse
	// status and body for every response; zero status answers 200 "ok"
	status int
	body   string
	// when set, the handler holds every request until it is canceled
	hold bool
	// when set, each request waits for a receive from it, or for it to close,
	// before it is answered
	release chan struct{}
	// receives once per request, when it arrives
	arrived chan struct{}
	// receives once per answered request, after the response is written
	answered chan struct{}
}

func newFakeSlackWebhook(t *testing.T) (*fakeSlackWebhook, string) {
	webhook := &fakeSlackWebhook{arrived: make(chan struct{}, 64), answered: make(chan struct{}, 64)}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		webhook.stateLock.Lock()
		webhook.requests++
		hold, status, body, release := webhook.hold, webhook.status, webhook.body, webhook.release
		retryAfter := ""
		if 0 < len(webhook.script) {
			next := webhook.script[0]
			webhook.script = webhook.script[1:]
			hold, status, body, retryAfter = next.hold, next.status, next.body, next.retryAfter
		}
		webhook.stateLock.Unlock()
		// the server notices a client disconnect only once the body is read
		requestBody, readErr := io.ReadAll(request.Body)
		select {
		case webhook.arrived <- struct{}{}:
		default:
		}
		if hold {
			<-request.Context().Done()
			return
		}
		if release != nil {
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
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
			readErr != nil || json.Unmarshal(requestBody, &payload) != nil || payload.UnfurlLinks {
			response.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(response, "invalid_payload")
			return
		}
		if status != 0 {
			if retryAfter != "" {
				response.Header().Set("Retry-After", retryAfter)
			}
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

// Queues answers for the next requests.
func (self *fakeSlackWebhook) answerNext(responses ...fakeSlackResponse) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.script = append(self.script, responses...)
}

// Makes later requests wait for the returned channel; closing it releases
// them all.
func (self *fakeSlackWebhook) holdUntilReleased() chan struct{} {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.release = make(chan struct{})
	return self.release
}

// Waits for count receives from a webhook barrier channel.
func awaitSlackWebhook(t *testing.T, barrier chan struct{}, count int, what string) {
	t.Helper()
	for i := 0; i < count; i++ {
		select {
		case <-barrier:
		case <-time.After(30 * time.Second):
			t.Fatalf("timed out waiting for %s %d of %d", what, i+1, count)
		}
	}
}

// A manually advanced clock, a log sink, and a retry wait that returns at once
// and records the delay it was asked for, shared by the notifier tests.
type slackTestHarness struct {
	stateLock   sync.Mutex
	now         time.Time
	logs        []string
	retryDelays []time.Duration
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

func (self *slackTestHarness) retryWait(ctx context.Context, delay time.Duration) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.retryDelays = append(self.retryDelays, delay)
	return ctx.Err() == nil
}

func (self *slackTestHarness) retryDelayLog() []time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]time.Duration(nil), self.retryDelays...)
}

// Settings that never post or flush on a wall-clock schedule: the only
// summary flushes are explicit, retries do not wait, and Close drains the
// queue.
func (self *slackTestHarness) settings(webhookUrl string) SlackAlertNotifierSettings {
	return SlackAlertNotifierSettings{
		WebhookUrl:      webhookUrl,
		Environment:     "synthetic",
		PostInterval:    0,
		PostTimeout:     5 * time.Second,
		SummaryInterval: time.Hour,
		Logf:            self.Logf,
		Now:             self.Now,
		retryWait:       self.retryWait,
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
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: alerts})
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
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: alerts})
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
// The executions are what the run loop hands over: the gate holds the first
// failed run's visibility alert below its Sustain of 2, so that run's gated
// alerts are empty.
func TestSlackAlertNotifierKeepsIncidentsThroughIncompleteRuns(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))

	cannotObserve := visibilityAlert(syntheticSettings(nil), slackTestSignal, errors.New("synthetic ssh timeout"))
	if cannotObserve.Sustain != 2 {
		t.Fatalf("visibility alert Sustain = %d, want 2", cannotObserve.Sustain)
	}
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityPage)}})
	// three runs that could not observe: an early clear of pg-1 would
	// appear here, ahead of the visibility clear
	notifier.Handle(slackTestSignal, SignalExecution{HeldAlerts: Alerts{cannotObserve}, Failed: true})
	for i := 0; i < 2; i++ {
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{cannotObserve}, Failed: true})
	}
	notifier.Handle(slackTestSignal, SignalExecution{})
	notifier.Handle(slackTestSignal, SignalExecution{})
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
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{shared(slackTestSignal)}})
	notifier.Handle(otherSignal, SignalExecution{Alerts: Alerts{shared(otherSignal)}})
	// the first signal stops reporting: an early clear would be followed by a
	// second raise from the other signal
	for i := 0; i < 3; i++ {
		notifier.Handle(slackTestSignal, SignalExecution{})
		notifier.Handle(otherSignal, SignalExecution{Alerts: Alerts{shared(otherSignal)}})
	}
	notifier.Handle(otherSignal, SignalExecution{})
	notifier.Handle(otherSignal, SignalExecution{})
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
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: burst})
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: burst})
	harness.advance(30 * time.Second)
	// still inside the minute of the first three posts
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: append(append(Alerts{}, burst...), slackTestAlert("pg-6", "", SeverityPage))})
	notifier.flushSummary()
	harness.advance(31 * time.Second)
	// the window has room again: the periodic flush posts the summary
	notifier.flushSummary()
	harness.advance(time.Second)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: burst})
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-1",
		"[WARN] raised (synthetic): Active client backends on pg-2",
		"[WARN] raised (synthetic): Active client backends on pg-3",
		"[RATE LIMIT] (synthetic) 3 alert transition(s) were not posted one by one (at most 3 messages per minute, or the sender was backlogged):",
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

// A summary the full queue refuses keeps its held transitions and counts, so
// the next summary still names them.
func TestSlackAlertNotifierKeepsTheSummaryWhenTheQueueIsFull(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	release := webhook.holdUntilReleased()
	harness := newSlackTestHarness()
	settings := harness.settings(webhookUrl)
	settings.MaxMessagesPerMinute = 2
	settings.QueueSize = 1
	notifier := NewSlackAlertNotifier(settings)

	active := Alerts{slackTestAlert("pg-1", "", SeverityWarn)}
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	// the sender holds pg-1 at the webhook, so the queue is empty again
	awaitSlackWebhook(t, webhook.arrived, 1, "request")
	// pg-2 fills the one-message queue, and pg-3 is over the cap
	active = append(active, slackTestAlert("pg-2", "", SeverityWarn))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	active = append(active, slackTestAlert("pg-3", "", SeverityWarn))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	harness.advance(61 * time.Second)
	// the cap allows the summary again, but the full queue refuses it
	notifier.flushSummary()
	close(release)
	awaitSlackWebhook(t, webhook.answered, 2, "answer")
	notifier.flushSummary()
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-1",
		"[WARN] raised (synthetic): Active client backends on pg-2",
		"[RATE LIMIT] (synthetic) 1 alert transition(s) were not posted one by one",
	)
	if !strings.Contains(texts[2], "- [WARN] raised: pg/active-pileup · active-pileup · pg-3") || strings.Contains(texts[2], "dropped") {
		t.Fatalf("summary = %q", texts[2])
	}
}

// Transitions still held at shutdown are posted as a final summary.
func TestSlackAlertNotifierPostsHeldTransitionsAtClose(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	settings := harness.settings(webhookUrl)
	settings.MaxMessagesPerMinute = 1
	notifier := NewSlackAlertNotifier(settings)

	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn), slackTestAlert("pg-2", "", SeverityWarn)}})
	closeSlackNotifier(t, notifier)
	// closed: later batches are ignored
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-3", "", SeverityWarn)}})

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
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityPage)}})
		notifier.Handle(slackTestSignal, SignalExecution{})
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
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: alerts})
		notifier.Handle(slackTestSignal, SignalExecution{})
		notifier.Handle(slackTestSignal, SignalExecution{})
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
	recovering.answerNext(fakeSlackResponse{status: http.StatusServiceUnavailable})
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(recoveringUrl))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: alerts})
	notifier.Handle(slackTestSignal, SignalExecution{})
	notifier.Handle(slackTestSignal, SignalExecution{})
	closeSlackNotifier(t, notifier)
	logs := harness.logLines()
	if len(logs) != 3 || !strings.Contains(logs[1], "(http-503)") || !strings.Contains(logs[2], "recovered after 1 failed post(s)") {
		t.Fatalf("recovery logs = %q", logs)
	}
	if strings.Contains(strings.Join(logs, "\n"), syntheticSlackSecret) {
		t.Fatalf("recovery logs quote the webhook: %q", logs)
	}
}

// A 429 or a 503 means Slack did not take the post, so it is retried once,
// after the Retry-After Slack sends, in seconds or as an HTTP date,
// RetryDelay when it sends none, and at most MaxRetryDelay.
func TestSlackAlertNotifierRetriesARateLimitOrUnavailableOnce(t *testing.T) {
	for _, c := range []struct {
		first     fakeSlackResponse
		wantDelay time.Duration
	}{
		{first: fakeSlackResponse{status: http.StatusServiceUnavailable}, wantDelay: time.Second},
		{first: fakeSlackResponse{status: http.StatusServiceUnavailable, retryAfter: "3"}, wantDelay: 3 * time.Second},
		{first: fakeSlackResponse{status: http.StatusTooManyRequests, retryAfter: "7"}, wantDelay: 7 * time.Second},
		{first: fakeSlackResponse{status: http.StatusTooManyRequests, retryAfter: "120"}, wantDelay: 30 * time.Second},
		{first: fakeSlackResponse{status: http.StatusTooManyRequests, retryAfter: "Wed, 14 Oct 2026 18:00:12 GMT"}, wantDelay: 12 * time.Second},
		{first: fakeSlackResponse{status: http.StatusServiceUnavailable, retryAfter: "soon"}, wantDelay: time.Second},
	} {
		webhook, webhookUrl := newFakeSlackWebhook(t)
		webhook.answerNext(c.first)
		harness := newSlackTestHarness()
		notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn)}})
		closeSlackNotifier(t, notifier)

		texts := webhook.posted()
		delays := harness.retryDelayLog()
		if len(texts) != 1 || !strings.HasPrefix(texts[0], "[WARN] raised (synthetic)") || webhook.requestCount() != 2 ||
			len(delays) != 1 || delays[0] != c.wantDelay {
			t.Errorf("first answer %+v: posted %q in %d requests after waits %v, want one raise in 2 requests after %v",
				c.first, texts, webhook.requestCount(), delays, c.wantDelay)
		}
	}
}

// Other failures are not retried: the message is undelivered at once, and the
// report at close names it. A timeout or another 5xx may come after Slack
// took the post, so a retry could post it twice.
func TestSlackAlertNotifierDoesNotRetryOtherFailures(t *testing.T) {
	for _, first := range []fakeSlackResponse{
		{status: http.StatusNotFound, body: "no_service"},
		{status: http.StatusBadRequest, body: "invalid_payload"},
		{status: http.StatusForbidden},
		{status: http.StatusInternalServerError, retryAfter: "3"},
		{status: http.StatusBadGateway},
		{status: http.StatusGatewayTimeout},
		{hold: true},
	} {
		webhook, webhookUrl := newFakeSlackWebhook(t)
		webhook.answerNext(first)
		harness := newSlackTestHarness()
		settings := harness.settings(webhookUrl)
		if first.hold {
			settings.PostTimeout = 500 * time.Millisecond
		}
		notifier := NewSlackAlertNotifier(settings)
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn)}})
		closeSlackNotifier(t, notifier)

		texts := webhook.posted()
		if delays := harness.retryDelayLog(); len(delays) != 0 || webhook.requestCount() != 2 || len(texts) != 1 ||
			!strings.HasPrefix(texts[0], "[UNDELIVERED] (synthetic) 1 message(s) queued between 2026-10-14T18:00:00Z and 2026-10-14T18:00:00Z failed to post:\n- [WARN] raised (synthetic): Active client backends on pg-1") {
			t.Errorf("first answer %+v: posted %q in %d requests after waits %v", first, texts, webhook.requestCount(), delays)
		}
	}

	// a refused connection is not retried either
	refused := httptest.NewServer(http.NotFoundHandler())
	refusedUrl := refused.URL + "/services/T0SYNTHETIC/B0SYNTHETIC/" + syntheticSlackSecret
	refused.Close()
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(refusedUrl))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn)}})
	closeSlackNotifier(t, notifier)
	if delays := harness.retryDelayLog(); len(delays) != 0 {
		t.Fatalf("refused connection waited %v to retry", delays)
	}
}

// Retry-After is seconds of any length or an HTTP date; the wait is at most
// MaxRetryDelay, and anything else waits RetryDelay.
func TestSlackAlertNotifierRetryAfterEdges(t *testing.T) {
	harness := newSlackTestHarness()
	notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{Now: harness.Now}.withDefaults()}
	for _, c := range []struct {
		retryAfter string
		wantDelay  time.Duration
	}{
		{retryAfter: "0", wantDelay: 0},
		{retryAfter: "7", wantDelay: 7 * time.Second},
		{retryAfter: "30", wantDelay: 30 * time.Second},
		{retryAfter: "31", wantDelay: 30 * time.Second},
		{retryAfter: "  45 ", wantDelay: 30 * time.Second},
		{retryAfter: "9223372036854775807", wantDelay: 30 * time.Second},
		{retryAfter: "99999999999999999999", wantDelay: 30 * time.Second},
		{retryAfter: "-1", wantDelay: time.Second},
		{retryAfter: "1.5", wantDelay: time.Second},
		{retryAfter: "", wantDelay: time.Second},
		{retryAfter: "Wed, 14 Oct 2026 17:59:00 GMT", wantDelay: 0},
		{retryAfter: "Wed, 14 Oct 2026 18:00:12 GMT", wantDelay: 12 * time.Second},
		{retryAfter: "Fri, 01 Jan 2100 00:00:00 GMT", wantDelay: 30 * time.Second},
	} {
		if delay := notifier.retryDelay(c.retryAfter); delay != c.wantDelay {
			t.Errorf("Retry-After %q: wait %s, want %s", c.retryAfter, delay, c.wantDelay)
		}
	}
}

// A gateway can answer 504 after Slack took the post: the message reaches
// the channel once, and the report says it may not have.
func TestSlackAlertNotifierDoesNotRepostAMessageSlackMayHaveTaken(t *testing.T) {
	var textsLock sync.Mutex
	received := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		textsLock.Lock()
		received = append(received, payload.Text)
		first := len(received) == 1
		textsLock.Unlock()
		if first {
			// delivered, then the gateway timed out
			response.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		_, _ = io.WriteString(response, "ok")
	}))
	defer server.Close()
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(server.URL + "/services/T0SYNTHETIC/B0SYNTHETIC/" + syntheticSlackSecret))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityPage)}})
	closeSlackNotifier(t, notifier)

	textsLock.Lock()
	defer textsLock.Unlock()
	if len(received) != 2 || !strings.HasPrefix(received[0], "[PAGE] raised") || !strings.HasPrefix(received[1], "[UNDELIVERED]") {
		t.Fatalf("received:\n%s", strings.Join(received, "\n---\n"))
	}
}

// Messages that still fail after their retry are reported once Slack takes a
// post again: one report with their count, when they were queued, and their
// first lines.
func TestSlackAlertNotifierReportsUndeliveredMessagesAfterRecovery(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	// pg-1 and pg-2 each fail twice
	for i := 0; i < 4; i++ {
		webhook.answerNext(fakeSlackResponse{status: http.StatusServiceUnavailable})
	}
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	active := Alerts{}
	for _, target := range []string{"pg-1", "pg-2", "pg-3"} {
		active = append(active, slackTestAlert(target, "", SeverityWarn))
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
		harness.advance(time.Minute)
	}
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-3",
		"[UNDELIVERED] (synthetic) 2 message(s) queued between 2026-10-14T18:00:00Z and 2026-10-14T18:01:00Z failed to post:\n"+
			"- [WARN] raised (synthetic): Active client backends on pg-1 exceed the band\n"+
			"- [WARN] raised (synthetic): Active client backends on pg-2 exceed the band\n"+
			"The monitor's alert output carries every alert.",
	)
	if webhook.requestCount() != 6 {
		t.Fatalf("requests = %d, want 6", webhook.requestCount())
	}
}

// Without a later message, the periodic check posts the report itself once
// Slack recovers; while Slack still refuses it, the report waits a minute
// between attempts.
func TestSlackAlertNotifierProbesWithTheUndeliveredReport(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	// pg-1 and its retry fail, and so does the first report with its retry
	for i := 0; i < 4; i++ {
		webhook.answerNext(fakeSlackResponse{status: http.StatusServiceUnavailable})
	}
	harness := newSlackTestHarness()
	settings := harness.settings(webhookUrl)
	ticks := make(chan time.Time)
	settings.after = func(time.Duration) <-chan time.Time { return ticks }
	notifier := NewSlackAlertNotifier(settings)
	tick := func() {
		t.Helper()
		select {
		case ticks <- harness.Now():
		case <-time.After(30 * time.Second):
			t.Fatal("the sender never waited for its periodic check")
		}
	}

	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn)}})
	awaitSlackWebhook(t, webhook.answered, 2, "answer")
	// the sender takes a tick only after it recorded pg-1 as undelivered
	tick()
	awaitSlackWebhook(t, webhook.answered, 2, "answer")
	// a minute has not passed: these ticks post nothing, and the second one
	// is taken only after the first is handled
	tick()
	tick()
	if webhook.requestCount() != 4 {
		t.Fatalf("requests before the report interval = %d, want 4", webhook.requestCount())
	}
	harness.advance(time.Minute)
	tick()
	awaitSlackWebhook(t, webhook.answered, 1, "answer")
	closeSlackNotifier(t, notifier)

	requireSlackTexts(t, webhook.posted(),
		"[UNDELIVERED] (synthetic) 1 message(s) queued between 2026-10-14T18:00:00Z and 2026-10-14T18:00:00Z failed to post:\n- [WARN] raised (synthetic): Active client backends on pg-1",
	)
}

// Handle returns while the sender waits to retry. Executions meanwhile fill
// the bounded queue, and what does not fit is held for the summary.
func TestSlackAlertNotifierHandleNeverWaitsForARetry(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	webhook.answerNext(fakeSlackResponse{status: http.StatusServiceUnavailable})
	harness := newSlackTestHarness()
	settings := harness.settings(webhookUrl)
	settings.QueueSize = 2
	settings.MaxMessagesPerMinute = 100
	retrying := make(chan struct{}, 1)
	release := make(chan struct{})
	settings.retryWait = func(ctx context.Context, _ time.Duration) bool {
		retrying <- struct{}{}
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	}
	notifier := NewSlackAlertNotifier(settings)

	active := Alerts{slackTestAlert("pg-1", "", SeverityWarn)}
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	awaitSlackWebhook(t, retrying, 1, "retry wait")
	handled := make(chan struct{})
	go func() {
		defer close(handled)
		for _, target := range []string{"pg-2", "pg-3", "pg-4", "pg-5", "pg-6"} {
			active = append(active, slackTestAlert(target, "", SeverityWarn))
			notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
		}
	}()
	select {
	case <-handled:
	case <-time.After(30 * time.Second):
		t.Fatal("Handle waited for the sender's retry")
	}
	close(release)
	// the 503, the retry, pg-2 and pg-3
	awaitSlackWebhook(t, webhook.answered, 4, "answer")
	notifier.flushSummary()
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-1",
		"[WARN] raised (synthetic): Active client backends on pg-2",
		"[WARN] raised (synthetic): Active client backends on pg-3",
		"[RATE LIMIT] (synthetic) 3 alert transition(s) were not posted one by one",
	)
	for _, target := range []string{"pg-4", "pg-5", "pg-6"} {
		if !strings.Contains(texts[3], "- [WARN] raised: pg/active-pileup · active-pileup · "+target) {
			t.Errorf("summary does not name %s: %q", target, texts[3])
		}
	}
}

// A transition the full queue refuses is held like one over the cap: the
// summary names it, an incident with a page first, instead of counting a drop.
func TestSlackAlertNotifierNamesTransitionsTheFullQueueRefused(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	release := webhook.holdUntilReleased()
	harness := newSlackTestHarness()
	settings := harness.settings(webhookUrl)
	settings.QueueSize = 1
	settings.MaxMessagesPerMinute = 100
	notifier := NewSlackAlertNotifier(settings)

	active := Alerts{slackTestAlert("pg-1", "", SeverityWarn)}
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	// the sender holds pg-1 at the webhook, and pg-2 fills the queue
	awaitSlackWebhook(t, webhook.arrived, 1, "request")
	active = append(active, slackTestAlert("pg-2", "", SeverityWarn))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	// the full queue refuses a warning, then a page
	active = append(active, slackTestAlert("pg-3", "", SeverityWarn))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	active = append(active, slackTestAlert("pg-4", "", SeverityPage))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	close(release)
	awaitSlackWebhook(t, webhook.answered, 2, "answer")
	notifier.flushSummary()
	closeSlackNotifier(t, notifier)

	requireSlackTexts(t, webhook.posted(),
		"[WARN] raised (synthetic): Active client backends on pg-1",
		"[WARN] raised (synthetic): Active client backends on pg-2",
		"[RATE LIMIT] (synthetic) 2 alert transition(s) were not posted one by one (at most 100 messages per minute, or the sender was backlogged):\n"+
			"- [PAGE] raised: pg/active-pileup · active-pileup · pg-4\n"+
			"- [WARN] raised: pg/active-pileup · active-pileup · pg-3\n",
	)
}

// A summary shows 20 lines; an incident with a page leads them, so a page held
// after many warnings is still named.
func TestSlackAlertNotifierListsHeldPagesFirst(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	settings := harness.settings(webhookUrl)
	settings.MaxMessagesPerMinute = 1
	notifier := NewSlackAlertNotifier(settings)

	active := Alerts{}
	for i := 1; i <= 26; i++ {
		active = append(active, slackTestAlert(fmt.Sprintf("pg-%02d", i), "", SeverityWarn))
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	}
	active = append(active, slackTestAlert("pg-27", "", SeverityPage))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	harness.advance(61 * time.Second)
	notifier.flushSummary()
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): Active client backends on pg-01",
		"[RATE LIMIT] (synthetic) 26 alert transition(s) were not posted one by one (at most 1 messages per minute, or the sender was backlogged):\n"+
			"- [PAGE] raised: pg/active-pileup · active-pileup · pg-27\n"+
			"- [WARN] raised: pg/active-pileup · active-pileup · pg-02\n",
	)
	if !strings.Contains(texts[1], "\n- and 6 more\n") {
		t.Fatalf("summary = %q", texts[1])
	}
}

// Close's deadline abandons the post in flight and what is still queued, and
// a held summary the full queue refuses at close is lost too. Close counts
// them all in one stderr line.
func TestSlackAlertNotifierLogsMessagesAbandonedAtClose(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	webhook.holdUntilReleased()
	harness := newSlackTestHarness()
	settings := harness.settings(webhookUrl)
	settings.MaxMessagesPerMinute = 4
	settings.QueueSize = 3
	notifier := NewSlackAlertNotifier(settings)

	active := Alerts{slackTestAlert("pg-1", "", SeverityWarn)}
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	// the sender holds pg-1 at the webhook
	awaitSlackWebhook(t, webhook.arrived, 1, "request")
	// pg-2 to pg-4 fill the queue, and pg-5 is over the cap
	for _, target := range []string{"pg-2", "pg-3", "pg-4", "pg-5"} {
		active = append(active, slackTestAlert(target, "", SeverityWarn))
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: active})
	}
	deadline, cancel := context.WithCancel(context.Background())
	cancel()
	notifier.Close(deadline)

	logs := harness.logLines()
	if len(logs) != 2 || logs[1] != "monitor: Slack alert notifier abandoned 5 unposted message(s) at close" {
		t.Fatalf("logs = %q", logs)
	}
	if posted := webhook.posted(); len(posted) != 0 {
		t.Fatalf("posted %q", posted)
	}
}

// Undelivered messages whose report also fails at close are logged.
func TestSlackAlertNotifierLogsUnreportedUndeliveredMessagesAtClose(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	webhook.respond(http.StatusNotFound, "no_service")
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn)}})
	harness.advance(time.Minute)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn), slackTestAlert("pg-2", "", SeverityWarn)}})
	closeSlackNotifier(t, notifier)

	logs := harness.logLines()
	want := "monitor: 2 Slack alert message(s) queued between 2026-10-14T18:00:00Z and 2026-10-14T18:01:00Z failed to post and were never reported in Slack"
	if len(logs) != 3 || !strings.Contains(logs[1], "Slack alert post failed (http-404 no_service)") || logs[2] != want {
		t.Fatalf("logs = %q", logs)
	}
}

// The harness settings with the production start digest.
func (self *slackTestHarness) digestSettings(webhookUrl string) SlackAlertNotifierSettings {
	settings := self.settings(webhookUrl)
	settings.StartDigestHold = DefaultSlackAlertNotifierSettings().StartDigestHold
	return settings
}

var slackOtherTestSignal = selectionSignal{number: "3.1", key: "redis-memory", id: "redis/memory"}

// Whether the notifier holds a start digest it has not offered yet.
func (self *SlackAlertNotifier) startDigestPending() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return 0 < len(self.startDigest)
}

// A new watcher's raises from each signal's first Sustain complete runs are
// folded into one start digest, posted StartDigestHold after its first raise.
// A raise past a signal's start window posts on its own.
func TestSlackAlertNotifierFoldsRaisesAtStartIntoOneDigest(t *testing.T) {
	if defaults := DefaultSlackAlertNotifierSettings(); defaults.StartDigestHold != 2*time.Minute || defaults.StartWindow != 30*time.Minute {
		t.Fatalf("default start digest hold %s and start window %s, want 2m and 30m", defaults.StartDigestHold, defaults.StartWindow)
	}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.digestSettings(webhookUrl))

	pg1 := slackTestAlert("pg-1", "", SeverityWarn)
	slow := slackTestAlert("pg-3", "", SeverityWarn)
	slow.Sustain = 2
	redis := Alert{
		SignalNumber: "3.1", SignalKey: "redis-memory", SignalID: "redis/memory", SignalName: "Redis memory",
		Severity: SeverityWarn, Class: "node-mem-high", Target: "redis-1", Symptom: "redis-1 memory is high",
	}
	// first runs: the gate holds pg-3 below its Sustain of 2
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pg1}, HeldAlerts: Alerts{slow}})
	harness.advance(time.Minute)
	notifier.Handle(slackOtherTestSignal, SignalExecution{Alerts: Alerts{redis}})
	harness.advance(30 * time.Second)
	// pg-3's second run is still within its Sustain
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pg1, slow}})
	harness.advance(29 * time.Second)
	notifier.flushSummary()
	if !notifier.startDigestPending() {
		t.Fatal("the digest posted before its hold")
	}
	harness.advance(time.Second)
	notifier.flushSummary()
	// pg-4 first appears in the third run, past its Sustain of 1
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pg1, slow, slackTestAlert("pg-4", "", SeverityWarn)}})
	closeSlackNotifier(t, notifier)

	requireSlackTexts(t, webhook.posted(),
		"[ACTIVE AT START] (synthetic) 3 incident(s) reported by this watcher's first runs of their signals:\n"+
			"- [WARN] pg/active-pileup · active-pileup · pg-1\n"+
			"- [WARN] pg/active-pileup · active-pileup · pg-3\n"+
			"- [WARN] redis/memory · node-mem-high · redis-1\n"+
			"Alert state is per watcher, so these can repeat earlier announcements.",
		"[WARN] raised (synthetic): Active client backends on pg-4",
	)
}

// A page joining the digest posts it at once, and raises that keep arriving
// never hold a digest past StartDigestHold from its first raise.
func TestSlackAlertNotifierPostsADigestWithAPageAtOnce(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.digestSettings(webhookUrl))
	start := harness.Now()
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityPage)}})
	if notifier.startDigestPending() {
		t.Fatal("a digest with a page waited")
	}
	// another signal's first run raises a warning every 90 seconds
	firstPendingTime := time.Time{}
	for i := 0; i < 8; i++ {
		for step := 0; step < 18; step++ {
			harness.advance(5 * time.Second)
			notifier.flushSummary()
			if !notifier.startDigestPending() {
				firstPendingTime = time.Time{}
			} else if held := harness.Now().Sub(firstPendingTime); 2*time.Minute < held {
				t.Fatalf("a digest was held %s at %s", held, harness.Now().Sub(start))
			}
		}
		other := selectionSignal{number: fmt.Sprintf("9.%d", i), key: fmt.Sprintf("other-%d", i), id: fmt.Sprintf("other/%d", i)}
		alert := slackTestAlert(fmt.Sprintf("host-%d", i), "", SeverityWarn)
		alert.SignalID, alert.SignalKey = other.id, other.key
		if !notifier.startDigestPending() {
			firstPendingTime = harness.Now()
		}
		notifier.Handle(other, SignalExecution{Alerts: Alerts{alert}})
	}
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	if len(texts) < 2 || !strings.HasPrefix(texts[0], "[ACTIVE AT START] (synthetic) 1 incident(s)") || !strings.Contains(texts[0], "- [PAGE] pg/active-pileup · active-pileup · pg-1") {
		t.Fatalf("posts:\n%s", strings.Join(texts, "\n---\n"))
	}
}

// A signal that fails from its start has no start left when it first
// completes hours later: its PAGE posts on its own, at once.
func TestSlackAlertNotifierPostsALateFirstCompleteRunOnItsOwn(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.digestSettings(webhookUrl))
	cannotObserve := visibilityAlert(syntheticSettings(nil), slackTestSignal, errors.New("synthetic ssh timeout"))
	notifier.Handle(slackTestSignal, SignalExecution{HeldAlerts: Alerts{cannotObserve}, Failed: true})
	for i := 0; i < 12; i++ {
		harness.advance(15 * time.Minute)
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{cannotObserve}, Failed: true})
		notifier.flushSummary()
	}
	harness.advance(15 * time.Minute)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityPage)}})
	if notifier.startDigestPending() {
		t.Fatal("the 3h15m first complete run joined a start digest")
	}
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	if len(texts) == 0 || !strings.HasPrefix(texts[len(texts)-1], "[PAGE] raised (synthetic): Active client backends on pg-1") {
		t.Fatalf("posts:\n%s", strings.Join(texts, "\n---\n"))
	}
}

// An at-start incident escalated before the digest posts is announced at page
// in the digest, which then posts at once, without a separate escalation.
func TestSlackAlertNotifierFoldsAnEscalationIntoThePendingDigest(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.digestSettings(webhookUrl))

	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn)}})
	harness.advance(time.Minute)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityPage)}})
	if notifier.startDigestPending() {
		t.Fatal("the digest waited for its hold after an escalation")
	}
	harness.advance(2 * time.Minute)
	notifier.flushSummary()
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityPage)}})
	closeSlackNotifier(t, notifier)

	requireSlackTexts(t, webhook.posted(),
		"[ACTIVE AT START] (synthetic) 1 incident(s) reported by this watcher's first runs of their signals:\n"+
			"- [PAGE] pg/active-pileup · active-pileup · pg-1\n",
	)
}

// A clear of an at-start incident posts the pending digest first, so the
// channel never shows a clear ahead of its raise.
func TestSlackAlertNotifierPostsTheDigestBeforeAClearOfItsIncident(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.digestSettings(webhookUrl))

	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn), slackTestAlert("pg-2", "", SeverityWarn)}})
	for i := 0; i < 2; i++ {
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-2", "", SeverityWarn)}})
	}
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[ACTIVE AT START] (synthetic) 2 incident(s) reported by this watcher's first runs of their signals:\n"+
			"- [WARN] pg/active-pileup · active-pileup · pg-1\n"+
			"- [WARN] pg/active-pileup · active-pileup · pg-2\n",
		"[CLEARED] WARN (synthetic): PostgreSQL active backends\npg/active-pileup · active-pileup · pg-1",
	)
}

// A digest the full queue refuses holds the clear of its incident behind it:
// the clear waits in the summary instead of being dropped or posted first.
func TestSlackAlertNotifierHoldsAClearBehindARefusedDigest(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	release := webhook.holdUntilReleased()
	harness := newSlackTestHarness()
	settings := harness.digestSettings(webhookUrl)
	settings.QueueSize = 1
	notifier := NewSlackAlertNotifier(settings)
	redis := func(target string) Alert {
		return Alert{
			SignalNumber: "3.1", SignalKey: "redis-memory", SignalID: "redis/memory", SignalName: "Redis memory",
			Severity: SeverityWarn, Class: "node-mem-high", Target: target, Symptom: target + " memory is high",
		}
	}

	// past its start window, the other signal's raises post one by one
	notifier.Handle(slackOtherTestSignal, SignalExecution{})
	notifier.Handle(slackOtherTestSignal, SignalExecution{Alerts: Alerts{redis("redis-1")}})
	// the sender holds redis-1 at the webhook, and redis-2 fills the queue
	awaitSlackWebhook(t, webhook.arrived, 1, "request")
	notifier.Handle(slackOtherTestSignal, SignalExecution{Alerts: Alerts{redis("redis-1"), redis("redis-2")}})
	// pg-1 joins the digest and clears while the queue is full
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn)}})
	notifier.Handle(slackTestSignal, SignalExecution{})
	notifier.Handle(slackTestSignal, SignalExecution{})
	close(release)
	awaitSlackWebhook(t, webhook.answered, 2, "answer")
	// the one-message queue takes the digest, then the summary
	notifier.flushSummary()
	awaitSlackWebhook(t, webhook.answered, 1, "answer")
	notifier.flushSummary()
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): redis-1 memory is high",
		"[WARN] raised (synthetic): redis-2 memory is high",
		"[ACTIVE AT START] (synthetic) 1 incident(s) reported by this watcher's first runs of their signals:\n- [WARN] pg/active-pileup · active-pileup · pg-1\n",
		"[RATE LIMIT] (synthetic) 1 alert transition(s) were not posted one by one",
	)
	if !strings.Contains(texts[3], "- [CLEARED] WARN: pg/active-pileup · active-pileup · pg-1") {
		t.Fatalf("summary = %q", texts[3])
	}
}

// An incident cleared behind a refused digest and raised again at another
// signal's start keeps only its latest state: the digest names it once as
// active, and the held clear is not posted after it.
func TestSlackAlertNotifierKeepsTheLatestStateOfAnIncidentHeldTwice(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	release := webhook.holdUntilReleased()
	harness := newSlackTestHarness()
	settings := harness.digestSettings(webhookUrl)
	settings.QueueSize = 1
	notifier := NewSlackAlertNotifier(settings)
	third := selectionSignal{number: "4.1", key: "third", id: "third/probe"}
	thirdAlert := func(target string) Alert {
		return Alert{
			SignalNumber: "4.1", SignalKey: "third", SignalID: "third/probe", SignalName: "Third",
			Severity: SeverityWarn, Class: "third", Target: target, Symptom: target + " third",
		}
	}

	// past its start window, the third signal's raises post one by one; the
	// sender holds r-1 at the webhook, and r-2 fills the queue
	notifier.Handle(third, SignalExecution{})
	notifier.Handle(third, SignalExecution{Alerts: Alerts{thirdAlert("r-1")}})
	awaitSlackWebhook(t, webhook.arrived, 1, "request")
	notifier.Handle(third, SignalExecution{Alerts: Alerts{thirdAlert("r-1"), thirdAlert("r-2")}})
	// pg-1 joins the digest at its signal's start and clears while the queue
	// is full, so the clear waits behind the refused digest
	shared := slackTestAlert("pg-1", "", SeverityWarn)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{shared}})
	notifier.Handle(slackTestSignal, SignalExecution{})
	notifier.Handle(slackTestSignal, SignalExecution{})
	// another signal's first run reports the same incident
	notifier.Handle(slackOtherTestSignal, SignalExecution{Alerts: Alerts{shared}})
	close(release)
	awaitSlackWebhook(t, webhook.answered, 2, "answer")
	notifier.flushSummary()
	awaitSlackWebhook(t, webhook.answered, 1, "answer")
	notifier.flushSummary()
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[WARN] raised (synthetic): r-1 third",
		"[WARN] raised (synthetic): r-2 third",
		"[ACTIVE AT START] (synthetic) 1 incident(s) reported by this watcher's first runs of their signals:\n"+
			"- [WARN] pg/active-pileup · active-pileup · pg-1\n",
	)
	notifier.stateLock.Lock()
	_, active := notifier.keyIncidents[slackIncidentKey(shared)]
	notifier.stateLock.Unlock()
	if !active {
		t.Fatal("pg-1 is not active in the notifier, though its signal still reports it")
	}
}

// A digest still settling at shutdown is posted by Close.
func TestSlackAlertNotifierPostsAPendingDigestAtClose(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.digestSettings(webhookUrl))
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn)}})
	closeSlackNotifier(t, notifier)
	requireSlackTexts(t, webhook.posted(),
		"[ACTIVE AT START] (synthetic) 1 incident(s) reported by this watcher's first runs of their signals:\n"+
			"- [WARN] pg/active-pileup · active-pileup · pg-1\n",
	)
}

// Failed runs do not use up a signal's start window, and a visibility raise
// from runs that all failed is at start. Once a run completed, a later
// visibility raise is new.
func TestSlackAlertNotifierStartWindowSkipsFailedRuns(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.digestSettings(webhookUrl))
	failure := errors.New("synthetic ssh timeout")
	cannotObserve := visibilityAlert(syntheticSettings(nil), slackTestSignal, failure)
	otherCannotObserve := visibilityAlert(syntheticSettings(nil), slackOtherTestSignal, failure)

	notifier.Handle(slackTestSignal, SignalExecution{HeldAlerts: Alerts{cannotObserve}, Failed: true})
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{cannotObserve}, Failed: true})
	notifier.Handle(slackOtherTestSignal, SignalExecution{})
	notifier.Handle(slackOtherTestSignal, SignalExecution{HeldAlerts: Alerts{otherCannotObserve}, Failed: true})
	notifier.Handle(slackOtherTestSignal, SignalExecution{Alerts: Alerts{otherCannotObserve}, Failed: true})
	// the first complete run of the failing signal
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{slackTestAlert("pg-1", "", SeverityWarn)}})
	harness.advance(2 * time.Minute)
	notifier.flushSummary()
	closeSlackNotifier(t, notifier)

	requireSlackTexts(t, webhook.posted(),
		"[WARN] raised (synthetic): Signal 3.1 (redis/memory) could not run",
		"[ACTIVE AT START] (synthetic) 2 incident(s) reported by this watcher's first runs of their signals:\n"+
			"- [WARN] monitor/visibility · cannot-observe · pg/active-pileup\n"+
			"- [WARN] pg/active-pileup · active-pileup · pg-1\n",
	)
}

// Slack text gets the address scrubbing of the process's stdout and stderr:
// addresses become [scrubbed], an IPv4 port stays, and the rest survives.
func TestSlackAlertNotifierScrubsAddressesFromMessages(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	alert := slackTestAlert("192.0.2.10:6379", "", SeverityPage)
	alert.Symptom = "Connections from 2001:db8::7 and 198.51.100.4 stall at 18:00:00"
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{alert}})
	closeSlackNotifier(t, notifier)

	texts := webhook.posted()
	requireSlackTexts(t, texts,
		"[PAGE] raised (synthetic): Connections from [scrubbed] and [scrubbed] stall at 18:00:00\n"+
			"pg/active-pileup · active-pileup · [scrubbed]:6379\n"+
			"SIGNALS.md §1.3 · active-pileup · playbook SIGNALS.md 5.8",
	)
	for _, address := range []string{"192.0.2.10", "2001:db8::7", "198.51.100.4"} {
		if strings.Contains(texts[0], address) {
			t.Fatalf("message carries %s: %q", address, texts[0])
		}
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
