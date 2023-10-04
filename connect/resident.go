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

	"golang.org/x/exp/maps"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/connect"
	"bringyour.com/protocol"
)


// note -
// we use one socket per client transport because the socket will block based on the slowest destination



// because message must be serialized/deserialized from memory,
// there is a global limit on the size per message
// messages above this size will be ignored from clients and the exchange
const MaximumMessageSizeBytes = 2048

// 8Gib minimum contract
// this is set high enough to limit the number of parallel contracts and avoid contract spam
const MinContractTransferBytes = 8 * 1024 * 1024 * 1024

const StartInternalPort = 5080
const MaxConcurrentForwardsPerResident = 32

const ResidentIdleTimeout = 5 * time.Minute
const ForwardReconnectTimeout = 30 * time.Second
const ResidentSyncTimeout = 30 * time.Second
const ForwardIdleTimeout = 1 * time.Minute
const ContractSyncTimeout = 30 * time.Second
const AbuseMinTimeout = 5 * time.Second
const ControlMinTimeout = 200 * time.Millisecond

const ExchangeConnectTimeout = 1 * time.Second


// each call overwrites the internal buffer
type ExchangeBuffer struct {
	buffer []byte
}

func NewDefaultExchangeBuffer() *ExchangeBuffer {
	return &ExchangeBuffer{
		buffer: make([]byte, MaximumMessageSizeBytes + 4),
	} 
}

func NewReceiveOnlyExchangeBuffer() *ExchangeBuffer {
	return &ExchangeBuffer{
		buffer: make([]byte, 33),
	} 
}

func (self *ExchangeBuffer) WriteHeader(ctx context.Context, conn net.Conn, header *ExchangeHeader) error {
	copy(self.buffer[0:16], header.ClientId.Bytes())
	copy(self.buffer[16:32], header.ResidentId.Bytes())
	self.buffer[32] = byte(header.Op)

	_, err := conn.Write(self.buffer[0:33])
	return err
}

func (self *ExchangeBuffer) ReadHeader(ctx context.Context, conn net.Conn) (*ExchangeHeader, error) {
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
	n := len(transferFrameBytes)

	if MaximumMessageSizeBytes < n {
		return errors.New(fmt.Sprintf("Maximum message size is %d (%d).", MaximumMessageSizeBytes, n))
	}

	binary.LittleEndian.PutUint32(self.buffer[0:4], uint32(n))
	copy(self.buffer[4:4+n], transferFrameBytes)

	_, err := conn.Write(self.buffer[0:4+n])
	return err
}

func (self *ExchangeBuffer) ReadMessage(ctx context.Context, conn net.Conn) ([]byte, error) {
	if _, err := io.ReadFull(conn, self.buffer[0:4]); err != nil {
		return nil, err
	}

	n := int(binary.LittleEndian.Uint32(self.buffer[0:4]))
	if MaximumMessageSizeBytes < n {
		return nil, errors.New(fmt.Sprintf("Maximum message size is %d (%d).", MaximumMessageSizeBytes, n))
	}

	// read into a new buffer
	message := make([]byte, n)

	if _, err := io.ReadFull(conn, message); err != nil {
		return nil, err
	}

	return message, nil
}


func safeSend[T any](ctx context.Context, channel chan T, message T) (err error) {
	defer func() {
		err = recover().(error)
	}()
	select {
	case channel <- message:
		return nil
	case <- ctx.Done():
		return errors.New("Done.")
	}
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
}

func NewExchangeConnection(
	ctx context.Context,
	clientId bringyour.Id,
	residentId bringyour.Id,
	host string,
	port int,
	op ExchangeOp,
) (*ExchangeConnection, error) {
	dialer := net.Dialer{
		Timeout: ExchangeConnectTimeout,
	}
	conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return nil, err
	}

	sendBuffer := NewDefaultExchangeBuffer()

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

	cancelCtx, cancel := context.WithCancel(ctx)

	connection := &ExchangeConnection{
		ctx: cancelCtx,
		cancel: cancel,
		op: op,
		conn: conn,
		sendBuffer: sendBuffer,
		receiveBuffer: NewReceiveOnlyExchangeBuffer(),
		send: make(chan []byte),
		receive: make(chan []byte),
	}
	go connection.Run()

	return connection, nil
}

func (self *ExchangeConnection) Run() {
	closeConnection := func() {
		self.cancel()
		close(self.send)
		close(self.receive)
		self.conn.Close()
	}
	defer closeConnection()

	// only a transport connection will receive messages
	switch self.op {
	case ExchangeOpTransport:
		go func() {
			defer closeConnection()
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
				select {
				case <- self.ctx.Done():
					return
				case self.receive <- message:
				}
			}
		}()
	default:
		close(self.receive)
	}

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
		}
	}
}

func (self *ExchangeConnection) Close() {
	close(self.send)
}


type ResidentTransport struct {	
	ctx context.Context
	cancel context.CancelFunc

	exchange *Exchange

	clientId bringyour.Id
	instanceId bringyour.Id

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
		send: make(chan []byte),
		receive: make(chan []byte),
	}
	go transport.Run()
	return transport
}

func (self *ResidentTransport) Run() {
	closeTransport := func() {
		self.cancel()
		close(self.send)
		close(self.receive)
	}
	defer closeTransport()

	handle := func(connection *ExchangeConnection) {
		closeHandle := func() {
			connection.Close()
		}
		defer closeHandle()

		go func() {
			defer closeHandle()
			// read
			for {
				select {
				case message, ok := <- connection.receive:
					if !ok {
						// need a new connection
						return
					}
					if err := safeSend(self.ctx, self.receive, message); err != nil {
						// transport closed
						closeTransport()
						return
					}
				case <- self.ctx.Done():
					return
				}
			}
		}()

		// write
		for {
			select {
			case message, ok := <- self.send:
				if !ok {
					// transport closed
					closeTransport()
					return
				}
				if err := safeSend(self.ctx, connection.send, message); err != nil {
					// need a new connection
					return
				}
			case <- self.ctx.Done():
				return
			}
		}
	}

	for {
		resident := model.GetResidentWithInstance(self.ctx, self.clientId, self.instanceId)
		if resident != nil && 0 < len(resident.ResidentInternalPorts) {
			port := resident.ResidentInternalPorts[rand.Intn(len(resident.ResidentInternalPorts))]
			exchangeConnection, err := NewExchangeConnection(
				self.ctx,
				self.clientId,
				resident.ResidentId,
				resident.ResidentHost,
				port,
				ExchangeOpTransport,
			)
			if err == nil {
				handle(exchangeConnection)
			}
		}
		select {
		case <- self.ctx.Done():
			return
		default:
		}
		var residentIdToReplace *bringyour.Id
		if resident != nil {
			residentIdToReplace = &resident.ResidentId
		}
		self.exchange.NominateLocalResident(self.clientId, self.instanceId, residentIdToReplace)
	}
}

func (self *ResidentTransport) Close() {
	close(self.send)
}


type ResidentForward struct {	
	ctx context.Context
	cancel context.CancelFunc

	exchange *Exchange

	clientId bringyour.Id

	send chan []byte
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
		send: make(chan []byte),
	}
	go transport.Run()
	return transport
}

func (self *ResidentForward) Run() {
	closeForward := func() {
		self.cancel()
		close(self.send)
	}
	defer closeForward()

	handle := func(connection *ExchangeConnection) {
		closeHandle := func() {
			connection.Close()
		}
		defer closeHandle()

		// write
		for {
			select {
			case message, ok := <- self.send:
				if !ok {
					// transport closed
					closeForward()
					return
				}
				if err := safeSend(self.ctx, connection.send, message); err != nil {
					// need a new connection
					return
				}
			case <- self.ctx.Done():
				return
			}
		}
	}

	for {
		resident := model.GetResident(self.ctx, self.clientId)
		if resident != nil && 0 < len(resident.ResidentInternalPorts) {
			port := resident.ResidentInternalPorts[rand.Intn(len(resident.ResidentInternalPorts))]
			exchangeConnection, err := NewExchangeConnection(
				self.ctx,
				self.clientId,
				resident.ResidentId,
				resident.ResidentHost,
				port,
				ExchangeOpForward,
			)
			if err == nil {
				handle(exchangeConnection)
			}
		}
		select {
		case <- self.ctx.Done():
			return
		case <- time.After(ForwardReconnectTimeout):
		}
	}
}

func (self *ResidentForward) Close() {
	close(self.send)
}


// residents live in the exchange
// a resident for a client id can be nominated to live in the exchange with `NominateLocalResident`
// any time a resident is not reachable by a transport, the transport should nominate a local resident
type Exchange struct {
	ctx context.Context
	cancel context.CancelFunc

	host string
	// any of the ports may be used
	// a range of ports are used to scale one socket per transport or forward,
	// since each port allows at most 65k connections from another connect instance
	ports []int

	residentsLock sync.RWMutex
	// clientId -> Resident
	residents map[bringyour.Id]*Resident
}

func NewExchange(ctx context.Context, host string, ports []int) *Exchange {
	cancelCtx, cancel := context.WithCancel(ctx)

	exchange := &Exchange{
		ctx: cancelCtx,
		cancel: cancel,
		host: host,
		ports: ports,
		residents: map[bringyour.Id]*Resident{},
	}

	go exchange.syncResidents()

	return exchange
}

// reads the host and port configuration from the env
func NewExchangeFromEnv(ctx context.Context) *Exchange {
	cancelCtx, cancel := context.WithCancel(ctx)

	host := bringyour.RequireHost()

	// service port -> host port
	hostPorts := bringyour.RequireHostPorts()
	// internal ports start at `StartInternalPort` and proceed consecutively
	// each port can handle 65k connections
	// the number of connections depends on the number of expected concurrent destinations
	// the expected port usage is `number_of_residents * expected(number_of_destinations_per_resident)`,
	// and at most `number_of_residents * MaxConcurrentForwardsPerResident`

	ports := []int{}
	servicePort := StartInternalPort
	for {
		hostPort, ok := hostPorts[servicePort]
		if !ok {
			break
		}
		ports = append(ports, hostPort)
		servicePort += 1
	}
	if len(ports) == 0 {
		panic(fmt.Errorf("No exchange internal ports found (starting with service port %d).", StartInternalPort))
	}

	exchange := &Exchange{
		ctx: cancelCtx,
		cancel: cancel,
		host: host,
		ports: ports,
		residents: map[bringyour.Id]*Resident{},
	}

	go exchange.syncResidents()

	return exchange
}

func (self *Exchange) NominateLocalResident(
	clientId bringyour.Id,
	instanceId bringyour.Id,
	residentIdToReplace *bringyour.Id,
) error {
	residentId := bringyour.NewId()
	resident := NewResident(
		self.ctx,
		self,
		clientId,
		instanceId,
		residentId,
	)
	success := false
	defer func() {
		if !success {
			resident.Close()
			self.residentsLock.Lock()
			if currentResident, ok := self.residents[clientId]; ok && currentResident == resident {
				delete(self.residents, clientId)
			}
			self.residentsLock.Unlock()
		}
	}()

	// make sure the new resident is local before nominating
	// this will prevent failed connections from other exchanges if the nomination succeeds
	self.residentsLock.Lock()
	currentResident, ok := self.residents[clientId]
	localReplaced := !ok && residentIdToReplace == nil || ok && residentIdToReplace != nil && currentResident.residentId == *residentIdToReplace
	if localReplaced {
		self.residents[clientId] = resident
	}
	self.residentsLock.Unlock()

	if localReplaced {
		nominated := model.NominateResident(self.ctx, residentIdToReplace, &model.NetworkClientResident{
			ClientId: clientId,
			InstanceId: instanceId,
			ResidentId: residentId,
			ResidentHost: self.host,
			ResidentService: bringyour.RequireService(),
			ResidentBlock: bringyour.RequireBlock(),
			ResidentInternalPorts: self.ports,
		})
		if nominated.ResidentId == resident.residentId {
			success = true
		}
	}

	if !success {
		return errors.New("Another resident was nominated.")
	}

	return nil
}

// continually cleans up the local resident state, connections, and model based on the latest nominations
func (self *Exchange) syncResidents() {
	// watch for this resident to change
	// FIMXE close all connection IDs for this resident on change
	lastRunTime := time.Now()
	for {
		timeout := lastRunTime.Add(ResidentSyncTimeout).Sub(time.Now())
		if 0 < timeout {
			select {
			case <- self.ctx.Done():
				return
			case <- time.After(ResidentSyncTimeout):
			}
		} else {
			select {
			case <- self.ctx.Done():
				return
			default:
			}
		}

		lastRunTime = time.Now()


		// check for differences between local residents and the model

		residentsToClose := []*Resident{}

		residentsForHostPort := model.GetResidentsForHostPorts(self.ctx, self.host, self.ports)
		residentIdsForHostPort := map[bringyour.Id]bool{}
		for _, residentsForHostPort := range residentsForHostPort {
			residentIdsForHostPort[residentsForHostPort.ResidentId] = true
		}

		residentsForHostPortToRemove := []*model.NetworkClientResident{}
		
		self.residentsLock.Lock()
		residentIds := map[bringyour.Id]bool{}
		for _, resident := range self.residents {
			residentIds[resident.residentId] = true
		}
		for _, residentForHostPort := range residentsForHostPort {
			if _, ok := residentIds[residentForHostPort.ResidentId]; !ok {
				residentsForHostPortToRemove = append(residentsForHostPortToRemove, residentForHostPort)
			}
		}
		for _, resident := range self.residents {
			if _, ok := residentIdsForHostPort[resident.residentId]; !ok {
				// this resident has been removed from the model
				residentsToClose = append(residentsToClose, resident)
				delete(self.residents, resident.clientId)
			}
		}
		self.residentsLock.Unlock()

		for _, residentForHostPort := range residentsForHostPortToRemove {
			model.RemoveResident(self.ctx, residentForHostPort.ClientId, residentForHostPort.ResidentId)
		}

		for _, resident := range residentsToClose {
			resident.Close()
		}


		// check for residents with no transports, and have no activity in some time
		residentsToRemove := []*Resident{}

		self.residentsLock.Lock()
		for _, resident := range self.residents {
			if resident.IsIdle() {
				residentsToRemove = append(residentsToRemove, resident)
				delete(self.residents, resident.clientId)
			}
		}
		self.residentsLock.Unlock()

		for _, resident := range residentsToRemove {
			model.RemoveResident(self.ctx, resident.clientId, resident.residentId)
			resident.Close()
		}
	}
}

// runs the exchange to expose local nominated residents
// there should be one local exchange per service
func (self *Exchange) Run() {
	defer func() {
		residentsCopy := map[bringyour.Id]*Resident{}
		self.residentsLock.Lock()
		maps.Copy(residentsCopy, self.residents)
		clear(self.residents)
		self.residentsLock.Unlock()
		for _, resident := range residentsCopy {
			resident.Close()
			model.RemoveResident(self.ctx, resident.clientId, resident.residentId)
		}
	}()

	for _, port := range self.ports {
		go func() {
			// leave host part empty to listen on all available interfaces
			server, err := net.Listen("tcp", fmt.Sprintf("%d", port))
			if err != nil {
				return
			}
			defer server.Close()

			for {
				select {
				case <- self.ctx.Done():
					return
				default:
				}

				socket, err := server.Accept()
				if err != nil {
					return
				}
				go self.handleExchangeClient(socket)
			}
		}()
	}

	self.syncResidents()
}

func (self *Exchange) handleExchangeClient(conn net.Conn) {
	closeClient := func() {
		conn.Close()
	}
	defer closeClient()

	receiveBuffer := NewReceiveOnlyExchangeBuffer()

	header, err := receiveBuffer.ReadHeader(self.ctx, conn)
	if err != nil {
		return
	}

	self.residentsLock.RLock()
	resident, ok := self.residents[header.ClientId]
	self.residentsLock.RUnlock()

	if !ok || resident.residentId != header.ResidentId {
		return
	}

	// echo back the header
	if err := receiveBuffer.WriteHeader(self.ctx, conn, header); err != nil {
		return
	}

	switch header.Op {
	case ExchangeOpTransport:
		send := make(chan []byte)
		receive := make(chan []byte)
		go func() {
			defer func() {
				close(send)
				closeClient()
			}()

			sendBuffer := NewDefaultExchangeBuffer()
			for {
				select {
				case message, ok := <- send:
					if !ok {
						return
					}
					if err := sendBuffer.WriteMessage(self.ctx, conn, message); err != nil {
						return
					}
				case <- self.ctx.Done():
					return
				}
			}
		}()
		closeTransport := resident.AddTransport(send, receive)
		defer closeTransport()

		// read
		// messages from the transport are to be received by the resident
		// messages not destined for the control id are handled by the resident forward
		for {
			message, err := receiveBuffer.ReadMessage(self.ctx, conn)
			if err != nil {
				return
			}
			if err := safeSend(self.ctx, receive, message); err != nil {
				return
			}
		}
	case ExchangeOpForward:
		// read
		// messages from the forward are to be forwarded by the resident
		// the only route a resident has is to its client_id
		// a forward is a send where the source id does not match the client
		for {
			message, err := receiveBuffer.ReadMessage(self.ctx, conn)
			if err != nil {
				return
			}
			resident.Forward(message)
		}
	}	
}


func (self *Exchange) Close() {
	self.cancel()
	
	// close all residents
	self.residentsLock.Lock()
	for _, resident := range self.residents {
		resident.Close()
		model.RemoveResident(self.ctx, resident.clientId, resident.residentId)	
	}
	clear(self.residents)
	self.residentsLock.Unlock()
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
	clientRouteManager *connect.RouteManager
	clientContractManager *connect.ContractManager
	contractManager *contractManager

	stateLock sync.Mutex

	transports map[*clientTransport]bool

	// destination id -> forward
	forwards map[bringyour.Id]*clientForward

	lastActivityTime time.Time

	abuseLimiter *limiter
	controlLimiter *limiter
}

func NewResident(
	ctx context.Context,
	exchange *Exchange,
	clientId bringyour.Id,
	instanceId bringyour.Id,
	residentId bringyour.Id,
) *Resident {
	cancelCtx, cancel := context.WithCancel(ctx)

	client := connect.NewClientWithDefaults(connect.ControlId, cancelCtx)

	clientRouteManager := connect.NewRouteManager(client)
	clientContractManager := connect.NewContractManagerWithDefaults(client)

	resident := &Resident{
		ctx: cancelCtx,
		cancel: cancel,
		exchange: exchange,
		client: client,
		clientRouteManager: clientRouteManager,
		clientContractManager: clientContractManager,
		contractManager: newContractManager(cancelCtx, clientId),
		transports: map[*clientTransport]bool{},
		forwards: map[bringyour.Id]*clientForward{},
		abuseLimiter: newLimiter(cancelCtx, AbuseMinTimeout),
		controlLimiter: newLimiter(cancelCtx, ControlMinTimeout),
	}

	client.AddReceiveCallback(resident.clientReceive)
	client.AddForwardCallback(resident.clientForward)

	go client.Run(clientRouteManager, clientContractManager)
	go resident.cleanupForwards()

	return resident
}

func (self *Resident) updateActivity() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lastActivityTime = time.Now()
}

func (self *Resident) cleanupForwards() {
	for {
		select {
		case <- self.ctx.Done():
			return
		default:
		}

		// clean up forwards that have not been used after an idle timeout

		self.stateLock.Lock()
		forwardsToRemove := []*clientForward{}
		for _, forward := range self.forwards {
			if ForwardIdleTimeout <= time.Now().Sub(forward.lastActivityTime) {
				forwardsToRemove = append(forwardsToRemove, forward)
			}
		}
		for _, forward := range forwardsToRemove {
			forward.Close()
			delete(self.forwards, forward.clientId)
		}
		self.stateLock.Unlock()
	}
}

// connect.ForwardFunction
func (self *Resident) clientForward(sourceId_ connect.Id, destinationId_ connect.Id, transferFrameBytes []byte) {
	self.updateActivity()

	sourceId := bringyour.Id(sourceId_)
	destinationId := bringyour.Id(destinationId_)

	if sourceId != self.clientId {
		// the message is not from the client
		// clients are not allowed to forward from other clients
		// drop
		self.abuseLimiter.delay()
		return
	}

	if !self.contractManager.HasActiveContract(sourceId, destinationId) {
		// there is no active contract
		// drop
		self.abuseLimiter.delay()
		return
	}

	self.stateLock.Lock()
	forward, ok := self.forwards[destinationId]
	if ok {
		forward.lastActivityTime = time.Now()
	} else if len(self.forwards) < MaxConcurrentForwardsPerResident {
		residentForward := NewResidentForward(self.ctx, self.exchange, destinationId)
		forward = &clientForward{
			ResidentForward: *residentForward,
			lastActivityTime: time.Now(),
		}
		self.forwards[destinationId] = forward
	}
	self.stateLock.Unlock()

	if forward != nil {
		select {
		case <- self.ctx.Done():
		case forward.send <- transferFrameBytes:
		}
	}
	// else drop the message
}

// connect.ReceiveFunction
func (self *Resident) clientReceive(sourceId_ connect.Id, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
	// these are messages to the control id
	// use `client.Send` to send messages back to the client

	self.updateActivity()
	self.controlLimiter.delay()

	for _, frame := range frames {
		switch frame.MessageType {
		case protocol.MessageType_TransferProvide:
			// FIXME
		case protocol.MessageType_TransferCreateContract:
			// FIXME always grant a contract from ControlId to any
			// FIXME
		case protocol.MessageType_TransferCloseContract:
			// FIXME
		}
	}
}

func (self *Resident) AddTransport(send chan []byte, receive chan []byte) func() {
	// in `connect` the transport is bidirectional
	// in the resident, each transport is a single direction
	transport := &clientTransport{
		sendTransport: newClientSendTransport(self.clientId, send),
		receiveTransport: newClientReceiveTransport(self.clientId, receive),
	}

	self.stateLock.Lock()
	self.transports[transport] = true
	self.clientRouteManager.UpdateTransport(transport.sendTransport, []connect.Route{send})
	self.clientRouteManager.UpdateTransport(transport.receiveTransport, []connect.Route{receive})
	self.stateLock.Unlock()

	return func() {
		self.stateLock.Lock()
		self.clientRouteManager.RemoveTransport(transport.sendTransport)
		self.clientRouteManager.RemoveTransport(transport.receiveTransport)
		delete(self.transports, transport)
		self.stateLock.Unlock()

		transport.Close()
	}
}

func (self *Resident) Forward(transferFrameBytes []byte) {
	self.updateActivity()
	self.client.Forward(transferFrameBytes)
}

// idle if no transports and no activity in `ResidentIdleTimeout`
func (self *Resident) IsIdle() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if 0 < len(self.transports) {
		return false
	}

	idleTimeout := time.Now().Sub(self.lastActivityTime)
	return ResidentIdleTimeout <= idleTimeout
}

func (self *Resident) Close() {
	self.cancel()

	self.client.Close()

	self.stateLock.Lock()
	// clear forwards
	for _, forward := range self.forwards {
		forward.Close()
	}
	clear(self.forwards)
	// clear transports
	for transport, _ := range self.transports {
		transport.Close()
	}
	clear(self.transports)
	self.stateLock.Unlock()
}


type contractManager struct {
	ctx context.Context

	clientId bringyour.Id

	stateLock sync.Mutex
	// unordered transfer pair -> contract ids
	pairContractIds map[model.TransferPair]map[bringyour.Id]bool
}

func newContractManager(ctx context.Context, clientId bringyour.Id) *contractManager {
	contractManager := &contractManager {
		ctx: ctx,
		clientId: clientId,
		pairContractIds: model.GetOpenContractIdsForSourceOrDestination(clientId),
	}

	go contractManager.syncContracts()

	return contractManager
}

func (self *contractManager) syncContracts() {
	for {
		select {
		case <- self.ctx.Done():
			return
		case <- time.After(ContractSyncTimeout):
		}

		pairContractIds_ := model.GetOpenContractIdsForSourceOrDestination(self.clientId)
		self.stateLock.Lock()
		self.pairContractIds = pairContractIds_
		// if a contract was added between the sync and set, it will be looked up from the model on miss
		self.stateLock.Unlock()
	}
}

func (self *contractManager) HasActiveContract(sourceId bringyour.Id, destinationId bringyour.Id) bool {
	transferPair := model.NewUnorderedTransferPair(sourceId, destinationId)

	self.stateLock.Lock()
	contracts, ok := self.pairContractIds[transferPair]
	self.stateLock.Unlock()

	if !ok {
		contractIds := model.GetOpenContractIds(sourceId, destinationId)
		contracts := map[bringyour.Id]bool{}
		for _, contractId := range contractIds {
			contracts[contractId] = true
		}
		self.stateLock.Lock()
		// if no contracts, store an empty map as a cache miss until the next `syncContracts` iteration
		self.pairContractIds[transferPair] = contracts
		self.stateLock.Unlock()
	}

	return 0 < len(contracts)
}

func (self *contractManager) CreateContract(
	sourceId bringyour.Id,
	destinationId bringyour.Id,
	transferBytes int,
) *model.TransferEscrow {
	contractTransferBytes := max(MinContractTransferBytes, transferBytes)
	escrow := model.CreateTransferEscrow(sourceId, destinationId, contractTransferBytes)

	// update the cache
	transferPair := model.NewUnorderedTransferPair(sourceId, destinationId)
	self.stateLock.Lock()
	contracts, ok := self.pairContractIds[transferPair]
	if !ok {
		contracts := map[bringyour.Id]bool{}
		self.pairContractIds[transferPair] = contracts
	}
	contracts[escrow.ContractId] = true
	self.stateLock.Unlock()

	return escrow
}


// each send on the forward updates the send time
// the cleanup removes forwards that haven't been used in some time
type clientForward struct {
	ResidentForward
	lastActivityTime time.Time
}


type clientTransport struct {
	sendTransport *clientSendTransport
	receiveTransport *clientReceiveTransport
}

func (self *clientTransport) Close() {
	self.sendTransport.Close()
	self.receiveTransport.Close()
}


// conforms to `connect.Transport`
type clientSendTransport struct {
	clientId bringyour.Id
	send chan []byte
}

func newClientSendTransport(clientId bringyour.Id, send chan []byte) *clientSendTransport {
	return &clientSendTransport{
		clientId: clientId,
		send: send,
	}
}

func (self *clientSendTransport) Priority() int {
	return 100
}

func (self *clientSendTransport) CanEvalRouteWeight(stats *connect.RouteStats, remainingStats map[connect.Transport]*connect.RouteStats) bool {
	return true
}

func (self *clientSendTransport) RouteWeight(stats *connect.RouteStats, remainingStats map[connect.Transport]*connect.RouteStats) float32 {
	// uniform weight
	return 1.0 / float32(1 + len(remainingStats))
}

func (self *clientSendTransport) MatchesSend(destinationId connect.Id) bool {
	// send to client id only
	return bringyour.Id(destinationId) == self.clientId
}

func (self *clientSendTransport) MatchesReceive(destinationId connect.Id) bool {
	return false
}

func (self *clientSendTransport) Downgrade(sourceId connect.Id) {
	// nothing to downgrade
}

func (self *clientSendTransport) Close() {
	close(self.send)
}


// conforms to `connect.Transport`
type clientReceiveTransport struct {
	clientId bringyour.Id
	receive chan []byte
}

func newClientReceiveTransport(clientId bringyour.Id, receive chan []byte) *clientReceiveTransport {
	return &clientReceiveTransport{
		clientId: clientId,
		receive: receive,
	}
}

func (self *clientReceiveTransport) Priority() int {
	return 100
}

func (self *clientReceiveTransport) CanEvalRouteWeight(stats *connect.RouteStats, remainingStats map[connect.Transport]*connect.RouteStats) bool {
	return true
}

func (self *clientReceiveTransport) RouteWeight(stats *connect.RouteStats, remainingStats map[connect.Transport]*connect.RouteStats) float32 {
	// uniform weight
	return 1.0 / float32(1 + len(remainingStats))
}

func (self *clientReceiveTransport) MatchesSend(destinationId connect.Id) bool {
	return false
}

func (self *clientReceiveTransport) MatchesReceive(destinationId connect.Id) bool {
	return true
}

func (self *clientReceiveTransport) Downgrade(sourceId connect.Id) {
	// nothing to downgrade
}

func (self *clientReceiveTransport) Close() {
	close(self.receive)
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
		lastCheckTime: time.UnixMilli(0),
	}
}

// a simple delay since the last call
func (self *limiter) delay() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	now := time.Now()
	timeout := now.Sub(self.lastCheckTime)
	self.lastCheckTime = now
	if self.minTimeout <= timeout {
		select {
		case <- self.ctx.Done():
		case <- time.After(timeout):
		}
	}
}