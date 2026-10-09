// Due-only evidence must never authorize the separate missing-payer index.
package model

import (
	"context"
	"crypto/sha256"
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
	return observeLegacySettlementPayerIndexWithCache(ctx, func(ctx context.Context) bool {
		if !full.hasReadyObservation(now()) {
			return false
		}
		identity, err := resourceIdentity(ctx)
		return err == nil && full.readyObservation(ctx, identity, now())
	}, read, now)
}
