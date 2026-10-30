// Exchange writers pace keepalive pings from their last write instead of
// re-arming the ping timer on every frame.
package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"google.golang.org/protobuf/proto"
)

// One end of a connection whose writes are counted as exchange frames, with
// the time of each write.
type exchangeWriteTimesConn struct {
	net.Conn
	counter    *exchangeFrameCounter
	mutex      sync.Mutex
	writeTimes []time.Time
}

func (self *exchangeWriteTimesConn) Write(b []byte) (int, error) {
	n, err := self.Conn.Write(b)
	self.counter.observe(b[:n])
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		self.writeTimes = append(self.writeTimes, time.Now())
	}()
	return n, err
}

// The longest wait between consecutive writes, and from the last write to end.
func (self *exchangeWriteTimesConn) longestGap(end time.Time) time.Duration {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	gap := time.Duration(0)
	for i := 1; i < len(self.writeTimes); i += 1 {
		gap = max(gap, self.writeTimes[i].Sub(self.writeTimes[i-1]))
	}
	if 0 < len(self.writeTimes) {
		gap = max(gap, end.Sub(self.writeTimes[len(self.writeTimes)-1]))
	}
	return gap
}

// An OpForward into a destination resident whose client transport is a real
// accepted OpTransport exchange. A forwarded frame crosses the dialing
// forward's socket writer and the accepting transport's socket writer, which
// each count the ping timers they arm. The client edge reads every frame and
// pings within the accepted read deadline.
type exchangePingTimerFixture struct {
	forward       *ResidentForward
	destination   *Resident
	forwardSent   *exchangeWriteTimesConn
	transportSent *exchangeWriteTimesConn
	delivered     chan []byte
	forwardArms   atomic.Int64
	transportArms atomic.Int64
}

func newExchangePingTimerFixture(t *testing.T) *exchangePingTimerFixture {
	t.Helper()
	f := &exchangePingTimerFixture{
		forwardSent:   &exchangeWriteTimesConn{counter: &exchangeFrameCounter{}},
		transportSent: &exchangeWriteTimesConn{counter: &exchangeFrameCounter{}},
		delivered:     make(chan []byte),
	}
	dialSettings := exchangePingFixtureSettings()
	dialSettings.afterPingTimerArmForTest = func() { f.forwardArms.Add(1) }
	acceptSettings := exchangePingFixtureSettings()
	acceptSettings.afterPingTimerArmForTest = func() { f.transportArms.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	destination := newResidentCallbackLifecycleFixture(t, ctx, acceptSettings)
	destination.residentId = server.NewId()
	destination.lastActivityNanos.Store(time.Now().UnixNano())
	exchange := destination.exchange
	exchange.ctx, exchange.cancel = ctx, cancel
	exchange.residents = map[server.Id]*Resident{destination.clientId: destination}
	exchange.connections = map[server.Id]map[server.Id]context.CancelFunc{}
	f.destination = destination

	edge, accepted := net.Pipe()
	f.transportSent.Conn = accepted
	transportDone := make(chan struct{})
	go func() {
		defer close(transportDone)
		exchange.handleExchangeConnection(f.transportSent)
	}()
	edgeSend := NewDefaultExchangeBuffer(acceptSettings)
	edgeReceive := NewReceiveOnlyExchangeBuffer(acceptSettings)
	header := &ExchangeHeader{Version: 1, ClientId: destination.clientId, ResidentId: destination.residentId, Op: ExchangeOpTransport}
	if err := edgeSend.WriteHeader(ctx, edge, header); err != nil {
		t.Fatal(err)
	}
	if _, err := edgeReceive.ReadHeader(ctx, edge); err != nil {
		t.Fatal(err)
	}
	edgeReadDone := make(chan struct{})
	go func() {
		defer close(edgeReadDone)
		for {
			message, err := edgeReceive.ReadMessage(edge)
			if err != nil {
				return
			}
			if len(message) == 0 {
				clientconnect.MessagePoolReturn(message)
				continue
			}
			select {
			case f.delivered <- message:
			case <-ctx.Done():
				clientconnect.MessagePoolReturn(message)
				return
			}
		}
	}()
	edgePingDone := make(chan struct{})
	go func() {
		defer close(edgePingDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(acceptSettings.ExchangePingTimeout):
			}
			if err := edgeSend.WriteMessage(edge, make([]byte, 0)); err != nil {
				return
			}
		}
	}()

	var dials atomic.Int64
	forwardSocketDone := make(chan struct{})
	dialSettings.DialContext = func(context.Context, string, string) (net.Conn, error) {
		if dials.Add(1) != 1 {
			return nil, errors.New("ping timer fixture redial")
		}
		local, remote := net.Pipe()
		go func() {
			defer close(forwardSocketDone)
			exchange.handleExchangeConnection(remote)
		}()
		f.forwardSent.Conn = local
		return f.forwardSent, nil
	}
	f.forward = NewResidentForward(ctx, &Exchange{settings: dialSettings}, destination.clientId)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		f.forward.runWithResidentLookup(func(context.Context, server.Id, time.Duration) *model.NetworkClientResident {
			return &model.NetworkClientResident{ResidentId: destination.residentId, ResidentHost: "ping-timer-fixture.invalid", ResidentInternalPorts: []int{1}}
		})
	}()
	t.Cleanup(func() {
		f.forward.Cancel()
		cancel()
		edge.Close()
		<-runDone
		if 0 < dials.Load() {
			<-forwardSocketDone
		}
		<-transportDone
		<-edgeReadDone
		<-edgePingDone
		if err := destination.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	// the accepted transport has published its route
	synctest.Wait()
	return f
}

// Forwards one payload to the destination's client and waits until the edge
// receives it through both exchange writers.
func (self *exchangePingTimerFixture) deliver(t *testing.T, marker string) {
	t.Helper()
	body, err := proto.Marshal(&protocol.TransferFrame{TransferPath: clientconnect.NewTransferPath(clientconnect.NewId(), clientconnect.Id(self.destination.clientId), clientconnect.Id{}).ToProtobuf(), Pack: &protocol.Pack{MessageId: []byte(marker)}})
	if err != nil {
		t.Fatal(err)
	}
	if !self.forward.UpdateActivity() {
		t.Fatal("live forward refused payload activity")
	}
	self.forward.send <- clientconnect.MessagePoolCopy(body)
	select {
	case got := <-self.delivered:
		if !bytes.Equal(got, body) {
			t.Error("framed payload changed")
		}
		clientconnect.MessagePoolReturn(got)
	case <-time.After(5 * time.Second):
		t.Fatal("framed payload was not delivered")
	}
}

// While frames flow, neither exchange writer re-arms its ping timer per frame:
// the timer is armed only when it fires, about once per ExchangePingTimeout.
// Once idle, each writer pings every ExchangePingTimeout, so its peer never
// waits longer than that between frames.
func TestExchangeWritersArmTheirPingTimerOnlyWhenItFires(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		f := newExchangePingTimerFixture(t)
		pingTimeout := exchangePingFixtureSettings().ExchangePingTimeout
		forwardArms, transportArms := f.forwardArms.Load(), f.transportArms.Load()
		start := time.Now()
		const frameCount = 100
		for i := range frameCount {
			f.deliver(t, fmt.Sprintf("frame %d", i))
			time.Sleep(time.Second)
		}
		busy := time.Since(start)
		forwardArms, transportArms = f.forwardArms.Load()-forwardArms, f.transportArms.Load()-transportArms
		forwardPings, transportPings := f.forwardSent.counter.pingCount(), f.transportSent.counter.pingCount()
		t.Logf("busy=%s frames=%d forward_arms=%d transport_arms=%d forward_pings=%d transport_pings=%d", busy, frameCount, forwardArms, transportArms, forwardPings, transportPings)
		// a fire about once per interval, against an arm per frame before
		maxArms := 2*int64(busy/pingTimeout) + 2
		if maxArms < forwardArms || maxArms < transportArms {
			t.Fatalf("%d frames armed the forward ping timer %d times and the transport's %d times, want at most %d", frameCount, forwardArms, transportArms, maxArms)
		}
		if forwardPings != 0 || transportPings != 0 {
			t.Fatalf("writers busy every second pinged: forward=%d transport=%d", forwardPings, transportPings)
		}

		idle := time.Minute
		time.Sleep(idle)
		synctest.Wait()
		end := time.Now()
		expected := int(idle / pingTimeout)
		for _, side := range []struct {
			name string
			sent *exchangeWriteTimesConn
		}{{"forward", f.forwardSent}, {"transport", f.transportSent}} {
			pings := side.sent.counter.pingCount()
			gap := side.sent.longestGap(end)
			t.Logf("%s idle_pings=%d longest_gap=%s", side.name, pings, gap)
			if pings < expected-1 || expected+1 < pings {
				t.Errorf("%s writer sent %d pings in %s idle, want %d±1", side.name, pings, idle, expected)
			}
			if pingTimeout < gap {
				t.Errorf("%s writer left its peer %s between frames, longer than the %s ping interval", side.name, gap, pingTimeout)
			}
		}
	})
}
