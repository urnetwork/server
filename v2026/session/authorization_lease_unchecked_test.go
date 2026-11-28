package session

// Decision 3 (2026-11-18) at the authorization lease: a revocation check Redis
// does not answer admits a transport unverified, and holds an admitted one.
// It never renews a lease as if verified, and never retires a transport. Each
// test runs the real lease goroutines in a synctest bubble.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// One scripted answer: an error, a verified acceptance, or an acceptance whose
// revocation check Redis did not answer.
type leaseCheckAnswer struct {
	err       error
	unchecked bool
}

var (
	leaseCheckVerified  = leaseCheckAnswer{}
	leaseCheckUnchecked = leaseCheckAnswer{unchecked: true}
)

type scriptedUncheckedLeaseCheck struct {
	stateLock sync.Mutex
	answers   []leaseCheckAnswer
	otherwise leaseCheckAnswer
	callTimes []time.Time
	paths     []string
}

func (self *scriptedUncheckedLeaseCheck) check(ctx context.Context, credential *ByJwt) error {
	self.stateLock.Lock()
	index := len(self.callTimes)
	self.callTimes = append(self.callTimes, time.Now())
	self.paths = append(self.paths, revocationCheckPathFromContext(ctx))
	answer := self.otherwise
	if index < len(self.answers) {
		answer = self.answers[index]
	}
	self.stateLock.Unlock()
	if answer.unchecked {
		noteUncheckedAcceptance(ctx, credential, revocationCheckPathFromContext(ctx))
		return nil
	}
	return answer.err
}

func (self *scriptedUncheckedLeaseCheck) calls() ([]time.Time, []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]time.Time(nil), self.callTimes...), append([]string(nil), self.paths...)
}

// The scripted credential's retry jitter stretches each delay by a quarter
// (scriptedLeaseCredential).
func scriptedRetryDelay(interval time.Duration) time.Duration {
	return interval + interval/4
}

// Root cause: admission required a check that answered, so a credential whose
// revocation record Redis could not reach was refused with 503 at connect.
// Now it is admitted unverified: the lease rechecks on the unavailable
// backoff, not a minute later, and the first answered check verifies it.
func TestAuthorizationLeaseAdmitsAnUncheckedCredentialUnverified(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		check := &scriptedUncheckedLeaseCheck{answers: []leaseCheckAnswer{leaseCheckUnchecked, leaseCheckVerified}, otherwise: leaseCheckVerified}
		accepted := testingUncheckedAcceptances(revocationCheckPathConnectH1, revocationCheckCredentialClient)
		closed := false
		start := time.Now()
		lease, err := startAuthorizationLeaseWithCheck(WithStateQuerySource(context.Background(), StateQueryConnectH1), scriptedLeaseCredential(), func(int) { closed = true }, nil, check.check)
		if err != nil || lease == nil {
			t.Fatalf("an unchecked credential was refused admission: %v", err)
		}
		defer lease.Close()
		if got := testingUncheckedAcceptances(revocationCheckPathConnectH1, revocationCheckCredentialClient) - accepted; got != 1 {
			t.Fatalf("counted %.0f unchecked admissions", got)
		}
		time.Sleep(scriptedRetryDelay(AuthorizationUnavailableRetryInterval))
		synctest.Wait()
		calls, paths := check.calls()
		if len(calls) != 2 || calls[1].Sub(start) != scriptedRetryDelay(AuthorizationUnavailableRetryInterval) {
			t.Fatalf("an unverified admission rechecked at %v, want once at %s", relativeTimes(start, calls), scriptedRetryDelay(AuthorizationUnavailableRetryInterval))
		}
		if paths[1] != revocationCheckPathLease {
			t.Errorf("the recheck counted on path %q, want %q", paths[1], revocationCheckPathLease)
		}
		// verified at the retry: no more retries, and the lease's own cadence
		// rechecks next
		time.Sleep(AuthorizationRecheckInterval - scriptedRetryDelay(AuthorizationUnavailableRetryInterval) - time.Second)
		synctest.Wait()
		if calls, _ := check.calls(); len(calls) != 2 {
			t.Fatalf("a verified lease retried again at %v", relativeTimes(start, calls))
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls, _ := check.calls(); len(calls) != 3 || calls[2].Sub(start) != AuthorizationRecheckInterval {
			t.Fatalf("a verified lease rechecked at %v, want next at its %s cadence", relativeTimes(start, calls), AuthorizationRecheckInterval)
		}
		if closed {
			t.Fatal("the transport was retired")
		}
	})
}

// An admitted transport's recheck that Redis does not answer is an unanswered
// recheck: the lease is held and rechecks on the backoff, never renewed as
// verified, and never retires, however long Redis stays away.
func TestAuthorizationLeaseUncheckedRecheckHoldsWithoutRenewing(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		check := &scriptedUncheckedLeaseCheck{answers: []leaseCheckAnswer{leaseCheckVerified}, otherwise: leaseCheckUnchecked}
		unanswered := testingLeaseUnavailableRechecks()
		accepted := testingUncheckedAcceptances(revocationCheckPathLease, revocationCheckCredentialClient)
		closed := false
		start := time.Now()
		lease, err := startAuthorizationLeaseWithCheck(context.Background(), scriptedLeaseCredential(), func(int) { closed = true }, nil, check.check)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		// a verified admission, then an hour of rechecks Redis does not answer
		time.Sleep(time.Hour)
		synctest.Wait()
		calls, _ := check.calls()
		if closed {
			t.Fatal("an unanswered recheck retired the transport")
		}
		// rechecks at 60s, then the backoff 6.25, 12.5, 25, 50 and 75s apart
		want := []time.Duration{0, AuthorizationRecheckInterval}
		next := AuthorizationRecheckInterval
		for interval := AuthorizationUnavailableRetryInterval; next+scriptedRetryDelay(interval) <= time.Hour; interval = min(2*interval, AuthorizationUnavailableRetryMaxInterval) {
			next += scriptedRetryDelay(interval)
			want = append(want, next)
		}
		if len(calls) != len(want) {
			t.Fatalf("rechecked at %v, want %v", relativeTimes(start, calls), want)
		}
		for i := range want {
			if calls[i].Sub(start) != want[i] {
				t.Fatalf("rechecked at %v, want %v", relativeTimes(start, calls), want)
			}
		}
		if got := testingLeaseUnavailableRechecks() - unanswered; int(got) != len(want)-1 {
			t.Errorf("counted %.0f unanswered rechecks, want %d", got, len(want)-1)
		}
		if got := testingUncheckedAcceptances(revocationCheckPathLease, revocationCheckCredentialClient) - accepted; int(got) != len(want)-1 {
			t.Errorf("counted %.0f unchecked lease acceptances, want %d", got, len(want)-1)
		}
		lease.stateLock.Lock()
		deadline := lease.state.deadline
		lease.stateLock.Unlock()
		if last := start.Add(want[len(want)-1]); !deadline.Equal(last.Add(AuthorizationLeaseDuration)) {
			t.Errorf("the held lease's deadline is %s after the last recheck, want %s", deadline.Sub(last), AuthorizationLeaseDuration)
		}
	})
}

// The connection authority's publication at admission is best effort within
// the check budget: a Redis that does not answer it neither refuses the
// transport nor holds its handshake, and the lease republishes at its first
// recheck, on the unavailable backoff. A caller that ended is still refused.
func TestAuthorizationLeaseAdmissionPublishIsBestEffort(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		check := &scriptedUncheckedLeaseCheck{otherwise: leaseCheckVerified}
		var publishLock sync.Mutex
		publishes := []time.Duration{}
		start := time.Now()
		publish := func(ctx context.Context, deadline time.Time) error {
			publishLock.Lock()
			publishes = append(publishes, time.Since(start))
			first := len(publishes) == 1
			publishLock.Unlock()
			if first {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}
		lease, err := startAuthorizationLeaseWithCheck(context.Background(), scriptedLeaseCredential(), func(int) {}, publish, check.check)
		if err != nil {
			t.Fatalf("an unpublished authority refused admission: %v", err)
		}
		defer lease.Close()
		if elapsed := time.Since(start); elapsed != RevocationCheckBudget {
			t.Errorf("the publication held admission for %s, want the %s budget", elapsed, RevocationCheckBudget)
		}
		time.Sleep(scriptedRetryDelay(AuthorizationUnavailableRetryInterval))
		synctest.Wait()
		publishLock.Lock()
		got := append([]time.Duration(nil), publishes...)
		publishLock.Unlock()
		if len(got) != 2 || got[1] != RevocationCheckBudget+scriptedRetryDelay(AuthorizationUnavailableRetryInterval) {
			t.Fatalf("published at %v, want again at the first recheck", got)
		}

		ended, cancel := context.WithCancel(context.Background())
		failing := func(ctx context.Context, deadline time.Time) error {
			cancel()
			return errors.New("synthetic publication failure")
		}
		if lease, err := startAuthorizationLeaseWithCheck(ended, scriptedLeaseCredential(), func(int) {}, failing, check.check); err == nil || lease != nil {
			t.Fatalf("an ended caller was admitted: %v", err)
		}
	})
}

func testingLeaseUnavailableRechecks() float64 {
	return testutil.ToFloat64(sessionLeaseUnavailableRechecks)
}
