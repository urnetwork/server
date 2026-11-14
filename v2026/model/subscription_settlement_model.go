// Applies captured settlement amounts without changing allocation or ownership.
package model

import (
	"slices"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Marks every captured row, including legacy zero-byte rows, in one statement.
// The contract and balance keys preserve the old per-row update boundary.
func queueEscrowSettlementUpdates(
	batch server.PgBatch,
	contractId server.Id,
	settleTime time.Time,
	sweepPayouts map[server.Id]sweepPayout,
) {
	if len(sweepPayouts) == 0 {
		return
	}
	balanceIds := make([]server.Id, 0, len(sweepPayouts))
	for balanceId := range sweepPayouts {
		balanceIds = append(balanceIds, balanceId)
	}
	slices.SortFunc(balanceIds, server.Id.Cmp)
	payoutByteCounts := make([]ByteCount, len(balanceIds))
	for index, balanceId := range balanceIds {
		payoutByteCounts[index] = sweepPayouts[balanceId].payoutByteCount
	}
	batch.Queue(`
		UPDATE transfer_escrow AS escrow
		SET settled = true, settle_time = $2, payout_byte_count = payout.byte_count
		FROM unnest($3::uuid[], $4::bigint[]) AS payout(balance_id, byte_count)
		WHERE escrow.contract_id = $1 AND escrow.balance_id = payout.balance_id
	`, contractId, settleTime, balanceIds, payoutByteCounts)
}

// The payout row keeps its first insertion clock across metadata replay. It
// leaves destination_id NULL: provider_payouts names every provider client,
// and only sweeps without provider_payouts read destination_id
// (contract_provider_attribution.go, BILLING-SPLIT.md).
const participantSweepInsertSQL = `
									INSERT INTO transfer_escrow_sweep (
										contract_id,
										balance_id,
										network_id,
										payout_byte_count,
										payout_net_revenue_nano_cents,
										provider_payouts,
										sweep_time
									)
									VALUES ($1, $2, $3, $4, $5, $6, now() AT TIME ZONE 'UTC')
									ON CONFLICT (contract_id, balance_id, network_id) DO UPDATE
									SET
										payout_byte_count = $4,
										payout_net_revenue_nano_cents = $5,
										provider_payouts = $6
								`
