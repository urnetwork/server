package timersweep

import (
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
// the profile counted them: the announce rounds (147k), the passive samples
// (86k), the lease horizons (81k) and the resident (78k) and forward (73k)
// idle watches, 465k of 1.1M. The lease recheck tickers, the resident
// registration refreshes and the verify egress refreshes also moved; the
// profile's table does not count them, so the "after" heap here keeps them,
// and the census (connect/timer_census_db_test.go) counts one of each for
// every lease, resident and verifying connection.
const productionSweptShare = 465.0 / 1100.0

// Spreads count far-off runtime timers over the Ps' heaps, each next to a
// filler allocation, as a server's are scattered through memory: a heap
// walk's cost is a cache miss per timer. Returns the stop.
func spreadRuntimeTimers(count int) func() {
	workers := runtime.GOMAXPROCS(0)
	timers := make([][]*time.Timer, workers)
	filler := make([][][]byte, workers)
	var setup sync.WaitGroup
	for w := range workers {
		setup.Add(1)
		go func() {
			defer setup.Done()
			for i := 0; i < count/workers; i += 1 {
				timers[w] = append(timers[w], time.AfterFunc(time.Hour+time.Duration(i)*time.Millisecond, func() {}))
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

// A connect process's timer heaps before and after the converted owners'
// timers moved to a sweep, under the modifications that set off the walks.
// Each op moves a pending runtime timer a short horizon ahead, as a socket
// deadline refresh does (§3.4: some modification keeps a deadline about
// 100–250 ms ahead), from goroutines spread over the Ps, so every P walks its
// heap when the moved deadline arrives, and idle Ps walk others' heaps as they
// steal. The process CPU per op includes those walks, and in "after" the
// sweep's own ticks. Profile with -cpuprofile for runtime.(*timers).check.
func BenchmarkTimerHeapWalk(b *testing.B) {
	for _, c := range []struct {
		name       string
		sweptShare float64
	}{
		{name: "before", sweptShare: 0},
		{name: "after", sweptShare: productionSweptShare},
	} {
		b.Run(c.name, func(b *testing.B) {
			total := productionTimersPerP * runtime.GOMAXPROCS(0)
			sweptCount := int(float64(total) * c.sweptShare)
			stopRuntime := spreadRuntimeTimers(total - sweptCount)
			defer stopRuntime()
			sweep := New(DefaultTick)
			stopSwept := spreadSweepTimers(sweep, sweptCount)
			defer stopSwept()
			b.ReportMetric(float64(total-sweptCount)/float64(runtime.GOMAXPROCS(0)), "heap-timers/P")
			runtime.GC()

			loops := 4 * runtime.GOMAXPROCS(0)
			startCpu := processCpu()
			b.ResetTimer()
			var run sync.WaitGroup
			for l := range loops {
				run.Add(1)
				go func() {
					defer run.Done()
					deadline := time.AfterFunc(time.Hour, func() {})
					defer deadline.Stop()
					for i := l; i < b.N; i += loops {
						deadline.Reset(150 * time.Millisecond)
						time.Sleep(time.Millisecond)
					}
				}()
			}
			run.Wait()
			b.StopTimer()
			b.ReportMetric(float64(processCpu()-startCpu)/float64(b.N), "cpu-ns/op")
		})
	}
}

// The sweep's cost per registration: arming a timer and stopping it, against
// a runtime timer's, each from goroutines on every P.
func BenchmarkTimerArmStop(b *testing.B) {
	b.Run("sweep", func(b *testing.B) {
		sweep := New(DefaultTick)
		// a shard's driver runs while any timer is armed, as in a process
		keep := sweep.NewTimer(time.Hour)
		defer keep.Stop()
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
		keep := sweep.NewTimer(time.Hour)
		defer keep.Stop()
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
