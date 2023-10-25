package main

import (
    "context"
    "time"
    "net/http"

    "github.com/gorilla/websocket"

    "bringyour.com/bringyour"
    "bringyour.com/bringyour/jwt"
    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/controller"
    "bringyour.com/connect"
    "bringyour.com/protocol"
)


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
    bringyour.Logger().Printf("CONNECT\b")

    upgrader := websocket.Upgrader{
        ReadBufferSize: 1024,
        WriteBufferSize: 1024,
    }

    ws, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        return
    }

    cancelCtx, cancel := context.WithCancel(self.ctx)

    closeHandle := func() {
        bringyour.Logger().Printf("NETWORK CLIENT CLOSE HANDLE\n",)
        cancel()
        ws.Close()
    }
    defer closeHandle()


    messageType, authFrameBytes, err := ws.ReadMessage()
    if err != nil {
        return
    }
    if messageType != websocket.BinaryMessage {
        return
    }

    message, err := connect.DecodeFrame(authFrameBytes)
    if err != nil {
        return
    }
    auth, ok := message.(*protocol.Auth)
    if !ok {
        return
    }

    byJwt, err := jwt.ParseByJwt(auth.ByJwt)
    if err != nil {
        return
    }

    if byJwt.ClientId == nil {
        return
    }

    instanceId, err := bringyour.IdFromBytes(auth.InstanceId)
    if err != nil {
        return
    }

    // verify the client is still part of the network
    // this will fail for example if the client has been removed
    client := model.GetNetworkClient(cancelCtx, *byJwt.ClientId)
    if client == nil || client.NetworkId != byJwt.NetworkId {
        return
    }

    // echo the auth message on successful auth
    err = ws.WriteMessage(websocket.BinaryMessage, authFrameBytes)
    if err != nil {
        return
    }

    // find the client ip:port from the request header
    // `X-Forwarded-For` is added by the warp lb
    clientAddress := r.Header.Get("X-Forwarded-For")
    if clientAddress == "" {
        // use the raw connection remote address
        clientAddress = r.RemoteAddr
    }

    connectionId := controller.ConnectNetworkClient(cancelCtx, *byJwt.ClientId, clientAddress)
    defer model.DisconnectNetworkClient(cancelCtx, connectionId)

    residentTransport := NewResidentTransport(
        cancelCtx,
        self.exchange,
        *byJwt.ClientId,
        instanceId,
    )
    defer residentTransport.Close()

    go bringyour.HandleError(func() {
    	// disconnect the client if the model marks the connection closed
        defer closeHandle()

    	for  {
    		select {
    		case <- cancelCtx.Done():
    			return
    		case <- time.After(SyncConnectionTimeout):
    		}

    		if !model.IsNetworkClientConnected(cancelCtx, connectionId) {
                bringyour.Logger().Printf("NETWORK CLIENT DISCONNECTED\n",)
    			return
    		}
    	}
    }, cancel)

    go bringyour.HandleError(func() {
        defer closeHandle()

    	for {
            messageType, message, err := ws.ReadMessage()
            bringyour.Logger().Printf("CONNECT HANDLER RECEIVE MESSAGE %s %s\n", message, err)
            if err != nil {
                return
            }
            switch messageType {
            case websocket.BinaryMessage:
                select {
                case <- cancelCtx.Done():
                    return
                case residentTransport.send <- message:
                }
            // else ignore
            }
        }
    }, cancel)


    for {
        select {
        case <- cancelCtx.Done():
            return
        case message, ok := <- residentTransport.receive:
            if !ok {
                return
            }
            bringyour.Logger().Printf("CONNECT HANDLER SEND MESSAGE %s\n", message)
            if err := ws.WriteMessage(websocket.BinaryMessage, message); err != nil {
                bringyour.Logger().Printf("CONNECT HANDLER SEND MESSAGE ERROR %s\n", err)
                return
            }
        case <- time.After(PingTimeout):
            if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
                bringyour.Logger().Printf("CONNECT HANDLER SEND PING ERROR %s\n", err)
                return
            }
        }
    }
}
