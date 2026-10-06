package model

import (
	"context"
	"fmt"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

// A cached amount is useful only at the exact durable revision in this same
// snapshot. Every lookup retains its primary-key boundary under stale stats.
// Admission calls this in a fresh statement AFTER its balance locks complete.
const netEscrowAdmissionCacheSQL = `
 SELECT requested.balance_id, COALESCE(revision.revision, 0),
     snapshot.reserved_byte_count, balance.end_time
 FROM unnest($1::uuid[]) AS requested(balance_id)
 LEFT JOIN LATERAL (
     SELECT revision FROM transfer_balance_net_escrow_revision
     WHERE balance_id = requested.balance_id OFFSET 0
 ) AS revision ON true
 LEFT JOIN LATERAL (
     SELECT end_time FROM transfer_balance
     WHERE balance_id = requested.balance_id OFFSET 0
 ) AS balance ON true
 LEFT JOIN LATERAL (
     SELECT revision, reserved_byte_count FROM transfer_balance_net_escrow_snapshot
     WHERE balance_id = requested.balance_id OFFSET 0
 ) AS snapshot ON snapshot.revision = COALESCE(revision.revision, 0)
`

// Only an exact census or its known in-transaction reservation delta reaches this
// statement. A concurrent/legacy writer can invalidate the amount; never stamp
// an older amount with its newer revision. Tombstones prevent revision ABA.
const netEscrowPublishAdmissionCacheSQL = `
 INSERT INTO transfer_balance_net_escrow_snapshot AS cached
     (balance_id, revision, reserved_byte_count)
 SELECT expected.balance_id, expected.revision, expected.reserved_byte_count
 FROM unnest($1::uuid[], $2::bigint[], $3::bigint[])
     AS expected(balance_id, revision, reserved_byte_count)
 CROSS JOIN LATERAL (
     SELECT balance_id FROM transfer_balance
     WHERE balance_id = expected.balance_id OFFSET 0
 ) AS balance
 LEFT JOIN LATERAL (
     SELECT revision FROM transfer_balance_net_escrow_revision
     WHERE balance_id = expected.balance_id OFFSET 0
 ) AS revision ON true
 WHERE COALESCE(revision.revision, 0) = expected.revision
 ORDER BY expected.balance_id
 ON CONFLICT (balance_id) DO UPDATE SET
     revision = EXCLUDED.revision, reserved_byte_count = EXCLUDED.reserved_byte_count
 WHERE cached.revision <= EXCLUDED.revision
`

var netEscrowAdmissionSnapshots = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_net_escrow_admission_snapshot_total",
	Help: "Attempted admission balance snapshots reused at a durable revision or reloaded from exact history; not committed contracts.",
}, []string{"result"})

func init() { prometheus.MustRegister(netEscrowAdmissionSnapshots) }

func netEscrowAdmissionCacheArgs(pending map[server.Id]netEscrowSnapshot, balanceIds []server.Id) []any {
	revisions := make([]int64, len(balanceIds))
	amounts := make([]int64, len(balanceIds))
	for i, id := range balanceIds {
		snapshot, ok := pending[id]
		if !ok || snapshot.revision < 0 || snapshot.reserved < 0 {
			panic("invalid admission snapshot cache publication")
		}
		revisions[i] = snapshot.revision
		amounts[i] = int64(snapshot.reserved)
	}
	return []any{balanceIds, revisions, amounts}
}

// The amount and matching durable revision come from one PostgreSQL snapshot.
// Admission calls this after its balance locks; mirror readers need no financial
// locks because their Redis publication retains the revision fence. Missing,
// stale and deleted-balance entries stay absent for exact fallback.
func readCachedNetEscrowSnapshots(ctx context.Context, query server.PgCanQuery, balanceIds []server.Id) map[server.Id]netEscrowSnapshot {
	pending := map[server.Id]netEscrowSnapshot{}
	if len(balanceIds) == 0 {
		return pending
	}
	rows, err := query.Query(ctx, netEscrowAdmissionCacheSQL, balanceIds)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var id server.Id
			var snapshot netEscrowSnapshot
			var amount *int64
			server.Raise(rows.Scan(&id, &snapshot.revision, &amount, &snapshot.endTime))
			if snapshot.revision < 0 || (amount != nil && *amount < 0) {
				server.Raise(fmt.Errorf("invalid admission reservation snapshot"))
			}
			if amount != nil && snapshot.endTime != nil {
				snapshot.reserved = ByteCount(*amount)
				pending[id] = snapshot
			}
		}
	})
	return pending
}

func missingNetEscrowSnapshots(pending map[server.Id]netEscrowSnapshot, balanceIds []server.Id) []server.Id {
	missing := make([]server.Id, 0, len(balanceIds)-len(pending))
	for _, id := range balanceIds {
		if _, ok := pending[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}

// Only snapshots freshly read from committed PostgreSQL state reach this
// helper. The read-only census has already released its connection and holds
// no financial row locks. A later writer may have advanced the revision: keep
// the observed revision, and let guarded publication reject that stale amount.
// Deleted balances remain absent. An older publication cannot replace a newer
// cached revision, even if it waited for that cache row's transaction.
func cacheCommittedNetEscrowSnapshots(ctx context.Context, pending map[server.Id]netEscrowSnapshot) {
	ids := make([]server.Id, 0, len(pending))
	for id, snapshot := range pending {
		if snapshot.endTime != nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	slices.SortFunc(ids, server.Id.Cmp)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, netEscrowPublishAdmissionCacheSQL, netEscrowAdmissionCacheArgs(pending, ids)...))
	}, server.TxReadCommitted)
}

// Existing balance locks serialize admission. This cache removes historical
// scans from that critical section after its first exact census. Old binaries
// and other financial writers continue to advance the guarded revision, making
// their unreflected changes misses. No Redis data authorizes credit here.
func readLockedNetEscrowSnapshots(ctx context.Context, tx server.PgTx, balanceIds []server.Id) map[server.Id]netEscrowSnapshot {
	defer server.EnterContractCreationStage(ctx, server.ContractStageReservationSnapshot)()
	pending := readCachedNetEscrowSnapshots(ctx, tx, balanceIds)
	if len(balanceIds) == 0 {
		return pending
	}
	missing := missingNetEscrowSnapshots(pending, balanceIds)
	if len(missing) > 0 {
		exact := readNetEscrowSnapshots(ctx, tx, missing)
		// Ordered cache writes follow the already ordered balance locks. Warming
		// also commits for an insufficient-credit result; no reservation is added.
		slices.SortFunc(missing, func(a, b server.Id) int { return a.Cmp(b) })
		server.RaisePgResult(tx.Exec(ctx, netEscrowPublishAdmissionCacheSQL, netEscrowAdmissionCacheArgs(exact, missing)...))
		for id, snapshot := range exact {
			pending[id] = snapshot
		}
	}
	netEscrowAdmissionSnapshots.WithLabelValues("reused").Add(float64(len(balanceIds) - len(missing)))
	netEscrowAdmissionSnapshots.WithLabelValues("reloaded").Add(float64(len(missing)))
	return pending
}
