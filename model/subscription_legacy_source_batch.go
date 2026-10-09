// Free source closes share bounded protocol exchanges without entering debit,
// escrow or payout work. Every contract retains its own outcome and usage.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// The page consumes this same chronological attempt prefix as paid cohorts.
// Read-only refusal returns to the individual owner; an attempted write never
// retries here. The parent/page deadline always bounds the shared child budget.
func flushLegacySourceSettlementBatch(ctx context.Context, contractIds []server.Id) (attempts []legacyFinancialCohortAttempt, returnErr error) {
	owner, scoped := ctx.Value(legacySettlementCloseScopeKey{}).(ContractCloseOwner)
	if !scoped || owner.Kind != ContractCloseOwnerSourceClient || !owner.valid() ||
		len(contractIds) < 2 || len(contractIds) > legacyFinancialCohortLimit {
		return nil, fmt.Errorf("invalid legacy source batch scope or size")
	}
	bounded, cancel := context.WithTimeout(ctx, legacyFinancialCohortTimeout)
	defer cancel()
	bounded = withLegacyFinancialCohortBudget(bounded)
	budget := bounded.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget)
	diagnostic := newLegacyFinancialDiagnostic(bounded, "source_batch")
	defer diagnostic.finish()
	budget.diagnostic = diagnostic
	finishFinancial := enterLegacySettlementTiming(ctx, legacySettlementFinancial)
	var posts []func() any
	traced := map[server.Id]context.Context{}
	if trace, _ := ctx.Value(legacyTargetTraceKey{}).(*legacyTargetTrace); trace != nil {
		for _, id := range contractIds {
			traced[id] = trace.selectTarget(ctx, id, false)
			traceLegacySettlement(traced[id], "source_batch", "entered")
		}
	}
	bodyReturned := false
	server.HandleError(func() {
		server.Tx(bounded, func(tx server.PgTx) {
			diagnostic.bind(tx)
			server.Raise(checkLegacyFinancialCohortBudget(bounded, "setup"))
			server.RaisePgResult(tx.Exec(bounded, `SET LOCAL statement_timeout='500ms'; SET LOCAL lock_timeout='250ms'`))
			var err error
			attempts, posts, err = flushLegacySourceSettlementBatchInTx(bounded, tx, contractIds)
			server.Raise(err)
			bodyReturned = true
		}, server.TxReadCommitted, server.OptNoRetry())
	}, func(err error) { returnErr = err })
	diagnostic.finish()
	finishFinancial()
	if returnErr != nil {
		attempts = nil
		for _, callCtx := range traced {
			traceLegacySettlement(callCtx, "source_batch", legacyTargetTraceCause(returnErr))
		}
		if !bodyReturned && !budget.written && ctx.Err() == nil {
			return []legacyFinancialCohortAttempt{{contractId: contractIds[0], fallback: true,
				deadlineFallback: errors.Is(returnErr, errLegacyFinancialCohortBudget) || legacyFinancialCohortDeadlineFallback(ctx, bounded, returnErr)}}, nil
		}
		return nil, returnErr
	}
	for _, attempt := range attempts {
		if callCtx := traced[attempt.contractId]; callCtx != nil {
			if attempt.fallback {
				traceLegacySettlement(callCtx, "source_batch", "individual_fallback")
			} else {
				traceLegacySettlement(callCtx, "commit", "confirmed_source_batch_tx_return")
				traceLegacySettlementResult(callCtx, attempt.completed, attempt.busy, attempt.busyGate, nil)
			}
		}
	}
	server.RunPosts(context.WithoutCancel(ctx), posts...)
	return
}

// Intent and header locks are nonwaiting and sorted. Escrow/report/participant
// reads use later Read Committed snapshots after header ownership; neither the
// page hint nor a live client lookup can establish source identity or free work.
func flushLegacySourceSettlementBatchInTx(ctx context.Context, tx server.PgTx, contractIds []server.Id) (attempts []legacyFinancialCohortAttempt, posts []func() any, returnErr error) {
	expected, scoped := ctx.Value(legacySettlementCloseScopeKey{}).(ContractCloseOwner)
	if !scoped || expected.Kind != ContractCloseOwnerSourceClient || !expected.valid() ||
		len(contractIds) < 2 || len(contractIds) > legacyFinancialCohortLimit {
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
			contract := &legacyFinancialCohortContract{}
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
	locked := map[server.Id]bool{}
	if len(ownedIds) > 0 {
		if err := checkLegacyFinancialCohortBudget(ctx, "headers"); err != nil {
			return nil, nil, err
		}
		rows, err = queryLegacyFinancialCohort(ctx, tx, `SELECT owned.contract_id,owned.source_network_id,owned.source_id,
            owned.destination_network_id,owned.destination_id,owned.payer_network_id,
            owned.companion_contract_id,owned.stream_id,owned.usage_origin_is_source,owned.outcome,
            owned.transfer_byte_count,owned.usage_unverified,owned.provider_usage
            FROM (SELECT unnest($1::uuid[]) AS contract_id ORDER BY contract_id OFFSET 0) AS requested
            CROSS JOIN LATERAL (SELECT contract_id,source_network_id,source_id,destination_network_id,destination_id,
            payer_network_id,companion_contract_id,stream_id,usage_origin_is_source,outcome,
            transfer_byte_count,usage_unverified,provider_usage FROM transfer_contract
            WHERE contract_id=requested.contract_id OFFSET 0 FOR UPDATE SKIP LOCKED) AS owned`, ownedIds)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				var owner contractSettlementOwner
				server.Raise(rows.Scan(&id, &owner.participants.sourceNetworkId, &owner.participants.sourceId,
					&owner.participants.destinationNetworkId, &owner.participants.destinationId,
					&owner.participants.payerNetworkId, &owner.participants.companionContractId, &owner.participants.streamId,
					&owner.usageOriginIsSource, &owner.priorOutcome, &owner.capacity, &owner.unverified, &owner.retained))
				contracts[id].owner = owner
				contracts[id].fallback = owner.priorOutcome != nil || owner.participants.payerNetworkId != nil || owner.participants.sourceId != expected.Id
				locked[id] = true
			}
		})
	}
	readyIds := make([]server.Id, 0, len(ownedIds))
	for _, id := range ownedIds {
		if locked[id] && !contracts[id].fallback {
			readyIds = append(readyIds, id)
		}
	}
	if len(readyIds) > 0 {
		if err := checkLegacyFinancialCohortBudget(ctx, "financial_reads"); err != nil {
			return nil, nil, err
		}
		batch := &pgx.Batch{}
		batch.Queue(`SELECT requested.contract_id,EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=requested.contract_id)
            FROM unnest($1::uuid[]) AS requested(contract_id)`, readyIds).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var id server.Id
				var hasEscrow bool
				if err := rows.Scan(&id, &hasEscrow); err != nil {
					return err
				}
				contracts[id].fallback = hasEscrow
			}
			return rows.Err()
		})
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
				if report.party == ContractPartyDestination {
					contracts[id].clockByteCount = report.byteCount
				}
			}
			return rows.Err()
		})
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
				contracts[id].usageParticipants = append(contracts[id].usageParticipants, participant)
			}
			if count > legacyFinancialCohortParticipantLimit {
				return errLegacyFinancialCohortUnsupported
			}
			return rows.Err()
		})
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return nil, nil, err
		}
	}
	admitted := make([]*legacyFinancialCohortContract, 0, len(contractIds))
	for _, id := range contractIds {
		attempt := legacyFinancialCohortAttempt{contractId: id}
		contract := contracts[id]
		switch {
		case contract == nil:
			attempt.busy, attempt.busyGate = true, legacySettlementBusyIntent
		case !locked[id]:
			attempt.busy, attempt.busyGate = true, legacySettlementBusyContract
		case contract.fallback:
			attempt.fallback = true
		default:
			contract.usage, err = legacySourceBatchUsage(contract)
			if err != nil {
				attempt.fallback = true
			} else {
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
	if err := checkLegacyFinancialCohortBudget(ctx, "outcomes"); err != nil {
		return nil, nil, err
	}
	batch := &pgx.Batch{}
	if err := queueLegacyFinancialCohortOutcomes(batch, admitted); err != nil {
		return nil, nil, err
	}
	// Set before dispatch: a failed exchange may have reached a write. It may
	// neither become a read-only fallback nor renew its soft admission budget.
	if budget, _ := ctx.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget); budget != nil {
		budget.written = true
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return nil, nil, err
	}
	signedCtx := providerWorkSessionContext(ctx)
	for _, contract := range admitted {
		if err := checkLegacyFinancialCohortBudget(ctx, "provenance"); err != nil {
			return nil, nil, err
		}
		server.AddTxCommitCount(tx, &contractClosedCounter, 1)
		providerWorkRetainOutcomeInTx(signedCtx, tx, contract.contractId, contract.outcome, contract.closedAt)
		contractHoleEventInTx(ctx, tx, contract.contractId, contract.owner.participants.sourceId, contract.owner.participants.destinationId, "remove")
		if contract.clockByteCount > 0 {
			posts = append(posts, legacySettlementClockPost(ctx, contract.clockByteCount))
		}
	}
	return attempts, posts, nil
}

// Free closes use the ordinary usage rules without billing eligibility. A
// missing legacy direction cannot satisfy the database usage guard, so its
// ordinary refusal must happen outside any healthy batch's write phase.
func legacySourceBatchUsage(contract *legacyFinancialCohortContract) (*contractUsageSnapshot, error) {
	owner := &contract.owner
	// Original proof remains immutable, but a partial report still needs
	// ordinary continuation before its final destination clock is known.
	closes := map[ContractParty]contractUsageClose{}
	for _, report := range owner.reports {
		closes[report.party] = contractUsageClose{ByteCount: report.byteCount, Checkpoint: report.checkpoint}
	}
	count, err := contractCompletedUsage(contract.outcome, owner.capacity, closes)
	if err != nil {
		return nil, err
	}
	if owner.unverified {
		return retainedContractExpiryUsage(owner.retained)
	}
	if owner.usageOriginIsSource == nil {
		return nil, errLegacyFinancialCohortUnsupported
	}
	participants, _, err := contractParticipantsFromRows(contract.contractId, owner.participants, owner.usageOriginIsSource, contract.usageParticipants)
	if err != nil {
		return nil, err
	}
	return newContractUsageSnapshot(count, participants)
}
