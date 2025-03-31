package session

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	// "encoding/base64"
	"errors"

	"github.com/golang/glog"

	// "github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
)

// https://www.rfc-editor.org/rfc/rfc6750
const authBearerPrefix = "Bearer "

type ClientSession struct {
	Ctx    context.Context
	Cancel context.CancelFunc
	// ip:port
	ClientAddress string
	ByJwt         *jwt.ByJwt
}

func NewClientSessionFromRequest(req *http.Request) (*ClientSession, error) {
	cancelCtx, cancel := context.WithCancel(req.Context())

	clientAddress := req.Header.Get("X-UR-Forwarded-For")

	if clientAddress == "" {
		clientAddress = req.Header.Get("X-Forwarded-For")
	}

	if clientAddress == "" {
		clientAddress = req.RemoteAddr
	}

	return &ClientSession{
		Ctx:           cancelCtx,
		Cancel:        cancel,
		ClientAddress: clientAddress,
	}, nil
}

func NewLocalClientSession(ctx context.Context, clientAddress string, byJwt *jwt.ByJwt) *ClientSession {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &ClientSession{
		Ctx:           cancelCtx,
		Cancel:        cancel,
		ClientAddress: clientAddress,
		ByJwt:         byJwt,
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
			glog.V(2).Infof("[session]authed as %s (%s %s)\n", byJwt.UserId, byJwt.NetworkName, byJwt.NetworkId)
			self.ByJwt = byJwt
			return nil
		}
	}
	return errors.New("Invalid auth.")
}

func (self *ClientSession) ClientIpPort() (ip string, port int) {
	parts := strings.Split(self.ClientAddress, ":")
	if 1 < len(parts) {
		if port_, err := strconv.Atoi(parts[len(parts)-1]); err == nil && 1024 <= port_ {
			// ipv4 or ipv6 with port
			ip = strings.Join(parts[:len(parts)-1], ":")
			port = port_
		} else {
			// ipv6 without port
			ip = strings.Join(parts, ":")
		}
	} else {
		// ipv4 without port
		ip = parts[0]
	}
	return
}

func Testing_CreateClientSession(ctx context.Context, byJwt *jwt.ByJwt) *ClientSession {
	cancelCtx, cancel := context.WithCancel(ctx)

	clientAddress := "0.0.0.0:0"

	return &ClientSession{
		Ctx:           cancelCtx,
		Cancel:        cancel,
		ClientAddress: clientAddress,
		ByJwt:         byJwt,
	}
}
