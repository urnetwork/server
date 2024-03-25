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

	mathrand "math/rand"

	"golang.org/x/exp/maps"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/connect"
	"bringyour.com/protocol"
)


// note -
// we use one socket per client transport because the socket will block based on the slowest destination


type ByteCount = model.ByteCount

var ControlId = bringyour.Id(connect.ControlId)


type ExchangeSettings struct {
	ConnectHandlerSettings

	ExchangeBufferSize int

	MaximumExchangeMessageByteCount ByteCount

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
	ForwardTimeout time.Duration

	ExchangeConnectTimeout time.Duration
	ExchangePingTimeout time.Duration
	ExchangeReadTimeout time.Duration
	ExchangeReadHeaderTimeout time.Duration
	ExchangeWriteTimeout time.Duration
	ExchangeWriteHeaderTimeout time.Duration
	ExchangeReconnectAfterErrorTimeout time.Duration

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
	exchangePingTimeout := 1 * time.Second
	exchangeResidentWaitTimeout := 1 * time.Second
	return &ExchangeSettings{
		ConnectHandlerSettings: *DefaultConnectHandlerSettings(),

		ExchangeBufferSize: 32,

		// a single exchange message size is encoded as an `int32`
		// because message must be serialized/deserialized from memory,
		// there is a global limit on the size per message
		// messages above this size will be ignored from clients and the exchange
		MaximumExchangeMessageByteCount: ByteCount(4 * 1024 * 1024),

		// 64kib minimum contract
		// this is set high enough to limit the number of parallel contracts and avoid contract spam
		MinContractTransferByteCount: ByteCount(64 * 1024),

		// this must match the warp `settings.yml` for the environment
		StartInternalPort: 5080,

		MaxConcurrentForwardsPerResident: 32,

		ResidentIdleTimeout: 5 * time.Minute,
		ResidentSyncTimeout: 30 * time.Second,
		ForwardIdleTimeout: 5 * time.Minute,
		ContractSyncTimeout: 30 * time.Second,
		AbuseMinTimeout: 5 * time.Second,
		ControlMinTimeout: 200 * time.Millisecond,

		ClientDrainTimeout: 30 * time.Second,
		TransportDrainTimeout: 30 * time.Second,
		ForwardTimeout: 5 * time.Second,

		ExchangeConnectTimeout: 1 * time.Second,
		ExchangePingTimeout: exchangePingTimeout,
		ExchangeReadTimeout: 2 * exchangePingTimeout,
		ExchangeReadHeaderTimeout: 1 * time.Second,
		ExchangeWriteTimeout: 5 * time.Second,
		ExchangeWriteHeaderTimeout: 1 * time.Second,
		ExchangeReconnectAfterErrorTimeout: 1 * time.Second,

		ExchangeConnectionResidentPollTimeout: 1 * time.Second,

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
	cleanupCtx context.Context
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

	residentsLock sync.RWMutex
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
	if nominated.ResidentId == residentId {
		// defer func() {
		// 	r := model.GetResidentWithInstance(self.ctx, clientId, instanceId)
		// 	// bringyour.Logger().Printf("NOMINATED RESIDENT VERIFIED %s <> %s\n", nominated.ResidentId.String(), r.ResidentId.String())
		// }()


		func() {
			self.residentsLock.Lock()
			defer self.residentsLock.Unlock()
			
			replacedResident := self.residents[clientId]
			// bringyour.Logger().Printf("SET LOCAL RESIDENT %s\n", clientId.String())

			if replacedResident != nil {
				replacedResident.Close()
				model.RemoveResident(self.ctx, replacedResident.clientId, replacedResident.residentId)
			}
			// fmt.Printf("exchange set resident clientId=%s\n", clientId)
			self.residents[clientId] = NewResident(
				self.ctx,
				self,
				clientId,
				instanceId,
				residentId,
			)
		}()

		return true
	} else {
		// another was nominated
		// self.residentsLock.Lock()
		// if currentResident, ok := self.residents[clientId]; ok && currentResident == resident {
		// 	// bringyour.Logger().Printf("DELETE LOCAL RESIDENT %s\n", clientId.String())
		// 	resident.Close()
		// 	fmt.Printf("exchange remove resident 3\n")
		// 	delete(self.residents, clientId)
		// }
		// self.residentsLock.Unlock()

		return false
	}

}

// continually cleans up the local resident state, connections, and model based on the latest nominations
func (self *Exchange) syncResidents() {
	// watch for this resident to change
	// FIMXE close all connection IDs for this resident on change
	lastRunTime := time.Now()
	for {
		timeout := lastRunTime.Add(self.settings.ResidentSyncTimeout).Sub(time.Now())
		if 0 < timeout {
			select {
			case <- self.ctx.Done():
				return
			case <- time.After(self.settings.ResidentSyncTimeout):
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

		residentsForHostPort := model.GetResidentsForHostPorts(self.ctx, self.host, maps.Keys(self.hostToServicePorts))
		
		func() {
			self.residentsLock.Lock()
			defer self.residentsLock.Unlock()

			residentsToClose := []*Resident{}
			
			residentIdsForHostPort := map[bringyour.Id]bool{}
			for _, residentsForHostPort := range residentsForHostPort {
				residentIdsForHostPort[residentsForHostPort.ResidentId] = true
			}

			residentsForHostPortToRemove := []*model.NetworkClientResident{}

			// check for residents with no transports, and have no activity in some time
			residentsToRemove := []*Resident{}


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
					// fmt.Printf("exchange remove resident 1\n")
					delete(self.residents, resident.clientId)
				}
			}

			for _, resident := range residentsToClose {
				// bringyour.Logger("CLOSE RESIDENT\n")
				resident.Close()
			}

			for _, residentForHostPort := range residentsForHostPortToRemove {
				// bringyour.Logger("REMOVE RESIDENT\n")
				model.RemoveResident(self.ctx, residentForHostPort.ClientId, residentForHostPort.ResidentId)
			}

			for _, resident := range self.residents {
				if resident.IsIdle() || resident.IsDone() {
					residentsToRemove = append(residentsToRemove, resident)
					// fmt.Printf("exchange remove resident 2\n")
					delete(self.residents, resident.clientId)
				}
			}

			for _, resident := range residentsToRemove {
				// bringyour.Logger("CLOSE AND REMOVE RESIDENT\n")
				resident.Close()
				model.RemoveResident(self.ctx, resident.clientId, resident.residentId)
			}
		}()
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

	// bringyour.Logger().Printf("START EXCHANGE LISTEN ON HOST %s PORTS %v", self.host, self.hostToServicePorts)

	for _, servicePort := range self.hostToServicePorts {
		port := servicePort
		go bringyour.HandleError(func() {
			defer self.cancel()

			// bringyour.Logger().Printf("EXCHANGE LISTEN ON PORT %d", port)
			// leave host part empty to listen on all available interfaces
			server, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
			if err != nil {
				// bringyour.Logger().Printf("EXCHANGE LISTEN ON PORT %d ERROR %s", port, err)
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
						// bringyour.Logger().Printf("EXCHANGE LISTEN ON PORT %d ACCEPT ERROR %s", port, err)
						return
					}
					// fmt.Printf("exchange %s accept connection\n", self.host)
					// bringyour.Logger().Printf("EXCHANGE LISTEN ON PORT %d ACCEPT %s", port, self.host)
					go bringyour.HandleError(
						func() {
							self.handleExchangeConnection(conn)
						},
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

	// bringyour.Logger().Printf("EXCHANGE HANDLE CLIENT\n")

	receiveBuffer := NewReceiveOnlyExchangeBuffer(self.settings)

	header, err := receiveBuffer.ReadHeader(handleCtx, conn)
	if err != nil {
		// fmt.Printf("exchange %s connection end 1\n", self.host)
		// bringyour.Logger().Printf("EXCHANGE HANDLE CLIENT READ HEADER ERROR %s\n", err)
		return
	}

	waitForResident := func()(*Resident) {
		endTime := time.Now().Add(self.settings.ExchangeResidentWaitTimeout)
		for {
			self.residentsLock.RLock()
			resident, ok := self.residents[header.ClientId]
			// // bringyour.Logger().Printf("EXCHANGE ALL RESIDENTS %v\n", self.residents)
			self.residentsLock.RUnlock()

			if ok && resident.residentId == header.ResidentId {
				// bringyour.Logger().Printf("EXCHANGE HANDLE CLIENT MISSING RESIDENT %s %s (%t, %v) \n", header.ClientId.String(), header.ResidentId.String(), ok, resident)
				return resident
			}

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

	resident := waitForResident()

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
				select {
				case <- handleCtx.Done():
					return
				case <- resident.Done():
					return
				case receive <- message:
				case <- time.After(self.settings.WriteTimeout):
					// fmt.Printf("rw timeout\n")
					// bringyour.Logger("TIMEOUT RF\n")
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
				success := resident.Forward(message) 
				if !success {
					// bringyour.Logger().Printf("RESIDENT FORWARD FALSE\n")
					// FIXME messages can be dropped - make sure back pressure is fine
					// return
				}
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
	
	
	// close all residents
	func() {
		self.residentsLock.Lock()
		defer self.residentsLock.Unlock()
		for _, resident := range self.residents {
			resident.Close()
			model.RemoveResident(self.ctx, resident.clientId, resident.residentId)	
		}
		// fmt.Printf("exchange clear residents\n")
		clear(self.residents)
	}()

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
	conn.SetWriteDeadline(time.Now().Add(self.settings.ExchangeWriteTimeout))

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
					continue
				}
				select {
				case <- self.ctx.Done():
					return
				case self.receive <- message:
				case <- time.After(self.settings.WriteTimeout):
					// bringyour.Logger("TIMEOUT RB\n")
				}
			}
		}, self.cancel)
	default:
		// nothing to receive, but time out on missing pings
		close(self.receive)

		go bringyour.HandleError(func() {
			defer func() {
				self.cancel()
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
			case <- time.After(self.settings.ExchangePingTimeout):
				// send a ping
				if err := self.sendBuffer.WriteMessage(self.ctx, self.conn, make([]byte, 0)); err != nil {
					// bringyour.Logger("ERROR WRITING PING MESSAGE %s\n", err)
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
	go bringyour.HandleError(transport.Run, cancel)
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
					case <- time.After(self.exchange.settings.ConnectHandlerSettings.WriteTimeout):
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
				case <- time.After(self.exchange.settings.ConnectHandlerSettings.WriteTimeout):
					// fmt.Printf("c timeout\n")
					// bringyour.Logger("TIMEOUT RC\n")
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

			if err == nil {
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
			} else {
				// bringyour.Logger().Printf("EXCHANGE CONNECTION ERROR: %s\n", err)
				// fmt.Printf("transport exchange connect error (%s)\n", err)
				select {
				case <- self.ctx.Done():
					return
				case <- time.After(self.exchange.settings.ExchangeReconnectAfterErrorTimeout):
				}
			}
		} else {
			// bringyour.Logger().Printf("EXCHANGE SKIP RESIDENT\n")
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
		// fmt.Printf("transport nominated success=%t %s\n", success, self.exchange.host)
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
		send: make(chan []byte, exchange.settings.ExchangeBufferSize),
		lastActivityTime: time.Now(),
	}
	go bringyour.HandleError(transport.Run, cancel)
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
				case <- time.After(self.exchange.settings.ConnectHandlerSettings.WriteTimeout):
					// fmt.Printf("f timeout\n")
					// bringyour.Logger("TIMEOUT RE\n")
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
			if err == nil {
				// handleCtx, handleCancel := context.WithCancel(self.ctx)
				handle(exchangeConnection, func()(bool) {
					currentResidentId, err := model.GetResidentId(self.ctx, self.clientId)
					if err != nil {
						return false
					}
					return resident.ResidentId == currentResidentId
				})
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

	lastActivityTime time.Time

	abuseLimiter *limiter
	controlLimiter *limiter

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

	client := connect.NewClientWithDefaults(cancelCtx, connect.ControlId)

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
		client,
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
	}

	clientReceiveUnsub := client.AddReceiveCallback(resident.handleClientReceive)
	resident.clientReceiveUnsub = clientReceiveUnsub

	clientForwardUnsub := client.AddForwardCallback(resident.handleClientForward)
	resident.clientForwardUnsub = clientForwardUnsub

	// client.Setup(clientRouteManager, clientContractManager)

	// go bringyour.HandleError(func() {
	// 	client.Run()
	// }, cancel)

	go bringyour.HandleError(resident.cleanupForwards, cancel)

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
					// bringyour.Logger("RESIDENT CHAOS SHUTDOWN host=%s clientId=%s residentId=%s\n", exchange.host, clientId.String(), residentId.String())
					resident.Cancel()
				}
			}
		}, cancel)
	}

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
		case <- time.After(self.exchange.settings.ForwardIdleTimeout):
		}

		// clean up forwards that have not been used after an idle timeout

		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			forwardsToRemove := []*ResidentForward{}
			for _, forward := range self.forwards {
				if self.exchange.settings.ForwardIdleTimeout <= time.Now().Sub(forward.lastActivityTime) {
					forwardsToRemove = append(forwardsToRemove, forward)
				}
			}
			for _, forward := range forwardsToRemove {
				// bringyour.Logger().Printf("CLEAN UP FORWARD\n")
				forward.Close()
				delete(self.forwards, forward.clientId)
			}
		}()
	}
}

// `connect.ForwardFunction`
func (self *Resident) handleClientForward(sourceId_ connect.Id, destinationId_ connect.Id, transferFrameBytes []byte) {
	sourceId := bringyour.Id(sourceId_)
	destinationId := bringyour.Id(destinationId_)

	// // bringyour.Logger().Printf("HANDLE CLIENT FORWARD %s %s %s %s\n", self.clientId.String(), sourceId.String(), destinationId.String(), transferFrameBytes)

	self.updateActivity()

	if sourceId != self.clientId {
		// // bringyour.Logger().Printf("HANDLE CLIENT FORWARD BAD SOURCE\n")

		// the message is not from the client
		// clients are not allowed to forward from other clients
		// drop
		self.abuseLimiter.delay()
		return
	}

	if self.exchange.settings.ForwardEnforceActiveContracts && !self.residentContractManager.HasActiveContract(sourceId, destinationId) {
		// // bringyour.Logger().Printf("HANDLE CLIENT FORWARD NO CONTRACT\n")

		// there is no active contract
		// drop
		self.abuseLimiter.delay()
		return
	}

	var forward *ResidentForward

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		var ok bool
		forward, ok = self.forwards[destinationId]
		if ok {
			forward.lastActivityTime = time.Now()
		} else if len(self.forwards) < self.exchange.settings.MaxConcurrentForwardsPerResident {
			forward = NewResidentForward(self.ctx, self.exchange, destinationId)
			self.forwards[destinationId] = forward
		}
	}()

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
	case <- time.After(self.exchange.settings.ForwardTimeout):
		// fmt.Printf("forward timeout\n")
		// FIXME need to debug this timeout case
		// bringyour.Logger().Printf("!!!! RESIDENT FORWARD TIMEOUT")	
	}

	// recreate the forward and try again
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		forward.Cancel()
		forward = NewResidentForward(self.ctx, self.exchange, destinationId)
		self.forwards[destinationId] = forward
	}()

	select {
	case <- self.ctx.Done():
		return
	case <- forward.Done():
		return
	case forward.send <- transferFrameBytes:
		return
	case <- time.After(self.exchange.settings.ForwardTimeout):
		// fmt.Printf("forward timeout\n")
		// bringyour.Logger("TIMEOUT RG\n")
		// drop the message
		return
	}
}

// `connect.ReceiveFunction`
func (self *Resident) handleClientReceive(sourceId_ connect.Id, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
	sourceId := bringyour.Id(sourceId_)

	// // bringyour.Logger().Printf("HANDLE CLIENT RECEIVE %s %s %s\n", sourceId.String(), provideMode, frames)

	// these are messages to the control id
	// use `client.Send` to send messages back to the client

	// the provideMode should always be `Network`, by configuration
	// `Network` is the highest privilege in the user network
	if model.ProvideMode(provideMode) != model.ProvideModeNetwork {
		// // bringyour.Logger().Printf("HANDLE CLIENT RECEIVE NOT IN NETWORK\n")
		return
	}

	if sourceId != self.clientId {
		// only messages from the resident client are processed by the resident
		return
	}

	self.updateActivity()
	self.controlLimiter.delay()

	for _, frame := range frames {
		// bringyour.Logger().Printf("HANDLE CLIENT RECEIVE FRAME %s\n", frame)
		if message, err := connect.FromFrame(frame); err == nil {
			self.residentController.HandleControlMessage(message)
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
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.transports[transport] = true
		routeManager := self.client.RouteManager()
		routeManager.UpdateTransport(transport.sendTransport, []connect.Route{send})
		routeManager.UpdateTransport(transport.receiveTransport, []connect.Route{receive})
	}()

	closeTransport = func() {
		// bringyour.Logger().Printf("REMOVE TRANSPORT %s\n", self.clientId.String())

		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			routeManager := self.client.RouteManager()
			routeManager.RemoveTransport(transport.sendTransport)
			routeManager.RemoveTransport(transport.receiveTransport)
			delete(self.transports, transport)
		}()

		// transport.Close()

		// close(send)

		go func() {
			select {
			case <- time.After(self.exchange.settings.TransportDrainTimeout):
			}

			close(send)
			// close(receive)
		}()
	}

	return
}

func (self *Resident) Forward(transferFrameBytes []byte) bool {
	self.updateActivity()
	return self.client.ForwardWithTimeout(transferFrameBytes, self.exchange.settings.ForwardTimeout)
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
	default:
		return false
	}
}

func (self *Resident) Cancel() {
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
		
	}()

	go func() {
		select {
		case <- time.After(self.exchange.settings.ClientDrainTimeout):
		}

		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			// close forwards
			for _, forward := range self.forwards {
				forward.Close()
			}
			clear(self.forwards)
		}()

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