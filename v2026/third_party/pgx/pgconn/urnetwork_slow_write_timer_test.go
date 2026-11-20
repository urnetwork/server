package pgconn

// urnetwork: the slow write timer arms a new timer for each potential
// write-read deadlock (enterPotentialWriteReadDeadlock) instead of resetting
// the last one, which is still in the runtime's timer heap when writes follow
// each other within 15ms. Under virtual time the new arming starts the
// background reader at the same instants as the Reset it replaced, through
// writes that end before and after 15ms. The benchmarks measure what one
// avoided heap scan saves.

import (
	"io"
	"math"
	"math/rand"
	"runtime"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn/internal/bgreader"
)

// A PgConn with only what the slow write timer uses, its background reader on
// a pipe that is never written. Each start of the background reader is
// recorded, as the time since origin. close ends the background reader.
type slowWriteTimerConnForTest struct {
	pgConn    *PgConn
	startLock sync.Mutex
	starts    []time.Duration
	close     func()
}

func newSlowWriteTimerConnForTest(origin time.Time) *slowWriteTimerConnForTest {
	r, w := io.Pipe()
	c := &slowWriteTimerConnForTest{}
	pgConn := &PgConn{
		bgReader:        bgreader.New(r),
		bgReaderStarted: make(chan struct{}),
	}
	pgConn.slowWriteTimerFunc = func() {
		c.startLock.Lock()
		c.starts = append(c.starts, time.Since(origin))
		c.startLock.Unlock()
		pgConn.bgReader.Start()
		pgConn.bgReaderStarted <- struct{}{}
	}
	pgConn.slowWriteTimer = time.AfterFunc(time.Duration(math.MaxInt64), pgConn.slowWriteTimerFunc)
	pgConn.slowWriteTimer.Stop()
	c.pgConn = pgConn
	c.close = func() {
		w.Close()
	}
	return c
}

// The upstream arming, which resets the stopped timer.
func enterPotentialWriteReadDeadlockByResetForTest(pgConn *PgConn) {
	if pgConn.slowWriteTimer.Reset(15 * time.Millisecond) {
		panic("BUG: slow write timer already active")
	}
}

// The instants the background reader starts for writes of the given lengths,
// each followed by its gap, armed by enter.
func slowWriteTimerStartsForTest(t *testing.T, writes []time.Duration, gaps []time.Duration, enter func(*PgConn)) []time.Duration {
	var starts []time.Duration
	synctest.Test(t, func(t *testing.T) {
		c := newSlowWriteTimerConnForTest(time.Now())
		for i := range writes {
			enter(c.pgConn)
			time.Sleep(writes[i])
			c.pgConn.exitPotentialWriteReadDeadlock()
			time.Sleep(gaps[i])
		}
		c.close()
		c.startLock.Lock()
		starts = slices.Clone(c.starts)
		c.startLock.Unlock()
	})
	return starts
}

// The new arming starts the background reader at the same instants as the
// reset: 15ms after the start of each write that is still in progress then,
// and never for a write that ends sooner, whatever the gap since the last.
func TestUrnetworkSlowWriteTimerStartsTheReaderAsTheResetDid(t *testing.T) {
	for seed := int64(1); seed <= 50; seed += 1 {
		rng := rand.New(rand.NewSource(seed))
		writes := []time.Duration{}
		gaps := []time.Duration{}
		for range 200 {
			// whole milliseconds, never 15, so no write ends at the instant its
			// timer fires
			write := time.Duration(rng.Intn(40)) * time.Millisecond
			if write == 15*time.Millisecond {
				write += time.Millisecond
			}
			writes = append(writes, write)
			gaps = append(gaps, time.Duration(rng.Intn(30))*time.Millisecond)
		}
		fresh := slowWriteTimerStartsForTest(t, writes, gaps, func(pgConn *PgConn) {
			pgConn.enterPotentialWriteReadDeadlock()
		})
		reset := slowWriteTimerStartsForTest(t, writes, gaps, enterPotentialWriteReadDeadlockByResetForTest)
		if !slices.Equal(fresh, reset) {
			t.Fatalf("seed %d: the background reader started at %v, with the reset at %v", seed, fresh, reset)
		}
		want := []time.Duration{}
		at := time.Duration(0)
		for i := range writes {
			if 15*time.Millisecond < writes[i] {
				want = append(want, at+15*time.Millisecond)
			}
			at += writes[i] + gaps[i]
		}
		if !slices.Equal(fresh, want) {
			t.Fatalf("seed %d: the background reader started at %v, want %v", seed, fresh, want)
		}
	}
}

// Each write arms a timer of its own and leaves the last one stopped.
func TestUrnetworkSlowWriteTimerArmsATimerPerWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newSlowWriteTimerConnForTest(time.Now())
		defer c.close()
		last := c.pgConn.slowWriteTimer
		c.pgConn.enterPotentialWriteReadDeadlock()
		if c.pgConn.slowWriteTimer == last {
			t.Fatal("a write reused the last timer")
		}
		if last.Stop() {
			t.Fatal("the last timer was active")
		}
		c.pgConn.exitPotentialWriteReadDeadlock()
		time.Sleep(time.Hour)
		c.startLock.Lock()
		defer c.startLock.Unlock()
		if 0 < len(c.starts) {
			t.Fatalf("the background reader started at %v for a write that ended at once", c.starts)
		}
	})
}

// A second arming during a write raises the bug with the timer armed for 15ms
// from the second arming, as the reset left it.
func TestUrnetworkSlowWriteTimerBugLeavesTheTimerArmedAsTheResetDid(t *testing.T) {
	for _, enter := range []func(*PgConn){
		func(pgConn *PgConn) {
			pgConn.enterPotentialWriteReadDeadlock()
		},
		enterPotentialWriteReadDeadlockByResetForTest,
	} {
		synctest.Test(t, func(t *testing.T) {
			c := newSlowWriteTimerConnForTest(time.Now())
			enter(c.pgConn)
			time.Sleep(5 * time.Millisecond)
			recovered := func() (r any) {
				defer func() {
					r = recover()
				}()
				enter(c.pgConn)
				return nil
			}()
			if recovered != "BUG: slow write timer already active" {
				t.Fatalf("a second arming during a write raised %v, want the active timer bug", recovered)
			}
			time.Sleep(20 * time.Millisecond)
			c.pgConn.exitPotentialWriteReadDeadlock()
			c.close()
			c.startLock.Lock()
			defer c.startLock.Unlock()
			if !slices.Equal(c.starts, []time.Duration{20 * time.Millisecond}) {
				t.Fatalf("the background reader started at %v, want 20ms", c.starts)
			}
		})
	}
}

var spacingSinkForTest []byte

// What one avoided heap scan saves. A connect process holds about 32,000
// timers per P, spaced through its heap. On one P beside 32,768 such timers,
// each operation moves a timer that is in the heap to now, as the reset moves
// the slow write timer earlier, so the next scheduling point scans the heap
// (runtime.(*timers).adjust); the new arming replaces the timer instead, and
// the next scheduling point only runs it. The difference is the cost of one
// scan, against the new timer's allocation and insert.
func benchmarkTimerHeapScanForTest(b *testing.B, rearm func(timer *time.Timer, f func()) *time.Timer) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	heldTimers := []*time.Timer{}
	for i := range 32 * 1024 {
		heldTimers = append(heldTimers, time.AfterFunc(time.Hour+time.Duration(i), func() {}))
		// spaced as a long-running process's timers are, so a scan reads each
		// from memory rather than from a cache line it shares
		spacingSinkForTest = make([]byte, 4096)
	}
	spacingSinkForTest = nil
	defer func() {
		for _, heldTimer := range heldTimers {
			heldTimer.Stop()
		}
	}()
	fired := 0
	f := func() {
		fired += 1
	}
	timer := time.AfterFunc(time.Hour, f)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i += 1 {
		timer = rearm(timer, f)
		// the scheduling point: the P checks its timers and runs the due one
		runtime.Gosched()
	}
	b.StopTimer()
	timer.Stop()
	if fired < b.N/2 {
		b.Fatalf("the timer fired %d times in %d operations", fired, b.N)
	}
}

func BenchmarkUrnetworkTimerHeapScanReset(b *testing.B) {
	benchmarkTimerHeapScanForTest(b, func(timer *time.Timer, f func()) *time.Timer {
		// in the heap again, then moved earlier
		timer.Reset(time.Hour)
		timer.Reset(0)
		return timer
	})
}

func BenchmarkUrnetworkTimerHeapScanNew(b *testing.B) {
	benchmarkTimerHeapScanForTest(b, func(timer *time.Timer, f func()) *time.Timer {
		timer.Stop()
		return time.AfterFunc(0, f)
	})
}
