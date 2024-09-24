package model

import (
	"context"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
)

type NetworkUser struct {
	UserId   bringyour.Id `json:"userId"`
	UserName string       `json:"userName"`
	UserAuth string       `json:"userAuth"`
	Verified bool         `json:"verified"`
	AuthType string       `json:"authType"`
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
				user_id,
				user_name,
				auth_type,
				user_auth,
				verified
			FROM network_user 
			WHERE user_id = $1
		`,
			userId,
		)
		bringyour.WithPgResult(result, err, func() {
			if result.Next() {

				networkUser = &NetworkUser{}

				bringyour.Raise(result.Scan(
					&networkUser.UserId,
					&networkUser.UserName,
					&networkUser.AuthType,
					&networkUser.UserAuth,
					&networkUser.Verified,
				))
			}
		})

	})

	return networkUser
}

func NetworkUserUpdate(
	name string,
	session *session.ClientSession,
) {

	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		bringyour.RaisePgResult(tx.Exec(
			session.Ctx,
			`
							UPDATE network_user
							SET
									user_name = $2
							WHERE
									user_id = $1
					`,
			session.ByJwt.UserId,
			name,
		))

	})

}
