package main



type ConnectHandler struct {
	residentManager *ResidentManager
}



func (self *ConnectHandler) Connect(w http.ResponseWriter, r *http.Request) {
    // each client connect is a transport for the resident client
    // there can be multiple simultaneous client connections from the same instance

    // FIXME
    // websocket
    // read auth header
    // decode jwt in auth header
    // validate that the client_id, network_id is valid (active)

    authBytes := ws.Read()

    controlAuth := proto.ParseTransferFrame(authBytes)

    byJwt, err := jwt.ParseByJwt(controlAuth.ByJwt)

    if err != nil {
        Close()
        return
    }

    if byJwt.ClientId == nil {
        Close()
        return
    }

    client := model.GetNetworkClient(byJwt.ClientId)
    if client == nil || client.NetworkId != byJwt.NetworkId {
        Close()
        return
    }


    cancelCtx, cancel := context.WithCancel(self.ctx)

    connectionId := ClientConnect()
    defer ClientDisconnect(connectionId)

    go func() {
    	// disconnect the client if the model marks the connection closed
    	for  {
    		select {
    		case <- cancelCtx.Done():
    			return
    		case <- time.After(30 * time.Second):
    		}

    		if !model.IsConnected(connectionId) {
    			cancel()
    			return
    		}
    	}
    }()

    residentSend, residentReceive := residentManager.CreateResidentTransport(cancelCtx, clientId, instanceId)
    defer cancel()

    go func() {
    	for {
    		message, ok := <- residentReceive
    		err := ws.Write(message)
            if err != nil {
                // connection closed
                cancel()
                return
            }
    	}
    }()

    func() {
        for {
        	message, err := ws.Read()
            if err != nil {
                // connection closed
                return
            }
        	residentSend <- message
        }
    }
}





