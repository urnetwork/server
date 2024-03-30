package main

import (
    "context"
    "time"
    "net/http"
    "fmt"

    "github.com/gorilla/websocket"

    "bringyour.com/bringyour"
    "bringyour.com/bringyour/jwt"
    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/controller"
    "bringyour.com/connect"
    "bringyour.com/protocol"
)


// each client connection is a transport for the resident client
// there can be multiple simultaneous client connections from the same client instance
// all connections from the same client will eventually terminate at the same resident,
// where each connection will be a `connect.Transport` and traffic will be distributed across the transports


type ConnectHandlerSettings struct {
    PingTimeout time.Duration
    WriteTimeout time.Duration
    ReadTimeout time.Duration
    SyncConnectionTimeout time.Duration
    MaximumExchangeMessageByteCount ByteCount
}


func DefaultConnectHandlerSettings() *ConnectHandlerSettings {
    platformTransportSettings := connect.DefaultPlatformTransportSettings()
    return &ConnectHandlerSettings{
        PingTimeout: platformTransportSettings.PingTimeout,
        WriteTimeout: platformTransportSettings.WriteTimeout,
        ReadTimeout: platformTransportSettings.ReadTimeout,
        SyncConnectionTimeout: 60 * time.Second,

        // a single exchange message size is encoded as an `int32`
        // because message must be serialized/deserialized from memory,
        // there is a global limit on the size per message
        // messages above this size will be ignored from clients and the exchange
        MaximumExchangeMessageByteCount: ByteCount(4 * 1024 * 1024),

    }
}


type ConnectHandler struct {
    ctx context.Context
	exchange *Exchange
    settings *ConnectHandlerSettings
}


func NewConnectHandlerWithDefaults(ctx context.Context, exchange *Exchange) *ConnectHandler {
    return NewConnectHandler(ctx, exchange, DefaultConnectHandlerSettings())
}

func NewConnectHandler(ctx context.Context, exchange *Exchange, settings *ConnectHandlerSettings) *ConnectHandler {
    return &ConnectHandler{
        ctx: ctx,
        exchange: exchange,
        settings: settings,
    }
}

func (self *ConnectHandler) Connect(w http.ResponseWriter, r *http.Request) {
    handleCtx, handleCancel := context.WithCancel(self.ctx)
    defer handleCancel()

    // bringyour.Logger().Printf("CONNECT\b")

    upgrader := websocket.Upgrader{
        ReadBufferSize: 4 * 1024,
        WriteBufferSize: 4 * 1024,
    }

    ws, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        return
    }
    defer ws.Close()

    // enforce the message size limit on messages in
    ws.SetReadLimit(self.settings.MaximumExchangeMessageByteCount)

    ws.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
    messageType, authFrameBytes, err := ws.ReadMessage()
    if err != nil {
        // bringyour.Logger("TIMEOUT HA\n")
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

    clientId := *byJwt.ClientId

    instanceId, err := bringyour.IdFromBytes(auth.InstanceId)
    if err != nil {
        return
    }


    // verify the client is still part of the network
    // this will fail for example if the client has been removed
    client := model.GetNetworkClient(handleCtx, clientId)
    if client == nil || client.NetworkId != byJwt.NetworkId {
        // bringyour.Logger("ERROR HB\n")
        return
    }

    // echo the auth message on successful auth
    ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
    err = ws.WriteMessage(websocket.BinaryMessage, authFrameBytes)
    if err != nil {
        // bringyour.Logger("TIMEOUT HC\n")
        return
    }

    // find the client ip:port from the request header
    // `X-Forwarded-For` is added by the warp lb
    clientAddress := r.Header.Get("X-Forwarded-For")
    // bringyour.Logger().Printf("X-Forwarded-For header is %s (%s)", clientAddress, r.RemoteAddr)
    if clientAddress == "" {
        // use the raw connection remote address
        clientAddress = r.RemoteAddr
    }

    bringyour.Trace(fmt.Sprintf("[rt]connect %s", clientId.String()), func() {
        connectionId := controller.ConnectNetworkClient(handleCtx, clientId, clientAddress)
        defer model.DisconnectNetworkClient(self.ctx, connectionId)
        
        go bringyour.HandleError(func() {
        	// disconnect the client if the model marks the connection closed
            defer func() {
                handleCancel()
            }()

        	for  {
        		select {
        		case <- handleCtx.Done():
        			return
        		case <- time.After(self.settings.SyncConnectionTimeout):
        		}

        		if !model.IsNetworkClientConnected(handleCtx, connectionId) {
                    // bringyour.Logger().Printf("NETWORK CLIENT DISCONNECTED\n",)
        			return
        		}
        	}
        }, handleCancel)


        residentTransport := NewResidentTransport(
            handleCtx,
            self.exchange,
            clientId,
            instanceId,
        )
        go func() {
            defer handleCancel()
            bringyour.HandleError(residentTransport.Run)
            residentTransport.Close()
            ws.Close()
        }()

        go bringyour.HandleError(func() {
            // close the transport in the send
            defer func() {
                handleCancel()
                residentTransport.Cancel()
            }()

        	for {
                ws.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
                messageType, message, err := ws.ReadMessage()
                // // bringyour.Logger().Printf("CONNECT HANDLER RECEIVE MESSAGE %s %s\n", message, err)
                if err != nil {
                    // bringyour.Logger("TIMEOUT HD\n")
                    return
                }


                switch messageType {
                case websocket.BinaryMessage:
                    if 0 == len(message) {
                        // ping
                        // fmt.Printf("[rtr]ping <-%s\n", clientId.String())
                        continue
                    }


                    // fmt.Printf("resident transport read message %s->\n", byJwt.ClientId)


                    select {
                    case <- handleCtx.Done():
                        return
                    case <- residentTransport.Done():
                        return
                    case residentTransport.send <- message:

                        fmt.Printf("[rtr] <-%s\n", clientId.String())
                    case <- time.After(self.settings.ReadTimeout):
                        // bringyour.Logger("TIMEOUT HE\n")
                    }
                // else ignore
                }
            }
        }, handleCancel)


        go bringyour.HandleError(func() {
            defer func() {
                handleCancel()
                residentTransport.Cancel()
            }()
            
            for {
                select {
                case <- handleCtx.Done():
                    return
                case <- residentTransport.Done():
                    return
                case message, ok := <- residentTransport.receive:
                    if !ok {
                        return
                    }
                    
                    // fmt.Printf("resident transport write message ->%s\n", byJwt.ClientId)

                    // bringyour.Logger("WRITE (%d) -> %s\n", len(message), byJwt.ClientId.String())
                    // // bringyour.Logger().Printf("CONNECT HANDLER SEND MESSAGE %s\n", message)
                    ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
                    if err := ws.WriteMessage(websocket.BinaryMessage, message); err != nil {
                        // note that for websocket a dealine timeout cannot be recovered
                        // bringyour.Logger().Printf("CONNECT HANDLER SEND MESSAGE ERROR %s\n", err)
                        return
                    }
                    fmt.Printf("[rts] ->%s\n", clientId.String())
                case <- time.After(self.settings.PingTimeout):
                    ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
                    if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 0)); err != nil {
                        // note that for websocket a dealine timeout cannot be recovered
                        // bringyour.Logger().Printf("CONNECT HANDLER SEND PING ERROR %s\n", err)
                        return
                    }
                    // fmt.Printf("[rts]ping ->%s\n", clientId.String())
                }
            }
        }, handleCancel)

        select {
        case <- handleCtx.Done():
            return
        case <- residentTransport.Done():
            return
        }
    })
}
