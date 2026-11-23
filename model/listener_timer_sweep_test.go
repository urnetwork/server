// A resident's stream hop and network peer listeners wait for their backstop
// polls on the process's timer sweep (CPUOPT7.md §11): each poll comes at its
// deadline or at most one sweep tick after it, never before; the inputs that
// leave the deadline where it was (a kick, a delta, a snapshot) leave the
// sweep timer armed for it; and the timer leaves the sweep when the listener
// ends. Each test runs the listener in a synctest bubble with a sweep of the
// bubble's own, its every shard ticking from the sweep's start, and the
// listener starting at an offset that is not a multiple of the tick, so its
// deadlines fall between ticks.
package model

import (
	"context"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/internal/timersweep"
)

// A sweep made inside the bubble whose every shard counts its ticks from the
// epoch: one pacer timer per shard keeps each shard's driver running.
type listenerBubbleSweep struct {
	sweep  *timersweep.Sweep
	epoch  time.Time
	pacers []*timersweep.Timer
}

func newListenerBubbleSweep() *listenerBubbleSweep {
	sweep := timersweep.New(timersweep.DefaultTick)
	epoch := time.Now()
	// new timers take the shards in turn, so this many reach every shard
	pacers := make([]*timersweep.Timer, runtime.GOMAXPROCS(0))
	for i := range pacers {
		pacers[i] = sweep.NewTimer(1000 * time.Hour)
	}
	return &listenerBubbleSweep{
		sweep:  sweep,
		epoch:  epoch,
		pacers: pacers,
	}
}

// The sweep's first tick at or after deadline.
func (self *listenerBubbleSweep) fireTime(deadline time.Time) time.Time {
	tick := self.sweep.Tick()
	elapsed := deadline.Sub(self.epoch)
	return self.epoch.Add((elapsed + tick - 1) / tick * tick)
}

// The listeners' timers armed now, the pacers aside.
func (self *listenerBubbleSweep) ownerArmedCount() int {
	return self.sweep.ArmedCount() - len(self.pacers)
}

func (self *listenerBubbleSweep) close() {
	for _, pacer := range self.pacers {
		pacer.Stop()
	}
}

// A poll within the sweep's bound: at or after its deadline, less than a tick
// after it, at the tick the sweep fires the deadline.
func requireListenerSweepFire(t *testing.T, sweep *listenerBubbleSweep, what string, deadline time.Time, at time.Time) {
	t.Helper()
	if at.Before(deadline) {
		t.Fatalf("%s came %s before its deadline", what, deadline.Sub(at))
	}
	if late := at.Sub(deadline); sweep.sweep.Tick() <= late {
		t.Fatalf("%s came %s after its deadline, a sweep tick or more", what, late)
	}
	if want := sweep.fireTime(deadline); !at.Equal(want) {
		t.Fatalf("%s came %s after its deadline, want %s", what, at.Sub(deadline), want.Sub(deadline))
	}
	if deadline.Equal(sweep.fireTime(deadline)) {
		t.Fatalf("%s's deadline is on a tick; the test needs one between ticks", what)
	}
}

// The next value, which must have come already.
func takeListenerSweepValue(t *testing.T, what string, values <-chan time.Time) time.Time {
	t.Helper()
	select {
	case value := <-values:
		return value
	default:
		t.Fatalf("no %s", what)
		return time.Time{}
	}
}

// That nothing came.
func requireNoListenerSweepValue(t *testing.T, what string, values <-chan time.Time) {
	t.Helper()
	select {
	case value := <-values:
		t.Fatalf("unexpected %s at %s", what, value)
	default:
	}
}

// The stream hop listener's poll comes within a sweep tick of its deadline, a
// kick reads without moving the deadline, and the poll's timer leaves the
// sweep when the listener ends.
func TestStreamHopListenerPollWaitsOnTheSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sweep := newListenerBubbleSweep()
		defer sweep.close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.Sleep(37*time.Millisecond + 13*time.Microsecond)

		reads := make(chan time.Time, 16)
		deadlines := make(chan time.Time, 16)
		// every pass reads the snapshot, so no pass reads the version from Redis
		listener := newStreamHopListenerWithPollArm(
			ctx,
			server.NewId(),
			func(*StreamHopEvent) {},
			10*time.Second,
			1,
			func(context.Context, server.Id) (int64, map[StreamHop]bool) {
				reads <- time.Now()
				return 1, map[StreamHop]bool{}
			},
			sweep.sweep,
			func(deadline time.Time) {
				deadlines <- deadline
			},
		)
		synctest.Wait()
		deadline := takeListenerSweepValue(t, "first poll deadline", deadlines)
		if armed := sweep.ownerArmedCount(); armed != 1 {
			t.Fatalf("%d listener timers on the sweep, want 1", armed)
		}

		// a kick reads now and leaves the deadline where it was
		time.Sleep(time.Until(deadline) / 2)
		listener.Kick()
		synctest.Wait()
		takeListenerSweepValue(t, "kick's read", reads)
		requireNoListenerSweepValue(t, "deadline from a kick", deadlines)

		fireTime := sweep.fireTime(deadline)
		time.Sleep(time.Until(fireTime) - time.Nanosecond)
		synctest.Wait()
		requireNoListenerSweepValue(t, "poll before its tick", reads)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		requireListenerSweepFire(t, sweep, "the poll", deadline, takeListenerSweepValue(t, "poll", reads))

		// the next deadline counts from the poll, and its poll keeps the bound
		nextDeadline := takeListenerSweepValue(t, "next poll deadline", deadlines)
		time.Sleep(time.Until(sweep.fireTime(nextDeadline)))
		synctest.Wait()
		requireListenerSweepFire(t, sweep, "the next poll", nextDeadline, takeListenerSweepValue(t, "next poll", reads))

		listener.CloseAndWait()
		if armed := sweep.ownerArmedCount(); armed != 0 {
			t.Fatalf("%d listener timers on the sweep after the listener ended", armed)
		}
	})
}

// The network peer listener's poll comes within a sweep tick of its deadline;
// a delta and a snapshot leave the deadline where it was, and a resync, whose
// full read stands for the poll, moves it; and the poll's timer leaves the
// sweep when the listener ends.
func TestNetworkPeerListenerPollWaitsOnTheSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sweep := newListenerBubbleSweep()
		defer sweep.close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.Sleep(41*time.Millisecond + 7*time.Microsecond)

		reads := make(chan time.Time, 16)
		deadlines := make(chan time.Time, 16)
		events := make(chan NetworkPeerEventType, 16)
		// every pass reads the peers, so no pass reads the version from Redis
		listener := newNetworkPeerListener(
			ctx,
			server.NewId(),
			func(event *NetworkPeerEvent) {
				events <- event.NetworkPeerEventType
			},
			10*time.Second,
			1,
			func(context.Context, server.Id) (int64, []*NetworkPeer) {
				reads <- time.Now()
				return 1, nil
			},
			sweep.sweep,
			func(deadline time.Time) {
				deadlines <- deadline
			},
		)
		synctest.Wait()
		deadline := takeListenerSweepValue(t, "first poll deadline", deadlines)
		if armed := sweep.ownerArmedCount(); armed != 1 {
			t.Fatalf("%d listener timers on the sweep, want 1", armed)
		}

		// a snapshot and a delta are applied and leave the deadline
		time.Sleep(time.Until(deadline) / 3)
		listener.ApplySnapshot(PrepareNetworkPeerSnapshot(2, nil))
		synctest.Wait()
		listener.ApplyDelta(newNetworkPeerDelta(server.NewId(), "del", func() networkPeerDeltaValue {
			return networkPeerDeltaValue{}
		}))
		synctest.Wait()
		if len(events) != 2 {
			t.Fatalf("%d events from a snapshot and a delta, want 2", len(events))
		}
		requireNoListenerSweepValue(t, "deadline from a snapshot or a delta", deadlines)
		requireNoListenerSweepValue(t, "read from a snapshot or a delta", reads)

		fireTime := sweep.fireTime(deadline)
		time.Sleep(time.Until(fireTime) - time.Nanosecond)
		synctest.Wait()
		requireNoListenerSweepValue(t, "poll before its tick", reads)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		requireListenerSweepFire(t, sweep, "the poll", deadline, takeListenerSweepValue(t, "poll", reads))
		pollDeadline := takeListenerSweepValue(t, "deadline after the poll", deadlines)

		// a resync reads at once and moves the deadline from itself
		time.Sleep(time.Until(pollDeadline) / 2)
		listener.Resync()
		synctest.Wait()
		resyncRead := takeListenerSweepValue(t, "resync's read", reads)
		resyncDeadline := takeListenerSweepValue(t, "deadline after the resync", deadlines)
		if !resyncDeadline.After(pollDeadline) {
			t.Fatalf("the resync at %s left the deadline at or before %s", resyncRead, pollDeadline)
		}
		time.Sleep(time.Until(sweep.fireTime(pollDeadline)))
		synctest.Wait()
		requireNoListenerSweepValue(t, "poll at the replaced deadline", reads)
		time.Sleep(time.Until(sweep.fireTime(resyncDeadline)))
		synctest.Wait()
		requireListenerSweepFire(t, sweep, "the poll after the resync", resyncDeadline, takeListenerSweepValue(t, "poll after the resync", reads))

		listener.CloseAndWait()
		if armed := sweep.ownerArmedCount(); armed != 0 {
			t.Fatalf("%d listener timers on the sweep after the listener ended", armed)
		}
	})
}
