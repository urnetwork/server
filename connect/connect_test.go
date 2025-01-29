package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	// "sync"
	"encoding/hex"
	mathrand "math/rand"
	"runtime"

	"golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

func TestConnectNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnect\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
			})
	})
}

func TestConnectWithSymmetricContractsNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithSymmetricContracts\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
			})
	})
}

func TestConnectWithAsymmetricContractsNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContracts\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{})
	})
}

func TestConnectWithChaosNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithChaos\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaosNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaos\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaosNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaos\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
			})
	})
}

func TestConnectNoTransportReformNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{})
	})
}

func TestConnectWithChaosNoTransportReformNoNack(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos: true,
			})
	})
}

// note large nack objects can be excessively dropped
// due to the arbitrary order of receipt
// the nack object must align with the contract it was sent under
// see the notes on how contracts work for acks in connect/transfer

func TestConnect(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnect\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
			})
	})
}

func TestConnectWithSymmetricContracts(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithSymmetricContracts\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
			})
	})
}

func TestConnectWithAsymmetricContracts(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContracts\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
			})
	})
}

func TestConnectWithChaos(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithChaos\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaos(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaos\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaos(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaos\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
			})
	})
}

func TestConnectNoTransportReform(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableNack: true,
			})
	})
}

func TestConnectWithChaosNoTransportReform(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos: true,
				enableNack:  true,
			})
	})
}

// new instance testing
// this tests that the sender/receiver can reconnect with a different instance
// e.g. logout and login

func TestConnectWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnect\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectWithSymmetricContractsWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithSymmetricContracts\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContracts\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectWithChaosWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithChaos\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectWithSymmetricContractsWithChaosWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithSymmetricContractsWithChaos\n")
		testConnect(t, contractTestSymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectWithAsymmetricContractsWithChaosWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithAsymmetricContractsWithChaos\n")
		testConnect(t, contractTestAsymmetric,
			&testConnectConfig{
				enableChaos:           true,
				enableTransportReform: true,
				enableNack:            true,
				enableNewInstance:     true,
			})
	})
}

func TestConnectNoTransportReformWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableNack:        true,
				enableNewInstance: true,
			})
	})
}

func TestConnectWithChaosNoTransportReformWithNewInstance(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		fmt.Printf("[progress]start TestConnectWithChaosNoTransportReform\n")
		testConnect(t, contractTestNone,
			&testConnectConfig{
				enableChaos:       true,
				enableNack:        true,
				enableNewInstance: true,
			})
	})
}

const (
	contractTestNone      int = 0
	contractTestSymmetric     = 1
	// the normal client-provider relationship
	contractTestAsymmetric = 2
)

type testConnectConfig struct {
	enableChaos           bool
	enableTransportReform bool
	enableNack            bool
	enableNewInstance     bool
}

// this test that two clients can communicate via the connect server
// spin up two connect servers on different ports, and connect one client to each server
// send message bursts between the clients
// contract logic is optional so that the effects of contracts can be isolated
// sets all sequence buffer sizes to 0 to test deadlock
func testConnect(
	t *testing.T,
	contractTest int,
	config *testConnectConfig,
) {
	if testing.Short() {
		return
	}

	type Message struct {
		sourceId    connect.Id
		frames      []*protocol.Frame
		provideMode protocol.ProvideMode
	}

	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")

	receiveTimeout := 300 * time.Second

	// larger values test the send queue and receive queue sizes
	messageContentSizes := []ByteCount{
		4 * 1024,
		128 * 1024,
		1024 * 1024,
	}

	transportCount := 6
	burstM := 6
	newInstanceM := 0
	if config.enableNewInstance {
		newInstanceM = 4
	}
	nackM := 0
	if config.enableNack {
		nackM = 6
	}

	nackDroppedByteCount := ByteCount(0)
	pauseTimeout := 200 * time.Millisecond
	sequenceIdleTimeout := 100 * time.Millisecond
	minResendInterval := 10 * time.Millisecond

	// note the receiver idle timeout must be sufficiently large
	// or else retransmits might be delivered multiple times
	// send and forward idle timeouts can be arbirary values

	randPauseTimeout := func() time.Duration {
		return time.Duration(mathrand.Int63n(int64(pauseTimeout)))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service := "testConnect"
	block := "test"

	clientIdA := server.NewId()
	clientAInstanceId := server.NewId()
	clientIdB := server.NewId()
	clientBInstanceId := server.NewId()

	routes := map[string]string{}
	for i := 0; i < 10; i += 1 {
		host := fmt.Sprintf("host%d", i)
		routes[host] = "127.0.0.1"
	}

	createServer := func(exchange *Exchange, port int) *http.Server {
		connectHandler := NewConnectHandlerWithDefaults(ctx, server.NewId(), exchange)

		routes := []*router.Route{
			router.NewRoute("GET", "/status", router.WarpStatus),
			router.NewRoute("GET", "/", connectHandler.Connect),
		}

		addr := fmt.Sprintf(":%d", port)
		routerHandler := router.NewRouter(ctx, routes)

		server := &http.Server{
			Addr:    addr,
			Handler: routerHandler,
		}
		return server
	}

	hostPorts := map[string]int{}
	exchanges := map[string]*Exchange{}
	servers := map[string]*http.Server{}
	for i := 0; i < 10; i += 1 {
		host := fmt.Sprintf("host%d", i)
		port := 8080 + 1
		hostPorts[host] = port

		hostToServicePorts := map[int]int{
			9000 + i: 9000 + i,
		}

		settings := DefaultExchangeSettings()
		settings.ExchangeBufferSize = 0
		settings.ResidentIdleTimeout = sequenceIdleTimeout
		settings.ForwardIdleTimeout = sequenceIdleTimeout
		if config.enableChaos {
			settings.ExchangeChaosSettings.ResidentShutdownPerSecond = 0.05
		}
		switch contractTest {
		case contractTestSymmetric, contractTestAsymmetric:
			settings.ForwardEnforceActiveContracts = true
		}

		exchange := NewExchange(ctx, host, service, block, hostToServicePorts, routes, settings)
		exchanges[host] = exchange

		server := createServer(exchange, port)
		servers[host] = server
		defer server.Close()
		go server.ListenAndServe()
	}

	randServer := func() string {
		ports := maps.Values(hostPorts)
		port := ports[mathrand.Intn(len(ports))]
		return fmt.Sprintf("ws://127.0.0.1:%d", port)
	}

	maxMessageContentSize := ByteCount(0)
	for _, messageContentSize := range messageContentSizes {
		maxMessageContentSize = max(maxMessageContentSize, messageContentSize)
	}
	standardContractTransferByteCount := 4 * maxMessageContentSize
	standardContractFillFraction := float32(0.5)

	clientStrategyA := connect.NewClientStrategyWithDefaults(ctx)
	clientStrategyB := connect.NewClientStrategyWithDefaults(ctx)

	clientSettingsA := connect.DefaultClientSettings()
	clientSettingsA.SendBufferSettings.SequenceBufferSize = 0
	clientSettingsA.SendBufferSettings.AckBufferSize = 0
	clientSettingsA.SendBufferSettings.AckTimeout = receiveTimeout
	clientSettingsA.SendBufferSettings.MinResendInterval = minResendInterval
	clientSettingsA.ReceiveBufferSettings.GapTimeout = receiveTimeout
	clientSettingsA.ReceiveBufferSettings.SequenceBufferSize = 0
	// clientSettingsA.ReceiveBufferSettings.AckBufferSize = 0
	clientSettingsA.ForwardBufferSettings.SequenceBufferSize = 0
	// disable scheduled network events
	clientSettingsA.ContractManagerSettings = connect.DefaultContractManagerSettingsNoNetworkEvents()
	// set this low enough to test new contracts in the transfer
	clientSettingsA.SendBufferSettings.ContractFillFraction = standardContractFillFraction
	clientSettingsA.ContractManagerSettings.StandardContractTransferByteCount = standardContractTransferByteCount
	clientSettingsA.SendBufferSettings.IdleTimeout = sequenceIdleTimeout
	clientSettingsA.ReceiveBufferSettings.IdleTimeout = receiveTimeout
	clientSettingsA.ForwardBufferSettings.IdleTimeout = sequenceIdleTimeout
	clientSettingsA.ControlPingTimeout = 30 * time.Second
	clientA := connect.NewClient(ctx, connect.Id(clientIdA), Testing_NewControllerOutOfBandControl(ctx, clientIdA), clientSettingsA)
	// routeManagerA := connect.NewRouteManager(clientA)
	// contractManagerA := connect.NewContractManagerWithDefaults(clientA)
	// clientA.Setup(routeManagerA, contractManagerA)
	// go clientA.Run()

	clientSettingsB := connect.DefaultClientSettings()
	clientSettingsB.SendBufferSettings.SequenceBufferSize = 0
	clientSettingsB.SendBufferSettings.AckBufferSize = 0
	clientSettingsB.SendBufferSettings.AckTimeout = receiveTimeout
	clientSettingsB.SendBufferSettings.MinResendInterval = minResendInterval
	clientSettingsB.ReceiveBufferSettings.GapTimeout = receiveTimeout
	clientSettingsB.ReceiveBufferSettings.SequenceBufferSize = 0
	// clientSettingsB.ReceiveBufferSettings.AckBufferSize = 0
	clientSettingsB.ForwardBufferSettings.SequenceBufferSize = 0
	// disable scheduled network events
	clientSettingsB.ContractManagerSettings = connect.DefaultContractManagerSettingsNoNetworkEvents()
	// set this low enough to test new contracts in the transfer
	clientSettingsB.SendBufferSettings.ContractFillFraction = standardContractFillFraction
	clientSettingsB.ContractManagerSettings.StandardContractTransferByteCount = standardContractTransferByteCount
	clientSettingsB.SendBufferSettings.IdleTimeout = sequenceIdleTimeout
	clientSettingsB.ReceiveBufferSettings.IdleTimeout = receiveTimeout
	clientSettingsB.ForwardBufferSettings.IdleTimeout = sequenceIdleTimeout
	clientSettingsB.ControlPingTimeout = 30 * time.Second
	clientB := connect.NewClient(ctx, connect.Id(clientIdB), Testing_NewControllerOutOfBandControl(ctx, clientIdB), clientSettingsB)
	// routeManagerB := connect.NewRouteManager(clientB)
	// contractManagerB := connect.NewContractManagerWithDefaults(clientB)
	// clientB.Setup(routeManagerB, contractManagerB)
	// go clientB.Run()

	networkIdA := server.NewId()
	networkNameA := "testConnectNetworkA"
	userIdA := server.NewId()
	deviceIdA := server.NewId()

	model.Testing_CreateNetwork(
		ctx,
		networkIdA,
		networkNameA,
		userIdA,
	)
	model.Testing_CreateDevice(
		ctx,
		networkIdA,
		deviceIdA,
		clientIdA,
		"a",
		"a",
	)

	networkIdB := server.NewId()
	networkNameB := "testConnectNetworkB"
	userIdB := server.NewId()
	deviceIdB := server.NewId()

	model.Testing_CreateNetwork(
		ctx,
		networkIdB,
		networkNameB,
		userIdB,
	)
	model.Testing_CreateDevice(
		ctx,
		networkIdB,
		deviceIdB,
		clientIdB,
		"b",
		"b",
	)

	// attach transports
	guestMode := false

	byJwtA := jwt.NewByJwt(networkIdA, userIdA, networkNameA, guestMode).Client(deviceIdA, clientIdA)

	authA := &connect.ClientAuth{
		ByJwt: byJwtA.Sign(),
		// ClientId: clientIdA,
		InstanceId: connect.Id(clientAInstanceId),
		AppVersion: "0.0.0",
	}

	transportAs := []*connect.PlatformTransport{}
	for i := 0; i < transportCount; i += 1 {
		transportA := connect.NewPlatformTransportWithDefaults(ctx, clientStrategyA, clientA.RouteManager(), randServer(), authA)
		transportAs = append(transportAs, transportA)
		// go transportA.Run(clientA.RouteManager())
	}

	byJwtB := jwt.NewByJwt(networkIdB, userIdB, networkNameB, guestMode).Client(deviceIdB, clientIdB)

	authB := &connect.ClientAuth{
		ByJwt: byJwtB.Sign(),
		// ClientId: clientIdB,
		InstanceId: connect.Id(clientBInstanceId),
		AppVersion: "0.0.0",
	}

	transportBs := []*connect.PlatformTransport{}
	for i := 0; i < transportCount; i += 1 {
		transportB := connect.NewPlatformTransportWithDefaults(ctx, clientStrategyB, clientB.RouteManager(), randServer(), authB)
		transportBs = append(transportBs, transportB)
		// go transportB.Run(clientB.RouteManager())
	}

	receiveA := make(chan *Message, 1024)
	receiveB := make(chan *Message, 1024)

	// printReceive := func(clientName string, frames []*protocol.Frame) {
	// 	for _, frame := range frames {
	// 		simpleMessage := connect.RequireFromFrame(frame).(*protocol.SimpleMessage)
	// 		if 0 < simpleMessage.MessageCount {
	// 			fmt.Printf("[%s] receive acked message %d\n", clientName, simpleMessage.MessageIndex)
	// 		} else {
	// 			fmt.Printf("[%s] receive nacked message %d\n", clientName, simpleMessage.MessageIndex)
	// 		}
	// 	}
	// }

	clientA.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
		// printReceive("a", frames)
		select {
		case <-ctx.Done():
		case receiveA <- &Message{
			sourceId:    source.SourceId,
			frames:      frames,
			provideMode: provideMode,
		}:
		}
	})

	clientB.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
		// printReceive("b", frames)
		select {
		case <-ctx.Done():
		case receiveB <- &Message{
			sourceId:    source.SourceId,
			frames:      frames,
			provideMode: provideMode,
		}:
		}
	})

	initialTransferBalance := ByteCount(1024) * ByteCount(1024) * ByteCount(1024) * ByteCount(1024)

	balanceCodeA, err := model.CreateBalanceCode(
		ctx,
		initialTransferBalance,
		0,
		"test-1",
		"",
		"",
	)
	assert.Equal(t, nil, err)

	result, err := model.RedeemBalanceCode(
		&model.RedeemBalanceCodeArgs{
			Secret: balanceCodeA.Secret,
		},
		session.NewLocalClientSession(ctx, "0.0.0.0", byJwtA),
	)
	assert.Equal(t, nil, err)
	assert.Equal(t, nil, result.Error)

	balanceCodeB, err := model.CreateBalanceCode(
		ctx,
		initialTransferBalance,
		0,
		"test-2",
		"",
		"",
	)
	assert.Equal(t, nil, err)

	result, err = model.RedeemBalanceCode(
		&model.RedeemBalanceCodeArgs{
			Secret: balanceCodeB.Secret,
		},
		session.NewLocalClientSession(ctx, "0.0.0.0", byJwtB),
	)
	assert.Equal(t, nil, err)
	assert.Equal(t, nil, result.Error)

	// set up provide
	switch contractTest {
	case contractTestNone:
		clientA.ContractManager().AddNoContractPeer(connect.Id(clientIdB))
		clientB.ContractManager().AddNoContractPeer(connect.Id(clientIdA))

	case contractTestSymmetric:
		// FIXME
		// clientA.ContractManager().AddNoContractPeer(connect.Id(clientIdB))
		// clientB.ContractManager().AddNoContractPeer(connect.Id(clientIdA))

		provideModes := map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
			protocol.ProvideMode_Public:  true,
		}

		func() {
			ack := make(chan struct{})
			clientA.ContractManager().SetProvideModesWithAckCallback(
				provideModes,
				func(err error) {
					close(ack)
				},
			)
			select {
			case <-ack:
			case <-time.After(5 * time.Second):
			}
		}()

		func() {
			ack := make(chan struct{})
			clientB.ContractManager().SetProvideModesWithAckCallback(
				provideModes,
				func(err error) {
					close(ack)
				},
			)
			select {
			case <-ack:
			case <-time.After(5 * time.Second):
			}
		}()

	case contractTestAsymmetric:
		// FIXME
		// clientA.ContractManager().AddNoContractPeer(connect.Id(clientIdB))
		// clientB.ContractManager().AddNoContractPeer(connect.Id(clientIdA))

		// a->b is provide
		// b->a is a companion

		func() {
			ack := make(chan struct{})
			clientA.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
				map[protocol.ProvideMode]bool{},
				func(err error) {
					close(ack)
				},
			)
			select {
			case <-ack:
			case <-time.After(5 * time.Second):
			}
		}()

		func() {
			ack := make(chan struct{})
			clientB.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
				map[protocol.ProvideMode]bool{
					protocol.ProvideMode_Network: true,
					protocol.ProvideMode_Public:  true,
				},
				func(err error) {
					close(ack)
				},
			)
			select {
			case <-ack:
			case <-time.After(5 * time.Second):
			}
		}()

	}

	for _, messageContentSize := range messageContentSizes {
		// the message bytes are hex encoded which doubles the size
		messageContentBytes := make([]byte, messageContentSize/2)
		mathrand.Read(messageContentBytes)
		messageContent := hex.EncodeToString(messageContentBytes)
		assert.Equal(t, int(messageContentSize), len(messageContent))

		ackA := make(chan error, 1024)
		ackB := make(chan error, 1024)

		for burstSize := 1; burstSize <= burstM; burstSize += 1 {
			for b := 0; b < 2; b += 1 {
				fmt.Printf(
					"[progress][%s] burstSize=%d b=%d\n",
					model.ByteCountHumanReadable(messageContentSize),
					burstSize,
					b,
				)

				if config.enableTransportReform {
					for _, transportA := range transportAs {
						transportA.Close()
					}
					fmt.Printf("pause\n")
					select {
					case <-ctx.Done():
						return
					case <-time.After(randPauseTimeout()):
					}
					if 0 < newInstanceM && 0 == mathrand.Intn(newInstanceM) {
						fmt.Printf("new instance\n")
						clientAInstanceId = server.NewId()
					}
					authA = &connect.ClientAuth{
						ByJwt: byJwtA.Sign(),
						// ClientId: clientIdA,
						InstanceId: connect.Id(clientAInstanceId),
						AppVersion: "0.0.0",
					}
					for i := 0; i < transportCount; i += 1 {
						fmt.Printf("new transport a\n")
						transportA := connect.NewPlatformTransportWithDefaults(ctx, clientStrategyA, clientA.RouteManager(), randServer(), authA)
						transportAs = append(transportAs, transportA)
						// go transportA.Run(clientA.RouteManager())
					}
					// let the closed transports remove, otherwise messages will be send to closing tranports
					// (this will affect the nack delivery)
					select {
					case <-time.After(200 * time.Millisecond):
					}
				}

				go func() {
					for i := 0; i < burstSize; i += 1 {
						if i == burstSize/2 {
							fmt.Printf("pause\n")
							select {
							case <-ctx.Done():
								return
							case <-time.After(randPauseTimeout()):
							}
						}
						for j := 0; j < nackM; j += 1 {
							_, err := clientA.SendWithTimeoutDetailed(
								connect.RequireToFrame(&protocol.SimpleMessage{
									MessageIndex: uint32(i*nackM + j),
									MessageCount: uint32(0),
									Content:      messageContent,
								}),
								connect.DestinationId(connect.Id(clientIdB)),
								nil,
								-1,
								connect.NoAck(),
							)
							if err != nil && !server.IsDoneError(err) {
								panic(fmt.Errorf("Could not send = %v", err))
							}
						}
						_, err := clientA.SendWithTimeoutDetailed(
							connect.RequireToFrame(&protocol.SimpleMessage{
								MessageIndex: uint32(i),
								MessageCount: uint32(burstSize),
								Content:      messageContent,
							}),
							connect.DestinationId(connect.Id(clientIdB)),
							func(err error) {
								ackA <- err
							},
							-1,
						)
						if err != nil && !server.IsDoneError(err) {
							panic(fmt.Errorf("Could not send = %v", err))
						}
					}
				}()

				// messagesToB := []*Message{}
				nackBCount := 0
				for i := 0; i < burstSize; i += 1 {
				ReceiveAckB:
					for {
						select {
						case message := <-receiveB:
							// messagesToB = append(messagesToB, message)

							// check in order
							for _, frame := range message.frames {
								switch v := connect.RequireFromFrame(frame).(type) {
								case *protocol.SimpleMessage:
									if 0 < v.MessageCount {
										assert.Equal(t, uint32(burstSize), v.MessageCount)
										assert.Equal(t, uint32(i), v.MessageIndex)
										break ReceiveAckB
									} else {
										nackBCount += 1
									}
								}
							}
						case <-time.After(receiveTimeout):
							// printAllStacks()
							panic(errors.New("Timeout."))
						}
					}
				}
				endTime := time.Now().Add(1 * time.Second)
				for nackBCount < nackM*burstSize {
					timeout := endTime.Sub(time.Now())
					if timeout <= 0 {
						break
					}
					select {
					case message := <-receiveB:
						// messagesToB = append(messagesToB, message)

						// check in order
						for _, frame := range message.frames {
							switch v := connect.RequireFromFrame(frame).(type) {
							case *protocol.SimpleMessage:
								if 0 < v.MessageCount {
									t.Fatal("Unexpected ack message.")
								} else {
									nackBCount += 1
								}
							}
						}
					case <-time.After(timeout):
					}
				}
				if nackBCount != nackM*burstSize {
					fmt.Printf("B dropped nacks: %d <> %d\n", nackBCount, nackM*burstSize)
					nackDroppedByteCount += messageContentSize * ByteCount(nackM*burstSize-nackBCount)
				}
				for i := 0; i < burstSize; i += 1 {
					select {
					case err := <-ackA:
						assert.Equal(t, err, nil)
					case <-time.After(receiveTimeout):
						// printAllStacks()
						panic(errors.New("Timeout."))
					}
				}
				select {
				case <-ackA:
					panic(errors.New("Too many acks."))
				default:
				}
				// check in order
				// for i, message := range messagesToB {
				// 	for _, frame := range message.frames {
				// 		simpleMessage := connect.RequireFromFrame(frame).(*protocol.SimpleMessage)
				// 		assert.Equal(t, uint32(i), simpleMessage.MessageIndex)
				// 	}
				// }

				if config.enableTransportReform {
					for _, transportB := range transportBs {
						transportB.Close()
					}
					fmt.Printf("pause\n")
					select {
					case <-ctx.Done():
						return
					case <-time.After(randPauseTimeout()):
					}
					if 0 < newInstanceM && 0 == mathrand.Intn(newInstanceM) {
						fmt.Printf("new instance\n")
						clientBInstanceId = server.NewId()
					}
					authB = &connect.ClientAuth{
						ByJwt: byJwtB.Sign(),
						// ClientId: clientIdB,
						InstanceId: connect.Id(clientBInstanceId),
						AppVersion: "0.0.0",
					}
					for i := 0; i < transportCount; i += 1 {
						fmt.Printf("new transport b\n")
						transportB := connect.NewPlatformTransportWithDefaults(ctx, clientStrategyB, clientB.RouteManager(), randServer(), authB)
						transportBs = append(transportBs, transportB)
						// go transportB.Run(clientB.RouteManager())
					}
					// let the closed transports remove, otherwise messages will be send to closing tranports
					// (this will affect the nack delivery)
					select {
					case <-time.After(200 * time.Millisecond):
					}
				}

				go func() {
					for i := 0; i < burstSize; i += 1 {
						if i == burstSize/2 {
							fmt.Printf("pause\n")
							select {
							case <-ctx.Done():
								return
							case <-time.After(randPauseTimeout()):
							}
						}
						for j := 0; j < nackM; j += 1 {
							opts := []any{
								connect.NoAck(),
							}
							if contractTest == contractTestAsymmetric {
								opts = append(opts, connect.CompanionContract())
							}

							_, err := clientB.SendWithTimeoutDetailed(
								connect.RequireToFrame(&protocol.SimpleMessage{
									MessageIndex: uint32(i*nackM + j),
									MessageCount: uint32(0),
									Content:      messageContent,
								}),
								connect.DestinationId(connect.Id(clientIdA)),
								nil,
								-1,
								opts...,
							)
							if err != nil && !server.IsDoneError(err) {
								panic(fmt.Errorf("Could not send = %v", err))
							}
						}
						opts := []any{}
						if contractTest == contractTestAsymmetric {
							opts = append(opts, connect.CompanionContract())
						}
						_, err := clientB.SendWithTimeoutDetailed(
							connect.RequireToFrame(&protocol.SimpleMessage{
								MessageIndex: uint32(i),
								MessageCount: uint32(burstSize),
								Content:      messageContent,
							}),
							connect.DestinationId(connect.Id(clientIdA)),
							func(err error) {
								ackB <- err
							},
							-1,
							opts...,
						)
						if err != nil && !server.IsDoneError(err) {
							panic(fmt.Errorf("Could not send = %v", err))
						}
					}
				}()

				// 	sendToA(burstSize - 1)
				// }()
				// messagesToA := []*Message{}
				nackACount := 0
				for i := 0; i < burstSize; i += 1 {
				ReceiveAckA:
					for {
						select {
						case message := <-receiveA:
							// messagesToB = append(messagesToB, message)

							// check in order
							for _, frame := range message.frames {
								switch v := connect.RequireFromFrame(frame).(type) {
								case *protocol.SimpleMessage:
									if 0 < v.MessageCount {
										assert.Equal(t, uint32(burstSize), v.MessageCount)
										assert.Equal(t, uint32(i), v.MessageIndex)
										break ReceiveAckA
									} else {
										nackACount += 1
									}
								}
							}
						case <-time.After(receiveTimeout):
							// printAllStacks()
							panic(errors.New("Timeout."))
						}
					}
				}
				endTime = time.Now().Add(1 * time.Second)
				for nackACount < nackM*burstSize {
					timeout := endTime.Sub(time.Now())
					if timeout <= 0 {
						break
					}
					select {
					case message := <-receiveA:
						// messagesToB = append(messagesToB, message)

						// check in order
						for _, frame := range message.frames {
							switch v := connect.RequireFromFrame(frame).(type) {
							case *protocol.SimpleMessage:
								if 0 < v.MessageCount {
									t.Fatal("Unexpected ack message.")
								} else {
									nackACount += 1
								}
							}
						}
					case <-time.After(timeout):
					}
				}
				if nackACount != nackM*burstSize {
					fmt.Printf("A dropped nacks: %d <> %d\n", nackACount, nackM*burstSize)
					nackDroppedByteCount += messageContentSize * ByteCount(nackM*burstSize-nackACount)
				}
				for i := 0; i < burstSize; i += 1 {
					select {
					case err := <-ackB:
						assert.Equal(t, err, nil)
					case <-time.After(receiveTimeout):
						// printAllStacks()
						panic(errors.New("Timeout."))
					}
				}
				select {
				case <-ackB:
					panic(errors.New("Too many acks."))
				default:
				}
				// check in order
				// for i, message := range messagesToA {
				// 	for _, frame := range message.frames {
				// 		simpleMessage := connect.RequireFromFrame(frame).(*protocol.SimpleMessage)
				// 		assert.Equal(t, uint32(i), simpleMessage.MessageIndex)
				// 	}
				// }

				resendItemCountA, resendItemByteCountA, sequenceIdA := clientA.ResendQueueSize(connect.DestinationId(connect.Id(clientIdB)), connect.MultiHopId{}, false, false)
				assert.Equal(t, resendItemCountA, 0)
				assert.Equal(t, resendItemByteCountA, ByteCount(0))

				resendItemCountB, resentItemByteCountB, sequenceIdB := clientB.ResendQueueSize(connect.DestinationId(connect.Id(clientIdA)), connect.MultiHopId{}, false, false)
				assert.Equal(t, resendItemCountB, 0)
				assert.Equal(t, resentItemByteCountB, ByteCount(0))

				receiveItemCountA, receiveItemByteCountA := clientA.ReceiveQueueSize(connect.DestinationId(connect.Id(clientIdB)), sequenceIdB)
				assert.Equal(t, receiveItemCountA, 0)
				assert.Equal(t, receiveItemByteCountA, ByteCount(0))

				receiveItemCountB, receiveItemByteCountB := clientB.ReceiveQueueSize(connect.DestinationId(connect.Id(clientIdA)), sequenceIdA)
				assert.Equal(t, receiveItemCountB, 0)
				assert.Equal(t, receiveItemByteCountB, ByteCount(0))
			}
		}
	}

	select {
	case <-time.After(1 * time.Second):
	}

	for _, transportA := range transportAs {
		transportA.Close()
	}
	for _, transportB := range transportBs {
		transportB.Close()
	}

	clientA.Cancel()
	clientB.Cancel()

	// close(receiveA)
	// close(receiveB)

	for _, server := range servers {
		server.Close()
	}
	for _, exchange := range exchanges {
		exchange.Close()
	}

	select {
	case <-time.After(1 * time.Second):
	}

	flushedContractIdsA := []server.Id{}
	for _, contractId := range clientA.ContractManager().Flush(false) {
		flushedContractIdsA = append(flushedContractIdsA, server.Id(contractId))
	}
	flushedContractIdsB := []server.Id{}
	for _, contractId := range clientB.ContractManager().Flush(false) {
		flushedContractIdsB = append(flushedContractIdsB, server.Id(contractId))
	}

	clientA.Flush()
	clientB.Flush()

	select {
	case <-time.After(1 * time.Second):
	}

	clientA.Close()
	clientB.Close()

	select {
	case <-time.After(1 * time.Second):
	}

	assert.Equal(
		t,
		ByteCount(0),
		clientA.ContractManager().LocalStats().ContractOpenByteCount(),
	)
	assert.Equal(
		t,
		ByteCount(0),
		clientB.ContractManager().LocalStats().ContractOpenByteCount(),
	)

	// these contracts were queued in the contract manager
	// and should be partially closed by the source
	contractIdPartialClosePartiesAToB := model.GetOpenContractIdsWithPartialClose(ctx, clientIdA, clientIdB)
	contractIdPartialClosePartiesBToA := model.GetOpenContractIdsWithPartialClose(ctx, clientIdB, clientIdA)

	// note ContractPartySource is for contracts that were queued up and never used
	for contractId, party := range contractIdPartialClosePartiesAToB {
		if party == model.ContractPartyCheckpoint || party == model.ContractPartySource {
			model.CloseContract(ctx, contractId, clientIdB, 0, false)
			delete(contractIdPartialClosePartiesAToB, contractId)
		}
	}
	for contractId, party := range contractIdPartialClosePartiesBToA {
		if party == model.ContractPartyCheckpoint || party == model.ContractPartySource {
			model.CloseContract(ctx, contractId, clientIdA, 0, false)
			delete(contractIdPartialClosePartiesBToA, contractId)
		}
	}

	assert.Equal(t, len(contractIdPartialClosePartiesAToB), 0)
	assert.Equal(t, len(contractIdPartialClosePartiesBToA), 0)

	// FIXME what are these other contracts?
	// for _, party := range contractIdPartialClosePartiesAToB {
	// 	assert.Equal(t, model.ContractPartySource, party)
	// }
	// for _, party := range contractIdPartialClosePartiesBToA {
	// 	assert.Equal(t, model.ContractPartySource, party)
	// }

	// SendSequence now flushes pending contracts when closed
	assert.Equal(t, len(flushedContractIdsA), 0)
	assert.Equal(t, len(flushedContractIdsB), 0)

	// if e := len(contractIdPartialClosePartiesAToB) - len(flushedContractIdsA); 1 < e {
	// 	assert.Equal(t, len(flushedContractIdsA), len(contractIdPartialClosePartiesAToB))
	// }
	// for _, contractId := range flushedContractIdsA {
	// 	party, ok := contractIdPartialClosePartiesAToB[contractId]
	// 	assert.Equal(t, true, ok)
	// 	assert.Equal(t, model.ContractPartySource, party)
	// }
	// if e := len(contractIdPartialClosePartiesBToA) - len(flushedContractIdsB); 1 < e {
	// 	assert.Equal(t, len(flushedContractIdsB), len(contractIdPartialClosePartiesBToA))
	// }
	// for _, contractId := range flushedContractIdsB {
	// 	party, ok := contractIdPartialClosePartiesBToA[contractId]
	// 	assert.Equal(t, true, ok)
	// 	assert.Equal(t, model.ContractPartySource, party)
	// }

	localStatsA := clientA.ContractManager().LocalStats()
	localStatsB := clientB.ContractManager().LocalStats()

	byteCountA := (localStatsA.ContractCloseByteCount + localStatsB.ReceiveContractCloseByteCount) / 2
	byteCountB := (localStatsB.ContractCloseByteCount + localStatsA.ReceiveContractCloseByteCount) / 2

	fmt.Printf("Net nack dropped bytes = %d\n", nackDroppedByteCount)
	byteCountEquivalent := func(a ByteCount, b ByteCount) {
		// because of resets and dropped nacks there may be some small discrepancy
		// (see note at top about messages closer to the contract size getting dropped more frequenctly)
		threshold := 64*model.Mib + nackDroppedByteCount
		e := a - b
		if e < -threshold || threshold < e {
			assert.Equal(t, a, b)
		}
	}

	switch contractTest {
	case contractTestNone:
		// no balance deducted
		byteCountEquivalent(
			initialTransferBalance,
			model.GetActiveTransferBalanceByteCount(ctx, networkIdA),
		)
		byteCountEquivalent(
			initialTransferBalance,
			model.GetActiveTransferBalanceByteCount(ctx, networkIdB),
		)
	case contractTestSymmetric:
		// a and b each have 1x balance deducted

		byteCountEquivalent(
			initialTransferBalance-
				byteCountA-
				standardContractTransferByteCount*ByteCount(len(contractIdPartialClosePartiesAToB)),
			model.GetActiveTransferBalanceByteCount(ctx, networkIdA),
		)
		byteCountEquivalent(
			initialTransferBalance-
				byteCountB-
				standardContractTransferByteCount*ByteCount(len(contractIdPartialClosePartiesBToA)),
			model.GetActiveTransferBalanceByteCount(ctx, networkIdB),
		)

	case contractTestAsymmetric:
		// a has 2x balance deducted and b has no balance deducted

		byteCountEquivalent(
			initialTransferBalance-
				byteCountA-
				standardContractTransferByteCount*ByteCount(len(contractIdPartialClosePartiesAToB))-
				byteCountB-
				standardContractTransferByteCount*ByteCount(len(contractIdPartialClosePartiesBToA)),
			model.GetActiveTransferBalanceByteCount(ctx, networkIdA),
		)
		byteCountEquivalent(
			initialTransferBalance,
			model.GetActiveTransferBalanceByteCount(ctx, networkIdB),
		)
	}
}

func printAllStacks() {
	b := make([]byte, 128*1024*1024)
	n := runtime.Stack(b, true)
	fmt.Printf("ALL STACKS: %s\n", string(b[0:n]))
}

type controllerOutOfBandControl struct {
	ctx      context.Context
	clientId server.Id
}

func Testing_NewControllerOutOfBandControl(ctx context.Context, clientId server.Id) connect.OutOfBandControl {
	return &controllerOutOfBandControl{
		ctx:      ctx,
		clientId: clientId,
	}
}

func (self *controllerOutOfBandControl) SendControl(frames []*protocol.Frame, callback func(resultFrames []*protocol.Frame, err error)) {
	server.HandleError(func() {
		resultFrames, err := controller.ConnectControlFrames(self.ctx, self.clientId, frames)
		callback(resultFrames, err)
	})
}
