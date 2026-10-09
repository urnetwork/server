// Due-only evidence must never authorize the separate missing-payer index.
package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/urnetwork/server"
)

var legacySettlementPayerDueIndexes legacySettlementPayerIndexCache

func legacySettlementPayerIndexResourceIdentity(ctx context.Context) ([sha256.Size]byte, error) {
	resource, err := server.Vault.SimpleResource(server.DefaultPgVaultResourceName)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	raw, err := resource.BytesBoundedE(ctx, 16*1024)
	return sha256.Sum256(raw), err
}

// Invocation-local dependencies exercise the exact ordinary due-entry owner
// from cold state without manually warming the separate full-index cache.
func observeLegacySettlementPayerDueIndexWithProof(ctx context.Context,
	full, due *legacySettlementPayerIndexCache,
	resourceIdentity func(context.Context) ([sha256.Size]byte, error),
	now func() time.Time, read func(context.Context) (bool, error),
) LegacySettlementPayerIndexReadiness {
	usedCache := false
	observation := observeLegacySettlementPayerDueIndex(ctx, func(bounded context.Context) (bool, error) {
		started := now()
		identity, err := resourceIdentity(bounded)
		if err != nil {
			return false, err
		}
		if full.readyObservation(bounded, identity, now()) || due.readyObservation(bounded, identity, now()) {
			usedCache = true
			return true, nil
		}
		// Every miss retains its one original query, including concurrent
		// misses. A positive proof never makes another caller wait for it.
		ready, err := read(bounded)
		if !ready || err != nil || bounded.Err() != nil {
			return ready, err
		}
		validatedIdentity, err := resourceIdentity(bounded)
		if err != nil {
			return false, err
		}
		if validatedIdentity != identity {
			return false, errors.New("legacy payer due index resource changed during validation")
		}
		due.publishDueProof(bounded, identity, started, now())
		return true, nil
	}, now)
	observation.Cached = usedCache && observation.Outcome == "ready"
	return observation
}

// Only fresh successful due checks write this separate one-entry cache. A
// failed or in-flight reader cannot suppress another caller's original probe
// or erase its positive proof. Reuse and late replies never move expiry later.
func (self *legacySettlementPayerIndexCache) publishDueProof(ctx context.Context,
	identity [sha256.Size]byte, started, now time.Time) {
	expires := started.Add(legacySettlementPayerIndexCacheLifetime)
	if ctx.Err() != nil || !now.Before(expires) {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if ctx.Err() != nil || self.ready && expires.Before(self.expires) {
		return
	}
	self.identity, self.expires, self.ready = identity, expires, true
}
