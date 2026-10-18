// Newly issued contracts have an immutable deadline independent of checkpoints.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The database creation clock starts this lifetime after admission lock waits.
// Only the deadline rounds down to wire milliseconds; create_time retains its
// full precision so it can never precede the admission fence that released it.
const DefaultContractExpiration = 60 * time.Minute

// A closed origin keeps anchoring companions until its original deadline, so
// settling a forward contract early never ends the provider's return path
// before an open one would have. Every origin read also requires the unexpired
// deadline, which never exceeds creation plus the lifetime above, and a close
// follows creation: this close-time bound admits every unexpired closed origin.
//
// The closed origin reads also bound contract_id below, which keeps them an
// index range on transfer_contract_open_source_id (open, source_id,
// destination_id, contract_id) instead of the pair's whole closed history.
// Contract ids are generated at admission and lead with their 48-bit creation
// millisecond (connect.NewId), so an unexpired contract's id is at least the
// smallest id of 75 minutes ago: the lifetime plus 15 minutes for the id
// preceding the database creation clock (admission waits, server clock skew).
const CompanionOriginCloseLinger = DefaultContractExpiration

// Reads the actual committed deadline for the signed contract response. Legacy
// rows remain nil; no deadline is inferred from a contract id or its age.
func GetContractExpirationTime(ctx context.Context, contractId server.Id) (expirationTime *time.Time, err error) {
	server.Db(ctx, func(conn server.PgConn) {
		err = conn.QueryRow(ctx, `SELECT expiration_time FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&expirationTime)
	})
	return
}

// Missing deadlines fall back to the creation clock plus the maximum lifetime.
// Quiet contracts may close earlier; a checkpoint cannot extend either deadline.
func contractExpirationDue(expirationTime *time.Time, created, lastReport, cutoff, now time.Time) bool {
	deadline := created.Add(DefaultContractExpiration)
	if expirationTime != nil {
		deadline = *expirationTime
	}
	return !lastReport.After(cutoff) || !now.Before(deadline)
}

// Origin selection precedes funding/client lock waits. Recheck its immutable
// deadline after those waits, before publishing a new reverse reservation.
// Missing deadlines use the same creation-based limit as cleanup. An early
// close keeps the origin eligible within the caller's close linger.
func validateCompanionContractExpirationInTx(ctx context.Context, tx server.PgTx, originContractId *server.Id) error {
	if originContractId == nil {
		return nil
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract
		WHERE contract_id=$1 AND COALESCE(expiration_time, create_time + interval '60 minutes') > clock_timestamp() AT TIME ZONE 'UTC')`,
		*originContractId).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrMissingCompanionOrigin
	}
	return nil
}
