package main


const PingTimeout = 5 * time.Second
const SyncConnectionTimeout = 60 * time.Second


// each client connection is a transport for the resident client
// there can be multiple simultaneous client connections from the same client instance
// all connections from the same client will eventually terminate at the same resident,
// where each connection will be a `connect.Transport` and traffic will be distributed across the transports


type ConnectHandler struct {
    ctx context.Context
	exchange *Exchange
}

func NewConnectHandler(ctx context.Context, exchange *Exchange) *ConnectHandler {
    return &ConnectHandler{
        ctx: ctx,
        exchange: exchange,
    }
}

func (self *ConnectHandler) Connect(w http.ResponseWriter, r *http.Request) {
    upgrader := websocket.Upgrader{
        ReadBufferSize: 1024,
        WriteBufferSize: 1024,
    }

    ws, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        return
    }

    cancelCtx, cancel := context.WithCancel(self.ctx)

    close := func() {
        cancel()
        ws.Close()
    }
    defer close()


    authBytes := ws.Read()

    controlAuth := proto.ParseTransferFrame(authBytes)

    byJwt, err := jwt.ParseByJwt(controlAuth.ByJwt)

    if err != nil {
        return
    }

    if byJwt.ClientId == nil {
        return
    }

    client := model.GetNetworkClient(byJwt.ClientId)
    if client == nil || client.NetworkId != byJwt.NetworkId {
        return
    }

    // FIXME use correct warp header
    // find the client IP from the request header
    ipStr := r.Headers.Get("X-IP")

    connectionId := controller.ConnectNetworkClient(byJwt.ClientId, ipStr)
    defer model.DisconnectNetworkClient(connectionId)

    residentTransport := NewResidentTransport(
        cancelCtx,
        self.exchange,
        byJwt.ClientId,
        controlAuth.InstanceId,
    )
    defer residentTransport.Close()

    go func() {
    	// disconnect the client if the model marks the connection closed
        defer close()

    	for  {
    		select {
    		case <- cancelCtx.Done():
    			return
    		case <- time.After(SyncConnectionTimeout):
    		}

    		if !model.IsNetworkClientConnected(connectionId) {
    			return
    		}
    	}
    }()

    go func() {
        defer close()

    	for {
            messageType, message, err := ws.ReadMessage()
            if err != nil {
                return
            }
            switch messageType {
            case websocket.BinaryMessage:
                select {
                case cancelCtx.Done():
                    return
                case residentSend <- message:
                }
            // else ignore
            }
        }
    }()


    for {
        select {
        case <- cancelCtx.Done():
            return
        case message, ok := <- residentTransport.receive:
            if err := ws.WriteMessage(websocket.BinaryMessage, message); err != nil {
                return
            }
        case time.After(PingTimeout):
            if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
                return
            }
    }

    
}
