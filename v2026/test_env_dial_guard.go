// Pins every PostgreSQL and Redis socket of a local process to the endpoints
// its launcher attested, for the whole run.
//
// The launcher (local/run-local.sh) maps local-pg.bringyour.com and
// local-redis.bringyour.com to its dedicated address in /etc/hosts while it
// runs, but the clients resolve their service names on every dial. When the
// launcher exits it removes its hosts-file block, and the names then resolve
// through whatever answers next: public DNS answered 127.0.0.1, and a stale
// cache, a hosts edit or a localhost default can do the same. On a host that
// runs a production data tunnel (xops/bin/by-patch-main-data), loopback is
// production. The guard resolves each dial itself, refuses unless every
// resolved endpoint is attested, and dials only the checked address literals,
// so a resolver change mid-run can only fail closed. A connection that is
// already open is not re-checked; its peer cannot change.
//
// Every local process is guarded: tests, the cli/*/run-local.sh services,
// bringyourctl and sim-latency alike, with WARP_ENV=local in any case or a
// local vault home and no WARP_ENV (testEnvLocalEnvironment). The attestation is
// WARP_TEST_ENV_ATTESTED_ADDRS, which test-env.sh exports after validating the
// launcher; local/local-env.sh exports the same attestation for a process that
// is not a test, and the sim-latency evaluator container attests its own
// Compose services. A local process without a valid attestation refuses every
// guarded dial. Every other process, production included, has a nil guard, and
// a nil guard returns the caller's own dial and lookup functions, so those dial
// paths are unchanged.
//
// A test binary, and any process that starts with WARP_ENV set, chooses its
// guard once at startup: a later environment change (a test's t.Setenv, or a
// write in main) can neither remove a local guard nor install one in a
// WARP_ENV=main process. Only a binary that starts without WARP_ENV chooses at
// its first guarded construction: its main may still default WARP_ENV=local,
// as sim-latency does after package initialization.
//
// Only real I/O entry points consult the process guard: the pool constructors,
// the fixture's own probe, lease and owner connections, connections that tools
// configure themselves (ConfigureLocalPgDial), and the destructive local
// endpoint check. Pure configuration helpers stay unguarded so their hermetic
// tests do not depend on the environment that runs them.
//
// A test may also point the pools at an in-process loopback proxy. It admits
// that proxy's own live listener with Testing_AdmitTestEnvLoopbackListener and
// dials the proxy's upstream with Testing_DialTestEnv, so the proxy too reaches
// only attested endpoints.
//
// A guard's attestation is immutable after construction; its admissions are
// guarded by stateLock. A guard is safe for concurrent use.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
)

// Carries the comma-separated ip:port endpoints the launcher attestation names.
const testEnvAttestedAddrsEnvName = "WARP_TEST_ENV_ATTESTED_ADDRS"

// Bounds a malformed attestation; test-env.sh attests one endpoint per service.
const testEnvAttestedAddrsMaxCount = 8

// The endpoints one local process may dial. A nil guard means the process is
// not a local process; see the file comment.
type testEnvDialGuard struct {
	// in attestation order, for diagnostics
	attestedAddrPorts []netip.AddrPort
	// set when a local process has no usable attestation; every dial is refused
	attestationErr error
	// the private portable root, the only mode whose attestation may be loopback
	privateRoot bool
	// resolves a name handed to a guarded dial
	lookupNetIp func(ctx context.Context, network string, host string) ([]netip.Addr, error)

	stateLock sync.Mutex
	// in-process loopback listeners admitted by running tests
	admittedAddrPortListeners map[netip.AddrPort]*net.TCPListener
}

// The loopback ports a production data tunnel forwards
// (xops/bin/by-patch-main-data): PostgreSQL, PgBouncer and the Redis cluster
// nodes. No loopback endpoint on them is ever admitted.
func testEnvLoopbackTunnelPort(port uint16) bool {
	return port == 5432 || port == 6432 || (6379 <= port && port <= 6427)
}

// Reports whether a loopback endpoint on port could be the production data
// tunnel, for fixtures in other packages that validate their own explicit
// loopback endpoints.
func Testing_LoopbackTunnelPort(port uint16) bool {
	return testEnvLoopbackTunnelPort(port)
}

// A closed listener's endpoint may already belong to another process, so an
// admission holds only while its listener is open.
func testEnvListenerLive(listener *net.TCPListener) bool {
	rawConn, err := listener.SyscallConn()
	if err != nil {
		return false
	}
	return rawConn.Control(func(uintptr) {}) == nil
}

// Admits one live loopback listener that this test process owns, such as an
// in-process proxy the PostgreSQL or Redis pools are pointed at, as a dial
// target until the test ends. Holding the open listener is the proof of
// ownership: only its exact ip:port is admitted, only while it stays open, and
// never on a production tunnel port. Outside a local process this does
// nothing.
func Testing_AdmitTestEnvLoopbackListener(t testing.TB, listener net.Listener) {
	t.Helper()
	guard := currentTestEnvDialGuard()
	if guard == nil {
		return
	}
	remove, err := guard.admit(listener)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
}

// Dials a fixture's own upstream PostgreSQL or Redis endpoint, as an in-process
// proxy forwarding to the fixture does, through the process guard, so the proxy
// reaches only attested endpoints. Outside a local process it is an ordinary
// dial.
func Testing_DialTestEnv(ctx context.Context, service string, network string, address string) (net.Conn, error) {
	return currentTestEnvDialGuard().wrapDial(service, (&net.Dialer{}).DialContext)(ctx, network, address)
}

// The process guard once chosen. The choice, not the guard, is nil until then;
// a chosen nil guard is the production case.
type testEnvDialGuardChoice struct {
	guard *testEnvDialGuard
}

// Chosen once; see the file comment. Tests may swap it to exercise the real
// constructors; atomic access keeps that race-free.
var testEnvProcessDialGuard atomic.Pointer[testEnvDialGuardChoice]

// Chooses this process's guard before any pool can open, unless the choice
// must wait for main.
func init() {
	testEnvProcessDialGuard.Store(chooseTestEnvStartupDialGuard(testing.Testing(), os.Getenv("WARP_ENV"), newTestEnvProcessDialGuard))
}

// The guard for this process's environment as it stands now. Its resolver is
// the fixture's files-first resolver, bounded like a PostgreSQL lookup: pgconn
// resolves before its connect timeout applies.
func newTestEnvProcessDialGuard() *testEnvDialGuard {
	resolver := newTestEnvironmentProbeResolver()
	lookupNetIp := func(ctx context.Context, network string, host string) ([]netip.Addr, error) {
		lookupCtx, cancel := context.WithTimeout(ctx, PgConnectTimeout)
		defer cancel()
		return resolver.LookupNetIP(lookupCtx, network, host)
	}
	return newTestEnvDialGuard(os.Getenv, lookupNetIp)
}

// The startup choice. A test binary, and a process that starts with WARP_ENV
// set, chooses now: a local process gets its guard, and any other, such as a
// WARP_ENV=main service, locks in nil. Only a process that starts without
// WARP_ENV defers to its first guarded construction, returning nil.
func chooseTestEnvStartupDialGuard(testBinary bool, warpEnv string, newGuard func() *testEnvDialGuard) *testEnvDialGuardChoice {
	guard := newGuard()
	if guard == nil && !testBinary && warpEnv == "" {
		// main may still default WARP_ENV=local (sim-latency)
		return nil
	}
	return &testEnvDialGuardChoice{guard: guard}
}

// The guard chosen for this process, or nil outside a local process.
func currentTestEnvDialGuard() *testEnvDialGuard {
	return chooseTestEnvDialGuard(&testEnvProcessDialGuard, newTestEnvProcessDialGuard)
}

// Returns the chosen guard, first choosing it from the current environment when
// the startup choice was deferred. Concurrent first calls agree on one choice.
func chooseTestEnvDialGuard(
	choice *atomic.Pointer[testEnvDialGuardChoice],
	newGuard func() *testEnvDialGuard,
) *testEnvDialGuard {
	if chosen := choice.Load(); chosen != nil {
		return chosen.guard
	}
	choice.CompareAndSwap(nil, &testEnvDialGuardChoice{guard: newGuard()})
	return choice.Load().guard
}

// Reports whether an environment selects the local resources. WARP_ENV=local
// matches in any case and with surrounding space: a case-insensitive
// filesystem such as APFS resolves vault/Local to vault/local. With WARP_ENV
// unset, a WARP_VAULT_HOME that names a local vault directory itself is local
// too, since the resolver reads that directory's own resources for any
// environment.
func testEnvLocalEnvironment(getenv func(string) string) bool {
	warpEnv := strings.TrimSpace(getenv("WARP_ENV"))
	if strings.EqualFold(warpEnv, "local") {
		return true
	}
	vaultHome := getenv("WARP_VAULT_HOME")
	return warpEnv == "" && vaultHome != "" && strings.EqualFold(filepath.Base(filepath.Clean(vaultHome)), "local")
}

// Chooses the guard for one environment. Every local process (see
// testEnvLocalEnvironment) gets a guard, refusing every dial when its
// attestation is missing or invalid; any other environment, production
// included, gets nil.
func newTestEnvDialGuard(
	getenv func(string) string,
	lookupNetIp func(ctx context.Context, network string, host string) ([]netip.Addr, error),
) *testEnvDialGuard {
	if !testEnvLocalEnvironment(getenv) {
		return nil
	}
	attestation := getenv(testEnvAttestedAddrsEnvName)
	// test-env.sh admits explicit 127.0.0.1 authorities only under a private
	// portable root, with both portable flags.
	privateRoot := getenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES") == "1" &&
		getenv("WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES") == "1" &&
		getenv("WARP_TEST_ENV_PORTABLE_ROOT") != ""
	// Reads the attested endpoints. Each host must be an address literal: a name
	// would bring back the per-dial resolution that the attestation pins.
	parse := func() ([]netip.AddrPort, error) {
		if attestation == "" {
			return nil, fmt.Errorf(
				"%s is not set, so this WARP_ENV=local process may not dial PostgreSQL or Redis: "+
					"start ./local/run-local.sh and wait for 'Local environment is up', then start the process "+
					"through its run-local.sh (which sources ./local/local-env.sh), or source ./test-env.sh for tests",
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
	// Loopback is where a production data tunnel listens. Only the private
	// portable root, which names its own 127.0.0.1 endpoints, may attest it,
	// and never on a tunnel port. The managed-local launcher and the suite
	// proxy never attest loopback.
	requireLoopbackMode := func(attestedAddrPorts []netip.AddrPort) error {
		for _, addrPort := range attestedAddrPorts {
			if !addrPort.Addr().IsLoopback() {
				continue
			}
			if testEnvLoopbackTunnelPort(addrPort.Port()) {
				return fmt.Errorf(
					"%s attests loopback endpoint %s on a production data tunnel port",
					testEnvAttestedAddrsEnvName,
					addrPort,
				)
			}
			if !privateRoot {
				return fmt.Errorf(
					"%s attests loopback endpoint %s outside the private portable root (WARP_TEST_ENV_PORTABLE_ROOT)",
					testEnvAttestedAddrsEnvName,
					addrPort,
				)
			}
		}
		return nil
	}
	attestedAddrPorts, err := parse()
	if err == nil {
		err = requireLoopbackMode(attestedAddrPorts)
	}
	if err != nil {
		return &testEnvDialGuard{
			attestationErr: err,
			lookupNetIp:    lookupNetIp,
		}
	}
	return &testEnvDialGuard{
		attestedAddrPorts: attestedAddrPorts,
		privateRoot:       privateRoot,
		lookupNetIp:       lookupNetIp,
	}
}

// Pins a fixture's own Redis client to the launcher attestation in a local
// process. The fixture clients use logical db 0, so a connection never sends
// SELECT: a redial that reached a production cluster would initialize and run
// its commands there. Without a guard the client keeps go-redis's own dialer.
func configureTestEnvRedisDial(guard *testEnvDialGuard, options *redis.Options, dial redisDialFunc) {
	if guard == nil {
		return
	}
	options.Dialer = guard.wrapDial("redis", dial)
}

// Admits one live TCP loopback listener's exact endpoint until the returned
// removal runs. A later admission of the same endpoint is kept by an earlier
// removal.
func (self *testEnvDialGuard) admit(listener net.Listener) (func(), error) {
	tcpListener, ok := listener.(*net.TCPListener)
	if !ok {
		return nil, fmt.Errorf("test environment admits only a TCP listener, not %T", listener)
	}
	addrPort := tcpListener.Addr().(*net.TCPAddr).AddrPort()
	addrPort = netip.AddrPortFrom(addrPort.Addr().Unmap(), addrPort.Port())
	if !addrPort.Addr().IsLoopback() || addrPort.Addr().Zone() != "" {
		return nil, fmt.Errorf("test environment admits only a loopback listener, not %s", addrPort)
	}
	if testEnvLoopbackTunnelPort(addrPort.Port()) {
		return nil, fmt.Errorf("test environment never admits loopback %s: a production data tunnel can listen on that port", addrPort)
	}
	if !testEnvListenerLive(tcpListener) {
		return nil, fmt.Errorf("test environment admits only an open listener; %s is closed", addrPort)
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.admittedAddrPortListeners == nil {
			self.admittedAddrPortListeners = map[netip.AddrPort]*net.TCPListener{}
		}
		self.admittedAddrPortListeners[addrPort] = tcpListener
	}()
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.admittedAddrPortListeners[addrPort] == tcpListener {
			delete(self.admittedAddrPortListeners, addrPort)
		}
	}, nil
}

// Reports whether a dial may reach endpoint: it is attested, or it belongs to
// an admitted listener that is still open.
func (self *testEnvDialGuard) admits(addrPort netip.AddrPort) bool {
	if slices.Contains(self.attestedAddrPorts, addrPort) {
		return true
	}
	listener := func() *net.TCPListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.admittedAddrPortListeners[addrPort]
	}()
	return listener != nil && testEnvListenerLive(listener)
}

// Reports whether addr is the address of an attested or admitted endpoint. A
// lookup has no port; the dial still checks the exact endpoint.
func (self *testEnvDialGuard) admitsAddr(addr netip.Addr) bool {
	sameAddr := func(addrPort netip.AddrPort) bool {
		return addrPort.Addr() == addr
	}
	if slices.ContainsFunc(self.attestedAddrPorts, sameAddr) {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for addrPort := range self.admittedAddrPortListeners {
		if sameAddr(addrPort) {
			return true
		}
	}
	return false
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

// Returns fallback itself for a nil guard. Otherwise the lookup resolves
// through the guard's own resolver, the one its dials use, and admits only
// attested or admitted addresses, so a refusal names the host; the dial still
// checks each endpoint's port. No caller's resolver can widen the attestation,
// and a test can script the guard's.
func (self *testEnvDialGuard) lookupFunc(
	service string,
	fallback func(ctx context.Context, host string) ([]string, error),
) func(ctx context.Context, host string) ([]string, error) {
	if self == nil {
		return fallback
	}
	return func(ctx context.Context, host string) ([]string, error) {
		if self.attestationErr != nil {
			return nil, self.unattested(service, host)
		}
		addrs, err := self.resolveHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("local environment refused %s lookup: could not resolve %s: %w", service, host, err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("local environment refused %s lookup: %s resolved to no address", service, host)
		}
		addresses := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			addr = addr.Unmap()
			if !self.admitsAddr(addr) {
				return nil, self.refusal(service, host, addr.String(), addr.IsLoopback())
			}
			addresses = append(addresses, addr.String())
		}
		return addresses, nil
	}
}

// Resolves a host once through the guard's resolver; an address literal is
// its own answer.
func (self *testEnvDialGuard) resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	return self.lookupNetIp(ctx, "ip", host)
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
			"local environment refused %s dial to %s over %s; only tcp to an attested endpoint is allowed",
			service,
			address,
			network,
		)
	}
	host, portString, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("local environment refused %s dial to %q: %w", service, address, err)
	}
	port, err := strconv.ParseUint(portString, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("local environment refused %s dial to %q: invalid port", service, address)
	}
	addrs, err := self.resolveHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("local environment refused %s dial: could not resolve %s: %w", service, host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("local environment refused %s dial: %s resolved to no address", service, host)
	}
	addrPorts := make([]netip.AddrPort, 0, len(addrs))
	for _, addr := range addrs {
		addrPort := netip.AddrPortFrom(addr.Unmap(), uint16(port))
		if !self.admits(addrPort) {
			return nil, self.refusal(service, host, addrPort.String(), addrPort.Addr().IsLoopback())
		}
		addrPorts = append(addrPorts, addrPort)
	}
	return addrPorts, nil
}

// Names the host, what it resolved to and the attestation, then what would
// admit the dial. Only a name that moved off the launcher's address is fixed by
// restarting the launcher; an address literal was chosen by the caller.
func (self *testEnvDialGuard) refusal(service string, host string, resolved string, loopback bool) error {
	attested := make([]string, 0, len(self.attestedAddrPorts))
	for _, addrPort := range self.attestedAddrPorts {
		attested = append(attested, addrPort.String())
	}
	advice := "a local process dials only attested endpoints"
	if _, err := netip.ParseAddr(host); err != nil {
		advice = "restart ./local/run-local.sh, wait for 'Local environment is up', and attest again " +
			"(restart the process through its run-local.sh, or source ./test-env.sh for tests)"
		if loopback {
			advice = "the local service names can fall through to 127.0.0.1 once the launcher is down, " +
				"where a production data tunnel can listen; " + advice
		}
	} else if loopback {
		advice = "a production data tunnel can listen on loopback; admit an in-process listener this test owns with " +
			"Testing_AdmitTestEnvLoopbackListener"
	}
	return fmt.Errorf(
		"local environment refused %s dial: %s resolved to %s, which the launcher did not attest (attested %s); %s",
		service,
		host,
		resolved,
		strings.Join(attested, ", "),
		advice,
	)
}

// Refuses every dial of a local process whose attestation is missing or invalid.
func (self *testEnvDialGuard) unattested(service string, target string) error {
	return fmt.Errorf("local environment refused %s dial to %s: %w", service, target, self.attestationErr)
}

// One PostgreSQL or Redis authority a pool of this process dials.
type testEnvServiceEndpoint struct {
	service   string
	authority string
}

// Refuses a destructive local operation, such as sim-latency's reset
// (TRUNCATE ... CASCADE and FLUSHALL), unless this is a local process with a
// valid launcher attestation and every PostgreSQL and Redis endpoint its pools
// dial resolves only to attested endpoints. A loopback endpoint is refused
// outside the private portable root, and an in-process listener a test admitted
// never qualifies: only the attestation does. The check runs before the
// operation sends anything and opens no connection; the pools stay guarded
// either way. A process without a usable attestation is refused before any
// resource is read; missing pool resources panic, as the pools' own
// constructors do.
func RequireAttestedLocalEndpoints(ctx context.Context) error {
	guard := currentTestEnvDialGuard()
	if err := guard.requireDestructiveAttestation(); err != nil {
		return err
	}
	return guard.requireDestructiveEndpoints(ctx, testEnvPoolEndpoints())
}

// The authorities the PostgreSQL pools (each with its own vault resource) and
// the Redis pools dial, as their constructors select them.
func testEnvPoolEndpoints() []testEnvServiceEndpoint {
	endpoints := []testEnvServiceEndpoint{}
	for _, pool := range []*safePgPool{safePool, safeMaintenancePool} {
		vaultKeys, _ := pool.resolveResources()
		endpoints = append(endpoints, testEnvServiceEndpoint{
			service:   "postgres",
			authority: vaultKeys.RequireString("authority"),
		})
	}
	return append(endpoints, testEnvServiceEndpoint{
		service:   "redis",
		authority: Vault.RequireSimpleResource("redis.yml").RequireString("authority"),
	})
}

// A destructive local operation needs a local process with a valid attestation.
func (self *testEnvDialGuard) requireDestructiveAttestation() error {
	if self == nil {
		return errors.New(
			"destructive local operation refused: this is not a WARP_ENV=local process, so no launcher attestation protects it",
		)
	}
	if self.attestationErr != nil {
		return fmt.Errorf("destructive local operation refused: %w", self.attestationErr)
	}
	return nil
}

// See RequireAttestedLocalEndpoints. Each endpoint resolves once through the
// guard's resolver and must be attested itself; admitted listeners do not count.
func (self *testEnvDialGuard) requireDestructiveEndpoints(ctx context.Context, endpoints []testEnvServiceEndpoint) error {
	if err := self.requireDestructiveAttestation(); err != nil {
		return err
	}
	for _, endpoint := range endpoints {
		host, portString, err := net.SplitHostPort(endpoint.authority)
		if err != nil {
			return fmt.Errorf("destructive local operation refused: %s authority %q: %w", endpoint.service, endpoint.authority, err)
		}
		port, err := strconv.ParseUint(portString, 10, 16)
		if err != nil || port == 0 {
			return fmt.Errorf("destructive local operation refused: %s authority %q has an invalid port", endpoint.service, endpoint.authority)
		}
		addrs, err := self.resolveHost(ctx, host)
		if err != nil {
			return fmt.Errorf("destructive local operation refused: %s host %s did not resolve: %w", endpoint.service, host, err)
		}
		if len(addrs) == 0 {
			return fmt.Errorf("destructive local operation refused: %s host %s resolved to no address", endpoint.service, host)
		}
		for _, addr := range addrs {
			addrPort := netip.AddrPortFrom(addr.Unmap(), uint16(port))
			if !slices.Contains(self.attestedAddrPorts, addrPort) {
				reason := ""
				if addrPort.Addr().IsLoopback() {
					reason = ", and loopback is where a production data tunnel can listen"
				}
				attested := make([]string, 0, len(self.attestedAddrPorts))
				for _, attestedAddrPort := range self.attestedAddrPorts {
					attested = append(attested, attestedAddrPort.String())
				}
				return fmt.Errorf(
					"destructive local operation refused: %s host %s resolved to %s, which the launcher did not attest "+
						"(attested %s)%s; only attested endpoints qualify, never an admitted test listener",
					endpoint.service,
					host,
					addrPort,
					strings.Join(attested, ", "),
					reason,
				)
			}
			if addrPort.Addr().IsLoopback() && !self.privateRoot {
				return fmt.Errorf(
					"destructive local operation refused: %s endpoint %s is loopback, where a production data tunnel "+
						"can listen; only the private portable root may attest loopback",
					endpoint.service,
					addrPort,
				)
			}
		}
	}
	return nil
}
