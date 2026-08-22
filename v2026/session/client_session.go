package session

import (
	"context"
	"net"
	"net/http"
	"os"

	// "strconv"
	// "crypto/sha256"
	// "net"
	"net/netip"
	// "regexp"
	// "strconv"
	"strings"
	"sync"
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

	clientAddress, err := ResolveClientAddress(req, trustedProxyPrefixes())
	if err != nil {
		cancel()
		return nil, err
	}

	return &ClientSession{
		Ctx:           cancelCtx,
		Cancel:        cancel,
		ClientAddress: clientAddress,
		Header:        map[string][]string(req.Header),
	}, nil
}

const trustedProxyCidrsEnvironment = "BRINGYOUR_TRUSTED_PROXY_CIDRS"

var trustedProxyPrefixes = sync.OnceValue(func() []netip.Prefix {
	raw := strings.TrimSpace(os.Getenv(trustedProxyCidrsEnvironment))
	if raw == "" {
		// The local warp/nginx sidecar is the safe portable default. A remote
		// proxy must be explicitly enumerated by the deployment.
		raw = "127.0.0.0/8,::1/128"
	}
	prefixes, err := ParseTrustedProxyPrefixes(raw)
	if err != nil {
		panic(err)
	}
	return prefixes
})

func ParseTrustedProxyPrefixes(raw string) ([]netip.Prefix, error) {
	parts := strings.Split(raw, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%s contains invalid CIDR %q: %w", trustedProxyCidrsEnvironment, part, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	if len(prefixes) == 0 {
		return nil, fmt.Errorf("%s must contain at least one CIDR", trustedProxyCidrsEnvironment)
	}
	return prefixes, nil
}

func parseRequestAddress(raw string) (netip.AddrPort, error) {
	if addrPort, err := netip.ParseAddrPort(raw); err == nil {
		return addrPort, nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return netip.AddrPort{}, err
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.AddrPort{}, err
	}
	portNumber, err := net.LookupPort("tcp", port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return netip.AddrPort{}, fmt.Errorf("invalid source port %q", port)
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(portNumber)), nil
}

func proxyIsTrusted(remote netip.Addr, trusted []netip.Prefix) bool {
	remote = remote.Unmap()
	for _, prefix := range trusted {
		if prefix.Contains(remote) {
			return true
		}
	}
	return false
}

// ResolveClientAddress honors forwarding headers only when the immediate TCP
// peer is in an explicit trusted CIDR. This makes source attribution an
// application invariant instead of relying solely on an ingress rewrite.
func ResolveClientAddress(req *http.Request, trusted []netip.Prefix) (string, error) {
	remote, err := parseRequestAddress(req.RemoteAddr)
	if err != nil {
		return "", fmt.Errorf("invalid remote address %q: %w", req.RemoteAddr, err)
	}
	if !proxyIsTrusted(remote.Addr(), trusted) {
		return remote.String(), nil
	}

	forwarded := strings.TrimSpace(req.Header.Get("X-UR-Forwarded-For"))
	if forwarded != "" {
		if strings.Contains(forwarded, ",") {
			return "", fmt.Errorf("invalid X-UR-Forwarded-For chain")
		}
		address, err := parseRequestAddress(forwarded)
		if err != nil {
			return "", fmt.Errorf("invalid X-UR-Forwarded-For: %w", err)
		}
		return address.String(), nil
	}

	forwardedIp := strings.TrimSpace(req.Header.Get("X-Forwarded-For"))
	forwardedPort := strings.TrimSpace(req.Header.Get("X-Forwarded-Source-Port"))
	if forwardedIp == "" && forwardedPort == "" {
		return remote.String(), nil
	}
	if forwardedIp == "" || forwardedPort == "" || strings.Contains(forwardedIp, ",") {
		return "", fmt.Errorf("forwarded source requires one IP and one port")
	}
	address, err := parseRequestAddress(net.JoinHostPort(strings.Trim(forwardedIp, "[]"), forwardedPort))
	if err != nil {
		return "", fmt.Errorf("invalid forwarded source: %w", err)
	}
	return address.String(), nil
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
