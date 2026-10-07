package connect

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Both directions use real Transfer clients, real resident callback shards,
// the OpForward header and socket pumps, and cumulative ACKs. Accepted work
// near the old idle deadline must keep the same forward generation usable.
func TestResidentForwardAcceptedTrafficRenewsDeadlineWithNativeACKs(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultExchangeSettings()
		settings.ForwardEnforceActiveContracts = false
		settings.ExchangePingTimeout = time.Minute
		settings.ExchangeReadTimeout = 3 * time.Minute
		var ledger residentPayloadLedger
		exchange := &Exchange{ctx: ctx, cancel: cancel, settings: settings, payloadOwnerLedger: &ledger,
			residents: map[server.Id]*Resident{}, connections: map[server.Id]map[server.Id]context.CancelFunc{}}
		residents := []*Resident{newResidentCallbackLifecycleFixture(t, ctx, settings), newResidentCallbackLifecycleFixture(t, ctx, settings)}
		for _, resident := range residents {
			resident.exchange = exchange
			resident.residentId = server.NewId()
			resident.clientForwardUnsub = resident.client.AddForwardCallback(resident.handleClientForward)
			exchange.residents[resident.clientId] = resident
		}
		var socketWorkers sync.WaitGroup
		settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
			local, remote := net.Pipe()
			socketWorkers.Add(1)
			go func() { defer socketWorkers.Done(); exchange.handleExchangeConnection(remote) }()
			return local, nil
		}
		var forwards []*ResidentForward
		var forwardDone, idleDone []<-chan struct{}
		for i, source := range residents {
			destination := residents[1-i]
			forward := NewResidentForward(ctx, exchange, destination.clientId)
			source.forwards[destination.clientId] = forward
			forwards = append(forwards, forward)
			forwardDone = append(forwardDone, startForwardDemand(forward, func(context.Context, server.Id, time.Duration) *model.NetworkClientResident {
				return &model.NetworkClientResident{ResidentId: destination.residentId, ResidentHost: "native-forward.invalid", ResidentInternalPorts: []int{1}}
			}))
			done := make(chan struct{})
			idleDone = append(idleDone, done)
			go func() { defer close(done); forward.runIdleWatcher(source.clientId) }()
		}
		var peers []*clientconnect.Client
		var detach []func()
		var routeQueues []chan []byte
		for i, resident := range residents {
			clientSettings := clientconnect.DefaultClientSettingsWithBufferSize(settings.ExchangeBufferSize)
			clientSettings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
			clientSettings.Log = clientconnect.NewNoopLogger()
			peer := clientconnect.NewClient(ctx, clientconnect.Id(resident.clientId), clientconnect.NewNoContractClientOob(), clientSettings)
			peer.ContractManager().AddNoContractPeer(clientconnect.Id(residents[1-i].clientId))
			send, receive, remove, err := resident.AddTransport()
			if err != nil {
				t.Fatal(err)
			}
			outbound := clientconnect.NewSendGatewayTransportWithType(clientconnect.TransportTypeH1)
			inbound := clientconnect.NewReceiveGatewayTransport()
			peer.RouteManager().UpdateTransport(outbound, []clientconnect.Route{receive})
			peer.RouteManager().UpdateTransportWithProperties(inbound, []clientconnect.Route{send}, clientconnect.TransferCarrierProperties{ReceiveReliability: clientconnect.CarrierReliabilityReliable})
			peers = append(peers, peer)
			detach = append(detach, remove)
			routeQueues = append(routeQueues, send, receive)
		}
		defer func() {
			cancel()
			for _, remove := range detach {
				remove()
			}
			for _, peer := range peers {
				if err := peer.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
			for _, resident := range residents {
				if err := resident.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
			for i := range forwardDone {
				<-forwardDone[i]
				<-idleDone[i]
			}
			socketWorkers.Wait()
			for _, queue := range routeQueues {
				returnReadyPooledMessages(queue)
			}
		}()
		var delivered atomic.Int64
		peers[1].AddReceiveCallback(func(_ clientconnect.TransferPath, frames []*protocol.Frame, _ clientconnect.Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_TestSimpleMessage {
					t.Errorf("unexpected delivered frame type %v", frame.MessageType)
				}
				delivered.Add(1)
			}
		})
		const burstSize = 512
		acked := 0
		burst := func() {
			ack := make(chan error, burstSize)
			var witnesses [][]byte
			for i := range burstSize {
				frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: fmt.Sprintf("%d:%s", acked+i, strings.Repeat("x", 700))})
				witnesses = append(witnesses, retainResidentPoolWitness(frame.MessageBytes))
				if !peers[0].SendWithTimeout(frame, clientconnect.Id(residents[1].clientId), func(err error) { ack <- err }, time.Second) {
					clientconnect.MessagePoolReturn(frame.MessageBytes)
					t.Fatal("native sender refused burst")
				}
			}
			for range burstSize {
				select {
				case err := <-ack:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("native forwarding/ACK burst did not complete")
				}
			}
			acked += burstSize
			synctest.Wait()
			if delivered.Load() != int64(acked) {
				t.Fatalf("delivered=%d ACKed=%d", delivered.Load(), acked)
			}
			requireResidentPoolOwnersReturned(t, witnesses, "native forwarding ACK burst")
			snapshot := ledger.snapshot()
			if !snapshot.Complete {
				t.Fatal("quiescent native forwarding ledger incomplete")
			}
			for _, group := range snapshot.Groups {
				if group.Messages != 0 || group.LogicalBytes != 0 || group.BackingCharge != 0 || group.Admitted != group.Released {
					t.Fatalf("delivered native forwarding retained stage ownership: %+v", snapshot)
				}
			}
		}
		burst()
		time.Sleep(14 * time.Minute)
		burst()
		time.Sleep(time.Minute)
		for _, forward := range forwards {
			if forward.IsDone() {
				t.Fatal("accepted native work did not renew the existing forward generation")
			}
		}
		burst()
		t.Logf("delivered=%d ACKed=%d accepted_bursts=3 forward_generations=2 payload_stage_owners=0", delivered.Load(), acked)
	})
}

// A full destination queue cannot accept another payload. Refused offers must
// not renew that queue's existing fifteen-minute inactivity allowance. Use
// the real callback shard, production-size output queue, owned lookup wait,
// idle watcher, accounting and pool returns; only resident discovery is held.
func TestResidentForwardRefusedOffersDoNotRenewIdleDeadline(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultExchangeSettings()
		settings.ForwardEnforceActiveContracts = false
		if settings.ForwardBufferSize != 4096 || settings.ForwardIdleTimeout != 15*time.Minute {
			t.Fatal("production forward bounds changed")
		}
		resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
		var ledger residentPayloadLedger
		resident.exchange.payloadOwnerLedger = &ledger
		destination := server.NewId()
		forward := NewResidentForward(ctx, resident.exchange, destination)
		resident.forwards[destination] = forward
		var lookups atomic.Int64
		runDone := startForwardDemand(forward, func(ctx context.Context, _ server.Id, _ time.Duration) *model.NetworkClientResident {
			lookups.Add(1)
			<-ctx.Done()
			return nil
		})
		idleDone := make(chan struct{})
		go func() { defer close(idleDone); forward.runIdleWatcher(resident.clientId) }()
		defer func() {
			cancel()
			forward.Cancel()
			<-runDone
			<-idleDone
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		path := clientconnect.TransferPath{SourceId: clientconnect.Id(resident.clientId), DestinationId: clientconnect.Id(destination)}
		var witnesses [][]byte
		offer := func() {
			message := clientconnect.MessagePoolGet(1200)
			witnesses = append(witnesses, retainResidentPoolWitness(message))
			resident.handleClientForward(path, message)
			clientconnect.MessagePoolReturn(message)
			synctest.Wait()
		}
		for range settings.ForwardBufferSize + 1 {
			offer()
		}
		acceptedAt := time.Now()
		snapshot := ledger.snapshot()
		if !snapshot.Complete || snapshot.Groups[residentPayloadForwardIngress].Messages != 0 ||
			snapshot.Groups[residentPayloadForwardOutput].Messages != int64(settings.ForwardBufferSize+1) ||
			len(forward.send) != settings.ForwardBufferSize || lookups.Load() != 1 {
			t.Fatalf("native held lookup and full output queue not established: %+v queue=%d lookup=%d", snapshot, len(forward.send), lookups.Load())
		}
		for range 14 {
			time.Sleep(time.Minute)
			offer() // Every offer is refused; none becomes new queued work.
		}
		snapshot = ledger.snapshot()
		if !snapshot.Complete || snapshot.Groups[residentPayloadForwardOutput].Messages != int64(settings.ForwardBufferSize+1) ||
			snapshot.Groups[residentPayloadForwardOutput].Released != 14 {
			t.Fatalf("refused output ownership did not conserve: %+v", snapshot)
		}
		time.Sleep(time.Minute - time.Nanosecond)
		if forward.IsDone() {
			t.Fatal("forward expired before its unchanged inactivity allowance")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		closed := forward.IsDone()
		t.Logf("accepted=%d refused=14 idle_seconds=%.0f last_activity_offset_seconds=%.0f closed=%t", settings.ForwardBufferSize+1, time.Since(acceptedAt).Seconds(), time.Unix(0, forward.lastActivityNanos.Load()).Sub(acceptedAt).Seconds(), closed)
		if !closed {
			t.Error("refused full-queue offers renewed the existing forward idle deadline")
			forward.Cancel()
		}
		<-runDone
		<-idleDone
		snapshot = ledger.snapshot()
		if !snapshot.Complete || snapshot.Groups[residentPayloadForwardOutput].Messages != 0 ||
			snapshot.Groups[residentPayloadForwardOutput].Admitted != snapshot.Groups[residentPayloadForwardOutput].Released {
			t.Fatalf("joined output owners did not return: %+v", snapshot)
		}
		requireResidentPoolOwnersReturned(t, witnesses, "full forward queue at existing idle deadline")
	})
}
