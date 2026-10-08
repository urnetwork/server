package model

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

// Lock the existing contract/balance primary-key tuples before reading the
// reservation snapshot. The offset boundary prevents a global partial-index
// scan when historical statistics incorrectly estimate no unsettled rows.
const settlementReservationRowsSQL = `
 SELECT escrow.balance_id, escrow.balance_byte_count, escrow.settled, escrow.redis_reserved
 FROM unnest(ARRAY[$1::uuid]) AS requested(contract_id)
 CROSS JOIN LATERAL (
     SELECT balance_id, balance_byte_count, settled, redis_reserved
     FROM transfer_escrow
     WHERE contract_id = requested.contract_id
     ORDER BY balance_id
     OFFSET 0 FOR UPDATE
 ) AS escrow
`

var netEscrowSettlementSnapshots = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_net_escrow_settlement_snapshot_total",
	Help: "Attempted settlement balance snapshots reused at a durable revision or deferred to committed mirror work; not committed settlements.",
}, []string{"result"})

func init() { prometheus.MustRegister(netEscrowSettlementSnapshots) }

func lockSettlementReservations(ctx context.Context, tx server.PgTx, contractId server.Id, balanceIds []server.Id) (map[server.Id]ByteCount, []server.Id) {
	allowed := make(map[server.Id]bool, len(balanceIds))
	for _, id := range balanceIds {
		allowed[id] = true
	}
	positive := map[server.Id]ByteCount{}
	var approximate []server.Id
	rows, err := tx.Query(ctx, settlementReservationRowsSQL, contractId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var id server.Id
			var amount ByteCount
			var settled, redisReserved bool
			server.Raise(rows.Scan(&id, &amount, &settled, &redisReserved))
			if allowed[id] && !settled && amount > 0 {
				if redisReserved {
					approximate = append(approximate, id)
				} else {
					positive[id] = amount
				}
			}
		}
	})
	return positive, approximate
}

func settlementReservationIds(positive map[server.Id]ByteCount) []server.Id {
	ids := make([]server.Id, 0, len(positive))
	for id := range positive {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, server.Id.Cmp)
	return ids
}

// The locked contract, exact escrow rows and close reports authorize settlement.
// A matching optional cache can preserve its known reservation delta. A cold
// cache must not make the payer's financial locks cover an unbounded history
// scan; committed mirror work can rebuild it after those locks are released.
// Admission still requires a current exact snapshot before reserving credit.
func readSettlementNetEscrowSnapshots(ctx context.Context, tx server.PgTx, ids []server.Id) map[server.Id]netEscrowSnapshot {
	pending := readCachedNetEscrowSnapshots(ctx, tx, ids)
	missing := missingNetEscrowSnapshots(pending, ids)
	netEscrowSettlementSnapshots.WithLabelValues("reused").Add(float64(len(ids) - len(missing)))
	netEscrowSettlementSnapshots.WithLabelValues("deferred").Add(float64(len(missing)))
	return pending
}

// The caller has made exactly one revision-advancing transition for each
// locked positive row. Any intervening legacy mutation makes expected+1 stale,
// so guarded publication skips it. No later durable revision is borrowed.
func publishSettlementNetEscrowSnapshots(ctx context.Context, tx server.PgTx, pending map[server.Id]netEscrowSnapshot, positive map[server.Id]ByteCount, release bool) {
	ids := []server.Id{}
	for _, id := range settlementReservationIds(positive) {
		snapshot, ok := pending[id]
		if !ok {
			continue
		}
		if release {
			if snapshot.reserved < positive[id] {
				server.Raise(fmt.Errorf("settlement reservation snapshot underflow"))
			}
			snapshot.reserved -= positive[id]
		}
		snapshot.revision++
		pending[id] = snapshot
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		server.RaisePgResult(tx.Exec(ctx, netEscrowPublishAdmissionCacheSQL, netEscrowAdmissionCacheArgs(pending, ids)...))
	}
}

// Visit every existing balance for this contract in settlement lock order.
// Keep the contract range and individual balance probes scoped even when
// historical statistics incorrectly estimate an empty escrow relation.
const settlementMetadataBalanceLocksSQL = `
 SELECT selected_balance.balance_id
 FROM (
     SELECT balance_id FROM transfer_escrow
     WHERE contract_id = $1 AND NOT redis_reserved
     ORDER BY balance_id OFFSET 0
 ) AS selected_escrow
 CROSS JOIN LATERAL (
     SELECT balance_id FROM transfer_balance
     WHERE balance_id = selected_escrow.balance_id
     OFFSET 0 FOR UPDATE
 ) AS selected_balance
`

// Reuse authority only inside the transaction that locked every grant and
// exact escrow tuple, then claimed the outcome. Every metadata target must
// match a positive unmarked reservation captured under those locks. The same
// snapshot map has already advanced for the outcome; advance it once more
// without releasing the reservation twice or borrowing a later revision.
func settleEscrowOwnedMetadataInTx(ctx context.Context, tx server.PgTx, contractId server.Id, settleTime time.Time, sweepPayouts map[server.Id]sweepPayout, positive map[server.Id]ByteCount, pending map[server.Id]netEscrowSnapshot) bool {
	if len(sweepPayouts) != len(positive) {
		return false
	}
	for id, payout := range sweepPayouts {
		if amount, ok := positive[id]; !ok || amount <= 0 || amount != payout.escrowBalanceByteCount {
			return false
		}
	}
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		queueEscrowSettlementUpdates(batch, contractId, settleTime, sweepPayouts)
	})
	publishSettlementNetEscrowSnapshots(ctx, tx, pending, positive, false)
	return true
}

// Metadata is still a separate post. Read committed authority here, never a
// prediction captured before the outcome commit: callbacks after rollback or
// ambiguous commits must not reuse an abandoned transaction's revision.
func settleEscrowMetadataInTx(ctx context.Context, tx server.PgTx, contractId server.Id, settleTime time.Time, sweepPayouts map[server.Id]sweepPayout) {
	var terminal bool
	rows, err := tx.Query(ctx, `SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, contractId)
	server.WithPgResult(rows, err, func() {
		if rows.Next() {
			server.Raise(rows.Scan(&terminal))
		}
	})
	if !terminal {
		return
	}
	admitted, err := tryContractTransferBalanceOwnershipInTx(ctx, tx, []server.Id{contractId})
	server.Raise(err)
	if !admitted {
		server.Raise(errTransferBalanceOwnershipBusy)
	}
	// Metadata advances the same revision as admission and financial settlement.
	// Keep its read/update/publication inside their balance fence, or a harmless
	// settled-flag update can invalidate current snapshots and force full history
	// reloads under a busy payer. Match contract -> sorted balances -> escrow ->
	// revision ordering, including balances outside a partial caller payout map.
	// Complete this locking statement before the separate fresh snapshot read.
	rows, err = tx.Query(ctx, settlementMetadataBalanceLocksSQL, contractId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
		}
	})
	ids := make([]server.Id, 0, len(sweepPayouts))
	for id := range sweepPayouts {
		ids = append(ids, id)
	}
	positive, _ := lockSettlementReservations(ctx, tx, contractId, ids)
	pending := readCachedNetEscrowSnapshots(ctx, tx, settlementReservationIds(positive))
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		queueEscrowSettlementUpdates(batch, contractId, settleTime, sweepPayouts)
	})
	// A terminal contract contributes zero before and after metadata changes.
	// Already-settled and zero rows cause no revision advance and need no write.
	publishSettlementNetEscrowSnapshots(ctx, tx, pending, positive, false)
}
