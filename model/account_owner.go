package model

import (
	"context"

	"github.com/urnetwork/server"
)

// Whether the user administers the network, read in the caller's transaction.
// The account changes that a network's own sign-in alone may make (an email
// change, a payout wallet disconnect) check it in the transaction of the
// change, so a user who no longer administers the network changes nothing.
func networkAdminInTx(ctx context.Context, tx server.PgTx, userId server.Id, networkId server.Id) (admin bool) {
	result, err := tx.Query(ctx, `SELECT 1 FROM network WHERE network_id = $1 AND admin_user_id = $2`, networkId, userId)
	server.WithPgResult(result, err, func() {
		admin = result.Next()
	})
	return
}
