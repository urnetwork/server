package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Server-directed moves end to end (MOVE.md §8): the intent handoff in the
// split-exchange topology, intent expiry, the compulsory deadline's moved close
// on each carrier, a drain that converges by intents, and the capability a
// client declares. These use the synthetic inventory of move_inventory_test.go:
// alpha-edge-0 is a source host and alpha-edge-1 a target host.
//
// Require the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.

// The env settings of an exchange on the given inventory host.
func residentMoveTestExchangeSettings(processHost string, mutate func(*ExchangeSettings)) func(*ExchangeSettings) {
	return func(exchangeSettings *ExchangeSettings) {
		// a moved resident must retire by its move, not by the registration
		// refresh at a quarter of this
		exchangeSettings.ExchangeResidentTtl = 2 * time.Minute
		exchangeSettings.edgeInventoryProcessHostForTest = processHost
		exchangeSettings.edgeInventoryLoadForTest = newEdgeInventoryTestLoader(edgeInventoryTestServicesYaml).load
		if mutate != nil {
			mutate(exchangeSettings)
		}
	}
}

// The targets of alpha-edge-1 in the synthetic inventory.
var residentMoveTestTargets = []netip.Addr{
	netip.MustParseAddr("198.51.100.21"),
	netip.MustParseAddr("2001:db8::21"),
}

// Waits until the client's local resident on the exchange satisfies test.
func waitForResidentMoveTestResident(
	ctx context.Context,
	t testing.TB,
	exchange *Exchange,
	clientId server.Id,
	test func(*Resident) bool,
) *Resident {
	t.Helper()
	endTime := time.Now().Add(60 * time.Second)
	for {
		if resident := exchange.localResident(clientId); resident != nil && test(resident) {
			return resident
		}
		if endTime.Before(time.Now()) {
			t.Fatalf("the resident of %s did not reach the expected state", clientId)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func anyResidentMoveTestResident(*Resident) bool {
	return true
}

// Waits until the exchange holds no resident of the client.
func waitForNoResidentMoveTestResident(ctx context.Context, t testing.TB, exchange *Exchange, clientId server.Id, timeout time.Duration) {
	t.Helper()
	endTime := time.Now().Add(timeout)
	for exchange.localResident(clientId) != nil {
		if endTime.Before(time.Now()) {
			t.Fatalf("the resident of %s stayed after %s", clientId, timeout)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// The split-exchange handoff (MOVE.md §5.3): the source sends a move with its
// intent and excuse; the client's replacement transport arrives at the target
// exchange, which takes the resident over by the intent instead of forwarding
// to it; the source retires its replaced resident within the poll interval, far
// inside the registration refresh, so two residents do not act together; the
// new resident relists the client's stream and routing reaches the client.
//
// Without the takeover the target forwards to the source's resident; without
// the poll the source keeps its resident until the refresh, 30 s here.
func TestResidentMoveHandsOffToTargetExchange(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		envA := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", nil))
		defer envA.Close()
		envB := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-1", nil))
		defer envB.Close()
		<-envA.exchange.EdgeInventory().Loaded()
		<-envB.exchange.EdgeInventory().Loaded()

		// both clients belong to envA's network; either exchange can host them
		clientIdX, byClientJwtX := envA.authClient(&model.AuthNetworkClientArgs{
			Description: "move x",
			DeviceSpec:  "spec x",
		})
		clientIdY, byClientJwtY := envA.authClient(&model.AuthNetworkClientArgs{
			Description: "move y",
			DeviceSpec:  "spec y",
		})
		clientX := envA.newClient(clientIdX)
		defer clientX.Close()
		clientY := envA.newClient(clientIdY)
		defer clientY.Close()
		receiveX := recordPeerReceives(clientX, clientIdY)
		// each exchange announces its new residents, and whether by intent
		recordArrivals := func(exchange *Exchange) chan bool {
			arrivals := make(chan bool, 8)
			exchange.SetResidentArrivalListener(func(clientId server.Id, byIntent bool) {
				if clientId == clientIdX {
					select {
					case arrivals <- byIntent:
					default:
					}
				}
			})
			return arrivals
		}
		arrivalsA := recordArrivals(envA.exchange)
		arrivalsB := recordArrivals(envB.exchange)

		instanceIdX := server.NewId()
		transportXA := envA.newTransport(byClientJwtX, instanceIdX, clientX.RouteManager())
		defer transportXA.Close()
		transportY := envA.newTransport(byClientJwtY, server.NewId(), clientY.RouteManager())
		defer transportY.Close()
		envA.setProvideModes(clientX, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})
		envA.setProvideModes(clientY, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})

		residentA := waitForResidentMoveTestResident(ctx, t, envA.exchange, clientIdX, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})
		connect.AssertEqual(t, false, <-arrivalsA)
		// a stream the client keeps across the move
		streamId := model.AddToStream(ctx, server.NewId(), clientIdX, clientIdY, nil)
		waitForResidentMoveTestResident(ctx, t, envA.exchange, clientIdX, func(resident *Resident) bool {
			return resident == residentA && resident.streamHopCount.Load() == 1
		})
		// the client declares moves; the library transport here predates the
		// declaration
		residentA.observeTransportEdge(ResidentTransportEdge{ResidentMoveVersion: residentMoveTargetVersion})

		candidates := envA.exchange.ResidentMoveCandidates()
		candidateIndex := slices.IndexFunc(candidates, func(candidate ResidentMoveCandidate) bool {
			return candidate.ClientId == clientIdX
		})
		if candidateIndex < 0 {
			t.Fatal("the resident is not a move candidate")
		}
		candidate := candidates[candidateIndex]
		connect.AssertEqual(t, residentA.residentId, candidate.ResidentId)
		connect.AssertEqual(t, true, candidate.Capable())
		connect.AssertEqual(t, true, candidate.DirectCarrier())
		connect.AssertEqual(t, 1, candidate.StreamCount)
		connect.AssertEqual(t, false, candidate.MoveOutstanding)
		connect.AssertEqual(t, false, candidate.ArrivedByMove)
		connect.AssertEqual(t, true, time.Since(candidate.CreateTime) < time.Minute)

		moveId, err := envA.exchange.SendResidentMove(ctx, ResidentMove{
			ClientId:   clientIdX,
			ResidentId: residentA.residentId,
			Kind:       model.ResidentMoveKindAdvisory,
			TargetHost: "alpha-edge-1",
			Targets:    residentMoveTestTargets,
		})
		connect.AssertEqual(t, nil, err)
		intent, err := model.GetResidentMoveIntent(ctx, clientIdX)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, moveId, intent.MoveId)
		connect.AssertEqual(t, model.ResidentMoveKindAdvisory, intent.Kind)
		connect.AssertEqual(t, residentA.residentId, intent.FromResidentId)
		connect.AssertEqual(t, envA.exchange.ExchangeId(), intent.FromExchangeId)
		connect.AssertEqual(t, "alpha-edge-1", intent.TargetHost)
		connect.AssertEqual(t, residentMoveTestTargets, intent.Targets)
		// the excuse markers precede the send (D16)
		connect.AssertEqual(t, true, model.HasDrainExcuse(ctx, clientIdX))
		connect.AssertEqual(t, true, model.HasDrainProvideChangeExcuse(ctx, clientIdX))
		// one move at a time
		_, err = envA.exchange.SendResidentMove(ctx, ResidentMove{
			ClientId:   clientIdX,
			Kind:       model.ResidentMoveKindAdvisory,
			TargetHost: "alpha-edge-1",
			Targets:    residentMoveTestTargets,
		})
		connect.AssertEqual(t, true, errors.Is(err, ErrResidentMoveOutstanding))

		// the client's replacement transport arrives at the target exchange
		arrivedBefore := testutil.ToFloat64(residentMovesArrivedCounter.WithLabelValues(string(model.ResidentMoveKindAdvisory), "alpha-edge-1"))
		transportXB := envB.newTransport(byClientJwtX, instanceIdX, clientX.RouteManager())
		defer transportXB.Close()
		residentB := waitForResidentMoveTestResident(ctx, t, envB.exchange, clientIdX, anyResidentMoveTestResident)
		takeoverTime := time.Now()
		connect.AssertEqual(t, moveId, residentB.arrivedMoveId)
		connect.AssertEqual(t, true, <-arrivalsB)
		current := model.GetResidentForClient(ctx, clientIdX, 0)
		connect.AssertEqual(t, residentB.residentId, current.ResidentId)
		taken, err := model.GetResidentMoveIntent(ctx, clientIdX)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, true, taken == nil)
		connect.AssertEqual(t, arrivedBefore+1, testutil.ToFloat64(residentMovesArrivedCounter.WithLabelValues(string(model.ResidentMoveKindAdvisory), "alpha-edge-1")))

		// the source retires the replaced resident within the poll interval
		select {
		case <-residentA.Done():
		case <-time.After(envA.exchange.settings.MoveIntentPollInterval + 2*time.Second):
			t.Fatal("the replaced resident was not retired within the poll interval")
		}
		t.Logf("retired %s after the takeover", time.Since(takeoverTime))
		connect.AssertEqual(t, true, residentA.moved.Load())
		waitForNoResidentMoveTestResident(ctx, t, envA.exchange, clientIdX, 10*time.Second)
		connect.AssertEqual(t, false, residentB.IsDone())

		// the new resident relists the client's stream
		waitForResidentMoveTestResident(ctx, t, envB.exchange, clientIdX, func(resident *Resident) bool {
			return resident == residentB && resident.streamHopCount.Load() == 1
		})
		_, streamHops := model.GetActiveStreamHops(ctx, clientIdX)
		relisted := false
		for streamHop := range streamHops {
			if streamHop.StreamId() == streamId {
				relisted = true
			}
		}
		connect.AssertEqual(t, true, relisted)

		// routing reaches the client through its new resident
		sendSimpleMessage(t, clientY, clientIdX)
		select {
		case <-receiveX:
		case <-time.After(60 * time.Second):
			t.Fatal("no message reached the moved client")
		}
	})
}

// A move whose client never arrives ends with its intent: the source counts it
// expired, stops watching, keeps its resident, and can move it again.
func TestResidentMoveExpiresWithoutArrival(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		arrivalTimeout := 2 * time.Second
		env := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", func(exchangeSettings *ExchangeSettings) {
			exchangeSettings.MoveArrivalTimeout = arrivalTimeout
		}))
		defer env.Close()
		<-env.exchange.EdgeInventory().Loaded()

		clientId, byClientJwt := env.authClient(&model.AuthNetworkClientArgs{
			Description: "move expiry",
			DeviceSpec:  "spec",
		})
		client := env.newClient(clientId)
		defer client.Close()
		transport := env.newTransport(byClientJwt, server.NewId(), client.RouteManager())
		defer transport.Close()
		resident := waitForResidentMoveTestResident(ctx, t, env.exchange, clientId, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})
		resident.observeTransportEdge(ResidentTransportEdge{ResidentMoveVersion: residentMoveTargetVersion})

		expiredBefore := testutil.ToFloat64(residentMovesExpiredCounter.WithLabelValues(string(model.ResidentMoveKindAdvisory)))
		move := ResidentMove{
			ClientId:   clientId,
			Kind:       model.ResidentMoveKindAdvisory,
			TargetHost: "alpha-edge-1",
			Targets:    residentMoveTestTargets,
		}
		_, err := env.exchange.SendResidentMove(ctx, move)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, true, resident.move.Load() != nil)

		endTime := time.Now().Add(arrivalTimeout + 10*time.Second)
		for resident.move.Load() != nil {
			if endTime.Before(time.Now()) {
				t.Fatal("the move outlived its arrival window")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		intent, err := model.GetResidentMoveIntent(ctx, clientId)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, true, intent == nil)
		connect.AssertEqual(t, expiredBefore+1, testutil.ToFloat64(residentMovesExpiredCounter.WithLabelValues(string(model.ResidentMoveKindAdvisory))))
		// the resident stays, current, and takes another move
		connect.AssertEqual(t, false, resident.IsDone())
		connect.AssertEqual(t, false, resident.moved.Load())
		connect.AssertEqual(t, resident.residentId, model.GetResidentForClient(ctx, clientId, 0).ResidentId)
		_, err = env.exchange.SendResidentMove(ctx, move)
		connect.AssertEqual(t, nil, err)
	})
}

// A raw H1 client authenticated by header, so the test reads exactly what
// the server writes.
func dialResidentMoveTestH1(
	ctx context.Context,
	t testing.TB,
	env *peerDiscoveryEnv,
	byClientJwt string,
	instanceId server.Id,
	residentMoveVersion string,
	provideIntent bool,
) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+byClientJwt)
	header.Set("X-UR-InstanceId", instanceId.String())
	header.Set("X-UR-AppVersion", "0.0.0")
	if residentMoveVersion != "" {
		header.Set(HeaderResidentMoveVersion, residentMoveVersion)
	}
	if provideIntent {
		header.Set(HeaderProvideIntent, "1")
	}
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	ws, _, err := dialer.DialContext(ctx, fmt.Sprintf("ws://127.0.0.1:%d/", env.port), header)
	if err != nil {
		t.Fatalf("h1 dial: %v", err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

// Reads a raw H1 client until the server closes it, reporting whether the moved
// close message arrived and the close error.
func readResidentMoveTestH1UntilClose(ws *websocket.Conn) (movedControl bool, closeErr error) {
	for {
		if err := ws.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return movedControl, err
		}
		messageType, message, err := ws.ReadMessage()
		if err != nil {
			return movedControl, err
		}
		if messageType == websocket.BinaryMessage && len(message) == 5 &&
			message[0] == connect.TransportControlClose && binary.BigEndian.Uint32(message[1:5]) == 6 {
			movedControl = true
		}
	}
}

// A compulsory move's deadline closes the client's H1 transport with reason 6
// and close code 4006 when its resident is still here, and retires the resident
// so it nominates afresh. The H1 header declared the move version and the
// resident recorded it with the H1 carrier.
//
// Without the deadline nothing closes the client.
func TestResidentMoveCompulsoryDeadlineClosesH1WithMoved(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		compulsoryTimeout := time.Second
		env := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", func(exchangeSettings *ExchangeSettings) {
			exchangeSettings.MoveCompulsoryTimeout = compulsoryTimeout
		}))
		defer env.Close()
		<-env.exchange.EdgeInventory().Loaded()

		clientId, byClientJwt := env.authClient(&model.AuthNetworkClientArgs{
			Description: "move h1",
			DeviceSpec:  "spec",
		})
		ws := dialResidentMoveTestH1(ctx, t, env, byClientJwt, server.NewId(), "1", false)
		resident := waitForResidentMoveTestResident(ctx, t, env.exchange, clientId, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})
		connect.AssertEqual(t, uint32(1), resident.moveVersion.Load())
		connect.AssertEqual(t, []ResidentTransportEdge{{ResidentMoveVersion: 1, Carrier: ResidentCarrierH1, IpFamily: 4}}, resident.moveCandidate().Transports)

		closedBefore := testutil.ToFloat64(residentMovesCompulsoryClosedCounter)
		closed := make(chan error, 1)
		movedControl := make(chan bool, 1)
		go func() {
			moved, err := readResidentMoveTestH1UntilClose(ws)
			movedControl <- moved
			closed <- err
		}()
		_, err := env.exchange.SendResidentMove(ctx, ResidentMove{
			ClientId:   clientId,
			Kind:       model.ResidentMoveKindCompulsory,
			TargetHost: "alpha-edge-1",
			Targets:    residentMoveTestTargets,
		})
		connect.AssertEqual(t, nil, err)

		select {
		case err := <-closed:
			connect.AssertEqual(t, true, <-movedControl)
			var closeErr *websocket.CloseError
			if !errors.As(err, &closeErr) || closeErr.Code != 4006 {
				t.Fatalf("close = %v, want code 4006", err)
			}
		case <-time.After(compulsoryTimeout + 30*time.Second):
			t.Fatal("the deadline did not close the client")
		}
		select {
		case <-resident.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("the resident outlived the deadline")
		}
		connect.AssertEqual(t, true, resident.moved.Load())
		connect.AssertEqual(t, closedBefore+1, testutil.ToFloat64(residentMovesCompulsoryClosedCounter))
	})
}

// A raw H3 client: the auth frame declares the move version, and the test
// keeps the stream open until the server closes the connection.
func dialResidentMoveTestH3(
	ctx context.Context,
	t testing.TB,
	env *peerDiscoveryEnv,
	byClientJwt string,
	instanceId server.Id,
	residentMoveVersion uint32,
	provideIntent bool,
	serving *sync.WaitGroup,
) (*quic.Conn, <-chan error) {
	t.Helper()
	listener, clientTlsConfig := listenConnectQuicTestEarly(t, env.handler.settings)
	source := newConnectQuicTestSource(t, deliverConnectQuicTestAll)
	conn, dialed := acceptConnectQuicTestSource(ctx, t, listener, clientTlsConfig, source)
	served := make(chan error, 1)
	serving.Go(func() {
		served <- env.handler.connectQuicWithCarrier(conn, ResidentCarrierH3)
	})
	client := waitForConnectQuicTestDial(ctx, t, dialed)
	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	authFrame, err := connect.EncodeFrame(&protocol.Auth{
		ByJwt:               byClientJwt,
		AppVersion:          "0.0.0",
		InstanceId:          instanceId.Bytes(),
		ResidentMoveVersion: residentMoveVersion,
		ProvideIntent:       provideIntent,
	}, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer connect.MessagePoolReturn(authFrame)
	framer := connect.NewFramer(DefaultConnectHandlerSettings().FramerSettings)
	if err := framer.Write(stream, authFrame); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	echo, err := framer.Read(stream)
	if err != nil {
		t.Fatalf("read auth echo: %v", err)
	}
	connect.MessagePoolReturn(echo)
	// keep the stream read so the server's frames do not stall it
	go func() {
		for {
			message, err := framer.Read(stream)
			if err != nil {
				return
			}
			connect.MessagePoolReturn(message)
		}
	}()
	return client, served
}

// The same deadline on H3 closes the connection with application error 4006.
// The Auth field declared the move version and the resident recorded it with
// the H3 carrier.
func TestResidentMoveCompulsoryDeadlineClosesH3WithMoved(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		var serving sync.WaitGroup
		defer serving.Wait()
		defer cancel()

		compulsoryTimeout := time.Second
		env := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", func(exchangeSettings *ExchangeSettings) {
			exchangeSettings.MoveCompulsoryTimeout = compulsoryTimeout
		}))
		defer env.Close()
		<-env.exchange.EdgeInventory().Loaded()

		clientId, byClientJwt := env.authClient(&model.AuthNetworkClientArgs{
			Description: "move h3",
			DeviceSpec:  "spec",
		})
		client, _ := dialResidentMoveTestH3(ctx, t, env, byClientJwt, server.NewId(), 1, false, &serving)
		resident := waitForResidentMoveTestResident(ctx, t, env.exchange, clientId, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})
		connect.AssertEqual(t, uint32(1), resident.moveVersion.Load())
		connect.AssertEqual(t, []ResidentTransportEdge{{ResidentMoveVersion: 1, Carrier: ResidentCarrierH3, IpFamily: 4}}, resident.moveCandidate().Transports)

		_, err := env.exchange.SendResidentMove(ctx, ResidentMove{
			ClientId:   clientId,
			Kind:       model.ResidentMoveKindCompulsory,
			TargetHost: "alpha-edge-1",
			Targets:    residentMoveTestTargets,
		})
		connect.AssertEqual(t, nil, err)

		select {
		case <-client.Context().Done():
		case <-time.After(compulsoryTimeout + 30*time.Second):
			t.Fatal("the deadline did not close the client")
		}
		var applicationErr *quic.ApplicationError
		if err := context.Cause(client.Context()); !errors.As(err, &applicationErr) || !applicationErr.Remote || applicationErr.ErrorCode != 4006 {
			t.Fatalf("close = %v, want application error 4006", err)
		}
		select {
		case <-resident.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("the resident outlived the deadline")
		}
		connect.AssertEqual(t, true, resident.moved.Load())
	})
}

// A client without a move version gets no advisory move, and a compulsory
// move reaches it as the legacy migrate with the deadline (MOVE.md §5.6).
func TestResidentMoveLegacyClientGetsOnlyCompulsoryMoves(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", nil))
		defer env.Close()
		<-env.exchange.EdgeInventory().Loaded()

		clientId, byClientJwt := env.authClient(&model.AuthNetworkClientArgs{
			Description: "move legacy",
			DeviceSpec:  "spec",
		})
		client := env.newClient(clientId)
		defer client.Close()
		migrates := make(chan *protocol.ResidentMigrate, 4)
		client.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
			if !source.IsControlSource() {
				return
			}
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_TransferResidentMigrate {
					continue
				}
				if message, err := connect.FromFrame(frame); err == nil {
					select {
					case migrates <- message.(*protocol.ResidentMigrate):
					default:
					}
				}
			}
		})
		// a transport that declares no move version, as a client from before
		// moves does
		transportSettings := connect.DefaultPlatformTransportSettings()
		transportSettings.EnableMove = false
		transportSettings.QuicTlsConfig.InsecureSkipVerify = true
		transportSettings.H3Port = env.h3Port
		transportSettings.DnsPort = 0
		transport := connect.NewPlatformTransportWithTargetMode(
			ctx,
			connect.NewClientStrategyWithDefaults(ctx),
			client.RouteManager(),
			fmt.Sprintf("ws://127.0.0.1:%d", env.port),
			&connect.ClientAuth{
				ByJwt:      byClientJwt,
				InstanceId: connect.Id(server.NewId()),
				AppVersion: "0.0.0",
			},
			connect.TransportModeH1,
			transportSettings,
		)
		defer transport.Close()
		resident := waitForResidentMoveTestResident(ctx, t, env.exchange, clientId, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})
		connect.AssertEqual(t, uint32(0), resident.moveVersion.Load())

		move := ResidentMove{
			ClientId:   clientId,
			Kind:       model.ResidentMoveKindAdvisory,
			TargetHost: "alpha-edge-1",
			Targets:    residentMoveTestTargets,
		}
		_, err := env.exchange.SendResidentMove(ctx, move)
		connect.AssertEqual(t, true, errors.Is(err, ErrResidentMoveNotCapable))
		intent, err := model.GetResidentMoveIntent(ctx, clientId)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, true, intent == nil)

		move.Kind = model.ResidentMoveKindCompulsory
		moveId, err := env.exchange.SendResidentMove(ctx, move)
		connect.AssertEqual(t, nil, err)
		select {
		case migrate := <-migrates:
			// only the legacy field
			connect.AssertEqual(t, 0, len(migrate.MoveId))
			connect.AssertEqual(t, protocol.ResidentMoveKind_ResidentMoveKindDrain, migrate.Kind)
			connect.AssertEqual(t, 0, len(migrate.Targets))
			connect.AssertEqual(t, uint64(0), migrate.CloseTime)
			connect.AssertEqual(t, true, 0 < migrate.MigrateTime)
		case <-time.After(30 * time.Second):
			t.Fatal("the legacy client received no migrate")
		}
		// the intent still lets an arrival take over, without targets to report on
		intent, err = model.GetResidentMoveIntent(ctx, clientId)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, moveId, intent.MoveId)
		connect.AssertEqual(t, 0, len(intent.Targets))
	})
}

// A capable client receives the move's fields: its id, kind, targets, deadline
// and the preference duration (MOVE.md §5.1).
func TestResidentMoveCapableClientGetsMoveFields(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", nil))
		defer env.Close()
		<-env.exchange.EdgeInventory().Loaded()

		clientId, byClientJwt := env.authClient(&model.AuthNetworkClientArgs{
			Description: "move capable",
			DeviceSpec:  "spec",
		})
		client := env.newClient(clientId)
		defer client.Close()
		migrates := make(chan *protocol.ResidentMigrate, 4)
		client.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
			if !source.IsControlSource() {
				return
			}
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_TransferResidentMigrate {
					continue
				}
				if message, err := connect.FromFrame(frame); err == nil {
					select {
					case migrates <- message.(*protocol.ResidentMigrate):
					default:
					}
				}
			}
		})
		transport := env.newTransport(byClientJwt, server.NewId(), client.RouteManager())
		defer transport.Close()
		resident := waitForResidentMoveTestResident(ctx, t, env.exchange, clientId, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})
		resident.observeTransportEdge(ResidentTransportEdge{ResidentMoveVersion: residentMoveTargetVersion})

		// the mover mints the move id it keys the client's cooldown with
		minted := server.NewId()
		migrateTime := time.Now().Add(5 * time.Second).Truncate(time.Millisecond)
		moveId, err := env.exchange.SendResidentMove(ctx, ResidentMove{
			MoveId:      minted,
			ClientId:    clientId,
			Kind:        model.ResidentMoveKindCompulsory,
			TargetHost:  "alpha-edge-1",
			Targets:     residentMoveTestTargets,
			MigrateTime: migrateTime,
		})
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, minted, moveId)
		select {
		case migrate := <-migrates:
			connect.AssertEqual(t, moveId.Bytes(), migrate.MoveId)
			connect.AssertEqual(t, protocol.ResidentMoveKind_ResidentMoveKindCompulsory, migrate.Kind)
			connect.AssertEqual(t, uint64(migrateTime.UnixMilli()), migrate.MigrateTime)
			connect.AssertEqual(t, uint64(migrateTime.Add(env.exchange.settings.MoveCompulsoryTimeout).UnixMilli()), migrate.CloseTime)
			connect.AssertEqual(t, uint64(env.exchange.settings.MovePreferDuration.Milliseconds()), migrate.PreferDuration)
			targets := []netip.Addr{}
			for _, target := range migrate.Targets {
				target, ok := netip.AddrFromSlice(target.Ip)
				connect.AssertEqual(t, true, ok)
				targets = append(targets, target)
			}
			connect.AssertEqual(t, residentMoveTestTargets, targets)
			// a v4 target is 4 bytes on the wire
			connect.AssertEqual(t, 4, len(migrate.Targets[0].Ip))
		case <-time.After(30 * time.Second):
			t.Fatal("the capable client received no move")
		}
	})
}

// A drain sends every local resident a compulsory move to its own host with a
// drain intent. A migrated client's new transport takes the resident over at
// the sibling as soon as it arrives, instead of forwarding back into the
// draining resident, its reconnect is excused, and the drain converges at the
// move deadlines plus the sweep rather than running long.
//
// Without drain intents the sibling forwards back into the draining resident
// until the sweep closes it.
func TestExchangeDrainMovesConvergeByIntent(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		excusedStatsRecorded := make(chan server.Id, 4)

		drainAllTimeout := 10 * time.Second
		drainMigrateWindow := 2 * time.Second
		compulsoryTimeout := 3 * time.Second
		drainStragglerSweepTimeout := time.Second
		mutateExchangeSettings := func(exchangeSettings *ExchangeSettings) {
			exchangeSettings.EnableDrainExcuse = true
			exchangeSettings.EnableDrainCoordination = true
			exchangeSettings.EnableMove = true
			exchangeSettings.EnableDrainMoves = true
			exchangeSettings.DrainExcuseTtl = time.Minute
			exchangeSettings.DrainAllTimeout = drainAllTimeout
			exchangeSettings.DrainMigrateWindow = drainMigrateWindow
			exchangeSettings.MoveCompulsoryTimeout = compulsoryTimeout
			exchangeSettings.DrainStragglerSweepTimeout = drainStragglerSweepTimeout
			exchangeSettings.DrainOneTimeout = 50 * time.Millisecond
		}
		mutateHandlerSettings := func(handlerSettings *ConnectHandlerSettings) {
			handlerSettings.ConnectionAnnounceTimeout = 100 * time.Millisecond
			handlerSettings.ConnectionAnnounceSettings.SyncConnectionTimeout = 200 * time.Millisecond
			handlerSettings.ConnectionAnnounceSettings.reliabilityStatsRecordedForTest = func(
				clientId server.Id,
				stats model.ClientReliabilityStats,
			) {
				if stats.ConnectionExcusedNewCount == 0 {
					return
				}
				select {
				case excusedStatsRecorded <- clientId:
				default:
				}
			}
		}
		env1 := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", mutateExchangeSettings), mutateHandlerSettings)
		defer env1.Close()
		env2 := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", mutateExchangeSettings), mutateHandlerSettings)
		defer env2.Close()
		<-env1.exchange.EdgeInventory().Loaded()
		<-env2.exchange.EdgeInventory().Loaded()

		lb := newTestLb(t, 0, env1.port)
		defer lb.Close()

		clientIdA, byClientJwtA := env1.authClient(&model.AuthNetworkClientArgs{
			Description: "drain move a",
			DeviceSpec:  "spec a",
		})
		clientA := env1.newClient(clientIdA)
		defer clientA.Close()
		migratingA := newMigratingTransport(ctx, clientA, byClientJwtA, server.NewId(), lb.port(), 0, 0)
		defer migratingA.Close()
		// public provide: the reliability stats (and so the excuse recording)
		// only apply to public providers
		env1.setProvideModes(clientA, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
			protocol.ProvideMode_Public:  true,
		})
		residentA := waitForResidentMoveTestResident(ctx, t, env1.exchange, clientIdA, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})

		// past the announce transition of both services, so the migrated
		// connection announces as new and consumes the excuse
		select {
		case <-ctx.Done():
			return
		case <-time.After(2*drainAllTimeout + time.Second):
		}
		waitForNextReliabilityBlock(ctx)

		arrivedBefore := testutil.ToFloat64(residentMovesArrivedCounter.WithLabelValues(string(model.ResidentMoveKindDrain), "alpha-edge-0"))
		closedBefore := testutil.ToFloat64(residentMovesCompulsoryClosedCounter)
		// new dials land on the sibling
		lb.setBackendPort(env2.port)
		drainDone := make(chan time.Duration, 1)
		go func() {
			drainStartTime := time.Now()
			env1.exchange.Drain()
			drainDone <- time.Since(drainStartTime)
		}()

		// the migrated client takes over at the sibling by the drain intent,
		// before the draining resident's deadline
		residentB := waitForResidentMoveTestResident(ctx, t, env2.exchange, clientIdA, anyResidentMoveTestResident)
		connect.AssertEqual(t, false, residentB.arrivedMoveId == (server.Id{}))
		connect.AssertEqual(t, arrivedBefore+1, testutil.ToFloat64(residentMovesArrivedCounter.WithLabelValues(string(model.ResidentMoveKindDrain), "alpha-edge-0")))
		connect.AssertEqual(t, residentB.residentId, model.GetResidentForClient(ctx, clientIdA, 0).ResidentId)

		var drainDuration time.Duration
		select {
		case drainDuration = <-drainDone:
		case <-time.After(2 * drainAllTimeout):
			t.Fatal("drain did not return")
		}
		t.Logf("drain took %s", drainDuration)
		// the move window, the deadline and the sweep, not the drain deadline
		if drainMigrateWindow+compulsoryTimeout+drainStragglerSweepTimeout+3*time.Second < drainDuration {
			t.Fatalf("the drain took %s", drainDuration)
		}
		// the client had moved, so its deadline retired the resident without
		// closing the client
		connect.AssertEqual(t, true, residentA.IsDone())
		connect.AssertEqual(t, closedBefore, testutil.ToFloat64(residentMovesCompulsoryClosedCounter))
		connect.AssertEqual(t, int64(1), migratingA.migratedCount.Load())

		// the reconnect is excused (the move's excuse markers, D16)
		select {
		case clientId := <-excusedStatsRecorded:
			connect.AssertEqual(t, clientIdA, clientId)
		case <-time.After(30 * time.Second):
			t.Fatal("the moved client's reconnect was not excused")
		}
	})
}

// Each edge carries the transport's provide intent to the resident, wherever
// it lives: an H1 transport by header, an H3 transport by its Auth field, and
// one that forwards from another exchange in the exchange header. The load
// counts count the provider resident once (MOVE.md §5.4).
func TestResidentTransportEdgeCarriesProvideIntent(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		var serving sync.WaitGroup
		defer serving.Wait()
		defer cancel()

		envA := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", nil))
		defer envA.Close()
		envB := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-1", nil))
		defer envB.Close()

		edgesOf := func(resident *Resident) []ResidentTransportEdge {
			return resident.moveCandidate().Transports
		}

		// an H1 transport declares provide intent by header
		clientIdH1, byClientJwtH1 := envA.authClient(&model.AuthNetworkClientArgs{
			Description: "provide h1",
			DeviceSpec:  "spec",
		})
		dialResidentMoveTestH1(ctx, t, envA, byClientJwtH1, server.NewId(), "", true)
		residentH1 := waitForResidentMoveTestResident(ctx, t, envA.exchange, clientIdH1, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})
		connect.AssertEqual(t, []ResidentTransportEdge{{Carrier: ResidentCarrierH1, IpFamily: 4, ProvideIntent: true}}, edgesOf(residentH1))

		// an H3 transport declares it in its Auth frame
		clientIdH3, byClientJwtH3 := envA.authClient(&model.AuthNetworkClientArgs{
			Description: "provide h3",
			DeviceSpec:  "spec",
		})
		dialResidentMoveTestH3(ctx, t, envA, byClientJwtH3, server.NewId(), 0, true, &serving)
		residentH3 := waitForResidentMoveTestResident(ctx, t, envA.exchange, clientIdH3, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})
		connect.AssertEqual(t, []ResidentTransportEdge{{Carrier: ResidentCarrierH3, IpFamily: 4, ProvideIntent: true}}, edgesOf(residentH3))

		// a client without intent, whose resident is on A, opens a provider
		// transport on B: B forwards it to A in the exchange header
		clientIdSplit, byClientJwtSplit := envA.authClient(&model.AuthNetworkClientArgs{
			Description: "provide split",
			DeviceSpec:  "spec",
		})
		instanceIdSplit := server.NewId()
		dialResidentMoveTestH1(ctx, t, envA, byClientJwtSplit, instanceIdSplit, "", false)
		residentSplit := waitForResidentMoveTestResident(ctx, t, envA.exchange, clientIdSplit, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})
		dialResidentMoveTestH1(ctx, t, envB, byClientJwtSplit, instanceIdSplit, "1", true)
		waitForResidentMoveTestResident(ctx, t, envA.exchange, clientIdSplit, func(resident *Resident) bool {
			return resident == residentSplit && resident.TransportCount() == 2
		})
		forwarded := 0
		for _, edge := range edgesOf(residentSplit) {
			if edge.ProvideIntent {
				forwarded += 1
				connect.AssertEqual(t, ResidentTransportEdge{ResidentMoveVersion: 1, Carrier: ResidentCarrierH1, IpFamily: 4, ProvideIntent: true}, edge)
			}
		}
		connect.AssertEqual(t, 1, forwarded)
		connect.AssertEqual(t, true, envB.exchange.localResident(clientIdSplit) == nil)

		// three residents, each a provider once, over four transports
		residentCount, providerResidentCount, transportCount := envA.exchange.ResidentMoveLoadCounts()
		connect.AssertEqual(t, 3, residentCount)
		connect.AssertEqual(t, 3, providerResidentCount)
		connect.AssertEqual(t, 4, transportCount)
	})
}
