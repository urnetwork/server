// Counts the runtime timers a function creates, from the allocation profile.
// A loop that calls time.After on every pass creates one timer per pass and
// abandons the ones that did not fire; a loop that reuses its timer creates
// one. The profile observes this without a production seam.
package connect

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// Profiles every allocation while work runs and returns the number of timers
// that function created itself, through time.NewTimer or time.After.
func countTimersCreated(t testing.TB, function string, work func()) int64 {
	t.Helper()
	rate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() {
		runtime.MemProfileRate = rate
	}()
	before := timersCreatedBy(function)
	work()
	return timersCreatedBy(function) - before
}

// The timers function has created so far. A garbage collection publishes the
// allocations of the cycle before it, so the profile is read after three.
func timersCreatedBy(function string) int64 {
	for range 3 {
		runtime.GC()
	}
	records := []runtime.MemProfileRecord{}
	for {
		n, ok := runtime.MemProfile(records, true)
		if ok {
			records = records[:n]
			break
		}
		records = make([]runtime.MemProfileRecord, n+64)
	}
	timerCount := int64(0)
	for _, record := range records {
		if timerCreator(record.Stack()) == function {
			timerCount += record.AllocObjects
		}
	}
	return timerCount
}

// The function that called time.NewTimer, directly or through time.After, for
// the allocation of a runtime timer, or "" for any other allocation.
func timerCreator(stack []uintptr) string {
	frames := runtime.CallersFrames(stack)
	timerAllocation := false
	newTimer := false
	for {
		frame, more := frames.Next()
		switch {
		case frame.Function == "runtime.newTimer" || frame.Function == "time.newTimer":
			timerAllocation = true
		case frame.Function == "time.NewTimer":
			newTimer = timerAllocation
		case newTimer && !strings.HasPrefix(frame.Function, "time."):
			return frame.Function
		}
		if !more {
			return ""
		}
	}
}

// Profiles every allocation while work runs and returns the number of sweep
// timers (internal/timersweep) that function created itself.
func countSweepTimersCreated(t testing.TB, function string, work func()) int64 {
	t.Helper()
	rate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() {
		runtime.MemProfileRate = rate
	}()
	before := allocationsBy(function, sweepTimerCreator)
	work()
	return allocationsBy(function, sweepTimerCreator) - before
}

// The allocations so far whose creator, by the given rule, is function. Read
// after three collections, as timersCreatedBy is.
func allocationsBy(function string, creator func([]uintptr) string) int64 {
	for range 3 {
		runtime.GC()
	}
	records := []runtime.MemProfileRecord{}
	for {
		n, ok := runtime.MemProfile(records, true)
		if ok {
			records = records[:n]
			break
		}
		records = make([]runtime.MemProfileRecord, n+64)
	}
	count := int64(0)
	for _, record := range records {
		if creator(record.Stack()) == function {
			count += record.AllocObjects
		}
	}
	return count
}

// The function that made a sweep timer, for the allocation of its channel, the
// one allocation of every sweep timer and ticker, or "" for any other.
func sweepTimerCreator(stack []uintptr) string {
	frames := runtime.CallersFrames(stack)
	channel := false
	sweepTimer := false
	for {
		frame, more := frames.Next()
		switch {
		case frame.Function == "runtime.makechan":
			channel = true
		case strings.HasPrefix(frame.Function, "runtime."):
		case frame.Function == "github.com/urnetwork/server/v2026/internal/timersweep.(*Sweep).NewTimer" ||
			frame.Function == "github.com/urnetwork/server/v2026/internal/timersweep.(*Sweep).NewTicker":
			if !channel {
				return ""
			}
			sweepTimer = true
		case sweepTimer:
			return frame.Function
		default:
			return ""
		}
		if !more {
			return ""
		}
	}
}

// Creates a timer per pass, as a select on time.After in a loop does.
func createTimerPerPassForTest(passCount int) {
	for range passCount {
		select {
		case <-time.After(time.Hour):
		default:
		}
	}
}

// Reuses one timer for every pass.
func reuseTimerForTest(passCount int) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for range passCount {
		timer.Reset(time.Hour)
	}
}

// The profile attributes each timer to the function that created it.
func TestCountTimersCreatedAttributesTimersToTheirCreator(t *testing.T) {
	perPass := countTimersCreated(t, "github.com/urnetwork/server/v2026/connect.createTimerPerPassForTest", func() {
		createTimerPerPassForTest(100)
	})
	if perPass != 100 {
		t.Fatalf("a time.After per pass counted %d timers for 100 passes", perPass)
	}
	reused := countTimersCreated(t, "github.com/urnetwork/server/v2026/connect.reuseTimerForTest", func() {
		reuseTimerForTest(100)
	})
	if reused != 1 {
		t.Fatalf("a reused timer counted %d timers for 100 passes", reused)
	}
}
