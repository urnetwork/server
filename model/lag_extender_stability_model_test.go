package model

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The rule of these tests: three passes of both families within 90 minutes,
// held two hours, with rows less than 30 seconds apart one pass, and a pass
// less than 30 minutes after a revocation not counted.
var testLagExtenderStabilityRule = &LagExtenderStabilityRule{
	PassGap:          30 * time.Second,
	BurstPasses:      3,
	BurstWindow:      90 * time.Minute,
	BurstHold:        2 * time.Hour,
	RevocationWindow: 30 * time.Minute,
}

// An extender with an active address of each given family, never probed.
func testLagStabilityExtender(ctx context.Context, ipVersions ...int) server.Id {
	extenderId := server.NewId()
	addresses := []*NetworkExtenderAddress{}
	for _, ipVersion := range ipVersions {
		ip := netip.MustParseAddr("192.0.2.1")
		if ipVersion == 6 {
			ip = netip.MustParseAddr("2001:db8::1")
		}
		addresses = append(addresses, &NetworkExtenderAddress{
			ExtenderId:   extenderId,
			IpVersion:    ipVersion,
			Ip:           ip,
			Carriers:     []string{connect.ExtenderCarrierTcp},
			ActivateTime: server.NowUtc(),
			Active:       true,
		})
	}
	Testing_CreateNetworkExtender(ctx, &NetworkExtender{
		ExtenderId: extenderId,
		NetworkId:  server.NewId(),
		ClientId:   server.NewId(),
		PublicKey:  server.NewId().Bytes(),
		CreateTime: server.NowUtc(),
		TcpPort:    443,
		Active:     true,
	}, addresses)
	return extenderId
}

// A pass that activates IPv4 and then IPv6 `gap` later, at `at`.
func testLagStabilityPass(ctx context.Context, extenderId server.Id, at time.Time, gap time.Duration) {
	Testing_AddNetworkExtenderActivation(ctx, extenderId, 4, at)
	Testing_AddNetworkExtenderActivation(ctx, extenderId, 6, at.Add(gap))
}

// A burst is three passes of both families within the window, the last within
// the hold; rows a pass gap or more apart are separate passes, and passes of
// one family, or soon after a revocation, never count. The latest uptime probe
// of an active address that failed makes an extender unstable, and one of an
// inactive address, one that succeeded or none at all does not.
func TestGetLagExtenderStability(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		minutes := func(m float64) time.Time {
			return now.Add(-time.Duration(m * float64(time.Minute)))
		}

		// three passes within 89 minutes, the last 10 minutes ago
		looping := testLagStabilityExtender(ctx, 4, 6)
		for _, m := range []float64{99, 60, 10} {
			testLagStabilityPass(ctx, looping, minutes(m), 5*time.Second)
		}
		// the same burst ended 119 minutes ago, so it is still held
		held := testLagStabilityExtender(ctx, 4, 6)
		for _, m := range []float64{200, 160, 119} {
			testLagStabilityPass(ctx, held, minutes(m), 5*time.Second)
		}
		// and 121 minutes ago, past the hold
		recovered := testLagStabilityExtender(ctx, 4, 6)
		for _, m := range []float64{200, 160, 121} {
			testLagStabilityPass(ctx, recovered, minutes(m), 5*time.Second)
		}
		// three passes 91 minutes from first to last are no burst
		spread := testLagStabilityExtender(ctx, 4, 6)
		for _, m := range []float64{101, 55, 10} {
			testLagStabilityPass(ctx, spread, minutes(m), 5*time.Second)
		}
		// three passes whose families are 35 seconds apart are six passes of
		// one family each, which count for nothing
		split := testLagStabilityExtender(ctx, 4, 6)
		for _, m := range []float64{60, 40, 20} {
			testLagStabilityPass(ctx, split, minutes(m), 35*time.Second)
		}
		// and 25 seconds apart are three passes of both
		joined := testLagStabilityExtender(ctx, 4, 6)
		for _, m := range []float64{60, 40, 20} {
			testLagStabilityPass(ctx, joined, minutes(m), 25*time.Second)
		}
		// five passes of one family in an hour, as a key many clients
		// activate posts them
		oneFamily := testLagStabilityExtender(ctx, 4)
		for _, m := range []float64{55, 41, 30, 12, 2} {
			Testing_AddNetworkExtenderActivation(ctx, oneFamily, 4, minutes(m))
		}
		// revoked by its probes every 40 minutes, and re-activated 10
		// minutes after each revocation, published or not yet
		reactivated := testLagStabilityExtender(ctx, 4, 6)
		for _, m := range []float64{90, 50, 10} {
			Testing_AddNetworkExtenderRevocation(ctx, reactivated, minutes(m+10))
			testLagStabilityPass(ctx, reactivated, minutes(m), 5*time.Second)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_extender_publish SET published_time = create_time + interval '1 second' WHERE extender_id = $1 AND create_time < $2`,
				reactivated,
				minutes(40),
			))
		})
		// a revocation 31 minutes before each pass leaves it counted
		revokedEarlier := testLagStabilityExtender(ctx, 4, 6)
		for _, m := range []float64{90, 50, 10} {
			Testing_AddNetworkExtenderRevocation(ctx, revokedEarlier, minutes(m+31))
			testLagStabilityPass(ctx, revokedEarlier, minutes(m), 5*time.Second)
		}

		// the latest probe of the IPv6 address failed
		probeFailed := testLagStabilityExtender(ctx, 4, 6)
		RecordNetworkExtenderProbeResult(ctx, probeFailed, 4, true, minutes(3), 6, nil)
		RecordNetworkExtenderProbeResult(ctx, probeFailed, 6, true, minutes(8), 6, nil)
		RecordNetworkExtenderProbeResult(ctx, probeFailed, 6, false, minutes(3), 6, nil)
		// a failure before the latest success does not count
		probeRecovered := testLagStabilityExtender(ctx, 4)
		RecordNetworkExtenderProbeResult(ctx, probeRecovered, 4, false, minutes(8), 6, nil)
		RecordNetworkExtenderProbeResult(ctx, probeRecovered, 4, true, minutes(3), 6, nil)
		// a failed probe stands through a later activation, which resets the
		// failure count but proves nothing about the next probe
		probeRestarted := testLagStabilityExtender(ctx, 4)
		RecordNetworkExtenderProbeResult(ctx, probeRestarted, 4, false, minutes(4), 6, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_extender_address SET consecutive_probe_failures = 0, activate_time = $2 WHERE extender_id = $1`,
				probeRestarted,
				minutes(1),
			))
		})
		// the failed address is no longer active, and the other serves
		inactiveFailed := testLagStabilityExtender(ctx, 4, 6)
		RecordNetworkExtenderProbeResult(ctx, inactiveFailed, 6, false, minutes(3), 6, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_extender_address SET active = false WHERE extender_id = $1 AND ip_version = 6`,
				inactiveFailed,
			))
		})
		// never probed and never activated
		unknown := testLagStabilityExtender(ctx, 4, 6)

		stabilities := GetLagExtenderStability(ctx, []server.Id{
			looping,
			held,
			recovered,
			spread,
			split,
			joined,
			oneFamily,
			reactivated,
			revokedEarlier,
			probeFailed,
			probeRecovered,
			probeRestarted,
			inactiveFailed,
			unknown,
		}, now, testLagExtenderStabilityRule)

		unstable := func(extenderId server.Id) bool {
			stability := stabilities[extenderId]
			return stability != nil && stability.Unstable()
		}
		connect.AssertEqual(t, unstable(looping), true)
		connect.AssertEqual(t, unstable(held), true)
		connect.AssertEqual(t, unstable(recovered), false)
		connect.AssertEqual(t, unstable(spread), false)
		connect.AssertEqual(t, unstable(split), false)
		connect.AssertEqual(t, unstable(joined), true)
		connect.AssertEqual(t, unstable(oneFamily), false)
		connect.AssertEqual(t, unstable(reactivated), false)
		connect.AssertEqual(t, unstable(revokedEarlier), true)
		connect.AssertEqual(t, unstable(probeFailed), true)
		connect.AssertEqual(t, unstable(probeRecovered), false)
		connect.AssertEqual(t, unstable(probeRestarted), true)
		connect.AssertEqual(t, unstable(inactiveFailed), false)
		connect.AssertEqual(t, unstable(unknown), false)
		connect.AssertEqual(t, len(stabilities), 6)

		// the evidence: the burst's last pass, and the failed probe
		connect.AssertEqual(t, stabilities[looping].ProbeFailed, false)
		connect.AssertEqual(t, stabilities[looping].BurstPassTime.Sub(minutes(10)).Abs() < time.Millisecond, true)
		connect.AssertEqual(t, stabilities[held].BurstPassTime.Sub(minutes(119)).Abs() < time.Millisecond, true)
		connect.AssertEqual(t, stabilities[probeFailed].ProbeFailed, true)
		connect.AssertEqual(t, stabilities[probeFailed].BurstPassTime, (*time.Time)(nil))

		connect.AssertEqual(t, len(GetLagExtenderStability(ctx, nil, now, testLagExtenderStabilityRule)), 0)
	})
}
