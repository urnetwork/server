// This file makes warmed full-TUN correctness a campaign gate on every
// production carrier and on representative long regional paths.
package perfvar

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// One warmed direction must acknowledge the route-local BDP before the
// separately hashed measured body and carrier interval can complete.
func measurePerfvarExactWarmedDirection(
	fixture *perfvarCorrectnessFixture,
	scenario perfvarScenario,
) error {
	if err := fixture.path.waitForMeasurementBoundary(fixture.ctx); err != nil {
		return fmt.Errorf("premeasurement boundary: %w", err)
	}
	result, err := measurePerfvarFullTun(fixture.ctx, fixture.path, scenario)
	if err != nil {
		deviceReceive := snapshotPerfvarDeviceReceive(fixture.path)
		providerReceive := snapshotPerfvarClientReceive(fixture.path.providerClient)
		return fmt.Errorf(
			"%w; device_p2p=%+v provider_p2p=%+v device_recovery=%+v provider_recovery=%+v device_h3=%+v provider_h3=%+v device_receive=%+v provider_receive=%+v device_packets=%+v provider_packets=%+v",
			err,
			fixture.path.deviceStats.Snapshot(),
			fixture.path.providerStats.Snapshot(),
			perfvarSendRecoveryCountersFor(deviceReceive.sendRecovery),
			perfvarSendRecoveryCountersFor(providerReceive.sendRecovery),
			fixture.path.deviceH3DatagramStats.Snapshot(),
			fixture.path.providerH3DatagramStats.Snapshot(),
			fixture.path.devicePlatformReceiveStats.Snapshot(),
			fixture.path.providerPlatformReceiveStats.Snapshot(),
			snapshotPerfvarDevicePacketStats(fixture.path),
			snapshotPerfvarProviderPacketStats(fixture.path),
		)
	}
	if !fixture.path.hasCarrierMeasurementStart() {
		return fmt.Errorf("warmed workload did not publish its measured carrier boundary")
	}
	if err := fixture.path.waitForPostWorkloadBoundary(fixture.ctx); err != nil {
		return fmt.Errorf("warmed post-workload boundary: %w", err)
	}
	carrier := observePerfvarWorkloadCarrier(fixture.path, perfvarCarrierBoundary{})
	if result.WarmupByteCount != scenario.WarmupByteCount ||
		result.WarmupDuration <= 0 ||
		result.UsefulByteCount != scenario.PayloadByteCount ||
		result.ContentHash != deterministicPayloadHash(scenario.PayloadByteCount) {
		return fmt.Errorf("warmed result=%+v scenario=%+v", result, scenario)
	}
	if carrier.WireByteCount == 0 {
		return fmt.Errorf("measured carrier interval recorded no bytes")
	}
	if err := fixture.path.verifyRoute(); err != nil {
		return err
	}
	return verifyPerfvarTopologyCarrier(scenario, carrier, result.UsefulByteCount)
}

// Every production route and direction must preserve the acknowledged warmup
// boundary and exact measured payload before any warmed campaign is trusted.
func TestPerfvarEveryRouteWarmedTCPDirectionsCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		for routeIndex, route := range perfvarCorrectnessRoutes() {
			profile := initialNetworkProfiles(2026081500 + int64(routeIndex))["clean-lan"]
			fixture, err := newPerfvarCorrectnessFixture(
				t,
				route,
				profile,
				profile,
				profile,
				defaultTunResourceProfile(),
				8*time.Minute,
			)
			if err != nil {
				t.Fatalf("construct %s: %v", route, err)
			}
			for _, direction := range []perfvarDirection{
				perfvarDirectionUpload,
				perfvarDirectionDownload,
			} {
				scenario := perfvarScenario{
					Route:                 route,
					Profile:               profile,
					ProviderAccessProfile: profile,
					Workload:              perfvarWorkloadTCPWarmed,
					Direction:             direction,
					Topology:              perfvarTopologyOneHop,
					Resource:              perfvarResourceDefault,
					PayloadByteCount:      128 * 1024,
					FlowCount:             1,
				}
				scenario.WarmupByteCount = perfvarDirectionalBandwidthDelayByteCount(scenario)
				if err := measurePerfvarExactWarmedDirection(fixture, scenario); err != nil {
					fixture.close()
					t.Fatalf("%s/%s warmed TCP: %v", route, direction, err)
				}
			}
			fixture.close()
		}
	})
}

// A regional warmed gate verifies exact delivery rather than enforcing a
// throughput threshold. Both H3 and P2P deliberately propagate bounded queue
// backpressure, whose scheduling cost is not represented by link-rate bytes;
// retain the same 25% wall-clock allowance without changing workload bytes.
func regionalWarmedCorrectnessDeadlineByteCount(scenario perfvarScenario) int64 {
	baseDeadlineByteCount := scenario.WarmupByteCount + scenario.PayloadByteCount
	return baseDeadlineByteCount + baseDeadlineByteCount/4
}

// Every long regional carrier gets the same exact-delivery deadline headroom.
func TestRegionalWarmedCorrectnessDeadlineIncludesBoundedCarrierHeadroom(t *testing.T) {
	const (
		warmupByteCount  = int64(6_275_000)
		payloadByteCount = int64(32 * 1024 * 1024)
	)
	want := (warmupByteCount + payloadByteCount) * 5 / 4
	for _, route := range []fullTunRoute{
		fullTunRouteExchangeH3,
		fullTunRouteP2pFast,
	} {
		scenario := perfvarScenario{
			Route:            route,
			WarmupByteCount:  warmupByteCount,
			PayloadByteCount: payloadByteCount,
		}
		if got := regionalWarmedCorrectnessDeadlineByteCount(scenario); got != want {
			t.Errorf("%s regional correctness deadline bytes=%d, want=%d", route, got, want)
		}
	}
}

// Representative 500 ms and 1 s regional paths use a full 32 MiB measured
// body after one route-local BDP, preventing short-buffer success from
// masquerading as steady-state warmed throughput.
func TestPerfvarRegionalWarmedTCPThirtyTwoMiBCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	cases := []struct {
		route       fullTunRoute
		profileName string
		direction   perfvarDirection
		seed        int64
	}{
		{
			route:       fullTunRouteExchangeH3,
			profileName: "single-region-500ms-rtt",
			direction:   perfvarDirectionUpload,
			seed:        2026081550,
		},
		{
			route:       fullTunRouteP2pFast,
			profileName: "single-region-1000ms-rtt",
			direction:   perfvarDirectionDownload,
			seed:        2026081510,
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(
			fmt.Sprintf("%s/%s/%s", testCase.route, testCase.profileName, testCase.direction),
			func(t *testing.T) {
				testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
				testEnvironment.Run(t, func(t testing.TB) {
					profiles := initialNetworkProfiles(testCase.seed)
					profile := profiles[testCase.profileName]
					providerProfile := profiles["clean-lan"]
					providerProfile.SourceNote = "synthetic provider colocated with server/connect"
					fixture, err := newPerfvarCorrectnessFixture(
						t,
						testCase.route,
						profile,
						profile,
						providerProfile,
						defaultTunResourceProfile(),
						20*time.Minute,
					)
					if err != nil {
						t.Fatalf("construct %s/%s: %v", testCase.route, testCase.profileName, err)
					}
					scenario := perfvarScenario{
						Route:                 testCase.route,
						Profile:               profile,
						ProviderAccessProfile: providerProfile,
						Workload:              perfvarWorkloadTCPWarmed,
						Direction:             testCase.direction,
						Topology:              perfvarTopologyOneHop,
						Resource:              perfvarResourceDefault,
						PayloadByteCount:      32 * 1024 * 1024,
						FlowCount:             1,
					}
					scenario.WarmupByteCount = perfvarDirectionalBandwidthDelayByteCount(scenario)
					scenario.correctnessDeadlineByteCount =
						regionalWarmedCorrectnessDeadlineByteCount(scenario)
					if err := validatePerfvarWarmedTCPContract(scenario); err != nil {
						fixture.close()
						t.Fatalf("%s/%s warmed contract: %v", testCase.route, testCase.profileName, err)
					}
					if err := measurePerfvarExactWarmedDirection(fixture, scenario); err != nil {
						fixture.close()
						t.Fatalf(
							"%s/%s/%s warmed TCP: %v",
							testCase.route,
							testCase.profileName,
							testCase.direction,
							err,
						)
					}
					fixture.close()
				})
			},
		)
	}
}
