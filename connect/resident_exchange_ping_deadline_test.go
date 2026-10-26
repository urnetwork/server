// Exchange keepalive cadence and socket deadline re-arming.
package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Counts the exchange frames written through one end of a connection: a
// 4-byte header (big-endian uint16 payload length, uint16 split index) and the
// payload. A ping is a zero-length frame.
type exchangeFrameCounter struct {
	mutex      sync.Mutex
	header     [exchangeIOFrameHeaderByteCount]byte
	headerFill int
	remaining  int
	frames     int
	pings      int
}

// Advances the frame parse over the next written bytes.
func (self *exchangeFrameCounter) observe(written []byte) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for 0 < len(written) {
		if 0 < self.remaining {
			n := min(self.remaining, len(written))
			self.remaining -= n
			written = written[n:]
			continue
		}
		n := copy(self.header[self.headerFill:], written)
		self.headerFill += n
		written = written[n:]
		if self.headerFill < len(self.header) {
			return
		}
		self.headerFill = 0
		self.frames += 1
		self.remaining = int(binary.BigEndian.Uint16(self.header[0:2]))
		if self.remaining == 0 {
			self.pings += 1
		}
	}
}

// The pings written so far.
func (self *exchangeFrameCounter) pingCount() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.pings
}

// One end of a connection whose writes are counted as exchange frames.
type exchangeFrameCountingConn struct {
	net.Conn
	counter *exchangeFrameCounter
}

func (self *exchangeFrameCountingConn) Write(b []byte) (int, error) {
	n, err := self.Conn.Write(b)
	self.counter.observe(b[:n])
	return n, err
}

// A real OpForward between a dialing forward and an accepting destination,
// each with its own exchange settings, as during a rolling deploy. The pipe
// ends count the frames each side writes.
type exchangePingFixture struct {
	forward     *ResidentForward
	destination *Resident
	send        chan []byte
	dialSent    *exchangeFrameCounter
	acceptSent  *exchangeFrameCounter
	dials       *atomic.Int64
	socketDone  chan struct{}
}

func newExchangePingFixture(t *testing.T, dialSettings *ExchangeSettings, acceptSettings *ExchangeSettings) *exchangePingFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	destination := newResidentCallbackLifecycleFixture(t, ctx, acceptSettings)
	destination.residentId = server.NewId()
	destination.lastActivityNanos.Store(time.Now().UnixNano())
	exchange := destination.exchange
	exchange.ctx, exchange.cancel = ctx, cancel
	exchange.residents = map[server.Id]*Resident{destination.clientId: destination}
	exchange.connections = map[server.Id]map[server.Id]context.CancelFunc{}
	send, _, remove, err := destination.AddTransport()
	if err != nil {
		t.Fatal(err)
	}
	f := &exchangePingFixture{
		destination: destination,
		send:        send,
		dialSent:    &exchangeFrameCounter{},
		acceptSent:  &exchangeFrameCounter{},
		dials:       &atomic.Int64{},
		socketDone:  make(chan struct{}),
	}
	dialSettings.DialContext = func(context.Context, string, string) (net.Conn, error) {
		if f.dials.Add(1) != 1 {
			return nil, errors.New("ping fixture redial")
		}
		local, remote := net.Pipe()
		go func() {
			defer close(f.socketDone)
			exchange.handleExchangeConnection(&exchangeFrameCountingConn{Conn: remote, counter: f.acceptSent})
		}()
		return &exchangeFrameCountingConn{Conn: local, counter: f.dialSent}, nil
	}
	f.forward = NewResidentForward(ctx, &Exchange{settings: dialSettings}, destination.clientId)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		f.forward.runWithResidentLookup(func(context.Context, server.Id, time.Duration) *model.NetworkClientResident {
			return &model.NetworkClientResident{ResidentId: destination.residentId, ResidentHost: "ping-fixture.invalid", ResidentInternalPorts: []int{1}}
		})
	}()
	t.Cleanup(func() {
		f.forward.Cancel()
		cancel()
		remove()
		<-runDone
		if 0 < f.dials.Load() {
			<-f.socketDone
		}
		if err := destination.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return f
}

// Opens the forward with one payload, so both pumps are running.
func (self *exchangePingFixture) open(t *testing.T) {
	t.Helper()
	deliverer := &idleForwardFixture{forward: self.forward, destination: self.destination, send: self.send}
	deliverer.deliver(t, "open")
}

// Whether the forward is still on its first connection.
func (self *exchangePingFixture) connected() bool {
	select {
	case <-self.socketDone:
		return false
	default:
		return self.dials.Load() == 1 && !self.forward.IsDone()
	}
}

// Production exchange settings with the queues the forward fixtures use.
func exchangePingFixtureSettings() *ExchangeSettings {
	settings := DefaultExchangeSettingsWithBufferSize(4)
	settings.ForwardBufferSize = 4
	settings.ForwardIdleTimeout = 15 * time.Minute
	return settings
}

// An exchange ping only keeps the peer's read deadline and the resident's
// activity TTL from lapsing on an idle connection. It used to be the clients'
// 1 s minimum ping, which made pings three quarters of all exchange frames.
func TestDefaultExchangePingIsAThirdOfTheReadDeadline(t *testing.T) {
	settings := DefaultExchangeSettings()
	if settings.ExchangeReadTimeout != 30*time.Second {
		t.Fatalf("exchange read deadline changed: %s", settings.ExchangeReadTimeout)
	}
	if settings.ExchangePingTimeout != settings.ExchangeReadTimeout/3 {
		t.Fatalf("exchange ping %s is not a third of the %s read deadline", settings.ExchangePingTimeout, settings.ExchangeReadTimeout)
	}
	if settings.ExchangeResidentTtl < 3*settings.ExchangePingTimeout {
		t.Fatalf("pings every %s cannot keep a %s resident TTL", settings.ExchangePingTimeout, settings.ExchangeResidentTtl)
	}
}

// With production settings on both sides, an idle forward is kept open by one
// ping per ExchangeReadTimeout/3 in each direction, not one a second.
func TestExchangeForwardIdlePingsAtTheDefaultCadence(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		f := newExchangePingFixture(t, exchangePingFixtureSettings(), exchangePingFixtureSettings())
		f.open(t)
		dialBefore, acceptBefore := f.dialSent.pingCount(), f.acceptSent.pingCount()
		idle := 2 * time.Minute
		time.Sleep(idle)
		synctest.Wait()
		dialPings, acceptPings := f.dialSent.pingCount()-dialBefore, f.acceptSent.pingCount()-acceptBefore
		t.Logf("idle=%s dial_pings=%d accept_pings=%d", idle, dialPings, acceptPings)
		expected := int(idle / (10 * time.Second))
		for _, side := range []struct {
			name  string
			pings int
		}{{"dialing", dialPings}, {"accepting", acceptPings}} {
			if side.pings < expected-1 || expected+1 < side.pings {
				t.Errorf("%s side sent %d pings in %s; want %d±1 at a 10 s cadence", side.name, side.pings, idle, expected)
			}
		}
		if !f.connected() {
			t.Fatal("idle forward lost its connection at the default cadence")
		}
	})
}

// During a rolling deploy, a peer that pings every second and a peer that
// pings every ten seconds keep each other's 30 s read deadlines alive in both
// roles. A peer that pings slower than its peer's read deadline is cut off,
// which shows the deadline is what the cadence must beat.
func TestExchangeForwardMixedVersionPingsKeepTheConnection(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	withPing := func(ping time.Duration) *ExchangeSettings {
		settings := exchangePingFixtureSettings()
		settings.ExchangePingTimeout = ping
		return settings
	}
	cases := []struct {
		name               string
		dialPing           time.Duration
		acceptPing         time.Duration
		expectConnected    bool
		expectedDialPings  int
		expectedAcceptPing int
	}{
		{name: "old dialing new accepting", dialPing: time.Second, acceptPing: 10 * time.Second, expectConnected: true, expectedDialPings: 180, expectedAcceptPing: 18},
		{name: "new dialing old accepting", dialPing: 10 * time.Second, acceptPing: time.Second, expectConnected: true, expectedDialPings: 18, expectedAcceptPing: 180},
		{name: "accepting slower than the read deadline", dialPing: 10 * time.Second, acceptPing: 40 * time.Second, expectConnected: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newExchangePingFixture(t, withPing(c.dialPing), withPing(c.acceptPing))
				f.open(t)
				dialBefore, acceptBefore := f.dialSent.pingCount(), f.acceptSent.pingCount()
				time.Sleep(3 * time.Minute)
				synctest.Wait()
				dialPings, acceptPings := f.dialSent.pingCount()-dialBefore, f.acceptSent.pingCount()-acceptBefore
				t.Logf("dial_ping=%s accept_ping=%s dial_pings=%d accept_pings=%d connected=%t", c.dialPing, c.acceptPing, dialPings, acceptPings, f.connected())
				if f.connected() != c.expectConnected {
					t.Fatalf("connected=%t, want %t", f.connected(), c.expectConnected)
				}
				if !c.expectConnected {
					return
				}
				if dialPings < c.expectedDialPings-1 || c.expectedDialPings+1 < dialPings {
					t.Errorf("dialing side sent %d pings, want %d±1", dialPings, c.expectedDialPings)
				}
				if acceptPings < c.expectedAcceptPing-1 || c.expectedAcceptPing+1 < acceptPings {
					t.Errorf("accepting side sent %d pings, want %d±1", acceptPings, c.expectedAcceptPing)
				}
			})
		})
	}
}
