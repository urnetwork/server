package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

type NetworkPointEvent string

/**
 * Point Events
 */
const (
	NetworkPointEventReferral NetworkPointEvent = "referral"
)

/**
 * Point Values
 */
const (
	NetworkPointEventReferralValue = 10
)

type NetworkPoint struct {
	NetworkId  server.Id `json:"network_id"`
	Event      string    `json:"event"`
	PointValue int       `json:"point_value"`
	CreateTime time.Time `json:"create_time"`
}

func ApplyNetworkPoints(
	ctx context.Context,
	networkId server.Id,
	event NetworkPointEvent,
	pointValue int,
) error {
	// Implement the logic to apply network points here
	// This is a placeholder implementation

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_point (
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

func FetchNetworkPoints(ctx context.Context, networkId server.Id) (networkPoints []NetworkPoint) {

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
						network_point
				WHERE 
						network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {

				networkPoint := NetworkPoint{}
				server.Raise(result.Scan(
					&networkPoint.NetworkId,
					&networkPoint.Event,
					&networkPoint.PointValue,
					&networkPoint.CreateTime,
				))
				networkPoints = append(networkPoints, networkPoint)
			}
		})
	})

	return networkPoints
}
