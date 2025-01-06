package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/protocol"
	"github.com/urnetwork/server/httproxy/connprovider"
	"github.com/urnetwork/server/httproxy/netstack"
)

type ProxyStats struct {
	ConnectionsOpen       uint64 `json:"connections_open"`
	TotalConnectionsCount uint64 `json:"total_connections_count"`
	BytesSent             uint64 `json:"bytes_sent"`
	BytesReceived         uint64 `json:"bytes_received"`
	mu                    sync.Mutex
}

func (p *ProxyStats) connectionOpened() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ConnectionsOpen += 1
	p.TotalConnectionsCount += 1
}

func (p *ProxyStats) connectionClosed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ConnectionsOpen -= 1
}

func (p *ProxyStats) bytesSent(n uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.BytesSent += n
}

func (p *ProxyStats) bytesReceived(n uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.BytesReceived += n
}

type ClientProxy struct {
	connectionInfo *connprovider.ConnectionInfo
	generator      *connect.ApiMultiClientGenerator
	dev            netstack.Device
	nc             *connect.RemoteUserNatMultiClient
	proxyServer    *goproxy.ProxyHttpServer
	http.Handler
}

func NewClientProxy(ctx context.Context, cc ConnectionConfig) (*ClientProxy, error) {

	provider := connprovider.New(ctx, cc.ApiURL)

	var connInfo *connprovider.ConnectionInfo
	var err error

	connInfo, err = provider.ConnectWithToken(
		cc.JWT,
		connprovider.ConnectionOptions{
			DeviceDescription: "my device",
			DeviceSpec:        "httpproxy",
		},
	)
	if err != nil {
		return nil, fmt.Errorf("connection with JWT failed: %w", err)
	}

	stats := &ProxyStats{}

	generator := connect.NewApiMultiClientGenerator(
		ctx,
		connInfo.ProvidersSpec,
		connect.NewClientStrategyWithDefaults(ctx),
		// exclude self
		[]connect.Id{
			connInfo.ClientID,
		},
		cc.ApiURL,
		connInfo.ClientJWT,
		cc.PlatformURL,
		"my device",
		"httpproxy",
		"1.2.3",
		// connect.DefaultClientSettingsNoNetworkEvents,
		connect.DefaultClientSettings,
		connect.DefaultApiMultiClientGeneratorSettings(),
	)

	dev, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("192.168.3.3")}, 1500)
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
			stats.bytesReceived(uint64(len(packet)))
		},
		protocol.ProvideMode_Network,
	)

	source := connect.SourceId(connInfo.ClientID)

	go func() {
		for {
			packet := make([]byte, 1500)
			n, err := dev.Read(packet)
			if err != nil {
				fmt.Println("read error:", err)
				return
			}
			packet = packet[:n]
			mc.SendPacket(
				source,
				protocol.ProvideMode_Network,
				packet,
				time.Second*15,
			)
			stats.bytesSent(uint64(len(packet)))
		}
	}()

	proxy := goproxy.NewProxyHttpServer()

	proxy.Tr = &http.Transport{
		Dial: func(network, addr string) (net.Conn, error) {
			return tnet.DialContext(context.Background(), network, addr)
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ap, err := netip.ParseAddrPort(addr)
			if err != nil {
				return nil, err
			}

			return tnet.DialContextTCP(ctx, net.TCPAddrFromAddrPort(ap))
		},
	}

	proxy.ConnectDialWithReq = func(req *http.Request, network string, addr string) (net.Conn, error) {
		return tnet.DialContext(req.Context(), network, addr)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxy/stats" && r.URL.Host == "" {
			w.Header().Set("Access-Control-Allow-Origin", "*")

			if r.Method == "OPTIONS" {
				w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "*")
				w.WriteHeader(http.StatusOK)
				return
			}

			if r.Method == "GET" {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(stats)
				return
			}
		}

		stats.connectionOpened()
		defer stats.connectionClosed()

		proxy.ServeHTTP(w, r)
	})

	return &ClientProxy{
		connectionInfo: connInfo,
		generator:      generator,
		dev:            dev,
		nc:             mc,
		proxyServer:    proxy,
		Handler:        handler,
	}, nil
}
