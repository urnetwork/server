// Pending authenticated registrations share only a finalized authority read
// that starts after their membership closes. No completed boundary is cached,
// and each caller retains its original Sql/signing/publication result. Ready
// local publications share only a finite quota window after Sql has returned.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urfoundation/sn/v2026/stabi"
	"github.com/urnetwork/server/v2026"
)

const (
	maxStClientKeyRegistrationSlots   = 1024
	stClientKeyRegistrationCollection = time.Second
)

// All fields describe the concrete operator authority, not a caller's claim.
// Endpoint order is part of the owner; failover may not splice domains.
type stClientKeyRegistrationAuthority struct {
	client         *CoreStClient
	domain         protocol.ClientKeyHistoryDomain
	deploymentId   string
	rootSigner     common.Address
	artifactSigner common.Address
	rpcUrls        []string
}

// Concurrent-safe state is finite per actual CoreStClient. At most one read
// and one collecting cohort exist. Slots include callers still in Sql or blob
// publication, so finishing the shared read cannot open an unbounded backlog.
type stClientKeyRegistrationCohorts struct {
	stateLock          sync.Mutex
	collection         time.Duration
	slots              int
	pending            *stClientKeyRegistrationCohort
	active             *stClientKeyRegistrationCohort
	publications       []*stClientKeyRegistrationPublication
	publicationRunning bool
	// Private timing/observation seams never provide authority or write results.
	beforeSealForTest                func(context.Context)
	afterAdmissionForTest            func()
	beforePublicationForTest         func(context.Context)
	afterPublicationAdmissionForTest func()
	afterPublicationForTest          func()
	afterPublicationWindowForTest    func(*server.LocalBlobWriteBatch, int)
}

// One constructor-owned worker ends after its read, cancellation or refusal.
// Its result is immutable after done closes and is reachable only by members
// admitted before sealing, never by a later registration.
type stClientKeyRegistrationCohort struct {
	parent      *stClientKeyRegistrationCohorts
	owner       *stClientKeyAuthorityOwner
	authority   stClientKeyRegistrationAuthority
	ctx         context.Context
	cancel      context.CancelFunc
	predecessor <-chan struct{}
	done        chan struct{}
	waiters     int
	sealed      bool
	closing     bool
	finished    bool
	boundary    protocol.ClientKeyEffectiveBoundary
	operator    stabi.STCoordinatorOperatorVersion
	err         error
}

// A member owns exactly one slot until its complete registration returns.
// Its waiting interest can end earlier without canceling surviving members.
type stClientKeyRegistrationReservation struct {
	cohort             *stClientKeyRegistrationCohort
	waiting            bool
	released           bool
	publicationClaimed bool
}

// The zero-delay value is used only by serial test fixtures. Actual dispatch
// always selects the fixed one-second collection, never a caller input.
func newStClientKeyRegistrationCohorts(collection time.Duration) *stClientKeyRegistrationCohorts {
	return &stClientKeyRegistrationCohorts{collection: collection}
}

// The production singleton Core is reused by both authenticated dispatches.
// This lazy state has no immortal worker or retained latest-head observation.
func (self *CoreStClient) registrationCohorts() *stClientKeyRegistrationCohorts {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.clientKeyRegistrations == nil {
		self.clientKeyRegistrations = newStClientKeyRegistrationCohorts(stClientKeyRegistrationCollection)
	}
	return self.clientKeyRegistrations
}

// Pure comparisons run under the owner lock; no external call is made there.
func (self stClientKeyRegistrationAuthority) matches(other stClientKeyRegistrationAuthority) bool {
	return self.client == other.client && self.domain == other.domain && self.deploymentId == other.deploymentId && self.rootSigner == other.rootSigner && self.artifactSigner == other.artifactSigner && slices.Equal(self.rpcUrls, other.rpcUrls)
}

// Admission is zero-wait and precedes any authority Rpc or durable mutation.
// A refusal is an error, not a durable registration or a retry promise.
func (self *stClientKeyRegistrationCohorts) admit(ctx context.Context, owner *stClientKeyAuthorityOwner) (*stClientKeyRegistrationReservation, error) {
	if self == nil || ctx == nil || owner == nil || owner.client == nil || owner.rootKey == nil || owner.artifactKey == nil {
		return nil, errors.New("client-key registration cohort owner is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	authority := stClientKeyRegistrationAuthority{client: owner.client, domain: owner.domain, deploymentId: owner.deploymentID, rootSigner: crypto.PubkeyToAddress(owner.rootKey.PublicKey), artifactSigner: crypto.PubkeyToAddress(owner.artifactKey.PublicKey), rpcUrls: slices.Clone(owner.rpcURLs)}
	var reservation *stClientKeyRegistrationReservation
	var admitted func()
	err := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.collection < 0 || self.collection > stClientKeyRegistrationCollection || self.slots >= maxStClientKeyRegistrationSlots {
			return errors.New("client-key registration finite cohort capacity is unavailable")
		}
		for _, cohort := range []*stClientKeyRegistrationCohort{self.active, self.pending} {
			if cohort != nil && !cohort.authority.matches(authority) {
				return errors.New("client-key registration cannot share another operator authority owner")
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if self.pending == nil {
			// No member's cancellation may impersonate another member's lifetime.
			// The shared read still has the original finite operation ceiling.
			readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stCallTimeout)
			cohort := &stClientKeyRegistrationCohort{parent: self, owner: owner, authority: authority, ctx: readCtx, cancel: cancel, done: make(chan struct{})}
			if self.active != nil {
				cohort.predecessor = self.active.done
			}
			self.pending = cohort
			go cohort.run()
		}
		if self.pending.closing {
			return errors.New("client-key registration cohort is closing")
		}
		if err := self.pending.ctx.Err(); err != nil {
			return errors.Join(errors.New("client-key registration cohort is closing"), err)
		}
		self.slots++
		self.pending.waiters++
		reservation = &stClientKeyRegistrationReservation{cohort: self.pending, waiting: true}
		admitted = self.afterAdmissionForTest
		return nil
	}()
	if err != nil {
		return nil, err
	}
	if admitted != nil {
		admitted()
	}
	return reservation, nil
}

// Collection and a preceding read may overlap, but authority reads cannot.
// Sealing occurs before even the cold connection's chain identity request.
func (self *stClientKeyRegistrationCohort) run() {
	var boundary protocol.ClientKeyEffectiveBoundary
	var operator stabi.STCoordinatorOperatorVersion
	var resultErr error
	defer func() {
		if recovered := recover(); recovered != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("client-key registration authority read panicked: %v", recovered))
		}
		resultErr = errors.Join(resultErr, self.ctx.Err())
		self.cancel()
		self.parent.stateLock.Lock()
		defer self.parent.stateLock.Unlock()
		if self.parent.pending == self {
			self.parent.pending = nil
		}
		if self.parent.active == self {
			self.parent.active = nil
		}
		self.finished, self.err = true, resultErr
		if resultErr == nil {
			self.boundary, self.operator = boundary, operator
		}
		self.owner = nil
		close(self.done)
	}()
	select {
	case <-self.ctx.Done():
		return
	case <-time.After(self.parent.collection):
	}
	if self.predecessor != nil {
		select {
		case <-self.ctx.Done():
			return
		case <-self.predecessor:
		}
	}
	if self.parent.beforeSealForTest != nil {
		self.parent.beforeSealForTest(self.ctx)
	}
	resultErr = func() error {
		self.parent.stateLock.Lock()
		defer self.parent.stateLock.Unlock()
		if err := self.ctx.Err(); err != nil {
			return err
		}
		if self.parent.pending != self || self.parent.active != nil || self.waiters == 0 {
			return errors.New("client-key registration cohort lost its exact pending membership")
		}
		self.parent.pending, self.parent.active = nil, self
		self.sealed = true
		return nil
	}()
	if resultErr != nil {
		return
	}
	boundary, operator, resultErr = self.owner.readBoundary(self.ctx, nil)
}

// The final canceled waiter joins the finite worker before releasing custody.
// Canceling one member does not cancel the read that its peers still need.
func (self *stClientKeyRegistrationReservation) leaveWaiting() {
	var cancel context.CancelFunc
	var done <-chan struct{}
	func() {
		self.cohort.parent.stateLock.Lock()
		defer self.cohort.parent.stateLock.Unlock()
		if !self.waiting {
			return
		}
		self.waiting = false
		self.cohort.waiters--
		if self.cohort.waiters == 0 && !self.cohort.finished {
			self.cohort.closing = true
			cancel, done = self.cohort.cancel, self.cohort.done
		}
	}()
	if cancel != nil {
		cancel()
		<-done
	}
}

// A canceled caller never receives authority even when completion races it.
func (self *stClientKeyRegistrationReservation) readBoundary(ctx context.Context) (protocol.ClientKeyEffectiveBoundary, stabi.STCoordinatorOperatorVersion, error) {
	defer self.leaveWaiting()
	select {
	case <-ctx.Done():
		return protocol.ClientKeyEffectiveBoundary{}, stabi.STCoordinatorOperatorVersion{}, ctx.Err()
	case <-self.cohort.done:
	}
	if err := errors.Join(self.cohort.err, ctx.Err()); err != nil {
		return protocol.ClientKeyEffectiveBoundary{}, stabi.STCoordinatorOperatorVersion{}, err
	}
	return self.cohort.boundary, self.cohort.operator, nil
}

// This is called only after the original caller's Sql and publication return.
func (self *stClientKeyRegistrationReservation) release() {
	self.leaveWaiting()
	self.cohort.parent.stateLock.Lock()
	defer self.cohort.parent.stateLock.Unlock()
	if !self.released {
		self.released = true
		self.cohort.parent.slots--
	}
}
