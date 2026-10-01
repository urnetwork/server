// Dynamic candidate reads keep internal-prober grant history off the contract
// hot path. Every selected window is locked before a fresh database reservation
// census. Failed windows release locks before the next window or full fallback.
package model

import (
	"context"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

const proberGrantFirstCount = 16
const proberGrantExtendedCount = 48

// One allocation owns these observations. Neither durable nor reserved bytes
// are cached across requests, and no process-wide admission budget is added.
type escrowTransferBalance struct {
	balanceId        server.Id
	paid             bool
	balanceByteCount ByteCount
	startTime        time.Time
	endTime          time.Time
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
	sql += ` ORDER BY balance_id FOR UPDATE`
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
	reserved := readNetEscrowSnapshots(ctx, tx, lockedBalanceIds)
	for _, balance := range balances {
		balance.balanceByteCount = max(0, balance.balanceByteCount-reserved[balance.balanceId].reserved)
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
	if requestedBytes > 0 {
		balances := []*escrowTransferBalance{}
		internalProber := false
		rawCount := 0
		rows, err := tx.Query(ctx, proberGrantSelectionSql, payerNetworkId, now, proberGrantFirstCount, ProberTransferBalanceTopUp)
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
			// A failed window must not carry higher-id locks into a later window
			// or full fallback; rollback releases only these speculative locks.
			server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT prober_grant_selection`))
			candidateIds := make([]server.Id, 0, len(balances))
			for _, balance := range balances {
				candidateIds = append(candidateIds, balance.balanceId)
			}
			locked := lockTransferEscrowBalances(ctx, tx, payerNetworkId, now, candidateIds, true)
			lockedBalanceIdBalances := map[server.Id]*escrowTransferBalance{}
			for _, balance := range locked {
				lockedBalanceIdBalances[balance.balanceId] = balance
			}
			balances = nil
			for _, id := range candidateIds {
				if balance := lockedBalanceIdBalances[id]; balance != nil {
					balances = append(balances, balance)
				}
			}
			if len(balances) == 0 {
				server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT prober_grant_selection`))
				server.RaisePgResult(tx.Exec(ctx, `RELEASE SAVEPOINT prober_grant_selection`))
				return nil
			}
			start := int(payerClientId.Hash() % uint64(len(balances)))
			for offset := range balances {
				balance := balances[(start+offset)%len(balances)]
				if requestedBytes <= balance.balanceByteCount {
					server.RaisePgResult(tx.Exec(ctx, `RELEASE SAVEPOINT prober_grant_selection`))
					return balance
				}
			}
			server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT prober_grant_selection`))
			server.RaisePgResult(tx.Exec(ctx, `RELEASE SAVEPOINT prober_grant_selection`))
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
