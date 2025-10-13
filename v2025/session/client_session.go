package session

import (
	"context"
	"net/http"
	// "strconv"
	// "crypto/sha256"
	// "net"
	"net/netip"
	// "regexp"
	// "strconv"
	"strings"
	// "sync"

	// "bytes"
	// "fmt"

	// "encoding/base64"
	"errors"

	"github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
)

// https://www.rfc-editor.org/rfc/rfc6750
const authBearerPrefix = "Bearer "

type ClientSession struct {
	Ctx    context.Context
	Cancel context.CancelFunc
	// ip:port
	ClientAddress string
	Header        map[string][]string
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
		Header:        map[string][]string(req.Header),
	}, nil
}

func NewLocalClientSession(ctx context.Context, clientAddress string, byJwt *jwt.ByJwt) *ClientSession {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &ClientSession{
		Ctx:           cancelCtx,
		Cancel:        cancel,
		ClientAddress: clientAddress,
		Header:        map[string][]string{},
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

func (self *ClientSession) ClientIpPort() (string, int, error) {
	return server.SplitClientAddress(self.ClientAddress)
}

func (self *ClientSession) ParseClientIpPort() (ip netip.Addr, port int, err error) {
	var ipStr string
	ipStr, port, err = server.SplitClientAddress(self.ClientAddress)
	if err != nil {
		return
	}
	ip, err = netip.ParseAddr(ipStr)
	return
}

func (self *ClientSession) ClientAddressHashPort() (clientAddressHash [32]byte, clientPort int, err error) {
	var clientIp string
	clientIp, clientPort, err = server.SplitClientAddress(self.ClientAddress)
	if err != nil {
		return
	}
	clientAddressHash, err = server.ClientIpHash(clientIp)
	return
}

func (self *ClientSession) WithByJwt(byJwt *jwt.ByJwt) *ClientSession {
	return &ClientSession{
		Ctx:           self.Ctx,
		Cancel:        self.Cancel,
		ClientAddress: self.ClientAddress,
		Header:        self.Header,
		ByJwt:         byJwt,
	}
}

func Testing_CreateClientSession(ctx context.Context, byJwt *jwt.ByJwt) *ClientSession {
	cancelCtx, cancel := context.WithCancel(ctx)

	clientAddress := "0.0.0.0:0"

	return &ClientSession{
		Ctx:           cancelCtx,
		Cancel:        cancel,
		ClientAddress: clientAddress,
		Header:        map[string][]string{},
		ByJwt:         byJwt,
	}
}
