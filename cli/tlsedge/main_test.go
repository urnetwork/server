// T3 for server/cli/tlsedge (TLS-M5-11): the host builds the engine on sidecarhost's ServiceHost with sidecartest's
// store, Redis, quota and gates, clients a test makes ready or not, and a scripted dial in place of Find. Each test
// waits on the engine's loads, the host's clients or a fake Redis's delivered events, never on a sleep, and time is a
// fake clock. Nothing here sends a request outside the process: the one socket is a loopback listener.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server/sidecarhost"
)

// The block's main scrubs its logs before anything can log, as every sidecar block's does.
func TestMainScrubsLogsFirst(t *testing.T) {
	if err := sidecarhost.CheckMainScrubsLogsFirst("main.go"); err != nil {
		t.Fatal(err)
	}
}

// The documentation root the tests' names live under.
const hostTestRoot = "edge.example"

// Every line the block logs.
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

// K clients a test connects and disconnects.
type hostTestClients struct {
	clientIds []sidecar.Id

	stateLock sync.Mutex
	ready     bool
	closed    bool
}

func newHostTestClients(clientCount int) *hostTestClients {
	clients := &hostTestClients{ready: true}
	for range clientCount {
		clients.clientIds = append(clients.clientIds, sidecar.NewId())
	}
	return clients
}

func (self *hostTestClients) Clients() []*sidecar.SocketEdgeClient { return nil }
func (self *hostTestClients) ClientIds() []sidecar.Id              { return slices.Clone(self.clientIds) }

func (self *hostTestClients) Ready() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.ready && !self.closed
}

func (self *hostTestClients) setReady(ready bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ready = ready
}

func (self *hostTestClients) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
}

func (self *hostTestClients) isClosed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.closed
}

// A stream that records what is written to it.
type hostTestStream struct {
	stateLock sync.Mutex
	written   []byte
	writeErr  error
	closed    bool
}

func (self *hostTestStream) Read(b []byte) (int, error) {
	return 0, errors.New("no listener in this test")
}

func (self *hostTestStream) Write(b []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.writeErr != nil {
		return 0, self.writeErr
	}
	self.written = append(self.written, b...)
	return len(b), nil
}

func (self *hostTestStream) CloseWrite() error { return nil }

func (self *hostTestStream) Close() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	return nil
}

func (self *hostTestStream) Reset(code protocol.StreamResetCode) error { return self.Close() }
func (self *hostTestStream) SetReadDeadline(t time.Time) error         { return nil }
func (self *hostTestStream) SetWriteDeadline(t time.Time) error        { return nil }

// A dial that records each request and answers as the test scripts.
type hostTestDial struct {
	stateLock sync.Mutex
	requests  []*sidecar.SocketEdgeDialRequest
	stream    *hostTestStream
	err       error
	dialed    chan struct{}
}

func newHostTestDial() *hostTestDial {
	return &hostTestDial{err: &sidecar.SocketCodeError{Err: sidecar.ErrSocketNoListener}, dialed: make(chan struct{}, 16)}
}

func (self *hostTestDial) dial(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requests = append(self.requests, request)
	select {
	case self.dialed <- struct{}{}:
	default:
	}
	if self.err != nil {
		return nil, self.err
	}
	return self.stream, nil
}

func (self *hostTestDial) Requests() []*sidecar.SocketEdgeDialRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

// A store whose label map reads fail while a test says so.
type hostTestStore struct {
	*sidecartest.FakeTlsEdgeStore

	stateLock sync.Mutex
	namesErr  error
}

func (self *hostTestStore) setNamesErr(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.namesErr = err
}

func (self *hostTestStore) NameSnapshot(ctx context.Context, root string) (*tlsedgeservice.TlsEdgeNameMap, error) {
	if err := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.namesErr
	}(); err != nil {
		return nil, err
	}
	return self.FakeTlsEdgeStore.NameSnapshot(ctx, root)
}

// What a test's host is built on.
type hostTestWorld struct {
	clock   *sidecartest.FakeClock
	faults  *sidecartest.FaultQueue
	redis   *sidecartest.FakeRedis
	quota   *sidecartest.FakeQuota
	gates   *sidecartest.FakeServiceGates
	store   *hostTestStore
	clients *hostTestClients
	dial    *hostTestDial
	log     *hostTestLog
}

// A world whose gate opens tlsedge to every network.
func newHostTestWorld(t testing.TB) *hostTestWorld {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	faults := sidecartest.NewFaultQueue()
	gates := sidecartest.NewFakeServiceGates(nil)
	if err := gates.Set("sidecar:\n  tlsedge: {enabled: true, percent: 100}\n"); err != nil {
		t.Fatal(err)
	}
	return &hostTestWorld{
		clock:   clock,
		faults:  faults,
		redis:   sidecartest.NewFakeRedis(t.Context(), clock, faults, nil, nil),
		quota:   sidecartest.NewFakeQuota(clock, faults, nil),
		gates:   gates,
		store:   &hostTestStore{FakeTlsEdgeStore: sidecartest.NewFakeTlsEdgeStore(nil)},
		clients: newHostTestClients(4),
		dial:    newHostTestDial(),
		log:     &hostTestLog{},
	}
}

// The engine's settings a test runs: the defaults on the documentation root, one block.
func hostTestSettings() *hostSettings {
	tlsEdge := tlsedgeservice.DefaultTlsEdgeSettings()
	tlsEdge.Root = hostTestRoot
	tlsEdge.Blocks = 1
	return defaultHostSettings(tlsEdge)
}

// Waits until counts answers at least target.
func hostTestAwait(t testing.TB, what string, target uint64, counts func() (uint64, chan struct{})) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		count, changed := counts()
		if target <= count {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("%s: at %d, want %d", what, count, target)
		}
	}
}

// Waits until the host's clients and dial exist.
func hostTestAwaitClients(t testing.TB, host *tlsEdgeHost) {
	t.Helper()
	select {
	case <-host.clientsMade:
	case <-time.After(30 * time.Second):
		t.Fatal("the host never made its clients")
	}
}

// A host on the world with every seam a block holds, changed by mutate, once its engine has read the label map
// and the kill switch once.
func (self *hostTestWorld) newHost(t testing.TB, mutate func(deps *hostDeps)) *tlsEdgeHost {
	t.Helper()
	deps := &hostDeps{
		Quota: self.quota,
		Store: self.store,
		NewClients: func(ctx context.Context) (hostClients, error) {
			return self.clients, nil
		},
		NewDial: func(clients hostClients) (hostSocketDial, error) {
			return self.dial.dial, nil
		},
		Gates: self.gates,
		Redis: self.redis,
		Clock: self.clock,
		Log:   self.log,
	}
	if mutate != nil {
		mutate(deps)
	}
	host, err := newTlsEdgeHost(t.Context(), hostTestSettings(), deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	if deps.Store != nil {
		hostTestAwait(t, "the label map's first read", 1, host.engine.NameLoads)
		hostTestAwait(t, "the kill switch's first read", 1, host.engine.KillLoads)
	}
	return host
}

// §19.4's tlsedge column: a block with its names loaded, the kill switch read, the key events held and all K
// clients connected is ready; with one client down it is not; and closing it closes its clients.
func TestHostIsReadyOnlyWithNamesKillsAndAllClients(t *testing.T) {
	world := newHostTestWorld(t)
	host := world.newHost(t, nil)
	hostTestAwaitClients(t, host)
	if missing := host.Missing(); len(missing) != 0 {
		t.Fatalf("missing %v", missing)
	}
	if !host.Ready() {
		t.Fatal("a block with names, kills and K clients is not ready")
	}
	// D-TLS-9: one client down is not ready, though the engine still holds K ids for its Finds
	world.clients.setReady(false)
	if host.Ready() {
		t.Fatal("ready with a client down")
	}
	if len(host.dialer.ClientIds()) != 4 || !host.engine.Ready() {
		t.Fatal("a client down took the engine's K client ids")
	}
	world.clients.setReady(true)
	if !host.Ready() {
		t.Fatal("not ready with every client back")
	}
	host.Close()
	if host.Ready() || !world.clients.isClosed() {
		t.Fatal("a closed block is ready or left its clients open")
	}
}

// The block never serves on an empty map: while the label map cannot be read it is not ready, and it is once a
// later read lands.
func TestHostIsNotReadyWithoutNames(t *testing.T) {
	world := newHostTestWorld(t)
	world.store.setNamesErr(errors.New("no label map in this test's store"))
	host := world.newHost(t, nil)
	hostTestAwaitClients(t, host)
	if host.Ready() || host.engine.Names().Loaded() {
		t.Fatal("ready with no label map")
	}
	world.store.setNamesErr(nil)
	// the names loop reads again after its interval
	world.clock.Advance(hostTestSettings().TlsEdge.NameReloadInterval)
	hostTestAwait(t, "the label map's second read", 2, host.engine.NameLoads)
	if !host.Ready() {
		t.Fatal("not ready once the label map loaded")
	}
}

// The block is not ready until its clients exist, and makes them again after ClientsRetryInterval when the source
// could not answer.
func TestHostIsNotReadyWithoutClientsAndRetries(t *testing.T) {
	world := newHostTestWorld(t)
	attempts := make(chan int, 16)
	attemptCount := 0
	host := world.newHost(t, func(deps *hostDeps) {
		deps.NewClients = func(ctx context.Context) (hostClients, error) {
			attemptCount += 1
			attempts <- attemptCount
			if attemptCount == 1 {
				return nil, errors.New("the source answered no client")
			}
			return world.clients, nil
		}
	})
	if <-attempts != 1 {
		t.Fatal("the first attempt was not the first")
	}
	if host.Ready() || len(host.dialer.ClientIds()) != 0 || host.engine.Ready() {
		t.Fatal("ready with no clients")
	}
	// a dial before the clients exist answers unavailable
	if _, err := host.dialer.Dial(t.Context(), &tlsedgeservice.TlsEdgeDialRequest{Target: &tlsedgeservice.TlsEdgeTarget{Name: "tls/443/@"}}); !errors.Is(err, sidecar.ErrSocketUnavailable) {
		t.Fatalf("a dial with no clients: %v", err)
	}
	// the retry waits on the clock: the engine's own timers are pending too, so watch until the retry's is
	retryAt := world.clock.Now().Add(hostTestSettings().ClientsRetryInterval)
	guard, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		pending, added := world.clock.WatchTimers()
		if slices.ContainsFunc(pending, func(at time.Time) bool { return at.Equal(retryAt) }) {
			break
		}
		select {
		case <-added:
		case <-guard.Done():
			t.Fatal("the host never armed its retry")
		}
	}
	world.clock.Advance(hostTestSettings().ClientsRetryInterval)
	hostTestAwaitClients(t, host)
	if !host.Ready() {
		t.Fatal("not ready once the clients were made")
	}
	if lines := world.log.Lines(); !slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, "no platform clients yet") }) {
		t.Fatalf("the failed attempt was not said: %v", lines)
	}
}

// A block given no Quota, no store and no clients says so and is never ready, the closed direction.
func TestHostWithNoSeamsIsNotReady(t *testing.T) {
	world := newHostTestWorld(t)
	host, err := newTlsEdgeHost(t.Context(), hostTestSettings(), &hostDeps{Gates: world.gates, Redis: world.redis, Clock: world.clock, Log: world.log})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if missing := host.Missing(); len(missing) != 3 {
		t.Fatalf("missing %v", missing)
	}
	if host.Ready() {
		t.Fatal("a block with no seam is ready")
	}
	if _, err := newTlsEdgeHost(t.Context(), nil, nil); err == nil {
		t.Fatal("a block with no tlsedge settings started")
	}
	settings := hostTestSettings()
	settings.Clients.ClientCount = settings.TlsEdge.Clients + 1
	if _, err := newTlsEdgeHost(t.Context(), settings, nil); err == nil {
		t.Fatal("a pool of another size than the engine's K started")
	}
}

// §13.9's kill switch on the block's one subscriber: a kill written to {tes_} with its trigger reaches the engine's
// kill set on the key event alone, with the clock not moved at all, so inside its 5 s.
func TestHostKillKeyEventReachesTheKillSet(t *testing.T) {
	world := newHostTestWorld(t)
	host := world.newHost(t, nil)
	ctx := t.Context()
	networkId := sidecar.NewId()
	if host.engine.Kills().Kills(networkId, "blog") {
		t.Fatal("killed before any kill")
	}
	startTime := world.clock.Now()
	changes, _ := host.engine.KillChanges()
	value, err := json.Marshal(&sidecar.TlsKillEntry{Host: "blog", Reason: "case"})
	if err != nil {
		t.Fatal(err)
	}
	if err := world.redis.HashSet(ctx, sidecar.TlsKillKey(networkId), map[string][]byte{"blog": value}); err != nil {
		t.Fatal(err)
	}
	if _, err := world.redis.Increment(ctx, sidecar.TlsKillTriggerKey(networkId), 0); err != nil {
		t.Fatal(err)
	}
	if err := world.redis.AwaitKeyEvents(ctx); err != nil {
		t.Fatal(err)
	}
	hostTestAwait(t, "the kill's landing", changes+1, host.engine.KillChanges)
	if !host.engine.Kills().Kills(networkId, "blog") {
		t.Fatal("the key event's kill did not land")
	}
	if elapsed := world.clock.Now().Sub(startTime); 5*time.Second <= elapsed {
		t.Fatalf("the kill took %v", elapsed)
	}
	if host.engine.Kills().Kills(networkId, "shop") {
		t.Fatal("a host the kill does not name is killed")
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

func (self *hostTestEvents) KillChanged(networkId sidecar.Id) { self.add("kill " + networkId.String()) }
func (self *hostTestEvents) KillsResync()                     { self.add("kills resync") }

// The one family: a network's {tes_}n reaches KillChanged with that network, another family's key reaches
// nothing, and a resubscription runs KillsResync, since events in the gap are gone.
func TestHostKeyEventsReachTheEngineAndAResubscribeResyncs(t *testing.T) {
	world := newHostTestWorld(t)
	ctx := t.Context()
	events := &hostTestEvents{}
	world.newHost(t, func(deps *hostDeps) {
		deps.Events = events
	})
	networkId := sidecar.NewId()
	if _, err := world.redis.Increment(ctx, sidecar.TlsKillTriggerKey(networkId), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := world.redis.Increment(ctx, sidecar.AclVersionKey(networkId), 0); err != nil {
		t.Fatal(err)
	}
	if err := world.redis.AwaitKeyEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := events.Calls(); !slices.Equal(calls, []string{"kill " + networkId.String()}) {
		t.Fatalf("the events reached %v", calls)
	}
	world.redis.ResyncKeyEvents()
	if err := world.redis.AwaitKeyEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := events.Calls(); !slices.Equal(calls, []string{"kill " + networkId.String(), "kills resync"}) {
		t.Fatalf("a resubscription ran %v", calls)
	}
}

// The engine's request as the sockets dialer's: the hello's first 2,048 bytes as early data and its rest written
// once the stream is open; a rest that cannot be written ends the stream; and a refusal passes through as it is.
func TestHostDialerSplitsTheHelloAtEarlyData(t *testing.T) {
	clients := newHostTestClients(4)
	dial := newHostTestDial()
	dialer := &hostDialer{earlyDataMaxByteCount: sidecar.MaxStreamEarlyDataByteCount}
	dialer.set(&hostDialerState{clients: clients, dial: dial.dial})
	networkId := sidecar.NewId()
	request := func(helloByteCount int) *tlsedgeservice.TlsEdgeDialRequest {
		hello := make([]byte, helloByteCount)
		for i := range hello {
			hello[i] = byte(i)
		}
		return &tlsedgeservice.TlsEdgeDialRequest{
			Target:      &tlsedgeservice.TlsEdgeTarget{NetworkId: networkId, Label: "abcdefghij", Host: "blog", Port: 443, Name: "tls/443/blog"},
			AlpnIds:     []string{"h2", "http/1.1"},
			AffinityKey: []byte("affinity"),
			HelloBytes:  hello,
		}
	}

	// a refusal is the engine's to read
	if _, err := dialer.Dial(t.Context(), request(100)); !errors.Is(err, sidecar.ErrSocketNoListener) {
		t.Fatalf("a refusal: %v", err)
	}
	sent := dial.Requests()[0]
	if sent.NetworkId != networkId || sent.NetworkLabel != "abcdefghij" || sent.Name != "tls/443/blog" || sent.Protocol != "tls" ||
		!slices.Equal(sent.Alpns, []string{"h2", "http/1.1"}) || string(sent.AffinityKey) != "affinity" || len(sent.EarlyData) != 100 {
		t.Fatalf("sent %+v", sent)
	}

	// a short hello is all early data
	dial.err = nil
	dial.stream = &hostTestStream{}
	if _, err := dialer.Dial(t.Context(), request(sidecar.MaxStreamEarlyDataByteCount)); err != nil {
		t.Fatal(err)
	}
	if len(dial.Requests()[1].EarlyData) != sidecar.MaxStreamEarlyDataByteCount || len(dial.stream.written) != 0 {
		t.Fatalf("a hello at the bound: %d early, %d written", len(dial.Requests()[1].EarlyData), len(dial.stream.written))
	}

	// a long one is split, in order
	dial.stream = &hostTestStream{}
	long := request(3000)
	if _, err := dialer.Dial(t.Context(), long); err != nil {
		t.Fatal(err)
	}
	early := dial.Requests()[2].EarlyData
	if !bytes.Equal(append(slices.Clone(early), dial.stream.written...), long.HelloBytes) || len(early) != sidecar.MaxStreamEarlyDataByteCount {
		t.Fatalf("a long hello: %d early, %d written", len(early), len(dial.stream.written))
	}

	// a rest that cannot be written ends the stream
	dial.stream = &hostTestStream{writeErr: errors.New("reset")}
	if _, err := dialer.Dial(t.Context(), request(3000)); !errors.Is(err, sidecar.ErrSocketUnavailable) || !dial.stream.closed {
		t.Fatalf("a failed write: %v, closed %t", err, dial.stream.closed)
	}

	// no target, no dial
	if _, err := dialer.Dial(t.Context(), &tlsedgeservice.TlsEdgeDialRequest{}); !errors.Is(err, sidecar.ErrSocketUnavailable) {
		t.Fatalf("no target: %v", err)
	}
}

// A block's listener answers the service port whatever host port warp bound, which is what the PROXY listener
// and the engine read; the engine refuses a port that is not one of the four.
func TestHostListenerNamesItsServicePort(t *testing.T) {
	world := newHostTestWorld(t)
	host := world.newHost(t, nil)
	bound, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := newProxyListenerWrap(t.Context(), 8443, hostTestSettings().TlsEdge.ProxyHeader, world.clock)(bound)
	if err != nil {
		t.Fatal(err)
	}
	defer wrapped.Close()
	if address, ok := wrapped.Addr().(*net.TCPAddr); !ok || address.Port != 8443 {
		t.Fatalf("the listener answers %v", wrapped.Addr())
	}
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := host.Serve(&servicePortListener{Listener: other, servicePort: 25}); !errors.Is(err, tlsedgeservice.ErrTlsEdgePort) {
		t.Fatalf("port 25 was served: %v", err)
	}
	options := hostRunOptions(t.Context(), 80, host)
	ports := []int{}
	for _, listener := range options.Listeners {
		ports = append(ports, listener.Port)
	}
	if !slices.Equal(ports, []int{443, 8443, 8883, 5671}) || options.Validate() != nil || options.Postgres {
		t.Fatalf("the block serves %v: %v", ports, options.Validate())
	}
}

// One visitor through the block's own listener, PROXY header and all, to a published name: the engine routes it to
// the dial, the visitor gets a TLS alert when no listener answers, and no line the block logs holds the visitor's
// address or the name it asked for (I-13).
func TestHostRoutesAVisitorAndLogsNoAddressOrName(t *testing.T) {
	world := newHostTestWorld(t)
	networkId := sidecar.NewId()
	label, err := world.store.AddLabel(hostTestRoot, networkId)
	if err != nil {
		t.Fatal(err)
	}
	host := world.newHost(t, nil)
	hostTestAwaitClients(t, host)
	if !host.Ready() {
		t.Fatal("the block is not ready")
	}
	bound, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := newProxyListenerWrap(host.ctx, 443, hostTestSettings().TlsEdge.ProxyHeader, world.clock)(bound)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- host.Serve(listener)
	}()

	visitorAddress := "203.0.113.77"
	serverName := "log-canary." + label + "." + hostTestRoot
	conn, err := net.Dial("tcp", bound.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("PROXY TCP4 " + visitorAddress + " 198.51.100.1 44444 443\r\n")); err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	handshakeErr := tls.Client(conn, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS13}).Handshake()
	if handshakeErr == nil {
		t.Fatal("a handshake finished with no listener behind the edge")
	}
	// no listener answers internal_error, as every refusal that is not the name's does
	if !strings.Contains(handshakeErr.Error(), "remote error: tls: internal error") {
		t.Fatalf("the visitor got no internal_error alert: %v", handshakeErr)
	}
	requests := world.dial.Requests()
	if len(requests) == 0 {
		t.Fatalf("the engine never dialed: the visitor got %v", handshakeErr)
	}
	if requests[0].NetworkId != networkId || requests[0].NetworkLabel != label || requests[0].Name != "tls/443/log-canary" || requests[0].Protocol != "tls" {
		t.Fatalf("dialed %+v", requests[0])
	}
	host.Close()
	if err := <-served; err != nil {
		t.Fatalf("Serve ended with %v", err)
	}
	for _, line := range world.log.Lines() {
		if strings.Contains(line, visitorAddress) || strings.Contains(line, "log-canary") || strings.Contains(line, label) {
			t.Errorf("a log line holds a visitor's address or a name: %s", line)
		}
	}
}

// A block whose tlsedge.yml the engine refuses, main's without its blocks, starts no engine: run serves /status not
// ready and nothing else, which notReadyEngine is.
func TestHostWithoutSettingsIsNeverReady(t *testing.T) {
	if _, err := tlsedgeservice.LoadTlsEdgeSettings([]byte("root: edge.example\nclients: 4\n")); err == nil {
		t.Fatal("a tlsedge.yml with no blocks loaded")
	}
	engine := notReadyEngine{}
	if engine.Ready() {
		t.Fatal("the engine of a block that could not start is ready")
	}
	options := &sidecarhost.RunOptions{Service: hostService, StatusPort: 80, Engine: engine}
	if err := options.Validate(); err != nil {
		t.Fatal(err)
	}
}
