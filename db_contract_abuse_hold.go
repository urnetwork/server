package server

import "context"

// A hold row written by an older binary can say "copied" without any
// contract parts. These values certify the placement snapshot and the full
// hold respectively. An unknown value cannot release a retention guard.
const (
	LegalHoldPlacementSnapshot = "placement_copy"
	LegalHoldVerifiedCopy      = "verified_copy"
)

const pendingContractHold = `hold.part = 0
	AND (hold.expire_time IS NULL OR hold.expire_time > timezone('UTC', statement_timestamp()))
	AND hold.copy_state IS DISTINCT FROM '` + LegalHoldPlacementSnapshot + `'
	AND hold.copy_state IS DISTINCT FROM '` + LegalHoldVerifiedCopy + `'`

// A weekly partition can have millions of rows. Read the small set of live
// holds before issuing a query against its leaf.
func pendingContractCopies(ctx context.Context, conn contractCatalogQuerier) bool {
	if !SidecarMg5Linked() {
		return false
	}
	var pending bool
	rows, err := conn.Query(ctx, `SELECT EXISTS (SELECT 1 FROM abuse_legal_hold hold WHERE `+pendingContractHold+`)`)
	WithPgResult(rows, err, func() {
		if rows.Next() {
			Raise(rows.Scan(&pending))
		}
	})
	return pending
}

// ContractPlacementCopyPendingSQL names the transfer_contract row "candidate".
// It also works on a detached leaf during the partition registry's final check.
func ContractPlacementCopyPendingSQL() string {
	if !SidecarMg5Linked() {
		return "false"
	}
	return `EXISTS (SELECT 1 FROM abuse_legal_hold hold WHERE ` + pendingContractHold + `
		AND (hold.place_time IS NULL OR candidate.create_time <= hold.place_time)
		AND hold.network_id IN (candidate.source_network_id, candidate.destination_network_id))`
}

// A close-evidence row can survive the contract or the client. Use both
// ownership records while available. If both disappeared before an old
// incomplete hold was recovered, the historical network cannot be proven.
func evidencePlacementCopyPendingSQL() string {
	if !SidecarMg5Linked() {
		return "false"
	}
	return `EXISTS (SELECT 1 FROM abuse_legal_hold hold WHERE ` + pendingContractHold + `
		AND (hold.place_time IS NULL OR evidence.accepted_at <= hold.place_time)
		AND (EXISTS (SELECT 1 FROM transfer_contract c WHERE c.contract_id = evidence.contract_id
				AND hold.network_id IN (c.source_network_id, c.destination_network_id))
			OR EXISTS (SELECT 1 FROM network_client c WHERE c.client_id = evidence.client_id AND c.network_id = hold.network_id)
			OR (NOT EXISTS (SELECT 1 FROM transfer_contract c WHERE c.contract_id = evidence.contract_id)
				AND NOT EXISTS (SELECT 1 FROM network_client c WHERE c.client_id = evidence.client_id))))`
}
