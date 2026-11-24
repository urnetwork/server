package sidecart3

// What a move of a T3 Net's clock holds every later request to (faults.go's clockCatchUp; QD-1111): one catch-up a
// move, which a request arriving while it is in flight waits for. Held here on T2's fakes, with a check standing in
// for server.FileClusters.Check and a store that refuses a write while that check has not read admin info at the
// Net's now, as a cluster refuses one on admin info CheckStaleAfter old; faults_t3_test.go holds the same on a T3
// Net's real clusters and Redis. Every order a test needs is forced through the catch-up's hooks, and every wait is
// bounded only to fail: nothing sleeps.

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// A channel closed once, however many times it is fired.
type testSignal struct {
	once    sync.Once
	channel chan struct{}
}

func newTestSignal() *testSignal {
	return &testSignal{channel: make(chan struct{})}
}

func (self *testSignal) fire() {
	self.once.Do(func() {
		close(self.channel)
	})
}

func (self *testSignal) fired() bool {
	select {
	case <-self.channel:
		return true
	default:
		return false
	}
}

// Waits for signal, within the barrier driver's bound, failing t past it.
func awaitSignal(t *testing.T, net *sidecartest.Net, what string, signal *testSignal) {
	t.Helper()
	if err := net.Barriers().AwaitChannel(t.Context(), what, signal.channel); err != nil {
		t.Fatal(err)
	}
}

type catchUpRequestNameKey struct{}

// ctx naming its request to the catch-up hooks.
func withRequestName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, catchUpRequestNameKey{}, name)
}

func requestName(ctx context.Context) string {
	name, _ := ctx.Value(catchUpRequestNameKey{}).(string)
	return name
}

// The catch-up hooks a test drives: the work of the request named held runs only once release fires, started firing as
// it begins; each request that finds a catch-up in flight fires its waiting signal; and every work that runs is
// counted by what it catches up.
type drivenCatchUps struct {
	// the request whose work is held, empty for none
	held string
	// whether the held work, once released, ends its goroutine without running, as a test's Fatal in it would
	heldExits bool
	started   *testSignal
	release   *testSignal

	stateLock sync.Mutex
	// by request name
	nameWaitings map[string]*testSignal
	// the works that ran to their end, by what they catch up
	whatWorkCounts map[string]int
}

func newDrivenCatchUps(held string) *drivenCatchUps {
	return &drivenCatchUps{
		held:           held,
		started:        newTestSignal(),
		release:        newTestSignal(),
		nameWaitings:   map[string]*testSignal{},
		whatWorkCounts: map[string]int{},
	}
}

func (self *drivenCatchUps) hooks() *catchUpHooks {
	return &catchUpHooks{
		work: func(ctx context.Context, what string, work func()) {
			if self.held != "" && requestName(ctx) == self.held {
				self.started.fire()
				<-self.release.channel
				if self.heldExits {
					runtime.Goexit()
				}
			}
			work()
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.whatWorkCounts[what] += 1
		},
		waiting: func(ctx context.Context, what string) {
			self.waiting(requestName(ctx)).fire()
		},
	}
}

// Fired when the request named finds a catch-up in flight.
func (self *drivenCatchUps) waiting(name string) *testSignal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	signal, ok := self.nameWaitings[name]
	if !ok {
		signal = newTestSignal()
		self.nameWaitings[name] = signal
	}
	return signal
}

// How many works catching up what have run to their end.
func (self *drivenCatchUps) workCount(what string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.whatWorkCounts[what]
}

// One call on a goroutine of its own.
type pendingCall struct {
	done chan struct{}
	// what the call returned, read once done has closed
	err error
	// whether it returned at all: a call whose goroutine ended inside it did not
	returned atomic.Bool
}

// Starts call once gate closes, or at once for a nil gate.
func startCall(gate <-chan struct{}, call func() error) *pendingCall {
	pending := &pendingCall{done: make(chan struct{})}
	go func() {
		defer close(pending.done)
		if gate != nil {
			<-gate
		}
		pending.err = call()
		pending.returned.Store(true)
	}()
	return pending
}

// What pending returned, waiting for it within the barrier driver's bound. A call that never returned fails t.
func awaitCall(t *testing.T, net *sidecartest.Net, what string, pending *pendingCall) error {
	t.Helper()
	if err := net.Barriers().AwaitChannel(t.Context(), what, pending.done); err != nil {
		t.Fatal(err)
	}
	if !pending.returned.Load() {
		t.Fatalf("%s never returned", what)
	}
	return pending.err
}

// Waits until the request named finds a catch-up in flight, failing t if its call returns first: it went on while
// the catch-up was in flight.
func awaitWaiting(t *testing.T, net *sidecartest.Net, driven *drivenCatchUps, name string, pending *pendingCall) {
	t.Helper()
	awaitWaitingOrDone(t, net, driven, name, pending.done, func() error {
		return pending.err
	})
}

// As awaitWaiting, for a request whose end done closes at, and err gives what it ended with once done has closed.
func awaitWaitingOrDone(t *testing.T, net *sidecartest.Net, driven *drivenCatchUps, name string, done <-chan struct{}, err func() error) {
	t.Helper()
	waiting := driven.waiting(name)
	either := newTestSignal()
	go func() {
		select {
		case <-waiting.channel:
		case <-done:
		}
		either.fire()
	}()
	awaitSignal(t, net, name+" waiting for a catch-up or ending", either)
	if !waiting.fired() {
		t.Fatalf("%s went on while a catch-up was in flight, and ended with %v", name, err())
	}
}

// What a cluster refuses a write on while its admin info is unread or stale.
var errFakeAdminInfoStale = errors.New("admin info is unread or stale")

// Admin info as a fake check reads it: the Net's time of the last read, and how many reads ran.
type fakeAdminInfo struct {
	clock *sidecartest.FakeClock

	stateLock sync.Mutex
	readTime  time.Time
	readCount int
}

// faultBlobs' check: reads admin info at the clock's now.
func (self *fakeAdminInfo) check(ctx context.Context) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readTime = self.clock.Now()
	self.readCount += 1
}

// Whether admin info was read at the clock's now: past it, a write is refused, as on a cluster with a CheckStaleAfter
// of any move.
func (self *fakeAdminInfo) fresh() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.readTime.Equal(self.clock.Now())
}

func (self *fakeAdminInfo) reads() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.readCount
}

// T2's blobs, whose first cluster refuses a write while admin info is not fresh.
type staleRefusingBlobs struct {
	sidecar.BlobStore
	admin *fakeAdminInfo
}

func (self *staleRefusingBlobs) FirstCluster() (sidecar.BlobCluster, error) {
	cluster, err := self.BlobStore.FirstCluster()
	if err != nil {
		return nil, err
	}
	return &staleRefusingCluster{BlobCluster: cluster, admin: self.admin}, nil
}

type staleRefusingCluster struct {
	sidecar.BlobCluster
	admin *fakeAdminInfo
}

func (self *staleRefusingCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	if !self.admin.fresh() {
		return "", errFakeAdminInfoStale
	}
	return self.BlobCluster.PutIfNoneMatch(ctx, key, body, options)
}

// T2's Net with a faultBlobs over a fake store: admin info the fake check reads, and a first cluster refusing a write
// on admin info not read at the Net's now.
type catchUpFixture struct {
	net     *sidecartest.Net
	store   *sidecartest.FakeBlobs
	admin   *fakeAdminInfo
	cluster sidecar.BlobCluster
}

// hooks nil drives nothing.
func newCatchUpFixture(t *testing.T, hooks *catchUpHooks) *catchUpFixture {
	t.Helper()
	net := sidecartest.NewNet(t.Context(), nil)
	t.Cleanup(net.Close)
	store := sidecartest.NewFakeBlobs(net.Clock(), sidecartest.NewFaultQueue(), nil, nil)
	admin := &fakeAdminInfo{clock: net.Clock()}
	// a reread after every move, as the fake refuses a write on admin info any move made stale
	blobs := newFaultBlobs(&staleRefusingBlobs{BlobStore: store, admin: admin}, admin.check, net, nil, 0, hooks)
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	return &catchUpFixture{net: net, store: store, admin: admin, cluster: cluster}
}

// A write of the request named, under ctx, once gate closes (nil for at once).
func (self *catchUpFixture) startWrite(ctx context.Context, gate <-chan struct{}, name string) *pendingCall {
	return startCall(gate, func() error {
		_, err := self.cluster.PutIfNoneMatch(withRequestName(ctx, name), "catch-up/"+name, []byte(name), nil)
		return err
	})
}

// A write of the request named, answered.
func (self *catchUpFixture) write(t *testing.T, name string) {
	t.Helper()
	if _, err := self.cluster.PutIfNoneMatch(withRequestName(self.net.Context(), name), "catch-up/"+name, []byte(name), nil); err != nil {
		t.Fatalf("%s read %v", name, err)
	}
}

// How many writes reached the fake store.
func (self *catchUpFixture) storedWrites() int {
	return self.store.RequestCount(self.cluster.ClusterId(), sidecartest.FaultOpPut)
}

// A write that arrives while another's admin info reread is in flight after a move waits for that reread and is
// answered on what it read, and the reread runs once: had it gone on, it would have been refused on admin info the
// move made stale.
func TestAWriteDuringAnAdminRereadWaitsForIt(t *testing.T) {
	driven := newDrivenCatchUps("A")
	fixture := newCatchUpFixture(t, driven.hooks())
	t.Cleanup(driven.release.fire)
	// the first request reads admin info whatever the clock says
	fixture.write(t, "first")
	fixture.net.Clock().Advance(time.Minute)
	a := fixture.startWrite(fixture.net.Context(), nil, "A")
	awaitSignal(t, fixture.net, "A's reread", driven.started)
	b := fixture.startWrite(fixture.net.Context(), nil, "B")
	awaitWaiting(t, fixture.net, driven, "B", b)
	if writes := fixture.storedWrites(); writes != 1 {
		t.Errorf("the store took %d writes, the first's among them, while A's reread was in flight", writes)
	}
	driven.release.fire()
	if err := awaitCall(t, fixture.net, "A's write", a); err != nil {
		t.Errorf("A read %v", err)
	}
	if err := awaitCall(t, fixture.net, "B's write", b); err != nil {
		t.Errorf("B read %v", err)
	}
	if reads := fixture.admin.reads(); reads != 2 {
		t.Errorf("admin info was read %d times for the first request and one move", reads)
	}
	if writes := fixture.storedWrites(); writes != 3 {
		t.Errorf("the store took %d writes of 3", writes)
	}
}

// A write whose ctx ends while it waits for an admin info reread returns its ctx's error and sends nothing, the reread
// going on for the others; and a write after it, the clock not moved again, reads nothing again.
func TestAWaitForAnAdminRereadEndsWithItsContext(t *testing.T) {
	driven := newDrivenCatchUps("A")
	fixture := newCatchUpFixture(t, driven.hooks())
	t.Cleanup(driven.release.fire)
	a := fixture.startWrite(fixture.net.Context(), nil, "A")
	awaitSignal(t, fixture.net, "A's reread", driven.started)
	bCtx, cancelB := context.WithCancel(fixture.net.Context())
	defer cancelB()
	b := fixture.startWrite(bCtx, nil, "B")
	awaitWaiting(t, fixture.net, driven, "B", b)
	cancelB()
	if err := awaitCall(t, fixture.net, "B's write", b); !errors.Is(err, context.Canceled) {
		t.Errorf("B, its ctx ended while it waited, read %v", err)
	}
	if writes := fixture.storedWrites(); writes != 0 {
		t.Errorf("the store took %d writes while A's reread was in flight", writes)
	}
	driven.release.fire()
	if err := awaitCall(t, fixture.net, "A's write", a); err != nil {
		t.Errorf("A read %v", err)
	}
	fixture.write(t, "C")
	if reads := fixture.admin.reads(); reads != 1 {
		t.Errorf("admin info was read %d times with no move", reads)
	}
	if writes := fixture.storedWrites(); writes != 2 {
		t.Errorf("the store took %d writes, A's and C's", writes)
	}
}

// An actor killed while it waits for an admin info reread goes no further, as at any fake: its run ends with no error
// and its write never reaches the store.
func TestAnActorKilledWaitingForAnAdminRereadGoesNoFurther(t *testing.T) {
	driven := newDrivenCatchUps("A")
	fixture := newCatchUpFixture(t, driven.hooks())
	t.Cleanup(driven.release.fire)
	a := fixture.startWrite(fixture.net.Context(), nil, "A")
	awaitSignal(t, fixture.net, "A's reread", driven.started)
	actor := fixture.net.Actors().Actor(fixture.net.Context(), "writerB")
	var returned atomic.Bool
	run := fixture.net.Barriers().Start(actor, func() error {
		_, err := fixture.cluster.PutIfNoneMatch(withRequestName(actor.Context(), "B"), "catch-up/B", []byte("B"), nil)
		returned.Store(true)
		return err
	})
	awaitWaitingOrDone(t, fixture.net, driven, "B", run.Ended(), run.Err)
	actor.Kill()
	if err := fixture.net.Barriers().AwaitChannel(t.Context(), "B's run", run.Ended()); err != nil {
		t.Fatal(err)
	}
	if err := run.Err(); err != nil || returned.Load() {
		t.Errorf("B's write, its actor killed while it waited, returned %t with %v", returned.Load(), err)
	}
	driven.release.fire()
	if err := awaitCall(t, fixture.net, "A's write", a); err != nil {
		t.Errorf("A read %v", err)
	}
	if writes := fixture.storedWrites(); writes != 1 {
		t.Errorf("the store took %d writes, A's alone", writes)
	}
}

// A reread that never returns, as at a test's Fatal inside it, records nothing: the write waiting for it reads admin
// info again itself and is answered.
func TestAnAdminRereadThatNeverReturnsIsTheNextWritesToRun(t *testing.T) {
	driven := newDrivenCatchUps("A")
	driven.heldExits = true
	fixture := newCatchUpFixture(t, driven.hooks())
	t.Cleanup(driven.release.fire)
	a := fixture.startWrite(fixture.net.Context(), nil, "A")
	awaitSignal(t, fixture.net, "A's reread", driven.started)
	b := fixture.startWrite(fixture.net.Context(), nil, "B")
	awaitWaiting(t, fixture.net, driven, "B", b)
	driven.release.fire()
	if err := fixture.net.Barriers().AwaitChannel(t.Context(), "A's write", a.done); err != nil {
		t.Fatal(err)
	}
	if a.returned.Load() {
		t.Error("A's write returned from a reread that ended its goroutine")
	}
	if err := awaitCall(t, fixture.net, "B's write", b); err != nil {
		t.Errorf("B read %v", err)
	}
	if reads := fixture.admin.reads(); reads != 1 {
		t.Errorf("admin info was read %d times: once, by B", reads)
	}
}

// Admin info is read again once each move of the clock, before the first request after it, a read as well as a
// write, and not between moves; two moves before a request take one reread.
func TestEachMoveOfTheClockRereadsAdminInfoOnce(t *testing.T) {
	fixture := newCatchUpFixture(t, nil)
	expectReads := func(reads int, after string) {
		t.Helper()
		if got := fixture.admin.reads(); got != reads {
			t.Errorf("after %s, admin info was read %d times, not %d", after, got, reads)
		}
	}
	fixture.write(t, "first")
	fixture.write(t, "second")
	expectReads(1, "two writes with no move")
	fixture.net.Clock().Advance(time.Minute)
	if _, err := fixture.cluster.Head(fixture.net.Context(), "catch-up/first"); err != nil {
		t.Fatal(err)
	}
	fixture.write(t, "third")
	expectReads(2, "a move, a read and a write")
	fixture.net.Clock().Advance(time.Minute)
	fixture.net.Clock().Advance(time.Minute)
	fixture.write(t, "fourth")
	fixture.write(t, "fifth")
	expectReads(3, "two moves and two writes")
}

// Writes sent at once after a move share one admin info reread, and every one is answered.
func TestWritesAtOnceAfterAMoveShareOneAdminReread(t *testing.T) {
	fixture := newCatchUpFixture(t, nil)
	fixture.write(t, "first")
	fixture.net.Clock().Advance(time.Minute)
	gate := make(chan struct{})
	names := []string{"A", "B", "C", "D", "E", "F", "G", "H"}
	pendings := []*pendingCall{}
	for _, name := range names {
		pendings = append(pendings, fixture.startWrite(fixture.net.Context(), gate, name))
	}
	close(gate)
	for index, pending := range pendings {
		if err := awaitCall(t, fixture.net, names[index]+"'s write", pending); err != nil {
			t.Errorf("%s read %v", names[index], err)
		}
	}
	if reads := fixture.admin.reads(); reads != 2 {
		t.Errorf("admin info was read %d times for the first request and one move", reads)
	}
}

// A catch-up runs from the time the last one ran to up to the clock's now, nothing before the clock moves past its
// start, and once for any number of moves before it.
func TestACatchUpRunsFromTheLastOneToNow(t *testing.T) {
	net := sidecartest.NewNet(t.Context(), nil)
	defer net.Close()
	start := net.Clock().Now()
	catchUp := newClockCatchUp(net, "test catch-up", start, nil)
	fromTimes := []time.Time{}
	toTimes := []time.Time{}
	run := func() {
		t.Helper()
		err := catchUp.catchUp(net.Context(), func(fromTime time.Time, toTime time.Time) {
			fromTimes = append(fromTimes, fromTime)
			toTimes = append(toTimes, toTime)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	run()
	net.Clock().Advance(time.Minute)
	net.Clock().Advance(time.Minute)
	run()
	run()
	net.Clock().Advance(time.Hour)
	run()
	expectFromTimes := []time.Time{start, start.Add(2 * time.Minute)}
	expectToTimes := []time.Time{start.Add(2 * time.Minute), start.Add(2*time.Minute + time.Hour)}
	if len(fromTimes) != len(expectFromTimes) {
		t.Fatalf("%d catch-ups ran, not %d", len(fromTimes), len(expectFromTimes))
	}
	for index := range fromTimes {
		if !fromTimes[index].Equal(expectFromTimes[index]) || !toTimes[index].Equal(expectToTimes[index]) {
			t.Errorf("catch-up %d ran from %s to %s, not from %s to %s", index, fromTimes[index], toTimes[index], expectFromTimes[index], expectToTimes[index])
		}
	}
}
