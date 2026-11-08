package pager

// Pages, the rota's policy on FakeSink, delivery through RetrySink and the
// webhook client. Each test steps time and retries by hand: nothing here
// sleeps.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// The secret part of a synthetic endpoint, which must never reach an error.
const syntheticWebhookSecret = "syntheticPagerSecret0001"

// How long a test waits on another goroutine before it fails.
const testWait = 30 * time.Second

// A clock a test moves by hand.
type testClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)}
}

func (self *testClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testClock) Advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(duration)
}

func testPage(dedupKey string) Page {
	return Page{
		DedupKey: dedupKey,
		Class:    "synthetic-class",
		Summary:  "synthetic summary",
		Time:     time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC),
	}
}

func newClockedFakeSink() (*FakeSink, *testClock) {
	clock := newTestClock()
	return NewFakeSink(FakeSinkSettings{Now: clock.Now}), clock
}

// Who was paged, in order.
func paged(sink *FakeSink) []string {
	people := []string{}
	for _, notification := range sink.Notifications() {
		people = append(people, notification.Person+":"+notification.Page.DedupKey)
	}
	return people
}

func requirePaged(t *testing.T, sink *FakeSink, want ...string) {
	t.Helper()
	got := paged(sink)
	if len(want) == 0 {
		want = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged %v, want %v", got, want)
	}
}

func requireFlushed(t *testing.T, retrySink *RetrySink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := retrySink.Flush(ctx); err != nil {
		t.Fatalf("the queue did not drain: %v", err)
	}
}

func closeRetrySink(t *testing.T, retrySink *RetrySink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	retrySink.Close(ctx)
}

// Hands retries to the test one at a time.
type retrySteps struct {
	waits   chan time.Duration
	release chan bool
}

func newRetrySteps() *retrySteps {
	return &retrySteps{waits: make(chan time.Duration), release: make(chan bool)}
}

func (self *retrySteps) wait(ctx context.Context, delay time.Duration) bool {
	select {
	case self.waits <- delay:
	case <-ctx.Done():
		return false
	}
	select {
	case proceed := <-self.release:
		return proceed
	case <-ctx.Done():
		return false
	}
}

// Waits for the sender to reach its next retry wait and returns the delay.
func (self *retrySteps) requireWait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case delay := <-self.waits:
		return delay
	case <-time.After(testWait):
		t.Fatal("the sender never waited to retry")
		return 0
	}
}

func (self *retrySteps) proceed(t *testing.T) {
	t.Helper()
	select {
	case self.release <- true:
	case <-time.After(testWait):
		t.Fatal("the sender was not waiting to retry")
	}
}

// Settings that step retries by hand and keep logs.
func steppedSettings(steps *retrySteps, logs *lockedLines) RetrySinkSettings {
	return RetrySinkSettings{
		RetryDelay:    time.Second,
		MaxRetryDelay: 4 * time.Second,
		RetryWait:     steps.wait,
		Logf:          logs.logf,
	}
}

// Lines written from another goroutine.
type lockedLines struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *lockedLines) logf(format string, args ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, fmt.Sprintf(format, args...))
}

func (self *lockedLines) joined() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.lines, "\n")
}

// A sink that holds the calls a test chooses, so a test can act while a
// hand-off is in flight. Every other call passes through.
type gatedSink struct {
	inner Sink
	// true for a call to hold; nil holds none
	holds func(resolve bool, dedupKey string) bool
	// receives once per held call, when it arrives
	arrived chan string
	// answers one held call: nil passes it to inner, an error fails it
	answers chan error
}

func newGatedSink(inner Sink, holds func(resolve bool, dedupKey string) bool) *gatedSink {
	return &gatedSink{inner: inner, holds: holds, arrived: make(chan string, 16), answers: make(chan error)}
}

func (self *gatedSink) hold(ctx context.Context, resolve bool, dedupKey string) error {
	if self.holds == nil || !self.holds(resolve, dedupKey) {
		return nil
	}
	self.arrived <- dedupKey
	select {
	case err := <-self.answers:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (self *gatedSink) Trigger(ctx context.Context, page Page) error {
	if err := self.hold(ctx, false, page.DedupKey); err != nil {
		return err
	}
	return self.inner.Trigger(ctx, page)
}

func (self *gatedSink) Resolve(ctx context.Context, dedupKey string) error {
	if err := self.hold(ctx, true, dedupKey); err != nil {
		return err
	}
	return self.inner.Resolve(ctx, dedupKey)
}

func (self *gatedSink) requireHeld(t *testing.T, want string) {
	t.Helper()
	select {
	case dedupKey := <-self.arrived:
		if dedupKey != want {
			t.Fatalf("held %s, want %s", dedupKey, want)
		}
	case <-time.After(testWait):
		t.Fatalf("%s never reached the sink", want)
	}
}

func (self *gatedSink) answer(t *testing.T, err error) {
	t.Helper()
	select {
	case self.answers <- err:
	case <-time.After(testWait):
		t.Fatal("no call was held")
	}
}

// The calls of a key, as "trigger" and "resolve" in order, refusals included.
func attemptsOf(sink *FakeSink, dedupKey string) []string {
	actions := []string{}
	for _, attempt := range sink.Attempts() {
		if attempt.DedupKey != dedupKey {
			continue
		}
		if attempt.Resolve {
			actions = append(actions, "resolve")
		} else {
			actions = append(actions, "trigger")
		}
	}
	return actions
}

func TestPageValidateRefusesWhatWouldFoldPagesOrLeak(t *testing.T) {
	if err := testPage("case-01:abc_DEF.9").Validate(); err != nil {
		t.Fatalf("an opaque key was refused: %v", err)
	}
	for _, c := range []struct {
		name string
		page Page
	}{
		{name: "empty key", page: testPage("")},
		{name: "space", page: testPage("two words")},
		{name: "raw identity", page: testPage("pg/active|class|host")},
		{name: "mail address", page: testPage("person@example.test")},
		{name: "path", page: testPage("a/b")},
		{name: "long key", page: testPage(strings.Repeat("a", 129))},
		{name: "no class", page: Page{DedupKey: "key", Summary: "summary", Time: time.Unix(1, 0)}},
		{name: "no summary", page: Page{DedupKey: "key", Class: "class", Time: time.Unix(1, 0)}},
		{name: "no time", page: Page{DedupKey: "key", Class: "class", Summary: "summary"}},
	} {
		if c.page.Validate() == nil {
			t.Errorf("%s: a page was accepted", c.name)
		}
	}
	err := testPage("person@example.test").Validate()
	if err == nil || strings.Contains(err.Error(), "person@") {
		t.Fatalf("a refused key reached its error: %v", err)
	}
}

// §19.8's rota: the first person at once, the second at 30 minutes unacknowledged
// and not before.
func TestFakeSinkPagesTheSecondPersonAt30MinutesUnacknowledged(t *testing.T) {
	sink, clock := newClockedFakeSink()
	if err := sink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	requirePaged(t, sink, "first:key")
	clock.Advance(30*time.Minute - time.Second)
	sink.Tick()
	requirePaged(t, sink, "first:key")
	clock.Advance(time.Second)
	sink.Tick()
	requirePaged(t, sink, "first:key", "second:key")
	opened := time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)
	if got := sink.Notifications()[1].Time; !got.Equal(opened.Add(30 * time.Minute)) {
		t.Fatalf("second paged at %s", got)
	}
	clock.Advance(time.Hour)
	sink.Tick()
	requirePaged(t, sink, "first:key", "second:key")
}

func TestFakeSinkAcknowledgmentStopsTheSecondPage(t *testing.T) {
	sink, clock := newClockedFakeSink()
	if err := sink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(29 * time.Minute)
	if !sink.Acknowledge("key") {
		t.Fatal("an open page could not be acknowledged")
	}
	clock.Advance(2 * time.Hour)
	sink.Tick()
	requirePaged(t, sink, "first:key")
	if sink.Acknowledge("other") {
		t.Fatal("a page that is not open was acknowledged")
	}
}

// A notice pages both at once, and there is nobody left to escalate to.
func TestFakeSinkBothAtOncePagesBothPeopleAtTheSameInstant(t *testing.T) {
	sink, clock := newClockedFakeSink()
	page := testPage("notice")
	page.BothAtOnce = true
	if err := sink.Trigger(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	requirePaged(t, sink, "first:notice", "second:notice")
	notifications := sink.Notifications()
	if !notifications[0].Time.Equal(notifications[1].Time) {
		t.Fatalf("paged at %s and %s", notifications[0].Time, notifications[1].Time)
	}
	clock.Advance(time.Hour)
	sink.Tick()
	requirePaged(t, sink, "first:notice", "second:notice")
}

// The dedup that a restart leans on: a repeat neither pages again nor restarts
// the clock.
func TestFakeSinkRepeatedTriggerChangesNothing(t *testing.T) {
	sink, clock := newClockedFakeSink()
	if err := sink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(20 * time.Minute)
	if err := sink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	requirePaged(t, sink, "first:key")
	clock.Advance(10 * time.Minute)
	sink.Tick()
	requirePaged(t, sink, "first:key", "second:key")
}

func TestFakeSinkResolvedPageNeverEscalatesAndCanOpenAgain(t *testing.T) {
	sink, clock := newClockedFakeSink()
	if err := sink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Resolve(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	sink.Tick()
	requirePaged(t, sink, "first:key")
	if got := sink.OpenKeys(); len(got) != 0 {
		t.Fatalf("open keys %v", got)
	}
	if err := sink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	requirePaged(t, sink, "first:key", "first:key")
}

func TestFakeSinkRefusesEveryCallWhileFailing(t *testing.T) {
	sink, _ := newClockedFakeSink()
	refusal := errors.New("synthetic outage")
	sink.Fail(refusal)
	if err := sink.Trigger(context.Background(), testPage("key")); !errors.Is(err, refusal) {
		t.Fatalf("trigger: %v", err)
	}
	if err := sink.Resolve(context.Background(), "key"); !errors.Is(err, refusal) {
		t.Fatalf("resolve: %v", err)
	}
	requirePaged(t, sink)
	sink.Fail(nil)
	if err := sink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	want := []FakeAttempt{{DedupKey: "key", Refused: true}, {Resolve: true, DedupKey: "key", Refused: true}, {DedupKey: "key"}}
	if got := sink.Attempts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("attempts %+v", got)
	}
}

func TestRetrySinkHandsEachEventToTheSink(t *testing.T) {
	sink, _ := newClockedFakeSink()
	retrySink := NewRetrySink(sink, RetrySinkSettings{})
	defer closeRetrySink(t, retrySink)
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	requireFlushed(t, retrySink)
	requirePaged(t, sink, "first:key")
	if err := retrySink.Resolve(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	requireFlushed(t, retrySink)
	if got := sink.OpenKeys(); len(got) != 0 {
		t.Fatalf("open keys %v", got)
	}
}

// BothAtOnce is carried through the queue.
func TestRetrySinkCarriesBothAtOnce(t *testing.T) {
	sink, _ := newClockedFakeSink()
	retrySink := NewRetrySink(sink, RetrySinkSettings{})
	defer closeRetrySink(t, retrySink)
	page := testPage("notice")
	page.BothAtOnce = true
	if err := retrySink.Trigger(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	requireFlushed(t, retrySink)
	requirePaged(t, sink, "first:notice", "second:notice")
}

// A sink stuck on one hand-off holds no caller.
func TestRetrySinkTriggerNeverWaitsOnTheSink(t *testing.T) {
	inner, _ := newClockedFakeSink()
	gated := newGatedSink(inner, func(_ bool, dedupKey string) bool { return dedupKey == "key-0" })
	retrySink := NewRetrySink(gated, RetrySinkSettings{})
	defer closeRetrySink(t, retrySink)
	if err := retrySink.Trigger(context.Background(), testPage("key-0")); err != nil {
		t.Fatal(err)
	}
	gated.requireHeld(t, "key-0")
	for i := 1; i < 100; i++ {
		if err := retrySink.Trigger(context.Background(), testPage(fmt.Sprintf("key-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	gated.answer(t, nil)
	requireFlushed(t, retrySink)
	if got := len(inner.OpenKeys()); got != 100 {
		t.Fatalf("%d pages reached the sink", got)
	}
}

// An outage loses nothing: the same key repeats until the sink takes it, and
// the failing state is told once each way.
func TestRetrySinkRetriesUntilTheSinkTakesThePage(t *testing.T) {
	sink, _ := newClockedFakeSink()
	sink.Fail(errors.New("synthetic outage"))
	steps := newRetrySteps()
	logs := &lockedLines{}
	failingStates := make(chan bool, 8)
	settings := steppedSettings(steps, logs)
	settings.DeliveryFailing = func(failing bool) { failingStates <- failing }
	retrySink := NewRetrySink(sink, settings)
	defer closeRetrySink(t, retrySink)

	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	steps.requireWait(t)
	if !<-failingStates {
		t.Fatal("the first failure did not say failing")
	}
	for range 3 {
		steps.proceed(t)
		steps.requireWait(t)
	}
	requirePaged(t, sink)
	sink.Fail(nil)
	steps.proceed(t)
	requireFlushed(t, retrySink)
	requirePaged(t, sink, "first:key")
	if failing := <-failingStates; failing {
		t.Fatal("the recovery said failing")
	}
	if got := attemptsOf(sink, "key"); !reflect.DeepEqual(got, []string{"trigger", "trigger", "trigger", "trigger", "trigger"}) {
		t.Fatalf("attempts %v", got)
	}
	if len(failingStates) != 0 {
		t.Fatal("the failing state was told more than once each way")
	}
	if !strings.Contains(logs.joined(), "synthetic outage") || !strings.Contains(logs.joined(), "after 4 failed") {
		t.Fatalf("logs:\n%s", logs.joined())
	}
}

// Failures back off to the cap, and a success starts the wait over.
func TestRetrySinkBacksOffToItsCapAndResets(t *testing.T) {
	sink, _ := newClockedFakeSink()
	sink.Fail(errors.New("synthetic outage"))
	steps := newRetrySteps()
	retrySink := NewRetrySink(sink, steppedSettings(steps, &lockedLines{}))
	defer closeRetrySink(t, retrySink)
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		if got := steps.requireWait(t); got != want {
			t.Fatalf("waited %s, want %s", got, want)
		}
		steps.proceed(t)
	}
	if got := steps.requireWait(t); got != 4*time.Second {
		t.Fatalf("waited %s", got)
	}
	sink.Fail(nil)
	steps.proceed(t)
	requireFlushed(t, retrySink)

	sink.Fail(errors.New("synthetic outage"))
	if err := retrySink.Trigger(context.Background(), testPage("other")); err != nil {
		t.Fatal(err)
	}
	if got := steps.requireWait(t); got != time.Second {
		t.Fatalf("after a success waited %s", got)
	}
	sink.Fail(nil)
	steps.proceed(t)
	requireFlushed(t, retrySink)
}

// A page that began and ended inside an outage still reaches the service, in
// order.
func TestRetrySinkKeepsTriggerBeforeResolveThroughAnOutage(t *testing.T) {
	sink, _ := newClockedFakeSink()
	sink.Fail(errors.New("synthetic outage"))
	steps := newRetrySteps()
	retrySink := NewRetrySink(sink, steppedSettings(steps, &lockedLines{}))
	defer closeRetrySink(t, retrySink)
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	steps.requireWait(t)
	if err := retrySink.Resolve(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	sink.Fail(nil)
	steps.proceed(t)
	requireFlushed(t, retrySink)
	requirePaged(t, sink, "first:key")
	if got := attemptsOf(sink, "key"); !reflect.DeepEqual(got, []string{"trigger", "trigger", "resolve"}) {
		t.Fatalf("attempts %v", got)
	}
	if got := sink.OpenKeys(); len(got) != 0 {
		t.Fatalf("open keys %v", got)
	}
}

// An incident that returns before its resolve went out keeps its open page,
// acknowledgment clock and all.
func TestRetrySinkCancelsAQueuedResolveWhenTheIncidentReturns(t *testing.T) {
	inner, _ := newClockedFakeSink()
	gated := newGatedSink(inner, func(_ bool, dedupKey string) bool { return dedupKey == "holder" })
	retrySink := NewRetrySink(gated, RetrySinkSettings{})
	defer closeRetrySink(t, retrySink)
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	requireFlushed(t, retrySink)
	// a hand-off in flight keeps the sender off the key's resolve
	if err := retrySink.Trigger(context.Background(), testPage("holder")); err != nil {
		t.Fatal(err)
	}
	gated.requireHeld(t, "holder")
	if err := retrySink.Resolve(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	gated.answer(t, nil)
	requireFlushed(t, retrySink)
	if got := attemptsOf(inner, "key"); !reflect.DeepEqual(got, []string{"trigger"}) {
		t.Fatalf("attempts %v", got)
	}
	if got := inner.OpenKeys(); !reflect.DeepEqual(got, []string{"holder", "key"}) {
		t.Fatalf("open keys %v", got)
	}
}

// A resolve in flight may land, so a trigger behind it must still go out.
func TestRetrySinkTriggerDuringAResolveInFlightReopensThePage(t *testing.T) {
	inner, _ := newClockedFakeSink()
	gated := newGatedSink(inner, func(resolve bool, _ string) bool { return resolve })
	retrySink := NewRetrySink(gated, RetrySinkSettings{})
	defer closeRetrySink(t, retrySink)
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	requireFlushed(t, retrySink)
	if err := retrySink.Resolve(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	gated.requireHeld(t, "key")
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	gated.answer(t, nil)
	requireFlushed(t, retrySink)
	requirePaged(t, inner, "first:key", "first:key")
	if got := inner.OpenKeys(); !reflect.DeepEqual(got, []string{"key"}) {
		t.Fatalf("open keys %v", got)
	}
}

// A second resolve queued behind one in flight must not let a trigger cancel
// it and leave the first to close a page that is wanted open.
func TestRetrySinkRepeatedResolveInFlightCannotSwallowATrigger(t *testing.T) {
	inner, _ := newClockedFakeSink()
	gated := newGatedSink(inner, func(resolve bool, _ string) bool { return resolve })
	retrySink := NewRetrySink(gated, RetrySinkSettings{})
	defer closeRetrySink(t, retrySink)
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	requireFlushed(t, retrySink)
	if err := retrySink.Resolve(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	gated.requireHeld(t, "key")
	if err := retrySink.Resolve(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	gated.answer(t, nil)
	requireFlushed(t, retrySink)
	if got := inner.OpenKeys(); !reflect.DeepEqual(got, []string{"key"}) {
		t.Fatalf("open keys %v", got)
	}
}

// A resolve that failed while the incident came back is not sent again: the
// page is wanted open, and the service still holds it.
func TestRetrySinkFailedResolveWithATriggerBehindItLeavesThePageOpen(t *testing.T) {
	inner, _ := newClockedFakeSink()
	gated := newGatedSink(inner, func(resolve bool, _ string) bool { return resolve })
	steps := newRetrySteps()
	retrySink := NewRetrySink(gated, steppedSettings(steps, &lockedLines{}))
	defer closeRetrySink(t, retrySink)
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	requireFlushed(t, retrySink)
	if err := retrySink.Resolve(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	gated.requireHeld(t, "key")
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	gated.answer(t, errors.New("synthetic outage"))
	steps.requireWait(t)
	steps.proceed(t)
	requireFlushed(t, retrySink)
	if got := attemptsOf(inner, "key"); !reflect.DeepEqual(got, []string{"trigger", "trigger"}) {
		t.Fatalf("attempts %v", got)
	}
	requirePaged(t, inner, "first:key")
	if got := inner.OpenKeys(); !reflect.DeepEqual(got, []string{"key"}) {
		t.Fatalf("open keys %v", got)
	}
}

// A sink refusing one page, as a bad body would, holds no other page behind it.
type refusingSink struct {
	inner   Sink
	refused string
}

func (self *refusingSink) Trigger(ctx context.Context, page Page) error {
	if page.DedupKey == self.refused {
		return errors.New("synthetic refusal")
	}
	return self.inner.Trigger(ctx, page)
}

func (self *refusingSink) Resolve(ctx context.Context, dedupKey string) error {
	return self.inner.Resolve(ctx, dedupKey)
}

func TestRetrySinkOneRefusedPageHoldsNoOther(t *testing.T) {
	inner, _ := newClockedFakeSink()
	steps := newRetrySteps()
	logs := &lockedLines{}
	retrySink := NewRetrySink(&refusingSink{inner: inner, refused: "poison"}, steppedSettings(steps, logs))
	if err := retrySink.Trigger(context.Background(), testPage("poison")); err != nil {
		t.Fatal(err)
	}
	steps.requireWait(t)
	if err := retrySink.Trigger(context.Background(), testPage("healthy")); err != nil {
		t.Fatal(err)
	}
	// the refused page queued first, so it fails once more and goes to the back
	steps.proceed(t)
	steps.requireWait(t)
	requirePaged(t, inner)
	// then the healthy page goes through, with the refused one still failing
	steps.proceed(t)
	steps.requireWait(t)
	requirePaged(t, inner, "first:healthy")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retrySink.Close(ctx)
	if !strings.Contains(logs.joined(), "1 page(s) still held") {
		t.Fatalf("logs:\n%s", logs.joined())
	}
}

// Flapping through an outage holds one slot and sends one trigger and one
// resolve, not one per flap.
func TestRetrySinkHoldsOneSlotPerKeyThroughFlapping(t *testing.T) {
	inner, _ := newClockedFakeSink()
	gated := newGatedSink(inner, func(_ bool, dedupKey string) bool { return dedupKey == "holder" })
	retrySink := NewRetrySink(gated, RetrySinkSettings{})
	defer closeRetrySink(t, retrySink)
	if err := retrySink.Trigger(context.Background(), testPage("holder")); err != nil {
		t.Fatal(err)
	}
	gated.requireHeld(t, "holder")
	for range 10000 {
		if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
			t.Fatal(err)
		}
		if err := retrySink.Resolve(context.Background(), "key"); err != nil {
			t.Fatal(err)
		}
	}
	func() {
		retrySink.stateLock.Lock()
		defer retrySink.stateLock.Unlock()
		if len(retrySink.keySlots) != 2 || len(retrySink.queuedKeys) != 1 {
			t.Fatalf("%d slots, %d queued", len(retrySink.keySlots), len(retrySink.queuedKeys))
		}
	}()
	gated.answer(t, nil)
	requireFlushed(t, retrySink)
	if got := attemptsOf(inner, "key"); !reflect.DeepEqual(got, []string{"trigger", "resolve"}) {
		t.Fatalf("attempts %v", got)
	}
}

func TestRetrySinkFlushEndsWithItsContext(t *testing.T) {
	inner, _ := newClockedFakeSink()
	gated := newGatedSink(inner, func(bool, string) bool { return true })
	retrySink := NewRetrySink(gated, RetrySinkSettings{})
	defer closeRetrySink(t, retrySink)
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	gated.requireHeld(t, "key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := retrySink.Flush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("flush: %v", err)
	}
	gated.answer(t, nil)
	requireFlushed(t, retrySink)
}

// Close hands over what is queued, retries included, then refuses more.
func TestRetrySinkCloseDrainsThenRefuses(t *testing.T) {
	sink, _ := newClockedFakeSink()
	sink.Fail(errors.New("synthetic outage"))
	steps := newRetrySteps()
	retrySink := NewRetrySink(sink, steppedSettings(steps, &lockedLines{}))
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	steps.requireWait(t)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		closeRetrySink(t, retrySink)
	}()
	// Close is not done while the page is unsent, whenever it took the lock
	select {
	case <-closed:
		t.Fatal("Close returned with a page unsent")
	default:
	}
	sink.Fail(nil)
	steps.proceed(t)
	select {
	case <-closed:
	case <-time.After(testWait):
		t.Fatal("Close did not finish")
	}
	requirePaged(t, sink, "first:key")
	if err := retrySink.Trigger(context.Background(), testPage("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("trigger after close: %v", err)
	}
	if err := retrySink.Resolve(context.Background(), "late"); !errors.Is(err, ErrClosed) {
		t.Fatalf("resolve after close: %v", err)
	}
}

// Close's deadline ends the retries and says what it left behind. A second
// Close says nothing more.
func TestRetrySinkCloseDeadlineAbandonsAndLogs(t *testing.T) {
	sink, _ := newClockedFakeSink()
	sink.Fail(errors.New("synthetic outage"))
	steps := newRetrySteps()
	logs := &lockedLines{}
	retrySink := NewRetrySink(sink, steppedSettings(steps, logs))
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	steps.requireWait(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retrySink.Close(ctx)
	retrySink.Close(ctx)
	if got := strings.Count(logs.joined(), "1 page(s) still held"); got != 1 {
		t.Fatalf("abandoned lines %d:\n%s", got, logs.joined())
	}
}

func TestRetrySinkRefusesABadPageAtOnce(t *testing.T) {
	sink, _ := newClockedFakeSink()
	retrySink := NewRetrySink(sink, RetrySinkSettings{})
	defer closeRetrySink(t, retrySink)
	if retrySink.Trigger(context.Background(), testPage("")) == nil {
		t.Fatal("a page with no key was queued")
	}
	if retrySink.Resolve(context.Background(), "") == nil {
		t.Fatal("a resolve with no key was queued")
	}
	requireFlushed(t, retrySink)
	if got := sink.Attempts(); len(got) != 0 {
		t.Fatalf("attempts %+v", got)
	}
}

// What a post carried.
type recordedPost struct {
	contentType string
	body        map[string]any
}

// A loopback endpoint that records posts and answers status.
func newRecordingEndpoint(t *testing.T, status int) (*httptest.Server, func() []recordedPost) {
	t.Helper()
	var stateLock sync.Mutex
	posts := []recordedPost{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body := map[string]any{}
		data, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(data, &body)
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			posts = append(posts, recordedPost{contentType: request.Header.Get("Content-Type"), body: body})
		}()
		response.WriteHeader(status)
	}))
	t.Cleanup(endpoint.Close)
	return endpoint, func() []recordedPost {
		stateLock.Lock()
		defer stateLock.Unlock()
		return append([]recordedPost(nil), posts...)
	}
}

func newLoopbackWebhookSink(t *testing.T, url string) *WebhookSink {
	t.Helper()
	settings := DefaultWebhookSinkSettings()
	settings.Url = url
	sink, err := NewWebhookSink(settings)
	if err != nil {
		t.Fatal(err)
	}
	return sink
}

func TestWebhookSinkPostsTriggersAndResolves(t *testing.T) {
	endpoint, posts := newRecordingEndpoint(t, http.StatusAccepted)
	sink := newLoopbackWebhookSink(t, endpoint.URL+"/events/"+syntheticWebhookSecret)
	page := testPage("key")
	page.BothAtOnce = true
	if err := sink.Trigger(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	if err := sink.Resolve(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	got := posts()
	if len(got) != 2 || got[0].contentType != "application/json" {
		t.Fatalf("posts %+v", got)
	}
	wantTrigger := map[string]any{
		"action": "trigger", "dedup_key": "key", "class": "synthetic-class", "summary": "synthetic summary",
		"time": "2026-11-07T02:00:00Z", "both_at_once": true,
	}
	if !reflect.DeepEqual(got[0].body, wantTrigger) {
		t.Fatalf("trigger body %v", got[0].body)
	}
	if !reflect.DeepEqual(got[1].body, map[string]any{"action": "resolve", "dedup_key": "key"}) {
		t.Fatalf("resolve body %v", got[1].body)
	}
}

// Fails closed: with no service chosen and no §2.8 row, a page cannot leave the
// site, however the endpoint is configured.
func TestWebhookSinkRefusesEveryEndpointOffLoopback(t *testing.T) {
	for _, webhookUrl := range []string{
		"https://pager.example/events/" + syntheticWebhookSecret,
		"http://pager.example/events/" + syntheticWebhookSecret,
		"https://192.0.2.1/events/" + syntheticWebhookSecret,
		"https://[2001:db8::1]/events/" + syntheticWebhookSecret,
		"https://localhost.pager.example/events/" + syntheticWebhookSecret,
	} {
		sink, err := NewWebhookSink(WebhookSinkSettings{Url: webhookUrl})
		if sink != nil || !errors.Is(err, ErrNoPagerVendor) {
			t.Errorf("%s: sink %v, err %v", webhookUrl, sink, err)
		}
		if err != nil && strings.Contains(err.Error(), syntheticWebhookSecret) {
			t.Errorf("%s: the refusal holds the endpoint: %v", webhookUrl, err)
		}
	}
}

func TestWebhookSinkRefusesAnEndpointItCannotUse(t *testing.T) {
	for _, c := range []struct {
		webhookUrl string
		want       error
	}{
		{webhookUrl: "", want: ErrNoWebhookUrl},
		{webhookUrl: " \n", want: ErrNoWebhookUrl},
		{webhookUrl: "not a url", want: ErrBadWebhookUrl},
		{webhookUrl: "/events/" + syntheticWebhookSecret, want: ErrBadWebhookUrl},
		{webhookUrl: "ftp://127.0.0.1/events", want: ErrBadWebhookUrl},
		{webhookUrl: "http://user:" + syntheticWebhookSecret + "@127.0.0.1/events", want: ErrBadWebhookUrl},
		{webhookUrl: "http://[::1/events", want: ErrBadWebhookUrl},
	} {
		sink, err := NewWebhookSink(WebhookSinkSettings{Url: c.webhookUrl})
		if sink != nil || !errors.Is(err, c.want) {
			t.Errorf("%q: sink %v, err %v, want %v", c.webhookUrl, sink, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), syntheticWebhookSecret) {
			t.Errorf("%q: the refusal holds the endpoint: %v", c.webhookUrl, err)
		}
	}
}

func TestWebhookSinkAcceptsEveryLoopbackName(t *testing.T) {
	for _, webhookUrl := range []string{"http://127.0.0.1:50001/events", "https://localhost:50001/events", "http://[::1]:50001/events", " http://127.0.0.1/events\n"} {
		if _, err := NewWebhookSink(WebhookSinkSettings{Url: webhookUrl}); err != nil {
			t.Errorf("%q: %v", webhookUrl, err)
		}
	}
}

func TestWebhookSinkFailsEveryAnswerOutside2xx(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		endpoint, _ := newRecordingEndpoint(t, status)
		sink := newLoopbackWebhookSink(t, endpoint.URL+"/events/"+syntheticWebhookSecret)
		err := sink.Trigger(context.Background(), testPage("key"))
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("http-%d", status)) || strings.Contains(err.Error(), syntheticWebhookSecret) {
			t.Errorf("status %d: %v", status, err)
		}
	}
}

// net/http errors embed the URL, so a failed post is reduced to a fixed class.
func TestWebhookSinkKeepsTheEndpointOutOfErrors(t *testing.T) {
	endpoint, _ := newRecordingEndpoint(t, http.StatusOK)
	sink := newLoopbackWebhookSink(t, endpoint.URL+"/events/"+syntheticWebhookSecret)

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancelExpired()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		ctx  context.Context
		want string
	}{
		{ctx: expired, want: "(timeout)"},
		{ctx: canceled, want: "(canceled)"},
	} {
		err := sink.Trigger(c.ctx, testPage("key"))
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), syntheticWebhookSecret) {
			t.Errorf("%s: %v", c.want, err)
		}
	}

	endpoint.Close()
	err := sink.Resolve(context.Background(), "key")
	if err == nil || strings.Contains(err.Error(), syntheticWebhookSecret) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("closed endpoint: %v", err)
	}
}

// A redirect would send a page to another host.
func TestWebhookSinkDoesNotFollowARedirect(t *testing.T) {
	elsewhere, posts := newRecordingEndpoint(t, http.StatusOK)
	redirecting := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, elsewhere.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	sink := newLoopbackWebhookSink(t, redirecting.URL+"/events")
	err := sink.Trigger(context.Background(), testPage("key"))
	if err == nil || !strings.Contains(err.Error(), "http-307") {
		t.Fatalf("redirect: %v", err)
	}
	if got := posts(); len(got) != 0 {
		t.Fatalf("the page followed a redirect: %+v", got)
	}
}

func TestWebhookSinkScrubsAddressesFromTheSummary(t *testing.T) {
	endpoint, posts := newRecordingEndpoint(t, http.StatusOK)
	sink := newLoopbackWebhookSink(t, endpoint.URL+"/events")
	page := testPage("key")
	page.Summary = "reached 192.0.2.7 and 2001:db8::7"
	if err := sink.Trigger(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	summary := fmt.Sprint(posts()[0].body["summary"])
	if strings.Contains(summary, "192.0.2.7") || strings.Contains(summary, "2001:db8::7") {
		t.Fatalf("summary %q", summary)
	}
}

func TestWebhookSinkRefusesABadPageBeforeThePost(t *testing.T) {
	endpoint, posts := newRecordingEndpoint(t, http.StatusOK)
	sink := newLoopbackWebhookSink(t, endpoint.URL+"/events")
	if sink.Trigger(context.Background(), testPage("")) == nil {
		t.Fatal("a page with no key was posted")
	}
	if sink.Resolve(context.Background(), "two words") == nil {
		t.Fatal("a resolve with a bad key was posted")
	}
	if got := posts(); len(got) != 0 {
		t.Fatalf("posts %+v", got)
	}
}

// The whole client against the service model: an outage answers 503, the sender
// retries the same key, and the rota's policy runs on what arrived.
func TestWebhookThroughRetrySinkReachesTheRotaAfterAnOutage(t *testing.T) {
	service, clock := newClockedFakeSink()
	endpoint := httptest.NewServer(service)
	defer endpoint.Close()
	service.Fail(errors.New("synthetic outage"))
	steps := newRetrySteps()
	retrySink := NewRetrySink(newLoopbackWebhookSink(t, endpoint.URL+"/events"), steppedSettings(steps, &lockedLines{}))
	defer closeRetrySink(t, retrySink)
	if err := retrySink.Trigger(context.Background(), testPage("key")); err != nil {
		t.Fatal(err)
	}
	steps.requireWait(t)
	requirePaged(t, service)
	service.Fail(nil)
	steps.proceed(t)
	requireFlushed(t, retrySink)
	requirePaged(t, service, "first:key")
	if got := service.Notifications()[0].Page; got != testPage("key") {
		t.Fatalf("page %+v", got)
	}
	clock.Advance(30 * time.Minute)
	service.Tick()
	requirePaged(t, service, "first:key", "second:key")
	if err := retrySink.Resolve(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	requireFlushed(t, retrySink)
	if got := service.OpenKeys(); len(got) != 0 {
		t.Fatalf("open keys %v", got)
	}
}

func TestFakeSinkAnswersAWebhookItCannotRead(t *testing.T) {
	service, _ := newClockedFakeSink()
	endpoint := httptest.NewServer(service)
	defer endpoint.Close()
	for _, c := range []struct {
		body string
		want int
	}{
		{body: "not json", want: http.StatusBadRequest},
		{body: `{"action":"page","dedup_key":"key"}`, want: http.StatusServiceUnavailable},
		{body: `{"action":"trigger","dedup_key":"key","class":"c","summary":"s","time":"yesterday"}`, want: http.StatusServiceUnavailable},
	} {
		response, err := http.Post(endpoint.URL, "application/json", strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.body, response.StatusCode, c.want)
		}
	}
	requirePaged(t, service)
}
