package main

// §13.5's setup baseline (TLS-M5-26; §22.8, §13.5's targets: setup adds at most 300 ms warm and 1.5 s cold at p95,
// the contract round trip and the end-to-end handshake included): BenchmarkTlsEdgeSetup times one visitor's setup
// through the block's host on sidecartest's fakes, from the TCP connect with the lb's PROXY header and a ClientHello to
// the first byte the listener writes back through the splice. Setup reads no store, so the fakes change nothing it
// measures; its Find and open are a stand-in that opens at once, so what is timed is tlsedge's own share: the PROXY
// listener, the hello's read and parse, the map, the kill switch, the gate, the caps, the negative cache, the lease and
// the splice's first byte. The contract round trip and Find over HTTP are the T5 run's, on the load harness
// (sidecartest.RunTlsEdgeLoad). It reports p50 and p95 beside ns/op and logs them against the targets: a baseline to
// compare a change against on this machine, not §22.8's evidence. The caps are lifted for the run, since one visitor
// opening thousands of connections a second is the bench's shape, not a visitor's.

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/tlsedgeservice"
)

// §13.5's p95 targets for what setup adds.
const (
	benchSetupWarmTarget = 300 * time.Millisecond
	benchSetupColdTarget = 1500 * time.Millisecond
)

// A stream whose listener writes one byte at once and then reads until the engine ends its direction.
type benchSetupStream struct {
	toListener   *io.PipeReader
	engineWrites *io.PipeWriter
	fromListener *io.PipeReader
	listener     *io.PipeWriter
	closeOnce    sync.Once
}

func newBenchSetupStream() *benchSetupStream {
	toListener, engineWrites := io.Pipe()
	fromListener, listener := io.Pipe()
	stream := &benchSetupStream{toListener: toListener, engineWrites: engineWrites, fromListener: fromListener, listener: listener}
	go func() {
		listener.Write([]byte{1})
		io.Copy(io.Discard, toListener)
		listener.Close()
	}()
	return stream
}

func (self *benchSetupStream) Read(b []byte) (int, error)  { return self.fromListener.Read(b) }
func (self *benchSetupStream) Write(b []byte) (int, error) { return self.engineWrites.Write(b) }
func (self *benchSetupStream) CloseWrite() error           { return self.engineWrites.Close() }
func (self *benchSetupStream) Close() error {
	self.closeOnce.Do(func() {
		self.engineWrites.Close()
		self.fromListener.Close()
	})
	return nil
}
func (self *benchSetupStream) Reset(code protocol.StreamResetCode) error { return self.Close() }
func (self *benchSetupStream) SetReadDeadline(t time.Time) error         { return nil }
func (self *benchSetupStream) SetWriteDeadline(t time.Time) error        { return nil }

// A ClientHello for serverName, as crypto/tls writes it first.
func benchSetupHello(b testing.TB, serverName string) []byte {
	b.Helper()
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	go tls.Client(client, &tls.Config{ServerName: serverName, NextProtos: []string{"h2", "http/1.1"}, InsecureSkipVerify: true}).Handshake()
	record := make([]byte, 5)
	if _, err := io.ReadFull(peer, record); err != nil {
		b.Fatal(err)
	}
	body := make([]byte, int(record[3])<<8|int(record[4]))
	if _, err := io.ReadFull(peer, body); err != nil {
		b.Fatal(err)
	}
	return append(record, body...)
}

// A block on fakes serving one network's name behind a loopback PROXY listener, its caps lifted, and the bytes one
// visitor sends: the PROXY header and the hello.
func newBenchSetupWorld(b testing.TB) (address string, visit []byte, closeWorld func()) {
	b.Helper()
	world := newHostTestWorld(b)
	networkId := sidecar.NewId()
	label, err := world.store.AddLabel(hostTestRoot, networkId)
	if err != nil {
		b.Fatal(err)
	}
	world.quota.SetBalance(connect.Id(networkId), 1<<50)
	none := int64(-1)
	world.quota.SetLimits(connect.Id(networkId), &sidecar.QuotaLimits{Tier: "team", Limits: map[string]int64{
		"tls_endpoint_concurrent":        none,
		"tls_endpoint_new_per_s":         none,
		"tls_endpoint_mbit_per_s":        none,
		"tls_visitor_concurrent":         none,
		"tls_visitor_new_per_s":          none,
		"tls_visitor_aggregate_multiple": none,
	}})
	settings := hostTestSettings()
	settings.TlsEdge.ColdLimits = &tlsedgeservice.TlsEndpointLimits{
		EndpointConcurrent: none, EndpointNewPerS: none, EndpointMbitPerS: none,
		VisitorConcurrent: none, VisitorNewPerS: none, VisitorAggregateMultiple: none,
	}
	dial := func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error) {
		if _, err := request.Metadata(0); err != nil {
			return nil, err
		}
		return newBenchSetupStream(), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	host, err := newTlsEdgeHost(ctx, settings, &hostDeps{
		Quota:      world.quota,
		Store:      world.store,
		NewClients: func(ctx context.Context) (hostClients, error) { return world.clients, nil },
		NewDial:    func(clients hostClients) (hostSocketDial, error) { return dial, nil },
		Gates:      world.gates,
		Redis:      world.redis,
		Clock:      world.clock,
		Log:        world.log,
	})
	if err != nil {
		cancel()
		b.Fatal(err)
	}
	hostTestAwait(b, "the label map's first read", 1, host.engine.NameLoads)
	hostTestAwait(b, "the kill switch's first read", 1, host.engine.KillLoads)
	hostTestAwaitClients(b, host)
	bound, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	listener, err := newProxyListenerWrap(host.ctx, 443, settings.TlsEdge.ProxyHeader, world.clock)(bound)
	if err != nil {
		b.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- host.Serve(listener)
	}()
	visit = append([]byte("PROXY TCP4 203.0.113.9 198.51.100.1 44444 443\r\n"), benchSetupHello(b, "blog."+label+"."+hostTestRoot)...)
	return bound.Addr().String(), visit, func() {
		listener.Close()
		<-served
		host.Close()
		cancel()
	}
}

// One visitor's setup: connect, send, read the listener's first byte through the splice, and close.
func benchSetupOne(b testing.TB, address string, visit []byte) time.Duration {
	start := time.Now()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := conn.Write(visit); err != nil {
		b.Fatal(err)
	}
	first := make([]byte, 1)
	if _, err := io.ReadFull(conn, first); err != nil || first[0] != 1 {
		b.Fatalf("the listener's first byte: %x, %v", first, err)
	}
	return time.Since(start)
}

// The quantile q of durations, which it sorts.
func benchSetupQuantile(durations []time.Duration, q float64) time.Duration {
	sort.Slice(durations, func(i int, j int) bool { return durations[i] < durations[j] })
	return durations[min(len(durations)-1, int(q*float64(len(durations))))]
}

// One cold setup and count warm ones on a fresh world: the cold one's time and the warm ones' p50 and p95.
func benchSetupRun(b testing.TB, count int, timer func(start bool)) (cold time.Duration, p50 time.Duration, p95 time.Duration) {
	address, visit, closeWorld := newBenchSetupWorld(b)
	defer closeWorld()
	// the first connection is cold: the network's limits and leases are loaded on it
	cold = benchSetupOne(b, address, visit)
	durations := make([]time.Duration, 0, count)
	timer(true)
	for range count {
		durations = append(durations, benchSetupOne(b, address, visit))
	}
	timer(false)
	p50 = benchSetupQuantile(slices.Clone(durations), 0.5)
	p95 = benchSetupQuantile(durations, 0.95)
	return cold, p50, p95
}

func BenchmarkTlsEdgeSetup(b *testing.B) {
	cold, p50, p95 := benchSetupRun(b, b.N, func(start bool) {
		if start {
			b.ResetTimer()
		} else {
			b.StopTimer()
		}
	})
	b.ReportMetric(float64(p50.Microseconds()), "p50-us")
	b.ReportMetric(float64(p95.Microseconds()), "p95-us")
	b.ReportMetric(float64(cold.Microseconds()), "cold-us")
	b.Logf("tlsedge's own setup: cold %v against §13.5's %v, warm p50 %v and p95 %v against %v, over %d connections",
		cold, benchSetupColdTarget, p50, p95, benchSetupWarmTarget, b.N)
}

// How many warm setups the baseline test times: enough for a p95, and few enough that repeated runs leave the host's
// ephemeral ports to spare, since each closed connection's port waits out TIME_WAIT.
const benchSetupBaselineCount = 500

// The benchmark's measure as a test, so a plain go test records the baseline: a fixed count's p95 is logged against
// §13.5's targets and held to them, since tlsedge's own share is a small part of what they bound.
func TestTlsEdgeSetupBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("the baseline runs only without -short")
	}
	cold, p50, p95 := benchSetupRun(t, benchSetupBaselineCount, func(start bool) {})
	t.Logf("tlsedge's own setup: cold %v, warm p50 %v and p95 %v over %d connections", cold, p50, p95, benchSetupBaselineCount)
	if benchSetupWarmTarget < p95 || benchSetupColdTarget < cold {
		t.Fatalf("tlsedge's own setup is past §13.5's targets: p95 %v (%v), cold %v (%v)", p95, benchSetupWarmTarget, cold, benchSetupColdTarget)
	}
}
