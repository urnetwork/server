// A party's answer that it holds no record of a contract the platform asked it
// to close (CLIENTCLOSE.md section 3; connect's ContractsNoRecord). A provider
// that lost a contract's state before its first checkpoint, to a hard kill, an
// OOM, a crash or a power loss, used to answer with a final of zero bytes,
// which settled at the mean of the two counts (half the sender's) or opened a
// dispute. The answer carries no count, so it writes nothing: the party stays
// what it was. One that never reported stays silent, and the expiry sweep
// settles the contract on the other party's count while its usage stays
// expired_unconfirmed, so SN25 credits none of the bytes only one side
// confirmed (contract_usage_expiry.go). A checkpoint the party did report
// stands, and the sweep finalizes it on its count.
package model

import (
	"context"

	"github.com/urnetwork/server"
)

// What the platform holds of the answering party's reports. Both fields are
// bounded metric labels.
type ContractNoRecordState struct {
	// source or destination; empty when the client is not a party or the
	// contract is gone
	Party  ContractParty
	Report ContractNoRecordReport
}

type ContractNoRecordReport = string

const (
	// the party never reported: the contract settles as receiver-silent
	ContractNoRecordReportNone ContractNoRecordReport = "none"
	// the party's checkpoint stands for the sweep to finalize
	ContractNoRecordReportCheckpoint ContractNoRecordReport = "checkpoint"
	// the party's final was already applied
	ContractNoRecordReportFinal ContractNoRecordReport = "final"
	// the contract has an outcome or is in dispute
	ContractNoRecordReportClosed ContractNoRecordReport = "closed"
	// not a party, or the contract is gone
	ContractNoRecordReportUnknown ContractNoRecordReport = "unknown"
)

// Reads each contract's state for the answering client in one statement,
// without a lock or a write. A contract that is not found, or of which the
// client is not a party, reads as unknown.
func GetContractNoRecordStates(ctx context.Context, clientId server.Id, contractIds []server.Id) map[server.Id]ContractNoRecordState {
	states := map[server.Id]ContractNoRecordState{}
	for _, contractId := range contractIds {
		states[contractId] = ContractNoRecordState{Report: ContractNoRecordReportUnknown}
	}
	if len(contractIds) == 0 {
		return states
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				WITH party AS MATERIALIZED (
					SELECT
						contract_id,
						CASE
							WHEN source_id = $1 THEN $3::text
							WHEN destination_id = $1 THEN $4::text
						END AS party,
						outcome IS NOT NULL OR dispute AS closed
					FROM transfer_contract
					WHERE contract_id = ANY($2::uuid[])
				)
				SELECT
					party.contract_id,
					COALESCE(party.party, ''),
					party.closed,
					contract_close.checkpoint
				FROM party
				LEFT JOIN contract_close ON
					contract_close.contract_id = party.contract_id AND
					contract_close.party = party.party
			`,
			clientId,
			contractIds,
			ContractPartySource,
			ContractPartyDestination,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId server.Id
				var party ContractParty
				var closed bool
				var checkpoint *bool
				server.Raise(result.Scan(&contractId, &party, &closed, &checkpoint))
				state := ContractNoRecordState{Party: party}
				switch {
				case party == "":
					state.Report = ContractNoRecordReportUnknown
				case closed:
					state.Report = ContractNoRecordReportClosed
				case checkpoint == nil:
					state.Report = ContractNoRecordReportNone
				case *checkpoint:
					state.Report = ContractNoRecordReportCheckpoint
				default:
					state.Report = ContractNoRecordReportFinal
				}
				states[contractId] = state
			}
		})
	})
	return states
}
