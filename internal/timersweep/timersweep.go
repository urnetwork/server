// Package timersweep keeps a process's logical timers out of the Go runtime's
// per-P timer heaps (server LBALTOPT2.md §8.1).
//
// Changing a runtime timer that is in a P's heap makes the runtime walk that
// whole heap once the changed deadline arrives, loading every timer in it, so
// every pending timer adds to the cost of every walk. A sweep holds its timers
// in hashed timing wheels instead, one per shard, and each shard is driven by
// a single runtime timer that fires at the shard's next tick. That timer only
// ever fires and is re-armed after it fired, which never sets off a walk.
//
// A sweep timer fires at the first tick boundary at or after its deadline:
// never early, and at most one tick late, before the scheduling delay any
// runtime timer has. A fire only signals the owner, with a non-blocking send
// on a one-slot channel, so a sweep never waits on an owner. As with a Go
// 1.23 runtime timer, no stale fire is received after Reset or Stop returns.
//
// Only logical timers belong in a sweep: rounds, samples, rechecks and idle
// watches. Socket deadlines stay native, and a heartbeat moves to a sweep only
// where its bound is proven unchanged.
package timersweep

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultTick is the resolution of a process's sweep, and so the most a sweep
// timer fires after its deadline.
const DefaultTick = 100 * time.Millisecond

// The slots of a shard's wheel. A deadline more than a rotation away (102.4 s
// at DefaultTick) waits in its slot through the earlier passes over it.
const defaultSlotCount = 1024

// A timer made without a shared sweep has a sweep of its own, which holds that
// one timer, so its wheel needs few slots.
const privateSlotCount = 16

// A Sweep is safe for concurrent use. Its shards start their drivers only
// while they hold an armed timer, so an idle sweep has no goroutine and no
// runtime timer, and needs no close.
type Sweep struct {
	tick   time.Duration
	shards []*shard
	// new timers take the shards in turn
	nextShard atomic.Uint32
}

// New makes a sweep with one shard per P, so timers on different shards never
// share a lock.
func New(tick time.Duration) *Sweep {
	return newSweep(tick, runtime.GOMAXPROCS(0), defaultSlotCount)
}

// slotCount must be a power of two.
func newSweep(tick time.Duration, shardCount int, slotCount int) *Sweep {
	if tick <= 0 {
		panic("timersweep: the tick must be positive")
	}
	if slotCount <= 0 || slotCount&(slotCount-1) != 0 {
		panic("timersweep: the slot count must be a power of two")
	}
	sweep := &Sweep{
		tick:   tick,
		shards: make([]*shard, max(1, shardCount)),
	}
	for i := range sweep.shards {
		sweep.shards[i] = &shard{
			tick:  tick,
			mask:  int64(slotCount - 1),
			slots: make([]*entry, slotCount),
		}
	}
	return sweep
}

// Tick is the most a timer of this sweep fires after its deadline. A nil sweep
// gives each timer a sweep of its own with DefaultTick.
func (self *Sweep) Tick() time.Duration {
	if self == nil {
		return DefaultTick
	}
	return self.tick
}

// ArmedCount is the number of timers armed in the sweep now.
func (self *Sweep) ArmedCount() int {
	if self == nil {
		return 0
	}
	count := 0
	for _, shard := range self.shards {
		count += shard.armedTimerCount()
	}
	return count
}

// The shard of a new timer. A nil sweep makes a sweep for the timer alone: an
// owner constructed without a shared sweep, as tests do, keeps the sweep's
// timing, and inside a synctest bubble its driver stays in that bubble.
func (self *Sweep) nextTimerShard() *shard {
	if self == nil {
		return newSweep(DefaultTick, 1, privateSlotCount).shards[0]
	}
	i := self.nextShard.Add(1)
	return self.shards[int(i%uint32(len(self.shards)))]
}

// NewTimer arms a timer that fires once, d from now.
func (self *Sweep) NewTimer(d time.Duration) *Timer {
	c := make(chan struct{}, 1)
	timer := &Timer{
		C:     c,
		entry: entry{c: c},
	}
	timer.shard = self.nextTimerShard()
	timer.shard.arm(&timer.entry, d, 0)
	return timer
}

// NewTicker arms a timer that fires every period, as a time.Ticker does: the
// fires keep their cadence from the start, never early and each at most a
// tick late, and a fire the owner has not received yet absorbs later ones.
func (self *Sweep) NewTicker(period time.Duration) *Ticker {
	if period <= 0 {
		panic("timersweep: the ticker period must be positive")
	}
	c := make(chan struct{}, 1)
	ticker := &Ticker{
		C:     c,
		entry: entry{c: c},
	}
	ticker.shard = self.nextTimerShard()
	ticker.shard.arm(&ticker.entry, period, period)
	return ticker
}

// A Timer fires once per arm. Reset and Stop may be called from any goroutine.
type Timer struct {
	// receives one value per fire
	C <-chan struct{}
	entry
}

// Reset arms the timer d from now, replacing any pending deadline, and
// reports whether the timer was pending: armed, or fired with the fire not yet
// received. A fire not yet received is discarded. A nonpositive d fires at
// once, as for a runtime timer.
func (self *Timer) Reset(d time.Duration) bool {
	return self.shard.arm(&self.entry, d, 0)
}

// Stop disarms the timer and discards a fire not yet received. It reports
// whether the timer was pending.
func (self *Timer) Stop() bool {
	return self.shard.disarm(&self.entry)
}

// A Ticker fires every period until stopped.
type Ticker struct {
	// receives one value per fire
	C <-chan struct{}
	entry
}

// Stop ends the ticker and discards a fire not yet received.
func (self *Ticker) Stop() {
	self.shard.disarm(&self.entry)
}

// A timer's place in its shard. Every field after c is guarded by the shard's
// stateLock.
type entry struct {
	shard *shard
	c     chan struct{}

	prev *entry
	next *entry
	// the deadline, from the shard driver's epoch
	when time.Duration
	// the tick that fires the timer: the first boundary at or after when
	due    int64
	period time.Duration
	armed  bool
}

// One hashed timing wheel and the driver that sweeps it.
type shard struct {
	tick time.Duration
	mask int64

	stateLock  sync.Mutex
	slots      []*entry
	armedCount int
	// the running driver, nil while no timer is armed
	driver *shardDriver
	// set when a driver starts: tick n ends at epoch + n*tick
	epoch time.Time
	// the last tick the driver has fired
	firedTick int64
	// drivers whose goroutine has not returned, the retired one included
	runningDriverCount atomic.Int32
}

type shardDriver struct {
	stop chan struct{}
}

func (self *shard) armedTimerCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.armedCount
}

func (self *shard) arm(e *entry, d time.Duration, period time.Duration) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	pending := self.unlinkWithLock(e)
	if drain(e.c) {
		pending = true
	}
	e.period = period
	if d <= 0 {
		// due already, so it fires now, as a runtime timer would
		signal(e.c)
		self.retireIdleDriverWithLock()
		return pending
	}
	if self.driver == nil {
		self.startDriverWithLock()
	}
	e.when = time.Since(self.epoch) + d
	if e.when < 0 || maxWhen(self.tick) < e.when {
		// as far out as the wheel counts, which is to say never
		e.when = maxWhen(self.tick)
	}
	self.linkWithLock(e)
	return pending
}

// The latest deadline whose tick the wheel can count without overflow.
func maxWhen(tick time.Duration) time.Duration {
	return time.Duration(math.MaxInt64) - tick
}

func (self *shard) disarm(e *entry) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	pending := self.unlinkWithLock(e)
	if drain(e.c) {
		pending = true
	}
	self.retireIdleDriverWithLock()
	return pending
}

// The shard is empty here, so no armed timer counts from the old epoch.
func (self *shard) startDriverWithLock() {
	driver := &shardDriver{
		stop: make(chan struct{}),
	}
	self.driver = driver
	self.epoch = time.Now()
	self.firedTick = 0
	self.runningDriverCount.Add(1)
	go self.run(driver, self.epoch.Add(self.tick))
}

func (self *shard) retireIdleDriverWithLock() {
	if self.armedCount == 0 && self.driver != nil {
		close(self.driver.stop)
		self.driver = nil
	}
}

// Wakes at each tick boundary, from the first, while the shard has an armed
// timer. Its runtime timer is re-armed only after it fired, when it is in no
// heap.
func (self *shard) run(driver *shardDriver, firstTickEnd time.Time) {
	defer self.runningDriverCount.Add(-1)
	timer := time.NewTimer(time.Until(firstTickEnd))
	defer timer.Stop()
	for {
		select {
		case <-driver.stop:
			return
		case <-timer.C:
		}
		wait, running := self.advance(driver)
		if !running {
			return
		}
		timer.Reset(wait)
	}
}

// Fires every timer due by now and returns the wait until the next tick, or
// false once the driver is retired.
func (self *shard) advance(driver *shardDriver) (time.Duration, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.driver != driver {
		return 0, false
	}
	nowTick := int64(time.Since(self.epoch) / self.tick)
	if self.firedTick < nowTick {
		first := self.firedTick + 1
		if slotCount := self.mask + 1; slotCount < nowTick-first+1 {
			// a driver more than a rotation late passes each slot once
			first = nowTick - slotCount + 1
		}
		for tick := first; tick <= nowTick; tick += 1 {
			self.fireSlotWithLock(tick&self.mask, nowTick)
		}
		self.firedTick = nowTick
	}
	if self.armedCount == 0 {
		self.driver = nil
		return 0, false
	}
	return time.Duration(self.firedTick+1)*self.tick - time.Since(self.epoch), true
}

// Fires the timers in one slot that are due by nowTick. A timer a rotation or
// more out stays. A ticker is linked again for its next deadline, which is
// after nowTick, so this pass does not fire it again.
func (self *shard) fireSlotWithLock(slot int64, nowTick int64) {
	e := self.slots[slot]
	for e != nil {
		next := e.next
		if e.due <= nowTick {
			self.unlinkWithLock(e)
			signal(e.c)
			if 0 < e.period {
				// the next deadline after now, skipping the ones missed, as
				// the runtime does for a time.Ticker
				late := time.Duration(nowTick)*self.tick - e.when
				e.when += e.period * (1 + late/e.period)
				self.linkWithLock(e)
			}
		}
		e = next
	}
}

func (self *shard) linkWithLock(e *entry) {
	due := int64((e.when + self.tick - 1) / self.tick)
	if due <= self.firedTick {
		due = self.firedTick + 1
	}
	e.due = due
	slot := due & self.mask
	head := self.slots[slot]
	e.prev = nil
	e.next = head
	if head != nil {
		head.prev = e
	}
	self.slots[slot] = e
	e.armed = true
	self.armedCount += 1
}

func (self *shard) unlinkWithLock(e *entry) bool {
	if !e.armed {
		return false
	}
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		self.slots[e.due&self.mask] = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	e.prev = nil
	e.next = nil
	e.armed = false
	self.armedCount -= 1
	return true
}

// Never blocks: a fire the owner has not received yet absorbs this one.
func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func drain(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}
