//go:build sidecar_pending

package quotahost

// QUOTA-M5-01's tls meter on T3 (§20.6, D-QUOTA-4, C9, X11; FI-13 (a)), the first edge end to end: tlsedge's
// engine, with the accounting TLS-M5-10 gave it, serves visitors over loopback TCP on this package's Quota over the
// private stack's Postgres and Redis. A network's connections share one Lease(network, QuotaMeterTls) in the
// process, each chunk is spent before it moves, and the engine's usage batcher sends its 60 s window under producer
// tlsedge; ApplyUsageBatch journals the lease's carve in service_debit_journal, and the debit flush takes it from
// the balance and adds it to network_service_usage under meter tls. FI-13 (a)'s oracle reads the balance and the
// meter's count, journal rows still to apply included, after each step: every byte the edge moved debits once, at
// the balance and at the meter. A Service contract (C9) between the edge's client and the listener, carrying the
// same bytes, escrows and settles nothing, so the lease is the one payer (D-QUOTA-4).
//
// §20.6's empty balance on the same path: a lease that runs dry resets every open stream of its network with
// RST_QUOTA and marks the network spent; a network with no balance is refused its lease, its stream reset with
// RST_QUOTA and the network marked spent; and sockets' over-limit poll, whose answer Find's step 4 reads before it
// answers quota, holds both marks at its next read, within its 5 s interval. Find's own answer for a spent network
// is sockets' T2 (TestSocketFindAnswersQuotaForASpentNetwork).
//
// What is not server's is a fake or scripted: the engine's label map, kill rows and connection records
// (FakeTlsEdgeStore), its {tes_} Redis, gates and metrics, and the dialer, which opens a stream the test plays as
// the listener, standing for Find and the listener's accept. Time is the T3 target's fake clock, which the Quota,
// the engine and the gate share; a real timer only fails a hung test.

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"
	"github.com/urnetwork/sidecar/socketsservice"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The root the tests' labels sit under.
const tlsMeterRoot = "edge.example"

// The longest a test waits on another goroutine before it fails as hung.
const tlsMeterGuardTimeout = 30 * time.Second

// One direction of a stream between the engine and the listener a test plays. Bytes written wait until they are
// read. An end is a clean close, read as EOF once drained, or a failure every later call returns.
type tlsMeterStreamHalf struct {
	stateLock sync.Mutex
	changed   *sync.Cond
	buffer    bytes.Buffer
	closed    bool
	err       error
}

func newTlsMeterStreamHalf() *tlsMeterStreamHalf {
	half := &tlsMeterStreamHalf{}
	half.changed = sync.NewCond(&half.stateLock)
	return half
}

func (self *tlsMeterStreamHalf) Write(b []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		return 0, self.err
	}
	if self.closed {
		return 0, net.ErrClosed
	}
	self.buffer.Write(b)
	self.changed.Broadcast()
	return len(b), nil
}

func (self *tlsMeterStreamHalf) Read(b []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for {
		if self.err != nil {
			return 0, self.err
		}
		if 0 < self.buffer.Len() {
			return self.buffer.Read(b)
		}
		if self.closed {
			return 0, io.EOF
		}
		self.changed.Wait()
	}
}

func (self *tlsMeterStreamHalf) closeWrite() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	self.changed.Broadcast()
}

func (self *tlsMeterStreamHalf) fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err == nil {
		self.err = err
	}
	self.changed.Broadcast()
}

// A stream from the engine to a listener the test plays: the engine holds it as a TlsEdgeStream, and the test
// reads what the engine sent, writes back, half-closes, and sees how the engine ended it.
type tlsMeterStream struct {
	toListener   *tlsMeterStreamHalf
	fromListener *tlsMeterStreamHalf
	// bytes the engine's Writes moved to the listener: what it spent past the hello, which the dialer sent
	engineByteCount atomic.Int64
	// the code of the engine's Reset, sent once
	resetCodes   chan protocol.StreamResetCode
	resetOnce    sync.Once
	engineClosed chan struct{}
	closeOnce    sync.Once
}

func newTlsMeterStream() *tlsMeterStream {
	return &tlsMeterStream{
		toListener:   newTlsMeterStreamHalf(),
		fromListener: newTlsMeterStreamHalf(),
		resetCodes:   make(chan protocol.StreamResetCode, 1),
		engineClosed: make(chan struct{}),
	}
}

var _ tlsedgeservice.TlsEdgeStream = (*tlsMeterStream)(nil)

// What a read or write of a reset stream answers.
var errTlsMeterStreamReset = &sidecar.SocketCodeError{Err: sidecar.ErrSocketReset}

// The engine's side.

func (self *tlsMeterStream) Read(b []byte) (int, error) {
	return self.fromListener.Read(b)
}

func (self *tlsMeterStream) Write(b []byte) (int, error) {
	n, err := self.toListener.Write(b)
	self.engineByteCount.Add(int64(n))
	return n, err
}

func (self *tlsMeterStream) CloseWrite() error {
	self.toListener.closeWrite()
	return nil
}

func (self *tlsMeterStream) Close() error {
	self.closeOnce.Do(func() {
		self.toListener.closeWrite()
		self.fromListener.fail(net.ErrClosed)
		close(self.engineClosed)
	})
	return nil
}

func (self *tlsMeterStream) Reset(code protocol.StreamResetCode) error {
	self.resetOnce.Do(func() {
		self.toListener.fail(errTlsMeterStreamReset)
		self.fromListener.fail(errTlsMeterStreamReset)
		self.resetCodes <- code
	})
	return nil
}

func (self *tlsMeterStream) SetReadDeadline(t time.Time) error {
	return nil
}

func (self *tlsMeterStream) SetWriteDeadline(t time.Time) error {
	return nil
}

// The listener's side.

// Reads exactly byteCount bytes of what the engine sent.
func (self *tlsMeterStream) listenerRead(t *testing.T, byteCount int) []byte {
	t.Helper()
	read := make([]byte, byteCount)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(self.toListener, read)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the listener read: %v", err)
		}
	case <-time.After(tlsMeterGuardTimeout):
		t.Fatal("the listener's bytes never came")
	}
	return read
}

// Waits for the engine's half-close and answers what came before it.
func (self *tlsMeterStream) listenerReadAll(t *testing.T) []byte {
	t.Helper()
	type result struct {
		read []byte
		err  error
	}
	done := make(chan *result, 1)
	go func() {
		read, err := io.ReadAll(self.toListener)
		done <- &result{read: read, err: err}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("the listener read: %v", result.err)
		}
		return result.read
	case <-time.After(tlsMeterGuardTimeout):
		t.Fatal("the engine never half-closed the stream")
	}
	return nil
}

func (self *tlsMeterStream) listenerWrite(t *testing.T, b []byte) {
	t.Helper()
	if _, err := self.fromListener.Write(b); err != nil {
		t.Fatalf("the listener wrote: %v", err)
	}
}

func (self *tlsMeterStream) listenerCloseWrite() {
	self.fromListener.closeWrite()
}

// Waits for the engine's Reset and answers its code.
func (self *tlsMeterStream) awaitReset(t *testing.T) protocol.StreamResetCode {
	t.Helper()
	select {
	case code := <-self.resetCodes:
		return code
	case <-time.After(tlsMeterGuardTimeout):
		t.Fatal("the engine never reset the stream")
	}
	return protocol.StreamResetCode_RST_UNSPECIFIED
}

func (self *tlsMeterStream) awaitEngineClose(t *testing.T) {
	t.Helper()
	select {
	case <-self.engineClosed:
	case <-time.After(tlsMeterGuardTimeout):
		t.Fatal("the engine never closed the stream")
	}
}

// Find and the listener's accept, scripted: each Dial opens a stream, asks for the open's metadata once, as one
// ticket does, and sends the hello into it as SocketEdgeDialer sends early data.
type tlsMeterDialer struct {
	clientIds []sidecar.Id
	opened    chan *tlsMeterStream
}

func newTlsMeterDialer(clientCount int) *tlsMeterDialer {
	dialer := &tlsMeterDialer{opened: make(chan *tlsMeterStream, 64)}
	for range clientCount {
		dialer.clientIds = append(dialer.clientIds, sidecar.NewId())
	}
	return dialer
}

var _ tlsedgeservice.TlsEdgeDialer = (*tlsMeterDialer)(nil)

func (self *tlsMeterDialer) ClientIds() []sidecar.Id {
	return slices.Clone(self.clientIds)
}

func (self *tlsMeterDialer) Dial(ctx context.Context, request *tlsedgeservice.TlsEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error) {
	if _, err := request.Metadata(0); err != nil {
		return nil, err
	}
	stream := newTlsMeterStream()
	stream.toListener.Write(request.HelloBytes)
	self.opened <- stream
	return stream, nil
}

func (self *tlsMeterDialer) awaitOpened(t *testing.T) *tlsMeterStream {
	t.Helper()
	select {
	case stream := <-self.opened:
		return stream
	case <-time.After(tlsMeterGuardTimeout):
		t.Fatal("no stream was opened")
	}
	return nil
}

// One batch the engine sent, as api answered it.
type tlsMeterReport struct {
	batch *sidecar.UsageBatch
	err   error
}

// One lease the engine took.
type tlsMeterLeaseRecord struct {
	networkId connect.Id
	meter     string
	closed    bool
}

// The Quota tlsedge holds: the T3 target's, keeping each lease the engine took and closed and each batch api
// answered, so a test waits on them rather than on time. Safe for concurrent use.
type tlsMeterQuota struct {
	sidecar.Quota

	stateLock sync.Mutex
	leases    []*tlsMeterLeaseRecord
	reports   []*tlsMeterReport
	// rises with each lease taken or closed and each batch answered
	changeCount *sidecar.MonitorValue[uint64]
}

func newTlsMeterQuota(quota sidecar.Quota) *tlsMeterQuota {
	return &tlsMeterQuota{
		Quota:       quota,
		changeCount: sidecar.NewMonitorValue(uint64(0)),
	}
}

func (self *tlsMeterQuota) changed() {
	self.changeCount.Update(func(count uint64) uint64 {
		return count + 1
	})
}

func (self *tlsMeterQuota) Lease(ctx context.Context, networkId connect.Id, meter string) (sidecar.QuotaLease, *sidecar.QuotaRefusal, error) {
	lease, refusal, err := self.Quota.Lease(ctx, networkId, meter)
	if lease == nil {
		return nil, refusal, err
	}
	record := &tlsMeterLeaseRecord{networkId: networkId, meter: meter}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.leases = append(self.leases, record)
	}()
	self.changed()
	return &tlsMeterLease{QuotaLease: lease, quota: self, record: record}, nil, nil
}

func (self *tlsMeterQuota) ReportUsage(ctx context.Context, batch *sidecar.UsageBatch) error {
	err := self.Quota.ReportUsage(ctx, batch)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.reports = append(self.reports, &tlsMeterReport{batch: batch, err: err})
	}()
	self.changed()
	return err
}

// Waits until done answers true, asked under the Quota's stateLock after each change.
func (self *tlsMeterQuota) await(t *testing.T, what string, done func() bool) {
	t.Helper()
	guard := time.After(tlsMeterGuardTimeout)
	for {
		_, changed := self.changeCount.Get()
		if func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return done()
		}() {
			return
		}
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("never: %s", what)
		}
	}
}

// Waits for the count-th batch api answered, and answers it.
func (self *tlsMeterQuota) awaitReport(t *testing.T, count int) *tlsMeterReport {
	t.Helper()
	var report *tlsMeterReport
	self.await(t, "a batch answered", func() bool {
		if len(self.reports) < count {
			return false
		}
		report = self.reports[count-1]
		return true
	})
	return report
}

// Waits until the engine has closed every lease it took of the network.
func (self *tlsMeterQuota) awaitLeasesClosed(t *testing.T, networkId connect.Id) {
	t.Helper()
	self.await(t, "the network's leases closed", func() bool {
		taken := false
		for _, record := range self.leases {
			if record.networkId != networkId {
				continue
			}
			taken = true
			if !record.closed {
				return false
			}
		}
		return taken
	})
}

// The leases the engine took of the network, by meter.
func (self *tlsMeterQuota) leaseMeters(networkId connect.Id) []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	meters := []string{}
	for _, record := range self.leases {
		if record.networkId == networkId {
			meters = append(meters, record.meter)
		}
	}
	return meters
}

// A lease the engine holds, telling the Quota when its last holder closed it.
type tlsMeterLease struct {
	sidecar.QuotaLease
	quota  *tlsMeterQuota
	record *tlsMeterLeaseRecord
}

func (self *tlsMeterLease) Close() {
	self.QuotaLease.Close()
	func() {
		self.quota.stateLock.Lock()
		defer self.quota.stateLock.Unlock()
		self.record.closed = true
	}()
	self.quota.changed()
}

// t3OracleSource's reading with meter tls's count: the network's network_service_usage rows on the meter and the
// journal's rows on its balances still to apply, so the count moves with the debit, in the flush's one
// transaction.
type tlsMeterOracleSource struct {
	t3OracleSource
}

func (self *tlsMeterOracleSource) ReadQuota(ctx context.Context, networkId connect.Id) (*sidecartest.QuotaOracleReading, error) {
	reading, err := self.t3OracleSource.ReadQuota(ctx, networkId)
	if err != nil {
		return nil, err
	}
	var usedByteCount int64
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			self.ctx,
			`
				SELECT
					(
						SELECT COALESCE(SUM(quantity), 0)
						FROM network_service_usage
						WHERE network_id = $1 AND meter = $2
					) + (
						SELECT COALESCE(SUM(service_debit_journal.debit_byte_count), 0)
						FROM service_debit_journal
						INNER JOIN transfer_balance ON transfer_balance.balance_id = service_debit_journal.balance_id
						WHERE transfer_balance.network_id = $1 AND service_debit_journal.meter = $2 AND NOT service_debit_journal.applied
					)
			`,
			server.Id(networkId),
			sidecar.QuotaMeterTls,
		).Scan(&usedByteCount))
	})
	reading.MeterUsedByteCounts = map[string]int64{sidecar.QuotaMeterTls: usedByteCount}
	return reading, nil
}

// A loopback listener whose address reads as port 443, since a test cannot bind it.
type tlsMeterListener struct {
	net.Listener
}

func (self *tlsMeterListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}
}

// One network a world routes: a label, the gate open to it, and a balance of balanceByteCount, none when zero.
type tlsMeterNetwork struct {
	networkId        connect.Id
	balanceByteCount int64
}

// The world of one test: the T3 target, tlsedge's engine on it serving loopback, and the networks it routes.
type tlsMeterWorld struct {
	ctx      context.Context
	target   *quotasuite.Target
	quota    *tlsMeterQuota
	host     *sidecartest.FakeHost
	store    *sidecartest.FakeTlsEdgeStore
	dialer   *tlsMeterDialer
	settings *tlsedgeservice.TlsEdgeSettings
	engine   *tlsedgeservice.TlsEdgeService
	oracle   *sidecartest.QuotaOracle
	// when the engine's usage batcher opened its first window
	start   time.Time
	address string
	labels  map[connect.Id]string
}

// Community's caps as each network's own, with the tier the lease's draw size reads.
func tlsMeterLimits() *sidecar.QuotaLimits {
	return &sidecar.QuotaLimits{Tier: "pro", Limits: map[string]int64{
		"tls_endpoint_concurrent":        256,
		"tls_endpoint_new_per_s":         50,
		"tls_endpoint_mbit_per_s":        20,
		"tls_visitor_concurrent":         64,
		"tls_visitor_new_per_s":          20,
		"tls_visitor_aggregate_multiple": 4,
	}}
}

// A ready engine on a fresh T3 target serving port 443 on loopback, with each network's label, balance and
// gate set before it reads the label map.
func newTlsMeterWorld(t *testing.T, ctx context.Context, networks ...*tlsMeterNetwork) *tlsMeterWorld {
	t.Helper()
	target := newT3Target(ctx)(t)
	host := sidecartest.NewFakeHost(ctx, target.Clock, nil, nil, nil)
	world := &tlsMeterWorld{
		ctx:    ctx,
		target: target,
		quota:  newTlsMeterQuota(target.Quota),
		host:   host,
		store:  sidecartest.NewFakeTlsEdgeStore(nil),
		oracle: sidecartest.NewQuotaOracle(&tlsMeterOracleSource{t3OracleSource: t3OracleSource{ctx: ctx}}),
		labels: map[connect.Id]string{},
	}
	networkIds := []connect.Id{}
	for _, network := range networks {
		if err := target.SetLimits(network.networkId, tlsMeterLimits()); err != nil {
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
		label, err := world.store.AddLabel(tlsMeterRoot, sidecar.Id(network.networkId))
		if err != nil {
			t.Fatal(err)
		}
		world.labels[network.networkId] = label
		networkIds = append(networkIds, network.networkId)
	}
	host.Gates.OpenService(sidecar.ServiceGateTlsedge, networkIds...)

	serviceHost := host.ServiceHost()
	serviceHost.Quota = world.quota
	world.settings = tlsedgeservice.DefaultTlsEdgeSettings()
	world.settings.Root = tlsMeterRoot
	world.settings.Blocks = 1
	world.dialer = newTlsMeterDialer(world.settings.Clients)
	world.start = target.Clock.Now()
	engine, err := tlsedgeservice.NewTlsEdgeService(ctx, serviceHost, world.store, world.dialer, world.settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	world.engine = engine
	tlsMeterAwaitCount(t, "the label map's first read", 1, engine.NameLoads)
	tlsMeterAwaitCount(t, "the kill rows' first read", 1, engine.KillLoads)
	if !engine.Ready() {
		t.Fatal("the engine is not ready")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	world.address = listener.Addr().String()
	go engine.Serve(&tlsMeterListener{Listener: listener})
	return world
}

// Waits until counts answers at least target.
func tlsMeterAwaitCount(t *testing.T, what string, target uint64, counts func() (uint64, chan struct{})) {
	t.Helper()
	guard := time.After(tlsMeterGuardTimeout)
	for {
		count, changed := counts()
		if target <= count {
			return
		}
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("%s: at %d, want %d", what, count, target)
		}
	}
}

// A visitor's connection to the engine's listener.
func (self *tlsMeterWorld) dial(t *testing.T) *net.TCPConn {
	t.Helper()
	conn, err := net.Dial("tcp", self.address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(tlsMeterGuardTimeout))
	return conn.(*net.TCPConn)
}

// The name of host in the network's label.
func (self *tlsMeterWorld) name(networkId connect.Id, host string) string {
	return host + "." + self.labels[networkId] + "." + tlsMeterRoot
}

// A visitor's connection to host of the network, through to the listener's stream, which has read the hello.
func (self *tlsMeterWorld) open(t *testing.T, networkId connect.Id, host string) (*net.TCPConn, *tlsMeterStream, []byte) {
	t.Helper()
	hello := tlsMeterHello(self.name(networkId, host))
	conn := self.dial(t)
	if _, err := conn.Write(hello); err != nil {
		t.Fatal(err)
	}
	stream := self.dialer.awaitOpened(t)
	if read := stream.listenerRead(t, len(hello)); !bytes.Equal(read, hello) {
		t.Fatal("the listener's hello is not the visitor's")
	}
	return conn, stream, hello
}

// Moves the clock to the end of the engine's first usage window and answers the batch api took for it.
func (self *tlsMeterWorld) sendFirstWindow(t *testing.T) *tlsMeterReport {
	t.Helper()
	self.target.Clock.AdvanceTo(self.start.Add(self.settings.Usage.WindowTimeout))
	report := self.quota.awaitReport(t, 1)
	if report.err != nil {
		t.Fatalf("api refused the window's batch: %v", report.err)
	}
	if report.batch.Producer != sidecar.QuotaProducerTlsEdge {
		t.Fatalf("a batch of producer %q", report.batch.Producer)
	}
	return report
}

// The batch's tls bytes for the network, and whether it carried any other event for it.
func tlsMeterBatchByteCount(batch *sidecar.UsageBatch, networkId connect.Id) (byteCount int64, other bool) {
	for _, event := range batch.Events {
		if event.NetworkId != networkId {
			continue
		}
		if event.Meter != sidecar.QuotaMeterTls || event.StoredByteDelta != 0 {
			other = true
			continue
		}
		byteCount += event.TransferByteCount
	}
	return
}

func (self *tlsMeterWorld) check(t *testing.T) {
	t.Helper()
	if err := self.oracle.Check(self.ctx); err != nil {
		t.Fatal(err)
	}
}

func (self *tlsMeterWorld) checkSettled(t *testing.T) {
	t.Helper()
	if err := self.target.Settle(); err != nil {
		t.Fatal(err)
	}
	if err := self.oracle.CheckSettled(self.ctx); err != nil {
		t.Fatal(err)
	}
}

// The journal's rows on the network's balances: their tls bytes still to apply, the batches they came in, and
// how many rows of any kind there are.
func tlsMeterJournal(ctx context.Context, networkId connect.Id) (pendingByteCount int64, batchIds []server.Id, rowCount int) {
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
				var meter string
				var debitByteCount int64
				var batchId *server.Id
				var applied bool
				server.Raise(result.Scan(&meter, &debitByteCount, &batchId, &applied))
				rowCount += 1
				if meter == sidecar.QuotaMeterTls && !applied {
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

// The producer service_usage_batch holds for the batch, "" for none.
func tlsMeterBatchProducer(ctx context.Context, batchId connect.Id) (producer string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT producer FROM service_usage_batch WHERE batch_id = $1`, server.Id(batchId))
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&producer))
			}
		})
	})
	return
}

// The network's network_service_usage rows under meter tls, by UTC day.
func tlsMeterUsageDays(ctx context.Context, networkId connect.Id) map[string]int64 {
	dayQuantities := map[string]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT usage_day, quantity FROM network_service_usage WHERE network_id = $1 AND meter = $2`,
			server.Id(networkId),
			sidecar.QuotaMeterTls,
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

// The network's spent mark in network_quota_state, nil when it is not spent.
func tlsMeterSpentTime(t *testing.T, ctx context.Context, networkId connect.Id) *time.Time {
	t.Helper()
	state, err := model.GetNetworkQuotaState(ctx, server.Id(networkId))
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		return nil
	}
	return state.SpentTime
}

// A big-endian length of byteCount bytes in front of data.
func tlsMeterPrefixed(byteCount int, data []byte) []byte {
	prefix := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	return append(prefix[len(prefix)-byteCount:], data...)
}

// A ClientHello for serverName offering h2 and http/1.1, in one record.
func tlsMeterHello(serverName string) []byte {
	extensions := []byte{}
	extension := func(extensionType uint16, data []byte) {
		extensions = binary.BigEndian.AppendUint16(extensions, extensionType)
		extensions = append(extensions, tlsMeterPrefixed(2, data)...)
	}
	entry := append([]byte{0}, tlsMeterPrefixed(2, []byte(serverName))...)
	extension(0, tlsMeterPrefixed(2, entry))
	alpnEntries := []byte{}
	for _, alpnId := range []string{"h2", "http/1.1"} {
		alpnEntries = append(alpnEntries, tlsMeterPrefixed(1, []byte(alpnId))...)
	}
	extension(16, tlsMeterPrefixed(2, alpnEntries))
	body := []byte{3, 3}
	body = append(body, bytes.Repeat([]byte{0x11}, 32)...)
	body = append(body, 0)
	body = append(body, tlsMeterPrefixed(2, []byte{0x13, 0x01})...)
	body = append(body, 1, 0)
	body = append(body, tlsMeterPrefixed(2, extensions)...)
	message := append([]byte{1}, tlsMeterPrefixed(3, body)...)
	return append([]byte{22, 3, 1}, tlsMeterPrefixed(2, message)...)
}

// The one alert a refused visitor reads.
func tlsMeterAlert(code tlsedgeservice.TlsAlertCode) []byte {
	return []byte{21, 3, 3, 0, 2, 2, byte(code)}
}

// Reads exactly byteCount bytes as the visitor.
func tlsMeterVisitorRead(t *testing.T, conn net.Conn, byteCount int) []byte {
	t.Helper()
	read := make([]byte, byteCount)
	if _, err := io.ReadFull(conn, read); err != nil {
		t.Fatalf("the visitor read: %v", err)
	}
	return read
}

// The money path: a connection's bytes both ways, the hello included, spent on the network's one lease, sent at
// the window's end under producer tlsedge, journaled as the lease's carve, and debited once by the flush, which
// counts them in network_service_usage under meter tls; the Service contract the stream rides escrows nothing
// and its settlement takes nothing.
func TestTlsMeterT3MoneyPath(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		// the process's limit snapshot is dropped at the end, so the next test reads limits as a fresh process does
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		ctx := t.Context()
		balanceByteCount := int64(1024 * 1024)
		payer := &tlsMeterNetwork{networkId: connect.NewId(), balanceByteCount: balanceByteCount}

		// the listening network and urnetwork-edge's, each with a client, for the Service contract
		payerNetworkId := server.Id(payer.networkId)
		edgeNetworkId := server.NewId()
		listenerClientId := server.NewId()
		edgeClientId := server.NewId()
		model.Testing_CreateNetwork(ctx, payerNetworkId, "tls-meter-"+payerNetworkId.String(), server.NewId())
		model.Testing_CreateDevice(ctx, payerNetworkId, server.NewId(), listenerClientId, "synthetic", "synthetic")
		model.Testing_CreateNetwork(ctx, edgeNetworkId, "tls-meter-edge-"+edgeNetworkId.String(), server.NewId())
		model.Testing_CreateDevice(ctx, edgeNetworkId, server.NewId(), edgeClientId, "synthetic", "synthetic")

		world := newTlsMeterWorld(t, ctx, payer)
		conn, stream, hello := world.open(t, payer.networkId, "blog")

		// the edge's stream to the listener rides a Service contract, as CreateContract issues it for the pair
		// Find recorded (§6.10.1): no escrow on either network
		contractId, _, err := model.CreateContractNoEscrowWithExpiration(ctx, edgeNetworkId, edgeClientId, payerNetworkId, listenerClientId, model.ByteCount(1024*1024), false)
		if err != nil {
			t.Fatal(err)
		}
		for _, networkId := range []server.Id{payerNetworkId, edgeNetworkId} {
			if open := model.GetOpenTransferByteCount(ctx, networkId); open != 0 {
				t.Fatalf("a Service contract escrowed %d", open)
			}
		}

		toListener := bytes.Repeat([]byte{0x5a}, 4000)
		if _, err := conn.Write(toListener); err != nil {
			t.Fatal(err)
		}
		if read := stream.listenerRead(t, len(toListener)); !bytes.Equal(read, toListener) {
			t.Fatal("the listener's bytes are not the visitor's")
		}
		toVisitor := bytes.Repeat([]byte{0xa5}, 3000)
		stream.listenerWrite(t, toVisitor)
		if read := tlsMeterVisitorRead(t, conn, len(toVisitor)); !bytes.Equal(read, toVisitor) {
			t.Fatal("the visitor's bytes are not the listener's")
		}
		// both halves close: the connection ends, and with it the network's lease
		conn.CloseWrite()
		if rest := stream.listenerReadAll(t); len(rest) != 0 {
			t.Fatalf("%d bytes after the visitor's FIN", len(rest))
		}
		stream.listenerCloseWrite()
		if rest, err := io.ReadAll(conn); err != nil || len(rest) != 0 {
			t.Fatalf("the visitor's end: %d bytes, %v", len(rest), err)
		}
		stream.awaitEngineClose(t)
		world.quota.awaitLeasesClosed(t, payer.networkId)
		if meters := world.quota.leaseMeters(payer.networkId); !slices.Equal(meters, []string{sidecar.QuotaMeterTls}) {
			t.Fatalf("the connection's leases: %v", meters)
		}
		meteredByteCount := int64(len(hello) + len(toListener) + len(toVisitor))
		world.oracle.Meter(payer.networkId, sidecar.QuotaMeterTls, meteredByteCount)
		world.check(t)

		// the contract settles from both parties' reports of those bytes: nothing moves
		for _, clientId := range []server.Id{edgeClientId, listenerClientId} {
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
		if byteCount, other := tlsMeterBatchByteCount(report.batch, payer.networkId); byteCount != meteredByteCount || other {
			t.Fatalf("the batch carried %d tls bytes (other events %t), the connection moved %d", byteCount, other, meteredByteCount)
		}
		if producer := tlsMeterBatchProducer(ctx, report.batch.BatchId); producer != sidecar.QuotaProducerTlsEdge {
			t.Fatalf("service_usage_batch holds the batch as %q", producer)
		}
		pendingByteCount, batchIds, _ := tlsMeterJournal(ctx, payer.networkId)
		if pendingByteCount != meteredByteCount {
			t.Fatalf("%d tls bytes journaled, the connection moved %d", pendingByteCount, meteredByteCount)
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
		if days := tlsMeterUsageDays(ctx, payer.networkId); len(days) != 1 || days[day] != meteredByteCount {
			t.Fatalf("network_service_usage holds %v under tls, want %d on %s", days, meteredByteCount, day)
		}
		if balances := t3ReadBalances(ctx, payerNetworkId); len(balances) != 1 || balances[0].ByteCount != balanceByteCount-meteredByteCount || balances[0].ReservedByteCount != 0 {
			t.Fatalf("after the flush: %+v, want %d left and nothing reserved", balances, balanceByteCount-meteredByteCount)
		}
		if _, _, rowCount := tlsMeterJournal(ctx, payer.networkId); rowCount != 0 {
			t.Fatalf("%d journal rows left after the flush", rowCount)
		}
		if open := model.GetOpenTransferByteCount(ctx, payerNetworkId); open != 0 {
			t.Fatalf("%d open after the flush", open)
		}
	})
}

// §20.6's empty balance: a lease that runs dry resets every open stream of its network with RST_QUOTA and marks
// it spent; a network with no balance is refused its lease and marked spent; sockets' gate holds both marks at
// its next over-limit read, within 5 s; and what moved before the lease ran out debits once, never past the
// balance.
func TestTlsMeterT3EmptyBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		// the process's limit snapshot is dropped at the end, so the next test reads limits as a fresh process does
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		ctx := t.Context()
		dryByteCount := int64(4 * 1024)
		dry := &tlsMeterNetwork{networkId: connect.NewId(), balanceByteCount: dryByteCount}
		empty := &tlsMeterNetwork{networkId: connect.NewId()}
		world := newTlsMeterWorld(t, ctx, dry, empty)

		// sockets' gate on the same Quota and clock, its first read taken now
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
		if gate.Spent(dry.networkId) || gate.Spent(empty.networkId) {
			t.Fatal("a network read as spent before anything ran dry")
		}

		// two connections hold the network's one lease, a byte back on each so both splice before it runs out
		conn, stream, hello := world.open(t, dry.networkId, "blog")
		idleConn, idleStream, idleHello := world.open(t, dry.networkId, "shop")
		for _, pair := range []struct {
			conn   net.Conn
			stream *tlsMeterStream
		}{{conn: conn, stream: stream}, {conn: idleConn, stream: idleStream}} {
			pair.stream.listenerWrite(t, []byte{1})
			tlsMeterVisitorRead(t, pair.conn, 1)
		}
		// more than the balance holds: the lease runs dry mid-write, and both streams reset
		if _, err := conn.Write(bytes.Repeat([]byte{0x5a}, 2*int(dryByteCount))); err != nil {
			t.Fatal(err)
		}
		if code := stream.awaitReset(t); code != protocol.StreamResetCode_RST_QUOTA {
			t.Fatalf("a lease run dry reset its stream with %v", code)
		}
		if code := idleStream.awaitReset(t); code != protocol.StreamResetCode_RST_QUOTA {
			t.Fatalf("a lease run dry reset the idle stream with %v", code)
		}
		world.quota.awaitLeasesClosed(t, dry.networkId)
		if meters := world.quota.leaseMeters(dry.networkId); !slices.Equal(meters, []string{sidecar.QuotaMeterTls}) {
			t.Fatalf("the two connections' leases: %v", meters)
		}
		if tlsMeterSpentTime(t, ctx, dry.networkId) == nil {
			t.Fatal("a lease run dry left its network unspent")
		}
		// the hellos and the two bytes back, and what reached the listener before the lease ran out
		meteredByteCount := int64(len(hello)+len(idleHello)+2) + stream.engineByteCount.Load() + idleStream.engineByteCount.Load()
		if dryByteCount < meteredByteCount {
			t.Fatalf("%d bytes moved on a balance of %d", meteredByteCount, dryByteCount)
		}
		world.oracle.Meter(dry.networkId, sidecar.QuotaMeterTls, meteredByteCount)
		world.check(t)

		// a network with no balance: its lease is refused, its stream reset with RST_QUOTA, the visitor answered
		emptyConn := world.dial(t)
		if _, err := emptyConn.Write(tlsMeterHello(world.name(empty.networkId, "blog"))); err != nil {
			t.Fatal(err)
		}
		emptyStream := world.dialer.awaitOpened(t)
		if code := emptyStream.awaitReset(t); code != protocol.StreamResetCode_RST_QUOTA {
			t.Fatalf("a refused lease reset its stream with %v", code)
		}
		if answer, err := io.ReadAll(emptyConn); err != nil || !bytes.Equal(answer, tlsMeterAlert(tlsedgeservice.TlsAlertInternalError)) {
			t.Fatalf("a network with no balance answered %x, %v", answer, err)
		}
		if meters := world.quota.leaseMeters(empty.networkId); len(meters) != 0 {
			t.Fatalf("a network with no balance holds leases: %v", meters)
		}
		if tlsMeterSpentTime(t, ctx, empty.networkId) == nil {
			t.Fatal("a refused lease left its network unspent")
		}

		// Find's step 4 reads the gate: its first read stands until the next, which holds both marks
		if gate.Spent(dry.networkId) || gate.Spent(empty.networkId) {
			t.Fatal("the gate changed before its next read")
		}
		readCount, _ := gateReads()
		world.target.Clock.AdvanceTo(world.start.Add(socketsservice.DefaultSocketGateSettings().OverLimitInterval))
		tlsMeterAwaitCount(t, "the gate's next over-limit read", readCount+1, gateReads)
		if !gate.Spent(dry.networkId) || !gate.Spent(empty.networkId) {
			t.Fatalf("5 s after the marks the gate reads dry %t, empty %t", gate.Spent(dry.networkId), gate.Spent(empty.networkId))
		}

		// what moved debits once at the window, never past the balance
		report := world.sendFirstWindow(t)
		if byteCount, other := tlsMeterBatchByteCount(report.batch, dry.networkId); byteCount != meteredByteCount || other {
			t.Fatalf("the batch carried %d tls bytes (other events %t), %d moved", byteCount, other, meteredByteCount)
		}
		if byteCount, other := tlsMeterBatchByteCount(report.batch, empty.networkId); byteCount != 0 || other {
			t.Fatalf("the batch carried %d bytes for the network with no balance", byteCount)
		}
		world.check(t)
		world.checkSettled(t)
		if balances := t3ReadBalances(ctx, server.Id(dry.networkId)); len(balances) != 1 || balances[0].ByteCount != dryByteCount-meteredByteCount || balances[0].ReservedByteCount != 0 {
			t.Fatalf("after the flush: %+v, want %d left and nothing reserved", balances, dryByteCount-meteredByteCount)
		}
		if days := tlsMeterUsageDays(ctx, empty.networkId); len(days) != 0 {
			t.Fatalf("the network with no balance counts tls usage: %v", days)
		}
	})
}
