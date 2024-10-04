package clientdevice

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/netip"
	"time"

	"bringyor.com/measure-throughput/bwclient"
	"bringyor.com/measure-throughput/clientdevice/netstack"
	"bringyor.com/measure-throughput/delayqueue"
	"bringyor.com/measure-throughput/jwtutil"
	"bringyor.com/measure-throughput/tcplogger"
	"bringyour.com/connect"
	"bringyour.com/protocol"
)

type ClientDevice struct {
	dev netstack.Device
	*netstack.Net
}

var dropProbability = 0.00

var packetDelay = time.Millisecond * 10

func Start(
	ctx context.Context,
	byJWT string,
	apiURL string,
	connectURL string,
	providerID connect.Id,
) (*ClientDevice, error) {

	myID, err := jwtutil.ParseClientID(byJWT)
	if err != nil {
		return nil, fmt.Errorf("failed to parse client id: %w", err)
	}

	// generator := connect.NewApiMultiClientGenerator(
	// 	ctx,
	// 	[]*connect.ProviderSpec{
	// 		{
	// 			ClientId: &providerID,
	// 		},
	// 	},
	// 	connect.NewClientStrategyWithDefaults(ctx),
	// 	// exclude self
	// 	[]connect.Id{
	// 		*myID,
	// 	},
	// 	apiURL,
	// 	byJWT,
	// 	connectURL,
	// 	"my device",
	// 	"test",
	// 	"1.2.3",
	// 	// connect.DefaultClientSettingsNoNetworkEvents,
	// 	connect.DefaultClientSettings,
	// 	connect.DefaultApiMultiClientGeneratorSettings(),
	// )

	dev, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("192.168.3.3")}, []netip.Addr{netip.MustParseAddr("100.100.100.100")}, 1500)
	if err != nil {
		return nil, fmt.Errorf("create net tun failed: %w", err)
	}

	// mc := connect.NewRemoteUserNatMultiClientWithDefaults(
	// 	ctx,
	// 	generator,
	// 	func(source connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {

	// 		if rand.Float64() < dropProbability {
	// 			fmt.Println("dropping incoming packet")
	// 			return
	// 		}

	// 		// go func() {
	// 		time.Sleep(packetDelay)

	// 		_, err := dev.Write(packet)
	// 		if err != nil {
	// 			// fmt.Println("packet write error:", err)
	// 		}
	// 		// }()
	// 	},
	// 	protocol.ProvideMode_Network,
	// )

	// mc.SendPacket()

	// source := connect.SourceId(*myID)

	cl, err := bwclient.CreateDeviceClient(ctx, apiURL, connectURL, byJWT)
	if err != nil {
		return nil, fmt.Errorf("create device client failed: %w", err)
	}

	tl, err := tcplogger.NewLogger("/tmp/client-remote-nat.csv")
	if err != nil {
		return nil, fmt.Errorf("failed to create tcp logger: %w", err)
	}

	inboundDelayedQueue := delayqueue.New(ctx, 30000)

	nc, err := connect.NewRemoteUserNatClient(
		cl,
		func(source connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {
			if rand.Float64() < dropProbability {
				fmt.Println("dropping incoming packet")
				return
			}

			inboundDelayedQueue.GoDelayed(packetDelay, func() {
				tl.Log(packet)
				_, err := dev.Write(packet)
				if err != nil {
				}
			})
		},
		[]connect.MultiHopId{
			connect.RequireMultiHopId(providerID),
		},
		protocol.ProvideMode_Network,
	)
	if err != nil {
		return nil, fmt.Errorf("create remote user nat client failed: %w", err)
	}

	source := connect.SourceId(*myID)

	// cl.ContractManager().CreateContract(connect.ContractKey{
	// 	Destination: connect.DestinationId(providerID),
	// }, 5*time.Second)

	// sendPacket := func(packet []byte, timeout time.Duration) error {
	// 	ipPacketToProvider := &protocol.IpPacketToProvider{
	// 		IpPacket: &protocol.IpPacket{
	// 			PacketBytes: packet,
	// 		},
	// 	}

	// 	frame, err := connect.ToFrame(ipPacketToProvider)
	// 	if err != nil {
	// 		return err
	// 	}

	// 	opts := []any{
	// 		// connect.ForceStream(),
	// 		connect.NoAck(),
	// 	}

	// 	_, err = cl.SendMultiHopWithTimeoutDetailed(
	// 		frame,
	// 		connect.RequireMultiHopId(providerID),
	// 		func(err error) {
	// 			fmt.Println("ack callback:", err)
	// 		},
	// 		time.Second,
	// 		opts...,
	// 	)
	// 	if err != nil {
	// 		return err
	// 	}

	// 	return nil

	// }

	outboundDelayedQueue := delayqueue.New(ctx, 30000)

	go func() {

		for ctx.Err() == nil {
			packet := make([]byte, 2000)
			n, err := dev.Read(packet)
			if err != nil {
				fmt.Println("read error:", err)
				return
			}
			packet = packet[:n]

			// if rand.Float64() < dropProbability {
			// 	fmt.Println("dropping outgoing packet")
			// 	continue
			// }

			outboundDelayedQueue.GoDelayed(packetDelay, func() {

				tl.Log(packet)

				_ = nc.SendPacket(
					source,
					protocol.ProvideMode_Network,
					packet,
					time.Second*15,
				)
			})

			// if !sent {
			// 	fmt.Println("packet not sent")
			// }
		}
		cl.Close()
	}()

	return &ClientDevice{
		dev: dev,
		Net: tnet,
	}, nil
}

func (cd *ClientDevice) Close() {
	cd.dev.Close()
}

func (cd *ClientDevice) Transport() *http.Transport {
	return &http.Transport{
		DialContext: cd.Net.DialContext,
	}
}
