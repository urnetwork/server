// Pure checks that a local test process dials PostgreSQL and Redis only at the
// endpoints test-env.sh attested. Every socket is a net.Pipe or a recording
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
	"net"
	"net/netip"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
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
// guard afterwards. Callers must not run in parallel.
func useTestEnvProcessDialGuard(t *testing.T, guard *testEnvDialGuard) {
	t.Helper()
	previous := testEnvProcessDialGuard.Swap(guard)
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

// Only a local test-env process gets a guard. Production, local services and a
// lone marker get none, even with a stray attestation in a production
// environment, so their dial paths cannot change.
func TestTestEnvDialGuardSelectsOnlyLocalTestProcesses(t *testing.T) {
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
		{name: "local service", values: map[string]string{"WARP_ENV": "local", "WARP_SERVICE": "api", "WARP_BLOCK": "local"}},
		{name: "service marker only", values: map[string]string{"WARP_ENV": "local", "WARP_SERVICE": "test"}},
		{name: "block marker only", values: map[string]string{"WARP_ENV": "local", "WARP_BLOCK": "test"}},
		{name: "markers without attestation", values: map[string]string{"WARP_ENV": "local", "WARP_SERVICE": "test", "WARP_BLOCK": "test"}, guarded: true},
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
		testEnvFuncPointer(guard.wrapLookup("postgres", lookup)) != testEnvFuncPointer(lookup) {
		t.Fatal("a nil guard replaced the caller's dial or lookup")
	}
	authority := net.JoinHostPort(testEnvFixtureRedisHost, "6379")
	if _, err := redisPoolDialer(guard, authority, map[string]string{}, dial)(t.Context(), "tcp", authority); !errors.Is(err, unreachable) {
		t.Fatalf("unguarded pool dial error = %v; want the caller's own result", err)
	}
	addresses, err := guard.wrapLookup("postgres", lookup)(t.Context(), testEnvFixturePgHost)
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
	resolver := &testEnvScriptedResolver{host: "pg.fixture.invalid", answers: [][]netip.Addr{{testEnvLoopbackAddr}}}
	useTestEnvProcessDialGuard(t, newTestEnvDialGuard(testEnvManagedEnvironment(testEnvFixtureAttestation), resolver.lookupNetIp))
	pool := &safePgPool{ctx: context.Background(), vaultResourceName: DefaultPgVaultResourceName, configResourceName: DefaultPgConfigResourceName}
	defer pool.reset()
	_, err := pool.open().Config().ConnConfig.DialFunc(t.Context(), "tcp", "pg.fixture.invalid:5432")
	if err == nil || !strings.Contains(err.Error(), "pg.fixture.invalid resolved to 127.0.0.1:5432") || resolver.lookupCount() != 1 {
		t.Fatalf("pool dial error = %v after %d lookups; want the process guard's refusal", err, resolver.lookupCount())
	}
}

// Every Redis pool constructor dials through the process guard: a name that
// resolves outside the attestation is refused before a socket opens.
func TestRedisPoolOpenDialsThroughProcessGuard(t *testing.T) {
	resolver := &testEnvScriptedResolver{host: "redis.fixture.invalid", answers: [][]netip.Addr{{testEnvLoopbackAddr}}}
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
		_, err := client.Options().Dialer(t.Context(), "tcp", "redis.fixture.invalid:6379")
		pool.close()
		if err == nil || !strings.Contains(err.Error(), "redis.fixture.invalid resolved to 127.0.0.1:6379") {
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
		postgresAuthority: "198.51.100.7:5432",
		postgresUser:      "synthetic",
		postgresPassword:  "not-a-secret",
		postgresDatabase:  "synthetic",
		redisAuthority:    "198.51.100.7:6379",
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

// The explicit portable-services mode attests its own 127.0.0.1 endpoints and
// may dial exactly those. Every other loopback endpoint, such as a forwarded
// production database port, stays refused.
func TestTestEnvDialGuardAdmitsOnlyAttestedPortableLoopbackEndpoints(t *testing.T) {
	guard := newTestEnvDialGuard(testEnvGetenv(map[string]string{
		"WARP_ENV":                             "local",
		"WARP_SERVICE":                         "test",
		"WARP_BLOCK":                           "test",
		"WARP_TEST_ENV_USE_PORTABLE_RESOURCES": "1",
		"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1",
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

// Outside the explicit portable mode a loopback attestation is itself invalid,
// so a managed or half-portable profile can never admit loopback.
func TestTestEnvDialGuardRefusesLoopbackAttestationOutsidePortableMode(t *testing.T) {
	for _, flags := range []map[string]string{
		{},
		{"WARP_TEST_ENV_USE_PORTABLE_RESOURCES": "1"},
		{"WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES": "1"},
	} {
		values := map[string]string{
			"WARP_ENV":                  "local",
			"WARP_SERVICE":              "test",
			"WARP_BLOCK":                "test",
			testEnvAttestedAddrsEnvName: "192.0.2.10:5432,127.0.0.1:6379",
		}
		for name, value := range flags {
			values[name] = value
		}
		guard := newTestEnvDialGuard(testEnvGetenv(values), testEnvUnusedResolver(t))
		err := guard.requireAttested()
		if err == nil || !strings.Contains(err.Error(), "attests loopback endpoint 127.0.0.1:6379") {
			t.Errorf("flags %v: attestation error = %v; want the loopback attestation refused", flags, err)
			continue
		}
		if _, err := guard.wrapDial("redis", nil)(t.Context(), "tcp", "192.0.2.10:5432"); err == nil {
			t.Errorf("flags %v: an invalid attestation still admitted a dial", flags)
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
	if _, err := guard.wrapLookup("redis", resolver.lookupHost)(t.Context(), testEnvFixtureRedisHost); err == nil ||
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

// The process guard is chosen once at startup; later environment changes, such
// as the proxy tests' temporary WARP_BLOCK, cannot remove it mid-run.
func TestTestEnvProcessDialGuardIgnoresLaterEnvironmentChanges(t *testing.T) {
	before := currentTestEnvDialGuard()
	t.Setenv("WARP_ENV", "main")
	t.Setenv("WARP_BLOCK", "synthetic")
	t.Setenv(testEnvAttestedAddrsEnvName, "")
	if currentTestEnvDialGuard() != before {
		t.Fatal("an environment change moved the process guard")
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
		{address: "198.51.100.30:15432", advice: "a test process dials only attested endpoints"},
		{address: movedName, advice: "public DNS answers the local service names with 127.0.0.1"},
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

// Every go-redis client this package builds dials through an explicit dialer:
// the guard's (configureTestEnvRedisDial), a pool's, or a test's synthetic or
// never-dialing one. A client left on go-redis's own dialer resolves its name
// on every dial outside the guard; two lease tests' db-0 clients did, which
// sends no SELECT and could run single-key writes on a production node. The
// source is the observable layer: those clients live in tests that need a
// live fixture, so a pure test can only find them here.
func TestRedisClientsDialThroughExplicitDialers(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	funcDecls := map[string]*ast.FuncDecl{}
	for _, path := range paths {
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
	checked := 0
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
				if !givesDialer(funcDecl.Body, call.Args[0], 0) {
					t.Errorf("%s: redis.%s leaves the client on go-redis's own dialer; route it through "+
						"configureTestEnvRedisDial(currentTestEnvDialGuard(), options, NewDialer(timeout).DialContext) "+
						"or give it an explicit Dialer", fileSet.Position(call.Pos()), selector.Sel.Name)
				}
				return true
			})
		}
	}
	if checked < 20 {
		t.Fatalf("found only %d go-redis client constructions; the scan no longer sees this package", checked)
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
