package session

// Recheck scheduling after an unavailable authorization store. Each test runs
// the real lease goroutines in a synctest bubble, so the 60 second ticker, the
// 90 second lease timer and every retry delay advance on the bubble's virtual
// clock, and the injected check decides each authoritative result.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// The injected authoritative check answers each call from a fixed script and
// records the virtual time of every call.
type scriptedLeaseCheck struct {
	stateLock sync.Mutex
	results   []error
	// the result for every call past the end of results
	otherwise error
	callTimes []time.Time
}

func (self *scriptedLeaseCheck) check(ctx context.Context, credential *ByJwt) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	index := len(self.callTimes)
	self.callTimes = append(self.callTimes, time.Now())
	if index < len(self.results) {
		return self.results[index]
	}
	return self.otherwise
}

func (self *scriptedLeaseCheck) calls() []time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]time.Time(nil), self.callTimes...)
}

// One retirement of the transport, with its virtual time.
type leaseClose struct {
	code int
	at   time.Time
}

// Fixed synthetic ids keep the retry jitter, and so every virtual time, the
// same on every run. The client id's tail 0x80 stretches each delay by a
// quarter: retries at 66.25s and 78.75s after the first failure at 60s.
func scriptedLeaseCredential() *ByJwt {
	clientId := server.Id{0: 0x01, 15: 0x80}
	return &ByJwt{NetworkId: server.Id{0: 0x02, 15: 0x11}, UserId: server.Id{0: 0x03}, ClientId: &clientId}
}

func startScriptedLease(t *testing.T, check *scriptedLeaseCheck) (*AuthorizationLease, chan leaseClose) {
	credential := scriptedLeaseCredential()
	closes := make(chan leaseClose, 1)
	lease, err := startAuthorizationLeaseWithCheck(context.Background(), credential, func(code int) {
		closes <- leaseClose{code: code, at: time.Now()}
	}, nil, check.check)
	if err != nil {
		t.Fatal(err)
	}
	return lease, closes
}

// Main 2026-10-10: the authoritative recheck reads PostgreSQL with a two
// second budget. With the lease at 90 seconds and the next scheduled recheck
// 60 seconds later, one slow read let the lease lapse and closed a healthy
// provider transport. An unavailable result must be retried before the
// deadline, and the retry's success must renew the lease.
func TestAuthorizationLeaseRetriesUnavailableRecheckBeforeDeadline(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		check := &scriptedLeaseCheck{
			// admission, then the first scheduled recheck meets an unavailable store
			results: []error{nil, ErrAuthUnavailable},
		}
		lease, closes := startScriptedLease(t, check)
		defer lease.Close()

		// past the original deadline and two further recheck intervals
		time.Sleep(4 * time.Minute)
		synctest.Wait()

		select {
		case retired := <-closes:
			t.Fatalf("one unavailable recheck retired the transport: code=%d at=%s calls=%v", retired.code, retired.at.Sub(start), relativeTimes(start, check.calls()))
		default:
		}
		calls := relativeTimes(start, check.calls())
		if len(calls) < 3 || calls[2] >= AuthorizationLeaseDuration {
			t.Fatalf("no recheck before the lease deadline: calls=%v", calls)
		}
	})
}

// Fail closed is unchanged: retries never renew, so a sustained outage still
// retires the transport with the unavailable cause at the original deadline,
// and the retries before it stay bounded.
func TestAuthorizationLeaseSustainedUnavailabilityRetiresAtDeadline(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		check := &scriptedLeaseCheck{
			results:   []error{nil},
			otherwise: ErrAuthUnavailable,
		}
		lease, closes := startScriptedLease(t, check)
		defer lease.Close()

		time.Sleep(4 * time.Minute)
		synctest.Wait()

		var retired leaseClose
		select {
		case retired = <-closes:
		default:
			t.Fatalf("sustained unavailability never retired the transport: calls=%v", relativeTimes(start, check.calls()))
		}
		if retired.code != AuthorizationUnavailableCloseCode {
			t.Fatalf("close code=%d, want %d", retired.code, AuthorizationUnavailableCloseCode)
		}
		if elapsed := retired.at.Sub(start); elapsed != AuthorizationLeaseDuration {
			t.Fatalf("retired after %s, want the original deadline %s", elapsed, AuthorizationLeaseDuration)
		}
		// admission, the scheduled recheck, then two backed-off retries inside
		// the lease window: 5s and 10s, each stretched by the fixture's quarter
		calls := relativeTimes(start, check.calls())
		want := []time.Duration{0, 60 * time.Second, 66250 * time.Millisecond, 78750 * time.Millisecond}
		if len(calls) != len(want) {
			t.Fatalf("calls=%v, want %v", calls, want)
		}
		for i := range want {
			if calls[i] != want[i] {
				t.Fatalf("calls=%v, want %v", calls, want)
			}
		}
	})
}

// A definitive rejection found by the retry retires the transport at once,
// with the rejected cause rather than a later unavailable lapse.
func TestAuthorizationLeaseRetryRejectionRetiresImmediately(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		check := &scriptedLeaseCheck{
			results:   []error{nil, ErrAuthUnavailable},
			otherwise: ErrByJwtInactive,
		}
		lease, closes := startScriptedLease(t, check)
		defer lease.Close()

		time.Sleep(4 * time.Minute)
		synctest.Wait()

		var retired leaseClose
		select {
		case retired = <-closes:
		default:
			t.Fatalf("rejected credential never retired: calls=%v", relativeTimes(start, check.calls()))
		}
		if retired.code != AuthorizationRejectedCloseCode {
			t.Fatalf("close code=%d at %s, want %d before the deadline", retired.code, retired.at.Sub(start), AuthorizationRejectedCloseCode)
		}
		if elapsed := retired.at.Sub(start); AuthorizationLeaseDuration <= elapsed {
			t.Fatalf("rejection waited for the lease deadline: retired after %s", elapsed)
		}
	})
}

func relativeTimes(start time.Time, times []time.Time) []time.Duration {
	durations := make([]time.Duration, 0, len(times))
	for _, at := range times {
		durations = append(durations, at.Sub(start))
	}
	return durations
}

// The fixture's rejection must stay a definitive result, not unavailability.
func TestScriptedLeaseCheckRejectionIsNotUnavailable(t *testing.T) {
	if errors.Is(ErrByJwtInactive, ErrAuthUnavailable) || errors.Is(ErrByJwtInactive, ErrSessionStoreUnavailable) {
		t.Fatal("fixture rejection is classified as unavailable")
	}
}
