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

// A session event rechecks at once even while a retry is pending. The connect
// revocation tests rely on this, through Testing_InterposeRecheck, to find a
// revocation with rechecks they drive: after a recheck the store does not
// answer within the check deadline, the next driven recheck finds the
// revocation at once rather than at the retry 6.25 seconds later.
func TestAuthorizationLeaseDrivenRecheckPreemptsPendingRetry(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		check := &scriptedLeaseCheck{
			results:   []error{nil},
			otherwise: ErrSessionRevoked,
		}
		lease, closes := startScriptedLease(t, check)
		defer lease.Close()
		held := true
		lease.Testing_InterposeRecheck(func(next func(context.Context, *ByJwt) error) func(context.Context, *ByJwt) error {
			return func(ctx context.Context, credential *ByJwt) error {
				if held {
					// the store does not answer within the check deadline
					held = false
					<-ctx.Done()
					return errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
				}
				return next(ctx, credential)
			}
		})

		lease.Recheck()
		// past the 2 second check deadline; the retry is pending at 8.25s
		time.Sleep(3 * time.Second)
		synctest.Wait()
		select {
		case retired := <-closes:
			t.Fatalf("an unanswered recheck retired the transport: code=%d at=%s", retired.code, retired.at.Sub(start))
		case <-lease.Testing_Done():
			t.Fatal("an unanswered recheck ended the lease")
		default:
		}

		lease.Recheck()
		synctest.Wait()
		var retired leaseClose
		select {
		case retired = <-closes:
		default:
			t.Fatalf("the driven recheck did not retire the transport: calls=%v", relativeTimes(start, check.calls()))
		}
		select {
		case <-lease.Testing_Done():
		default:
			t.Fatal("the retired lease did not end")
		}
		if retired.code != AuthorizationRevokedCloseCode || retired.at.Sub(start) != 3*time.Second {
			t.Fatalf("close code=%d at %s, want %d at 3s", retired.code, retired.at.Sub(start), AuthorizationRevokedCloseCode)
		}
		// the admission check and the driven recheck; the held recheck never reached the store
		if calls := relativeTimes(start, check.calls()); len(calls) != 2 || calls[0] != 0 || calls[1] != 3*time.Second {
			t.Fatalf("calls=%v, want [0s 3s]", calls)
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

// The fixture's rejection must stay a definitive result, not unavailability,
// and the unavailable sentinels must never read as an answer.
func TestScriptedLeaseCheckRejectionIsNotUnavailable(t *testing.T) {
	if errors.Is(ErrByJwtInactive, ErrAuthUnavailable) || errors.Is(ErrByJwtInactive, ErrSessionStoreUnavailable) {
		t.Fatal("fixture rejection is classified as unavailable")
	}
	for _, err := range []error{ErrByJwtInactive, ErrSessionRevoked, rejectByJwt(AuthRejectionExpired, "synthetic"), rejectByJwtClaims(AuthRejectionMissingClaims, "synthetic")} {
		if !authorizationRejectionDefinitive(err) {
			t.Fatalf("%v is not definitive", err)
		}
	}
	for _, err := range []error{ErrAuthUnavailable, ErrSessionStoreUnavailable, errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)} {
		if authorizationRejectionDefinitive(err) {
			t.Fatalf("%v is definitive", err)
		}
	}
}
