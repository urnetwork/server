package session

// The lease's horizon timer and recheck ticker wait on the process's timer
// sweep (server LBALTOPT2.md §8.1). Each test runs the real lease goroutines in
// a synctest bubble, with a sweep of the bubble's own whose ticks do not fall
// on the lease's deadlines: every fire comes at its deadline or at most one
// sweep tick after it, never before, and a closed lease leaves the sweep.

import (
	"context"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server/v2026/internal/timersweep"
)

// A sweep made inside the bubble whose every shard counts its ticks from now:
// one pacer per shard keeps the shard's driver, and so its epoch, running.
type leaseTestSweep struct {
	sweep  *timersweep.Sweep
	epoch  time.Time
	pacers []*timersweep.Timer
}

func newLeaseTestSweep() *leaseTestSweep {
	sweep := timersweep.New(timersweep.DefaultTick)
	epoch := time.Now()
	pacers := make([]*timersweep.Timer, runtime.GOMAXPROCS(0))
	for i := range pacers {
		pacers[i] = sweep.NewTimer(1000 * time.Hour)
	}
	return &leaseTestSweep{sweep: sweep, epoch: epoch, pacers: pacers}
}

// The sweep's first tick at or after deadline.
func (self *leaseTestSweep) fireTime(deadline time.Time) time.Time {
	tick := self.sweep.Tick()
	return self.epoch.Add((deadline.Sub(self.epoch) + tick - 1) / tick * tick)
}

func (self *leaseTestSweep) ownerArmedCount() int {
	return self.sweep.ArmedCount() - len(self.pacers)
}

func (self *leaseTestSweep) close() {
	for _, pacer := range self.pacers {
		pacer.Stop()
	}
}

func (self *leaseTestSweep) requireFire(t *testing.T, what string, deadline time.Time, at time.Time) {
	t.Helper()
	if at.Before(deadline) {
		t.Fatalf("%s came %s before its deadline", what, deadline.Sub(at))
	}
	if late := at.Sub(deadline); self.sweep.Tick() <= late {
		t.Fatalf("%s came %s after its deadline, a sweep tick or more", what, late)
	}
	if want := self.fireTime(deadline); !at.Equal(want) {
		t.Fatalf("%s came %s after its deadline, want %s", what, at.Sub(deadline), want.Sub(deadline))
	}
}

// The credential's horizon retires the transport at the horizon or at most
// one sweep tick after it, never before, while the store does not answer.
// Closing ends both lease goroutines, and their timers leave the sweep.
func TestAuthorizationLeaseHorizonRetiresWithinASweepTick(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		// the sweep's ticks fall 37 ms past each whole second
		time.Sleep(37 * time.Millisecond)
		sweep := newLeaseTestSweep()
		defer sweep.close()

		credential := scriptedLeaseCredential()
		sessionId := credential.NetworkId
		credential.SessionId = &sessionId
		horizon := start.Add(3 * time.Minute)
		credential.ExpiresAt = gojwt.NewNumericDate(horizon.Add(-SessionGrace - clockLeeway))
		check := &scriptedLeaseCheck{
			results:   []error{nil},
			otherwise: ErrAuthUnavailable,
		}
		closes := make(chan leaseClose, 1)
		lease, err := startAuthorizationLeaseWithSweep(context.Background(), credential, func(code int) {
			closes <- leaseClose{code: code, at: time.Now()}
		}, nil, check.check, sweep.sweep)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		synctest.Wait()
		if count := sweep.ownerArmedCount(); count != 2 {
			t.Fatalf("the lease armed %d sweep timers, want 2: its horizon and its rechecks", count)
		}

		time.Sleep(time.Until(horizon) - time.Nanosecond)
		synctest.Wait()
		select {
		case retired := <-closes:
			t.Fatalf("the credential retired %s before its horizon with %d", horizon.Sub(retired.at), retired.code)
		default:
		}
		time.Sleep(sweep.sweep.Tick())
		synctest.Wait()
		var retired leaseClose
		select {
		case retired = <-closes:
		default:
			t.Fatal("the credential outlived its horizon by a sweep tick")
		}
		if retired.code != AuthorizationExpiredCloseCode {
			t.Fatalf("the horizon retired the transport with %d, want %d", retired.code, AuthorizationExpiredCloseCode)
		}
		sweep.requireFire(t, "the horizon retirement", horizon, retired.at)
		select {
		case <-lease.Testing_Done():
		default:
			t.Fatal("the retired lease did not end")
		}
		synctest.Wait()
		if count := sweep.ownerArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after the lease ended", count)
		}
	})
}

// Scheduled rechecks keep the 60 second cadence from the lease's start, each
// at most one sweep tick late and never early, with no drift across rounds.
// Closing the lease takes both of its timers out of the sweep.
func TestAuthorizationLeaseRechecksKeepTheirCadenceOnTheSweep(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		sweep := newLeaseTestSweep()
		defer sweep.close()
		// the lease's cadence falls 37 ms past the sweep's ticks
		time.Sleep(37 * time.Millisecond)
		start := time.Now()
		check := &scriptedLeaseCheck{}
		credential := scriptedLeaseCredential()
		lease, err := startAuthorizationLeaseWithSweep(context.Background(), credential, func(code int) {
			t.Errorf("a healthy lease retired its transport with %d", code)
		}, nil, check.check, sweep.sweep)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		// no horizon: only the recheck ticker
		if count := sweep.ownerArmedCount(); count != 1 {
			t.Fatalf("the lease armed %d sweep timers, want 1", count)
		}

		const rechecks = 30
		time.Sleep(rechecks*AuthorizationRecheckInterval + time.Second)
		synctest.Wait()
		calls := check.calls()
		if len(calls) != 1+rechecks {
			t.Fatalf("%d checks in %d intervals, want the admission and %d rechecks", len(calls), rechecks, rechecks)
		}
		for i, at := range calls[1:] {
			deadline := start.Add(time.Duration(i+1) * AuthorizationRecheckInterval)
			sweep.requireFire(t, "a scheduled recheck", deadline, at)
		}

		lease.Close()
		synctest.Wait()
		if count := sweep.ownerArmedCount(); count != 0 {
			t.Fatalf("%d timers armed after the lease closed", count)
		}
	})
}
