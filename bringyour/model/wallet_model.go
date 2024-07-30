package model

import (
	"context"

	"bringyour.com/bringyour"
)

type CircleUC struct {
    NetworkId bringyour.Id `json:"network_id"`
    UserId bringyour.Id `json:"user_id"`
    CircleUCUserId bringyour.Id `json:"circle_uc_user_id"`
}

func GetCircleUCByCircleUCUserId(
    ctx context.Context, 
    circleUCUserId bringyour.Id,
) *CircleUC {
    var circleUC *CircleUC

    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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

        bringyour.WithPgResult(result, err, func() {
            if result.Next() {
                circleUC = &CircleUC{}
                bringyour.Raise(result.Scan(&circleUC.NetworkId, &circleUC.UserId, &circleUC.CircleUCUserId))
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
    networkId bringyour.Id,
    userId bringyour.Id,
) (circleUserId bringyour.Id) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
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
        bringyour.WithPgResult(result, err, func() {
            if result.Next() {
                bringyour.Raise(result.Scan(&circleUserId))
                set = true
            }
        })

        if set {
            return
        }

        circleUserId = bringyour.NewId()

        bringyour.RaisePgResult(tx.Exec(
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
    networkId bringyour.Id,
    userId bringyour.Id,
    circleUserId bringyour.Id,
) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        bringyour.RaisePgResult(tx.Exec(
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

