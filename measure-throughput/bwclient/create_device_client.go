package bwclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"bringyor.com/measure-throughput/jwtutil"
	"bringyour.com/connect"
	"bringyour.com/protocol"
)

func CreateDeviceClient(ctx context.Context, apiURL, connectURL, byClientJwt string) (*connect.Client, error) {

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

	ack := make(chan struct{})
	connectClient.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
		map[protocol.ProvideMode]bool{},
		func(err error) {
			close(ack)
		},
	)
	select {
	case <-ack:
	case <-time.After(3 * time.Second):
		connectClient.Cancel()
		return nil, errors.New("could not enable return traffic for client")
	}

	return connectClient, nil

	// connectClient.ContractManager().CreateContract(connect.ContractKey{
	// 	Destination: connect.DestinationId(providerID),
	// }, 5*time.Second)

}
