// Client sessions carry request identity and authentication state through the server.
package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
	"github.com/urnetwork/server/jwt"
)

// https://www.rfc-editor.org/rfc/rfc6750
const authBearerPrefix = "Bearer "

// The Warp ingress overwrites this with the address accepted from the client.
const urForwardedForHeader = "X-UR-Forwarded-For"

// Request-scoped client identity and authentication state.
type ClientSession struct {
	Ctx    context.Context
	Cancel context.CancelFunc
	// ip:port
	ClientAddress string
	// pre-computed peppered address hash + port (see server.ClientIpHash),
	// set instead of ClientAddress when the session is reconstructed from
	// storage that persists only the hash (deferred tasks). The raw address is
	// deliberately absent on such sessions: consumers that need the ip itself
	// (geo lookup, egress parsing) get a parse error, exactly as they would for
	// any unparseable address, while consumers of ClientAddressHashPort keep
	// working against the stored hash.
	clientAddressHash     *[32]byte
	clientAddressHashPort int
	Header                map[string][]string
	ByJwt                 *jwt.ByJwt
}

// Resolves the ingress-owned client address before exposing request state.
func NewClientSessionFromRequest(req *http.Request) (*ClientSession, error) {
	cancelCtx, cancel := context.WithCancel(req.Context())

	clientAddress, err := ResolveClientAddress(req)
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

// Normalizes an ip:port pair, including ipv4-mapped ipv6.
func parseRequestAddress(raw string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		// Older ingress configurations emitted IPv6 without brackets. Split at
		// the last colon only when everything before it is itself valid IPv6;
		// otherwise preserve SplitHostPort's rejection.
		lastColon := strings.LastIndexByte(raw, ':')
		if lastColon <= 0 {
			return netip.AddrPort{}, err
		}
		unbracketedAddr, unbracketedErr := netip.ParseAddr(raw[:lastColon])
		if unbracketedErr != nil || !unbracketedAddr.Is6() {
			return netip.AddrPort{}, err
		}
		host = raw[:lastColon]
		port = raw[lastColon+1:]
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

// Fleet-standard source attribution entry point.
func ResolveClientAddressFromRequest(req *http.Request) (string, error) {
	return ResolveClientAddress(req)
}

// Uses the one header owned by every UR-controlled ingress.
//
// The ingress must overwrite X-UR-Forwarded-For with one ip:port pair from the
// socket it accepted and backend service ports must remain unreachable from
// clients. Standard X-Forwarded-For and X-Forwarded-Source-Port are deliberately
// ignored. Without the UR header, direct and internal requests use RemoteAddr.
func ResolveClientAddress(req *http.Request) (string, error) {
	remote, err := parseRequestAddress(req.RemoteAddr)
	if err != nil {
		return "", fmt.Errorf("invalid remote address %q: %w", req.RemoteAddr, err)
	}

	forwardedValues := req.Header.Values(urForwardedForHeader)
	if len(forwardedValues) == 0 {
		return remote.String(), nil
	}
	if len(forwardedValues) != 1 {
		glog.Errorf(
			"[session]%s must be one ingress-overwritten ip:port value; using peer %s\n",
			urForwardedForHeader,
			remote.Addr(),
		)
		return remote.String(), nil
	}

	forwardedValue := strings.TrimSpace(forwardedValues[0])
	if forwardedValue == "" {
		return remote.String(), nil
	}
	forwarded, err := parseRequestAddress(forwardedValue)
	if err != nil {
		// Do not log the value: a future ingress regression could make it
		// caller-controlled. The peer and fixed header name are actionable.
		glog.Errorf(
			"[session]%s from ingress peer %s was not one ip:port value; using the peer address\n",
			urForwardedForHeader,
			remote.Addr(),
		)
		return remote.String(), nil
	}
	return forwarded.String(), nil
}

// Creates a session for trusted in-process work.
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

// Reconstructs a session from storage
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

// Sets authentication claims or returns an authentication error.
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
				if glog.V(2) {
					glog.Infof("[session]authed via api key as (%s %s)\n", network.NetworkName, network.NetworkId)
				}
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
				if glog.V(2) {
					glog.Infof("[session]authed as %s (%s %s)\n", byJwt.UserId, byJwt.NetworkName, byJwt.NetworkId)
				}
				self.ByJwt = byJwt
				return nil
			}
		}
	}
	return errors.New("Invalid auth.")
}

// Splits the normalized client address.
func (self *ClientSession) ClientIpPort() (string, int, error) {
	return server.SplitClientAddress(self.ClientAddress)
}

// Splits and parses the normalized client address.
func (self *ClientSession) ParseClientIpPort() (ip netip.Addr, port int, err error) {
	var ipStr string
	ipStr, port, err = server.SplitClientAddress(self.ClientAddress)
	if err != nil {
		return
	}
	ip, err = netip.ParseAddr(ipStr)
	return
}

// Returns the privacy-preserving address key and source port.
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

// Returns a session view with updated authentication claims.
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

// Creates deterministic local state for server tests.
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
