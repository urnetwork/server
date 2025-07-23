package session

import (
	"context"
	"net/http"
	// "strconv"
	"crypto/sha256"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"

	// "bytes"
	"fmt"

	// "encoding/base64"
	"errors"

	"github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
)

var clientIpHashPepper = sync.OnceValue(func() []byte {
	clientKeys := server.Vault.RequireSimpleResource("client.yml")
	pepper := clientKeys.RequireString("client_ip_hash_pepper")
	return []byte(pepper)
})

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

func (self *ClientSession) ClientIpPort() (string, int, error) {
	return SplitClientAddress(self.ClientAddress)
}

func (self *ClientSession) ClientAddressHashPort() (clientAddressHash []byte, clientPort int, err error) {
	var clientIp string
	clientIp, clientPort, err = SplitClientAddress(self.ClientAddress)
	if err != nil {
		return
	}
	clientAddressHash, err = ClientIpHash(clientIp)
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

func ClientIpHash(clientIp string) ([]byte, error) {
	parsedAddr, err := netip.ParseAddr(clientIp)
	if err != nil {
		return nil, err
	}

	h := sha256.New()
	h.Write(parsedAddr.AsSlice())
	h.Write(clientIpHashPepper())
	clientIpHash := h.Sum(nil)
	return clientIpHash, nil
}

func SplitClientAddress(clientAddress string) (host string, port int, err error) {
	columnCount := strings.Count(clientAddress, ":")
	bracketCount := strings.Count(clientAddress, "[")

	var portStr string
	if 1 < columnCount && bracketCount == 0 {
		// malformed ipv6. extract the address from the address:port string
		groups := malformedIPV6WithPort.FindStringSubmatch(clientAddress)
		if len(groups) != 3 {
			err = fmt.Errorf("Could not split malformed ipv6 client address.")
		} else {
			host = groups[1]
			portStr = groups[2]
		}
	} else {
		host, portStr, err = net.SplitHostPort(clientAddress)
	}
	if err != nil {
		// the client address might be just an ip
		_, parsedErr := netip.ParseAddr(clientAddress)
		if parsedErr == nil {
			host = clientAddress
			port = 0
			err = nil
		}
		return
	}
	port, err = strconv.Atoi(portStr)
	return
}

// matches the first group to the IPV6 address when the input is <ipv6>:<port>
// example: 2001:5a8:4683:4e00:3a76:dcec:7cb:f180:40894
var malformedIPV6WithPort = regexp.MustCompile(`^(.+):(\d+)$`)
