//go:build sidecar_pending

package main

// T3 for mxedge's router on server's host (MAIL-M7-27; §22.5's §15 row and its hot-path gate, §22.7's S7, I-12, I-13,
// D-MAIL-1, D-MAIL-2, §2.8's mx-1 and mx-2): mxedgeservice's router role on a sidecarhost ServiceHost over the private
// stack's Redis and Postgres (.stack/server-test.sh, the test's own databases), with model's MxEdgeStore writing
// mail_connection_record and mxedgehost's engine deps reading each ClientHello through tlsedgeservice, as
// mxedgehost.NewRouterHost composes the block. The Find and open alone are a test's: the block's dial is
// SocketEdgeDialer over its platform clients, whose stream only the sockets layer makes and whose T3 is SOCK's, so
// here a receiver ends TLS itself on a certificate for the SNI and echoes. On it:
//
//   - budgets.yml's mxedge.route (§22.5): one session from the banner through the pre-TLS dialog, a real ClientHello,
//     the route, the lease and the open to a TLS handshake the receiver ends and bytes both ways through the splice,
//     under sidecart3.InlinePath: no PostgreSQL acquisition, and one Redis call on the stack's Redis, the route's read;
//   - S7's router clauses: plaintext MAIL with no closed handshake behind it gets 530 linking the published deviation;
//     a name no route holds closes in the handshake with unrecognized_name, writing the sender's /48 to the fleet's
//     closed-handshake memory first, so a plaintext retry from a sibling address in that /48 gets 451 4.7.0; and the
//     edge holds nothing of either session: no stored byte, key or row names the sender, its EHLO name or the name it
//     asked for, and nothing waits for a receiver;
//   - mx-2: the session that named a network leaves one mail_connection_record row once the engine's last batch is
//     written, holding the network, its start, its bytes and its result alone; the session that named none leaves
//     none. A canary the sender wrote inside TLS reaches no store, log or metric.
//
// Nothing sleeps: the engine's first probe and its STARTTLS quiet time are awaited by their timers on a fake clock,
// which starts at the wall clock's hour so the records land in a partition the store keeps.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/mxedgehost"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/sidecart3"
)

// The names and documentation addresses a T3 run uses, synthetic (connect/CODESTYLE.md's test data rule).
const (
	t3RouterRoot           = "root.example"
	t3RouterLabel          = "t3label0001"
	t3RouterSni            = "mx." + t3RouterLabel + "." + t3RouterRoot
	t3RouterUnknownSni     = "mx.nosuchlabel." + t3RouterRoot
	t3RouterBannerHostName = "mx.banner.example"
	t3RouterDeviationUrl   = "https://deviation.example/rfc3207"
	t3RouterEhloName       = "relay.t3sender.example"
	// RFC 3849's documentation prefix: the sender, a sibling in its /48, and a sender of its own /48.
	t3RouterSender        = "2001:db8:5f1::25"
	t3RouterSenderSibling = "2001:db8:5f1:7::26"
	t3RouterOtherSender   = "2001:db8:6a2::27"
	// The lb's address a session's PROXY line would name, which the engine does not keep.
	t3RouterLbAddress = "192.0.2.25"
	// How long one step may wait before the test fails: a bound on a hang, never a pause.
	t3RouterStepTimeout = 30 * time.Second
	// The engine's probe interval, apart from every other timer it sets, so a test sees the first probe land by its
	// timer alone.
	t3RouterProbeInterval = 7 * time.Second
)

// A Redis whose calls go to the one set last: the stack's own, and inside the inline path InlinePath's counted copy of
// it, so the engine built before the path reaches Redis on the path only through what the path is given.
type t3SwitchRedis struct {
	target atomic.Pointer[sidecar.Redis]
}

var _ sidecar.Redis = (*t3SwitchRedis)(nil)

func (self *t3SwitchRedis) use(redis sidecar.Redis) {
	self.target.Store(&redis)
}

func (self *t3SwitchRedis) redis() sidecar.Redis {
	return *self.target.Load()
}

func (self *t3SwitchRedis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return self.redis().Get(ctx, key)
}

func (self *t3SwitchRedis) GetMany(ctx context.Context, keys []string) ([][]byte, error) {
	return self.redis().GetMany(ctx, keys)
}

func (self *t3SwitchRedis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return self.redis().Set(ctx, key, value, ttl)
}

func (self *t3SwitchRedis) SetIfAbsent(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	return self.redis().SetIfAbsent(ctx, key, value, ttl)
}

func (self *t3SwitchRedis) CompareAndSet(ctx context.Context, key string, expected []byte, value []byte, ttl time.Duration) (bool, error) {
	return self.redis().CompareAndSet(ctx, key, expected, value, ttl)
}

func (self *t3SwitchRedis) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	return self.redis().CompareAndDelete(ctx, key, expected)
}

func (self *t3SwitchRedis) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	return self.redis().Increment(ctx, key, ttl)
}

func (self *t3SwitchRedis) Delete(ctx context.Context, keys []string) error {
	return self.redis().Delete(ctx, keys)
}

func (self *t3SwitchRedis) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return self.redis().Expire(ctx, key, ttl)
}

func (self *t3SwitchRedis) HashGet(ctx context.Context, key string, field string) ([]byte, bool, error) {
	return self.redis().HashGet(ctx, key, field)
}

func (self *t3SwitchRedis) HashGetAll(ctx context.Context, key string) (map[string][]byte, error) {
	return self.redis().HashGetAll(ctx, key)
}

func (self *t3SwitchRedis) HashSet(ctx context.Context, key string, fieldValues map[string][]byte) error {
	return self.redis().HashSet(ctx, key, fieldValues)
}

func (self *t3SwitchRedis) HashDelete(ctx context.Context, key string, fields []string) error {
	return self.redis().HashDelete(ctx, key, fields)
}

func (self *t3SwitchRedis) HashExpire(ctx context.Context, key string, fields []string, ttl time.Duration) error {
	return self.redis().HashExpire(ctx, key, fields, ttl)
}

func (self *t3SwitchRedis) StreamAdd(ctx context.Context, key string, maxLength int64, fieldValues map[string][]byte) (string, error) {
	return self.redis().StreamAdd(ctx, key, maxLength, fieldValues)
}

func (self *t3SwitchRedis) StreamRange(ctx context.Context, key string, afterEntryId string, count int64) ([]*sidecar.RedisStreamEntry, error) {
	return self.redis().StreamRange(ctx, key, afterEntryId, count)
}

func (self *t3SwitchRedis) StreamGroupCreate(ctx context.Context, key string, group string) error {
	return self.redis().StreamGroupCreate(ctx, key, group)
}

func (self *t3SwitchRedis) StreamGroupRead(ctx context.Context, key string, group string, consumer string, count int64, block time.Duration) ([]*sidecar.RedisStreamEntry, error) {
	return self.redis().StreamGroupRead(ctx, key, group, consumer, count, block)
}

func (self *t3SwitchRedis) StreamGroupAck(ctx context.Context, key string, group string, entryIds []string) error {
	return self.redis().StreamGroupAck(ctx, key, group, entryIds)
}

func (self *t3SwitchRedis) StreamGroupClaim(ctx context.Context, key string, group string, consumer string, minIdle time.Duration, cursor string, count int64) ([]*sidecar.RedisStreamEntry, string, error) {
	return self.redis().StreamGroupClaim(ctx, key, group, consumer, minIdle, cursor, count)
}

func (self *t3SwitchRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	return self.redis().RunScript(ctx, script, keys, args)
}

func (self *t3SwitchRedis) AddKeyEventCallback(keyPattern string, callback sidecar.RedisKeyEventFunction) (func(), error) {
	return self.redis().AddKeyEventCallback(keyPattern, callback)
}

// One end of a stream to the receiver, over two io.Pipes.
type t3RouterStream struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

func (self *t3RouterStream) Read(b []byte) (int, error)  { return self.reader.Read(b) }
func (self *t3RouterStream) Write(b []byte) (int, error) { return self.writer.Write(b) }
func (self *t3RouterStream) CloseWrite() error           { return self.writer.Close() }
func (self *t3RouterStream) Close() error {
	self.writer.Close()
	return self.reader.Close()
}
func (self *t3RouterStream) Reset(code protocol.StreamResetCode) error {
	self.writer.CloseWithError(errors.New("reset"))
	return self.reader.CloseWithError(errors.New("reset"))
}
func (self *t3RouterStream) SetReadDeadline(t time.Time) error  { return nil }
func (self *t3RouterStream) SetWriteDeadline(t time.Time) error { return nil }

// The receiver's side of a stream as a net.Conn: the early data first, then what the router splices.
type t3RouterReceiverConn struct {
	net.Conn
	reader io.Reader
	writer *io.PipeWriter
}

func (self *t3RouterReceiverConn) Read(b []byte) (int, error)  { return self.reader.Read(b) }
func (self *t3RouterReceiverConn) Write(b []byte) (int, error) { return self.writer.Write(b) }
func (self *t3RouterReceiverConn) Close() error                { return self.writer.Close() }

// What a receiver took of one stream: the open's MailIngress and the line it read inside TLS.
type t3RouterReceived struct {
	ingress *protocol.MailIngress
	line    string
	err     error
}

// The Find and open: K client ids, and a receiver that ends TLS itself on a certificate for the SNI, reads one line
// and echoes it, as a real one ends TLS on its own key. Each open's MailIngress is held to the format.
type t3RouterDialer struct {
	certificate tls.Certificate
	now         time.Time
	clientIds   []sidecar.Id
	received    chan *t3RouterReceived

	stateLock sync.Mutex
	dials     []*mxedgeservice.MxEdgeDialRequest
}

func (self *t3RouterDialer) ClientIds() []sidecar.Id { return slices.Clone(self.clientIds) }
func (self *t3RouterDialer) Ready() bool             { return true }

func (self *t3RouterDialer) Dial(ctx context.Context, request *mxedgeservice.MxEdgeDialRequest) (mxedgeservice.MxEdgeStream, uint64, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.dials = append(self.dials, request)
	}()
	metadata, err := request.Metadata(0)
	if err != nil {
		return nil, 0, err
	}
	ingress, err := sidecar.ParseMailIngress(metadata, sidecar.DefaultMailIngressFormatSettings().MaxByteCount)
	if err != nil {
		return nil, 0, err
	}
	toReceiverReader, toReceiverWriter := io.Pipe()
	toRouterReader, toRouterWriter := io.Pipe()
	receiverConn := &t3RouterReceiverConn{
		Conn:   &net.TCPConn{},
		reader: io.MultiReader(bytes.NewReader(slices.Clone(request.HelloBytes)), toReceiverReader),
		writer: toRouterWriter,
	}
	go func() {
		server := tls.Server(receiverConn, &tls.Config{Certificates: []tls.Certificate{self.certificate}, Time: func() time.Time { return self.now }})
		defer server.Close()
		if err := server.Handshake(); err != nil {
			self.received <- &t3RouterReceived{ingress: ingress, err: err}
			return
		}
		line, err := bufio.NewReader(server).ReadString('\n')
		if err != nil {
			self.received <- &t3RouterReceived{ingress: ingress, err: err}
			return
		}
		server.Write([]byte("250 echoed " + line))
		self.received <- &t3RouterReceived{ingress: ingress, line: line}
	}()
	return &t3RouterStream{reader: toRouterReader, writer: toReceiverWriter}, 0, nil
}

func (self *t3RouterDialer) dialCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.dials)
}

// A listener a test feeds.
type t3RouterListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func (self *t3RouterListener) Accept() (net.Conn, error) {
	select {
	case conn := <-self.conns:
		return conn, nil
	case <-self.closed:
		return nil, net.ErrClosed
	}
}

func (self *t3RouterListener) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

func (self *t3RouterListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(t3RouterLbAddress), Port: 25}
}

// The server's end of a pipe, reporting the sender the PROXY listener would.
type t3RouterServerConn struct {
	net.Conn
	sender net.Addr
}

func (self *t3RouterServerConn) RemoteAddr() net.Addr { return self.sender }

// A conn whose reads go through the reader that read the dialog, so no byte it buffered is lost.
type t3RouterBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (self *t3RouterBufferedConn) Read(b []byte) (int, error) { return self.reader.Read(b) }

// A log that keeps every line, for the scan.
type t3RouterLog struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *t3RouterLog) keep(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *t3RouterLog) Lines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

func (self *t3RouterLog) Info(args ...any)                 { self.keep(fmt.Sprint(args...)) }
func (self *t3RouterLog) Infof(format string, args ...any) { self.keep(fmt.Sprintf(format, args...)) }
func (self *t3RouterLog) Warningf(format string, args ...any) {
	self.keep(fmt.Sprintf(format, args...))
}
func (self *t3RouterLog) Errorf(format string, args ...any)    { self.keep(fmt.Sprintf(format, args...)) }
func (self *t3RouterLog) V(level int32) sidecar.Verbose        { return &t3RouterVerbose{log: self} }
func (self *t3RouterVerbose) Enabled() bool                    { return true }
func (self *t3RouterVerbose) Info(args ...any)                 { self.log.Info(args...) }
func (self *t3RouterVerbose) Infof(format string, args ...any) { self.log.Infof(format, args...) }

type t3RouterVerbose struct {
	log *t3RouterLog
}

// The router on server's host over the stack's stores, serving a listener the test feeds.
type t3RouterWorld struct {
	t         testing.TB
	ctx       context.Context
	clock     *sidecartest.FakeClock
	settings  *mxedgeservice.MxEdgeSettings
	host      *sidecar.ServiceHost
	fakes     *sidecartest.FakeHost
	redis     *t3SwitchRedis
	stack     sidecar.Redis
	log       *t3RouterLog
	engine    *mxedgeservice.MxEdgeService
	dialer    *t3RouterDialer
	listener  *t3RouterListener
	networkId sidecar.Id
	roots     *x509.CertPool
	start     time.Time
}

// A certificate for the SNI, self-signed, valid around now, with its pool.
func t3RouterCertificate(t testing.TB, now time.Time) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(27),
		Subject:      pkix.Name{CommonName: t3RouterSni},
		DNSNames:     []string{t3RouterSni},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
	}
	certificateBytes, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(certificateBytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{certificateBytes}, PrivateKey: privateKey}, roots
}

// The engine as the router block builds it (mxedgehost.NewRouterHost): a sidecarhost ServiceHost holding a Postgres
// pool and the stack's Redis, model's store, mxedgehost's engine deps and mxedge.yml's settings for the test's names,
// with the fake clock, gates, quota and metrics, and the test's dialer. Returns once the first probe has found the
// store and today's k and the engine is ready, the network's route written as model's projection writes it.
func newT3RouterWorld(t testing.TB) *t3RouterWorld {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	// the wall clock's second, so the session records fall in an hour the store keeps
	start := time.Now().UTC().Truncate(time.Second)
	clock := sidecartest.NewFakeClock(start)
	fakes := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	networkId := sidecar.NewId()
	gates := sidecartest.NewFakeServiceGates(nil)
	gates.OpenService(sidecar.ServiceGateMxedge, connect.Id(networkId))
	fakes.Quota.SetBalance(connect.Id(networkId), 64*1024*1024)

	stack := sidecar.Redis(sidecarhost.NewRedis(nil))
	switchRedis := &t3SwitchRedis{}
	switchRedis.use(stack)
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{Postgres: true, Quota: fakes.Quota, Clock: clock})
	host.Redis = switchRedis
	host.Gates = gates
	// the fakes' registry, so the test reads every metric and two engines in one binary register nothing twice
	host.Metrics = fakes.Metrics

	settings := mxedgeservice.DefaultMxEdgeSettings()
	settings.BannerHostName = t3RouterBannerHostName
	settings.DeviationUrl = t3RouterDeviationUrl
	settings.RouteRootDomainName = t3RouterRoot
	settings.ReadyProbeInterval = t3RouterProbeInterval
	// the batches wait for the engine's last write, at its close
	settings.RecordFlushInterval = 1000 * time.Hour

	certificate, roots := t3RouterCertificate(t, start)
	dialer := &t3RouterDialer{certificate: certificate, now: start, received: make(chan *t3RouterReceived, 4)}
	for range settings.Clients {
		dialer.clientIds = append(dialer.clientIds, sidecar.NewId())
	}
	log := &t3RouterLog{}
	engine, err := mxedgeservice.NewMxEdgeRouterService(ctx, host, model.NewMxEdgeStore(nil), mxedgehost.NewEngineDeps(host, log), &mxedgeservice.MxEdgeRouterDeps{
		Dialer: dialer,
		// the host's resolver answers no name for a documentation address; the lookups still run
		LookupPtr:  func(ctx context.Context, sender netip.Addr) ([]string, error) { return nil, nil },
		LookupHost: func(ctx context.Context, name string) ([]netip.Addr, error) { return nil, nil },
	}, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	routeValue, err := json.Marshal(&model.MailRoute{NetworkId: server.Id(networkId), Label: t3RouterLabel, Host: "mx"})
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.Set(sidecartest.InspectContext(ctx), model.MailRouteKey(t3RouterLabel), routeValue, 0); err != nil {
		t.Fatal(err)
	}
	world := &t3RouterWorld{
		t: t, ctx: ctx, clock: clock, settings: settings, host: host, fakes: fakes, redis: switchRedis, stack: stack, log: log,
		engine: engine, dialer: dialer, networkId: networkId, roots: roots, start: start,
		listener: &t3RouterListener{conns: make(chan net.Conn, 1), closed: make(chan struct{})},
	}
	// the first probe has read the store and made today's k once its next wait is armed
	world.awaitTimerAt(start.Add(t3RouterProbeInterval))
	if !engine.Ready() {
		t.Fatal("the router is not ready on the stack's stores")
	}
	go engine.Serve(world.listener)
	t.Cleanup(func() { world.listener.Close() })
	return world
}

// Waits until a timer is set for at, so a step moves the fake clock only once the wait it means is armed.
func (self *t3RouterWorld) awaitTimerAt(at time.Time) {
	self.t.Helper()
	ctx, cancel := context.WithTimeout(self.ctx, t3RouterStepTimeout)
	defer cancel()
	for {
		pending, added := self.clock.WatchTimers()
		if slices.ContainsFunc(pending, at.Equal) {
			return
		}
		select {
		case <-added:
		case <-ctx.Done():
			self.t.Fatalf("no timer was set for %v", at)
		}
	}
}

// A sender's end of one connection, as the lb's PROXY listener hands the engine the sender's address.
type t3RouterClient struct {
	t      testing.TB
	world  *t3RouterWorld
	conn   net.Conn
	reader *bufio.Reader
}

func (self *t3RouterWorld) connect(sender string) *t3RouterClient {
	self.t.Helper()
	serverConn, clientConn := net.Pipe()
	self.t.Cleanup(func() { clientConn.Close() })
	select {
	case self.listener.conns <- &t3RouterServerConn{Conn: serverConn, sender: &net.TCPAddr{IP: net.ParseIP(sender), Port: 2525}}:
	case <-time.After(t3RouterStepTimeout):
		self.t.Fatal("the engine never accepted")
	}
	return &t3RouterClient{t: self.t, world: self, conn: clientConn, reader: bufio.NewReader(clientConn)}
}

func (self *t3RouterClient) write(text string) {
	self.t.Helper()
	if _, err := self.conn.Write([]byte(text)); err != nil {
		self.t.Fatalf("write: %v", err)
	}
}

// Reads one line and compares it, CRLF aside.
func (self *t3RouterClient) expect(want string) {
	self.t.Helper()
	line, err := self.reader.ReadString('\n')
	if err != nil || line != want+"\r\n" {
		self.t.Fatalf("read %q, %v; want %q", line, err, want)
	}
}

// The banner and EHLO before TLS.
func (self *t3RouterClient) ehlo() {
	self.t.Helper()
	self.expect("220 " + t3RouterBannerHostName + " ESMTP")
	self.write("EHLO " + t3RouterEhloName + "\r\n")
	self.expect("250-" + t3RouterBannerHostName)
	self.expect("250-ENHANCEDSTATUSCODES")
	self.expect("250 STARTTLS")
}

// STARTTLS and its 220 once the quiet time has passed.
func (self *t3RouterClient) startTls() {
	self.t.Helper()
	quietAt := self.world.clock.Now().Add(self.world.settings.StartTlsQuietTimeout)
	self.write("STARTTLS\r\n")
	self.world.awaitTimerAt(quietAt)
	self.world.clock.Advance(self.world.settings.StartTlsQuietTimeout)
	self.expect("220 2.0.0 Ready to start TLS")
}

// The sender's TLS over the bytes the dialog's reader has not taken, which are none past STARTTLS's 220.
func (self *t3RouterClient) tlsClient(serverName string) *tls.Conn {
	return tls.Client(&t3RouterBufferedConn{Conn: self.conn, reader: self.reader}, &tls.Config{
		ServerName: serverName,
		RootCAs:    self.world.roots,
		Time:       func() time.Time { return self.world.start },
	})
}

// One session through the splice: the dialog, STARTTLS, a handshake the receiver ends, a line in and its echo out.
func (self *t3RouterWorld) spliceSession(sender string, line string) {
	self.t.Helper()
	client := self.connect(sender)
	client.ehlo()
	client.startTls()
	senderTls := client.tlsClient(t3RouterSni)
	if err := senderTls.HandshakeContext(self.ctx); err != nil {
		self.t.Fatalf("the TLS handshake through the splice: %v", err)
	}
	if _, err := senderTls.Write([]byte(line)); err != nil {
		self.t.Fatal(err)
	}
	select {
	case received := <-self.dialer.received:
		if received.err != nil || received.line != line {
			self.t.Fatalf("the receiver read %q, %v", received.line, received.err)
		}
		if received.ingress.GetSni() != t3RouterSni || received.ingress.GetEhloName() != t3RouterEhloName {
			self.t.Fatalf("the open's MailIngress names %q and %q", received.ingress.GetSni(), received.ingress.GetEhloName())
		}
	case <-time.After(t3RouterStepTimeout):
		self.t.Fatal("the receiver read nothing")
	}
	echo, err := io.ReadAll(senderTls)
	if err != nil || string(echo) != "250 echoed "+line {
		self.t.Fatalf("the sender read %q, %v", echo, err)
	}
	client.conn.Close()
}

// budgets.yml's mxedge.route (§22.5, I-12): a whole session on server's host, the banner to the splice and data both
// ways, reaches no PostgreSQL and makes one Redis call on the stack's Redis, the route's read; the next session, the
// route in the engine's cache, makes none.
func TestT3MxedgeRouteHoldsToItsInlinePathRow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3RouterWorld(t)
		result := sidecart3.InlinePath(t, "mxedge.route", world.stack, func(counted sidecar.Redis) {
			world.redis.use(counted)
			defer world.redis.use(world.stack)
			world.spliceSession(t3RouterSender, "MAIL FROM:<a@sender.example>\n")
		})
		if result.RedisCallCount != 1 || result.RedisCalls["get"] != 1 {
			t.Fatalf("the cold session made %v", result.RedisCalls)
		}
		warm := sidecart3.InlinePath(t, "mxedge.route", world.stack, func(counted sidecar.Redis) {
			world.redis.use(counted)
			defer world.redis.use(world.stack)
			world.spliceSession(t3RouterOtherSender, "MAIL FROM:<b@sender.example>\n")
		})
		if warm.RedisCallCount != 0 {
			t.Fatalf("the warm session made %v", warm.RedisCalls)
		}
	})
}

// S7's router clauses on T3, and mx-2's record: plaintext gets 530; an unroutable name closes in the handshake and
// writes the sender's /48 to the fleet's memory first, so a sibling's plaintext retry gets 451 4.7.0; a routed session
// splices; at the engine's close one record is written, for the routed session alone, holding no address or name; and
// no store, log or metric holds the sender, its EHLO name, a name it asked for or a canary it wrote inside TLS.
func TestT3S7RouterRefusesHoldsNothingAndRecordsNoAddress(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3RouterWorld(t)
		captureNet := sidecartest.NewNet(world.ctx, nil)
		defer captureNet.Close()
		capture := captureNet.Capture()
		canary := capture.CanaryText("mail inside TLS")

		// plaintext with no closed handshake behind it: 530, linking the deviation
		plain := world.connect(t3RouterSender)
		plain.ehlo()
		plain.write("MAIL FROM:<a@sender.example>\r\n")
		plain.expect("530 5.7.0 Must issue a STARTTLS command first, see " + t3RouterDeviationUrl)
		plain.conn.Close()

		// a name no route holds: unrecognized_name in the handshake, nothing dialed, nothing held
		unroutable := world.connect(t3RouterSender)
		unroutable.ehlo()
		unroutable.startTls()
		err := unroutable.tlsClient(t3RouterUnknownSni).HandshakeContext(world.ctx)
		if err == nil || !strings.Contains(err.Error(), "unrecognized name") {
			t.Fatalf("an unroutable name's handshake: %v", err)
		}
		unroutable.conn.Close()
		if count := world.dialer.dialCount(); count != 0 {
			t.Fatalf("an unroutable name was dialed %d times", count)
		}

		// the sender's plaintext retry from a sibling address in its /48: 451, a temporary failure, never 530
		retry := world.connect(t3RouterSenderSibling)
		retry.ehlo()
		retry.write("MAIL FROM:<a@sender.example>\r\n")
		retry.expect("451 4.7.0 TLS required, try again later")
		retry.conn.Close()

		// a routed session, the canary inside its TLS
		world.spliceSession(t3RouterOtherSender, "MAIL FROM:<"+canary+"@sender.example>\n")

		// the engine's last batch at its close: one record, the routed session's, holding no address or name
		world.engine.Drain()
		world.engine.Close()
		inspect := sidecartest.InspectContext(world.ctx)
		records := model.GetMailConnectionRecords(inspect, server.Id(world.networkId), world.start.Add(-time.Hour), world.start.Add(time.Hour))
		if len(records) != 1 || records[0].ByteCount <= 0 || records[0].Result == "" {
			t.Fatalf("records %+v", records)
		}
		// and the sessions that named no network left none: the table holds the one row, of mx-2's four columns
		var rowCount int
		var columns []string
		server.Db(inspect, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(inspect, `SELECT count(*) FROM mail_connection_record`).Scan(&rowCount))
			server.Raise(conn.QueryRow(
				inspect,
				`SELECT array_agg(column_name::text ORDER BY column_name::text) FROM information_schema.columns WHERE table_name = 'mail_connection_record'`,
			).Scan(&columns))
		})
		if rowCount != 1 || !slices.Equal(columns, []string{"byte_count", "network_id", "result", "start_time"}) {
			t.Fatalf("mail_connection_record holds %d rows of columns %v", rowCount, columns)
		}

		// no stored byte, log line or metric names the sender, its sibling, its EHLO name, an asked name or the canary
		captureSettings := sidecart3.DefaultCaptureStoresSettings()
		// sidecart3's path is from its own directory
		captureSettings.SidecarColumnsPath = "../../local/sidecar-columns.yml"
		if _, err := sidecart3.CaptureStores(captureNet, captureSettings); err != nil {
			t.Fatal(err)
		}
		for _, needle := range []struct {
			label string
			value string
		}{
			{"sender address", t3RouterSender},
			{"sibling address", t3RouterSenderSibling},
			{"other sender address", t3RouterOtherSender},
			{"ehlo name", t3RouterEhloName},
			{"unroutable name", t3RouterUnknownSni},
			{"sender's /48", "2001:db8:5f1::"},
		} {
			capture.Forbid(needle.label, []byte(needle.value))
		}
		for _, line := range world.log.Lines() {
			capture.AddItem(&sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassLog, Name: "log", Body: []byte(line)})
		}
		for _, line := range world.fakes.Metrics.Dump() {
			capture.AddItem(&sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassMetric, Name: "metric", Body: []byte(line)})
		}
		if capture.ItemCount(sidecartest.PrivacyClassRedis) == 0 || capture.ItemCount(sidecartest.PrivacyClassPostgres) == 0 {
			t.Fatal("the scan read no Redis key or no Postgres row")
		}
		for _, finding := range capture.Scan() {
			t.Errorf("%s", finding)
		}
	})
}
