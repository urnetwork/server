// Deadline closure owns reconciliation through the terminal commit. The caller
// supplies the retirement clock; task admission alone checks the wall clock.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// An expiration close must finish with the existing contract closed and its
// best possible fair settlement committed. Accounting inconsistencies must
// be reconciled during the close; leaving the contract open or enqueueing
// settlement does not satisfy this requirement. See
// ReconcileContractAtDeadline for the reconciliation and retry guarantees.
// Keep the historical signature for stored task callers; no handoff is needed.
func CloseContractAtDeadline(ctx context.Context, contractId server.Id, deadline time.Time) (*ContractCloseOwner, error) {
	_, err := ReconcileContractAtDeadline(ctx, contractId, deadline)
	return nil, err
}

// Counts describe this invocation's committed financial changes. Original
// reports and actual escrow payouts remain the durable reconciliation record.
type ContractDeadlineReconciliation struct {
	ContractId    server.Id       `json:"contract_id"`
	Outcome       ContractOutcome `json:"outcome"`
	Requested     ByteCount       `json:"requested_bytes"`
	Charged       ByteCount       `json:"charged_bytes"`
	AlreadyClosed bool            `json:"already_closed,omitempty"`
	Missing       bool            `json:"missing,omitempty"`
	Adjustments   []string        `json:"adjustments,omitempty"`
}

// Grant values are read after sorted grant locks. Missing grants remain visible
// so every reservation can be retired, including orphaned legacy escrows.
type contractDeadlineEscrow struct {
	contractSettlementEscrow
	networkId     *server.Id
	available     ByteCount
	settled       bool
	priorPayout   *ByteCount
	redisReserved bool
	journal       *ByteCount
}

// Force an existing contract closed at expiration.
// Its required postcondition is a committed terminal outcome, including when
// accounting is incomplete or inconsistent. Make the fairest settlement that
// the retained reports, adjudication and available funds support: attempt normal
// settlement, deduct only the remaining available balance when funding is short,
// pay providers from the amount actually charged, and release unused escrow.
// Preserve the original evidence and make repeated closes financially idempotent.
// An accounting anomaly must not leave the contract open for another task to fix.
//
// The caller's deadline is authoritative, even ahead of the process clock; task
// admission owns the eligibility check. Contract, intent, debit, earnings, escrow
// metadata and outcome commit atomically. A database, ownership or cancellation
// failure that prevents that commit must return an explicit error for retry,
// never acknowledge an open contract as closed. Optional caches run after commit.
func ReconcileContractAtDeadline(ctx context.Context, contractId server.Id, deadline time.Time) (result *ContractDeadlineReconciliation, returnErr error) {
	if contractId == (server.Id{}) || deadline.IsZero() {
		return nil, fmt.Errorf("invalid scheduled contract close")
	}
	var posts []server.PostFunction
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, posts = reconcileContractAtDeadlineInTx(ctx, tx, contractId, deadline)
		}, server.TxReadCommitted, server.OptNoRetry())
		server.RunPosts(ctx, posts...)
	}, func(err error) { result, returnErr = nil, err })
	return
}

// Reuse the caller's transaction when an overdue intent already owns its rows.
// All errors abort that transaction; returned posts require a confirmed commit.
func reconcileContractAtDeadlineInTx(ctx context.Context, tx server.PgTx, contractId server.Id, deadline time.Time) (result *ContractDeadlineReconciliation, posts []server.PostFunction) {
	result = &ContractDeadlineReconciliation{ContractId: contractId}
	owner, err := readContractSettlementRowsInTx(ctx, tx, contractId)
	if errors.Is(err, pgx.ErrNoRows) {
		result.Missing = true
		return
	}
	server.Raise(err)
	if owner.priorOutcome != nil {
		result.Outcome, result.AlreadyClosed = *owner.priorOutcome, true
		return
	}
	if owner.participants.payerNetworkId == nil {
		resolved, _, resolveErr := readContractCloseOwnerInConn(ctx, tx, contractId)
		if resolveErr == nil && resolved.Kind == ContractCloseOwnerPayerNetwork {
			owner.participants.payerNetworkId = &resolved.Id
		} else if resolveErr != nil {
			if !errors.Is(resolveErr, errContractCloseOwnerUnresolved) {
				server.Raise(resolveErr)
			}
			result.Adjustments = append(result.Adjustments, "unresolved_payer")
		}
	}
	result.Outcome = ContractOutcomeSettled
	// Intent workers acquire intent then try the contract without waiting.
	// This path holds the contract, so it must never wait for that intent.
	err = tx.QueryRow(ctx, `SELECT outcome FROM legacy_settlement_intent WHERE contract_id=$1 FOR UPDATE NOWAIT`, contractId).Scan(&result.Outcome)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		server.Raise(err)
	}
	result.Requested, _ = deadlineReportAmounts(owner.reports, result.Outcome, &result.Adjustments)
	participants, originNetworkId, err := contractParticipantsFromOwnerInTx(ctx, tx, contractId, owner.participants, nil)
	if err != nil {
		// Conflicting identities cannot authorize payment. Keep original
		// rows and retire without guessing a recipient.
		result.Adjustments = append(result.Adjustments, "ambiguous_participants")
		participants = nil
	}
	balanceIds, err := contractTransferBalanceIdsInTx(ctx, tx, []server.Id{contractId})
	server.Raise(err)
	networkIds := []server.Id{}
	for _, participant := range participants {
		// No escrow means no earnings write. Same-network shares are also
		// ineligible, so neither case may compete for an account owner.
		if len(balanceIds) > 0 && participant.NetworkId != originNetworkId {
			networkIds = append(networkIds, participant.NetworkId)
		}
	}
	slices.SortFunc(networkIds, server.Id.Cmp)
	networkIds = slices.Compact(networkIds)
	keys := append(transferBalanceOwnershipKeys(balanceIds), accountBalanceOwnershipKeys(networkIds)...)
	if len(keys) > 0 {
		admitted, err := server.TryTxOwnership(ctx, tx, keys)
		server.Raise(err)
		if !admitted {
			server.Raise(errTransferBalanceOwnershipBusy)
		}
	}
	rows, err := tx.Query(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=ANY($1) ORDER BY balance_id FOR UPDATE`, balanceIds)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
		}
	})
	positive, redisReservations := lockSettlementReservations(ctx, tx, contractId, balanceIds)
	escrows := []contractDeadlineEscrow{}
	rows, err = tx.Query(ctx, `SELECT e.balance_id,e.balance_byte_count,COALESCE(b.start_balance_byte_count,0),
				COALESCE(b.net_revenue_nano_cents,0),b.network_id,
				GREATEST(0,COALESCE(b.balance_byte_count,0)::numeric-COALESCE((SELECT sum(debit_byte_count)
				FROM transfer_debit_journal j WHERE j.balance_id=e.balance_id AND NOT j.applied),0))::bigint,
				e.settled,e.payout_byte_count,e.redis_reserved,
				(SELECT debit_byte_count FROM transfer_debit_journal j WHERE j.balance_id=e.balance_id AND j.contract_id=e.contract_id)
				FROM transfer_escrow e LEFT JOIN transfer_balance b USING(balance_id)
				WHERE e.contract_id=$1 ORDER BY b.end_time NULLS LAST,e.balance_id`, contractId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var escrow contractDeadlineEscrow
			server.Raise(rows.Scan(&escrow.balanceId, &escrow.amount, &escrow.start, &escrow.revenue, &escrow.networkId,
				&escrow.available, &escrow.settled, &escrow.priorPayout, &escrow.redisReserved, &escrow.journal))
			escrows = append(escrows, escrow)
		}
	})
	fundedUsage := result.Requested
	if len(escrows) == 0 && owner.participants.payerNetworkId == nil {
		fundedUsage = 0
	}
	payouts := deadlineEscrowPayouts(fundedUsage, escrows, originNetworkId, len(participants) > 0, &result.Adjustments)
	participantPayouts, accountPayouts := allocateContractParticipantPayouts(participants, originNetworkId, payouts)
	usageParticipants, _, usageErr := contractParticipantsFromOwnerInTx(ctx, tx, contractId, owner.participants, owner.usageOriginIsSource)
	if usageErr != nil {
		usageParticipants = nil
	}
	usage := deadlineUsageSnapshot(owner, usageParticipants, &result.Adjustments)
	// Accepted adjudication survives. Retirement clears the dispute only
	// with its best funded settlement. The intent guard remains intact.
	server.RaisePgResult(tx.Exec(ctx, `DELETE FROM legacy_settlement_intent WHERE contract_id=$1`, contractId))
	server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=false,usage_unverified=true,
				expiration_time=LEAST(COALESCE(expiration_time,$2),$2) WHERE contract_id=$1`, contractId, deadline))
	claimed, err := claimContractOutcomeWithUsageValueInTx(ctx, tx, contractId, result.Outcome, usage, true)
	server.Raise(err)
	if !claimed {
		server.Raise(fmt.Errorf("deadline reconciliation did not close locked contract"))
	}
	for _, balanceId := range balanceIds {
		payout, exists := payouts[balanceId]
		if !exists {
			continue
		}
		if payout.payoutByteCount > 0 {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=balance_byte_count-$2 WHERE balance_id=$1`, balanceId, payout.payoutByteCount))
			result.Charged += payout.payoutByteCount
		}
	}
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		queueEscrowSettlementUpdates(batch, contractId, deadline, payouts)
		for key, payout := range participantPayouts {
			batch.Queue(participantSweepInsertSQL, contractId, key.balanceId, key.networkId,
				payout.payoutByteCount, payout.payout, payout.destinationId, payout.providerPayouts)
		}
		for _, networkId := range networkIds {
			if payout := accountPayouts[networkId]; payout != nil {
				batch.Queue(legacyProviderTotalWriteSql, networkId, payout.payoutByteCount, payout.payout)
			}
		}
	})
	// Outcome/metadata triggers invalidate the cache revision. A bad
	// optional snapshot must not block financial reconciliation.
	if len(positive) > 0 {
		ids := settlementReservationIds(positive)
		posts = append(posts, func() any { refreshNetEscrow(ctx, ids); return nil })
	}
	if len(redisReservations) > 0 {
		posts = append(posts, func() any { ReconcileRedisContractReservation(ctx, contractId); return nil })
	}
	_, clockBytes := deadlineReportAmounts(owner.reports, result.Outcome, nil)
	if clockBytes > 0 {
		posts = append(posts, legacySettlementClockPost(ctx, clockBytes))
	}
	if result.Charged > 0 {
		if payerId, _, _, err := contractOrigin(contractId, owner.participants, nil); err == nil {
			posts = append(posts, func() any { RecordClientDataUsage(ctx, payerId, result.Charged, deadline); return nil })
		}
	}
	posts = append(posts, legacySettlementStreamPost(ctx, contractId))
	return
}

// Match ordinary averaging and one-sided expiry billing. Invalid reports grant
// no authority; adjudication never invents its missing selected side.
func deadlineReportAmounts(reports []contractSettlementReport, outcome ContractOutcome, adjustments *[]string) (used, clock ByteCount) {
	counts := map[ContractParty]ByteCount{}
	for _, report := range reports {
		if report.byteCount < 0 || (report.party != ContractPartySource && report.party != ContractPartyDestination) {
			if adjustments != nil {
				*adjustments = append(*adjustments, "invalid_report")
			}
			continue
		}
		counts[report.party] = report.byteCount
	}
	source, sourceOk := counts[ContractPartySource]
	destination, destinationOk := counts[ContractPartyDestination]
	clock = destination
	switch outcome {
	case ContractOutcomeDisputeResolvedToSource:
		used = source
	case ContractOutcomeDisputeResolvedToDestination:
		used = destination
	default:
		switch {
		case sourceOk && destinationOk:
			used, _ = meanContractByteCount(source, destination)
		case sourceOk:
			used, clock = source, source
		case destinationOk:
			used = destination
		}
	}
	return
}

// Spend only this reservation and the remaining grant, after accepted journals.
// Missing or invalid grants pay zero. Prior consumption is never repeated.
func deadlineEscrowPayouts(used ByteCount, escrows []contractDeadlineEscrow, payerId server.Id, validParticipants bool, adjustments *[]string) map[server.Id]sweepPayout {
	payouts := map[server.Id]sweepPayout{}
	remaining := used
	var totalRevenue NanoCents
	// Account for prior consumption across the complete set before allocating
	// any new debit; an older untouched grant can precede an already paid one.
	for _, escrow := range escrows {
		if escrow.settled || escrow.journal != nil {
			prior := escrow.priorPayout
			if escrow.journal != nil {
				prior = escrow.journal
			}
			if prior != nil {
				remaining -= min(remaining, max(0, *prior))
			}
			*adjustments = append(*adjustments, "retained_prior_consumption")
		}
	}
	for _, escrow := range escrows {
		if escrow.settled || escrow.journal != nil {
			continue
		}
		count := min(remaining, max(0, escrow.amount), max(0, escrow.available))
		valid := escrow.networkId != nil && *escrow.networkId == payerId && escrow.start > 0 && escrow.revenue >= 0 && validParticipants
		if !valid {
			count = 0
			*adjustments = append(*adjustments, "unfunded_or_invalid_escrow")
		}
		var revenue NanoCents
		if count > 0 {
			value := math.Round(ProviderRevenueShare * float64(escrow.revenue) * float64(count) / float64(escrow.start))
			if value < 0 || math.IsInf(value, 0) || value >= float64(math.MaxInt64-totalRevenue) {
				count = 0
				*adjustments = append(*adjustments, "invalid_escrow_valuation")
			} else {
				revenue = NanoCents(value)
			}
		}
		remaining -= count
		totalRevenue += revenue
		payouts[escrow.balanceId] = sweepPayout{escrowBalanceByteCount: escrow.amount, payoutByteCount: count,
			returnByteCount: max(0, escrow.amount) - count, payout: revenue}
	}
	if remaining > 0 {
		*adjustments = append(*adjustments, "funding_shortfall")
	}
	return payouts
}

// Preserve immutable proof exactly. New credit requires both original reports
// and sound identities; billing fallback never authenticates a missing peer.
func deadlineUsageSnapshot(owner *contractSettlementOwner, participants []ContractParticipant, adjustments *[]string) any {
	if len(owner.retained) > 0 {
		if _, err := decodeContractUsageSnapshot(owner.retained); err != nil {
			*adjustments = append(*adjustments, "invalid_retained_usage")
		}
		return json.RawMessage(owner.retained)
	}
	snapshot := &contractUsageSnapshot{Version: 1, Providers: []contractProviderUsage{}, ExcludedReason: "expired_unconfirmed"}
	if owner.unverified || owner.usageOriginIsSource == nil {
		return snapshot
	}
	proof := &contractUsageExpiry{Capacity: owner.capacity, Reports: map[ContractParty]contractUsageClose{}}
	for _, report := range owner.reports {
		proof.Reports[report.party] = contractUsageClose{ByteCount: report.byteCount, Checkpoint: report.checkpoint}
	}
	count, err := contractExpiryCompletedUsage(proof)
	if err != nil {
		*adjustments = append(*adjustments, "invalid_usage_reports")
		return snapshot
	}
	if len(proof.Reports) < 2 {
		snapshot.Expiry = proof
		return snapshot
	}
	verified, err := newContractUsageSnapshot(count, participants)
	if err != nil {
		*adjustments = append(*adjustments, "invalid_usage_participants")
		return snapshot
	}
	verified.Expiry = proof
	return verified
}
