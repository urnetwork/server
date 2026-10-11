// Authorization leases of admitted transports. Only a definitive answer from
// the authorization store, or the credential's own acceptance horizon, retires
// an admitted transport. A check that produces no answer (the store is
// unavailable, a deadline or cancellation, an unexpected error) keeps the
// transport and its lease, and rechecks on a backoff until the store answers.
// This is the owner decision of 2026-10-11 (REVOKE-FINAL.md): during the
// 2026-10-10 database saturation, retiring on unavailability disconnected
// about 3 million healthy provider transports. Admission is unchanged: a new
// transport still needs a successful check.
package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

const AuthorizationLeaseDuration = 90 * time.Second
const AuthorizationRecheckInterval = 60 * time.Second

// A recheck that produces no answer is retried after this delay, doubling per
// consecutive unanswered recheck up to AuthorizationUnavailableRetryMaxInterval,
// the normal recheck cadence, so an outage does not add load beyond the usual
// rechecks while a recovered store is found within one cadence. While retries
// are pending they replace the scheduled ticks; session events still recheck
// at once. Each unanswered recheck keeps the lease alive for another
// AuthorizationLeaseDuration, capped by the credential's horizon.
const AuthorizationUnavailableRetryInterval = 5 * time.Second
const AuthorizationUnavailableRetryMaxInterval = AuthorizationRecheckInterval

// Each retry delay is stretched by up to half, by a fraction fixed per
// credential, so leases that failed together, such as every lease of a
// network rechecked by one session event, do not all retry in one instant.
// The fraction comes from the random tail of the credential's id.
func authorizationRetryJitter(credential *ByJwt) float64 {
	id := credential.NetworkId
	if credential.ClientId != nil {
		id = *credential.ClientId
	}
	return float64(id[len(id)-1]) / 256
}

// Retained in the close vocabulary. A lease no longer closes with it: an
// unavailable store keeps admitted transports.
const AuthorizationUnavailableCloseCode = 4002
const AuthorizationRevokedCloseCode = 4003
const AuthorizationExpiredCloseCode = 4004
const AuthorizationRejectedCloseCode = 4005

// The deadline is the published connection authority's deadline. It no longer
// retires the transport by itself; only closed does.
type authorizationLeaseState struct {
	deadline   time.Time
	closed     bool
	generation uint64
}

// A successful authoritative check started at start. A late result can never
// renew a closed or replaced generation; a credential past its horizon closes.
func (self *authorizationLeaseState) renew(start, now, terminal time.Time, generation uint64) bool {
	if self.closed || generation != self.generation {
		return false
	}
	if !terminal.IsZero() && !now.Before(terminal) {
		self.closed = true
		return false
	}
	self.extend(start.Add(AuthorizationLeaseDuration), terminal)
	return true
}

// A check that produced no answer at now keeps the lease alive without
// verifying it. A credential past its horizon is left to the horizon timer.
func (self *authorizationLeaseState) hold(now, terminal time.Time, generation uint64) bool {
	if self.closed || generation != self.generation {
		return false
	}
	if !terminal.IsZero() && !now.Before(terminal) {
		return false
	}
	self.extend(now.Add(AuthorizationLeaseDuration), terminal)
	return true
}

// Never shortens the deadline, except to the credential's horizon.
func (self *authorizationLeaseState) extend(deadline, terminal time.Time) {
	if self.deadline.Before(deadline) {
		self.deadline = deadline
	}
	if !terminal.IsZero() && terminal.Before(self.deadline) {
		self.deadline = terminal
	}
}

// Only the credential's acceptance horizon is a timed retirement.
func (self *authorizationLeaseState) expire(now, terminal time.Time) bool {
	if self.closed || terminal.IsZero() || now.Before(terminal) {
		return false
	}
	self.closed = true
	return true
}

// A definitive refusal: the store answered and the answer is no. Anything else,
// an unavailable store, a deadline, a cancellation, a recovered panic or an
// unexpected error, is not an answer and never retires an admitted transport.
func authorizationRejectionDefinitive(err error) bool {
	var rejection *byJwtRejection
	return errors.Is(err, ErrSessionRevoked) || errors.Is(err, ErrByJwtInactive) || errors.As(err, &rejection)
}

// Process-wide summary of unanswered rechecks, logged at most once per
// interval so an outage does not log one line per lease.
const authorizationUnansweredLogInterval = 30 * time.Second

var authorizationUnansweredLog struct {
	lastUnixNano          atomic.Int64
	count                 atomic.Int64
	maxUnverifiedUnixNano atomic.Int64
}

func noteAuthorizationUnanswered(credential *ByJwt, unverified time.Duration, err error) {
	sessionLeaseUnavailableRechecks.Inc()
	sessionLeaseUnverifiedSeconds.Observe(unverified.Seconds())
	if glog.V(1) {
		glog.Infof("[session]lease recheck unanswered network=%s unverified=%s err=%v\n", credential.NetworkId, unverified, err)
	}
	authorizationUnansweredLog.count.Add(1)
	for {
		longest := authorizationUnansweredLog.maxUnverifiedUnixNano.Load()
		if int64(unverified) <= longest || authorizationUnansweredLog.maxUnverifiedUnixNano.CompareAndSwap(longest, int64(unverified)) {
			break
		}
	}
	now := time.Now().UnixNano()
	last := authorizationUnansweredLog.lastUnixNano.Load()
	if now-last < int64(authorizationUnansweredLogInterval) || !authorizationUnansweredLog.lastUnixNano.CompareAndSwap(last, now) {
		return
	}
	count := authorizationUnansweredLog.count.Swap(0)
	longest := time.Duration(authorizationUnansweredLog.maxUnverifiedUnixNano.Swap(0))
	glog.Warningf("[session]%d lease rechecks unanswered since the last report, longest unverified %s; admitted transports are kept until a definitive answer\n", count, longest)
}

type AuthorizationLease struct {
	ctx            context.Context
	cancel         context.CancelFunc
	stateLock      sync.Mutex
	state          authorizationLeaseState
	wake           chan struct{}
	terminal       time.Time
	credential     *ByJwt
	closeTransport func(int)
	once           sync.Once
	publish        func(context.Context, time.Time) error
	// the authoritative credential check, checkLeaseCredential outside tests;
	// read under stateLock, since Testing_InterposeRecheck replaces it
	check func(context.Context, *ByJwt) error
	// start of the admission check, the first verification
	started time.Time
}

// Registration into the owning connection map must happen before calling this.
// The final check closes the earlier-auth/registration race before admission.
func StartAuthorizationLease(ctx context.Context, credential *ByJwt, closeTransport func(int)) (*AuthorizationLease, error) {
	return startAuthorizationLease(ctx, credential, closeTransport, nil)
}
func StartConnectionAuthorizationLease(ctx context.Context, credential *ByJwt, generation server.Id, version uint32, closeTransport func(int)) (*AuthorizationLease, error) {
	return startAuthorizationLease(ctx, credential, closeTransport, func(ctx context.Context, deadline time.Time) error {
		return PublishConnectionAuthority(ctx, credential, generation, version, deadline)
	})
}
func startAuthorizationLease(ctx context.Context, credential *ByJwt, closeTransport func(int), publish func(context.Context, time.Time) error) (*AuthorizationLease, error) {
	return startAuthorizationLeaseWithCheck(ctx, credential, closeTransport, publish, checkLeaseCredential)
}

// The check performs every authoritative credential read: the final check
// before admission and each recheck that can renew the lease.
func startAuthorizationLeaseWithCheck(ctx context.Context, credential *ByJwt, closeTransport func(int), publish func(context.Context, time.Time) error, check func(context.Context, *ByJwt) error) (*AuthorizationLease, error) {
	start := time.Now()
	terminal := credential.AcceptUntil()
	if credential.SessionId == nil && credential.RootClientId == nil && !rejectExpired() {
		terminal = time.Time{}
	} else if rejectExpired() && credential.ExpiresAt != nil {
		strict := credential.ExpiresAt.Time.Add(clockLeeway)
		if terminal.IsZero() || strict.Before(terminal) {
			terminal = strict
		}
	}
	checkCtx, cancelCheck := context.WithTimeout(ctx, 2*time.Second)
	err := check(checkCtx, credential)
	cancelCheck()
	if err != nil {
		return nil, err
	}
	deadline := start.Add(AuthorizationLeaseDuration)
	if !terminal.IsZero() && terminal.Before(deadline) {
		deadline = terminal
	}
	if !time.Now().Before(deadline) {
		return nil, errors.New("credential expired")
	}
	if publish != nil {
		if err := publish(ctx, deadline); err != nil {
			return nil, ErrAuthUnavailable
		}
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	lease := &AuthorizationLease{ctx: leaseCtx, cancel: cancel, state: authorizationLeaseState{deadline: deadline, generation: 1}, wake: make(chan struct{}, 1), terminal: terminal, credential: credential, closeTransport: closeTransport, publish: publish, check: check, started: start}
	go lease.runTimer()
	go lease.runChecks()
	return lease, nil
}
func checkLeaseCredential(ctx context.Context, credential *ByJwt) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrAuthUnavailable
		}
	}()
	return ValidateByJwtState(ctx, credential, true)
}
func (self *AuthorizationLease) Close() {
	self.cancel()
	self.stateLock.Lock()
	self.state.closed = true
	self.state.generation++
	self.stateLock.Unlock()
}
func (self *AuthorizationLease) Recheck() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}
func (self *AuthorizationLease) closeWithCause(code int) {
	self.once.Do(func() {
		cause := "unavailable"
		switch code {
		case AuthorizationRevokedCloseCode:
			cause = "revoked"
		case AuthorizationExpiredCloseCode:
			cause = "expired"
		case AuthorizationRejectedCloseCode:
			cause = "invalid"
		}
		sessionLeaseRetirements.WithLabelValues(cause).Inc()
		self.Close()
		self.closeTransport(code)
	})
}

// Retires the transport at the credential's acceptance horizon, a definitive
// expiry that needs no store. Without a horizon only a definitive check retires.
func (self *AuthorizationLease) runTimer() {
	if self.terminal.IsZero() {
		<-self.ctx.Done()
		return
	}
	timer := time.NewTimer(max(0, time.Until(self.terminal)))
	defer timer.Stop()
	select {
	case <-self.ctx.Done():
		return
	case <-timer.C:
	}
	self.stateLock.Lock()
	expired := self.state.expire(time.Now(), self.terminal)
	self.stateLock.Unlock()
	if expired {
		self.closeWithCause(AuthorizationExpiredCloseCode)
	}
}
func (self *AuthorizationLease) runChecks() {
	ticker := time.NewTicker(AuthorizationRecheckInterval)
	defer ticker.Stop()
	// armed only after an unanswered recheck; a nil channel never fires
	var retry <-chan time.Time
	retryInterval := AuthorizationUnavailableRetryInterval
	verified := self.started
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-ticker.C:
			if retry != nil {
				// the backoff paces rechecks while the store does not answer
				continue
			}
		case <-self.wake:
		case <-retry:
		}
		retry = nil
		start := time.Now()
		self.stateLock.Lock()
		generation, closed, check := self.state.generation, self.state.closed, self.check
		self.stateLock.Unlock()
		if closed {
			return
		}
		ctx, cancel := context.WithTimeout(self.ctx, 2*time.Second)
		err := check(ctx, self.credential)
		cancel()
		if err != nil {
			if !authorizationRejectionDefinitive(err) {
				noteAuthorizationUnanswered(self.credential, start.Sub(verified), err)
				self.stateLock.Lock()
				held := self.state.hold(time.Now(), self.terminal, generation)
				deadline := self.state.deadline
				self.stateLock.Unlock()
				if held && self.publish != nil {
					// best effort: dependent stream grants keep the endpoint
					// while only the database is unavailable
					ctx, cancel := context.WithTimeout(self.ctx, 2*time.Second)
					_ = self.publish(ctx, deadline)
					cancel()
				}
				retry = time.After(retryInterval + time.Duration(float64(retryInterval)*authorizationRetryJitter(self.credential)/2))
				retryInterval = min(2*retryInterval, AuthorizationUnavailableRetryMaxInterval)
				continue
			}
			cause := AuthorizationRejectedCloseCode
			if errors.Is(err, ErrSessionRevoked) {
				cause = AuthorizationRevokedCloseCode
			} else if !self.terminal.IsZero() && !time.Now().Before(self.terminal) {
				cause = AuthorizationExpiredCloseCode
			}
			self.closeWithCause(cause)
			return
		}
		retryInterval = AuthorizationUnavailableRetryInterval
		verified = start
		self.stateLock.Lock()
		renewed := self.state.renew(start, time.Now(), self.terminal, generation)
		deadline, closed := self.state.deadline, self.state.closed
		self.stateLock.Unlock()
		if renewed && self.publish != nil {
			ctx, cancel := context.WithTimeout(self.ctx, 2*time.Second)
			_ = self.publish(ctx, deadline)
			cancel()
		}
		if !renewed {
			if closed && !self.terminal.IsZero() && !time.Now().Before(self.terminal) {
				// the credential reached its horizon during the check
				self.closeWithCause(AuthorizationExpiredCloseCode)
			}
			return
		}
	}
}

// Routes every later recheck through interpose(check), so a test outside the
// package can observe each authoritative result or stand in for a store that
// does not answer. The admission check has already run. A recheck already in
// flight keeps the check it started with.
func (self *AuthorizationLease) Testing_InterposeRecheck(interpose func(check func(context.Context, *ByJwt) error) func(context.Context, *ByJwt) error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.check = interpose(self.check)
}

// Closes when the lease ends: when it retires its transport, or when the
// transport's own close closes it.
func (self *AuthorizationLease) Testing_Done() <-chan struct{} {
	return self.ctx.Done()
}

// Testing_Expire delivers the horizon timer's retirement at an explicit clock,
// without waiting for the credential's horizon. Before the horizon it is a
// no-op: an elapsed renewal deadline alone retires nothing.
func (self *AuthorizationLease) Testing_Expire(now time.Time) {
	self.stateLock.Lock()
	expired := self.state.expire(now, self.terminal)
	self.stateLock.Unlock()
	if expired {
		self.closeWithCause(AuthorizationExpiredCloseCode)
	}
}
