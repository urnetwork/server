package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

type NetworkPoint struct {
	NetworkId  server.Id `json:"network_id"`
	Event      string    `json:"event"`
	PointValue int       `json:"point_value"`
	CreateTime time.Time `json:"create_time"`
}

func ApplyNetworkPoints(ctx context.Context, networkId server.Id, event string) error {
	// Implement the logic to apply network points here
	// This is a placeholder implementation

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_point (
						network_id,
						event
				)
				VALUES ($1, $2)
			`,
			networkId,
			event,
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
						np.network_id,
						np.event,
						np.create_time,
						npe.point_value
				FROM 
						network_point np
				JOIN 
						network_point_event npe ON np.event = npe.event
				WHERE 
						np.network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {

				networkPoint := NetworkPoint{}
				server.Raise(result.Scan(
					&networkPoint.NetworkId,
					&networkPoint.Event,
					&networkPoint.CreateTime,
					&networkPoint.PointValue,
				))
				networkPoints = append(networkPoints, networkPoint)
			}
		})
	})

	return networkPoints
}
