package connect

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// testingCounterValue reads a prometheus counter's current value
func testingCounterValue(counter interface{ Write(*dto.Metric) error }) float64 {
	var m dto.Metric
	if err := counter.Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// waitForNextReliabilityBlock aligns the caller to just after the next
// reliability block boundary, so a following drain lands its excused
// reconnect in a block free of the test setup's organic provide changes (in
// production these are minutes apart; the compressed test would otherwise
// share a block ~half the time)
func waitForNextReliabilityBlock(ctx context.Context) {
	nextBlockTime := time.UnixMilli((testingReliabilityBlockNumber(server.NowUtc()) + 1) * int64(model.ReliabilityBlockDuration/time.Millisecond)).Add(1 * time.Second)
	if wait := time.Until(nextBlockTime); 0 < wait {
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

// watchPeerBlip polls the observer's peer view and records whether the watched
// client ever disappears or shows as disconnected. Start it once the observer
// sees the watched client; stop with the returned cancel.
func watchPeerBlip(ctx context.Context, observer *connect.Client, watchedClientId server.Id) (blipped *atomic.Bool, stop func()) {
	watchCtx, watchCancel := context.WithCancel(ctx)
	blipped = &atomic.Bool{}
	go func() {
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			connected, disconnectedCount := observer.NetworkPeers()
			if findPeer(connected, watchedClientId) == nil || 0 < disconnectedCount {
				blipped.Store(true)
			}
		}
	}()
	return blipped, watchCancel
}

// A whole-service drain with the excuse path (Track A, coordination off so
// the clients redial the same service): the drained providers' reconnects are
// recorded as excused and the drain leaves no persistent peer disconnect
// marker (CONNECTDRAIN2.md §3.1, §3.2).
//
// Requires the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.
func TestExchangeDrainExcuseE2e(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// DrainAllTimeout also sets the announce transition time
		// (2x DrainAllTimeout after start): after that, a reconnect announces
		// as a new connection and consumes the excuse marker
		drainAllTimeout := 3 * time.Second

		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, 8103, 9023,
			func(exchangeSettings *ExchangeSettings) {
				exchangeSettings.EnableDrainExcuse = true
				// the clients must be able to redial this same service
				exchangeSettings.EnableDrainCoordination = false
				exchangeSettings.DrainExcuseTtl = 1 * time.Minute
				exchangeSettings.DrainAllTimeout = drainAllTimeout
				exchangeSettings.DrainStragglerSweepTimeout = 2 * time.Second
				exchangeSettings.DrainOneTimeout = 50 * time.Millisecond
			},
			func(handlerSettings *ConnectHandlerSettings) {
				handlerSettings.ConnectionAnnounceTimeout = 100 * time.Millisecond
				handlerSettings.ConnectionAnnounceSettings.SyncConnectionTimeout = 200 * time.Millisecond
			})
		defer env.Close()

		clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{
			Description: "drain a",
			DeviceSpec:  "spec a",
		})
		clientIdB, byClientJwtB := env.authClient(&model.AuthNetworkClientArgs{
			Description: "drain b",
			DeviceSpec:  "spec b",
		})

		clientA := env.newClient(clientIdA)
		defer clientA.Close()
		clientB := env.newClient(clientIdB)
		defer clientB.Close()

		startBlock := testingReliabilityBlockNumber(server.NowUtc()) - 1

		transportA := env.newTransport(byClientJwtA, server.NewId(), clientA.RouteManager())
		defer transportA.Close()
		transportB := env.newTransport(byClientJwtB, server.NewId(), clientB.RouteManager())
		defer transportB.Close()

		// public provide: the reliability stats (and so the excuse recording)
		// only apply to public providers
		env.setProvideModes(clientA, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
			protocol.ProvideMode_Public:  true,
		})
		env.setProvideModes(clientB, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
			protocol.ProvideMode_Public:  true,
		})

		ok := waitForPeers(ctx, clientB, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdA) != nil
		})
		connect.AssertEqual(t, true, ok)

		// wait past the announce transition so the post-drain reconnect runs
		// the new-connection branch
		select {
		case <-ctx.Done():
			return
		case <-time.After(2*drainAllTimeout + 1*time.Second):
		}

		excusesWrittenBefore := testingCounterValue(drainExcusesWrittenCounter)

		drainStartTime := time.Now()
		env.exchange.Drain()
		connect.AssertEqual(t, true, time.Since(drainStartTime) < 15*time.Second)

		// the drain synchronously wrote an excuse marker for each of the two
		// providers it evicted (deterministic: Drain marks every resident up
		// front)
		excusesWritten := testingCounterValue(drainExcusesWrittenCounter) - excusesWrittenBefore
		connect.AssertEqual(t, true, 2 <= excusesWritten)

		// the redialing connections consume the markers. Whichever bounce
		// consumes the marker records an excused reconnect; across the fleet
		// at least one excused reconnect lands (the strict per-client
		// excused-recording is asserted deterministically in
		// TestConnectionAnnounceDrainExcuse and TestExchangeDrainMigrateE2e).
		var totals testingReliabilityTotals
		anyExcused := false
		endTime := time.Now().Add(90 * time.Second)
		for {
			model.RollupClientReliabilityStats(ctx, server.NowUtc().Add(3*model.ReliabilityBlockDuration))
			endBlock := testingReliabilityBlockNumber(server.NowUtc()) + 1
			for _, clientId := range []server.Id{clientIdA, clientIdB} {
				if 1 <= testingReadClientReliabilityTotals(ctx, clientId, startBlock, endBlock).excusedNew {
					anyExcused = true
				}
			}
			totals = testingReadClientReliabilityTotals(ctx, clientIdA, startBlock, endBlock)
			// the markers are consumed once the clients have redialed
			markersConsumed := !model.HasDrainExcuse(ctx, clientIdA) && !model.HasDrainExcuse(ctx, clientIdB)
			if anyExcused && markersConsumed {
				break
			}
			if endTime.Before(time.Now()) {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
		connect.AssertEqual(t, true, anyExcused)
		connect.AssertEqual(t, false, model.HasDrainExcuse(ctx, clientIdA))
		connect.AssertEqual(t, false, model.HasDrainExcuse(ctx, clientIdB))
		connect.AssertEqual(t, true, totals.anyValid)

		// the drain left no persistent disconnect marker: the observer still
		// sees the provider once the dust settles
		ok = waitForPeers(ctx, clientB, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdA) != nil && disconnectedCount == 0
		})
		connect.AssertEqual(t, true, ok)
	})
}

// testLb is a minimal tcp load balancer: new connections dial the current
// backend; existing connections keep piping after a backend switch, which is
// what lets make-before-break overlap the old and new transports
type testLb struct {
	listener    net.Listener
	backendPort atomic.Int64
}

func newTestLb(t testing.TB, lbPort int, backendPort int) *testLb {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", lbPort))
	connect.AssertEqual(t, err, nil)
	lb := &testLb{
		listener: listener,
	}
	lb.backendPort.Store(int64(backendPort))
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				backend, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", lb.backendPort.Load()))
				if err != nil {
					return
				}
				defer backend.Close()
				go func() {
					defer conn.Close()
					defer backend.Close()
					io.Copy(backend, conn)
				}()
				io.Copy(conn, backend)
			}()
		}
	}()
	return lb
}

func (self *testLb) setBackendPort(backendPort int) {
	self.backendPort.Store(int64(backendPort))
}

func (self *testLb) Close() {
	self.listener.Close()
}

// migratingTransport replicates the sdk's make-before-break handling with raw
// connect primitives: on a `ResidentMigrate` control frame, build a
// replacement platform transport through the lb at the migrate time, wait for
// it to connect, then close the old transport
type migratingTransport struct {
	ctx            context.Context
	client         *connect.Client
	byClientJwt    string
	instanceId     server.Id
	lbPort         int
	h3Port         int
	dnsPort        int
	migratedCount  atomic.Int64
	stateLock      sync.Mutex
	current        *connect.PlatformTransport
	receiveUnsub   func()
	migrateForever sync.Once
}

func newMigratingTransport(ctx context.Context, client *connect.Client, byClientJwt string, instanceId server.Id, lbPort int, h3Port int, dnsPort int) *migratingTransport {
	mt := &migratingTransport{
		ctx:         ctx,
		client:      client,
		byClientJwt: byClientJwt,
		instanceId:  instanceId,
		lbPort:      lbPort,
		h3Port:      h3Port,
		dnsPort:     dnsPort,
	}
	mt.current = mt.newTransport()
	mt.receiveUnsub = client.AddReceiveCallback(mt.handleControlFrames)
	return mt
}

func (self *migratingTransport) newTransport() *connect.PlatformTransport {
	auth := &connect.ClientAuth{
		ByJwt:      self.byClientJwt,
		InstanceId: connect.Id(self.instanceId),
		AppVersion: "0.0.0",
	}
	settings := connect.DefaultPlatformTransportSettings()
	settings.QuicTlsConfig.InsecureSkipVerify = true
	settings.H3Port = self.h3Port
	settings.DnsPort = self.dnsPort
	return connect.NewPlatformTransportWithTargetMode(
		self.ctx,
		connect.NewClientStrategyWithDefaults(self.ctx),
		self.client.RouteManager(),
		fmt.Sprintf("ws://127.0.0.1:%d", self.lbPort),
		auth,
		connect.TransportModeH1,
		settings,
	)
}

// ReceiveFunction
func (self *migratingTransport) handleControlFrames(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
	if !source.IsControlSource() {
		return
	}
	for _, frame := range frames {
		if frame.MessageType != protocol.MessageType_TransferResidentMigrate {
			continue
		}
		message, err := connect.FromFrame(frame)
		if err != nil {
			continue
		}
		residentMigrate, ok := message.(*protocol.ResidentMigrate)
		if !ok {
			continue
		}
		migrateTime := time.UnixMilli(int64(residentMigrate.MigrateTime))
		self.migrateForever.Do(func() {
			go func() {
				if wait := time.Until(migrateTime); 0 < wait {
					select {
					case <-self.ctx.Done():
						return
					case <-time.After(wait):
					}
				}
				next := self.newTransport()
				connectEndTime := time.Now().Add(30 * time.Second)
				for {
					notify := next.ConnectedNotify()
					if next.IsConnected() {
						break
					}
					if connectEndTime.Before(time.Now()) {
						next.Close()
						return
					}
					select {
					case <-self.ctx.Done():
						next.Close()
						return
					case <-notify:
					case <-time.After(1 * time.Second):
					}
				}
				var previous *connect.PlatformTransport
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					previous = self.current
					self.current = next
				}()
				previous.Close()
				self.migratedCount.Add(1)
			}()
		})
	}
}

func (self *migratingTransport) Close() {
	self.receiveUnsub()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.current.Close()
}

// A coordinated drain (Track B): the draining service broadcasts migrate
// frames, the clients make-before-break to a sibling service through the lb,
// traffic keeps flowing with zero ack failures (no gap), the peer view never
// blips, and the reconnects are recorded as excused (CONNECTDRAIN2.md §3.3).
//
// Requires the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.
func TestExchangeDrainMigrateE2e(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		drainAllTimeout := 10 * time.Second
		mutateExchangeSettings := func(exchangeSettings *ExchangeSettings) {
			exchangeSettings.EnableDrainExcuse = true
			exchangeSettings.EnableDrainCoordination = true
			exchangeSettings.DrainExcuseTtl = 1 * time.Minute
			exchangeSettings.DrainAllTimeout = drainAllTimeout
			exchangeSettings.DrainMigrateWindow = 3 * time.Second
			exchangeSettings.DrainStragglerSweepTimeout = 2 * time.Second
			exchangeSettings.DrainOneTimeout = 50 * time.Millisecond
		}
		mutateHandlerSettings := func(handlerSettings *ConnectHandlerSettings) {
			handlerSettings.ConnectionAnnounceTimeout = 100 * time.Millisecond
			handlerSettings.ConnectionAnnounceSettings.SyncConnectionTimeout = 200 * time.Millisecond
		}

		env1 := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, 8104, 9024, mutateExchangeSettings, mutateHandlerSettings)
		defer env1.Close()
		env2 := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, 8105, 9025, mutateExchangeSettings, mutateHandlerSettings)
		defer env2.Close()

		lb := newTestLb(t, 8106, 8104)
		defer lb.Close()

		// the clients belong to env1's network; either exchange can host them
		clientIdA, byClientJwtA := env1.authClient(&model.AuthNetworkClientArgs{
			Description: "migrate a",
			DeviceSpec:  "spec a",
		})
		clientIdB, byClientJwtB := env1.authClient(&model.AuthNetworkClientArgs{
			Description: "migrate b",
			DeviceSpec:  "spec b",
		})

		clientA := env1.newClient(clientIdA)
		defer clientA.Close()
		clientB := env1.newClient(clientIdB)
		defer clientB.Close()

		startBlock := testingReliabilityBlockNumber(server.NowUtc()) - 1

		mtA := newMigratingTransport(ctx, clientA, byClientJwtA, server.NewId(), 8106, 8104+443, 8104+53)
		defer mtA.Close()
		mtB := newMigratingTransport(ctx, clientB, byClientJwtB, server.NewId(), 8106, 8104+443, 8104+53)
		defer mtB.Close()

		// public provide: the reliability stats (and so the excuse recording)
		// only apply to public providers
		env1.setProvideModes(clientA, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
			protocol.ProvideMode_Public:  true,
		})
		env1.setProvideModes(clientB, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
			protocol.ProvideMode_Public:  true,
		})

		ok := waitForPeers(ctx, clientB, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdA) != nil
		})
		connect.AssertEqual(t, true, ok)

		blipped, stopWatch := watchPeerBlip(ctx, clientB, clientIdA)
		defer stopWatch()

		// continuous traffic a->b: every ack must arrive for the drain to be
		// gapless
		receiveB := recordPeerReceives(clientB, clientIdA)
		var sentCount, ackCount, failCount atomic.Int64
		trafficCtx, trafficCancel := context.WithCancel(ctx)
		defer trafficCancel()
		go func() {
			for {
				select {
				case <-trafficCtx.Done():
					return
				case <-time.After(200 * time.Millisecond):
				}
				frame, err := connect.ToFrame(&protocol.SimpleMessage{
					Content: "drain traffic",
				}, connect.DefaultProtocolVersion)
				if err != nil {
					failCount.Add(1)
					continue
				}
				sentCount.Add(1)
				sent := clientA.SendWithTimeout(
					frame,
					connect.DestinationId(connect.Id(clientIdB)),
					func(err error) {
						if err == nil {
							ackCount.Add(1)
						} else {
							failCount.Add(1)
						}
					},
					30*time.Second,
				)
				if !sent {
					failCount.Add(1)
				}
			}
		}()

		// wait past the announce transition of both services so the migrated
		// connections announce as new and consume the excuse markers
		select {
		case <-ctx.Done():
			return
		case <-time.After(2*drainAllTimeout + 1*time.Second):
		}

		// align the drain to a fresh reliability block (see
		// waitForNextReliabilityBlock)
		waitForNextReliabilityBlock(ctx)

		// take env1 out of rotation, then drain it: the broadcast asks the
		// clients to migrate; new dials land on env2
		lb.setBackendPort(8105)
		drainDone := make(chan time.Duration, 1)
		go func() {
			drainStartTime := time.Now()
			env1.exchange.Drain()
			drainDone <- time.Since(drainStartTime)
		}()

		migrateEndTime := time.Now().Add(60 * time.Second)
		for mtA.migratedCount.Load() < 1 || mtB.migratedCount.Load() < 1 {
			if migrateEndTime.Before(time.Now()) {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
		connect.AssertEqual(t, int64(1), mtA.migratedCount.Load())
		connect.AssertEqual(t, int64(1), mtB.migratedCount.Load())

		var drainDuration time.Duration
		select {
		case drainDuration = <-drainDone:
		case <-time.After(2 * drainAllTimeout):
			t.Fatal("drain did not return")
		}
		connect.AssertEqual(t, true, drainDuration < drainAllTimeout+5*time.Second)

		// both clients are now hosted by env2 (the nomination can lag the
		// transport connect by a beat)
		hostedEndTime := time.Now().Add(30 * time.Second)
		for {
			okA, okB := func() (bool, bool) {
				env2.exchange.stateLock.Lock()
				defer env2.exchange.stateLock.Unlock()
				_, okA := env2.exchange.residents[clientIdA]
				_, okB := env2.exchange.residents[clientIdB]
				return okA, okB
			}()
			if okA && okB {
				break
			}
			if hostedEndTime.Before(time.Now()) {
				connect.AssertEqual(t, true, okA)
				connect.AssertEqual(t, true, okB)
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}

		// let traffic run a little on the new service, then stop and check:
		// gapless means zero failures and every send acked
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		trafficCancel()
		ackEndTime := time.Now().Add(60 * time.Second)
		for ackCount.Load()+failCount.Load() < sentCount.Load() {
			if ackEndTime.Before(time.Now()) {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
		connect.AssertEqual(t, int64(0), failCount.Load())
		connect.AssertEqual(t, sentCount.Load(), ackCount.Load())
		connect.AssertEqual(t, true, 20 <= sentCount.Load())
		// messages were delivered, not only acked
		connect.AssertEqual(t, true, 20 <= int64(len(receiveB)))

		// the peer view never blipped through the migration
		stopWatch()
		connect.AssertEqual(t, false, blipped.Load())

		// the migrated reconnects are excused, with no invalid blocks
		var totalsA, totalsB testingReliabilityTotals
		rollupEndTime := time.Now().Add(90 * time.Second)
		for {
			model.RollupClientReliabilityStats(ctx, server.NowUtc().Add(3*model.ReliabilityBlockDuration))
			endBlock := testingReliabilityBlockNumber(server.NowUtc()) + 1
			totalsA = testingReadClientReliabilityTotals(ctx, clientIdA, startBlock, endBlock)
			totalsB = testingReadClientReliabilityTotals(ctx, clientIdB, startBlock, endBlock)
			if 1 <= totalsA.excusedNew && 1 <= totalsB.excusedNew {
				break
			}
			if rollupEndTime.Before(time.Now()) {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
		connect.AssertEqual(t, true, 1 <= totalsA.excusedNew)
		connect.AssertEqual(t, true, 1 <= totalsB.excusedNew)
		connect.AssertEqual(t, int64(0), totalsA.connectionNew)
		connect.AssertEqual(t, int64(0), totalsB.connectionNew)
		// the excused reconnects kept their blocks valid
		connect.AssertEqual(t, true, totalsA.excusedRowsAllValid)
		connect.AssertEqual(t, true, totalsB.excusedRowsAllValid)
	})
}
