package bwclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/protocol"
	"github.com/urnetwork/server/measure-throughput/jwtutil"
)

func CreateDeviceClient(ctx context.Context, apiURL, connectURL, byClientJwt string) (*connect.Client, error) {

	clientId, err := jwtutil.ParseClientID(byClientJwt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse client id: %w", err)
	}

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)

	clientOob := connect.NewApiOutOfBandControl(ctx, clientStrategy, byClientJwt, apiURL)
	// connectClient := connect.NewClientWithDefaults(ctx, *clientId, clientOob)

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
