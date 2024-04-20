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
	// "errors"
	"slices"
	mathrand "math/rand"
	// "runtime/debug"

	"golang.org/x/exp/maps"

	"google.golang.org/protobuf/proto"

	"github.com/golang/glog"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/connect"
	"bringyour.com/protocol"
)


// note -
// we use one socket per client transport because the socket will block based on the slowest destination


/*
resident packet flow A->D

client (A)
	-> routes
		-> client transport (ws)
			-> exchange transport (ws)
			    -> resident transport (exchange connection)
			        -> routes 
			            -> control resident A (clientId=ControlId) 
			      	        -> client receive (resident_controller)
			      	        -> client forward
			      	        	-> resident forward D (exchange connection)
			      	        		-> resident.forward
			      	        			-> control resident D (clientId=ControlId)
			      	        				resident.forward sends the data on the most appropriate route
			      	        				which for D will put the transfer frame on client (D)  
			      	        				(see the reflection of client to resident flow)
			      	        				-> routes
			      	        					-> resident transport (exchange connection)
			      	        						-> exchange transport (ws)
			      	        							-> client transport (ws)
			      	        								-> routes
			      	        									-> client(D)
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

	MinContractTransferByteCount ByteCount

	StartInternalPort int
	MaxConcurrentForwardsPerResident int

	ResidentIdleTimeout time.Duration
	// ResidentSyncTimeout time.Duration
	ForwardIdleTimeout time.Duration
	// ContractSyncTimeout time.Duration
	AbuseMinTimeout time.Duration
	ControlMinTimeout time.Duration

	// ClientDrainTimeout time.Duration
	// TransportDrainTimeout time.Duration

	ExchangeConnectTimeout time.Duration
	ExchangePingTimeout time.Duration
	ExchangeReadTimeout time.Duration
	ExchangeReadHeaderTimeout time.Duration
	ExchangeWriteHeaderTimeout time.Duration
	ExchangeReconnectAfterErrorTimeout time.Duration

	ExchangeConnectionResidentPollTimeout time.Duration

	ExchangeResidentWaitTimeout time.Duration
	ExchangeResidentPollTimeout time.Duration

	ForwardEnforceActiveContracts bool

	ContractManagerCheckTimeout time.Duration

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

		ResidentIdleTimeout: 60 * time.Minute,
		// ResidentSyncTimeout: 30 * time.Second,
		ForwardIdleTimeout: 60 * time.Minute,
		// ContractSyncTimeout: 60 * time.Second,
		AbuseMinTimeout: 5 * time.Second,
		ControlMinTimeout: 5 * time.Millisecond,

		// ClientDrainTimeout: 30 * time.Second,
		// TransportDrainTimeout: 30 * time.Second,

		ExchangeConnectTimeout: 5 * time.Second,
		ExchangePingTimeout: exchangePingTimeout,
		ExchangeReadTimeout: 2 * exchangePingTimeout,
		ExchangeReadHeaderTimeout: exchangeResidentWaitTimeout,
		// ExchangeWriteTimeout: 5 * time.Second,
		ExchangeWriteHeaderTimeout: exchangeResidentWaitTimeout,
		ExchangeReconnectAfterErrorTimeout: 1 * time.Second,
		ExchangeConnectionResidentPollTimeout: 30 * time.Second,

		ExchangeResidentWaitTimeout: exchangeResidentWaitTimeout,
		ExchangeResidentPollTimeout: exchangeResidentWaitTimeout / 8,

		ForwardEnforceActiveContracts: false,

		ExchangeChaosSettings: *DefaultExchangeChaosSettings(),

		ContractManagerCheckTimeout: 5 * time.Second,
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
	if !nominated {
		return false
	}

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
			glog.Infof("[r]close %s\n", clientId)

			self.residentsLock.Lock()
			defer self.residentsLock.Unlock()
			resident.Close()
			if currentResident := self.residents[clientId]; resident == currentResident {
				delete(self.residents, clientId)
			}
			// this will remove the resident only if current
			// it will always clear ports associated with the resident
			model.RemoveResident(
				self.ctx,
				resident.clientId,
				resident.residentId,
			)
		})
		go func() {
			defer resident.Cancel()
			for {
				select {
				case <- resident.Done():
					return
				case <- time.After(self.settings.ResidentIdleTimeout):
				}
				
				if resident.IsIdle() {
					glog.Infof("[r]idle %s\n", clientId)
					return
				}
			}
		}()
		// poll the resident the same as exchange connections
		go func() {
			defer resident.Cancel()
			for {
				select {
				case <- resident.Done():
					return
				case <- time.After(self.settings.ExchangeConnectionResidentPollTimeout):
				}

				pollResident := func()(bool) {
					currentResidentId, err := model.GetResidentIdWithInstance(self.ctx, clientId, instanceId)
					if err != nil {
						return false
					}
					return residentId == currentResidentId
				}
				
				if !pollResident() {
					glog.Infof("[r]not current %s\n", clientId)
					return
				}
			}
		}()
		
		if replacedResident, ok := self.residents[clientId]; ok {
			replacedResident.Cancel()
		}
		self.residents[clientId] = resident
		glog.Infof("[r]open %s\n", clientId)
	}()

	return true
}

// runs the exchange to expose local nominated residents
// there should be one local exchange per service
func (self *Exchange) Run() {
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
	defer conn.Close()
	handleCtx, handleCancel := context.WithCancel(self.ctx)
	defer handleCancel()

	receiveBuffer := NewReceiveOnlyExchangeBuffer(self.settings)

	header, err := receiveBuffer.ReadHeader(handleCtx, conn)
	if err != nil {
		return
	}

	c := func()(*Resident) {
		endTime := time.Now().Add(self.settings.ExchangeResidentWaitTimeout)
		for {
			var resident *Resident
			var ok bool
			func() {
				self.residentsLock.Lock()
				defer self.residentsLock.Unlock()

				resident, ok = self.residents[header.ClientId]
			}()

			if ok && resident.residentId == header.ResidentId {
				return resident
			}

			glog.V(1).Infof("[ecr]wait for resident %s/%s\n", header.ClientId, header.ResidentId)

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
	}
	var resident *Resident
	if glog.V(2) {
		// use shallow log to avoid unsafe memory reads on print
		resident = bringyour.TraceWithReturnShallowLog(
			fmt.Sprintf("[ecr]wait for resident %s/%s", header.ClientId, header.ResidentId),
			c,
		)
	} else {
		resident = c()
	}

	if resident == nil {
		glog.Infof("[ecr]no resident\n")
		return
	}

	if resident.IsDone() {
		glog.Infof("[ecr]resident done %s/%s\n", header.ClientId, header.ResidentId)
		return
	}

	// echo back the header
	if err := receiveBuffer.WriteHeader(handleCtx, conn, header); err != nil {
		glog.Infof("[ecr]write header %s/%s error = %s\n", header.ClientId, header.ResidentId, err)
		return
	}

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
					if !ok {
						return
					}
					resident.UpdateActivity()
					if err := sendBuffer.WriteMessage(handleCtx, conn, message); err != nil {
						return
					}

					glog.V(1).Infof("[ecrs] %s/%s\n", resident.clientId, resident.residentId)

				case <- time.After(self.settings.ExchangePingTimeout):
					// send a ping
					if err := sendBuffer.WriteMessage(handleCtx, conn, make([]byte, 0)); err != nil {
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
				if err != nil {
					return
				}
				if len(message) == 0 {
					// just a ping
					continue
				}
				
				if glog.V(2) {
					if resident.IsDone() {
						glog.Warning("[ecrr] %s/%s done\n", resident.clientId, resident.residentId)
					}

					multiRouteReader := resident.client.RouteManager().OpenMultiRouteReader(resident.client.ClientId())
					if !slices.Contains(multiRouteReader.GetActiveRoutes(), receive) {
						glog.Warning("[ecrr] %s/%s missing receive route\n", resident.clientId, resident.residentId)
					}
					resident.client.RouteManager().CloseMultiRouteReader(multiRouteReader)
				}

				glog.V(2).Infof("[ecrr] %s/%s waiting\n", resident.clientId, resident.residentId)

				select {
				case <- handleCtx.Done():
					return
				case <- resident.Done():
					return
				case receive <- message:
					resident.UpdateActivity()
					glog.V(2).Infof("[ecrr] %s/%s\n", resident.clientId, resident.residentId)
				case <- time.After(self.settings.WriteTimeout):
					glog.V(1).Infof("[ecrr]drop %s/%s\n", resident.clientId, resident.residentId)
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
						return
					}
				}
			}
		}, handleCancel)

		go bringyour.HandleError(func() {
			defer handleCancel()

			for {
				message, err := receiveBuffer.ReadMessage(handleCtx, conn)
				if err != nil {
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

				c := func()(bool) {
					return resident.Forward(message) 
				}
				if glog.V(2) {
					bringyour.TraceWithReturn(
						fmt.Sprintf("[ecrf]forward %s/%s", resident.clientId, resident.residentId),
						c,
					)
				} else {
					c()
				}
			}
		}, handleCancel)
	}

	select {
	case <- handleCtx.Done():
		glog.Infof("[ecr]handle done\n")
	case <- resident.Done():
		glog.Infof("[ecr]resident done\n")
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
	copy(self.buffer[0:16], header.ClientId.Bytes())
	copy(self.buffer[16:32], header.ResidentId.Bytes())
	self.buffer[32] = byte(header.Op)

	conn.SetWriteDeadline(time.Now().Add(self.settings.ExchangeWriteHeaderTimeout))
	_, err := conn.Write(self.buffer[0:33])
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
	n := ByteCount(len(transferFrameBytes))

	if self.settings.MaximumExchangeMessageByteCount < n {
		return fmt.Errorf("Maximum message size is %d (%d).", self.settings.MaximumExchangeMessageByteCount, n)
	}

	binary.LittleEndian.PutUint32(self.buffer[0:4], uint32(n))

	conn.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
	_, err := conn.Write(self.buffer[0:4])
	if err != nil {
		return err
	}
	_, err = conn.Write(transferFrameBytes)
	return err
}

func (self *ExchangeBuffer) ReadMessage(ctx context.Context, conn net.Conn) ([]byte, error) {
	conn.SetReadDeadline(time.Now().Add(self.settings.ExchangeReadTimeout))
	if _, err := io.ReadFull(conn, self.buffer[0:4]); err != nil {
		return nil, err
	}

	n := ByteCount(int(binary.LittleEndian.Uint32(self.buffer[0:4])))
	if self.settings.MaximumExchangeMessageByteCount < n {
		return nil, fmt.Errorf("Maximum message size is %d (%d).", self.settings.MaximumExchangeMessageByteCount, n)
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
	// forward calls `Forward` on the resident and does not use routes
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
	hostRoute, ok := routes[host]
	if !ok {
		// use the hostname as the route
		// this requires the DNS to be configured correctly at the site
		hostRoute = host
	}

	authority := fmt.Sprintf("%s:%d", hostRoute, port)

	dialer := net.Dialer{
		Timeout: settings.ExchangeConnectTimeout,
	}
	conn, err := dialer.DialContext(ctx, "tcp", authority)
	if err != nil {
		return nil, err
	}
	// tcpConn := conn.(*net.TCPConn)
	// tcpConn.SetNoDelay(false)

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
		return nil, err
	}

	// the connection echoes back the header if connected to the resident
	// else the connection is closed
	_, err = sendBuffer.ReadHeader(ctx, conn)
	if err != nil {
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
					return
				}
				if len(message) == 0 {
					// just a ping
					continue
				}

				select {
				case <- self.ctx.Done():
					return
				case self.receive <- message:
					glog.V(2).Infof("[ecr] %s/%s@%s:%d\n", self.clientId, self.residentId, self.host, self.port)
				case <- time.After(self.settings.WriteTimeout):
					glog.V(1).Infof("[ecr]drop %s/%s@%s:%d\n", self.clientId, self.residentId, self.host, self.port)
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
					return
				}
				if len(message) == 0 {
					// just a ping
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
					return
				}
				glog.V(2).Infof("[ecs] %s/%s@%s:%d\n", self.clientId, self.residentId, self.host, self.port)
			case <- time.After(self.settings.ExchangePingTimeout):
				// send a ping
				if err := self.sendBuffer.WriteMessage(self.ctx, self.conn, make([]byte, 0)); err != nil {
					return
				}
			}
		}
	}, self.cancel)

	select {
	case <- self.ctx.Done():
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
					return
				case <- connection.Done():
					return
				case message, ok := <- self.send:
					if !ok {
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
					}
				}
			}
		}, self.cancel)

		// read
		for {
			select {
			case <- handleCtx.Done():
				return
			case <- connection.Done():
				return
			case message, ok := <- connection.receive:
				if !ok {
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
					glog.V(1).Infof("[rt]drop %s->\n", self.clientId)
				}
			}
		}
	}

	for {
		resident := model.GetResidentWithInstance(self.ctx, self.clientId, self.instanceId)
		retryDelay := false
		if resident != nil && 0 < len(resident.ResidentInternalPorts) {
			port := resident.ResidentInternalPorts[rand.Intn(len(resident.ResidentInternalPorts))]
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
				glog.Infof("[rt]exchange connection error %s->%s@%s:%d = %s\n", self.clientId, resident.ResidentId, resident.ResidentHost, port, err)
				retryDelay = true
			}

			if err == nil {
				c := func() {
					// TODO the current test would be more efficient as a model notification instead of polling
					handle(exchangeConnection, func()(bool) {
						currentResidentId, err := model.GetResidentIdWithInstance(self.ctx, self.clientId, self.instanceId)
						if err != nil {
							return false
						}
						return resident.ResidentId == currentResidentId
					})
				}
				if glog.V(2) {
					bringyour.Trace(
						fmt.Sprintf("[rt]exchange connection %s->%s@%s:%d", self.clientId, resident.ResidentId, resident.ResidentHost, port),
						c,
					)
				} else {
					c()
				}
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

		c := func()(bool) {				
			return self.exchange.NominateLocalResident(
				self.clientId,
				self.instanceId,
				residentIdToReplace,
			)
		}
		if glog.V(2) {
			bringyour.TraceWithReturn(
				fmt.Sprintf("[rt]nominate %s", self.clientId),
				c,
			)
		} else {
			c()
		}
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
					glog.Infof("[rf]drop %s->\n", self.clientId)
				}
			}
		}
	}

	for {
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
				glog.Infof("[rf]exchange connection error %s->%s@%s:%d = %s\n", self.clientId, resident.ResidentId, resident.ResidentHost, port, err)
			}
			if err == nil {
				c := func() {
					// handleCtx, handleCancel := context.WithCancel(self.ctx)
					handle(exchangeConnection, func()(bool) {
						currentResidentId, err := model.GetResidentId(self.ctx, self.clientId)
						if err != nil {
							return false
						}
						return resident.ResidentId == currentResidentId
					})
					retryDelay = false
				}
				if glog.V(2) {
					bringyour.Trace(
						fmt.Sprintf("[rf]exchange connection %s->%s@%s:%d", self.clientId, resident.ResidentId, resident.ResidentHost, port),
						c,
					)
				} else {
					c()
				}
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

	// go bringyour.HandleError(resident.clientForward, cancel)
	if 0 < exchange.settings.ExchangeChaosSettings.ResidentShutdownPerSecond {
		go bringyour.HandleError(resident.chaos, cancel)
	}

	return resident
}

func (self *Resident) chaos() {
	for {
		select {
		case <- self.ctx.Done():
			return
		case <- time.After(1 * time.Second):
		}

		if mathrand.Float64() < self.exchange.settings.ExchangeChaosSettings.ResidentShutdownPerSecond {
			glog.Infof("[chaos]%s\n", self.residentId)
			self.Cancel()
		}
	}
}

func (self *Resident) Run() {
	defer self.cancel()

	select {
	case <- self.ctx.Done():
	case <- self.client.Done():
	}
}

/*
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

			self.handleClientForward(sourceId, destinationId, transferFrameBytes)
		}
	}
}
*/

// `connect.ForwardFunction`
func (self *Resident) handleClientForward(sourceId_ connect.Id, destinationId_ connect.Id, transferFrameBytes []byte) {
	sourceId := bringyour.Id(sourceId_)
	destinationId := bringyour.Id(destinationId_)

	self.UpdateActivity()

	if destinationId == ControlId {
		// the resident client id is `ControlId`. It should never forward to itself.
		panic("Bad forward destination.")
	}
	
	if sourceId != self.clientId {
		glog.Infof("[rf]abuse not from client (%s<>%s)\n", sourceId, self.clientId)
		// the message is not from the client
		// clients are not allowed to forward from other clients
		// drop
		self.abuseLimiter.delay()
		return
	}

	// FIXME deep packet inspection to look at the contract frames and verify contracts before forwarding

	if self.exchange.settings.ForwardEnforceActiveContracts {
		if !isAck(transferFrameBytes) {
			hasActiveContract := self.residentContractManager.HasActiveContract(sourceId, destinationId)
			if !hasActiveContract {
				fmt.Printf("[rf]abuse no active contract %s->%s\n", sourceId, destinationId)
				// there is no active contract
				// drop
				self.abuseLimiter.delay()
				return
			}
		}
	}

	c := func()(bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		nextForward := func()(*ResidentForward) {
			forward := NewResidentForward(self.ctx, self.exchange, destinationId)
			go func() {
				bringyour.HandleError(forward.Run)
				glog.Infof("[rf]close %s->%s\n", sourceId, destinationId)

				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				forward.Close()
				if currentForward := self.forwards[destinationId]; forward == currentForward {
					delete(self.forwards, destinationId)
				}
			}()
			go func(){
				defer forward.Cancel()
				for {
					if forward.IsIdle() {
						glog.Infof("[rf]idle %s->%s\n", sourceId, destinationId)
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
			glog.Infof("[rf]open %s->%s\n", sourceId, destinationId)

			return forward
		}


		limit := false
		forward, ok := self.forwards[destinationId]
		if !ok && self.exchange.settings.MaxConcurrentForwardsPerResident <= len(self.forwards) {
			limit = true
		}

		if forward == nil && limit {
			glog.Infof("[rf]abuse forward limit %s->%s", sourceId, destinationId)
			self.abuseLimiter.delay()
			return false
		}

		if forward == nil || forward.IsDone() {
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
			glog.Infof("[rf]drop %s->%s", sourceId, destinationId)
			return false
		}
	}

	if glog.V(2) {
		bringyour.TraceWithReturn(
			fmt.Sprintf("[rf]handle client forward %s->%s", sourceId, destinationId),
			c,
		)
	} else {
		c()
	}
}

// `connect.ReceiveFunction`
func (self *Resident) handleClientReceive(sourceId_ connect.Id, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
	sourceId := bringyour.Id(sourceId_)

	if sourceId != self.clientId {
		glog.Infof("[rr]abuse not from client (%s<>%s)\n", sourceId, self.clientId)
		// only messages from the resident client are processed by the resident
		// drop
		self.abuseLimiter.delay()
		return
	}

	self.UpdateActivity()
	self.controlLimiter.delay()

	for _, frame := range frames {
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
	send = make(chan []byte, self.exchange.settings.ExchangeBufferSize)
	receive = make(chan []byte, self.exchange.settings.ExchangeBufferSize)

	// in `connect` the transport is bidirectional
	// in the resident, each transport is a single direction
	transport := &clientTransport{
		sendTransport: connect.NewSendClientTransport(connect.Id(self.clientId)),
		receiveTransport: connect.NewReceiveGatewayTransport(),
	}

	func() {
		routeManager := self.client.RouteManager()
		routeManager.UpdateTransport(transport.sendTransport, []connect.Route{send})
		routeManager.UpdateTransport(transport.receiveTransport, []connect.Route{receive})

		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.transports[transport] = true
	}()

	closeTransport = func() {
		func() {
			routeManager := self.client.RouteManager()
			routeManager.RemoveTransport(transport.sendTransport)
			routeManager.RemoveTransport(transport.receiveTransport)

			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			delete(self.transports, transport)
		}()

		// go func() {
		// 	select {
		// 	case <- self.ctx.Done():
		// 		return
		// 	case <- time.After(self.exchange.settings.TransportDrainTimeout):
		// 	}

		// 	close(send)
		// }()
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
	self.cancel()
	self.client.Cancel()
}

func (self *Resident) Close() {
	self.cancel()
	self.client.Cancel()

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
	if 0 < timeout {
		glog.V(2).Infof("[limiter]delay for %.2fms\n", float64(timeout)/float64(time.Millisecond))
		select {
		case <- self.ctx.Done():
		case <- time.After(timeout):
		}
	}
}


func isAck(transferFrameBytes []byte) bool {
	var filteredTransferFrameWithFrame protocol.FilteredTransferFrameWithFrame
	if err := proto.Unmarshal(transferFrameBytes, &filteredTransferFrameWithFrame); err != nil {
		// bad protobuf
		return false
	}
	return filteredTransferFrameWithFrame.Frame.MessageType == protocol.MessageType_TransferAck
}
