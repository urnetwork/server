package perfvar

import (
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	connectserver "github.com/urnetwork/server/connect"
)

// Full simulated TUN -> IP stack -> authenticated H1 -> exchange -> provider
// path. The mode remains H1 in both arms and actual carrier counters prove
// either compact framing or a fresh standard-WS fallback to an old provider.
func TestFullTunH1PlusCorrectnessAndOldProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("local PERFVAR environment required")
	}
	for _, old := range []bool{false, true} {
		name := "framer"
		if old {
			name = "old-provider-websocket"
		}
		t.Run(name, func(t *testing.T) {
			env := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
			env.Run(t, func(t testing.TB) {
				profile := initialNetworkProfiles(20260922)["clean-lan"]
				deviceStats, providerStats := &clientconnect.H1PlusStats{}, &clientconnect.H1PlusStats{}
				hooks := &fullTunConstructionTestHooks{
					configureConnectHandlerSettings:   func(s *connectserver.ConnectHandlerSettings) { s.EnableH1Plus = !old },
					configureDevicePlatformSettings:   func(s *clientconnect.PlatformTransportSettings) { s.EnableH1Plus = true; s.H1PlusStats = deviceStats },
					configureProviderPlatformSettings: func(s *clientconnect.PlatformTransportSettings) { s.EnableH1Plus = true; s.H1PlusStats = providerStats },
				}
				fixture, err := newPerfvarCorrectnessFixtureWithHooks(t, fullTunRouteExchangeH1, profile, profile, profile, defaultTunResourceProfile(), 2*time.Minute, hooks)
				if err != nil {
					t.Fatal(err)
				}
				defer fixture.close()
				pair, err := fixture.measureExactTCP(256 * 1024)
				if err != nil {
					t.Fatal(err)
				}
				for endpoint, stats := range map[string]*clientconnect.H1PlusStats{"device": deviceStats, "provider": providerStats} {
					s := stats.Snapshot()
					if old {
						if s.Accepted != 0 || s.WebSocketSelected == 0 {
							t.Fatalf("%s old provider carrier=%+v", endpoint, s)
						}
					} else if s.Accepted == 0 || s.Messages == 0 {
						t.Fatalf("%s custom carrier=%+v", endpoint, s)
					}
				}
				for _, carrier := range []perfvarCarrierObservation{pair.Upload.Carrier, pair.Download.Carrier} {
					for _, recovery := range []perfvarSendRecoveryObservation{carrier.DeviceSendRecovery, carrier.ProviderSendRecovery} {
						if !recovery.Available || recovery.GenerationChanged || recovery.TimeoutResendWriteCount+recovery.CarrierChangeWriteCount+recovery.SelectiveGapWriteCount+recovery.AckTailProbeWriteCount+recovery.CumulativeProbeWriteCount != 0 {
							t.Fatalf("clean reliable H1 carried recovery writes: %+v", recovery)
						}
					}
				}
				t.Logf("H1+%s upload=%+v download=%+v", name, pair.Upload.Result, pair.Download.Result)
			})
		})
	}
}
