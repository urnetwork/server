// Independent authorization timers retire transports even while a recheck stalls.
package session

import (
	"context"
	"errors"
	"github.com/urnetwork/server/v2026"
	"sync"
	"time"
)

const AuthorizationLeaseDuration = 90 * time.Second
const AuthorizationRecheckInterval = 60 * time.Second
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
	err := checkLeaseCredential(checkCtx, credential)
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
	lease := &AuthorizationLease{ctx: leaseCtx, cancel: cancel, state: authorizationLeaseState{deadline: deadline, generation: 1}, wake: make(chan struct{}, 1), terminal: terminal, credential: credential, closeTransport: closeTransport, publish: publish}
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
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-ticker.C:
		case <-self.wake:
		}
		start := time.Now()
		self.stateLock.Lock()
		generation, closed := self.state.generation, self.state.closed
		self.stateLock.Unlock()
		if closed {
			return
		}
		ctx, cancel := context.WithTimeout(self.ctx, 2*time.Second)
		err := checkLeaseCredential(ctx, self.credential)
		cancel()
		if err != nil {
			if errors.Is(err, ErrAuthUnavailable) || errors.Is(err, ErrSessionStoreUnavailable) {
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
