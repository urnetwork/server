// Newly issued contracts have an immutable deadline independent of checkpoints.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The database creation clock starts this lifetime after admission lock waits.
// Only the deadline rounds down to wire milliseconds; create_time retains its
// full precision so it can never precede the admission fence that released it.
const DefaultContractExpiration = 60 * time.Minute

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
// Missing deadlines use the same creation-based limit as cleanup. A normal
// early close still permits the existing origin linger window.
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
