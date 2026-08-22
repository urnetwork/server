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
	"time"
	// "sync"

	// "bytes"
	"fmt"

	// "encoding/base64"
	"errors"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/apikey"
	"github.com/urnetwork/server/jwt"
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
		// Unmap so a 4-in-6 peer ([::ffff:203.0.113.9]) and the same host
		// arriving as plain ipv4 are one address everywhere downstream. Without
		// this, proxyIsTrusted (which unmaps) and server.ClientIpHashForAddr
		// (which does not) disagree about what host a request came from, and
		// one host holds two per-address budgets and two report slots.
		return netip.AddrPortFrom(addrPort.Addr().Unmap(), addrPort.Port()), nil
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

// forwardingHeaders are the headers ResolveClientAddress honors from a trusted
// peer. Their presence on a request from an UNTRUSTED peer is the
// misconfiguration signal reported below.
var forwardingHeaders = []string{"X-UR-Forwarded-For", "X-Forwarded-For"}

// forwardingHeaderChain is the whole of one forwarding header, as one chain.
//
// Header.Get would return only the FIRST field line, and RFC 9110 makes
// repeated field lines equivalent to one comma-joined line. A proxy that
// appends a second X-Forwarded-For line instead of extending the first is
// entirely legal, and reading only the first line there would hand back a
// value the CLIENT sent while discarding the one the proxy vouched for --
// the exact forgery forwardedClientAddr walks right-to-left to prevent.
func forwardingHeaderChain(req *http.Request, header string) string {
	return strings.TrimSpace(strings.Join(req.Header.Values(header), ","))
}

func forwardingHeaderPresent(req *http.Request) string {
	for _, header := range forwardingHeaders {
		if forwardingHeaderChain(req, header) != "" {
			return header
		}
	}
	return ""
}

// proxyReportInterval bounds how often one peer is named for one condition.
// A wedged deployment stays loud for as long as it is wedged -- these paths
// never stop firing -- while no caller can flood the log.
const proxyReportInterval = time.Minute

// proxyReportedMax bounds the memory the suppression map can take. Any client
// can set a forwarding header on a directly reachable api, so the set of peers
// that reach a report is caller-influenced and must not grow without limit.
const proxyReportedMax = 1024

// proxyReportKind separates the two operator-facing conditions so one does not
// suppress the other for the same peer. They have different remedies.
type proxyReportKind string

const (
	// an UNTRUSTED peer sent a forwarding header: either the deployment forgot
	// to enumerate its ingress proxy, or a client is spoofing
	proxyReportUnenumerated proxyReportKind = "unenumerated-proxy"
	// a TRUSTED peer sent a forwarding header this service cannot read: the
	// client identity is lost and every client behind that proxy collapses onto
	// one address, which is the same fleet-wide-budget failure, just on the
	// other side of the trust boundary
	proxyReportUnusableHeader proxyReportKind = "unusable-forwarding-header"
)

type proxyReportKey struct {
	peer netip.Addr
	kind proxyReportKind
}

var (
	proxyReportMutex  sync.Mutex
	proxyReportedAt   = map[proxyReportKey]time.Time{}
	proxyReportLastAt time.Time
	proxyReportPruned time.Time
	// proxyReportPruneScans counts prune passes so a test can assert the scan
	// stays amortized. The scan runs under the global mutex on a path any
	// caller can trigger, so one scan per request while the map is full would
	// be a contention amplifier aimed at whoever is keeping it full.
	proxyReportPruneScans int
)

// shouldReportProxy rate limits an operator report to once per interval per
// (peer, condition).
//
// The timestamp map is only written when a report is actually emitted, so it
// cannot grow faster than one entry per emitted line, and entries older than
// the interval are pruned once it reaches its cap. Should a burst of distinct
// peers fill it anyway, the report degrades to a single global token per
// interval rather than either going silent or growing without bound.
//
// That global-token tier is the one place a caller who can reach the api
// directly can starve the signal, and it costs them more than proxyReportedMax
// distinct source addresses inside one interval to get there. It is acceptable
// because the failure mode is fewer log lines, never a wrong address: in the
// deployment this exists to diagnose, the wedged proxy is essentially all of
// the traffic, so it wins the token on nearly every pass.
func shouldReportProxy(key proxyReportKey, now time.Time) bool {
	key.peer = key.peer.Unmap()

	proxyReportMutex.Lock()
	defer proxyReportMutex.Unlock()

	if lastAt, seen := proxyReportedAt[key]; seen {
		if now.Sub(lastAt) < proxyReportInterval {
			return false
		}
		proxyReportedAt[key] = now
		proxyReportLastAt = now
		return true
	}

	// Prune at most once per interval.
	//
	// Not because a more frequent scan would find nothing -- entries are
	// written at different times and so age out continuously, and a scan a
	// second later can free one the previous scan could not. Because the scan
	// walks the whole map while holding this process-global mutex, on a path
	// any caller who can reach the api directly can trigger, so an unguarded
	// scan hands the caller who is keeping the map full an O(cap) critical
	// section per request: a contention amplifier built out of the flood guard.
	//
	// The prune is best-effort reclamation, not a correctness mechanism, which
	// is what makes rate-limiting it safe. An entry that lingers past its
	// interval only delays reuse of one slot; memory is bounded by the cap
	// either way, and the tier below keeps the report firing while the map is
	// full, so nothing goes silent while a prune is being deferred.
	if proxyReportedMax <= len(proxyReportedAt) && proxyReportInterval <= now.Sub(proxyReportPruned) {
		proxyReportPruned = now
		proxyReportPruneScans += 1
		for staleKey, lastAt := range proxyReportedAt {
			if proxyReportInterval <= now.Sub(lastAt) {
				delete(proxyReportedAt, staleKey)
			}
		}
	}
	if proxyReportedMax <= len(proxyReportedAt) {
		// the map is full of entries that are all still inside the interval
		if now.Sub(proxyReportLastAt) < proxyReportInterval {
			return false
		}
		proxyReportLastAt = now
		return true
	}

	proxyReportedAt[key] = now
	proxyReportLastAt = now
	return true
}

// resetProxyReports clears the suppression state so one test's report does not
// silence the next.
func resetProxyReports() {
	proxyReportMutex.Lock()
	defer proxyReportMutex.Unlock()
	proxyReportedAt = map[proxyReportKey]time.Time{}
	proxyReportLastAt = time.Time{}
	proxyReportPruned = time.Time{}
	proxyReportPruneScans = 0
}

// glogErrorf is a var so a test can read a report's actual text rather than
// asserting only that some report happened. It is also the single seam the
// report tests drive: nothing stubs the report functions themselves, so the
// rate limiting below is exercised by the same tests that count reports.
var glogErrorf = glog.Errorf

// reportUnenumeratedProxy names a peer that sent a forwarding header this
// service will not honor.
//
// This exists because the failure it reports is otherwise silent and total. If
// the deployment's ingress proxy is not enumerated in
// BRINGYOUR_TRUSTED_PROXY_CIDRS then every request resolves to that proxy's own
// address, every per-address budget in the service collapses into one budget
// for the whole fleet, and the only symptom is legitimate users refused for
// something strangers did. Nothing else in the service can tell that apart from
// real abuse.
//
// The report deliberately does NOT trust the peer. Honoring a forwarding header
// from an unenumerated peer would let any client claim any source address and
// step outside every per-address limit in the service.
//
// The remedy it prints is the whole remedy. An operator who enumerates the
// subnet and stops there must not end up worse off, so the message also states
// what the proxy has to send and, more importantly, what it must NOT do:
// forward a client-supplied value unchanged. The attacker-supplied header
// VALUE is never logged -- only the header name, which is one of two package
// constants.
func reportUnenumeratedProxy(peer netip.Addr, header string, trusted []netip.Prefix) {
	if !shouldReportProxy(proxyReportKey{peer: peer, kind: proxyReportUnenumerated}, time.Now()) {
		return
	}
	glogErrorf(
		"[session]%s from untrusted peer %s was IGNORED. If %s is this deployment's "+
			"ingress proxy then it is missing from %s (currently %v) and EVERY client "+
			"behind it now shares one client address and therefore one rate-limit "+
			"budget -- signup and login will be refused for users who did nothing. "+
			"Add its subnet to %s. That proxy must also OVERWRITE the forwarding "+
			"header with the address of the connection it received, not pass a "+
			"client-supplied value through: send X-Forwarded-For (nginx "+
			"$proxy_add_x_forwarded_for, or any ALB/CloudFront/Cloudflare default), "+
			"optionally with X-Forwarded-Source-Port, or X-UR-Forwarded-For as "+
			"ip:port. This service reads the last entry of the chain, so a proxy "+
			"that appends is safe and a proxy that forwards the client's own value "+
			"unchanged lets that client choose its rate-limit bucket. Enumerate the "+
			"NARROWEST range that contains only proxies: an address inside an "+
			"enumerated CIDR is read as a proxy hop rather than as a client, so any "+
			"real client arriving from inside that range loses its own rate-limit "+
			"bucket and shares the proxy's. If %s is not a "+
			"proxy of this deployment then a client is sending a forwarding header "+
			"it is not entitled to: the header is correctly ignored and no "+
			"configuration change is wanted.\n",
		header,
		peer,
		peer,
		trustedProxyCidrsEnvironment,
		trusted,
		trustedProxyCidrsEnvironment,
		peer,
	)
}

// reportUnusableForwardingHeader names a TRUSTED peer whose forwarding header
// could not be read.
//
// This is the report that keeps the degrade-instead-of-error behaviour in
// ResolveClientAddress honest. Before, an unreadable header from a trusted peer
// returned an error that wrapWithInput and wrap both turn into HTTP 500 for
// every endpoint -- loud, but a total outage. Falling back to the peer address
// instead cannot be wrong (the peer is the address the packets actually came
// from, the most restrictive answer available) but it silently reintroduces the
// exact fleet-wide-budget collapse this file exists to prevent. So it is not
// silent.
//
// The header VALUE is deliberately not logged: it is attacker-influenced on any
// proxy that forwards client headers, and this format string is a constant.
func reportUnusableForwardingHeader(peer netip.Addr, header string, trusted []netip.Prefix) {
	if !shouldReportProxy(proxyReportKey{peer: peer, kind: proxyReportUnusableHeader}, time.Now()) {
		return
	}
	glogErrorf(
		"[session]%s from TRUSTED peer %s could not be read as a client address, so "+
			"the client address fell back to %s itself. Every client behind that peer "+
			"now shares one client address and therefore one rate-limit budget -- "+
			"signup and login will be refused for users who did nothing. The peer is "+
			"trusted by %s (currently %v), so this is the proxy's header, not a "+
			"client's: it must send X-Forwarded-For as one ip per hop (a bare ip is "+
			"fine, X-Forwarded-Source-Port is optional) or X-UR-Forwarded-For as "+
			"ip:port. The value is not logged here because a proxy that forwards "+
			"client headers makes it client-controlled.\n",
		header,
		peer,
		peer,
		trustedProxyCidrsEnvironment,
		trusted,
	)
}

// parseForwardedElement reads one entry of a forwarding header. Real proxies
// disagree about the form: nginx, ALB, CloudFront and Cloudflare all write a
// bare ip into X-Forwarded-For, the warp sidecar writes ip:port into
// X-UR-Forwarded-For, and some proxies write ip:port into X-Forwarded-For for
// ipv4. All four are accepted; a missing port comes back as 0.
//
// ParseAddr is tried before ParseAddrPort deliberately: a bare ipv6 is full of
// colons, and reading "2001:db8::7" as host:port would be wrong.
func parseForwardedElement(raw string) (netip.Addr, uint16, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Addr{}, 0, false
	}
	if addr, err := netip.ParseAddr(raw); err == nil {
		return addr.Unmap(), 0, true
	}
	if addr, err := netip.ParseAddr(strings.Trim(raw, "[]")); err == nil {
		return addr.Unmap(), 0, true
	}
	if addrPort, err := parseRequestAddress(raw); err == nil {
		return addrPort.Addr(), addrPort.Port(), true
	}
	return netip.Addr{}, 0, false
}

// forwardedClientAddr reads the client address a trusted peer vouched for.
//
// The chain is walked RIGHT TO LEFT, skipping entries that are themselves
// trusted proxies, and the first entry that is not a trusted proxy wins. That
// direction is the whole security property. A proxy appends what it observed to
// the right (nginx's $proxy_add_x_forwarded_for, and every managed load
// balancer), so everything to the LEFT of the entry a trusted hop wrote is a
// value the client supplied. Reading left to right -- taking "the original
// client" -- would let any client behind the proxy prepend an address of its
// choosing and pick its own rate-limit bucket. Reading right to left, a client
// that sends "X-Forwarded-For: 6.6.6.6" gets "6.6.6.6, <its real ip>" and is
// attributed to its real ip.
//
// found is false when no entry survives: either every entry is a trusted proxy
// (the client genuinely is one), or an entry could not be parsed at all. The
// two are told apart by malformed, because only the second is worth reporting.
// Walking stops at the first unparseable entry rather than skipping it, because
// an entry that cannot be read cannot be shown to be a trusted hop, and
// stepping over it is exactly the step that walks into client-controlled text.
func forwardedClientAddr(chain string, trusted []netip.Prefix) (addr netip.Addr, port uint16, found bool, malformed bool) {
	elements := strings.Split(chain, ",")
	for i := len(elements) - 1; 0 <= i; i -= 1 {
		elementAddr, elementPort, ok := parseForwardedElement(elements[i])
		if !ok {
			return netip.Addr{}, 0, false, true
		}
		if proxyIsTrusted(elementAddr, trusted) {
			continue
		}
		return elementAddr, elementPort, true, false
	}
	return netip.Addr{}, 0, false, false
}

// forwardedSourcePort reads the optional companion header that carries the
// client's source port for a bare X-Forwarded-For.
//
// It is honored only for a single-hop chain, which is exactly the shape the
// previous implementation required, so the pairing this service already
// documented keeps its meaning and nothing new is inferred from it. On a longer
// chain the port belongs to whichever hop wrote it and there is no way to tell
// which, so it is dropped rather than guessed.
func forwardedSourcePort(req *http.Request, chain string) (uint16, bool) {
	if strings.Contains(chain, ",") {
		return 0, false
	}
	raw := strings.TrimSpace(req.Header.Get("X-Forwarded-Source-Port"))
	if raw == "" {
		return 0, false
	}
	portNumber, err := net.LookupPort("tcp", raw)
	if err != nil || portNumber < 0 || 65535 < portNumber {
		return 0, false
	}
	return uint16(portNumber), true
}

// ResolveClientAddress honors forwarding headers only when the immediate TCP
// peer is in an explicit trusted CIDR. This makes source attribution an
// application invariant instead of relying solely on an ingress rewrite.
//
// A forwarding header this service cannot read is never an error. It used to
// be, and that error became HTTP 500 on every endpoint (router.wrap and
// router.wrapWithInput both turn a session construction failure into a 500), so
// an operator who enumerated a stock nginx -- which sends a bare
// X-Forwarded-For with no X-Forwarded-Source-Port, and appends a comma on the
// second hop -- took the whole api down by following the advice in
// reportUnenumeratedProxy. Both of those shapes are now read correctly; anything
// still unreadable falls back to the peer address, which is where the packets
// actually came from and therefore the most restrictive answer available, and
// is reported loudly rather than served as a 500.
func ResolveClientAddress(req *http.Request, trusted []netip.Prefix) (string, error) {
	remote, err := parseRequestAddress(req.RemoteAddr)
	if err != nil {
		return "", fmt.Errorf("invalid remote address %q: %w", req.RemoteAddr, err)
	}
	if !proxyIsTrusted(remote.Addr(), trusted) {
		// The header is not honored -- see reportUnenumeratedProxy -- but its
		// presence means either the deployment forgot to enumerate its ingress
		// proxy, which silently collapses every per-address budget onto one
		// address, or a client is spoofing. Both are worth saying out loud.
		if header := forwardingHeaderPresent(req); header != "" {
			reportUnenumeratedProxy(remote.Addr(), header, trusted)
		}
		return remote.String(), nil
	}

	header := forwardingHeaderPresent(req)
	if header == "" {
		return remote.String(), nil
	}
	chain := forwardingHeaderChain(req, header)
	addr, port, found, malformed := forwardedClientAddr(chain, trusted)
	if malformed {
		reportUnusableForwardingHeader(remote.Addr(), header, trusted)
		return remote.String(), nil
	}
	if !found {
		// Every entry in the chain is inside an enumerated CIDR. That is
		// usually one of this deployment's own components calling in through
		// the proxy, in which case the peer address is the right answer and
		// there is nothing to report. It can ALSO be a real client whose
		// address happens to fall inside a range the operator enumerated, and
		// that client has just lost its own rate-limit bucket and joined the
		// proxy's -- the shared-budget failure this file exists to prevent, on
		// a narrow path.
		//
		// It is not reported, because the two are indistinguishable from here
		// and the first is ordinary traffic: an alert on every internal call
		// would bury the reports that are actionable. The remedy belongs to the
		// operator and reportUnenumeratedProxy states it -- enumerate the
		// narrowest range that contains only proxies. Note this is the ONE
		// place the new reader is more conservative than the old one, which
		// never checked a forwarded value against the trusted set at all; see
		// TestForwardedAddressInsideAnEnumeratedCidrSharesTheProxyBucket.
		return remote.String(), nil
	}
	if port == 0 {
		if sourcePort, ok := forwardedSourcePort(req, chain); ok {
			port = sourcePort
		}
	}
	return netip.AddrPortFrom(addr, port).String(), nil
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
