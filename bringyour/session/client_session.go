package session

import (
	"context"
	"net/http"
	"strings"
	"strconv"
	// "encoding/base64"
	"errors"

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

	return &ClientSession{
		Ctx: cancelCtx,
		Cancel: cancel,
		ClientAddress: clientAddress,
	}, nil
}

func NewLocalClientSession(ctx context.Context, clientAddress string, byJwt *jwt.ByJwt) *ClientSession {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &ClientSession{
		Ctx: cancelCtx,
		Cancel: cancel,
		ClientAddress: clientAddress,
		ByJwt: byJwt,
	}
}

// either sets `ByJwt` or returns and error
func (self *ClientSession) Auth(req *http.Request) error {
	if auth := req.Header.Get("Authorization"); auth != "" {
    	if strings.HasPrefix(auth, authBearerPrefix) {
    		jwtStr := auth[len(authBearerPrefix):]

    		// to validate the jwt:
    		// 1. parse it which tests the signing key.
    		//    this will fail if the signature is invalid.
    		// 2. test the create time and sessions against
    		//    inactive sessions. For various security reasons sessions may be expired.

			byJwt, err := jwt.ParseByJwt(jwtStr)
			if err != nil {
				return err
			}
			if !jwt.IsByJwtActive(self.Ctx, byJwt) {
				return errors.New("JWT expired.")
			}
	    	bringyour.Logger().Printf("Authed as %s (%s %s)\n", byJwt.UserId, byJwt.NetworkName, byJwt.NetworkId)
	    	self.ByJwt = byJwt
	    	return nil
    	}
    }
    return errors.New("Invalid auth.")
}

func (self *ClientSession) ClientIpPort() (ip string, port int) {
	parts := strings.Split(self.ClientAddress, ":")
	ip = parts[0]
	if 1 < len(parts) {
		port, _ = strconv.Atoi(parts[1])
	}
	return
}


func Testing_CreateClientSession(ctx context.Context, byJwt *jwt.ByJwt) *ClientSession {
	cancelCtx, cancel := context.WithCancel(ctx)

	clientAddress := "0.0.0.0:0"

	return &ClientSession{
		Ctx: cancelCtx,
		Cancel: cancel,
		ClientAddress: clientAddress,
		ByJwt: byJwt,
	}
}