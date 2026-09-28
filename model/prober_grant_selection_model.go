// Dynamic candidate reads keep internal-prober grant history off the contract
// hot path. Only read order changes; reservations and all financial writes use
// the existing allocator, with its complete earliest-expiry fallback.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
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
// Ordinary payers use the same single query with no limit, then the unchanged
// earliest-expiry ordering in Go. The persisted singleton is the only scope.
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
		LIMIT CASE WHEN allocation.internal_prober THEN $3::bigint ELSE NULL END
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

// Reads current reservation mirrors with exactly the general allocator's
// missing/negative/error semantics. Nothing is reserved or refreshed here.
func applyTransferEscrowReservations(ctx context.Context, balances []*escrowTransferBalance) {
	server.Redis(ctx, func(r server.RedisClient) {
		commands := map[server.Id]*redis.StringCmd{}
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, balance := range balances {
				commands[balance.balanceId] = pipe.Get(ctx, netEscrowKey(balance.balanceId))
			}
			return nil
		})
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		for _, balance := range balances {
			reserved, err := commands[balance.balanceId].Int64()
			if errors.Is(err, redis.Nil) {
				reserved = 0
			} else {
				server.Raise(err)
			}
			balance.balanceByteCount = max(0, balance.balanceByteCount-max(int64(0), reserved))
		}
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
			applyTransferEscrowReservations(ctx, balances)
			return balances
		}

		outcome := "error"
		defer func() { proberGrantSelectionResults.WithLabelValues(outcome).Inc() }()
		selectBalance := func() *escrowTransferBalance {
			applyTransferEscrowReservations(ctx, balances)
			if len(balances) == 0 {
				return nil
			}
			start := int(payerClientId.Hash() % uint64(len(balances)))
			for offset := range balances {
				balance := balances[(start+offset)%len(balances)]
				if requestedBytes <= balance.balanceByteCount {
					return balance
				}
			}
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

	balances := []*escrowTransferBalance{}
	rows, err := tx.Query(ctx, escrowTransferBalanceSql, payerNetworkId, now)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			balance := &escrowTransferBalance{}
			server.Raise(rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount,
				&balance.startTime, &balance.endTime))
			if !balance.startTime.After(now) && now.Before(balance.endTime) {
				balances = append(balances, balance)
			}
		}
	})
	applyTransferEscrowReservations(ctx, balances)
	return balances
}
