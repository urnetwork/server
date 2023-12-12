package session

import (
	"context"
	"net/http"
	"strings"
	"strconv"
	// "encoding/base64"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
)


// https://www.rfc-editor.org/rfc/rfc6750
const authBearerPrefix = "Bearer "


type ClientSession struct {
	Ctx context.Context
	Cancel context.CancelFunc
	// ip:port
	ClientAddress string
	ByJwt *jwt.ByJwt
}

func NewClientSessionFromRequest(req *http.Request) (*ClientSession, error) {
	cancelCtx, cancel := context.WithCancel(req.Context())

	clientAddress := req.Header.Get("X-Forwarded-For")
	if clientAddress == "" {
		clientAddress = req.RemoteAddr
	}

	var byJwt *jwt.ByJwt
	if auth := req.Header.Get("Authorization"); auth != "" {
    	if strings.HasPrefix(auth, authBearerPrefix) {
    		jwtStr := auth[len(authBearerPrefix):]
    		var err error
			byJwt, err = jwt.ParseByJwt(jwtStr)
			if err != nil {
				return nil, err
			}
	    	bringyour.Logger().Printf("Authed as %s (%s %s)\n", byJwt.UserId, byJwt.NetworkName, byJwt.NetworkId)
    	}
    }

	return &ClientSession{
		Ctx: cancelCtx,
		Cancel: cancel,
		ClientAddress: clientAddress,
		ByJwt: byJwt,
	}, nil
}

func NewLocalClientSession(ctx context.Context, byJwt *jwt.ByJwt) *ClientSession {
	cancelCtx, cancel := context.WithCancel(ctx)

	clientAddress := "0.0.0.0:0"

	return &ClientSession{
		Ctx: cancelCtx,
		Cancel: cancel,
		ClientAddress: clientAddress,
		ByJwt: byJwt,
	}
}


func (self *ClientSession) ClientIpPort() (ip string, port int) {
	parts := strings.Split(self.ClientAddress, ":")
	ip = parts[0]
	if 1 < len(parts) {
		port, _ = strconv.Atoi(parts[1])
	}
	return
}
