// The heartbeat's retirement decision (handler_heartbeat.go), driven round by
// round over a scripted store: a retired process holds its transports while
// the database does not answer, restarts its confirmation on every unanswered
// read, spreads its shutdown over the jitter, resumes if the row is back, and
// shuts down whenever its loop ends. Each test reads the shutdown where it
// shows: the store counts the calls the connect router's cancel would take.
package connect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The error a heartbeat returns for a handler row that expiry deleted.
var errHandlerHeartbeatTestGone = errors.New("Handler does not exist.")

// The error of a read the database did not answer.
var errHandlerHeartbeatTestUnanswered = errors.New("timeout: context deadline exceeded")

// A scripted store and shutdown: heartbeat and readExists answer from their
// queues, the last answer repeating, and shutdown counts its calls.
type handlerHeartbeatTestStore struct {
	heartbeatErrs       []error
	heartbeatPanics     []bool
	existsAnswers       []bool
	existsErrs          []error
	heartbeatCallCount  int
	readExistsCallCount int
	// each read's deadline, zero for none
	readDeadlines []time.Time
	shutdownCount int
}

// The next scripted heartbeat: a raise where heartbeatPanics says so, else the
// next error.
func (self *handlerHeartbeatTestStore) heartbeat(ctx context.Context, handlerId server.Id) error {
	n := self.heartbeatCallCount
	self.heartbeatCallCount += 1
	if n < len(self.heartbeatPanics) && self.heartbeatPanics[n] {
		panic(errHandlerHeartbeatTestUnanswered)
	}
	return self.heartbeatErrs[min(n, len(self.heartbeatErrs)-1)]
}

// The next scripted confirmation read. Records the read's deadline.
func (self *handlerHeartbeatTestStore) readExists(ctx context.Context, handlerId server.Id) (bool, error) {
	n := self.readExistsCallCount
	self.readExistsCallCount += 1
	deadline, _ := ctx.Deadline()
	self.readDeadlines = append(self.readDeadlines, deadline)
	return self.existsAnswers[min(n, len(self.existsAnswers)-1)], self.existsErrs[min(n, len(self.existsErrs)-1)]
}

// Counts a shutdown of the process.
func (self *handlerHeartbeatTestStore) shutdown() {
	self.shutdownCount += 1
}

// Three confirmations, each read bounded by two seconds, and a thirty second
// jitter; rounds are driven directly, so the interval only paces a loop.
func handlerHeartbeatTestSettings() *HandlerHeartbeatSettings {
	return &HandlerHeartbeatSettings{
		HeartbeatInterval:       time.Millisecond,
		RetirementConfirmations: 3,
		ConfirmationTimeout:     2 * time.Second,
		ShutdownJitter:          30 * time.Second,
	}
}

// A heartbeat over store whose jitter is always jitter, within the setting.
func newHandlerHeartbeatForTest(store *handlerHeartbeatTestStore, jitter time.Duration) *handlerHeartbeat {
	return newHandlerHeartbeatWithStore(
		context.Background(),
		server.NewId(),
		handlerHeartbeatTestSettings(),
		store.heartbeat,
		store.readExists,
		func(maxDelay time.Duration) time.Duration {
			return min(jitter, maxDelay)
		},
		store.shutdown,
	)
}

// The retirement metric's count for event.
func handlerRetirementCount(event string) float64 {
	return testutil.ToFloat64(handlerRetirementCounter.WithLabelValues(event))
}

// The 2026-10-19 shape: expiry deleted the handler while the database path was
// failing, and a heartbeat got through. The process must keep its transports
// for as long as the database does not answer, and shut down once three
// answered reads still find the row gone.
func TestHandlerHeartbeatHoldsARetiredProcessWhileTheDatabaseDoesNotAnswer(t *testing.T) {
	store := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{errHandlerHeartbeatTestGone},
		existsAnswers: []bool{false},
		existsErrs:    []error{errHandlerHeartbeatTestUnanswered},
	}
	heartbeat := newHandlerHeartbeatForTest(store, 0)
	detectedBefore := handlerRetirementCount(handlerRetirementDetected)
	unansweredBefore := handlerRetirementCount(handlerRetirementUnanswered)
	now := time.Unix(1_800_000_000, 0)

	heartbeat.round(now)
	if store.shutdownCount != 0 {
		t.Fatal("the heartbeat that found the handler row gone shut the process down at once")
	}
	for i := range 100 {
		heartbeat.round(now.Add(time.Duration(i+1) * 5 * time.Second))
		if store.shutdownCount != 0 {
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
	heartbeat.round(recovered)
	heartbeat.round(recovered.Add(5 * time.Second))
	if store.shutdownCount != 0 {
		t.Fatal("the process shut down before three answered reads")
	}
	heartbeat.round(recovered.Add(10 * time.Second))
	if store.shutdownCount != 1 {
		t.Fatalf("three answered reads found the row gone and the process shut down %d times, want once", store.shutdownCount)
	}
	heartbeat.round(recovered.Add(15 * time.Second))
	if store.shutdownCount != 1 || store.readExistsCallCount != 3 {
		t.Fatal("a round after the shutdown read or shut down again")
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
	heartbeat := newHandlerHeartbeatForTest(store, 0)
	now := time.Unix(1_800_000_000, 0)

	// detect, answered, answered, unanswered, answered, answered, answered
	wantShutdownCounts := []int{0, 0, 0, 0, 0, 0, 1}
	for i, wantShutdownCount := range wantShutdownCounts {
		heartbeat.round(now.Add(time.Duration(i) * 5 * time.Second))
		if store.shutdownCount != wantShutdownCount {
			t.Fatalf("after round %d the process shut down %d times, want %d", i, store.shutdownCount, wantShutdownCount)
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
	heartbeat := newHandlerHeartbeatForTest(store, 20*time.Second)
	now := time.Unix(1_800_000_000, 0)

	heartbeat.round(now)
	heartbeat.round(now.Add(5 * time.Second))
	heartbeat.round(now.Add(10 * time.Second))
	confirmed := now.Add(15 * time.Second)
	heartbeat.round(confirmed)
	if store.shutdownCount != 0 {
		t.Fatal("the process shut down without waiting its jitter")
	}
	heartbeat.round(confirmed.Add(19 * time.Second))
	if store.shutdownCount != 0 {
		t.Fatal("the process shut down before its jitter elapsed")
	}

	store.existsErrs = []error{errHandlerHeartbeatTestUnanswered}
	store.readExistsCallCount = 0
	heartbeat.round(confirmed.Add(20 * time.Second))
	if store.shutdownCount != 0 {
		t.Fatal("the process shut down on an unanswered read")
	}
	store.existsErrs = []error{nil}
	store.readExistsCallCount = 0
	restarted := confirmed.Add(25 * time.Second)
	heartbeat.round(restarted)
	heartbeat.round(restarted.Add(5 * time.Second))
	heartbeat.round(restarted.Add(10 * time.Second))
	if store.shutdownCount != 0 {
		t.Fatal("the process shut down before a new confirmation and its jitter")
	}
	heartbeat.round(restarted.Add(30 * time.Second))
	if store.shutdownCount != 1 {
		t.Fatalf("after a new confirmation and its jitter the process shut down %d times, want once", store.shutdownCount)
	}
}

// A row that is back means nothing retired the handler: heartbeats resume.
func TestHandlerHeartbeatResumesWhenTheRowIsBack(t *testing.T) {
	store := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{errHandlerHeartbeatTestGone, nil},
		existsAnswers: []bool{true},
		existsErrs:    []error{nil},
	}
	heartbeat := newHandlerHeartbeatForTest(store, 0)
	restoredBefore := handlerRetirementCount(handlerRetirementRestored)
	now := time.Unix(1_800_000_000, 0)

	for i := range 6 {
		heartbeat.round(now.Add(time.Duration(i) * 5 * time.Second))
		if store.shutdownCount != 0 {
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
// shuts down, and heartbeats again next round. The code before the fix did
// the same; this pins it.
func TestHandlerHeartbeatRetriesARaisedHeartbeat(t *testing.T) {
	store := &handlerHeartbeatTestStore{
		heartbeatErrs:   []error{nil},
		heartbeatPanics: []bool{true, true, false},
		existsAnswers:   []bool{false},
		existsErrs:      []error{nil},
	}
	heartbeat := newHandlerHeartbeatForTest(store, 0)
	now := time.Unix(1_800_000_000, 0)

	for i := range 3 {
		heartbeat.round(now.Add(time.Duration(i) * 5 * time.Second))
	}
	if store.shutdownCount != 0 || heartbeat.retired || store.readExistsCallCount != 0 {
		t.Fatal("a raised heartbeat retired the handler or shut the process down")
	}
	if store.heartbeatCallCount != 3 {
		t.Fatalf("heartbeat %d times, want 3", store.heartbeatCallCount)
	}
}

// Each confirmation read is bounded by the confirmation timeout: its deadline
// lies the timeout after some instant within its round.
func TestHandlerHeartbeatBoundsEachConfirmationRead(t *testing.T) {
	store := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{errHandlerHeartbeatTestGone},
		existsAnswers: []bool{false},
		existsErrs:    []error{errHandlerHeartbeatTestUnanswered},
	}
	heartbeat := newHandlerHeartbeatForTest(store, 0)
	timeout := handlerHeartbeatTestSettings().ConfirmationTimeout
	now := time.Unix(1_800_000_000, 0)

	// the first round detects the retirement and reads nothing
	heartbeat.round(now)
	for i := range 3 {
		before := time.Now()
		heartbeat.round(now.Add(time.Duration(i+1) * 5 * time.Second))
		after := time.Now()
		if len(store.readDeadlines) != i+1 {
			t.Fatalf("round %d left %d reads, want %d", i+1, len(store.readDeadlines), i+1)
		}
		deadline := store.readDeadlines[i]
		if deadline.IsZero() || deadline.Before(before.Add(timeout)) || after.Add(timeout).Before(deadline) {
			t.Fatalf("read %d has deadline %v, want within [%v, %v]", i, deadline, before.Add(timeout), after.Add(timeout))
		}
	}
}

// The loop shuts the process down when its context ends, and a confirming
// round ends the loop through the shutdown, as the connect router's cancel
// does.
func TestHandlerHeartbeatLoopShutsDownWhenItEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{nil},
		existsAnswers: []bool{true},
		existsErrs:    []error{nil},
	}
	heartbeat := newHandlerHeartbeatWithStore(
		ctx,
		server.NewId(),
		handlerHeartbeatTestSettings(),
		store.heartbeat,
		store.readExists,
		func(maxDelay time.Duration) time.Duration {
			return 0
		},
		store.shutdown,
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
	if store.shutdownCount != 1 {
		t.Fatalf("the loop ended and shut the process down %d times, want once", store.shutdownCount)
	}

	processCtx, processCancel := context.WithCancel(context.Background())
	defer processCancel()
	retiredStore := &handlerHeartbeatTestStore{
		heartbeatErrs: []error{errHandlerHeartbeatTestGone},
		existsAnswers: []bool{false},
		existsErrs:    []error{nil},
	}
	retired := newHandlerHeartbeatWithStore(
		processCtx,
		server.NewId(),
		handlerHeartbeatTestSettings(),
		retiredStore.heartbeat,
		retiredStore.readExists,
		func(maxDelay time.Duration) time.Duration {
			return 0
		},
		func() {
			retiredStore.shutdown()
			processCancel()
		},
	)
	retiredDone := make(chan struct{})
	go func() {
		defer close(retiredDone)
		retired.run()
	}()
	select {
	case <-retiredDone:
	case <-time.After(10 * time.Second):
		t.Fatal("a confirmed retirement did not end the loop")
	}
	if retiredStore.readExistsCallCount != 3 || retiredStore.shutdownCount < 1 {
		t.Fatalf("the loop ended after %d confirmation reads and %d shutdowns, want 3 reads and a shutdown", retiredStore.readExistsCallCount, retiredStore.shutdownCount)
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
			func(maxDelay time.Duration) time.Duration {
				return 0
			},
			store.shutdown,
		)
		if *heartbeat.settings != *DefaultHandlerHeartbeatSettings() {
			t.Errorf("settings %+v fell back to %+v, want the defaults", settings, *heartbeat.settings)
		}
	}
}
