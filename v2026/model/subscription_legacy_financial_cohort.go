// A bounded cohort amortizes financial ownership and protocol waits without
// sharing reports, rounding, payout authority or replay fences across contracts.
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
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

const legacyFinancialCohortLimit = 8
const legacyFinancialCohortTimeout = time.Second
const legacyFinancialCohortEscrowLimit = 64
const legacyFinancialCohortParticipantLimit = 256

var errLegacyFinancialCohortUnsupported = errors.New("legacy financial cohort requires individual settlement")

// A fallback has made no financial change for that contract. A successful
// cohort may precede it, so callers consume only this returned input prefix.
type legacyFinancialCohortAttempt struct {
	contractId             server.Id
	completed              bool
	busy                   bool
	busyGate               legacySettlementBusyGate
	fallback               bool
	deadlineFallback       bool
	financialWriteRollback bool
}

// Every field is scoped to one transaction. Membership and grant data are read
// after ownership; neither a prior page nor Redis supplies financial evidence.
type legacyFinancialCohortContract struct {
	contractId          server.Id
	outcome             ContractOutcome
	clearDispute        bool
	owner               contractSettlementOwner
	expectedBalanceIds  []server.Id
	positive            map[server.Id]ByteCount
	escrows             []contractSettlementEscrow
	billingParticipants []ContractParticipant
	usageParticipants   []ContractParticipant
	sweepPayouts        map[server.Id]sweepPayout
	participantPayouts  map[participantSweepKey]*participantSweepPayout
	accountPayouts      map[server.Id]*contractPayout
	usage               *contractUsageSnapshot
	clockByteCount      ByteCount
	closedAt            time.Time
	fallback            bool
}

// No hint lease is needed: one bounded set of nonwaiting PostgreSQL probes
// supplies every admitted contract's authority. Heads keep their individual
// wait policy. Optional hint state is never credited with an unobserved probe.
func flushLegacySettlementCohort(ctx context.Context, contractIds []server.Id) (attempts []legacyFinancialCohortAttempt, returnErr error) {
	if len(contractIds) < 2 || len(contractIds) > legacyFinancialCohortLimit {
		return nil, fmt.Errorf("invalid legacy financial cohort size: %d", len(contractIds))
	}
	bounded, cancel := context.WithTimeout(ctx, legacyFinancialCohortTimeout)
	defer cancel()
	bounded = withLegacyFinancialCohortBudget(bounded)
	diagnostic := newLegacyFinancialDiagnostic(bounded, "cohort")
	defer diagnostic.finish()
	bounded.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget).diagnostic = diagnostic
	databaseTiming, observeDatabase := observeLegacyFinancialCohort(ctx)
	finishFinancial := enterLegacySettlementTiming(ctx, legacySettlementFinancial)
	var posts []func() any
	traced := map[server.Id]context.Context{}
	if trace, _ := ctx.Value(legacyTargetTraceKey{}).(*legacyTargetTrace); trace != nil {
		for _, id := range contractIds {
			traced[id] = trace.selectTarget(ctx, id, false)
			traceLegacySettlement(traced[id], "cohort_financial", "entered")
		}
	}
	bodyReturned := false
	server.HandleError(func() {
		server.Tx(bounded, func(tx server.PgTx) {
			diagnostic.bind(tx)
			server.Raise(checkLegacyFinancialCohortBudget(bounded, "setup"))
			server.RaisePgResult(tx.Exec(bounded, `SET LOCAL statement_timeout='500ms'; SET LOCAL lock_timeout='250ms'`))
			var err error
			attempts, posts, err = flushLegacySettlementCohortInTx(bounded, tx, contractIds)
			server.Raise(err)
			bodyReturned = true
		}, server.TxReadCommitted, server.OptNoRetry(), databaseTiming)
	}, func(err error) { returnErr = err })
	diagnostic.finish()
	observeDatabase()
	finishFinancial()
	if returnErr != nil {
		attempts = nil
		for _, callCtx := range traced {
			traceLegacySettlement(callCtx, "cohort_financial", legacyTargetTraceCause(returnErr))
		}
		// This branch is before any commit attempt: Tx has joined rollback.
		// A lost acknowledgement after body return is never replayed here.
		if !bodyReturned && ctx.Err() == nil {
			budget, _ := bounded.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget)
			return []legacyFinancialCohortAttempt{{contractId: contractIds[0], fallback: true,
				financialWriteRollback: budget != nil && budget.written,
				deadlineFallback:       errors.Is(returnErr, errLegacyFinancialCohortBudget) || legacyFinancialCohortDeadlineFallback(ctx, bounded, returnErr)}}, nil
		}
		return nil, returnErr
	}
	for _, attempt := range attempts {
		if callCtx := traced[attempt.contractId]; callCtx != nil {
			if attempt.fallback {
				traceLegacySettlement(callCtx, "cohort_financial", "individual_fallback")
			} else {
				traceLegacySettlement(callCtx, "commit", "confirmed_cohort_tx_return")
				traceLegacySettlementResult(callCtx, attempt.completed, attempt.busy, attempt.busyGate, nil)
			}
		}
	}
	server.RunPosts(context.WithoutCancel(ctx), posts...)
	return
}

// Lock intents before contracts, using nonwaiting probes at both boundaries.
// Ordinary foreground queueing takes the inverse pair, so waiting here could
// deadlock it. Grants are then acquired once in global balance-id order.
func flushLegacySettlementCohortInTx(ctx context.Context, tx server.PgTx, contractIds []server.Id) (attempts []legacyFinancialCohortAttempt, posts []func() any, returnErr error) {
	if len(contractIds) < 2 || len(contractIds) > legacyFinancialCohortLimit {
		return nil, nil, errLegacyFinancialCohortUnsupported
	}
	sortedIds := slices.Clone(contractIds)
	slices.SortFunc(sortedIds, server.Id.Cmp)
	if len(slices.Compact(slices.Clone(sortedIds))) != len(sortedIds) {
		return nil, nil, errLegacyFinancialCohortUnsupported
	}
	contracts := map[server.Id]*legacyFinancialCohortContract{}
	if err := checkLegacyFinancialCohortBudget(ctx, "intents"); err != nil {
		return nil, nil, err
	}
	rows, err := queryLegacyFinancialCohort(ctx, tx, `SELECT owned.contract_id,owned.outcome,owned.clear_dispute
        FROM (SELECT unnest($1::uuid[]) AS contract_id ORDER BY contract_id OFFSET 0) AS requested
        CROSS JOIN LATERAL (SELECT contract_id,outcome,clear_dispute FROM legacy_settlement_intent
        WHERE contract_id=requested.contract_id OFFSET 0 FOR UPDATE SKIP LOCKED) AS owned`, sortedIds)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			contract := &legacyFinancialCohortContract{positive: map[server.Id]ByteCount{}}
			server.Raise(rows.Scan(&contract.contractId, &contract.outcome, &contract.clearDispute))
			contracts[contract.contractId] = contract
		}
	})
	ownedIds := make([]server.Id, 0, len(contracts))
	for _, id := range sortedIds {
		if contracts[id] != nil {
			ownedIds = append(ownedIds, id)
		}
	}
	lockedContracts := map[server.Id]bool{}
	if len(ownedIds) > 0 {
		if err := checkLegacyFinancialCohortBudget(ctx, "headers"); err != nil {
			return nil, nil, err
		}
		rows, err = queryLegacyFinancialCohort(ctx, tx, `SELECT owned.contract_id,owned.source_network_id,owned.source_id,
            owned.destination_network_id,owned.destination_id,owned.payer_network_id,
            owned.companion_contract_id,owned.stream_id,owned.usage_origin_is_source,owned.outcome,
            owned.transfer_byte_count,owned.usage_unverified,owned.provider_usage,owned.expiration_time
            FROM (SELECT unnest($1::uuid[]) AS contract_id ORDER BY contract_id OFFSET 0) AS requested
            CROSS JOIN LATERAL (SELECT contract_id,source_network_id,source_id,destination_network_id,destination_id,
            payer_network_id,companion_contract_id,stream_id,usage_origin_is_source,outcome,
            transfer_byte_count,usage_unverified,provider_usage,expiration_time FROM transfer_contract
            WHERE contract_id=requested.contract_id OFFSET 0 FOR UPDATE SKIP LOCKED) AS owned`, ownedIds)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				var owner contractSettlementOwner
				var expirationTime *time.Time
				server.Raise(rows.Scan(&id, &owner.participants.sourceNetworkId, &owner.participants.sourceId,
					&owner.participants.destinationNetworkId, &owner.participants.destinationId,
					&owner.participants.payerNetworkId, &owner.participants.companionContractId, &owner.participants.streamId,
					&owner.usageOriginIsSource, &owner.priorOutcome, &owner.capacity, &owner.unverified, &owner.retained, &expirationTime))
				contracts[id].owner = owner
				contracts[id].fallback = owner.priorOutcome != nil || expirationTime != nil && !server.NowUtc().Before(*expirationTime)
				lockedContracts[id] = true
			}
		})
	}
	// The common selector validates each locked contract before shared grant
	// ownership. A stale hint falls back to the individual rekey transaction.
	for _, id := range ownedIds {
		if lockedContracts[id] && !contracts[id].fallback {
			if err := checkLegacyFinancialCohortBudget(ctx, "owner"); err != nil {
				return nil, nil, err
			}
			closeOwner, admitted, err := validateLegacyCloseOwnerHeaderInTx(ctx, tx, id, contracts[id].owner.participants.sourceId, contracts[id].owner.participants.payerNetworkId, nil)
			if err != nil {
				return nil, nil, err
			}
			if !admitted || closeOwner.Kind == ContractCloseOwnerSourceClient {
				contracts[id].fallback = true
			} else if contracts[id].owner.participants.payerNetworkId == nil {
				// Billing consumes the same verified financial payer as routing.
				// Only this transaction-local header changes; original metadata
				// and the independent usage direction remain retained as written.
				contracts[id].owner.participants.payerNetworkId = &closeOwner.Id
			}
		}
	}
	ownedIds = ownedIds[:0]
	for _, id := range sortedIds {
		if lockedContracts[id] && !contracts[id].fallback {
			ownedIds = append(ownedIds, id)
		}
	}
	balanceIdSet := map[server.Id]bool{}
	ownershipBalanceIdSet := map[server.Id]bool{}
	if len(ownedIds) > 0 {
		if err := checkLegacyFinancialCohortBudget(ctx, "membership"); err != nil {
			return nil, nil, err
		}
		// Missing grants still have escrow revision ownership, but do not
		// become financial membership. Bound each exact contract seek before
		// its join so a dangling history cannot expand before the sentinel.
		rows, err = queryLegacyFinancialCohort(ctx, tx, `SELECT requested.contract_id,escrow.balance_id,balance.balance_id IS NOT NULL
            FROM unnest($1::uuid[]) AS requested(contract_id)
            CROSS JOIN LATERAL (SELECT balance_id FROM transfer_escrow
                WHERE contract_id=requested.contract_id ORDER BY balance_id LIMIT $2 OFFSET 0) AS escrow
            LEFT JOIN LATERAL (SELECT balance_id FROM transfer_balance
                WHERE balance_id=escrow.balance_id OFFSET 0) AS balance ON true LIMIT $2`, ownedIds, legacyFinancialCohortEscrowLimit+1)
		escrowRows := 0
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				escrowRows++
				var id, balanceId server.Id
				var member bool
				server.Raise(rows.Scan(&id, &balanceId, &member))
				ownershipBalanceIdSet[balanceId] = true
				if member {
					contracts[id].expectedBalanceIds = append(contracts[id].expectedBalanceIds, balanceId)
					balanceIdSet[balanceId] = true
				}
			}
		})
		if escrowRows > legacyFinancialCohortEscrowLimit {
			return nil, nil, errLegacyFinancialCohortUnsupported
		}
	}
	ownershipBalanceIds := make([]server.Id, 0, len(ownershipBalanceIdSet))
	for id := range ownershipBalanceIdSet {
		ownershipBalanceIds = append(ownershipBalanceIds, id)
	}
	// Without an owned financial contract there is no shared key set to
	// admit. Preserve the individual private-row busy reasons below.
	if len(ownedIds) > 0 {
		if err := checkLegacyFinancialCohortBudget(ctx, "ownership"); err != nil {
			return nil, nil, err
		}
		ownershipAdmitted, err := server.TryTxOwnershipExec(ctx, tx, legacyFinancialOwnershipKeys(ownedIds, ownershipBalanceIds))
		if err != nil {
			return nil, nil, err
		}
		if !ownershipAdmitted {
			// No shared financial statement has entered. Keep every durable
			// intent and yield this bounded payer cohort to its current owner.
			for _, id := range contractIds {
				attempts = append(attempts, legacyFinancialCohortAttempt{
					contractId: id, busy: true, busyGate: legacySettlementBusyAdmission,
				})
			}
			return attempts, nil, nil
		}
	}
	balanceIds := make([]server.Id, 0, len(balanceIdSet))
	for id := range balanceIdSet {
		balanceIds = append(balanceIds, id)
	}
	if len(balanceIds) > legacyFinancialCohortEscrowLimit {
		return nil, nil, errLegacyFinancialCohortUnsupported
	}
	slices.SortFunc(balanceIds, server.Id.Cmp)
	lockedBalanceIdSet := map[server.Id]bool{}
	if len(balanceIds) > 0 {
		if err := checkLegacyFinancialCohortBudget(ctx, "grant_rows"); err != nil {
			return nil, nil, err
		}
		rows, err = queryLegacyFinancialCohort(ctx, tx, `SELECT owned.balance_id FROM (SELECT unnest($1::uuid[]) AS balance_id ORDER BY balance_id OFFSET 0) AS requested
            CROSS JOIN LATERAL (SELECT balance_id FROM transfer_balance
            WHERE balance_id=requested.balance_id OFFSET 0 FOR UPDATE SKIP LOCKED) AS owned`, balanceIds)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				lockedBalanceIdSet[id] = true
			}
		})
	}
	grantBusyIds := map[server.Id]bool{}
	readyIds := make([]server.Id, 0, len(ownedIds))
	for _, id := range ownedIds {
		for _, balanceId := range contracts[id].expectedBalanceIds {
			if !lockedBalanceIdSet[balanceId] {
				grantBusyIds[id] = true
			}
		}
		if !grantBusyIds[id] {
			readyIds = append(readyIds, id)
		}
	}
	// Complete tuple ownership before taking separate Read Committed report,
	// membership, joined-escrow and cache snapshots. No locking snapshot is
	// reused as the financial amounts snapshot.
	if len(readyIds) > 0 {
		if err := checkLegacyFinancialCohortBudget(ctx, "escrow_rows"); err != nil {
			return nil, nil, err
		}
		rows, err = queryLegacyFinancialCohort(ctx, tx, `SELECT requested.contract_id,owned.balance_id,owned.balance_byte_count,
            owned.settled,owned.redis_reserved
            FROM unnest($1::uuid[]) AS requested(contract_id)
            CROSS JOIN LATERAL (SELECT balance_id,balance_byte_count,settled,redis_reserved
            FROM transfer_escrow WHERE contract_id=requested.contract_id
            ORDER BY balance_id OFFSET 0 FOR UPDATE) AS owned LIMIT $2`, readyIds, legacyFinancialCohortEscrowLimit+1)
		count := 0
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				count++
				var id, balanceId server.Id
				var amount ByteCount
				var settled, redisReserved bool
				server.Raise(rows.Scan(&id, &balanceId, &amount, &settled, &redisReserved))
				contract := contracts[id]
				if !lockedBalanceIdSet[balanceId] {
					// A missing joined grant is harmless; a newly joined grant
					// is detected by the following fresh joined read.
					continue
				}
				if amount <= 0 || settled || redisReserved {
					contract.fallback = true
					continue
				}
				contract.positive[balanceId] = amount
			}
		})
		if count > legacyFinancialCohortEscrowLimit {
			return nil, nil, errLegacyFinancialCohortUnsupported
		}
	}
	cache := map[server.Id]netEscrowSnapshot{}
	if len(readyIds) > 0 {
		if err := checkLegacyFinancialCohortBudget(ctx, "financial_reads"); err != nil {
			return nil, nil, err
		}
		batch := &pgx.Batch{}
		batch.Queue(`SELECT requested.contract_id,report.party,report.used_transfer_byte_count,report.checkpoint
            FROM unnest($1::uuid[]) AS requested(contract_id)
            CROSS JOIN LATERAL (SELECT party,used_transfer_byte_count,checkpoint FROM contract_close
            WHERE contract_id=requested.contract_id OFFSET 0) AS report`, readyIds).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var id server.Id
				var report contractSettlementReport
				if err := rows.Scan(&id, &report.party, &report.byteCount, &report.checkpoint); err != nil {
					return err
				}
				contracts[id].owner.reports = append(contracts[id].owner.reports, report)
			}
			return rows.Err()
		})
		batch.Queue(`SELECT requested.contract_id,escrow.balance_id,escrow.balance_byte_count,
            balance.start_balance_byte_count,balance.net_revenue_nano_cents
            FROM unnest($1::uuid[]) AS requested(contract_id)
            CROSS JOIN LATERAL (SELECT balance_id,balance_byte_count FROM transfer_escrow
            WHERE contract_id=requested.contract_id OFFSET 0) AS escrow
            CROSS JOIN LATERAL (SELECT start_balance_byte_count,net_revenue_nano_cents,end_time FROM transfer_balance
            WHERE balance_id=escrow.balance_id OFFSET 0) AS balance
            ORDER BY requested.contract_id,balance.end_time LIMIT $2`, readyIds, legacyFinancialCohortEscrowLimit+1).Query(func(rows pgx.Rows) error {
			count := 0
			for rows.Next() {
				count++
				var id server.Id
				var escrow contractSettlementEscrow
				if err := rows.Scan(&id, &escrow.balanceId, &escrow.amount, &escrow.start, &escrow.revenue); err != nil {
					return err
				}
				contract := contracts[id]
				if !lockedBalanceIdSet[escrow.balanceId] || contract.positive[escrow.balanceId] != escrow.amount {
					contract.fallback = true
				}
				contract.escrows = append(contract.escrows, escrow)
			}
			if count > legacyFinancialCohortEscrowLimit {
				return errLegacyFinancialCohortUnsupported
			}
			return rows.Err()
		})
		// Keep billing and usage membership as successive fresh statements.
		// Shared stream mutations are not protected by one contract's lock.
		for _, usage := range []bool{false, true} {
			batch.Queue(`SELECT requested.contract_id,member.client_id,member.network_id
                FROM unnest($1::uuid[]) AS requested(contract_id)
                CROSS JOIN LATERAL (
                    SELECT participant.client_id,participant.network_id FROM transfer_contract AS contract
                    JOIN contract_participant AS participant USING(stream_id) WHERE contract.contract_id=requested.contract_id
                    UNION ALL SELECT client_id,network_id FROM contract_extender WHERE contract_id=requested.contract_id
                ) AS member LIMIT $2`, readyIds, legacyFinancialCohortParticipantLimit+1).Query(func(rows pgx.Rows) error {
				count := 0
				for rows.Next() {
					count++
					var id server.Id
					var participant ContractParticipant
					if err := rows.Scan(&id, &participant.ClientId, &participant.NetworkId); err != nil {
						return err
					}
					if usage {
						contracts[id].usageParticipants = append(contracts[id].usageParticipants, participant)
					} else {
						contracts[id].billingParticipants = append(contracts[id].billingParticipants, participant)
					}
				}
				if count > legacyFinancialCohortParticipantLimit {
					return errLegacyFinancialCohortUnsupported
				}
				return rows.Err()
			})
		}
		cacheIds := make([]server.Id, 0, len(lockedBalanceIdSet))
		for _, id := range balanceIds {
			if lockedBalanceIdSet[id] {
				cacheIds = append(cacheIds, id)
			}
		}
		batch.Queue(netEscrowAdmissionCacheSQL, cacheIds).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var id server.Id
				var snapshot netEscrowSnapshot
				var amount *int64
				if err := rows.Scan(&id, &snapshot.revision, &amount, &snapshot.endTime); err != nil {
					return err
				}
				if snapshot.revision < 0 || amount != nil && *amount < 0 {
					return fmt.Errorf("invalid cohort admission snapshot")
				}
				if amount != nil && snapshot.endTime != nil {
					snapshot.reserved = ByteCount(*amount)
					cache[id] = snapshot
				}
			}
			return rows.Err()
		})
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return nil, nil, err
		}
	}
	admitted := make([]*legacyFinancialCohortContract, 0, len(contractIds))
	// Only a chronological prefix can commit before individual accounting or
	// compatibility handling. Busy rows retain their own ownership reason.
	for _, id := range contractIds {
		attempt := legacyFinancialCohortAttempt{contractId: id}
		contract := contracts[id]
		switch {
		case contract == nil:
			attempt.busy = true
			attempt.busyGate = legacySettlementBusyIntent
		case !lockedContracts[id]:
			attempt.busy = true
			attempt.busyGate = legacySettlementBusyContract
		case grantBusyIds[id]:
			attempt.busy = true
			attempt.busyGate = legacySettlementBusyGrantSet
		default:
			if contract.fallback || len(contract.escrows) == 0 || len(contract.positive) != len(contract.escrows) {
				attempt.fallback = true
			} else if err := contract.prepare(); err != nil {
				attempt.fallback = true
			}
			if !attempt.fallback {
				admitted = append(admitted, contract)
				attempt.completed = true
			}
		}
		attempts = append(attempts, attempt)
		if attempt.fallback {
			break
		}
	}
	if len(admitted) == 0 {
		return attempts, nil, nil
	}
	posts, returnErr = writeLegacyFinancialCohortInTx(ctx, tx, admitted, cache)
	return
}

// Each contract is funded and rounded separately before any aggregate debit.
// Insufficient own escrow never borrows another member's unused reservation.
func (self *legacyFinancialCohortContract) prepare() error {
	reports := self.owner.reports
	// Retained usage proof cannot replace the report continuation. Refuse
	// partials before shared writes so their individual expiry owner can act.
	closes := map[ContractParty]contractUsageClose{}
	for _, report := range reports {
		closes[report.party] = contractUsageClose{ByteCount: report.byteCount, Checkpoint: report.checkpoint}
	}
	count, err := contractCompletedUsage(self.outcome, self.owner.capacity, closes)
	if err != nil {
		return err
	}
	used, clock, err := contractSettlementReportAmounts(reports, self.outcome)
	if err != nil {
		return err
	}
	self.clockByteCount = clock
	self.sweepPayouts, err = contractSettlementSweepPayouts(used, self.escrows)
	if err != nil {
		return err
	}

	participants, origin, err := contractParticipantsFromRows(self.contractId, self.owner.participants, nil, self.billingParticipants)
	if err != nil {
		return err
	}
	self.participantPayouts, self.accountPayouts = allocateContractParticipantPayouts(participants, origin, self.sweepPayouts)
	if self.owner.unverified {
		self.usage, err = retainedContractExpiryUsage(self.owner.retained)
		return err
	}
	if self.owner.usageOriginIsSource == nil {
		// New outcomes require immutable usage. The individual owner may
		// retain an expiry exclusion; no healthy cohort write precedes it.
		return errLegacyFinancialCohortUnsupported
	}
	participants, _, err = contractParticipantsFromRows(self.contractId, self.owner.participants, self.owner.usageOriginIsSource, self.usageParticipants)
	if err != nil {
		return err
	}
	self.usage, err = newContractUsageSnapshot(count, participants)
	return err
}

// All outcome guards are checked before financial writes are queued. Every
// failed query rolls the whole cohort back; optional posts are registered only
// with its real transaction owner and remain outside grant ownership.
func writeLegacyFinancialCohortInTx(ctx context.Context, tx server.PgTx, contracts []*legacyFinancialCohortContract, cache map[server.Id]netEscrowSnapshot) (posts []func() any, returnErr error) {
	if err := checkLegacyFinancialCohortBudget(ctx, "outcomes"); err != nil {
		return nil, err
	}
	batch := &pgx.Batch{}
	if err := queueLegacyFinancialCohortOutcomes(batch, contracts); err != nil {
		return nil, err
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return nil, err
	}
	if budget, _ := ctx.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget); budget != nil {
		budget.written = true
	}
	if err := checkLegacyFinancialCohortBudget(ctx, "post_outcomes"); err != nil {
		return nil, err
	}
	debitByBalance := map[server.Id]ByteCount{}
	mirrorBalanceIdSet := map[server.Id]bool{}
	for _, contract := range contracts {
		for balanceId, payout := range contract.sweepPayouts {
			if payout.payoutByteCount > math.MaxInt64-debitByBalance[balanceId] {
				return nil, fmt.Errorf("cohort debit overflow")
			}
			debitByBalance[balanceId] += payout.payoutByteCount
		}
		for balanceId, amount := range contract.positive {
			mirrorBalanceIdSet[balanceId] = true
			if snapshot, present := cache[balanceId]; present {
				if snapshot.reserved < amount {
					return nil, fmt.Errorf("cohort reservation snapshot underflow or overflow")
				}
				snapshot.reserved -= amount
				cache[balanceId] = snapshot
			}
		}
	}
	balances := make([]server.Id, 0, len(mirrorBalanceIdSet))
	for id := range mirrorBalanceIdSet {
		balances = append(balances, id)
	}
	slices.SortFunc(balances, server.Id.Cmp)
	batch = &pgx.Batch{}
	for _, id := range balances {
		if amount := debitByBalance[id]; amount > 0 {
			batch.Queue(`UPDATE transfer_balance SET balance_byte_count=balance_byte_count-$2 WHERE balance_id=$1`, id, amount)
		}
	}
	queueLegacyFinancialCohortMetadata(batch, contracts)
	cacheIds := make([]server.Id, 0, len(balances))
	for _, id := range balances {
		if snapshot, present := cache[id]; present {
			// Each of the two ordered statement triggers advances this
			// distinct grant once, regardless of its contract count.
			if snapshot.revision > math.MaxInt64-2 {
				return nil, fmt.Errorf("cohort reservation revision overflow")
			}
			snapshot.revision += 2
			cache[id] = snapshot
			cacheIds = append(cacheIds, id)
		}
	}
	if len(cacheIds) > 0 {
		batch.Queue(netEscrowPublishAdmissionCacheSQL, netEscrowAdmissionCacheArgs(cache, cacheIds)...)
	}
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	if err := queueLegacyFinancialCohortPayouts(batch, contracts); err != nil {
		return nil, err
	}
	var required []task.RequiredTaskBatchItem[json.RawMessage]
	for _, contract := range contracts {
		if len(contract.accountPayouts) > 0 {
			payload := legacyProviderTotalsPayload{Private: true, Version: 1, ContractId: contract.contractId}
			for id, payout := range contract.accountPayouts {
				payload.Totals = append(payload.Totals, legacyProviderTotal{NetworkId: id, Bytes: payout.payoutByteCount, Revenue: payout.payout})
			}
			slices.SortFunc(payload.Totals, func(a, b legacyProviderTotal) int { return a.NetworkId.Cmp(b.NetworkId) })
			data, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			required = append(required, task.RequiredTaskBatchItem[json.RawMessage]{Args: json.RawMessage(data),
				RunOnce: task.RunOnce("legacy_provider_totals", contract.contractId)})
		}
	}
	if len(required) > 0 {
		task.QueueRequiredTasksInBatch(tx, batch, ApplyLegacyProviderTotals, required, owner, task.MaxTime(10*time.Second), task.RequireQueueOwnership(tx))
	}
	for _, id := range balances {
		data, err := json.Marshal(legacyNetEscrowMirrorPayload{Private: true, Version: 1, BalanceId: id})
		if err != nil {
			return nil, err
		}
		task.QueueTaskInBatch(tx, batch, ApplyLegacyNetEscrowMirror, json.RawMessage(data), owner,
			task.RunOnce("legacy_net_escrow_mirror", id), task.MaxTime(netEscrowMirrorTimeout), task.RequireQueueOwnership(tx))
	}
	if err := checkLegacyFinancialCohortBudget(ctx, "financial_writes"); err != nil {
		return nil, err
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return nil, err
	}
	// Signing continues to use the exact retained original evidence. It is
	// optional provenance, never reconstructed from another contract's plan.
	signedCtx := providerWorkSessionContext(ctx)
	for _, contract := range contracts {
		if err := checkLegacyFinancialCohortBudget(ctx, "provenance"); err != nil {
			return nil, err
		}
		server.AddTxCommitCount(tx, &contractClosedCounter, 1)
		providerWorkRetainOutcomeInTx(signedCtx, tx, contract.contractId, contract.outcome, contract.closedAt)
		if err := checkLegacyFinancialCohortBudget(ctx, "post_provenance"); err != nil {
			return nil, err
		}
		contractHoleEventInTx(ctx, tx, contract.contractId, contract.owner.participants.sourceId, contract.owner.participants.destinationId, "remove")
		if contract.clockByteCount > 0 {
			posts = append(posts, legacySettlementClockPost(ctx, contract.clockByteCount))
		}
		posts = append(posts, legacySettlementStreamPost(ctx, contract.contractId))
	}
	posts = append(posts, legacySettlementMirrorPost(ctx, balances))
	return posts, nil
}
