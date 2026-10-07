package model

import (
	"context"

	"github.com/urnetwork/server"
)

// The resident needs existence, not the complete contract/close inventory. Each
// direction can stop at its first qualifying contract. Keep the structural open
// predicate so false-zero statistics cannot select the global open indexes.
// OFFSET 0 keeps close checks correlated: a right anti hash join would otherwise
// materialize every pair contract before returning the first existence result.
const hasOpenContractForPairSQL = `
SELECT EXISTS (
    SELECT 1 FROM transfer_contract AS contract
    WHERE (CASE WHEN contract.outcome IS NULL THEN contract.dispute = false ELSE false END)
        AND contract.source_id = $1 AND contract.destination_id = $2
        AND (contract.expiration_time IS NULL OR contract.expiration_time > statement_timestamp() AT TIME ZONE 'UTC')
        AND NOT EXISTS (
            SELECT 1 FROM contract_close AS close
            WHERE close.contract_id = contract.contract_id
                AND (COALESCE(close.checkpoint, false) OR COALESCE(close.party, '') <> '')
            OFFSET 0
        )
) OR ($1 <> $2 AND EXISTS (
    SELECT 1 FROM transfer_contract AS contract
    WHERE (CASE WHEN contract.outcome IS NULL THEN contract.dispute = false ELSE false END)
        AND contract.source_id = $2 AND contract.destination_id = $1
        AND (contract.expiration_time IS NULL OR contract.expiration_time > statement_timestamp() AT TIME ZONE 'UTC')
        AND NOT EXISTS (
            SELECT 1 FROM contract_close AS close
            WHERE close.contract_id = contract.contract_id
                AND (COALESCE(close.checkpoint, false) OR COALESCE(close.party, '') <> '')
            OFFSET 0
        )
))
`

// HasOpenContractForPair reports whether either direction has an unresolved,
// undisputed contract without a meaningful close/checkpoint row. It preserves
// GetOpenContractIdsWithNoPartialClose's treatment of empty/null party metadata.
// Both directions use one SQL snapshot; this does not cache a negative result.
func HasOpenContractForPair(ctx context.Context, sourceId, destinationId server.Id) bool {
	var found bool
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, hasOpenContractForPairSQL, sourceId, destinationId).Scan(&found))
	})
	return found
}
