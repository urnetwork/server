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
	"fmt"

	// "encoding/base64"
	"errors"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/apikey"
	"github.com/urnetwork/server/v2026/jwt"
)

// https://www.rfc-editor.org/rfc/rfc6750
const authBearerPrefix = "Bearer "

type ClientSession struct {
	Ctx    context.Context
	Cancel context.CancelFunc
	// ip:port
	ClientAddress string
	// pre-computed peppered address hash + port (see server.ClientIpHash),
	// set instead of ClientAddress when the session is reconstructed from
	// storage that persists only the hash (deferred tasks). The raw address
	// is deliberately absent on such sessions: consumers that need the ip
	// itself (geo lookup, egress parsing) get a parse error, exactly as they
	// would for any unparseable address, while consumers of
	// ClientAddressHashPort keep working against the stored hash.
	clientAddressHash     *[32]byte
	clientAddressHashPort int
	Header                map[string][]string
	ByJwt                 *jwt.ByJwt
}

func NewClientSessionFromRequest(req *http.Request) (*ClientSession, error) {
	cancelCtx, cancel := context.WithCancel(req.Context())

	clientAddress := req.Header.Get("X-UR-Forwarded-For")

	if clientAddress == "" {
		clientIpStr := req.Header.Get("X-Forwarded-For")
		clientPortStr := req.Header.Get("X-Forwarded-Source-Port")
		if clientIpStr != "" && clientPortStr != "" {
			clientAddress = fmt.Sprintf("%s:%s", clientIpStr, clientPortStr)
		}
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

// NewLocalClientSessionWithAddressHash reconstructs a session from storage
// that persists only the peppered address hash + port (deferred tasks), never
// the raw ip:port. ClientAddress stays empty on the returned session — see
// the field comment on ClientSession.
func NewLocalClientSessionWithAddressHash(ctx context.Context, clientAddressHash [32]byte, clientPort int, byJwt *jwt.ByJwt) *ClientSession {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &ClientSession{
		Ctx:                   cancelCtx,
		Cancel:                cancel,
		clientAddressHash:     &clientAddressHash,
		clientAddressHashPort: clientPort,
		Header:                map[string][]string{},
		ByJwt:                 byJwt,
	}
}

// either sets `ByJwt` or returns and error
func (self *ClientSession) Auth(req *http.Request) error {
	if auth := req.Header.Get("Authorization"); auth != "" {
		if strings.HasPrefix(auth, authBearerPrefix) {
			authStr := auth[len(authBearerPrefix):]

			if strings.HasPrefix(authStr, "urn_") {
				// handle API KEY authentication

				// The network_client_proxy_model uses a sha1 hash with a local secret to avoid a db lookup on fake api keys.
				// Could use a cryptographic hash like jwt/ncpm could be a done here to mitigate possible abuse.

				if len(authStr) != 56 {
					return errors.New("Invalid API key.")
				}

				network := apikey.GetNetworkByApiKey(authStr, self.Ctx)
				if network == nil {
					return errors.New("Invalid API key.")
				}
				self.ByJwt = jwt.NewByJwt(
					network.NetworkId,
					network.UserId,
					network.NetworkName,
					false,
					false, // pro mode - for api keys we don't need to thread this for now
				)
				glog.V(2).Infof("[session]authed via api key as (%s %s)\n", network.NetworkName, network.NetworkId)
				return nil

			} else {
				// handle JWT authentication
				// to validate the jwt, parse it, which tests the signing key.
				// this will fail if the signature is invalid.

				byJwt, err := jwt.ParseByJwtForAudience(self.Ctx, authStr, jwt.ByJwtAudienceApi)
				if err != nil {
					return err
				}
				if err := jwt.ValidateByJwtState(self.Ctx, byJwt, false); err != nil {
					return err
				}
				glog.V(2).Infof("[session]authed as %s (%s %s)\n", byJwt.UserId, byJwt.NetworkName, byJwt.NetworkId)
				self.ByJwt = byJwt
				return nil
			}

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
	// a session reconstructed from hash-only storage carries the hash
	// directly; there is no raw address to derive it from
	if self.clientAddressHash != nil {
		return *self.clientAddressHash, self.clientAddressHashPort, nil
	}
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
		Ctx:                   self.Ctx,
		Cancel:                self.Cancel,
		ClientAddress:         self.ClientAddress,
		clientAddressHash:     self.clientAddressHash,
		clientAddressHashPort: self.clientAddressHashPort,
		Header:                self.Header,
		ByJwt:                 byJwt,
	}
}

func Testing_CreateClientSession(ctx context.Context, byJwt *jwt.ByJwt) *ClientSession {
	cancelCtx, cancel := context.WithCancel(ctx)

	// tests commonly hand a bare &ByJwt{NetworkId, UserId} literal; tokens
	// minted or derived from it must survive full claims validation
	if byJwt != nil {
		jwt.Testing_NormalizeClaims(byJwt)
	}

	clientAddress := "0.0.0.0:0"

	return &ClientSession{
		Ctx:           cancelCtx,
		Cancel:        cancel,
		ClientAddress: clientAddress,
		Header:        map[string][]string{},
		ByJwt:         byJwt,
	}
}
