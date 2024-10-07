package main

import (
	"context"
	"fmt"

	"bringyor.com/measure-throughput/bwclient"
	"bringyor.com/measure-throughput/jwtutil"
	"bringyor.com/measure-throughput/tcplogger"
	"bringyour.com/connect"
	"bringyour.com/protocol"
	"github.com/jedib0t/go-pretty/v6/progress"
)

const apiURL = "http://localhost:8080"
const connectURL = "ws://localhost:7070"

func runProvider(
	ctx context.Context,
	byClientJwt string,
	pw progress.Writer,
	logTCP bool,
) (err error) {

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

	connectClient, err := bwclient.CreateProviderClient(ctx, apiURL, connectURL, byClientJwt)
	if err != nil {
		return fmt.Errorf("failed to create provider client: %w", err)
	}

	var tl *tcplogger.TCPLogger

	if logTCP {
		tl, err = tcplogger.NewLogger("/tmp/provider-local-nat.csv")
		if err != nil {
			return fmt.Errorf("failed to create tcp logger: %w", err)
		}
	}

	connectClient.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
		for _, frame := range frames {
			switch frame.MessageType {
			case protocol.MessageType_IpIpPacketToProvider:
				ipPacketToProvider_, err := connect.FromFrame(frame)
				if err != nil {
					return
				}
				ipPacketToProvider, ok := ipPacketToProvider_.(*protocol.IpPacketToProvider)
				if !ok {
					pw.Log("failed to cast to IpPacketToProvider")
					return
				}

				packet := ipPacketToProvider.IpPacket.PacketBytes
				tl.Log(packet)

			}
		}
	})

	connectClient.AddForwardCallback(func(path connect.TransferPath, transferFrameBytes []byte) {
		fmt.Println("Provider received forward callback")
	})

	localUserNat := connect.NewLocalUserNatWithDefaults(ctx, clientId.String())
	remoteUserNatProvider := connect.NewRemoteUserNatProviderWithDefaults(connectClient, localUserNat)

	// remoteUserNatProvider.Receive()

	provideModes := map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Public:  true,
		protocol.ProvideMode_Network: true,
	}

	connectClient.ContractManager().SetProvideModes(provideModes)

	localUserNat.AddReceivePacketCallback(func(source connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {
		tl.Log(packet)
	})

	defer remoteUserNatProvider.Close()
	defer localUserNat.Close()
	defer connectClient.Cancel()

	<-ctx.Done()
	tl.Close()

	return nil

}
