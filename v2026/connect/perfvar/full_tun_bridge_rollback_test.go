package perfvar

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// No generated client, transport, provider, database or network access exists
// in this fixture. Its real multi-client can only wait for a route or cancel.
type fullTunNoRouteGenerator struct{}

func (*fullTunNoRouteGenerator) NextDestinations(int, []clientconnect.MultiHopId, string) (map[clientconnect.MultiHopId]clientconnect.DestinationStats, error) {
	return nil, nil
}

func (*fullTunNoRouteGenerator) NewClientArgs() (*clientconnect.MultiClientGeneratorClientArgs, error) {
	return nil, errors.New("no route in rollback fixture")
}

func (*fullTunNoRouteGenerator) RemoveClientArgs(*clientconnect.MultiClientGeneratorClientArgs) {}

func (*fullTunNoRouteGenerator) RemoveClientWithArgs(*clientconnect.Client, *clientconnect.MultiClientGeneratorClientArgs) {
}

func (*fullTunNoRouteGenerator) NewClientSettings() *clientconnect.ClientSettings {
	return clientconnect.DefaultClientSettings()
}

func (*fullTunNoRouteGenerator) NewClient(context.Context, *clientconnect.MultiClientGeneratorClientArgs, *clientconnect.ClientSettings) (*clientconnect.Client, error) {
	return nil, errors.New("no client in rollback fixture")
}

func (*fullTunNoRouteGenerator) FixedDestinationSize() (int, bool) { return 1, true }

// Always select remote egress so packet policy cannot turn the no-route
// regression into an early rejection or a local-network operation.
type fullTunNoRoutePolicy struct{ clientconnect.SecurityPolicy }

func (fullTunNoRoutePolicy) InspectEgress(protocol.ProvideMode, *clientconnect.IpPath, []byte) (clientconnect.SecurityPolicyResult, error) {
	return clientconnect.SecurityPolicyResultAllow, nil
}

func fullTunRollbackPacket(marker byte) []byte {
	packet := clientconnect.MessagePoolGet(32)
	clear(packet)
	packet[0], packet[8], packet[9] = 0x45, 64, 17 // IPv4, TTL, UDP wire protocol.
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	copy(packet[12:16], []byte{192, 0, 2, 1})
	copy(packet[16:20], []byte{198, 51, 100, 1})
	binary.BigEndian.PutUint16(packet[20:22], 41000)
	binary.BigEndian.PutUint16(packet[22:24], 42000)
	binary.BigEndian.PutUint16(packet[24:26], uint16(len(packet)-20))
	packet[28] = marker
	return packet
}

func TestFullTunBridgeTeardownCancelsNoRoute(t *testing.T) {
	// The process-wide pool diagnostics goroutine must not be owned by a
	// finite fake-clock bubble. Initialize it before entering the fixture.
	_ = routeMessagePoolOutstanding()
	for _, disposition := range []struct {
		name        string
		bridge      bool
		normalClose bool
	}{
		{name: "rollback_after_multi_client"},
		{name: "rollback_after_bridge", bridge: true},
		{name: "normal_close", bridge: true, normalClose: true},
	} {
		for _, singular := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/singular_%t", disposition.name, singular), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					parent, parentCancel := context.WithTimeout(context.Background(), 10*time.Minute)
					defer parentCancel()
					multiCtx, multiCancel := context.WithCancel(parent)
					defer multiCancel()
					poolBefore := routeMessagePoolOutstanding()
					settings := clientconnect.DefaultMultiClientSettings()
					settings.Log = clientconnect.NewNoopLogger()
					settings.ProviderProbe = false
					settings.HeartbeatInterval = 0
					settings.SchedulerPauseTolerance = 0
					settings.IpAssocSettings = nil
					settings.SequenceIdleTimeout = time.Hour
					settings.TcpSequenceIdleTimeout = time.Hour
					settings.WindowOutcomeDeadline = 0
					settings.WindowOutcomeRebuildDeadline = 0
					settings.SecurityPolicyGenerator = func(ctx context.Context, stats *clientconnect.SecurityPolicyStatsCollector) clientconnect.SecurityPolicy {
						return fullTunNoRoutePolicy{clientconnect.DefaultSecurityPolicyWithStats(ctx, stats)}
					}
					multiClient := clientconnect.NewRemoteUserNatMultiClient(multiCtx, &fullTunNoRouteGenerator{}, nil, protocol.ProvideMode_Network, settings)
					defer multiClient.Close()
					path := &fullTunPath{
						t: t, ctx: parent, multiClient: multiClient, multiClientCancel: multiCancel,
						deviceClientId: clientconnect.NewId(), bridgeSends: newFullTunBridgeSendTracker(),
						deviceTransports: newPlatformTransportOwner(), deviceNoAckSends: newNoAckSendTracker(),
						devicePackSends: newSendPackLifecycleTracker(), providerReturns: newProviderReturnSendTracker(),
					}
					defer path.deviceNoAckSends.close()
					defer path.devicePackSends.close()
					defer path.providerReturns.close()
					var cleanupOrder []fullTunConstructionResource
					path.afterConstructionCleanupForTest = func(resource fullTunConstructionResource) {
						if resource == fullTunConstructionResourceBridge {
							if multiCtx.Err() != context.Canceled || routeMessagePoolOutstanding() != poolBefore {
								t.Error("bridge joined before cancellation/packet returns settled")
							}
						}
						cleanupOrder = append(cleanupOrder, resource)
					}
					var packets [][]byte
					if disposition.bridge {
						packets = [][]byte{fullTunRollbackPacket(1), fullTunRollbackPacket(2), fullTunRollbackPacket(3)}
						read := false
						path.startBridge(tunResourceProfile{BatchSize: len(packets), SingularBridgeSend: singular}, func(dst [][]byte) (int, error) {
							if read {
								return 0, io.EOF
							}
							read = true
							return copy(dst, packets), nil
						})
					}
					synctest.Wait()
					if path.bridgeSends.startedCount.Load() != uint64(len(packets)) {
						t.Fatal("actual bridge did not admit the pooled packet batch")
					}
					path.bridgeSends.stateLock.Lock()
					pending := len(path.bridgeSends.liveEntries)
					path.bridgeSends.stateLock.Unlock()
					if pending != len(packets) || routeMessagePoolOutstanding() != poolBefore+int64(len(packets)) {
						t.Fatalf("bridge not blocked owning no-route packets: pending=%d pool=%d->%d", pending, poolBefore, routeMessagePoolOutstanding())
					}
					rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer rollbackCancel()
					start := time.Now()
					owner := newFullTunConstructionOwner(path)
					closePath := func() error { return owner.rollback(rollbackCtx) }
					if disposition.normalClose {
						closePath = func() error { path.close(); return nil }
					}
					if err := closePath(); err != nil {
						t.Fatalf("rollback after %s: %v", time.Since(start), err)
					}
					if time.Since(start) != 0 || parent.Err() != nil || rollbackCtx.Err() != nil || multiCtx.Err() != context.Canceled {
						t.Fatalf("wrong teardown context ownership: elapsed=%s parent=%v rollback=%v consumer=%v", time.Since(start), parent.Err(), rollbackCtx.Err(), multiCtx.Err())
					}
					cleanupCounts := map[fullTunConstructionResource]int{}
					for _, resource := range cleanupOrder {
						cleanupCounts[resource]++
					}
					expectedResources := []fullTunConstructionResource{
						fullTunConstructionResourceMultiClient,
						fullTunConstructionResourceDeviceTransports, fullTunConstructionResourceNoAckTracker,
						fullTunConstructionResourcePackTracker, fullTunConstructionResourceReturnTracker,
					}
					if disposition.bridge {
						expectedResources = append([]fullTunConstructionResource{fullTunConstructionResourceBridge}, expectedResources...)
					}
					if len(cleanupOrder) != len(expectedResources) {
						t.Fatalf("cleanup order=%v, want %v", cleanupOrder, expectedResources)
					}
					for i, resource := range expectedResources {
						if cleanupOrder[i] != resource {
							t.Fatalf("consumer closed before bridge ownership settled: %v", cleanupOrder)
						}
					}
					assertFullTunConstructionRolledBack(t, "no-route bridge", path, cleanupCounts, expectedResources)
					path.bridgeSends.stateLock.Lock()
					pending = len(path.bridgeSends.liveEntries)
					path.bridgeSends.stateLock.Unlock()
					if pending != 0 || path.bridgeSends.failureCount.Load() != uint64(len(packets)) {
						t.Fatalf("bridge terminal ownership: pending=%d failed=%d", pending, path.bridgeSends.failureCount.Load())
					}
					if snapshot, balanced := routeMessagePoolBalance(poolBefore); !balanced {
						t.Fatalf("rollback packet pool did not reconcile: %d -> %d classes=%v", poolBefore, snapshot.outstanding, snapshot.classes)
					}
					// Cancel-only teardown and destructive close must be safe to
					// repeat independently, not just behind the owner's once guard.
					path.multiClientCancel()
					if err := multiClient.CloseAndWait(rollbackCtx); err != nil {
						t.Fatalf("repeated cancel/consumer close: %v", err)
					}
					wantCleanupCount := len(expectedResources)
					if disposition.normalClose {
						wantCleanupCount *= 2
					}
					if err := closePath(); err != nil || len(cleanupOrder) != wantCleanupCount {
						t.Fatalf("repeated close changed ownership: err=%v cleanup=%v", err, cleanupOrder)
					}
					if snapshot, balanced := routeMessagePoolBalance(poolBefore); !balanced {
						t.Fatalf("repeated close changed packet ownership: %d -> %d", poolBefore, snapshot.outstanding)
					}
				})
			})
		}
	}
}
