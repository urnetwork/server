package clientdevice

import (
	"context"
	"fmt"
	"log/slog"
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
	"gvisor.dev/gvisor/pkg/tcpip"
)

type ClientDevice struct {
	dev netstack.Device
	*netstack.Net
}

var dropProbability = 0.00

var packetDelay = 0 * time.Millisecond

func Start(
	ctx context.Context,
	byJWT string,
	apiURL string,
	connectURL string,
	providerID connect.Id,
	logTCP bool,
	useMultiClient bool,
) (*ClientDevice, error) {

	myID, err := jwtutil.ParseClientID(byJWT)
	if err != nil {
		return nil, fmt.Errorf("failed to parse client id: %w", err)
	}

	dev, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("192.168.3.3")}, []netip.Addr{netip.MustParseAddr("100.100.100.100")}, 1500)
	if err != nil {
		return nil, fmt.Errorf("create net tun failed: %w", err)
	}

	var tl *tcplogger.TCPLogger

	if logTCP {
		tl, err = tcplogger.NewLogger("/tmp/client-remote-nat.csv")
		if err != nil {
			return nil, fmt.Errorf("failed to create tcp logger: %w", err)
		}
		go func() {
			<-ctx.Done()
			tl.Close()
		}()
	}

	if useMultiClient {

		generator := connect.NewApiMultiClientGenerator(
			ctx,
			[]*connect.ProviderSpec{
				{
					ClientId: &providerID,
				},
			},
			connect.NewClientStrategyWithDefaults(ctx),
			// exclude self
			[]connect.Id{
				*myID,
			},
			apiURL,
			byJWT,
			connectURL,
			"my device",
			"test",
			"1.2.3",
			// connect.DefaultClientSettingsNoNetworkEvents,
			connect.DefaultClientSettings,
			connect.DefaultApiMultiClientGeneratorSettings(),
		)

		inboundDelayedQueue := delayqueue.New(ctx, 30000)

		mc := connect.NewRemoteUserNatMultiClientWithDefaults(
			ctx,
			generator,
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
				// }()
			},
			protocol.ProvideMode_Network,
		)

		outboundDelayedQueue := delayqueue.New(ctx, 30000)

		mc.Monitor().AddMonitorEventCallback(func(windowExpandEvent *connect.WindowExpandEvent, providerEvents map[connect.Id]*connect.ProviderEvent) {
			slog.Info("monitor event", "event", windowExpandEvent)
		})

		go func() {

			for ctx.Err() == nil {
				packet := make([]byte, 2000)
				n, err := dev.Read(packet)
				if err != nil {
					fmt.Println("read error:", err)
					return
				}
				packet = packet[:n]

				if rand.Float64() < dropProbability {
					fmt.Println("dropping outgoing packet")
					continue
				}

				source := connect.SourceId(*myID)

				outboundDelayedQueue.GoDelayed(packetDelay, func() {

					tl.Log(packet)

					_ = mc.SendPacket(
						source,
						protocol.ProvideMode_Network,
						packet,
						time.Second*15,
					)
				})

			}
			mc.Close()
		}()

		return &ClientDevice{
			dev: dev,
			Net: tnet,
		}, nil

	}

	source := connect.SourceId(*myID)

	cl, err := bwclient.CreateDeviceClient(ctx, apiURL, connectURL, byJWT)
	if err != nil {
		return nil, fmt.Errorf("create device client failed: %w", err)
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

			if rand.Float64() < dropProbability {
				fmt.Println("dropping outgoing packet")
				continue
			}

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
		nc.Close()
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

func (cd *ClientDevice) GetStats() tcpip.Stats {
	return cd.dev.Stats()
}
