package session

// Only a definitive answer retires an admitted transport (owner decision,
// 2026-10-11). The scripted check from authorization_lease_retry_test.go drives
// the real lease goroutines in a synctest bubble; fixed synthetic ids stretch
// every retry delay by a quarter.

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

// Main 2026-10-10/11: with the primary saturated, unavailable rechecks retired
// about 3 million healthy provider transports. A store that stays unavailable
// for ten minutes must close nothing, and the rechecks must back off to the
// normal cadence instead of running every tick.
func TestAuthorizationLeaseOutageNeverRetiresAdmittedTransport(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		check := &scriptedLeaseCheck{
			results:   []error{nil},
			otherwise: ErrAuthUnavailable,
		}
		lease, closes := startScriptedLease(t, check)
		defer lease.Close()

		time.Sleep(10 * time.Minute)
		synctest.Wait()

		select {
		case retired := <-closes:
			t.Fatalf("an unavailable store retired the transport: code=%d at=%s calls=%v", retired.code, retired.at.Sub(start), relativeTimes(start, check.calls()))
		default:
		}
		// the scheduled recheck at 60s, then 5, 10, 20, 40 and 60 second
		// retries capped at the normal cadence, each stretched by a quarter
		calls := relativeTimes(start, check.calls())
		want := []time.Duration{0, 60 * time.Second, 66250 * time.Millisecond, 78750 * time.Millisecond, 103750 * time.Millisecond,
			153750 * time.Millisecond, 228750 * time.Millisecond, 303750 * time.Millisecond, 378750 * time.Millisecond,
			453750 * time.Millisecond, 528750 * time.Millisecond}
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

// A revocation that lands during the outage retires the transport with the
// revoked cause at the first recheck the store answers.
func TestAuthorizationLeaseRevocationAfterOutageRetiresPromptly(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		check := &scriptedLeaseCheck{
			// admission, then six unavailable rechecks through 228.75s
			results:   []error{nil, ErrAuthUnavailable, ErrAuthUnavailable, ErrAuthUnavailable, ErrAuthUnavailable, ErrAuthUnavailable, ErrAuthUnavailable},
			otherwise: ErrSessionRevoked,
		}
		lease, closes := startScriptedLease(t, check)
		defer lease.Close()

		time.Sleep(10 * time.Minute)
		synctest.Wait()

		var retired leaseClose
		select {
		case retired = <-closes:
		default:
			t.Fatalf("revocation never retired the transport: calls=%v", relativeTimes(start, check.calls()))
		}
		if retired.code != AuthorizationRevokedCloseCode {
			t.Fatalf("close code=%d at %s, want %d", retired.code, retired.at.Sub(start), AuthorizationRevokedCloseCode)
		}
		// the next capped retry after the last unavailable recheck at 228.75s
		if at := retired.at.Sub(start); at != 303750*time.Millisecond {
			t.Fatalf("retired at %s, want 5m3.75s, the first recheck the store answered", at)
		}
	})
}

// A revocation found while the store is healthy retires the transport at the
// scheduled recheck, as before.
func TestAuthorizationLeaseHealthyRevocationRetiresAtRecheck(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		check := &scriptedLeaseCheck{
			results:   []error{nil},
			otherwise: ErrSessionRevoked,
		}
		lease, closes := startScriptedLease(t, check)
		defer lease.Close()

		time.Sleep(2 * time.Minute)
		synctest.Wait()

		var retired leaseClose
		select {
		case retired = <-closes:
		default:
			t.Fatalf("healthy revocation never retired the transport: calls=%v", relativeTimes(start, check.calls()))
		}
		if retired.code != AuthorizationRevokedCloseCode || retired.at.Sub(start) != AuthorizationRecheckInterval {
			t.Fatalf("close code=%d at %s, want %d at %s", retired.code, retired.at.Sub(start), AuthorizationRevokedCloseCode, AuthorizationRecheckInterval)
		}
	})
}

// A new transport still needs a successful check: an unavailable store refuses
// admission and starts no lease.
func TestAuthorizationLeaseAdmissionRefusedWhileStoreUnavailable(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		check := &scriptedLeaseCheck{otherwise: ErrAuthUnavailable}
		closed := false
		lease, err := startAuthorizationLeaseWithCheck(context.Background(), scriptedLeaseCredential(), func(int) { closed = true }, nil, check.check)
		if err == nil || lease != nil {
			t.Fatalf("admitted during an outage: lease=%v err=%v", lease, err)
		}
		synctest.Wait()
		if closed || len(check.calls()) != 1 {
			t.Fatalf("refused admission closed=%t after %d checks", closed, len(check.calls()))
		}
	})
}

// A deadline, cancellation or unexpected error is not an answer either. Before
// this rule any error other than the two unavailable sentinels was read as a
// rejection and retired the transport with 4005.
func TestAuthorizationLeaseUnclassifiedErrorKeepsTransport(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		check := &scriptedLeaseCheck{
			results:   []error{nil, context.DeadlineExceeded, context.Canceled},
			otherwise: nil,
		}
		lease, closes := startScriptedLease(t, check)
		defer lease.Close()

		time.Sleep(5 * time.Minute)
		synctest.Wait()

		select {
		case retired := <-closes:
			t.Fatalf("an unanswered check retired the transport: code=%d at=%s calls=%v", retired.code, retired.at.Sub(start), relativeTimes(start, check.calls()))
		default:
		}
	})
}

// The credential's own horizon is definitive: it retires the transport as
// expired at that instant even while the store is unavailable.
func TestAuthorizationLeaseCredentialHorizonRetiresDuringOutage(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		credential := scriptedLeaseCredential()
		sessionId := credential.NetworkId
		credential.SessionId = &sessionId
		// accept until start + 3 minutes
		credential.ExpiresAt = gojwt.NewNumericDate(start.Add(3*time.Minute - SessionGrace - clockLeeway))
		check := &scriptedLeaseCheck{
			results:   []error{nil},
			otherwise: ErrAuthUnavailable,
		}
		closes := make(chan leaseClose, 1)
		lease, err := startAuthorizationLeaseWithCheck(context.Background(), credential, func(code int) {
			closes <- leaseClose{code: code, at: time.Now()}
		}, nil, check.check)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()

		time.Sleep(10 * time.Minute)
		synctest.Wait()

		var retired leaseClose
		select {
		case retired = <-closes:
		default:
			t.Fatalf("expired credential never retired: calls=%v", relativeTimes(start, check.calls()))
		}
		if retired.code != AuthorizationExpiredCloseCode || retired.at.Sub(start) != 3*time.Minute {
			t.Fatalf("close code=%d at %s, want %d at 3m0s", retired.code, retired.at.Sub(start), AuthorizationExpiredCloseCode)
		}
	})
}
