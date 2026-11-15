package model

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The rule of these tests, the controller's: four passes of both families
// within two hours, held two hours, with rows less than 30 seconds apart one
// pass, and a pass less than 30 minutes after a revocation not counted.
var testLagExtenderRestartLoopRule = &LagExtenderRestartLoopRule{
	PassGap:          30 * time.Second,
	BurstPasses:      4,
	BurstWindow:      2 * time.Hour,
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

// A burst is four passes of both families within the window, the last within
// the hold; rows a pass gap or more apart are separate passes, and passes of
// one family, or soon after a revocation, never count. Each bound is exact:
// rows 30 s apart are two passes, four passes 120 min apart are no burst, a
// burst that ended 2 h ago is not held, and a pass 30 min after a revocation
// counts. Connect's timer grid makes no burst. The uptime probes are not read.
func TestGetLagExtenderRestartLoops(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// whole microseconds, as the timestamps are stored, so the bounds are
		// exact
		now := server.NowUtc().Truncate(time.Microsecond)
		ago := func(d time.Duration) time.Time {
			return now.Add(-d)
		}
		m := time.Minute
		us := time.Microsecond

		names := []string{}
		ids := map[string]server.Id{}
		want := map[string]bool{}
		add := func(name string, inLoop bool, ipVersions ...int) server.Id {
			if len(ipVersions) == 0 {
				ipVersions = []int{4, 6}
			}
			extenderId := testLagStabilityExtender(ctx, ipVersions...)
			names = append(names, name)
			ids[name] = extenderId
			want[name] = inLoop
			return extenderId
		}
		passes := func(extenderId server.Id, gap time.Duration, agos ...time.Duration) {
			for _, a := range agos {
				testLagStabilityPass(ctx, extenderId, ago(a), gap)
			}
		}

		// four passes within 90 minutes, the last 10 minutes ago
		passes(add("looping", true), 5*time.Second, 100*m, 70*m, 40*m, 10*m)
		// three passes are no burst, however close
		passes(add("three passes", false), 5*time.Second, 60*m, 40*m, 20*m)
		// a burst that ended 119 minutes ago is still held, and one that
		// ended exactly 2 h ago, or 1 us less than that, is held or not
		passes(add("held", true), 5*time.Second, 230*m, 200*m, 160*m, 119*m)
		passes(add("hold exactly 2h", false), 5*time.Second, 230*m, 200*m, 160*m, 120*m)
		passes(add("hold 2h minus 1us", true), 5*time.Second, 230*m, 200*m, 160*m, 120*m-us)
		// four passes exactly 120 minutes from first to last are no burst,
		// and 1 us less is one
		passes(add("span exactly 120m", false), 2*time.Second, 130*m, 100*m, 60*m, 10*m)
		passes(add("span 120m minus 1us", true), 2*time.Second, 130*m-us, 100*m, 60*m, 10*m)
		// families exactly 30 s apart are two passes of one family each,
		// which count for nothing, and 1 us less is one pass of both
		passes(add("gap exactly 30s", false), 30*time.Second, 70*m, 50*m, 30*m, 10*m)
		passes(add("gap 30s minus 1us", true), 30*time.Second-us, 70*m, 50*m, 30*m, 10*m)
		// five passes of one family in an hour, as a key many clients
		// activate posts them
		oneFamily := add("one family", false, 4)
		for _, a := range []time.Duration{55 * m, 41 * m, 30 * m, 12 * m, 2 * m} {
			Testing_AddNetworkExtenderActivation(ctx, oneFamily, 4, ago(a))
		}
		// connect's timer grid: a backoff success, then hourly checks 40
		// and 90 minutes later, 0.12 s fast as on Main, and the next check
		// an hour after that
		passes(
			add("timer grid", false),
			4*time.Second,
			160*m-240*time.Millisecond,
			120*m-240*time.Millisecond,
			70*m-120*time.Millisecond,
			10*m,
		)

		// revoked by its probes every 40 minutes, and re-activated 10
		// minutes after each revocation, published or not yet
		reactivated := add("re-activated", false)
		for _, a := range []time.Duration{130 * m, 90 * m, 50 * m, 10 * m} {
			Testing_AddNetworkExtenderRevocation(ctx, reactivated, ago(a+10*m))
			passes(reactivated, 5*time.Second, a)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_extender_publish SET published_time = create_time + interval '1 second' WHERE extender_id = $1 AND create_time < $2`,
				reactivated,
				ago(40*m),
			))
		})
		// a pass exactly 30 minutes after a revocation counts, one at the
		// revocation's instant does not, and a revocation after a pass's
		// first row leaves the pass counted. The passes are 35 minutes apart,
		// so each revocation is within 30 minutes of its own pass only
		for _, revocation := range []struct {
			name   string
			inLoop bool
			before time.Duration
		}{
			{"revocation exactly 30m before", true, 30 * m},
			{"revocation at the pass", false, 0},
			{"revocation 1s into the pass", true, -time.Second},
		} {
			extenderId := add(revocation.name, revocation.inLoop)
			for _, a := range []time.Duration{115 * m, 80 * m, 45 * m, 10 * m} {
				Testing_AddNetworkExtenderRevocation(ctx, extenderId, ago(a+revocation.before))
				passes(extenderId, 2*time.Second, a)
			}
		}
		// the passes before a burst's last are counted passes: restarts 140,
		// 30, 20 and 10 minutes ago span 130 minutes, though a re-activation
		// 60 minutes ago, after a revocation 65 minutes ago, would make the
		// last four span 50
		skipped := add("burst skips a re-activation", false)
		passes(skipped, 2*time.Second, 140*m)
		Testing_AddNetworkExtenderRevocation(ctx, skipped, ago(65*m))
		passes(skipped, 2*time.Second, 60*m, 30*m, 20*m, 10*m)

		// its latest probes failed, and it has one pass
		probeFailed := add("probe failed", false)
		passes(probeFailed, 5*time.Second, 30*m)
		RecordNetworkExtenderProbeResult(ctx, probeFailed, 4, false, ago(3*m), 6, nil)
		RecordNetworkExtenderProbeResult(ctx, probeFailed, 6, false, ago(3*m), 6, nil)
		// never activated
		add("never activated", false)

		extenderIds := []server.Id{}
		for _, name := range names {
			extenderIds = append(extenderIds, ids[name])
		}
		loops := GetLagExtenderRestartLoops(ctx, extenderIds, now, testLagExtenderRestartLoopRule)
		inLoop := 0
		for _, name := range names {
			_, ok := loops[ids[name]]
			if ok != want[name] {
				t.Errorf("%s: in a restart loop = %t, want %t", name, ok, want[name])
			}
			if want[name] {
				inLoop += 1
			}
		}
		connect.AssertEqual(t, len(loops), inLoop)

		// each with the start of the pass that ended its latest burst
		connect.AssertEqual(t, loops[ids["looping"]].Equal(ago(10*m)), true)
		connect.AssertEqual(t, loops[ids["held"]].Equal(ago(119*m)), true)

		connect.AssertEqual(t, len(GetLagExtenderRestartLoops(ctx, nil, now, testLagExtenderRestartLoopRule)), 0)
	})
}
