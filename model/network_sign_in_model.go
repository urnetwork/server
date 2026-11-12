package model

import (
	"context"

	"github.com/urnetwork/server"
)

// Whether a network's admin user has a login method (HasAnyAuthMethod): a
// password or verified email or phone, an SSO, a wallet or a seed phrase. A
// legacy guest network has none, and neither does a network that does not
// exist. For a purchase made for a network the buyer names, where the
// caller's own session says nothing about it.
func NetworkHasAnyAuthMethod(ctx context.Context, networkId server.Id) bool {
	var adminUserId *server.Id
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT admin_user_id FROM network WHERE network_id = $1`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&adminUserId))
			}
		})
	})
	if adminUserId == nil {
		return false
	}
	return HasAnyAuthMethod(ctx, *adminUserId)
}
