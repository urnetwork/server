package model

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// DESIGN.md 20.6's settle and bound rows for relayed sidecar contracts (relayed_contract_model.go;
// R-QUOTA-3, D-QUOTA-4, FS-45, X11): the settled count debits once, keyed by contract_id in
// service_debit_journal under meter relay, and the rest of the reservation returns.
//
//	shape    payer_network_id set to the network both ends are in, no transfer_escrow row, positive
//	         bytes and no companion origin: zero-escrow contracts set no payer, and escrow contracts
//	         hold escrow rows, so the shape names a relayed contract (plan-QUOTA-relayed-custody)
//	hook     the outcome claim (claimContractOutcomeWithUsageValueInTx), where an outcome is claimed
//	         exactly once, so each free close site settles it: the ordinary no-escrow close, the legacy
//	         no-escrow close and the deadline close with no escrow
//	count    the mean of both parties' close reports, or the one report a deadline close has, or the
//	         side a dispute resolves to (deadlineReportAmounts), at most the contract's grant; a
//	         contract bound to a stream counts nothing, since P2P bytes never count
//	debit    from the payer's balances in window at the contract's creation, in draw order, each up
//	         to its room, the rest overdrafting the last as an evs debit does (plan-QUOTA-small-readings
//	         #5); every such balance gets a row, a zero charge too, because the flush's release of the
//	         row's token, the contract id, is what returns the reservation if anything else is lost
//	recovery a token recovery enrolls is published while the contract is open and released once it is
//	         terminal (recoverRedisReservationWithFenceInTx), so reservations never confuse the contract
//	         reconciler (20.15)
//
// The rows are read in the claim's own transaction, from SQL alone: no Redis inside a transaction.
// So the balances debited are the ones in window at creation in draw order, not those whose token a
// read would find, and which balance pays may differ from which one reserved, as for evs's debits
// after use; the total debits once. With Redis lost the debit still lands.

// Whether a claimed contract's row has the relayed shape, before the escrow check.
func relayedContractCandidate(payerNetworkId *server.Id, sourceNetworkId server.Id, destinationNetworkId server.Id, transferByteCount ByteCount, companionContractId *server.Id) bool {
	return server.SidecarMg2Linked() &&
		payerNetworkId != nil &&
		*payerNetworkId == sourceNetworkId &&
		sourceNetworkId == destinationNetworkId &&
		0 < transferByteCount &&
		companionContractId == nil
}

// Whether the contract is relayed: its row has the shape and it holds no escrow. In the caller's
// transaction, which holds the contract row.
func isRelayedContractInTx(ctx context.Context, tx server.PgTx, contractId server.Id) (relayed bool, payerNetworkId server.Id, createTime time.Time) {
	if !server.SidecarMg2Linked() {
		return false, server.Id{}, time.Time{}
	}
	var payer *server.Id
	var sourceNetworkId, destinationNetworkId server.Id
	var transferByteCount ByteCount
	var companionContractId *server.Id
	var hasEscrow bool
	err := tx.QueryRow(
		ctx,
		`
			SELECT
				payer_network_id,
				source_network_id,
				destination_network_id,
				transfer_byte_count,
				companion_contract_id,
				create_time,
				EXISTS (SELECT 1 FROM transfer_escrow WHERE contract_id = $1)
			FROM transfer_contract
			WHERE contract_id = $1
		`,
		contractId,
	).Scan(&payer, &sourceNetworkId, &destinationNetworkId, &transferByteCount, &companionContractId, &createTime, &hasEscrow)
	if err != nil {
		return false, server.Id{}, time.Time{}
	}
	if hasEscrow || !relayedContractCandidate(payer, sourceNetworkId, destinationNetworkId, transferByteCount, companionContractId) {
		return false, server.Id{}, time.Time{}
	}
	return true, *payer, createTime
}

// Debits a relayed contract's settled count, once, in the transaction that claims its outcome.
// The caller has claimed the outcome in tx and holds the contract row. Rows already journaled for
// the contract are kept, so a replay journals nothing twice.
func SettleRelayedContractInTx(ctx context.Context, tx server.PgTx, contractId server.Id, outcome ContractOutcome) error {
	relayed, payerNetworkId, createTime := isRelayedContractInTx(ctx, tx, contractId)
	if !relayed {
		return errRelayedContractShape
	}
	return settleRelayedContractInTx(ctx, tx, contractId, payerNetworkId, createTime, outcome)
}

var errRelayedContractShape = errors.New("contract is not relayed")

func settleRelayedContractInTx(ctx context.Context, tx server.PgTx, contractId server.Id, payerNetworkId server.Id, createTime time.Time, outcome ContractOutcome) error {
	var transferByteCount ByteCount
	var streamBound bool
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT transfer_byte_count, stream_id IS NOT NULL FROM transfer_contract WHERE contract_id = $1`,
		contractId,
	).Scan(&transferByteCount, &streamBound))
	reports := []contractSettlementReport{}
	rows, err := tx.Query(ctx, `SELECT party, used_transfer_byte_count, checkpoint FROM contract_close WHERE contract_id = $1`, contractId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var report contractSettlementReport
			server.Raise(rows.Scan(&report.party, &report.byteCount, &report.checkpoint))
			reports = append(reports, report)
		}
	})
	settled, _ := deadlineReportAmounts(reports, outcome, nil)
	settled = min(max(0, settled), transferByteCount)
	if streamBound {
		// a contract later bound to a P2P stream returns all of it
		settled = 0
	}

	balances := LoadDrawOrderBalancesInTx(ctx, tx, payerNetworkId, createTime)
	if len(balances) == 0 {
		if 0 < settled {
			quotaUnbilledByteCounter.WithLabelValues(sidecar.QuotaMeterRelay).Add(float64(settled))
			glog.Infof("[quota][relayed]contract %s settled %d bytes with no in-window balance at creation; unbilled\n", contractId, settled)
		}
		return nil
	}
	debits := relayedContractDebits(balances, settled)
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for index, balance := range balances {
			batch.Queue(
				`
					INSERT INTO service_debit_journal (usage_id, balance_id, meter, debit_byte_count, batch_id)
					VALUES ($1, $2, $3, $4, NULL)
					ON CONFLICT (balance_id, usage_id) DO NOTHING
				`,
				contractId,
				balance.BalanceId,
				sidecar.QuotaMeterRelay,
				debits[index],
			)
		}
	})
	return nil
}

// The settled count over the balances in draw order: each up to its room, the rest on the last.
// One entry per balance, zero included.
func relayedContractDebits(balances []*DrawOrderBalance, settled ByteCount) []ByteCount {
	debits := make([]ByteCount, len(balances))
	remaining := settled
	for index, balance := range balances {
		take := min(remaining, balance.RoomByteCount())
		debits[index] = take
		remaining -= take
	}
	if 0 < remaining && 0 < len(debits) {
		debits[len(debits)-1] += remaining
	}
	return debits
}

// Returns a relayed contract's whole reservation once it is bound to a stream, since P2P bytes never
// count: releases the contract's token on each of the payer's balances in window at its creation.
// Outside any transaction. A release that fails leaves the token to the close's journaled zero rows
// or its 24 h lease.
func releaseRelayedContractReservations(ctx context.Context, contractId server.Id, payerNetworkId server.Id, createTime time.Time) {
	balances := LoadDrawOrderBalances(ctx, payerNetworkId, createTime)
	for _, balance := range balances {
		if _, err := redisContractReservation(ctx, "release", balance.BalanceId, contractId, 0, 0, redisContractReservationLease, server.OptRedisKeepLateReplies()); err != nil {
			glog.Infof("[quota][relayed]contract %s bound to a stream; release on %s failed: %s\n", contractId, balance.BalanceId, err)
		}
	}
}
