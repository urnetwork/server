package mxedgehost

// The router role on server's side (MAIL-M7-19): the route value model writes read by the engine, the dialer's
// adaptation of SocketEdgeDialer, the block's seams and options, and §22.5's hot path: a real TLS handshake from
// the dialog to the splice, through tlsedgeservice's hello reader, with every PostgreSQL acquisition refused
// and the path's Redis calls counted.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The names a router test routes, synthetic.
const (
	routerTestRoot  = "root.example"
	routerTestLabel = "ab12cd34ef"
	routerTestSni   = "mx." + routerTestLabel + "." + routerTestRoot
)

// mxedge.yml for the router, its probe on hostTestProbeInterval and its record flush parked.
func routerTestSettings() *mxedgeservice.MxEdgeSettings {
	settings := hostTestMxEdgeSettings()
	settings.RouteRootDomainName = routerTestRoot
	settings.RecordFlushInterval = 1000 * time.Hour
	return settings
}

// The value model's projection writes is the value the router reads, field for field, label included.
func TestModelRouteValueReadsAsTheRoutersRoute(t *testing.T) {
	networkId := server.NewId()
	receiverId := server.NewId()
	value, err := json.Marshal(&model.MailRoute{
		NetworkId:          networkId,
		Label:              routerTestLabel,
		Host:               "mx",
		Flags:              model.MailRouteFlagAlias,
		ReceiverSidecarIds: []server.Id{receiverId},
	})
	if err != nil {
		t.Fatal(err)
	}
	route, ok := mxedgeservice.ParseMailRoute(value, mxedgeservice.DefaultMxEdgeSettings().HostLabelMaxByteCount)
	if !ok {
		t.Fatalf("the router cannot read %s", value)
	}
	if route.NetworkId != sidecar.Id(networkId) || route.Label != routerTestLabel || route.Host != "mx" ||
		route.Flags != mxedgeservice.MailRouteFlagAlias || !slices.Equal(route.ReceiverSidecarIds, []sidecar.Id{sidecar.Id(receiverId)}) {
		t.Errorf("read %+v", route)
	}
	if model.MailRouteKey("acme") != mxedgeservice.MailRouteKey("acme") || model.MailRouteName(routerTestRoot, "mx.acme."+routerTestRoot) != "acme" {
		t.Error("model and the router name keys apart")
	}
	if model.MailRouteFlagAlias != mxedgeservice.MailRouteFlagAlias || model.MailRouteFlagScope != mxedgeservice.MailRouteFlagScope ||
		model.MailRouteFlagCustomHost != mxedgeservice.MailRouteFlagCustomHost {
		t.Error("model's and the router's flags differ")
	}
}

// The dialer adapts the engine's request to SocketEdgeDialer's: protocol smtp-starttls, the whole hello as
// early data, the label and name, MailIngress as the metadata; and with no clients yet it holds no id, is not
// ready, and answers unavailable.
func TestRouterDialerAdaptsTheEdgeDialer(t *testing.T) {
	dialer := &routerDialer{}
	if 0 < len(dialer.ClientIds()) || dialer.Ready() {
		t.Fatal("a dialer with no clients holds some")
	}
	if _, _, err := dialer.Dial(t.Context(), &mxedgeservice.MxEdgeDialRequest{}); !errors.Is(err, sidecar.ErrSocketUnavailable) {
		t.Fatalf("a dialer with no clients dialed: %v", err)
	}
	var asked *sidecar.SocketEdgeDialRequest
	clients := &routerTestClients{clientIds: []sidecar.Id{sidecar.NewId()}, ready: true}
	dialer.set(&routerDialerState{clients: clients, dial: func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (*sidecar.SocketEdgeStream, error) {
		asked = request
		return nil, &sidecar.SocketCodeError{Err: sidecar.ErrSocketNoListener}
	}})
	if !dialer.Ready() || len(dialer.ClientIds()) != 1 {
		t.Fatal("a dialer with its clients is not ready")
	}
	hello := bytes.Repeat([]byte{0x16}, 5000)
	networkId := sidecar.NewId()
	_, _, err := dialer.Dial(t.Context(), &mxedgeservice.MxEdgeDialRequest{
		NetworkId:    networkId,
		NetworkLabel: routerTestLabel,
		Name:         "smtp/25/mx",
		Metadata:     func(uint64) ([]byte, error) { return []byte("ingress"), nil },
		HelloBytes:   hello,
	})
	if !errors.Is(err, sidecar.ErrSocketNoListener) {
		t.Fatalf("the dial's refusal came back as %v", err)
	}
	metadata, _ := asked.Metadata(0)
	if asked.NetworkId != networkId || asked.NetworkLabel != routerTestLabel || asked.Name != "smtp/25/mx" ||
		asked.Protocol != "smtp-starttls" || !bytes.Equal(asked.EarlyData, hello) || string(metadata) != "ingress" {
		t.Errorf("asked %+v", asked)
	}
}

// What a block's clients are in a test.
type routerTestClients struct {
	clientIds []sidecar.Id
	ready     bool
	closed    bool
}

func (self *routerTestClients) Clients() []*sidecar.SocketEdgeClient { return nil }
func (self *routerTestClients) ClientIds() []sidecar.Id              { return slices.Clone(self.clientIds) }
func (self *routerTestClients) Ready() bool                          { return self.ready && !self.closed }
func (self *routerTestClients) Close()                               { self.closed = true }

// A block given none of its seams says what it lacks and is never ready; a block refuses settings with no
// route root, and a pool whose K is not mxedge.yml's.
func TestRouterHostNeedsItsSeams(t *testing.T) {
	settings := DefaultHostSettings()
	settings.MxEdge = routerTestSettings()
	fakeHost := sidecartest.NewFakeHost(t.Context(), sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), nil, nil, nil)
	routerHost, err := NewRouterHost(t.Context(), settings, nil, &RouterDeps{Redis: fakeHost.Redis, Gates: fakeHost.Gates})
	if err != nil {
		t.Fatal(err)
	}
	defer routerHost.Close()
	<-routerHost.ClientsMade()
	if routerHost.Ready() || len(routerHost.Missing()) != 3 {
		t.Errorf("ready %t without %v", routerHost.Ready(), routerHost.Missing())
	}
	for _, missing := range routerHost.Missing() {
		if strings.Contains(missing, "platform-mail") {
			t.Errorf("the router asks for the platform's key: %s", missing)
		}
	}

	noRoot := DefaultHostSettings()
	noRoot.MxEdge = routerTestSettings()
	noRoot.MxEdge.RouteRootDomainName = ""
	if _, err := NewRouterHost(t.Context(), noRoot, nil, nil); !errors.Is(err, mxedgeservice.ErrMxEdgeSettingsRefused) {
		t.Errorf("no route root: %v", err)
	}
	routerSettings := DefaultRouterSettings(settings.MxEdge)
	routerSettings.Clients.ClientCount = settings.MxEdge.Clients + 1
	if _, err := NewRouterHost(t.Context(), settings, routerSettings, nil); err == nil {
		t.Error("a pool of another K was taken")
	}
}

// The block's clients come after the engine, made again until the source answers; it is ready once they are
// and every seam is there.
func TestRouterHostIsReadyOnceItsClientsAre(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fakeHost := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	settings := DefaultHostSettings()
	settings.MxEdge = routerTestSettings()
	routerSettings := DefaultRouterSettings(settings.MxEdge)
	clients := &routerTestClients{ready: true}
	for range settings.MxEdge.Clients {
		clients.clientIds = append(clients.clientIds, sidecar.NewId())
	}
	var attemptsLock sync.Mutex
	attempts := 0
	routerHost, err := NewRouterHost(t.Context(), settings, routerSettings, &RouterDeps{
		Quota: fakeHost.Quota,
		Store: sidecartest.NewFakeMxEdgeStore(nil),
		NewClients: func(ctx context.Context) (RouterClients, error) {
			attemptsLock.Lock()
			defer attemptsLock.Unlock()
			attempts += 1
			if attempts == 1 {
				return nil, errors.New("the source has not answered index 0")
			}
			return clients, nil
		},
		NewDial: func(clients RouterClients) (RouterSocketDial, error) {
			return func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (*sidecar.SocketEdgeStream, error) {
				return nil, &sidecar.SocketCodeError{Err: sidecar.ErrSocketNoListener}
			}, nil
		},
		Redis: fakeHost.Redis,
		Gates: fakeHost.Gates,
		Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer routerHost.Close()
	start := sidecartest.DefaultClockStart()
	// the first probe found the store and today's key once its next wait is armed, and the first try at the
	// clients failed, so the retry waits on the clock
	awaitHostTestTimerAt(t, t.Context(), clock, start.Add(hostTestProbeInterval))
	awaitHostTestTimerAt(t, t.Context(), clock, start.Add(routerSettings.ClientsRetryInterval))
	if routerHost.Ready() {
		t.Fatal("ready before its clients")
	}
	clock.Advance(routerSettings.ClientsRetryInterval)
	<-routerHost.ClientsMade()
	if !routerHost.Ready() {
		t.Error("not ready with every seam and its clients")
	}
	clients.ready = false
	if routerHost.Ready() {
		t.Error("ready while its clients are not connected")
	}
	routerHost.Close()
	if !clients.closed {
		t.Error("the block's close left its clients open")
	}
}

// The block runs on TCP 25 behind the PROXY wrap, with a Postgres pool for its store and MG7.
func TestRouterRunOptionsHoldPostgresAndTheProxyListener(t *testing.T) {
	groups := RouterStoreGroups()
	options := routerRunOptions(t.Context(), notReadyStreamEngine{}, DefaultHostSettings(), &RouterDeps{StoreGroups: groups}, &HostPorts{Status: hostTestStatusPort, Smtp: hostTestSmtpPort})
	if err := options.Validate(); err != nil {
		t.Fatal(err)
	}
	if options.Service != "mxedge" || !options.Postgres || len(options.Groups) != 1 || options.Groups[0].Name != "MG7" {
		t.Fatalf("options %+v", options)
	}
	if options.Stream == nil || len(options.Listeners) != 1 || options.Listeners[0].Port != hostTestSmtpPort || options.Listeners[0].Wrap == nil {
		t.Fatalf("listeners %+v", options.Listeners)
	}
}

// One end of a stream to the receiver, over two io.Pipes.
type routerTestStream struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

func (self *routerTestStream) Read(b []byte) (int, error)  { return self.reader.Read(b) }
func (self *routerTestStream) Write(b []byte) (int, error) { return self.writer.Write(b) }
func (self *routerTestStream) CloseWrite() error           { return self.writer.Close() }
func (self *routerTestStream) Close() error {
	self.writer.Close()
	return self.reader.Close()
}
func (self *routerTestStream) Reset(code protocol.StreamResetCode) error {
	self.writer.CloseWithError(errors.New("reset"))
	return self.reader.CloseWithError(errors.New("reset"))
}
func (self *routerTestStream) SetReadDeadline(t time.Time) error  { return nil }
func (self *routerTestStream) SetWriteDeadline(t time.Time) error { return nil }

// The receiver's side of the stream as a net.Conn: the early data first, then what the router splices.
type routerTestReceiverConn struct {
	net.Conn
	reader io.Reader
	writer *io.PipeWriter
}

func (self *routerTestReceiverConn) Read(b []byte) (int, error)  { return self.reader.Read(b) }
func (self *routerTestReceiverConn) Write(b []byte) (int, error) { return self.writer.Write(b) }
func (self *routerTestReceiverConn) Close() error                { return self.writer.Close() }

// A dialer whose receiver ends TLS itself, as a real one does, on a certificate for the SNI.
type routerTestTlsDialer struct {
	t           *testing.T
	certificate tls.Certificate
	now         time.Time
	clientIds   []sidecar.Id
	// what the receiver read after its handshake, then echoed
	received chan []byte
}

func (self *routerTestTlsDialer) ClientIds() []sidecar.Id { return self.clientIds }
func (self *routerTestTlsDialer) Ready() bool             { return true }

func (self *routerTestTlsDialer) Dial(ctx context.Context, request *mxedgeservice.MxEdgeDialRequest) (mxedgeservice.MxEdgeStream, uint64, error) {
	toReceiverReader, toReceiverWriter := io.Pipe()
	toRouterReader, toRouterWriter := io.Pipe()
	receiverConn := &routerTestReceiverConn{
		Conn:   &net.TCPConn{},
		reader: io.MultiReader(bytes.NewReader(slices.Clone(request.HelloBytes)), toReceiverReader),
		writer: toRouterWriter,
	}
	go func() {
		server := tls.Server(receiverConn, &tls.Config{Certificates: []tls.Certificate{self.certificate}, Time: func() time.Time { return self.now }})
		if err := server.Handshake(); err != nil {
			self.received <- []byte("handshake: " + err.Error())
			return
		}
		line, err := bufio.NewReader(server).ReadString('\n')
		if err != nil {
			self.received <- []byte("read: " + err.Error())
			return
		}
		self.received <- []byte(line)
		server.Write([]byte("250 echoed " + line))
		server.Close()
	}()
	return &routerTestStream{reader: toRouterReader, writer: toReceiverWriter}, 0, nil
}

// §22.5's hot path: from the banner through the pre-TLS dialog, the real ClientHello, the route, the lease
// and the open, to a TLS handshake the receiver ends and data both ways through the splice, with every
// PostgreSQL acquisition in the process refused and the path's Redis calls counted: none reaches Postgres, and
// Redis answers only the route, once. Byte-identity holds end to end, or TLS would fail.
func TestRouterInlinePathReachesNoPostgres(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	start := sidecartest.DefaultClockStart()
	clock := sidecartest.NewFakeClock(start)
	fakeHost := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	networkId := sidecar.NewId()
	fakeHost.Gates.OpenService(sidecar.ServiceGateMxedge, connect.Id(networkId))
	fakeHost.Quota.SetBalance(connect.Id(networkId), 64*1024*1024)
	routeValue, err := json.Marshal(&model.MailRoute{NetworkId: server.Id(networkId), Label: routerTestLabel, Host: "mx"})
	if err != nil {
		t.Fatal(err)
	}
	inspect := sidecartest.InspectContext(ctx)
	if err := fakeHost.Redis.Set(inspect, model.MailRouteKey(routerTestLabel), routeValue, 0); err != nil {
		t.Fatal(err)
	}
	// the first probe makes today's key, as a fleet's first process does
	settings := routerTestSettings()
	counting := sidecarhost.NewCountingRedis(fakeHost.Redis)
	serviceHost := fakeHost.ServiceHost()
	serviceHost.Redis = counting

	chainPem, keyPem := hostTestCertificatePem(t, []string{routerTestSni}, start.Add(-time.Hour), start.Add(24*time.Hour))
	certificate, err := tls.X509KeyPair(chainPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	dialer := &routerTestTlsDialer{t: t, certificate: certificate, now: start, received: make(chan []byte, 1)}
	for range settings.Clients {
		dialer.clientIds = append(dialer.clientIds, sidecar.NewId())
	}
	engine, err := mxedgeservice.NewMxEdgeRouterService(ctx, serviceHost, sidecartest.NewFakeMxEdgeStore(nil), NewEngineDeps(serviceHost, nil),
		&mxedgeservice.MxEdgeRouterDeps{Dialer: dialer}, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	// the first probe read today's key once its next wait is armed
	awaitHostTestTimerAt(t, ctx, clock, start.Add(hostTestProbeInterval))
	if !engine.Ready() {
		t.Fatal("the router is not ready")
	}
	listener := &routerTestListener{conns: make(chan net.Conn, 1), closed: make(chan struct{})}
	go engine.Serve(listener)

	attempts, stop := server.DenyPostgresForTest(t)
	defer stop()
	before := counting.OpCount()

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	listener.conns <- &routerTestServerConn{Conn: serverConn, sender: &net.TCPAddr{IP: net.ParseIP(hostTestSender), Port: 2525}}
	reader := bufio.NewReader(clientConn)
	expect := func(want string) {
		t.Helper()
		line, err := reader.ReadString('\n')
		if err != nil || line != want+"\r\n" {
			t.Fatalf("read %q, %v; want %q", line, err, want)
		}
	}
	expect("220 " + hostTestBannerHostName + " ESMTP")
	clientConn.Write([]byte("EHLO " + hostTestEhloName + "\r\n"))
	expect("250-" + hostTestBannerHostName)
	expect("250-ENHANCEDSTATUSCODES")
	expect("250 STARTTLS")
	quietAt := clock.Now().Add(settings.StartTlsQuietTimeout)
	clientConn.Write([]byte("STARTTLS\r\n"))
	for {
		pending, added := clock.WatchTimers()
		if slices.ContainsFunc(pending, func(at time.Time) bool { return at.Equal(quietAt) }) {
			break
		}
		<-added
	}
	clock.Advance(settings.StartTlsQuietTimeout)
	expect("220 2.0.0 Ready to start TLS")

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(chainPem)
	// the sender's own TLS over the bytes the reader has not taken, which are none: STARTTLS's 220 was the last
	senderTls := tls.Client(&routerTestBufferedConn{Conn: clientConn, reader: reader}, &tls.Config{ServerName: routerTestSni, RootCAs: roots, Time: func() time.Time { return start }})
	if err := senderTls.Handshake(); err != nil {
		t.Fatalf("the TLS handshake through the splice failed: %v", err)
	}
	if _, err := senderTls.Write([]byte("MAIL FROM:<a@sender.example>\n")); err != nil {
		t.Fatal(err)
	}
	if received := <-dialer.received; string(received) != "MAIL FROM:<a@sender.example>\n" {
		t.Fatalf("the receiver read %q", received)
	}
	echo, err := io.ReadAll(senderTls)
	if err != nil || string(echo) != "250 echoed MAIL FROM:<a@sender.example>\n" {
		t.Fatalf("the sender read %q, %v", echo, err)
	}
	clientConn.Close()
	// returns once the session has ended: the fake clock never reaches the drain's bound
	engine.Drain()
	stop()

	if count := attempts(); count != 0 {
		t.Errorf("the inline path reached PostgreSQL %d times (I-12)", count)
	}
	if calls := counting.OpCount() - before; calls != 1 {
		t.Errorf("the inline path made %d Redis calls %v, want the route's one", calls, counting.OpCounts())
	}
}

// A listener a test feeds.
type routerTestListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func (self *routerTestListener) Accept() (net.Conn, error) {
	select {
	case conn := <-self.conns:
		return conn, nil
	case <-self.closed:
		return nil, net.ErrClosed
	}
}

func (self *routerTestListener) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

func (self *routerTestListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(hostTestLbAddress), Port: 25}
}

// The server's end of a pipe, reporting the sender the PROXY listener would.
type routerTestServerConn struct {
	net.Conn
	sender net.Addr
}

func (self *routerTestServerConn) RemoteAddr() net.Addr { return self.sender }

// A conn whose reads go through the reader that read the dialog, so no byte it buffered is lost.
type routerTestBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (self *routerTestBufferedConn) Read(b []byte) (int, error) { return self.reader.Read(b) }

// Nothing the router's process runs writes mail_direct_mx or a DNS table (QD-1103, owner decision 639; QD-992,
// D-OPS-1), by the SQL its stores can reach in model (router_sql_reach_test.go): the router's packages name model only
// for its two stores; the direct-MX prober's store reaches only reads, of the rows it probes by, and its record
// reaches no SQL at all, since api records each outcome; and the connection store's only writes are its
// connection records. A model name the router's packages start to use fails here until the listing covers it.
func TestRouterStoresWriteNoDirectMxOrDnsTable(t *testing.T) {
	if names := routerModelNames(t); !slices.Equal(names, []string{"NewMxDirectMxStore", "NewMxEdgeStore"}) {
		t.Fatalf("the router's packages name model's %v: list what each reaches here before it ships", names)
	}
	reachModel := loadRouterSqlReachModel(t, filepath.Join("..", "model"))

	record := newRouterSqlReach(reachModel)
	if !record.reachName("mxDirectMxStore.RecordDirectMxProbe") {
		t.Fatal("model holds no direct-MX store record")
	}
	record.run()
	if statements := record.sortedStatements(); 0 < len(statements) {
		t.Errorf("the direct-MX store's record reaches SQL, where api records each outcome: %q", statements)
	}

	directMx := newRouterSqlReach(reachModel)
	directMx.reachName("NewMxDirectMxStore")
	if built := directMx.reachBuiltTypes("NewMxDirectMxStore"); !slices.Equal(built, []string{"mxDirectMxStore"}) {
		t.Fatalf("NewMxDirectMxStore builds %v", built)
	}
	directMx.run()
	directMxStatements := directMx.sortedStatements()
	for _, statement := range directMxStatements {
		t.Logf("direct mx: %s", statement)
		if !routerSqlReadOnly(statement) {
			t.Errorf("the direct-MX store reaches a statement that is not a read: %q", statement)
		}
	}
	// the listing found the reads the prober lists by, so an empty listing cannot pass
	for _, table := range []string{"mail_direct_mx", "mail_host_key", "dns_label"} {
		if !slices.ContainsFunc(directMxStatements, func(statement string) bool { return strings.Contains(statement, "FROM "+table) }) {
			t.Errorf("the direct-MX store's listing holds no read of %s: %q", table, directMxStatements)
		}
	}

	edge := newRouterSqlReach(reachModel)
	edge.reachName("NewMxEdgeStore")
	if built := edge.reachBuiltTypes("NewMxEdgeStore"); !slices.Equal(built, []string{"MxEdgeStore"}) {
		t.Fatalf("NewMxEdgeStore builds %v", built)
	}
	edge.run()
	edgeWrites := []string{}
	for _, statement := range edge.sortedStatements() {
		t.Logf("edge: %s", statement)
		if routerSqlReadOnly(statement) {
			continue
		}
		tables := routerSqlWriteTables(statement)
		if len(tables) == 0 {
			t.Errorf("the connection store reaches a write whose table the listing cannot name: %q", statement)
		}
		for _, table := range tables {
			if routerSqlZoneTable(table) || table != "mail_connection_record" {
				t.Errorf("the connection store writes %q: %q", table, statement)
			}
			edgeWrites = append(edgeWrites, table)
		}
	}
	if !slices.Contains(edgeWrites, "mail_connection_record") {
		t.Errorf("the connection store's listing holds no insert of its records: %q", edge.sortedStatements())
	}
}

// A mirror directory holding files, each modified at its time.
func feedMirrorTestDirectory(t *testing.T, files map[string]string, modifiedTimes map[string]time.Time) string {
	t.Helper()
	directory := t.TempDir()
	for name, text := range files {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if modifiedTime, ok := modifiedTimes[name]; ok {
			if err := os.Chtimes(path, modifiedTime, modifiedTime); err != nil {
				t.Fatal(err)
			}
		}
	}
	return directory
}

// What a load gave, prefix to its categories.
func feedMirrorTestLoad(t *testing.T, directory string) (map[string][]string, time.Time, error) {
	t.Helper()
	settings := DefaultFeedMirrorSettings()
	settings.Directory = directory
	source, err := NewFeedMirrorSource(settings)
	if err != nil {
		t.Fatal(err)
	}
	loaded := map[string][]string{}
	currentTime, err := source.Load(t.Context(), func(prefix netip.Prefix, category string) error {
		loaded[prefix.String()] = append(loaded[prefix.String()], category)
		return nil
	})
	return loaded, currentTime, err
}

// Each zone file's entries load under its file's category, rbldnsd's comments, directives and values
// skipped, and the mirror is as current as its oldest file.
func TestFeedMirrorLoadsZoneFilesByCategory(t *testing.T) {
	older := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	newer := older.Add(4 * time.Minute)
	directory := feedMirrorTestDirectory(t, map[string]string{
		"exploit.rbldnsd": "$SOA 300 ns.feed.example. hostmaster.feed.example. 1 3600 600 86400 300\n" +
			"# exploited hosts\n" +
			":127.0.0.4:Listed\n" +
			"192.0.2.0/24\n" +
			"203.0.113.9 :127.0.0.4:a listing's text\n" +
			"; a comment\n" +
			"\n",
		"pbl.rbldnsd":  "198.51.100.7\n2001:db8::/32 :127.0.0.10:policy\n",
		"README.txt":   "not a zone file, and not read\n",
		"botnet.other": "192.0.2.1\n",
	}, map[string]time.Time{"exploit.rbldnsd": newer, "pbl.rbldnsd": older})
	loaded, currentTime, err := feedMirrorTestLoad(t, directory)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"192.0.2.0/24":    {"exploit"},
		"203.0.113.9/32":  {"exploit"},
		"198.51.100.7/32": {"pbl"},
		"2001:db8::/32":   {"pbl"},
	}
	if len(loaded) != len(want) {
		t.Errorf("loaded %v", loaded)
	}
	for prefix, categories := range want {
		if !slices.Equal(loaded[prefix], categories) {
			t.Errorf("%s loaded as %v", prefix, loaded[prefix])
		}
	}
	if !currentTime.Equal(older) {
		t.Errorf("current at %v, not the oldest file's %v", currentTime, older)
	}
}

// A file the adapter cannot read whole fails the load, an exclusion above all, so nothing it would misread
// can reject mail; so does a mirror with no zone file.
func TestFeedMirrorRefusesWhatItCannotReadWhole(t *testing.T) {
	for name, line := range map[string]string{
		"an exclusion":            "!192.0.2.7",
		"a range":                 "192.0.2.1-192.0.2.9",
		"an abbreviated address":  "10.1",
		"a prefix with host bits": "192.0.2.1/24",
		"text that is no value":   "192.0.2.1 listed",
		"an address with a zone":  "fe80::1%eth0",
	} {
		directory := feedMirrorTestDirectory(t, map[string]string{"exploit.rbldnsd": "192.0.2.0/24\n" + line + "\n"}, nil)
		if _, _, err := feedMirrorTestLoad(t, directory); err == nil {
			t.Errorf("%s: loaded", name)
		} else if strings.Contains(err.Error(), line) {
			t.Errorf("%s: the refusal quotes the line: %v", name, err)
		}
	}
	if _, _, err := feedMirrorTestLoad(t, t.TempDir()); err == nil {
		t.Error("a mirror with no zone file loaded")
	}
	if _, err := NewFeedMirrorSource(DefaultFeedMirrorSettings()); err == nil {
		t.Error("a mirror with no directory was made")
	}
}
