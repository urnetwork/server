// Dynamic candidate reads keep internal-prober grant history off the contract
// hot path. The fast path locks only its selected grant before a fresh database
// reservation census. Failed candidates release their speculative locks.
package model

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

const proberGrantFirstCount = 16
const proberGrantExtendedCount = 48

// Cap exact reservation work when optimistic durable-credit candidates are full.
const proberGrantAttemptsPerWindow = 4

// One allocation owns these observations. Neither durable nor reserved bytes
// are cached across requests, and no process-wide admission budget is added.
type escrowTransferBalance struct {
	balanceId        server.Id
	paid             bool
	balanceByteCount ByteCount
	startTime        time.Time
	endTime          time.Time
	reservation      netEscrowSnapshot
}

var proberGrantSelectionResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_prober_grant_selection_total",
	Help: "Internal-prober positive-byte allocations by bounded candidate selection outcome; not completed probes.",
}, []string{"result"})

// Registers only finite outcomes, without payer, client, or grant labels.
func init() {
	prometheus.MustRegister(proberGrantSelectionResults)
}

const escrowTransferBalanceSql = `
	SELECT balance_id, paid, balance_byte_count, start_time, end_time
	FROM transfer_balance
	WHERE network_id = $1 AND active AND start_time <= $2 AND $2 < end_time
`

// The existing active/network/start/end index supplies this order directly.
// The limit precedes free-grant eligibility: unusual paid or small grants
// cannot turn a bounded candidate read into an unbounded search for a match.
// Ordinary payers skip discovery and use the complete locked read, then the
// existing earliest-expiry order. Reading their grants here would duplicate
// that authoritative read. The persisted singleton is the scope.
const proberGrantSelectionSql = `
	WITH allocation AS MATERIALIZED (
		SELECT EXISTS (
			SELECT 1 FROM prober_identity WHERE singleton AND network_id = $1
			UNION ALL SELECT 1 FROM prober_shard_run WHERE network_id = $1
		) AS internal_prober
	)
	SELECT selected.balance_id, selected.paid, selected.balance_byte_count,
		selected.start_time, selected.end_time, allocation.internal_prober,
		NOT selected.paid AND NOT selected.pro
			AND selected.net_revenue_nano_cents = 0
			AND selected.subsidy_net_revenue_nano_cents = 0
			AND selected.start_balance_byte_count >= $4 AS preferred
	FROM allocation
	CROSS JOIN LATERAL (
		SELECT balance_id, paid, balance_byte_count, start_time, end_time,
			pro, net_revenue_nano_cents, subsidy_net_revenue_nano_cents,
			start_balance_byte_count
		FROM transfer_balance
		WHERE network_id = $1 AND active AND start_time <= $2 AND $2 < end_time
		ORDER BY start_time DESC, end_time DESC
		LIMIT CASE WHEN allocation.internal_prober THEN $3::bigint ELSE 0 END
	) AS selected
`

// Rechecking a small adjacent window handles reserved newest grants. Offset
// counts at most the first window again and avoids an unindexed identifier
// tie-break sort. A tie or concurrent row movement can only miss a fast-path
// opportunity: complete fallback remains authoritative for funding.
const proberGrantExtendedSql = `
	SELECT balance_id, paid, balance_byte_count, start_time, end_time,
		NOT paid AND NOT pro AND net_revenue_nano_cents = 0
			AND subsidy_net_revenue_nano_cents = 0
			AND start_balance_byte_count >= $5 AS preferred
	FROM transfer_balance
	WHERE network_id = $1 AND active AND start_time <= $2 AND $2 < end_time
	ORDER BY start_time DESC, end_time DESC
	LIMIT $3 OFFSET $4
`

// Locks a whole candidate set in the same id order as settlement. Nil selects
// all current grants; a nonnil set rechecks the prober's free-grant predicates.
// Read committed must take a new snapshot after any lock wait. Zero-byte
// anchors retain the request's original time boundary and consume no credit.
func lockTransferEscrowBalances(
	ctx context.Context, tx server.PgTx, payerNetworkId server.Id, now time.Time,
	candidateIds []server.Id, recheckExpiry bool,
) []*escrowTransferBalance {
	return lockTransferEscrowBalanceRows(ctx, tx, payerNetworkId, now, candidateIds, recheckExpiry, false)
}

func lockTransferEscrowBalanceRows(
	ctx context.Context, tx server.PgTx, payerNetworkId server.Id, now time.Time,
	candidateIds []server.Id, recheckExpiry, skipLocked bool,
) []*escrowTransferBalance {
	if candidateIds != nil && len(candidateIds) == 0 {
		return nil
	}
	sql := escrowTransferBalanceSql
	args := []any{payerNetworkId, now}
	if candidateIds != nil {
		sql += ` AND balance_id = ANY($3) AND NOT paid AND NOT pro
			AND net_revenue_nano_cents = 0 AND subsidy_net_revenue_nano_cents = 0
			AND start_balance_byte_count >= $4`
		args = append(args, candidateIds, ProberTransferBalanceTopUp)
	}
	ownershipIds := candidateIds
	if ownershipIds == nil {
		// Discover the complete eligible key set before any grant row lock.
		// The locking read below is constrained to those admitted identities;
		// a newly inserted grant cannot enter it without an ownership check.
		rows, err := tx.Query(ctx, `SELECT selected.balance_id FROM (`+sql+`) AS selected`, args...)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				ownershipIds = append(ownershipIds, id)
			}
		})
	}
	if len(ownershipIds) == 0 {
		return nil
	}
	admitted, err := tryTransferBalanceOwnershipInTx(ctx, tx, ownershipIds)
	server.Raise(err)
	if !admitted {
		server.Raise(errTransferBalanceOwnershipBusy)
	}
	args = append(args, ownershipIds)
	sql += fmt.Sprintf(" AND balance_id=ANY($%d::uuid[])", len(args))
	if skipLocked {
		// Read at most one row from this bounded preference list. LIMIT stops
		// row locking after one available grant; SKIP LOCKED lets a different
		// client use another grant while its preferred grant is busy.
		if len(candidateIds) == 0 || len(candidateIds) > proberGrantExtendedCount {
			panic("speculative grant lock requires a bounded candidate window")
		}
		sql += ` ORDER BY array_position($3::uuid[], balance_id) LIMIT 1 FOR UPDATE SKIP LOCKED`
	} else {
		// Complete fallback retains its blocking, ordered authority check.
		sql += ` ORDER BY balance_id FOR UPDATE`
	}
	balances := []*escrowTransferBalance{}
	lockedBalanceIds := []server.Id{}
	rows, err := tx.Query(ctx, sql, args...)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			balance := &escrowTransferBalance{}
			server.Raise(rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount,
				&balance.startTime, &balance.endTime))
			balances = append(balances, balance)
			lockedBalanceIds = append(lockedBalanceIds, balance.balanceId)
		}
	})
	// This statement must remain separate from the locking query: its snapshot
	// must include reservations committed by the preceding balance-lock owner.
	reserved := readLockedNetEscrowSnapshots(ctx, tx, lockedBalanceIds)
	approxReserved := readRedisContractReservations(ctx, lockedBalanceIds)
	for _, balance := range balances {
		balance.reservation = reserved[balance.balanceId]
		balance.balanceByteCount = max(0, balance.balanceByteCount-balance.reservation.reserved)
		balance.balanceByteCount = max(0, balance.balanceByteCount-approxReserved[balance.balanceId])
	}
	if recheckExpiry {
		now = server.NowUtc()
	}
	return slices.DeleteFunc(balances, func(balance *escrowTransferBalance) bool {
		return balance.startTime.After(now) || !now.Before(balance.endTime)
	})
}

// Only a free, data-only internal grant funding the whole positive request
// wins the fast path. Otherwise the original full read and reservation check
// restart from scratch; partial candidates never create financial writes.
func loadTransferEscrowBalances(
	ctx context.Context, tx server.PgTx, payerNetworkId, payerClientId server.Id,
	now time.Time, requestedBytes ByteCount,
) []*escrowTransferBalance {
	defer server.EnterContractCreationStage(ctx, server.ContractStageGrantSelection)()
	if requestedBytes == 0 {
		// An anchor consumes no credit. Preserve its earliest-expiry priority
		// without queuing behind a financial allocation or scanning escrow
		// history for amounts that cannot affect this zero-byte reservation.
		balances := []*escrowTransferBalance{}
		rows, err := tx.Query(ctx, escrowTransferBalanceSql+`
			ORDER BY end_time, start_time, balance_id LIMIT 1`, payerNetworkId, now)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				balance := &escrowTransferBalance{}
				server.Raise(rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount,
					&balance.startTime, &balance.endTime))
				balances = append(balances, balance)
			}
		})
		return balances
	}
	if requestedBytes > 0 {
		// This compatibility allocator may expand its16/48 preference to
		// the complete eligible set. Admit that possible scope once before
		// shared locks or speculative savepoints. Only IDs are discovered;
		// the established preference and exact financial census stay below.
		// Public Redis admission returns before this legacy path.
		var ownershipIds []server.Id
		rows, err := tx.Query(ctx, `SELECT balance_id FROM transfer_balance
 WHERE network_id=$1 AND active AND start_time<=$2 AND $2<end_time ORDER BY balance_id`, payerNetworkId, now)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				ownershipIds = append(ownershipIds, id)
			}
		})
		admitted, err := tryTransferBalanceOwnershipInTx(ctx, tx, ownershipIds)
		server.Raise(err)
		if !admitted {
			server.Raise(errTransferBalanceOwnershipBusy)
		}
		balances := []*escrowTransferBalance{}
		internalProber := false
		rawCount := 0
		rows, err = tx.Query(ctx, proberGrantSelectionSql, payerNetworkId, now, proberGrantFirstCount, ProberTransferBalanceTopUp)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				balance := &escrowTransferBalance{}
				preferred := false
				server.Raise(rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount,
					&balance.startTime, &balance.endTime, &internalProber, &preferred))
				rawCount++
				if !internalProber || preferred {
					balances = append(balances, balance)
				}
			}
		})
		if !internalProber {
			return lockTransferEscrowBalances(ctx, tx, payerNetworkId, now, nil, true)
		}

		outcome := "error"
		defer func() { proberGrantSelectionResults.WithLabelValues(outcome).Inc() }()
		selectBalance := func() *escrowTransferBalance {
			if len(balances) == 0 {
				return nil
			}
			// Durable credit is an optimistic hint only. An exact reservation
			// census across all 16/48 candidates repeated thousands of escrow
			// lookups per grant even when the first selected grant could fund us.
			// Never authorize bytes until a fresh census AFTER locking one row.
			start := int(payerClientId.Hash() % uint64(len(balances)))
			candidateIds := make([]server.Id, 0, len(balances))
			for offset := range balances {
				candidate := balances[(start+offset)%len(balances)]
				if candidate.balanceByteCount >= requestedBytes {
					candidateIds = append(candidateIds, candidate.balanceId)
				}
			}
			for attempt := 0; attempt < proberGrantAttemptsPerWindow && len(candidateIds) > 0; attempt++ {
				// SKIP LOCKED chooses another available grant without joining a
				// busy grant's queue. LIMIT 1 bounds each exact reservation census.
				server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT prober_grant_selection`))
				locked := lockTransferEscrowBalanceRows(ctx, tx, payerNetworkId, now,
					candidateIds, true, true)
				if len(locked) == 1 && requestedBytes <= locked[0].balanceByteCount {
					server.RaisePgResult(tx.Exec(ctx, `RELEASE SAVEPOINT prober_grant_selection`))
					return locked[0]
				}
				// Release a rejected grant BEFORE touching another grant. This
				// preserves the complete fallback/settlement lock order and keeps
				// speculative attempts free of partial financial writes.
				server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT prober_grant_selection`))
				server.RaisePgResult(tx.Exec(ctx, `RELEASE SAVEPOINT prober_grant_selection`))
				if len(locked) == 0 {
					break
				}
				rejected := locked[0].balanceId
				candidateIds = slices.DeleteFunc(candidateIds, func(id server.Id) bool { return id == rejected })
			}
			// Optimistic hints or SKIP LOCKED may miss available credit. The
			// original blocking full read remains authoritative for funding.
			return nil
		}
		if balance := selectBalance(); balance != nil {
			outcome = "selected_first"
			return []*escrowTransferBalance{balance}
		}
		if rawCount == proberGrantFirstCount {
			balances = nil
			rows, err = tx.Query(ctx, proberGrantExtendedSql, payerNetworkId, now,
				proberGrantExtendedCount, proberGrantFirstCount, ProberTransferBalanceTopUp)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					balance := &escrowTransferBalance{}
					preferred := false
					server.Raise(rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount,
						&balance.startTime, &balance.endTime, &preferred))
					if preferred {
						balances = append(balances, balance)
					}
				}
			})
			if balance := selectBalance(); balance != nil {
				outcome = "selected_extended"
				return []*escrowTransferBalance{balance}
			}
		}
		outcome = "fallback"
	}

	return lockTransferEscrowBalances(ctx, tx, payerNetworkId, server.NowUtc(), nil, requestedBytes > 0)
}
