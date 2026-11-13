package connect

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The mover wired to the exchange (move_exchange.go) end to end, over the
// synthetic inventory of edge_inventory_test.go and the split-exchange harness
// of resident_move_e2e_test.go: alpha-edge-0 holds the residents and
// alpha-edge-1 is empty.
//
// Require the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.

// A CPU sampler that always reads the same host: a host without /proc/stat
// measures no host CPU, and such a host is never a target.
type moveExchangeTestCpuSampler struct {
	hostCpuBusy float64
	hostCores   int
}

func (self *moveExchangeTestCpuSampler) NextCpuSample() moveCpuSample {
	return moveCpuSample{
		processCpu:  0.1,
		hostCpuBusy: self.hostCpuBusy,
		hostCores:   self.hostCores,
	}
}

// The settings of a mover that moves at its first chance: every resident is
// old and idle enough, the band has no tolerance, and the DB guard is off so a
// loaded test machine does not pause the ticks.
func moveExchangeTestSettings(processHost string) func(*ExchangeSettings) {
	return residentMoveTestExchangeSettings(processHost, func(exchangeSettings *ExchangeSettings) {
		exchangeSettings.MoveDefaultMode = "on"
		exchangeSettings.MoveTickInterval = 200 * time.Millisecond
		exchangeSettings.MoveLoadPublishInterval = 100 * time.Millisecond
		exchangeSettings.MoveMinResidentAge = 0
		exchangeSettings.MoveIdleTimeout = 0
		exchangeSettings.MoveBalanceTolerance = 0
		exchangeSettings.MoveDbWaitPauseThreshold = 0
	})
}

// An H1 transport that declares no move version, as a client from before moves
// does.
func newMoveExchangeTestLegacyTransport(
	env *peerDiscoveryEnv,
	byClientJwt string,
	routeManager *connect.RouteManager,
) *connect.PlatformTransport {
	settings := connect.DefaultPlatformTransportSettings()
	settings.EnableMove = false
	settings.QuicTlsConfig.InsecureSkipVerify = true
	settings.H3Port = env.h3Port
	settings.DnsPort = 0
	return connect.NewPlatformTransportWithTargetMode(
		env.ctx,
		connect.NewClientStrategyWithDefaults(env.ctx),
		routeManager,
		fmt.Sprintf("ws://127.0.0.1:%d", env.port),
		&connect.ClientAuth{
			ByJwt:      byClientJwt,
			InstanceId: connect.Id(server.NewId()),
			AppVersion: "0.0.0",
		},
		connect.TransportModeH1,
		settings,
	)
}

// A model call's context: every move model call needs a deadline.
func moveExchangeTestModelContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 10*time.Second)
}

// The mover of a host above the band moves an idle capable client to the empty
// host through the move primitives: the load table joins the inventory, the
// budget and cooldown are spent, the replacement takes over by intent, the
// source retires the old resident, and the client is never moved back.
func TestExchangeMoverMovesAnIdleClientToTheEmptyHost(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		envA := testing_newPeerDiscoveryEnvWithSettings(ctx, t, moveExchangeTestSettings("alpha-edge-0"))
		defer envA.Close()
		envB := testing_newPeerDiscoveryEnvWithSettings(ctx, t, moveExchangeTestSettings("alpha-edge-1"))
		defer envB.Close()
		<-envA.exchange.EdgeInventory().Loaded()
		<-envB.exchange.EdgeInventory().Loaded()

		// two residents on alpha-edge-0: x declares moves, as the library does
		// by default, and y is a client from before moves
		clientIdX, byClientJwtX := envA.authClient(&model.AuthNetworkClientArgs{
			Description: "mover x",
			DeviceSpec:  "spec x",
		})
		clientIdY, byClientJwtY := envA.authClient(&model.AuthNetworkClientArgs{
			Description: "mover y",
			DeviceSpec:  "spec y",
		})
		clientX := envA.newClient(clientIdX)
		defer clientX.Close()
		clientY := envA.newClient(clientIdY)
		defer clientY.Close()
		instanceIdX := server.NewId()
		transportXA := envA.newTransport(byClientJwtX, instanceIdX, clientX.RouteManager())
		defer transportXA.Close()
		transportY := newMoveExchangeTestLegacyTransport(envA, byClientJwtY, clientY.RouteManager())
		defer transportY.Close()
		residentA := waitForResidentMoveTestResident(ctx, t, envA.exchange, clientIdX, func(resident *Resident) bool {
			return 0 < resident.TransportCount() && resident.moveVersion.Load() == residentMoveTargetVersion
		})
		residentY := waitForResidentMoveTestResident(ctx, t, envA.exchange, clientIdY, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})
		connect.AssertEqual(t, uint32(0), residentY.moveVersion.Load())

		sentBefore := testutil.ToFloat64(movesSentCounter.WithLabelValues(moveKindAdvisory, moveTriggerBalance, "alpha-edge-0", "alpha-edge-1"))
		sentBackBefore := testutil.ToFloat64(movesSentCounter.WithLabelValues(moveKindAdvisory, moveTriggerBalance, "alpha-edge-1", "alpha-edge-0"))
		sampler := &moveExchangeTestCpuSampler{hostCpuBusy: 0.2, hostCores: 4}
		stopA := startExchangeMover(ctx, envA.exchange, sampler)
		defer stopA()
		stopB := startExchangeMover(ctx, envB.exchange, sampler)
		defer stopB()

		// the mover sends x its move to alpha-edge-1
		var intent *model.ResidentMoveIntent
		endTime := time.Now().Add(30 * time.Second)
		for intent == nil {
			if endTime.Before(time.Now()) {
				t.Fatal("the mover sent no move")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			var err error
			intent, err = model.GetResidentMoveIntent(ctx, clientIdX)
			connect.AssertEqual(t, nil, err)
		}
		connect.AssertEqual(t, model.ResidentMoveKindAdvisory, intent.Kind)
		connect.AssertEqual(t, "alpha-edge-1", intent.TargetHost)
		connect.AssertEqual(t, residentA.residentId, intent.FromResidentId)
		// the client was seen on IPv4 only, so the move offers that family's
		// target alone (D4)
		connect.AssertEqual(t, residentMoveTestTargets[:1], intent.Targets)
		connect.AssertEqual(t, sentBefore+1, testutil.ToFloat64(movesSentCounter.WithLabelValues(moveKindAdvisory, moveTriggerBalance, "alpha-edge-0", "alpha-edge-1")))
		// y, which declared no moves, got none
		intentY, err := model.GetResidentMoveIntent(ctx, clientIdY)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, true, intentY == nil)
		// the cooldown is the move's
		modelCtx, modelCancel := moveExchangeTestModelContext(ctx)
		defer modelCancel()
		cooling, err := model.GetConnectMoveCooldowns(modelCtx, []server.Id{clientIdX, clientIdY})
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, true, cooling[clientIdX])
		connect.AssertEqual(t, false, cooling[clientIdY])

		// both processes publish their record under their inventory host
		loads, _, err := model.GetConnectMoveLoads(modelCtx)
		connect.AssertEqual(t, nil, err)
		loadHosts := []string{}
		for _, load := range loads {
			if load.ExchangeId == envA.exchange.ExchangeId() || load.ExchangeId == envB.exchange.ExchangeId() {
				loadHosts = append(loadHosts, load.Host)
			}
		}
		slices.Sort(loadHosts)
		connect.AssertEqual(t, []string{"alpha-edge-0", "alpha-edge-1"}, loadHosts)

		// the client's replacement arrives at alpha-edge-1 and takes over
		transportXB := envB.newTransport(byClientJwtX, instanceIdX, clientX.RouteManager())
		defer transportXB.Close()
		residentB := waitForResidentMoveTestResident(ctx, t, envB.exchange, clientIdX, anyResidentMoveTestResident)
		connect.AssertEqual(t, intent.MoveId, residentB.arrivedMoveId)
		select {
		case <-residentA.Done():
		case <-time.After(envA.exchange.settings.MoveIntentPollInterval + 5*time.Second):
			t.Fatal("the source did not retire the moved resident")
		}

		// the arrival is never moved again, and the fleet is now in band: no
		// move follows over several ticks of both movers
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * envA.exchange.settings.MoveTickInterval):
		}
		connect.AssertEqual(t, sentBefore+1, testutil.ToFloat64(movesSentCounter.WithLabelValues(moveKindAdvisory, moveTriggerBalance, "alpha-edge-0", "alpha-edge-1")))
		connect.AssertEqual(t, sentBackBefore, testutil.ToFloat64(movesSentCounter.WithLabelValues(moveKindAdvisory, moveTriggerBalance, "alpha-edge-1", "alpha-edge-0")))
		connect.AssertEqual(t, false, residentB.IsDone())
	})
}

// The drain's switch for drain moves is the Redis move control: drains move
// clients while the control allows them, and fall back to the legacy migrate
// when an operator turns them off.
func TestExchangeMoverDrainSwitchReadsTheMoveControl(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", nil))
		defer env.Close()
		stop := startExchangeMover(ctx, env.exchange, &moveExchangeTestCpuSampler{hostCpuBusy: 0.2, hostCores: 4})
		defer stop()

		controlCtx, controlCancel := context.WithTimeout(ctx, 10*time.Second)
		defer controlCancel()

		connect.AssertEqual(t, true, env.exchange.drainMovesEnabled())
		connect.AssertEqual(t, nil, model.SetConnectMoveControl(controlCtx, map[string]string{
			model.ConnectMoveControlFieldDrainMoves: "false",
		}))
		connect.AssertEqual(t, false, env.exchange.drainMovesEnabled())
		connect.AssertEqual(t, nil, model.SetConnectMoveControl(controlCtx, map[string]string{
			model.ConnectMoveControlFieldDrainMoves: "true",
		}))
		connect.AssertEqual(t, true, env.exchange.drainMovesEnabled())
		connect.AssertEqual(t, nil, model.RemoveConnectMoveControl(controlCtx))
		connect.AssertEqual(t, true, env.exchange.drainMovesEnabled())
	})
}
