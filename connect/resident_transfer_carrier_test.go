// Resident carrier tests pin unreliable delivery metadata across the edge to
// resident exchange boundary.
package connect

import (
	"bytes"
	"context"
	"encoding/gob"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// Models the exchange header understood by a pre-flight-control server.
type legacyTransferCarrierExchangeHeader struct {
	Version    int
	ClientId   server.Id
	ResidentId server.Id
	Op         ExchangeOp
}

// Gob ignores the added field at an old receiver and supplies false when a new
// receiver reads an old header, so mixed-version deployment stays reliable.
func TestExchangeHeaderUnreliableTransferGobCompatibility(t *testing.T) {
	current := ExchangeHeader{
		Version:                               1,
		ClientId:                              server.NewId(),
		ResidentId:                            server.NewId(),
		Op:                                    ExchangeOpTransport,
		UnreliableTransfer:                    true,
		UnreliableTransferMaxMessageByteCount: 733,
		UnreliableFlowIsolation:               true,
		UnreliableFlowReserve:                 true,
	}
	var currentBytes bytes.Buffer
	if err := gob.NewEncoder(&currentBytes).Encode(&current); err != nil {
		t.Fatal(err)
	}
	var legacy legacyTransferCarrierExchangeHeader
	if err := gob.NewDecoder(&currentBytes).Decode(&legacy); err != nil {
		t.Fatalf("old receiver rejected current header: %v", err)
	}
	if legacy.Version != current.Version || legacy.ClientId != current.ClientId ||
		legacy.ResidentId != current.ResidentId || legacy.Op != current.Op {
		t.Fatalf("old receiver decoded %+v, want current identity %+v", legacy, current)
	}

	legacy = legacyTransferCarrierExchangeHeader{
		Version:    1,
		ClientId:   server.NewId(),
		ResidentId: server.NewId(),
		Op:         ExchangeOpTransport,
	}
	var legacyBytes bytes.Buffer
	if err := gob.NewEncoder(&legacyBytes).Encode(&legacy); err != nil {
		t.Fatal(err)
	}
	var decoded ExchangeHeader
	if err := gob.NewDecoder(&legacyBytes).Decode(&decoded); err != nil {
		t.Fatalf("current receiver rejected old header: %v", err)
	}
	if decoded.UnreliableTransfer || decoded.UnreliableFlowIsolation ||
		decoded.UnreliableFlowReserve ||
		decoded.UnreliableTransferMaxMessageByteCount != 0 ||
		decoded.Version != legacy.Version ||
		decoded.ClientId != legacy.ClientId || decoded.ResidentId != legacy.ResidentId ||
		decoded.Op != legacy.Op {
		t.Fatalf("current receiver decoded %+v, want reliable legacy identity %+v", decoded, legacy)
	}
}

// The edge-side constructor stamps only explicitly negotiated unreliable
// carriers; legacy H1/H3 construction remains zero-valued and reliable.
func TestResidentTransportConstructorCarriesTransferProperties(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchange := &Exchange{settings: DefaultExchangeSettings()}
	clientId := server.NewId()
	instanceId := server.NewId()
	unreliable := NewResidentTransportWithProperties(
		ctx,
		exchange,
		clientId,
		instanceId,
		clientconnect.TransferCarrierProperties{
			Unreliable:                    true,
			UnreliableMaxMessageByteCount: 733,
			UnreliableFlowIsolation:       true,
			UnreliableFlowReserve:         true,
		},
	)
	defer unreliable.Close()
	if !unreliable.header.UnreliableTransfer ||
		unreliable.header.UnreliableTransferMaxMessageByteCount != 733 ||
		!unreliable.header.UnreliableFlowIsolation ||
		!unreliable.header.UnreliableFlowReserve {
		t.Fatal("unreliable resident transport omitted its exchange property")
	}
	roundTrip := unreliable.header.transferCarrierProperties()
	if !roundTrip.Unreliable || roundTrip.UnreliableMaxMessageByteCount != 733 ||
		!roundTrip.UnreliableFlowIsolation || !roundTrip.UnreliableFlowReserve {
		t.Fatalf("exchange carrier round trip=%+v", roundTrip)
	}
	reliable := NewResidentTransport(ctx, exchange, clientId, instanceId)
	defer reliable.Close()
	if reliable.header.UnreliableTransfer ||
		reliable.header.UnreliableTransferMaxMessageByteCount != 0 ||
		reliable.header.UnreliableFlowIsolation ||
		reliable.header.UnreliableFlowReserve {
		t.Fatal("legacy resident transport was marked unreliable")
	}
}

// Resident route publication reaches the real Transfer sender rather than
// stopping at the exchange header.
func TestResidentUnreliableTransportPublishesTransferFlightPolicy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchangeSettings := DefaultExchangeSettings()
	exchange := &Exchange{settings: exchangeSettings}
	clientSettings := clientconnect.DefaultClientSettingsWithBufferSize(
		exchangeSettings.ExchangeBufferSize,
	)
	clientSettings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	clientSettings.SendBufferSettings.UnreliableInitialFlightByteCount = 1
	clientSettings.SendBufferSettings.UnreliableMinimumFlightByteCount = 1
	clientSettings.SendBufferSettings.UnreliableMaximumFlightByteCount = 1
	clientSettings.SendBufferSettings.UnreliableFlightIncreaseByteCount = 1
	client := clientconnect.NewClient(
		ctx,
		clientconnect.ControlId,
		clientconnect.NewNoContractClientOob(),
		clientSettings,
	)
	clientId := server.NewId()
	client.ContractManager().AddNoContractPeer(clientconnect.Id(clientId))
	residentCtx, residentCancel := context.WithCancel(ctx)
	resident := &Resident{
		ctx:                residentCtx,
		cancel:             residentCancel,
		exchange:           exchange,
		clientId:           clientId,
		instanceId:         server.NewId(),
		residentId:         server.NewId(),
		client:             client,
		transports:         map[*clientTransport]bool{},
		forwards:           map[server.Id]*ResidentForward{},
		clientReceiveUnsub: func() {},
		clientForwardUnsub: func() {},
	}
	send, _, closeTransport, err := resident.AddTransportWithProperties(
		clientconnect.TransferCarrierProperties{Unreliable: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()

	frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "resident unreliable flight"},
	)
	if !client.SendWithTimeout(
		frame,
		clientconnect.Id(clientId),
		nil,
		5*time.Second,
	) {
		clientconnect.MessagePoolReturn(frame.MessageBytes)
		t.Fatal("resident client did not admit the flight test frame")
	}
	select {
	case transferFrameBytes := <-send:
		clientconnect.MessagePoolReturn(transferFrameBytes)
	case <-time.After(10 * time.Second):
		t.Fatal("resident unreliable route did not receive a Transfer Pack")
	}

	deadline := time.After(10 * time.Second)
	for client.SendRecoveryStats().UnreliableFlightMaximumLimitByteCount == 0 {
		select {
		case <-deadline:
			t.Fatal("resident route did not activate unreliable flight control")
		case <-time.After(time.Millisecond):
		}
	}
	if limit := client.SendRecoveryStats().UnreliableFlightMaximumLimitByteCount; limit != 1 {
		t.Fatalf("resident unreliable flight limit = %d, want 1", limit)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer closeCancel()
	if err := client.CloseAndWait(closeCtx); err != nil {
		t.Fatal(err)
	}
}
