package monitor

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Records belong to one store instance. A ready record proves either that the
// admitted caller reached its existing joined-release boundary, or that acquire
// failed before handing any reservation to native transport. Queue expiry and
// owner liveness never supply that proof. A fresh store inherits no such proof.
type sshAdmissionClaim struct {
	token string
	owner sshAdmissionOwner
	ready bool
}

type sshAdmissionClaims struct {
	mu      sync.Mutex
	records map[string]*sshAdmissionClaim
}

func (self *sshAdmissionStore) trackClaim(token string, owner sshAdmissionOwner) (*sshAdmissionClaim, error) {
	self.claims.mu.Lock()
	defer self.claims.mu.Unlock()
	if self.claims.records == nil {
		self.claims.records = map[string]*sshAdmissionClaim{}
	}
	// Reserve local ownership before a possibly ambiguous publication. Failed
	// unpublished attempts cannot create an unbounded retry backlog.
	if len(self.claims.records) >= sshAdmissionQueueLimit {
		return nil, errors.New("SSH admission local claim capacity unavailable")
	}
	if _, present := self.claims.records[token]; present {
		return nil, errors.New("SSH admission local claim collision")
	}
	claim := &sshAdmissionClaim{token: token, owner: owner}
	self.claims.records[token] = claim
	return claim, nil
}

func (self *sshAdmissionStore) forgetClaim(claim *sshAdmissionClaim) {
	self.claims.mu.Lock()
	defer self.claims.mu.Unlock()
	if self.claims.records[claim.token] == claim {
		delete(self.claims.records, claim.token)
	}
}

// The original one-second cleanup remains bounded. A failed cleanup retains
// its proof for the next ordinary acquisition, without spawning retry workers.
func (self *sshAdmissionStore) releaseClaim(claim *sshAdmissionClaim) error {
	func() {
		self.claims.mu.Lock()
		defer self.claims.mu.Unlock()
		if self.claims.records[claim.token] == claim {
			claim.ready = true
		}
	}()
	cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
	if self.cleanupContext != nil {
		cancel()
		cleanup, cancel = self.cleanupContext()
	}
	defer cancel()
	return self.recoverClaims(cleanup)
}

// Snapshot proofs under the instance lock, then perform bounded disk I/O with
// no instance lock held. Concurrent recoveries are harmless: exact removal is
// idempotent, and only the successfully published snapshot is forgotten.
func (self *sshAdmissionStore) recoverClaims(ctx context.Context) error {
	ready := func() map[string]*sshAdmissionClaim {
		self.claims.mu.Lock()
		defer self.claims.mu.Unlock()
		result := map[string]*sshAdmissionClaim{}
		for token, claim := range self.claims.records {
			if claim.ready {
				result[token] = claim
			}
		}
		return result
	}()
	if len(ready) == 0 {
		return nil
	}
	if err := self.update(ctx, func(state *sshAdmissionState, _ int64) error {
		// Validate all matching owners before mutating the in-memory state.
		for _, request := range state.Requests {
			if claim, present := ready[request.Token]; present && request.Owner != claim.owner {
				return errors.New("SSH admission recovery owner mismatch")
			}
		}
		kept := state.Requests[:0]
		for _, request := range state.Requests {
			if _, present := ready[request.Token]; !present {
				kept = append(kept, request)
			}
		}
		state.Requests = kept
		return nil
	}); err != nil {
		return err
	}
	self.claims.mu.Lock()
	defer self.claims.mu.Unlock()
	for token, claim := range ready {
		if self.claims.records[token] == claim {
			delete(self.claims.records, token)
		}
	}
	return nil
}
