package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

type AccountPointEvent string

/**
 * Point Events
 */
const (
	AccountPointEventPayout              AccountPointEvent = "payout"
	AccountPointEventPayoutLinkedAccount AccountPointEvent = "payout_linked_account"
	AccountPointEventPayoutMultiplier    AccountPointEvent = "payout_multiplier"
)

type AccountPoint struct {
	NetworkId       server.Id  `json:"network_id"`
	Event           string     `json:"event"`
	PointValue      int        `json:"point_value"`
	PaymentPlanId   *server.Id `json:"payment_plan_id,omitempty"`
	LinkedNetworkId *server.Id `json:"linked_network_id,omitempty"`
	CreateTime      time.Time  `json:"create_time"`
}

type ApplyAccountPointsArgs struct {
	NetworkId       server.Id
	Event           AccountPointEvent
	PointValue      NanoPoints
	PaymentPlanId   *server.Id
	LinkedNetworkId *server.Id
}

func ApplyAccountPoints(
	ctx context.Context,
	args ApplyAccountPointsArgs,
) error {
	// Implement the logic to apply network points here
	// This is a placeholder implementation

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO account_point (
						network_id,
						event,
						point_value,
						payment_plan_id,
						linked_network_id
				)
				VALUES ($1, $2, $3, $4, $5)
			`,
			args.NetworkId,
			args.Event,
			args.PointValue,
			args.PaymentPlanId,
			args.LinkedNetworkId,
		))
	})

	return nil
}

func FetchAccountPoints(ctx context.Context, networkId server.Id) (accountPoints []AccountPoint) {

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
						network_id,
						event,
						point_value,
						payment_plan_id,
						linked_network_id,
						create_time
				FROM
						account_point
				WHERE
						network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {

				accountPoint := AccountPoint{}
				server.Raise(result.Scan(
					&accountPoint.NetworkId,
					&accountPoint.Event,
					&accountPoint.PointValue,
					&accountPoint.PaymentPlanId,
					&accountPoint.LinkedNetworkId,
					&accountPoint.CreateTime,
				))
				accountPoints = append(accountPoints, accountPoint)
			}
		})
	})

	return accountPoints
}
