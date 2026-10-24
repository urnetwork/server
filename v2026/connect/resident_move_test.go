package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The move settings this file adds beside the design's (MOVE.md §5.8).
func TestDefaultMoveTargetBreakerWindow(t *testing.T) {
	settings := DefaultExchangeSettings()
	connect.AssertEqual(t, 10*time.Minute, settings.MoveTargetBreakerWindow)
	connect.AssertEqual(t, 10*time.Minute, settings.MoveTargetBreakerTimeout)
	// the window must cover the client's 60 s connect timeout, after which a
	// failure is reported
	connect.AssertEqual(t, true, 60*time.Second < settings.MoveTargetBreakerWindow)
}

// The H1 header declares the move version as a decimal; anything else is no
// declaration (MOVE.md D2).
func TestResidentMoveVersionFromHeader(t *testing.T) {
	for _, test := range []struct {
		value string
		want  uint32
	}{
		{value: "1", want: 1},
		{value: " 2 ", want: 2},
		{value: "", want: 0},
		{value: "0", want: 0},
		{value: "-1", want: 0},
		{value: "1.0", want: 0},
		{value: "one", want: 0},
		{value: "4294967296", want: 0},
	} {
		if got := residentMoveVersionFromHeader(test.value); got != test.want {
			t.Fatalf("version of %q = %d, want %d", test.value, got, test.want)
		}
	}
}

// The QUIC listeners name their carriers; anything else is unknown.
func TestResidentCarrierForQuicListener(t *testing.T) {
	connect.AssertEqual(t, ResidentCarrierH3, residentCarrierForQuicListener(connectListenerTransportH3))
	connect.AssertEqual(t, ResidentCarrierH3Dns, residentCarrierForQuicListener(connectListenerTransportH3Dns))
	connect.AssertEqual(t, ResidentCarrierUnknown, residentCarrierForQuicListener("other"))
}

// The moved close on H1 is transport control 3 with reason 6, the code
// beside the client limit (4001) and authorization (4002-4005) closes.
func TestMovedCloseSignal(t *testing.T) {
	message := movedControlMessage()
	connect.AssertEqual(t, 5, len(message))
	connect.AssertEqual(t, byte(connect.TransportControlClose), message[0])
	connect.AssertEqual(t, uint32(6), binary.BigEndian.Uint32(message[1:5]))
	connect.AssertEqual(t, 4006, movedCloseCode)
	connect.AssertEqual(t, int(4000+closeReasonMoved), movedCloseCode)
	connect.AssertEqual(t, "moved", connectH1CloseLabels[connectH1CloseMoved])
}

// The exchange header carries the edge's move version and carrier to the
// resident; a peer that predates them decodes and sends them as zero.
func TestExchangeHeaderCarriesTransportEdge(t *testing.T) {
	header := ExchangeHeader{
		ClientId:            server.NewId(),
		ResidentId:          server.NewId(),
		Op:                  ExchangeOpTransport,
		ResidentMoveVersion: 1,
		Carrier:             ResidentCarrierH3Dns,
		IpFamily:            6,
		ProvideIntent:       true,
	}
	b := bytes.NewBuffer(nil)
	if err := gob.NewEncoder(b).Encode(&header); err != nil {
		t.Fatal(err)
	}
	var decoded ExchangeHeader
	if err := gob.NewDecoder(bytes.NewBuffer(b.Bytes())).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, ResidentTransportEdge{ResidentMoveVersion: 1, Carrier: ResidentCarrierH3Dns, IpFamily: 6, ProvideIntent: true}, decoded.transportEdge())

	// the header of a peer that predates the edge fields
	type legacyExchangeHeader struct {
		Version    int
		ClientId   server.Id
		ResidentId server.Id
		Op         ExchangeOp
	}
	legacyBytes := bytes.NewBuffer(nil)
	if err := gob.NewEncoder(legacyBytes).Encode(&legacyExchangeHeader{ClientId: header.ClientId, ResidentId: header.ResidentId, Op: ExchangeOpTransport}); err != nil {
		t.Fatal(err)
	}
	var fromLegacy ExchangeHeader
	if err := gob.NewDecoder(legacyBytes).Decode(&fromLegacy); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, ResidentTransportEdge{}, fromLegacy.transportEdge())
	var toLegacy legacyExchangeHeader
	if err := gob.NewDecoder(bytes.NewBuffer(b.Bytes())).Decode(&toLegacy); err != nil {
		t.Fatalf("a peer that predates the edge fields cannot read the header: %v", err)
	}
	connect.AssertEqual(t, header.ClientId, toLegacy.ClientId)
}

// The resident keeps the highest move version any transport declared.
func TestResidentObservesHighestMoveVersion(t *testing.T) {
	resident := &Resident{}
	resident.observeTransportEdge(ResidentTransportEdge{Carrier: ResidentCarrierH1})
	connect.AssertEqual(t, uint32(0), resident.moveVersion.Load())
	resident.observeTransportEdge(ResidentTransportEdge{ResidentMoveVersion: 2, Carrier: ResidentCarrierH3})
	resident.observeTransportEdge(ResidentTransportEdge{ResidentMoveVersion: 1, Carrier: ResidentCarrierH1})
	connect.AssertEqual(t, uint32(2), resident.moveVersion.Load())
}

func TestResidentMoveCandidateCapabilityCarrierAndFamilies(t *testing.T) {
	for _, test := range []struct {
		carriers []ResidentCarrier
		direct   bool
	}{
		{carriers: nil, direct: false},
		{carriers: []ResidentCarrier{ResidentCarrierH1}, direct: true},
		{carriers: []ResidentCarrier{ResidentCarrierH1, ResidentCarrierH3}, direct: true},
		{carriers: []ResidentCarrier{ResidentCarrierH3, ResidentCarrierH3Dns}, direct: false},
		{carriers: []ResidentCarrier{ResidentCarrierUnknown}, direct: false},
	} {
		candidate := ResidentMoveCandidate{}
		for _, carrier := range test.carriers {
			candidate.Transports = append(candidate.Transports, ResidentTransportEdge{Carrier: carrier})
		}
		if candidate.DirectCarrier() != test.direct {
			t.Fatalf("carriers %v direct = %t", test.carriers, !test.direct)
		}
	}
	connect.AssertEqual(t, false, ResidentMoveCandidate{}.Capable())
	connect.AssertEqual(t, true, ResidentMoveCandidate{ResidentMoveVersion: 1}.Capable())

	ipv4, ipv6 := ResidentMoveCandidate{Transports: []ResidentTransportEdge{{IpFamily: 6}, {IpFamily: 0}}}.IpFamilies()
	connect.AssertEqual(t, false, ipv4)
	connect.AssertEqual(t, true, ipv6)
	connect.AssertEqual(t, uint8(4), residentTransportIpFamily(4))
	connect.AssertEqual(t, uint8(6), residentTransportIpFamily(6))
	connect.AssertEqual(t, uint8(0), residentTransportIpFamily(0))
}

// An exchange with a synthetic inventory of the given process host, for the
// rules that need no store.
func newResidentMoveTestExchange(ctx context.Context, t testing.TB, processHost string) *Exchange {
	t.Helper()
	inventory := newEdgeInventory(ctx, processHost, 0, newEdgeInventoryTestLoader(edgeInventoryTestServicesYaml).load)
	<-inventory.Loaded()
	return &Exchange{
		ctx:           ctx,
		exchangeId:    server.NewId(),
		host:          "host0",
		settings:      DefaultExchangeSettings(),
		edgeInventory: inventory,
		residents:     map[server.Id]*Resident{},
	}
}

// A balance move's intent matches only on its target host and against the
// resident it moved; a drain move's matches any exchange but its source.
func TestResidentMoveIntentMatchesTargetAndResident(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := newResidentMoveTestExchange(ctx, t, "alpha-edge-1")
	current := &model.NetworkClientResident{ResidentId: server.NewId()}
	intent := func(kind model.ResidentMoveKind, targetHost string) *model.ResidentMoveIntent {
		return &model.ResidentMoveIntent{
			MoveId:         server.NewId(),
			Kind:           kind,
			FromResidentId: current.ResidentId,
			FromExchangeId: server.NewId(),
			TargetHost:     targetHost,
		}
	}

	for _, test := range []struct {
		name   string
		intent *model.ResidentMoveIntent
		want   bool
	}{
		{name: "advisory to this host", intent: intent(model.ResidentMoveKindAdvisory, "alpha-edge-1"), want: true},
		{name: "compulsory to this host", intent: intent(model.ResidentMoveKindCompulsory, "alpha-edge-1"), want: true},
		{name: "advisory by WARP_HOST", intent: intent(model.ResidentMoveKindAdvisory, "host0"), want: true},
		{name: "advisory to another host", intent: intent(model.ResidentMoveKindAdvisory, "alpha-edge-0"), want: false},
		{name: "advisory without a host", intent: intent(model.ResidentMoveKindAdvisory, ""), want: false},
		{name: "drain to any host", intent: intent(model.ResidentMoveKindDrain, "alpha-edge-0"), want: true},
		{name: "unknown kind", intent: intent(model.ResidentMoveKind("unknown"), "alpha-edge-1"), want: false},
	} {
		if got := target.matchesResidentMoveIntent(test.intent, current); got != test.want {
			t.Fatalf("%s: match = %t, want %t", test.name, got, test.want)
		}
	}

	// a resident that is no longer the moved one
	other := intent(model.ResidentMoveKindDrain, "alpha-edge-1")
	other.FromResidentId = server.NewId()
	connect.AssertEqual(t, false, target.matchesResidentMoveIntent(other, current))
	// never at the source exchange itself
	source := intent(model.ResidentMoveKindDrain, "alpha-edge-1")
	source.FromExchangeId = target.exchangeId
	connect.AssertEqual(t, false, target.matchesResidentMoveIntent(source, current))
	// the host name intents and the load table use is the inventory's
	connect.AssertEqual(t, "alpha-edge-1", target.MoveHost())
	target.edgeInventory = nil
	connect.AssertEqual(t, "host0", target.MoveHost())
	// a disabled exchange takes over nothing, and reads no intent
	target.settings.EnableMove = false
	connect.AssertEqual(t, false, target.takeOverMovedResident(ctx, server.NewId(), server.NewId(), current, false))
}

// A move's targets come only from the target host's inventory addresses, one
// per family, and its labels from fixed sets (MOVE.md §5.10).
func TestValidateResidentMove(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchange := newResidentMoveTestExchange(ctx, t, "alpha-edge-0")
	valid := ResidentMove{
		ClientId:   server.NewId(),
		Kind:       model.ResidentMoveKindAdvisory,
		TargetHost: "alpha-edge-1",
		Targets:    []netip.Addr{netip.MustParseAddr("198.51.100.21"), netip.MustParseAddr("2001:db8::21")},
	}
	if err := exchange.validateResidentMove(valid); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*ResidentMove)
	}{
		{name: "drain kind", change: func(move *ResidentMove) { move.Kind = model.ResidentMoveKindDrain }},
		{name: "unknown kind", change: func(move *ResidentMove) { move.Kind = "synthetic" }},
		{name: "unknown host", change: func(move *ResidentMove) { move.TargetHost = "alpha-edge-9" }},
		{name: "no targets", change: func(move *ResidentMove) { move.Targets = nil }},
		{name: "another host's address", change: func(move *ResidentMove) { move.Targets = []netip.Addr{netip.MustParseAddr("192.0.2.10")} }},
		{name: "two of one family", change: func(move *ResidentMove) {
			move.Targets = []netip.Addr{netip.MustParseAddr("198.51.100.21"), netip.MustParseAddr("198.51.100.21")}
		}},
		{name: "not unicast", change: func(move *ResidentMove) { move.Targets = []netip.Addr{netip.MustParseAddr("224.0.0.1")} }},
	} {
		move := valid
		test.change(&move)
		if err := exchange.validateResidentMove(move); !errors.Is(err, ErrResidentMoveInvalid) {
			t.Fatalf("%s: validate = %v, want %v", test.name, err, ErrResidentMoveInvalid)
		}
	}

	// without an inventory nothing is a valid target
	exchange.edgeInventory = nil
	if err := exchange.validateResidentMove(valid); !errors.Is(err, ErrResidentMoveInvalid) {
		t.Fatalf("validate without an inventory = %v", err)
	}
}

// The mover's refusals that need no store.
func TestSendResidentMoveRefusesWithoutWriting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchange := newResidentMoveTestExchange(ctx, t, "alpha-edge-0")
	move := ResidentMove{
		ClientId:   server.NewId(),
		Kind:       model.ResidentMoveKindAdvisory,
		TargetHost: "alpha-edge-1",
		Targets:    []netip.Addr{netip.MustParseAddr("198.51.100.21")},
	}
	if _, err := exchange.SendResidentMove(ctx, move); !errors.Is(err, ErrResidentMoveNoResident) {
		t.Fatalf("move without a resident = %v", err)
	}
	exchange.draining.Store(true)
	if _, err := exchange.SendResidentMove(ctx, move); !errors.Is(err, ErrResidentMoveDraining) {
		t.Fatalf("move while draining = %v", err)
	}
	exchange.settings.EnableMove = false
	if _, err := exchange.SendResidentMove(ctx, move); !errors.Is(err, ErrResidentMoveDisabled) {
		t.Fatalf("move while disabled = %v", err)
	}
}

// The drain move control: code switches first, then the installed decision,
// which defaults to allowed and allows when it raises.
func TestDrainMovesEnabledFollowsSwitchesAndControl(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchange := newResidentMoveTestExchange(ctx, t, "alpha-edge-0")
	connect.AssertEqual(t, true, exchange.drainMovesEnabled())

	exchange.SetDrainMovesAllowed(func(ctx context.Context) bool {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the drain move control ran without a deadline")
		}
		return false
	})
	connect.AssertEqual(t, false, exchange.drainMovesEnabled())

	exchange.SetDrainMovesAllowed(func(context.Context) bool {
		panic(errors.New("synthetic control failure"))
	})
	connect.AssertEqual(t, true, exchange.drainMovesEnabled())

	exchange.SetDrainMovesAllowed(nil)
	exchange.settings.EnableDrainMoves = false
	connect.AssertEqual(t, false, exchange.drainMovesEnabled())
	exchange.settings.EnableDrainMoves = true
	exchange.settings.EnableMove = false
	connect.AssertEqual(t, false, exchange.drainMovesEnabled())
}

// The mover's breaker read applies the settings' thresholds per address,
// labels the gauge by the address's inventory host, interface and family, and
// answers open when it cannot tell.
func TestExchangeTargetBreakerOpenAppliesSettingsAndLabels(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		exchange := newResidentMoveTestExchange(ctx, t, "alpha-edge-0")
		failingIp := netip.MustParseAddr("198.51.100.21")
		healthyIp := netip.MustParseAddr("2001:db8::21")
		for range exchange.settings.MoveTargetBreakerMinSends {
			for _, ip := range []netip.Addr{failingIp, healthyIp} {
				connect.AssertEqual(t, nil, model.RecordResidentMoveTargetSend(ctx, ip, exchange.settings.MoveTargetBreakerWindow))
			}
		}
		// past the 0.2 ratio of 20 sends
		for range 5 {
			counted, err := model.RecordResidentMoveTargetFailure(ctx, failingIp)
			connect.AssertEqual(t, nil, err)
			connect.AssertEqual(t, true, counted)
		}

		connect.AssertEqual(t, true, exchange.TargetBreakerOpen(ctx, failingIp))
		connect.AssertEqual(t, float64(1), testutil.ToFloat64(residentMoveTargetBreakerOpenGauge.WithLabelValues("alpha-edge-1", "eno1np0", "4")))
		connect.AssertEqual(t, false, exchange.TargetBreakerOpen(ctx, healthyIp))
		connect.AssertEqual(t, float64(0), testutil.ToFloat64(residentMoveTargetBreakerOpenGauge.WithLabelValues("alpha-edge-1", "eno1np0", "6")))

		// a read that cannot complete answers open
		canceledCtx, canceledCancel := context.WithCancel(ctx)
		canceledCancel()
		connect.AssertEqual(t, true, exchange.TargetBreakerOpen(canceledCtx, healthyIp))
	})
}

// The load counts count a resident with provide intent on any of its
// transports once, whatever its transport count, and skip ended residents.
func TestResidentMoveLoadCountsProviderResidentsOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	residents := map[server.Id]*Resident{}
	newResident := func(edges ...ResidentTransportEdge) *Resident {
		residentCtx, residentCancel := context.WithCancel(ctx)
		client := connect.NewClient(residentCtx, connect.NewId(), connect.NewNoContractClientOob(), connect.DefaultClientSettings())
		t.Cleanup(func() {
			residentCancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		})
		resident := &Resident{
			ctx:        residentCtx,
			cancel:     residentCancel,
			client:     client,
			clientId:   server.NewId(),
			residentId: server.NewId(),
			transports: map[*clientTransport]bool{},
		}
		for _, edge := range edges {
			resident.transports[&clientTransport{edge: edge}] = true
		}
		residents[resident.clientId] = resident
		return resident
	}
	newResident(
		ResidentTransportEdge{Carrier: ResidentCarrierH1, ProvideIntent: true},
		ResidentTransportEdge{Carrier: ResidentCarrierH3, ProvideIntent: true},
		ResidentTransportEdge{Carrier: ResidentCarrierH3},
	)
	newResident(ResidentTransportEdge{Carrier: ResidentCarrierH1})
	newResident()
	ended := newResident(ResidentTransportEdge{Carrier: ResidentCarrierH1, ProvideIntent: true})
	ended.cancel()

	exchange := &Exchange{residents: residents}
	residentCount, providerResidentCount, transportCount := exchange.ResidentMoveLoadCounts()
	connect.AssertEqual(t, 3, residentCount)
	connect.AssertEqual(t, 1, providerResidentCount)
	connect.AssertEqual(t, 4, transportCount)
}
