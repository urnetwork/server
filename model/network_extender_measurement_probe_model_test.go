package model

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/lagprobe"
)

// The run gap of these tests, the sample's: a probe older than this does not
// continue a run of failures.
const testMeasurementProbeRunGap = 15 * time.Minute

// Stores one probe at `at`.
func recordTestMeasurementProbe(ctx context.Context, extenderId server.Id, ip string, outcome lagprobe.Outcome, at time.Time) {
	RecordNetworkExtenderMeasurementProbes(ctx, []*NetworkExtenderMeasurementProbeResult{
		{ExtenderId: extenderId, Ip: netip.MustParseAddr(ip), Outcome: outcome},
	}, at, testMeasurementProbeRunGap)
}

// The latest probe of one family of one extender, nil when it has none.
func getTestMeasurementProbe(ctx context.Context, extenderId server.Id, ipVersion int) *NetworkExtenderMeasurementProbe {
	for _, probe := range GetNetworkExtenderMeasurementProbes(ctx, []server.Id{extenderId})[extenderId] {
		if probe.IpVersion == ipVersion {
			return probe
		}
	}
	return nil
}

// The times of these tests, whole microseconds as the column stores them.
func testMeasurementProbeNow() time.Time {
	return server.NowUtc().Truncate(time.Microsecond)
}

// A failure adds one to the failures in a row and a success clears them, each
// keeping the probe time, the outcome and the latest success as the column
// reads them.
func TestRecordNetworkExtenderMeasurementProbesCountsAFamilysRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		extenderId := server.NewId()
		ip := "192.0.2.1"
		start := testMeasurementProbeNow().Add(-time.Hour)

		connect.AssertEqual(t, getTestMeasurementProbe(ctx, extenderId, 4), (*NetworkExtenderMeasurementProbe)(nil))

		// the first failure
		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeTlsTimeout, start)
		probe := getTestMeasurementProbe(ctx, extenderId, 4)
		connect.AssertEqual(t, probe.ExtenderId, extenderId)
		connect.AssertEqual(t, probe.Ip, netip.MustParseAddr(ip))
		connect.AssertEqual(t, probe.ProbeTime.UTC(), start)
		connect.AssertEqual(t, probe.Outcome, lagprobe.OutcomeTlsTimeout)
		connect.AssertEqual(t, probe.SuccessTime, (*time.Time)(nil))
		connect.AssertEqual(t, probe.ConsecutiveFailures, 1)

		// the next, of any kind
		second := start.Add(2 * time.Minute)
		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeDial, second)
		probe = getTestMeasurementProbe(ctx, extenderId, 4)
		connect.AssertEqual(t, probe.ProbeTime.UTC(), second)
		connect.AssertEqual(t, probe.Outcome, lagprobe.OutcomeDial)
		connect.AssertEqual(t, probe.SuccessTime, (*time.Time)(nil))
		connect.AssertEqual(t, probe.ConsecutiveFailures, 2)

		// a success clears them and is remembered
		third := start.Add(4 * time.Minute)
		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeOk, third)
		probe = getTestMeasurementProbe(ctx, extenderId, 4)
		connect.AssertEqual(t, probe.Outcome, lagprobe.OutcomeOk)
		connect.AssertEqual(t, probe.SuccessTime.UTC(), third)
		connect.AssertEqual(t, probe.ConsecutiveFailures, 0)

		// the success outlives the failure after it
		fourth := start.Add(6 * time.Minute)
		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeCors, fourth)
		probe = getTestMeasurementProbe(ctx, extenderId, 4)
		connect.AssertEqual(t, probe.Outcome, lagprobe.OutcomeCors)
		connect.AssertEqual(t, probe.SuccessTime.UTC(), third)
		connect.AssertEqual(t, probe.ConsecutiveFailures, 1)
	})
}

// Unprobed says nothing about the extender: it is no failure, it keeps the
// latest success, and it breaks a run, so the failures either side of it are
// not one after another.
func TestRecordNetworkExtenderMeasurementProbesUnprobedIsNoFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := testMeasurementProbeNow().Add(-time.Hour)

		// a first probe that could not be made
		neverProbed := server.NewId()
		recordTestMeasurementProbe(ctx, neverProbed, "2001:db8::1", lagprobe.OutcomeUnprobed, start)
		probe := getTestMeasurementProbe(ctx, neverProbed, 6)
		connect.AssertEqual(t, probe.Outcome, lagprobe.OutcomeUnprobed)
		connect.AssertEqual(t, probe.ProbeTime.UTC(), start)
		connect.AssertEqual(t, probe.SuccessTime, (*time.Time)(nil))
		connect.AssertEqual(t, probe.ConsecutiveFailures, 0)

		extenderId := server.NewId()
		ip := "2001:db8::2"
		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeOk, start)
		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeDial, start.Add(2*time.Minute))
		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeDial, start.Add(4*time.Minute))
		connect.AssertEqual(t, getTestMeasurementProbe(ctx, extenderId, 6).ConsecutiveFailures, 2)

		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeUnprobed, start.Add(6*time.Minute))
		probe = getTestMeasurementProbe(ctx, extenderId, 6)
		connect.AssertEqual(t, probe.Outcome, lagprobe.OutcomeUnprobed)
		connect.AssertEqual(t, probe.ProbeTime.UTC(), start.Add(6*time.Minute))
		connect.AssertEqual(t, probe.SuccessTime.UTC(), start)
		connect.AssertEqual(t, probe.ConsecutiveFailures, 0)

		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeDial, start.Add(8*time.Minute))
		connect.AssertEqual(t, getTestMeasurementProbe(ctx, extenderId, 6).ConsecutiveFailures, 1)
	})
}

// A failure more than the run gap after the last probe starts a new run: the
// chain stopped between them. At exactly the gap it still continues the run.
func TestRecordNetworkExtenderMeasurementProbesAGapStartsANewRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		extenderId := server.NewId()
		ip := "192.0.2.1"
		at := testMeasurementProbeNow().Add(-2 * time.Hour)

		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeDial, at)
		at = at.Add(testMeasurementProbeRunGap)
		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeDial, at)
		connect.AssertEqual(t, getTestMeasurementProbe(ctx, extenderId, 4).ConsecutiveFailures, 2)

		at = at.Add(testMeasurementProbeRunGap + time.Second)
		recordTestMeasurementProbe(ctx, extenderId, ip, lagprobe.OutcomeDial, at)
		connect.AssertEqual(t, getTestMeasurementProbe(ctx, extenderId, 4).ConsecutiveFailures, 1)
	})
}

// A family that moved to a new address keeps nothing of the old one: its
// failures and its latest success were another machine's, perhaps.
func TestRecordNetworkExtenderMeasurementProbesAMovedAddressStartsOver(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := testMeasurementProbeNow().Add(-time.Hour)

		// a failure at the new address is its first
		extenderId := server.NewId()
		recordTestMeasurementProbe(ctx, extenderId, "192.0.2.1", lagprobe.OutcomeOk, start)
		recordTestMeasurementProbe(ctx, extenderId, "192.0.2.1", lagprobe.OutcomeDial, start.Add(2*time.Minute))
		recordTestMeasurementProbe(ctx, extenderId, "192.0.2.1", lagprobe.OutcomeDial, start.Add(4*time.Minute))
		recordTestMeasurementProbe(ctx, extenderId, "192.0.2.2", lagprobe.OutcomeTls, start.Add(6*time.Minute))
		probe := getTestMeasurementProbe(ctx, extenderId, 4)
		connect.AssertEqual(t, probe.Ip, netip.MustParseAddr("192.0.2.2"))
		connect.AssertEqual(t, probe.ConsecutiveFailures, 1)
		connect.AssertEqual(t, probe.SuccessTime, (*time.Time)(nil))

		// unprobed at the new address keeps nothing either
		recordTestMeasurementProbe(ctx, extenderId, "192.0.2.2", lagprobe.OutcomeOk, start.Add(8*time.Minute))
		recordTestMeasurementProbe(ctx, extenderId, "192.0.2.3", lagprobe.OutcomeUnprobed, start.Add(10*time.Minute))
		probe = getTestMeasurementProbe(ctx, extenderId, 4)
		connect.AssertEqual(t, probe.Ip, netip.MustParseAddr("192.0.2.3"))
		connect.AssertEqual(t, probe.ConsecutiveFailures, 0)
		connect.AssertEqual(t, probe.SuccessTime, (*time.Time)(nil))

		// a success at the new address is its own
		recordTestMeasurementProbe(ctx, extenderId, "192.0.2.4", lagprobe.OutcomeOk, start.Add(12*time.Minute))
		probe = getTestMeasurementProbe(ctx, extenderId, 4)
		connect.AssertEqual(t, probe.SuccessTime.UTC(), start.Add(12*time.Minute))
	})
}

// One pass stores every extender and family in one call, each with its own
// run. A family appears once however often a pass names it, the last result
// standing, and a mapped address is stored as its IPv4 address.
func TestRecordNetworkExtenderMeasurementProbesStoresAPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := testMeasurementProbeNow().Add(-time.Hour)
		a := server.NewId()
		b := server.NewId()
		other := server.NewId()
		recordTestMeasurementProbe(ctx, other, "192.0.2.9", lagprobe.OutcomeDial, start)

		RecordNetworkExtenderMeasurementProbes(ctx, []*NetworkExtenderMeasurementProbeResult{
			{ExtenderId: a, Ip: netip.MustParseAddr("192.0.2.1"), Outcome: lagprobe.OutcomeTlsTimeout},
			{ExtenderId: a, Ip: netip.MustParseAddr("2001:db8::1"), Outcome: lagprobe.OutcomeOk},
			{ExtenderId: b, Ip: netip.MustParseAddr("2001:db8::2"), Outcome: lagprobe.OutcomeDial},
			{ExtenderId: b, Ip: netip.MustParseAddr("2001:db8::2"), Outcome: lagprobe.OutcomeHttp},
			{ExtenderId: b, Ip: netip.MustParseAddr("::ffff:192.0.2.2"), Outcome: lagprobe.OutcomeStatus},
		}, start, testMeasurementProbeRunGap)

		probes := GetNetworkExtenderMeasurementProbes(ctx, []server.Id{a, b, server.NewId()})
		connect.AssertEqual(t, len(probes), 2)
		connect.AssertEqual(t, len(probes[a]), 2)
		connect.AssertEqual(t, len(probes[b]), 2)
		// each extender's families in order
		connect.AssertEqual(t, probes[a][0].IpVersion, 4)
		connect.AssertEqual(t, probes[a][0].Outcome, lagprobe.OutcomeTlsTimeout)
		connect.AssertEqual(t, probes[a][1].IpVersion, 6)
		connect.AssertEqual(t, probes[a][1].Outcome, lagprobe.OutcomeOk)
		connect.AssertEqual(t, probes[a][1].SuccessTime.UTC(), start)
		connect.AssertEqual(t, probes[b][0].IpVersion, 4)
		connect.AssertEqual(t, probes[b][0].Ip, netip.MustParseAddr("192.0.2.2"))
		connect.AssertEqual(t, probes[b][1].Outcome, lagprobe.OutcomeHttp)
		connect.AssertEqual(t, probes[b][1].ConsecutiveFailures, 1)

		// nothing to store, or to read, is no query
		RecordNetworkExtenderMeasurementProbes(ctx, nil, start, testMeasurementProbeRunGap)
		connect.AssertEqual(t, len(GetNetworkExtenderMeasurementProbes(ctx, nil)), 0)
	})
}

// A pass's probes read back by the time they were stored at, and only while they
// are their family's latest: a later probe of one family takes that family out
// of what is left of the pass, and leaves the others.
func TestGetNetworkExtenderMeasurementProbesAtReadsWhatIsLeftOfAPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		passTime := testMeasurementProbeNow().Add(-time.Hour)
		a := server.NewId()
		b := server.NewId()
		other := server.NewId()
		RecordNetworkExtenderMeasurementProbes(ctx, []*NetworkExtenderMeasurementProbeResult{
			{ExtenderId: a, Ip: netip.MustParseAddr("192.0.2.1"), Outcome: lagprobe.OutcomeTlsTimeout},
			{ExtenderId: a, Ip: netip.MustParseAddr("2001:db8::1"), Outcome: lagprobe.OutcomeOk},
			{ExtenderId: b, Ip: netip.MustParseAddr("192.0.2.2"), Outcome: lagprobe.OutcomeUnprobed},
		}, passTime, testMeasurementProbeRunGap)
		recordTestMeasurementProbe(ctx, other, "192.0.2.3", lagprobe.OutcomeDial, passTime.Add(-2*time.Minute))

		read := func() map[[2]string]lagprobe.Outcome {
			outcomes := map[[2]string]lagprobe.Outcome{}
			for _, probe := range GetNetworkExtenderMeasurementProbesAt(ctx, passTime) {
				connect.AssertEqual(t, probe.ProbeTime.UTC(), passTime)
				outcomes[[2]string{probe.ExtenderId.String(), probe.Ip.String()}] = probe.Outcome
			}
			return outcomes
		}
		connect.AssertEqual(t, read(), map[[2]string]lagprobe.Outcome{
			{a.String(), "192.0.2.1"}:   lagprobe.OutcomeTlsTimeout,
			{a.String(), "2001:db8::1"}: lagprobe.OutcomeOk,
			{b.String(), "192.0.2.2"}:   lagprobe.OutcomeUnprobed,
		})

		// a second look at one family moves it on
		recordTestMeasurementProbe(ctx, a, "192.0.2.1", lagprobe.OutcomeTlsTimeout, passTime.Add(30*time.Second))
		connect.AssertEqual(t, read(), map[[2]string]lagprobe.Outcome{
			{a.String(), "2001:db8::1"}: lagprobe.OutcomeOk,
			{b.String(), "192.0.2.2"}:   lagprobe.OutcomeUnprobed,
		})

		connect.AssertEqual(t, len(GetNetworkExtenderMeasurementProbesAt(ctx, passTime.Add(time.Second))), 0)
	})
}

// The stats count the rows probed since the cut by outcome, a row older than the
// cut in no count, and name the newest probe of any row, however old: that age is
// what says the chain stopped. An empty table has no newest probe.
func TestGetNetworkExtenderMeasurementProbeStats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := testMeasurementProbeNow()
		cut := now.Add(-15 * time.Minute)

		empty := GetNetworkExtenderMeasurementProbeStats(ctx, cut)
		connect.AssertEqual(t, len(empty.OutcomeCounts), 0)
		connect.AssertEqual(t, empty.NewestProbeTime, (*time.Time)(nil))

		// a pass an hour ago, past the cut
		recordTestMeasurementProbe(ctx, server.NewId(), "192.0.2.1", lagprobe.OutcomeTlsTimeout, now.Add(-time.Hour))
		stale := GetNetworkExtenderMeasurementProbeStats(ctx, cut)
		connect.AssertEqual(t, stale.OutcomeCounts, map[string]int64{string(lagprobe.OutcomeTlsTimeout): 0})
		connect.AssertEqual(t, stale.NewestProbeTime.UTC(), now.Add(-time.Hour))

		// and the last one, with a guard that tripped
		RecordNetworkExtenderMeasurementProbes(ctx, []*NetworkExtenderMeasurementProbeResult{
			{ExtenderId: server.NewId(), Ip: netip.MustParseAddr("192.0.2.2"), Outcome: lagprobe.OutcomeOk},
			{ExtenderId: server.NewId(), Ip: netip.MustParseAddr("192.0.2.3"), Outcome: lagprobe.OutcomeOk},
			{ExtenderId: server.NewId(), Ip: netip.MustParseAddr("2001:db8::1"), Outcome: lagprobe.OutcomeUnprobed},
		}, now.Add(-time.Minute), testMeasurementProbeRunGap)
		stats := GetNetworkExtenderMeasurementProbeStats(ctx, cut)
		connect.AssertEqual(t, stats.OutcomeCounts, map[string]int64{
			string(lagprobe.OutcomeOk):         2,
			string(lagprobe.OutcomeUnprobed):   1,
			string(lagprobe.OutcomeTlsTimeout): 0,
		})
		connect.AssertEqual(t, stats.NewestProbeTime.UTC(), now.Add(-time.Minute))
	})
}

// The retention takes what was probed before the cut and nothing else, and
// says how many.
func TestRemoveNetworkExtenderMeasurementProbesBefore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := testMeasurementProbeNow()
		old := server.NewId()
		recordTestMeasurementProbe(ctx, old, "192.0.2.1", lagprobe.OutcomeOk, now.Add(-8*24*time.Hour))
		recordTestMeasurementProbe(ctx, old, "2001:db8::1", lagprobe.OutcomeDial, now.Add(-8*24*time.Hour))
		atCut := server.NewId()
		recordTestMeasurementProbe(ctx, atCut, "192.0.2.2", lagprobe.OutcomeOk, now.Add(-7*24*time.Hour))
		recent := server.NewId()
		recordTestMeasurementProbe(ctx, recent, "192.0.2.3", lagprobe.OutcomeOk, now)

		connect.AssertEqual(t, RemoveNetworkExtenderMeasurementProbesBefore(ctx, now.Add(-7*24*time.Hour)), int64(2))
		connect.AssertEqual(t, getTestMeasurementProbe(ctx, old, 4), (*NetworkExtenderMeasurementProbe)(nil))
		connect.AssertEqual(t, getTestMeasurementProbe(ctx, old, 6), (*NetworkExtenderMeasurementProbe)(nil))
		connect.AssertEqual(t, getTestMeasurementProbe(ctx, atCut, 4) != nil, true)
		connect.AssertEqual(t, getTestMeasurementProbe(ctx, recent, 4) != nil, true)
		connect.AssertEqual(t, RemoveNetworkExtenderMeasurementProbesBefore(ctx, now.Add(-7*24*time.Hour)), int64(0))
	})
}
