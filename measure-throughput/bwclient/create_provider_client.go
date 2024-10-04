package bwclient

import (
	"context"
	"fmt"

	"bringyor.com/measure-throughput/jwtutil"
	"bringyour.com/connect"
)

func CreateProviderClient(ctx context.Context, apiURL, connectURL, byClientJwt string) (*connect.Client, error) {
	// parse the clientId
	clientId, err := jwtutil.ParseClientID(byClientJwt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse client id: %w", err)
	}

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)

	clientOob := connect.NewApiOutOfBandControl(ctx, clientStrategy, byClientJwt, apiURL)
	settings := connect.DefaultClientSettings()

	// settings.SendBufferSettings.SequenceBufferSize = 0
	// settings.SendBufferSettings.AckBufferSize = 0
	// settings.SendBufferSettings.AckTimeout = 90 * time.Second
	// settings.SendBufferSettings.IdleTimeout = 180 * time.Second
	// settings.ReceiveBufferSettings.SequenceBufferSize = 0
	// settings.ReceiveBufferSettings.GapTimeout = 90 * time.Second
	// settings.ReceiveBufferSettings.IdleTimeout = 180 * time.Second

	connectClient := connect.NewClient(
		ctx,
		*clientId,
		clientOob,
		settings,
	)

	instanceId := connect.NewId()

	auth := &connect.ClientAuth{
		ByJwt:      byClientJwt,
		InstanceId: instanceId,
		AppVersion: "1.0.0",
	}

	connect.NewPlatformTransportWithDefaults(ctx, clientStrategy, connectClient.RouteManager(), connectURL, auth)

	return connectClient, nil
}
