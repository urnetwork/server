package main

import (
	"testing"

	"context"
	"net/http"
	"net/url"
	// "net"
	"encoding/json"
	"fmt"
	"io"
	"time"
	// "strings"
	"crypto/tls"
	mathrand "math/rand"

	"golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

func TestConnectProxy(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		testConnectProxy(t)
	})
}

func testConnectProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	maxMessageContentSize := model.ByteCount(4096)

	// sequenceIdleTimeout := 100 * time.Millisecond
	// receiveTimeout := 900 * time.Second
	// minResendInterval := 10 * time.Millisecond
	// standardContractTransferByteCount := 4 * maxMessageContentSize
	// standardContractFillFraction := float32(0.5)

	service := "connect"
	block := "test"

	routes := map[string]string{}
	for i := 0; i < 10; i += 1 {
		host := fmt.Sprintf("host%d", i)
		routes[host] = "127.0.0.1"
	}

	// FIXME server quic tls to create a temp cert
	// FIXME client tls to trust self signed cert
	createServer := func(exchange *Exchange, port int) *http.Server {
		settings := DefaultConnectHandlerSettings()
		// settings.EnableTlsSelfSign = true
		settings.ListenH3Port = port + 443
		settings.ListenDnsPort = port + 53
		settings.EnableProxyProtocol = false
		settings.FramerSettings.MaxMessageLen = int(2 * maxMessageContentSize)
		settings.TransportTlsSettings.EnableSelfSign = true
		settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
		settings.ConnectionAnnounceTimeout = 0
		// settings.ConnectionRateLimitSettings.MaxTotalConnectionCount = 1000
		settings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
		// settings.MaximumExchangeMessageByteCount = 2 * int(messageContentSizes[len(messageContentSizes)-1])

		proxySettings := DefaultProxyConnectHandlerSettings()
		// settings.EnableTlsSelfSign = true
		proxySettings.ListenSocksPort = port + 1080
		proxySettings.EnableProxyProtocol = false
		connectRouter := NewConnectRouter(ctx, cancel, exchange, settings, proxySettings)

		transportTlsSettings := DefaultTransportTlsSettings()
		transportTlsSettings.EnableSelfSign = true
		transportTls, err := NewTransportTlsFromConfig(transportTlsSettings)
		assert.Equal(t, err, nil)

		tlsConfig := &tls.Config{
			GetConfigForClient: transportTls.GetTlsConfigForClient,
		}

		fmt.Printf("create server :%d (:%d :%d :%d)\n", port, settings.ListenH3Port, settings.ListenDnsPort, proxySettings.ListenSocksPort)

		routes := []*router.Route{
			router.NewRoute("GET", "/status", router.WarpStatus),
			router.NewRoute("GET", "/", connectRouter.Connect),
			router.NewRoute("CONNECT", "", connectRouter.ProxyConnect),
		}

		addr := fmt.Sprintf(":%d", port)
		routerHandler := router.NewRouter(ctx, routes)

		server := &http.Server{
			Addr:      addr,
			Handler:   routerHandler,
			TLSConfig: tlsConfig,
		}
		return server
	}

	hostPorts := map[string]int{}
	exchanges := map[string]*Exchange{}
	servers := map[string]*http.Server{}
	for i := 0; i < 10; i += 1 {
		host := fmt.Sprintf("host%d", i)
		port := 8080 + i
		hostPorts[host] = port

		hostToServicePorts := map[int]int{
			9000 + i: 9000 + i,
		}

		settings := DefaultExchangeSettings()
		settings.ExchangeBufferSize = 0
		// settings.ResidentIdleTimeout = sequenceIdleTimeout
		// settings.ForwardIdleTimeout = sequenceIdleTimeout
		settings.FramerSettings.MaxMessageLen = int(2 * maxMessageContentSize)
		settings.ForwardEnforceActiveContracts = true

		exchange := NewExchange(ctx, host, service, block, hostToServicePorts, routes, settings)
		exchanges[host] = exchange

		server := createServer(exchange, port)
		servers[host] = server
		defer server.Close()
		go server.ListenAndServeTLS("", "")
	}

	select {
	case <-time.After(2 * time.Second):
	}

	// 127.0.0.1 will proxy in go
	// see https://github.com/golang/go/issues/28866

	randServerUrl := func() (string, int) {
		ports := maps.Values(hostPorts)
		port := ports[mathrand.Intn(len(ports))]
		return fmt.Sprintf("wss://local-test.bringyour.com:%d", port), port
	}

	randServerHostPort := func() (string, int) {
		ports := maps.Values(hostPorts)
		port := ports[mathrand.Intn(len(ports))]
		return fmt.Sprintf("local-test.bringyour.com:%d", port), port
	}

	// connect a new provider
	runProvider := func() (providerClientId server.Id) {
		networkId := server.NewId()
		userId := server.NewId()
		providerInstanceId := connect.NewId()

		model.Testing_CreateNetwork(
			ctx,
			networkId,
			networkId.String(),
			userId,
		)

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		// add balance
		func() {
			initialTransferBalance := ByteCount(1024) * ByteCount(1024) * ByteCount(1024) * ByteCount(1024)

			subscriptionYearDuration := 365 * 24 * time.Hour

			balanceCode, err := model.CreateBalanceCode(
				ctx,
				initialTransferBalance,
				subscriptionYearDuration,
				0,
				networkId.String(),
				networkId.String(),
				"",
			)
			assert.Equal(t, nil, err)

			result, err := model.RedeemBalanceCode(
				&model.RedeemBalanceCodeArgs{
					Secret: balanceCode.Secret,
				},
				session,
			)
			assert.Equal(t, nil, err)
			assert.Equal(t, nil, result.Error)
		}()

		args := &model.AuthNetworkClientArgs{
			Description: "test",
			DeviceSpec:  "test",
		}
		result, err := model.AuthNetworkClient(args, session)
		assert.Equal(t, err, nil)
		// session := session.Testing_CreateClientSession(ctx, providerByJwt)

		providerClientId = *result.ClientId
		providerByJwt := *result.ByClientJwt

		clientStrategySettings := connect.DefaultClientStrategySettings()
		// clientStrategySettings.ProxySettings = proxySettings
		clientSettings := connect.DefaultClientSettings()
		localUserNatSettings := connect.DefaultLocalUserNatSettings()
		localUserNatSettings.TcpBufferSettings.ConnectSettings = clientStrategySettings.ConnectSettings
		localUserNatSettings.UdpBufferSettings.ConnectSettings = clientStrategySettings.ConnectSettings
		remoteUserNatProviderSettings := connect.DefaultRemoteUserNatProviderSettings()

		providerClient := connect.NewClient(ctx, connect.Id(providerClientId), Testing_NewControllerOutOfBandControl(ctx, providerClientId), clientSettings)
		// defer providerClient.Close()

		providerAuth := &connect.ClientAuth{
			ByJwt: providerByJwt,
			// ClientId: clientId,
			InstanceId: providerInstanceId,
			AppVersion: "0.0.0",
		}

		providerClientStrategy := connect.NewClientStrategy(ctx, clientStrategySettings)

		platformUrl, port := randServerUrl()
		providerTransportSettings := connect.DefaultPlatformTransportSettings()
		providerTransportSettings.QuicTlsConfig.InsecureSkipVerify = true
		providerTransportSettings.H3Port = port + 443
		providerTransportSettings.DnsPort = port + 53
		providerTransportSettings.FramerSettings.MaxMessageLen = int(2 * maxMessageContentSize)
		connect.NewPlatformTransport(
			ctx,
			providerClientStrategy,
			providerClient.RouteManager(),
			platformUrl,
			providerAuth,
			providerTransportSettings,
		)
		// defer transport.Close()

		localUserNat := connect.NewLocalUserNat(ctx, providerClientId.String(), localUserNatSettings)
		// defer localUserNat.Close()
		connect.NewRemoteUserNatProvider(providerClient, localUserNat, remoteUserNatProviderSettings)
		// defer remoteUserNatProvider.Close()

		provideModes := map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Public:  true,
			protocol.ProvideMode_Network: true,
		}
		providerClient.ContractManager().SetProvideModes(provideModes)

		return
	}

	providerClientIds := []server.Id{}
	for range 2 {
		providerClientId := runProvider()
		// providerClientId := server.NewId()
		providerClientIds = append(providerClientIds, providerClientId)
	}

	select {
	case <-time.After(2 * time.Second):
	}

	for range 2 {
		// create a proxy client, connect to a random server, and route to a random provider

		providerClientId := providerClientIds[mathrand.Intn(len(providerClientIds))]

		networkId := server.NewId()
		userId := server.NewId()

		model.Testing_CreateNetwork(
			ctx,
			networkId,
			networkId.String(),
			userId,
		)

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		// add balance
		func() {
			initialTransferBalance := ByteCount(1024) * ByteCount(1024) * ByteCount(1024) * ByteCount(1024)

			subscriptionYearDuration := 365 * 24 * time.Hour

			balanceCode, err := model.CreateBalanceCode(
				ctx,
				initialTransferBalance,
				subscriptionYearDuration,
				0,
				networkId.String(),
				networkId.String(),
				"",
			)
			assert.Equal(t, nil, err)

			result, err := model.RedeemBalanceCode(
				&model.RedeemBalanceCodeArgs{
					Secret: balanceCode.Secret,
				},
				session,
			)
			assert.Equal(t, nil, err)
			assert.Equal(t, nil, result.Error)
		}()

		args := &model.AuthNetworkClientArgs{
			Description: "test",
			DeviceSpec:  "test",

			ProxyConfig: &model.ProxyConfig{
				InitialDeviceState: &model.ProxyDeviceState{
					Location: &sdk.ConnectLocation{
						ConnectLocationId: &sdk.ConnectLocationId{
							ClientId: server.ToSdkId(providerClientId),
						},
					},
				},
			},
		}
		result, err := model.AuthNetworkClient(args, session)
		assert.Equal(t, err, nil)

		// httpsProxyUrlStr := result.ProxyConfigResult.HttpsProxyUrl
		// for the test we do not use tls
		// TODO for some reason go does not pass the host header in the proxy
		// TODO force the auth token
		// FIXME see ProxyConnectHeader
		addr, _ := randServerHostPort()
		httpsProxyUrlStr := fmt.Sprintf("https://%s", addr)
		httpsProxyUrl, err := url.Parse(httpsProxyUrlStr)
		assert.Equal(t, err, nil)

		// proxyDialer := &net.Dialer{
		// 	Timeout: 15 * time.Second,
		// 	KeepAliveConfig: net.KeepAliveConfig{
		// 		Enable: true,
		// 	},
		// }

		proxyConnectHeader := http.Header{}
		proxyConnectHeader.Add("Proxy-Authorization", fmt.Sprintf("Bearer %s", result.ProxyConfigResult.AuthToken))

		proxyHttpClient := &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				// TLSHandshakeTimeout: 15 * time.Second,
				Proxy: http.ProxyURL(httpsProxyUrl),
				// DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				// 	addr, _ = randServerHostPort()
				// 	fmt.Printf("DIAL %s VIA %s %s\n", httpsProxyUrl, network, addr)
				//     return proxyDialer.DialContext(ctx, network, addr)
				// },
				ProxyConnectHeader: proxyConnectHeader,
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		}

		for range 2 {
			addr, _ := randServerHostPort()
			targetUrlString := fmt.Sprintf("https://%s/status", addr)
			// targetUrlString := "https://api.bringyour.com/hello"
			r, err := proxyHttpClient.Get(targetUrlString)
			assert.Equal(t, err, nil)
			if err == nil {
				b, err := io.ReadAll(r.Body)
				assert.Equal(t, err, nil)
				r.Body.Close()
				m := map[string]any{}
				err = json.Unmarshal(b, &m)
				assert.Equal(t, err, nil)
				assert.NotEqual(t, m["client_address"], nil)
			}
		}

		proxyHttpClient.CloseIdleConnections()

	}

	for _, server := range servers {
		server.Close()
	}
	for _, exchange := range exchanges {
		exchange.Close()
	}

}
