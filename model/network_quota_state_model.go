package model

import (
	"context"

	"github.com/urnetwork/server"
)

// network_quota_state (DESIGN.md 20.7, §2.8 api-13): each network's stored-byte count,
// whether it is over its storage limit, and when it was marked spent. A row lives until
// its network is deleted. The table belongs to MG2 (QUOTA-M2-02); its readers and writers,
// OverLimit, the stored count and the spent marks, are QUOTA-M2-04's.

// Removes the network's row in its deletion transaction, the one way a row goes.
func removeNetworkQuotaStateInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_quota_state
			WHERE network_id = $1
		`,
		networkId,
	))
}
