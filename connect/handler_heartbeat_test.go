// The heartbeat's retirement decision (handler_heartbeat.go), driven round by
// round over a scripted store: a retired process holds its transports while
// the database does not answer, restarts its confirmation on every unanswered
// read, spreads its shutdown over the jitter, resumes if the row is back, and
// shuts down whenever its loop ends.
package connect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var errHandlerHeartbeatTestGone = errors.New("Handler does not exist.")

var errHandlerHeartbeatTestUnanswered = errors.New("timeout: context deadline exceeded")

// A scripted store: heartbeat and readExists answer from their queues, the
// last answer repeating.
type handlerHeartbeatTestStore struct {
	heartbeatErrs       []error
	heartbeatPanics     []bool
	existsAnswers       []bool
	existsErrs          []error
	heartbeatCallCount  int
	readExistsCallCount int
	readDeadlines       []time.Duration
}

func (self *handlerHeartbeatTestStore) heartbeat(ctx context.Context, handlerId server.Id) error {
	n := self.heartbeatCallCount
	self.heartbeatCallCount += 1
	if n < len(self.heartbeatPanics) && self.heartbeatPanics[n] {
		panic(errHandlerHeartbeatTestUnanswered)
	}
	return self.heartbeatErrs[min(n, len(self.heartbeatErrs)-1)]
}

func (self *handlerHeartbeatTestStore) readExists(ctx context.Context, handlerId server.Id) (bool, error) {
	n := self.readExistsCallCount
	self.readExistsCallCount += 1
	if deadline, ok := ctx.Deadline(); ok {
		self.readDeadlines = append(self.readDeadlines, time.Until(deadline))
	} else {
		self.readDeadlines = append(self.readDeadlines, -1)
	}
	return self.existsAnswers[min(n, len(self.existsAnswers)-1)], self.existsErrs[min(n, len(self.existsErrs)-1)]
}

func handlerHeartbeatTestSettings() *HandlerHeartbeatSettings {
	return &HandlerHeartbeatSettings{
		HeartbeatInterval:       time.Millisecond,
		RetirementConfirmations: 3,
		ConfirmationTimeout:     2 * time.Second,
		ShutdownJitter:          30 * time.Second,
	}
}

func newHandlerHeartbeatForTest(store *handlerHeartbeatTestStore, jitter time.Duration, shutdown func()) *handlerHeartbeat {
	return newHandlerHeartbeatWithStore(
		context.Background(),
		server.NewId(),
		handlerHeartbeatTestSettings(),
		store.heartbeat,
		store.readExists,
		func(maxDelay time.Duration) time.Duration {
			return min(jitter, maxDelay)
		},
		shutdown,
	)
}

func handlerRetirementCount(event string) float64 {
	return testutil.ToFloat64(handlerRetirementCounter.WithLabelValues(event))
}

// The 2026-10-19 shape: expiry deleted the handler while the database path was
// failing, and a heartbeat got through. The process must keep its transports
// for as long as the database does not answer, and shut down only after three
// answered reads still find the row gone.
func TestHandlerHeartbeatHoldsARetiredProcessWhileTheDatabaseDoesNotAnswer(t *testing.T) {
	store := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{errHandlerHeartbeatTestGone},
		existsAnswers: []bool{false},
		existsErrs:    []error{errHandlerHeartbeatTestUnanswered},
	}
	heartbeat := newHandlerHeartbeatForTest(store, 0, func() {})
	detectedBefore := handlerRetirementCount(handlerRetirementDetected)
	unansweredBefore := handlerRetirementCount(handlerRetirementUnanswered)
	now := time.Unix(1_800_000_000, 0)

	if heartbeat.round(now) {
		t.Fatal("the heartbeat that found the handler row gone shut the process down at once")
	}
	for i := range 100 {
		if heartbeat.round(now.Add(time.Duration(i+1) * 5 * time.Second)) {
			t.Fatalf("round %d shut the process down while the database did not answer", i+1)
		}
	}
	if got := handlerRetirementCount(handlerRetirementDetected) - detectedBefore; got != 1 {
		t.Fatalf("detected %v retirements, want 1", got)
	}
	if got := handlerRetirementCount(handlerRetirementUnanswered) - unansweredBefore; got != 100 {
		t.Fatalf("counted %v unanswered reads, want 100", got)
	}
	if store.heartbeatCallCount != 1 {
		t.Fatalf("a retired handler heartbeat %d times, want only the one that found it gone", store.heartbeatCallCount)
	}

	// the database answers again
	store.existsErrs = []error{nil}
	store.readExistsCallCount = 0
	shutdownBefore := handlerRetirementCount(handlerRetirementShutdown)
	recovered := now.Add(time.Hour)
	if heartbeat.round(recovered) || heartbeat.round(recovered.Add(5*time.Second)) {
		t.Fatal("the process shut down before three answered reads")
	}
	if !heartbeat.round(recovered.Add(10 * time.Second)) {
		t.Fatal("three answered reads found the row gone and the process did not shut down")
	}
	if got := handlerRetirementCount(handlerRetirementShutdown) - shutdownBefore; got != 1 {
		t.Fatalf("counted %v shutdowns, want 1", got)
	}
}

// An unanswered read between answered ones starts the count again.
func TestHandlerHeartbeatRestartsTheConfirmationOnAnUnansweredRead(t *testing.T) {
	store := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{errHandlerHeartbeatTestGone},
		existsAnswers: []bool{false},
		existsErrs:    []error{nil, nil, errHandlerHeartbeatTestUnanswered, nil, nil, nil},
	}
	heartbeat := newHandlerHeartbeatForTest(store, 0, func() {})
	now := time.Unix(1_800_000_000, 0)

	shutdowns := []bool{}
	for i := range 7 {
		shutdowns = append(shutdowns, heartbeat.round(now.Add(time.Duration(i)*5*time.Second)))
	}
	// detect, answered, answered, unanswered, answered, answered, answered
	want := []bool{false, false, false, false, false, false, true}
	for i := range want {
		if shutdowns[i] != want[i] {
			t.Fatalf("round %d shut down %t, want %t (rounds %v)", i, shutdowns[i], want[i], shutdowns)
		}
	}
}

// The shutdown waits its jitter after the last confirmation, and an unanswered
// read in the wait starts the confirmation again.
func TestHandlerHeartbeatSpreadsTheShutdownOverTheJitter(t *testing.T) {
	store := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{errHandlerHeartbeatTestGone},
		existsAnswers: []bool{false},
		existsErrs:    []error{nil},
	}
	heartbeat := newHandlerHeartbeatForTest(store, 20*time.Second, func() {})
	now := time.Unix(1_800_000_000, 0)

	heartbeat.round(now)
	heartbeat.round(now.Add(5 * time.Second))
	heartbeat.round(now.Add(10 * time.Second))
	confirmed := now.Add(15 * time.Second)
	if heartbeat.round(confirmed) {
		t.Fatal("the process shut down without waiting its jitter")
	}
	if heartbeat.round(confirmed.Add(19 * time.Second)) {
		t.Fatal("the process shut down before its jitter elapsed")
	}

	store.existsErrs = []error{errHandlerHeartbeatTestUnanswered}
	store.readExistsCallCount = 0
	if heartbeat.round(confirmed.Add(20 * time.Second)) {
		t.Fatal("the process shut down on an unanswered read")
	}
	store.existsErrs = []error{nil}
	store.readExistsCallCount = 0
	restarted := confirmed.Add(25 * time.Second)
	if heartbeat.round(restarted) || heartbeat.round(restarted.Add(5*time.Second)) || heartbeat.round(restarted.Add(10*time.Second)) {
		t.Fatal("the process shut down before a new confirmation and its jitter")
	}
	if !heartbeat.round(restarted.Add(30 * time.Second)) {
		t.Fatal("the process did not shut down after a new confirmation and its jitter")
	}
}

// A row that is back means nothing retired the handler: heartbeats resume.
func TestHandlerHeartbeatResumesWhenTheRowIsBack(t *testing.T) {
	store := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{errHandlerHeartbeatTestGone, nil},
		existsAnswers: []bool{true},
		existsErrs:    []error{nil},
	}
	heartbeat := newHandlerHeartbeatForTest(store, 0, func() {})
	restoredBefore := handlerRetirementCount(handlerRetirementRestored)
	now := time.Unix(1_800_000_000, 0)

	for i := range 6 {
		if heartbeat.round(now.Add(time.Duration(i) * 5 * time.Second)) {
			t.Fatalf("round %d shut the process down although its row is back", i)
		}
	}
	if heartbeat.retired {
		t.Fatal("the heartbeat stayed retired after its row was back")
	}
	if store.heartbeatCallCount != 5 {
		t.Fatalf("heartbeat %d times, want 5: one that found the row gone, then every round after the row was back", store.heartbeatCallCount)
	}
	if got := handlerRetirementCount(handlerRetirementRestored) - restoredBefore; got != 1 {
		t.Fatalf("counted %v restorations, want 1", got)
	}
}

// A raised heartbeat failure is transient: the process neither retires nor
// shuts down, and heartbeats again next round.
func TestHandlerHeartbeatRetriesARaisedHeartbeat(t *testing.T) {
	store := &handlerHeartbeatTestStore{
		heartbeatErrs:   []error{nil},
		heartbeatPanics: []bool{true, true, false},
		existsAnswers:   []bool{false},
		existsErrs:      []error{nil},
	}
	heartbeat := newHandlerHeartbeatForTest(store, 0, func() {})
	now := time.Unix(1_800_000_000, 0)

	for i := range 3 {
		if heartbeat.round(now.Add(time.Duration(i) * 5 * time.Second)) {
			t.Fatalf("round %d shut the process down on a raised heartbeat", i)
		}
	}
	if heartbeat.retired || store.readExistsCallCount != 0 {
		t.Fatal("a raised heartbeat retired the handler")
	}
	if store.heartbeatCallCount != 3 {
		t.Fatalf("heartbeat %d times, want 3", store.heartbeatCallCount)
	}
}

// Each confirmation read is bounded by the confirmation timeout.
func TestHandlerHeartbeatBoundsEachConfirmationRead(t *testing.T) {
	store := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{errHandlerHeartbeatTestGone},
		existsAnswers: []bool{false},
		existsErrs:    []error{errHandlerHeartbeatTestUnanswered},
	}
	heartbeat := newHandlerHeartbeatForTest(store, 0, func() {})
	now := time.Unix(1_800_000_000, 0)

	for i := range 4 {
		heartbeat.round(now.Add(time.Duration(i) * 5 * time.Second))
	}
	if len(store.readDeadlines) != 3 {
		t.Fatalf("read %d times, want 3", len(store.readDeadlines))
	}
	for i, remaining := range store.readDeadlines {
		if remaining <= 0 || handlerHeartbeatTestSettings().ConfirmationTimeout < remaining {
			t.Fatalf("read %d had %v left, want a deadline within the confirmation timeout", i, remaining)
		}
	}
}

// The loop shuts the process down when its context ends, and when a round
// confirms the retirement.
func TestHandlerHeartbeatLoopShutsDownWhenItEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	shutdown := make(chan struct{})
	heartbeat := newHandlerHeartbeatWithStore(
		ctx,
		server.NewId(),
		handlerHeartbeatTestSettings(),
		func(ctx context.Context, handlerId server.Id) error { return nil },
		func(ctx context.Context, handlerId server.Id) (bool, error) { return true, nil },
		func(maxDelay time.Duration) time.Duration { return 0 },
		func() { close(shutdown) },
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		heartbeat.run()
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not end with its context")
	}
	select {
	case <-shutdown:
	default:
		t.Fatal("the loop ended without shutting the process down")
	}

	confirmedShutdown := make(chan struct{})
	store := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{errHandlerHeartbeatTestGone},
		existsAnswers: []bool{false},
		existsErrs:    []error{nil},
	}
	confirmed := newHandlerHeartbeatWithStore(
		context.Background(),
		server.NewId(),
		handlerHeartbeatTestSettings(),
		store.heartbeat,
		store.readExists,
		func(maxDelay time.Duration) time.Duration { return 0 },
		func() { close(confirmedShutdown) },
	)
	go confirmed.run()
	select {
	case <-confirmedShutdown:
	case <-time.After(10 * time.Second):
		t.Fatal("a confirmed retirement did not shut the process down")
	}
	if store.readExistsCallCount != 3 {
		t.Fatalf("shut down after %d confirmation reads, want 3", store.readExistsCallCount)
	}
}

// The defaults keep the heartbeat at the pace connect always used, bound each
// confirmation read within one round, and spread a shutdown over no more than
// one heartbeat timeout.
func TestDefaultHandlerHeartbeatSettings(t *testing.T) {
	settings := DefaultHandlerHeartbeatSettings()
	if want := min(5*time.Second, model.NetworkClientHandlerHeartbeatTimeout/2); settings.HeartbeatInterval != want {
		t.Fatalf("heartbeat interval %v, want %v", settings.HeartbeatInterval, want)
	}
	if settings.RetirementConfirmations < 2 {
		t.Fatalf("%d confirmations: one answered read cannot confirm a retirement", settings.RetirementConfirmations)
	}
	if settings.ConfirmationTimeout <= 0 || settings.HeartbeatInterval < settings.ConfirmationTimeout {
		t.Fatalf("confirmation timeout %v, want within the heartbeat interval %v", settings.ConfirmationTimeout, settings.HeartbeatInterval)
	}
	if settings.ShutdownJitter <= 0 || model.NetworkClientHandlerHeartbeatTimeout < settings.ShutdownJitter {
		t.Fatalf("shutdown jitter %v, want positive and at most the heartbeat timeout", settings.ShutdownJitter)
	}
}

// Unset settings fall back to the defaults instead of spinning the loop or
// shutting down on no evidence.
func TestHandlerHeartbeatFallsBackToTheDefaultSettings(t *testing.T) {
	store := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{nil},
		existsAnswers: []bool{true},
		existsErrs:    []error{nil},
	}
	for _, settings := range []*HandlerHeartbeatSettings{
		nil,
		{},
		{HeartbeatInterval: time.Second, RetirementConfirmations: 0, ConfirmationTimeout: time.Second},
		{HeartbeatInterval: time.Second, RetirementConfirmations: 3, ConfirmationTimeout: 0},
	} {
		heartbeat := newHandlerHeartbeatWithStore(
			context.Background(),
			server.NewId(),
			settings,
			store.heartbeat,
			store.readExists,
			func(maxDelay time.Duration) time.Duration { return 0 },
			func() {},
		)
		if *heartbeat.settings != *DefaultHandlerHeartbeatSettings() {
			t.Errorf("settings %+v fell back to %+v, want the defaults", settings, *heartbeat.settings)
		}
	}
}
