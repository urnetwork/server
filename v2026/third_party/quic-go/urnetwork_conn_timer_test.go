package quic

// urnetwork: the connection's run loop timer (resetTimer) against the Reset it
// replaced. Under virtual time, a loop like the connection's sets a deadline
// that stays, moves earlier or later, or is already due, then waits for the
// timer or for an event that comes first; the timer must fire at the same
// instants with either arming, and a new timer is armed only when the deadline
// changes or the last one fired.

import (
	"math/rand"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/quic-go/quic-go/internal/monotime"
)

// The instants the timer fires, as the time since the loop started, and the
// number of timers armed after the first.
func connTimerFiresForTest(t *testing.T, seed int64, reset func(c *Conn, deadline monotime.Time), received func(c *Conn)) ([]time.Duration, int) {
	var fires []time.Duration
	armed := 0
	synctest.Test(t, func(t *testing.T) {
		rng := rand.New(rand.NewSource(seed))
		start := monotime.Now()
		c := &Conn{}
		c.timerDeadline = start.Add(time.Second)
		c.timer = time.NewTimer(monotime.Until(c.timerDeadline))
		c.timerPending = true
		deadline := c.timerDeadline
		for range 1000 {
			switch rng.Intn(5) {
			case 0:
				// unchanged
			case 1:
				deadline = monotime.Now().Add(time.Duration(1+rng.Intn(50)) * time.Millisecond)
			case 2:
				deadline = deadline.Add(time.Duration(1+rng.Intn(20)) * time.Millisecond)
			case 3:
				deadline = deadline.Add(-time.Duration(1+rng.Intn(20)) * time.Millisecond)
			case 4:
				// already due
				deadline = monotime.Now().Add(-time.Millisecond)
			}
			timer := c.timer
			reset(c, deadline)
			if c.timer != timer {
				armed += 1
			}
			// an event comes after a while, never at the deadline's instant
			eventIn := time.Duration(1+rng.Intn(60)) * time.Millisecond
			if monotime.Now().Add(eventIn) == deadline {
				eventIn += time.Nanosecond
			}
			event := time.NewTimer(eventIn)
			select {
			case <-c.timer.C:
				received(c)
				fires = append(fires, monotime.Since(start))
			case <-event.C:
			}
			event.Stop()
		}
		c.timer.Stop()
	})
	return fires, armed
}

func TestUrnetworkConnTimerFiresAsTheResetDid(t *testing.T) {
	for seed := int64(1); seed <= 100; seed += 1 {
		fires, armed := connTimerFiresForTest(t, seed, func(c *Conn, deadline monotime.Time) {
			c.resetTimer(deadline)
		}, func(c *Conn) {
			c.timerPending = false
		})
		resetFires, _ := connTimerFiresForTest(t, seed, func(c *Conn, deadline monotime.Time) {
			c.timer.Reset(monotime.Until(deadline))
		}, func(c *Conn) {})
		if !slices.Equal(fires, resetFires) {
			t.Fatalf("seed %d: the timer fired at %v, with the reset at %v", seed, fires, resetFires)
		}
		if len(fires) < 100 {
			t.Fatalf("seed %d: the timer fired %d times: the loop did not exercise it", seed, len(fires))
		}
		if 1000 <= armed {
			t.Fatalf("seed %d: %d new timers for 1000 iterations: unchanged pending deadlines rearmed", seed, armed)
		}
	}
}

// A pending timer for the same deadline is kept; a changed deadline, or the
// same one after the timer fired, gets a new timer and leaves the last stopped.
func TestUrnetworkConnTimerKeepsAPendingDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &Conn{}
		deadline := monotime.Now().Add(time.Second)
		c.timerDeadline = deadline
		c.timer = time.NewTimer(monotime.Until(deadline))
		c.timerPending = true
		first := c.timer
		c.resetTimer(deadline)
		if c.timer != first {
			t.Fatal("an unchanged pending deadline armed a new timer")
		}
		c.resetTimer(deadline.Add(-time.Millisecond))
		if c.timer == first {
			t.Fatal("a changed deadline kept the timer")
		}
		if first.Stop() {
			t.Fatal("the replaced timer was still active")
		}
		<-c.timer.C
		c.timerPending = false
		fired := c.timer
		c.resetTimer(c.timerDeadline)
		if c.timer == fired {
			t.Fatal("the deadline of a timer that fired kept the timer")
		}
		c.timer.Stop()
	})
}
