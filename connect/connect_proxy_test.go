//go:build !race

// note race detection on this test progressively slows down the test until it stops working
// FIXME the root cause might be a timeout/bad recovery in some part of the system
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
	"os/exec"
	"sync"
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
	// goproxy "golang.org/x/net/proxy"
)

func TestConnectProxy(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		testConnectProxy(t)
	})
}

func testConnectProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	func() {
		fmt.Printf(`

** This test requires many file descriptions.
** Setting "ulimit -n 1048576".
** The value will not be reset when the test ends.

`)
		_, err := exec.Command("/bin/sh", "-c", "ulimit -n 1048576").CombinedOutput()
		if err != nil {
			panic(err)
		}
	}()

	maxMessageContentSize := model.ByteCount(4096)

	// clean up lingering residents at this timeout
	// idleTimeout := 5 * time.Minute
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
		proxySettings.ListenHttpsPort = port + 444
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
			// router.NewRoute("CONNECT", "", connectRouter.ProxyConnect),
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
		// settings.ResidentIdleTimeout = idleTimeout
		// settings.ForwardIdleTimeout = idleTimeout
		settings.FramerSettings.MaxMessageLen = int(2 * maxMessageContentSize)
		settings.ForwardEnforceActiveContracts = true
		settings.IngressSecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
		settings.EgressSecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats

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
		return fmt.Sprintf("wss://local-connect.bringyour.com:%d", port), port
	}

	randServerHostPort := func() (string, int) {
		ports := maps.Values(hostPorts)
		port := ports[mathrand.Intn(len(ports))]
		return fmt.Sprintf("local-connect.bringyour.com:%d", port), port
	}

	randHttpsProxyServerHostPort := func() (string, int) {
		ports := maps.Values(hostPorts)
		port := ports[mathrand.Intn(len(ports))] + 444
		return fmt.Sprintf("local-connect.bringyour.com:%d", port), port
	}

	// randSocksProxyServerHostPort := func() (string, int) {
	// 	ports := maps.Values(hostPorts)
	// 	port := ports[mathrand.Intn(len(ports))] + 1080
	// 	return fmt.Sprintf("local-connect.bringyour.com:%d", port), port
	// }

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
					Secret:    balanceCode.Secret,
					NetworkId: session.ByJwt.NetworkId,
				},
				session.Ctx,
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
		remoteUserNatProviderSettings.EgressSecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats

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
	for range 32 {
		providerClientId := runProvider()
		// providerClientId := server.NewId()
		providerClientIds = append(providerClientIds, providerClientId)
	}

	select {
	case <-time.After(2 * time.Second):
	}

	for i := range 1024 {
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
					Secret:    balanceCode.Secret,
					NetworkId: session.ByJwt.NetworkId,
				},
				session.Ctx,
			)
			assert.Equal(t, nil, err)
			assert.Equal(t, nil, result.Error)
		}()

		args := &model.AuthNetworkClientArgs{
			Description: "test",
			DeviceSpec:  "test",

			ProxyConfig: &model.ProxyConfig{
				InitialDeviceState: &model.ExtendedProxyDeviceState{
					ProxyDeviceState: model.ProxyDeviceState{
						Location: &sdk.ConnectLocation{
							ConnectLocationId: &sdk.ConnectLocationId{
								ClientId: server.ToSdkId(providerClientId),
							},
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
		// addr, _ := randHttpsPoxyServerHostPort()
		// httpsProxyUrlStr := fmt.Sprintf("https://%s", addr)
		// httpsProxyUrl, err := url.Parse(httpsProxyUrlStr)
		// assert.Equal(t, err, nil)

		// proxyDialer := &net.Dialer{
		// 	Timeout: 15 * time.Second,
		// 	KeepAliveConfig: net.KeepAliveConfig{
		// 		Enable: true,
		// 	},
		// }

		// proxyConnectHeader := http.Header{}
		// proxyConnectHeader.Add("Proxy-Authorization", fmt.Sprintf("Bearer %s", result.ProxyConfigResult.AuthToken))

		proxyHttpClient := &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				// MaxIdleConns:       1,             // Max idle connections in the pool
				// IdleConnTimeout:    30 * time.Second, // Max time an idle connection stays open
				// DisableKeepAlives:  true,
				// TLSHandshakeTimeout: 15 * time.Second,
				// Proxy: http.ProxyURL(httpsProxyUrl),
				Proxy: func(r *http.Request) (*url.URL, error) {
					addr, _ := randHttpsProxyServerHostPort()
					httpsProxyUrlStr := fmt.Sprintf("https://%s:@%s", result.ProxyConfigResult.AuthToken, addr)
					return url.Parse(httpsProxyUrlStr)
				},
				// DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				// 	addr, _ = randServerHostPort()
				// 	fmt.Printf("DIAL %s VIA %s %s\n", httpsProxyUrl, network, addr)
				//     return proxyDialer.DialContext(ctx, network, addr)
				// },
				// ProxyConnectHeader: proxyConnectHeader,
			},
		}
		/*
			socksHttpClient := func() (*http.Client){

				addr, _ := randSocksProxyServerHostPort()
				// socksProxyUrlStr := fmt.Sprintf("socks5h://%s", addr)
				// return url.Parse(httpsProxyUrlStr)

				auth := &goproxy.Auth{
					User: result.ProxyConfigResult.AuthToken,
				}
				socksDialer, err := goproxy.SOCKS5("tcp", addr, auth, goproxy.Direct)
				assert.Equal(t, err, nil)

				return &http.Client{
					Timeout: 120 * time.Second,
					Transport: &http.Transport{
						// MaxIdleConns:       1,             // Max idle connections in the pool
						// IdleConnTimeout:    30 * time.Second, // Max time an idle connection stays open
						// DisableKeepAlives:  true,
						// TLSHandshakeTimeout: 15 * time.Second,
						// Proxy: http.ProxyURL(httpsProxyUrl),
						// Proxy: func(r *http.Request) (*url.URL, error) {
						// 	addr, _ := randHttpsProxyServerHostPort()
						// 	httpsProxyUrlStr := fmt.Sprintf("https://%s:@%s", result.ProxyConfigResult.AuthToken, addr)
						// 	return url.Parse(httpsProxyUrlStr)
						// },
						// DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
						// 	addr, _ = randServerHostPort()
						// 	fmt.Printf("DIAL %s VIA %s %s\n", httpsProxyUrl, network, addr)
						//     return proxyDialer.DialContext(ctx, network, addr)
						// },
						// ProxyConnectHeader: proxyConnectHeader,
						DialContext: socksDialer.(goproxy.ContextDialer).DialContext,
					},
				}
			}
		*/

		runOne := func() {
			// var err error
			// for range 2 {

			var httpClient *http.Client
			switch mathrand.Intn(2) {
			// case 1:
			// 	httpClient = socksHttpClient()
			default:
				httpClient = proxyHttpClient
			}

			addr, _ := randServerHostPort()
			targetUrlString := fmt.Sprintf("https://%s/status", addr)
			// targetUrlString := "https://api.bringyour.com/hello"
			// var r *http.Response
			r, err := httpClient.Get(targetUrlString)
			assert.Equal(t, err, nil)
			if err == nil {
				b, err := io.ReadAll(r.Body)
				assert.Equal(t, err, nil)
				r.Body.Close()
				m := map[string]any{}
				err = json.Unmarshal(b, &m)
				assert.Equal(t, err, nil)
				assert.NotEqual(t, m["client_address"], nil)
				return
			}
			// FIXME
			// else something flaky about the proxy connection
			// try again
			// }
			// assert.Equal(t, err, nil)
		}

		// run in serial
		for j := range 32 {
			fmt.Printf("[%d][%d][s]start\n", i, j)
			runOne()
		}

		// run in parallel
		var wg sync.WaitGroup
		for j := range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()

				fmt.Printf("[%d][%d][p]start\n", i, j)
				runOne()
			}()
		}
		wg.Wait()

		/*
			runPublic := func(targetUrlString string) {
				r, err := proxyHttpClient.Get(targetUrlString)
				assert.Equal(t, err, nil)
				if err == nil {
					_, err := io.ReadAll(r.Body)
					assert.Equal(t, err, nil)
					r.Body.Close()
					// m := map[string]any{}
					// err = json.Unmarshal(b, &m)
					// assert.Equal(t, err, nil)
					// assert.NotEqual(t, m["client_address"], nil)
					return
				}
			}

			publicHosts := []string{
				"https://api.bringyour.com/hello",
				// this will get rate limited
				// "https://ur.io",
			}

			for _, publicHost := range publicHosts {
				// run in serial
				for j := range 8 {
					fmt.Printf("[%d][%d][s][public]start\n", i, j)
					runPublic(publicHost)
				}

				// run in parallel
				var wg sync.WaitGroup
				for j := range 8 {
					wg.Add(1)
					go func() {
						defer wg.Done()

						fmt.Printf("[%d][%d][p][public]start\n", i, j)
						runPublic(publicHost)
					}()
				}
				wg.Wait()
			}
		*/

		proxyHttpClient.CloseIdleConnections()

	}

	for _, server := range servers {
		server.Close()
	}
	for _, exchange := range exchanges {
		exchange.Close()
	}

}
