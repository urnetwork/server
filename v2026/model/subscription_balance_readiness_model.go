package model

import (
	"context"

	"github.com/urnetwork/server/v2026"
)

const activeTransferBalanceReadinessCount = 16

// The existing active/network/start/end index can stop after the newest
// window. No credit or reservation is cached between readiness observations.
const activeTransferBalanceReadinessSql = `
	SELECT balance_id, balance_byte_count
	FROM transfer_balance
	WHERE network_id = $1 AND active AND start_time <= $2 AND $2 < end_time
	ORDER BY start_time DESC, end_time DESC
	LIMIT $3
`

// HasActiveTransferBalance observes whether the current approximate available
// balance reaches minimum. Contract admission still owns the locked durable
// reservation check. A sufficient subset proves readiness; an insufficient
// window requires the complete reader before reporting a funding shortfall.
func HasActiveTransferBalance(ctx context.Context, networkId server.Id, minimum ByteCount) bool {
	if minimum <= 0 {
		return true
	}
	balances := []*TransferBalance{}
	server.Db(ctx, func(conn server.PgConn) {
		// A retried query must discard any rows decoded before its failure.
		balances = nil
		rows, err := conn.Query(ctx, activeTransferBalanceReadinessSql,
			networkId, server.NowUtc(), activeTransferBalanceReadinessCount)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				balance := &TransferBalance{}
				server.Raise(rows.Scan(&balance.BalanceId, &balance.BalanceByteCount))
				balances = append(balances, balance)
			}
		})
	})
	// Release the PostgreSQL connection before reading the Redis mirror, just
	// as the complete balance reader does. Errors still propagate to readiness.
	applyActiveTransferEscrow(ctx, balances)
	if activeTransferBalancesReach(balances, minimum) {
		return true
	}
	if len(balances) < activeTransferBalanceReadinessCount {
		return false
	}
	// Start over: summing the window with a later full read would double-count
	// grants and mix observations. The fallback retains every current grant.
	return activeTransferBalancesReach(GetActiveTransferBalances(ctx, networkId), minimum)
}

func activeTransferBalancesReach(balances []*TransferBalance, minimum ByteCount) bool {
	for _, balance := range balances {
		if minimum <= balance.BalanceByteCount {
			return true
		}
		// Subtracting from the positive target avoids overflowing a sum of
		// individually valid grants. Escrow adjustment has clamped each to zero.
		minimum -= balance.BalanceByteCount
	}
	return false
}
