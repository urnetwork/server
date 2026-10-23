package connect

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Drain gates (MOVE.md §2.3, §5.6): a draining exchange admits no new client
// transport on any carrier, refuses an inbound exchange handshake for a
// resident that is not here instead of holding it, and converges on the
// client transports and residents on this process, not on the inbound
// exchange connections that route into those residents.

// An exchange serving its internal port on a loopback listener, as the peer
// discovery env builds one, without a connect handler.
func newDrainGateTestExchange(ctx context.Context, t testing.TB, settings *ExchangeSettings) (*Exchange, int) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("exchange listener: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	exchange := NewExchangeWithListeners(
		ctx,
		"host0",
		"connect",
		"test",
		map[int]int{port: port},
		map[string]string{"host0": "127.0.0.1"},
		settings,
		map[int]net.Listener{port: listener},
	)
	return exchange, port
}

func newDrainGateTestExchangeSettings() *ExchangeSettings {
	settings := DefaultExchangeSettings()
	settings.KeyEventDelivery.Enabled = false
	settings.EnableDrainCoordination = true
	// a handshake for a missing resident would otherwise wait this long
	settings.ExchangeResidentWaitTimeout = 2 * time.Minute
	settings.ExchangeResidentPollTimeout = 2 * time.Minute
	return settings
}

// Every QUIC carrier (H3, DNS-carried H3 and the alt front) shares connectQuic.
// While the exchange drains, a new connection is closed before the handshake
// wait and before any admission charge, as the H1 handler answers 503.
//
// Without the gate the connection is admitted: it is charged and then waits
// for its auth frame until the auth deadline.
func TestConnectQuicRefusesWhileDraining(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		var serving sync.WaitGroup
		defer serving.Wait()
		defer cancel()

		exchangeSettings := newDrainGateTestExchangeSettings()
		exchange := NewExchange(ctx, "host0", "connect", "test", map[int]int{}, map[string]string{}, exchangeSettings)
		defer exchange.Close()
		// nothing to shed: the drain only closes the gates
		exchange.Drain()

		settings := DefaultConnectHandlerSettings()
		// bounds a connection the gate fails to refuse
		settings.ReadTimeout = 2 * time.Second
		handler := &ConnectHandler{ctx: ctx, handlerId: server.NewId(), exchange: exchange, settings: settings}
		listener, clientTlsConfig := listenConnectQuicTestEarly(t, settings)

		refusedBefore := testutil.ToFloat64(drainRefusalsCounter.WithLabelValues(drainRefusalCarrierQuic))
		source := newConnectQuicTestSource(t, deliverConnectQuicTestAll)
		conn, dialed := acceptConnectQuicTestSource(ctx, t, listener, clientTlsConfig, source)
		served := make(chan error, 1)
		serving.Go(func() {
			served <- handler.connectQuic(conn)
		})
		if err := waitForConnectQuicTestServed(ctx, t, served); !errors.Is(err, errConnectQuicDraining) {
			t.Fatalf("connectQuic while draining = %v, want %v", err, errConnectQuicDraining)
		}
		if refused := testutil.ToFloat64(drainRefusalsCounter.WithLabelValues(drainRefusalCarrierQuic)); refused != refusedBefore+1 {
			t.Fatalf("quic drain refusals = %v, want %v", refused, refusedBefore+1)
		}
		// the client's connection ends, whether before or after its handshake
		select {
		case result := <-dialed:
			if result.err == nil {
				defer result.conn.CloseWithError(0, "")
				select {
				case <-result.conn.Context().Done():
				case <-ctx.Done():
					t.Fatal("the refused connection stayed open at the client")
				}
			}
		case <-ctx.Done():
			t.Fatal("the client dial did not end")
		}
		if charges := connectQuicTestCharges(ctx, t, "127.0.0.1"); len(charges) != 0 {
			t.Fatalf("a refused connection was charged %v", charges)
		}
	})
}

// A draining exchange nominates no resident, so an inbound exchange handshake
// for a resident that is not here is refused at once instead of holding its
// socket for the resident wait. Nothing is counted toward the drain.
//
// Without the gate the handshake waits for the resident poll, so the dialer
// reads no answer until its header deadline.
func TestExchangeRefusesMissingResidentHandshakeWhileDraining(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	settings := newDrainGateTestExchangeSettings()
	exchange, port := newDrainGateTestExchange(ctx, t, settings)
	defer exchange.Close()
	// the gate alone, without a sweep that would cancel a held handshake
	exchange.draining.Store(true)

	dialSettings := newDrainGateTestExchangeSettings()
	dialSettings.ExchangeReadHeaderTimeout = 30 * time.Second
	refusedBefore := testutil.ToFloat64(drainRefusalsCounter.WithLabelValues(drainRefusalCarrierExchange))
	header := ExchangeHeader{
		ClientId:   server.NewId(),
		ResidentId: server.NewId(),
		Op:         ExchangeOpForward,
	}
	startTime := time.Now()
	connection, err := NewExchangeConnection(ctx, header, "host0", port, map[string]string{"host0": "127.0.0.1"}, dialSettings)
	if err == nil {
		connection.Close()
		t.Fatal("a draining exchange echoed a handshake for a missing resident")
	}
	if refusedAfter := time.Since(startTime); 10*time.Second < refusedAfter {
		t.Fatalf("the refusal took %s; the handshake waited for the resident", refusedAfter)
	}
	if refused := testutil.ToFloat64(drainRefusalsCounter.WithLabelValues(drainRefusalCarrierExchange)); refused != refusedBefore+1 {
		t.Fatalf("exchange drain refusals = %v, want %v", refused, refusedBefore+1)
	}
	func() {
		exchange.stateLock.Lock()
		defer exchange.stateLock.Unlock()
		connect.AssertEqual(t, 0, exchange.drainClientCountLocked())
		connect.AssertEqual(t, 0, len(exchange.residentChanges))
	}()
}

// A handshake already waiting for a missing resident when the drain starts is
// woken and refused, so the drain neither waits for it nor keeps its socket.
func TestExchangeDrainWakesWaitingHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	settings := newDrainGateTestExchangeSettings()
	settings.DrainAllTimeout = 30 * time.Second
	settings.DrainStragglerSweepTimeout = 100 * time.Millisecond
	settings.DrainOneTimeout = 10 * time.Millisecond
	exchange, port := newDrainGateTestExchange(ctx, t, settings)
	defer exchange.Close()

	waiting := make(chan struct{})
	var waitingOnce sync.Once
	exchange.afterResidentMissingForTest = func() {
		waitingOnce.Do(func() { close(waiting) })
	}

	dialSettings := newDrainGateTestExchangeSettings()
	dialSettings.ExchangeReadHeaderTimeout = time.Minute
	header := ExchangeHeader{
		ClientId:   server.NewId(),
		ResidentId: server.NewId(),
		Op:         ExchangeOpForward,
	}
	dialed := make(chan error, 1)
	go func() {
		connection, err := NewExchangeConnection(ctx, header, "host0", port, map[string]string{"host0": "127.0.0.1"}, dialSettings)
		if err == nil {
			connection.Close()
		}
		dialed <- err
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("the handshake did not reach the resident wait")
	}

	drainStartTime := time.Now()
	exchange.Drain()
	if drainDuration := time.Since(drainStartTime); 10*time.Second < drainDuration {
		t.Fatalf("the drain took %s", drainDuration)
	}
	select {
	case err := <-dialed:
		if err == nil {
			t.Fatal("the waiting handshake was answered after the drain started")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting handshake was not woken by the drain")
	}
}

// Inbound exchange handshakes for a resident that is not here keep arriving
// while the exchange drains (a dialer re-reads a stale resident record and
// redials). The drain converges on its own client transports and residents
// and finishes without waiting for them.
//
// Before the gate each handshake registered as one of the drain's connections
// and waited for the resident, so the sweep found a new one after each
// eviction and ran to the drain deadline.
func TestExchangeDrainConvergesWithStragglerHandshakes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		drainAllTimeout := 20 * time.Second
		settings := newDrainGateTestExchangeSettings()
		settings.DrainAllTimeout = drainAllTimeout
		settings.DrainStragglerSweepTimeout = 100 * time.Millisecond
		settings.DrainOneTimeout = 10 * time.Millisecond
		exchange, port := newDrainGateTestExchange(ctx, t, settings)
		defer exchange.Close()

		// several dialers, each redialing at once after its handshake ends
		stragglerCtx, stragglerCancel := context.WithCancel(ctx)
		var stragglers sync.WaitGroup
		defer stragglers.Wait()
		defer stragglerCancel()
		dialSettings := newDrainGateTestExchangeSettings()
		dialSettings.ExchangeReadHeaderTimeout = time.Minute
		waiting := make(chan struct{})
		var waitingOnce sync.Once
		exchange.afterResidentMissingForTest = func() {
			waitingOnce.Do(func() { close(waiting) })
		}
		for range 8 {
			header := ExchangeHeader{
				ClientId:   server.NewId(),
				ResidentId: server.NewId(),
				Op:         ExchangeOpForward,
			}
			stragglers.Go(func() {
				for {
					select {
					case <-stragglerCtx.Done():
						return
					default:
					}
					connection, err := NewExchangeConnection(stragglerCtx, header, "host0", port, map[string]string{"host0": "127.0.0.1"}, dialSettings)
					if err == nil {
						connection.Close()
					}
				}
			})
		}
		select {
		case <-waiting:
		case <-ctx.Done():
			t.Fatal("no straggler reached the resident wait")
		}

		drainStartTime := time.Now()
		exchange.Drain()
		drainDuration := time.Since(drainStartTime)
		if drainAllTimeout/2 < drainDuration {
			t.Fatalf("the drain took %s, held by straggler handshakes", drainDuration)
		}
	})
}
