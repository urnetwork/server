package monitor

// FS-50's core (D-OPS-9, R-PHASE-12): page classes reach the rota's pager on a
// fake clock, through the same executions the continuous CLI hands Slack. Time
// moves only by hand, and a flush of the sender stands in for every wait.

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/pager"
)

// How long a test waits on the sender goroutine before it fails.
const pagerTestWait = 30 * time.Second

// The secret part of a synthetic pager endpoint.
const syntheticPagerSecret = "syntheticPagerSecret0001"

// A clock a test moves by hand, standing at 02:00.
type pagerTestClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func newPagerTestClock() *pagerTestClock {
	return &pagerTestClock{now: time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)}
}

func (self *pagerTestClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *pagerTestClock) Advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(duration)
}

// One test's rota: the clock, the service and what the notifier said.
type pagerHarness struct {
	clock *pagerTestClock
	sink  *pager.FakeSink

	stateLock sync.Mutex
	logs      []string
	// the executions the notifier handed Warn, as Slack would see them
	warnExecutions []SignalExecution
}

func newPagerHarness() *pagerHarness {
	clock := newPagerTestClock()
	return &pagerHarness{clock: clock, sink: pager.NewFakeSink(pager.FakeSinkSettings{Now: clock.Now})}
}

func (self *pagerHarness) settings() PagerAlertNotifierSettings {
	return PagerAlertNotifierSettings{
		Sink:        self.sink,
		Environment: "synthetic",
		Now:         self.clock.Now,
		Logf: func(format string, args ...any) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.logs = append(self.logs, fmt.Sprintf(format, args...))
		},
		Warn: func(_ Signal, execution SignalExecution) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.warnExecutions = append(self.warnExecutions, execution)
		},
	}
}

func (self *pagerHarness) warned() []SignalExecution {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]SignalExecution(nil), self.warnExecutions...)
}

func (self *pagerHarness) logged() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.logs, "\n")
}

func flushPager(t *testing.T, notifier *PagerAlertNotifier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pagerTestWait)
	defer cancel()
	if err := notifier.sender.Flush(ctx); err != nil {
		t.Fatalf("the sender did not drain: %v", err)
	}
}

func closePager(t *testing.T, notifier *PagerAlertNotifier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pagerTestWait)
	defer cancel()
	notifier.Close(ctx)
}

// An alert of class on a synthetic target.
func pagerTestAlert(class string, severity Severity) Alert {
	alert := slackTestAlert("pg-1", "", severity)
	alert.Class = class
	return alert
}

func requirePagedPeople(t *testing.T, sink *pager.FakeSink, want ...string) {
	t.Helper()
	got := []string{}
	for _, notification := range sink.Notifications() {
		got = append(got, notification.Person)
	}
	if len(want) == 0 {
		want = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged %v, want %v", got, want)
	}
}

// FS-50's core for every page class in the table: at 02:00 the first person is
// paged at once, the second at 30 minutes unacknowledged and not before; a row
// that pages both at once pages both at 02:00 and nobody after.
func TestPageClassAt0200PagesTheSecondPersonAt30MinutesNotBefore(t *testing.T) {
	for _, row := range pageClasses {
		harness := newPagerHarness()
		notifier := NewPagerAlertNotifier(harness.settings())
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert(row.class, SeverityPage)}})
		flushPager(t, notifier)
		atOnce := []string{pager.FakeFirstPerson}
		if row.bothAtOnce {
			atOnce = append(atOnce, pager.FakeSecondPerson)
		}
		requirePagedPeople(t, harness.sink, atOnce...)
		for _, notification := range harness.sink.Notifications() {
			if notification.Page.Class != row.class || notification.Page.BothAtOnce != row.bothAtOnce || !notification.Time.Equal(time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)) {
				t.Fatalf("%s: paged %+v", row.class, notification)
			}
		}

		harness.clock.Advance(30*time.Minute - time.Second)
		harness.sink.Tick()
		requirePagedPeople(t, harness.sink, atOnce...)
		harness.clock.Advance(time.Second)
		harness.sink.Tick()
		requirePagedPeople(t, harness.sink, pager.FakeFirstPerson, pager.FakeSecondPerson)
		closePager(t, notifier)
	}
}

func TestAcknowledgmentStopsTheSecondPage(t *testing.T) {
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("filecluster-set-at-risk", SeverityPage)}})
	flushPager(t, notifier)
	harness.clock.Advance(29 * time.Minute)
	if !harness.sink.Acknowledge(harness.sink.OpenKeys()[0]) {
		t.Fatal("the page was not open")
	}
	harness.clock.Advance(time.Hour)
	harness.sink.Tick()
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
}

// A warn class never pages, and neither does a Slack-only page: owner decision
// 143 is §19.8's page classes only.
func TestOnlyAPageClassAtPageSeverityPages(t *testing.T) {
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{
		pagerTestAlert("active-pileup", SeverityWarn),
		pagerTestAlert("redis-process", SeverityPage),
		pagerTestAlert("postgres-primary-down", SeverityWarn),
	}})
	// and each clears without a resolve
	notifier.Handle(slackTestSignal, SignalExecution{})
	notifier.Handle(slackTestSignal, SignalExecution{})
	flushPager(t, notifier)
	if got := harness.sink.Attempts(); len(got) != 0 {
		t.Fatalf("attempts %+v", got)
	}
}

// PageSustain promotes a warn behind the real gate: nothing pages until the
// alert reaches page severity, and a one-run dip neither ends nor repeats the
// page.
func TestAlertPromotedToPageSeverityPagesOnceBehindTheGate(t *testing.T) {
	alert := pagerTestAlert("write-budget-p0-past-refill", SeverityWarn)
	alert.Sustain = 2
	alert.PageSustain = 3
	present := scriptedExecution{alerts: Alerts{alert}}
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)

	// the sink's calls after each execution
	attemptCounts := []int{}
	flushErrors := []error{}
	handle := func(signal Signal, execution SignalExecution) {
		notifier.Handle(signal, execution)
		ctx, cancel := context.WithTimeout(context.Background(), pagerTestWait)
		defer cancel()
		if err := notifier.sender.Flush(ctx); err != nil {
			flushErrors = append(flushErrors, err)
		}
		attemptCounts = append(attemptCounts, len(harness.sink.Attempts()))
	}
	runScriptedExecutions(t, []scriptedExecution{present, present, present, present, {}, present, present, present, present}, handle)
	if len(flushErrors) != 0 {
		t.Fatalf("flush: %v", flushErrors)
	}
	// held, warn, page; then a dip the gate restarts at held, warn and page
	if want := []int{0, 0, 1, 1, 1, 1, 1, 1, 1}; !reflect.DeepEqual(attemptCounts, want) {
		t.Fatalf("attempts after each run %v, want %v", attemptCounts, want)
	}
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
}

// A signal that cannot see does not end a page: a failed run, a visibility
// alert, gated or held, and a held alert each keep it open, and only complete
// runs without the incident resolve it.
func TestVisibilityAlertDoesNotClearAPage(t *testing.T) {
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	page := pagerTestAlert("postgres-primary-down", SeverityPage)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{page}})

	cannotObserve := visibilityAlert(syntheticSettings(nil), slackTestSignal, errors.New("synthetic ssh timeout"))
	if !cannotObserve.unobservable {
		t.Fatal("the visibility alert is not marked")
	}
	incomplete := []SignalExecution{
		{Alerts: Alerts{cannotObserve}},
		{HeldAlerts: Alerts{cannotObserve}},
		{Failed: true},
		{HeldAlerts: Alerts{page}},
	}
	for _, execution := range incomplete {
		// one complete run short of a clear, so each case must break the streak
		notifier.Handle(slackTestSignal, SignalExecution{})
		notifier.Handle(slackTestSignal, execution)
		flushPager(t, notifier)
		if got := len(harness.sink.OpenKeys()); got != 1 {
			t.Fatalf("%+v ended the page", execution)
		}
	}
	notifier.Handle(slackTestSignal, SignalExecution{})
	notifier.Handle(slackTestSignal, SignalExecution{})
	flushPager(t, notifier)
	if got := harness.sink.OpenKeys(); len(got) != 0 {
		t.Fatalf("complete runs did not resolve the page: %v", got)
	}
}

// The same behind the real gate, where a held visibility alert reaches the
// notifier as a run with nothing in Alerts.
func TestAFailedRunBehindTheGateDoesNotClearAPage(t *testing.T) {
	present := scriptedExecution{alerts: Alerts{pagerTestAlert("slo-burn-page", SeverityPage)}}
	failed := scriptedExecution{err: errors.New("synthetic ssh timeout")}
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	runScriptedExecutions(t, []scriptedExecution{present, failed, {}, failed, {}}, notifier.Handle)
	flushPager(t, notifier)
	if got := len(harness.sink.OpenKeys()); got != 1 {
		t.Fatalf("%d pages open", got)
	}
	runScriptedExecutions(t, []scriptedExecution{{}, {}}, notifier.Handle)
	flushPager(t, notifier)
	if got := harness.sink.OpenKeys(); len(got) != 0 {
		t.Fatalf("open keys %v", got)
	}
}

// A page ends only when every signal reporting its incident stops.
func TestAPageStaysOpenWhileASecondSignalReportsIt(t *testing.T) {
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	other := selectionSignal{number: "1.4", key: "other-reporter", id: slackTestSignal.id}
	alert := pagerTestAlert("filecluster-key-held", SeverityPage)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{alert}})
	notifier.Handle(other, SignalExecution{Alerts: Alerts{alert}})
	for range 3 {
		notifier.Handle(slackTestSignal, SignalExecution{})
	}
	flushPager(t, notifier)
	if got := len(harness.sink.OpenKeys()); got != 1 {
		t.Fatalf("%d pages open", got)
	}
	for range 2 {
		notifier.Handle(other, SignalExecution{})
	}
	flushPager(t, notifier)
	if got := harness.sink.OpenKeys(); len(got) != 0 {
		t.Fatalf("open keys %v", got)
	}
}

// The stepped retry wait of one notifier.
type pagerRetrySteps struct {
	waits   chan struct{}
	release chan struct{}
}

func newPagerRetrySteps() *pagerRetrySteps {
	return &pagerRetrySteps{waits: make(chan struct{}), release: make(chan struct{})}
}

func (self *pagerRetrySteps) wait(ctx context.Context, _ time.Duration) bool {
	select {
	case self.waits <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	select {
	case <-self.release:
		return true
	case <-ctx.Done():
		return false
	}
}

func (self *pagerRetrySteps) requireWait(t *testing.T) {
	t.Helper()
	select {
	case <-self.waits:
	case <-time.After(pagerTestWait):
		t.Fatal("the sender never waited to retry")
	}
}

func (self *pagerRetrySteps) proceed(t *testing.T) {
	t.Helper()
	select {
	case self.release <- struct{}{}:
	case <-time.After(pagerTestWait):
		t.Fatal("the sender was not waiting to retry")
	}
}

// A sink outage retries and warns, losing nothing: the page and its resolve
// both reach the service once it returns, and the warn raises and clears
// through the real Slack rule.
func TestSinkOutageRetriesAndWarnsLosingNothing(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	slackHarness := newSlackTestHarness()
	slack := NewSlackAlertNotifier(slackHarness.settings(webhookUrl))

	harness := newPagerHarness()
	harness.sink.Fail(errors.New("synthetic outage"))
	steps := newPagerRetrySteps()
	settings := harness.settings()
	settings.Delivery = pager.RetrySinkSettings{RetryWait: steps.wait}
	settings.Warn = slack.Handle
	notifier := NewPagerAlertNotifier(settings)
	defer closePager(t, notifier)

	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("filecluster-refusing-writes", SeverityPage)}})
	steps.requireWait(t)
	requirePagedPeople(t, harness.sink)
	steps.proceed(t)
	steps.requireWait(t)
	notifier.Handle(slackTestSignal, SignalExecution{})
	notifier.Handle(slackTestSignal, SignalExecution{})

	harness.sink.Fail(nil)
	steps.proceed(t)
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
	if got := harness.sink.OpenKeys(); len(got) != 0 {
		t.Fatalf("the resolve did not follow the page: %v", got)
	}
	closeSlackNotifier(t, slack)
	requireSlackTexts(t, webhook.posted(),
		"[WARN] raised (synthetic): The pager sink is not taking pages",
		"[CLEARED] WARN (synthetic): Pager sink",
	)
}

// The warn clears on its carrier's count, which need not be the pager's own,
// and a count short of the carrier's leaves the warn standing.
func TestSinkRecoveryClearsTheWarnOnItsCarriersCount(t *testing.T) {
	for _, c := range []struct {
		warnClearRuns int
		wantCleared   bool
	}{
		{warnClearRuns: 4, wantCleared: true},
		{warnClearRuns: 3, wantCleared: false},
	} {
		webhook, webhookUrl := newFakeSlackWebhook(t)
		slackSettings := newSlackTestHarness().settings(webhookUrl)
		slackSettings.ClearAfterCompleteRuns = 4
		slack := NewSlackAlertNotifier(slackSettings)

		harness := newPagerHarness()
		harness.sink.Fail(errors.New("synthetic outage"))
		steps := newPagerRetrySteps()
		settings := harness.settings()
		settings.Delivery = pager.RetrySinkSettings{RetryWait: steps.wait}
		settings.Warn = slack.Handle
		settings.WarnClearRuns = c.warnClearRuns
		notifier := NewPagerAlertNotifier(settings)

		notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("postgres-primary-down", SeverityPage)}})
		steps.requireWait(t)
		harness.sink.Fail(nil)
		steps.proceed(t)
		// the sender has told the recovery once it has stopped
		closePager(t, notifier)
		requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
		closeSlackNotifier(t, slack)
		want := []string{"[WARN] raised (synthetic): The pager sink is not taking pages"}
		if c.wantCleared {
			want = append(want, "[CLEARED] WARN (synthetic): Pager sink")
		}
		requireSlackTexts(t, webhook.posted(), want...)
	}
}

// A restart forgets every incident, so it re-raises under the one key, and the
// sink's dedup leaves the page and its clock alone.
func TestRestartReRaisesUnderOneKey(t *testing.T) {
	harness := newPagerHarness()
	alert := pagerTestAlert("filecluster-set-at-risk", SeverityPage)
	first := NewPagerAlertNotifier(harness.settings())
	first.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{alert}})
	closePager(t, first)
	harness.clock.Advance(20 * time.Minute)

	restarted := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, restarted)
	// a later report of the incident varies only in what the key leaves out
	again := alert
	again.Frame = "another-frame"
	again.ObservedAt = harness.clock.Now()
	restarted.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{again}})
	flushPager(t, restarted)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
	attempts := harness.sink.Attempts()
	if len(attempts) != 2 || attempts[0].DedupKey != attempts[1].DedupKey {
		t.Fatalf("attempts %+v", attempts)
	}

	harness.clock.Advance(10 * time.Minute)
	harness.sink.Tick()
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson, pager.FakeSecondPerson)

	// the new process owns the incident now, so its clear ends the page
	restarted.Handle(slackTestSignal, SignalExecution{})
	restarted.Handle(slackTestSignal, SignalExecution{})
	flushPager(t, restarted)
	if got := harness.sink.OpenKeys(); len(got) != 0 {
		t.Fatalf("open keys %v", got)
	}
}

// A key holds one incident: it differs by target, class and environment, and
// not by a frame or a time.
func TestPagerDedupKeyIsTheIncidentIdentity(t *testing.T) {
	alert := pagerTestAlert("filecluster-set-at-risk", SeverityPage)
	key := pagerDedupKey("main", alert)
	if err := pager.ValidateDedupKey(key); err != nil {
		t.Fatal(err)
	}
	same := alert
	same.Frame = "another-frame"
	same.ObservedAt = time.Unix(1, 0)
	same.Symptom = "another symptom"
	if pagerDedupKey("main", same) != key {
		t.Fatal("a frame, time or symptom moved the key")
	}
	otherTarget := alert
	otherTarget.Target = "pg-2"
	otherClass := alert
	otherClass.Class = "filecluster-refusing-writes"
	otherSignal := alert
	otherSignal.SignalID = "pg/other"
	for name, other := range map[string]string{
		"target":      pagerDedupKey("main", otherTarget),
		"class":       pagerDedupKey("main", otherClass),
		"signal":      pagerDedupKey("main", otherSignal),
		"environment": pagerDedupKey("canary", alert),
	} {
		if other == key {
			t.Errorf("another %s kept the key", name)
		}
	}
}

// I-13: what leaves the site is the class, the time, the environment, an opaque
// key and the row's fixed text. Everything else in an alert stays home.
func TestPageCarriesNothingFromTheAlertsFreeText(t *testing.T) {
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	secrets := []string{"host-secret-name", "198.51.100.23", "network-secret-name", "token-secret-value", "evidence-secret", "person@example.test"}
	alert := pagerTestAlert("slo-burn-page", SeverityPage)
	alert.Target = secrets[0] + " " + secrets[1]
	alert.Frame = secrets[1]
	alert.Symptom = secrets[2]
	alert.Mechanism = secrets[3]
	alert.Evidence = secrets[4]
	alert.Observed = secrets[5]
	alert.Context = secrets[1]
	alert.Action = secrets[3]
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{alert}})
	flushPager(t, notifier)
	notifications := harness.sink.Notifications()
	if len(notifications) != 1 {
		t.Fatalf("notifications %+v", notifications)
	}
	page := notifications[0].Page
	body := fmt.Sprintf("%+v", page)
	for _, secret := range secrets {
		if strings.Contains(body, secret) {
			t.Errorf("the page carries %q: %s", secret, body)
		}
	}
	row, _ := pageClassOf("slo-burn-page")
	if !strings.Contains(page.Summary, row.row) || !strings.Contains(page.Summary, row.action) || !strings.Contains(page.Summary, "[synthetic]") {
		t.Fatalf("summary %q", page.Summary)
	}
}

// A stuck sink holds neither Handle nor the executions behind it.
type stuckPagerSink struct {
	arrived chan struct{}
	release chan struct{}
}

func (self *stuckPagerSink) Trigger(ctx context.Context, _ pager.Page) error {
	select {
	case self.arrived <- struct{}{}:
	default:
	}
	select {
	case <-self.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (self *stuckPagerSink) Resolve(context.Context, string) error {
	return nil
}

func TestHandleNeverWaitsOnTheNetwork(t *testing.T) {
	sink := &stuckPagerSink{arrived: make(chan struct{}, 1), release: make(chan struct{})}
	harness := newPagerHarness()
	settings := harness.settings()
	settings.Sink = sink
	notifier := NewPagerAlertNotifier(settings)
	defer closePager(t, notifier)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("postgres-primary-down", SeverityPage)}})
	select {
	case <-sink.arrived:
	case <-time.After(pagerTestWait):
		t.Fatal("the page never reached the sink")
	}
	// the sink holds its call, and this goroutine still gets through
	for i := range 1000 {
		alert := pagerTestAlert("filecluster-set-at-risk", SeverityPage)
		alert.Target = fmt.Sprintf("set-%d", i)
		notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{alert}})
	}
	close(sink.release)
	flushPager(t, notifier)
}

// A watcher asked to page that has no sink says so: on stderr once, and on
// Slack, and everything else is a quiet no-op.
func TestNotifierWithoutASinkWarnsAndPagesNothing(t *testing.T) {
	harness := newPagerHarness()
	settings := harness.settings()
	settings.Sink = nil
	settings.DisabledReason = pager.ErrNoPagerVendor.Error()
	notifier := NewPagerAlertNotifier(settings)
	if !strings.Contains(harness.logged(), "pager notifications disabled") || !strings.Contains(harness.logged(), "plan-OPS-pager-service") {
		t.Fatalf("logs:\n%s", harness.logged())
	}
	warned := harness.warned()
	if len(warned) != 1 || len(warned[0].Alerts) != 1 || warned[0].Alerts[0].Class != "pager-sink-failing" ||
		!strings.Contains(warned[0].Alerts[0].Symptom, "cannot page") || warned[0].Alerts[0].Severity != SeverityWarn {
		t.Fatalf("warned %+v", warned)
	}
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("postgres-primary-down", SeverityPage)}})
	closePager(t, notifier)
	if got := harness.sink.Attempts(); len(got) != 0 {
		t.Fatalf("attempts %+v", got)
	}
}

func TestNotifierWithoutAWarnCarrierKeepsTheWarnInItsLog(t *testing.T) {
	harness := newPagerHarness()
	harness.sink.Fail(errors.New("synthetic outage"))
	steps := newPagerRetrySteps()
	settings := harness.settings()
	settings.Delivery = pager.RetrySinkSettings{RetryWait: steps.wait}
	settings.Warn = nil
	notifier := NewPagerAlertNotifier(settings)
	defer closePager(t, notifier)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("postgres-primary-down", SeverityPage)}})
	steps.requireWait(t)
	harness.sink.Fail(nil)
	steps.proceed(t)
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
	if !strings.Contains(harness.logged(), "synthetic outage") {
		t.Fatalf("logs:\n%s", harness.logged())
	}
}

// The executions after Close are dropped, and Close gives the sender its
// chance first.
func TestCloseHandsOverWhatIsQueuedAndStopsTakingExecutions(t *testing.T) {
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("postgres-primary-down", SeverityPage)}})
	closePager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
	notifier.Handle(slackTestSignal, SignalExecution{Alerts: Alerts{pagerTestAlert("slo-burn-page", SeverityPage)}})
	closePager(t, notifier)
	if got := len(harness.sink.Attempts()); got != 1 {
		t.Fatalf("%d attempts", got)
	}
}

// The composed path: the real loop and gate, the real sender and the real
// webhook client, with the rota's policy at the far end of an http post.
func TestPageReachesTheRotaThroughTheLoopSenderAndWebhook(t *testing.T) {
	harness := newPagerHarness()
	endpoint := httptest.NewServer(harness.sink)
	defer endpoint.Close()
	webhookSettings := pager.DefaultWebhookSinkSettings()
	webhookSettings.Url = endpoint.URL + "/events/" + syntheticPagerSecret
	sink, err := pager.NewWebhookSink(webhookSettings)
	if err != nil {
		t.Fatal(err)
	}
	settings := harness.settings()
	settings.Sink = sink
	notifier := NewPagerAlertNotifier(settings)
	defer closePager(t, notifier)

	alert := pagerTestAlert("filecluster-set-at-risk", SeverityPage)
	alert.Sustain = 2
	present := scriptedExecution{alerts: Alerts{alert}}
	// one run stays below its Sustain, and each loop starts the gate over
	runScriptedExecutions(t, []scriptedExecution{present}, notifier.Handle)
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink)

	runScriptedExecutions(t, []scriptedExecution{present, present}, notifier.Handle)
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
	harness.clock.Advance(30 * time.Minute)
	harness.sink.Tick()
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson, pager.FakeSecondPerson)

	runScriptedExecutions(t, []scriptedExecution{{}, {}}, notifier.Handle)
	flushPager(t, notifier)
	if got := harness.sink.OpenKeys(); len(got) != 0 {
		t.Fatalf("open keys %v", got)
	}
}

// §19.8's M3 rows, copied here so an edit to the table has to say why.
func TestPageClassesHoldSection19_8sM3Rows(t *testing.T) {
	want := map[string]string{
		"filecluster-set-at-risk":     "a set at_risk, or a cluster refusing writes",
		"filecluster-refusing-writes": "a set at_risk, or a cluster refusing writes",
		"postgres-primary-down":       "the Postgres primary down (the token exchange stops)",
		"slo-burn-page":               "2% of a 30-day budget burned in 1 h",
		"write-budget-p0-past-refill": "P0 past the refill: a metadata cluster's P0 bytes over the last hour above that hour's refill",
		"filecluster-key-held":        "a file-cluster key held over 1 min by a write that cannot settle",
	}
	for class, row := range want {
		got, ok := pageClassOf(class)
		if !ok || got.row != row || got.firstMilestone != "M3" || got.action == "" {
			t.Errorf("%s: row %+v", class, got)
		}
	}
	if _, ok := pageClassOf("active-pileup"); ok {
		t.Error("a class outside §19.8 has a row")
	}
}

// §19.8's M5 rows: both dns edges failing and the canary signature (DNS-M5-17's
// classes), an interface's split probe, and the notices, which page both at
// once and whose words are the ones pager.PageNotice sends.
func TestPageClassesHoldSection19_8sM5Rows(t *testing.T) {
	for class, want := range map[string]pageClass{
		"dns-ns-down":            {class: "dns-ns-down", row: "both dns edges failing probes", firstMilestone: "M5", action: "§12.6's runbook"},
		"dns-canary-signature":   {class: "dns-canary-signature", row: "the canary signature failing", firstMilestone: "M5", action: "§12.6's runbook"},
		"lb-split-probe-failing": {class: "lb-split-probe-failing", row: "an interface's split probe failing (W1)", firstMilestone: "M5", action: "that interface's prior lb version"},
		"abuse-notice": {
			class:          "abuse-notice",
			row:            "registrar, registry, law-enforcement notices",
			firstMilestone: "M5",
			action: "§21.8: removal orders and registrar or registry notices handled within 1 h by the rota; " +
				"production orders go to counsel's out-of-hours contact",
			bothAtOnce: true,
		},
	} {
		got, ok := pageClassOf(class)
		if !ok || got != want {
			t.Errorf("%s: row %+v", class, got)
		}
	}
	notice, _ := pageClassOf(pager.NoticePageClass)
	if notice.row != pager.NoticePageRow || notice.action != pager.NoticePageAction || !notice.bothAtOnce {
		t.Fatalf("the notice row and pager's words differ: %+v", notice)
	}
	for _, row := range pageClasses {
		if row.bothAtOnce && row.class != pager.NoticePageClass {
			t.Errorf("%s pages both at once, which §19.8 asks only of the notices", row.class)
		}
	}
}

// What the class table and the row table disagree on, and any row that could
// not reach a person: a page class with no row, a class with two, a row whose
// class is not a page class, and a row missing a column.
func pageClassIssues(classKinds map[string]alertClassKind, rows []pageClass) []string {
	issues := []string{}
	classRowCounts := map[string]int{}
	for _, row := range rows {
		classRowCounts[row.class]++
		if kind, ok := classKinds[row.class]; !ok || kind != pageAlertClass {
			issues = append(issues, row.class+" has a row but is not a page class")
		}
		if row.row == "" || row.firstMilestone == "" || row.action == "" {
			issues = append(issues, row.class+" has an empty column")
		}
	}
	for class, kind := range classKinds {
		if kind == pageAlertClass && classRowCounts[class] == 0 {
			issues = append(issues, class+" is a page class with no row")
		}
	}
	for class, count := range classRowCounts {
		if 1 < count {
			issues = append(issues, class+" has more than one row")
		}
	}
	sort.Strings(issues)
	return issues
}

// Every page class has one row in §19.8's table, so none is declared and then
// quietly never pages, and every row is declared.
func TestEveryPageClassHasOneTableRow(t *testing.T) {
	for _, issue := range pageClassIssues(alertClassKinds, pageClasses) {
		t.Error(issue)
	}
	if len(pageClasses) < 6 {
		t.Fatalf("%d rows: the table lost §19.8's M3 rows", len(pageClasses))
	}
}

// The check above fails each way it should.
func TestPageClassIssuesNameEachDisagreement(t *testing.T) {
	good := pageClass{class: "synthetic-page", row: "a row", firstMilestone: "M3", action: "an action"}
	for _, c := range []struct {
		name       string
		classKinds map[string]alertClassKind
		rows       []pageClass
		want       []string
	}{
		{name: "agreed", classKinds: map[string]alertClassKind{"synthetic-page": pageAlertClass}, rows: []pageClass{good}, want: []string{}},
		{
			name:       "no row",
			classKinds: map[string]alertClassKind{"synthetic-page": pageAlertClass},
			want:       []string{"synthetic-page is a page class with no row"},
		},
		{
			name:       "incident with a row",
			classKinds: map[string]alertClassKind{"synthetic-page": incidentAlertClass},
			rows:       []pageClass{good},
			want:       []string{"synthetic-page has a row but is not a page class"},
		},
		{
			name: "unclassified row",
			rows: []pageClass{good},
			want: []string{"synthetic-page has a row but is not a page class"},
		},
		{
			name:       "two rows",
			classKinds: map[string]alertClassKind{"synthetic-page": pageAlertClass},
			rows:       []pageClass{good, good},
			want:       []string{"synthetic-page has more than one row"},
		},
		{
			name:       "empty column",
			classKinds: map[string]alertClassKind{"synthetic-page": pageAlertClass},
			rows:       []pageClass{{class: "synthetic-page", row: "a row", firstMilestone: "M3"}},
			want:       []string{"synthetic-page has an empty column"},
		},
	} {
		if got := pageClassIssues(c.classKinds, c.rows); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: issues %v, want %v", c.name, got, c.want)
		}
	}
}

// A page class is an incident to everything that asks about visibility: it
// cannot be mistaken for an unknown state.
func TestPageClassIsNeverAVisibilityClass(t *testing.T) {
	for _, row := range pageClasses {
		if isVisibilityAlertClass(row.class) {
			t.Errorf("%s reads as a visibility class", row.class)
		}
		if strings.HasSuffix(row.class, "-unobservable") {
			t.Errorf("%s is named as an unknown state", row.class)
		}
		settings := syntheticSettings(nil)
		if alertFromFinding(settings, "0", "synthetic", "Synthetic", finding{probeId: "synthetic/probe", tier: tierPage, class: row.class, target: "target"}).unobservable {
			t.Errorf("%s marks its alert unobservable", row.class)
		}
	}
}

// The production loader carries vault monitor.yml alerts.pager_webhook_url into
// the settings the CLI hands the notifier, with the Slack webhook unchanged.
func TestLoadSignalSettingsCarriesThePagerWebhook(t *testing.T) {
	t.Setenv("WARP_ENV", "synthetic")
	t.Setenv("WARP_HOME", t.TempDir())
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
	popMonitor := server.Vault.PushSimpleResource("monitor.yml", []byte("ssh:\n  user: monitor\nhosts:\n  - name: pg-1\n    overlay_ip: 192.0.2.10\n    roles: [pg-primary]\nalerts:\n  pager_webhook_url: \" https://pager.example/events/"+syntheticPagerSecret+" \"\n"))
	defer popMonitor()
	popServices := server.Vault.PushSimpleResource("services.yml", []byte("domain: example.test\nversions:\n  - services:\n      api:\n        blocks:\n          - g1: 100\n"))
	defer popServices()

	settings, err := loadSignalSettingsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if settings.AlertNotifications.PagerWebhookUrl != "https://pager.example/events/"+syntheticPagerSecret || settings.AlertNotifications.SlackWebhookUrl != "" {
		t.Fatal("loaded settings do not carry the trimmed pager webhook alone")
	}
}
