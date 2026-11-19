//go:build sidecar_pending

package main

// T3 for tlsedge on server's host (TLS-M5-26; R-TLS-2, R-TLS-9, D-TLS-9, FS-34, I-12, I-13, X11; §22.5's §13 row and its
// hot-path gate): the block's host as main composes it (newTlsEdgeHost) on model.NewTlsEdgeStore over the private
// stack's Postgres, the test's own database, and the stack's Redis, through .stack/server-test.sh (Q-TEST-1), with
// sidecartest's Quota, gates and metrics and a fake clock that starts at the wall clock's second, so the records land in
// an hour the store keeps. The names are model's: a root, a network and its label made as publishing makes them, and
// the kill switch is model.SetTlsSuspension's row and {tes_} keys with the trigger's key event on the host's one
// subscriber. The Find and open alone are the test's: the block's dial is SocketEdgeDialer over its platform clients,
// whose stream only the sockets layer makes and whose T3 is SOCK-M5-17's, so here the device ends TLS itself on its own
// key behind the dial (t3EdgeDevice), or §13.7's front does (tlsedge_front_t3_test.go). On it:
//
//   - budgets.yml's tlsedge.route: one visitor's whole connection, its PROXY header, its hello, the map, the kill switch,
//     the gate, the caps, the negative cache, the dial, the lease, the splice both ways and its record's queue, under
//     sidecart3.InlinePath: no PostgreSQL acquisition and no Redis call (§22.5, I-12); the sockets block's own Find is
//     sockets.find's row;
//   - passthrough (X1): the visitor completes TLS with the device's key, the bytes the edge spliced are the hello and
//     ciphertext, and no store holds what the visitor wrote; tls_connection_record holds the visit with a 16-byte hash
//     and no address (tls-3, I-13);
//   - the kill switch (§13.9): a kill written through model.SetTlsSuspension reaches the engine through the {tes_*}n
//     key event with the fake clock unmoved, so well inside 5 s, refusing new visitors with unrecognized_name and no
//     Find and resetting an open splice; a lift lets visitors in again;
//   - the route probe (§13.4): a hello offering ur-route-probe gets unrecognized_name with no Find;
//   - CROSSSLOT (§22.4): every multi-key call and script the engine sends the stack names its keys under one hash tag as
//     it runs, the {tes_} hash and its trigger share one, and the static lint finds no crossing in the packages that
//     write {tes_} and {tcb_}.
//
// tlsedge serving only 443, 8443, 8883 and 5671 is main_test.go's on fakes, the port list being the host's. FS-41's and
// FS-42's M5 steps are tlsedge_recovery_test.go's, and the setup baseline tlsedge_bench_test.go's. Nothing sleeps:
// loads, kills and connection ends are awaited by the engine's own counts, and a real timer is only a guard.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/crossslot"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/sidecart3"
)

// A guard on what a test waits for, never a pause: each wait ends at the event it waits on.
const t3EdgeGuardTimeout = 60 * time.Second

// A Redis whose calls go to the one set last: the stack's own, and inside the inline path InlinePath's counted copy of
// it, so the engine built before the path reaches Redis on the path only through what the path is given.
type t3EdgeSwitchRedis struct {
	target atomic.Pointer[sidecar.Redis]
}

var _ sidecar.Redis = (*t3EdgeSwitchRedis)(nil)

func (self *t3EdgeSwitchRedis) use(redis sidecar.Redis) {
	self.target.Store(&redis)
}

func (self *t3EdgeSwitchRedis) redis() sidecar.Redis {
	return *self.target.Load()
}

func (self *t3EdgeSwitchRedis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return self.redis().Get(ctx, key)
}

func (self *t3EdgeSwitchRedis) GetMany(ctx context.Context, keys []string) ([][]byte, error) {
	return self.redis().GetMany(ctx, keys)
}

func (self *t3EdgeSwitchRedis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return self.redis().Set(ctx, key, value, ttl)
}

func (self *t3EdgeSwitchRedis) SetIfAbsent(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	return self.redis().SetIfAbsent(ctx, key, value, ttl)
}

func (self *t3EdgeSwitchRedis) CompareAndSet(ctx context.Context, key string, expected []byte, value []byte, ttl time.Duration) (bool, error) {
	return self.redis().CompareAndSet(ctx, key, expected, value, ttl)
}

func (self *t3EdgeSwitchRedis) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	return self.redis().CompareAndDelete(ctx, key, expected)
}

func (self *t3EdgeSwitchRedis) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	return self.redis().Increment(ctx, key, ttl)
}

func (self *t3EdgeSwitchRedis) Delete(ctx context.Context, keys []string) error {
	return self.redis().Delete(ctx, keys)
}

func (self *t3EdgeSwitchRedis) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return self.redis().Expire(ctx, key, ttl)
}

func (self *t3EdgeSwitchRedis) HashGet(ctx context.Context, key string, field string) ([]byte, bool, error) {
	return self.redis().HashGet(ctx, key, field)
}

func (self *t3EdgeSwitchRedis) HashGetAll(ctx context.Context, key string) (map[string][]byte, error) {
	return self.redis().HashGetAll(ctx, key)
}

func (self *t3EdgeSwitchRedis) HashSet(ctx context.Context, key string, fieldValues map[string][]byte) error {
	return self.redis().HashSet(ctx, key, fieldValues)
}

func (self *t3EdgeSwitchRedis) HashDelete(ctx context.Context, key string, fields []string) error {
	return self.redis().HashDelete(ctx, key, fields)
}

func (self *t3EdgeSwitchRedis) HashExpire(ctx context.Context, key string, fields []string, ttl time.Duration) error {
	return self.redis().HashExpire(ctx, key, fields, ttl)
}

func (self *t3EdgeSwitchRedis) StreamAdd(ctx context.Context, key string, maxLength int64, fieldValues map[string][]byte) (string, error) {
	return self.redis().StreamAdd(ctx, key, maxLength, fieldValues)
}

func (self *t3EdgeSwitchRedis) StreamRange(ctx context.Context, key string, afterEntryId string, count int64) ([]*sidecar.RedisStreamEntry, error) {
	return self.redis().StreamRange(ctx, key, afterEntryId, count)
}

func (self *t3EdgeSwitchRedis) StreamGroupCreate(ctx context.Context, key string, group string) error {
	return self.redis().StreamGroupCreate(ctx, key, group)
}

func (self *t3EdgeSwitchRedis) StreamGroupRead(ctx context.Context, key string, group string, consumer string, count int64, block time.Duration) ([]*sidecar.RedisStreamEntry, error) {
	return self.redis().StreamGroupRead(ctx, key, group, consumer, count, block)
}

func (self *t3EdgeSwitchRedis) StreamGroupAck(ctx context.Context, key string, group string, entryIds []string) error {
	return self.redis().StreamGroupAck(ctx, key, group, entryIds)
}

func (self *t3EdgeSwitchRedis) StreamGroupClaim(ctx context.Context, key string, group string, consumer string, minIdle time.Duration, cursor string, count int64) ([]*sidecar.RedisStreamEntry, string, error) {
	return self.redis().StreamGroupClaim(ctx, key, group, consumer, minIdle, cursor, count)
}

func (self *t3EdgeSwitchRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	return self.redis().RunScript(ctx, script, keys, args)
}

func (self *t3EdgeSwitchRedis) AddKeyEventCallback(keyPattern string, callback sidecar.RedisKeyEventFunction) (func(), error) {
	return self.redis().AddKeyEventCallback(keyPattern, callback)
}

// The visitor every T3 connection comes from, as the lb's PROXY header names it: a documentation address.
const t3EdgeVisitorAddress = "203.0.113.41"

// What a device's service writes back first, so a visitor's read proves the splice reached it.
const t3EdgeDeviceGreeting = "device:"

// The multi-key calls and scripts a world's Redis saw whose keys do not share one hash tag.
type t3EdgeTags struct {
	stateLock sync.Mutex
	crossings []string
}

func (self *t3EdgeTags) check(call string, keys []string) {
	if len(keys) < 2 || sidecar.RedisKeysShareTag(keys) {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.crossings = append(self.crossings, fmt.Sprintf("%s %v", call, keys))
}

func (self *t3EdgeTags) Crossings() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.crossings)
}

// The stack's Redis with every multi-key call's keys checked for one tag as it runs.
type t3EdgeTaggedRedis struct {
	sidecar.Redis
	tags *t3EdgeTags
}

func (self *t3EdgeTaggedRedis) GetMany(ctx context.Context, keys []string) ([][]byte, error) {
	self.tags.check("GetMany", keys)
	return self.Redis.GetMany(ctx, keys)
}

func (self *t3EdgeTaggedRedis) Delete(ctx context.Context, keys []string) error {
	self.tags.check("Delete", keys)
	return self.Redis.Delete(ctx, keys)
}

func (self *t3EdgeTaggedRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	self.tags.check("RunScript", keys)
	return self.Redis.RunScript(ctx, script, keys, args)
}

// One direction of a stream between the engine and a device: io.Pipe's, whose writes wait for reads.
type t3EdgePipe struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

func newT3EdgePipe() *t3EdgePipe {
	reader, writer := io.Pipe()
	return &t3EdgePipe{reader: reader, writer: writer}
}

// The engine's end of a stream to a device, as the dial hands it over.
type t3EdgeStream struct {
	toDevice   *t3EdgePipe
	fromDevice *t3EdgePipe
	// what the engine wrote toward the device, the hello's rest included: what the edge spliced
	sent      *t3EdgeRecording
	resets    chan protocol.StreamResetCode
	resetOnce sync.Once
}

var _ tlsedgeservice.TlsEdgeStream = (*t3EdgeStream)(nil)

func (self *t3EdgeStream) Read(b []byte) (int, error) { return self.fromDevice.reader.Read(b) }

func (self *t3EdgeStream) Write(b []byte) (int, error) {
	self.sent.add(b)
	return self.toDevice.writer.Write(b)
}

func (self *t3EdgeStream) CloseWrite() error { return self.toDevice.writer.Close() }

func (self *t3EdgeStream) Close() error {
	self.toDevice.writer.Close()
	self.fromDevice.reader.Close()
	return nil
}

func (self *t3EdgeStream) Reset(code protocol.StreamResetCode) error {
	self.resetOnce.Do(func() {
		reset := errors.New("stream reset")
		self.toDevice.writer.CloseWithError(reset)
		self.fromDevice.reader.CloseWithError(reset)
		self.resets <- code
	})
	return nil
}

func (self *t3EdgeStream) SetReadDeadline(t time.Time) error  { return nil }
func (self *t3EdgeStream) SetWriteDeadline(t time.Time) error { return nil }

// Bytes as one side sent them, in order.
type t3EdgeRecording struct {
	stateLock sync.Mutex
	bytes     []byte
}

func (self *t3EdgeRecording) add(b []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.bytes = append(self.bytes, b...)
}

func (self *t3EdgeRecording) Bytes() []byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.bytes)
}

// The device's end of a stream as a net.Conn: the hello's early data first, then what the engine writes.
type t3EdgeDeviceConn struct {
	reader io.Reader
	stream *t3EdgeStream
}

var _ net.Conn = (*t3EdgeDeviceConn)(nil)

func (self *t3EdgeDeviceConn) Read(b []byte) (int, error) { return self.reader.Read(b) }
func (self *t3EdgeDeviceConn) Write(b []byte) (int, error) {
	return self.stream.fromDevice.writer.Write(b)
}

func (self *t3EdgeDeviceConn) Close() error {
	self.stream.fromDevice.writer.Close()
	self.stream.toDevice.reader.Close()
	return nil
}

// Ends the device's direction alone, as a stream's CloseWrite does.
func (self *t3EdgeDeviceConn) CloseWrite() error { return self.stream.fromDevice.writer.Close() }

func (self *t3EdgeDeviceConn) LocalAddr() net.Addr                { return t3EdgeStreamAddr{} }
func (self *t3EdgeDeviceConn) RemoteAddr() net.Addr               { return t3EdgeStreamAddr{} }
func (self *t3EdgeDeviceConn) SetDeadline(t time.Time) error      { return nil }
func (self *t3EdgeDeviceConn) SetReadDeadline(t time.Time) error  { return nil }
func (self *t3EdgeDeviceConn) SetWriteDeadline(t time.Time) error { return nil }

type t3EdgeStreamAddr struct{}

func (t3EdgeStreamAddr) Network() string { return "ur" }
func (t3EdgeStreamAddr) String() string  { return "stream" }

// One dial the engine made and the stream it opened.
type t3EdgeOpened struct {
	request  *sidecar.SocketEdgeDialRequest
	metadata []byte
	stream   *t3EdgeStream
}

// The block's dial for a test: each request recorded, a refusal queued answered first, and otherwise a stream opened
// with its TlsEdgeOpen and handed to serve, the device, as a ticket's listener would get it, or, once a route is set,
// the request handed to the route: the real sockets engine's Find and a listener's front
// (tlsedge_custom_route_t3_test.go).
type t3EdgeDial struct {
	// the grant expiry each ticket names, past every test's end
	expireTimeMillis uint64
	serve            func(conn *t3EdgeDeviceConn, opened *t3EdgeOpened)

	stateLock sync.Mutex
	requests  []*sidecar.SocketEdgeDialRequest
	refusals  []error
	opened    chan *t3EdgeOpened
	route     func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error)
}

// Hands every dial past a queued refusal to route from now on.
func (self *t3EdgeDial) setRoute(route func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.route = route
}

func (self *t3EdgeDial) refuseNext(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refusals = append(self.refusals, err)
}

func (self *t3EdgeDial) Requests() []*sidecar.SocketEdgeDialRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

func (self *t3EdgeDial) dial(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error) {
	refusal, route := func() (error, func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error)) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, request)
		if 0 < len(self.refusals) {
			refusal := self.refusals[0]
			self.refusals = self.refusals[1:]
			return refusal, nil
		}
		return nil, self.route
	}()
	if refusal != nil {
		return nil, refusal
	}
	if route != nil {
		return route(ctx, request)
	}
	metadata, err := request.Metadata(self.expireTimeMillis)
	if err != nil {
		return nil, err
	}
	stream := &t3EdgeStream{
		toDevice:   newT3EdgePipe(),
		fromDevice: newT3EdgePipe(),
		sent:       &t3EdgeRecording{},
		resets:     make(chan protocol.StreamResetCode, 1),
	}
	stream.sent.add(request.EarlyData)
	opened := &t3EdgeOpened{request: request, metadata: metadata, stream: stream}
	conn := &t3EdgeDeviceConn{reader: io.MultiReader(bytes.NewReader(slices.Clone(request.EarlyData)), stream.toDevice.reader), stream: stream}
	go self.serve(conn, opened)
	select {
	case self.opened <- opened:
	default:
	}
	return stream, nil
}

// A device's certificate for its network's names, on its own key: what a visitor's handshake ends on (X1).
type t3EdgeDeviceCertificate struct {
	certificate tls.Certificate
	roots       *x509.CertPool
}

func newT3EdgeDeviceCertificate(t testing.TB, names []string, now time.Time) *t3EdgeDeviceCertificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: names[0]},
		DNSNames:              names,
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return &t3EdgeDeviceCertificate{certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, roots: roots}
}

// A device's service behind the stream: TLS on the device's key, then the greeting and an echo of what the visitor
// writes, until the visitor ends its direction.
func (self *t3EdgeDeviceCertificate) serve(conn *t3EdgeDeviceConn, opened *t3EdgeOpened) {
	defer conn.Close()
	server := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{self.certificate}, MinVersion: tls.VersionTLS13})
	if err := server.Handshake(); err != nil {
		return
	}
	if _, err := server.Write([]byte(t3EdgeDeviceGreeting)); err != nil {
		return
	}
	io.Copy(server, server)
	server.CloseWrite()
}

// One tlsedge block on the stack's stores and its network.
type t3EdgeWorld struct {
	t         testing.TB
	ctx       context.Context
	clock     *sidecartest.FakeClock
	fakes     *sidecartest.FakeHost
	stack     sidecar.Redis
	redis     *t3EdgeSwitchRedis
	tags      *t3EdgeTags
	settings  *hostSettings
	host      *tlsEdgeHost
	dial      *t3EdgeDial
	device    *t3EdgeDeviceCertificate
	root      string
	networkId sidecar.Id
	label     string
	address   string
	start     time.Time
	log       *hostTestLog
}

// A root of the test's own under the documentation suffix, made as the dns block makes one.
func newT3EdgeRoot(t testing.TB, ctx context.Context) string {
	t.Helper()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	root := "t" + hex.EncodeToString(suffix) + ".example"
	if err := model.EnsureDnsRoot(ctx, root+"."); err != nil {
		t.Fatal(err)
	}
	return root
}

// A network with its label in root, made as publishing makes them: its zone first, so its label takes its place in
// the map's serial order. Its id is networkId's when one is given.
func newT3EdgeNetwork(t testing.TB, ctx context.Context, root string, givenNetworkId *sidecar.Id) (sidecar.Id, string) {
	t.Helper()
	networkId := server.NewId()
	if givenNetworkId != nil {
		networkId = server.Id(*givenNetworkId)
	}
	model.Testing_CreateNetwork(ctx, networkId, "tls-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), server.NewId())
	if _, err := model.EnsureNetworkZone(ctx, networkId, root); err != nil {
		t.Fatal(err)
	}
	label, err := model.EnsureNetworkLabel(ctx, networkId, root)
	if err != nil {
		t.Fatal(err)
	}
	return sidecar.Id(networkId), label
}

// A block on the stack's Postgres and Redis serving its network's names behind a loopback PROXY listener on 443, ready,
// its settings changed by configure first.
func newT3EdgeWorld(t testing.TB, configure func(settings *hostSettings)) *t3EdgeWorld {
	t.Helper()
	return newT3EdgeWorldOn(t, sidecartest.NewFakeClock(time.Now().UTC().Truncate(time.Second)), nil, configure)
}

// newT3EdgeWorld on clock, which a world of other engines shares, for the network networkId when one is given: one an
// authorization log of its own already names.
func newT3EdgeWorldOn(t testing.TB, clock *sidecartest.FakeClock, givenNetworkId *sidecar.Id, configure func(settings *hostSettings)) *t3EdgeWorld {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	start := clock.Now()
	fakes := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	root := newT3EdgeRoot(t, ctx)
	networkId, label := newT3EdgeNetwork(t, ctx, root, givenNetworkId)
	fakes.Quota.SetLimits(connect.Id(networkId), &sidecar.QuotaLimits{Tier: "free", Limits: map[string]int64{
		"tls_endpoint_concurrent":        256,
		"tls_endpoint_new_per_s":         50,
		"tls_endpoint_mbit_per_s":        20,
		"tls_visitor_concurrent":         64,
		"tls_visitor_new_per_s":          20,
		"tls_visitor_aggregate_multiple": 4,
	}})
	fakes.Quota.SetBalance(connect.Id(networkId), 1<<30)
	fakes.Gates.OpenService(sidecar.ServiceGateTlsedge, connect.Id(networkId))

	tags := &t3EdgeTags{}
	stack := sidecar.Redis(&t3EdgeTaggedRedis{Redis: sidecarhost.NewRedis(nil), tags: tags})
	switchRedis := &t3EdgeSwitchRedis{}
	switchRedis.use(stack)

	tlsEdge := tlsedgeservice.DefaultTlsEdgeSettings()
	tlsEdge.Root = root
	tlsEdge.Blocks = 1
	// the records wait for the engine's last write, at its close
	tlsEdge.RecordFlushInterval = 1000 * time.Hour
	settings := defaultHostSettings(tlsEdge)
	if configure != nil {
		configure(settings)
	}
	device := newT3EdgeDeviceCertificate(t, []string{"*." + label + "." + root, label + "." + root}, start)
	dial := &t3EdgeDial{
		expireTimeMillis: uint64(start.Add(24 * time.Hour).UnixMilli()),
		serve:            device.serve,
		opened:           make(chan *t3EdgeOpened, 64),
	}
	clients := newHostTestClients(settings.TlsEdge.Clients)
	log := &hostTestLog{}
	deps := &hostDeps{
		Quota: fakes.Quota,
		Store: model.NewTlsEdgeStore(nil),
		NewClients: func(ctx context.Context) (hostClients, error) {
			return clients, nil
		},
		NewDial: func(clients hostClients) (hostSocketDial, error) {
			return dial.dial, nil
		},
		Gates:   fakes.Gates,
		Redis:   switchRedis,
		Metrics: fakes.Metrics,
		Clock:   clock,
		Log:     log,
	}
	host, err := newTlsEdgeHost(ctx, settings, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	hostTestAwait(t, "the label map's first read", 1, host.engine.NameLoads)
	hostTestAwait(t, "the kill switch's first read", 1, host.engine.KillLoads)
	hostTestAwaitClients(t, host)
	if !host.Ready() {
		t.Fatalf("the block is not ready: %v", host.Missing())
	}
	if _, ok := host.engine.Names().Resolve("blog."+label+"."+root, 443); !ok {
		t.Fatal("the block's map does not hold the network's label")
	}

	bound, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := newProxyListenerWrap(host.ctx, 443, settings.TlsEdge.ProxyHeader, clock)(bound)
	if err != nil {
		bound.Close()
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- host.Serve(listener)
	}()
	t.Cleanup(func() {
		listener.Close()
		<-served
	})
	return &t3EdgeWorld{
		t: t, ctx: ctx, clock: clock, fakes: fakes, stack: stack, redis: switchRedis, tags: tags, settings: settings,
		host: host, dial: dial, device: device, root: root, networkId: networkId, label: label,
		address: bound.Addr().String(), start: start, log: log,
	}
}

// The name a visitor reaches host of the world's network by.
func (self *t3EdgeWorld) serverName(host string) string {
	return host + "." + self.label + "." + self.root
}

// A visitor's TCP connection to the block with the lb's PROXY v1 header naming the visitor, its deadline the guard.
func (self *t3EdgeWorld) dialVisitor() net.Conn {
	self.t.Helper()
	conn, err := net.Dial("tcp", self.address)
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(t3EdgeGuardTimeout))
	if _, err := conn.Write([]byte("PROXY TCP4 " + t3EdgeVisitorAddress + " 198.51.100.1 44444 443\r\n")); err != nil {
		self.t.Fatal(err)
	}
	return conn
}

// A visitor's whole visit to serverName: TLS with the device, its greeting read, payload written and echoed back, and
// both directions ended. Returns the TLS state the visitor saw.
func (self *t3EdgeWorld) visit(serverName string, alpnIds []string, payload []byte) tls.ConnectionState {
	self.t.Helper()
	conn := self.dialVisitor()
	client := tls.Client(conn, &tls.Config{ServerName: serverName, RootCAs: self.device.roots, NextProtos: alpnIds, MinVersion: tls.VersionTLS13})
	if err := client.Handshake(); err != nil {
		self.t.Fatalf("the visitor's handshake for %s: %v", serverName, err)
	}
	greeting := make([]byte, len(t3EdgeDeviceGreeting))
	if _, err := io.ReadFull(client, greeting); err != nil || string(greeting) != t3EdgeDeviceGreeting {
		self.t.Fatalf("the device's greeting: %q, %v", greeting, err)
	}
	if _, err := client.Write(payload); err != nil {
		self.t.Fatal(err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(client, echo); err != nil || !bytes.Equal(echo, payload) {
		self.t.Fatalf("the echo: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		self.t.Fatal(err)
	}
	if rest, err := io.ReadAll(client); err != nil || 0 < len(rest) {
		self.t.Fatalf("after the echo: %d bytes, %v", len(rest), err)
	}
	state := client.ConnectionState()
	conn.Close()
	return state
}

// What a refused visitor gets for hello: the alert and the close, all it ever reads.
func (self *t3EdgeWorld) answer(hello []byte) []byte {
	self.t.Helper()
	conn := self.dialVisitor()
	if _, err := conn.Write(hello); err != nil {
		self.t.Fatal(err)
	}
	answer, err := io.ReadAll(conn)
	if err != nil {
		self.t.Fatalf("the visitor read: %v", err)
	}
	return answer
}

// A ClientHello for serverName offering alpnIds, as a browser's would begin: the bytes crypto/tls writes first.
func t3EdgeHello(t testing.TB, serverName string, alpnIds []string) []byte {
	t.Helper()
	recording := &t3EdgeHelloConn{written: make(chan []byte, 1)}
	client := tls.Client(recording, &tls.Config{ServerName: serverName, NextProtos: alpnIds, InsecureSkipVerify: true})
	go client.Handshake()
	select {
	case hello := <-recording.written:
		client.Close()
		return hello
	case <-time.After(t3EdgeGuardTimeout):
		t.Fatal("crypto/tls wrote no hello")
	}
	return nil
}

// A conn that keeps the first write, a hello, and answers nothing.
type t3EdgeHelloConn struct {
	written chan []byte
	once    sync.Once
	closed  chan struct{}
}

func (self *t3EdgeHelloConn) Read(b []byte) (int, error) {
	self.once.Do(func() { self.closed = make(chan struct{}) })
	<-self.closed
	return 0, io.EOF
}

func (self *t3EdgeHelloConn) Write(b []byte) (int, error) {
	select {
	case self.written <- slices.Clone(b):
	default:
	}
	return len(b), nil
}

func (self *t3EdgeHelloConn) Close() error {
	self.once.Do(func() { self.closed = make(chan struct{}) })
	select {
	case <-self.closed:
	default:
		close(self.closed)
	}
	return nil
}

func (self *t3EdgeHelloConn) LocalAddr() net.Addr                { return t3EdgeStreamAddr{} }
func (self *t3EdgeHelloConn) RemoteAddr() net.Addr               { return t3EdgeStreamAddr{} }
func (self *t3EdgeHelloConn) SetDeadline(t time.Time) error      { return nil }
func (self *t3EdgeHelloConn) SetReadDeadline(t time.Time) error  { return nil }
func (self *t3EdgeHelloConn) SetWriteDeadline(t time.Time) error { return nil }

// The alert a refused visitor reads: one TLS 1.2 alert record, fatal.
func t3EdgeAlert(code tlsedgeservice.TlsAlertCode) []byte {
	return []byte{21, 3, 3, 0, 2, 2, byte(code)}
}

// Waits until the engine has ended count connections since it started.
func (self *t3EdgeWorld) awaitEnded(count uint64) {
	self.t.Helper()
	hostTestAwait(self.t, "connections ended", count, self.host.engine.ConnectionEnds)
}

// Writes a kill of host through the model as api's abuse action does, and waits for the engine to take it from the
// trigger's key event, with no clock moved.
func (self *t3EdgeWorld) kill(host string) {
	self.t.Helper()
	count, _ := self.host.engine.KillChanges()
	if err := model.SetTlsSuspension(self.ctx, &sidecar.TlsSuspendArgs{NetworkId: self.networkId, Host: host, Reason: "t3"}); err != nil {
		self.t.Fatal(err)
	}
	hostTestAwait(self.t, "the kill's key event", count+1, self.host.engine.KillChanges)
}

// Lifts host's kill through the model and waits for the engine to take it.
func (self *t3EdgeWorld) lift(host string) {
	self.t.Helper()
	count, _ := self.host.engine.KillChanges()
	if err := model.LiftTlsSuspension(self.ctx, server.Id(self.networkId), host); err != nil {
		self.t.Fatal(err)
	}
	hostTestAwait(self.t, "the lift's key event", count+1, self.host.engine.KillChanges)
}

// Waits for the dial's next open.
func (self *t3EdgeWorld) awaitOpened() *t3EdgeOpened {
	self.t.Helper()
	select {
	case opened := <-self.dial.opened:
		return opened
	case <-time.After(t3EdgeGuardTimeout):
		self.t.Fatal("the engine opened no stream")
	}
	return nil
}

// The network's records once the engine's last batch is written, at its close.
func (self *t3EdgeWorld) closeAndReadRecords() []*tlsedgeservice.TlsConnectionRecord {
	self.t.Helper()
	self.host.Close()
	return model.Testing_ReadTlsConnectionRecords(self.ctx, self.networkId, self.start.Add(-time.Hour))
}

// §22.5's hot-path gate for §13 (budgets.yml's tlsedge.route): one visitor's whole connection through the block, from
// its PROXY header to the splice's end both ways and its record's queue, reaches no PostgreSQL and makes no Redis call
// on the stack; a second, warm, the same.
func TestT3TlsedgeRouteHoldsToItsInlinePathRow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		for index, payload := range []string{"cold visit", "warm visit"} {
			result := sidecart3.InlinePath(t, "tlsedge.route", world.stack, func(counted sidecar.Redis) {
				world.redis.use(counted)
				defer world.redis.use(world.stack)
				world.visit(world.serverName("blog"), []string{"h2", "http/1.1"}, []byte(payload))
				world.awaitEnded(uint64(index + 1))
			})
			if result.RedisCallCount != 0 {
				t.Fatalf("visit %d made %v", index, result.RedisCalls)
			}
		}
		if requests := world.dial.Requests(); len(requests) != 2 || requests[0].Name != "tls/443/blog" || requests[0].ServerName != "" {
			t.Fatalf("the dials %+v", requests)
		}
	})
}

// X1 on T3: a visitor completes TLS with the device's own certificate through the block, what the edge spliced toward
// the device is the hello and ciphertext alone, the record of the visit holds no address and no store holds what the
// visitor wrote; the listener got the TlsEdgeOpen naming the visitor and the ticket's expiry.
func TestT3TlsedgePassthroughEndsOnTheDeviceKey(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		canary := []byte("t3-canary-" + hex.EncodeToString(world.networkId[:8]))
		state := world.visit(world.serverName("blog"), []string{"h2"}, canary)
		if len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, world.device.certificate.Certificate[0]) {
			t.Fatal("the visitor's handshake ended on another certificate than the device's")
		}
		opened := world.awaitOpened()
		sent := opened.stream.sent.Bytes()
		if bytes.Contains(sent, canary) {
			t.Fatal("the edge spliced the visitor's plaintext")
		}
		if len(sent) < 5 || sent[0] != 22 {
			t.Fatal("what the edge sent first is not the visitor's hello")
		}
		open := &protocol.TlsEdgeOpen{}
		if err := proto.Unmarshal(opened.metadata, open); err != nil {
			t.Fatal(err)
		}
		if open.GetServerName() != world.serverName("blog") || open.GetExpireTimeMillis() != world.dial.expireTimeMillis ||
			net.IP(open.GetClientIp()).String() != t3EdgeVisitorAddress {
			t.Fatalf("the TlsEdgeOpen %+v", open)
		}
		world.awaitEnded(1)
		records := world.closeAndReadRecords()
		if len(records) != 1 {
			t.Fatalf("%d records", len(records))
		}
		record := records[0]
		if record.Host != "blog" || record.ByteCount <= int64(len(canary)) || len(record.VisitorHash) != 16 {
			t.Fatalf("the record %+v", record)
		}
		if bytes.Contains(record.VisitorHash, []byte(t3EdgeVisitorAddress)) || bytes.Contains(record.VisitorHash, net.ParseIP(t3EdgeVisitorAddress).To4()) {
			t.Fatal("the record holds the visitor's address")
		}
		for _, line := range world.log.Lines() {
			if strings.Contains(line, t3EdgeVisitorAddress) || strings.Contains(line, world.label) || bytes.Contains([]byte(line), canary) {
				t.Errorf("a log line holds an address, a name or the canary: %s", line)
			}
		}
	})
}

// §13.9's kill switch on T3: a kill written through the model reaches the engine on the trigger's key event with the
// fake clock unmoved, so within 5 s; a new visitor gets unrecognized_name with no Find and an open splice resets with
// RST_REVOKED; the lift lets a visitor in again.
func TestT3TlsedgeKillSwitchEndsNewAndOpenConnections(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		// an open splice: the device's greeting read, the visitor waiting
		conn := world.dialVisitor()
		client := tls.Client(conn, &tls.Config{ServerName: world.serverName("blog"), RootCAs: world.device.roots, MinVersion: tls.VersionTLS13})
		if err := client.Handshake(); err != nil {
			t.Fatal(err)
		}
		greeting := make([]byte, len(t3EdgeDeviceGreeting))
		if _, err := io.ReadFull(client, greeting); err != nil {
			t.Fatal(err)
		}
		opened := world.awaitOpened()
		killTime := world.clock.Now()
		world.kill("blog")
		select {
		case code := <-opened.stream.resets:
			if code != protocol.StreamResetCode_RST_REVOKED {
				t.Fatalf("the open splice reset with %v", code)
			}
		case <-time.After(t3EdgeGuardTimeout):
			t.Fatal("the open splice was not reset")
		}
		if elapsed := world.clock.Now().Sub(killTime); 5*time.Second < elapsed {
			t.Fatalf("the kill took %v of the clock", elapsed)
		}
		dials := len(world.dial.Requests())
		if answer := world.answer(t3EdgeHello(t, world.serverName("blog"), []string{"h2"})); !bytes.Equal(answer, t3EdgeAlert(tlsedgeservice.TlsAlertUnrecognizedName)) {
			t.Fatalf("a killed name answered %x", answer)
		}
		if len(world.dial.Requests()) != dials {
			t.Fatal("a killed name reached Find")
		}
		world.lift("blog")
		world.visit(world.serverName("blog"), []string{"h2"}, []byte("after the lift"))
	})
}

// §13.4's route probe on T3: a hello offering ur-route-probe gets unrecognized_name with no Find, whatever it names.
func TestT3TlsedgeRouteProbeMakesNoFind(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		for _, serverName := range []string{world.serverName("blog"), "0123456789ab.map.probe.invalid", "probe.edge." + world.root} {
			answer := world.answer(t3EdgeHello(t, serverName, []string{"ur-route-probe", "http/1.1"}))
			if !bytes.Equal(answer, t3EdgeAlert(tlsedgeservice.TlsAlertUnrecognizedName)) {
				t.Fatalf("a route probe for %s answered %x", serverName, answer)
			}
		}
		if requests := world.dial.Requests(); 0 < len(requests) {
			t.Fatalf("route probes made %d dials", len(requests))
		}
	})
}

// §22.4's slot rule for §13's families on T3: every multi-key call and script the engine sends the stack names one hash
// tag while kills and lifts are written through the model, whose MULTI sets a network's {tes_} hash and INCRs its
// trigger, two keys of one tag; and the static lint finds no crossing in the packages that send {tes_} and {tcb_}
// through sidecar.Redis: this block, sidecar's tlsedgeservice and certsservice, whose budget script holds {tcb_}.
// model's MULTI goes through go-redis, which the lint does not read, so the pair's tag is asserted here instead. The
// runtime half on three masters waits on T3's cluster (TEST-M1-08): the stack's Redis is one node.
func TestT3TlsedgeKeysShareTheirSlot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		world.kill("blog")
		world.kill(sidecar.TlsKillAllHosts)
		world.lift("blog")
		world.lift(sidecar.TlsKillAllHosts)
		if crossings := world.tags.Crossings(); 0 < len(crossings) {
			t.Fatalf("calls crossing slots: %v", crossings)
		}
		if keys := []string{sidecar.TlsKillKey(world.networkId), sidecar.TlsKillTriggerKey(world.networkId)}; !sidecar.RedisKeysShareTag(keys) {
			t.Fatalf("the kill's MULTI names %v", keys)
		}
		for _, check := range []struct {
			directory string
			patterns  []string
		}{
			{directory: "../..", patterns: []string{"./cli/tlsedge"}},
			{directory: "../../../sidecar", patterns: []string{"./certsservice", "./tlsedgeservice"}},
		} {
			findings, err := crossslot.Check(check.directory, check.patterns...)
			if err != nil {
				t.Fatal(err)
			}
			for _, finding := range findings {
				t.Errorf("%s", finding)
			}
		}
	})
}

// TLS-M6-02's fleet key on T3 (plan-TLS-visitor-hash-key; §9.7, tls-3): two tlsedge blocks on the stack's Redis, on one
// clock so they read one UTC day, take one daily key, {vhk}tls:<yyyymmdd>, which the first to read it creates with a
// ttl of at most 25 h: one visitor's Finds carry the same 16-byte affinity key from either block, and its records the
// same 16-byte hash, the two keys apart, and neither holding the visitor's address.
func TestT3TlsedgeBlocksShareTheFleetsDailyVisitorKey(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		clock := sidecartest.NewFakeClock(time.Now().UTC().Truncate(time.Second))
		worlds := []*t3EdgeWorld{newT3EdgeWorldOn(t, clock, nil, nil), newT3EdgeWorldOn(t, clock, nil, nil)}
		for _, world := range worlds {
			hostTestAwait(t, "the fleet key's first read", 1, world.host.engine.VisitorKeyRefreshes)
			if !world.host.engine.VisitorKeys().FleetHeld() {
				t.Fatal("a block holds no fleet key after its first read")
			}
		}
		affinityKeys := [][]byte{}
		visitorHashes := [][]byte{}
		for index, world := range worlds {
			world.visit(world.serverName("blog"), []string{"h2"}, []byte(fmt.Sprintf("a visit to block %d", index)))
			world.awaitEnded(1)
			requests := world.dial.Requests()
			if len(requests) != 1 || len(requests[0].AffinityKey) != 16 {
				t.Fatalf("block %d's dials: %+v", index, requests)
			}
			affinityKeys = append(affinityKeys, requests[0].AffinityKey)
			records := world.closeAndReadRecords()
			if len(records) != 1 || len(records[0].VisitorHash) != 16 {
				t.Fatalf("block %d's records: %+v", index, records)
			}
			visitorHashes = append(visitorHashes, records[0].VisitorHash)
		}
		if !bytes.Equal(affinityKeys[0], affinityKeys[1]) {
			t.Fatal("the two blocks' Finds carry different affinity keys for one visitor")
		}
		if !bytes.Equal(visitorHashes[0], visitorHashes[1]) {
			t.Fatal("the two blocks' records hold different hashes for one visitor")
		}
		if bytes.Equal(affinityKeys[0], visitorHashes[0]) {
			t.Fatal("the affinity key is the record hash")
		}
		address := net.ParseIP(t3EdgeVisitorAddress).To4()
		for _, key := range [][]byte{affinityKeys[0], visitorHashes[0]} {
			if bytes.Contains(key, address) {
				t.Fatal("a visitor key holds the visitor's address")
			}
		}
		name := "{vhk}tls:" + clock.Now().UTC().Format("20060102")
		server.Redis(worlds[0].ctx, func(client server.RedisClient) {
			value, err := client.Get(worlds[0].ctx, name).Bytes()
			if err != nil || len(value) != 32 {
				t.Fatalf("the fleet key at %s: %d bytes, %v", name, len(value), err)
			}
			ttl, err := client.TTL(worlds[0].ctx, name).Result()
			if err != nil || ttl <= 0 || 25*time.Hour < ttl {
				t.Fatalf("the fleet key's ttl: %v, %v", ttl, err)
			}
		})
	})
}
