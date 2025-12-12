package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	// "errors"
	// mathrand "math/rand"
	cryptorand "crypto/rand"
	"slices"

	// "runtime/debug"

	"golang.org/x/exp/maps"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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

var ControlId = server.Id(connect.ControlId)

// use 0 for deadlock testing
// the client can suffer from head of queue blocking when the forwards/clients are changing rates
// (flow control in the application protocol level should establish a steady rate)
// a larger exchange buffer size helps mitigate this
const DefaultExchangeBufferSize = 1024

// message writes on all layers have a single `WriteTimeout`
// this is because all layers have the same back pressure
// layers may have different read timeouts because of different keep alive/ping settings
type ExchangeSettings struct {
	ConnectHandlerSettings

	ExchangeBufferSize int

	MinContractTransferByteCount ByteCount

	StartInternalPort                int
	MaxConcurrentForwardsPerResident int

	ResidentIdleTimeout time.Duration
	ForwardIdleTimeout  time.Duration
	AbuseMinTimeout     time.Duration
	ControlMinTimeout   time.Duration

	ExchangeConnectTimeout             time.Duration
	ExchangePingTimeout                time.Duration
	ExchangeReadTimeout                time.Duration
	ExchangeReadHeaderTimeout          time.Duration
	ExchangeWriteHeaderTimeout         time.Duration
	ExchangeReconnectAfterErrorTimeout time.Duration

	ExchangeResidentTtl time.Duration

	ExchangeResidentWaitTimeout time.Duration
	ExchangeResidentPollTimeout time.Duration

	ForwardEnforceActiveContracts bool

	ContractManagerCheckTimeout time.Duration
	DrainOneTimeout             time.Duration

	ExchangeChaosSettings
}

type ExchangeChaosSettings struct {
	ResidentShutdownPerSecond float64
}

func DefaultExchangeSettings() *ExchangeSettings {
	connectionHandlerSettings := DefaultConnectHandlerSettings()
	exchangeResidentWaitTimeout := 30 * time.Second
	return &ExchangeSettings{
		ConnectHandlerSettings: *connectionHandlerSettings,

		ExchangeBufferSize: DefaultExchangeBufferSize,

		// 64kib minimum contract
		// this is set high enough to limit the number of parallel contracts and avoid contract spam
		MinContractTransferByteCount: ByteCount(64 * 1024),

		// this must match the warp `settings.yml` for the environment
		StartInternalPort: 5080,

		MaxConcurrentForwardsPerResident: 8 * 1024,

		ResidentIdleTimeout: 15 * time.Minute,
		ForwardIdleTimeout:  15 * time.Minute,
		AbuseMinTimeout:     5 * time.Second,
		ControlMinTimeout:   5 * time.Millisecond,

		ExchangeConnectTimeout:             5 * time.Second,
		ExchangePingTimeout:                connectionHandlerSettings.MinPingTimeout,
		ExchangeReadTimeout:                connectionHandlerSettings.ReadTimeout,
		ExchangeReadHeaderTimeout:          exchangeResidentWaitTimeout,
		ExchangeWriteHeaderTimeout:         exchangeResidentWaitTimeout,
		ExchangeReconnectAfterErrorTimeout: 1 * time.Second,
		ExchangeResidentTtl:                120 * time.Second,

		ExchangeResidentWaitTimeout: exchangeResidentWaitTimeout,
		ExchangeResidentPollTimeout: 15 * time.Second,

		ForwardEnforceActiveContracts: false,

		ExchangeChaosSettings: *DefaultExchangeChaosSettings(),
		// default drain 300/minute
		DrainOneTimeout: 200 * time.Millisecond,

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
	ctx    context.Context
	cancel context.CancelFunc

	host    string
	service string
	block   string
	// any of the ports may be used
	// a range of ports are used to scale one socket per transport or forward,
	// since each port allows at most 65k connections from another connect instance
	hostToServicePorts map[int]int
	routes             map[string]string

	settings *ExchangeSettings

	stateLock sync.Mutex
	// client id -> resident
	residents map[server.Id]*Resident

	// client id -> connection id -> cancel func
	connections map[server.Id]map[server.Id]context.CancelFunc
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
		ctx:                cancelCtx,
		cancel:             cancel,
		host:               host,
		service:            service,
		block:              block,
		hostToServicePorts: hostToServicePorts,
		routes:             routes,
		settings:           settings,
		residents:          map[server.Id]*Resident{},
		connections:        map[server.Id]map[server.Id]context.CancelFunc{},
	}

	go server.HandleError(exchange.Run, cancel)

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
	host := server.RequireHost()
	service := server.RequireService()
	block := server.RequireBlock()
	routes := server.Routes()

	// service port -> host port
	hostPorts := server.RequireHostPorts()
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
	clientId server.Id,
	instanceId server.Id,
	residentIdToReplace *server.Id,
) bool {
	residentId := server.NewId()

	nomination := &model.NetworkClientResident{
		ClientId:              clientId,
		InstanceId:            instanceId,
		ResidentId:            residentId,
		ResidentHost:          self.host,
		ResidentService:       self.service,
		ResidentBlock:         self.block,
		ResidentInternalPorts: maps.Keys(self.hostToServicePorts),
	}
	nominated := model.NominateResident(
		self.ctx,
		residentIdToReplace,
		nomination,
		self.settings.ExchangeResidentTtl,
	)
	if !nominated {
		return false
	}

	resident := NewResident(
		self.ctx,
		self,
		clientId,
		instanceId,
		residentId,
	)
	go server.HandleError(func() {
		server.HandleError(resident.Run)
		glog.V(1).Infof("[r]close %s\n", clientId)

		resident.Close()

		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if currentResident := self.residents[clientId]; resident == currentResident {
				delete(self.residents, clientId)
			}
		}()

		cleanupCtx := context.Background()
		model.RemoveResidentForClient(
			cleanupCtx,
			clientId,
			resident.residentId,
		)
	})
	go server.HandleError(func() {
		defer resident.Cancel()
		for {
			select {
			case <-resident.Done():
				return
			case <-time.After(self.settings.ResidentIdleTimeout):
			}

			if resident.IsIdle() {
				glog.V(1).Infof("[r]idle %s\n", clientId)
				return
			}
		}
	}, resident.Cancel)
	// poll the resident the same as exchange connections
	go server.HandleError(func() {
		defer resident.Cancel()
		for {
			select {
			case <-resident.Done():
				return
			case <-time.After(self.settings.ExchangeResidentTtl / 2):
			}

			pollResident := func() bool {
				return server.HandleErrorWithReturn(func() bool {
					currentResident := model.GetResidentForClientWithInstance(self.ctx, clientId, instanceId, self.settings.ExchangeResidentTtl)
					if currentResident == nil {
						return false
					}
					return residentId == currentResident.ResidentId
				})
			}

			if !pollResident() {
				glog.V(1).Infof("[r]not current %s\n", clientId)
				return
			}
		}
	}, resident.Cancel)

	var replacedResident *Resident
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		replacedResident = self.residents[clientId]
		self.residents[clientId] = resident
	}()
	if replacedResident != nil {
		replacedResident.Cancel()
	}
	glog.V(1).Infof("[r]open %s\n", clientId)

	return true
}

// runs the exchange to expose local nominated residents
// there should be one local exchange per service
func (self *Exchange) Run() {
	// start exchange connection servers
	for _, servicePort := range self.hostToServicePorts {
		port := servicePort
		go server.HandleError(
			func() {
				self.serveExchangeConnection(port)
			},
			self.cancel,
		)
	}

	select {
	case <-self.ctx.Done():
	}
}

func (self *Exchange) serveExchangeConnection(port int) {
	defer self.cancel()

	listenIpv4, _, listenPort := server.RequireListenIpPort(port)

	listenConfig := net.ListenConfig{
		Control: server.SoReusePort,
	}

	// leave host part empty to listen on all available interfaces
	serverSocket, err := listenConfig.Listen(
		self.ctx,
		"tcp",
		fmt.Sprintf("%s:%d", listenIpv4, listenPort),
	)
	if err != nil {
		return
	}
	defer serverSocket.Close()

	go server.HandleError(func() {
		defer self.cancel()

		for {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			conn, err := serverSocket.Accept()
			if err != nil {
				return
			}
			go server.HandleError(
				func() {
					self.handleExchangeConnection(conn)
				},
				self.cancel,
			)
		}
	}, self.cancel)

	select {
	case <-self.ctx.Done():
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

	connectionId := server.NewId()
	self.registerConnection(header.ClientId, connectionId, handleCancel)
	defer self.unregisterConnection(header.ClientId, connectionId)

	c := func() *Resident {
		endTime := time.Now().Add(self.settings.ExchangeResidentWaitTimeout)
		for {
			var resident *Resident
			var ok bool
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
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
			case <-handleCtx.Done():
				return nil
			case <-time.After(self.settings.ExchangeResidentPollTimeout):
			case <-time.After(timeout):
			}
		}
	}
	var resident *Resident
	if glog.V(2) {
		// use shallow log to avoid unsafe memory reads on print
		resident = server.TraceWithReturnShallowLog(
			fmt.Sprintf("[ecr]wait for resident %s/%s", header.ClientId, header.ResidentId),
			c,
		)
	} else {
		resident = c()
	}

	if resident == nil {
		glog.V(1).Infof("[ecr]no resident\n")
		return
	}

	if resident.IsDone() {
		glog.V(1).Infof("[ecr]resident done %s/%s\n", header.ClientId, header.ResidentId)
		return
	}

	// echo back the header
	if err := receiveBuffer.WriteHeader(handleCtx, conn, header); err != nil {
		glog.V(1).Infof("[ecr]write header %s/%s error = %s\n", header.ClientId, header.ResidentId, err)
		return
	}

	switch header.Op {
	case ExchangeOpTransport:
		// this must close `receive`
		send, receive, closeTransport := resident.AddTransport()
		defer closeTransport()

		go server.HandleError(func() {
			defer handleCancel()

			sendBuffer := NewDefaultExchangeBuffer(self.settings)
			for {
				select {
				case <-handleCtx.Done():
					return
				case <-resident.Done():
					return
				case message, ok := <-send:
					if !ok {
						return
					}
					resident.UpdateActivity()
					if err := sendBuffer.WriteMessage(handleCtx, conn, message); err != nil {
						return
					}

					glog.V(1).Infof("[ecrs] %s/%s\n", resident.clientId, resident.residentId)

				case <-time.After(self.settings.ExchangePingTimeout):
					// send a ping
					if err := sendBuffer.WriteMessage(handleCtx, conn, make([]byte, 0)); err != nil {
						return
					}
				}
			}
		}, handleCancel)

		go server.HandleError(func() {
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
					resident.UpdateActivity()
					continue
				}

				if glog.V(2) {
					if resident.IsDone() {
						glog.Warning("[ecrr] %s/%s done\n", resident.clientId, resident.residentId)
					}

					multiRouteReader := resident.client.RouteManager().OpenMultiRouteReader(connect.DestinationId(resident.client.ClientId()))
					if !slices.Contains(multiRouteReader.GetActiveRoutes(), receive) {
						glog.Warning("[ecrr] %s/%s missing receive route\n", resident.clientId, resident.residentId)
					}
					resident.client.RouteManager().CloseMultiRouteReader(multiRouteReader)
				}

				glog.V(2).Infof("[ecrr] %s/%s waiting\n", resident.clientId, resident.residentId)

				select {
				case <-handleCtx.Done():
					return
				case <-resident.Done():
					return
				case receive <- message:
					resident.UpdateActivity()
					glog.V(2).Infof("[ecrr] %s/%s\n", resident.clientId, resident.residentId)
				case <-time.After(self.settings.WriteTimeout):
					glog.V(1).Infof("[ecrr]drop %s/%s\n", resident.clientId, resident.residentId)
				}
			}
		}, handleCancel)

	case ExchangeOpForward:
		// read
		// messages from the forward are to be forwarded by the resident
		// the only route a resident has is to its client_id
		// a forward is a send where the source id does not match the client

		forward, closeForward := resident.AddForward()
		defer closeForward()

		go server.HandleError(func() {
			defer handleCancel()

			sendBuffer := NewDefaultExchangeBuffer(self.settings)
			for {
				select {
				case <-handleCtx.Done():
					return
				case <-resident.Done():
					return
				case <-time.After(self.settings.ExchangePingTimeout):
					// send a ping
					if err := sendBuffer.WriteMessage(handleCtx, conn, make([]byte, 0)); err != nil {
						return
					}
				}
			}
		}, handleCancel)

		go server.HandleError(func() {
			defer handleCancel()

			for {
				message, err := receiveBuffer.ReadMessage(handleCtx, conn)
				if err != nil {
					return
				}
				if len(message) == 0 {
					// just a ping
					resident.UpdateActivity()
					continue
				}

				select {
				case <-handleCtx.Done():
					return
				case <-resident.Done():
					return
				case forward <- message:
					resident.UpdateActivity()
					glog.V(2).Infof("[ecrf]forward %s/%s", resident.clientId, resident.residentId)
				case <-time.After(self.settings.WriteTimeout):
					glog.V(1).Infof("[ecrf]drop %s/%s", resident.clientId, resident.residentId)
				}
			}
		}, handleCancel)
	}

	select {
	case <-handleCtx.Done():
		glog.V(1).Infof("[ecr]handle done\n")
	case <-resident.Done():
		glog.V(1).Infof("[ecr]resident done\n")
	}
}

func (self *Exchange) registerConnection(clientId server.Id, connectionId server.Id, handleCancel context.CancelFunc) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	handleCancels, ok := self.connections[clientId]
	if !ok {
		handleCancels = map[server.Id]context.CancelFunc{}
		self.connections[clientId] = handleCancels
	}
	handleCancels[connectionId] = handleCancel
}

func (self *Exchange) unregisterConnection(clientId server.Id, connectionId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	handleCancels, ok := self.connections[clientId]
	if ok {
		delete(handleCancels, connectionId)
		if len(handleCancels) == 0 {
			delete(self.connections, clientId)
		}
	}
}

func (self *Exchange) Drain() {
	for i := 0; ; i += 1 {
		select {
		case <-self.ctx.Done():
			return
		default:
		}
		resident, handleCancels, remainingCount, ok := func() (*Resident, []context.CancelFunc, int, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			n := max(len(self.connections), len(self.residents))

			// active connections with potential residents
			for clientId, handleCancels := range self.connections {
				resident := self.residents[clientId]
				return resident, maps.Values(handleCancels), n - 1, true
			}
			// residents without active connections
			for _, resident := range self.residents {
				return resident, nil, n - 1, true
			}
			return nil, nil, 0, false
		}()
		if !ok {
			glog.Infof("[c]drain complete\n")
			break
		}
		if i%100 == 0 {
			glog.Infof("[c][%d]drain in progress (at least %d remaining)\n", i, remainingCount)
		}
		if resident != nil {
			resident.Close()
		}
		for _, handleCancel := range handleCancels {
			handleCancel()
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.DrainOneTimeout):
		}
	}
}

func (self *Exchange) Close() {
	self.cancel()
}

// each call overwrites the internal buffer
type ExchangeBuffer struct {
	settings *ExchangeSettings
	buffer   []byte
}

func NewDefaultExchangeBuffer(settings *ExchangeSettings) *ExchangeBuffer {
	return &ExchangeBuffer{
		settings: settings,
		buffer:   make([]byte, settings.MaximumExchangeMessageByteCount+4),
	}
}

func NewReceiveOnlyExchangeBuffer(settings *ExchangeSettings) *ExchangeBuffer {
	return &ExchangeBuffer{
		settings: settings,
		buffer:   make([]byte, 33),
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
		ClientId:   server.Id(self.buffer[0:16]),
		ResidentId: server.Id(self.buffer[16:32]),
		Op:         ExchangeOp(self.buffer[32]),
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
	ClientId   server.Id
	ResidentId server.Id
	Op         ExchangeOp
}

type ExchangeConnection struct {
	ctx           context.Context
	cancel        context.CancelFunc
	op            ExchangeOp
	conn          net.Conn
	sendBuffer    *ExchangeBuffer
	receiveBuffer *ExchangeBuffer
	send          chan []byte
	receive       chan []byte
	settings      *ExchangeSettings

	clientId   server.Id
	residentId server.Id
	host       string
	port       int
}

func NewExchangeConnection(
	ctx context.Context,
	clientId server.Id,
	residentId server.Id,
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
		Op:         op,
		ClientId:   clientId,
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
		ctx:           cancelCtx,
		cancel:        cancel,
		op:            op,
		conn:          conn,
		sendBuffer:    sendBuffer,
		receiveBuffer: NewReceiveOnlyExchangeBuffer(settings),
		send:          make(chan []byte, settings.ExchangeBufferSize),
		receive:       make(chan []byte, settings.ExchangeBufferSize),
		settings:      settings,
		clientId:      clientId,
		residentId:    residentId,
		host:          host,
		port:          port,
	}
	go server.HandleError(connection.Run, cancel)

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
		go server.HandleError(func() {
			defer func() {
				self.cancel()
				close(self.receive)
			}()

			for {
				select {
				case <-self.ctx.Done():
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
				case <-self.ctx.Done():
					return
				case self.receive <- message:
					glog.V(2).Infof("[ecr] %s/%s@%s:%d\n", self.clientId, self.residentId, self.host, self.port)
				case <-time.After(self.settings.WriteTimeout):
					glog.V(1).Infof("[ecr]drop %s/%s@%s:%d\n", self.clientId, self.residentId, self.host, self.port)
				}
			}
		}, self.cancel)
	case ExchangeOpForward:
		// nothing to receive, but time out on missing pings
		close(self.receive)

		go server.HandleError(func() {
			defer self.cancel()

			for {
				select {
				case <-self.ctx.Done():
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

	go server.HandleError(func() {
		defer self.cancel()

		for {
			select {
			case <-self.ctx.Done():
				return
			case message, ok := <-self.send:
				if !ok {
					return
				}
				if err := self.sendBuffer.WriteMessage(self.ctx, self.conn, message); err != nil {
					return
				}
				glog.V(2).Infof("[ecs] %s/%s@%s:%d\n", self.clientId, self.residentId, self.host, self.port)
			case <-time.After(self.settings.ExchangePingTimeout):
				// send a ping
				if err := self.sendBuffer.WriteMessage(self.ctx, self.conn, make([]byte, 0)); err != nil {
					return
				}
			}
		}
	}, self.cancel)

	select {
	case <-self.ctx.Done():
	}
}

func (self *ExchangeConnection) IsDone() bool {
	select {
	case <-self.ctx.Done():
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
	ctx    context.Context
	cancel context.CancelFunc

	exchange *Exchange

	clientId   server.Id
	instanceId server.Id

	routes map[string]string

	send    chan []byte
	receive chan []byte
}

func NewResidentTransport(
	ctx context.Context,
	exchange *Exchange,
	clientId server.Id,
	instanceId server.Id,
) *ResidentTransport {
	cancelCtx, cancel := context.WithCancel(ctx)
	transport := &ResidentTransport{
		ctx:        cancelCtx,
		cancel:     cancel,
		exchange:   exchange,
		clientId:   clientId,
		instanceId: instanceId,
		send:       make(chan []byte, exchange.settings.ExchangeBufferSize),
		receive:    make(chan []byte, exchange.settings.ExchangeBufferSize),
	}
	return transport
}

func (self *ResidentTransport) Run() {
	defer func() {
		self.cancel()
		close(self.receive)
	}()

	handle := func(connection *ExchangeConnection) {
		handleCtx, handleCancel := context.WithCancel(self.ctx)
		defer handleCancel()

		go server.HandleError(func() {
			defer func() {
				handleCancel()
				connection.Close()
			}()

			// write
			for {
				select {
				case <-handleCtx.Done():
					return
				case <-connection.Done():
					return
				case message, ok := <-self.send:
					if !ok {
						// transport closed
						self.cancel()
						return
					}
					select {
					case <-handleCtx.Done():
						return
					case <-connection.Done():
						return
					case connection.send <- message:
					case <-time.After(self.exchange.settings.WriteTimeout):
					}
				}
			}
		}, self.cancel)

		// read
		for {
			select {
			case <-handleCtx.Done():
				return
			case <-connection.Done():
				return
			case message, ok := <-connection.receive:
				if !ok {
					// need a new connection
					return
				}
				select {
				case <-handleCtx.Done():
					return
				case <-connection.Done():
					return
				case self.receive <- message:
				case <-time.After(self.exchange.settings.WriteTimeout):
					glog.V(1).Infof("[rt]drop %s->\n", self.clientId)
				}
			}
		}
	}

	for {
		reconnect := connect.NewReconnect(self.exchange.settings.ExchangeReconnectAfterErrorTimeout)
		resident := model.GetResidentForClientWithInstance(self.ctx, self.clientId, self.instanceId, self.exchange.settings.ExchangeResidentTtl)
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
				glog.V(1).Infof("[rt]exchange connection error %s->%s@%s:%d = %s\n", self.clientId, resident.ResidentId, resident.ResidentHost, port, err)
			}

			if err == nil {
				c := func() {
					handle(exchangeConnection)
				}
				if glog.V(2) {
					server.Trace(
						fmt.Sprintf("[rt]exchange connection %s->%s@%s:%d", self.clientId, resident.ResidentId, resident.ResidentHost, port),
						c,
					)
				} else {
					c()
				}
			}
		}

		select {
		case <-self.ctx.Done():
			return
		case <-reconnect.After():
		}

		var residentIdToReplace *server.Id
		if resident != nil {
			residentIdToReplace = &resident.ResidentId
		}

		c := func() bool {
			return self.exchange.NominateLocalResident(
				self.clientId,
				self.instanceId,
				residentIdToReplace,
			)
		}
		if glog.V(2) {
			server.TraceWithReturn(
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
	case <-self.ctx.Done():
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
	ctx    context.Context
	cancel context.CancelFunc

	exchange *Exchange

	clientId server.Id

	send chan []byte

	stateLock        sync.Mutex
	lastActivityTime time.Time
}

func NewResidentForward(
	ctx context.Context,
	exchange *Exchange,
	clientId server.Id,
) *ResidentForward {
	cancelCtx, cancel := context.WithCancel(ctx)
	transport := &ResidentForward{
		ctx:              cancelCtx,
		cancel:           cancel,
		exchange:         exchange,
		clientId:         clientId,
		send:             make(chan []byte, exchange.settings.ExchangeBufferSize),
		lastActivityTime: time.Now(),
	}
	return transport
}

func (self *ResidentForward) Run() {
	defer self.cancel()

	handle := func(connection *ExchangeConnection) {
		handleCtx, handleCancel := context.WithCancel(self.ctx)
		defer func() {
			handleCancel()
			connection.Close()
		}()

		// write
		for {
			select {
			case <-handleCtx.Done():
				return
			case message, ok := <-self.send:
				if !ok {
					// transport closed
					return
				}
				select {
				case <-handleCtx.Done():
					return
				case <-connection.Done():
					return
				case connection.send <- message:
				case <-time.After(self.exchange.settings.WriteTimeout):
					glog.V(1).Infof("[rf]drop %s->\n", self.clientId)
				}
			}
		}
	}

	for {
		reconnect := connect.NewReconnect(self.exchange.settings.ExchangeReconnectAfterErrorTimeout)
		resident := model.GetResidentForClient(self.ctx, self.clientId, self.exchange.settings.ExchangeResidentTtl)
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
				glog.V(1).Infof("[rf]exchange connection error %s->%s@%s:%d = %s\n", self.clientId, resident.ResidentId, resident.ResidentHost, port, err)
			}
			if err == nil {
				c := func() {
					handle(exchangeConnection)
				}
				if glog.V(2) {
					server.Trace(
						fmt.Sprintf("[rf]exchange connection %s->%s@%s:%d", self.clientId, resident.ResidentId, resident.ResidentHost, port),
						c,
					)
				} else {
					c()
				}
			}
		}
		select {
		case <-self.ctx.Done():
			return
		case <-reconnect.After():
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
	case <-self.ctx.Done():
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
	ctx    context.Context
	cancel context.CancelFunc

	exchange *Exchange

	clientId   server.Id
	instanceId server.Id
	residentId server.Id

	// the client id in the resident is always `connect.ControlId`
	client                  *connect.Client
	residentContractManager *residentContractManager
	residentController      *residentController

	stateLock sync.Mutex

	transports map[*clientTransport]bool

	// destination id -> forward
	forwards map[server.Id]*ResidentForward

	abuseLimiter   *limiter
	controlLimiter *limiter

	lastActivityTime time.Time

	clientReceiveUnsub func()
	clientForwardUnsub func()
}

func NewResident(
	ctx context.Context,
	exchange *Exchange,
	clientId server.Id,
	instanceId server.Id,
	residentId server.Id,
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
		ctx:                     cancelCtx,
		cancel:                  cancel,
		exchange:                exchange,
		clientId:                clientId,
		instanceId:              instanceId,
		residentId:              residentId,
		client:                  client,
		residentContractManager: residentContractManager,
		residentController:      residentController,
		transports:              map[*clientTransport]bool{},
		forwards:                map[server.Id]*ResidentForward{},
		abuseLimiter:            newLimiter(cancelCtx, exchange.settings.AbuseMinTimeout),
		controlLimiter:          newLimiter(cancelCtx, exchange.settings.ControlMinTimeout),
		lastActivityTime:        time.Now(),
	}

	clientReceiveUnsub := client.AddReceiveCallback(resident.handleClientReceive)
	resident.clientReceiveUnsub = clientReceiveUnsub

	clientForwardUnsub := client.AddForwardCallback(resident.handleClientForward)
	resident.clientForwardUnsub = clientForwardUnsub

	// go server.HandleError(resident.clientForward, cancel)
	if 0 < exchange.settings.ExchangeChaosSettings.ResidentShutdownPerSecond {
		go server.HandleError(resident.chaos, cancel)
	}

	return resident
}

func (self *Resident) chaos() {
	defer self.Cancel()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}

		bs := make([]byte, 1)
		cryptorand.Read(bs)
		b := int(bs[0])
		c := int(256.0 * self.exchange.settings.ExchangeChaosSettings.ResidentShutdownPerSecond)

		if b < c {
			glog.Infof("[chaos][%s]%d <> %d\n", self.residentId, b, c)
			return
		}
	}
}

func (self *Resident) Run() {
	defer self.cancel()

	select {
	case <-self.ctx.Done():
	case <-self.client.Done():
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
func (self *Resident) handleClientForward(path connect.TransferPath, transferFrameBytes []byte) {
	sourceId := server.Id(path.SourceId)
	destinationId := server.Id(path.DestinationId)

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
				glog.Infof("[rf]abuse no active contract %s->%s\n", sourceId, destinationId)
				// there is no active contract
				// drop
				self.abuseLimiter.delay()
				return
			}
		}
	}

	c := func() bool {

		nextForward := func() *ResidentForward {
			forward := NewResidentForward(self.ctx, self.exchange, destinationId)
			go server.HandleError(func() {
				forward.Run()

				glog.V(1).Infof("[rf]close %s->%s\n", sourceId, destinationId)

				// note we don't call close here because only the sender should call close
				forward.Cancel()
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					if currentForward := self.forwards[destinationId]; forward == currentForward {
						delete(self.forwards, destinationId)
					}
				}()
			}, forward.Cancel)
			go server.HandleError(func() {
				defer forward.Cancel()
				for {
					if forward.IsIdle() {
						glog.V(1).Infof("[rf]idle %s->%s\n", sourceId, destinationId)
						return
					}

					select {
					case <-self.ctx.Done():
						return
					case <-forward.Done():
						return
					case <-time.After(self.exchange.settings.ForwardIdleTimeout):
					}
				}
			}, forward.Cancel)

			var replacedForward *ResidentForward
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				replacedForward = self.forwards[destinationId]
				self.forwards[destinationId] = forward
			}()
			if replacedForward != nil {
				replacedForward.Cancel()
			}
			glog.V(1).Infof("[rf]open %s->%s\n", sourceId, destinationId)

			return forward
		}

		limit := false
		var forward *ResidentForward
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
			glog.Infof("[rf]abuse forward limit %s->%s", sourceId, destinationId)
			self.abuseLimiter.delay()
			return false
		}

		if forward == nil || forward.IsDone() {
			forward = nextForward()
		}

		select {
		case <-self.ctx.Done():
			return false
		case <-forward.Done():
			return false
		case forward.send <- transferFrameBytes:
			forward.UpdateActivity()
			return true
		case <-time.After(self.exchange.settings.WriteTimeout):
			glog.V(1).Infof("[rf]drop %s->%s", sourceId, destinationId)
			return false
		}
	}

	if glog.V(2) {
		server.TraceWithReturn(
			fmt.Sprintf("[rf]handle client forward %s->%s", sourceId, destinationId),
			c,
		)
	} else {
		c()
	}
}

// `connect.ReceiveFunction`
func (self *Resident) handleClientReceive(source connect.TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
	sourceId := server.Id(source.SourceId)

	if sourceId != self.clientId {
		glog.V(1).Infof("[rr]abuse not from client (%s<>%s)\n", sourceId, self.clientId)
		// only messages from the resident client are processed by the resident
		// drop
		self.abuseLimiter.delay()
		return
	}

	self.UpdateActivity()
	self.controlLimiter.delay()

	err := self.residentController.HandleControlFrames(frames)
	if err == nil {
		glog.V(1).Infof("[rr]control error = %s\n", err)
	}
}

// caller must close `receive`
func (self *Resident) AddTransport() (
	send chan []byte,
	receive chan []byte,
	closeTransport func(),
) {
	send = make(chan []byte, self.exchange.settings.ExchangeBufferSize)
	receive = make(chan []byte, self.exchange.settings.ExchangeBufferSize)

	// in `connect` the transport is bidirectional
	// in the resident, each transport is a single direction
	transport := &clientTransport{
		sendTransport:    connect.NewSendClientTransport(connect.DestinationId(connect.Id(self.clientId))),
		receiveTransport: connect.NewReceiveGatewayTransport(),
	}

	routeManager := self.client.RouteManager()
	routeManager.UpdateTransport(transport.sendTransport, []connect.Route{send})
	routeManager.UpdateTransport(transport.receiveTransport, []connect.Route{receive})

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.transports[transport] = true
	}()

	closeTransport = func() {
		routeManager := self.client.RouteManager()
		routeManager.RemoveTransport(transport.sendTransport)
		routeManager.RemoveTransport(transport.receiveTransport)

		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			delete(self.transports, transport)
		}()

		// note `send` is not closed. This channel is left open.
		// it used to be closed after a delay, but it is not needed to close it.
	}

	return
}

func (self *Resident) AddForward() (
	forward chan []byte,
	closeForward func(),
) {
	forward = make(chan []byte, self.exchange.settings.ExchangeBufferSize)

	forwardCtx, forwardCancel := context.WithCancel(self.ctx)

	go server.HandleError(func() {
		defer forwardCancel()
		for {
			select {
			case <-forwardCtx.Done():
				return
			case message := <-forward:
				self.client.ForwardWithTimeout(message, self.exchange.settings.WriteTimeout)
			}
		}
	}, self.cancel)

	closeForward = func() {
		forwardCancel()

		// note `forward` is not closed. This channel is left open.
		// it used to be closed after a delay, but it is not needed to close it.
	}

	return
}

// func (self *Resident) Forward(transferFrameBytes []byte) bool {
// 	self.UpdateActivity()
// 	return self.client.ForwardWithTimeout(transferFrameBytes, self.exchange.settings.WriteTimeout)
// }

func (self *Resident) UpdateActivity() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.lastActivityTime = time.Now()
}

// idle if no activity in `ResidentIdleTimeout`
func (self *Resident) IsIdle() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	idleTimeout := time.Now().Sub(self.lastActivityTime)
	return self.exchange.settings.ResidentIdleTimeout <= idleTimeout
}

func (self *Resident) IsDone() bool {
	select {
	case <-self.ctx.Done():
		return true
	case <-self.client.Done():
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

	forwards := []*ResidentForward{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		forwards = maps.Values(self.forwards)
		clear(self.forwards)
	}()
	for _, forward := range forwards {
		forward.Cancel()
	}
}

type clientTransport struct {
	sendTransport    connect.Transport
	receiveTransport connect.Transport
}

type limiter struct {
	ctx           context.Context
	mutex         sync.Mutex
	minTimeout    time.Duration
	lastCheckTime time.Time
}

func newLimiter(ctx context.Context, minTimeout time.Duration) *limiter {
	return &limiter{
		ctx:           ctx,
		minTimeout:    minTimeout,
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
		case <-self.ctx.Done():
		case <-time.After(timeout):
		}
	}
}

func isAck(transferFrameBytes []byte) bool {
	var filteredTransferFrameWithFrame protocol.FilteredTransferFrameWithFrame
	if err := proto.Unmarshal(transferFrameBytes, &filteredTransferFrameWithFrame); err != nil {
		// bad protobuf
		return false
	}
	if filteredTransferFrameWithFrame.Frame != nil {
		return filteredTransferFrameWithFrame.Frame.MessageType == protocol.MessageType_TransferAck
	}
	if filteredTransferFrameWithFrame.MessageType != nil {
		return *filteredTransferFrameWithFrame.MessageType == protocol.MessageType_TransferAck
	}
	return false
}
