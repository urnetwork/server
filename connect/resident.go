


// UGH I FUCKED UP
// EACH RemoteExchange needs to be its own socket
// EACH socket has a header which is ClientId, ResidentId
// USE A BLOCK OF PORTS for each host, so a host can be internalPort + range(0, 20) or something
// THEN RANDOMLY CHOOSE ONE PORT


	// we use one socket per client transport because the socket will block based on the slowest destination





// because message must be serialized/deserialized from memory,
// there is a global limit on the size per message
// messaged above this size will be ignored from clients and the exchange
const MaximumMessageSizeBytes = 8192

type ConnectMessage = connect.TransferFrame




type ExchangeBuffer struct {

}

func (self *ExchangeBuffer) WriteHeader(conn net.Conn, header *ExchangeHeader) error {

}

func (self *ExchangeBuffer) ReadHeader(conn net.Conn) (*ExchangeHeader, error) {

}

func (self *ExchangeBuffer) WriteFrame(conn net.Conn, message []byte) error {

}

func (self *ExchangeBuffer) ReadFrame(conn net.Conn) ([]byte, error) {

}

func (self *ExchangeBuffer) WriteMessage(conn net.Conn, transferFrame *ConnectMessage) error {

}

func (self *ExchangeBuffer) ReadMessage(conn net.Conn) (*ConnectMessage, error) {

}






type ExchangeOp byte


const (
	ExchangeOpTransport ExchangeOp = 0x01
	// forward does not add a transport to the client
	// in forward op, call Send on the Resident not Receive
	ExchangeOpForward ExchangeOp = 0x02
)


type ExchangeHeader struct {
	clientId Id
	residentId Id
	Op ExchangeOp
}


type ExchangeConnection struct {
	ctx context.Context
	cancel CancelFunc
	conn net.Conn
	sendBuffer &ExchangeBuffer
	receiveBuffer &ExchangeBuffer
	send chan *ConnectMessage
	receive chan *ConnectMessage
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
				case receive <- message:
				case <- ctx.Done():
					close()
					return
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


type ExchangeConnectionDestination struct {
	host string
	// any of the ports may be used
	// a range of ports are used to scale one socket per transport or forward,
	// since each port allows at most 65k connections from another connect instance
	ports []int
}


// an exchange multiplexes messages and transports between service instances
// the goal is to be able to scale to an unlimited number of clients
// e.g. one socket per client would run out at 65k clients


type ResidentTransport struct {
	send chan []byte
	receive chan []byte

	ctx context.Context
	cancel CancelFunc

	clientId Id
	instanceId Id

}

func NewResidentTransport(ctx context.Context, clientId Id, instanceId Id) &ResidentTransport {
	send := make(chan []byte)
	receive := make(chan []byte)
}


func (self *ResidentTransport) Run() {
	

	defer close(send)
	defer close(receive)

	for {
		resident := model.GetResident(clientId, instanceId)
		
		cancelCtx, cancel := context.WithCancel(ctx)

		port := RandomSelection(resident.ports)

		exchangeConnection, err := NewExchangeConnection(resident.host, port, ExchangeOpTransport)
		if err != nil {
			self.localExchange.nominateLocalResident(clientId, instanceId)
			continue
		}


		// write an announce header
		send <- ExchangeMessage{
			OP: OPEN,
			residentId: residentId,
		}

		closeConnection := func() {
			// write an announce header
			send <- ExchangeMessage{
				OP: CLOSE,
				residentId: residentId,
			}
		} 

		// exchangeTransport := exchangeConnection.OpenTransport(clientId, instanceId)

		


		go func() {
			defer func() {
				if recover() {
					// writing to the exchange failed
					// try connecting to the resident again
					cancel()
				}
			}
			for {
				select {
					case cancelCtx.Done():
						return
					case message, ok := receive:
						if !ok {
							cancel()
							return
						}
						exchangeTransport.send <- message
						if message == CLOSE {
							cancel()
							return	
						}
				}
			}
		}()

		func() {
			defer func() {
				if recover() {
					// writing to the exchange failed
					// try connecting to the resident again
					closeConnection()
				}
			}
			for {
				select {
				case cancelCtx.Done():
					return
				case message, ok := <- exchangeTransport.receive:
					if !ok {
						closeConnection()
						return
					}
					if CLIENT_NOT_RESIDENT {
						// this happens when the client is not resident on this connection
						closeConnection()
						return
					} else {
						receive <- message
					}
				}
			}
		}()
	}

}

func (self *ResidentTransport) Close() {
	cancel()
} 





type ResidentForward struct {
	send chan []byte
	receive chan []byte

	ctx context.Context
	cancel CancelFunc

	clientId Id

}


// FIXME
// since the only message a forward will receive is CLOSE,
// the forward times out after some time if there is not a new message to send
// it waits for a message before connecting again
func NewResidentForward(ctx context.Context, clientId Id) &ResidentForward {
	send := make(chan []byte)
	receive := make(chan []byte)
}


func (self *ResidentForward) Run() {
	

	defer close(send)
	defer close(receive)

	for {
		resident := model.GetResident(clientId)
		
		cancelCtx, cancel := context.WithCancel(ctx)

		port := RandomSelection(resident.ports)

		exchangeConnection, err := NewExchangeConnection(resident.host, port, ExchangeOpForward)
		if err != nil {
			// FIXME retry timeout
			continue
		}


		// write an announce header
		send <- ExchangeMessage{
			OP: OPEN,
			residentId: residentId,
		}

		closeConnection := func() {
			// write an announce header
			send <- ExchangeMessage{
				OP: CLOSE,
				residentId: residentId,
			}
		} 

		// exchangeTransport := exchangeConnection.OpenTransport(clientId, instanceId)

		


		go func() {
			defer func() {
				if recover() {
					// writing to the exchange failed
					// try connecting to the resident again
					cancel()
				}
			}
			for {
				select {
					case cancelCtx.Done():
						return
					case message, ok := receive:
						if !ok {
							cancel()
							return
						}
						exchangeTransport.send <- message
						if message == CLOSE {
							cancel()
							return	
						}
				}
			}
		}()

		func() {
			defer func() {
				if recover() {
					// writing to the exchange failed
					// try connecting to the resident again
					closeConnection()
				}
			}
			for {
				select {
				case cancelCtx.Done():
					return
				case message, ok := <- exchangeTransport.receive:
					if !ok {
						closeConnection()
						return
					}
					if CLIENT_NOT_RESIDENT {
						// this happens when the client is not resident on this connection
						closeConnection()
						return
					} else {
						receive <- message
					}
				}
			}
		}()
	}

}


func (self *ResidentForward) Close() {
	cancel()
} 




// the resident lives in the exchange

type Exchange struct {
	ExchangeConnectionDestination

	connectionLock RWMutex
	// clientId -> Resident
	residents map[Id]*Resident

	ctx context.Context
	cancel CancelFunc
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
func (self *Exchange) cleanupLocalResidents(ctx context.Context) {
	// watch for this resident to change
	// FIMXE close all connection IDs for this resident on change
	lastRunTime = time.Now()
	for {
		timeout := lastRunTime + RESIDENT_CHECK_TIMEOUT - time.Now()
		if 0 < timeout {
			select {
			case self.ctx.Done():
				return
			case time.After(RESIDENT_CHECK_TIMEOUT):
			}
		} else {
			select {
			case self.ctx.Done():
				return
			default:
			}
		}

		lastRunTime = time.Now()


		// check for local residents that are no longer the nominated resident

		residentsCopy := map[Id]*Resident{}
		residentLock.RLock()
		maps.Copy(residentsCopy, self.residents)
		residentLock.RUnlock()

		residentIdsToRemove := []Id{}

		for _, resident := range residentsCopy {
			currentResident := model.GetResident(resident.ClientId)
			if currentResident == nil || resident.ResidentId != currentResident.ResidentId {
				resident.Close()
				residentIdsToRemove = append(residentIdsToRemove, resident.ResidentId)
			}
		}

		residentLock.WLock()
		for _, residentId := range residentIdsToRemove {
			delete(self.residents, residentId)
		}
		residentLock.WUnLock()


		// check now for residents in the model associated with this destination
		// that are no longer resident. Remove them from the model.

		residentsForHostPort := model.GetResidentForHostPort(self.host, self.port)
		residentForHostPortToRemove := []*NetworkClientResident{}
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
		residentLock.WUnlock()

		for _, residentForHostPort := range residentForHostPortToRemove {
			model.RemoveResident(residentForHostPort.ClientId, residentForHostPort.ResidentId)
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


func (self *ResidentManager) handleExchangeClient(socket net.Conn) {
	defer socket.Close()

	// FIXME read header

	send := make(chan []byte)
	defer close(send)

	cancelCtx, cancel := context.WithCancel(self.ctx)

	close := func() {
		cancel()
		socket.Close()
	}

	go func() {
		exchangeBuffer := NewExchangeBuffer(32 * 1024)
		for {
			select {
			case exchangeMessage, ok := <- send:
				if !ok {
					close()
					return
				}
				err := exchangeBuffer.Write(socket, exchangeMessage)
				if err != nil {
					close()
					return
				}
				if exchangeMessage.Op == ExchangeOpClose {
					close()
					return
				}
			}
		}
	}()

	exchangeBuffer := NewExchangeBuffer(32 * 1024)
	for {
		exchangeMessage, err := exchangeBuffer.Read(socket)
		if err != nil {
			close()
			return
		}

		switch exchangeMessage.Op {
		case ExchangeOpOpen:
			var resident *Resident
			var ok bool
			residentLock.RLock()
			resident, ok = self.residents[exchangeMessage.ClientId]
			residentLock.RUnlock()

			if ok && resident.ResidentId == exchangeMessage.ResidentId {
				residentMatch.AddTransport(exchangeMessage.exchangeConnectionId, send)
			} else {
				send <- &ExchangeMessage{
					exchangeConnectionId: exchangeMessage.exchangeConnectionId,
					Op: ExchangeOpClose,
				}
			}
		case ExchangeOpClose:
			var resident *Resident
			var ok bool
			residentLock.RLock()
			resident, ok = self.residents[exchangeMessage.ClientId]
			residentLock.RUnlock()

			if ok && resident.ResidentId == exchangeMessage.ResidentId {
				resident.RemoveTransport(exchangeMessage.exchangeConnectionId)
				// ignore error
			}
			// else ignore
		case ExchangeOpMessage:
			var resident *Resident
			var ok bool
			residentLock.RLock()
			resident, ok = self.residents[exchangeMessage.ClientId]
			residentLock.RUnlock()

			if ok && resident.ResidentId == exchangeMessage.ResidentId {
				if transferFrame, err := exchangeBuffer.ParseTransferFrame(exchangeMessage.messageBytes); err != nil {
					resident.Receive(exchangeMessage.exchangeConnectionId, transferFrame)
				}
				// ignore error
			}
			// else ignore

		// else unknown op, drop the message
		}
	}
}


func (self *Exchange) Close() {
	cancel()
}









type Resident struct {
	// FIXME create client, run client

	stateLock RWMutex

	transports := map[Id]TRANSPORT
}

// FIXME handle control messages


// FIXME forward cleanup process cleans up forward connections that have not been used since the last cleanup


// the client has ClientId == CONTROL_ID
// forward, if destination == clientId, use the transports
// transports match destination of clientId only
// forward, if destination != clientId, 
//   find the resident
//   if no resident, wait for resident
//   if resident, open a connection to the resident


func (self *Resident) clientForward(message []byte) {
	// FIXME verify the sourceId of messages is clientId
	// FIXME verify there is a valid contract between clientId and destinationId
	//      use a local cache of created contracts, and a background func that validates the open contracts continually

	send := self.GetExchange(destinationId)

	// we need back pressure from the send to limit the receive
	send <- message
}


func Run() {

}


func Close() {
	resident.WRITELOCK()
	for _, transport := range resident.transports {
		transport.send <- ExchangeMessage{
			exchangeConnectionId: transport.exchangeConnectionId,
			OP: CLOSE,
		}
		resident.rotueManager.updateTransport(transport, nil)
	}
	clear(self.transports)
	resident.UNWRITELOCK()
}


func handleControlMessage(client Client, message TransferFrame) {
	
}


// FIXME AddTransfer, RemoveTransport, Receive, CLose must acquire stateLock
// FIXME must check context for closed


