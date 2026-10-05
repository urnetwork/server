package connect

import (
	"context"
	"testing"

	clientconnect "github.com/urnetwork/connect"
)

func TestPrivateHeapCompanionLifecycle(t *testing.T) {
	if privateHeapProfileCompanion(nil, nil).Available {
		t.Fatal("missing owners reported available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchange := &Exchange{ctx: ctx}
	key := connectListenerKey{transport: connectListenerTransportH3, port: 443}
	handler := &ConnectHandler{ctx: ctx, exchange: exchange, listenerStates: map[connectListenerKey]bool{key: true}}
	s := privateHeapProfileCompanion(exchange, handler)
	if !s.Available || !s.ListenerReady || !s.ServingActive || s.SDKPayload != nil {
		t.Fatal("live owner or unavailable SDK qualifier lost")
	}
	handler.exchange = &Exchange{ctx: ctx}
	if privateHeapProfileCompanion(exchange, handler).Available {
		t.Fatal("unrelated handler joined")
	}
	handler.exchange = exchange
	handler.listenerStates[key] = false
	s = privateHeapProfileCompanion(exchange, handler)
	if s.ListenerReady || !s.ServingActive {
		t.Fatal("listener and lifecycle states conflated")
	}
	exchange.draining.Store(true)
	s = privateHeapProfileCompanion(exchange, handler)
	if !s.Available || s.ServingActive {
		t.Fatal("draining owner state")
	}
	exchange.draining.Store(false)
	handler.closing = true
	if privateHeapProfileCompanion(exchange, handler).ServingActive {
		t.Fatal("closing owner reported serving")
	}
	handler.closing = false
	cancel()
	if privateHeapProfileCompanion(exchange, handler).ServingActive {
		t.Fatal("canceled owner reported serving")
	}
}

func TestPrivateHeapCompanionPayloadConservation(t *testing.T) {
	ledger := &residentPayloadLedger{}
	exchange := &Exchange{ctx: context.Background(), payloadOwnerLedger: ledger}
	charge := residentPayloadCharge{messages: 2, logical: 200, backing: 512}
	ledger.update(residentPayloadForwardOutput, 7, charge, true)
	s := privateHeapProfileCompanion(exchange, nil).ResidentPayload
	if s == nil || !s.Enabled || !s.Complete || s.Stages[2].Stage != "forward_output" || s.Stages[2].Messages != 2 || s.Stages[2].BackingByteCharges != 512 || s.Stages[2].AdmittedTotal != 2 {
		t.Fatal("held ownership context")
	}
	ledger.shards[0].writers.Add(1)
	s = privateHeapProfileCompanion(exchange, nil).ResidentPayload
	ledger.shards[0].writers.Add(-1)
	if s == nil || !s.Enabled || s.Complete {
		t.Fatal("incomplete snapshot became authoritative")
	}
	for _, stage := range s.Stages {
		if stage.Messages != 0 || stage.BackingByteCharges != 0 || stage.AdmittedTotal != 0 || stage.Stage != "" {
			t.Fatal("incomplete ownership values retained")
		}
	}
	ledger.update(residentPayloadForwardOutput, 7, charge, false)
	s = privateHeapProfileCompanion(exchange, nil).ResidentPayload
	if !s.Complete || s.Stages[2].Messages != 0 || s.Stages[2].ReleasedTotal != 2 || s.Stages[2].BackingByteCharges != 0 {
		t.Fatal("final release missing")
	}
	exchange.payloadOwnerLedger = nil
	s = privateHeapProfileCompanion(exchange, nil).ResidentPayload
	if s == nil || s.Enabled || s.Complete {
		t.Fatal("disabled payload accounting represented as zero")
	}
}

func TestPrivateHeapCompanionPoolRootConservation(t *testing.T) {
	before := privateHeapProfileCompanion(nil, nil)
	if !before.PoolClassesComplete {
		t.Fatal("fixed class context unavailable")
	}
	for i, size := range [...]int{256, 2048, 4096, 8192} {
		if before.PoolClasses[i].Size != size {
			t.Fatal("pool class layout")
		}
	}
	message := clientconnect.MessagePoolGet(100)
	shared := clientconnect.MessagePoolShareReadOnly(message)
	during := privateHeapProfileCompanion(nil, nil)
	if during.PoolClasses[0].Taken != before.PoolClasses[0].Taken+1 || during.PoolClasses[0].Returned != before.PoolClasses[0].Returned {
		t.Fatal("shared pool root charged more than once")
	}
	clientconnect.MessagePoolReturn(message)
	partial := privateHeapProfileCompanion(nil, nil)
	if partial.PoolClasses[0].Returned != before.PoolClasses[0].Returned {
		t.Fatal("root returned while shared owner still holds it")
	}
	clientconnect.MessagePoolReturn(shared)
	after := privateHeapProfileCompanion(nil, nil)
	if after.PoolClasses[0].Returned != before.PoolClasses[0].Returned+1 {
		t.Fatal("final pool root return missing")
	}
}

func TestPrivateHeapDefaultAndInvalidTarget(t *testing.T) {
	s, err := startPrivateHeapProfile(context.Background(), "", nil, nil)
	if err != nil || s != nil {
		t.Fatal("default touched runtime authority")
	}
	s, err = startPrivateHeapProfile(context.Background(), "by-us-fmt-5-edge-5/g1", nil, nil)
	if err == nil || s != nil {
		t.Fatal("invalid target accepted")
	}
}
