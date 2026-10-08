// Pure settlement arithmetic and retained participant merging are shared by
// individual and bounded cohort owners; only their PostgreSQL acquisition differs.
package model

import (
	"fmt"
	"math"
	"slices"

	"github.com/urnetwork/server/v2026"
)

type contractSettlementEscrow struct {
	balanceId server.Id
	amount    ByteCount
	start     ByteCount
	revenue   NanoCents
}

// Billing and the destination clock keep their original report/count rules.
func contractSettlementReportAmounts(reports []contractSettlementReport, outcome ContractOutcome) (usedTransferByteCount ByteCount, clockTransferByteCount ByteCount, returnErr error) {
	switch outcome {
	case ContractOutcomeSettled:
		var partyByteCounts [2]ByteCount
		partyCount := 0
		checkpointCount := 0
		for _, report := range reports {
			// Billing counts a checkpoint's final contribution. The independent
			// usage guard below still requires its original completed reports.
			if report.checkpoint {
				checkpointCount++
			}
			if partyCount < len(partyByteCounts) {
				partyByteCounts[partyCount] = report.byteCount
			}
			if report.party == ContractPartyDestination {
				clockTransferByteCount = report.byteCount
			}
			partyCount++
		}
		if partyCount != 2 {
			returnErr = fmt.Errorf("Must have 2 parties to settle contract (found %d).", partyCount)
			return
		}
		// Defensive: refuse to settle if both parties are checkpoint.
		// settleContract shouldn't route here; flag the logic bug rather than settle.
		if checkpointCount == 2 {
			returnErr = fmt.Errorf("Cannot settle contract with both parties checkpoint.")
			return
		}
		usedTransferByteCount, returnErr = meanContractByteCount(partyByteCounts[0], partyByteCounts[1])
		if returnErr != nil {
			return
		}
	case ContractOutcomeDisputeResolvedToSource, ContractOutcomeDisputeResolvedToDestination:
		var party ContractParty
		switch outcome {
		case ContractOutcomeDisputeResolvedToSource:
			party = ContractPartySource
		default:
			party = ContractPartyDestination
		}
		for _, report := range reports {
			if report.party == party {
				usedTransferByteCount = report.byteCount
			}
			if report.party == ContractPartyDestination {
				clockTransferByteCount = report.byteCount
			}
		}
	default:
		returnErr = fmt.Errorf("Unknown contract outcome: %s", outcome)
		return
	}
	if usedTransferByteCount < 0 || clockTransferByteCount < 0 {
		returnErr = fmt.Errorf("negative contract close byte count")
		return
	}

	return
}

// Keep input order, per-grant rounding and per-contract sufficiency. A cohort
// aggregates only the resulting exact debits, never reservations or reports.
func contractSettlementSweepPayouts(used ByteCount, escrows []contractSettlementEscrow) (map[server.Id]sweepPayout, error) {
	payouts := map[server.Id]sweepPayout{}
	settled := ByteCount(0)
	for _, escrow := range escrows {
		if escrow.amount < 0 {
			return nil, fmt.Errorf("negative escrow byte count")
		}
		count := min(used-settled, escrow.amount)
		settled += count
		payouts[escrow.balanceId] = sweepPayout{escrowBalanceByteCount: escrow.amount,
			payoutByteCount: count, returnByteCount: escrow.amount - count,
			payout: NanoCents(math.Round(ProviderRevenueShare * float64(escrow.revenue) * float64(count) / float64(escrow.start)))}
	}
	if settled < used {
		return nil, errContractInsufficientEscrow
	}
	return payouts, nil
}

// Endpoint direction, duplicate identities, same-network exclusions and stable
// participant order are independent of the owner that fetched these rows.
func contractParticipantsFromRows(contractId server.Id, owner contractParticipantOwner, usageOrigin *bool, retained []ContractParticipant) ([]ContractParticipant, server.Id, error) {
	originIsSource := owner.companionContractId == nil
	originNetworkId := owner.sourceNetworkId
	if owner.payerNetworkId != nil {
		originNetworkId = *owner.payerNetworkId
		if owner.sourceNetworkId != owner.destinationNetworkId {
			switch *owner.payerNetworkId {
			case owner.sourceNetworkId:
				originIsSource = true
			case owner.destinationNetworkId:
				originIsSource = false
			default:
				return nil, server.Id{}, fmt.Errorf("contract payer is not an endpoint: %s", contractId)
			}
		}
	} else if !originIsSource {
		originNetworkId = owner.destinationNetworkId
	}
	if usageOrigin != nil {
		originIsSource = *usageOrigin
	}
	originId := owner.sourceId
	egress := ContractParticipant{ClientId: owner.destinationId, NetworkId: owner.destinationNetworkId}
	if !originIsSource {
		originId = owner.destinationId
		egress = ContractParticipant{ClientId: owner.sourceId, NetworkId: owner.sourceNetworkId}
	}
	participants := map[server.Id]ContractParticipant{}
	for _, participant := range append([]ContractParticipant{egress}, retained...) {
		if participant.ClientId == originId {
			continue
		}
		if prior, exists := participants[participant.ClientId]; exists && prior.NetworkId != participant.NetworkId {
			return nil, server.Id{}, fmt.Errorf("contract provider has conflicting retained networks: %s", contractId)
		}
		participants[participant.ClientId] = participant
	}
	ordered := make([]ContractParticipant, 0, len(participants))
	for _, participant := range participants {
		ordered = append(ordered, participant)
	}
	slices.SortFunc(ordered, func(a, b ContractParticipant) int { return a.ClientId.Cmp(b.ClientId) })
	return ordered, originNetworkId, nil
}
