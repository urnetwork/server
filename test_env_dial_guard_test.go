// Pure checks that a local process, test or not, dials PostgreSQL and Redis
// only at the endpoints its launcher attested. Every socket is a net.Pipe or a recording
// fake and every name resolves through a scripted resolver, so nothing here
// reaches a service, the host resolver, or a loopback tunnel.
package server

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

const (
	testEnvFixtureRedisHost = "redis.fixture.example"
	testEnvFixturePgHost    = "pg.fixture.example"
	// what test-env.sh exports for a managed launcher on a synthetic address
	testEnvFixtureAttestation = "192.0.2.10:5432,192.0.2.10:6379"
)

var (
	testEnvFixtureAddr  = netip.MustParseAddr("192.0.2.10")
	testEnvLoopbackAddr = netip.MustParseAddr("127.0.0.1")
	// Answers names in tests whose base dialer is real. Were a regression to
	// admit it, the dial would leave for documentation space on a port no
	// production tunnel forwards, never loopback.
	testEnvUnattestedAddr = netip.MustParseAddr("198.51.100.20")
)

// Serves scripted answers for one name, one per lookup; the last answer
// repeats. The first answer models the launcher's hosts mapping and a later
// one public DNS after the launcher withdrew it.
type testEnvScriptedResolver struct {
	stateLock sync.Mutex
	host      string
	answers   [][]netip.Addr
	lookups   int
}

// Serves the next scripted answer for the scripted name; any other name fails.
func (self *testEnvScriptedResolver) lookupNetIp(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if host != self.host || len(self.answers) == 0 {
		return nil, fmt.Errorf("unscripted lookup of %s", host)
	}
	answer := self.answers[min(self.lookups, len(self.answers)-1)]
	self.lookups += 1
	return answer, nil
}

// The same script in pgconn's LookupFunc shape.
func (self *testEnvScriptedResolver) lookupHost(ctx context.Context, host string) ([]string, error) {
	addrs, err := self.lookupNetIp(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	addresses := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		addresses = append(addresses, addr.String())
	}
	return addresses, nil
}

// The number of lookups served so far.
func (self *testEnvScriptedResolver) lookupCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.lookups
}

// Fails the test if a guard consults the resolver at all.
func testEnvUnusedResolver(t *testing.T) func(context.Context, string, string) ([]netip.Addr, error) {
	return func(_ context.Context, _ string, host string) ([]netip.Addr, error) {
		t.Errorf("unexpected resolution of %s", host)
		return nil, errors.New("unexpected resolution")
	}
}

// Models net.Dialer: a name is resolved at dial time through the same script,
// as the pre-fix dial path did. Each "connection" is one end of a pipe; no
// socket is opened.
type testEnvRecordingDialer struct {
	stateLock sync.Mutex
	resolver  *testEnvScriptedResolver
	connected []string
	conns     []net.Conn
}

// Closes every pipe end the dialer handed out when the test ends.
func newTestEnvRecordingDialer(t *testing.T, resolver *testEnvScriptedResolver) *testEnvRecordingDialer {
	dialer := &testEnvRecordingDialer{resolver: resolver}
	t.Cleanup(func() {
		dialer.stateLock.Lock()
		defer dialer.stateLock.Unlock()
		for _, conn := range dialer.conns {
			conn.Close()
		}
	})
	return dialer
}

// Resolves a name through the script, records the endpoint and returns a pipe.
func (self *testEnvRecordingDialer) dial(ctx context.Context, _ string, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if _, err := netip.ParseAddr(host); err != nil {
		addrs, err := self.resolver.lookupNetIp(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		address = net.JoinHostPort(addrs[0].String(), port)
	}
	client, peer := net.Pipe()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connected = append(self.connected, address)
	self.conns = append(self.conns, client, peer)
	return client, nil
}

// The endpoints dialed so far, in order.
func (self *testEnvRecordingDialer) connectedEndpoints() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.connected)
}

// An environment exactly as a variable lookup sees it.
func testEnvGetenv(values map[string]string) func(string) string {
	return func(name string) string {
		return values[name]
	}
}

// The environment test-env.sh exports for a managed-local run.
func testEnvManagedEnvironment(attestation string) func(string) string {
	return testEnvGetenv(map[string]string{
		"WARP_ENV":                  "local",
		"WARP_SERVICE":              "test",
		"WARP_BLOCK":                "test",
		testEnvAttestedAddrsEnvName: attestation,
	})
}

// Installs guard as the process guard for one test and restores the startup
// choice afterwards. Callers must not run in parallel.
func useTestEnvProcessDialGuard(t *testing.T, guard *testEnvDialGuard) {
	t.Helper()
	previous := testEnvProcessDialGuard.Swap(&testEnvDialGuardChoice{guard: guard})
	t.Cleanup(func() {
		testEnvProcessDialGuard.Store(previous)
	})
}

// A function's code pointer: equal for one function value or one closure
// literal, so a replaced dial or lookup is visible without calling it.
func testEnvFuncPointer(function any) uintptr {
	return reflect.ValueOf(function).Pointer()
}

// The incident: a pool connected while the launcher mapped its name, then
// redialed after the launcher withdrew its hosts block, when the same name
// resolved through public DNS to loopback. The redial must be refused before a
// socket opens, with an error naming the host and where it now resolves.
func TestRedisPoolRedialRefusesNameMovedToLoopback(t *testing.T) {
	resolver := &testEnvScriptedResolver{
		host:    testEnvFixtureRedisHost,
		answers: [][]netip.Addr{{testEnvFixtureAddr}, {testEnvLoopbackAddr}},
	}
	dialer := newTestEnvRecordingDialer(t, resolver)
	guard := newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), resolver.lookupNetIp)
	authority := net.JoinHostPort(testEnvFixtureRedisHost, "6379")
	dial := redisPoolDialer(guard, authority, map[string]string{}, dialer.dial)

	conn, err := dial(t.Context(), "tcp", authority)
	if err != nil {
		t.Fatalf("dial while the name maps to the attested address: %v", err)
	}
	conn.Close()
	conn, err = dial(t.Context(), "tcp", authority)
	if err == nil {
		conn.Close()
	}
	if connected := dialer.connectedEndpoints(); !slices.Equal(connected, []string{"192.0.2.10:6379"}) {
		t.Fatalf("connected endpoints = %q; the redial after the name moved to loopback must not connect", connected)
	}
	if err == nil || !strings.Contains(err.Error(), "redis.fixture.example resolved to 127.0.0.1:6379, which the launcher did not attest") {
		t.Fatalf("loopback redial error = %v; want a refusal naming the host and its loopback address", err)
	}
}

// The same withdrawal seen by pgconn, which resolves its host on every
// connect. The first connect reaches the attested endpoint; the next is refused
// at lookup, before any dial.
func TestPgConnectRefusesNameMovedToLoopback(t *testing.T) {
	resolver := &testEnvScriptedResolver{
		host:    testEnvFixturePgHost,
		answers: [][]netip.Addr{{testEnvFixtureAddr}, {testEnvLoopbackAddr}},
	}
	guard := newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), resolver.lookupNetIp)
	config, err := pgx.ParseConfig("postgres://synthetic@" + testEnvFixturePgHost + ":5432/synthetic?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	// The scripted lookup and the recording dial stand in for net.Resolver and
	// net.Dialer; reaching the dialer ends each connect without a socket.
	reached := errors.New("synthetic dial reached")
	dialed := []string{}
	config.LookupFunc = resolver.lookupHost
	config.DialFunc = func(_ context.Context, _ string, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return nil, reached
	}
	configurePgTestEnvDial(guard, config)

	if _, err := pgx.ConnectConfig(t.Context(), config); !errors.Is(err, reached) {
		t.Fatalf("connect while the name maps to the attested address = %v; want it to reach the dialer", err)
	}
	_, err = pgx.ConnectConfig(t.Context(), config)
	if !slices.Equal(dialed, []string{"192.0.2.10:5432"}) {
		t.Fatalf("dialed endpoints = %q; the reconnect after the name moved to loopback must not dial", dialed)
	}
	if err == nil || !strings.Contains(err.Error(), "pg.fixture.example resolved to 127.0.0.1, which the launcher did not attest") {
		t.Fatalf("loopback reconnect error = %v; want a refusal naming the host and its loopback address", err)
	}
}

// The lease coordinator uses logical db 0 and so sends no SELECT: its redial
// is the one that ran a lease command on a production cluster. Drive a real
// go-redis client against a synthetic peer: the first connection is served,
// and a later redial is refused before the dialer opens anything.
func TestRedisLeaseCoordinatorRedialRefusesNameMovedToLoopback(t *testing.T) {
	peer := newDeadlineRedisPeer(t, "")
	resolver := &testEnvScriptedResolver{
		host:    testEnvFixtureRedisHost,
		answers: [][]netip.Addr{{testEnvFixtureAddr}, {testEnvLoopbackAddr}},
	}
	guard := newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), resolver.lookupNetIp)
	var stateLock sync.Mutex
	connected := []string{}
	options := &redis.Options{
		Addr:                  net.JoinHostPort(testEnvFixtureRedisHost, "6379"),
		DB:                    testRedisLeaseCoordinatorDb,
		MaxRetries:            -1,
		DialerRetries:         1,
		PoolSize:              2,
		DialTimeout:           time.Second,
		ReadTimeout:           time.Second,
		WriteTimeout:          time.Second,
		ContextTimeoutEnabled: true,
		// synthetic RESP peer only; a preset endpoint type keeps client
		// construction from consulting the host resolver
		Protocol:        2,
		DisableIdentity: true,
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode:         maintnotifications.ModeDisabled,
			EndpointType: maintnotifications.EndpointTypeExternalIP,
		},
	}
	configureTestEnvRedisDial(guard, options, func(ctx context.Context, network string, address string) (net.Conn, error) {
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			connected = append(connected, address)
		}()
		return peer.dial(ctx, network, address)
	})
	if options.Dialer == nil {
		t.Fatal("a test process left the lease coordinator on go-redis's own dialer, which resolves its name on every dial")
	}
	client := redis.NewClient(options)
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// Hold the first connection so the second command must redial.
	first := client.Conn()
	defer first.Close()
	if err := first.Ping(ctx).Err(); err != nil {
		t.Fatalf("lease command while the name maps to the attested address: %v", err)
	}
	second := client.Conn()
	defer second.Close()
	err := second.Ping(ctx).Err()
	stateLock.Lock()
	defer stateLock.Unlock()
	if !slices.Equal(connected, []string{"192.0.2.10:6379"}) || peer.dials.Load() != 1 {
		t.Fatalf("connected endpoints = %q, peer dials = %d; the loopback redial must not connect", connected, peer.dials.Load())
	}
	if err == nil || !strings.Contains(err.Error(), "redis.fixture.example resolved to 127.0.0.1:6379") {
		t.Fatalf("loopback redial error = %v; want a refusal naming the host and its loopback address", err)
	}
	if peer.count("ping") != 1 {
		t.Fatalf("synthetic peer served %d lease commands; want only the first", peer.count("ping"))
	}
}

// Every local process gets a guard, test or not; one without an attestation
// refuses every dial. Production and every other environment get none, even
// with stray test markers and an attestation, so their dial paths cannot
// change.
func TestTestEnvDialGuardSelectsEveryLocalProcess(t *testing.T) {
	cases := []struct {
		name     string
		values   map[string]string
		guarded  bool
		attested bool
	}{
		{name: "unset", values: map[string]string{}},
		{name: "production service", values: map[string]string{"WARP_ENV": "main", "WARP_SERVICE": "api", "WARP_BLOCK": "g1"}},
		{name: "production with stray test markers and attestation", values: map[string]string{
			"WARP_ENV": "main", "WARP_SERVICE": "test", "WARP_BLOCK": "test", testEnvAttestedAddrsEnvName: testEnvFixtureAttestation,
		}},
		{name: "another environment with an attestation", values: map[string]string{
			"WARP_ENV": "sim-testnet-op-1", "WARP_SERVICE": "api", testEnvAttestedAddrsEnvName: testEnvFixtureAttestation,
		}},
		{name: "local service", values: map[string]string{"WARP_ENV": "local", "WARP_SERVICE": "api", "WARP_BLOCK": "local"}, guarded: true},
		{name: "local without identity", values: map[string]string{"WARP_ENV": "local"}, guarded: true},
		{name: "service marker only", values: map[string]string{"WARP_ENV": "local", "WARP_SERVICE": "test"}, guarded: true},
		{name: "block marker only", values: map[string]string{"WARP_ENV": "local", "WARP_BLOCK": "test"}, guarded: true},
		{name: "markers without attestation", values: map[string]string{"WARP_ENV": "local", "WARP_SERVICE": "test", "WARP_BLOCK": "test"}, guarded: true},
		{name: "attested local service", values: map[string]string{
			"WARP_ENV": "local", "WARP_SERVICE": "api", "WARP_BLOCK": "local", testEnvAttestedAddrsEnvName: testEnvFixtureAttestation,
		}, guarded: true, attested: true},
		{name: "attestation without markers", values: map[string]string{
			"WARP_ENV": "local", testEnvAttestedAddrsEnvName: testEnvFixtureAttestation,
		}, guarded: true, attested: true},
		{name: "markers and attestation", values: map[string]string{
			"WARP_ENV": "local", "WARP_SERVICE": "test", "WARP_BLOCK": "test", testEnvAttestedAddrsEnvName: testEnvFixtureAttestation,
		}, guarded: true, attested: true},
	}
	for _, c := range cases {
		guard := newTestEnvDialGuard(testEnvGetenv(c.values), testEnvUnusedResolver(t))
		if (guard != nil) != c.guarded {
			t.Errorf("%s: guarded = %t; want %t", c.name, guard != nil, c.guarded)
			continue
		}
		if c.guarded && (guard.requireAttested() == nil) != c.attested {
			t.Errorf("%s: attestation error = %v; want attested %t", c.name, guard.requireAttested(), c.attested)
		}
	}
}

// WARP_ENV=local selects the local resources in any case and with surrounding
// space, since a case-insensitive filesystem such as APFS resolves vault/Local
// to vault/local, so each spelling is guarded.
func TestLocalWarpEnvMatchesAnyCaseAndSpace(t *testing.T) {
	for _, value := range []string{"local", "Local", "LOCAL", " local", "local ", "local\n", "\tLocal "} {
		values := map[string]string{"WARP_ENV": value, "WARP_VAULT_HOME": "/synthetic/vault"}
		guard := newTestEnvDialGuard(testEnvGetenv(values), testEnvUnusedResolver(t))
		if guard == nil {
			t.Errorf("WARP_ENV=%q got no guard", value)
			continue
		}
		if err := guard.requireAttested(); err == nil || !strings.Contains(err.Error(), testEnvAttestedAddrsEnvName+" is not set") {
			t.Errorf("WARP_ENV=%q: attestation error = %v; want the missing attestation refused", value, err)
		}
	}
}

// With WARP_ENV unset, a WARP_VAULT_HOME that names a local vault directory
// itself selects the local resources, so the process is guarded. A process
// that sets WARP_ENV, production included, keeps its own selection.
func TestLocalVaultHomeWithoutWarpEnvIsGuarded(t *testing.T) {
	cases := []struct {
		values  map[string]string
		guarded bool
	}{
		{values: map[string]string{"WARP_VAULT_HOME": "/synthetic/vault/local"}, guarded: true},
		{values: map[string]string{"WARP_VAULT_HOME": "/synthetic/vault/local/"}, guarded: true},
		{values: map[string]string{"WARP_VAULT_HOME": "/synthetic/vault/Local"}, guarded: true},
		{values: map[string]string{"WARP_ENV": " ", "WARP_VAULT_HOME": "/synthetic/vault/local"}, guarded: true},
		{values: map[string]string{"WARP_VAULT_HOME": "/synthetic/vault"}},
		{values: map[string]string{"WARP_VAULT_HOME": "/synthetic/vault/local/main"}},
		{values: map[string]string{"WARP_ENV": "main", "WARP_VAULT_HOME": "/synthetic/vault/local"}},
		{values: map[string]string{}},
	}
	for _, c := range cases {
		guard := newTestEnvDialGuard(testEnvGetenv(c.values), testEnvUnusedResolver(t))
		if (guard != nil) != c.guarded {
			t.Errorf("%v: guarded = %t; want %t", c.values, guard != nil, c.guarded)
		}
	}
}

// A test binary whose vault home names a vault's local directory itself starts
// with a dial guard, and the test vault guard still reads none of that
// directory's credentials, even when test-env.sh names it as the launcher's
// vault: the launcher scope needs WARP_ENV=local, and then reads only a real
// local directory below the vault home, which a local directory has not. The
// guard is chosen from the environment alone, so each lookup ends in one
// refusal that names no credential.
func TestLocalVaultHomeTestBinaryIsGuardedAndReadsNoLauncherCredentials(t *testing.T) {
	cases := []struct {
		warpEnv   string
		errorText string
	}{
		{warpEnv: "", errorText: "only with WARP_ENV=local"},
		{warpEnv: "local", errorText: "only from a real local directory"},
	}
	for _, c := range cases {
		vaultHome, _ := writeTestLauncherVault(t)
		localVaultHome := filepath.Join(vaultHome, "local")
		t.Setenv("WARP_ENV", c.warpEnv)
		t.Setenv("WARP_VAULT_HOME", localVaultHome)
		t.Setenv(testLauncherVaultHomeEnvName, localVaultHome)
		t.Setenv(testEnvAttestedAddrsEnvName, testLauncherAttestedAddrs)
		// the choice of a test binary started from this environment
		choice := chooseTestEnvStartupDialGuard(true, c.warpEnv, func() *testEnvDialGuard {
			return newTestEnvDialGuard(os.Getenv, testEnvUnusedResolver(t))
		})
		if choice == nil || choice.guard.requireAttested() != nil {
			t.Fatalf("WARP_ENV %q: a test binary with vault home %s started without an attested dial guard", c.warpEnv, localVaultHome)
		}
		useTestEnvProcessDialGuard(t, choice.guard)
		for _, name := range []string{DefaultPgVaultResourceName, "redis.yml"} {
			_, err := Vault.SimpleResource(name)
			if !errors.Is(err, ErrResourceNotFound) || !strings.Contains(err.Error(), c.errorText) {
				t.Errorf("WARP_ENV %q: %s read %v; want a refusal naming %q", c.warpEnv, name, err, c.errorText)
			} else if strings.Contains(err.Error(), testLauncherPassword) {
				t.Errorf("WARP_ENV %q: the %s refusal names a credential", c.warpEnv, name)
			}
		}
		if _, err := loadTestEnvironmentConfiguration(); err == nil {
			t.Errorf("WARP_ENV %q: a DB test loaded the launcher's settings from vault home %s", c.warpEnv, localVaultHome)
		}
	}
}

// A nil guard is the production case: it hands back the caller's own dial and
// lookup, which then see the unresolved name exactly as before, and leaves
// every client configuration untouched.
func TestNilTestEnvDialGuardLeavesDialPathsUnchanged(t *testing.T) {
	var guard *testEnvDialGuard
	dialed := []string{}
	unreachable := errors.New("synthetic unreachable")
	dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return nil, unreachable
	}
	lookedUp := []string{}
	lookup := func(_ context.Context, host string) ([]string, error) {
		lookedUp = append(lookedUp, host)
		return []string{"198.51.100.7"}, nil
	}
	if testEnvFuncPointer(guard.wrapDial("redis", dial)) != testEnvFuncPointer(dial) ||
		testEnvFuncPointer(guard.lookupFunc("postgres", lookup)) != testEnvFuncPointer(lookup) {
		t.Fatal("a nil guard replaced the caller's dial or lookup")
	}
	authority := net.JoinHostPort(testEnvFixtureRedisHost, "6379")
	if _, err := redisPoolDialer(guard, authority, map[string]string{}, dial)(t.Context(), "tcp", authority); !errors.Is(err, unreachable) {
		t.Fatalf("unguarded pool dial error = %v; want the caller's own result", err)
	}
	addresses, err := guard.lookupFunc("postgres", lookup)(t.Context(), testEnvFixturePgHost)
	if err != nil || !slices.Equal(addresses, []string{"198.51.100.7"}) {
		t.Fatalf("unguarded lookup = %q, %v; want the caller's own answer", addresses, err)
	}
	if !slices.Equal(dialed, []string{authority}) || !slices.Equal(lookedUp, []string{testEnvFixturePgHost}) {
		t.Fatalf("dialed %q and looked up %q; want the unresolved name passed through unchanged", dialed, lookedUp)
	}

	config, err := pgx.ParseConfig("postgres://synthetic@" + testEnvFixturePgHost + ":5432/synthetic?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	dialPointer, lookupPointer := testEnvFuncPointer(config.DialFunc), testEnvFuncPointer(config.LookupFunc)
	configurePgTestEnvDial(guard, config)
	if testEnvFuncPointer(config.DialFunc) != dialPointer || testEnvFuncPointer(config.LookupFunc) != lookupPointer {
		t.Fatal("a nil guard changed the PostgreSQL dial or lookup")
	}
	options := &redis.Options{Addr: authority}
	configureTestEnvRedisDial(guard, options, dial)
	if options.Dialer != nil {
		t.Fatal("a nil guard replaced go-redis's own dialer")
	}
	if guard.requireAttested() == nil {
		t.Fatal("a process without a guard qualified for integration fixtures")
	}
}

// Without a guard the production pool constructor keeps exactly the dial and
// lookup its own producers configure (the write-tracking dialer over pgx's, and
// the bounded liveness lookup); only a test process gets both replaced.
func TestPgPoolOpenReplacesDialFunctionsOnlyForTestProcess(t *testing.T) {
	pushTestEnvGuardPgResources(t)
	reference, err := pgxpool.ParseConfig("postgres://synthetic:synthetic@192.0.2.10:5432/synthetic?sslmode=disable&connect_timeout=5")
	if err != nil {
		t.Fatal(err)
	}
	configurePgPoolLiveness(reference)
	configurePgPoolWriteTracking(reference)
	configurePgPoolStatementErrors(reference)

	useTestEnvProcessDialGuard(t, nil)
	pool := &safePgPool{ctx: context.Background(), vaultResourceName: DefaultPgVaultResourceName, configResourceName: DefaultPgConfigResourceName}
	defer pool.reset()
	production := pool.open().Config().ConnConfig
	if testEnvFuncPointer(production.DialFunc) != testEnvFuncPointer(reference.ConnConfig.DialFunc) ||
		testEnvFuncPointer(production.LookupFunc) != testEnvFuncPointer(reference.ConnConfig.LookupFunc) {
		t.Fatal("the pool constructor changed its dial or lookup without a test guard")
	}
	pool.reset()

	useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), testEnvUnusedResolver(t)))
	guarded := pool.open().Config().ConnConfig
	if testEnvFuncPointer(guarded.DialFunc) == testEnvFuncPointer(reference.ConnConfig.DialFunc) ||
		testEnvFuncPointer(guarded.LookupFunc) == testEnvFuncPointer(reference.ConnConfig.LookupFunc) {
		t.Fatal("the pool constructor left a test process on unguarded dial or lookup")
	}
}

// The real PostgreSQL pool constructor dials through the process guard: a name
// that resolves outside the attestation is refused before a socket opens.
func TestPgPoolOpenDialsThroughProcessGuard(t *testing.T) {
	pushTestEnvGuardPgResources(t)
	resolver := &testEnvScriptedResolver{host: "pg.fixture.invalid", answers: [][]netip.Addr{{testEnvUnattestedAddr}}}
	useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), resolver.lookupNetIp))
	pool := &safePgPool{ctx: context.Background(), vaultResourceName: DefaultPgVaultResourceName, configResourceName: DefaultPgConfigResourceName}
	defer pool.reset()
	_, err := pool.open().Config().ConnConfig.DialFunc(t.Context(), "tcp", "pg.fixture.invalid:15432")
	if err == nil || !strings.Contains(err.Error(), "pg.fixture.invalid resolved to 198.51.100.20:15432") || resolver.lookupCount() != 1 {
		t.Fatalf("pool dial error = %v after %d lookups; want the process guard's refusal", err, resolver.lookupCount())
	}
}

// Every Redis pool constructor dials through the process guard: a name that
// resolves outside the attestation is refused before a socket opens.
func TestRedisPoolOpenDialsThroughProcessGuard(t *testing.T) {
	resolver := &testEnvScriptedResolver{host: "redis.fixture.invalid", answers: [][]netip.Addr{{testEnvUnattestedAddr}}}
	useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), resolver.lookupNetIp))
	// an address-literal authority keeps client construction off the resolver
	popVault := Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 192.0.2.10:6379\npassword: \"\"\ndb: 3\n"))
	defer popVault()
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
	defer popConfig()
	pools := []*safeRedisClient{
		{},
		{disableCommandRetry: true},
		{disableCommandRetry: true, contextTimeoutEnabled: true},
		{disableCommandRetry: true, contextTimeoutEnabled: true, authentication: true},
	}
	for index, pool := range pools {
		client, ok := pool.open().(*redis.Client)
		if !ok {
			t.Fatalf("pool %d constructor did not make a standalone client", index)
		}
		_, err := client.Options().Dialer(t.Context(), "tcp", "redis.fixture.invalid:16379")
		pool.close()
		if err == nil || !strings.Contains(err.Error(), "redis.fixture.invalid resolved to 198.51.100.20:16379") {
			t.Errorf("pool %d dial error = %v; want the process guard's refusal", index, err)
		}
	}
	if resolver.lookupCount() != len(pools) {
		t.Fatalf("guard resolved %d times; want once per pool", resolver.lookupCount())
	}
}

// The fixture probe refuses before any connection when the process has no
// usable attestation, the state of a direct go test that skipped test-env.sh,
// and with one it reaches only attested endpoints.
func TestTestEnvironmentProbeRequiresAttestation(t *testing.T) {
	configuration := testEnvironmentConfiguration{
		postgresAuthority: "198.51.100.7:15432",
		postgresUser:      "synthetic",
		postgresPassword:  "not-a-secret",
		postgresDatabase:  "synthetic",
		redisAuthority:    "198.51.100.7:16379",
	}
	useTestEnvProcessDialGuard(t, nil)
	for _, service := range []string{"postgres", "redis"} {
		err := probeTestEnvironmentService(t.Context(), service, configuration)
		if err == nil || !strings.Contains(err.Error(), testEnvAttestedAddrsEnvName) {
			t.Errorf("%s probe without a guard = %v; want a refusal naming the attestation", service, err)
		}
	}
	useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvManagedEnvironment(""), testEnvUnusedResolver(t)))
	for _, service := range []string{"postgres", "redis"} {
		err := probeTestEnvironmentService(t.Context(), service, configuration)
		if err == nil || !strings.Contains(err.Error(), testEnvAttestedAddrsEnvName+" is not set") {
			t.Errorf("%s probe without an attestation = %v; want a refusal naming the attestation", service, err)
		}
	}
	useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), testEnvUnusedResolver(t)))
	for _, service := range []string{"postgres", "redis"} {
		err := probeTestEnvironmentService(t.Context(), service, configuration)
		if err == nil || !strings.Contains(err.Error(), "which the launcher did not attest") {
			t.Errorf("%s probe of an unattested endpoint = %v; want the guard's refusal", service, err)
		}
	}
}

// The private portable root attests its own 127.0.0.1 endpoints and may dial
// exactly those. Every other loopback endpoint, such as a forwarded production
// database port, stays refused.
func TestTestEnvDialGuardAdmitsOnlyAttestedPortableLoopbackEndpoints(t *testing.T) {
	guard := newTestEnvDialGuard(testEnvGetenv(map[string]string{
		"WARP_ENV":                             "local",
		"WARP_SERVICE":                         "test",
		"WARP_BLOCK":                           "test",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES": "1",
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1",
		"WARP_TEST_ENV_PORTABLE_ROOT":                     "/synthetic/private-root",
		testEnvAttestedAddrsEnvName:                       "127.0.0.1:35431,127.0.0.1:36371",
	}), testEnvUnusedResolver(t))
	if err := guard.requireAttested(); err != nil {
		t.Fatalf("portable loopback attestation refused: %v", err)
	}
	dialed := []string{}
	dial := guard.wrapDial("postgres", func(_ context.Context, _ string, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		client, peer := net.Pipe()
		peer.Close()
		return client, nil
	})
	for _, address := range []string{"127.0.0.1:35431", "127.0.0.1:36371"} {
		conn, err := dial(t.Context(), "tcp", address)
		if err != nil {
			t.Errorf("attested portable endpoint %s refused: %v", address, err)
			continue
		}
		conn.Close()
	}
	for _, address := range []string{"127.0.0.1:5432", "127.0.0.1:6379", "127.0.0.1:6432", "[::1]:35431", "127.0.0.2:35431"} {
		if _, err := dial(t.Context(), "tcp", address); err == nil || !strings.Contains(err.Error(), "production data tunnel") {
			t.Errorf("unattested loopback endpoint %s = %v; want a loopback refusal", address, err)
		}
	}
	if !slices.Equal(dialed, []string{"127.0.0.1:35431", "127.0.0.1:36371"}) {
		t.Fatalf("dialed %q; want only the attested portable endpoints", dialed)
	}
}

// Outside the private portable root a loopback attestation is itself invalid,
// so a managed profile, or unmanaged services named 127.0.0.1 without the
// root, can never admit loopback.
func TestTestEnvDialGuardRefusesLoopbackAttestationOutsidePrivateRoot(t *testing.T) {
	for _, flags := range []map[string]string{
		{},
		{"WARP_TEST_ENV_USE_PORTABLE_RESOURCES": "1"},
		{"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1"},
		{"WARP_TEST_ENV_USE_PORTABLE_RESOURCES": "1", "WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1"},
		{"WARP_TEST_ENV_PORTABLE_ROOT": "/synthetic/private-root"},
	} {
		values := map[string]string{
			"WARP_ENV":                  "local",
			"WARP_SERVICE":              "test",
			"WARP_BLOCK":                "test",
			testEnvAttestedAddrsEnvName: "192.0.2.10:5432,127.0.0.1:16379",
		}
		for name, value := range flags {
			values[name] = value
		}
		guard := newTestEnvDialGuard(testEnvGetenv(values), testEnvUnusedResolver(t))
		err := guard.requireAttested()
		if err == nil || !strings.Contains(err.Error(), "attests loopback endpoint 127.0.0.1:16379 outside the private portable root") {
			t.Errorf("flags %v: attestation error = %v; want the loopback attestation refused", flags, err)
			continue
		}
		if _, err := guard.wrapDial("redis", nil)(t.Context(), "tcp", "192.0.2.10:5432"); err == nil {
			t.Errorf("flags %v: an invalid attestation still admitted a dial", flags)
		}
	}
}

// Even the private portable root may not attest loopback on a production data
// tunnel port: on a host running the tunnel those ports are production.
func TestTestEnvDialGuardRefusesLoopbackTunnelPortAttestation(t *testing.T) {
	for _, endpoint := range []string{"127.0.0.1:5432", "127.0.0.1:6432", "127.0.0.1:6379", "127.0.0.1:6411", "[::1]:6427"} {
		guard := newTestEnvDialGuard(testEnvGetenv(map[string]string{
			"WARP_ENV":                             "local",
			"WARP_SERVICE":                         "test",
			"WARP_BLOCK":                           "test",
			"WARP_TEST_ENV_USE_PORTABLE_RESOURCES": "1",
			"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1",
			"WARP_TEST_ENV_PORTABLE_ROOT":                     "/synthetic/private-root",
			testEnvAttestedAddrsEnvName:                       endpoint + ",127.0.0.1:36371",
		}), testEnvUnusedResolver(t))
		if err := guard.requireAttested(); err == nil || !strings.Contains(err.Error(), "on a production data tunnel port") {
			t.Errorf("attestation of %s = %v; want a tunnel-port refusal", endpoint, err)
		}
	}
}

// The suite proxy attests its direct host-IP endpoints, which its resources
// name as address literals, so those dials need no resolution.
func TestTestEnvDialGuardAdmitsSuiteProxyEndpoints(t *testing.T) {
	guard := newTestEnvDialGuard(testEnvManagedEnvironment("192.0.2.44:15432,192.0.2.44:16379"), testEnvUnusedResolver(t))
	dialed := []string{}
	dial := guard.wrapDial("postgres", func(_ context.Context, _ string, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		client, peer := net.Pipe()
		peer.Close()
		return client, nil
	})
	for _, address := range []string{"192.0.2.44:15432", "192.0.2.44:16379"} {
		conn, err := dial(t.Context(), "tcp", address)
		if err != nil {
			t.Errorf("attested suite proxy endpoint %s refused: %v", address, err)
			continue
		}
		conn.Close()
	}
	for _, address := range []string{"192.0.2.44:5432", "192.0.2.45:15432"} {
		if _, err := dial(t.Context(), "tcp", address); err == nil {
			t.Errorf("unattested endpoint %s was admitted", address)
		}
	}
	if !slices.Equal(dialed, []string{"192.0.2.44:15432", "192.0.2.44:16379"}) {
		t.Fatalf("dialed %q; want only the attested suite proxy endpoints", dialed)
	}
}

// One unattested address taints the whole answer: a resolver that merges the
// hosts mapping with a public answer must not leave loopback as a fallback.
func TestTestEnvDialGuardRefusesAnyUnattestedResolvedAddress(t *testing.T) {
	resolver := &testEnvScriptedResolver{
		host:    testEnvFixtureRedisHost,
		answers: [][]netip.Addr{{testEnvFixtureAddr, testEnvLoopbackAddr}},
	}
	guard := newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), resolver.lookupNetIp)
	dialed := 0
	dial := guard.wrapDial("redis", func(context.Context, string, string) (net.Conn, error) {
		dialed += 1
		return nil, errors.New("unexpected dial")
	})
	if _, err := dial(t.Context(), "tcp", net.JoinHostPort(testEnvFixtureRedisHost, "6379")); err == nil ||
		!strings.Contains(err.Error(), "resolved to 127.0.0.1:6379") {
		t.Fatalf("mixed answer dial = %v; want the loopback address refused", err)
	}
	if _, err := guard.lookupFunc("redis", resolver.lookupHost)(t.Context(), testEnvFixtureRedisHost); err == nil ||
		!strings.Contains(err.Error(), "resolved to 127.0.0.1") {
		t.Fatalf("mixed answer lookup = %v; want the loopback address refused", err)
	}
	if dialed != 0 {
		t.Fatalf("a mixed answer reached the dialer %d times", dialed)
	}
}

// A malformed attestation fails every dial closed rather than admitting
// anything; names, partial endpoints and wildcard addresses are not endpoints.
func TestTestEnvDialGuardRejectsMalformedAttestation(t *testing.T) {
	for _, attestation := range []string{
		"192.0.2.10",
		"pg.fixture.example:5432",
		"192.0.2.10:0",
		"192.0.2.10:65536",
		"0.0.0.0:5432",
		"[::]:5432",
		"224.0.0.1:6379",
		"[fe80::1%en0]:5432",
		"[::ffff:192.0.2.10]:5432",
		"192.0.2.10:5432,",
		"192.0.2.1:1,192.0.2.2:2,192.0.2.3:3,192.0.2.4:4,192.0.2.5:5,192.0.2.6:6,192.0.2.7:7,192.0.2.8:8,192.0.2.9:9",
	} {
		guard := newTestEnvDialGuard(testEnvManagedEnvironment(attestation), testEnvUnusedResolver(t))
		if guard.requireAttested() == nil {
			t.Errorf("attestation %q was accepted", attestation)
			continue
		}
		dial := guard.wrapDial("postgres", func(context.Context, string, string) (net.Conn, error) {
			t.Errorf("attestation %q admitted a dial", attestation)
			return nil, errors.New("unexpected dial")
		})
		if _, err := dial(t.Context(), "tcp", "192.0.2.10:5432"); err == nil {
			t.Errorf("attestation %q did not refuse the dial", attestation)
		}
	}
}

// Only TCP to an attested endpoint is admitted; a socket path or another
// transport could reach a server the attestation never named.
func TestTestEnvDialGuardRefusesNonTcpNetworks(t *testing.T) {
	guard := newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), testEnvUnusedResolver(t))
	dial := guard.wrapDial("postgres", func(context.Context, string, string) (net.Conn, error) {
		t.Error("a non-TCP dial reached the dialer")
		return nil, errors.New("unexpected dial")
	})
	for _, target := range [][2]string{{"unix", "/tmp/.s.PGSQL.5432"}, {"udp", "192.0.2.10:5432"}} {
		if _, err := dial(t.Context(), target[0], target[1]); err == nil || !strings.Contains(err.Error(), "only tcp") {
			t.Errorf("%s dial = %v; want a transport refusal", target[0], err)
		}
	}
}

// The pools keep the guard chosen at startup when the environment changes
// mid-run, as the proxy tests' temporary WARP_BLOCK does. A constructor that
// re-derived its guard from the environment would find none here, dial the
// host resolver instead, and get no such host rather than the guard's refusal.
func TestPoolsKeepStartupGuardAfterEnvironmentChanges(t *testing.T) {
	pushTestEnvGuardPgResources(t)
	popVault := Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 192.0.2.10:6379\npassword: \"\"\ndb: 3\n"))
	defer popVault()
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
	defer popConfig()
	resolver := useTestEnvUnattestedNameGuard(t, "pool.fixture.invalid")
	t.Setenv("WARP_SERVICE", "synthetic")
	t.Setenv("WARP_BLOCK", "synthetic")
	t.Setenv(testEnvAttestedAddrsEnvName, "")

	redisPool := &safeRedisClient{}
	defer redisPool.close()
	_, err := redisPool.open().(*redis.Client).Options().Dialer(t.Context(), "tcp", "pool.fixture.invalid:16379")
	if err == nil || !strings.Contains(err.Error(), "pool.fixture.invalid resolved to 198.51.100.20:16379") || resolver.lookupCount() != 1 {
		t.Fatalf("redis pool dial after the environment changed = %v; want the startup guard's refusal", err)
	}
	pgPool := &safePgPool{ctx: context.Background(), vaultResourceName: DefaultPgVaultResourceName, configResourceName: DefaultPgConfigResourceName}
	defer pgPool.reset()
	_, err = pgPool.open().Config().ConnConfig.DialFunc(t.Context(), "tcp", "pool.fixture.invalid:15432")
	if err == nil || !strings.Contains(err.Error(), "pool.fixture.invalid resolved to 198.51.100.20:15432") || resolver.lookupCount() != 2 {
		t.Fatalf("postgres pool dial after the environment changed = %v; want the startup guard's refusal", err)
	}
}

// A refusal says what would admit the dial. Restarting the launcher fixes only
// a name that moved off its address; an in-process loopback endpoint needs its
// own listener admitted, and another literal was never the launcher's.
func TestTestEnvRefusalAdvisesByTarget(t *testing.T) {
	resolver := &testEnvScriptedResolver{host: testEnvFixtureRedisHost, answers: [][]netip.Addr{{testEnvLoopbackAddr}}}
	guard := newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), resolver.lookupNetIp)
	dial := guard.wrapDial("postgres", func(context.Context, string, string) (net.Conn, error) {
		t.Error("a refused endpoint reached the dialer")
		return nil, errors.New("unexpected dial")
	})
	movedName := net.JoinHostPort(testEnvFixtureRedisHost, "16379")
	cases := []struct {
		address string
		advice  string
	}{
		{address: "127.0.0.1:54321", advice: "admit an in-process listener this test owns with Testing_AdmitTestEnvLoopbackListener"},
		{address: "198.51.100.30:15432", advice: "a local process dials only attested endpoints"},
		{address: movedName, advice: "the local service names can fall through to 127.0.0.1"},
	}
	for _, c := range cases {
		_, err := dial(t.Context(), "tcp", c.address)
		if err == nil || !strings.Contains(err.Error(), c.advice) {
			t.Errorf("refusal of %s = %v; want advice %q", c.address, err, c.advice)
			continue
		}
		if restart := strings.Contains(err.Error(), "restart ./local/run-local.sh"); restart != (c.address == movedName) {
			t.Errorf("refusal of %s = %v; restarting the launcher fixes only a moved name", c.address, err)
		}
	}
}

// An in-process proxy that the pools are pointed at, as the model fixtures do:
// its own live loopback listener is admitted for the test and reached through
// the real PostgreSQL pool path, while the same endpoint before admission,
// after the test's cleanup, or once the listener closes is refused.
func TestTestEnvAdmitsOnlyLiveOwnedLoopbackListener(t *testing.T) {
	guard := newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), testEnvUnusedResolver(t))
	useTestEnvProcessDialGuard(t, guard)
	pushTestEnvGuardPgResources(t)
	pool := &safePgPool{ctx: context.Background(), vaultResourceName: DefaultPgVaultResourceName, configResourceName: DefaultPgConfigResourceName}
	defer pool.reset()
	config := pool.open().Config().ConnConfig
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := listener.Addr().String()

	if _, err := config.DialFunc(t.Context(), "tcp", address); err == nil {
		t.Fatal("an unadmitted in-process listener was dialed")
	}
	runScopedChildTest(t, "admitted proxy", func(scoped testing.TB) {
		Testing_AdmitTestEnvLoopbackListener(scoped, listener)
		addresses, err := config.LookupFunc(t.Context(), "127.0.0.1")
		if err != nil || !slices.Equal(addresses, []string{"127.0.0.1"}) {
			t.Fatalf("admitted proxy lookup = %q, %v", addresses, err)
		}
		conn, err := config.DialFunc(t.Context(), "tcp", address)
		if err != nil {
			t.Fatalf("admitted proxy dial: %v", err)
		}
		conn.Close()
		// One exact endpoint is admitted, never its address's other ports.
		port := listener.Addr().(*net.TCPAddr).Port
		neighbor := net.JoinHostPort("127.0.0.1", fmt.Sprint(port+1))
		if _, err := guard.wrapDial("postgres", func(context.Context, string, string) (net.Conn, error) {
			t.Error("a neighboring loopback port reached the dialer")
			return nil, errors.New("unexpected dial")
		})(t.Context(), "tcp", neighbor); err == nil {
			t.Fatalf("admission of %s also admitted %s", address, neighbor)
		}
	})
	if _, err := config.DialFunc(t.Context(), "tcp", address); err == nil {
		t.Fatal("an admission outlived its test")
	}
	runScopedChildTest(t, "closed proxy", func(scoped testing.TB) {
		Testing_AdmitTestEnvLoopbackListener(scoped, listener)
		listener.Close()
		if _, err := config.DialFunc(t.Context(), "tcp", address); err == nil {
			t.Fatal("a closed listener's endpoint stayed admitted")
		}
	})
	if _, err := guard.admit(listener); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("admission of a closed listener = %v; want refused", err)
	}
	if _, err := guard.admit(testEnvFakeListener{}); err == nil || !strings.Contains(err.Error(), "only a TCP listener") {
		t.Fatalf("admission of a non-TCP listener = %v; want refused", err)
	}
}

// Stands in for a listener that is not a real TCP socket.
type testEnvFakeListener struct{}

// Never yields a connection.
func (self testEnvFakeListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }

// Holds nothing to close.
func (self testEnvFakeListener) Close() error { return nil }

// Claims a loopback endpoint it does not hold.
func (self testEnvFakeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 16379}
}

// The production data tunnel's loopback ports are never admitted: PostgreSQL,
// PgBouncer and every Redis cluster node port the tunnel forwards.
func TestTestEnvLoopbackTunnelPorts(t *testing.T) {
	for _, port := range []uint16{5432, 6432, 6379, 6380, 6411, 6412, 6427} {
		if !testEnvLoopbackTunnelPort(port) {
			t.Errorf("tunnel port %d was not recognized", port)
		}
	}
	for _, port := range []uint16{5431, 5433, 6378, 6428, 6431, 6433, 15432, 16379, 35431, 36371} {
		if testEnvLoopbackTunnelPort(port) {
			t.Errorf("port %d was taken for a tunnel port", port)
		}
	}
}

// A proxy's upstream dial goes through the process guard: an endpoint outside
// the attestation is refused before a socket opens, and outside a test-env
// process the same call is an ordinary dial.
func TestTestingDialTestEnvGuardsProxyUpstream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), testEnvUnusedResolver(t)))
	for _, address := range []string{"198.51.100.30:15432", listener.Addr().String()} {
		if conn, err := Testing_DialTestEnv(t.Context(), "postgres", "tcp", address); err == nil {
			conn.Close()
			t.Errorf("proxy upstream %s was dialed outside the attestation", address)
		}
	}
	useTestEnvProcessDialGuard(t, nil)
	conn, err := Testing_DialTestEnv(t.Context(), "postgres", "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("ordinary proxy upstream dial outside a test process: %v", err)
	}
	conn.Close()
}

// Every go-redis client this module builds dials through an explicit dialer:
// the guard's (configureTestEnvRedisDial here, server.Testing_DialTestEnv in
// another package), a pool's, or a test's synthetic or never-dialing one. A
// client left on go-redis's own dialer resolves its name on every dial outside
// the guard; two lease tests' db-0 clients did, which sends no SELECT and
// could run single-key writes on a production node. The source is the
// observable layer: those clients live in tests that need a live fixture, so
// a pure test can only find them here.
func TestRedisClientsDialThroughExplicitDialers(t *testing.T) {
	// Every package of the module, one directory at a time so a package's own
	// helpers resolve; testdata, hidden directories and nested modules are not
	// the module's packages.
	packagePaths := map[string][]string{}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path == "." {
				return nil
			}
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			packagePaths[filepath.Dir(path)] = append(packagePaths[filepath.Dir(path)], path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	var funcDecls map[string]*ast.FuncDecl
	literalHasDialer := func(literal *ast.CompositeLit) bool {
		for _, element := range literal.Elts {
			if keyValue, ok := element.(*ast.KeyValueExpr); ok {
				if key, ok := keyValue.Key.(*ast.Ident); ok && key.Name == "Dialer" {
					return true
				}
			}
		}
		return false
	}
	configuresGuard := func(call *ast.CallExpr, name string) bool {
		function, ok := call.Fun.(*ast.Ident)
		if !ok || function.Name != "configureTestEnvRedisDial" || len(call.Args) != 3 {
			return false
		}
		argument := call.Args[1]
		if unary, ok := argument.(*ast.UnaryExpr); ok {
			argument = unary.X
		}
		ident, ok := argument.(*ast.Ident)
		return ok && (name == "" || ident.Name == name)
	}
	assignsDialer := func(assign *ast.AssignStmt, name string) bool {
		for _, lhs := range assign.Lhs {
			if selector, ok := lhs.(*ast.SelectorExpr); ok && selector.Sel.Name == "Dialer" {
				if ident, ok := selector.X.(*ast.Ident); ok && (name == "" || ident.Name == name) {
					return true
				}
			}
		}
		return false
	}
	// A local helper that builds options gives them a dialer in its body.
	helperGivesDialer := func(name string) bool {
		funcDecl := funcDecls[name]
		if funcDecl == nil || funcDecl.Body == nil {
			return false
		}
		found := false
		ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CompositeLit:
				found = found || literalHasDialer(n)
			case *ast.AssignStmt:
				found = found || assignsDialer(n, "")
			case *ast.CallExpr:
				found = found || configuresGuard(n, "")
			}
			return !found
		})
		return found
	}
	// Whether expr yields options that dial explicitly, where body is the
	// enclosing function an identifier is defined and configured in.
	var givesDialer func(body *ast.BlockStmt, expr ast.Expr, depth int) bool
	givesDialer = func(body *ast.BlockStmt, expr ast.Expr, depth int) bool {
		if 8 < depth {
			return false
		}
		switch e := expr.(type) {
		case *ast.ParenExpr:
			return givesDialer(body, e.X, depth+1)
		case *ast.UnaryExpr:
			return givesDialer(body, e.X, depth+1)
		case *ast.CompositeLit:
			return literalHasDialer(e)
		case *ast.CallExpr:
			function, ok := e.Fun.(*ast.Ident)
			return ok && helperGivesDialer(function.Name)
		case *ast.Ident:
			found := false
			ast.Inspect(body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.AssignStmt:
					found = found || assignsDialer(n, e.Name)
					if len(n.Lhs) == len(n.Rhs) {
						for index, lhs := range n.Lhs {
							if ident, ok := lhs.(*ast.Ident); ok && ident.Name == e.Name && n.Rhs[index] != expr {
								found = found || givesDialer(body, n.Rhs[index], depth+1)
							}
						}
					}
				case *ast.CallExpr:
					found = found || configuresGuard(n, e.Name)
				}
				return !found
			})
			return found
		}
		return false
	}
	constructors := map[string]bool{
		"NewClient": true, "NewClusterClient": true, "NewUniversalClient": true,
		"NewFailoverClient": true, "NewFailoverClusterClient": true, "NewRing": true,
	}
	checked, checkedOutsideRoot := 0, 0
	for _, directory := range slices.Sorted(maps.Keys(packagePaths)) {
		files := []*ast.File{}
		funcDecls = map[string]*ast.FuncDecl{}
		for _, path := range packagePaths[directory] {
			file, err := parser.ParseFile(fileSet, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file)
			for _, decl := range file.Decls {
				if funcDecl, ok := decl.(*ast.FuncDecl); ok && funcDecl.Recv == nil {
					funcDecls[funcDecl.Name.Name] = funcDecl
				}
			}
		}
		for _, file := range files {
			for _, decl := range file.Decls {
				funcDecl, ok := decl.(*ast.FuncDecl)
				if !ok || funcDecl.Body == nil {
					continue
				}
				ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					selector, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !constructors[selector.Sel.Name] {
						return true
					}
					if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "redis" || len(call.Args) != 1 {
						return true
					}
					checked += 1
					if directory != "." {
						checkedOutsideRoot += 1
					}
					if !givesDialer(funcDecl.Body, call.Args[0], 0) {
						t.Errorf("%s: redis.%s leaves the client on go-redis's own dialer; route it through the process "+
							"guard (configureTestEnvRedisDial(currentTestEnvDialGuard(), options, NewDialer(timeout).DialContext) "+
							"in this package, server.Testing_DialTestEnv elsewhere) or give it an explicit Dialer",
							fileSet.Position(call.Pos()), selector.Sel.Name)
					}
					return true
				})
			}
		}
	}
	if checked < 20 || checkedOutsideRoot < 2 {
		t.Fatalf("found %d go-redis client constructions, %d outside the server package; the scan no longer sees the module",
			checked, checkedOutsideRoot)
	}
}

// Installs a process guard whose scripted resolver answers an unattested
// documentation address for one .invalid name. A wiring that bypassed the
// guard would ask the host resolver instead, get no such host, and open no
// socket.
func useTestEnvUnattestedNameGuard(t *testing.T, host string) *testEnvScriptedResolver {
	t.Helper()
	resolver := &testEnvScriptedResolver{host: host, answers: [][]netip.Addr{{testEnvUnattestedAddr}}}
	useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), resolver.lookupNetIp))
	return resolver
}

// Runs a fixture function that raises on failure and returns what it raised.
func testEnvRecoverRaised(run func()) (raised any) {
	defer func() {
		raised = recover()
	}()
	run()
	return nil
}

// The lease coordinator, the db-0 client of the incident, dials through the
// process guard.
func TestRedisLeaseCoordinatorDialsThroughProcessGuard(t *testing.T) {
	resolver := useTestEnvUnattestedNameGuard(t, "redis.fixture.invalid")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	raised := testEnvRecoverRaised(func() {
		acquireTestRedisDbLease(ctx, "redis.fixture.invalid:16379", "", 0, "synthetic-token", 0, time.Minute)
	})
	if !strings.Contains(fmt.Sprint(raised), "redis.fixture.invalid resolved to 198.51.100.20:16379") || resolver.lookupCount() == 0 {
		t.Fatalf("lease coordinator raised %v after %d guard lookups; want the guard's refusal", raised, resolver.lookupCount())
	}
}

// Integration setup pins its PostgreSQL resource through the process guard,
// before any orphan sweep, CREATE DATABASE or Redis lease.
func TestTestEnvSetupResolvesThroughProcessGuard(t *testing.T) {
	childTestResources(t)
	t.Setenv("BRINGYOUR_POSTGRES_HOSTNAME", "pg.fixture.invalid")
	resolver := useTestEnvUnattestedNameGuard(t, "pg.fixture.invalid")
	raised := testEnvRecoverRaised(func() {
		teardown := (&TestEnv{ApplyDbMigrations: false}).setup()
		teardown()
	})
	if !strings.Contains(fmt.Sprint(raised), "pg.fixture.invalid resolved to 198.51.100.20") || resolver.lookupCount() != 1 {
		t.Fatalf("setup raised %v after %d guard lookups; want the guard's refusal", raised, resolver.lookupCount())
	}
}

// The database owner session keeps the guarded pool dialer and resolves its
// host through the process guard.
func TestTestPgDbLeaseResolvesThroughProcessGuard(t *testing.T) {
	childTestResources(t)
	t.Setenv("BRINGYOUR_POSTGRES_HOSTNAME", "pg.fixture.invalid")
	resolver := useTestEnvUnattestedNameGuard(t, "pg.fixture.invalid")
	PgReset()
	t.Cleanup(PgReset)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	raised := testEnvRecoverRaised(func() {
		closePgConnection(ctx, acquireTestPgDbLease(ctx, childTestDatabase))
	})
	if !strings.Contains(fmt.Sprint(raised), "pg.fixture.invalid resolved to 198.51.100.20") || resolver.lookupCount() != 1 {
		t.Fatalf("owner session raised %v after %d guard lookups; want the guard's refusal", raised, resolver.lookupCount())
	}
}

// A child process qualifies its inherited PostgreSQL endpoint through the
// process guard. The child reports only that the endpoint differs, so the
// guard's own resolver is the observable boundary.
func TestTestEnvironmentChildValidationResolvesThroughProcessGuard(t *testing.T) {
	pg, redisResource := childTestResources(t)
	t.Setenv("BRINGYOUR_POSTGRES_HOSTNAME", "pg.fixture.invalid")
	resolver := useTestEnvUnattestedNameGuard(t, "pg.fixture.invalid")
	err := ValidateTestEnvironmentChildResources(t.Context(), childTestWire(t, pg), childTestWire(t, redisResource))
	if err == nil || resolver.lookupCount() != 1 {
		t.Fatalf("child validation = %v after %d guard lookups; want it refused through the guard", err, resolver.lookupCount())
	}
}

// A child's read of the parent's lease dials through the process guard.
func TestTestEnvironmentChildLeaseReadDialsThroughProcessGuard(t *testing.T) {
	resolver := useTestEnvUnattestedNameGuard(t, "redis.fixture.invalid")
	_, err := readTestEnvironmentChildRedisLease(t.Context(), testEnvironmentChildResources{
		redisAuthority: "redis.fixture.invalid:16379",
		redisDatabase:  1,
	})
	if err == nil || !strings.Contains(err.Error(), "redis.fixture.invalid resolved to 198.51.100.20:16379") || resolver.lookupCount() == 0 {
		t.Fatalf("child lease read = %v after %d guard lookups; want the guard's refusal", err, resolver.lookupCount())
	}
}

// A child's owner query resolves and dials through the process guard.
func TestTestEnvironmentChildOwnerQueryDialsThroughProcessGuard(t *testing.T) {
	resolver := useTestEnvUnattestedNameGuard(t, "pg.fixture.invalid")
	_, err := readTestEnvironmentChildPgOwner(t.Context(), testEnvironmentChildResources{
		postgresAuthority: "pg.fixture.invalid:15432",
		postgresUser:      "synthetic",
		postgresPassword:  "not-a-secret",
		postgresDatabase:  childTestDatabase,
	})
	if err == nil || !strings.Contains(err.Error(), "pg.fixture.invalid resolved to 198.51.100.20") || resolver.lookupCount() != 1 {
		t.Fatalf("child owner query = %v after %d guard lookups; want the guard's refusal", err, resolver.lookupCount())
	}
}

// Pushes a synthetic PostgreSQL resource whose pool opens no connection.
func pushTestEnvGuardPgResources(t *testing.T) {
	t.Helper()
	t.Cleanup(Vault.PushSimpleResource(DefaultPgVaultResourceName, []byte(
		"authority: 192.0.2.10:5432\nuser: synthetic\npassword: synthetic\ndb: synthetic\n",
	)))
	t.Cleanup(Config.PushSimpleResource(DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n")))
}

// The environment test-env.sh exports for the private portable root, whose
// services are explicit 127.0.0.1 authorities.
func testEnvPortableEnvironment(attestation string) map[string]string {
	return map[string]string{
		"WARP_ENV":                             "local",
		"WARP_SERVICE":                         "test",
		"WARP_BLOCK":                           "test",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES": "1",
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1",
		"WARP_TEST_ENV_PORTABLE_ROOT":                     "/synthetic/private-root",
		testEnvAttestedAddrsEnvName:                       attestation,
	}
}

// The environments the local entry points that are not tests run with: the
// cli/*/run-local.sh services, bringyourctl/run-local.sh, sim-latency's own
// defaults, and a bare WARP_ENV=local.
var testEnvLocalServiceEnvironments = []map[string]string{
	{"WARP_ENV": "local", "WARP_SERVICE": "api", "WARP_BLOCK": "local", "WARP_HOST": "127.0.0.1"},
	{"WARP_ENV": "local", "WARP_SERVICE": "connect", "WARP_BLOCK": "local", "WARP_HOST": "127.0.0.1"},
	{"WARP_ENV": "local", "WARP_SERVICE": "taskworker", "WARP_BLOCK": "local", "WARP_HOST": "127.0.0.1"},
	{"WARP_ENV": "local", "WARP_SERVICE": "proxy", "WARP_BLOCK": "local", "WARP_DOMAIN": "bringyour.com"},
	{"WARP_ENV": "local", "WARP_SERVICE": "bringyourctl", "WARP_BLOCK": "local", "WARP_HOST": "127.0.0.1"},
	{"WARP_ENV": "local", "WARP_SERVICE": "sim", "WARP_BLOCK": "sim", "WARP_HOST": "127.0.0.1"},
	{"WARP_ENV": "local"},
}

// Adds an attestation to a copy of one environment.
func testEnvWithAttestation(values map[string]string, attestation string) map[string]string {
	attested := map[string]string{testEnvAttestedAddrsEnvName: attestation}
	for name, value := range values {
		attested[name] = value
	}
	return attested
}

// The reported exposure: a local process that is not a test, started while the
// launcher is down or still running after it exited, resolves its local service
// name to 127.0.0.1, where the production data tunnel listens. Its Redis pool
// dial and its PostgreSQL connect must both be refused before any socket opens,
// with an error that tells the user to start the launcher.
func TestLocalServiceProcessRefusesNameFallingThroughToLoopback(t *testing.T) {
	for _, values := range testEnvLocalServiceEnvironments {
		redisResolver := &testEnvScriptedResolver{host: testEnvFixtureRedisHost, answers: [][]netip.Addr{{testEnvLoopbackAddr}}}
		dialer := newTestEnvRecordingDialer(t, redisResolver)
		authority := net.JoinHostPort(testEnvFixtureRedisHost, "6379")
		dial := redisPoolDialer(newTestEnvDialGuard(testEnvGetenv(values), redisResolver.lookupNetIp), authority, map[string]string{}, dialer.dial)
		conn, err := dial(t.Context(), "tcp", authority)
		if err == nil {
			conn.Close()
		}
		if connected := dialer.connectedEndpoints(); len(connected) != 0 {
			t.Errorf("%v: the Redis pool connected to %q; a local process without an attestation must not dial", values, connected)
		}
		if err == nil || !strings.Contains(err.Error(), testEnvAttestedAddrsEnvName+" is not set") ||
			!strings.Contains(err.Error(), "start ./local/run-local.sh") {
			t.Errorf("%v: Redis pool dial error = %v; want a refusal telling the user to start ./local/run-local.sh", values, err)
		}

		pgResolver := &testEnvScriptedResolver{host: testEnvFixturePgHost, answers: [][]netip.Addr{{testEnvLoopbackAddr}}}
		config, err := pgx.ParseConfig("postgres://synthetic@" + testEnvFixturePgHost + ":5432/synthetic?sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		// The scripted lookup and the recording dial stand in for net.Resolver
		// and net.Dialer; reaching the dialer ends the connect without a socket.
		dialed := []string{}
		config.LookupFunc = pgResolver.lookupHost
		config.DialFunc = func(_ context.Context, _ string, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return nil, errors.New("synthetic dial reached")
		}
		configurePgTestEnvDial(newTestEnvDialGuard(testEnvGetenv(values), pgResolver.lookupNetIp), config)
		_, err = pgx.ConnectConfig(t.Context(), config)
		if len(dialed) != 0 {
			t.Errorf("%v: PostgreSQL dialed %q; a local process without an attestation must not dial", values, dialed)
		}
		if err == nil || !strings.Contains(err.Error(), testEnvAttestedAddrsEnvName+" is not set") {
			t.Errorf("%v: PostgreSQL connect error = %v; want the missing attestation refused", values, err)
		}
	}
}

// local/local-env.sh exports the launcher's attestation for a process that is
// not a test. Such a process dials the attested endpoint while the launcher maps
// its name, and a redial after the name falls through to loopback is refused.
func TestAttestedLocalServiceRedialRefusesNameMovedToLoopback(t *testing.T) {
	for _, values := range testEnvLocalServiceEnvironments {
		resolver := &testEnvScriptedResolver{
			host:    testEnvFixtureRedisHost,
			answers: [][]netip.Addr{{testEnvFixtureAddr}, {testEnvLoopbackAddr}},
		}
		dialer := newTestEnvRecordingDialer(t, resolver)
		guard := newTestEnvDialGuard(testEnvGetenv(testEnvWithAttestation(values, testEnvFixtureAttestation)), resolver.lookupNetIp)
		authority := net.JoinHostPort(testEnvFixtureRedisHost, "6379")
		dial := redisPoolDialer(guard, authority, map[string]string{}, dialer.dial)
		conn, err := dial(t.Context(), "tcp", authority)
		if err != nil {
			t.Errorf("%v: dial while the name maps to the attested address: %v", values, err)
			continue
		}
		conn.Close()
		conn, err = dial(t.Context(), "tcp", authority)
		if err == nil {
			conn.Close()
		}
		if connected := dialer.connectedEndpoints(); !slices.Equal(connected, []string{"192.0.2.10:6379"}) {
			t.Errorf("%v: connected endpoints = %q; the redial after the name moved to loopback must not connect", values, connected)
		}
		if err == nil || !strings.Contains(err.Error(), "redis.fixture.example resolved to 127.0.0.1:6379, which the launcher did not attest") {
			t.Errorf("%v: loopback redial error = %v; want a refusal naming the host and its loopback address", values, err)
		}
	}
}

// A test binary, and any process that starts with WARP_ENV set, chooses its
// guard at startup; a WARP_ENV=main service locks in nil there. Only a service
// binary that starts without WARP_ENV defers the choice to its first guarded
// construction.
func TestStartupDialGuardChoice(t *testing.T) {
	cases := []struct {
		name       string
		testBinary bool
		values     map[string]string
		chosen     bool
		guarded    bool
	}{
		{name: "production service", values: map[string]string{"WARP_ENV": "main", "WARP_SERVICE": "api"}, chosen: true},
		{name: "other environment", values: map[string]string{"WARP_ENV": "sim-testnet-op-1"}, chosen: true},
		{name: "service without an environment", values: map[string]string{}},
		{name: "local service", values: map[string]string{"WARP_ENV": "local", "WARP_SERVICE": "api"}, chosen: true, guarded: true},
		{name: "test binary outside the local environment", testBinary: true, values: map[string]string{}, chosen: true},
		{name: "local test binary", testBinary: true, values: map[string]string{"WARP_ENV": "local"}, chosen: true, guarded: true},
	}
	for _, c := range cases {
		choice := chooseTestEnvStartupDialGuard(c.testBinary, c.values["WARP_ENV"], func() *testEnvDialGuard {
			return newTestEnvDialGuard(testEnvGetenv(c.values), testEnvUnusedResolver(t))
		})
		if (choice != nil) != c.chosen {
			t.Errorf("%s: chosen at startup = %t; want %t", c.name, choice != nil, c.chosen)
			continue
		}
		if choice != nil && (choice.guard != nil) != c.guarded {
			t.Errorf("%s: guarded = %t; want %t", c.name, choice.guard != nil, c.guarded)
		}
	}
}

// A service binary that starts with WARP_ENV=main locks in a nil guard at
// startup: a WARP_ENV=local written before its first pool open, by main or a
// library, cannot install a guard that would refuse every production dial.
func TestProductionServiceLocksNilGuardAtStartup(t *testing.T) {
	values := map[string]string{"WARP_ENV": "main", "WARP_SERVICE": "api"}
	newGuard := func() *testEnvDialGuard {
		return newTestEnvDialGuard(testEnvGetenv(values), testEnvUnusedResolver(t))
	}
	var choice atomic.Pointer[testEnvDialGuardChoice]
	choice.Store(chooseTestEnvStartupDialGuard(false, values["WARP_ENV"], newGuard))
	if choice.Load() == nil {
		t.Fatal("a WARP_ENV=main service deferred its guard choice past startup")
	}
	values["WARP_ENV"] = "local"
	if guard := chooseTestEnvDialGuard(&choice, newGuard); guard != nil {
		t.Fatalf("a WARP_ENV=local written after startup installed a guard on a production service: %v", guard.requireAttested())
	}
}

// sim-latency sets WARP_ENV=local in main, after package initialization, when
// it is started without one. A service binary therefore defers its choice, and
// its first pool open, after main, must be guarded; the choice then holds for
// the rest of the run.
func TestServiceBinaryChoosesGuardAfterMainSetsLocalEnvironment(t *testing.T) {
	values := map[string]string{}
	newGuard := func() *testEnvDialGuard {
		return newTestEnvDialGuard(testEnvGetenv(values), testEnvUnusedResolver(t))
	}
	var choice atomic.Pointer[testEnvDialGuardChoice]
	choice.Store(chooseTestEnvStartupDialGuard(false, values["WARP_ENV"], newGuard))

	// main's defaults (sim-latency setDefaultEnv)
	values["WARP_ENV"], values["WARP_SERVICE"], values["WARP_BLOCK"] = "local", "sim", "sim"
	guard := chooseTestEnvDialGuard(&choice, newGuard)
	if guard == nil {
		t.Fatal("a service binary that main put in the local environment opened its pools without a guard")
	}
	if err := guard.requireAttested(); err == nil || !strings.Contains(err.Error(), testEnvAttestedAddrsEnvName+" is not set") {
		t.Fatalf("attestation error = %v; want the missing attestation refused", err)
	}
	values["WARP_ENV"] = "main"
	if chooseTestEnvDialGuard(&choice, newGuard) != guard {
		t.Fatal("a later environment change moved the chosen guard")
	}
}

// A service binary that starts without WARP_ENV and whose main then selects a
// production environment chooses at its first guarded construction and gets a
// nil guard, so its pool dial and lookup stay the callers' own functions; a
// later environment change cannot add a guard to it.
func TestProductionServiceChoosesNilGuardAtFirstUse(t *testing.T) {
	values := map[string]string{}
	newGuard := func() *testEnvDialGuard {
		return newTestEnvDialGuard(testEnvGetenv(values), testEnvUnusedResolver(t))
	}
	var choice atomic.Pointer[testEnvDialGuardChoice]
	choice.Store(chooseTestEnvStartupDialGuard(false, values["WARP_ENV"], newGuard))
	if choice.Load() != nil {
		t.Fatal("a service binary without WARP_ENV chose its guard before main ran")
	}
	values["WARP_ENV"], values["WARP_SERVICE"], values["WARP_BLOCK"] = "main", "api", "g1"
	values[testEnvAttestedAddrsEnvName] = testEnvFixtureAttestation
	guard := chooseTestEnvDialGuard(&choice, newGuard)
	if guard != nil {
		t.Fatal("a production service got a dial guard")
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("synthetic unreachable")
	}
	if testEnvFuncPointer(guard.wrapDial("redis", dial)) != testEnvFuncPointer(dial) {
		t.Fatal("the production Redis pool dial is not the caller's own")
	}
	config, err := pgx.ParseConfig("postgres://synthetic@" + testEnvFixturePgHost + ":5432/synthetic?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	dialPointer, lookupPointer := testEnvFuncPointer(config.DialFunc), testEnvFuncPointer(config.LookupFunc)
	configurePgTestEnvDial(guard, config)
	if testEnvFuncPointer(config.DialFunc) != dialPointer || testEnvFuncPointer(config.LookupFunc) != lookupPointer {
		t.Fatal("the production PostgreSQL dial or lookup changed")
	}
	values["WARP_ENV"] = "local"
	if chooseTestEnvDialGuard(&choice, newGuard) != nil {
		t.Fatal("a later environment change added a guard to a production service")
	}
}

// No mode may attest a loopback port the production data tunnel forwards, the
// private portable root included: a portable profile pointed at 127.0.0.1:5432
// would otherwise reach production through the tunnel. Other loopback ports
// remain available to that root.
func TestTestEnvDialGuardRefusesLoopbackOnProductionTunnelPortsInEveryMode(t *testing.T) {
	modeEnvironments := map[string]func(string) map[string]string{
		"private portable root": testEnvPortableEnvironment,
		"managed launcher": func(attestation string) map[string]string {
			return map[string]string{"WARP_ENV": "local", "WARP_SERVICE": "test", "WARP_BLOCK": "test", testEnvAttestedAddrsEnvName: attestation}
		},
		"local service": func(attestation string) map[string]string {
			return map[string]string{"WARP_ENV": "local", "WARP_SERVICE": "api", "WARP_BLOCK": "local", testEnvAttestedAddrsEnvName: attestation}
		},
	}
	for mode, environment := range modeEnvironments {
		for _, endpoint := range []string{"127.0.0.1:5432", "127.0.0.1:6432", "127.0.0.1:6379", "127.0.0.1:6400", "127.0.0.1:6427", "[::1]:6379", "127.0.0.2:5432"} {
			guard := newTestEnvDialGuard(testEnvGetenv(environment("192.0.2.10:5432,"+endpoint)), testEnvUnusedResolver(t))
			if err := guard.requireAttested(); err == nil || !strings.Contains(err.Error(), "on a production data tunnel port") {
				t.Errorf("%s: attestation of %s = %v; want the tunnel port refused", mode, endpoint, err)
			}
		}
	}
	for _, endpoint := range []string{"127.0.0.1:6378", "127.0.0.1:6428", "127.0.0.1:36371", "192.0.2.10:6379"} {
		guard := newTestEnvDialGuard(testEnvGetenv(testEnvPortableEnvironment("127.0.0.1:35431,"+endpoint)), testEnvUnusedResolver(t))
		if err := guard.requireAttested(); err != nil {
			t.Errorf("private portable attestation of %s refused: %v", endpoint, err)
		}
	}
}

// A destructive local operation (sim-latency's reset) runs only in a local
// process with a valid attestation: production and every other environment
// are refused, and so is a local process that was never attested.
func TestDestructiveLocalEndpointsRequireAttestedLocalProcess(t *testing.T) {
	endpoints := []testEnvServiceEndpoint{
		{service: "postgres", authority: "192.0.2.10:5432"},
		{service: "redis", authority: "192.0.2.10:6379"},
	}
	var production *testEnvDialGuard
	if err := production.requireDestructiveEndpoints(t.Context(), endpoints); err == nil ||
		!strings.Contains(err.Error(), "not a WARP_ENV=local process") {
		t.Errorf("destructive check outside the local environment = %v; want a refusal", err)
	}
	for _, values := range testEnvLocalServiceEnvironments {
		unattested := newTestEnvDialGuard(testEnvGetenv(values), testEnvUnusedResolver(t))
		if err := unattested.requireDestructiveEndpoints(t.Context(), endpoints); err == nil ||
			!strings.Contains(err.Error(), testEnvAttestedAddrsEnvName+" is not set") {
			t.Errorf("%v: destructive check without an attestation = %v; want a refusal naming the attestation", values, err)
		}
		attested := newTestEnvDialGuard(testEnvGetenv(testEnvWithAttestation(values, testEnvFixtureAttestation)), testEnvUnusedResolver(t))
		if err := attested.requireDestructiveEndpoints(t.Context(), endpoints); err != nil {
			t.Errorf("%v: destructive check of attested endpoints refused: %v", values, err)
		}
	}
}

// With a managed attestation, any endpoint outside it is refused before the
// operation runs: a name that fell through to loopback, a loopback literal such
// as a forwarded production port, another address, or another port.
func TestDestructiveLocalEndpointsRefuseUnattestedAndLoopbackEndpoints(t *testing.T) {
	cases := []struct {
		endpoint  testEnvServiceEndpoint
		errorText string
	}{
		{
			endpoint:  testEnvServiceEndpoint{service: "postgres", authority: net.JoinHostPort(testEnvFixturePgHost, "5432")},
			errorText: "pg.fixture.example resolved to 127.0.0.1:5432, which the launcher did not attest",
		},
		{endpoint: testEnvServiceEndpoint{service: "postgres", authority: "127.0.0.1:5432"}, errorText: "production data tunnel"},
		{endpoint: testEnvServiceEndpoint{service: "redis", authority: "127.0.0.1:6379"}, errorText: "production data tunnel"},
		{endpoint: testEnvServiceEndpoint{service: "redis", authority: "[::1]:6379"}, errorText: "production data tunnel"},
		{endpoint: testEnvServiceEndpoint{service: "redis", authority: "192.0.2.10:6380"}, errorText: "resolved to 192.0.2.10:6380"},
		{endpoint: testEnvServiceEndpoint{service: "postgres", authority: "198.51.100.7:5432"}, errorText: "resolved to 198.51.100.7:5432"},
	}
	for _, c := range cases {
		resolver := &testEnvScriptedResolver{host: testEnvFixturePgHost, answers: [][]netip.Addr{{testEnvLoopbackAddr}}}
		guard := newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), resolver.lookupNetIp)
		endpoints := []testEnvServiceEndpoint{{service: "redis", authority: "192.0.2.10:6379"}, c.endpoint}
		err := guard.requireDestructiveEndpoints(t.Context(), endpoints)
		if err == nil || !strings.Contains(err.Error(), "destructive local operation refused") || !strings.Contains(err.Error(), c.errorText) {
			t.Errorf("%s endpoint %s: destructive check = %v; want a refusal containing %q", c.endpoint.service, c.endpoint.authority, err, c.errorText)
		}
	}
}

// Loopback passes the destructive check only as the private portable root's
// own attested endpoints. A guard admitting loopback outside that root could
// only come from a broken constructor, and the check refuses it on its own.
func TestDestructiveLocalEndpointsAdmitLoopbackOnlyInPortableMode(t *testing.T) {
	endpoints := []testEnvServiceEndpoint{
		{service: "postgres", authority: "127.0.0.1:35431"},
		{service: "redis", authority: "127.0.0.1:36371"},
	}
	portable := newTestEnvDialGuard(testEnvGetenv(testEnvPortableEnvironment("127.0.0.1:35431,127.0.0.1:36371")), testEnvUnusedResolver(t))
	if err := portable.requireDestructiveEndpoints(t.Context(), endpoints); err != nil {
		t.Fatalf("destructive check of the attested portable endpoints refused: %v", err)
	}
	broken := &testEnvDialGuard{
		attestedAddrPorts: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:35431"), netip.MustParseAddrPort("127.0.0.1:36371")},
		lookupNetIp:       testEnvUnusedResolver(t),
	}
	if err := broken.requireDestructiveEndpoints(t.Context(), endpoints); err == nil || !strings.Contains(err.Error(), "is loopback") {
		t.Fatalf("destructive check of loopback outside the portable mode = %v; want a refusal", err)
	}
}

// The exported check reads every PostgreSQL pool's own vault resource, the
// maintenance pool's included, and the Redis resource, and checks them against
// the process guard.
func TestRequireAttestedLocalEndpointsChecksEveryPoolResource(t *testing.T) {
	pushTestEnvGuardPgResources(t)
	t.Cleanup(Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 192.0.2.10:6379\npassword: \"\"\ndb: 0\n")))
	useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), testEnvUnusedResolver(t)))
	if err := RequireAttestedLocalEndpoints(t.Context()); err != nil {
		t.Fatalf("destructive check of attested pool resources refused: %v", err)
	}

	popMaintenance := Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte(
		"authority: 127.0.0.1:5432\nuser: synthetic\npassword: synthetic\ndb: synthetic\n",
	))
	err := RequireAttestedLocalEndpoints(t.Context())
	popMaintenance()
	if err == nil || !strings.Contains(err.Error(), "resolved to 127.0.0.1:5432") {
		t.Fatalf("destructive check with a loopback maintenance resource = %v; want a refusal", err)
	}

	popRedis := Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 127.0.0.1:6379\npassword: \"\"\ndb: 0\n"))
	err = RequireAttestedLocalEndpoints(t.Context())
	popRedis()
	if err == nil || !strings.Contains(err.Error(), "resolved to 127.0.0.1:6379") {
		t.Fatalf("destructive check with a loopback Redis resource = %v; want a refusal", err)
	}

	useTestEnvProcessDialGuard(t, nil)
	if err := RequireAttestedLocalEndpoints(t.Context()); err == nil || !strings.Contains(err.Error(), "not a WARP_ENV=local process") {
		t.Fatalf("destructive check without a local guard = %v; want a refusal", err)
	}
}

// A PostgreSQL connection that a tool or fixture configures itself, outside the
// pools, is pinned exactly like a pool's in a local process: a name that falls
// through to loopback is refused before any socket. In production the
// configuration keeps the caller's own dial and lookup.
func TestConfigureLocalPgDialPinsOnlyLocalProcesses(t *testing.T) {
	config, err := pgx.ParseConfig("postgres://synthetic@" + testEnvFixturePgHost + ":5432/synthetic?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	dialPointer, lookupPointer := testEnvFuncPointer(config.DialFunc), testEnvFuncPointer(config.LookupFunc)
	useTestEnvProcessDialGuard(t, nil)
	ConfigureLocalPgDial(config)
	if testEnvFuncPointer(config.DialFunc) != dialPointer || testEnvFuncPointer(config.LookupFunc) != lookupPointer {
		t.Fatal("a production tool connection changed its dial or lookup")
	}

	for _, values := range testEnvLocalServiceEnvironments {
		resolver := &testEnvScriptedResolver{host: testEnvFixturePgHost, answers: [][]netip.Addr{{testEnvLoopbackAddr}}}
		useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvGetenv(testEnvWithAttestation(values, testEnvFixtureAttestation)), resolver.lookupNetIp))
		config, err := pgx.ParseConfig("postgres://synthetic@" + testEnvFixturePgHost + ":5432/synthetic?sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		dialed := []string{}
		config.LookupFunc = resolver.lookupHost
		config.DialFunc = func(_ context.Context, _ string, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return nil, errors.New("synthetic dial reached")
		}
		ConfigureLocalPgDial(config)
		_, err = pgx.ConnectConfig(t.Context(), config)
		if len(dialed) != 0 {
			t.Errorf("%v: a tool connection dialed %q after its name fell through to loopback", values, dialed)
		}
		if err == nil || !strings.Contains(err.Error(), "pg.fixture.example resolved to 127.0.0.1, which the launcher did not attest") {
			t.Errorf("%v: tool connect error = %v; want the guard's refusal", values, err)
		}
	}
}

// The destructive check trusts only the attestation. A loopback listener a test
// admitted is dialable through the guard, but it never qualifies an endpoint
// for a destructive operation such as sim-latency's reset.
func TestDestructiveLocalEndpointsIgnoreAdmittedListeners(t *testing.T) {
	guard := newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), testEnvUnusedResolver(t))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	remove, err := guard.admit(listener)
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	address := listener.Addr().String()
	addrPort := listener.Addr().(*net.TCPAddr).AddrPort()
	if !guard.admits(netip.AddrPortFrom(addrPort.Addr().Unmap(), addrPort.Port())) {
		t.Fatal("the admitted listener is not dialable through the guard")
	}
	endpoints := []testEnvServiceEndpoint{
		{service: "postgres", authority: "192.0.2.10:5432"},
		{service: "redis", authority: address},
	}
	if err := guard.requireDestructiveEndpoints(t.Context(), endpoints); err == nil ||
		!strings.Contains(err.Error(), "never an admitted test listener") {
		t.Fatalf("destructive check of an admitted listener = %v; want a refusal", err)
	}
}

// A host that resolves to no address, without a resolver error, is refused
// with that reason, and a resolver failure keeps its cause; neither leaves a
// nil error formatted into the refusal.
func TestDestructiveLocalEndpointsNameUnresolvedHosts(t *testing.T) {
	failure := errors.New("synthetic resolver failure")
	cases := []struct {
		lookupNetIp func(context.Context, string, string) ([]netip.Addr, error)
		errorText   string
	}{
		{
			lookupNetIp: func(context.Context, string, string) ([]netip.Addr, error) { return nil, nil },
			errorText:   "postgres host pg.fixture.example resolved to no address",
		},
		{
			lookupNetIp: func(context.Context, string, string) ([]netip.Addr, error) { return nil, failure },
			errorText:   "postgres host pg.fixture.example did not resolve: synthetic resolver failure",
		},
	}
	for _, c := range cases {
		guard := newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), c.lookupNetIp)
		err := guard.requireDestructiveEndpoints(t.Context(), []testEnvServiceEndpoint{
			{service: "postgres", authority: net.JoinHostPort(testEnvFixturePgHost, "5432")},
		})
		if err == nil || !strings.Contains(err.Error(), c.errorText) || strings.Contains(err.Error(), "%!") {
			t.Errorf("destructive check = %v; want a refusal containing %q", err, c.errorText)
		}
	}
}
