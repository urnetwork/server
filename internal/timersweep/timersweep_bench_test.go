package timersweep

import (
	"fmt"
	mathrand "math/rand/v2"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

// processCpu is the process's user and system CPU time.
func processCpu() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// The runtime timers of a connect process at the 2026-11-20 profile
// (LBALTOPT2.md §3.4): about 1.1M in the heaps of GOMAXPROCS 20, 55k a P.
const productionTimersPerP = 55_000

// The share of those made by the owners this change moves to the sweep, as
// the profile's table counts them: the announce rounds (147k), the passive
// samples (86k), the lease horizons (81k) and the resident (78k) and forward
// (73k) idle watches, 465k of 1.1M. The table counts none of the lease
// recheck tickers, resident registration refreshes, stream grant republishes
// or verify egress refreshes, which moved too, so production's heap before
// may be larger than "before" here. The census
// (connect/timer_census_db_test.go) counts one of each for every lease,
// resident and verifying connection.
const productionSweptShare = 465.0 / 1100.0

// Spreads count far-off runtime timers over the Ps' heaps, each next to a
// filler allocation, as a server's are scattered through memory, with random
// deadlines, so a heap's order has nothing to do with where its timers are:
// a heap walk's cost is a cache miss per timer. Returns the stop.
func spreadRuntimeTimers(count int) func() {
	workers := runtime.GOMAXPROCS(0)
	timers := make([][]*time.Timer, workers)
	filler := make([][][]byte, workers)
	var setup sync.WaitGroup
	for w := range workers {
		setup.Add(1)
		go func() {
			defer setup.Done()
			random := mathrand.New(mathrand.NewPCG(uint64(w), 1))
			for i := 0; i < count/workers; i += 1 {
				timers[w] = append(timers[w], time.AfterFunc(time.Hour+time.Duration(random.Int64N(int64(time.Hour))), func() {}))
				filler[w] = append(filler[w], make([]byte, 512))
				if i%1024 == 0 {
					runtime.Gosched()
				}
			}
		}()
	}
	setup.Wait()
	return func() {
		for _, workerTimers := range timers {
			for _, timer := range workerTimers {
				timer.Stop()
			}
		}
		runtime.KeepAlive(filler)
	}
}

// Arms count far-off timers on the sweep, each next to a filler allocation.
// Returns the stop.
func spreadSweepTimers(sweep *Sweep, count int) func() {
	timers := make([]*Timer, count)
	filler := make([][]byte, count)
	for i := range timers {
		timers[i] = sweep.NewTimer(time.Hour + time.Duration(i)*time.Millisecond)
		filler[i] = make([]byte, 512)
	}
	return func() {
		for _, timer := range timers {
			timer.Stop()
		}
		runtime.KeepAlive(filler)
	}
}

// The CPU of one walk of a P's timer heap, at a connect process's heap size
// before and after the converted owners' timers left it. With one P, each op
// moves a pending timer ahead of the heap's other modifications, as a socket
// deadline refresh does, so the P walks its whole heap when that deadline
// arrives microseconds later, and the moved timer then fires. The CPU per op
// is one walk and a fixed overhead. Profile with -cpuprofile for
// runtime.(*timers).check.
func BenchmarkTimerHeapWalkCost(b *testing.B) {
	for _, c := range []struct {
		name   string
		timers int
	}{
		{name: "before", timers: productionTimersPerP},
		{name: "after", timers: int(productionTimersPerP * (1 - productionSweptShare))},
	} {
		b.Run(c.name, func(b *testing.B) {
			procs := runtime.GOMAXPROCS(1)
			defer runtime.GOMAXPROCS(procs)
			stop := spreadRuntimeTimers(c.timers)
			defer stop()
			fired := make(chan struct{}, 1)
			moved := time.AfterFunc(time.Hour, func() {
				select {
				case fired <- struct{}{}:
				default:
				}
			})
			defer moved.Stop()
			b.ReportMetric(float64(c.timers), "heap-timers")
			runtime.GC()
			startCpu := processCpu()
			b.ResetTimer()
			for range b.N {
				// re-armed after its last fire, then moved earlier: the move
				// makes the next timer check walk the heap
				moved.Reset(time.Hour)
				moved.Reset(10 * time.Microsecond)
				for waiting := true; waiting; {
					select {
					case <-fired:
						waiting = false
					default:
						runtime.Gosched()
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(processCpu()-startCpu)/float64(b.N), "cpu-ns/op")
		})
	}
}

// What one source of timer changes costs in heap walks, by the horizon of its
// changes (CPUOPT7.md §11). Each op re-arms a channel timer and waits on it in
// a select that another goroutine's message ends first, as the connect
// library's ACK compression wait does when a quiet-head signal wakes it: the
// select leaves the timer in the heap as a zombie, and the next Reset moves
// it. On one P, whose heap holds a connect process's 55,000 timers or none of
// them, the P walks its heap when a moved deadline comes due, about
// 1/(op interval + horizon) times a second, so the same rate of changes costs
// a walk every few ops at a short horizon and almost none at a long one. The
// walk CPU of an op is its process CPU on the full heap less on the empty one.
func BenchmarkTimerWalksByChangeHorizon(b *testing.B) {
	for _, horizon := range []time.Duration{
		500 * time.Microsecond,
		5 * time.Millisecond,
		50 * time.Millisecond,
		500 * time.Millisecond,
		5 * time.Second,
		50 * time.Second,
	} {
		for _, c := range []struct {
			name   string
			timers int
		}{
			{name: "production", timers: productionTimersPerP},
			{name: "empty", timers: 0},
		} {
			b.Run(fmt.Sprintf("%s/%s", horizon, c.name), func(b *testing.B) {
				procs := runtime.GOMAXPROCS(1)
				defer runtime.GOMAXPROCS(procs)
				stop := spreadRuntimeTimers(c.timers)
				defer stop()
				// the message that ends each wait, sent once the wait has
				// blocked: with one P the sender runs only then
				ready := make(chan struct{}, 1)
				wake := make(chan struct{})
				done := make(chan struct{})
				go func() {
					for {
						select {
						case <-done:
							return
						case <-ready:
						}
						select {
						case <-done:
							return
						case wake <- struct{}{}:
						}
					}
				}()
				defer close(done)
				timer := time.NewTimer(time.Hour)
				defer timer.Stop()
				b.ReportMetric(float64(c.timers), "heap-timers")
				runtime.GC()
				startCpu := processCpu()
				startTime := time.Now()
				b.ResetTimer()
				for range b.N {
					timer.Reset(horizon)
					// a sender holding an earlier message needs no other: a
					// wait the timer ended left that message unreceived
					select {
					case ready <- struct{}{}:
					default:
					}
					select {
					case <-wake:
					case <-timer.C:
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(processCpu()-startCpu)/float64(b.N), "cpu-ns/op")
				b.ReportMetric(float64(b.N)/time.Since(startTime).Seconds(), "changes/s")
			})
		}
	}
}

// Keeps every shard's driver running, as a busy process's are, so a benchmark
// measures its timers rather than drivers starting. Returns the stop.
func keepDriversRunning(sweep *Sweep) func() {
	keep := make([]*Timer, len(sweep.shards))
	for i := range keep {
		keep[i] = sweep.NewTimer(1000 * time.Hour)
	}
	return func() {
		for _, timer := range keep {
			timer.Stop()
		}
	}
}

// The sweep's cost per registration: arming a timer and stopping it, against
// a runtime timer's, each from goroutines on every P.
func BenchmarkTimerArmStop(b *testing.B) {
	b.Run("sweep", func(b *testing.B) {
		sweep := New(DefaultTick)
		defer keepDriversRunning(sweep)()
		b.RunParallel(func(pb *testing.PB) {
			timer := sweep.NewTimer(time.Hour)
			defer timer.Stop()
			for pb.Next() {
				timer.Reset(time.Minute)
				timer.Stop()
			}
		})
	})
	b.Run("runtime", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			timer := time.AfterFunc(time.Hour, func() {})
			defer timer.Stop()
			for pb.Next() {
				timer.Reset(time.Minute)
				timer.Stop()
			}
		})
	})
}

// The cost of one fire and its receipt, with the sweep's ticks and the owner's
// wake, against a runtime channel timer's. Owners on every P wait a short
// timer and re-arm it after each fire; the process CPU per op is per fire.
func BenchmarkTimerFire(b *testing.B) {
	const owners = 4096
	const tick = time.Millisecond
	for _, c := range []struct {
		name  string
		owner func(*Sweep, int)
	}{
		{name: "sweep", owner: func(sweep *Sweep, fires int) {
			timer := sweep.NewTimer(tick)
			defer timer.Stop()
			for range fires {
				<-timer.C
				timer.Reset(tick)
			}
		}},
		{name: "runtime", owner: func(_ *Sweep, fires int) {
			timer := time.NewTimer(tick)
			defer timer.Stop()
			for range fires {
				<-timer.C
				timer.Reset(tick)
			}
		}},
	} {
		b.Run(c.name, func(b *testing.B) {
			sweep := New(tick)
			fires := max(1, b.N/owners)
			startCpu := processCpu()
			b.ResetTimer()
			var run sync.WaitGroup
			for range owners {
				run.Add(1)
				go func() {
					defer run.Done()
					c.owner(sweep, fires)
				}()
			}
			run.Wait()
			b.StopTimer()
			b.ReportMetric(float64(processCpu()-startCpu)/float64(fires*owners), "cpu-ns/fire")
		})
	}
}

// The memory of one timer with its channel: a sweep timer's, the shard's
// slots aside, against a runtime timer's.
func BenchmarkNewTimer(b *testing.B) {
	b.Run("sweep", func(b *testing.B) {
		sweep := New(DefaultTick)
		defer keepDriversRunning(sweep)()
		timers := make([]*Timer, 0, b.N)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			timers = append(timers, sweep.NewTimer(time.Hour))
		}
		b.StopTimer()
		for _, timer := range timers {
			timer.Stop()
		}
	})
	b.Run("runtime", func(b *testing.B) {
		timers := make([]*time.Timer, 0, b.N)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			timers = append(timers, time.NewTimer(time.Hour))
		}
		b.StopTimer()
		for _, timer := range timers {
			timer.Stop()
		}
	})
}

// The sweep's standing cost: its drivers' ticks over a production population
// of armed timers that are not due, per armed timer per second.
func BenchmarkSweepIdleTicks(b *testing.B) {
	count := productionTimersPerP * runtime.GOMAXPROCS(0)
	sweep := New(DefaultTick)
	stop := spreadSweepTimers(sweep, count)
	defer stop()
	runtime.GC()
	startCpu := processCpu()
	start := time.Now()
	b.ResetTimer()
	// one op is one tick of every shard
	time.Sleep(time.Duration(b.N) * DefaultTick)
	b.StopTimer()
	elapsed := time.Since(start)
	cpu := processCpu() - startCpu
	b.ReportMetric(float64(cpu)/float64(b.N*len(sweep.shards)), "cpu-ns/shard-tick")
	b.ReportMetric(float64(cpu)/elapsed.Seconds()/float64(count), "cpu-ns/timer-second")
}
