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
	connectClient := connect.NewClientWithDefaults(ctx, *clientId, clientOob)

	instanceId := connect.NewId()

	auth := &connect.ClientAuth{
		ByJwt:      byClientJwt,
		InstanceId: instanceId,
		AppVersion: "1.0.0",
	}

	connect.NewPlatformTransportWithDefaults(ctx, clientStrategy, connectClient.RouteManager(), connectURL, auth)

	return connectClient, nil
}
