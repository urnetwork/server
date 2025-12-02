package model

import (
	"context"

	"github.com/urnetwork/server/v2025"
)

type CircleUC struct {
	NetworkId      server.Id `json:"network_id"`
	UserId         server.Id `json:"user_id"`
	CircleUCUserId server.Id `json:"circle_uc_user_id"`
}

func GetCircleUCByCircleUCUserId(
	ctx context.Context,
	circleUCUserId server.Id,
) *CircleUC {
	var circleUC *CircleUC

	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    network_id,
                    user_id,
                    circle_uc_user_id
                FROM circle_uc
                WHERE
                    circle_uc_user_id = $1
            `,
			circleUCUserId,
		)

		server.WithPgResult(result, err, func() {
			if result.Next() {
				circleUC = &CircleUC{}
				server.Raise(result.Scan(&circleUC.NetworkId, &circleUC.UserId, &circleUC.CircleUCUserId))
			}
		})
	})
	return circleUC
}

// this user id is what is used for the Circle api:
// - create a user token
// - list wallets
// - create a wallet challenge
// https://developers.circle.com/w3s/reference
func GetOrCreateCircleUserId(
	ctx context.Context,
	networkId server.Id,
	userId server.Id,
) (circleUserId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    circle_uc_user_id
                FROM circle_uc
                WHERE
                    network_id = $1 AND
                    user_id = $2
            `,
			networkId,
			userId,
		)
		set := false
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&circleUserId))
				set = true
			}
		})

		if set {
			return
		}

		circleUserId = server.NewId()

		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO circle_uc (
                    network_id,
                    user_id,
                    circle_uc_user_id
                )
                VALUES ($1, $2, $3)
            `,
			networkId,
			userId,
			circleUserId,
		))
	})
	return
}

// used for testing
func SetCircleUserId(
	ctx context.Context,
	networkId server.Id,
	userId server.Id,
	circleUserId server.Id,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO circle_uc (
                    network_id,
                    user_id,
                    circle_uc_user_id
                )
                VALUES ($1, $2, $3)
                ON CONFLICT (network_id, user_id) DO UPDATE
                SET
                	circle_uc_user_id = $3
            `,
			networkId,
			userId,
			circleUserId,
		))
	})
}

func GetCircleUCUsers(ctx context.Context) (users []CircleUC) {
	server.Tx(ctx, func(tx server.PgTx) {
		result, txErr := tx.Query(
			ctx,
			`
                SELECT
                    *
                FROM circle_uc
            `,
		)

		server.WithPgResult(result, txErr, func() {

			for result.Next() {
				user := CircleUC{}
				server.Raise(result.Scan(
					&user.NetworkId,
					&user.UserId,
					&user.CircleUCUserId,
				))
				users = append(users, user)
			}
		})
	})

	return users
}
