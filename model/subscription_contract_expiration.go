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

// Reads the actual committed deadline for the signed contract response. Legacy
// rows remain nil; no deadline is inferred from a contract id or its age.
func GetContractExpirationTime(ctx context.Context, contractId server.Id) (expirationTime *time.Time, err error) {
	server.Db(ctx, func(conn server.PgConn) {
		err = conn.QueryRow(ctx, `SELECT expiration_time FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&expirationTime)
	})
	return
}

// The quiet cutoff still governs legacy rows. A recent checkpoint cannot
// withdraw a candidate once its absolute deadline has arrived.
func contractExpirationDue(expirationTime *time.Time, lastReport, cutoff, now time.Time) bool {
	return !lastReport.After(cutoff) || (expirationTime != nil && !now.Before(*expirationTime))
}

// Origin selection precedes funding/client lock waits. Recheck its immutable
// deadline after those waits, before publishing a new reverse reservation.
// A normal early close still permits the existing origin linger window.
func validateCompanionContractExpirationInTx(ctx context.Context, tx server.PgTx, originContractId *server.Id) error {
	if originContractId == nil {
		return nil
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract
		WHERE contract_id=$1 AND (expiration_time IS NULL OR expiration_time > clock_timestamp() AT TIME ZONE 'UTC'))`,
		*originContractId).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrMissingCompanionOrigin
	}
	return nil
}
