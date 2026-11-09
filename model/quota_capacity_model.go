package model

import (
	"context"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// Row-counted capacity (DESIGN.md 20.7's Counters, first sentence): entries, custom
// domains, clients and members count exactly, in the transaction that creates the row,
// under a transaction-scoped per-network lock. §12.8's record CAS, §7.5's member routes
// and §14.6's custom-host-add call, in this order:
//
//	LockNetworkQuotaInTx(ctx, tx, networkId)
//	usedCount := <count the network's rows of the kind, in tx>
//	if err := CheckCapacityInTx(ctx, tx, networkId, limitKey, usedCount); err != nil {
//		return err // a *QuotaRefusalError; tx creates nothing
//	}
//	<create the row in tx>
//
// so two transactions racing for the last unit serialize at the lock, and the second
// counts the first's row. The lock is pg_advisory_xact_lock: transaction-scoped, so it is
// safe behind PgBouncer's transaction pooling, where a session lock could outlive its
// transaction on a server another client then gets. Call CheckCapacityInTx only to
// create: zero means not included and refuses creation, and never activation, reads,
// deletes, erasure, export or maintenance (D-QUOTA-2).

// Takes the network's quota lock until tx ends.
func LockNetworkQuotaInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('network_quota:' || $1::uuid::text, 0))`,
		networkId,
	))
}

// Whether the network may create one more of what limitKey counts, having usedCount:
// nil when it may, else a *QuotaRefusalError naming the limit. Nil, with no I/O, while
// enforce_sidecar_limits is false (R-QUOTA-5). A limit of -1 (`none`) always passes, and
// 0 refuses. A key no limit row names reads 0 and refuses, so a limit this build does not
// know fails closed. Reads the limits through tx where the caches miss.
func CheckCapacityInTx(ctx context.Context, tx server.PgTx, networkId server.Id, limitKey string, usedCount int64) error {
	if !Pro().EnforceSidecarLimits {
		return nil
	}
	tier, limits := networkSidecarLimitsWithTier(ctx, tx, networkId)
	limit := limits.Limit(limitKey)
	if limit == proSidecarLimitNone || usedCount < limit {
		return nil
	}
	hasRow := networkSidecarLimitHasRow(ctx, tx, networkId, limitKey)
	return NewQuotaRefusalError(&sidecar.QuotaRefusal{
		LimitKey: limitKey,
		Limit:    limit,
		Used:     usedCount,
		Upgrade:  quotaRefusalUpgrade(tier, limitKey, usedCount, hasRow),
	})
}

// The network's effective limits as GET /sidecar/limits and Quota.Limits answer them
// (D-QUOTA-11): its tier, then every key's limit after its network_sidecar_limit rows,
// the sidecar_abuse: keys among them, directory_* included, so §21.7's tightening reaches
// daemons and readers. It reports the limits as configured whether or not
// enforce_sidecar_limits is on, as ProConfig.FeatureEnabled reports the plan.
func NetworkQuotaLimits(ctx context.Context, networkId server.Id) *sidecar.QuotaLimits {
	tier, limits := networkSidecarLimitsWithTier(ctx, nil, networkId)
	return &sidecar.QuotaLimits{
		Tier:   string(tier),
		Limits: limits.Limits(),
	}
}
