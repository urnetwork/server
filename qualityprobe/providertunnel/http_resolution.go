// Provider URL resolution is an explicit bounded phase before target sockets.
package providertunnel

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/urnetwork/connect"
)

// Each independent tunnel supplies its own cache and socket dialer. A URL
// probe request resolves and dials one IP family, its grant's
// (providerUrlProbeTarget): the A record and IPv4 addresses for 4, the AAAA
// record and IPv6 addresses for 6, never the other family's. Every other
// request resolves IPv4 alone, as before families were probed. This does not
// change general Connect resolution.
type providerUrlResolver struct {
	waitRoute           func(context.Context) error
	query               func(context.Context, string, string) ([]netip.Addr, bool)
	dial                func(context.Context, string, string, []netip.Addr) (net.Conn, error)
	observations        *DnsObservations
	pathState           func() dnsPathState
	routeState          func() dnsRouteSnapshot
	contractUnavailable func() bool
	// The URL-probe address policy, publicUrlProbeAddressFor when nil, which
	// is what production leaves it. A test of a loopback origin sets it on its
	// own tunnel's resolver.
	publicAddress func(netip.Addr, int) bool
}

// Whether a URL probe request of the family may be dialed to ip.
func (self *providerUrlResolver) addressAllowed(ip netip.Addr, ipVersion int) bool {
	if self.publicAddress != nil {
		return self.publicAddress(ip, ipVersion)
	}
	return publicUrlProbeAddressFor(ip, ipVersion)
}

// The IP family a request resolves and dials, and whether it is a scoped URL
// probe request: its grant's family, or IPv4 for any other request.
func providerUrlRequestFamily(ctx context.Context) (int, bool) {
	target, scoped := ctx.Value(providerUrlProbeKey{}).(providerUrlProbeTarget)
	if !scoped {
		return 4, false
	}
	return target.family(), true
}

// The network a URL probe request's socket is dialed on: the family-narrowed
// form of network (tcp4 or tcp6, udp4 or udp6), so the tunnel's dial admits no
// address of the other family whatever an answer held. Any other request
// keeps network.
func providerUrlDialNetwork(ctx context.Context, network string) string {
	ipVersion, scoped := providerUrlRequestFamily(ctx)
	if !scoped {
		return network
	}
	switch network {
	case "tcp", "tcp4", "tcp6":
		return "tcp" + strconv.Itoa(ipVersion)
	case "udp", "udp4", "udp6":
		return "udp" + strconv.Itoa(ipVersion)
	}
	return network
}

// Keeps the answers of one family, unmapped: IPv4 for 4, and for 6 the IPv6
// addresses that are not IPv4-mapped, which are IPv4.
func addressesOfFamily(addrs []netip.Addr, ipVersion int) []netip.Addr {
	kept := make([]netip.Addr, 0, len(addrs))
	for _, addr := range addrs {
		addr = addr.Unmap()
		if (ipVersion == 6 && addr.Is6()) || (ipVersion != 6 && addr.Is4()) {
			kept = append(kept, addr)
		}
	}
	return kept
}

// Retries resolver failures before attempting a socket. All attempts share the
// HTTP request's hard deadline; one timeout cannot consume every retry or the
// final connection allowance. Outer sampled-load retries remain independent.
func (self *providerUrlResolver) dialContext(ctx context.Context, network, address string, observation *providerHttpDialTrace) (net.Conn, error) {
	addrs, err := self.resolve(ctx, address, observation)
	if err != nil {
		return nil, err
	}
	return self.dialResolved(ctx, network, address, addrs)
}

// The bounded resolution phase alone: the answers of the request's family a
// target socket may be dialed to (providerUrlRequestFamily), through this
// tunnel's own resolver, every one public in that family when the request is
// a scoped URL probe. Only that family's record is asked for: A for IPv4,
// AAAA for IPv6. The HTTPS leg dials them over TCP and the HTTP/3 leg over
// UDP, so both legs of a URL in one family resolve the same way.
func (self *providerUrlResolver) resolve(ctx context.Context, address string, observation *providerHttpDialTrace) ([]netip.Addr, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ipVersion, scoped := providerUrlRequestFamily(ctx)
	if literal, err := netip.ParseAddr(host); err == nil {
		if scoped && !self.addressAllowed(literal, ipVersion) {
			return nil, &providerHttpStageError{stage: "policy", err: fmt.Errorf("provider URL is a literal that is not a public IPv%d address", ipVersion)}
		}
		return []netip.Addr{literal.Unmap()}, nil
	}
	recordType := "A"
	if ipVersion == 6 {
		recordType = "AAAA"
	}
	observation.observe(connect.TunDialDnsStarted)
	// One URL-only budget covers route waiting, all resolver waves and jitter.
	// It must never become the target TCP/TLS/body deadline.
	resolutionCtx, resolutionCancel := providerUrlPhaseContext(ctx, urlProbeDnsTimeout)
	defer resolutionCancel()
	const attempts = 3
	var addrs []netip.Addr
	var authoritative bool
resolution:
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if resolutionCtx.Err() != nil {
			break
		}
		lookupTimeout := 10 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			// Share the actual request allowance, including a cold tunnel's
			// longer establishment window. A fixed per-wave ceiling would
			// exhaust retries while that window still has usable time left.
			lookupTimeout = time.Until(deadline) / time.Duration(attempts-attempt+1)
		}
		if deadline, ok := resolutionCtx.Deadline(); ok {
			lookupTimeout = min(lookupTimeout, time.Until(deadline))
		}
		if lookupTimeout <= 0 {
			return nil, context.DeadlineExceeded
		}
		// Resolver-internal DoH HTTP/TCP traces must never impersonate progress
		// of the sampled website. Carry cancellation/deadline, not trace values.
		lookupCtx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		stop := context.AfterFunc(resolutionCtx, cancel)
		if resolutionCtx.Err() != nil {
			cancel()
		}
		var waveStart, waveEnd time.Time
		var before, after dnsRouteSnapshot
		if self.observations != nil {
			if self.routeState != nil {
				before = self.routeState()
			}
			waveStart = time.Now()
		}
		if self.waitRoute == nil || self.waitRoute(lookupCtx) == nil {
			addrs, authoritative = self.query(lookupCtx, recordType, host)
		}
		lookupErr := lookupCtx.Err()
		if resolutionCtx.Err() != nil {
			// A late answer cannot reopen an expired phase.
			addrs, authoritative = nil, false
			lookupErr = resolutionCtx.Err()
		}
		if self.observations != nil {
			waveEnd = time.Now()
		}
		// Snapshot before our own lookup cancellation can release a wait.
		// This exact tunnel's monotonic write fence is owned by Connect;
		// registration/admission and separate process counters cannot supply it.
		contractEvidence := 0
		if self.observations != nil && self.contractUnavailable != nil {
			contractEvidence = 2
			if self.contractUnavailable() {
				contractEvidence = 1
			}
		}
		stop()
		cancel()
		if self.observations != nil && self.routeState != nil {
			after = self.routeState()
		}
		addrs = addressesOfFamily(addrs, ipVersion)
		if self.observations != nil {
			result := dnsUnanswered
			switch {
			case ctx.Err() != nil:
				result = dnsCanceled
			case len(addrs) != 0:
				result = dnsAnswer
			case authoritative:
				result = dnsAuthoritativeEmpty
			case errors.Is(lookupErr, context.DeadlineExceeded):
				result = dnsTimeout
			}
			path := dnsPathUnknown
			if self.routeState != nil {
				path = after.path
			} else if self.pathState != nil {
				path = self.pathState()
			}
			self.observations.record(result, path)
			self.observations.recordRouteTiming(result, waveStart, waveEnd, before, after)
			self.observations.recordContract(result, contractEvidence, waveEnd.Sub(waveStart))
		}
		if len(addrs) != 0 || authoritative {
			break
		}
		if attempt+1 < attempts {
			select {
			case <-resolutionCtx.Done():
				if ctx.Err() != nil {
					return nil, context.Cause(ctx)
				}
				break resolution
			case <-time.After(50*time.Millisecond + time.Duration(rand.Int64N(int64(150*time.Millisecond)))):
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if resolutionCtx.Err() != nil {
		return nil, &net.DNSError{Err: "provider URL resolution timed out", Name: host, IsTimeout: true, IsTemporary: true}
	}
	if len(addrs) == 0 {
		return nil, &net.DNSError{Err: "provider URL resolution exhausted", Name: host, IsNotFound: authoritative, IsTemporary: !authoritative}
	}
	if scoped {
		for _, address := range addrs {
			if !self.addressAllowed(address, ipVersion) {
				return nil, &providerHttpStageError{stage: "policy", err: errors.New("provider URL resolved to a nonpublic address")}
			}
		}
	}
	observation.observe(connect.TunDialDnsAnswered)
	return addrs, nil
}

// Starts a fresh socket budget after DNS; it cannot extend a shorter caller
// deadline or affect the successfully returned connection's lifetime. A URL
// probe request dials on its family's network alone (providerUrlDialNetwork).
func (self *providerUrlResolver) dialResolved(ctx context.Context, network, address string, addrs []netip.Addr) (net.Conn, error) {
	dialCtx, cancel := providerUrlPhaseContext(ctx, urlProbeTcpTimeout)
	defer cancel()
	return self.dial(dialCtx, providerUrlDialNetwork(ctx, network), address, addrs)
}
