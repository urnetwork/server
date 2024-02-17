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
	type Message struct {
		sourceId connect.Id
		frames []*protocol.Frame
		provideMode protocol.ProvideMode
	}

	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")


	receiveTimeout := 60 * time.Second
	// 8MiB
	messageContentSize := 1024 * 1024


	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()


	hostA := "testConnectA"
	hostB := "testConnectB"


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


	exchangeA := NewExchange(ctx, hostA, hostToServicePortsA, routes)
	exchangeB := NewExchange(ctx, hostB, hostToServicePortsB, routes)


	clientSettingsA := connect.DefaultClientSettings()
	clientA := connect.NewClient(ctx, clientIdA, clientSettingsA)
	routeManagerA := connect.NewRouteManager(clientA)
	contractManagerA := connect.NewContractManagerWithDefaults(clientA)
	go clientA.Run(routeManagerA, contractManagerA)


	clientSettingsB := connect.DefaultClientSettings()
	clientB := connect.NewClient(ctx, clientIdB, clientSettingsB)
	routeManagerB := connect.NewRouteManager(clientB)
	contractManagerB := connect.NewContractManagerWithDefaults(clientB)
	go clientB.Run(routeManagerB, contractManagerB)


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


	clientA.AddReceiveCallback(func(sourceId connect.Id, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
		receiveA <- &Message{
			sourceId: sourceId,
			frames: frames,
			provideMode: provideMode,
		}
	})

	clientB.AddReceiveCallback(func(sourceId connect.Id, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
		receiveB <- &Message{
			sourceId: sourceId,
			frames: frames,
			provideMode: provideMode,
		}
	})


	messageContentBytes := make([]byte, messageContentSize)
	mathrand.Read(messageContentBytes)
	messageContent := hex.EncodeToString(messageContentBytes)


	ackA := make(chan error, 1024)
	ackB := make(chan error, 1024)
	

	for burstSize := 1; burstSize < 128; burstSize += 1 {
		for b := 0; b < 2; b += 1 {
			fmt.Printf("\n\nITER %d %d\n\n", burstSize, b)

			go func() {
				for i := 0; i < burstSize; i += 1 {
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
			for i := 0; i < burstSize; i += 1 {
				select {
				case message := <- receiveB:
					// messagesToB = append(messagesToB, message)

					// check in order
					for _, frame := range message.frames {
						simpleMessage := connect.RequireFromFrame(frame).(*protocol.SimpleMessage)
						assert.Equal(t, uint32(i), simpleMessage.MessageIndex)
					}
				case <- time.After(receiveTimeout):
					printAllStacks()
					panic(errors.New("Timeout."))
				}
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

			
			// messagesToA := []*Message{}
			for i := 0; i < burstSize; i += 1 {
				select {
				case message := <- receiveA:
					// check in order
					for _, frame := range message.frames {
						simpleMessage := connect.RequireFromFrame(frame).(*protocol.SimpleMessage)
						assert.Equal(t, uint32(i), simpleMessage.MessageIndex)
					}
				case <- time.After(receiveTimeout):
					printAllStacks()
					panic(errors.New("Timeout."))
				}
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







