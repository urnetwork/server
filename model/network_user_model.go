package model

import (
	"context"

	"github.com/urnetwork/server"
)

type NetworkUser struct {
	UserId        server.Id `json:"user_id"`
	UserAuth      *string   `json:"user_auth,omitempty"`
	Verified      bool      `json:"verified"`
	AuthType      string    `json:"auth_type"`
	NetworkName   string    `json:"network_name"`
	WalletAddress *string   `json:"wallet_address,omitempty"`
}

func GetNetworkUser(
	ctx context.Context,
	userId server.Id,
) *NetworkUser {

	var networkUser *NetworkUser

	server.Tx(ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			ctx,
			`
			SELECT
				network_user.user_id,
				network_user.auth_type,
				network_user.user_auth,
				network_user.verified,
				network_user.wallet_address,
				network.network_name
			FROM network_user
			LEFT JOIN network ON
				network.admin_user_id = network_user.user_id
			WHERE user_id = $1
		`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {

				networkUser = &NetworkUser{}

				server.Raise(result.Scan(
					&networkUser.UserId,
					&networkUser.AuthType,
					&networkUser.UserAuth,
					&networkUser.Verified,
					&networkUser.WalletAddress,
					&networkUser.NetworkName,
				))
			}
		})

	})

	return networkUser
}
