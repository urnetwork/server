package main

import (
	"context"
	"fmt"

	"bringyor.com/measure-throughput/jwtutil"
	"bringyour.com/connect"
	"bringyour.com/protocol"
	"github.com/jedib0t/go-pretty/v6/progress"
)

const apiURL = "http://localhost:8080"
const connectURL = "ws://localhost:7070"

func runProvider(ctx context.Context, byClientJwt string, pw progress.Writer) (err error) {

	defer func() {
		if ctx.Err() != nil {
			err = nil
		}
	}()

	tracker := &progress.Tracker{
		Message: "Provider is running",
		Total:   0,
	}

	pw.AppendTracker(tracker)
	tracker.Start()

	defer func() {
		if err != nil {
			tracker.UpdateMessage(fmt.Sprintf("Provider failed: %v", err))
			tracker.MarkAsErrored()
			return
		}
		tracker.UpdateMessage("Provider is done")
		tracker.MarkAsDone()
	}()

	// parse the clientId
	clientId, err := jwtutil.ParseClientID(byClientJwt)
	if err != nil {
		return fmt.Errorf("failed to parse client id: %w", err)
	}

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)

	clientOob := connect.NewApiOutOfBandControl(ctx, clientStrategy, byClientJwt, apiURL)
	connectClient := connect.NewClientWithDefaults(ctx, *clientId, clientOob)

	instanceId := connect.NewId()

	auth := &connect.ClientAuth{
		ByJwt: byClientJwt,
		// ClientId: clientId,
		InstanceId: instanceId,
		AppVersion: "1.0.0",
	}

	connect.NewPlatformTransportWithDefaults(ctx, clientStrategy, connectClient.RouteManager(), connectURL, auth)
	// go platformTransport.Run(connectClient.RouteManager())

	localUserNat := connect.NewLocalUserNatWithDefaults(ctx, clientId.String())
	remoteUserNatProvider := connect.NewRemoteUserNatProviderWithDefaults(connectClient, localUserNat)

	provideModes := map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Public:  true,
		protocol.ProvideMode_Network: true,
	}

	connectClient.ContractManager().SetProvideModes(provideModes)

	defer remoteUserNatProvider.Close()
	defer localUserNat.Close()
	defer connectClient.Cancel()

	<-ctx.Done()

	return nil

}
