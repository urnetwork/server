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
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
)

// https://www.rfc-editor.org/rfc/rfc6750
const authBearerPrefix = "Bearer "

// An API key (apikey) is sent as the bearer: "urn_" and 52 more characters.
const apiKeyTokenPrefix = "urn_"
const apiKeyTokenLength = 56

// The Warp ingress overwrites this with the address accepted from the client.
const urForwardedForHeader = "X-UR-Forwarded-For"

// A DPoP-bound token is sent with this scheme (RFC 9449 §7.1): an admin token
// (§7.8), with its proof in the DPoP header.
const authDpopPrefix = "DPoP "

// One use of a one-command admin token (§7.8, D-TEAM-7): the token verified,
// its DPoP proof checked and its cmd matched to this request's method, path and
// body. The route's model spends the jti in the command's own transaction
// (model.ClaimAdminTokenUseInTx), so a replay returns the stored result and runs
// nothing.
type AdminCommandUse struct {
	NetworkId server.Id
	// the member the token names: the user in a personal network
	MemberId server.Id
	// the token's jti, spent once
	Jti string
	// the token's cmd, which this request's digest matched
	Cmd string
	// the token's scopes
	Scopes []string
	// the token's exp
	ExpireTime time.Time
}

// Verifies an admin token sent as "DPoP <token>" against its request: the
// token, its proof, and its cmd against the request's method, path and body,
// leaving the body readable again. oauth installs it, since admin tokens are
// signed by its adm- key set; a process without one refuses every admin token.
type AdminTokenAuthenticator func(ctx context.Context, req *http.Request, token string) (*AdminCommandUse, error)

var adminTokenAuthenticator atomic.Pointer[AdminTokenAuthenticator]

// Installs the process's admin token check; nil removes it.
func SetAdminTokenAuthenticator(authenticator AdminTokenAuthenticator) {
	if authenticator == nil {
		adminTokenAuthenticator.Store(nil)
		return
	}
	adminTokenAuthenticator.Store(&authenticator)
}

// The routes whose model spends an admin token's jti in the command's
// transaction, by "METHOD path". An admin token reaches no other route, since
// one whose model never spent its jti could run again (v0: POST
// /network/auth-keys).
var adminCommandRoutesLock sync.Mutex
var adminCommandRoutes = map[string]bool{}

// Adds a route whose model spends an admin token's use in its transaction.
func RegisterAdminCommandRoute(method string, path string) {
	adminCommandRoutesLock.Lock()
	defer adminCommandRoutesLock.Unlock()
	adminCommandRoutes[method+" "+path] = true
}

func adminCommandRoute(method string, path string) bool {
	adminCommandRoutesLock.Lock()
	defer adminCommandRoutesLock.Unlock()
	return adminCommandRoutes[method+" "+path]
}

type clientObservationContextKey struct{}

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
	// the host the request was addressed to: the Host header, or the
	// :authority of an h2 or h3 request, as the ingress passed it on. Empty on
	// a session that was not made from a request.
	Host       string
	ClientInfo ClientInfo
	ByJwt      *ByJwt
	// ApiKeyAuthenticated is true when the request authenticated with an API
	// key (urn_…) rather than a signed token. ByJwt is then the network
	// identity the key stands for, built for this request only: the caller
	// holds the key, not a token, so no route may sign ByJwt back to it as one.
	// Only authenticate sets it; a session built any other way never has it.
	ApiKeyAuthenticated bool
	// The id of the API key that authenticated the request, set with
	// ApiKeyAuthenticated. It stays off ByJwt: only a credential that descends
	// from the key through an auth code carries the key's id as a claim, and a
	// client token this session mints must not end with the key.
	ApiKeyId *server.Id
	// OAuth/MCP request identities and admin tokens are unsigned and cannot be
	// renewed as logins.
	UnsignedIdentity bool
	// The admin token use this request authenticated with, nil for every other
	// credential. Only authenticate sets it, with UnsignedIdentity, and ByJwt is
	// then the token's network and member, built for this request only.
	AdminCommand *AdminCommandUse
	// An opaque id for this request, minted when the session is made from a
	// request, which an admin event records (§7.11). It names nothing but the
	// request, and a session not made from a request has none.
	RequestId *server.Id
}

// Drops the request's cookie headers (cookies.go) and resolves the
// ingress-owned client address before exposing request state.
func NewClientSessionFromRequest(req *http.Request) (*ClientSession, error) {
	DropCookies(req)

	cancelCtx, cancel := context.WithCancel(req.Context())

	clientAddress, err := ResolveClientAddress(req)
	if err != nil {
		cancel()
		return nil, err
	}

	requestId := server.NewId()
	value := &ClientSession{
		Ctx:           cancelCtx,
		Cancel:        cancel,
		ClientAddress: clientAddress,
		Header:        map[string][]string(req.Header),
		Host:          req.Host,
		ClientInfo:    ClientInfoFromHeader(req.Header),
		RequestId:     &requestId,
	}
	value.Ctx = context.WithValue(cancelCtx, clientObservationContextKey{}, value)
	return value, nil
}

// The client address (ip:port) that the session of a request resolved, for
// work that runs under the session's context and has no session of its own,
// such as a BeforeStateAnswer. False under any other context, and for a
// session that holds no raw address.
func ClientAddressFromContext(ctx context.Context) (string, bool) {
	clientSession, ok := ctx.Value(clientObservationContextKey{}).(*ClientSession)
	if !ok || clientSession.ClientAddress == "" {
		return "", false
	}
	return clientSession.ClientAddress, true
}

// The id of the request whose session ctx belongs to (ClientSession.RequestId),
// for work that runs under the session's context. False under any other
// context, and for a session not made from a request.
func RequestIdFromContext(ctx context.Context) (server.Id, bool) {
	clientSession, ok := ctx.Value(clientObservationContextKey{}).(*ClientSession)
	if !ok || clientSession == nil || clientSession.RequestId == nil {
		return server.Id{}, false
	}
	return *clientSession.RequestId, true
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
		noteClientAddressSource(clientAddressSourcePeerAbsent, remote)
		return remote.String(), nil
	}
	if len(forwardedValues) != 1 {
		noteClientAddressSource(clientAddressSourcePeerRepeated, remote)
		glog.Errorf(
			"[session]%s must be one ingress-overwritten ip:port value; using peer %s\n",
			urForwardedForHeader,
			remote.Addr(),
		)
		return remote.String(), nil
	}

	forwardedValue := strings.TrimSpace(forwardedValues[0])
	if forwardedValue == "" {
		noteClientAddressSource(clientAddressSourcePeerEmpty, remote)
		return remote.String(), nil
	}
	forwarded, err := parseRequestAddress(forwardedValue)
	if err != nil {
		noteClientAddressSource(clientAddressSourcePeerMalformed, remote)
		// Do not log the value: a future ingress regression could make it
		// caller-controlled. The peer and fixed header name are actionable.
		glog.Errorf(
			"[session]%s from ingress peer %s was not one ip:port value; using the peer address\n",
			urForwardedForHeader,
			remote.Addr(),
		)
		return remote.String(), nil
	}
	noteClientAddressSource(clientAddressSourceUrHeader, remote)
	return forwarded.String(), nil
}

// Creates a session for trusted in-process work.
func NewLocalClientSession(ctx context.Context, clientAddress string, byJwt *ByJwt) *ClientSession {
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
func NewLocalClientSessionWithAddressHash(ctx context.Context, clientAddressHash [32]byte, clientPort int, byJwt *ByJwt) *ClientSession {
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

// Replies to a client credential's request before its client state query.
// It runs with the credential's verified client id after the signature, the
// claims, the session horizon, the session revocation marker and this
// process's remembered state refusals, and returns whether it replied. A
// reply must grant nothing: it may only report again a refusal this process
// recorded for that same client, or refuse the request because that client
// is over its rate limit. No result of the state query is assumed.
type BeforeStateAnswer func(ctx context.Context, clientId server.Id) bool

// The user of a live team membership, false when memberId names none in
// networkId, as in a personal network, whose member_id is its owner's user id,
// or in a binary that does not link MG6.
func teamMemberUser(ctx context.Context, networkId server.Id, memberId server.Id) (userId server.Id, ok bool) {
	if !server.SidecarMg6Linked() {
		return server.Id{}, false
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT user_id
				FROM network_member
				WHERE member_id = $1 AND network_id = $2 AND NOT pending
			`,
			memberId,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&userId))
				ok = true
			}
		})
	})
	return
}

// Sets authentication claims or returns an authentication error. A non-nil
// answer may reply to a client credential instead (see BeforeStateAnswer);
// the request is then not authenticated and ByJwt stays nil.
func (self *ClientSession) authenticate(ctx context.Context, req *http.Request, answer BeforeStateAnswer) (answered bool, returnErr error) {
	// only a resolved API key sets them, below
	self.ApiKeyAuthenticated = false
	self.ApiKeyId = nil
	self.AdminCommand = nil
	if auth := req.Header.Get("Authorization"); auth != "" {
		if strings.HasPrefix(auth, authDpopPrefix) {
			// a one-command admin token (§7.8): only on a route whose model
			// spends its use, and only for the command its cmd names
			if !adminCommandRoute(req.Method, req.URL.Path) {
				return false, errors.New("An admin token cannot call this route.")
			}
			authenticator := adminTokenAuthenticator.Load()
			if authenticator == nil {
				return false, errors.New("Invalid auth.")
			}
			use, err := (*authenticator)(ctx, req, auth[len(authDpopPrefix):])
			if err != nil {
				return false, err
			}
			self.ByJwt = NewByJwt(use.NetworkId, use.MemberId, "", false, false)
			// a team's member_id is no user id: the token acts as the
			// member's user, as that membership (TEAM-M6-02)
			if memberUserId, ok := teamMemberUser(ctx, use.NetworkId, use.MemberId); ok {
				memberId := use.MemberId
				self.ByJwt = NewByJwt(use.NetworkId, memberUserId, "", false, false)
				self.ByJwt.MemberId = &memberId
			}
			self.UnsignedIdentity = true
			self.AdminCommand = use
			if glog.V(2) {
				glog.Infof("[session]authed via an admin token as %s (%s)\n", use.MemberId, use.NetworkId)
			}
			return false, nil
		}
		if strings.HasPrefix(auth, authBearerPrefix) {
			authStr := auth[len(authBearerPrefix):]

			if strings.HasPrefix(authStr, apiKeyTokenPrefix) {
				// handle API KEY authentication

				// The network_client_proxy_model uses a sha1 hash with a local secret to avoid a db lookup on fake api keys.
				// Could use a cryptographic hash like jwt/ncpm could be a done here to mitigate possible abuse.

				if len(authStr) != apiKeyTokenLength {
					return false, errors.New("Invalid API key.")
				}

				network := apikey.GetNetworkByApiKey(authStr, ctx)
				if network == nil {
					return false, errors.New("Invalid API key.")
				}
				self.ByJwt = NewByJwt(
					network.NetworkId,
					network.UserId,
					network.NetworkName,
					false,
					false, // pro mode - for api keys we don't need to thread this for now
				)
				// a team key acts as its creator's membership (TEAM-M6-02)
				self.ByJwt.MemberId = network.MemberId
				self.ApiKeyAuthenticated = true
				self.ApiKeyId = &network.ApiKeyId
				if glog.V(2) {
					glog.Infof("[session]authed via api key as (%s %s)\n", network.NetworkName, network.NetworkId)
				}
				return false, nil

			} else {
				// handle JWT authentication
				// to validate the jwt, parse it, which tests the signing key.
				// this will fail if the signature is invalid.

				byJwt, err := ParseByJwtForAudience(ctx, authStr, ByJwtAudienceApi)
				if err != nil {
					return false, err
				}
				stateCtx := WithStateQuerySource(ctx, sessionStateQuerySource(req))
				if answer != nil && byJwt.ClientId != nil {
					// the checks ValidateByJwtState makes before its query,
					// then the answer, then only the query
					if err := validateByJwtStateBeforeQuery(stateCtx, byJwt, false); err != nil {
						return false, err
					}
					if answer(stateCtx, *byJwt.ClientId) {
						return true, nil
					}
					if err := validateByJwtStateAfterBeforeQuery(stateCtx, byJwt); err != nil {
						return false, err
					}
				} else if err := ValidateByJwtState(stateCtx, byJwt, false); err != nil {
					return false, err
				}
				if glog.V(2) {
					glog.Infof("[session]authed as %s (%s %s)\n", byJwt.UserId, byJwt.NetworkName, byJwt.NetworkId)
				}
				self.ByJwt = byJwt
				self.ObserveAuthenticatedUse()
				return false, nil
			}
		}
	}
	return false, errors.New("Invalid auth.")
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

// Returns a session view with updated authentication claims. The view does not
// carry ApiKeyAuthenticated: byJwt replaces the identity the key stood for.
func (self *ClientSession) WithByJwt(byJwt *ByJwt) *ClientSession {
	return &ClientSession{
		Ctx:                   self.Ctx,
		Cancel:                self.Cancel,
		ClientAddress:         self.ClientAddress,
		clientAddressHash:     self.clientAddressHash,
		clientAddressHashPort: self.clientAddressHashPort,
		Header:                self.Header,
		Host:                  self.Host,
		ClientInfo:            self.ClientInfo,
		ByJwt:                 byJwt,
		RequestId:             self.RequestId,
	}
}

// Creates deterministic local state for server tests.
func Testing_CreateClientSession(ctx context.Context, byJwt *ByJwt) *ClientSession {
	cancelCtx, cancel := context.WithCancel(ctx)

	// tests commonly hand a bare &ByJwt{NetworkId, UserId} literal; tokens
	// minted or derived from it must survive full claims validation
	if byJwt != nil {
		Testing_NormalizeClaims(byJwt)
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
