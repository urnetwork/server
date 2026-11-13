package alt

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"

	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
)

// An alt with its real exchange, connect handler and fronts that needs no
// test env: no exchange service port, no redis key events, no move inventory,
// and self-signed certificates for the test names.
func newEnvFreeAlt(ctx context.Context, t testing.TB, apiHandler http.Handler, mutate func(*Settings)) *Alt {
	popTls := server.Config.PushSimpleResource(
		"tls.yml",
		[]byte(fmt.Sprintf("allowed_hosts:\n- %s\n- %s\n", testApiHost, testConnectHost)),
	)
	t.Cleanup(popTls)

	settings := DefaultSettings()
	settings.ApiHosts = []string{testApiHost}
	settings.ConnectHosts = []string{testConnectHost}
	settings.ExchangeSettings.TransportTlsSettings.EnableSelfSign = true
	settings.ExchangeSettings.KeyEventDelivery.Enabled = false
	settings.ExchangeSettings.EnableMove = false
	if mutate != nil {
		mutate(settings)
	}
	exchange := connectserver.NewExchangeWithListeners(
		ctx,
		"host0",
		"connect",
		"test",
		map[int]int{},
		map[string]string{},
		settings.ExchangeSettings,
		map[int]net.Listener{},
	)
	t.Cleanup(exchange.Close)
	connectHandler := connectserver.NewConnectHandlerWithPacketConns(
		ctx,
		server.NewId(),
		exchange,
		&settings.ExchangeSettings.ConnectHandlerSettings,
		connectserver.ConnectHandlerPacketConns{},
	)
	altServer, err := NewAlt(ctx, connectHandler, apiHandler, settings)
	if err != nil {
		t.Fatal(err)
	}
	return altServer
}

// Serves the alt's h3 listener on a loopback socket until the alt closes.
// Returns the address and the listener's completion.
func listenEnvFreeAltH3(t testing.TB, altServer *Alt) (string, chan struct{}) {
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { packetConn.Close() })
	listenerDone := make(chan struct{})
	go server.HandleError(func() {
		defer close(listenerDone)
		altServer.ListenH3(packetConn)
	})
	for altServer.ListenerReady() != nil {
		select {
		case <-listenerDone:
			t.Fatal("the h3 listener exited before it was up")
		case <-time.After(time.Millisecond):
		}
	}
	return packetConn.LocalAddr().String(), listenerDone
}

// An api client of one connection, with handshake room for a loaded machine's
// dropped loopback packets.
func newEnvFreeAltApiClient(t testing.TB, altServer *Alt) *http3.Transport {
	client := &http3.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    testFixtureRoots(t, altServer.transportTls, testApiHost),
			ServerName: testApiHost,
		},
		QUICConfig: &quic.Config{
			HandshakeIdleTimeout: testProgressTimeout,
			MaxIdleTimeout:       time.Minute,
		},
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func waitForAltTeardown(t testing.TB, dones ...chan struct{}) {
	t.Helper()
	for _, done := range dones {
		select {
		case <-done:
		case <-time.After(testProgressTimeout):
			t.Fatal("the teardown did not finish")
		}
	}
}

// Holds each server connection inside its close window. quic-go records a
// connection's close after it has closed the connection's streams and before
// it cancels the connection's context, so while the record is held every
// accept on the connection fails at once and its context is still live. In
// production the window stays open while Transport.close holds the transport
// mutex over its destroy fan-out and while every core is busy.
type altCloseWindowHold struct {
	held        atomic.Int64
	release     chan struct{}
	releaseOnce sync.Once
}

func newAltCloseWindowHold() *altCloseWindowHold {
	return &altCloseWindowHold{
		release: make(chan struct{}),
	}
}

// quic.Config.Tracer
func (self *altCloseWindowHold) Tracer(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
	return altCloseWindowTrace{hold: self}
}

func (self *altCloseWindowHold) Held() int {
	return int(self.held.Load())
}

func (self *altCloseWindowHold) Release() {
	self.releaseOnce.Do(func() {
		close(self.release)
	})
}

type altCloseWindowTrace struct {
	hold *altCloseWindowHold
}

func (self altCloseWindowTrace) AddProducer() qlogwriter.Recorder {
	return altCloseWindowRecorder{hold: self.hold}
}

// only the connection records here; http3 keeps its own events off this trace
func (self altCloseWindowTrace) SupportsSchemas(string) bool {
	return false
}

type altCloseWindowRecorder struct {
	hold *altCloseWindowHold
}

func (self altCloseWindowRecorder) RecordEvent(event qlogwriter.Event) {
	if _, ok := event.(qlog.ConnectionClosed); ok {
		self.hold.held.Add(1)
		<-self.hold.release
	}
}

func (self altCloseWindowRecorder) Close() error {
	return nil
}

// The old alt's exit burst (10-24 to 10-26). Alt's exit cancels its listeners,
// whose transports destroy every connection, while Close releases the api
// front. An http3 graceful shutdown at that moment turns each connection's
// accept loop into a spin for as long as the connection is closed but not yet
// done, and every pass starts a GOAWAY writer (quic-go v0.61.0
// http3/server.go:493-538). With every connection held in that window, the
// front may leave at most a few goroutines per connection: quic-go's destroy
// waits once per connection id.
func TestAltCloseStartsNoGoroutineBurstPerApiConnection(t *testing.T) {
	const (
		connectionCount         = 32
		goroutinesPerConnection = 16
		// how long every connection stays held before release
		holdDuration = 25 * time.Millisecond
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	altServer := newEnvFreeAlt(ctx, t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}), nil)
	hold := newAltCloseWindowHold()
	defer hold.Release()
	altServer.quicConfig.Tracer = hold.Tracer
	address, listenerDone := listenEnvFreeAltH3(t, altServer)

	// idle api connections, each after one request, a few dialing at a time
	const dialParallelism = 4
	connectErrs := make(chan error, connectionCount)
	dialSlots := make(chan struct{}, dialParallelism)
	var connected sync.WaitGroup
	for range connectionCount {
		client := newEnvFreeAltApiClient(t, altServer)
		connected.Add(1)
		go func() {
			defer connected.Done()
			dialSlots <- struct{}{}
			defer func() { <-dialSlots }()
			httpClient := &http.Client{
				Transport: client,
				Timeout:   testProgressTimeout,
			}
			response, err := httpClient.Get(fmt.Sprintf("https://%s/", address))
			if err != nil {
				connectErrs <- err
				return
			}
			defer response.Body.Close()
			io.Copy(io.Discard, response.Body)
		}()
	}
	connected.Wait()
	close(connectErrs)
	for err := range connectErrs {
		t.Fatal(err)
	}

	// alt's exit order: the drain's cancel stops every listener, whose
	// deferred transport close destroys its connections, and Run then closes
	// the fronts
	baseline := runtime.NumGoroutine()
	limit := baseline + goroutinesPerConnection*connectionCount
	cancel()
	closed := make(chan struct{})
	go server.HandleError(func() {
		defer close(closed)
		altServer.Close()
	})

	peak := baseline
	var allHeldTime time.Time
	endTime := time.Now().Add(testProgressTimeout)
	for {
		goroutines := runtime.NumGoroutine()
		peak = max(peak, goroutines)
		if limit < goroutines {
			// a burst: release at once rather than let it grow
			break
		}
		if hold.Held() == connectionCount {
			if allHeldTime.IsZero() {
				allHeldTime = time.Now()
			} else if holdDuration <= time.Since(allHeldTime) {
				break
			}
		}
		if endTime.Before(time.Now()) {
			t.Fatalf("%d of %d connections reached their close window", hold.Held(), connectionCount)
		}
		time.Sleep(100 * time.Microsecond)
	}
	hold.Release()
	waitForAltTeardown(t, closed, listenerDone)
	t.Logf(
		"the teardown of %d api connections peaked at +%.1f goroutines per connection",
		connectionCount,
		float64(peak-baseline)/float64(connectionCount),
	)
	if limit < peak {
		t.Fatalf(
			"the teardown of %d api connections peaked at %d goroutines over a baseline of %d (+%.1f per connection, at most %d)",
			connectionCount,
			peak,
			baseline,
			float64(peak-baseline)/float64(connectionCount),
			goroutinesPerConnection,
		)
	}
}

// Close still waits for the api requests in flight, as Shutdown did: it
// cancels each request with its connection and returns only after the
// handler has, so the api router's owners close after the last request.
func TestAltCloseWaitsForAnInFlightApiRequest(t *testing.T) {
	// what the handler still does once its request is canceled
	const handlerCleanup = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inFlight := make(chan struct{})
	handlerDone := make(chan struct{})
	altServer := newEnvFreeAlt(ctx, t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		close(inFlight)
		<-r.Context().Done()
		time.Sleep(handlerCleanup)
	}), nil)
	address, listenerDone := listenEnvFreeAltH3(t, altServer)
	httpClient := &http.Client{
		Transport: newEnvFreeAltApiClient(t, altServer),
		Timeout:   testProgressTimeout,
	}
	go server.HandleError(func() {
		if response, err := httpClient.Get(fmt.Sprintf("https://%s/", address)); err == nil {
			response.Body.Close()
		}
	})
	select {
	case <-inFlight:
	case <-time.After(testProgressTimeout):
		t.Fatal("the request did not reach its handler")
	}

	cancel()
	altServer.Close()
	select {
	case <-handlerDone:
	default:
		t.Fatal("Close returned while an api request's handler was still running")
	}
	waitForAltTeardown(t, listenerDone)
}

// A handler that never returns holds Close for the drain timeout and no
// longer: past it the exit goes on. Shutdown's fallback to Close had no bound.
func TestAltCloseBoundsAStuckApiRequestByTheDrainTimeout(t *testing.T) {
	const drainTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inFlight := make(chan struct{})
	stuck := make(chan struct{})
	var unstickOnce sync.Once
	unstick := func() { unstickOnce.Do(func() { close(stuck) }) }
	defer unstick()
	altServer := newEnvFreeAlt(ctx, t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inFlight)
		// ignores its canceled request
		<-stuck
	}), func(settings *Settings) {
		settings.DrainTimeout = drainTimeout
	})
	address, listenerDone := listenEnvFreeAltH3(t, altServer)
	httpClient := &http.Client{
		Transport: newEnvFreeAltApiClient(t, altServer),
		Timeout:   testProgressTimeout,
	}
	go server.HandleError(func() {
		if response, err := httpClient.Get(fmt.Sprintf("https://%s/", address)); err == nil {
			response.Body.Close()
		}
	})
	select {
	case <-inFlight:
	case <-time.After(testProgressTimeout):
		t.Fatal("the request did not reach its handler")
	}

	cancel()
	closed := make(chan struct{})
	go server.HandleError(func() {
		defer close(closed)
		altServer.Close()
	})
	select {
	case <-closed:
	case <-time.After(50 * drainTimeout):
		t.Fatalf("Close waited over %s for a stuck api request, past its %s drain timeout", 50*drainTimeout, drainTimeout)
	}
	unstick()
	waitForAltTeardown(t, listenerDone)
}
