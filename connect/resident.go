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
	"crypto/hmac"
	"crypto/sha256"

	// "golang.org/x/exp/maps"

	"google.golang.org/protobuf/proto"

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
// const ForwardReconnectTimeout = 30 * time.Second
const ResidentSyncTimeout = 30 * time.Second
const ForwardIdleTimeout = 1 * time.Minute
const ContractSyncTimeout = 30 * time.Second
const AbuseMinTimeout = 5 * time.Second
const ControlMinTimeout = 200 * time.Millisecond

const ExchangeConnectTimeout = 30 * time.Second

// const NominateLocalResidentTimeout = 1 * time.Second

const ClientDrainTimeout = 30 * time.Second
const TransportDrainTimeout = 30 * time.Second
const ForwardTimeout = 200 * time.Millisecond

const ExchangePingTimeout = 15 * time.Second
const ExchangeReadWriteTimeout = 30 * time.Second


var ControlId = bringyour.Id(connect.ControlId)


// FIXME heartbeat timeout
// FIXME read/write timeout is 2*heartbeat


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
	conn.SetWriteDeadline(time.Now().Add(ExchangeReadWriteTimeout))

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
	conn.SetReadDeadline(time.Now().Add(ExchangeReadWriteTimeout))

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
	conn.SetWriteDeadline(time.Now().Add(ExchangeReadWriteTimeout))

	n := len(transferFrameBytes)

	if MaximumMessageSizeBytes < n {
		return errors.New(fmt.Sprintf("Maximum message size is %d (%d).", MaximumMessageSizeBytes, n))
	}

	binary.LittleEndian.PutUint32(self.buffer[0:4], uint32(n))
	copy(self.buffer[4:4+n], transferFrameBytes)

	// conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
	_, err := conn.Write(self.buffer[0:4+n])
	// if 4+n != n_ {
	// 	panic(fmt.Errorf("Bad write %s", err))
	// }
	return err
}

func (self *ExchangeBuffer) ReadMessage(ctx context.Context, conn net.Conn) ([]byte, error) {
	conn.SetReadDeadline(time.Now().Add(ExchangeReadWriteTimeout))

	// conn.SetReadDeadline(time.Now().Add(30 * time.Second))
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


/*
// FIXME if cancel is called before closing, this is not needed
func safeSend[T any](ctx context.Context, channel chan T, message T) (err error) {
	defer func() {
		if err_ := recover(); err != nil {
			err = err_.(error)
		}
	}()
	select {
	case <- ctx.Done():
		return errors.New("Done.")
	case channel <- message:
		return nil
	}
}
*/


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
		bringyour.Logger().Printf("EXCHANGE CONNECTION ERROR CONNECT %s\n", err)
		return nil, err
	}

	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()

	sendBuffer := NewDefaultExchangeBuffer()

	// write header
	err = sendBuffer.WriteHeader(ctx, conn, &ExchangeHeader{
		Op: op,
		ClientId: clientId,
		ResidentId: residentId,
	})
	if err != nil {
		bringyour.Logger().Printf("EXCHANGE CONNECTION ERROR WRITE HEADER %s\n", err)
		return nil, err
	}

	// the connection echoes back the header if connected to the resident
	// else the connection is closed
	_, err = sendBuffer.ReadHeader(ctx, conn)
	if err != nil {
		bringyour.Logger().Printf("EXCHANGE CONNECTION ERROR READ HEADER %s\n", err)
		return nil, err
	}

	// tcpConn := conn.(*net.TCPConn)
	// tcpConn.SetKeepAlive(true)
	// tcpConn.SetKeepAlivePeriod(1 * time.Second)

	success = true

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
	go bringyour.HandleError(connection.Run, cancel)

	return connection, nil
}

func (self *ExchangeConnection) Run() {
	defer func() {
		self.cancel()
		close(self.receive)
		self.conn.Close()
	}()

	// only a transport connection will receive messages
	switch self.op {
	case ExchangeOpTransport:
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
				select {
				case <- self.ctx.Done():
					return
				case self.receive <- message:
				}
			}
		}, self.cancel)
	default:
		// do nothing for receive
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
				// FIXME set write timeout
				if err := self.sendBuffer.WriteMessage(self.ctx, self.conn, message); err != nil {
					return
				}
			case <- time.After(ExchangePingTimeout):
				// send a ping
				if err := self.sendBuffer.WriteMessage(self.ctx, self.conn, make([]byte, 0)); err != nil {
					return
				}
			}
		}
	}, self.cancel)

	select {
	case <- self.ctx.Done():
		return
	}
}

func (self *ExchangeConnection) Close() {
	self.cancel()

	close(self.send)
}

func (self *ExchangeConnection) Cancel() {
	self.cancel()
}

func (self *ExchangeConnection) Done() <-chan struct{} {
	return self.ctx.Done()
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
	go bringyour.HandleError(transport.Run, cancel)
	return transport
}

func (self *ResidentTransport) Run() {
	defer func() {
		self.cancel()
		close(self.receive)
	}()


	// FIXME take a handle ctx
	handle := func(connection *ExchangeConnection) {
		defer connection.Close()

		go bringyour.HandleError(func() {
			// read
			for {
				select {
				case <- self.ctx.Done():
					bringyour.Logger().Printf("READ 3\n")
					return
				case <- connection.Done():
					return
				case message, ok := <- connection.receive:
					if !ok {
						bringyour.Logger().Printf("READ 1\n")
						// need a new connection
						return
					}
					select {
					case <- self.ctx.Done():
						return
					case <- connection.Done():
						return
					case self.receive <- message:
					}
				}
			}
		}, self.cancel)

		// write
		for {
			select {
			case <- self.ctx.Done():
				bringyour.Logger().Printf("WRITE 3\n")
				return
			case <- connection.Done():
				return
			case message, ok := <- self.send:
				if !ok {
					bringyour.Logger().Printf("WRITE 1\n")
					// transport closed
					self.cancel()
					return
				}
				select {
				case <- self.ctx.Done():
					return
				case <- connection.Done():
					return
				case connection.send <- message:
				}
			}
		}
	}

	for {
		resident := model.GetResidentWithInstance(self.ctx, self.clientId, self.instanceId)
		bringyour.Logger().Printf("EXCHANGE FOUND RESIDENT %s\n", resident)
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
				// FIXME poll the resident and cancel the handle ctx if different

				bringyour.Logger().Printf("EXCHANGE CONNECTION ENTER\n")
				handle(exchangeConnection)
				bringyour.Logger().Printf("EXCHANGE CONNECTION EXIT\n")
			} else {
				bringyour.Logger().Printf("EXCHANGE CONNECTION ERROR: %s\n", err)
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
		err := self.exchange.NominateLocalResident(self.clientId, self.instanceId, residentIdToReplace)
		// FIXME for testing
		if err != nil {
			panic(err)
		}
	}
}

func (self *ResidentTransport) Close() {
	self.cancel()

	close(self.send)
}

func (self *ResidentTransport) Cancel() {
	self.cancel()
}

func (self *ResidentTransport) Done() <-chan struct{} {
	return self.ctx.Done()
}


type ResidentForward struct {	
	ctx context.Context
	cancel context.CancelFunc

	exchange *Exchange

	clientId bringyour.Id

	send chan []byte

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
		send: make(chan []byte),
		lastActivityTime: time.Now(),
	}
	go bringyour.HandleError(transport.Run, cancel)
	return transport
}

func (self *ResidentForward) Run() {
	defer self.cancel()

	handle := func(connection *ExchangeConnection) {
		// defer handleCancel()
		defer connection.Close()

		// write
		for {
			select {
			case <- self.ctx.Done():
				return
			case message, ok := <- self.send:
				if !ok {
					// transport closed
					return
				}
				select {
				case <- self.ctx.Done():
					return
				case <- connection.Done():
					return
				case connection.send <- message:
				}
			}
		}
	}

	for {
		bringyour.Logger().Printf("FORWARD GET A NEW CONNECTION -> %s\n", self.clientId)
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
			)
			if err == nil {
				// handleCtx, handleCancel := context.WithCancel(self.ctx)
				handle(exchangeConnection)
				retryDelay = false
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
			case <- time.After(ForwardTimeout):
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

func (self *ResidentForward) Close() {
	self.cancel()

	close(self.send)
}

func (self *ResidentForward) Cancel() {
	self.cancel()
}

func (self *ResidentForward) Done() <-chan struct{} {
	return self.ctx.Done()
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

	go bringyour.HandleError(exchange.Run, cancel)

	return exchange
}

// reads the host and port configuration from the env
func NewExchangeFromEnv(ctx context.Context) *Exchange {
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

	bringyour.Logger().Printf("FOUND EXCHANGE PORTS %s\n", ports)

	return NewExchange(ctx, host, ports)
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
			model.RemoveResident(self.ctx, resident.clientId, resident.residentId)
			self.residentsLock.Lock()
			if currentResident, ok := self.residents[clientId]; ok && currentResident == resident {
				bringyour.Logger().Printf("DELETE LOCAL RESIDENT %s\n", clientId.String())
				delete(self.residents, clientId)
			}
			self.residentsLock.Unlock()
		}
	}()

	var replacedResident *Resident

	// make sure the new resident is local before nominating
	// this will prevent failed connections from other exchanges if the nomination succeeds
	self.residentsLock.Lock()
	replacedResident = self.residents[clientId]
	bringyour.Logger().Printf("SET LOCAL RESIDENT %s\n", clientId.String())
	self.residents[clientId] = resident
	self.residentsLock.Unlock()

	if replacedResident != nil {
		replacedResident.Close()
		model.RemoveResident(self.ctx, replacedResident.clientId, replacedResident.residentId)
	}

	nominated := model.NominateResident(self.ctx, residentIdToReplace, &model.NetworkClientResident{
		ClientId: clientId,
		InstanceId: instanceId,
		ResidentId: residentId,
		ResidentHost: self.host,
		ResidentService: bringyour.RequireService(),
		ResidentBlock: bringyour.RequireBlock(),
		ResidentInternalPorts: self.ports,
	})
	bringyour.Logger().Printf("NOMINATED RESIDENT %s <> %s\n", nominated.ResidentId.String(), resident.residentId.String())
	if nominated.ResidentId == resident.residentId {
		// defer func() {
		// 	r := model.GetResidentWithInstance(self.ctx, clientId, instanceId)
		// 	bringyour.Logger().Printf("NOMINATED RESIDENT VERIFIED %s <> %s\n", nominated.ResidentId.String(), r.ResidentId.String())
		// }()
		success = true
		return nil
	}

	return errors.New("Another resident was nominated.")
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

		for _, resident := range residentsToClose {
			resident.Close()
		}

		for _, residentForHostPort := range residentsForHostPortToRemove {
			model.RemoveResident(self.ctx, residentForHostPort.ClientId, residentForHostPort.ResidentId)
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
			resident.Close()
			model.RemoveResident(self.ctx, resident.clientId, resident.residentId)
		}
	}
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

	for _, port_ := range self.ports {
		port := port_
		go bringyour.HandleError(func() {
			defer self.cancel()

			bringyour.Logger().Printf("EXCHANGE LISTEN ON PORT %d", port)
			// leave host part empty to listen on all available interfaces
			server, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
			if err != nil {
				bringyour.Logger().Printf("EXCHANGE LISTEN ON PORT %d ERROR %s", port, err)
				return
			}
			defer server.Close()

			go bringyour.HandleError(func() {
					for {
					select {
					case <- self.ctx.Done():
						return
					default:
					}

					conn, err := server.Accept()
					if err != nil {
						bringyour.Logger().Printf("EXCHANGE LISTEN ON PORT %d ACCEPT ERROR %s", port, err)
						return
					}
					go bringyour.HandleError(
						func() {self.handleExchangeConnection(conn)},
						self.Close,
					)
				}
			}, self.cancel)

			select {
			case <- self.ctx.Done():
			}
		}, self.cancel)
	}

	self.syncResidents()
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

	bringyour.Logger().Printf("EXCHANGE HANDLE CLIENT\n")

	receiveBuffer := NewReceiveOnlyExchangeBuffer()

	header, err := receiveBuffer.ReadHeader(handleCtx, conn)
	if err != nil {
		bringyour.Logger().Printf("EXCHANGE HANDLE CLIENT READ HEADER ERROR %s\n", err)
		return
	}

	self.residentsLock.RLock()
	resident, ok := self.residents[header.ClientId]
	bringyour.Logger().Printf("EXCHANGE ALL RESIDENTS %s\n", self.residents)
	self.residentsLock.RUnlock()

	if !ok || resident.residentId != header.ResidentId {
		bringyour.Logger().Printf("EXCHANGE HANDLE CLIENT MISSING RESIDENT %s %s (%s, %s) \n", header.ClientId.String(), header.ResidentId.String(), ok, resident)
		return
	}

	// echo back the header
	if err := receiveBuffer.WriteHeader(handleCtx, conn, header); err != nil {
		bringyour.Logger().Printf("EXCHANGE HANDLE CLIENT WRITE HEADER ERROR %s\n", err)
		return
	}

	switch header.Op {
	case ExchangeOpTransport:
		send, receive, closeTransport := resident.AddTransport()
		defer closeTransport()

		go bringyour.HandleError(func() {
			defer handleCancel()

			sendBuffer := NewDefaultExchangeBuffer()
			for {
				select {
				case <- handleCtx.Done():
					return
				case <- resident.Done():
					return
				case message, ok := <- send:
					bringyour.Logger().Printf("RESIDENT SEND %s %s\n", message, ok)
					if !ok {
						return
					}
					// FIXME SET WRITE TIMEOUT
					if err := sendBuffer.WriteMessage(handleCtx, conn, message); err != nil {
						bringyour.Logger().Printf("RESIDENT SEND ERROR %s\n", err)
						return
					}
				case <- time.After(ExchangePingTimeout):
					// send a ping
					if err := sendBuffer.WriteMessage(handleCtx, conn, make([]byte, 0)); err != nil {
						return
					}
				}
			}
		}, handleCancel)
		
		go bringyour.HandleError(func() {
			defer handleCancel()

			// read
			// messages from the transport are to be received by the resident
			// messages not destined for the control id are handled by the resident forward
			for {
				// FIXME SET READ IDLE TIMEOUT
				message, err := receiveBuffer.ReadMessage(handleCtx, conn)
				bringyour.Logger().Printf("RESIDENT RECEIVE %s %s\n", message, err)
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
				case receive <- message:
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

			sendBuffer := NewDefaultExchangeBuffer()
			for {
				select {
				case <- handleCtx.Done():
					return
				case <- resident.Done():
					return
				case <- time.After(ExchangePingTimeout):
					// send a ping
					if err := sendBuffer.WriteMessage(handleCtx, conn, make([]byte, 0)); err != nil {
						return
					}
				}
			}
		}, handleCancel)

		go bringyour.HandleError(func() {
			defer handleCancel()

			bringyour.Logger().Printf("RESIDENT FORWARD %s\n", header.ClientId.String())
			for {
				// FIXME SET READ IDLE TIMEOUT
				message, err := receiveBuffer.ReadMessage(handleCtx, conn)
				bringyour.Logger().Printf("RESIDENT FORWARD %s %s\n", message, err)
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
				if !resident.Forward(message) {
					bringyour.Logger().Printf("RESIDENT FORWARD FALSE %s\n", message)
					return
				}
			}
		}, handleCancel)
	}

	select {
	case <- handleCtx.Done():
		bringyour.Logger().Printf("!!!! HANDLE DONE\n")
	case <- resident.Done():
		bringyour.Logger().Printf("!!!! RESIDENT DONE\n")
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
	forwards map[bringyour.Id]*ResidentForward

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

	client := connect.NewClientWithDefaults(cancelCtx, connect.ControlId)

	clientRouteManager := connect.NewRouteManager(client)
	clientContractManager := connect.NewContractManagerWithDefaults(client)
	// no contract is required between the platform and client
	bringyour.Logger().Printf("NO CONTRACT PEER %s\n", clientId.String())
	clientContractManager.AddNoContractPeer(connect.Id(clientId))

	resident := &Resident{
		ctx: cancelCtx,
		cancel: cancel,
		exchange: exchange,
		clientId: clientId,
		instanceId: instanceId,
		residentId: residentId,
		client: client,
		clientRouteManager: clientRouteManager,
		clientContractManager: clientContractManager,
		contractManager: newContractManager(cancelCtx, cancel, clientId),
		transports: map[*clientTransport]bool{},
		forwards: map[bringyour.Id]*ResidentForward{},
		abuseLimiter: newLimiter(cancelCtx, AbuseMinTimeout),
		controlLimiter: newLimiter(cancelCtx, ControlMinTimeout),
	}

	client.AddReceiveCallback(resident.handleClientReceive)
	client.AddForwardCallback(resident.handleClientForward)

	go bringyour.HandleError(
		func() {client.Run(clientRouteManager, clientContractManager)},
		cancel,
	)
	go bringyour.HandleError(resident.cleanupForwards, cancel)

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
		forwardsToRemove := []*ResidentForward{}
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

// `connect.ForwardFunction`
func (self *Resident) handleClientForward(sourceId_ connect.Id, destinationId_ connect.Id, transferFrameBytes []byte) {
	sourceId := bringyour.Id(sourceId_)
	destinationId := bringyour.Id(destinationId_)

	bringyour.Logger().Printf("HANDLE CLIENT FORWARD %s %s %s %s\n", self.clientId.String(), sourceId.String(), destinationId.String(), transferFrameBytes)

	self.updateActivity()

	if sourceId != self.clientId {
		bringyour.Logger().Printf("HANDLE CLIENT FORWARD BAD SOURCE\n")

		// the message is not from the client
		// clients are not allowed to forward from other clients
		// drop
		self.abuseLimiter.delay()
		return
	}

	if !self.contractManager.HasActiveContract(sourceId, destinationId) {
		bringyour.Logger().Printf("HANDLE CLIENT FORWARD NO CONTRACT\n")

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
		forward := NewResidentForward(self.ctx, self.exchange, destinationId)
		self.forwards[destinationId] = forward
	}


	// TEST this solves the issue
	// forward = NewResidentForward(self.ctx, self.exchange, destinationId)
	// self.forwards[destinationId] = forward
	

	self.stateLock.Unlock()

	if forward == nil {
		// drop the message
		return
	}
	
	select {
	case <- self.ctx.Done():
		return
	case <- forward.Done():
		
	case forward.send <- transferFrameBytes:
		return
	case <- time.After(ForwardTimeout):
		// FIXME need to debug this timeout case
		bringyour.Logger().Printf("!!!! RESIDENT FORWARD TIMEOUT")
		
	}

	// recreate the forward and try again
	self.stateLock.Lock()
	forward.Cancel()
	forward = NewResidentForward(self.ctx, self.exchange, destinationId)
	self.forwards[destinationId] = forward
	self.stateLock.Unlock()

	select {
	case <- self.ctx.Done():
		return
	case <- forward.Done():
		return
	case forward.send <- transferFrameBytes:
		return
	case <- time.After(ForwardTimeout):
		// drop the message
		return
	}
}

// `connect.ReceiveFunction`
func (self *Resident) handleClientReceive(sourceId_ connect.Id, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
	sourceId := bringyour.Id(sourceId_)

	bringyour.Logger().Printf("HANDLE CLIENT RECEIVE %s %s %s\n", sourceId.String(), provideMode, frames)

	// these are messages to the control id
	// use `client.Send` to send messages back to the client

	// the provideMode should always be `Network`, by configuration
	// `Network` is the highest privilege in the user network
	if model.ProvideMode(provideMode) != model.ProvideModeNetwork {
		bringyour.Logger().Printf("HANDLE CLIENT RECEIVE NOT IN NETWORK\n")
		return
	}

	self.updateActivity()
	self.controlLimiter.delay()

	for _, frame := range frames {
		bringyour.Logger().Printf("HANDLE CLIENT RECEIVE FRAME %s\n", frame)
		if message, err := connect.FromFrame(frame); err == nil {
		switch v := message.(type) {
			case *protocol.Provide:
				// FIXME sourceId is not clientId
				self.controlProvide(sourceId, v)

			case *protocol.CreateContract:
				self.controlCreateContract(sourceId, v)

			case *protocol.CloseContract:
				self.controlCloseContract(sourceId, v)
			}
		}
	}
}

func (self *Resident) controlProvide(sourceId bringyour.Id, provide *protocol.Provide) {
	secretKeys := map[model.ProvideMode][]byte{}			
	for _, provideKey := range provide.Keys {
		secretKeys[model.ProvideMode(provideKey.Mode)] = provideKey.ProvideSecretKey	
	}
	bringyour.Logger().Printf("SET PROVIDE %s %s\n", sourceId.String(), secretKeys)
	model.SetProvide(self.ctx, sourceId, secretKeys)
	bringyour.Logger().Printf("SET PROVIDE COMPLETE %s %s\n", sourceId.String(), secretKeys)
}

func (self *Resident) controlCreateContract(sourceId bringyour.Id, createContract *protocol.CreateContract) {
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT\n")

	destinationId := bringyour.Id(createContract.DestinationId)

	minRelationship := self.contractManager.GetProvideRelationship(sourceId, destinationId)

	maxProvideMode := self.contractManager.GetProvideMode(destinationId)
	if maxProvideMode < minRelationship {
		bringyour.Logger().Printf("CONTROL CREATE CONTRACT ERROR NO PERMISSION\n")
		contractError := protocol.ContractError_NoPermission
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame, err := connect.ToFrame(result)
		bringyour.Raise(err)
		self.client.Send(frame, connect.Id(sourceId), nil)
		return
	}

	provideSecretKey, err := model.GetProvideSecretKey(self.ctx, destinationId, minRelationship)
	if err != nil {
		bringyour.Logger().Printf("CONTROL CREATE CONTRACT ERROR NO SECRET KEY\n")
		contractError := protocol.ContractError_NoPermission
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame, err := connect.ToFrame(result)
		bringyour.Raise(err)
		self.client.Send(frame, connect.Id(sourceId), nil)
		return
	}

	// if `minRelationship < Public`, use CreateContractNoEscrow
	// else use CreateTransferEscrow
	contractId, contractByteCount, err := self.contractManager.CreateContract(
		sourceId,
		destinationId,
		int(createContract.TransferByteCount),
		minRelationship,
	)
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT TRANSFER BYTE COUNT %d %d %d\n", int(createContract.TransferByteCount), contractByteCount, uint64(contractByteCount))

	if err != nil {
		bringyour.Logger().Printf("CONTROL CREATE CONTRACT ERROR INSUFFICIENT BALANCE\n")
		contractError := protocol.ContractError_InsufficientBalance
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame, err := connect.ToFrame(result)
		bringyour.Raise(err)
		self.client.Send(frame, connect.Id(sourceId), nil)
		return
	}

	storedContract := &protocol.StoredContract{
		ContractId: contractId.Bytes(),
		TransferByteCount: uint64(contractByteCount),
		SourceId: sourceId.Bytes(),
		DestinationId: destinationId.Bytes(),
	}
	storedContractBytes, err := proto.Marshal(storedContract)
	if err != nil {
		bringyour.Logger().Printf("CONTROL CREATE CONTRACT STORED ERROR\n")
		contractError := protocol.ContractError_Setup
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame, err := connect.ToFrame(result)
		bringyour.Raise(err)
		self.client.Send(frame, connect.Id(sourceId), nil)
		return
	}
	mac := hmac.New(sha256.New, provideSecretKey)
	storedContractHmac := mac.Sum(storedContractBytes)

	result := &protocol.CreateContractResult{
		Contract: &protocol.Contract{
			StoredContractBytes: storedContractBytes,
			StoredContractHmac: storedContractHmac,
			ProvideMode: protocol.ProvideMode(minRelationship),
		},
	}
	frame, err := connect.ToFrame(result)
	bringyour.Raise(err)
	self.client.Send(frame, connect.Id(sourceId), nil)
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT SENT\n")
}

func (self *Resident) controlCloseContract(sourceId bringyour.Id, closeContract *protocol.CloseContract) {
	self.contractManager.CloseContract(
		bringyour.RequireIdFromBytes(closeContract.ContractId),
		sourceId,
		int(closeContract.AckedByteCount),
	)
}

func (self *Resident) AddTransport() (
	send chan[] byte,
	receive chan[] byte,
	closeTransport func(),
) {
	send = make(chan []byte)
	receive = make(chan []byte)

	// in `connect` the transport is bidirectional
	// in the resident, each transport is a single direction
	transport := &clientTransport{
		sendTransport: newClientSendTransport(self.clientId),
		receiveTransport: newClientReceiveTransport(),
	}

	bringyour.Logger().Printf("ADD TRANSPORT %s\n", self.clientId.String())

	self.stateLock.Lock()
	self.transports[transport] = true
	self.clientRouteManager.UpdateTransport(transport.sendTransport, []connect.Route{send})
	self.clientRouteManager.UpdateTransport(transport.receiveTransport, []connect.Route{receive})
	self.stateLock.Unlock()

	closeTransport = func() {
		bringyour.Logger().Printf("REMOVE TRANSPORT %s\n", self.clientId.String())

		self.stateLock.Lock()
		self.clientRouteManager.RemoveTransport(transport.sendTransport)
		self.clientRouteManager.RemoveTransport(transport.receiveTransport)
		delete(self.transports, transport)
		self.stateLock.Unlock()

		// transport.Close()

		go func() {
			select {
			case <- time.After(TransportDrainTimeout):
			}

			close(send)
			close(receive)
		}()
	}

	return
}

func (self *Resident) Forward(transferFrameBytes []byte) bool {
	self.updateActivity()
	return self.client.ForwardWithTimeout(transferFrameBytes, ForwardTimeout)
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

	self.client.Cancel()

	self.client.RemoveReceiveCallback(self.handleClientReceive)
	self.client.RemoveForwardCallback(self.handleClientForward)

	self.stateLock.Lock()
	// clear forwards
	for _, forward := range self.forwards {
		forward.Cancel()
	}
	// clear transports
	// for transport, _ := range self.transports {
	// 	self.clientRouteManager.RemoveTransport(transport.sendTransport)
	// 	self.clientRouteManager.RemoveTransport(transport.receiveTransport)
	// 	transport.Close()
	// }
	// clear(self.transports)
	self.stateLock.Unlock()

	go func() {
		select {
		case <- time.After(ClientDrainTimeout):
		}

		self.stateLock.Lock()
		// close forwards
		for _, forward := range self.forwards {
			forward.Close()
		}
		clear(self.forwards)
		self.stateLock.Unlock()

		self.client.Close()
	}()
}

// func (self *Resident) Cancel() {
// 	self.cancel()
// 	self.client.Cancel()
// }

func (self *Resident) Done() <-chan struct{} {
	return self.ctx.Done()
}


type contractManager struct {
	ctx context.Context
	cancel context.CancelFunc

	clientId bringyour.Id

	stateLock sync.Mutex
	// unordered transfer pair -> contract ids
	pairContractIds map[model.TransferPair]map[bringyour.Id]bool
}

func newContractManager(
	ctx context.Context,
	cancel context.CancelFunc,
	clientId bringyour.Id,
) *contractManager {
	contractManager := &contractManager {
		ctx: ctx,
		cancel: cancel,
		clientId: clientId,
		pairContractIds: model.GetOpenContractIdsForSourceOrDestination(ctx, clientId),
	}

	go bringyour.HandleError(contractManager.syncContracts, cancel)

	return contractManager
}

func (self *contractManager) syncContracts() {
	for {
		select {
		case <- self.ctx.Done():
			return
		case <- time.After(ContractSyncTimeout):
		}

		pairContractIds_ := model.GetOpenContractIdsForSourceOrDestination(self.ctx, self.clientId)
		self.stateLock.Lock()
		self.pairContractIds = pairContractIds_
		// if a contract was added between the sync and set, it will be looked up from the model on miss
		self.stateLock.Unlock()

		// FIXME close expired contracts
	}
}

// this is the "min" or most specific relationship
func (self *contractManager) GetProvideRelationship(sourceId bringyour.Id, destinationId bringyour.Id) model.ProvideMode {
	if sourceId == ControlId || destinationId == ControlId {
		return model.ProvideModeNetwork
	}

	if sourceId == destinationId {
		return model.ProvideModeNetwork
	}

	if sourceClient := model.GetNetworkClient(self.ctx, sourceId); sourceClient != nil {
		if destinationClient := model.GetNetworkClient(self.ctx, destinationId); destinationClient != nil {
			if sourceClient.NetworkId == destinationClient.NetworkId {
				return model.ProvideModeNetwork
			}
		}
	}

	// TODO network and friends-and-family not implemented yet

	return model.ProvideModePublic
}

func (self *contractManager) GetProvideMode(destinationId bringyour.Id) model.ProvideMode {

	if destinationId == ControlId {
		return model.ProvideModeNetwork
	}

	provideMode, err := model.GetProvideMode(self.ctx, destinationId)
	if err != nil {
		return model.ProvideModeNone
	}
	return provideMode
}


func (self *contractManager) HasActiveContract(sourceId bringyour.Id, destinationId bringyour.Id) bool {
	transferPair := model.NewUnorderedTransferPair(sourceId, destinationId)

	self.stateLock.Lock()
	contracts, ok := self.pairContractIds[transferPair]
	self.stateLock.Unlock()

	if !ok {
		contractIds := model.GetOpenContractIds(self.ctx, sourceId, destinationId)
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
	provideMode model.ProvideMode,
) (contractId bringyour.Id, contractTransferBytes int, returnErr error) {
	sourceNetworkId, err := model.FindClientNetwork(self.ctx, sourceId)
	if err != nil {
		// the source is not a real client
		returnErr = err
		return
	}
	destinationNetworkId, err := model.FindClientNetwork(self.ctx, destinationId)
	if err != nil {
		// the destination is not a real client
		returnErr = err
		return
	}
	
	contractTransferBytes = max(MinContractTransferBytes, transferBytes)

	if provideMode < model.ProvideModePublic {
		contractId, err = model.CreateContractNoEscrow(
			self.ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferBytes,
		)
		if err != nil {
			returnErr = err
			return
		}
	} else {
		escrow, err := model.CreateTransferEscrow(
			self.ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferBytes,
		)
		if err != nil {
			returnErr = err
			return
		}
		contractId = escrow.ContractId
	}

	// update the cache
	transferPair := model.NewUnorderedTransferPair(sourceId, destinationId)
	self.stateLock.Lock()
	contracts, ok := self.pairContractIds[transferPair]
	if !ok {
		contracts = map[bringyour.Id]bool{}
		self.pairContractIds[transferPair] = contracts
	}
	contracts[contractId] = true
	self.stateLock.Unlock()

	return
}

func (self *contractManager) CloseContract(
	contractId bringyour.Id,
	clientId bringyour.Id,
	usedTransferBytes int,
) error {
	// update the cache
	self.stateLock.Lock()
	for transferPair, contracts := range self.pairContractIds {
		if transferPair.A == clientId || transferPair.B == clientId {
			delete(contracts, contractId)
		}
	}
	self.stateLock.Unlock()

	err := model.CloseContract(self.ctx, contractId, clientId, usedTransferBytes)
	if err != nil {
		return err
	}

	return nil
}


// each send on the forward updates the send time
// the cleanup removes forwards that haven't been used in some time
// type clientForward struct {
// 	ResidentForward
// 	lastActivityTime time.Time
// }


type clientTransport struct {
	sendTransport *clientSendTransport
	receiveTransport *clientReceiveTransport
}

// func (self *clientTransport) Close() {
// 	self.sendTransport.Close()
// 	self.receiveTransport.Close()
// }


// conforms to `connect.Transport`
type clientSendTransport struct {
	clientId bringyour.Id
}

func newClientSendTransport(clientId bringyour.Id) *clientSendTransport {
	return &clientSendTransport{
		clientId: clientId,
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

// func (self *clientSendTransport) Close() {
// 	close(self.send)
// }


// conforms to `connect.Transport`
type clientReceiveTransport struct {
}

func newClientReceiveTransport() *clientReceiveTransport {
	return &clientReceiveTransport{}
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

// func (self *clientReceiveTransport) Close() {
// 	close(self.receive)
// }


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
	bringyour.Logger().Printf("DELAY FOR %s", timeout)
	if 0 < timeout {
		select {
		case <- self.ctx.Done():
		case <- time.After(timeout):
		}
	}
}