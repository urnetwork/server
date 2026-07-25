package controller

import (
	"context"
	// "strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func ConnectNetworkClient(
	ctx context.Context,
	clientId server.Id,
	clientAddress string,
	handlerId server.Id,
	retryLocationTimeout time.Duration,
) (connectionId server.Id, clientAddressHash [32]byte, err error) {
	var clientIp string
	connectionId, clientIp, _, clientAddressHash, err = model.ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
	if err != nil {
		return
	}

	locationErr := SetConnectionLocation(ctx, connectionId, clientIp)
	if locationErr != nil && 0 < retryLocationTimeout {
		// keep the client ip in memory and do not persist to task, etc
		// the retry remains active as long as the context (which should be the connection context)
		go server.HandleError(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(retryLocationTimeout):
				}

				locationErr := SetConnectionLocation(ctx, connectionId, clientIp)
				if locationErr == nil {
					return
				}
			}
		})
	}

	return
}

// FIXME store the result in a db by client address hash
// FIXME the client ip should be in memory only, never persisted
// FIXME consider using dbip+a latency test for quality metrics

func SetConnectionLocation(
	ctx context.Context,
	connectionId server.Id,
	clientIp string,
) error {
	// a provider probed through its own egress is located from that probe, not
	// from a lookup on its control-connection ip: the egress is where user
	// traffic actually exits, and the probed value is cross-checked across
	// several sources. see
	// docs/superpowers/specs/2026-07-24-provider-egress-geolocation-design.md
	if clientId := model.GetNetworkClientForConnection(ctx, connectionId); clientId != nil {
		if egress := model.GetFreshProviderEgressLocation(
			ctx,
			*clientId,
			model.ProviderEgressLocationMaxAge,
		); egress != nil {
			scores := &model.ConnectionLocationScores{}
			if egress.Hosting {
				scores.NetTypeHosting = 1
			}
			if egress.Proxy {
				scores.NetTypePrivacy = 1
			}
			if egress.Mobile {
				scores.NetTypeVirtual = 1
			}
			err := model.SetConnectionLocation(ctx, connectionId, egress.LocationId, scores)
			if err == nil {
				return nil
			}
			// fall through to the mmdb path on a storage error
			glog.Infof("[ncc][%s]could not set probed egress location. err = %s\n", connectionId, err)
		}
	}

	location, connectionLocationScores, err := GetLocationForIp(ctx, clientIp)
	if err != nil {
		// server.Logger().Printf("Get ip for location error: %s", err)
		glog.Infof("[ncc][%s]could not find client location. err = %s\n", connectionId, err)
		return err
	}

	model.CreateLocation(ctx, location)
	err = model.SetConnectionLocation(ctx, connectionId, location.LocationId, connectionLocationScores)
	if err != nil {
		// server.Logger().Printf("Get ip for location error: %s", err)
		glog.Infof("[ncc][%s]could set connection location. err = %s\n", connectionId, err)
		return err
	}
	return nil
}

/*
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
*/
