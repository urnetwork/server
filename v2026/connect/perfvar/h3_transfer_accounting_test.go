package perfvar

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The carrier comparison equates wire observations with completed recovery
// attempts. A valid ACK may arrive after observation, including while a route
// write is waiting for capacity. Exercise both orderings without QUIC, loss,
// sleeps in real time, or a probabilistic race-detector schedule.
func TestH3TransferRecoveryAccountingWithConcurrentAck(t *testing.T) {
	// Keep process-global pool maintenance outside virtual-time ownership.
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	for _, ackBeforeRoute := range []bool{true, false} {
		name := "ack-during-rejected-route-write"
		if ackBeforeRoute {
			name = "ack-after-wire-observation"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h3TransferRecoveryAccountingWithAck(t, ackBeforeRoute)
			})
		})
	}
}

func h3TransferRecoveryAccountingWithAck(t *testing.T, ackBeforeRoute bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	peer := clientconnect.NewId()
	retryObserved := make(chan *protocol.Pack, 1)
	releaseObserver := make(chan struct{})
	var wire h3TransferCarrierWireStats
	settings := clientconnect.DefaultClientSettings()
	settings.Log = clientconnect.NewNoopLogger()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	// Public identity registration uses OOB instead of creating an unrelated
	// retained Transfer sequence inside the counters under test.
	settings.ClientKeyRegistrationRequired = true
	settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Now().Add(time.Hour)
	settings.SendBufferSettings.WriteTimeout = 250 * time.Millisecond
	settings.SendBufferSettings.TransferWireMessageObserver = func(observation clientconnect.TransferWireMessageObservation) {
		wire.observe(observation)
		if !observation.Resend {
			return
		}
		var frame protocol.TransferFrame
		if err := clientconnect.ProtoUnmarshal(observation.TransferFrameBytes, &frame); err != nil || frame.Pack == nil {
			t.Errorf("retry observation is not a Pack: %v", err)
			return
		}
		// The decoded message owns its fields. No borrowed Transfer bytes cross
		// this test-only barrier. Production observers remain nonblocking.
		retryObserved <- frame.Pack
		select {
		case <-releaseObserver:
		case <-ctx.Done():
		}
	}
	client := clientconnect.NewClient(ctx, clientconnect.NewId(), clientconnect.NewNoContractClientOob(), settings)
	outbound := make(clientconnect.Route, 1)
	inbound := make(clientconnect.Route, 1)
	client.RouteManager().UpdateTransportWithProperties(
		clientconnect.NewSendClientTransport(clientconnect.DestinationId(peer)),
		[]clientconnect.Route{outbound},
		clientconnect.TransferCarrierProperties{Unreliable: true},
	)
	client.RouteManager().UpdateTransport(clientconnect.NewReceiveGatewayTransport(), []clientconnect.Route{inbound})
	client.ContractManager().AddNoContractPeer(peer)
	defer func() {
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		for len(outbound) > 0 {
			clientconnect.MessagePoolReturn(<-outbound)
		}
		for len(inbound) > 0 {
			clientconnect.MessagePoolReturn(<-inbound)
		}
	}()
	message, err := clientconnect.ToFrame(&protocol.SimpleMessage{Content: "account one physical recovery attempt"}, clientconnect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	acknowledged := make(chan error, 1)
	if accepted, err := client.SendWithTimeoutDetailed(message, peer, func(err error) { acknowledged <- err }, time.Second); !accepted || err != nil {
		clientconnect.MessagePoolReturn(message.MessageBytes)
		t.Fatalf("send admission: %t %v", accepted, err)
	}
	synctest.Wait()
	if len(outbound) != 1 {
		t.Fatal("initial Pack did not reach the route")
	}
	if ackBeforeRoute {
		clientconnect.MessagePoolReturn(<-outbound)
	}
	// Virtual time advances to the retained Pack's actual resend timer.
	pack := <-retryObserved
	if !ackBeforeRoute {
		close(releaseObserver)
		// The first Pack still owns the only route slot. Quiescence proves the
		// retry is inside its blocked physical write before ACK delivery.
		synctest.Wait()
	}
	ack, err := clientconnect.ProtoMarshal(&protocol.TransferFrame{
		TransferPath: clientconnect.TransferPath{SourceId: peer, DestinationId: client.ClientId()}.ToProtobuf(),
		Ack:          &protocol.Ack{SequenceId: pack.SequenceId, MessageId: pack.MessageId, Tag: pack.Tag},
	})
	if err != nil {
		t.Fatal(err)
	}
	inbound <- ack
	synctest.Wait()
	if ackBeforeRoute {
		close(releaseObserver)
	}
	if err := <-acknowledged; err != nil {
		t.Fatalf("valid ACK did not complete the original Pack: %v", err)
	}
	synctest.Wait()
	if err := client.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats, observed := client.SendRecoveryStats(), wire.snapshot()
	recoveries := stats.TimeoutResendWriteCount + stats.CarrierChangeWriteCount + stats.SelectiveGapWriteCount + stats.AckTailProbeWriteCount + stats.CumulativeProbeWriteCount
	if observed.ResendPackCount != 1 || recoveries != observed.ResendPackCount {
		t.Fatalf("observed physical retries lost recovery outcomes: wire=%+v recovery=%+v", observed, stats)
	}
	wantErrors := uint64(1)
	if ackBeforeRoute {
		wantErrors = 0
	}
	if stats.RecoveryWriteErrorCount != wantErrors || len(outbound) != 1 {
		t.Fatalf("late ACK changed physical route outcome: errors=%d want=%d pending=%d", stats.RecoveryWriteErrorCount, wantErrors, len(outbound))
	}
}
