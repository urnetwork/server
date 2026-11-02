package model

import (
	"context"

	"github.com/urnetwork/server"
)

// The camouflage key of an extender (connect/EXTENDER.md P6;
// connect/EXTENDER-UPDATE2.md §3, phase 14b) is two columns of
// network_extender: the X25519 key the latest passing activation posted, and
// the network that posted it. ActivateNetworkExtender writes both, in the
// upsert that only ever updates a row of the activating network, and every
// reader a record is signed from selects the key through the same guard:
//
//	CASE WHEN reality_public_key_network_id = network_id THEN reality_public_key END
//
// So a record carries the key only while the network that posted it still
// holds the identity key, whatever moved the row.

// Testing_GetNetworkExtenderStoredRealityPublicKey reads one extender's
// camouflage key columns as stored: the key and the network that posted it,
// whether or not a record would carry the key. Both are nil for an extender
// that stores none, or one that is gone.
func Testing_GetNetworkExtenderStoredRealityPublicKey(
	ctx context.Context,
	extenderId server.Id,
) (realityPublicKey []byte, postingNetworkId *server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT reality_public_key, reality_public_key_network_id
			FROM network_extender
			WHERE extender_id = $1
			`,
			extenderId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&realityPublicKey, &postingNetworkId))
			}
		})
	})
	return
}

// Testing_MoveNetworkExtenderToNetwork registers an extender's identity key to
// another network without an activation, as a support correction would.
func Testing_MoveNetworkExtenderToNetwork(
	ctx context.Context,
	extenderId server.Id,
	networkId server.Id,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender
			SET network_id = $2
			WHERE extender_id = $1
			`,
			extenderId,
			networkId,
		))
	})
}
