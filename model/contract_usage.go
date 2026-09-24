// Subnet usage snapshots preserve completed provider work independently of
// account balances, escrow splits, and later membership changes.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"

	"github.com/urnetwork/server"
)

// One immutable contract total is partitioned once over the service clients.
// Empty providers are permitted only for a zero-byte contract.
type contractUsageSnapshot struct {
	Version        int                     `json:"version"`
	ByteCount      ByteCount               `json:"byte_count"`
	Providers      []contractProviderUsage `json:"providers"`
	ExcludedReason string                  `json:"excluded_reason,omitempty"`
	Expiry         *contractUsageExpiry    `json:"expiry,omitempty"`
}

// Includes same-network service clients; monetary eligibility is unrelated.
type contractProviderUsage struct {
	ClientId  server.Id `json:"client_id"`
	NetworkId server.Id `json:"network_id"`
	ByteCount ByteCount `json:"byte_count"`
}

// A checkpoint does not complete ordinary settlement. Once expiry owns
// retirement, its accumulated bytes remain an authenticated lower bound.
type contractUsageClose struct {
	ByteCount  ByteCount `json:"byte_count"`
	Checkpoint bool      `json:"checkpoint"`
}

// Ordinary usage requires both completed reports and credits their lower
// bound. An adjudicated dispute uses its selected report. The contract's
// admitted capacity bounds credit even if a party reports more bytes.
func contractCompletedUsage(outcome ContractOutcome, capacity ByteCount, closes map[ContractParty]contractUsageClose) (ByteCount, error) {
	if capacity < 0 {
		return 0, fmt.Errorf("negative contract usage capacity")
	}
	for party, close := range closes {
		if (party != ContractPartySource && party != ContractPartyDestination) || close.ByteCount < 0 {
			return 0, fmt.Errorf("invalid contract usage close")
		}
	}
	source, sourceOk := closes[ContractPartySource]
	destination, destinationOk := closes[ContractPartyDestination]
	switch outcome {
	case ContractOutcomeSettled:
		if !sourceOk || !destinationOk || source.Checkpoint || destination.Checkpoint {
			return 0, fmt.Errorf("contract usage requires two completed reports")
		}
		return min(capacity, source.ByteCount, destination.ByteCount), nil
	case ContractOutcomeDisputeResolvedToSource:
		if !sourceOk || source.Checkpoint {
			return 0, fmt.Errorf("contract usage lacks the adjudicated source report")
		}
		return min(capacity, source.ByteCount), nil
	case ContractOutcomeDisputeResolvedToDestination:
		if !destinationOk || destination.Checkpoint {
			return 0, fmt.Errorf("contract usage lacks the adjudicated destination report")
		}
		return min(capacity, destination.ByteCount), nil
	default:
		return 0, fmt.Errorf("unknown contract usage outcome %q", outcome)
	}
}

// Stable client order assigns remainders exactly and never suppresses a
// provider merely because its network also contains the origin client.
func newContractUsageSnapshot(byteCount ByteCount, participants []ContractParticipant) (*contractUsageSnapshot, error) {
	if byteCount < 0 || (byteCount > 0 && len(participants) == 0) {
		return nil, fmt.Errorf("contract usage has invalid total or missing providers")
	}
	participants = slices.Clone(participants)
	slices.SortFunc(participants, func(a, b ContractParticipant) int { return a.ClientId.Cmp(b.ClientId) })
	snapshot := &contractUsageSnapshot{Version: 1, ByteCount: byteCount, Providers: []contractProviderUsage{}}
	for index, participant := range participants {
		if participant.ClientId == (server.Id{}) || participant.NetworkId == (server.Id{}) ||
			(index > 0 && participant.ClientId == participants[index-1].ClientId) {
			return nil, fmt.Errorf("contract usage has invalid or duplicate provider")
		}
		snapshot.Providers = append(snapshot.Providers, contractProviderUsage{
			ClientId: participant.ClientId, NetworkId: participant.NetworkId,
			ByteCount: ByteCount(evenContractPayoutShare(int64(byteCount), index, len(participants))),
		})
	}
	return snapshot, nil
}

// Locks the outcome owner before reading its completed reports and exact
// participants. Legacy directions remain explicitly uncredited; guessing a
// normalized same-network return would pay the consumer as its provider.
func contractUsageSnapshotInTx(ctx context.Context, tx server.PgTx, contractId server.Id, outcome ContractOutcome) (*contractUsageSnapshot, error) {
	var usageOriginIsSource *bool
	var priorOutcome *ContractOutcome
	var capacity ByteCount
	var unverified bool
	var retained []byte
	if err := tx.QueryRow(ctx, `
		SELECT usage_origin_is_source, outcome, transfer_byte_count, usage_unverified, provider_usage
		FROM transfer_contract WHERE contract_id = $1 FOR UPDATE
	`, contractId).Scan(&usageOriginIsSource, &priorOutcome, &capacity, &unverified, &retained); err != nil {
		return nil, fmt.Errorf("read contract usage owner: %w", err)
	}
	if priorOutcome != nil {
		return nil, nil
	}
	if unverified {
		return retainedContractExpiryUsage(retained)
	}
	if usageOriginIsSource == nil {
		return nil, nil
	}
	closes := map[ContractParty]contractUsageClose{}
	rows, err := tx.Query(ctx, `SELECT party, used_transfer_byte_count, checkpoint FROM contract_close WHERE contract_id = $1`, contractId)
	if err != nil {
		return nil, fmt.Errorf("read completed contract usage: %w", err)
	}
	for rows.Next() {
		var party ContractParty
		var close contractUsageClose
		if err := rows.Scan(&party, &close.ByteCount, &close.Checkpoint); err != nil {
			rows.Close()
			return nil, err
		}
		closes[party] = close
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	byteCount, err := contractCompletedUsage(outcome, capacity, closes)
	if err != nil {
		return nil, err
	}
	participants, _, err := contractParticipantsWithUsageOriginInTx(ctx, tx, contractId, usageOriginIsSource)
	if err != nil {
		return nil, err
	}
	return newContractUsageSnapshot(byteCount, participants)
}

// Refuses incomplete or tampered snapshots before any provider receives
// credit. Reading never joins mutable memberships or falls back to billing.
func decodeContractUsageSnapshot(data []byte) (*contractUsageSnapshot, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, fmt.Errorf("missing immutable contract usage; epoch predates complete usage activation")
	}
	var record struct {
		Version   int        `json:"version"`
		ByteCount *ByteCount `json:"byte_count"`
		Providers *[]struct {
			ClientId  server.Id  `json:"client_id"`
			NetworkId server.Id  `json:"network_id"`
			ByteCount *ByteCount `json:"byte_count"`
		} `json:"providers"`
		ExcludedReason string `json:"excluded_reason,omitempty"`
		Expiry         *struct {
			Capacity *ByteCount `json:"capacity"`
			Reports  *map[ContractParty]struct {
				ByteCount  *ByteCount `json:"byte_count"`
				Checkpoint *bool      `json:"checkpoint"`
			} `json:"reports"`
		} `json:"expiry,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, fmt.Errorf("decode contract usage: %w", err)
	}
	if record.ByteCount == nil || record.Providers == nil {
		return nil, fmt.Errorf("missing contract usage fields")
	}
	snapshot := contractUsageSnapshot{Version: record.Version, ByteCount: *record.ByteCount, Providers: []contractProviderUsage{}, ExcludedReason: record.ExcludedReason}
	if record.Expiry != nil {
		if record.Expiry.Capacity == nil || record.Expiry.Reports == nil {
			return nil, fmt.Errorf("missing original expiry report fields")
		}
		snapshot.Expiry = &contractUsageExpiry{Capacity: *record.Expiry.Capacity, Reports: map[ContractParty]contractUsageClose{}}
		for party, report := range *record.Expiry.Reports {
			if report.ByteCount == nil || report.Checkpoint == nil {
				return nil, fmt.Errorf("incomplete original expiry report")
			}
			snapshot.Expiry.Reports[party] = contractUsageClose{ByteCount: *report.ByteCount, Checkpoint: *report.Checkpoint}
		}
		byteCount, err := contractExpiryCompletedUsage(snapshot.Expiry)
		if err != nil {
			return nil, err
		}
		if byteCount != snapshot.ByteCount {
			return nil, fmt.Errorf("expiry usage differs from original bilateral reports")
		}
		if (len(snapshot.Expiry.Reports) < 2) != (snapshot.ExcludedReason == "expired_unconfirmed") {
			return nil, fmt.Errorf("expiry exclusion differs from original report presence")
		}
	}
	for _, provider := range *record.Providers {
		if provider.ByteCount == nil {
			return nil, fmt.Errorf("missing contract provider usage count")
		}
		snapshot.Providers = append(snapshot.Providers, contractProviderUsage{ClientId: provider.ClientId, NetworkId: provider.NetworkId, ByteCount: *provider.ByteCount})
	}
	if snapshot.Version != 1 || snapshot.ByteCount < 0 || snapshot.Providers == nil ||
		(snapshot.ExcludedReason != "" && (snapshot.ExcludedReason != "expired_unconfirmed" || snapshot.ByteCount != 0 || len(snapshot.Providers) != 0)) {
		return nil, fmt.Errorf("invalid contract usage snapshot header")
	}
	var total int64
	for index, provider := range snapshot.Providers {
		if provider.ClientId == (server.Id{}) || provider.NetworkId == (server.Id{}) || provider.ByteCount < 0 ||
			(index > 0 && !snapshot.Providers[index-1].ClientId.Less(provider.ClientId)) ||
			int64(provider.ByteCount) > math.MaxInt64-total {
			return nil, fmt.Errorf("invalid, ambiguous, or overflowing contract provider usage")
		}
		total += int64(provider.ByteCount)
	}
	if total != int64(snapshot.ByteCount) {
		return nil, fmt.Errorf("nonconserving contract provider usage")
	}
	return &snapshot, nil
}
