// Cleanup proofs belong to one store instance. Only a returned transport or
// an acquisition that never handed out a grant may make its claim retryable.
// Unknown claims from another store or process retain their existing owner.
package monitor

import (
	"context"
	"errors"
	"slices"
	"time"
)

// Immutable owner identity plus the caller's joined-or-unstarted proof. The
// store mutex protects these records, never file locking or state publication.
type sshAdmissionClaim struct {
	owner sshAdmissionOwner
	ready bool
}

// Reserve bounded recovery ownership before any possibly ambiguous publication.
func (self *sshAdmissionStore) trackClaim(token string, owner sshAdmissionOwner) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.tokenClaims) >= sshAdmissionQueueLimit {
		return errors.New("SSH admission local claim limit")
	}
	if _, exists := self.tokenClaims[token]; exists {
		return errors.New("SSH admission local token collision")
	}
	if self.tokenClaims == nil {
		self.tokenClaims = map[string]sshAdmissionClaim{}
	}
	self.tokenClaims[token] = sshAdmissionClaim{owner: owner}
	return nil
}

// Forget only exact proofs after successful removal, or before enrollment was
// attempted. A failed or ambiguous removal keeps ownership for a future call.
func (self *sshAdmissionStore) forgetClaims(claims map[string]sshAdmissionOwner) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for token, owner := range claims {
		if claim, exists := self.tokenClaims[token]; exists && claim.owner == owner {
			delete(self.tokenClaims, token)
		}
	}
}

// The existing caller release seam supplies the proof; cancellation or age
// alone never reaches it while a transport remains inside its owned callback.
func (self *sshAdmissionStore) releaseClaim(token string, owner sshAdmissionOwner) error {
	found, err := func() (bool, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		claim, exists := self.tokenClaims[token]
		if !exists {
			return false, nil
		}
		if claim.owner != owner {
			return false, errors.New("SSH admission local owner mismatch")
		}
		claim.ready = true
		self.tokenClaims[token] = claim
		return true, nil
	}()
	if err != nil || !found {
		return err
	}
	cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return self.removeClaims(cleanup, map[string]sshAdmissionOwner{token: owner})
}

// A normal subsequent acquisition retries all locally proved cleanup in one
// bounded state update, within its unchanged owner and admission deadline.
func (self *sshAdmissionStore) recoverClaims(ctx context.Context) error {
	claims := func() map[string]sshAdmissionOwner {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		claims := map[string]sshAdmissionOwner{}
		for token, claim := range self.tokenClaims {
			if claim.ready {
				claims[token] = claim.owner
			}
		}
		return claims
	}()
	if len(claims) == 0 {
		return nil
	}
	cleanup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return self.removeClaims(cleanup, claims)
}

// Removal is idempotent, including after a previous publication lost its reply.
// One mismatched owner refuses the whole update and preserves every proof.
func (self *sshAdmissionStore) removeClaims(ctx context.Context, claims map[string]sshAdmissionOwner) error {
	err := self.update(ctx, func(state *sshAdmissionState, _ int64) error {
		for _, request := range state.Requests {
			if owner, exists := claims[request.Token]; exists && request.Owner != owner {
				return errors.New("SSH admission release owner mismatch")
			}
		}
		state.Requests = slices.DeleteFunc(state.Requests, func(request sshAdmissionRequest) bool {
			_, exists := claims[request.Token]
			return exists
		})
		return nil
	})
	if err == nil {
		self.forgetClaims(claims)
	}
	return err
}
