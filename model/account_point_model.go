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
	AccountPointEventReferral AccountPointEvent = "referral"
	AccountPointEventPayout   AccountPointEvent = "payout"
)

/**
 * Point Values
 */
const (
	AccountPointEventReferralValue = 10
)

type AccountPoint struct {
	NetworkId  server.Id `json:"network_id"`
	Event      string    `json:"event"`
	PointValue int       `json:"point_value"`
	CreateTime time.Time `json:"create_time"`
}

func ApplyAccountPoints(
	ctx context.Context,
	networkId server.Id,
	event AccountPointEvent,
	pointValue NanoPoints,
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
						point_value
				)
				VALUES ($1, $2, $3)
			`,
			networkId,
			event,
			pointValue,
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
					&accountPoint.CreateTime,
				))
				accountPoints = append(accountPoints, accountPoint)
			}
		})
	})

	return accountPoints
}
