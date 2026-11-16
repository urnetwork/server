// Package confinement verifies at startup that this process cannot reach a
// probe destination directly.
//
// The prober's entire guarantee is that every probe request egresses through
// a provider, so a destination sees the provider's address and never the
// operator's, and what the prober measures is the provider's traffic, not its
// own host's. That is enforced outside this process -- a restricted network
// under docker compose, systemd IPAddressDeny/IPAddressAllow otherwise -- and
// the mechanism differs per deployment.
//
// Rather than inspect a mechanism it cannot portably know, the prober tests the
// property: it attempts a direct connection and refuses to run if one succeeds.
// Operator configuration therefore stops being an assumption and becomes a
// precondition.
//
// The governing rule everywhere in this package is that inability to verify is
// not evidence of confinement. Every way the check could come out "passed"
// without having obtained real evidence -- no addresses, no resolvable host, a
// timeout too short for a connection to have completed, a hostname standing in
// for an address that could not be resolved -- is an error, not a pass. A check
// that cannot learn anything must refuse to run rather than report success.
//
// This is a precondition check, not the enforcement. The Go-level enforcement
// (every request is issued on an http.Client bound to a provider tunnel, and
// providertunnel refuses any host outside its allowlist) stays exactly as
// it was; this only refuses to start when the outer confinement that backs it
// is absent.
package confinement

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// The smallest per-address timeout Verify accepts.
//
// The check reads a failed dial as "the packet did not get through", which is
// only sound if the budget was long enough that a connection could plausibly
// have completed within it. Below that, every dial fails because the clock ran
// out rather than because anything blocked it, and the check reports success
// having tested nothing -- the same vacuous pass as a zero timeout, reached by
// a smaller number. 500ms is comfortably above a loopback or same-region
// handshake and still short enough that three hosts cost under two seconds at
// startup.
const MinTimeout = 500 * time.Millisecond

// Reports that a direct connection succeeded.
var ErrNotConfined = errors.New("confinement: a direct connection to a probe address succeeded; this process is not confined")

// Reports an empty address list, which would make the check
// vacuous.
var ErrNoAddresses = errors.New("confinement: at least one address is required")

// Reports that hosts were offered but not one of them resolved,
// so no direct connection could be attempted and the check learned nothing.
//
// This is distinct from ErrNotConfined: it does not say the process is
// unconfined, it says the check cannot tell. Both refuse to start, because a
// check that obtained no evidence must not be reported as a pass.
var ErrNoEvidence = errors.New("confinement: not one probe host could be resolved, so no direct connection was attempted and the check has no evidence this process is confined; allow dns resolution for this process, or supply the addresses to dial explicitly")

// Reports an empty host list passed to Addresses. Same defect as
// ErrNoAddresses, one step earlier.
var ErrNoHosts = errors.New("confinement: at least one host is required")

// Reports a nil dial function.
var ErrNoDialer = errors.New("confinement: a dial function is required")

// Reports a nil lookup function. Addresses does not quietly
// substitute the real resolver: this package's whole job is refusing to
// proceed on an assumption, and resolving through a resolver the caller never
// passed is exactly that. Verify rejects a nil dialer for the same reason.
var ErrNoLookup = errors.New("confinement: a lookup function is required")

// Reports that the caller's context was cancelled or expired
// while the check ran. Every remaining dial then fails in microseconds with
// a context error, which is indistinguishable at the dial site from a
// refusal -- so counting those dials would produce a vacuous pass on a host
// that might have full egress. An interrupted check refuses instead.
var ErrInterrupted = errors.New("confinement: the check was interrupted before it finished; an interrupted check is not evidence of confinement")

// Reports a per-address timeout below MinTimeout. A budget
// that expires before a connection could have completed makes every dial fail
// for a reason unrelated to confinement, and the check passes vacuously. A
// zero or negative duration is the extreme case: context.WithTimeout produces
// an already-expired context.
var ErrInvalidTimeout = errors.New("confinement: the per-address timeout is too short to be evidence of confinement")

// The signature of net.Dialer.DialContext.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// The signature of net.Resolver.LookupHost.
type LookupFunc func(ctx context.Context, host string) ([]string, error)

// Returns nil only when every address refuses a direct connection.
//
// A dial error is the expected, healthy outcome. A successful connection means
// the confinement is missing and returns ErrNotConfined. A timeout counts as
// refused: a dropped packet is what a deny rule looks like from inside.
//
// unresolved carries the hosts Addresses could not resolve. They are not
// dialed -- a bare hostname handed to the production dialer resolves through
// the same resolver that just failed, so it is a guaranteed failure carrying no
// signal -- but they are not ignored either: when they are the only thing the
// caller had, addrs is empty and Verify returns ErrNoEvidence rather than nil.
// That is the deny-all deployment where dns is blocked too, in which the old
// hostname fallback made the check verify nothing and always pass.
//
// Every address is attempted even after one refuses, because partial
// confinement -- one allow rule too many, one endpoint added to the table
// after the firewall was written -- is the realistic failure, not a wholesale
// absence.
func Verify(ctx context.Context, dial DialFunc, addrs []string, unresolved []string, timeout time.Duration) error {
	if dial == nil {
		return ErrNoDialer
	}
	if timeout < MinTimeout {
		return fmt.Errorf("%w (got %s, minimum %s): every dial would expire before a connection could complete, so each address would look blocked whether or not it is", ErrInvalidTimeout, timeout, MinTimeout)
	}
	if len(addrs) == 0 {
		if 0 < len(unresolved) {
			return fmt.Errorf("%w (unresolved: %s)", ErrNoEvidence, strings.Join(unresolved, " "))
		}
		return ErrNoAddresses
	}
	for _, addr := range addrs {
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		conn, err := dial(attemptCtx, "tcp", addr)
		cancel()
		if err == nil {
			if conn != nil {
				conn.Close()
			}
			return fmt.Errorf("%w: %s", ErrNotConfined, addr)
		}
		// The dial failed -- but if it failed for the caller's reason rather
		// than the network's, it is not evidence. Counting it as "refused"
		// would let a cancelled or short-deadlined run pass having tested
		// nothing, through a path the MinTimeout floor cannot see (the floor
		// validates the parameter, not the context it nests in).
		//
		// The error must actually carry the parent's cause: a context that
		// dies in the window after a genuine refusal came back is a check
		// that did finish, and discarding its real evidence would report
		// "interrupted before it finished" about a run that was not.
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return fmt.Errorf("%w (%s, while dialing %s)", ErrInterrupted, ctxErr, addr)
		}
	}
	return nil
}

// Resolves hosts into the dialable "ip:port" addresses Verify should
// test, de-duplicated and in host order, and separately reports the hosts that
// could not be resolved.
//
// Only genuine ip literals are returned. A host that will not resolve is never
// emitted as a bare "host:port": handing that to the production dialer just
// resolves it through the resolver that already failed, so the dial fails at
// resolution and proves nothing about whether the address behind the name is
// reachable. Under a deny-all confinement, where dns is blocked too, every host
// takes that path -- so the old fallback turned the check into a guaranteed
// pass that tested nothing, in precisely the deployment it was written for.
//
// Unresolved hosts are returned to the caller instead, which must treat them as
// a gap in coverage: a warning when some hosts did resolve, and a refusal to
// start when none did (Verify returns ErrNoEvidence). The remedy for a jail
// that legitimately cannot resolve is to supply the addresses explicitly and
// skip resolution altogether.
func Addresses(ctx context.Context, lookup LookupFunc, hosts []string, port string) (addrs []string, unresolved []string, err error) {
	if len(hosts) == 0 {
		return nil, nil, ErrNoHosts
	}
	if lookup == nil {
		return nil, nil, ErrNoLookup
	}

	// Reports whether ip is RFC1918 / ULA / CGNAT space. These are global
	// unicast, so IsGlobalUnicast alone does not exclude them, and a public
	// probe destination never legitimately resolves to one.
	//
	// They matter in the dangerous direction, not the vacuous one. AdGuard
	// Home's "Custom IP" blocking mode and most corporate split-horizon
	// resolvers answer a blocked or internal name with a LAN address rather
	// than 0.0.0.0. If the router's admin UI happens to listen on 443, the dial
	// succeeds, and the prober exits refusing to start -- "a direct connection
	// to a probe address succeeded" -- on a host that is correctly confined.
	// That is a false accusation that reads like a real one, and it takes the
	// deployment down.
	isSiteLocal := func(ip net.IP) bool {
		if ip.IsPrivate() { // RFC1918 and ULA (fc00::/7)
			return true
		}
		// CGNAT (100.64.0.0/10). Not private by Go's definition, equally
		// never a public probe destination.
		if v4 := ip.To4(); v4 != nil {
			return v4[0] == 100 && 64 <= v4[1] && v4[1] <= 127
		}
		return false
	}

	seen := map[string]bool{}
	addrs = make([]string, 0, len(hosts))
	add := func(addr string) {
		if !seen[addr] {
			seen[addr] = true
			addrs = append(addrs, addr)
		}
	}

	for _, host := range hosts {
		ips, err := lookup(ctx, host)
		resolved := false
		if err == nil {
			for _, ip := range ips {
				// A record that is not an ip literal cannot serve as evidence:
				// it would just be re-resolved at dial time.
				ipAddr := net.ParseIP(ip)
				if ipAddr == nil {
					continue
				}
				// A record outside global-unicast space is not the host's
				// address either. Filtering resolvers (Pi-hole, AdGuard,
				// NextDNS, corporate DNS) answer 0.0.0.0 or :: for a blocked
				// name, and loopback/link-local/multicast records are equally
				// incapable of standing in for an internet host. Dialing one
				// produces evidence about this machine, not about the
				// confinement: on Linux 0.0.0.0 connects to loopback, so with
				// nothing listening the refusal reads as a vacuous pass, and
				// with a local service on the port it reads as ErrNotConfined
				// with a wildly misleading diagnosis.
				if !ipAddr.IsGlobalUnicast() || isSiteLocal(ipAddr) {
					continue
				}
				add(net.JoinHostPort(ip, port))
				resolved = true
			}
		}
		if !resolved {
			unresolved = append(unresolved, host)
		}
		// A dead context here is the same defect Verify refuses on, one step
		// earlier. The resolution budget is shared across all hosts, so a
		// resolver that hangs on the first one leaves every remaining host
		// failing instantly for the caller's reason and landing in
		// unresolved -- and the caller then logs a degraded warning and
		// proceeds, having proven confinement for whatever handful resolved
		// before the clock ran out. That is a pass obtained by not looking.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, fmt.Errorf("%w (%s, after resolving %d of %d host(s))", ErrInterrupted, ctxErr, len(addrs), len(hosts))
		}
	}
	return addrs, unresolved, nil
}
