package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Local transport end to end, through the real connect handler, exchange,
// nomination and move or drain (resident_local_transport.go).
//
// Require the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.

// Exchange settings with in-process delivery on for every block.
func localTransportE2eSettings(mutate func(*ExchangeSettings)) func(*ExchangeSettings) {
	return func(exchangeSettings *ExchangeSettings) {
		exchangeSettings.EnableLocalTransport = true
		exchangeSettings.LocalTransportDefault = model.ConnectLocalTransportAll
		if mutate != nil {
			mutate(exchangeSettings)
		}
	}
}

// Waits for test, polling, within timeout.
func waitForLocalTransportE2e(ctx context.Context, t testing.TB, timeout time.Duration, description string, test func() bool) {
	t.Helper()
	endTime := time.Now().Add(timeout)
	for !test() {
		if endTime.Before(time.Now()) {
			t.Fatalf("timed out waiting for %s", description)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A client whose resident moves to another process loses its in-process link
// with the old resident, and its transport on the source redials the new
// resident over the exchange; routing reaches the client through the new
// resident.
func TestLocalTransportMoveAwayRedialsTcp(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		envA := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", localTransportE2eSettings(nil)))
		defer envA.Close()
		envB := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-1", localTransportE2eSettings(nil)))
		defer envB.Close()
		<-envA.exchange.EdgeInventory().Loaded()
		<-envB.exchange.EdgeInventory().Loaded()

		clientIdX, byClientJwtX := envA.authClient(&model.AuthNetworkClientArgs{
			Description: "local move x",
			DeviceSpec:  "spec x",
		})
		clientIdY, byClientJwtY := envA.authClient(&model.AuthNetworkClientArgs{
			Description: "local move y",
			DeviceSpec:  "spec y",
		})
		clientX := envA.newClient(clientIdX)
		defer clientX.Close()
		clientY := envA.newClient(clientIdY)
		defer clientY.Close()
		receiveX := recordPeerReceives(clientX, clientIdY)

		outboundBefore := testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", "transport"))
		instanceIdX := server.NewId()
		transportXA := envA.newTransport(byClientJwtX, instanceIdX, clientX.RouteManager())
		defer transportXA.Close()
		transportY := envA.newTransport(byClientJwtY, server.NewId(), clientY.RouteManager())
		defer transportY.Close()
		envA.setProvideModes(clientX, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		envA.setProvideModes(clientY, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})

		residentA := waitForResidentMoveTestResident(ctx, t, envA.exchange, clientIdX, func(resident *Resident) bool {
			return 0 < resident.TransportCount() && resident.localAttachments.Load() == 1
		})
		waitForResidentMoveTestResident(ctx, t, envA.exchange, clientIdY, func(resident *Resident) bool {
			return resident.localAttachments.Load() == 1
		})
		// both transports reach their residents in process: no transport socket
		connect.AssertEqual(t, 2, envA.exchange.localTransport.linkCount())
		connect.AssertEqual(t, outboundBefore, testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", "transport")))

		residentA.observeTransportEdge(ResidentTransportEdge{ResidentMoveVersion: residentMoveTargetVersion})
		_, err := envA.exchange.SendResidentMove(ctx, ResidentMove{
			ClientId:   clientIdX,
			ResidentId: residentA.residentId,
			Kind:       model.ResidentMoveKindAdvisory,
			TargetHost: "alpha-edge-1",
			Targets:    residentMoveTestTargets,
		})
		connect.AssertEqual(t, nil, err)
		residentDoneBefore := snapshotLocalTransportDetaches(localTransportDetachResidentDone)
		transportXB := envB.newTransport(byClientJwtX, instanceIdX, clientX.RouteManager())
		defer transportXB.Close()
		residentB := waitForResidentMoveTestResident(ctx, t, envB.exchange, clientIdX, func(resident *Resident) bool {
			return resident.localAttachments.Load() == 1
		})
		select {
		case <-residentA.Done():
		case <-time.After(envA.exchange.settings.MoveIntentPollInterval + 5*time.Second):
			t.Fatal("the source did not retire the moved resident")
		}

		// the source's link detaches with its resident, and the source's
		// transport redials the new resident over the exchange
		waitForLocalTransportE2e(ctx, t, 30*time.Second, "the source transport to redial over the exchange", func() bool {
			return envA.exchange.localTransport.linkCount() == 1 &&
				outboundBefore+1 <= testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", "transport"))
		})
		connect.AssertEqual(t, true, residentDoneBefore+1 <= snapshotLocalTransportDetaches(localTransportDetachResidentDone))
		connect.AssertEqual(t, int64(0), residentA.localAttachments.Load())
		connect.AssertEqual(t, false, residentB.IsDone())

		sendSimpleMessage(t, clientY, clientIdX)
		select {
		case <-receiveX:
		case <-time.After(60 * time.Second):
			t.Fatal("no message reached the moved client")
		}
	})
}

// A coordinated drain of an exchange whose transports are attached in process
// converges as before: the clients migrate through the lb to a sibling,
// traffic between them is gapless, every link on the drained exchange
// detaches, and the sibling attaches the migrated transports in process.
func TestLocalTransportDrainConvergence(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		drainAllTimeout := 10 * time.Second
		mutateExchangeSettings := localTransportE2eSettings(func(exchangeSettings *ExchangeSettings) {
			exchangeSettings.EnableDrainCoordination = true
			exchangeSettings.DrainAllTimeout = drainAllTimeout
			exchangeSettings.DrainMigrateWindow = 3 * time.Second
			exchangeSettings.DrainStragglerSweepTimeout = 2 * time.Second
			exchangeSettings.DrainOneTimeout = 50 * time.Millisecond
		})
		env1 := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, mutateExchangeSettings, nil)
		defer env1.Close()
		env2 := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, mutateExchangeSettings, nil)
		defer env2.Close()
		lb := newTestLb(t, 0, env1.port)
		defer lb.Close()

		clientIdA, byClientJwtA := env1.authClient(&model.AuthNetworkClientArgs{
			Description: "local drain a",
			DeviceSpec:  "spec a",
		})
		clientIdB, byClientJwtB := env1.authClient(&model.AuthNetworkClientArgs{
			Description: "local drain b",
			DeviceSpec:  "spec b",
		})
		clientA := env1.newClient(clientIdA)
		defer clientA.Close()
		clientB := env1.newClient(clientIdB)
		defer clientB.Close()
		outboundBefore := testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", "transport"))
		attachmentsBefore := testutil.ToFloat64(exchangeLocalAttachmentsGauge)
		mtA := newMigratingTransport(ctx, clientA, byClientJwtA, server.NewId(), lb.port(), 0, 0)
		defer mtA.Close()
		mtB := newMigratingTransport(ctx, clientB, byClientJwtB, server.NewId(), lb.port(), 0, 0)
		defer mtB.Close()
		env1.setProvideModes(clientA, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		env1.setProvideModes(clientB, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		waitForLocalTransportE2e(ctx, t, 30*time.Second, "both transports to attach in process", func() bool {
			return env1.exchange.localTransport.linkCount() == 2
		})
		connect.AssertEqual(t, outboundBefore, testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", "transport")))
		connect.AssertEqual(t, attachmentsBefore+2, testutil.ToFloat64(exchangeLocalAttachmentsGauge))

		// gapless traffic a->b through the drain
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
				frame, err := connect.ToFrame(&protocol.SimpleMessage{Content: "local drain traffic"}, connect.DefaultProtocolVersion)
				if err != nil {
					failCount.Add(1)
					continue
				}
				sentCount.Add(1)
				if !clientA.SendWithTimeout(frame, connect.Id(clientIdB), func(err error) {
					if err == nil {
						ackCount.Add(1)
					} else {
						failCount.Add(1)
					}
				}, 30*time.Second) {
					failCount.Add(1)
				}
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}

		lb.setBackendPort(env2.port)
		drainDone := make(chan time.Duration, 1)
		go func() {
			drainStartTime := time.Now()
			env1.exchange.Drain()
			drainDone <- time.Since(drainStartTime)
		}()
		var drainDuration time.Duration
		select {
		case drainDuration = <-drainDone:
		case <-time.After(2 * drainAllTimeout):
			t.Fatal("the drain did not return")
		}
		connect.AssertEqual(t, true, drainDuration < drainAllTimeout+5*time.Second)
		connect.AssertEqual(t, int64(1), mtA.migratedCount.Load())
		connect.AssertEqual(t, int64(1), mtB.migratedCount.Load())

		waitForLocalTransportE2e(ctx, t, 30*time.Second, "the links to move to the sibling", func() bool {
			return env1.exchange.localTransport.linkCount() == 0 && env2.exchange.localTransport.linkCount() == 2
		})
		waitForLocalTransportE2e(ctx, t, 30*time.Second, "the attachments and transport sockets to settle", func() bool {
			return testutil.ToFloat64(exchangeLocalAttachmentsGauge) == attachmentsBefore+2 &&
				testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", "transport")) == outboundBefore
		})

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		trafficCancel()
		waitForLocalTransportE2e(ctx, t, 60*time.Second, "every send to settle", func() bool {
			return sentCount.Load() <= ackCount.Load()+failCount.Load()
		})
		connect.AssertEqual(t, int64(0), failCount.Load())
		connect.AssertEqual(t, sentCount.Load(), ackCount.Load())
		connect.AssertEqual(t, true, 20 <= sentCount.Load())
	})
}
