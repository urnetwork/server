package model

// The record that an account was deleted. An account deletion removes the
// network row and its users (RemoveNetworkWithRemovals, the lag guest expiry),
// and a missing row alone does not say why it is missing. A store purchase
// whose original account is deleted may move to the account its owner signs
// in to now (apple_subscription_move_model.go, owner 2026-11-17: "Restore
// moves it."), and only an account this table names was deleted. Each
// deletion writes its row in the deletion's own transaction, so a network
// cannot be gone without its record. The rows of networks deleted before this
// table were written once, by its migration, from the App Store records that
// name a network whose row is gone (the same evidence the deleted network
// cleanup finds them by, FindDeletedNetworkIds). A row is never removed: a
// network id is not reused.

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// How a network came to be deleted.
const (
	// the account was deleted, by its owner in the app or on ur.io, or by an
	// operator (RemoveNetworkWithRemovals)
	NetworkDeletionSourceAccount = "account"
	// an unpaid lag guest expired (RemoveExpiredLagGuestsWithRemovals)
	NetworkDeletionSourceLagGuest = "lag_guest"
	// deleted before this record existed; written by the migration that added
	// it, from the App Store records naming a network whose row is gone
	NetworkDeletionSourceBackfill = "backfill"
)

// Records, in the deletion's own transaction, that the network is deleted. A
// second record of the same network changes nothing.
func RecordNetworkDeletionInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	source string,
	deleteTime time.Time,
) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO deleted_network (
				network_id,
				delete_time,
				source
			)
			VALUES ($1, $2, $3)
			ON CONFLICT (network_id) DO NOTHING
		`,
		networkId,
		deleteTime.UTC(),
		source,
	))
}

// Whether the network was deleted: it has a deletion record. A network that
// merely isn't found (an id that never named a network) has none.
func IsNetworkDeletedInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
) (deleted bool) {
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT EXISTS (
				SELECT 1
				FROM deleted_network
				WHERE network_id = $1
			)
		`,
		networkId,
	).Scan(&deleted))
	return
}

// IsNetworkDeletedInTx outside a transaction, for tests and tools.
func IsNetworkDeleted(ctx context.Context, networkId server.Id) (deleted bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		deleted = IsNetworkDeletedInTx(tx, ctx, networkId)
	}, server.OptReadOnly())
	return
}
