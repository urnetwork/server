package clientdevice

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"bringyor.com/measure-throughput/clientdevice/netstack"
	"bringyor.com/measure-throughput/jwtutil"
	"bringyour.com/connect"
	"bringyour.com/protocol"
)

type ClientDevice struct {
	dev netstack.Device
	*netstack.Net
}

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

	dev, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("192.168.3.3")}, []netip.Addr{netip.MustParseAddr("100.100.100.100")}, 1500)
	if err != nil {
		return nil, fmt.Errorf("create net tun failed: %w", err)
	}

	mc := connect.NewRemoteUserNatMultiClientWithDefaults(
		ctx,
		generator,
		func(source connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {
			_, err := dev.Write(packet)
			if err != nil {
				fmt.Println("packet write error:", err)
			}
		},
		protocol.ProvideMode_Network,
	)

	source := connect.SourceId(*myID)

	go func() {
		packet := make([]byte, 2000)
		for ctx.Err() == nil {
			n, err := dev.Read(packet)
			if err != nil {
				fmt.Println("read error:", err)
				return
			}
			packet = packet[:n]
			sent := mc.SendPacket(
				source,
				protocol.ProvideMode_Network,
				packet,
				time.Second*15,
			)
			if !sent {
				fmt.Println("packet not sent")
			}
		}
		mc.Close()
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
