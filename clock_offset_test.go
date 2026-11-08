package server

import (
	"cmp"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/sidecar"
)

// Stops compiling if the check drifts from what ServiceHost holds.
var _ sidecar.ClockCheck = (*ChronyClockCheck)(nil)

// How long a test waits on another goroutine before calling it a hang. Only a failing run waits this
// long, and nothing's order depends on it.
const chronyTestHangTimeout = 30 * time.Second

// Where a test's clock starts: a fixed date on a whole second, so the expected offsets below are exact.
func chronyTestClockStart() time.Time {
	return time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
}

type chronyFakeTimer struct {
	at   time.Time
	seq  uint64
	fire func(at time.Time)
}

// The clock the check reads in a test, moved only by Advance, so no test holds a real-time race. It
// keeps sidecartest's FakeClock's promises, which schedules rely on, without making server's tests
// link sidecartest and the modules it needs. Safe for concurrent use.
type chronyFakeClock struct {
	stateLock sync.Mutex
	now       time.Time
	timerSeq  uint64
	timers    []*chronyFakeTimer
	// closed and replaced whenever a timer is added, for awaitTimers
	timerAdded chan struct{}
}

func newChronyFakeClock(start time.Time) *chronyFakeClock {
	return &chronyFakeClock{
		now:        start,
		timerAdded: make(chan struct{}),
	}
}

func (self *chronyFakeClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *chronyFakeClock) addTimer(timeout time.Duration, fire func(at time.Time)) *chronyFakeTimer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.timerSeq += 1
	timer := &chronyFakeTimer{at: self.now.Add(timeout), seq: self.timerSeq, fire: fire}
	self.timers = append(self.timers, timer)
	close(self.timerAdded)
	self.timerAdded = make(chan struct{})
	return timer
}

// Fires once the clock reaches now plus timeout. A timeout that is not positive fires at once.
func (self *chronyFakeClock) After(timeout time.Duration) <-chan time.Time {
	fired := make(chan time.Time, 1)
	if timeout <= 0 {
		fired <- self.Now()
		return fired
	}
	self.addTimer(timeout, func(at time.Time) {
		fired <- at
	})
	return fired
}

// Calls function on the goroutine that moves the clock past now plus timeout. stop returns false
// once function has been called or stopped.
func (self *chronyFakeClock) AfterFunc(timeout time.Duration, function func()) (stop func() bool) {
	if timeout <= 0 {
		function()
		return func() bool { return false }
	}
	timer := self.addTimer(timeout, func(at time.Time) {
		function()
	})
	return func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		index := slices.Index(self.timers, timer)
		if index < 0 {
			return false
		}
		self.timers = slices.Delete(self.timers, index, index+1)
		return true
	}
}

// Moves the clock forward, firing every timer it passes in time order, each at its own time and with
// no lock held, so a timer that adds a nearer one sees it fire before the move ends.
func (self *chronyFakeClock) Advance(timeout time.Duration) {
	if timeout < 0 {
		panic("the fake clock only moves forward")
	}
	target := self.Now().Add(timeout)
	for {
		var next *chronyFakeTimer
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			for _, timer := range self.timers {
				if timer.at.After(target) {
					continue
				}
				if next == nil || timer.at.Before(next.at) || (timer.at.Equal(next.at) && timer.seq < next.seq) {
					next = timer
				}
			}
			if next == nil {
				self.now = target
				return
			}
			index := slices.Index(self.timers, next)
			self.timers = slices.Delete(self.timers, index, index+1)
			self.now = next.at
		}()
		if next == nil {
			return
		}
		next.fire(next.at)
	}
}

// Timers set and not yet fired, nearest first.
func (self *chronyFakeClock) pendingTimers() []time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	times := make([]time.Time, 0, len(self.timers))
	for _, timer := range self.timers {
		times = append(times, timer.at)
	}
	slices.SortFunc(times, func(a time.Time, b time.Time) int { return cmp.Compare(a.UnixNano(), b.UnixNano()) })
	return times
}

// Returns once at least timerCount timers are pending, or ctx ends: how a test knows a goroutine has
// reached its wait before moving the clock past it, with no sleep.
func (self *chronyFakeClock) awaitTimers(ctx context.Context, timerCount int) error {
	for {
		var added chan struct{}
		var enough bool
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			added = self.timerAdded
			enough = timerCount <= len(self.timers)
		}()
		if enough {
			return nil
		}
		select {
		case <-added:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// How fakeChronyd answers each query from now on.
type fakeChronydAnswer struct {
	// No reply. The read ends at once, as its deadline would a second on.
	silent bool
	// How far true time runs ahead of the check's clock, so a clock running fast is negative.
	ahead time.Duration
	// Wire values.
	leap           byte
	stratum        byte
	rootDelay      uint32
	rootDispersion uint32
	// How long the reply takes to come back, on the fake clock.
	returnDelay time.Duration
	// Sent ahead of the reply, built from the query they follow.
	strayPackets func(request []byte) [][]byte
	// Writes over the reply.
	editReply func(reply []byte)
}

// A healthy chronyd of a stratum 3 host, on a clock that agrees with the check's.
func healthyChronydAnswer() fakeChronydAnswer {
	return fakeChronydAnswer{stratum: 3}
}

// chronyd on a fake clock, answering each query in NTP's bytes. The check's own clock is that clock,
// and true time runs ahead of it by the answer's ahead.
type fakeChronyd struct {
	clock *chronyFakeClock
	// One value for each query whose socket has closed.
	finished chan struct{}

	stateLock sync.Mutex
	answer    fakeChronydAnswer
	dialErr   error
	requests  [][]byte
}

func newFakeChronyd(clock *chronyFakeClock) *fakeChronyd {
	return &fakeChronyd{
		clock:    clock,
		finished: make(chan struct{}, 1024),
		answer:   healthyChronydAnswer(),
	}
}

func (self *fakeChronyd) setAnswer(answer fakeChronydAnswer) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answer = answer
}

func (self *fakeChronyd) setDialErr(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.dialErr = err
}

func (self *fakeChronyd) requestPackets() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([][]byte{}, self.requests...)
}

func (self *fakeChronyd) dial(ctx context.Context) (chronyConn, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.dialErr != nil {
		self.finished <- struct{}{}
		return nil, self.dialErr
	}
	return &fakeChronydConn{chronyd: self, closed: make(chan struct{})}, nil
}

// One query's socket. Its deadline is the check's business and a fake clock's, so reads end here by
// the answer, not by a real timer.
type fakeChronydConn struct {
	chronyd   *fakeChronyd
	closeOnce sync.Once
	closed    chan struct{}

	stateLock   sync.Mutex
	packets     [][]byte
	returnDelay time.Duration
}

// Takes the query and queues its answer, stamped by true time now.
func (self *fakeChronydConn) Write(request []byte) (int, error) {
	self.chronyd.stateLock.Lock()
	defer self.chronyd.stateLock.Unlock()
	self.chronyd.requests = append(self.chronyd.requests, append([]byte{}, request...))

	answer := self.chronyd.answer
	if answer.silent {
		return len(request), nil
	}
	stamp := ntpTimestamp(self.chronyd.clock.Now().Add(answer.ahead))
	reply := make([]byte, ntpPacketByteCount)
	reply[0] = answer.leap<<6 | ntpVersion<<3 | ntpModeServer
	reply[1] = answer.stratum
	binary.BigEndian.PutUint32(reply[ntpRootDelayIndex:], answer.rootDelay)
	binary.BigEndian.PutUint32(reply[ntpRootDispersionIndex:], answer.rootDispersion)
	copy(reply[ntpOriginTimestampIndex:], request[ntpTransmitIndex:ntpTransmitIndex+8])
	binary.BigEndian.PutUint64(reply[ntpReceiveIndex:], stamp)
	binary.BigEndian.PutUint64(reply[ntpTransmitIndex:], stamp)
	if answer.editReply != nil {
		answer.editReply(reply)
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if answer.strayPackets != nil {
		self.packets = append(self.packets, answer.strayPackets(request)...)
	}
	self.packets = append(self.packets, reply)
	self.returnDelay = answer.returnDelay
	return len(request), nil
}

func (self *fakeChronydConn) Read(packet []byte) (int, error) {
	var next []byte
	var returnDelay time.Duration
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if 0 < len(self.packets) {
			next = self.packets[0]
			self.packets = self.packets[1:]
		}
		returnDelay = self.returnDelay
	}()
	if next == nil {
		return 0, os.ErrDeadlineExceeded
	}
	if 0 < returnDelay {
		select {
		case <-self.chronyd.clock.After(returnDelay):
		case <-self.closed:
			return 0, net.ErrClosed
		}
	}
	return copy(packet, next), nil
}

func (self *fakeChronydConn) SetDeadline(deadline time.Time) error {
	return nil
}

func (self *fakeChronydConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
		self.chronyd.finished <- struct{}{}
	})
	return nil
}

// The check's clock in a test: the fake clock, telling the fixture each time the check begins waiting
// for its next query, its only call to After. The fixture moves the clock only once that wait has
// begun, since a move between the check reading the clock and setting its timer would put the next
// query an interval late.
type chronyFixtureClock struct {
	*chronyFakeClock
	// one value for each wait begun
	waits chan struct{}
}

func (self *chronyFixtureClock) After(timeout time.Duration) <-chan time.Time {
	fired := self.chronyFakeClock.After(timeout)
	select {
	case self.waits <- struct{}{}:
	default:
		panic("a test left 1024 of the check's waits untaken")
	}
	return fired
}

// A check on a fake clock and a fake chronyd, not started until a test says what chronyd answers.
type chronyFixture struct {
	clock      *chronyFakeClock
	checkClock *chronyFixtureClock
	chronyd    *fakeChronyd
	registry   *prometheus.Registry
	settings   *ChronyClockCheckSettings
	ctx        context.Context
	cancel     context.CancelFunc
	check      *ChronyClockCheck
}

func newChronyFixture(t *testing.T) *chronyFixture {
	return newChronyFixtureAt(t, chronyTestClockStart())
}

func newChronyFixtureAt(t *testing.T, start time.Time) *chronyFixture {
	clock := newChronyFakeClock(start)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return &chronyFixture{
		clock:      clock,
		checkClock: &chronyFixtureClock{chronyFakeClock: clock, waits: make(chan struct{}, 1024)},
		chronyd:    newFakeChronyd(clock),
		registry:   prometheus.NewRegistry(),
		settings:   DefaultChronyClockCheckSettings(),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Starts the check on clock, with its first query under way.
func (self *chronyFixture) beginOn(clock chronyClock) {
	self.check = newChronyClockCheck(self.ctx, clock, self.chronyd.dial, self.registry, self.settings)
}

// Starts the check, and returns once its first query has ended.
func (self *chronyFixture) start(t *testing.T) {
	t.Helper()
	self.beginOn(self.checkClock)
	self.awaitQuery(t)
}

// Returns once the check has ended a query, taken what chronyd said, and begun waiting for its next.
func (self *chronyFixture) awaitQuery(t *testing.T) {
	t.Helper()
	select {
	case <-self.checkClock.waits:
	case <-time.After(chronyTestHangTimeout):
		t.Fatal("the query did not end")
	}
}

// Returns once a query's socket has closed, as it does when the check ends mid query.
func (self *chronyFixture) awaitQueryEnd(t *testing.T) {
	t.Helper()
	select {
	case <-self.chronyd.finished:
	case <-time.After(chronyTestHangTimeout):
		t.Fatal("the query's socket did not close")
	}
}

// Moves the clock to the next query, and returns once the check has taken what chronyd said.
func (self *chronyFixture) tick(t *testing.T) {
	t.Helper()
	self.clock.Advance(self.settings.QueryInterval)
	self.awaitQuery(t)
}

// Returns once timerCount timers are waiting on the clock.
func (self *chronyFixture) awaitTimers(t *testing.T, timerCount int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), chronyTestHangTimeout)
	defer cancel()
	if err := self.clock.awaitTimers(ctx, timerCount); err != nil {
		t.Fatalf("the clock has fewer than %d timers: %v", timerCount, err)
	}
}

func (self *chronyFixture) wantOffset(t *testing.T, want time.Duration) {
	t.Helper()
	if offset, known := self.check.Offset(); offset != want || !known {
		t.Errorf("offset %v known %t, want %v", offset, known, want)
	}
}

func (self *chronyFixture) wantUnknown(t *testing.T) {
	t.Helper()
	if offset, known := self.check.Offset(); offset != 0 || known {
		t.Errorf("offset %v known %t, want unknown", offset, known)
	}
	if self.check.Sure() {
		t.Error("an unknown offset is sure")
	}
}

// Whether channel has closed, without waiting: every edge here happens before this reads it.
func chronyChannelClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func TestDefaultChronyClockCheckSettingsMatchDesign(t *testing.T) {
	settings := DefaultChronyClockCheckSettings()
	if settings.QueryInterval != 1*time.Second {
		t.Errorf("query interval %v", settings.QueryInterval)
	}
	if settings.UnknownTimeout != 3*time.Second {
		t.Errorf("unknown timeout %v", settings.UnknownTimeout)
	}
	if settings.SureOffset != 50*time.Millisecond {
		t.Errorf("sure offset %v", settings.SureOffset)
	}
	if _, port, err := net.SplitHostPort(settings.ChronyAddress); err != nil || port != "123" {
		t.Errorf("address %q: %v", settings.ChronyAddress, err)
	}
}

// Written out by hand from RFC 5905's figure 8, apart from anything the fake builds, so the fake and
// the parser cannot share a wrong layout.
func TestChronyReplyBytes(t *testing.T) {
	packet, err := hex.DecodeString(strings.Join([]string{
		"24",               // leap 0, version 4, mode 4
		"02",               // stratum 2
		"06",               // poll
		"ec",               // precision
		"00000800",         // root delay, 2048 / 65536 s: 31.25 ms
		"00000400",         // root dispersion, 1024 / 65536 s: 15.625 ms
		"7f000001",         // reference id
		"ed01b42400000000", // reference timestamp
		"0123456789abcdef", // origin timestamp: the query's
		"ed01b42580000000", // receive timestamp: 2026-01-02 03:04:05.5 UTC
		"ed01b42580000000", // transmit timestamp
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	reply, ok := parseChronyReply(packet, 0x0123456789abcdef)
	if !ok {
		t.Fatal("not read as a reply")
	}
	if !reply.synchronised {
		t.Error("not synchronised")
	}
	if reply.rootDelay != 31250*time.Microsecond || reply.rootDispersion != 15625*time.Microsecond {
		t.Errorf("root delay %v dispersion %v", reply.rootDelay, reply.rootDispersion)
	}
	// sent and answered in one instant, 100 ms behind chronyd
	sent := time.Date(2026, time.January, 2, 3, 4, 5, 400*1000*1000, time.UTC)
	if offset := reply.offset(sent, sent); offset != 131250*time.Microsecond {
		t.Errorf("offset %v", offset)
	}
}

func TestChronyRequestIsOneClientPacket(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.start(t)
	fixture.tick(t)
	requests := fixture.chronyd.requestPackets()
	if len(requests) != 2 {
		t.Fatalf("%d requests", len(requests))
	}
	for _, request := range requests {
		if len(request) != 48 {
			t.Fatalf("%d bytes", len(request))
		}
		// leap 0, version 4, mode 3
		if request[0] != 0x23 {
			t.Errorf("first byte %#x", request[0])
		}
		for _, value := range request[1:40] {
			if value != 0 {
				t.Errorf("request %x carries more than its transmit timestamp", request)
				break
			}
		}
	}
	if string(requests[0][40:]) == string(requests[1][40:]) {
		t.Error("two queries share a transmit timestamp, so one could take the other's reply")
	}
}

func TestChronyOffsetIsMeasuredOffsetPlusRootDistance(t *testing.T) {
	fixture := newChronyFixture(t)
	answer := healthyChronydAnswer()
	answer.ahead = 20 * time.Millisecond
	// 1 / 64 s and 1 / 128 s, so root delay's half and root dispersion are each 7.8125 ms
	answer.rootDelay = 1024
	answer.rootDispersion = 512
	fixture.chronyd.setAnswer(answer)
	fixture.start(t)
	fixture.wantOffset(t, 35625*time.Microsecond)
	if !fixture.check.Sure() {
		t.Error("35.625 ms is not sure")
	}
}

// A clock running fast is as far off as one running slow, and a sum that kept the sign would call it
// well inside the bound.
func TestChronyOffsetIsAMagnitude(t *testing.T) {
	fixture := newChronyFixture(t)
	answer := healthyChronydAnswer()
	answer.ahead = -40 * time.Millisecond
	answer.rootDispersion = 1024
	fixture.chronyd.setAnswer(answer)
	fixture.start(t)
	fixture.wantOffset(t, 55625*time.Microsecond)
	if fixture.check.Sure() {
		t.Error("a clock 40 ms fast with 15.625 ms of dispersion is sure")
	}
}

// A clock 60 ms slow whose reply is held 40 ms on the way back measures only 40 ms, so the exchange's
// own round trip has to count, or the stall reads as sure.
func TestChronyOffsetCountsItsOwnRoundTrip(t *testing.T) {
	fixture := newChronyFixture(t)
	answer := healthyChronydAnswer()
	answer.ahead = 60 * time.Millisecond
	answer.returnDelay = 40 * time.Millisecond
	fixture.chronyd.setAnswer(answer)
	fixture.beginOn(fixture.checkClock)
	fixture.awaitTimers(t, 1)
	fixture.clock.Advance(answer.returnDelay)
	fixture.awaitQuery(t)
	fixture.wantOffset(t, 60*time.Millisecond)
	if fixture.check.Sure() {
		t.Error("a stalled exchange hid a clock 60 ms slow")
	}
}

// The first era ends in 2036, and a process alive then must not read the wrap as 136 years of offset.
func TestChronyOffsetAcrossAnEraEnd(t *testing.T) {
	eraEnd := time.Date(2036, time.February, 7, 6, 28, 16, 0, time.UTC)
	fixture := newChronyFixtureAt(t, eraEnd.Add(-10*time.Millisecond))
	answer := healthyChronydAnswer()
	answer.ahead = 30 * time.Millisecond
	fixture.chronyd.setAnswer(answer)
	fixture.start(t)
	fixture.wantOffset(t, 30*time.Millisecond)
}

func TestChronyStartsUnknownUntilItsFirstReply(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.chronyd.setAnswer(fakeChronydAnswer{silent: true})
	fixture.start(t)
	fixture.wantUnknown(t)
	if !chronyChannelClosed(fixture.check.UnsureChannel()) {
		t.Error("a check that has measured nothing has an open channel")
	}
	fixture.chronyd.setAnswer(healthyChronydAnswer())
	fixture.tick(t)
	fixture.wantOffset(t, 0)
	if !fixture.check.Sure() {
		t.Error("the first reply did not make it sure")
	}
}

func TestChronyUnknownAfterThreeSilentSeconds(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.start(t)
	unsure := fixture.check.UnsureChannel()

	fixture.chronyd.setAnswer(fakeChronydAnswer{silent: true})
	fixture.tick(t)
	fixture.tick(t)
	fixture.clock.Advance(fixture.settings.QueryInterval - 1)
	fixture.wantOffset(t, 0)
	if !fixture.check.Sure() {
		t.Error("unsure before 3 s")
	}
	if chronyChannelClosed(unsure) {
		t.Error("the channel closed before 3 s")
	}

	fixture.clock.Advance(1)
	fixture.wantUnknown(t)
	if !chronyChannelClosed(unsure) {
		t.Error("the channel did not close at 3 s")
	}
}

// Every reply starts the 3 s again, so a clock that keeps answering never lapses.
func TestChronyEachReplyRestartsTheUnknownTimeout(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.start(t)
	unsure := fixture.check.UnsureChannel()
	for range 10 {
		fixture.tick(t)
	}
	if !fixture.check.Sure() || chronyChannelClosed(unsure) {
		t.Error("a clock answering every second lapsed")
	}
	if timerCount := len(fixture.clock.pendingTimers()); 2 < timerCount {
		t.Errorf("%d timers pending, so a reply left its timer behind", timerCount)
	}
}

// The timer ends the offset on time even with the check's one goroutine stuck in a read.
func TestChronyQueryBlockedInItsReadCannotHoldTheOffset(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.start(t)
	unsure := fixture.check.UnsureChannel()

	answer := healthyChronydAnswer()
	answer.returnDelay = time.Hour
	fixture.chronyd.setAnswer(answer)
	fixture.clock.Advance(fixture.settings.QueryInterval)
	// the lapse timer and the blocked read's
	fixture.awaitTimers(t, 2)
	fixture.clock.Advance(fixture.settings.UnknownTimeout - fixture.settings.QueryInterval)
	fixture.wantUnknown(t)
	if !chronyChannelClosed(unsure) {
		t.Error("the channel did not close at 3 s")
	}
}

// A clock whose timers never fire: the check must still lapse when it is read.
type chronyStalledTimerClock struct {
	*chronyFixtureClock
}

func (self chronyStalledTimerClock) AfterFunc(timeout time.Duration, function func()) (stop func() bool) {
	return func() bool { return false }
}

func TestChronyReadsLapseWithoutTheTimer(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.beginOn(chronyStalledTimerClock{chronyFixtureClock: fixture.checkClock})
	fixture.awaitQuery(t)
	unsure := fixture.check.UnsureChannel()

	fixture.chronyd.setAnswer(fakeChronydAnswer{silent: true})
	fixture.tick(t)
	fixture.tick(t)
	fixture.clock.Advance(fixture.settings.QueryInterval - 1)
	if !fixture.check.Sure() {
		t.Error("unsure before 3 s")
	}
	fixture.clock.Advance(1)
	if fixture.check.Sure() {
		t.Error("a stalled timer stretched the offset past 3 s")
	}
	if !chronyChannelClosed(unsure) {
		t.Error("a read that found the offset lapsed did not close the channel")
	}
}

// chronyd's own words that it has lost its sources end the offset at once, with no 3 s of grace.
func TestChronyUnsynchronisedRepliesAreUnknownAtOnce(t *testing.T) {
	cases := []struct {
		name    string
		leap    byte
		stratum byte
	}{
		{name: "leap 3", leap: 3, stratum: 3},
		{name: "stratum 16", leap: 0, stratum: 16},
		{name: "leap 3 and stratum 16, as chronyd sends", leap: 3, stratum: 16},
		{name: "stratum 0, a kiss code", leap: 0, stratum: 0},
		{name: "a reserved stratum", leap: 0, stratum: 17},
	}
	for _, c := range cases {
		fixture := newChronyFixture(t)
		fixture.start(t)
		unsure := fixture.check.UnsureChannel()
		if !fixture.check.Sure() {
			t.Fatalf("%s: not sure to begin with", c.name)
		}

		answer := healthyChronydAnswer()
		answer.leap = c.leap
		answer.stratum = c.stratum
		fixture.chronyd.setAnswer(answer)
		fixture.tick(t)
		if _, known := fixture.check.Offset(); known {
			t.Errorf("%s: still known", c.name)
		}
		if fixture.check.Sure() {
			t.Errorf("%s: still sure", c.name)
		}
		if !chronyChannelClosed(unsure) {
			t.Errorf("%s: the channel stayed open", c.name)
		}
		fixture.cancel()
	}
}

// A leap second on its way is a synchronised server saying so.
func TestChronyLeapWarningsStayKnown(t *testing.T) {
	for _, leap := range []byte{1, 2} {
		fixture := newChronyFixture(t)
		answer := healthyChronydAnswer()
		answer.leap = leap
		fixture.chronyd.setAnswer(answer)
		fixture.start(t)
		if !fixture.check.Sure() {
			t.Errorf("leap %d: not sure", leap)
		}
		fixture.cancel()
	}
}

func TestChronyStratumRange(t *testing.T) {
	for _, stratum := range []byte{1, 15} {
		fixture := newChronyFixture(t)
		answer := healthyChronydAnswer()
		answer.stratum = stratum
		fixture.chronyd.setAnswer(answer)
		fixture.start(t)
		if !fixture.check.Sure() {
			t.Errorf("stratum %d: not sure", stratum)
		}
		fixture.cancel()
	}
}

// A reply that answers this query and carries no time cannot be measured, and a zero t3 would read
// as a clock 126 years off, so it counts as chronyd saying it has nothing.
func TestChronyZeroTimestampsAreUnknownAtOnce(t *testing.T) {
	for _, index := range []int{ntpReceiveIndex, ntpTransmitIndex} {
		fixture := newChronyFixture(t)
		fixture.start(t)
		answer := healthyChronydAnswer()
		answer.editReply = func(reply []byte) {
			binary.BigEndian.PutUint64(reply[index:], 0)
		}
		fixture.chronyd.setAnswer(answer)
		fixture.tick(t)
		fixture.wantUnknown(t)
		fixture.cancel()
	}
}

func TestChronyFiftyMillisecondEdgeAndItsChannel(t *testing.T) {
	fixture := newChronyFixture(t)
	answer := healthyChronydAnswer()
	// 7.8125 ms of root dispersion and the rest measured, to the nanosecond
	answer.rootDispersion = 512
	answer.ahead = 42187500 * time.Nanosecond
	fixture.chronyd.setAnswer(answer)
	fixture.start(t)
	fixture.wantOffset(t, 50*time.Millisecond)
	if !fixture.check.Sure() {
		t.Error("exactly 50 ms is not sure")
	}
	first := fixture.check.UnsureChannel()
	if chronyChannelClosed(first) {
		t.Error("the channel is closed at exactly 50 ms")
	}

	answer.ahead += 1
	fixture.chronyd.setAnswer(answer)
	fixture.tick(t)
	fixture.wantOffset(t, 50*time.Millisecond+1)
	if fixture.check.Sure() {
		t.Error("past 50 ms is sure")
	}
	if !chronyChannelClosed(first) {
		t.Error("the channel did not close the moment it passed 50 ms")
	}
	if !chronyChannelClosed(fixture.check.UnsureChannel()) {
		t.Error("a channel taken while unsure is open")
	}

	answer.ahead -= 1
	fixture.chronyd.setAnswer(answer)
	fixture.tick(t)
	if !fixture.check.Sure() {
		t.Error("back at 50 ms is not sure")
	}
	second := fixture.check.UnsureChannel()
	if chronyChannelClosed(second) {
		t.Error("the new stretch's channel is closed")
	}
	if !chronyChannelClosed(first) {
		t.Error("the first stretch's channel reopened")
	}
}

func TestChronyMovingWithinTheBoundKeepsTheChannel(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.start(t)
	unsure := fixture.check.UnsureChannel()
	answer := healthyChronydAnswer()
	answer.ahead = 49 * time.Millisecond
	fixture.chronyd.setAnswer(answer)
	fixture.tick(t)
	fixture.wantOffset(t, 49*time.Millisecond)
	if fixture.check.UnsureChannel() != unsure || chronyChannelClosed(unsure) {
		t.Error("a move within the bound ended the sure stretch")
	}
}

func TestChronySendsOnePacketEachSecondAnsweredOrNot(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.start(t)
	fixture.tick(t)
	fixture.chronyd.setAnswer(fakeChronydAnswer{silent: true})
	fixture.tick(t)
	fixture.tick(t)
	if requests := fixture.chronyd.requestPackets(); len(requests) != 4 {
		t.Errorf("%d packets in 3 s", len(requests))
	}
}

// A reply that took half the interval does not put the next packet half an interval late.
func TestChronyKeepsItsSecondsThroughASlowReply(t *testing.T) {
	fixture := newChronyFixture(t)
	answer := healthyChronydAnswer()
	answer.returnDelay = 400 * time.Millisecond
	fixture.chronyd.setAnswer(answer)
	fixture.beginOn(fixture.checkClock)
	fixture.awaitTimers(t, 1)
	started := fixture.clock.Now()
	fixture.clock.Advance(answer.returnDelay)
	fixture.awaitQuery(t)

	// the lapse timer and the wait for the next packet
	fixture.awaitTimers(t, 2)
	pending := fixture.clock.pendingTimers()
	if !pending[0].Equal(started.Add(fixture.settings.QueryInterval)) {
		t.Errorf("next packet at %v, %v after the last", pending[0], pending[0].Sub(started))
	}
}

// An address that does not resolve, or a socket that will not open, is a second with no reply.
func TestChronyDialFailureIsASecondWithNoReply(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.chronyd.setDialErr(errors.New("no such host"))
	fixture.start(t)
	fixture.wantUnknown(t)
	fixture.tick(t)
	fixture.wantUnknown(t)

	fixture.chronyd.setDialErr(nil)
	fixture.tick(t)
	if !fixture.check.Sure() {
		t.Error("a query after the failure did not recover")
	}
}

// Everything here reaches the check's socket and answers nothing it asked.
func TestChronyStrayPacketsAreNotReplies(t *testing.T) {
	strayPacketFunctions := []func(request []byte) [][]byte{
		// an answer to some other query, with a later time on it
		func(request []byte) [][]byte {
			packet := make([]byte, ntpPacketByteCount)
			packet[0] = ntpVersion<<3 | ntpModeServer
			packet[1] = 3
			binary.BigEndian.PutUint64(packet[ntpOriginTimestampIndex:], binary.BigEndian.Uint64(request[ntpTransmitIndex:])+1)
			binary.BigEndian.PutUint64(packet[ntpReceiveIndex:], 1)
			binary.BigEndian.PutUint64(packet[ntpTransmitIndex:], 1)
			return [][]byte{packet}
		},
		// its own query, echoed
		func(request []byte) [][]byte {
			return [][]byte{request}
		},
		// right origin, wrong mode: a broadcast
		func(request []byte) [][]byte {
			packet := make([]byte, ntpPacketByteCount)
			packet[0] = ntpVersion<<3 | 5
			copy(packet[ntpOriginTimestampIndex:], request[ntpTransmitIndex:ntpTransmitIndex+8])
			return [][]byte{packet}
		},
		// right origin, wrong version
		func(request []byte) [][]byte {
			packet := make([]byte, ntpPacketByteCount)
			packet[0] = 3<<3 | ntpModeServer
			copy(packet[ntpOriginTimestampIndex:], request[ntpTransmitIndex:ntpTransmitIndex+8])
			return [][]byte{packet}
		},
		// too short to hold a reply
		func(request []byte) [][]byte {
			return [][]byte{{ntpVersion<<3 | ntpModeServer, 3}, {}}
		},
	}
	for index, strayPackets := range strayPacketFunctions {
		fixture := newChronyFixture(t)
		answer := healthyChronydAnswer()
		answer.ahead = 10 * time.Millisecond
		answer.strayPackets = strayPackets
		fixture.chronyd.setAnswer(answer)
		fixture.start(t)
		// the real reply behind them is the one taken
		if offset, known := fixture.check.Offset(); offset != 10*time.Millisecond || !known {
			t.Errorf("stray %d: offset %v known %t", index, offset, known)
		}
		fixture.cancel()
	}
}

// A stray packet is no reply, so it neither keeps the offset alive nor ends it.
func TestChronyStrayPacketsAloneLeaveTheOffsetToLapse(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.start(t)
	answer := healthyChronydAnswer()
	answer.editReply = func(reply []byte) {
		// no longer an answer to the query, and saying it has lost its sources
		reply[ntpOriginTimestampIndex] ^= 0xff
		reply[0] |= ntpLeapUnsynchronised << 6
	}
	fixture.chronyd.setAnswer(answer)
	fixture.tick(t)
	fixture.tick(t)
	fixture.clock.Advance(fixture.settings.QueryInterval - 1)
	fixture.wantOffset(t, 0)
	fixture.clock.Advance(1)
	fixture.wantUnknown(t)
}

func TestChronyExportsItsOffset(t *testing.T) {
	fixture := newChronyFixture(t)
	answer := healthyChronydAnswer()
	answer.ahead = 20 * time.Millisecond
	fixture.chronyd.setAnswer(answer)
	fixture.start(t)
	if count, err := testutil.GatherAndCount(fixture.registry, "urnetwork_clock_offset_seconds"); err != nil || count != 1 {
		t.Errorf("%d series: %v", count, err)
	}
	if seconds := testutil.ToFloat64(fixture.check.gauge); seconds != 0.02 {
		t.Errorf("exported %v", seconds)
	}
}

// Past monitor's 25 ms the export holds what the check holds, and unknown must trip the same warning,
// since warp's grafana front would drop a NaN.
func TestChronyExportsUnknownAsAnInfiniteOffset(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.chronyd.setAnswer(fakeChronydAnswer{silent: true})
	fixture.start(t)
	if seconds := testutil.ToFloat64(fixture.check.gauge); !math.IsInf(seconds, 1) {
		t.Errorf("exported %v", seconds)
	}
}

func TestChronyEndingItsContextEndsTheCheck(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.start(t)
	unsure := fixture.check.UnsureChannel()
	fixture.cancel()
	select {
	case <-fixture.check.Done():
	case <-time.After(chronyTestHangTimeout):
		t.Fatal("the check did not end")
	}
	if !chronyChannelClosed(unsure) {
		t.Error("the channel stayed open")
	}
	fixture.wantUnknown(t)
	if count, err := testutil.GatherAndCount(fixture.registry, "urnetwork_clock_offset_seconds"); err != nil || count != 0 {
		t.Errorf("%d series after the check ended: %v", count, err)
	}
	// so a host may start another one
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	newChronyClockCheck(ctx, fixture.clock, fixture.chronyd.dial, fixture.registry, fixture.settings)
}

// A read held at the end of a check does not hold the check.
func TestChronyEndingItsContextEndsABlockedRead(t *testing.T) {
	fixture := newChronyFixture(t)
	answer := healthyChronydAnswer()
	answer.returnDelay = time.Hour
	fixture.chronyd.setAnswer(answer)
	fixture.beginOn(fixture.checkClock)
	fixture.awaitTimers(t, 1)
	fixture.cancel()
	fixture.awaitQueryEnd(t)
	select {
	case <-fixture.check.Done():
	case <-time.After(chronyTestHangTimeout):
		t.Fatal("the check did not end")
	}
}

func TestChronyNonPositiveIntervalSendsNothing(t *testing.T) {
	fixture := newChronyFixture(t)
	fixture.settings.QueryInterval = 0
	fixture.beginOn(fixture.checkClock)
	select {
	case <-fixture.check.Done():
	case <-time.After(chronyTestHangTimeout):
		t.Fatal("the check did not end")
	}
	fixture.wantUnknown(t)
	if requests := fixture.chronyd.requestPackets(); len(requests) != 0 {
		t.Errorf("%d packets", len(requests))
	}
}

// The real socket, through the same constructor a host calls, with queries answered over loopback.
// Each query arriving proves the one before it ended, so the check holds whatever that reply said by
// then. A reply that misses its query's deadline on a loaded machine leaves only that query
// unanswered, so this waits query by query until the check holds an offset, and on nothing's time.
func TestChronyOverUdp(t *testing.T) {
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	// one value for each query answered
	queries := make(chan struct{}, 1024)
	go func() {
		packet := make([]byte, ntpPacketByteCount)
		for {
			byteCount, address, err := listener.ReadFrom(packet)
			if err != nil {
				return
			}
			if byteCount != ntpPacketByteCount {
				t.Errorf("%d bytes", byteCount)
			}
			stamp := ntpTimestamp(time.Now())
			reply := make([]byte, ntpPacketByteCount)
			reply[0] = ntpVersion<<3 | ntpModeServer
			reply[1] = 3
			binary.BigEndian.PutUint32(reply[ntpRootDispersionIndex:], 512)
			copy(reply[ntpOriginTimestampIndex:], packet[ntpTransmitIndex:])
			binary.BigEndian.PutUint64(reply[ntpReceiveIndex:], stamp)
			binary.BigEndian.PutUint64(reply[ntpTransmitIndex:], stamp)
			if _, err := listener.WriteTo(reply, address); err != nil {
				return
			}
			select {
			case queries <- struct{}{}:
			default:
			}
		}
	}()

	ctx, cancel := context.WithCancel(t.Context())
	settings := DefaultChronyClockCheckSettings()
	settings.ChronyAddress = listener.LocalAddr().String()
	settings.QueryInterval = 100 * time.Millisecond
	settings.UnknownTimeout = time.Hour
	check := NewChronyClockCheck(ctx, settings)
	// ends before the listener closes, and releases the default registry's gauge for the next run
	t.Cleanup(func() {
		cancel()
		select {
		case <-check.Done():
		case <-time.After(chronyTestHangTimeout):
			t.Error("the check did not end")
		}
	})

	hang := time.After(chronyTestHangTimeout)
	for queryCount := 1; ; queryCount += 1 {
		select {
		case <-queries:
		case <-hang:
			t.Fatal("no reply was taken")
		}
		// the first query's arrival says nothing of its reply
		if queryCount < 2 {
			continue
		}
		if _, known := check.Offset(); known {
			break
		}
	}
	// no upper bound on it: how long the exchange took here depends on the machine's load
	if offset, known := check.Offset(); !known || offset < 7812500*time.Nanosecond {
		t.Errorf("offset %v known %t", offset, known)
	}
}
