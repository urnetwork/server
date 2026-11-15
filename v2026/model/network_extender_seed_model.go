package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// What the public seed publisher reads beyond the dns pool
// (connect/EXTENDER-UPDATE2.md §2, taskworker/work/extender_seed_publish.go).
//
// The cohort is drawn from GetActiveNetworkExtenderDnsAddresses, the same open
// pool the geo dns sets are sampled from, so the seed and the dns sets cannot
// disagree about which extenders are open. This file adds only the revoked
// half: a cohort member the operator revoked since the last bundle gets a
// root-signed revocation in the next one, so a client holding the old bundle
// learns it was withdrawn rather than waiting for its record to expire.

// The most identity keys one revocation lookup takes. The previous cohort is
// at most the envelope's eight records, so this is a guard, not a page size.
const networkExtenderSeedRevocationLookupMax = 64

// One revoked extender: its identity key and when it was revoked. A
// revocation signed at RevokeTime orders exactly as the one the probe queued
// when it revoked the extender (RecordNetworkExtenderProbeResult): it
// dominates every record issued before then and none issued after a
// re-activation.
type NetworkExtenderSeedRevocation struct {
	ExtenderId server.Id
	PublicKey  []byte
	RevokeTime time.Time
}

// GetRevokedNetworkExtendersByPublicKey reads which of the given identity keys
// belong to an extender that is inactive with a revocation time. A key that
// is active again, unknown or never revoked is not returned: an extender that
// merely left the cohort is not withdrawn, and revoking it would drop it from
// every client that holds its record from any source.
func GetRevokedNetworkExtendersByPublicKey(
	ctx context.Context,
	publicKeys [][]byte,
) []*NetworkExtenderSeedRevocation {
	revocations := []*NetworkExtenderSeedRevocation{}
	if len(publicKeys) == 0 {
		return revocations
	}
	if networkExtenderSeedRevocationLookupMax < len(publicKeys) {
		publicKeys = publicKeys[:networkExtenderSeedRevocationLookupMax]
	}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				extender_id,
				public_key,
				revoke_time
			FROM network_extender
			WHERE
				public_key = ANY($1) AND
				NOT active AND
				revoke_time IS NOT NULL
			ORDER BY public_key
			`,
			publicKeys,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				revocation := &NetworkExtenderSeedRevocation{}
				server.Raise(result.Scan(
					&revocation.ExtenderId,
					&revocation.PublicKey,
					&revocation.RevokeTime,
				))
				revocations = append(revocations, revocation)
			}
		})
	})

	return revocations
}
