package connect

import (
	"bytes"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"google.golang.org/protobuf/proto"
)

type idleForwardFixture struct {
	forward                       *ResidentForward
	destination                   *Resident
	send                          chan []byte
	remove                        func()
	runDone, idleDone, socketDone <-chan struct{}
	dials                         *atomic.Int64
}

// Real resident transport removal, the real OpForward header/socket pumps,
// real pooled payload routing, and the production idle owner run under a
// deterministic clock. Only Redis discovery and the TCP carrier are injected.
func newIdleForwardFixture(t *testing.T, destinationId ...server.Id) *idleForwardFixture {
	t.Helper()
	settings := DefaultExchangeSettingsWithBufferSize(4)
	settings.ForwardBufferSize = 4
	settings.ExchangePingTimeout = time.Second
	settings.ExchangeReadTimeout = 30 * time.Second
	settings.ForwardIdleTimeout = 15 * time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	destination := newResidentCallbackLifecycleFixture(t, ctx, settings)
	if len(destinationId) > 0 {
		destination.clientId = destinationId[0]
	}
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
	var dials atomic.Int64
	socketDone := make(chan struct{})
	settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
		if dials.Add(1) != 1 {
			t.Error("idle fixture unexpectedly redialed")
		}
		local, remote := net.Pipe()
		go func() { defer close(socketDone); exchange.handleExchangeConnection(remote) }()
		return local, nil
	}
	forward := NewResidentForward(ctx, &Exchange{settings: settings}, destination.clientId)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		forward.runWithResidentLookup(func(context.Context, server.Id, time.Duration) *model.NetworkClientResident {
			return &model.NetworkClientResident{ResidentId: destination.residentId, ResidentHost: "idle-fixture.invalid", ResidentInternalPorts: []int{1}}
		})
	}()
	idleDone := make(chan struct{})
	go func() { defer close(idleDone); forward.runIdleWatcher(server.NewId()) }()
	t.Cleanup(func() {
		forward.Cancel()
		cancel()
		remove()
		<-runDone
		<-idleDone
		if dials.Load() > 0 {
			<-socketDone
		}
		if err := destination.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return &idleForwardFixture{forward, destination, send, remove, runDone, idleDone, socketDone, &dials}
}
func (f *idleForwardFixture) deliver(t *testing.T, marker string) {
	t.Helper()
	body, err := proto.Marshal(&protocol.TransferFrame{TransferPath: clientconnect.NewTransferPath(clientconnect.NewId(), clientconnect.Id(f.destination.clientId), clientconnect.Id{}).ToProtobuf(), Pack: &protocol.Pack{MessageId: []byte(marker)}})
	if err != nil {
		t.Fatal(err)
	}
	message := clientconnect.MessagePoolCopy(body)
	witness := retainResidentPoolWitness(message)
	if !f.forward.UpdateActivity() {
		t.Fatal("live forward refused payload activity")
	}
	f.forward.send <- message
	select {
	case got := <-f.send:
		if !bytes.Equal(got, body) {
			t.Error("framed payload changed")
		}
		clientconnect.MessagePoolReturn(got)
	case <-time.After(5 * time.Second):
		t.Fatal("framed payload was not delivered")
	}
	synctest.Wait()
	requireResidentPoolOwnerReturned(t, witness, "live idle forward payload")
}

// A short-lived client's transport is gone, yet OpForward pings keep its
// resident alive. Expiry must occur at the existing last-payload+15m boundary,
// not the next 15m sweep. Both endpoint owners must close and pings must stop.
func TestResidentForwardIdleExactAfterClientTransportRemoval(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		f := newIdleForwardFixture(t)
		f.deliver(t, "initial")
		time.Sleep(time.Second)
		f.deliver(t, "final")
		removedAt := time.Now()
		f.remove()
		if f.destination.TransportCount() != 0 {
			t.Fatal("client transport removal did not complete")
		}
		pingBefore := snapshotExchangeIO("sent", "ping").frames
		time.Sleep(6 * time.Minute)
		if f.destination.CancelIfIdle() {
			t.Fatal("baseline mechanism missing: inbound pings did not retain disconnected resident")
		}
		if snapshotExchangeIO("sent", "ping").frames <= pingBefore {
			t.Fatal("removed client did not retain live OpForward pings")
		}
		time.Sleep(9*time.Minute - time.Nanosecond)
		if f.forward.IsDone() {
			t.Fatal("forward expired before last payload idle deadline")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		closed := f.forward.IsDone()
		t.Logf("transport_removed=true idle_elapsed_seconds=%.9f forward_closed=%t sent_ping_delta=%.0f", time.Since(removedAt).Seconds(), closed, snapshotExchangeIO("sent", "ping").frames-pingBefore)
		if !closed {
			t.Fatal("forward retained live pings beyond the existing 15-minute last-payload deadline")
		}
		<-f.runDone
		<-f.idleDone
		<-f.socketDone
		after := snapshotExchangeIO("sent", "ping").frames
		time.Sleep(5*time.Minute + time.Second)
		if snapshotExchangeIO("sent", "ping").frames != after {
			t.Fatal("closed forward continued sending pings")
		}
		if !f.destination.CancelIfIdle() {
			t.Fatal("disconnected resident stayed active after forward pings stopped")
		}
	})
}

// Work before expiry moves the deadline; healthy connected peers and every
// admitted payload remain usable. Close is tested only after the new allowance.
func TestResidentForwardIdleActivePeerExtendsDeadline(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		f := newIdleForwardFixture(t)
		f.deliver(t, "initial")
		time.Sleep(14 * time.Minute)
		f.deliver(t, "late-1")
		f.deliver(t, "late-2")
		time.Sleep(time.Minute)
		if f.forward.IsDone() || f.destination.TransportCount() != 1 {
			t.Fatal("active peer was retired at its old deadline")
		}
		f.deliver(t, "after-old-deadline")
		time.Sleep(15*time.Minute - time.Nanosecond)
		if f.forward.IsDone() {
			t.Fatal("active payload deadline was shortened")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !f.forward.IsDone() {
			t.Fatal("idle forward did not close at updated payload deadline")
		}
		if f.destination.TransportCount() != 1 || f.destination.IsDone() {
			t.Fatal("forward expiry canceled an active destination peer")
		}
	})
}

// Queue actual framed packets across the old sweep boundary. Each admitted
// payload keeps its FIFO position and pool owner; the new deadline cannot
// shorten the configured inactivity allowance or cancel the attached peer.
func TestResidentForwardIdleQueuedTrafficCrossesOldDeadline(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		f := newIdleForwardFixture(t)
		f.deliver(t, "initial")
		time.Sleep(15*time.Minute - time.Second)
		var bodies, witnesses [][]byte
		for _, marker := range []string{"queued-one", "queued-two", "queued-three"} {
			body, err := proto.Marshal(&protocol.TransferFrame{TransferPath: clientconnect.NewTransferPath(clientconnect.NewId(), clientconnect.Id(f.destination.clientId), clientconnect.Id{}).ToProtobuf(), Pack: &protocol.Pack{MessageId: []byte(marker)}})
			if err != nil {
				t.Fatal(err)
			}
			message := clientconnect.MessagePoolCopy(body)
			bodies = append(bodies, body)
			witnesses = append(witnesses, retainResidentPoolWitness(message))
			if !f.forward.UpdateActivity() {
				t.Fatal("near-deadline work refused")
			}
			f.forward.send <- message
		}
		time.Sleep(2 * time.Second)
		if f.forward.IsDone() {
			t.Fatal("queued traffic was canceled at the old sweep deadline")
		}
		for i, body := range bodies {
			select {
			case got := <-f.send:
				if !bytes.Equal(got, body) {
					t.Errorf("queued packet %d lost FIFO or payload", i)
				}
				clientconnect.MessagePoolReturn(got)
			case <-time.After(5 * time.Second):
				t.Fatalf("queued packet %d lost", i)
			}
		}
		synctest.Wait()
		requireResidentPoolOwnersReturned(t, witnesses, "queued traffic across old deadline")
		if f.destination.TransportCount() != 1 || f.destination.IsDone() {
			t.Fatal("healthy active destination was canceled")
		}
	})
}
