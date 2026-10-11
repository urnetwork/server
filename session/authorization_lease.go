// Independent authorization timers retire transports even while a recheck stalls.
package session

import (
	"context"
	"errors"
	"github.com/urnetwork/server"
	"sync"
	"time"
)

const AuthorizationLeaseDuration = 90 * time.Second
const AuthorizationRecheckInterval = 60 * time.Second

// An authoritative check that finds the authorization store unavailable is
// retried after this delay, doubling per consecutive unavailable result up to
// AuthorizationUnavailableRetryMaxInterval. The next scheduled recheck is 60
// seconds away while the lease has at most 30 seconds left, so without a
// retry one slow read lets a valid transport lapse. A retry never renews by
// itself: only a successful check does, and the independent timer still
// retires the transport at its deadline when the store stays unavailable.
const AuthorizationUnavailableRetryInterval = 5 * time.Second
const AuthorizationUnavailableRetryMaxInterval = 20 * time.Second

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

const AuthorizationUnavailableCloseCode = 4002
const AuthorizationRevokedCloseCode = 4003
const AuthorizationExpiredCloseCode = 4004
const AuthorizationRejectedCloseCode = 4005

type authorizationLeaseState struct {
	deadline   time.Time
	closed     bool
	generation uint64
}

func (self *authorizationLeaseState) renew(start, now, terminal time.Time, generation uint64) bool {
	if self.closed || generation != self.generation || !now.Before(self.deadline) {
		return false
	}
	deadline := start.Add(AuthorizationLeaseDuration)
	if !terminal.IsZero() && terminal.Before(deadline) {
		deadline = terminal
	}
	if !now.Before(deadline) {
		self.closed = true
		return false
	}
	self.deadline = deadline
	return true
}
func (self *authorizationLeaseState) expire(now time.Time) bool {
	if self.closed || now.Before(self.deadline) {
		return false
	}
	self.closed = true
	return true
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
	// the authoritative credential check, checkLeaseCredential outside tests
	check func(context.Context, *ByJwt) error
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
	lease := &AuthorizationLease{ctx: leaseCtx, cancel: cancel, state: authorizationLeaseState{deadline: deadline, generation: 1}, wake: make(chan struct{}, 1), terminal: terminal, credential: credential, closeTransport: closeTransport, publish: publish, check: check}
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
func (self *AuthorizationLease) runTimer() {
	for {
		self.stateLock.Lock()
		deadline, closed := self.state.deadline, self.state.closed
		self.stateLock.Unlock()
		if closed {
			return
		}
		timer := time.NewTimer(max(0, time.Until(deadline)))
		select {
		case <-self.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		self.stateLock.Lock()
		expired := self.state.expire(time.Now())
		self.stateLock.Unlock()
		if expired {
			code := AuthorizationUnavailableCloseCode
			if !self.terminal.IsZero() && !time.Now().Before(self.terminal) {
				code = AuthorizationExpiredCloseCode
			}
			self.closeWithCause(code)
			return
		}
	}
}
func (self *AuthorizationLease) runChecks() {
	ticker := time.NewTicker(AuthorizationRecheckInterval)
	defer ticker.Stop()
	// armed only after an unavailable result; a nil channel never fires
	var retry <-chan time.Time
	retryInterval := AuthorizationUnavailableRetryInterval
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-ticker.C:
		case <-self.wake:
		case <-retry:
		}
		retry = nil
		start := time.Now()
		self.stateLock.Lock()
		generation, closed := self.state.generation, self.state.closed
		self.stateLock.Unlock()
		if closed {
			return
		}
		ctx, cancel := context.WithTimeout(self.ctx, 2*time.Second)
		err := self.check(ctx, self.credential)
		cancel()
		if err != nil {
			if errors.Is(err, ErrAuthUnavailable) || errors.Is(err, ErrSessionStoreUnavailable) {
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
		self.stateLock.Lock()
		renewed := self.state.renew(start, time.Now(), self.terminal, generation)
		deadline := self.state.deadline
		self.stateLock.Unlock()
		if renewed && self.publish != nil {
			ctx, cancel := context.WithTimeout(self.ctx, 2*time.Second)
			_ = self.publish(ctx, deadline)
			cancel()
		}
		if !renewed {
			self.closeWithCause(AuthorizationUnavailableCloseCode)
			return
		}
	}
}

// Testing_Expire delivers the same independent retirement transition after an
// explicit clock advance, without waiting through a ninety-second test sleep.
func (self *AuthorizationLease) Testing_Expire(now time.Time) {
	self.stateLock.Lock()
	expired := self.state.expire(now)
	self.stateLock.Unlock()
	if expired {
		cause := AuthorizationUnavailableCloseCode
		if !self.terminal.IsZero() && !now.Before(self.terminal) {
			cause = AuthorizationExpiredCloseCode
		}
		self.closeWithCause(cause)
	}
}
