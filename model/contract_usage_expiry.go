// Expiry retains original, authenticated reports before any billing fallback
// fills a missing peer. A checkpoint is a delivered-byte lower bound even
// though the transfer can resume until the quiet period expires.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// Reports are captured once under the outcome owner's lock. A missing map
// entry remains missing on replay; a later synthetic close cannot complete it.
type contractUsageExpiry struct {
	Capacity ByteCount                            `json:"capacity"`
	Reports  map[ContractParty]contractUsageClose `json:"reports"`
}

// The scanner's state is only a candidate. The expiry owner reloads these
// fields under the same row lock used by an authenticated CloseContract.
type contractExpiryState struct {
	contractId    server.Id
	sourceId      server.Id
	destinationId server.Id
	dispute       bool

	sourceCloseTime             *time.Time
	sourceUsedTransferByteCount *ByteCount
	sourceCheckpoint            *bool

	destinationCloseTime             *time.Time
	destinationUsedTransferByteCount *ByteCount
	destinationCheckpoint            *bool
}

// Both endpoints must have independently reported these bytes. Finalization
// changes the lifecycle, not the smaller authenticated delivered-byte count.
func contractExpiryCompletedUsage(proof *contractUsageExpiry) (ByteCount, error) {
	if proof == nil || proof.Capacity < 0 || proof.Reports == nil {
		return 0, fmt.Errorf("invalid original expiry reports")
	}
	for party, report := range proof.Reports {
		if (party != ContractPartySource && party != ContractPartyDestination) || report.ByteCount < 0 {
			return 0, fmt.Errorf("invalid original expiry report")
		}
	}
	source, sourceOk := proof.Reports[ContractPartySource]
	destination, destinationOk := proof.Reports[ContractPartyDestination]
	if !sourceOk || !destinationOk {
		return 0, nil
	}
	return min(proof.Capacity, source.ByteCount, destination.ByteCount), nil
}

// An old interrupted expiry may have written its sticky marker before a
// proof existed. It stays uncredited; reconstructing from its current rows
// could accidentally authenticate a synthetic peer.
func retainedContractExpiryUsage(data []byte) (*contractUsageSnapshot, error) {
	if len(data) == 0 || string(data) == "null" {
		return &contractUsageSnapshot{Version: 1, Providers: []contractProviderUsage{}, ExcludedReason: "expired_unconfirmed"}, nil
	}
	snapshot, err := decodeContractUsageSnapshot(data)
	if err != nil {
		return nil, err
	}
	if snapshot.Expiry == nil && snapshot.ExcludedReason != "expired_unconfirmed" {
		return nil, fmt.Errorf("expiry lacks its original report proof")
	}
	return snapshot, nil
}

// A successful preparation commits the original proof before billing can
// synthesize or finalize a close. Restarts reuse that exact proof. A recent
// real report withdraws a stale scan candidate without closing its stream.
func prepareContractExpiryInTx(ctx context.Context, tx server.PgTx, contractId server.Id, cutoff time.Time) (*contractExpiryState, error) {
	state := &contractExpiryState{contractId: contractId}
	var outcome *ContractOutcome
	var created time.Time
	var originIsSource *bool
	var unverified bool
	var retained []byte
	proof := &contractUsageExpiry{Reports: map[ContractParty]contractUsageClose{}}
	if err := tx.QueryRow(ctx, `
		SELECT source_id, destination_id, dispute, outcome, create_time,
			usage_origin_is_source, usage_unverified, provider_usage, transfer_byte_count
		FROM transfer_contract WHERE contract_id=$1 FOR UPDATE
	`, contractId).Scan(&state.sourceId, &state.destinationId, &state.dispute, &outcome, &created,
		&originIsSource, &unverified, &retained, &proof.Capacity); err != nil {
		return nil, fmt.Errorf("lock expiring contract: %w", err)
	}
	if outcome != nil {
		return nil, errContractAlreadySettled
	}
	lastReport := created
	rows, err := tx.Query(ctx, `SELECT party, used_transfer_byte_count, checkpoint, close_time FROM contract_close WHERE contract_id=$1`, contractId)
	if err != nil {
		return nil, fmt.Errorf("read original expiry reports: %w", err)
	}
	for rows.Next() {
		var party ContractParty
		var report contractUsageClose
		var reported time.Time
		if err := rows.Scan(&party, &report.ByteCount, &report.Checkpoint, &reported); err != nil {
			rows.Close()
			return nil, err
		}
		proof.Reports[party] = report
		if lastReport.Before(reported) {
			lastReport = reported
		}
		switch party {
		case ContractPartySource:
			state.sourceCloseTime, state.sourceUsedTransferByteCount, state.sourceCheckpoint = &reported, &report.ByteCount, &report.Checkpoint
		case ContractPartyDestination:
			state.destinationCloseTime, state.destinationUsedTransferByteCount, state.destinationCheckpoint = &reported, &report.ByteCount, &report.Checkpoint
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if unverified {
		// The earlier expiry already owned retirement. Synthetic close times
		// cannot postpone its durable continuation by another quiet period.
		_, err := retainedContractExpiryUsage(retained)
		return state, err
	}
	if lastReport.After(cutoff) {
		return nil, nil
	}
	byteCount, err := contractExpiryCompletedUsage(proof)
	if err != nil {
		return nil, err
	}
	snapshot := &contractUsageSnapshot{Version: 1, Providers: []contractProviderUsage{}, ExcludedReason: "expired_unconfirmed"}
	if originIsSource != nil {
		snapshot.Expiry = proof
		if len(proof.Reports) == 2 {
			participants, _, err := contractParticipantsWithUsageOriginInTx(ctx, tx, contractId, originIsSource)
			if err != nil {
				return nil, err
			}
			snapshot, err = newContractUsageSnapshot(byteCount, participants)
			if err != nil {
				return nil, err
			}
			snapshot.Expiry = proof
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE transfer_contract SET usage_unverified=true,provider_usage=$2 WHERE contract_id=$1 AND outcome IS NULL`, contractId, snapshot); err != nil {
		return nil, fmt.Errorf("retain original expiry proof: %w", err)
	}
	return state, nil
}
