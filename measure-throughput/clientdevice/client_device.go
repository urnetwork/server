package clientdevice

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"time"

	"bringyor.com/measure-throughput/clientdevice/netstack"
	"bringyor.com/measure-throughput/jwtutil"
	"bringyour.com/connect"
	"bringyour.com/protocol"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
)

type ClientDevice struct {
	dev netstack.Device
	*netstack.Net
}

const capFile = "/tmp/cap.pcap"

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

	f, err := os.OpenFile(capFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open cap file: %w", err)
	}

	pcapWriter := pcapgo.NewWriter(f)
	err = pcapWriter.WriteFileHeader(65536, layers.LinkTypeIPv4)
	if err != nil {
		return nil, fmt.Errorf("failed to write pcap file header: %w", err)
	}

	context.AfterFunc(ctx, func() {
		f.Close()
	})

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

			pcapWriter.WritePacket(gopacket.CaptureInfo{
				Timestamp:      time.Now(),
				CaptureLength:  len(packet),
				Length:         len(packet),
				InterfaceIndex: 0,
			}, packet)

			// fmt.Printf("\n\n\n\npacket from %v: %v\n\n\n\n", source, packet)
			_, err := dev.Write(packet)
			if err != nil {
				fmt.Println("packet write error:", err)
			}
		},
		protocol.ProvideMode_Network,
	)

	source := connect.SourceId(*myID)

	go func() {

		for ctx.Err() == nil {
			packet := make([]byte, 2000)
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
