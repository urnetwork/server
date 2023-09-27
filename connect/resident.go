package main

import (
	"math/rand"
	"encoding/binary"
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

const StartInternalPort = 7300
const MaxConcurrentForwardsPerResident = 32

const ResidentIdleTimeout = 5 * time.Minute
const ForwardReconnectTimeout = 30 * time.Second
const ResidentSyncTimeout =  = 30 * time.Second
const ForwardIdleTimeout = 1 * time.Minute
const ContractSyncTimeout = 30 * time.Second
const AbuseMinTimeout = 5 * time.Second
const ControlMinTimeout = 200 * time.Millisecond


// each call overwrites the internal buffer
type ExchangeBuffer struct {
	buffer []byte
}

func NewDefaultExchangeBuffer() &ExchangeBuffer {
	return &ExchangeBuffer{
		buffer: make([]byte, MaximumMessageSizeBytes + 4),
	} 
}

func NewReceiveOnlyExchangeBuffer() &ExchangeBuffer {
	return &ExchangeBuffer{
		buffer: make([]byte, 33),
	} 
}

func (self *ExchangeBuffer) WriteHeader(conn net.Conn, header *ExchangeHeader) error {
	buffer[0:16] = header.ClientId
	buffer[16:32] = header.ResidentId
	buffer[32] = header.Op

	_, err := conn.Write(buffer[0:33])
	return err
}

func (self *ExchangeBuffer) ReadHeader(conn net.Conn) (*ExchangeHeader, error) {
	if err := io.ReadFull(conn, buffer[0:33]); err != nil {
		return nil, err
	}

	return &ExchangeHeader{
		ClientId: Id(buffer[0:16]),
		ResidentId: Id(buffer[16:32]),
		Op: ExchangeOp(buffer[32]),
	}
}

func (self *ExchangeBuffer) WriteMessage(conn net.Conn, transferFrameBytes []byte) error {
	n := len(transferFrameBytes)

	if MaximumMessageSizeBytes < n {
		return errors.New(fmt.Sprintf("Maximum message size is %d (%d).", MaximumMessageSizeBytes, n))
	}

	binary.LittleEndian.PutUint32(buffer[0:4], uint32(n))
	buffer[4:4+n] = transferFrameBytes

	_, err := conn.Write(buffer[0:4+n])
	return err
}

func (self *ExchangeBuffer) ReadMessage(conn net.Conn) ([]byte, error) {
	if err := conn.ReadFull(buffer[0:4]); err != nil {
		return nil, err
	}

	n = int(binary.LittleEndian.Uint32(buffer[0:4]))
	if MaximumMessageSizeBytes < n {
		return nil, errors.New(fmt.Sprintf("Maximum message size is %d (%d).", MaximumMessageSizeBytes, n))
	}

	// read into a new buffer
	message := make([]byte, n)

	if err := conn.ReadFull(message); err != nil {
		return nil, err
	}

	return message, nil
}


func safeSend[T any](ctx context.Context, channel chan T, message T) (err error) {
	defer func() {
		err = recover()
	}()
	select {
	case self.send <- message:
	case ctx.Done():
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
	ClientId Id
	ResidentId Id
	Op ExchangeOp
}


type ExchangeConnection struct {
	ctx context.Context
	cancel CancelFunc
	conn net.Conn
	sendBuffer &ExchangeBuffer
	receiveBuffer &ExchangeBuffer
	send chan []byte
	receive chan []byte
}

func NewExchangeConnection(
	ctx context.Context,
	clientId Id,
	residentId Id,
	host string,
	port int,
	op ExchangeOp,
) (*ExchangeConnection, error) {
	conn, err := CONNECT()
	if err != nil {
		return err
	}

	sendBuffer := NewDefaultExchangeBuffer()

	// write header
	err := sendBuffer.WriteHeader(conn, &ExchangeHeader{
		Op: op,
		clientId: clientId,
		residentId: residentId,
	})
	if err != nil {
		return err
	}

	// the connection echoes back the header if connected to the resident
	// else the connection is closed
	_, err := receiveBuffer.ReadHeader(conn)
	if err != nil {
		return err
	}

	connection := &ExchangeConnection{
		cancelCtx,
		cancel,
		op,
		conn,
		sendBuffer,
		receiveBuffer,
		send,
		receive,
	}
	go connection.Run()
}

func (self *ExchangeConnection) Run() {
	close = func() {
		cancel()
		close(send)
		close(receive)
		conn.Close()
	}
	defer close()

	// only a transport connection will receive messages
	switch self.op {
	case ExchangeOpTransport:
		go func() {
			for {
				select {
				case <- ctx.Done():
					close()
					return
				default:
				}
				message, err := receiveBuffer.ReadMessage(conn)
				if err != nil {
					close()
					return
				}
				select {
				case <- ctx.Done():
					close()
					return
				case receive <- message:
				}
			}
		}()
	default:
		close(receive)
	}

	for {
		select {
		case <- ctx.Done():
			return
		case message, ok := <- self.send:
			if !ok {
				return
			}
			if err := sendBuffer.WriteMessage(conn, message); err != nil {
				return
			}
		}
	}
}

func (self *ExchangeConnection) Close() {
	close(send)
}


type ResidentTransport struct {	
	ctx context.Context
	cancel CancelFunc

	exchange Exchange

	clientId Id
	instanceId Id

	send chan []byte
	receive chan []byte
}

func NewResidentTransport(
	ctx context.Context,
	exchange *Exchange,
	clientId Id,
	instanceId Id,
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
	close := func() {
		cancel()
		close(send)
		close(receive)
	}
	defer close()

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
						close()
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
					close()
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
		resident := model.GetResidentWithInstance(clientId, instanceId)
		if resident != nil && 0 < len(resident.ResidentInternalPorts) {
			port := resident.ResidentInternalPorts[rand.Intn(len(resident.ResidentInternalPorts))]
			if exchangeConnection, err := NewExchangeConnection(resident.host, port, ExchangeOpTransport); err == nil {
				handle(exchangeConnection)
			}
		}
		select {
		case <- self.ctx.Done():
			return
		default:
		}
		self.exchange.NominateLocalResident(clientId, instanceId)
	}
}

func (self *ResidentTransport) Close() {
	close(send)
}


type ResidentForward struct {	
	ctx context.Context
	cancel CancelFunc

	exchange Exchange

	clientId Id
	instanceId Id

	send chan []byte
}

func NewResidentForward(
	ctx context.Context,
	exchange *Exchange,
	clientId Id,
) *ResidentForward {
	cancelCtx, cancel := context.WithCancel(ctx)
	transport := &ResidentTransport{
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
	close := func() {
		cancel()
		close(send)
	}
	defer close()

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
					close()
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
		resident := model.GetResident(clientId)
		if resident != nil && 0 < len(resident.ResidentInternalPorts) {
			port := resident.ResidentInternalPorts[rand.Intn(len(resident.ResidentInternalPorts))]
			if exchangeConnection, err := NewExchangeConnection(resident.host, port, ExchangeOpForward); err == nil {
				handle(exchangeConnection)
			}
		}
		select {
		case <- self.ctx.Done():
			return
		case time.After(ForwardReconnectTimeout):
		}
	}
}

func (self *ResidentForward) Close() {
	close(send)
}


// residents live in the exchange
// a resident for a client id can be nominated to live in the exchange with `NominateLocalResident`
// any time a resident is not reachable by a transport, the transport should nominate a local resident
type Exchange struct {
	ctx context.Context
	cancel CancelFunc

	host string
	// any of the ports may be used
	// a range of ports are used to scale one socket per transport or forward,
	// since each port allows at most 65k connections from another connect instance
	ports []int

	residentsLock RWMutex
	// clientId -> Resident
	residents map[Id]*Resident
}

func (self *Exchange) NewExchange(ctx context.Context, host string, ports []int) {
	cancelCtx, cancel := context.WithCancel(ctx)

	exchange := &Exchange{
		ctx: cancelCtx,
		cancel: cancel,
		host: host,
		ports: ports,
		residents: map[Id]*Resident{}
	}

	go exchange.syncResidents()

	return exchange
}

// reads the host and port configuration from the env
func (self *Exchange) NewExchangeFromEnv(ctx context.Context) {
	host := env.Host()

	// service port -> host port
	hostPorts := env.HostPorts()
	// internal ports start at `StartInternalPort` and proceed consecutively
	// each port can handle 65k connections
	// the number of connections depends on the number of expected concurrent destinations
	// the expected port usage is `number_of_residents * expected(number_of_destinations_per_resident)`,
	// and at most `number_of_residents * MaxConcurrentForwardsPerResident`

}

func (self *Exchange) NominateLocalResident(instanceId Id, residentIdToReplace *Id) error {
	resident := NewResident()
	success := false
	defer func() {
		if !success {
			resident.Close()
		}
	}

	func() {
		residentLock.WLock()
		defer residentLock.WUnlock()
		if currentResident, ok := self.residents[clientId]; 
				!ok || ok && residentIdToReplace != nil && currentResident.ResidentId == *residentIdToReplace {
			self.residents[clientId] = resident
		}
	}()

	nominated := model.Nominate(residentIdToReplace, &NetworkClientResident{
		ClientId: clientId,
		InstanceId: instanceId,
		ResidentId: residentId,
		ResidentHost: self.host,
		ResidentService: bringyour.RequireService(),
		ResidentBlock: bringyour.RequireBlock(),
		ResidentInternalPort: self.port,
	})
	if nominated.ResidentId == resident.ResidentId {
		success = true
	}

	if !success {
		return errors.New("Another resident was nominated.")
	}

	return nil
}

// continually cleans up the local resident state, connections, and model based on the latest nominations
func (self *Exchange) syncResidents(ctx context.Context) {
	// watch for this resident to change
	// FIMXE close all connection IDs for this resident on change
	lastRunTime = time.Now()
	for {
		timeout := lastRunTime + ResidentSyncTimeout - time.Now()
		if 0 < timeout {
			select {
			case self.ctx.Done():
				return
			case time.After(ResidentSyncTimeout):
			}
		} else {
			select {
			case self.ctx.Done():
				return
			default:
			}
		}

		lastRunTime = time.Now()


		// check for differences between local residents and the model

		residentsToClose := []*Resident{}

		residentsForHostPort := model.GetResidentsForHostPort(self.host, self.port)
		residentIdsForHostPort := map[Id]bool{}
		for _, residentsForHostPort := range residentsForHostPort {
			residentIdsForHostPort[residentsForHostPort.ResidentId] = true
		}

		residentsForHostPortToRemove := []*NetworkClientResident{}
		
		residentLock.WLock()
		residentIds := map[Id]bool{}
		for _, resident := range self.residents {
			residentIds[resident.ResidentId] = true
		}
		for _, residentsForHostPort := range residentsForHostPort {
			if _, ok := residentIds[residentForHostPort.ResidentId]; !ok {
				residentsForHostPortToRemove = append(residentsForHostPortToRemove, residentForHostPort)
			}
		}
		for _, resident := range self.residents {
			if _, ok := residentIdsForHostPort[resident.ResidentId]; !ok {
				// this resident has been removed from the model
				residentsToClose = append(residentsToClose, resident)
				delete(self.residents, resident.ClientId)
			}
		}
		residentLock.WUnlock()

		for _, residentForHostPort := range residentForHostPortToRemove {
			model.RemoveResident(residentForHostPort.ClientId, residentForHostPort.ResidentId)
		}

		for _, resident := range residentsToClose {
			resident.Close()
		}


		// check for residents with no transports, and have no activity in some time
		residentsToRemove := []*Resident{}

		residentLock.WLock()
		for _, resident := range self.residents {
			if resident.IsIdle() {
				residentsToRemove = append(residentsToRemove, resident)
				delete(self.residents, resident.ClientId)
			}
		}
		residentLock.WUnlock()

		for _, resident := range residentsToRemove {
			model.RemoveResident(resident.ClientId, resident.ResidentId)
			resident.Close()
		}
	}
}

// runs the exchange to expose local nominated residents
// there should be one local exchange per service
func (self *Exchange) Run() {
	defer func() {
		var residentsCopy := map[Id]*Resident{}
		residentLock.WLock()
		maps.Copy(residentsCopy, self.residents)
		clear(self.residents)
		residentLock.WUnLock()
		for _, resident := range residentsCopy {
			resident.Close()
			model.RemoveResident(resident.ClientId, resident.ResidentId)
		}
	}()

	for _, port := range self.ports {
		go func() {
			server, err := net.Listen("tcp", self.port)
			if err != nil {
				return
			}
			defer server.Close()

			for {
				select {
				case self.ctx.Done():
					return
				default:
				}

				socket, err := server.Accept()
				if err != nil {
					return
				}
				go handleExchangeClient(socket)
			}
		}()
	}

	cleanupLocalResidents()
}

func (self *ResidentManager) handleExchangeClient(conn net.Conn) {
	close := func() {
		conn.Close()
	}
	defer close()

	receiveBuffer := NewReceiveOnlyExchangeBuffer()

	header, err := receiveBuffer.ReadHeader(self.ctx, conn)
	if err != nil {
		return
	}

	residentsLock.RLock()
	resident, ok := residents[header.ClientId]
	residentsLock.RUnlock()

	if !ok || resident.ResidentId != header.ResidentId {
		return
	}

	// echo back the header
	if err := receiveBuffer.WriteHeader(ctx, conn, header); err != nil {
		return
	}

	switch header.Op {
	case ExchangeOpTransport:
		send := make(chan []byte)
		receive := make(chan []byte)
		go func() {
			defer func() {
				close(send)
				close()
			}

			sendBuffer := NewDefaultExchangeBuffer()
			for {
				select {
				case message, ok := <- send:
					if !ok {
						return
					}
					if err := sendBuffer.WriteMessage(ctx, conn, message); err != nil {
						return
					}
				case <- ctx.Done():
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
	cancel()
	
	// close all residents
	residentsLock.WLock()
	for _, resident := range self.residents {
		resident.Close()
		model.RemoveResident(resident.ClientId, resident.ResidentId)	
	}
	clear(self.residents)
	residentsLock.WUnlock()
}


type Resident struct {
	ctx context.Context
	cancel CancelFunc

	exchange *Exchange

	clientId Id
	instanceId Id
	residentId Id

	// the client id in the resident is always `connect.ControlId`
	client *connect.Client

	stateLock sync.Mutex

	transports := map[*clientTransport]bool

	// destination id -> forward
	forwards := map[Id]*ClientForward

	lastActivityTime time.Time

	abuseLimiter *limiter
	controlLimiter *limiter
}

func NewResident(
	ctx context.Context,
	exchange *Exchange,
	clientId Id,
	instanceId Id,
	residentId Id
) *Resident {
	cancelCtx, cancel := context.WithCancel(ctx)

	client := connect.NewClientWithDefaults(connect.ControlId, cancelCtx)

	resident := &Resident{
		ctx: cancelCtx,
		cancel: cancel,
		exchange: exchange,
		client: client,
		transports: map[*ClientTransport]bool{},
		forwards: map[*ClientForward]bool{},
		abuseLimiter: newLimiter(cancelCtx, AbuseMinTimeout),
		controlLimiter: newLimiter(cancelCtx, ControlMinTimeout),
	}

	client.AddReceiveFunction(resident.clientReceive)
	client.AddForwardFunction(resident.clientForward)

	go client.Run()
	go resident.cleanupForwards()

	return resident
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
		forwardsToRemove := []*ClientForward{}
		for _, forward := self.forwards {
			if ForwardIdleTimeout <= time.Now() - forward.lastSendTime {
				forwardsToRemove = append(forwardsToRemove, forward)
			}
		}
		for _, forward := range forwardsToRemove {
			forward.Close()
			delete(self.forwards, forward.destinationId)
		}
		self.stateLock.Unlock()
	}
}

// connect.ForwardFunction
func (self *Resident) clientForward(sourceId_ connect.Id, destinationId_ connect.Id, transferFrameBytes []byte) {
	self.updateActivity()

	sourceId := Id(sourceId_)
	destinationId := Id(destinationId_)

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
		forward.lastSendTime = time.Now()
	} else if len(self.forwards) < MaxConcurrentForwardsPerResident {
		forward = &ClientForward{
			forward: NewResidentForward(self.ctx, self.exchange),
			lastSendTime: time.Now(),
		}
		self.forwards[destinationId] = forward
	}
	self.stateLock.Unlock()

	if forward != nil {
		select {
		case <- ctx.Done():
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
		case protocol.MessageType_PROVIDE:
			// FIXME
		case protocol.MessageType_CREATE_CONTRACT:
			// FIXME
		case protocol.MessageType_CLOSE_CONTRACT:
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
	transports[transport] = true
	self.client.routeManager.updateTransport(transport.sendTransport, send)
	self.client.routeManager.updateTransport(transport.receiveTransport, receive)
	self.stateLock.Unlock()


	return func() {
		self.stateLock.Lock()
		self.client.routeManager.removeTransport(transport.sendTransport)
		self.client.routeManager.removeTransport(transport.receiveTransport)
		delete(transports, transport)
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

	if 0 < len(transport) {
		return false
	}

	idleTimeout := ResidentIdleTimeout - (time.Now() - self.lastActivityTime)
	return idleTimeout <= 0
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

	clientId Id

	// unordered transfer pair -> contract ids
	pairContractIds map[model.TransferPair]map[Id]bool
}

func newContractManager(ctx context.Context, clientId Id) *contractManager {
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
		case time.After(ContractSyncTimeout):
		}

		pairContractIds_ := model.GetOpenContractIdsForSourceOrDestination(clientId)
		self.stateLock.Lock()
		self.pairContractIds = pairContractIds_
		self.stateLock.Unlock()
	}
}

func (self *contractManager) HasActiveContract(sourceId Id, destinationId Id) bool {
	transferPair := model.NewUnorderedTransferPair(sourceId, destinationId)

	self.stateLock.Lock()
	contracts, ok := self.pairContractIds[transferPair]
	self.stateLock.Unlock()

	if !ok {
		contractIds = model.GetOpenContractIds(sourceId, destinationId)
		contracts := map[Id]bool{}
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

func (self *contractManager) CreateContract(sourceId Id, destinationId Id, transferBytes int) *TransferEscrow {
	contractTransferBytes := max(MinContractTransferBytes, transferBytes)
	escrow := model.TransferEscrow(sourceId, destinationId, contractTransferBytes)

	// update the cache
	transferPair := model.NewUnorderedTransferPair(sourceId, destinationId)
	self.stateLock.Lock()
	contracts, ok := self.pairContractIds[transferPair]
	if !ok {
		contracts := map[Id]bool{}
		self.pairContractIds[transferPair] = contracts
	}
	contracts[escrow.ContractId] = true
	self.stateLock.Unlock()

	return escrow
}


// each send on the forward updates the send time
// the cleanup removes forwards that haven't been used in some time
type clientForward struct {
	destinationId Id
	forward *ResidentForward
	lastUpdateTime time.Time
}

func NewClientForward(destinationId Id, forward *ResidentForward) *clientForward {
	return &clientForward{
		destinationId: destinationId,
		forward: forward,
		lastUpdateTime: time.Now(),
	}
}

func (self *clientForward) Close() {
	forward.Close()
}


type clientTransport struct {
	sendTransport *clientSendTransport
	receiveTransport *clientReceiveTransport
}


// conforms to connect.Transport
type clientSendTransport struct {
	clientId Id
	send chan []byte
}

func newClientSendTransport(clientId Id, send chan []byte) *clientSendTransport {
	return &clientSendTransport{
		clientId: clientId,
		send: send,
	}
}

func (self *clientSendTransport) Priority() int {
	return 100
}

func (self *clientSendTransport) CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool {
	return true
}

func (self *clientSendTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	// uniform weight
	return 1.0 / float32(1 + len(remainingStats))
}

func (self *clientSendTransport) MatchesSend(destinationId Id) bool {
	// send to client id only
	return destinationId == self.clientId
}

func (self *clientSendTransport) MatchesReceive(destinationId Id) bool {
	return false
}

func (self *clientSendTransport) Downgrade(sourceId Id) {
	// nothing to downgrade
}

func (self *clientSendTransport) Close() {
	close(send)
}


// conforms to connect.Transport
type clientReceiveTransport struct {
	clientId Id
	receive chan []byte
}

func newClientReceiveTransport(clientId Id, receive chan []byte) *clientReceiveTransport {
	return &clientReceiveTransport{
		clientId: clientId,
		receive: receive,
	}
}

func (self *clientReceiveTransport) Priority() int {
	return 100
}

func (self *clientReceiveTransport) CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool {
	return true
}

func (self *clientReceiveTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	// uniform weight
	return 1.0 / float32(1 + len(remainingStats))
}

func (self *clientReceiveTransport) MatchesSend(destinationId Id) bool {
	return false
}

func (self *clientReceiveTransport) MatchesReceive(destinationId Id) bool {
	return true
}

func (self *clientReceiveTransport) Downgrade(sourceId Id) {
	// nothing to downgrade
}

func (self *clientReceiveTransport) Close() {
	close(receive)
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
		lastCheckTime: 0,
	}
}

// a simple delay since the last call
func (self *limiter) delay() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	now := time.Now
	timeout := (now - self.lastCheckTime) - self.minTimeout
	self.lastCheckTime = now
	if 0 < timeout {
		select {
		case <- self.ctx.Done():
		case time.After(timeout):
		}
	}
}