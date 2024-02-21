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

    // "golang.org/x/exp/maps"

    "github.com/go-playground/assert/v2"

    "bringyour.com/connect"
    "bringyour.com/protocol"
    "bringyour.com/bringyour"
    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/jwt"
    "bringyour.com/bringyour/router"
    // "bringyour.com/bringyour/session"
)


// this test that two clients can communicate via the connect server
// spin up two connect servers on different ports, and connect one client to each server
// send message bursts between the clients
func TestConnect(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	// FIXME the chaos is messed up
	ChaosResidentShutdownPerSecond = 0.01


	type Message struct {
		sourceId connect.Id
		frames []*protocol.Frame
		provideMode protocol.ProvideMode
	}

	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")


	receiveTimeout := 120 * time.Second

	// larger values test the send queue and receive queue sizes
	messageContentSizes := []ByteCount{
		4 * 1024,
		128 * 1024,
		1024 * 1024,
	}


	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()


	hostA := "testConnectA"
	hostB := "testConnectB"

	service := "testConnect"
	block := "test"


	clientIdA := connect.NewId()
	clientIdB := connect.NewId()

	portA := 8080
	portB := 8081
	hostToServicePortsA := map[int]int{
		8090: 8090,
	}
	hostToServicePortsB := map[int]int{
		8091: 8091,
	}
	
	routes := map[string]string{
		hostA: "127.0.0.1",
		hostB: "127.0.0.1",
	}


	exchangeA := NewExchange(ctx, hostA, service, block, hostToServicePortsA, routes)
	exchangeB := NewExchange(ctx, hostB, service, block, hostToServicePortsB, routes)


	clientSettingsA := connect.DefaultClientSettings()
	clientA := connect.NewClient(ctx, clientIdA, clientSettingsA)
	routeManagerA := connect.NewRouteManager(clientA)
	contractManagerA := connect.NewContractManagerWithDefaults(clientA)
	clientA.Setup(routeManagerA, contractManagerA)
	go clientA.Run()


	clientSettingsB := connect.DefaultClientSettings()
	clientB := connect.NewClient(ctx, clientIdB, clientSettingsB)
	routeManagerB := connect.NewRouteManager(clientB)
	contractManagerB := connect.NewContractManagerWithDefaults(clientB)
	clientB.Setup(routeManagerB, contractManagerB)
	go clientB.Run()


	createServer := func(exchange *Exchange, port int) *http.Server {
	    connectHandler := NewConnectHandler(ctx, exchange)

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


	serverA := createServer(exchangeA, portA)
	defer serverA.Close()
	go serverA.ListenAndServe()


	serverB := createServer(exchangeB, portB)
	defer serverB.Close()
	go serverB.ListenAndServe()



	
	
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
		bringyour.Id(clientIdA),
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
		bringyour.Id(clientIdB),
		"b",
		"b",
	)

	// attach transports

	byJwtA := jwt.NewByJwt(networkIdA, userIdA, networkNameA).Client(deviceIdA, bringyour.Id(clientIdA)).Sign()

	authA := &connect.ClientAuth {
	    ByJwt: byJwtA,
	    InstanceId: clientA.InstanceId(),
	    AppVersion: "0.0.0",
	}

	transportA := connect.NewPlatformTransportWithDefaults(ctx, fmt.Sprintf("ws://127.0.0.1:%d", portA), authA)
	go transportA.Run(routeManagerA)


	byJwtB := jwt.NewByJwt(networkIdB, userIdB, networkNameB).Client(deviceIdB, bringyour.Id(clientIdB)).Sign()

	authB := &connect.ClientAuth {
	    ByJwt: byJwtB,
	    InstanceId: clientB.InstanceId(),
	    AppVersion: "0.0.0",
	}

	transportB := connect.NewPlatformTransportWithDefaults(ctx, fmt.Sprintf("ws://127.0.0.1:%d", portB), authB)
	go transportB.Run(routeManagerB)


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


	for _, messageContentSize := range messageContentSizes {
		messageContentBytes := make([]byte, messageContentSize)
		mathrand.Read(messageContentBytes)
		messageContent := hex.EncodeToString(messageContentBytes)


		ackA := make(chan error, 1024)
		ackB := make(chan error, 1024)

		nackM := 4
		

		for burstSize := 1; burstSize < 64; burstSize += 1 {
			for b := 0; b < 2; b += 1 {
				fmt.Printf(
					"[%s] burstSize=%d b=%d\n",
					model.ByteCountHumanReadable(messageContentSize),
					burstSize,
					b,
				)

				go func() {
					for i := 0; i < burstSize; i += 1 {
						for j := 0; j < nackM; j += 1 {
							success := clientA.SendWithTimeout(
								connect.RequireToFrame(&protocol.SimpleMessage{
									MessageIndex: uint32(i * nackM + j),
									MessageCount: uint32(0),
									Content: messageContent,
								}),
								clientIdB,
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
							clientIdB,
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
							assert.Equal(t, 1, len(message.frames))
							frame := message.frames[0]
							simpleMessage := connect.RequireFromFrame(frame).(*protocol.SimpleMessage)
							if 0 < simpleMessage.MessageCount {
								assert.Equal(t, uint32(i), simpleMessage.MessageIndex)
								break ReceiveAckB
							} else {
								nackBCount += 1
							}
						case <- time.After(receiveTimeout):
							printAllStacks()
							panic(errors.New("Timeout."))
						}
					}
				}
				if nackBCount != nackM * burstSize {
					fmt.Printf("B dropped nacks: %d <> %d\n", nackBCount, nackM * burstSize)
				}
				select {
				case <- receiveB:
					panic(errors.New("Too many messages."))
				default:
				}
				for i := 0; i < burstSize; i += 1 {
					select {
					case err := <- ackA:
						assert.Equal(t, err, nil)
					case <- time.After(receiveTimeout):
						printAllStacks()
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


				go func() {
					for i := 0; i < burstSize; i += 1 {
						for j := 0; j < nackM; j += 1 {
							success := clientB.SendWithTimeout(
								connect.RequireToFrame(&protocol.SimpleMessage{
									MessageIndex: uint32(i * nackM + j),
									MessageCount: uint32(0),
									Content: messageContent,
								}),
								clientIdA,
								nil,
								-1,
								connect.NoAck(),
							)
							if !success {
								panic(errors.New("Could not send."))
							}
						}
						success := clientB.Send(
							connect.RequireToFrame(&protocol.SimpleMessage{
								MessageIndex: uint32(i),
								MessageCount: uint32(burstSize),
								Content: messageContent,
							}),
							clientIdA,
							func (err error) {
								ackB <- err
							},
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
							assert.Equal(t, 1, len(message.frames))
							frame := message.frames[0]
							simpleMessage := connect.RequireFromFrame(frame).(*protocol.SimpleMessage)
							if 0 < simpleMessage.MessageCount {
								assert.Equal(t, uint32(i), simpleMessage.MessageIndex)
								break ReceiveAckA
							} else {
								nackACount += 1
							}
						case <- time.After(receiveTimeout):
							printAllStacks()
							panic(errors.New("Timeout."))
						}
					}
				}
				if nackACount != nackM * burstSize {
					fmt.Printf("A dropped nacks: %d <> %d\n", nackACount, nackM * burstSize)
				}
				select {
				case <- receiveA:
					panic(errors.New("Too many messages."))
				default:
				}
				for i := 0; i < burstSize; i += 1 {
					select {
					case err := <- ackB:
						assert.Equal(t, err, nil)
					case <- time.After(receiveTimeout):
						printAllStacks()
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


				resendItemCountA, resendItemByteCountA, sequenceIdA := clientA.ResendQueueSize(clientIdB)
				assert.Equal(t, resendItemCountA, 0)
				assert.Equal(t, resendItemByteCountA, 0)

				resendItemCountB, resentItemByteCountB, sequenceIdB := clientB.ResendQueueSize(clientIdA)
				assert.Equal(t, resendItemCountB, 0)
				assert.Equal(t, resentItemByteCountB, 0)

				receiveItemCountA, receiveItemByteCountA := clientA.ReceiveQueueSize(clientIdB, sequenceIdB)
				assert.Equal(t, receiveItemCountA, 0)
				assert.Equal(t, receiveItemByteCountA, 0)

				receiveItemCountB, receiveItemByteCountB := clientB.ReceiveQueueSize(clientIdA, sequenceIdA)
				assert.Equal(t, receiveItemCountB, 0)
				assert.Equal(t, receiveItemByteCountB, 0)
			}
		}
	}

	transportA.Close()
	transportB.Close()

	clientA.Cancel()
	clientB.Cancel()

	close(receiveA)
	close(receiveB)

	serverA.Close()
	serverB.Close()

	exchangeA.Close()
	exchangeB.Close()

	select {
	case <- time.After(1 * time.Second):
	}

	clientA.Close()
	clientB.Close()
})}


func printAllStacks() {
	b := make([]byte, 128 * 1024 * 1024)
	n := runtime.Stack(b, true)
	fmt.Printf("ALL STACKS: %s\n", string(b[0:n]))
}



// FIXME TestConnectSmallBuffer







