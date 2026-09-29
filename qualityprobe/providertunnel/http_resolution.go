// Provider URL resolution is an explicit bounded phase before target sockets.
package providertunnel

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"time"

	"github.com/urnetwork/connect"
)

// Each independent tunnel supplies its own cache and socket dialer. Probe
// tunnels are IPv4-only; this does not change general Connect resolution.
type providerUrlResolver struct {
	query        func(context.Context, string, string) ([]netip.Addr, bool)
	dial         func(context.Context, string, string, []netip.Addr) (net.Conn, error)
	observations *DnsObservations
	pathState    func() dnsPathState
	routeState   func() dnsRouteSnapshot
}

// Retries resolver failures before attempting a socket. All attempts share the
// HTTP request's hard deadline; one timeout cannot consume every retry or the
// final connection allowance. Outer sampled-load retries remain independent.
func (self *providerUrlResolver) dialContext(ctx context.Context, network, address string, observation *providerHttpDialTrace) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if literal, err := netip.ParseAddr(host); err == nil {
		if _, scoped := ctx.Value(providerUrlProbeKey{}).(providerUrlProbeTarget); scoped && !publicUrlProbeAddress(literal) {
			return nil, &providerHttpStageError{stage: "policy", err: errors.New("provider URL resolved to a nonpublic address")}
		}
		return self.dial(ctx, network, address, []netip.Addr{literal.Unmap()})
	}
	observation.observe(connect.TunDialDnsStarted)
	const attempts = 3
	var addrs []netip.Addr
	var authoritative bool
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lookupTimeout := 10 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			// Share the actual request allowance, including a cold tunnel's
			// longer establishment window. A fixed per-wave ceiling would
			// exhaust retries while that window still has usable time left.
			lookupTimeout = time.Until(deadline) / time.Duration(attempts-attempt+1)
		}
		if lookupTimeout <= 0 {
			return nil, context.DeadlineExceeded
		}
		// Resolver-internal DoH HTTP/TCP traces must never impersonate progress
		// of the sampled website. Carry cancellation/deadline, not trace values.
		lookupCtx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		stop := context.AfterFunc(ctx, cancel)
		if ctx.Err() != nil {
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
		addrs, authoritative = self.query(lookupCtx, "A", host)
		lookupErr := lookupCtx.Err()
		if self.observations != nil {
			waveEnd = time.Now()
		}
		stop()
		cancel()
		if self.observations != nil && self.routeState != nil {
			after = self.routeState()
		}
		ipv4 := make([]netip.Addr, 0, len(addrs))
		for _, addr := range addrs {
			if addr.Unmap().Is4() {
				ipv4 = append(ipv4, addr.Unmap())
			}
		}
		addrs = ipv4
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
		}
		if len(addrs) != 0 || authoritative {
			break
		}
		if attempt+1 < attempts {
			select {
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			case <-time.After(50*time.Millisecond + time.Duration(rand.Int64N(int64(150*time.Millisecond)))):
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, &net.DNSError{Err: "provider URL resolution exhausted", Name: host, IsNotFound: authoritative, IsTemporary: !authoritative}
	}
	if _, scoped := ctx.Value(providerUrlProbeKey{}).(providerUrlProbeTarget); scoped {
		for _, address := range addrs {
			if !publicUrlProbeAddress(address) {
				return nil, &providerHttpStageError{stage: "policy", err: errors.New("provider URL resolved to a nonpublic address")}
			}
		}
	}
	observation.observe(connect.TunDialDnsAnswered)
	return self.dial(ctx, network, address, addrs)
}
