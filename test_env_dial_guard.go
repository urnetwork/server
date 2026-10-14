// Pins every PostgreSQL and Redis socket of a local test process to the
// endpoints that test-env.sh attested, for the whole run.
//
// test-env.sh validates the launcher's lock, readiness record and hosts-file
// block once, but the clients resolve their service names on every dial. When
// the launcher exits it removes its hosts-file block, and local-pg.bringyour.com
// and local-redis.bringyour.com then resolve through public DNS, which answers
// 127.0.0.1. On a host that runs a production data tunnel
// (xops/bin/by-patch-main-data), loopback is production. The guard resolves each
// dial itself, refuses unless every resolved endpoint is attested, and dials
// only the checked address literals, so a resolver change mid-run can only fail
// closed. A connection that is already open is not re-checked; its peer cannot
// change.
//
// The process guard is chosen once, at startup, from the environment that
// test-env.sh exports: WARP_ENV=local with WARP_TEST_ENV_ATTESTED_ADDRS, or with
// both of the WARP_SERVICE=test and WARP_BLOCK=test markers. A marked process
// without a valid attestation refuses every guarded dial. Every other process,
// production included, has a nil guard, and a nil guard returns the caller's own
// dial and lookup functions, so those dial paths are unchanged.
//
// Only real I/O entry points consult the process guard: the pool constructors
// and the fixture's own probe, lease and owner connections. Pure configuration
// helpers stay unguarded so their hermetic tests do not depend on the
// environment that runs them.
//
// A guard is immutable after construction and safe for concurrent use.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/redis/go-redis/v9"
)

// Carries the comma-separated ip:port endpoints test-env.sh attested.
const testEnvAttestedAddrsEnvName = "WARP_TEST_ENV_ATTESTED_ADDRS"

// Bounds a malformed attestation; test-env.sh attests one endpoint per service.
const testEnvAttestedAddrsMaxCount = 8

// The endpoints one test process may dial. A nil guard means the process is
// not a test process; see the file comment.
type testEnvDialGuard struct {
	// in attestation order, for diagnostics
	attestedAddrPorts []netip.AddrPort
	// set when a test process has no usable attestation; every dial is refused
	attestationErr error
	// resolves a name handed to a guarded dial
	lookupNetIp func(ctx context.Context, network string, host string) ([]netip.Addr, error)
}

// Set once at startup. Tests may swap it to exercise the real constructors;
// atomic access keeps that race-free.
var testEnvProcessDialGuard atomic.Pointer[testEnvDialGuard]

// Chooses this process's guard before any pool can open.
func init() {
	testEnvProcessDialGuard.Store(newTestEnvDialGuard(os.Getenv, newTestEnvironmentProbeResolver().LookupNetIP))
}

// The guard chosen for this process at startup, or nil outside a test process.
func currentTestEnvDialGuard() *testEnvDialGuard {
	return testEnvProcessDialGuard.Load()
}

// Chooses the guard for one environment. Only WARP_ENV=local with the
// attestation or both test-env.sh markers selects a guard; any other
// environment, production included, gets nil.
func newTestEnvDialGuard(
	getenv func(string) string,
	lookupNetIp func(ctx context.Context, network string, host string) ([]netip.Addr, error),
) *testEnvDialGuard {
	if getenv("WARP_ENV") != "local" {
		return nil
	}
	attestation := getenv(testEnvAttestedAddrsEnvName)
	if attestation == "" && (getenv("WARP_SERVICE") != "test" || getenv("WARP_BLOCK") != "test") {
		return nil
	}
	attestedAddrPorts, err := parseTestEnvAttestedAddrPorts(attestation)
	if err == nil {
		err = requireTestEnvLoopbackAttestationMode(attestedAddrPorts, getenv)
	}
	if err != nil {
		return &testEnvDialGuard{
			attestationErr: err,
			lookupNetIp:    lookupNetIp,
		}
	}
	return &testEnvDialGuard{
		attestedAddrPorts: attestedAddrPorts,
		lookupNetIp:       lookupNetIp,
	}
}

// Reads the attested endpoints. Each host must be an address literal: a name
// would bring back the per-dial resolution that the attestation pins.
func parseTestEnvAttestedAddrPorts(attestation string) ([]netip.AddrPort, error) {
	if attestation == "" {
		return nil, fmt.Errorf(
			"%s is not set; start ./local/run-local.sh, wait for 'Local environment is up', then source ./test-env.sh",
			testEnvAttestedAddrsEnvName,
		)
	}
	endpoints := strings.Split(attestation, ",")
	if testEnvAttestedAddrsMaxCount < len(endpoints) {
		return nil, fmt.Errorf(
			"%s lists %d endpoints; at most %d are allowed",
			testEnvAttestedAddrsEnvName,
			len(endpoints),
			testEnvAttestedAddrsMaxCount,
		)
	}
	attestedAddrPorts := make([]netip.AddrPort, 0, len(endpoints))
	for _, endpoint := range endpoints {
		addrPort, err := netip.ParseAddrPort(endpoint)
		addr := addrPort.Addr()
		if err != nil || addrPort.Port() == 0 || addr.Zone() != "" || addr.Is4In6() ||
			addr.IsUnspecified() || addr.IsMulticast() {
			return nil, fmt.Errorf("%s has an invalid endpoint %q; want ip:port", testEnvAttestedAddrsEnvName, endpoint)
		}
		attestedAddrPorts = append(attestedAddrPorts, addrPort)
	}
	return attestedAddrPorts, nil
}

// Loopback is where a production tunnel listens. Only the explicit
// portable-services mode, which names its 127.0.0.1 endpoints itself, may
// attest it; the managed-local launcher and the suite proxy never do.
func requireTestEnvLoopbackAttestationMode(attestedAddrPorts []netip.AddrPort, getenv func(string) string) error {
	portable := getenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES") == "1" &&
		getenv("WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES") == "1"
	for _, addrPort := range attestedAddrPorts {
		if addrPort.Addr().IsLoopback() && !portable {
			return fmt.Errorf(
				"%s attests loopback endpoint %s outside the explicit portable-services mode",
				testEnvAttestedAddrsEnvName,
				addrPort,
			)
		}
	}
	return nil
}

// Pins a fixture's own Redis client to the launcher attestation in a test
// process. The fixture clients use logical db 0, so a connection never sends
// SELECT: a redial that reached a production cluster would initialize and run
// its commands there. Without a guard the client keeps go-redis's own dialer.
func configureTestEnvRedisDial(guard *testEnvDialGuard, options *redis.Options, dial redisDialFunc) {
	if guard == nil {
		return
	}
	options.Dialer = guard.wrapDial("redis", dial)
}

// Integration fixtures create and drop databases, so they need a guard with a
// valid attestation: a process that test-env.sh qualified against its launcher.
func (self *testEnvDialGuard) requireAttested() error {
	if self == nil {
		return fmt.Errorf(
			"integration tests need the launcher attestation (%s) that ./test-env.sh exports; run ./test.sh or source ./test-env.sh",
			testEnvAttestedAddrsEnvName,
		)
	}
	return self.attestationErr
}

// Returns dial itself for a nil guard. Otherwise every dial resolves its
// address once, refuses unless each resolved endpoint is attested, and then
// dials only those address literals, in resolver order.
func (self *testEnvDialGuard) wrapDial(
	service string,
	dial func(ctx context.Context, network string, address string) (net.Conn, error),
) func(ctx context.Context, network string, address string) (net.Conn, error) {
	if self == nil {
		return dial
	}
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		addrPorts, err := self.resolveAttested(ctx, service, network, address)
		if err != nil {
			return nil, err
		}
		dialErrs := []error{}
		for _, addrPort := range addrPorts {
			conn, err := dial(ctx, network, addrPort.String())
			if err == nil {
				return conn, nil
			}
			dialErrs = append(dialErrs, err)
		}
		return nil, errors.Join(dialErrs...)
	}
}

// Returns lookup itself for a nil guard. Otherwise a name may resolve only to
// attested addresses, so a refusal names the host; the dial guard still checks
// each endpoint's port.
func (self *testEnvDialGuard) wrapLookup(
	service string,
	lookup func(ctx context.Context, host string) ([]string, error),
) func(ctx context.Context, host string) ([]string, error) {
	if self == nil {
		return lookup
	}
	return func(ctx context.Context, host string) ([]string, error) {
		if self.attestationErr != nil {
			return nil, self.unattested(service, host)
		}
		addresses, err := lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("test environment refused %s lookup: %s resolved to no address", service, host)
		}
		for _, address := range addresses {
			addr, err := netip.ParseAddr(address)
			if err != nil {
				return nil, self.refusal(service, host, address, false)
			}
			addr = addr.Unmap()
			attested := slices.ContainsFunc(self.attestedAddrPorts, func(addrPort netip.AddrPort) bool {
				return addrPort.Addr() == addr
			})
			if !attested {
				return nil, self.refusal(service, host, address, addr.IsLoopback())
			}
		}
		return addresses, nil
	}
}

// Resolves one dial address to the attested endpoints it may use, or refuses.
func (self *testEnvDialGuard) resolveAttested(
	ctx context.Context,
	service string,
	network string,
	address string,
) ([]netip.AddrPort, error) {
	if self.attestationErr != nil {
		return nil, self.unattested(service, address)
	}
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf(
			"test environment refused %s dial to %s over %s; only tcp to an attested endpoint is allowed",
			service,
			address,
			network,
		)
	}
	host, portString, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("test environment refused %s dial to %q: %w", service, address, err)
	}
	port, err := strconv.ParseUint(portString, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("test environment refused %s dial to %q: invalid port", service, address)
	}
	resolve := func() ([]netip.Addr, error) {
		if addr, err := netip.ParseAddr(host); err == nil {
			return []netip.Addr{addr}, nil
		}
		return self.lookupNetIp(ctx, "ip", host)
	}
	addrs, err := resolve()
	if err != nil {
		return nil, fmt.Errorf("test environment refused %s dial: could not resolve %s: %w", service, host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("test environment refused %s dial: %s resolved to no address", service, host)
	}
	addrPorts := make([]netip.AddrPort, 0, len(addrs))
	for _, addr := range addrs {
		addrPort := netip.AddrPortFrom(addr.Unmap(), uint16(port))
		if !slices.Contains(self.attestedAddrPorts, addrPort) {
			return nil, self.refusal(service, host, addrPort.String(), addrPort.Addr().IsLoopback())
		}
		addrPorts = append(addrPorts, addrPort)
	}
	return addrPorts, nil
}

// Names the host, what it resolved to and the attestation. Loopback also gets
// the reason it is never taken for the local stack.
func (self *testEnvDialGuard) refusal(service string, host string, resolved string, loopback bool) error {
	reason := ""
	if loopback {
		reason = "; public DNS answers the local service names with 127.0.0.1, where a production data tunnel can listen"
	}
	attested := make([]string, 0, len(self.attestedAddrPorts))
	for _, addrPort := range self.attestedAddrPorts {
		attested = append(attested, addrPort.String())
	}
	return fmt.Errorf(
		"test environment refused %s dial: %s resolved to %s, which the launcher did not attest (attested %s)%s; "+
			"restart ./local/run-local.sh, wait for 'Local environment is up', and source ./test-env.sh again",
		service,
		host,
		resolved,
		strings.Join(attested, ", "),
		reason,
	)
}

// Refuses every dial of a test process whose attestation is missing or invalid.
func (self *testEnvDialGuard) unattested(service string, target string) error {
	return fmt.Errorf("test environment refused %s dial to %s: %w", service, target, self.attestationErr)
}
