// Constructor failures retain the original error but must not orphan owned
// clients or the detached in-band controller. All identities are generated.
package connect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

// Uses the real constructor without listeners or external transports.
func newResidentConstructionExchange(ctx context.Context) *Exchange {
	exchangeCtx, cancel := context.WithCancel(ctx)
	settings := DefaultExchangeSettings()
	settings.EnableNetworkPeers = false
	settings.KeyEventDelivery.Enabled = false
	return &Exchange{
		ctx: exchangeCtx, cancel: cancel, settings: settings,
		host: "synthetic-resident.example", service: "connect", block: "synthetic",
		residents: map[server.Id]*Resident{}, connections: map[server.Id]map[server.Id]context.CancelFunc{},
		drainedClients: map[server.Id]struct{}{}, hostToServicePorts: map[int]int{},
	}
}

// Cleanup is explicit even on the pre-fix RED path; tests do not leak the very
// workers whose ownership they inspect. The injected failure is the exact
// panic contract used by the subsequent model lookup's server.Raise.
func residentConstructionFailure(t testing.TB) (*Exchange, *Resident, any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	exchange := newResidentConstructionExchange(ctx)
	var captured *Resident
	t.Cleanup(func() {
		defer cancel()
		exchange.Close()
		if captured != nil {
			if err := captured.CloseAndWait(ctx); err != nil {
				t.Errorf("synthetic constructor cleanup: %v", err)
			}
		}
	})
	want := errors.New("synthetic peer-profile read failure")
	exchange.beforeResidentProfileForTest = func(resident *Resident) {
		captured = resident
		server.Raise(want)
	}
	var got any
	func() {
		defer func() { got = recover() }()
		NewResident(ctx, exchange, server.NewId(), server.NewId(), server.NewId())
	}()
	if captured == nil {
		t.Fatal("fixture did not reach the peer-profile boundary")
	}
	return exchange, captured, got, want
}

// Recovered lookup failure must not leave the client on the process-long parent.
func TestResidentConstructionAbortCancelsClient(t *testing.T) {
	_, resident, _, _ := residentConstructionFailure(t)
	if resident.ctx.Err() != context.Canceled || !resident.client.IsDone() {
		t.Fatal("constructor failure returned with the resident client still live")
	}
}

// The controller deliberately outlives transport cancellation, so cancellation
// of the parent alone cannot reclaim it after a failed construction.
func TestResidentConstructionAbortCancelsDetachedController(t *testing.T) {
	_, resident, _, _ := residentConstructionFailure(t)
	if resident.residentController.ctx.Err() != context.Canceled {
		t.Fatal("constructor failure returned with detached controller still live")
	}
}

// Cleanup may neither hide the owning database error nor cancel sibling owners.
func TestResidentConstructionAbortPreservesPanicAndParent(t *testing.T) {
	exchange, _, got, want := residentConstructionFailure(t)
	if got != want {
		t.Fatalf("constructor panic changed: got %T, want original sentinel", got)
	}
	if exchange.ctx.Err() != nil {
		t.Fatal("constructor failure canceled the exchange parent")
	}
	if len(exchange.residents) != 0 {
		t.Fatal("failed constructor published a resident")
	}
}

// Holding an actual client ACK forces the abort owner to join before unwinding.
// Entry and release channels, not elapsed time, establish the ordering.
func TestResidentConstructionAbortJoinsHeldAck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	exchange := newResidentConstructionExchange(ctx)
	want := errors.New("synthetic profile failure with retained ACK")
	prepared := make(chan *Resident, 1)
	failNow := make(chan struct{})
	finished := make(chan any, 1)
	joinEntered := make(chan struct{})
	ackEntered := make(chan struct{})
	releaseAck := make(chan struct{})
	var joinOnce, ackOnce, failOnce, releaseOnce sync.Once
	exchange.beforeResidentProfileForTest = func(resident *Resident) {
		resident.beforeClientCloseJoinForTest = func() { joinOnce.Do(func() { close(joinEntered) }) }
		prepared <- resident
		select {
		case <-failNow:
		case <-ctx.Done():
		}
		server.Raise(want)
	}
	go func() {
		defer func() { finished <- recover() }()
		NewResident(ctx, exchange, server.NewId(), server.NewId(), server.NewId())
	}()
	var resident *Resident
	select {
	case resident = <-prepared:
	case <-ctx.Done():
		t.Fatal("constructor did not reach prepared boundary")
	}
	constructorFinished := false
	defer func() {
		failOnce.Do(func() { close(failNow) })
		releaseOnce.Do(func() { close(releaseAck) })
		if !constructorFinished {
			select {
			case <-finished:
			case <-ctx.Done():
				t.Error("constructor fixture did not join")
			}
		}
		exchange.Close()
		if err := resident.CloseAndWait(ctx); err != nil {
			t.Errorf("held-ACK fixture cleanup: %v", err)
		}
	}()
	send, _, closeTransport, err := resident.AddTransport()
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "synthetic retained constructor ownership"})
	if !resident.client.SendWithTimeout(frame, clientconnect.Id(resident.clientId), func(error) {
		ackOnce.Do(func() { close(ackEntered) })
		<-releaseAck
	}, time.Second) {
		clientconnect.MessagePoolReturn(frame.MessageBytes)
		t.Fatal("synthetic constructor client refused send")
	}
	select {
	case transferFrameBytes := <-send:
		clientconnect.MessagePoolReturn(transferFrameBytes)
	case <-ctx.Done():
		t.Fatal("synthetic constructor client did not write")
	}
	failOnce.Do(func() { close(failNow) })
	select {
	case <-joinEntered:
	case <-finished:
		constructorFinished = true
		t.Fatal("constructor failure unwound without entering its owned client join")
	case <-ctx.Done():
		t.Fatal("constructor failure did not begin cleanup")
	}
	select {
	case <-ackEntered:
	case <-finished:
		constructorFinished = true
		t.Fatal("constructor returned before retained ACK cleanup")
	case <-ctx.Done():
		t.Fatal("constructor cancellation did not reach retained ACK")
	}
	select {
	case <-finished:
		constructorFinished = true
		t.Fatal("constructor returned while ACK ownership was held")
	default:
	}
	releaseOnce.Do(func() { close(releaseAck) })
	select {
	case got := <-finished:
		constructorFinished = true
		if got != want {
			t.Fatalf("constructor panic changed: %T", got)
		}
	case <-ctx.Done():
		t.Fatal("released constructor did not finish owned cleanup")
	}
	if resident.residentController.ctx.Err() != context.Canceled {
		t.Fatal("joined construction left its controller alive")
	}
}

// A successful real profile lookup must transfer ownership to the returned
// resident, leaving its loopback usable until the caller explicitly closes it.
func TestResidentConstructionHealthyModelTransfersOwnership(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		exchange := newResidentConstructionExchange(ctx)
		defer exchange.Close()
		resident := NewResident(ctx, exchange, server.NewId(), server.NewId(), server.NewId())
		defer func() {
			if err := resident.CloseAndWait(ctx); err != nil {
				t.Error(err)
			}
		}()
		if resident.ctx.Err() != nil || resident.client.IsDone() || resident.residentController.ctx.Err() != nil {
			t.Fatal("successful constructor prematurely closed owned state")
		}
		acked := make(chan error, 1)
		frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "synthetic healthy constructor"})
		if !resident.client.SendWithTimeout(frame, clientconnect.ControlId, func(err error) { acked <- err }, time.Second) {
			clientconnect.MessagePoolReturn(frame.MessageBytes)
			t.Fatal("healthy constructor refused loopback")
		}
		select {
		case err := <-acked:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("healthy constructor loopback did not finish")
		}
	})
}
