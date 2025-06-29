package controller

import (
	"context"
	// "strings"
	"time"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func ConnectNetworkClient(
	ctx context.Context,
	clientId server.Id,
	clientAddress string,
	handlerId server.Id,
) server.Id {
	connectionId := model.ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
	// server.Logger().Printf("Parse client address: %s", clientAddress)

	if ipStr, _, err := server.ParseClientAddress(clientAddress); err == nil {
		go server.HandleError(func() {
			SetConnectionLocation(ctx, connectionId, ipStr)
		})
	}
	return connectionId
}

func SetConnectionLocation(
	ctx context.Context,
	connectionId server.Id,
	ipStr string,
) error {
	location, connectionLocationScores, err := GetLocationForIp(ctx, ipStr)
	if err != nil {
		// server.Logger().Printf("Get ip for location error: %s", err)
		glog.Infof("[ncc][%s]Could not find client location. Skipping. err = %s %s\n", connectionId, err, ipStr)
		return err
	}

	model.CreateLocation(ctx, location)
	model.SetConnectionLocation(ctx, connectionId, location.LocationId, connectionLocationScores)
	return nil
}

func SetMissingConnectionLocations(ctx context.Context, minTime time.Time) {
	connectionIpStrs := map[server.Id]string{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
				    network_client_connection.connection_id,
				    network_client_connection.client_address
				FROM network_client_connection
				
				LEFT JOIN network_client_location ON network_client_location.connection_id = network_client_connection.connection_id
				
				WHERE
					network_client_connection.connect_time < $1 AND
					network_client_connection.connected AND
					network_client_location.connection_id IS NULL
			`,
			minTime,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var connectionId server.Id
				var clientAddress string
				server.Raise(result.Scan(
					&connectionId,
					&clientAddress,
				))
				host, _, err := server.ParseClientAddress(clientAddress)
				if err == nil {
					connectionIpStrs[connectionId] = host
				} else {
					glog.Infof("[ncc][%s]Could not parse client address. Skipping.\n", connectionId)
				}
			}
		})
	})

	for connectionId, ipStr := range connectionIpStrs {
		SetConnectionLocation(ctx, connectionId, ipStr)
	}
}
