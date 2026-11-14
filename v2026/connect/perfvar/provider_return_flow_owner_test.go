package perfvar

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// The rollover is deliberately limited to the fixture's newest-generated
// pointer. The real registered flow and its authenticated return owner stay
// alive. This proves the expected-key bug, not the cause of a historical run.
func TestFullTunUDPDownloadUsesRegistrationOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("requires local full-TUN fixture")
	}
	for _, rollover := range []bool{false, true} {
		name := "same_owner"
		if rollover {
			name = "newest_pointer_rollover"
		}
		t.Run(name, func(t *testing.T) {
			testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
			testEnvironment.Run(t, func(t testing.TB) {
				fixtureCtx, fixtureCancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer fixtureCancel()
				profile := initialNetworkProfiles(187)["clean-lan"]
				environment := newRouteEnvironmentWithNetworkPeers(fixtureCtx, t, profile, false)
				defer environment.close()
				path := newFullTunPath(fixtureCtx, t, environment, fullTunRouteExchangeH1)
				defer path.close()
				workloadCtx, cancelWorkload := context.WithCancel(fixtureCtx)
				defer cancelWorkload()

				var original, newest *clientconnect.Client
				var flow clientconnect.RemoteUserNatProviderReturnFlowKey
				var watchdog *time.Timer
				var lock sync.Mutex
				var started, completed int
				var startedBytes, completedBytes clientconnect.ByteCount
				var foreignOwners, rejected int
				path.afterUdpRegistrationForTest = func(ctx context.Context, connection net.Conn) error {
					original = path.deviceClient.Load()
					if original == nil || original.Ctx().Err() != nil {
						return fmt.Errorf("fixture has no live generated registration client")
					}
					var err error
					flow, err = fullTunProviderReturnUdpFlowKey(original.ClientId(), connection.RemoteAddr(), connection.LocalAddr())
					if err != nil {
						return err
					}
					path.providerReturns.setBeforeObserverPublishForTest(func(observation clientconnect.RemoteUserNatProviderReturnSendObservation) {
						actual := observation.FlowKey
						actual.DestinationId = flow.DestinationId
						if actual != flow {
							return // Unrelated traffic cannot supply this fixture's proof.
						}
						lock.Lock()
						defer lock.Unlock()
						if observation.FlowKey.DestinationId != original.ClientId() {
							foreignOwners++
						}
						switch observation.Phase {
						case clientconnect.RemoteUserNatProviderReturnSendPhaseStarted:
							started += observation.PacketCount
							startedBytes += observation.PacketByteCount
						case clientconnect.RemoteUserNatProviderReturnSendPhaseCompleted:
							completed += observation.PacketCount
							completedBytes += observation.PacketByteCount
							if !observation.Sent {
								rejected += observation.PacketCount
							}
						}
					})
					if rollover {
						newest = clientconnect.NewClient(ctx, clientconnect.NewId(), clientconnect.NewNoContractClientOob(), clientconnect.DefaultClientSettings())
						observeGeneratedDeviceClient(path.deviceClient, nil, nil, newest)
						if path.deviceClient.Load() != newest || newest == original {
							return fmt.Errorf("fixture did not advance its newest generated pointer")
						}
					}
					// Only a failure watchdog; ownership ordering comes from the
					// completed registration and synchronous callback above.
					watchdog = time.AfterFunc(15*time.Second, cancelWorkload)
					return nil
				}
				defer func() {
					if watchdog != nil {
						watchdog.Stop()
					}
					if newest != nil {
						path.deviceClient.CompareAndSwap(newest, original)
						closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer closeCancel()
						if err := newest.CloseAndWait(closeCtx); err != nil {
							t.Errorf("close replacement pointer fixture: %v", err)
						}
					}
				}()

				const target = 24
				result, err := measureFullTunUDPDirection(workloadCtx, path, false, time.Second, 192_000, 1000)
				lock.Lock()
				actualStarted, actualCompleted := started, completed
				actualStartedBytes, actualCompletedBytes := startedBytes, completedBytes
				actualForeign, actualRejected := foreignOwners, rejected
				lock.Unlock()
				if actualForeign != 0 || actualRejected != 0 || actualStarted < target || actualCompleted < target ||
					actualStartedBytes < target*1028 || actualCompletedBytes < target*1028 {
					t.Fatalf("fixture did not prove complete original-owner returns: starts=%d/%d completions=%d/%d foreign=%d rejected=%d workload=%v",
						actualStarted, actualStartedBytes, actualCompleted, actualCompletedBytes, actualForeign, actualRejected, err)
				}
				if rollover && (path.deviceClient.Load() != newest || original.Ctx().Err() != nil) {
					t.Fatal("fixture did not retain the original flow across the newest-pointer rollover")
				}
				if err != nil {
					t.Fatalf("all %d original-owner packets returned but the workload failed after pointer rollover=%t: %v", actualCompleted, rollover, err)
				}
				if result.OfferedPacketCount != target || result.DeliveredPacketCount != target || result.UsefulByteCount != target*1000 ||
					result.CorruptPacketCount != 0 || result.TerminalMarkerAttemptCount != 1 ||
					actualStarted != target+1 || actualCompleted != target+1 ||
					actualStartedBytes != (target+1)*1028 || actualCompletedBytes != (target+1)*1028 {
					t.Fatalf("registration-owned UDP measurement was not exact: result=%+v starts=%d/%d completions=%d/%d",
						result, actualStarted, actualStartedBytes, actualCompleted, actualCompletedBytes)
				}
			})
		})
	}
}
