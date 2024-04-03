package main

import (
	"context"
	"math/rand"
	"encoding/binary"
	"sync"
	"net"
	"time"
	"io"
	"fmt"
	"errors"
	"slices"
	mathrand "math/rand"
	// "runtime/debug"

	"golang.org/x/exp/maps"

	"google.golang.org/protobuf/proto"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/connect"
	"bringyour.com/protocol"
)


// note -
// we use one socket per client transport because the socket will block based on the slowest destination


/*
resident packet flow

client (A)
    <-> resident transport (exchange connection)
        <-> routes 
            <-> control resident A (clientId=ControlId) 
      	        <-> client receive (resident_controller)
      	        <-> client forward
      	        	<-> resident forward D (exchange connection)
      	        		<-> resident.forward
      	        			<-> control resident D (clientId=ControlId)
      	        				resident.forward sends the data on the most appropriate route
      	        				which for D will put the transfer frame on client (D)  
      	        				(see the reflection of client to resident flow)

*/


type ByteCount = model.ByteCount

var ControlId = bringyour.Id(connect.ControlId)

// use 0 for deadlock testing
const DefaultExchangeBufferSize = 32



// message writes on all layers have a single `WriteTimeout`
// this is because all layers have the same back pressure
// layers may have different read timeouts because of different keep alive/ping settings
type ExchangeSettings struct {
	ConnectHandlerSettings

	ExchangeBufferSize int

	// MaximumExchangeMessageByteCount ByteCount

	MinContractTransferByteCount ByteCount

	StartInternalPort int
	MaxConcurrentForwardsPerResident int

	ResidentIdleTimeout time.Duration
	ResidentSyncTimeout time.Duration
	ForwardIdleTimeout time.Duration
	ContractSyncTimeout time.Duration
	AbuseMinTimeout time.Duration
	ControlMinTimeout time.Duration

	ClientDrainTimeout time.Duration
	TransportDrainTimeout time.Duration

	// ClientWriteTimeout time.Duration

	ExchangeConnectTimeout time.Duration
	ExchangePingTimeout time.Duration
	ExchangeReadTimeout time.Duration
	ExchangeReadHeaderTimeout time.Duration
	// ExchangeWriteTimeout time.Duration
	ExchangeWriteHeaderTimeout time.Duration
	ExchangeReconnectAfterErrorTimeout time.Duration
	// ExchangeForwardTimeout time.Duration
	// ExchangeForwardRecreateTimeout time.Duration

	ExchangeConnectionResidentPollTimeout time.Duration

	ExchangeResidentWaitTimeout time.Duration
	ExchangeResidentPollTimeout time.Duration

	ForwardEnforceActiveContracts bool

	ExchangeChaosSettings
}


type ExchangeChaosSettings struct {
	ResidentShutdownPerSecond float64
}



func DefaultExchangeSettings() *ExchangeSettings {
	connectionHandlerSettings := DefaultConnectHandlerSettings()
	exchangePingTimeout := connectionHandlerSettings.WriteTimeout
	exchangeResidentWaitTimeout := 5 * time.Second
	return &ExchangeSettings{
		ConnectHandlerSettings: *connectionHandlerSettings,

		ExchangeBufferSize: DefaultExchangeBufferSize,

		// 64kib minimum contract
		// this is set high enough to limit the number of parallel contracts and avoid contract spam
		MinContractTransferByteCount: ByteCount(64 * 1024),

		// this must match the warp `settings.yml` for the environment
		StartInternalPort: 5080,

		MaxConcurrentForwardsPerResident: 256,

		ResidentIdleTimeout: 60 * time.Second,
		ResidentSyncTimeout: 30 * time.Second,
		ForwardIdleTimeout: 60 * time.Second,
		ContractSyncTimeout: 60 * time.Second,
		AbuseMinTimeout: 5 * time.Second,
		ControlMinTimeout: 5 * time.Millisecond,

		ClientDrainTimeout: 30 * time.Second,
		TransportDrainTimeout: 30 * time.Second,

		// ClientWriteTimeout: 5 * time.Second,

		ExchangeConnectTimeout: 5 * time.Second,
		ExchangePingTimeout: exchangePingTimeout,
		ExchangeReadTimeout: 2 * exchangePingTimeout,
		ExchangeReadHeaderTimeout: exchangeResidentWaitTimeout,
		// ExchangeWriteTimeout: 5 * time.Second,
		ExchangeWriteHeaderTimeout: exchangeResidentWaitTimeout,
		ExchangeReconnectAfterErrorTimeout: 1 * time.Second,
		ExchangeConnectionResidentPollTimeout: 30 * time.Second,
		// ExchangeForwardTimeout: 30 * time.Second,
		// ExchangeForwardRecreateTimeout: 10 * time.Second,

		ExchangeResidentWaitTimeout: exchangeResidentWaitTimeout,
		ExchangeResidentPollTimeout: exchangeResidentWaitTimeout / 8,

		ForwardEnforceActiveContracts: false,

		ExchangeChaosSettings: *DefaultExchangeChaosSettings(),
	}
}


func DefaultExchangeChaosSettings() *ExchangeChaosSettings {
	return &ExchangeChaosSettings{
		ResidentShutdownPerSecond: 0.0,
	}
}


// residents live in the exchange
// a resident for a client id can be nominated to live in the exchange with `NominateLocalResident`
// any time a resident is not reachable by a transport, the transport should nominate a local resident
type Exchange struct {
	// cleanupCtx context.Context
	ctx context.Context
	cancel context.CancelFunc

	host string
	service string
	block string
	// any of the ports may be used
	// a range of ports are used to scale one socket per transport or forward,
	// since each port allows at most 65k connections from another connect instance
	hostToServicePorts map[int]int
	routes map[string]string

	settings *ExchangeSettings

	residentsLock sync.Mutex
	// clientId -> Resident
	residents map[bringyour.Id]*Resident
}

func NewExchange(
	ctx context.Context,
	host string,
	service string,
	block string,
	hostToServicePorts map[int]int,
	routes map[string]string,
	settings *ExchangeSettings,
) *Exchange {
	cancelCtx, cancel := context.WithCancel(ctx)

	exchange := &Exchange{
		ctx: cancelCtx,
		cancel: cancel,
		host: host,
		service: service,
		block: block,
		hostToServicePorts: hostToServicePorts,
		routes: routes,
		residents: map[bringyour.Id]*Resident{},
		settings: settings,
	}

	go bringyour.HandleError(exchange.Run, cancel)
	// go bringyour.HandleError(exchange.syncResidents, cancel)

	return exchange
}

func NewExchangeWithDefaults(
	ctx context.Context,
	host string,
	service string,
	block string,
	hostToServicePorts map[int]int,
	routes map[string]string,
) *Exchange {
	return NewExchange(ctx, host, service, block, hostToServicePorts, routes, DefaultExchangeSettings())
}

// reads the host and port configuration from the env
func NewExchangeFromEnv(ctx context.Context, settings *ExchangeSettings) *Exchange {
	host := bringyour.RequireHost()
	service := bringyour.RequireService()
	block := bringyour.RequireBlock()
	routes := bringyour.Routes()

	// service port -> host port
	hostPorts := bringyour.RequireHostPorts()
	// internal ports start at `StartInternalPort` and proceed consecutively
	// each port can handle 65k connections
	// the number of connections depends on the number of expected concurrent destinations
	// the expected port usage is `number_of_residents * expected(number_of_destinations_per_resident)`,
	// and at most `number_of_residents * MaxConcurrentForwardsPerResident`

	// host port -> service port
	hostToServicePorts := map[int]int{}
	servicePort := settings.StartInternalPort
	for {
		hostPort, ok := hostPorts[servicePort]
		if !ok {
			break
		}
		hostToServicePorts[hostPort] = servicePort
		servicePort += 1
	}
	if len(hostToServicePorts) == 0 {
		panic(fmt.Errorf("No exchange internal ports found (starting with service port %d).", settings.StartInternalPort))
	}

	// bringyour.Logger().Printf("FOUND EXCHANGE PORTS %v\n", hostToServicePorts)

	return NewExchange(ctx, host, service, block, hostToServicePorts, routes, settings)
}

func NewExchangeFromEnvWithDefaults(ctx context.Context) *Exchange {
	return NewExchangeFromEnv(ctx, DefaultExchangeSettings())
}

func (self *Exchange) NominateLocalResident(
	clientId bringyour.Id,
	instanceId bringyour.Id,
	residentIdToReplace *bringyour.Id,
) bool {
	residentId := bringyour.NewId()
	

	

	nominated := model.NominateResident(self.ctx, residentIdToReplace, &model.NetworkClientResident{
		ClientId: clientId,
		InstanceId: instanceId,
		ResidentId: residentId,
		ResidentHost: self.host,
		ResidentService: self.service,
		ResidentBlock: self.block,
		ResidentInternalPorts: maps.Keys(self.hostToServicePorts),
	})
	// fmt.Printf("exchange nominated=%s proposed=%s\n", nominated.ResidentId, residentId)
	// bringyour.Logger().Printf("NOMINATED RESIDENT %s <> %s\n", nominated.ResidentId.String(), resident.residentId.String())
	if nominated.ResidentId != residentId {
		return false
	}

	// defer func() {
	// 	r := model.GetResidentWithInstance(self.ctx, clientId, instanceId)
	// 	// bringyour.Logger().Printf("NOMINATED RESIDENT VERIFIED %s <> %s\n", nominated.ResidentId.String(), r.ResidentId.String())
	// }()


	func() {
		self.residentsLock.Lock()
		defer self.residentsLock.Unlock()


		resident := NewResident(
			self.ctx,
			self,
			clientId,
			instanceId,
			residentId,
		)
		go bringyour.HandleError(func() {
			bringyour.HandleError(resident.Run)
			fmt.Printf("RESIDENT DONE\n")
			resident.Close()

			self.residentsLock.Lock()
			defer self.residentsLock.Unlock()
			if currentResident := self.residents[clientId]; resident == currentResident {
				delete(self.residents, clientId)
				model.RemoveResident(
					self.ctx,
					resident.clientId,
					resident.residentId,
				)	
			}
		})
		go func() {
			defer resident.Cancel()

			for {
				if resident.IsIdle() {
					return
				}

				select {
				case <- resident.Done():
					return
				case <- time.After(self.settings.ResidentIdleTimeout):
				}
			}
		}()
		
		if replacedResident, ok := self.residents[clientId]; ok {
			fmt.Printf("REPLACE RESIDENT\n")
			replacedResident.Cancel()
		}
		// bringyour.Logger().Printf("SET LOCAL RESIDENT %s\n", clientId.String())

		// fmt.Printf("exchange set resident clientId=%s\n", clientId)
		self.residents[clientId] = resident
	}()

	return true
}


// runs the exchange to expose local nominated residents
// there should be one local exchange per service
func (self *Exchange) Run() {
	// defer func() {
	// 	residentsCopy := map[bringyour.Id]*Resident{}
	// 	self.residentsLock.Lock()
	// 	maps.Copy(residentsCopy, self.residents)
	// 	clear(self.residents)
	// 	self.residentsLock.Unlock()
	// 	for _, resident := range residentsCopy {
	// 		resident.Close()
	// 		model.RemoveResident(self.ctx, resident.clientId, resident.residentId)
	// 	}
	// }()

	// bringyour.Logger().Printf("START EXCHANGE LISTEN ON HOST %s PORTS %v", self.host, self.hostToServicePorts)

	// remove existing residents at host:port
	func() {
		self.residentsLock.Lock()
		defer self.residentsLock.Unlock()
		residentIds := map[bringyour.Id]bool{}
		for _, resident := range self.residents {
			residentIds[resident.residentId] = true
		}
		residentsForHostPort := model.GetResidentsForHostPorts(
			self.ctx,
			self.host,
			maps.Keys(self.hostToServicePorts),
		)
		for _, residentForHostPort := range residentsForHostPort {
			if _, ok := residentIds[residentForHostPort.ResidentId]; !ok {
				model.RemoveResident(
					self.ctx,
					residentForHostPort.ClientId,
					residentForHostPort.ResidentId,
				)
			}
		}
	}()


	// start exchange connection servers
	for _, servicePort := range self.hostToServicePorts {
		port := servicePort
		go bringyour.HandleError(
			func() {
				self.serveExchangeConnection(port)
			},
			self.cancel,
		)
	}


	select {
	case <- self.ctx.Done():
	}
}

func (self *Exchange) serveExchangeConnection(port int) {
	defer self.cancel()

	// leave host part empty to listen on all available interfaces
	server, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return
	}
	defer server.Close()

	go bringyour.HandleError(func() {
		defer self.cancel()

		for {
			select {
			case <- self.ctx.Done():
				return
			default:
			}

			conn, err := server.Accept()
			if err != nil {
				return
			}
			go bringyour.HandleError(
				func() {
					self.handleExchangeConnection(conn)
				},
				self.cancel,
			)
		}
	}, self.cancel)

	select {
	case <- self.ctx.Done():
	}
}

func (self *Exchange) handleExchangeConnection(conn net.Conn) {
	handleCtx, handleCancel := context.WithCancel(self.ctx)
	defer func() {
		handleCancel()
		conn.Close()
	}()

	// tcpConn := conn.(*net.TCPConn)
	// tcpConn.SetKeepAlive(true)
	// tcpConn.SetKeepAlivePeriod(1 * time.Second)

	// bringyour.Logger().Printf("EXCHANGE HANDLE CLIENT\n")

	receiveBuffer := NewReceiveOnlyExchangeBuffer(self.settings)

	header, err := receiveBuffer.ReadHeader(handleCtx, conn)
	if err != nil {
		// fmt.Printf("exchange %s connection end 1\n", self.host)
		// bringyour.Logger().Printf("EXCHANGE HANDLE CLIENT READ HEADER ERROR %s\n", err)
		return
	}

	resident := bringyour.TraceWithReturnShallowLog(
		fmt.Sprintf("[ecr]wait for resident %s/%s", header.ClientId.String(), header.ResidentId.String()),
		func()(*Resident) {
			endTime := time.Now().Add(self.settings.ExchangeResidentWaitTimeout)
			for {
				var resident *Resident
				var ok bool
				func() {
					self.residentsLock.Lock()
					defer self.residentsLock.Unlock()

					resident, ok = self.residents[header.ClientId]
					// // bringyour.Logger().Printf("EXCHANGE ALL RESIDENTS %v\n", self.residents)
				}()

				if ok && resident.residentId == header.ResidentId {
					// bringyour.Logger().Printf("EXCHANGE HANDLE CLIENT MISSING RESIDENT %s %s (%t, %v) \n", header.ClientId.String(), header.ResidentId.String(), ok, resident)
					return resident
				}

				fmt.Printf("[ecr]wait for resident %s/%s\n", header.ClientId.String(), header.ResidentId.String())

				timeout := endTime.Sub(time.Now())
				if timeout <= 0 {
					return nil
				}
				select {
				case <- handleCtx.Done():
					return nil
				case <- time.After(self.settings.ExchangeResidentPollTimeout):
				case <- time.After(timeout):
				}
			}
		},
	)

	if resident == nil {
		// fmt.Printf("exchange %s connection end 2 clientId=%s (%v)\n", self.host, header.ClientId, resident)
		return
	}

	if resident.IsDone() {
		// fmt.Printf("exchange %s connection end 3\n", self.host)
		// bringyour.Logger().Printf("RESIDENT DONE.\n")
		return
	}

	// echo back the header
	if err := receiveBuffer.WriteHeader(handleCtx, conn, header); err != nil {
		// fmt.Printf("exchange %s connection end 4\n", self.host)
		// bringyour.Logger().Printf("EXCHANGE HANDLE CLIENT WRITE HEADER ERROR %s\n", err)
		return
	}

	// fmt.Printf("exchange %s connection start\n", self.host)


	switch header.Op {
	case ExchangeOpTransport:
		// this must close `receive`
		send, receive, closeTransport := resident.AddTransport()
		defer closeTransport()

		go bringyour.HandleError(func() {
			defer handleCancel()

			sendBuffer := NewDefaultExchangeBuffer(self.settings)
			for {
				select {
				case <- handleCtx.Done():
					return
				case <- resident.Done():
					return
				case message, ok := <- send:
					// // bringyour.Logger().Printf("RESIDENT SEND %s %s\n", message, ok)
					if !ok {
						return
					}
					if err := sendBuffer.WriteMessage(handleCtx, conn, message); err != nil {
						// fmt.Printf("bw timeout\n")
						// bringyour.Logger().Printf("RESIDENT SEND ERROR %s\n", err)
						return
					}

					fmt.Printf("[ecrs] %s/%s\n", resident.clientId.String(), resident.residentId.String())

				case <- time.After(self.settings.ExchangePingTimeout):
					// send a ping
					if err := sendBuffer.WriteMessage(handleCtx, conn, make([]byte, 0)); err != nil {
						// bringyour.Logger().Printf("RESIDENT PING ERROR %s\n", err)
						return
					}
				}
			}
		}, handleCancel)
		
		go bringyour.HandleError(func() {
			defer func() {
				handleCancel()
				close(receive)
			}()

			// read
			// messages from the transport are to be received by the resident
			// messages not destined for the control id are handled by the resident forward
			for {
				message, err := receiveBuffer.ReadMessage(handleCtx, conn)
				// // bringyour.Logger().Printf("RESIDENT RECEIVE %s %s\n", message, err)
				if err != nil {
					// bringyour.Logger().Printf("RESIDENT RECEIVE ERROR %s\n", err)
					return
				}
				if len(message) == 0 {
					// just a ping
					continue
				}
				
				// VV {
				if resident.client.IsDone() {
					fmt.Printf("[ecrr] %s/%s client done\n", resident.clientId.String(), resident.residentId.String())
				}
				
				// FIXME validate that client multi route writer has active route for the receive route
				multiRouteReader := resident.client.RouteManager().OpenMultiRouteReader(resident.client.ClientId())
				if !slices.Contains(multiRouteReader.GetActiveRoutes(), receive) {
					fmt.Printf("[ecrr] %s/%s missing receive route\n", resident.clientId.String(), resident.residentId.String())
				}
				resident.client.RouteManager().CloseMultiRouteReader(multiRouteReader)

				fmt.Printf("[ecrr] %s/%s waiting\n", resident.clientId.String(), resident.residentId.String())
				// }

				select {
				case <- handleCtx.Done():
					return
				case <- resident.Done():
					return
				case receive <- message:
					fmt.Printf("[ecrr] %s/%s\n", resident.clientId.String(), resident.residentId.String())

				case <- time.After(self.settings.WriteTimeout):
					// fmt.Printf("rw timeout\n")
					// bringyour.Logger("TIMEOUT RF\n")
					fmt.Printf("[ecrr]drop %s/%s\n", resident.clientId.String(), resident.residentId.String())

				}
			}
		}, handleCancel)

		
	case ExchangeOpForward:
		// read
		// messages from the forward are to be forwarded by the resident
		// the only route a resident has is to its client_id
		// a forward is a send where the source id does not match the client
		go bringyour.HandleError(func() {
			defer handleCancel()

			sendBuffer := NewDefaultExchangeBuffer(self.settings)
			for {
				select {
				case <- handleCtx.Done():
					return
				case <- resident.Done():
					return
				case <- time.After(self.settings.ExchangePingTimeout):
					// send a ping
					if err := sendBuffer.WriteMessage(handleCtx, conn, make([]byte, 0)); err != nil {
						// bringyour.Logger().Printf("RESIDENT PING ERROR %s\n", err)
						return
					}
				}
			}
		}, handleCancel)

		go bringyour.HandleError(func() {
			defer handleCancel()

			// // bringyour.Logger().Printf("RESIDENT FORWARD %s\n", header.ClientId.String())
			for {
				message, err := receiveBuffer.ReadMessage(handleCtx, conn)
				// // bringyour.Logger().Printf("RESIDENT FORWARD %s %s\n", message, err)
				if err != nil {
					// bringyour.Logger().Printf("RESIDENT FORWARD ERROR %s\n", err)
					return
				}
				if len(message) == 0 {
					// just a ping
					continue
				}
				select {
				case <- handleCtx.Done():
					return
				case <- resident.Done():
					return
				default:
				}

				// FIXME messages can be dropped - make sure back pressure is fine
				bringyour.TraceWithReturn(
					fmt.Sprintf("[ecrf]forward %s/%s", resident.clientId.String(), resident.residentId.String()),
					func()(bool) {
						return resident.Forward(message) 
					},
				)
			}
		}, handleCancel)
	}

	select {
	case <- handleCtx.Done():
		// bringyour.Logger().Printf("!!!! HANDLE DONE\n")
	case <- resident.Done():
		// bringyour.Logger().Printf("!!!! RESIDENT DONE\n")
	}
}

func (self *Exchange) Close() {
	
	self.cancel()	
	

}


// each call overwrites the internal buffer
type ExchangeBuffer struct {
	settings *ExchangeSettings
	buffer []byte
}

func NewDefaultExchangeBuffer(settings *ExchangeSettings) *ExchangeBuffer {
	return &ExchangeBuffer{
		settings: settings,
		buffer: make([]byte, settings.MaximumExchangeMessageByteCount + 4),
	} 
}

func NewReceiveOnlyExchangeBuffer(settings *ExchangeSettings) *ExchangeBuffer {
	return &ExchangeBuffer{
		settings: settings,
		buffer: make([]byte, 33),
	} 
}

func (self *ExchangeBuffer) WriteHeader(ctx context.Context, conn net.Conn, header *ExchangeHeader) error {
	conn.SetWriteDeadline(time.Now().Add(self.settings.ExchangeWriteHeaderTimeout))

	copy(self.buffer[0:16], header.ClientId.Bytes())
	copy(self.buffer[16:32], header.ResidentId.Bytes())
	self.buffer[32] = byte(header.Op)

	// conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
	_, err := conn.Write(self.buffer[0:33])
	// if 33 != n_ {
	// 	panic(fmt.Errorf("Bad write %s", err))
	// }
	return err
}

func (self *ExchangeBuffer) ReadHeader(ctx context.Context, conn net.Conn) (*ExchangeHeader, error) {
	conn.SetReadDeadline(time.Now().Add(self.settings.ExchangeReadHeaderTimeout))

	if _, err := io.ReadFull(conn, self.buffer[0:33]); err != nil {
		return nil, err
	}

	return &ExchangeHeader{
		ClientId: bringyour.Id(self.buffer[0:16]),
		ResidentId: bringyour.Id(self.buffer[16:32]),
		Op: ExchangeOp(self.buffer[32]),
	}, nil
}

func (self *ExchangeBuffer) WriteMessage(ctx context.Context, conn net.Conn, transferFrameBytes []byte) error {
	conn.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))

	n := ByteCount(len(transferFrameBytes))

	if self.settings.MaximumExchangeMessageByteCount < n {
		return errors.New(fmt.Sprintf("Maximum message size is %d (%d).", self.settings.MaximumExchangeMessageByteCount, n))
	}

	binary.LittleEndian.PutUint32(self.buffer[0:4], uint32(n))

	/*
	copy(self.buffer[4:4+n], transferFrameBytes)

	_, err := conn.Write(self.buffer[:4+n])
	return err
	*/

	// conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
	_, err := conn.Write(self.buffer[0:4])
	if err != nil {
		return err
	}
	_, err = conn.Write(transferFrameBytes)
	// if 4+n != n_ {
	// 	panic(fmt.Errorf("Bad write %s", err))
	// }
	return err
}

func (self *ExchangeBuffer) ReadMessage(ctx context.Context, conn net.Conn) ([]byte, error) {
	conn.SetReadDeadline(time.Now().Add(self.settings.ExchangeReadTimeout))

	// conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(conn, self.buffer[0:4]); err != nil {
		return nil, err
	}

	n := ByteCount(int(binary.LittleEndian.Uint32(self.buffer[0:4])))
	if self.settings.MaximumExchangeMessageByteCount < n {
		return nil, errors.New(fmt.Sprintf("Maximum message size is %d (%d).", self.settings.MaximumExchangeMessageByteCount, n))
	}

	// read into a new buffer
	message := make([]byte, n)

	if _, err := io.ReadFull(conn, message); err != nil {
		return nil, err
	}

	return message, nil
}


type ExchangeOp byte

const (
	ExchangeOpTransport ExchangeOp = 0x01
	// forward does not add a transport to the client
	// in forward op, call Send on the Resident not Receive
	ExchangeOpForward ExchangeOp = 0x02
)


type ExchangeHeader struct {
	ClientId bringyour.Id
	ResidentId bringyour.Id
	Op ExchangeOp
}


type ExchangeConnection struct {
	ctx context.Context
	cancel context.CancelFunc
	op ExchangeOp
	conn net.Conn
	sendBuffer *ExchangeBuffer
	receiveBuffer *ExchangeBuffer
	send chan []byte
	receive chan []byte
	settings *ExchangeSettings

	clientId bringyour.Id
	residentId bringyour.Id
	host string
	port int
}

func NewExchangeConnection(
	ctx context.Context,
	clientId bringyour.Id,
	residentId bringyour.Id,
	host string,
	port int,
	op ExchangeOp,
	routes map[string]string,
	settings *ExchangeSettings,
) (*ExchangeConnection, error) {
	// look up the host in the env routes
	// bringyour.Logger().Printf("EXCHANGE CONNECTION USING ROUTES %v\n", routes)
	hostRoute, ok := routes[host]
	if !ok {
		// use the hostname as the route
		// this requires the DNS to be configured correctly at the site
		hostRoute = host
	}

	authority := fmt.Sprintf("%s:%d", hostRoute, port)

	// bringyour.Logger().Printf("EXCHANGE CONNECTION DIAL %s\n", authority)

	dialer := net.Dialer{
		Timeout: settings.ExchangeConnectTimeout,
	}
	conn, err := dialer.DialContext(ctx, "tcp", authority)
	if err != nil {
		// bringyour.Logger().Printf("EXCHANGE CONNECTION ERROR CONNECT %s\n", err)
		return nil, err
	}
	tcpConn := conn.(*net.TCPConn)
	tcpConn.SetNoDelay(false)
	// tcpConn.SetWriteBuffer(MaximumExchangeMessageByteCount + 4)
	// tcpConn.SetReadBuffer(MaximumExchangeMessageByteCount + 4)
	// tcpConn.SetKeepAlive(true)
	// tcpConn.SetKeepAlivePeriod(1 * time.Second)

	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()

	sendBuffer := NewDefaultExchangeBuffer(settings)

	// write header
	err = sendBuffer.WriteHeader(ctx, conn, &ExchangeHeader{
		Op: op,
		ClientId: clientId,
		ResidentId: residentId,
	})
	if err != nil {
		// bringyour.Logger().Printf("EXCHANGE CONNECTION ERROR WRITE HEADER %s\n", err)
		return nil, err
	}

	// the connection echoes back the header if connected to the resident
	// else the connection is closed
	_, err = sendBuffer.ReadHeader(ctx, conn)
	if err != nil {
		// bringyour.Logger().Printf("EXCHANGE CONNECTION ERROR READ HEADER %s\n", err)
		return nil, err
	}


	success = true

	cancelCtx, cancel := context.WithCancel(ctx)

	connection := &ExchangeConnection{
		ctx: cancelCtx,
		cancel: cancel,
		op: op,
		conn: conn,
		sendBuffer: sendBuffer,
		receiveBuffer: NewReceiveOnlyExchangeBuffer(settings),
		send: make(chan []byte, settings.ExchangeBufferSize),
		receive: make(chan []byte, settings.ExchangeBufferSize),
		settings: settings,
		clientId: clientId,
		residentId: residentId,
		host: host,
		port: port,
	}
	go bringyour.HandleError(connection.Run, cancel)

	return connection, nil
}

func (self *ExchangeConnection) Run() {
	defer func() {
		self.cancel()
		self.conn.Close()
	}()

	// only a transport connection will receive messages
	switch self.op {
	case ExchangeOpTransport:
		go bringyour.HandleError(func() {
			defer func() {
				self.cancel()
				close(self.receive)
			}()

			for {
				select {
				case <- self.ctx.Done():
					return
				default:
				}
				message, err := self.receiveBuffer.ReadMessage(self.ctx, self.conn)
				if err != nil {
					// bringyour.Logger("TIMEOUT RA\n")
					return
				}
				if len(message) == 0 {
					// just a ping
					// fmt.Printf("[ecr]ping %s/%s@%s:%d\n", self.clientId.String(), self.residentId.String(), self.host, self.port)
					continue
				}


				select {
				case <- self.ctx.Done():
					return
				case self.receive <- message:

					fmt.Printf("[ecr] %s/%s@%s:%d\n", self.clientId.String(), self.residentId.String(), self.host, self.port)
				case <- time.After(self.settings.WriteTimeout):
					// bringyour.Logger("TIMEOUT RB\n")
					fmt.Printf("[ecr]drop %s/%s@%s:%d\n", self.clientId.String(), self.residentId.String(), self.host, self.port)
				}
			}
		}, self.cancel)
	case ExchangeOpForward:
		// nothing to receive, but time out on missing pings
		close(self.receive)

		go bringyour.HandleError(func() {
			defer self.cancel()

			for {
				select {
				case <- self.ctx.Done():
					return
				default:
				}
				message, err := self.receiveBuffer.ReadMessage(self.ctx, self.conn)
				if err != nil {
					// bringyour.Logger("TIMEOUT RA\n")
					return
				}
				if len(message) == 0 {
					// just a ping
					// fmt.Printf("[ecr]ping %s/%s@%s:%d\n", self.clientId.String(), self.residentId.String(), self.host, self.port)
					continue
				}
			}
		}, self.cancel)
	}

	go bringyour.HandleError(func() {
		defer self.cancel()

		for {
			select {
			case <- self.ctx.Done():
				return
			case message, ok := <- self.send:
				if !ok {
					return
				}
				if err := self.sendBuffer.WriteMessage(self.ctx, self.conn, message); err != nil {
					// bringyour.Logger("ERROR WRITING MESSAGE %s\n", err)
					// fmt.Printf("write error\n")
					return
				}
				fmt.Printf("[ecs] %s/%s@%s:%d\n", self.clientId.String(), self.residentId.String(), self.host, self.port)
			case <- time.After(self.settings.ExchangePingTimeout):
				// send a ping
				if err := self.sendBuffer.WriteMessage(self.ctx, self.conn, make([]byte, 0)); err != nil {
					// bringyour.Logger("ERROR WRITING PING MESSAGE %s\n", err)
					return
				}
				// fmt.Printf("[ecs]ping %s/%s@%s:%d\n", self.clientId.String(), self.residentId.String(), self.host, self.port)
			}
		}
	}, self.cancel)

	select {
	case <- self.ctx.Done():
		return
	}
}

func (self *ExchangeConnection) IsDone() bool {
	select {
	case <- self.ctx.Done():
		return true
	default:
		return false
	}
}

func (self *ExchangeConnection) Done() <-chan struct{} {
	return self.ctx.Done()
}

func (self *ExchangeConnection) Close() {
	self.cancel()

	close(self.send)
}

func (self *ExchangeConnection) Cancel() {
	self.cancel()
}




type ResidentTransport struct {	
	ctx context.Context
	cancel context.CancelFunc

	exchange *Exchange

	clientId bringyour.Id
	instanceId bringyour.Id

	routes map[string]string

	send chan []byte
	receive chan []byte
}

func NewResidentTransport(
	ctx context.Context,
	exchange *Exchange,
	clientId bringyour.Id,
	instanceId bringyour.Id,
) *ResidentTransport {
	cancelCtx, cancel := context.WithCancel(ctx)
	transport := &ResidentTransport{
		ctx: cancelCtx,
		cancel: cancel,
		exchange: exchange,
		clientId: clientId,
		instanceId: instanceId,
		send: make(chan []byte, exchange.settings.ExchangeBufferSize),
		receive: make(chan []byte, exchange.settings.ExchangeBufferSize),
	}
	// go bringyour.HandleError(transport.Run, cancel)
	return transport
}

func (self *ResidentTransport) Run() {
	defer func() {
		self.cancel()
		close(self.receive)
	}()

	handle := func(connection *ExchangeConnection, pollResident func()(bool)) {
		handleCtx, handleCancel := context.WithCancel(self.ctx)
		defer handleCancel()

		go bringyour.HandleError(func() {
			defer handleCancel()
			for {
				select {
				case <- handleCtx.Done():
					return
				case <- time.After(self.exchange.settings.ExchangeConnectionResidentPollTimeout):
				}
				if !pollResident() {
					return
				}
			}
		})

		go bringyour.HandleError(func() {
			defer func() {
				handleCancel()
				connection.Close()
			}()

			// write
			for {
				select {
				case <- handleCtx.Done():
					// bringyour.Logger().Printf("WRITE 3\n")
					return
				case <- connection.Done():
					return
				case message, ok := <- self.send:
					if !ok {
						// bringyour.Logger().Printf("WRITE 1\n")
						// transport closed
						self.cancel()
						return
					}
					select {
					case <- handleCtx.Done():
						return
					case <- connection.Done():
						return
					case connection.send <- message:
					case <- time.After(self.exchange.settings.WriteTimeout):
						// fmt.Printf("c timeout\n")
						// bringyour.Logger("TIMEOUT RD\n")
					}
				}
			}
		}, self.cancel)

		// read
		for {
			select {
			case <- handleCtx.Done():
				// bringyour.Logger().Printf("READ 3\n")
				return
			case <- connection.Done():
				return
			case message, ok := <- connection.receive:
				if !ok {
					// bringyour.Logger().Printf("READ 1\n")
					// need a new connection
					return
				}
				select {
				case <- handleCtx.Done():
					return
				case <- connection.Done():
					return
				case self.receive <- message:
				case <- time.After(self.exchange.settings.WriteTimeout):
					// fmt.Printf("c timeout\n")
					// bringyour.Logger("TIMEOUT RC\n")
					fmt.Printf("[rt]drop %s->\n", self.clientId.String())
				
				}
			}
		}
	}

	for {
		resident := model.GetResidentWithInstance(self.ctx, self.clientId, self.instanceId)
		// if resident != nil {
		// 	fmt.Printf("transport hasResident=true %s (%d ports)\n", resident.ResidentHost, len(resident.ResidentInternalPorts))
		// } else {
		// 	fmt.Printf("transport hasResident=false\n")
		// }
		// bringyour.Logger().Printf("EXCHANGE FOUND RESIDENT %v\n", resident)
		retryDelay := false
		if resident != nil && 0 < len(resident.ResidentInternalPorts) {
			port := resident.ResidentInternalPorts[rand.Intn(len(resident.ResidentInternalPorts))]
			// fmt.Printf("transport exchange connect %s %d\n", resident.ResidentHost, port)
			exchangeConnection, err := NewExchangeConnection(
				self.ctx,
				self.clientId,
				resident.ResidentId,
				resident.ResidentHost,
				port,
				ExchangeOpTransport,
				self.exchange.routes,
				self.exchange.settings,
			)

			if err != nil {
				fmt.Printf("[rt]exchange connection error %s->%s@%s:%d = %s\n", self.clientId.String(), resident.ResidentId.String(), resident.ResidentHost, port, err)
				retryDelay = true
			}

			if err == nil {
				bringyour.Trace(
					fmt.Sprintf("[rt]exchange connection %s->%s@%s:%d", self.clientId.String(), resident.ResidentId.String(), resident.ResidentHost, port),
					func() {
						// bringyour.Logger().Printf("EXCHANGE CONNECTION ENTER\n")
						// TODO the current test would be more efficient as a model notification instead of polling
						handle(exchangeConnection, func()(bool) {
							currentResidentId, err := model.GetResidentIdWithInstance(self.ctx, self.clientId, self.instanceId)
							if err != nil {
								return false
							}
							return resident.ResidentId == currentResidentId
						})
						// fmt.Printf("transport exchange connect done\n")
						// bringyour.Logger().Printf("EXCHANGE CONNECTION EXIT\n")
					},
				)
			}
		}
		
		if retryDelay {
			select {
			case <- self.ctx.Done():
				return
			case <- time.After(self.exchange.settings.ExchangeReconnectAfterErrorTimeout):
			}
		} else {
			select {
			case <- self.ctx.Done():
				return
			default:
			}
		}

		var residentIdToReplace *bringyour.Id
		if resident != nil {
			residentIdToReplace = &resident.ResidentId
		}

		bringyour.TraceWithReturn(
			fmt.Sprintf("[rt]nominate %s", self.clientId.String()),
			func()(bool) {				
				return self.exchange.NominateLocalResident(
					self.clientId,
					self.instanceId,
					residentIdToReplace,
				)
			},
		)
		// fmt.Printf("transport nominated success=%t %s\n", success, self.exchange.host)
	}
}

func (self *ResidentTransport) IsDone() bool {
	select {
	case <- self.ctx.Done():
		return true
	default:
		return false
	}
}

func (self *ResidentTransport) Done() <-chan struct{} {
	return self.ctx.Done()
}

func (self *ResidentTransport) Close() {
	self.cancel()

	close(self.send)
}

func (self *ResidentTransport) Cancel() {
	self.cancel()
}


type ResidentForward struct {	
	ctx context.Context
	cancel context.CancelFunc

	exchange *Exchange

	clientId bringyour.Id

	send chan []byte

	stateLock sync.Mutex
	lastActivityTime time.Time
}

func NewResidentForward(
	ctx context.Context,
	exchange *Exchange,
	clientId bringyour.Id,
) *ResidentForward {
	cancelCtx, cancel := context.WithCancel(ctx)
	transport := &ResidentForward{
		ctx: cancelCtx,
		cancel: cancel,
		exchange: exchange,
		clientId: clientId,
		send: make(chan []byte, exchange.settings.ExchangeBufferSize),
		lastActivityTime: time.Now(),
	}
	// go bringyour.HandleError(transport.Run, cancel)
	return transport
}

func (self *ResidentForward) Run() {
	defer self.cancel()

	handle := func(connection *ExchangeConnection, pollResident func()(bool)) {
		handleCtx, handleCancel := context.WithCancel(self.ctx)
		defer func() {
			handleCancel()
			connection.Close()
		}()

		go bringyour.HandleError(func() {
			defer handleCancel()
			for {
				select {
				case <- handleCtx.Done():
					return
				case <- time.After(self.exchange.settings.ExchangeConnectionResidentPollTimeout):
				}
				if !pollResident() {
					return
				}
			}
		})

		// write
		for {
			select {
			case <- handleCtx.Done():
				return
			case message, ok := <- self.send:
				if !ok {
					// transport closed
					return
				}
				select {
				case <- handleCtx.Done():
					return
				case <- connection.Done():
					return
				case connection.send <- message:
				case <- time.After(self.exchange.settings.WriteTimeout):
					// fmt.Printf("f timeout\n")
					// bringyour.Logger("TIMEOUT RE\n")
					fmt.Printf("[rf]drop %s->\n", self.clientId.String())
				}
			}
		}
	}

	for {
		// bringyour.Logger().Printf("FORWARD GET A NEW CONNECTION -> %s\n", self.clientId.String())
		resident := model.GetResident(self.ctx, self.clientId)
		retryDelay := true
		if resident != nil && 0 < len(resident.ResidentInternalPorts) {
			port := resident.ResidentInternalPorts[rand.Intn(len(resident.ResidentInternalPorts))]
			exchangeConnection, err := NewExchangeConnection(
				self.ctx,
				self.clientId,
				resident.ResidentId,
				resident.ResidentHost,
				port,
				ExchangeOpForward,
				self.exchange.routes,
				self.exchange.settings,
			)
			if err != nil {
				fmt.Printf("[rf]exchange connection error %s->%s@%s:%d = %s\n", self.clientId.String(), resident.ResidentId.String(), resident.ResidentHost, port, err)
			}
			if err == nil {
				bringyour.Trace(
					fmt.Sprintf("[rf]exchange connection %s->%s@%s:%d", self.clientId.String(), resident.ResidentId.String(), resident.ResidentHost, port),
					func() {
						// handleCtx, handleCancel := context.WithCancel(self.ctx)
						handle(exchangeConnection, func()(bool) {
							currentResidentId, err := model.GetResidentId(self.ctx, self.clientId)
							if err != nil {
								return false
							}
							return resident.ResidentId == currentResidentId
						})
						retryDelay = false
					},
				)
				// go func() {
				// 	defer handleCancel()
				// 	for {
				// 		select {
				// 		case <- handleCtx.Done():
				// 			return
				// 		case <- time.After(1 * time.Second):
				// 		}

				// 		currentResident := model.GetResident(self.ctx, self.clientId)
				// 		if currentResident == nil {
				// 			return
				// 		}
				// 		if currentResident.ResidentId != resident.ResidentId {
				// 			return
				// 		}
				// 	}
				// }()
			}
		}
		if retryDelay {
			select {
			case <- self.ctx.Done():
				return
			case <- time.After(self.exchange.settings.ExchangeReconnectAfterErrorTimeout):
			}
		} else {
			select {
			case <- self.ctx.Done():
				return
			default:
			}
		}
	}
}

func (self *ResidentForward) UpdateActivity() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.lastActivityTime = time.Now()
}

func (self *ResidentForward) IsIdle() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	idleTimeout := time.Now().Sub(self.lastActivityTime)
	return self.exchange.settings.ForwardIdleTimeout <= idleTimeout
}

func (self *ResidentForward) IsDone() bool {
	select {
	case <- self.ctx.Done():
		return true
	default:
		return false
	}
}

func (self *ResidentForward) Done() <-chan struct{} {
	return self.ctx.Done()
}

func (self *ResidentForward) Close() {
	self.cancel()

	close(self.send)
}

func (self *ResidentForward) Cancel() {
	self.cancel()
}


type Resident struct {
	ctx context.Context
	cancel context.CancelFunc

	exchange *Exchange

	clientId bringyour.Id
	instanceId bringyour.Id
	residentId bringyour.Id

	// the client id in the resident is always `connect.ControlId`
	client *connect.Client
	// clientRouteManager *connect.RouteManager
	// clientContractManager *connect.ContractManager
	residentContractManager *residentContractManager
	residentController *residentController

	stateLock sync.Mutex

	transports map[*clientTransport]bool

	// destination id -> forward
	forwards map[bringyour.Id]*ResidentForward

	abuseLimiter *limiter
	controlLimiter *limiter

	lastActivityTime time.Time

	clientReceiveUnsub func()
	clientForwardUnsub func()
}

func NewResident(
	ctx context.Context,
	exchange *Exchange,
	clientId bringyour.Id,
	instanceId bringyour.Id,
	residentId bringyour.Id,
) *Resident {
	cancelCtx, cancel := context.WithCancel(ctx)

	// use a tag with the client so that the logging does not show up as the control id 
	clientTag := fmt.Sprintf("c(%s)", clientId.String())
	clientSettings := connect.DefaultClientSettings()
	client := connect.NewClientWithTag(cancelCtx, connect.ControlId, clientTag, connect.NewNoContractClientOob(), clientSettings)

	// no contract is required between the platform and client
	// because the platform creates the contracts for the client
	client.ContractManager().AddNoContractPeer(connect.Id(clientId))


	residentContractManager := newResidentContractManager(
		cancelCtx,
		cancel,
		clientId,
		exchange.settings,
	)

	residentController := newResidentController(
		cancelCtx,
		cancel,
		clientId,
		residentContractManager,
		exchange.settings,
	)

	resident := &Resident{
		ctx: cancelCtx,
		cancel: cancel,
		exchange: exchange,
		clientId: clientId,
		instanceId: instanceId,
		residentId: residentId,
		client: client,
		residentContractManager: residentContractManager,
		residentController: residentController,
		transports: map[*clientTransport]bool{},
		forwards: map[bringyour.Id]*ResidentForward{},
		abuseLimiter: newLimiter(cancelCtx, exchange.settings.AbuseMinTimeout),
		controlLimiter: newLimiter(cancelCtx, exchange.settings.ControlMinTimeout),
		lastActivityTime: time.Now(),
	}

	clientReceiveUnsub := client.AddReceiveCallback(resident.handleClientReceive)
	resident.clientReceiveUnsub = clientReceiveUnsub

	clientForwardUnsub := client.AddForwardCallback(resident.handleClientForward)
	resident.clientForwardUnsub = clientForwardUnsub

	go bringyour.HandleError(resident.clientForward, cancel)



	// client.Setup(clientRouteManager, clientContractManager)

	// go bringyour.HandleError(func() {
	// 	client.Run()
	// }, cancel)

	// go bringyour.HandleError(resident.cleanupForwards, cancel)

	// FIXME clean up
	if 0 < exchange.settings.ExchangeChaosSettings.ResidentShutdownPerSecond {
		go bringyour.HandleError(func() {
			for {
				select {
				case <- cancelCtx.Done():
					return
				case <- time.After(1 * time.Second):
				}

				// bringyour.Logger("RESIDENT ALIVE host=%s clientId=%s residentId=%s\n", exchange.host, clientId.String(), residentId.String())

				
				if mathrand.Float64() < exchange.settings.ExchangeChaosSettings.ResidentShutdownPerSecond {
					fmt.Printf("[chaos]%s\n", residentId.String())
					// bringyour.Logger("RESIDENT CHAOS SHUTDOWN host=%s clientId=%s residentId=%s\n", exchange.host, clientId.String(), residentId.String())
					resident.Cancel()
				}
			}
		}, cancel)
	}

	return resident
}

func (self *Resident) Run() {
	defer self.cancel()

	select {
	case <- self.ctx.Done():
		fmt.Printf("RESIDENT CTX DONE\n")
		return
	case <- self.client.Done():
		fmt.Printf("RESIDENT CLIENT DONE\n")
		return
	}
}

func (self *Resident) clientForward() {
	defer self.cancel()
	
	// resident transports via `AddTransport` will route to the clientId only
	// add a route for all other destinations to route via the exchange forward

	forward := make(chan []byte, self.exchange.settings.ExchangeBufferSize)
	forwardTransport := connect.NewSendClientTransportWithComplement(true, connect.Id(self.clientId))

	routeManager := self.client.RouteManager()
	routeManager.UpdateTransport(forwardTransport, []connect.Route{forward})

	defer func() {
		routeManager.RemoveTransport(forwardTransport)
	}()

	for {
		select {
		case <- self.ctx.Done():
			return
		case transferFrameBytes := <- forward:
			filteredTransferFrame := &protocol.FilteredTransferFrame{}
			if err := proto.Unmarshal(transferFrameBytes, filteredTransferFrame); err != nil {
				// bad protobuf (unexpected)
				continue
			}
			if filteredTransferFrame.TransferPath == nil {
				// bad protobuf (unexpected)
				continue
			}
			sourceId, err := connect.IdFromBytes(filteredTransferFrame.TransferPath.SourceId)
			if err != nil {
				// bad protobuf (unexpected)
				continue
			}
			destinationId, err := connect.IdFromBytes(filteredTransferFrame.TransferPath.DestinationId)
			if err != nil {
				// bad protobuf (unexpected)
				continue
			}

			fmt.Printf("CLIENT FORWARD %s -> %s\n", sourceId.String(), destinationId.String())

			self.handleClientForward(sourceId, destinationId, transferFrameBytes)
		}
	}
}

// `connect.ForwardFunction`
func (self *Resident) handleClientForward(sourceId_ connect.Id, destinationId_ connect.Id, transferFrameBytes []byte) {
	sourceId := bringyour.Id(sourceId_)
	destinationId := bringyour.Id(destinationId_)

	// // bringyour.Logger().Printf("HANDLE CLIENT FORWARD %s %s %s %s\n", self.clientId.String(), sourceId.String(), destinationId.String(), transferFrameBytes)

	self.UpdateActivity()

	if sourceId != self.clientId {
		panic("Bad forward source.")
	}
	if destinationId == ControlId {
		panic("Bad forward destination.")
	}
	
	if sourceId != self.clientId {
		fmt.Printf("[abuse] Not from client (%s<>%s)\n", sourceId.String(), self.clientId.String())
		// the message is not from the client
		// clients are not allowed to forward from other clients
		// drop
		self.abuseLimiter.delay()
		return
	}

	// FIXME deep packet inspection to look at the contract frames and verify contracts before forwarding
	/*
	if self.exchange.settings.ForwardEnforceActiveContracts {		
		start := time.Now()
		hasActiveContract := self.residentContractManager.HasActiveContract(sourceId, destinationId)
		end := time.Now()
		millis := float32(end.Sub(start)) / float32(time.Millisecond)
		fmt.Printf("active contract = %t (%.2fms)\n", hasActiveContract, millis)
		if !hasActiveContract {
			fmt.Printf("[abuse] No active contract (%s->%s)\n", sourceId.String(), destinationId.String())
			// there is no active contract
			// drop
			self.abuseLimiter.delay()
			return
		}
	}
	*/

	bringyour.TraceWithReturn(
		fmt.Sprintf("[rf]handle client forward %s->%s", sourceId, destinationId),
		func()(bool) {

			nextForward := func()(*ResidentForward) {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()

				forward := NewResidentForward(self.ctx, self.exchange, destinationId)
				go func() {
					bringyour.HandleError(forward.Run)
					forward.Close()

					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					
					if currentForward := self.forwards[destinationId]; forward == currentForward {
						delete(self.forwards, destinationId)
					}
				}()
				go func(){
					defer forward.Cancel()
					for {
						if forward.IsIdle() {
							return
						}

						select {
						case <- self.ctx.Done():
							return
						case <- forward.Done():
							return
						case <- time.After(self.exchange.settings.ForwardIdleTimeout):
						}
					}
				}()
				
				if replacedForward, ok := self.forwards[destinationId]; ok {
					replacedForward.Cancel()
				}

				self.forwards[destinationId] = forward

				return forward
			}


			var forward *ResidentForward
			limit := false
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				var ok bool
				forward, ok = self.forwards[destinationId]
				if !ok && self.exchange.settings.MaxConcurrentForwardsPerResident <= len(self.forwards) {
					limit = true
				}
			}()

			if forward == nil && limit {
				fmt.Sprintf("[rf]abuse limit %s->%s", sourceId, destinationId)
				self.abuseLimiter.delay()
				return false
			}

			if forward == nil {
				forward = nextForward()
			}

			// test if the forward is alive
			if forward.IsDone() {
				// recreate the forward
				forward = nextForward()
			}
		
			select {
			case <- self.ctx.Done():
				return false
			case <- forward.Done():
				return false
			case forward.send <- transferFrameBytes:
				forward.UpdateActivity()
				return true
			case <- time.After(self.exchange.settings.WriteTimeout):
				fmt.Sprintf("[rf]drop %s->%s", sourceId, destinationId)
				return false
			}
		},
	)
}

// `connect.ReceiveFunction`
func (self *Resident) handleClientReceive(sourceId_ connect.Id, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
	sourceId := bringyour.Id(sourceId_)

	// // bringyour.Logger().Printf("HANDLE CLIENT RECEIVE %s %s %s\n", sourceId.String(), provideMode, frames)

	// these are messages to the control id
	// use `client.Send` to send messages back to the client

	/*
	// the provideMode should always be `Network`, by configuration
	// `Network` is the highest privilege in the user network
	if model.ProvideMode(provideMode) != model.ProvideModeNetwork {
		// // bringyour.Logger().Printf("HANDLE CLIENT RECEIVE NOT IN NETWORK\n")
		return
	}
	*/

	if sourceId != self.clientId {
		// only messages from the resident client are processed by the resident
		return
	}

	self.UpdateActivity()
	self.controlLimiter.delay()

	for _, frame := range frames {
		// bringyour.Logger().Printf("HANDLE CLIENT RECEIVE FRAME %s\n", frame)
		if message, err := connect.FromFrame(frame); err == nil {
			bringyour.HandleError(func() {
				self.residentController.HandleControlMessage(message)
			})
		}
	}
}


// caller must close `receive`
func (self *Resident) AddTransport() (
	send chan[] byte,
	receive chan[] byte,
	closeTransport func(),
) {
	// fmt.Printf("resident add transport\n")

	send = make(chan []byte, self.exchange.settings.ExchangeBufferSize)
	receive = make(chan []byte, self.exchange.settings.ExchangeBufferSize)

	// in `connect` the transport is bidirectional
	// in the resident, each transport is a single direction
	transport := &clientTransport{
		sendTransport: connect.NewSendClientTransport(connect.Id(self.clientId)),
		receiveTransport: connect.NewReceiveGatewayTransport(),
	}

	// bringyour.Logger().Printf("ADD TRANSPORT %s\n", self.clientId.String())

	func() {
		routeManager := self.client.RouteManager()
		routeManager.UpdateTransport(transport.sendTransport, []connect.Route{send})
		routeManager.UpdateTransport(transport.receiveTransport, []connect.Route{receive})

		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.transports[transport] = true
	}()

	closeTransport = func() {
		// bringyour.Logger().Printf("REMOVE TRANSPORT %s\n", self.clientId.String())

		func() {
			routeManager := self.client.RouteManager()
			routeManager.RemoveTransport(transport.sendTransport)
			routeManager.RemoveTransport(transport.receiveTransport)

			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			delete(self.transports, transport)
		}()

		go func() {
			select {
			case <- self.ctx.Done():
				return
			case <- time.After(self.exchange.settings.TransportDrainTimeout):
			}

			close(send)
		}()
	}

	return
}

func (self *Resident) Forward(transferFrameBytes []byte) bool {
	self.UpdateActivity()
	return self.client.ForwardWithTimeout(transferFrameBytes, self.exchange.settings.WriteTimeout)
}

func (self *Resident) UpdateActivity() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.lastActivityTime = time.Now()
}

// idle if no transports and no activity in `ResidentIdleTimeout`
func (self *Resident) IsIdle() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if 0 < len(self.transports) {
		return false
	}

	idleTimeout := time.Now().Sub(self.lastActivityTime)
	return self.exchange.settings.ResidentIdleTimeout <= idleTimeout
}

func (self *Resident) IsDone() bool {
	select {
	case <- self.ctx.Done():
		return true
	case <- self.client.Done():
		return true
	default:
		return false
	}
}

func (self *Resident) Done() <-chan struct{} {
	return self.ctx.Done()
}

func (self *Resident) Cancel() {
	// debug.PrintStack()
	fmt.Printf("RESIDENT CANCEL\n")
	self.cancel()

	self.client.Cancel()
}

func (self *Resident) Close() {
	self.cancel()

	self.client.Cancel()

	// self.client.RemoveReceiveCallback(self.handleClientReceive)
	// self.client.RemoveForwardCallback(self.handleClientForward)
	self.clientReceiveUnsub()
	self.clientForwardUnsub()
	

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, forward := range self.forwards {
			forward.Cancel()
		}
	}()
}

// func (self *Resident) Cancel() {
// 	self.cancel()
// 	self.client.Cancel()
// }


type clientTransport struct {
	sendTransport connect.Transport
	receiveTransport connect.Transport
}


type limiter struct {
	ctx context.Context
	mutex sync.Mutex
	minTimeout time.Duration
	lastCheckTime time.Time
}

func newLimiter(ctx context.Context, minTimeout time.Duration) *limiter {
	return &limiter{
		ctx: ctx,
		minTimeout: minTimeout,
		lastCheckTime: time.Time{},
	}
}

// a simple delay since the last call
func (self *limiter) delay() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	now := time.Now()
	timeout := self.minTimeout - now.Sub(self.lastCheckTime)
	self.lastCheckTime = now
	// bringyour.Logger().Printf("DELAY FOR %s", timeout)
	if 0 < timeout {
		select {
		case <- self.ctx.Done():
		case <- time.After(timeout):
		}
	}
}