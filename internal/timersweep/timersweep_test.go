package timersweep

import (
	"math"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The goroutines of a sweep's drivers, retired ones that have not returned
// included.
func (self *Sweep) runningDriverCount() int {
	count := 0
	for _, shard := range self.shards {
		count += int(shard.runningDriverCount.Load())
	}
	return count
}

// The time each fire of c arrives, on a goroutine of its own.
type fireLog struct {
	stateLock sync.Mutex
	times     []time.Time
}

func (self *fireLog) receive(c <-chan struct{}, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-c:
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.times = append(self.times, time.Now())
			}()
		}
	}
}

func (self *fireLog) offsets(start time.Time) []time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	offsets := make([]time.Duration, 0, len(self.times))
	for _, at := range self.times {
		offsets = append(offsets, at.Sub(start))
	}
	return offsets
}

func ceilTicks(d time.Duration, tick time.Duration) time.Duration {
	return (d + tick - 1) / tick * tick
}

// Each timer fires at the first tick boundary at or after its deadline: never
// early and less than a tick late. A deadline more than a rotation out shares
// its slot with nearer ones and still fires only at its own tick.
func TestTimerFiresAtTheFirstTickAtOrAfterItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const tick = 100 * time.Millisecond
		// 16 slots: a rotation is 1.6 s
		sweep := newSweep(tick, 1, 16)
		start := time.Now()
		deadlines := []time.Duration{
			time.Nanosecond,
			50 * time.Millisecond,
			tick,
			tick + time.Nanosecond,
			250 * time.Millisecond,
			time.Second,
			// the 1 s timer's slot, a rotation later
			2600 * time.Millisecond,
			// and two rotations later, off the boundary
			4199 * time.Millisecond,
			time.Hour + 7*time.Millisecond,
		}
		done := make(chan struct{})
		logs := make([]*fireLog, len(deadlines))
		timers := make([]*Timer, len(deadlines))
		for i, deadline := range deadlines {
			timers[i] = sweep.NewTimer(deadline)
			logs[i] = &fireLog{}
			go logs[i].receive(timers[i].C, done)
		}
		time.Sleep(time.Hour + time.Second)
		synctest.Wait()
		close(done)
		for i, deadline := range deadlines {
			offsets := logs[i].offsets(start)
			if len(offsets) != 1 {
				t.Fatalf("the %s timer fired %d times, want once", deadline, len(offsets))
			}
			if want := ceilTicks(deadline, tick); offsets[0] != want {
				t.Errorf("the %s timer fired at %s, want the tick at %s", deadline, offsets[0], want)
			}
			if offsets[0] < deadline || deadline+tick <= offsets[0] {
				t.Errorf("the %s timer fired at %s, outside [deadline, deadline+tick)", deadline, offsets[0])
			}
		}
		synctest.Wait()
		if count := sweep.ArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after every one fired", count)
		}
		if count := sweep.runningDriverCount(); count != 0 {
			t.Fatalf("%d drivers running with no timer armed", count)
		}
	})
}

// Random deadlines, armed at random times and reset before they fire, on a
// sweep with several shards: every fire comes at or after its deadline and
// within a tick of it.
func TestTimerNeverFiresEarlyAndAtMostATickLate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const tick = 100 * time.Millisecond
		sweep := newSweep(tick, 4, 64)
		random := rand.New(rand.NewPCG(1, 2))
		var waits sync.WaitGroup
		for range 200 {
			armDelay := time.Duration(random.Int64N(int64(10 * time.Second)))
			first := time.Duration(1 + random.Int64N(int64(20*time.Second)))
			second := time.Duration(1 + random.Int64N(int64(20*time.Second)))
			resetAfter := time.Duration(random.Int64N(int64(first)))
			waits.Add(1)
			go func() {
				defer waits.Done()
				time.Sleep(armDelay)
				timer := sweep.NewTimer(first)
				defer timer.Stop()
				time.Sleep(resetAfter)
				resetAt := time.Now()
				timer.Reset(second)
				<-timer.C
				late := time.Since(resetAt) - second
				if late < 0 || tick <= late {
					t.Errorf("a timer reset for %s fired %s after its deadline", second, late)
				}
			}()
		}
		waits.Wait()
		synctest.Wait()
		if count := sweep.runningDriverCount(); count != 0 {
			t.Fatalf("%d drivers running with no timer armed", count)
		}
	})
}

// Reset replaces the pending deadline, earlier or later, and discards a fire
// the owner has not received; Stop discards one too. Both report whether the
// timer was pending.
func TestTimerResetAndStopLeaveNoStaleFire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sweep := newSweep(100*time.Millisecond, 1, 16)
		start := time.Now()
		timer := sweep.NewTimer(10 * time.Second)

		time.Sleep(time.Second)
		if !timer.Reset(500 * time.Millisecond) {
			t.Fatal("Reset of an armed timer reported it was not pending")
		}
		<-timer.C
		if at := time.Since(start); at != 1500*time.Millisecond {
			t.Fatalf("the timer moved earlier fired at %s, want 1.5s", at)
		}

		// a later deadline replaces an earlier one
		timer.Reset(time.Second)
		time.Sleep(500 * time.Millisecond)
		timer.Reset(2 * time.Second)
		time.Sleep(time.Second)
		select {
		case <-timer.C:
			t.Fatal("the timer fired at a deadline it was moved from")
		default:
		}
		<-timer.C
		if at := time.Since(start); at != 4*time.Second {
			t.Fatalf("the timer moved later fired at %s, want 4s", at)
		}

		// a fire the owner never received
		timer.Reset(100 * time.Millisecond)
		time.Sleep(time.Second)
		if !timer.Reset(time.Second) {
			t.Fatal("Reset of a timer with an unreceived fire reported it was not pending")
		}
		select {
		case <-timer.C:
			t.Fatal("a fire received after Reset")
		default:
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if !timer.Stop() {
			t.Fatal("Stop of a timer with an unreceived fire reported it was not pending")
		}
		select {
		case <-timer.C:
			t.Fatal("a fire received after Stop")
		default:
		}
		if timer.Stop() {
			t.Fatal("Stop of a stopped timer reported it was pending")
		}

		// a nonpositive duration fires at once, as for a runtime timer
		resetAt := time.Now()
		timer.Reset(0)
		<-timer.C
		if waited := time.Since(resetAt); waited != 0 {
			t.Fatalf("a timer reset for 0 fired after %s", waited)
		}
		synctest.Wait()
		if count := sweep.runningDriverCount(); count != 0 {
			t.Fatalf("%d drivers running with no timer armed", count)
		}
	})
}

// Reset and Stop at the very instant the timer is due, racing the driver's
// fire: after either returns, no stale fire arrives.
func TestTimerResetAtItsDeadlineLeavesNoStaleFire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sweep := newSweep(100*time.Millisecond, 1, 16)
		for i := range 50 {
			timer := sweep.NewTimer(time.Second)
			time.Sleep(time.Second)
			if i%2 == 0 {
				timer.Reset(time.Second)
				resetAt := time.Now()
				select {
				case <-timer.C:
					t.Fatal("a stale fire arrived after Reset")
				default:
				}
				<-timer.C
				if waited := time.Since(resetAt); waited != time.Second {
					t.Fatalf("the reset timer fired after %s, want 1s", waited)
				}
			} else {
				timer.Stop()
				time.Sleep(2 * time.Second)
				select {
				case <-timer.C:
					t.Fatal("a stale fire arrived after Stop")
				default:
				}
			}
		}
	})
}

// A ticker keeps its cadence from its start, each fire never early and less
// than a tick late, with no drift. A fire the owner has not received absorbs
// the ones after it, and the cadence goes on.
func TestTickerKeepsItsCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const tick = 100 * time.Millisecond
		sweep := newSweep(tick, 1, 16)
		start := time.Now()
		const period = 250 * time.Millisecond
		ticker := sweep.NewTicker(period)
		for n := 1; n <= 400; n += 1 {
			<-ticker.C
			deadline := time.Duration(n) * period
			if at := time.Since(start); at != ceilTicks(deadline, tick) {
				t.Fatalf("fire %d at %s, want %s", n, at, ceilTicks(deadline, tick))
			}
		}
		// the owner falls behind by ten periods, waking between two fires
		time.Sleep(10*period + 30*time.Millisecond)
		<-ticker.C
		select {
		case <-ticker.C:
			t.Fatal("missed fires queued up")
		default:
		}
		<-ticker.C
		if at := time.Since(start); at != ceilTicks(411*period, tick) {
			t.Fatalf("after the missed fires the ticker fired at %s, want %s", at, ceilTicks(411*period, tick))
		}
		ticker.Stop()
		synctest.Wait()
		if count := sweep.runningDriverCount(); count != 0 {
			t.Fatalf("%d drivers running after the ticker stopped", count)
		}
	})
}

// An owner that never receives does not hold up the sweep: the other timers
// on its shard fire on time.
func TestSweepNeverWaitsOnAnOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sweep := newSweep(100*time.Millisecond, 1, 16)
		ignored := make([]*Ticker, 100)
		for i := range ignored {
			ignored[i] = sweep.NewTicker(100 * time.Millisecond)
		}
		start := time.Now()
		timer := sweep.NewTimer(10 * time.Second)
		<-timer.C
		if at := time.Since(start); at != 10*time.Second {
			t.Fatalf("a timer beside unreceived tickers fired at %s, want 10s", at)
		}
		for _, ticker := range ignored {
			ticker.Stop()
		}
	})
}

// A shard's driver runs only while the shard has an armed timer: Stop of the
// last one ends it, as does the fire of the last one, and the next arm starts
// another.
func TestDriverRunsOnlyWhileATimerIsArmed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sweep := newSweep(100*time.Millisecond, 2, 16)
		if count := sweep.runningDriverCount(); count != 0 {
			t.Fatalf("a new sweep runs %d drivers", count)
		}
		first := sweep.NewTimer(time.Minute)
		second := sweep.NewTimer(time.Minute)
		synctest.Wait()
		if count := sweep.runningDriverCount(); count != 2 {
			t.Fatalf("two armed timers on two shards run %d drivers, want 2", count)
		}
		first.Stop()
		synctest.Wait()
		if count := sweep.runningDriverCount(); count != 1 {
			t.Fatalf("one armed timer runs %d drivers, want 1", count)
		}
		<-second.C
		synctest.Wait()
		if count := sweep.runningDriverCount(); count != 0 {
			t.Fatalf("%d drivers running after the last timer fired", count)
		}
		start := time.Now()
		second.Reset(time.Second)
		<-second.C
		if at := time.Since(start); at != time.Second {
			t.Fatalf("a timer armed on a restarted driver fired at %s, want 1s", at)
		}
		synctest.Wait()
		if count := sweep.runningDriverCount(); count != 0 {
			t.Fatalf("%d drivers running after the last timer fired", count)
		}
	})
}

// A deadline too far out to count never fires and does not wrap around to
// an early one.
func TestTimerBeyondTheWheelsRangeNeverFires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sweep := newSweep(100*time.Millisecond, 1, 16)
		timer := sweep.NewTimer(time.Duration(math.MaxInt64))
		near := sweep.NewTimer(time.Second)
		<-near.C
		time.Sleep(time.Hour)
		select {
		case <-timer.C:
			t.Fatal("a timer armed for the end of time fired")
		default:
		}
		if !timer.Stop() {
			t.Fatal("the far timer was not pending")
		}
	})
}

// A timer of a nil sweep has a sweep of its own. Its tick is the process's
// for a production-length wait and a sixteenth of a shorter one, down to a
// millisecond, so a test that compresses a wait to milliseconds keeps the
// sweep's proportions.
func TestNilSweepTimerHasASweepOfItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var sweep *Sweep
		for _, c := range []struct {
			wait time.Duration
			tick time.Duration
		}{
			{wait: 500 * time.Microsecond, tick: time.Millisecond},
			{wait: 7 * time.Millisecond, tick: time.Millisecond},
			{wait: 150 * time.Millisecond, tick: 9375 * time.Microsecond},
			{wait: 1601 * time.Millisecond, tick: DefaultTick},
			{wait: 5*time.Minute + 3*time.Millisecond, tick: DefaultTick},
		} {
			if tick := privateTick(c.wait); tick != c.tick {
				t.Fatalf("a %s wait has a %s tick, want %s", c.wait, tick, c.tick)
			}
			start := time.Now()
			timer := sweep.NewTimer(c.wait)
			<-timer.C
			if at := time.Since(start); at != ceilTicks(c.wait, c.tick) {
				t.Fatalf("a %s timer fired at %s, want %s", c.wait, at, ceilTicks(c.wait, c.tick))
			}
		}
		other := sweep.NewTimer(time.Hour)
		start := time.Now()
		ticker := sweep.NewTicker(time.Second)
		<-ticker.C
		<-ticker.C
		if at := time.Since(start); at != 2*time.Second {
			t.Fatalf("the ticker's second fire came at %s, want 2s", at)
		}
		if other.shard == ticker.shard {
			t.Fatal("two timers of a nil sweep share a shard")
		}
		ticker.Stop()
		other.Stop()
	})
}

// A driver held up for more than a rotation fires every timer that came due
// while it waited, once, when it next runs, and none early.
func TestLateDriverFiresEveryDueTimerOnce(t *testing.T) {
	const tick = time.Millisecond
	// 16 slots: a rotation is 16 ms
	sweep := newSweep(tick, 1, 16)
	shard := sweep.shards[0]
	start := time.Now()
	deadlines := []time.Duration{3 * time.Millisecond, 9 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 400 * time.Millisecond}
	timers := make([]*Timer, len(deadlines))
	for i, deadline := range deadlines {
		timers[i] = sweep.NewTimer(deadline)
	}
	// hold the driver for more than three rotations
	func() {
		shard.stateLock.Lock()
		defer shard.stateLock.Unlock()
		time.Sleep(60 * time.Millisecond)
	}()
	for i, deadline := range deadlines[:4] {
		select {
		case <-timers[i].C:
		case <-time.After(time.Second):
			t.Fatalf("the %s timer did not fire after the driver resumed", deadline)
		}
		if elapsed := time.Since(start); elapsed < deadline {
			t.Fatalf("the %s timer fired early, at %s", deadline, elapsed)
		}
		select {
		case <-timers[i].C:
			t.Fatalf("the %s timer fired twice", deadline)
		default:
		}
	}
	select {
	case <-timers[4].C:
		if elapsed := time.Since(start); elapsed < deadlines[4] {
			t.Fatalf("the 400ms timer fired early, at %s", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the 400ms timer did not fire")
	}
	waitForDrivers(t, sweep, 0)
}

func waitForDrivers(t *testing.T, sweep *Sweep, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for sweep.runningDriverCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%d drivers running, want %d", sweep.runningDriverCount(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// Owners on every shard arm, reset, stop and receive at once, in real time:
// each fire is on time and the shards end empty with their drivers gone.
func TestSweepConcurrentOwners(t *testing.T) {
	const tick = time.Millisecond
	sweep := newSweep(tick, runtime.GOMAXPROCS(0), 64)
	var owners sync.WaitGroup
	for o := range 64 {
		owners.Add(1)
		go func() {
			defer owners.Done()
			random := rand.New(rand.NewPCG(uint64(o), 7))
			timer := sweep.NewTimer(time.Hour)
			defer timer.Stop()
			for range 200 {
				d := time.Duration(random.Int64N(int64(3 * time.Millisecond)))
				switch random.IntN(3) {
				case 0:
					timer.Stop()
				case 1:
					timer.Reset(time.Hour)
				default:
					armedAt := time.Now()
					timer.Reset(d)
					<-timer.C
					if elapsed := time.Since(armedAt); elapsed < d {
						t.Errorf("a %s timer fired after %s", d, elapsed)
					}
				}
			}
		}()
	}
	owners.Wait()
	if count := sweep.ArmedCount(); count != 0 {
		t.Fatalf("%d timers armed after every owner stopped", count)
	}
	waitForDrivers(t, sweep, 0)
}
