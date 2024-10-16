package model

import (
	"context"

	"bringyour.com/bringyour"
)

type NetworkUser struct {
	UserId      bringyour.Id `json:"user_id"`
	UserAuth    string       `json:"user_auth"`
	Verified    bool         `json:"verified"`
	AuthType    string       `json:"auth_type"`
	NetworkName string       `json:"network_name"`
}

func GetNetworkUser(
	ctx context.Context,
	userId bringyour.Id,
) *NetworkUser {

	var networkUser *NetworkUser

	bringyour.Tx(ctx, func(tx bringyour.PgTx) {

		result, err := tx.Query(
			ctx,
			`
			SELECT
				network_user.user_id,
				network_user.auth_type,
				network_user.user_auth,
				network_user.verified,
				network.network_name
			FROM network_user
			LEFT JOIN network ON
				network.admin_user_id = network_user.user_id
			WHERE user_id = $1
		`,
			userId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {

				networkUser = &NetworkUser{}

				bringyour.Raise(result.Scan(
					&networkUser.UserId,
					&networkUser.AuthType,
					&networkUser.UserAuth,
					&networkUser.Verified,
					&networkUser.NetworkName,
				))
			}
		})

	})

	return networkUser
}
