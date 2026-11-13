// T3 for server/cli/sockets (SOCK-M5-08) on the private test Redis that test-env.sh attests (.stack/server-test.sh):
// the host builds the engine on server's socket store and sidecarhost's Redis and key-event subscriber, with a fake
// Quota, a projection source that loads nothing, and a key file the test writes. Each test waits on the engine's
// first probe and the gate's first over-limit read, never on a sleep. Nothing here sends a request outside the
// process.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server/sidecarhost"
)

// The block's main scrubs its logs before anything can log, as every sidecar block's does.
func TestMainScrubsLogsFirst(t *testing.T) {
	if err := sidecarhost.CheckMainScrubsLogsFirst("main.go"); err != nil {
		t.Fatal(err)
	}
}

// A source that holds no network: every load fails, so no network is ever served.
type hostTestSource struct{}

func (hostTestSource) Genesis(ctx context.Context, networkId sidecar.Id) ([]byte, error) {
	return nil, sidecar.ErrBlobNotFound
}

func (hostTestSource) Entries(ctx context.Context, networkId sidecar.Id, after *bridge.AuthPin) ([][]byte, error) {
	return nil, sidecar.ErrBlobNotFound
}

func (hostTestSource) Rows(ctx context.Context, networkId sidecar.Id, rowsSha256 []byte) ([]byte, error) {
	return nil, sidecar.ErrBlobNotFound
}

// Every line the block and the engine log.
type hostTestLog struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *hostTestLog) add(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *hostTestLog) Lines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

func (self *hostTestLog) Info(args ...any)                    { self.add(fmt.Sprint(args...)) }
func (self *hostTestLog) Infof(format string, args ...any)    { self.add(fmt.Sprintf(format, args...)) }
func (self *hostTestLog) Warningf(format string, args ...any) { self.add(fmt.Sprintf(format, args...)) }
func (self *hostTestLog) Errorf(format string, args ...any)   { self.add(fmt.Sprintf(format, args...)) }
func (self *hostTestLog) V(level int32) sidecar.Verbose       { return hostTestVerbose{log: self} }
func (self hostTestVerbose) Enabled() bool                    { return true }
func (self hostTestVerbose) Info(args ...any)                 { self.log.add(fmt.Sprint(args...)) }
func (self hostTestVerbose) Infof(format string, args ...any) {
	self.log.add(fmt.Sprintf(format, args...))
}

// Every verbose line too, so the scan sees what a raised level would write.
type hostTestVerbose struct {
	log *hostTestLog
}

// A key file of one fresh key.
func hostTestKeyFile(t testing.TB, now time.Time) func() ([]byte, error) {
	t.Helper()
	data, err := socketsservice.SocketTicketKeyFileAdd(nil, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	return func() ([]byte, error) { return data, nil }
}

// What a test's host is built on.
type hostTestWorld struct {
	clock         *sidecartest.FakeClock
	faults        *sidecartest.FaultQueue
	quota         *sidecartest.FakeQuota
	authenticator *sidecartest.FakeAuthenticator
	gates         *sidecartest.FakeServiceGates
	log           *hostTestLog
}

// A world whose gates open sockets to every network.
func newHostTestWorld(t testing.TB) *hostTestWorld {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	faults := sidecartest.NewFaultQueue()
	gates := sidecartest.NewFakeServiceGates(nil)
	if err := gates.Set("sidecar:\n  sockets: {enabled: true, percent: 100}\n"); err != nil {
		t.Fatal(err)
	}
	return &hostTestWorld{
		clock:         clock,
		faults:        faults,
		quota:         sidecartest.NewFakeQuota(clock, faults, nil),
		authenticator: sidecartest.NewFakeAuthenticator(clock, faults),
		gates:         gates,
		log:           &hostTestLog{},
	}
}

// A host on the world with every seam a block holds, changed by mutate, once its engine has probed the store and
// its gate has read the over-limit list.
func (self *hostTestWorld) newHost(t testing.TB, mutate func(deps *hostDeps)) *socketsHost {
	t.Helper()
	deps := &hostDeps{
		Quota:         self.quota,
		Source:        hostTestSource{},
		KeyFile:       hostTestKeyFile(t, self.clock.Now()),
		Authenticator: self.authenticator,
		Gates:         self.gates,
		Clock:         self.clock,
		Log:           self.log,
	}
	if mutate != nil {
		mutate(deps)
	}
	host, err := newSocketsHost(t.Context(), nil, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for _, counts := range []func() (int, chan struct{}){host.engine.Probes, host.gate.OverLimitReads} {
		for {
			count, changed := counts()
			if 1 <= count {
				break
			}
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatal("the host never probed its store or read the over-limit list")
			}
		}
	}
	return host
}

// One request through the host's handler, from remoteAddr when it is set.
func hostTestServe(host *socketsHost, method string, path string, token string, body any, remoteAddr string) *httptest.ResponseRecorder {
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, "http://sockets.example"+path, nil)
	} else {
		bodyBytes, _ := json.Marshal(body)
		request = httptest.NewRequest(method, "http://sockets.example"+path, bytes.NewReader(bodyBytes))
	}
	if remoteAddr != "" {
		request.RemoteAddr = remoteAddr
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	host.Handler().ServeHTTP(recorder, request)
	return recorder
}

// A sidecar's token in networkId.
func (self *hostTestWorld) sidecarToken(networkId connect.Id) (string, connect.Id) {
	clientId := connect.NewId()
	sidecarId := connect.NewId()
	return self.authenticator.Issue(&sidecar.Principal{
		NetworkId: networkId,
		ClientId:  clientId,
		SidecarId: &sidecarId,
		Audience:  socketsservice.SocketsAudience,
		IssuedAt:  self.clock.Now(),
		ExpiresAt: self.clock.Now().Add(time.Hour),
	}), clientId
}

// §19.4's sockets column: a block with Redis, the over-limit list, a source and a key that signs is ready, and
// publishes its key; the same block whose key file cannot be read is not, with nothing missing but its key.
func TestHostIsReadyOnlyWithATicketKey(t *testing.T) {
	world := newHostTestWorld(t)
	with := world.newHost(t, nil)
	without := world.newHost(t, func(deps *hostDeps) {
		deps.KeyFile = func() ([]byte, error) { return nil, errors.New("no key file in this test's vault") }
	})
	if missing := with.Missing(); len(missing) != 0 {
		t.Fatalf("missing %v", missing)
	}
	if !with.Ready() {
		t.Fatal("a block with a ticket key is not ready")
	}
	if without.Ready() {
		t.Fatal("a block with no ticket key is ready")
	}
	recorder := hostTestServe(with, http.MethodGet, "/socket/keys", "", nil, "")
	keys := &sidecar.SocketKeysResult{}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), keys) != nil || len(keys.Keys) != 1 {
		t.Fatalf("GET /socket/keys answered %d, %s", recorder.Code, recorder.Body.String())
	}
}

// A block given no Quota and no projection source says so and is never ready, the closed direction.
func TestHostWithNoQuotaOrSourceIsNotReady(t *testing.T) {
	world := newHostTestWorld(t)
	host, err := newSocketsHost(t.Context(), nil, &hostDeps{KeyFile: hostTestKeyFile(t, world.clock.Now()), Clock: world.clock, Log: world.log})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if missing := host.Missing(); len(missing) != 2 {
		t.Fatalf("missing %v", missing)
	}
	if host.Ready() {
		t.Fatal("a block with no Quota and no source is ready")
	}
}

// §19.11's sockets gate closed for one network answers that network 503 with Retry-After and passes another, and
// the block stays ready.
func TestHostSocketsGateClosedForOneNetwork(t *testing.T) {
	world := newHostTestWorld(t)
	openNetworkId, closedNetworkId := connect.NewId(), connect.NewId()
	if err := world.gates.Set("sidecar:\n  sockets: {enabled: true, networks: [" + openNetworkId.String() + "]}\n"); err != nil {
		t.Fatal(err)
	}
	host := world.newHost(t, nil)
	args := &sidecar.SocketRegistrationsArgs{SetVersion: 1}
	closedToken, _ := world.sidecarToken(closedNetworkId)
	recorder := hostTestServe(host, http.MethodPost, "/socket/registrations", closedToken, args, "")
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "60" {
		t.Fatalf("a closed network answered %d, Retry-After %q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	openToken, _ := world.sidecarToken(openNetworkId)
	if recorder := hostTestServe(host, http.MethodPost, "/socket/registrations", openToken, args, ""); recorder.Code != http.StatusOK {
		t.Fatalf("an open network answered %d", recorder.Code)
	}
	if !host.Ready() {
		t.Fatal("a closed gate made the block not ready")
	}
}

// What the key events reached, in order.
type hostTestEvents struct {
	stateLock sync.Mutex
	calls     []string
}

func (self *hostTestEvents) add(call string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, call)
}

func (self *hostTestEvents) Calls() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.calls)
}

func (self *hostTestEvents) ClientRemoved(clientId connect.Id) {
	self.add("removed " + clientId.String())
}
func (self *hostTestEvents) ProjectionChanged(networkId connect.Id) {
	self.add("projection " + networkId.String())
}
func (self *hostTestEvents) ProjectionsResync()               { self.add("projections resync") }
func (self *hostTestEvents) KillChanged(networkId connect.Id) { self.add("kill " + networkId.String()) }
func (self *hostTestEvents) KillsResync()                     { self.add("kills resync") }

// The block's one subscriber carries each family to its engine method: a client's c_removed, a network's {acl_}v
// and its {tes_}n; a resubscription runs both resyncs, and nothing else.
func TestHostKeyEventsReachTheEngineAndAResubscribeResyncs(t *testing.T) {
	world := newHostTestWorld(t)
	ctx := t.Context()
	redis := sidecartest.NewFakeRedis(ctx, world.clock, world.faults, nil, nil)
	events := &hostTestEvents{}
	host := world.newHost(t, func(deps *hostDeps) {
		deps.Redis = redis
		deps.Events = events
	})
	if !host.Ready() {
		t.Fatal("the block with every family subscribed is not ready")
	}
	networkId, clientId := connect.NewId(), connect.NewId()
	if _, err := redis.Increment(ctx, sidecar.AclVersionKey(sidecar.Id(networkId)), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := redis.Increment(ctx, sidecar.TlsKillTriggerKey(sidecar.Id(networkId)), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := redis.Set(ctx, "{"+clientId.String()+"}c_removed", []byte("1"), 0); err != nil {
		t.Fatal(err)
	}
	if err := redis.AwaitKeyEvents(ctx); err != nil {
		t.Fatal(err)
	}
	calls := events.Calls()
	for _, want := range []string{"projection " + networkId.String(), "kill " + networkId.String(), "removed " + clientId.String()} {
		if !slices.Contains(calls, want) {
			t.Fatalf("the events reached %v, not %q", calls, want)
		}
	}
	redis.ResyncKeyEvents()
	if err := redis.AwaitKeyEvents(ctx); err != nil {
		t.Fatal(err)
	}
	resyncs := slices.DeleteFunc(events.Calls()[len(calls):], func(call string) bool { return !strings.HasSuffix(call, "resync") })
	slices.Sort(resyncs)
	if !slices.Equal(resyncs, []string{"kills resync", "projections resync"}) {
		t.Fatalf("a resubscription ran %v", resyncs)
	}
}

// No line the block or the engine logs holds a caller's address or a socket name (I-13), through a registration
// and a Find that name both, while the gate's failed loads log what they are.
func TestHostLogsNoAddressOrSocketName(t *testing.T) {
	world := newHostTestWorld(t)
	host := world.newHost(t, nil)
	networkId := connect.NewId()
	token, _ := world.sidecarToken(networkId)
	address := "203.0.113.77"
	name := "tls/443/sockets-host-log-canary"
	listenId := connect.NewId()
	hostTestServe(host, http.MethodPost, "/socket/registrations", token, &sidecar.SocketRegistrationsArgs{
		SetVersion:    1,
		Registrations: []*sidecar.SocketRegistration{{Name: name, ListenId: &listenId, Protocol: "tls"}},
	}, address+":4444")
	hostTestServe(host, http.MethodPost, "/socket/find", token, &sidecar.SocketFindArgs{Name: name, NetworkId: &networkId}, address+":4445")
	lines := world.log.Lines()
	if len(lines) == 0 {
		t.Fatal("the block logged nothing, so the scan is blind")
	}
	for _, line := range lines {
		if strings.Contains(line, address) || strings.Contains(line, "sockets-host-log-canary") {
			t.Errorf("a log line holds a caller's address or a name: %s", line)
		}
	}
}
