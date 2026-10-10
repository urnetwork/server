// Settlement reuses only facts protected by its owning contract row lock.
package model

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// The contract row owns these retained endpoint and stream references. Stream
// participants themselves can change through a sibling contract and stay fresh
// reads at each billing/usage call; this is not a cross-transaction cache.
type contractParticipantOwner struct {
	sourceNetworkId      server.Id
	sourceId             server.Id
	destinationNetworkId server.Id
	destinationId        server.Id
	payerNetworkId       *server.Id
	companionContractId  *server.Id
	streamId             *server.Id
}

// Both financial and usage rules consume the same reports under the contract
// lock. Keep the raw rows so their separate validation and count rules survive.
type contractSettlementReport struct {
	party      ContractParty
	byteCount  ByteCount
	checkpoint bool
}

// The caller keeps this owner local to one transaction and never returns it in
// a post. A rollback, retry or another contract always performs a new read.
type contractSettlementOwner struct {
	participants        contractParticipantOwner
	usageOriginIsSource *bool
	priorOutcome        *ContractOutcome
	capacity            ByteCount
	unverified          bool
	retained            []byte
	reports             []contractSettlementReport
}

// Keep the row lock and report read as successive statements: Read Committed
// must take the report snapshot after any competing contract owner commits.
// One batch removes client waits without combining their statement snapshots.
func readContractSettlementOwnerInTx(ctx context.Context, tx server.PgTx, contractId server.Id, closeOwners ...ContractCloseOwner) (*contractSettlementOwner, error) {
	owner := &contractSettlementOwner{}
	batch := &pgx.Batch{}
	batch.Queue(`SELECT source_network_id,source_id,destination_network_id,destination_id,
        payer_network_id,companion_contract_id,stream_id,
        usage_origin_is_source,outcome,transfer_byte_count,usage_unverified,provider_usage
        FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, contractId).QueryRow(func(row pgx.Row) error {
		return row.Scan(&owner.participants.sourceNetworkId, &owner.participants.sourceId,
			&owner.participants.destinationNetworkId, &owner.participants.destinationId,
			&owner.participants.payerNetworkId, &owner.participants.companionContractId, &owner.participants.streamId,
			&owner.usageOriginIsSource, &owner.priorOutcome, &owner.capacity, &owner.unverified, &owner.retained)
	})
	batch.Queue(`SELECT party,used_transfer_byte_count,checkpoint FROM contract_close WHERE contract_id=$1`, contractId).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var report contractSettlementReport
			if err := rows.Scan(&report.party, &report.byteCount, &report.checkpoint); err != nil {
				return err
			}
			owner.reports = append(owner.reports, report)
		}
		return rows.Err()
	})
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return nil, fmt.Errorf("read contract settlement owner: %w", err)
	}
	if owner.participants.payerNetworkId == nil {
		// A legacy worker passes the owner it just resolved under this same
		// contract lock. Direct settlement resolves it on the caller's tx.
		// Never let the old companion endpoint fallback replace real escrow
		// evidence for billing or the paying client's usage meter.
		var resolved ContractCloseOwner
		if len(closeOwners) > 0 {
			resolved = closeOwners[0]
		} else {
			var err error
			resolved, _, err = readContractCloseOwnerInConn(ctx, tx, contractId)
			if err != nil {
				return nil, err
			}
		}
		if resolved.Kind == ContractCloseOwnerPayerNetwork {
			owner.participants.payerNetworkId = &resolved.Id
		}
	}
	return owner, nil
}

// Monetary eligibility and usage direction intentionally differ. Reuse the
// owned header/reports, then read current retained participants for usage just
// as the independent usage reader does. All original usage guards remain here.
func (self *contractSettlementOwner) usageSnapshotInTx(ctx context.Context, tx server.PgTx, contractId server.Id, outcome ContractOutcome) (*contractUsageSnapshot, error) {
	if self.priorOutcome != nil {
		return nil, nil
	}
	if self.unverified {
		return retainedContractExpiryUsage(self.retained)
	}
	if self.usageOriginIsSource == nil {
		return nil, nil
	}
	closes := map[ContractParty]contractUsageClose{}
	for _, report := range self.reports {
		closes[report.party] = contractUsageClose{ByteCount: report.byteCount, Checkpoint: report.checkpoint}
	}
	byteCount, err := contractCompletedUsage(outcome, self.capacity, closes)
	if err != nil {
		return nil, err
	}
	participants, _, err := contractParticipantsFromOwnerInTx(ctx, tx, contractId, self.participants, self.usageOriginIsSource)
	if err != nil {
		return nil, err
	}
	return newContractUsageSnapshot(byteCount, participants)
}
