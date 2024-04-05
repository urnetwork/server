package main

import (
    "context"
    "testing"
    "net/http"
    "fmt"
    "errors"
    "time"
    "os"
    // "sync"
    mathrand "math/rand"
    "encoding/hex"
    "runtime"

    "golang.org/x/exp/maps"

    "github.com/go-playground/assert/v2"

    "bringyour.com/connect"
    "bringyour.com/protocol"
    "bringyour.com/bringyour"
    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/controller"
    "bringyour.com/bringyour/jwt"
    "bringyour.com/bringyour/router"
    "bringyour.com/bringyour/session"
)


func TestConnect(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	testConnect(t, contractTestNone, false)
})}


func TestConnectWithSymmetricContracts(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	testConnect(t, contractTestSymmetric, false)
})}


func TestConnectWithAsymmetricContracts(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	testConnect(t, contractTestAsymmetric, false)
})}


func TestConnectWithChaos(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	testConnect(t, contractTestNone, true)
})}


func TestConnectWithSymmetricContractsWithChaos(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	testConnect(t, contractTestSymmetric, true)
})}


func TestConnectWithAsymmetricContractsWithChaos(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	testConnect(t, contractTestAsymmetric, true)
})}


const (
	contractTestNone int = 0
	contractTestSymmetric = 1
	// the normal client-provider relationship
	contractTestAsymmetric = 2
)


// this test that two clients can communicate via the connect server
// spin up two connect servers on different ports, and connect one client to each server
// send message bursts between the clients
// contract logic is optional so that the effects of contracts can be isolated
// FIXME set all sequence buffer sizes to 0
func testConnect(t *testing.T, contractTest int, enableChaos bool) {

	type Message struct {
		sourceId connect.Id
		frames []*protocol.Frame
		provideMode protocol.ProvideMode
	}

	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")


	receiveTimeout := 60 * time.Second

	// larger values test the send queue and receive queue sizes
	messageContentSizes := []ByteCount{
		4 * 1024,
		128 * 1024,
		1024 * 1024,
	}

	
	transportCount := 8
	burstM := 8
	newInstanceM := -1
	nackM := 4


	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()


	service := "testConnect"
	block := "test"


	clientIdA := bringyour.NewId()
	clientAInstanceId := bringyour.NewId()
	clientIdB := bringyour.NewId()
	clientBInstanceId := bringyour.NewId()

	routes := map[string]string{}
	for i := 0; i < 10; i += 1 {
		host := fmt.Sprintf("host%d", i)
		routes[host] = "127.0.0.1"
	}


	createServer := func(exchange *Exchange, port int) *http.Server {
	    connectHandler := NewConnectHandlerWithDefaults(ctx, exchange)

	    routes := []*router.Route{
	        router.NewRoute("GET", "/status", router.WarpStatus),
	        router.NewRoute("GET", "/", connectHandler.Connect),
	    }

	    addr := fmt.Sprintf(":%d", port)
	    routerHandler := router.NewRouter(ctx, routes)

	    server := &http.Server{
	    	Addr: addr,
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
		if enableChaos {
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

	randServer := func()(string) {
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

	clientSettingsA := connect.DefaultClientSettings()
	clientSettingsA.SendBufferSettings.SequenceBufferSize = 0
	clientSettingsA.SendBufferSettings.AckBufferSize = 0
	clientSettingsA.ReceiveBufferSettings.SequenceBufferSize = 0
	// clientSettingsA.ReceiveBufferSettings.AckBufferSize = 0
	clientSettingsA.ForwardBufferSettings.SequenceBufferSize = 0
	// disable scheduled network events
	clientSettingsA.ContractManagerSettings = connect.DefaultContractManagerSettingsNoNetworkEvents()
	// set this low enough to test new contracts in the transfer
	clientSettingsA.SendBufferSettings.ContractFillFraction = standardContractFillFraction
	clientSettingsA.ContractManagerSettings.StandardContractTransferByteCount = standardContractTransferByteCount
	clientA := connect.NewClient(ctx, connect.Id(clientIdA), Testing_NewControllerOutOfBandControl(ctx, clientIdA), clientSettingsA)
	// routeManagerA := connect.NewRouteManager(clientA)
	// contractManagerA := connect.NewContractManagerWithDefaults(clientA)
	// clientA.Setup(routeManagerA, contractManagerA)
	// go clientA.Run()


	clientSettingsB := connect.DefaultClientSettings()
	clientSettingsB.SendBufferSettings.SequenceBufferSize = 0
	clientSettingsB.SendBufferSettings.AckBufferSize = 0
	clientSettingsB.ReceiveBufferSettings.SequenceBufferSize = 0
	// clientSettingsB.ReceiveBufferSettings.AckBufferSize = 0
	clientSettingsB.ForwardBufferSettings.SequenceBufferSize = 0
	// disable scheduled network events
	clientSettingsB.ContractManagerSettings = connect.DefaultContractManagerSettingsNoNetworkEvents()
	// set this low enough to test new contracts in the transfer
	clientSettingsB.SendBufferSettings.ContractFillFraction = standardContractFillFraction
	clientSettingsB.ContractManagerSettings.StandardContractTransferByteCount = standardContractTransferByteCount
	clientB := connect.NewClient(ctx, connect.Id(clientIdB), Testing_NewControllerOutOfBandControl(ctx, clientIdB), clientSettingsB)
	// routeManagerB := connect.NewRouteManager(clientB)
	// contractManagerB := connect.NewContractManagerWithDefaults(clientB)
	// clientB.Setup(routeManagerB, contractManagerB)
	// go clientB.Run()


	
	networkIdA := bringyour.NewId()
	networkNameA := "testConnectNetworkA"
	userIdA := bringyour.NewId()
	deviceIdA := bringyour.NewId()

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

	networkIdB := bringyour.NewId()
	networkNameB := "testConnectNetworkB"
	userIdB := bringyour.NewId()
	deviceIdB := bringyour.NewId()

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

	byJwtA := jwt.NewByJwt(networkIdA, userIdA, networkNameA).Client(deviceIdA, clientIdA)

	authA := &connect.ClientAuth {
	    ByJwt: byJwtA.Sign(),
	    // ClientId: clientIdA,
	    InstanceId: connect.Id(clientAInstanceId),
	    AppVersion: "0.0.0",
	}

	transportAs := []*connect.PlatformTransport{}
	for i := 0; i < transportCount; i += 1 {
		transportA := connect.NewPlatformTransportWithDefaults(ctx, randServer(), authA, clientA.RouteManager())
		transportAs = append(transportAs, transportA)
		// go transportA.Run(clientA.RouteManager())
	}


	byJwtB := jwt.NewByJwt(networkIdB, userIdB, networkNameB).Client(deviceIdB, clientIdB)

	authB := &connect.ClientAuth {
	    ByJwt: byJwtB.Sign(),
	    // ClientId: clientIdB,
	    InstanceId: connect.Id(clientBInstanceId),
	    AppVersion: "0.0.0",
	}

	transportBs := []*connect.PlatformTransport{}
	for i := 0; i < transportCount; i += 1 {
		transportB := connect.NewPlatformTransportWithDefaults(ctx, randServer(), authB, clientB.RouteManager())
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


	clientA.AddReceiveCallback(func(sourceId connect.Id, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
		// printReceive("a", frames)
		receiveA <- &Message{
			sourceId: sourceId,
			frames: frames,
			provideMode: provideMode,
		}
	})

	clientB.AddReceiveCallback(func(sourceId connect.Id, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
		// printReceive("b", frames)
		receiveB <- &Message{
			sourceId: sourceId,
			frames: frames,
			provideMode: provideMode,
		}
	})


	initialTransferBalance := ByteCount(1024 * 1024 * 1024 * 1024)

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
	        protocol.ProvideMode_Public: true,
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
		    case <- ack:
		    case <- time.After(5 * time.Second):
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
		    case <- ack:
		    case <- time.After(5 * time.Second):
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
		    case <- ack:
		    case <- time.After(5 * time.Second):
		    }
		}()

		func() {
		    ack := make(chan struct{})
			clientB.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
				map[protocol.ProvideMode]bool{
			        protocol.ProvideMode_Network: true,
			        protocol.ProvideMode_Public: true,
			    },
				func(err error) {
		            close(ack)
		        },
		    )
		    select {
		    case <- ack:
		    case <- time.After(5 * time.Second):
		    }
		}()

		

	}


	for _, messageContentSize := range messageContentSizes {
		// the message bytes are hex encoded which doubles the size
		messageContentBytes := make([]byte, messageContentSize / 2)
		mathrand.Read(messageContentBytes)
		messageContent := hex.EncodeToString(messageContentBytes)
		assert.Equal(t, int(messageContentSize), len(messageContent))


		ackA := make(chan error, 1024)
		ackB := make(chan error, 1024)

		

		for burstSize := 1; burstSize < burstM; burstSize += 1 {
			for b := 0; b < 2; b += 1 {
				fmt.Printf(
					"[%s] burstSize=%d b=%d\n",
					model.ByteCountHumanReadable(messageContentSize),
					burstSize,
					b,
				)

				for _, transportA := range transportAs {
					transportA.Close()
				}
				if 0 < newInstanceM && 0 == mathrand.Intn(newInstanceM) {
					fmt.Printf("new instance\n")
					clientAInstanceId = bringyour.NewId()
				}
				authA = &connect.ClientAuth {
				    ByJwt: byJwtA.Sign(),
				    // ClientId: clientIdA,
				    InstanceId: connect.Id(clientAInstanceId),
				    AppVersion: "0.0.0",
				}
				for i := 0; i < transportCount; i += 1 {
					fmt.Printf("new transport a\n")
					transportA := connect.NewPlatformTransportWithDefaults(ctx, randServer(), authA, clientA.RouteManager())
					transportAs = append(transportAs, transportA)
					// go transportA.Run(clientA.RouteManager())
				}
				// let the closed transports remove, otherwise messages will be send to closing tranports
				// (this will affect the nack delivery)
				select {
				case <- time.After(200 * time.Millisecond):
				}

				go func() {
					for i := 0; i < burstSize; i += 1 {
						for j := 0; j < nackM; j += 1 {
							success := clientA.SendWithTimeout(
								connect.RequireToFrame(&protocol.SimpleMessage{
									MessageIndex: uint32(i * nackM + j),
									MessageCount: uint32(0),
									Content: messageContent,
								}),
								connect.Id(clientIdB),
								nil,
								-1,
								connect.NoAck(),
							)
							if !success {
								panic(errors.New("Could not send."))
							}
						}
						success := clientA.Send(
							connect.RequireToFrame(&protocol.SimpleMessage{
								MessageIndex: uint32(i),
								MessageCount: uint32(burstSize),
								Content: messageContent,
							}),
							connect.Id(clientIdB),
							func (err error) {
								ackA <- err
							},
						)
						if !success {
							panic(errors.New("Could not send."))
						}
					}
				}()

				// messagesToB := []*Message{}
				nackBCount := 0
				for i := 0; i < burstSize; i += 1 {
					ReceiveAckB:
					for {
						select {
						case message := <- receiveB:
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
						case <- time.After(receiveTimeout):
							// printAllStacks()
							panic(errors.New("Timeout."))
						}
					}
				}
				endTime := time.Now().Add(1 * time.Second)
				for nackBCount < nackM * burstSize {
					timeout := endTime.Sub(time.Now())
					if timeout <= 0 {
						break
					}
					select {
					case message := <- receiveB:
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
					case <- time.After(timeout):
					}
				}
				if nackBCount != nackM * burstSize {
					fmt.Printf("B dropped nacks: %d <> %d\n", nackBCount, nackM * burstSize)
				}
				for i := 0; i < burstSize; i += 1 {
					select {
					case err := <- ackA:
						assert.Equal(t, err, nil)
					case <- time.After(receiveTimeout):
						// printAllStacks()
						panic(errors.New("Timeout."))
					}
				}
				select {
				case <- ackA:
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


				for _, transportB := range transportBs {
					transportB.Close()
				}
				if 0 < newInstanceM && 0 == mathrand.Intn(newInstanceM) {
					fmt.Printf("new instance\n")
					clientBInstanceId = bringyour.NewId()
				}
				authB = &connect.ClientAuth {
				    ByJwt: byJwtB.Sign(),
				    // ClientId: clientIdB,
				    InstanceId: connect.Id(clientBInstanceId),
				    AppVersion: "0.0.0",
				}
				for i := 0; i < transportCount; i += 1 {
					fmt.Printf("new transport b\n")
					transportB := connect.NewPlatformTransportWithDefaults(ctx, randServer(), authB, clientB.RouteManager())
					transportBs = append(transportBs, transportB)
					// go transportB.Run(clientB.RouteManager())
				}
				// let the closed transports remove, otherwise messages will be send to closing tranports
				// (this will affect the nack delivery)
				select {
				case <- time.After(200 * time.Millisecond):
				}

				go func() {
					for i := 0; i < burstSize; i += 1 {
						for j := 0; j < nackM; j += 1 {
							opts := []any{
								connect.NoAck(),
							}
							if contractTest == contractTestAsymmetric {
								opts = append(opts, connect.CompanionContract())
							}

							success := clientB.SendWithTimeout(
								connect.RequireToFrame(&protocol.SimpleMessage{
									MessageIndex: uint32(i * nackM + j),
									MessageCount: uint32(0),
									Content: messageContent,
								}),
								connect.Id(clientIdA),
								nil,
								-1,
								opts...,
							)
							if !success {
								panic(errors.New("Could not send."))
							}
						}
						opts := []any{}
						if contractTest == contractTestAsymmetric {
							opts = append(opts, connect.CompanionContract())
						}
						success := clientB.SendWithTimeout(
							connect.RequireToFrame(&protocol.SimpleMessage{
								MessageIndex: uint32(i),
								MessageCount: uint32(burstSize),
								Content: messageContent,
							}),
							connect.Id(clientIdA),
							func (err error) {
								ackB <- err
							},
							-1,
							opts...,
						)
						if !success {
							panic(errors.New("Could not send."))
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
						case message := <- receiveA:
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
						case <- time.After(receiveTimeout):
							// printAllStacks()
							panic(errors.New("Timeout."))
						}
					}
				}
				endTime = time.Now().Add(1 * time.Second)
				for nackACount < nackM * burstSize {
					timeout := endTime.Sub(time.Now())
					if timeout <= 0 {
						break
					}
					select {
					case message := <- receiveA:
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
					case <- time.After(timeout):
					}
				}
				if nackACount != nackM * burstSize {
					fmt.Printf("A dropped nacks: %d <> %d\n", nackACount, nackM * burstSize)
				}
				for i := 0; i < burstSize; i += 1 {
					select {
					case err := <- ackB:
						assert.Equal(t, err, nil)
					case <- time.After(receiveTimeout):
						// printAllStacks()
						panic(errors.New("Timeout."))
					}
				}
				select {
				case <- ackB:
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


				resendItemCountA, resendItemByteCountA, sequenceIdA := clientA.ResendQueueSize(connect.Id(clientIdB), false)
				assert.Equal(t, resendItemCountA, 0)
				assert.Equal(t, resendItemByteCountA, ByteCount(0))

				resendItemCountB, resentItemByteCountB, sequenceIdB := clientB.ResendQueueSize(connect.Id(clientIdA), false)
				assert.Equal(t, resendItemCountB, 0)
				assert.Equal(t, resentItemByteCountB, ByteCount(0))

				receiveItemCountA, receiveItemByteCountA := clientA.ReceiveQueueSize(connect.Id(clientIdB), sequenceIdB)
				assert.Equal(t, receiveItemCountA, 0)
				assert.Equal(t, receiveItemByteCountA, ByteCount(0))

				receiveItemCountB, receiveItemByteCountB := clientB.ReceiveQueueSize(connect.Id(clientIdA), sequenceIdA)
				assert.Equal(t, receiveItemCountB, 0)
				assert.Equal(t, receiveItemByteCountB, ByteCount(0))
			}
		}
	}

	select {
	case <- time.After(10 * time.Second):
	}

	flushedContractIdsA := []bringyour.Id{}
	for _, contractId := range clientA.ContractManager().Flush(false) {
		flushedContractIdsA = append(flushedContractIdsA, bringyour.Id(contractId))
	}
	flushedContractIdsB := []bringyour.Id{}
	for _, contractId := range clientB.ContractManager().Flush(false) {
		flushedContractIdsB = append(flushedContractIdsB, bringyour.Id(contractId))
	}

	clientA.Flush()
	clientB.Flush()

	select {
	case <- time.After(4 * time.Second):
	}

	for _, transportA := range transportAs {
		transportA.Close()
	}
	for _, transportB := range transportBs {
		transportB.Close()
	}

	clientA.Cancel()
	clientB.Cancel()

	close(receiveA)
	close(receiveB)

	for _, server := range servers {
		server.Close()
	}
	for _, exchange := range exchanges {
		exchange.Close()
	}

	select {
	case <- time.After(1 * time.Second):
	}

	clientA.Close()
	clientB.Close()


	select {
	case <- time.After(1 * time.Second):
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

	// FIXME what are these other contracts?
	// for _, party := range contractIdPartialClosePartiesAToB {
	// 	assert.Equal(t, model.ContractPartySource, party)
	// }
	// for _, party := range contractIdPartialClosePartiesBToA {
	// 	assert.Equal(t, model.ContractPartySource, party)
	// }

	if e := len(contractIdPartialClosePartiesAToB) - len(flushedContractIdsA); 1 < e {
		assert.Equal(t, len(flushedContractIdsA), len(contractIdPartialClosePartiesAToB))
	}
	for _, contractId := range flushedContractIdsA {
		party, ok := contractIdPartialClosePartiesAToB[contractId]
		assert.Equal(t, true, ok)
		assert.Equal(t, model.ContractPartySource, party)
	}
	if e := len(contractIdPartialClosePartiesBToA) - len(flushedContractIdsB); 1 < e {
		assert.Equal(t, len(flushedContractIdsB), len(contractIdPartialClosePartiesBToA))
	}
	for _, contractId := range flushedContractIdsB {
		party, ok := contractIdPartialClosePartiesBToA[contractId]
		assert.Equal(t, true, ok)
		assert.Equal(t, model.ContractPartySource, party)
	}

	localStatsA := clientA.ContractManager().LocalStats()
	localStatsB := clientB.ContractManager().LocalStats()

	byteCountA := (localStatsA.ContractCloseByteCount + localStatsB.ReceiveContractCloseByteCount) / 2
	byteCountB := (localStatsB.ContractCloseByteCount + localStatsA.ReceiveContractCloseByteCount) / 2


	byteCountEquivalent := func(a ByteCount, b ByteCount) {
		// because of resets and dropped nacks there may be some small discrepancy
		threshold := 8 * model.Mib
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
			initialTransferBalance - 
				byteCountA - 
				standardContractTransferByteCount * ByteCount(len(contractIdPartialClosePartiesAToB)),
			model.GetActiveTransferBalanceByteCount(ctx, networkIdA),
		)
		byteCountEquivalent(
			initialTransferBalance - 
				byteCountB - 
				standardContractTransferByteCount * ByteCount(len(contractIdPartialClosePartiesBToA)),
			model.GetActiveTransferBalanceByteCount(ctx, networkIdB),
		)

	case contractTestAsymmetric:
		// a has 2x balance deducted and b has no balance deducted
		
		byteCountEquivalent(
			initialTransferBalance - 
				byteCountA - 
				standardContractTransferByteCount * ByteCount(len(contractIdPartialClosePartiesAToB)) -
				byteCountB - 
				standardContractTransferByteCount * ByteCount(len(contractIdPartialClosePartiesBToA)),
			model.GetActiveTransferBalanceByteCount(ctx, networkIdA),
		)
		byteCountEquivalent(
			initialTransferBalance,
			model.GetActiveTransferBalanceByteCount(ctx, networkIdB),
		)
	}
}


func printAllStacks() {
	b := make([]byte, 128 * 1024 * 1024)
	n := runtime.Stack(b, true)
	fmt.Printf("ALL STACKS: %s\n", string(b[0:n]))
}


type controllerOutOfBandControl struct {
	ctx context.Context
	clientId bringyour.Id
}

func Testing_NewControllerOutOfBandControl(ctx context.Context, clientId bringyour.Id) connect.OutOfBandControl {
	return &controllerOutOfBandControl{
		ctx: ctx,
		clientId: clientId,
	}
} 

func (self *controllerOutOfBandControl) SendControl(frames []*protocol.Frame, callback func(resultFrames []*protocol.Frame, err error)) {
	bringyour.HandleError(func() {
		resultFrames, err := controller.ConnectControlFrames(self.ctx, self.clientId, frames)
		callback(resultFrames, err)
	})
}

