package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server/internal/privateheapprofile"
)

func TestPrivateHeapSDKPayloadProjectionQualifiers(t *testing.T) {
	for _, settings := range []*ExchangeSettings{DefaultExchangeSettings(), exchangeSettingsForRun(RunOptions{MemoryOwnerLedger: true})} {
		if settings.SDKPayloadOwnerLedger != nil {
			t.Fatal("private packet accounting enabled by ordinary lifecycle metrics")
		}
	}
	if privateHeapProfileCompanion(&Exchange{}, nil).SDKPayload != nil {
		t.Fatal("disabled SDK scope reported measured zero")
	}
	snapshot := clientconnect.TransferPayloadOwnerSnapshot{
		Enabled: true, Complete: true, Revision: 12,
		SendAck: clientconnect.TransferPayloadOwnerGroup{Owners: 2, BackingByteCharges: 4120, AdmittedTotal: 1<<54 + 2, ReleasedTotal: 1 << 54},
		Forward: clientconnect.TransferPayloadOwnerGroup{Owners: 3, BackingByteCharges: 6180, AdmittedTotal: 8, ReleasedTotal: 5},
	}
	got := privateHeapSDKPayloadSnapshot(snapshot)
	if !got.Enabled || !got.Complete || got.Revision != snapshot.Revision ||
		got.SendAck.Owners != 2 || got.SendAck.BackingByteCharges != 4120 || got.SendAck.AdmittedTotal != 1<<54+2 || got.SendAck.ReleasedTotal != 1<<54 ||
		got.Forward.Owners != 3 || got.Forward.BackingByteCharges != 6180 || got.Forward.AdmittedTotal != 8 || got.Forward.ReleasedTotal != 5 {
		t.Fatal("fixed projection lost typed ownership values")
	}
	for _, disabled := range []bool{false, true} {
		snapshot.Enabled, snapshot.Complete = !disabled, disabled
		got = privateHeapSDKPayloadSnapshot(snapshot)
		if got.Enabled != snapshot.Enabled || got.Complete != snapshot.Complete || got.Revision != snapshot.Revision ||
			got.SendAck != (privateheapprofile.TransferPayloadOwnerGroup{}) || got.Forward != (privateheapprofile.TransferPayloadOwnerGroup{}) {
			t.Fatal("unavailable SDK snapshot retained authoritative ownership values")
		}
	}
}

func TestPrivateHeapSDKCapturedNativeForwardOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ledger, replacement clientconnect.TransferPayloadOwnerLedger
		settings := DefaultExchangeSettings()
		settings.KeyEventDelivery.Enabled = false
		settings.SDKPayloadOwnerLedger = &ledger
		exchangeCtx, stopExchange := context.WithCancel(context.Background())
		stopExchange()
		exchange := NewExchange(exchangeCtx, "synthetic", "connect", "synthetic", nil, nil, settings)
		defer exchange.Close()
		settings.SDKPayloadOwnerLedger = &replacement
		first, second := exchange.residentClientSettings(), exchange.residentClientSettings()
		if first == second || first.PayloadOwnerLedger != &ledger || second.PayloadOwnerLedger != &ledger {
			t.Fatal("resident settings lost the Exchange's immutable SDK scope")
		}

		ctx, cancel := context.WithCancel(context.Background())
		first.Log = clientconnect.NewNoopLogger()
		client := clientconnect.NewClient(ctx, clientconnect.ControlId, clientconnect.NewNoContractClientOob(), first)
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		first.PayloadOwnerLedger = &replacement
		destination := clientconnect.DestinationId(clientconnect.NewId())
		sequence := clientconnect.NewForwardSequence(ctx, client, destination, clientconnect.DefaultForwardBufferSettingsWithBufferSize(2))
		wire := clientconnect.MessagePoolGet(100)
		witness := clientconnect.MessagePoolShareReadOnly(wire)
		defer func() {
			if !clientconnect.MessagePoolReturn(witness) {
				t.Error("final local owner did not release the shared root")
			}
		}()
		closeSequence := sync.OnceFunc(sequence.Close)
		defer closeSequence()
		if ok, err := sequence.Pack(&clientconnect.ForwardPack{Destination: destination, TransferFrameBytes: wire, Ctx: ctx}, 0); !ok || err != nil {
			clientconnect.MessagePoolReturn(wire)
			t.Fatalf("native Forward admission=%t err=%v", ok, err)
		}
		held := privateHeapProfileCompanion(exchange, nil).SDKPayload
		if held == nil || !held.Enabled || !held.Complete || held.Forward.Owners != 1 || held.Forward.BackingByteCharges != int64(cap(wire)) || held.Forward.AdmittedTotal != 1 || held.Forward.ReleasedTotal != 0 {
			t.Fatalf("native held owner not reflected: %+v", held)
		}
		closeSequence()
		final := privateHeapProfileCompanion(exchange, nil).SDKPayload
		if !final.Complete || final.Forward.Owners != 0 || final.Forward.BackingByteCharges != 0 || final.Forward.AdmittedTotal != 1 || final.Forward.ReleasedTotal != 1 {
			t.Fatalf("native final release not reflected: %+v", final)
		}
		if s := replacement.Snapshot(); !s.Complete || s.Forward.AdmittedTotal != 0 {
			t.Fatal("mutation of original settings split admission and return")
		}
	})
}
