//go:build sidecar_pending

package quotahost

// QUOTA-M7-01's mx meter on T3 (§20.6, §15.4, §15.6, D-QUOTA-3, D-QUOTA-4, C9, X11; FI-13 (a)): mxedge's router
// role, with the spending MAIL-M7-19 gave it, takes senders' sessions on this package's Quota over the private
// stack's Postgres and Redis, its engine deps composed as server/mxedgehost composes the block's. A network's
// sessions share one Lease(network, QuotaMeterMx) in the process, the hello and every chunk spent before it moves,
// and the engine's usage batcher sends its 60 s window under producer mxedge; ApplyUsageBatch journals the lease's
// carve, and the debit flush takes it from the balance and adds it to network_service_usage under meter mx. FI-13
// (a)'s oracle reads the balances and every meter's count after each step: each byte the edge moved debits once, at
// the balance and at the meter. The Service contract (C9) the edge's stream to the receiver rides escrows and
// settles nothing, so the lease is the one payer (D-QUOTA-4).
//
// Inline (I-12, §22.5): a session whose network's lease opens on the balance reading the lease set holds, as every
// session does but a process's first of a network in BalanceRefreshAge, runs from the banner to the splice's end
// with every PostgreSQL acquisition in the process refused and counted (server.DenyPostgresForTest), and makes none.
//
// §20.6's empty balance on the same path: a lease run dry resets every splice of its network with RST_QUOTA and
// marks the network spent; a network with no balance is refused its lease, and its session closes in the
// handshake, the temporary failure §15.6 gives, its sender's plaintext retry getting 451 4.7.0 from the fleet's
// closed-handshake memory. A network another producer made spent while this process's lease of it still holds
// bytes is refused the same way at Find, whose step 4 reads sockets' over-limit list, re-read every 5 s: a cached
// list, so that refusal also reaches no PostgreSQL. The storage limit's 452 4.2.2 at RCPT is the receiver's, from
// GET /mail/status (§15.5, MAIL-M7-14), since the router never reads past the hello, and FS-47's mail case stays
// MAIL's.
//
// What is not server's is a fake or scripted: the engine's {mxr_} routes, daily key and closed-handshake memory in
// FakeRedis, its gates and metrics, its connection records (FakeMxEdgeStore), and the dialer, which stands for Find
// and the receiver's accept and opens a stream the test plays as the receiver. The stream, the recording Quota and
// the ClientHello are tls_meter_t3_test.go's. Time is the T3 target's fake clock, which the Quota, the engine and
// sockets' gate share; a real timer only fails a hung test.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"
	"github.com/urnetwork/sidecar/socketsservice"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/mxedgehost"
)

// The synthetic names and documentation addresses the tests use (connect/CODESTYLE.md's test data rule).
const (
	// The root receivers' names sit under: mx.<label>.<root>.
	mxMeterRoot           = "root.example"
	mxMeterBannerHostName = "mx.banner.example"
	mxMeterDeviationUrl   = "https://deviation.example/rfc3207"
	mxMeterEhloName       = "relay.sender.example"
	// The lb's address a session's PROXY line names, which the engine does not keep.
	mxMeterLbAddress = "192.0.2.25"
	// The engine's probe interval, apart from every other timer it sets, so a test sees the first probe land by
	// its timer alone.
	mxMeterProbeInterval = 7 * time.Second
)

// Two senders in two /24s, each with a sibling address in its own /24, RFC 5737's.
const (
	mxMeterSenderAddress        = "198.51.100.10"
	mxMeterSenderSiblingAddress = "198.51.100.11"
	mxMeterOtherSenderAddress   = "203.0.113.20"
	mxMeterOtherSiblingAddress  = "203.0.113.21"
)

// What a plaintext transaction gets within an hour of a closed handshake from its sender's /24 (§15.4).
const mxMeterTemporaryReply = "451 4.7.0 TLS required, try again later"

// A listener the test feeds.
type mxMeterListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func newMxMeterListener() *mxMeterListener {
	return &mxMeterListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (self *mxMeterListener) Accept() (net.Conn, error) {
	select {
	case conn := <-self.conns:
		return conn, nil
	case <-self.closed:
		return nil, net.ErrClosed
	}
}

func (self *mxMeterListener) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

func (self *mxMeterListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(mxMeterLbAddress), Port: 25}
}

// The engine's end of one session, reporting the sender the lb's PROXY listener would.
type mxMeterServerConn struct {
	net.Conn
	sender net.Addr
}

func (self *mxMeterServerConn) RemoteAddr() net.Addr {
	return self.sender
}

// Find and the receiver's accept, scripted: each Dial holds the open's MailIngress to its format and, unless
// Find's step 4 would answer quota for the network, opens a stream the test plays as the receiver, the hello
// already in it as the early data. A network spent answers true for is refused as sockets refuses it.
type mxMeterDialer struct {
	clientIds []sidecar.Id
	opened    chan *tlsMeterStream

	stateLock sync.Mutex
	// Find's step 4: sockets' gate's over-limit list; nil reads no network as spent
	spent     func(networkId connect.Id) bool
	dialCount int
	ingresses []*protocol.MailIngress
}

func newMxMeterDialer(clientCount int) *mxMeterDialer {
	dialer := &mxMeterDialer{opened: make(chan *tlsMeterStream, 64)}
	for range clientCount {
		dialer.clientIds = append(dialer.clientIds, sidecar.NewId())
	}
	return dialer
}

var _ mxedgeservice.MxEdgeDialer = (*mxMeterDialer)(nil)

func (self *mxMeterDialer) ClientIds() []sidecar.Id {
	return slices.Clone(self.clientIds)
}

func (self *mxMeterDialer) Ready() bool {
	return true
}

// Reads the network's spent mark through spent from now on, as Find's step 4 reads sockets' gate.
func (self *mxMeterDialer) setSpent(spent func(networkId connect.Id) bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.spent = spent
}

func (self *mxMeterDialer) Dial(ctx context.Context, request *mxedgeservice.MxEdgeDialRequest) (mxedgeservice.MxEdgeStream, uint64, error) {
	metadata, err := request.Metadata(0)
	if err != nil {
		return nil, 0, err
	}
	ingress, err := sidecar.ParseMailIngress(metadata, sidecar.DefaultMailIngressFormatSettings().MaxByteCount)
	if err != nil {
		return nil, 0, err
	}
	spent := func() func(networkId connect.Id) bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.dialCount += 1
		self.ingresses = append(self.ingresses, ingress)
		return self.spent
	}()
	if spent != nil && spent(connect.Id(request.NetworkId)) {
		return nil, 0, &sidecar.SocketCodeError{Err: sidecar.ErrSocketQuota}
	}
	stream := newTlsMeterStream()
	stream.toListener.Write(request.HelloBytes)
	self.opened <- stream
	return stream, 0, nil
}

// How many Finds the engine asked, and the MailIngress of the last.
func (self *mxMeterDialer) dials() (int, *protocol.MailIngress) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.ingresses) == 0 {
		return self.dialCount, nil
	}
	return self.dialCount, self.ingresses[len(self.ingresses)-1]
}

func (self *mxMeterDialer) awaitOpened(t *testing.T) *tlsMeterStream {
	t.Helper()
	select {
	case stream := <-self.opened:
		return stream
	case <-time.After(tlsMeterGuardTimeout):
		t.Fatal("no stream was opened")
	}
	return nil
}

// t3OracleSource's reading with every meter's count: the network's network_service_usage rows by meter and the
// journal's rows on its balances still to apply, so a count moves with its debit, in the flush's one transaction.
// tls_meter_t3_test.go's source reads meter tls alone; this one serves the mx and smtp meters' tests, where
// another producer's bytes may land on the same balances.
type edgeMeterOracleSource struct {
	t3OracleSource
}

func (self *edgeMeterOracleSource) ReadQuota(ctx context.Context, networkId connect.Id) (*sidecartest.QuotaOracleReading, error) {
	reading, err := self.t3OracleSource.ReadQuota(ctx, networkId)
	if err != nil {
		return nil, err
	}
	meterUsedByteCounts := map[string]int64{}
	server.Db(self.ctx, func(conn server.PgConn) {
		add := func(query string) {
			result, err := conn.Query(self.ctx, query, server.Id(networkId))
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var meter string
					var byteCount int64
					server.Raise(result.Scan(&meter, &byteCount))
					meterUsedByteCounts[meter] += byteCount
				}
			})
		}
		add(`SELECT meter, COALESCE(SUM(quantity), 0) FROM network_service_usage WHERE network_id = $1 GROUP BY meter`)
		add(`
			SELECT service_debit_journal.meter, COALESCE(SUM(service_debit_journal.debit_byte_count), 0)
			FROM service_debit_journal
			INNER JOIN transfer_balance ON transfer_balance.balance_id = service_debit_journal.balance_id
			WHERE transfer_balance.network_id = $1 AND NOT service_debit_journal.applied
			GROUP BY service_debit_journal.meter
		`)
	})
	reading.MeterUsedByteCounts = meterUsedByteCounts
	return reading, nil
}

// The batch's transfer bytes on meter for the network, and whether it carried any other event for it.
func edgeMeterBatchByteCount(batch *sidecar.UsageBatch, networkId connect.Id, meter string) (byteCount int64, other bool) {
	for _, event := range batch.Events {
		if event.NetworkId != networkId {
			continue
		}
		if event.Meter != meter || event.StoredByteDelta != 0 {
			other = true
			continue
		}
		byteCount += event.TransferByteCount
	}
	return
}

// The journal's rows on the network's balances: their bytes on meter still to apply, the batches they came in,
// and how many rows of any kind there are.
func edgeMeterJournal(ctx context.Context, networkId connect.Id, meter string) (pendingByteCount int64, batchIds []server.Id, rowCount int) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT service_debit_journal.meter, service_debit_journal.debit_byte_count, service_debit_journal.batch_id, service_debit_journal.applied
				FROM service_debit_journal
				INNER JOIN transfer_balance ON transfer_balance.balance_id = service_debit_journal.balance_id
				WHERE transfer_balance.network_id = $1
			`,
			server.Id(networkId),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var rowMeter string
				var debitByteCount int64
				var batchId *server.Id
				var applied bool
				server.Raise(result.Scan(&rowMeter, &debitByteCount, &batchId, &applied))
				rowCount += 1
				if rowMeter == meter && !applied {
					pendingByteCount += debitByteCount
				}
				if batchId != nil {
					batchIds = append(batchIds, *batchId)
				}
			}
		})
	})
	return
}

// The network's network_service_usage rows under meter, by UTC day.
func edgeMeterUsageDays(ctx context.Context, networkId connect.Id, meter string) map[string]int64 {
	dayQuantities := map[string]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT usage_day, quantity FROM network_service_usage WHERE network_id = $1 AND meter = $2`,
			server.Id(networkId),
			meter,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var usageDay time.Time
				var quantity int64
				server.Raise(result.Scan(&usageDay, &quantity))
				dayQuantities[usageDay.Format("2006-01-02")] = quantity
			}
		})
	})
	return dayQuantities
}

// One network a world routes: its label's {mxr_} route, the mxedge gate open to it, and a balance of
// balanceByteCount, none when zero.
type mxMeterNetwork struct {
	networkId        connect.Id
	balanceByteCount int64
}

// The world of one test: the T3 target, mxedge's router on it serving a listener the test feeds, and the networks
// it routes.
type mxMeterWorld struct {
	ctx      context.Context
	target   *quotasuite.Target
	quota    *tlsMeterQuota
	host     *sidecartest.FakeHost
	store    *sidecartest.FakeMxEdgeStore
	dialer   *mxMeterDialer
	settings *mxedgeservice.MxEdgeSettings
	engine   *mxedgeservice.MxEdgeService
	listener *mxMeterListener
	oracle   *sidecartest.QuotaOracle
	// when the engine's usage batcher opened its first window
	start  time.Time
	labels map[connect.Id]string
}

// Pro's tier, which the lease's draw size reads, and the message cap the splice's byte bound reads.
func mxMeterLimits() *sidecar.QuotaLimits {
	return &sidecar.QuotaLimits{Tier: "pro", Limits: map[string]int64{
		"mail_inbound_message_bytes": 50 * 1024 * 1024,
	}}
}

// A ready router on a fresh T3 target, with each network's route, balance and gate set before it serves.
func newMxMeterWorld(t *testing.T, ctx context.Context, networks ...*mxMeterNetwork) *mxMeterWorld {
	t.Helper()
	target := newT3Target(ctx)(t)
	host := sidecartest.NewFakeHost(ctx, target.Clock, nil, nil, nil)
	world := &mxMeterWorld{
		ctx:      ctx,
		target:   target,
		quota:    newTlsMeterQuota(target.Quota),
		host:     host,
		store:    sidecartest.NewFakeMxEdgeStore(nil),
		listener: newMxMeterListener(),
		oracle:   sidecartest.NewQuotaOracle(&edgeMeterOracleSource{t3OracleSource: t3OracleSource{ctx: ctx}}),
		labels:   map[connect.Id]string{},
	}
	networkIds := []connect.Id{}
	for _, network := range networks {
		if err := target.SetLimits(network.networkId, mxMeterLimits()); err != nil {
			t.Fatal(err)
		}
		balances := []*sidecartest.QuotaBalance{}
		if 0 < network.balanceByteCount {
			balances = append(balances, &sidecartest.QuotaBalance{ByteCount: network.balanceByteCount})
		}
		if err := target.SetBalances(network.networkId, balances); err != nil {
			t.Fatal(err)
		}
		world.oracle.Grant(network.networkId, network.balanceByteCount)
		// the route model.ProjectMailRoutes writes for a mail-enabled network's label: the whole id, since two
		// ids made in one millisecond share their leading bytes
		label := "m" + hex.EncodeToString(network.networkId[:])
		routeValue, err := json.Marshal(&mxedgeservice.MailRoute{NetworkId: sidecar.Id(network.networkId), Label: label, Host: "mx"})
		if err != nil {
			t.Fatal(err)
		}
		if err := host.Redis.Set(sidecartest.InspectContext(ctx), mxedgeservice.MailRouteKey(label), routeValue, 0); err != nil {
			t.Fatal(err)
		}
		world.labels[network.networkId] = label
		networkIds = append(networkIds, network.networkId)
	}
	host.Gates.OpenService(sidecar.ServiceGateMxedge, networkIds...)

	serviceHost := host.ServiceHost()
	serviceHost.Quota = world.quota
	settings := mxedgeservice.DefaultMxEdgeSettings()
	settings.BannerHostName = mxMeterBannerHostName
	settings.DeviationUrl = mxMeterDeviationUrl
	settings.RouteRootDomainName = mxMeterRoot
	settings.ReadyProbeInterval = mxMeterProbeInterval
	// the records wait for the engine's close
	settings.RecordFlushInterval = 1000 * time.Hour
	world.settings = settings
	world.dialer = newMxMeterDialer(settings.Clients)
	world.start = target.Clock.Now()
	engine, err := mxedgeservice.NewMxEdgeRouterService(
		ctx,
		serviceHost,
		world.store,
		mxedgehost.NewEngineDeps(serviceHost, nil),
		&mxedgeservice.MxEdgeRouterDeps{Dialer: world.dialer},
		settings,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	world.engine = engine
	// the first probe has read the store and made today's k once its next wait is armed
	world.awaitTimerAt(t, world.start.Add(mxMeterProbeInterval))
	if !engine.Ready() {
		t.Fatal("the router is not ready")
	}
	go engine.Serve(world.listener)
	t.Cleanup(func() { world.listener.Close() })
	return world
}

// Waits until a timer is set for at, so a step moves the fake clock only once the wait it means is armed.
func (self *mxMeterWorld) awaitTimerAt(t *testing.T, at time.Time) {
	t.Helper()
	guard := time.After(tlsMeterGuardTimeout)
	for {
		pending, added := self.target.Clock.WatchTimers()
		if slices.ContainsFunc(pending, at.Equal) {
			return
		}
		select {
		case <-added:
		case <-guard:
			t.Fatalf("no timer was set for %v", at)
		}
	}
}

// The name the network's receivers answer at, which a sender's SNI names.
func (self *mxMeterWorld) sni(networkId connect.Id) string {
	return "mx." + self.labels[networkId] + "." + mxMeterRoot
}

func (self *mxMeterWorld) check(t *testing.T) {
	t.Helper()
	if err := self.oracle.Check(self.ctx); err != nil {
		t.Fatal(err)
	}
}

func (self *mxMeterWorld) checkSettled(t *testing.T) {
	t.Helper()
	if err := self.target.Settle(); err != nil {
		t.Fatal(err)
	}
	if err := self.oracle.CheckSettled(self.ctx); err != nil {
		t.Fatal(err)
	}
}

// Moves the clock to the end of the engine's first usage window and answers the batch api took for it.
func (self *mxMeterWorld) sendFirstWindow(t *testing.T) *tlsMeterReport {
	t.Helper()
	self.target.Clock.AdvanceTo(self.start.Add(self.settings.Usage.WindowTimeout))
	report := self.quota.awaitReport(t, 1)
	if report.err != nil {
		t.Fatalf("api refused the window's batch: %v", report.err)
	}
	if report.batch.Producer != sidecar.QuotaProducerMxEdge {
		t.Fatalf("a batch of producer %q", report.batch.Producer)
	}
	return report
}

// The engine's connection records, once its close has written its last batch.
func (self *mxMeterWorld) closeRecords() []*mxedgeservice.MxConnectionRecord {
	self.engine.Close()
	return self.store.Records()
}

// A sender's end of one session.
type mxMeterSender struct {
	t      *testing.T
	world  *mxMeterWorld
	conn   net.Conn
	reader *bufio.Reader
}

// Connects as sender, an address with no port, as the lb's PROXY listener hands the engine a session.
func (self *mxMeterWorld) connect(t *testing.T, sender string) *mxMeterSender {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })
	clientConn.SetDeadline(time.Now().Add(tlsMeterGuardTimeout))
	select {
	case self.listener.conns <- &mxMeterServerConn{Conn: serverConn, sender: &net.TCPAddr{IP: net.ParseIP(sender), Port: 2525}}:
	case <-time.After(tlsMeterGuardTimeout):
		t.Fatal("the engine never accepted")
	}
	return &mxMeterSender{t: t, world: self, conn: clientConn, reader: bufio.NewReader(clientConn)}
}

func (self *mxMeterSender) write(b []byte) {
	self.t.Helper()
	if _, err := self.conn.Write(b); err != nil {
		self.t.Fatalf("the sender wrote: %v", err)
	}
}

// Reads one line and compares it, CRLF aside.
func (self *mxMeterSender) expect(want string) {
	self.t.Helper()
	line, err := self.reader.ReadString('\n')
	if err != nil || line != want+"\r\n" {
		self.t.Fatalf("the sender read %q, %v; want %q", line, err, want)
	}
}

// Reads exactly byteCount bytes.
func (self *mxMeterSender) read(byteCount int) []byte {
	self.t.Helper()
	read := make([]byte, byteCount)
	if _, err := io.ReadFull(self.reader, read); err != nil {
		self.t.Fatalf("the sender read: %v", err)
	}
	return read
}

// Reads to the end, which must come with nothing more said.
func (self *mxMeterSender) expectEnd() {
	self.t.Helper()
	if rest, err := io.ReadAll(self.reader); err != nil || len(rest) != 0 {
		self.t.Fatalf("the sender's end: %d bytes, %v", len(rest), err)
	}
}

// The banner and EHLO before TLS.
func (self *mxMeterSender) ehlo() {
	self.t.Helper()
	self.expect("220 " + mxMeterBannerHostName + " ESMTP")
	self.write([]byte("EHLO " + mxMeterEhloName + "\r\n"))
	self.expect("250-" + mxMeterBannerHostName)
	self.expect("250-ENHANCEDSTATUSCODES")
	self.expect("250 STARTTLS")
}

// STARTTLS and its 220 once the quiet time has passed.
func (self *mxMeterSender) startTls() {
	self.t.Helper()
	clock := self.world.target.Clock
	quietAt := clock.Now().Add(self.world.settings.StartTlsQuietTimeout)
	self.write([]byte("STARTTLS\r\n"))
	self.world.awaitTimerAt(self.t, quietAt)
	clock.Advance(self.world.settings.StartTlsQuietTimeout)
	self.expect("220 2.0.0 Ready to start TLS")
}

// The dialog to STARTTLS's 220, then the ClientHello for the network's name, which the router reads and routes
// on. Answers the hello's bytes.
func (self *mxMeterSender) hello(networkId connect.Id) []byte {
	self.t.Helper()
	self.ehlo()
	self.startTls()
	hello := tlsMeterHello(self.world.sni(networkId))
	self.write(hello)
	return hello
}

// A plaintext transaction's first reply, then QUIT.
func (self *mxMeterSender) plaintextMail(want string) {
	self.t.Helper()
	self.ehlo()
	self.write([]byte("MAIL FROM:<a@sender.example>\r\n"))
	self.expect(want)
	self.write([]byte("QUIT\r\n"))
	self.expect("221 2.0.0 Bye")
	self.expectEnd()
}

// The handshake closed with the one alert the router writes for everything it cannot route past.
func (self *mxMeterSender) expectClosedHandshake() {
	self.t.Helper()
	alert := tlsMeterAlert(tlsedgeservice.TlsAlertCode(mxedgeservice.MxTlsAlertInternalError))
	if read := self.read(len(alert)); !bytes.Equal(read, alert) {
		self.t.Fatalf("the sender read %x, want the internal_error alert", read)
	}
	self.expectEnd()
}

// One session spliced to the receiver's stream, which has read the hello.
type mxMeterSession struct {
	sender *mxMeterSender
	stream *tlsMeterStream
	hello  []byte
}

// A session of the network from sender, through to the receiver's stream.
func (self *mxMeterWorld) splice(t *testing.T, networkId connect.Id, sender string) *mxMeterSession {
	t.Helper()
	session := &mxMeterSession{sender: self.connect(t, sender)}
	session.hello = session.sender.hello(networkId)
	session.stream = self.dialer.awaitOpened(t)
	if read := session.stream.listenerRead(t, len(session.hello)); !bytes.Equal(read, session.hello) {
		t.Fatal("the receiver's hello is not the sender's")
	}
	if _, ingress := self.dialer.dials(); ingress.GetSni() != self.sni(networkId) || ingress.GetEhloName() != mxMeterEhloName {
		t.Fatalf("the open's MailIngress names %q and %q", ingress.GetSni(), ingress.GetEhloName())
	}
	return session
}

// toReceiver from the sender and toSender back, then the receiver's close, which ends the splice: answers the
// bytes the session moved past its hello.
func (self *mxMeterSession) exchange(t *testing.T, toReceiver []byte, toSender []byte) int64 {
	t.Helper()
	self.sender.write(toReceiver)
	if read := self.stream.listenerRead(t, len(toReceiver)); !bytes.Equal(read, toReceiver) {
		t.Fatal("the receiver's bytes are not the sender's")
	}
	self.stream.listenerWrite(t, toSender)
	if read := self.sender.read(len(toSender)); !bytes.Equal(read, toSender) {
		t.Fatal("the sender's bytes are not the receiver's")
	}
	self.stream.listenerCloseWrite()
	self.sender.expectEnd()
	self.stream.awaitEngineClose(t)
	return int64(len(toReceiver) + len(toSender))
}

// The money path: a session's bytes both ways, the hello included, spent on the network's one lease on meter mx,
// sent at the window's end under producer mxedge, journaled as the lease's carve, and debited once by the flush,
// which counts them in network_service_usage under meter mx; the Service contract the stream rides escrows nothing
// and its settlement takes nothing.
func TestMxMeterT3MoneyPath(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		// the process's limit snapshot is dropped at the end, so the next test reads limits as a fresh process does
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		ctx := t.Context()
		balanceByteCount := int64(1024 * 1024)
		payer := &mxMeterNetwork{networkId: connect.NewId(), balanceByteCount: balanceByteCount}

		// the receiving network and urnetwork-edge's, each with a client, for the Service contract
		payerNetworkId := server.Id(payer.networkId)
		edgeNetworkId := server.NewId()
		receiverClientId := server.NewId()
		edgeClientId := server.NewId()
		model.Testing_CreateNetwork(ctx, payerNetworkId, "mx-meter-"+payerNetworkId.String(), server.NewId())
		model.Testing_CreateDevice(ctx, payerNetworkId, server.NewId(), receiverClientId, "synthetic", "synthetic")
		model.Testing_CreateNetwork(ctx, edgeNetworkId, "mx-meter-edge-"+edgeNetworkId.String(), server.NewId())
		model.Testing_CreateDevice(ctx, edgeNetworkId, server.NewId(), edgeClientId, "synthetic", "synthetic")

		world := newMxMeterWorld(t, ctx, payer)
		session := world.splice(t, payer.networkId, mxMeterSenderAddress)

		// the edge's stream to the receiver rides a Service contract, as CreateContract issues it for the pair
		// Find recorded (§6.10.1): no escrow on either network
		contractId, _, err := model.CreateContractNoEscrowWithExpiration(ctx, edgeNetworkId, edgeClientId, payerNetworkId, receiverClientId, model.ByteCount(1024*1024), false)
		if err != nil {
			t.Fatal(err)
		}
		for _, networkId := range []server.Id{payerNetworkId, edgeNetworkId} {
			if open := model.GetOpenTransferByteCount(ctx, networkId); open != 0 {
				t.Fatalf("a Service contract escrowed %d", open)
			}
		}

		movedByteCount := session.exchange(t, bytes.Repeat([]byte{0x5a}, 4000), bytes.Repeat([]byte{0xa5}, 3000))
		world.quota.awaitLeasesClosed(t, payer.networkId)
		if meters := world.quota.leaseMeters(payer.networkId); !slices.Equal(meters, []string{sidecar.QuotaMeterMx}) {
			t.Fatalf("the session's leases: %v", meters)
		}
		meteredByteCount := int64(len(session.hello)) + movedByteCount
		world.oracle.Meter(payer.networkId, sidecar.QuotaMeterMx, meteredByteCount)
		world.check(t)

		// the contract settles from both parties' reports of those bytes: nothing moves
		for _, clientId := range []server.Id{edgeClientId, receiverClientId} {
			if err := model.CloseContract(ctx, contractId, clientId, model.ByteCount(meteredByteCount), false); err != nil {
				t.Fatal(err)
			}
		}
		if open := model.GetOpenTransferByteCount(ctx, payerNetworkId); open != 0 {
			t.Fatalf("the settled Service contract left %d open", open)
		}
		if balances := t3ReadBalances(ctx, payerNetworkId); len(balances) != 1 || balances[0].ByteCount != balanceByteCount {
			t.Fatalf("the Service contract's settlement moved the balance: %+v", balances)
		}
		world.check(t)

		// the window's batch: the lease's bytes, journaled as its carve
		report := world.sendFirstWindow(t)
		if byteCount, other := edgeMeterBatchByteCount(report.batch, payer.networkId, sidecar.QuotaMeterMx); byteCount != meteredByteCount || other {
			t.Fatalf("the batch carried %d mx bytes (other events %t), the session moved %d", byteCount, other, meteredByteCount)
		}
		if producer := tlsMeterBatchProducer(ctx, report.batch.BatchId); producer != sidecar.QuotaProducerMxEdge {
			t.Fatalf("service_usage_batch holds the batch as %q", producer)
		}
		pendingByteCount, batchIds, _ := edgeMeterJournal(ctx, payer.networkId, sidecar.QuotaMeterMx)
		if pendingByteCount != meteredByteCount {
			t.Fatalf("%d mx bytes journaled, the session moved %d", pendingByteCount, meteredByteCount)
		}
		for _, batchId := range batchIds {
			if batchId != server.Id(report.batch.BatchId) {
				t.Fatalf("a journal row of batch %s, not the window's", batchId)
			}
		}
		world.check(t)

		// the flush: one debit, counted on the day it applied
		world.checkSettled(t)
		day := world.target.Clock.Now().UTC().Format("2006-01-02")
		if days := edgeMeterUsageDays(ctx, payer.networkId, sidecar.QuotaMeterMx); len(days) != 1 || days[day] != meteredByteCount {
			t.Fatalf("network_service_usage holds %v under mx, want %d on %s", days, meteredByteCount, day)
		}
		if balances := t3ReadBalances(ctx, payerNetworkId); len(balances) != 1 || balances[0].ByteCount != balanceByteCount-meteredByteCount || balances[0].ReservedByteCount != 0 {
			t.Fatalf("after the flush: %+v, want %d left and nothing reserved", balances, balanceByteCount-meteredByteCount)
		}
		if _, _, rowCount := edgeMeterJournal(ctx, payer.networkId, sidecar.QuotaMeterMx); rowCount != 0 {
			t.Fatalf("%d journal rows left after the flush", rowCount)
		}
		if open := model.GetOpenTransferByteCount(ctx, payerNetworkId); open != 0 {
			t.Fatalf("%d open after the flush", open)
		}
		if records := world.closeRecords(); len(records) != 1 || records[0].Result != "splice_closed" || records[0].ByteCount != meteredByteCount {
			t.Fatalf("the engine's records: %+v", records)
		}
	})
}

// I-12 on the mx meter's path: a session whose lease opens on the reading the lease set holds from the network's
// last session runs from the banner through the splice to its lease's close with every PostgreSQL acquisition in
// the process refused, and makes none; the tripwire is armed, since a read the test makes inside it counts. Both
// sessions' bytes then debit once.
func TestMxMeterT3SessionPathReachesNoPostgres(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		ctx := t.Context()
		balanceByteCount := int64(1024 * 1024)
		payer := &mxMeterNetwork{networkId: connect.NewId(), balanceByteCount: balanceByteCount}
		world := newMxMeterWorld(t, ctx, payer)

		// the network's first session in the process reads its balances and its limits
		first := world.splice(t, payer.networkId, mxMeterSenderAddress)
		meteredByteCount := int64(len(first.hello)) + first.exchange(t, bytes.Repeat([]byte{1}, 2000), bytes.Repeat([]byte{2}, 1000))
		world.quota.awaitLeasesClosed(t, payer.networkId)

		attempts, stop := server.DenyPostgresForTest(t)
		func() {
			defer func() { recover() }()
			model.GetNetworkQuotaState(ctx, server.Id(payer.networkId))
		}()
		if armed := attempts(); armed != 1 {
			t.Fatalf("a read inside the tripwire counted %d", armed)
		}
		// the next one, inside BalanceRefreshAge of the reading: route, caps, a new lease's first draw, the Find,
		// the splice both ways and the lease's settle at its close
		second := world.splice(t, payer.networkId, mxMeterOtherSenderAddress)
		meteredByteCount += int64(len(second.hello)) + second.exchange(t, bytes.Repeat([]byte{3}, 3000), bytes.Repeat([]byte{4}, 500))
		world.quota.awaitLeasesClosed(t, payer.networkId)
		stop()
		if count := attempts(); count != 1 {
			t.Fatalf("the session reached PostgreSQL %d times (I-12)", count-1)
		}
		if meters := world.quota.leaseMeters(payer.networkId); !slices.Equal(meters, []string{sidecar.QuotaMeterMx, sidecar.QuotaMeterMx}) {
			t.Fatalf("the sessions' leases: %v", meters)
		}
		world.oracle.Meter(payer.networkId, sidecar.QuotaMeterMx, meteredByteCount)
		world.check(t)

		report := world.sendFirstWindow(t)
		if byteCount, other := edgeMeterBatchByteCount(report.batch, payer.networkId, sidecar.QuotaMeterMx); byteCount != meteredByteCount || other {
			t.Fatalf("the batch carried %d mx bytes (other events %t), the sessions moved %d", byteCount, other, meteredByteCount)
		}
		world.check(t)
		world.checkSettled(t)
		if balances := t3ReadBalances(ctx, server.Id(payer.networkId)); len(balances) != 1 || balances[0].ByteCount != balanceByteCount-meteredByteCount || balances[0].ReservedByteCount != 0 {
			t.Fatalf("after the flush: %+v, want %d left and nothing reserved", balances, balanceByteCount-meteredByteCount)
		}
	})
}

// §20.6's empty balance at mxedge: a lease that runs dry resets every splice of its network with RST_QUOTA and
// marks it spent; a network with no balance is refused its lease, nothing is dialed, and its session closes in the
// handshake, a temporary failure, its sender's plaintext retry from its /24 getting 451 4.7.0; what moved before
// the lease ran out debits once, never past the balance.
func TestMxMeterT3EmptyBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		ctx := t.Context()
		dryByteCount := int64(4 * 1024)
		dry := &mxMeterNetwork{networkId: connect.NewId(), balanceByteCount: dryByteCount}
		empty := &mxMeterNetwork{networkId: connect.NewId()}
		world := newMxMeterWorld(t, ctx, dry, empty)

		// two sessions hold the network's one lease, a byte back on each so both splice before it runs out
		session := world.splice(t, dry.networkId, mxMeterSenderAddress)
		idle := world.splice(t, dry.networkId, mxMeterOtherSenderAddress)
		for _, each := range []*mxMeterSession{session, idle} {
			each.stream.listenerWrite(t, []byte{1})
			each.sender.read(1)
		}
		// more than the balance holds: the lease runs dry, and both splices reset
		go session.sender.conn.Write(bytes.Repeat([]byte{0x5a}, 2*int(dryByteCount)))
		if code := session.stream.awaitReset(t); code != protocol.StreamResetCode_RST_QUOTA {
			t.Fatalf("a lease run dry reset its stream with %v", code)
		}
		if code := idle.stream.awaitReset(t); code != protocol.StreamResetCode_RST_QUOTA {
			t.Fatalf("a lease run dry reset the idle stream with %v", code)
		}
		world.quota.awaitLeasesClosed(t, dry.networkId)
		if meters := world.quota.leaseMeters(dry.networkId); !slices.Equal(meters, []string{sidecar.QuotaMeterMx}) {
			t.Fatalf("the two sessions' leases: %v", meters)
		}
		if tlsMeterSpentTime(t, ctx, dry.networkId) == nil {
			t.Fatal("a lease run dry left its network unspent")
		}
		// the hellos and the two bytes back, and what reached the receiver before the lease ran out
		meteredByteCount := int64(len(session.hello)+len(idle.hello)+2) + session.stream.engineByteCount.Load() + idle.stream.engineByteCount.Load()
		if dryByteCount < meteredByteCount {
			t.Fatalf("%d bytes moved on a balance of %d", meteredByteCount, dryByteCount)
		}
		world.oracle.Meter(dry.networkId, sidecar.QuotaMeterMx, meteredByteCount)
		world.check(t)

		// a network with no balance: refused its lease before any Find, its handshake closed
		dialCount, _ := world.dialer.dials()
		refused := world.connect(t, mxMeterSenderAddress)
		refused.hello(empty.networkId)
		refused.expectClosedHandshake()
		if count, _ := world.dialer.dials(); count != dialCount {
			t.Fatal("a session refused its lease was dialed")
		}
		if meters := world.quota.leaseMeters(empty.networkId); len(meters) != 0 {
			t.Fatalf("a network with no balance holds leases: %v", meters)
		}
		if tlsMeterSpentTime(t, ctx, empty.networkId) == nil {
			t.Fatal("a refused lease left its network unspent")
		}
		// the sender's plaintext retry, from another address in its /24: a temporary failure, never 530
		world.connect(t, mxMeterSenderSiblingAddress).plaintextMail(mxMeterTemporaryReply)

		// what moved debits once at the window, never past the balance
		report := world.sendFirstWindow(t)
		if byteCount, other := edgeMeterBatchByteCount(report.batch, dry.networkId, sidecar.QuotaMeterMx); byteCount != meteredByteCount || other {
			t.Fatalf("the batch carried %d mx bytes (other events %t), %d moved", byteCount, other, meteredByteCount)
		}
		if byteCount, other := edgeMeterBatchByteCount(report.batch, empty.networkId, sidecar.QuotaMeterMx); byteCount != 0 || other {
			t.Fatalf("the batch carried %d bytes for the network with no balance", byteCount)
		}
		world.check(t)
		world.checkSettled(t)
		if balances := t3ReadBalances(ctx, server.Id(dry.networkId)); len(balances) != 1 || balances[0].ByteCount != dryByteCount-meteredByteCount || balances[0].ReservedByteCount != 0 {
			t.Fatalf("after the flush: %+v, want %d left and nothing reserved", balances, dryByteCount-meteredByteCount)
		}
		if days := edgeMeterUsageDays(ctx, empty.networkId, sidecar.QuotaMeterMx); len(days) != 0 {
			t.Fatalf("the network with no balance counts mx usage: %v", days)
		}
		results := []string{}
		for _, record := range world.closeRecords() {
			results = append(results, record.Result)
		}
		slices.Sort(results)
		if !slices.Equal(results, []string{"spent", "splice_quota", "splice_quota"}) {
			t.Fatalf("the engine's records: %v", results)
		}
	})
}

// A network evs made spent (§20.6: an evs debit past every balance's room), while this process's lease of it still
// holds bytes for a live session: sockets' over-limit list holds the mark at its next read, within 5 s, and the
// network's next session, admitted to the lease it shares, is refused at Find from that cached list, closing in the
// handshake as spent with no PostgreSQL acquisition on its path; its sender's plaintext retry gets 451 4.7.0. The
// live session's bytes and evs's still debit once.
func TestMxMeterT3SpentNetworkRefusedFromTheCachedList(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		ctx := t.Context()
		balanceByteCount := int64(1024 * 1024)
		payer := &mxMeterNetwork{networkId: connect.NewId(), balanceByteCount: balanceByteCount}
		world := newMxMeterWorld(t, ctx, payer)

		// sockets' gate on the same Quota and clock, its first read taken now
		gateStart := world.target.Clock.Now()
		gateHost := &sidecar.ServiceHost{Quota: world.target.Quota, Metrics: sidecartest.NewFakeMetrics(nil), Clock: world.target.Clock}
		gate, err := socketsservice.NewSocketProjectionGate(ctx, gateHost, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(gate.Close)
		gateReads := func() (uint64, chan struct{}) {
			count, changed := gate.OverLimitReads()
			return uint64(count), changed
		}
		tlsMeterAwaitCount(t, "the gate's first over-limit read", 1, gateReads)
		world.dialer.setSpent(gate.Spent)

		// a live session holds the network's lease, its draw unspent but for its hello and a byte back
		live := world.splice(t, payer.networkId, mxMeterSenderAddress)
		live.stream.listenerWrite(t, []byte{1})
		live.sender.read(1)

		// evs's window: reads of the network's repos past every balance's room, which overdraw and mark it spent
		evsByteCount := 2 * balanceByteCount
		evsBatch := &sidecar.UsageBatch{
			BatchId:     sidecar.NewUsageBatchId(connect.NewId(), 1),
			Producer:    sidecar.QuotaProducerEvs,
			WindowStart: world.target.Clock.Now().UTC().Truncate(time.Minute),
			Events:      []*sidecar.UsageEvent{{NetworkId: payer.networkId, Meter: sidecar.QuotaMeterEvs, TransferByteCount: evsByteCount}},
		}
		if err := world.target.Quota.ReportUsage(ctx, evsBatch); err != nil {
			t.Fatal(err)
		}
		world.oracle.Meter(payer.networkId, sidecar.QuotaMeterEvs, evsByteCount)
		if tlsMeterSpentTime(t, ctx, payer.networkId) == nil {
			t.Fatal("evs's debit past every balance left the network unspent")
		}
		world.check(t)

		// the gate holds the mark at its next read
		readCount, _ := gateReads()
		world.target.Clock.AdvanceTo(gateStart.Add(socketsservice.DefaultSocketGateSettings().OverLimitInterval))
		tlsMeterAwaitCount(t, "the gate's next over-limit read", readCount+1, gateReads)
		if !gate.Spent(payer.networkId) {
			t.Fatal("5 s after the mark the gate reads the network as not spent")
		}

		attempts, stop := server.DenyPostgresForTest(t)
		func() {
			defer func() { recover() }()
			model.GetNetworkQuotaState(ctx, server.Id(payer.networkId))
		}()
		if armed := attempts(); armed != 1 {
			t.Fatalf("a read inside the tripwire counted %d", armed)
		}
		// the next session: the route, its caps and the lease it shares pass, and Find refuses it from the list
		dialCount, _ := world.dialer.dials()
		refused := world.connect(t, mxMeterOtherSenderAddress)
		refused.hello(payer.networkId)
		refused.expectClosedHandshake()
		if count, _ := world.dialer.dials(); count != dialCount+1 {
			t.Fatalf("the refused session asked %d Finds", count-dialCount)
		}
		// its sender's plaintext retry, from another address in its /24
		world.connect(t, mxMeterOtherSiblingAddress).plaintextMail(mxMeterTemporaryReply)
		stop()
		if count := attempts(); count != 1 {
			t.Fatalf("the refusal reached PostgreSQL %d times (I-12)", count-1)
		}
		if meters := world.quota.leaseMeters(payer.networkId); !slices.Equal(meters, []string{sidecar.QuotaMeterMx}) {
			t.Fatalf("the sessions' leases: %v", meters)
		}

		// the live session ends; its bytes and evs's debit once
		live.stream.listenerCloseWrite()
		live.sender.expectEnd()
		live.stream.awaitEngineClose(t)
		world.quota.awaitLeasesClosed(t, payer.networkId)
		meteredByteCount := int64(len(live.hello) + 1)
		world.oracle.Meter(payer.networkId, sidecar.QuotaMeterMx, meteredByteCount)
		world.check(t)
		report := world.sendFirstWindow(t)
		if byteCount, other := edgeMeterBatchByteCount(report.batch, payer.networkId, sidecar.QuotaMeterMx); byteCount != meteredByteCount || other {
			t.Fatalf("the batch carried %d mx bytes (other events %t), the live session moved %d", byteCount, other, meteredByteCount)
		}
		world.check(t)
		world.checkSettled(t)
		if balances := t3ReadBalances(ctx, server.Id(payer.networkId)); len(balances) != 1 || balances[0].ByteCount != balanceByteCount-evsByteCount-meteredByteCount || balances[0].ReservedByteCount != 0 {
			t.Fatalf("after the flush: %+v, want %d left and nothing reserved", balances, balanceByteCount-evsByteCount-meteredByteCount)
		}
		results := []string{}
		for _, record := range world.closeRecords() {
			results = append(results, record.Result)
		}
		slices.Sort(results)
		if !slices.Equal(results, []string{"spent", "splice_closed"}) {
			t.Fatalf("the engine's records: %v", results)
		}
	})
}
