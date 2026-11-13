package connect

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func payloadEventually(t testing.TB, check func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("payload owner state did not reach the expected boundary")
}

func TestResidentPayloadSnapshotAndMetricAuthority(t *testing.T) {
	var ledger residentPayloadLedger
	charge := residentPayloadCharge{3, 17, 256}
	ledger.update(residentPayloadControl, 0, charge, true)
	s := ledger.snapshot()
	if !s.Enabled || !s.Complete || s.Groups[0] != (residentPayloadGroup{3, 17, 256, 3, 0}) {
		t.Fatal("coherent ownership lost")
	}
	if ledger.snapshotAt(func(shard int) { ledger.update(residentPayloadControl, 0, charge, false) }).Complete {
		t.Fatal("overlap declared coherent")
	}
	registry := prometheus.NewPedanticRegistry()
	state := s
	registry.MustRegister(newResidentPayloadCollector(func() residentPayloadSnapshot { return state }))
	for _, tc := range []struct {
		name     string
		snapshot residentPayloadSnapshot
		series   int
	}{
		{"healthy", s, 17},
		{"missing", residentPayloadSnapshot{}, 2},
		{"overlap", residentPayloadSnapshot{Enabled: true}, 2},
		{"invalid", residentPayloadSnapshot{Enabled: true, Complete: true, Groups: [3]residentPayloadGroup{{Messages: -1}}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state = tc.snapshot
			families, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, family := range families {
				for _, metric := range family.Metric {
					n++
					for _, label := range metric.Label {
						if label.GetName() != "stage" || (label.GetValue() != "control_ingress" && label.GetValue() != "forward_ingress" && label.GetValue() != "forward_output") {
							t.Fatal("nonfinite label")
						}
					}
				}
			}
			if n != tc.series {
				t.Fatalf("series=%d want=%d", n, tc.series)
			}
		})
	}
}

func TestResidentPayloadDefaultOffAndRegistration(t *testing.T) {
	if exchangeSettingsForRun(RunOptions{}).payloadOwnerLedger != nil {
		t.Fatal("default enabled")
	}
	settings := exchangeSettingsForRun(RunOptions{MemoryOwnerLedger: true})
	if settings.payloadOwnerLedger == nil || settings.payloadOwnerLedger == exchangeSettingsForRun(RunOptions{MemoryOwnerLedger: true}).payloadOwnerLedger {
		t.Fatal("ledger scope not owned")
	}
	registry := prometheus.NewRegistry()
	stop, err := registerResidentPayloadMetrics(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	families, err := registry.Gather()
	if err != nil || len(families) != 0 {
		t.Fatal("nil published measurements")
	}
	stop, err = registerResidentPayloadMetrics(registry, settings.payloadOwnerLedger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registerResidentPayloadMetrics(registry, &residentPayloadLedger{}); err == nil {
		t.Fatal("duplicate scope accepted")
	}
	stop()
	stop, err = registerResidentPayloadMetrics(registry, &residentPayloadLedger{})
	if err != nil {
		t.Fatal(err)
	}
	stop()
}

func TestResidentPayloadControlAcceptedTailAndRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settings := DefaultExchangeSettings()
	settings.ResidentControlQueueSize = 2
	settings.ControlMinTimeout = 0
	resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
	var ledger residentPayloadLedger
	resident.exchange.payloadOwnerLedger = &ledger
	entered, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	resident.residentController.beforeHandleControlFramesForTest = func() { once.Do(func() { close(entered); <-release }) }
	defer func() {
		releaseOnce.Do(func() { close(release) })
		if err := resident.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	}()
	var witnesses [][]byte
	send := func() {
		frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.ControlPing{})
		witnesses = append(witnesses, retainResidentPoolWitness(frame.MessageBytes))
		resident.handleClientReceive(clientconnect.SourceId(clientconnect.Id(resident.clientId)), []*protocol.Frame{frame}, clientconnect.Peer{})
		clientconnect.MessagePoolReturn(frame.MessageBytes)
	}
	send()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("controller did not enter")
	}
	send()
	send()
	s := ledger.snapshot()
	if !s.Complete || s.Groups[0].Messages != 3 || s.Groups[0].BackingCharge != 3*256 {
		t.Fatalf("accepted control owners=%+v", s)
	}
	send() // Finite queue refusal releases only this offer; accepted work stays.
	s = ledger.snapshot()
	if !resident.IsDone() || !s.Complete || s.Groups[0].Messages != 3 || s.Groups[0].Admitted != 4 || s.Groups[0].Released != 1 {
		t.Fatalf("refused control state=%+v", s)
	}
	releaseOnce.Do(func() { close(release) })
	if err := resident.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	s = ledger.snapshot()
	if !s.Complete || s.Groups[0].Messages != 0 || s.Groups[0].Admitted != s.Groups[0].Released {
		t.Fatalf("control tail not released=%+v", s)
	}
	requireResidentPoolOwnersReturned(t, witnesses, "control ledger ownership")
}

func TestResidentPayloadForwardPendingRefusalAndCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settings := DefaultExchangeSettings()
	settings.ForwardBufferSize = 2
	settings.ForwardEnforceActiveContracts = false
	resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
	var ledger residentPayloadLedger
	resident.exchange.payloadOwnerLedger = &ledger
	destination := server.NewId()
	forward := NewResidentForward(ctx, resident.exchange, destination)
	resident.stateLock.Lock()
	resident.forwards[destination] = forward
	resident.stateLock.Unlock()
	defer func() {
		forward.Close()
		if err := resident.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	}()
	var witnesses [][]byte
	for range 3 {
		message := clientconnect.MessagePoolGet(127)
		witnesses = append(witnesses, retainResidentPoolWitness(message))
		resident.handleClientForward(clientconnect.TransferPath{SourceId: clientconnect.Id(resident.clientId), DestinationId: clientconnect.Id(destination)}, message)
		clientconnect.MessagePoolReturn(message)
	}
	payloadEventually(t, func() bool {
		s := ledger.snapshot()
		return s.Complete && s.Groups[1].Admitted == 3 && s.Groups[1].Messages == 0 && s.Groups[2].Admitted == 3 && s.Groups[2].Messages == 2
	})
	entered := make(chan struct{})
	done := startForwardDemand(forward, func(ctx context.Context, _ server.Id, _ time.Duration) *model.NetworkClientResident {
		close(entered)
		<-ctx.Done()
		return nil
	})
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("pending lookup not entered")
	}
	s := ledger.snapshot()
	if !s.Complete || s.Groups[2].Messages != 2 || s.Groups[2].LogicalBytes != 254 {
		t.Fatalf("pending owner lost=%+v", s)
	}
	forward.Cancel()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("forward did not join")
	}
	s = ledger.snapshot()
	if !s.Complete || s.Groups[2].Messages != 0 || s.Groups[2].Admitted != s.Groups[2].Released {
		t.Fatalf("pending/queued return lost=%+v", s)
	}
	requireResidentPoolOwnersReturned(t, witnesses, "forward ledger cancellation")
}

func TestResidentPayloadForwardHealthySocketHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settings := DefaultExchangeSettings()
	settings.ForwardEnforceActiveContracts = false
	resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
	var ledger residentPayloadLedger
	resident.exchange.payloadOwnerLedger = &ledger
	destination := server.NewId()
	forward := NewResidentForward(ctx, resident.exchange, destination)
	resident.stateLock.Lock()
	resident.forwards[destination] = forward
	resident.stateLock.Unlock()
	got := make(chan int, 1)
	peerStop := make(chan struct{})
	peerDone := make(chan struct{})
	settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
		local, peer := net.Pipe()
		go func() {
			defer close(peerDone)
			defer peer.Close()
			buffer := NewDefaultExchangeBuffer(settings)
			header, err := buffer.ReadHeader(ctx, peer)
			if err != nil {
				t.Error(err)
				return
			}
			if err = buffer.WriteHeader(ctx, peer, header); err != nil {
				t.Error(err)
				return
			}
			message, err := buffer.ReadMessage(peer)
			if err != nil {
				t.Error(err)
				return
			}
			got <- len(message)
			clientconnect.MessagePoolReturn(message)
			<-peerStop
		}()
		return local, nil
	}
	done := startForwardDemand(forward, func(context.Context, server.Id, time.Duration) *model.NetworkClientResident {
		return &model.NetworkClientResident{ResidentId: server.NewId(), ResidentHost: "fixture.invalid", ResidentInternalPorts: []int{1}}
	})
	var shutdown sync.Once
	stop := func() {
		shutdown.Do(func() {
			forward.Cancel()
			close(peerStop)
			<-done
			select {
			case <-peerDone:
			case <-ctx.Done():
				t.Error("peer did not join")
			}
			if err := resident.CloseAndWait(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	defer stop()
	message := clientconnect.MessagePoolGet(1200)
	witness := retainResidentPoolWitness(message)
	resident.handleClientForward(clientconnect.TransferPath{SourceId: clientconnect.Id(resident.clientId), DestinationId: clientconnect.Id(destination)}, message)
	clientconnect.MessagePoolReturn(message)
	select {
	case n := <-got:
		if n != 1200 {
			t.Fatal("payload length changed")
		}
	case <-ctx.Done():
		t.Fatal("healthy handoff stalled")
	}
	payloadEventually(t, func() bool {
		s := ledger.snapshot()
		return s.Complete && s.Groups[1].Admitted == 1 && s.Groups[1].Messages == 0 && s.Groups[2].Admitted == 1 && s.Groups[2].Messages == 0
	})
	// The stage transfers ownership into the socket queue; its zero charge
	// is not a claim that the downstream writer has returned the pool root.
	stop()
	requireResidentPoolOwnerReturned(t, witness, "joined forward socket handoff")
}

func BenchmarkResidentPayloadAccounting(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		name := "off"
		var ledger *residentPayloadLedger
		if enabled {
			name = "on"
			ledger = &residentPayloadLedger{}
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			charge := residentPayloadCharge{1, 1200, 2048}
			for b.Loop() {
				if ledger != nil {
					ledger.update(residentPayloadForwardIngress, 0, charge, true)
					ledger.update(residentPayloadForwardIngress, 0, charge, false)
				}
			}
		})
	}
}

func TestResidentPayloadShardConservationAndRefusal(t *testing.T) {
	for _, mode := range []string{"shared", "collision", "spread"} {
		t.Run(mode, func(t *testing.T) {
			var ledger residentPayloadLedger
			const workers, perWorker = 96, 257
			var done sync.WaitGroup
			done.Add(workers)
			for worker := range workers {
				go func() {
					defer done.Done()
					owner := byte(0)
					if mode == "spread" {
						owner = byte(worker)
					}
					if mode == "collision" {
						owner = byte(worker%2) * residentPayloadShardCount
					}
					stage := residentPayloadStage(worker % 3)
					charge := residentPayloadCharge{1, 17, 256}
					for range perWorker {
						ledger.update(stage, owner, charge, true)
						ledger.update(stage, owner, charge, false)
					}
				}()
			}
			done.Wait()
			s := ledger.snapshot()
			if !s.Enabled || !s.Complete {
				t.Fatal("joined shards unavailable")
			}
			for _, g := range s.Groups {
				if g.Messages != 0 || g.LogicalBytes != 0 || g.BackingCharge != 0 || g.Admitted != workers/3*perWorker || g.Released != g.Admitted {
					t.Fatalf("shard conservation=%+v", g)
				}
			}
		})
	}
	for owner := range residentPayloadShardCount {
		var ledger residentPayloadLedger
		charge := residentPayloadCharge{1, 17, 256}
		ledger.update(residentPayloadControl, byte(owner), charge, true)
		if ledger.snapshotAt(func(shard int) {
			if shard == owner {
				ledger.update(residentPayloadControl, byte(owner), charge, false)
			}
		}).Complete {
			t.Fatalf("overlap hidden in shard%d", owner)
		}
		ledger.shards[owner].writers.Add(1)
		if ledger.snapshot().Complete {
			t.Fatalf("live writer hidden in shard%d", owner)
		}
		ledger.shards[owner].writers.Add(-1)
		if !ledger.snapshot().Complete {
			t.Fatalf("joined writer unavailable in shard%d", owner)
		}
	}
	var invalid residentPayloadLedger
	invalid.shards[0].groups[0].messages.Store(-1)
	invalid.shards[1].groups[0].messages.Store(1)
	if invalid.snapshot().Complete {
		t.Fatal("opposite invalid shards hid corruption")
	}
}

func TestResidentPayloadAliasesRemainSeparateCharges(t *testing.T) {
	var ledger residentPayloadLedger
	message := clientconnect.MessagePoolGet(1200)
	shared := clientconnect.MessagePoolShareReadOnly(message)
	ledger.update(residentPayloadControl, 0, payloadCharge(message), true)
	ledger.update(residentPayloadForwardIngress, 1, payloadCharge(shared), true)
	s := ledger.snapshot()
	if !s.Complete || s.Groups[0].LogicalBytes != 1200 || s.Groups[1].LogicalBytes != 1200 || s.Groups[0].BackingCharge != 2048 || s.Groups[1].BackingCharge != 2048 {
		t.Fatal("two owner references collapsed into a physical-heap claim")
	}
	ledger.update(residentPayloadControl, 0, payloadCharge(message), false)
	if clientconnect.MessagePoolReturn(message) {
		t.Fatal("shared owner prematurely returned")
	}
	ledger.update(residentPayloadForwardIngress, 1, payloadCharge(shared), false)
	if !clientconnect.MessagePoolReturn(shared) {
		t.Fatal("last shared owner leaked")
	}
	s = ledger.snapshot()
	if !s.Complete || s.Groups[0].Messages != 0 || s.Groups[1].Messages != 0 {
		t.Fatal("alias charges did not conserve")
	}
}
