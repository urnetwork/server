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
var testLagExtenderRestartLoopRule = &LagExtenderRestartLoopRule{
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
// one family, or soon after a revocation, never count. The uptime probes are
// not read.
func TestGetLagExtenderRestartLoops(t *testing.T) {
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
		// its latest probes failed, and it has one pass
		probeFailed := testLagStabilityExtender(ctx, 4, 6)
		testLagStabilityPass(ctx, probeFailed, minutes(30), 5*time.Second)
		RecordNetworkExtenderProbeResult(ctx, probeFailed, 4, false, minutes(3), 6, nil)
		RecordNetworkExtenderProbeResult(ctx, probeFailed, 6, false, minutes(3), 6, nil)
		// never activated
		unknown := testLagStabilityExtender(ctx, 4, 6)

		loops := GetLagExtenderRestartLoops(ctx, []server.Id{
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
			unknown,
		}, now, testLagExtenderRestartLoopRule)

		looped := []server.Id{}
		for _, extenderId := range []server.Id{
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
			unknown,
		} {
			if _, ok := loops[extenderId]; ok {
				looped = append(looped, extenderId)
			}
		}
		connect.AssertEqual(t, looped, []server.Id{looping, held, joined, revokedEarlier})
		connect.AssertEqual(t, len(loops), 4)

		// each with the start of the pass that ended its latest burst
		connect.AssertEqual(t, loops[looping].Sub(minutes(10)).Abs() < time.Millisecond, true)
		connect.AssertEqual(t, loops[held].Sub(minutes(119)).Abs() < time.Millisecond, true)

		connect.AssertEqual(t, len(GetLagExtenderRestartLoops(ctx, nil, now, testLagExtenderRestartLoopRule)), 0)
	})
}
