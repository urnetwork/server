package controller

import (
	"context"
	"errors"
	"math"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/lagprobe"
	"github.com/urnetwork/server/model"
)

// Tests of the measurement api probes' rule (lag.dev/PLAN.md A4): an extender
// whose latest probes failed twice in a row on every family they could reach is
// not offered, and whatever the probes do not know offers it.

// The rule's edges, with no database: every family of the extender, its latest
// probe of that address, how many failures, and how old.
func TestLagExtenderUnreachable(t *testing.T) {
	now := server.NowUtc()
	v4 := netip.MustParseAddr("192.0.2.1")
	v6 := netip.MustParseAddr("2001:db8::1")
	probe := func(ip netip.Addr, outcome lagprobe.Outcome, failures int, age time.Duration) *model.NetworkExtenderMeasurementProbe {
		return &model.NetworkExtenderMeasurementProbe{
			IpVersion:           server.IpVersionForAddr(ip),
			Ip:                  ip,
			ProbeTime:           now.Add(-age),
			Outcome:             outcome,
			ConsecutiveFailures: failures,
		}
	}
	failed := func(ip netip.Addr, failures int) *model.NetworkExtenderMeasurementProbe {
		return probe(ip, lagprobe.OutcomeTlsTimeout, failures, 2*time.Minute)
	}
	ok := func(ip netip.Addr) *model.NetworkExtenderMeasurementProbe {
		return probe(ip, lagprobe.OutcomeOk, 0, 2*time.Minute)
	}
	unprobed := func(ip netip.Addr) *model.NetworkExtenderMeasurementProbe {
		return probe(ip, lagprobe.OutcomeUnprobed, 0, 2*time.Minute)
	}
	maxAge := lagExtenderUnreachableRule.ProbeMaxAge
	connect.AssertEqual(t, lagExtenderUnreachableRule.ConsecutiveFailures, 2)
	connect.AssertEqual(t, maxAge, 15*time.Minute)

	for _, test := range []struct {
		name   string
		ips    []netip.Addr
		probes []*model.NetworkExtenderMeasurementProbe
		want   bool
	}{
		// the threshold, on one family
		{name: "one family that passed", ips: []netip.Addr{v4}, probes: []*model.NetworkExtenderMeasurementProbe{ok(v4)}},
		{name: "one failure", ips: []netip.Addr{v4}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 1)}},
		{name: "two failures", ips: []netip.Addr{v4}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 2)}, want: true},
		{name: "many failures", ips: []netip.Addr{v4}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 40)}, want: true},
		{name: "two failures on IPv6 alone", ips: []netip.Addr{v6}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v6, 2)}, want: true},

		// every family must fail
		{name: "both failed", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 2), failed(v6, 2)}, want: true},
		{name: "IPv6 passed", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 9), ok(v6)}},
		{name: "IPv4 passed", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{ok(v4), failed(v6, 9)}},
		{name: "one family short of the threshold", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 9), failed(v6, 1)}},

		// a family that could not be probed counts for neither side
		{name: "the other family unprobed", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 2), unprobed(v6)}, want: true},
		{name: "unprobed beside one failure", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 1), unprobed(v6)}},
		{name: "unprobed beside a pass", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{ok(v4), unprobed(v6)}},
		{name: "every family unprobed", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{unprobed(v4), unprobed(v6)}},
		{name: "its only family unprobed", ips: []netip.Addr{v6}, probes: []*model.NetworkExtenderMeasurementProbe{unprobed(v6)}},

		// missing
		{name: "no probes", ips: []netip.Addr{v4, v6}},
		{name: "no addresses", probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 2)}},
		{name: "no probe of the other family", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 2)}},
		{name: "the probe of a family no longer published", ips: []netip.Addr{v6}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 2)}},
		{name: "the probes of another address", ips: []netip.Addr{v4}, probes: []*model.NetworkExtenderMeasurementProbe{failed(netip.MustParseAddr("192.0.2.2"), 2)}},
		{name: "another address beside a failure", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 2), failed(netip.MustParseAddr("2001:db8::2"), 2)}},

		// stale
		{name: "probed just inside the age", ips: []netip.Addr{v4}, probes: []*model.NetworkExtenderMeasurementProbe{probe(v4, lagprobe.OutcomeDial, 2, maxAge-time.Second)}, want: true},
		{name: "probed at the age", ips: []netip.Addr{v4}, probes: []*model.NetworkExtenderMeasurementProbe{probe(v4, lagprobe.OutcomeDial, 2, maxAge)}},
		{name: "probed past the age", ips: []netip.Addr{v4}, probes: []*model.NetworkExtenderMeasurementProbe{probe(v4, lagprobe.OutcomeDial, 2, maxAge+time.Hour)}},
		{name: "one family stale", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 2), probe(v6, lagprobe.OutcomeDial, 2, maxAge)}},
		{name: "stale unprobed beside a failure", ips: []netip.Addr{v4, v6}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 2), probe(v6, lagprobe.OutcomeUnprobed, 0, maxAge)}},

		// an address as the published list may spell it
		{name: "a mapped address is its IPv4 address", ips: []netip.Addr{netip.MustParseAddr("::ffff:192.0.2.1")}, probes: []*model.NetworkExtenderMeasurementProbe{failed(v4, 2)}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			connect.AssertEqual(t, lagExtenderUnreachable(test.ips, test.probes, now, &lagExtenderUnreachableRule), test.want)
		})
	}
}

// Stores one pass of probes of an extender's address at `at`, as the chain
// does.
func addLagTestProbe(ctx context.Context, extender *lagTestExtender, ip string, outcome lagprobe.Outcome, at time.Time) {
	model.RecordNetworkExtenderMeasurementProbes(ctx, []*model.NetworkExtenderMeasurementProbeResult{
		{ExtenderId: extender.extenderId, Ip: netip.MustParseAddr(ip), Outcome: outcome},
	}, at, ExtenderMeasurementProbeMaxAge)
}

// Two failed probes two minutes apart, the last a minute ago.
func addLagTestFailedProbes(ctx context.Context, extender *lagTestExtender, now time.Time, ips ...string) {
	for _, ip := range ips {
		addLagTestProbe(ctx, extender, ip, lagprobe.OutcomeTlsTimeout, now.Add(-3*time.Minute))
		addLagTestProbe(ctx, extender, ip, lagprobe.OutcomeTlsTimeout, now.Add(-time.Minute))
	}
}

// The extender ids of a serving set, sorted.
func lagTestSetIds(set *lagServingSet) []server.Id {
	ids := []server.Id{}
	for _, extender := range set.extenders {
		ids = append(ids, extender.ExtenderId)
	}
	slices.SortFunc(ids, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	return ids
}

// An extender the probes cannot reach is out of the sample and of the tickets'
// set, and comes back with its first success. The rule applies beside the
// restart loop rule: either one leaves an extender out.
func TestLagExtendersLeaveOutUnreachableExtenders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		// a load lasts until the test says, however slowly the test runs
		settings := DefaultLagSettings()
		settings.ExtenderSampleCache = time.Hour
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		now := server.NowUtc()
		guest := newLagTestGuestSession(t, ctx, "198.51.100.30:1234")

		stalled := newLagServingTestExtender(t, ctx, places, lagTestPlace("Saginaw", "us", 43.4, -83.9), "192.0.2.81", "2001:db8:81::1")
		addLagTestFailedProbes(ctx, stalled, now, "192.0.2.81", "2001:db8:81::1")

		// passed the probes
		healthy := newLagServingTestExtender(t, ctx, places, lagTestPlace("Paris", "fr", 48.9, 2.4), "192.0.2.82", "2001:db8:82::1")
		addLagTestProbe(ctx, healthy, "192.0.2.82", lagprobe.OutcomeOk, now.Add(-time.Minute))
		addLagTestProbe(ctx, healthy, "2001:db8:82::1", lagprobe.OutcomeOk, now.Add(-time.Minute))

		// one family down, which a page on the other reaches
		halfDown := newLagServingTestExtender(t, ctx, places, lagTestPlace("Tokyo", "jp", 35.7, 139.7), "192.0.2.83", "2001:db8:83::1")
		addLagTestFailedProbes(ctx, halfDown, now, "192.0.2.83")
		addLagTestProbe(ctx, halfDown, "2001:db8:83::1", lagprobe.OutcomeOk, now.Add(-time.Minute))

		// one failure so far
		onceFailed := newLagServingTestExtender(t, ctx, places, lagTestPlace("Berlin", "de", 52.5, 13.4), "192.0.2.84")
		addLagTestProbe(ctx, onceFailed, "192.0.2.84", lagprobe.OutcomeDial, now.Add(-time.Minute))

		// never probed
		unknown := newLagServingTestExtender(t, ctx, places, lagTestPlace("Riga", "lv", 56.9, 24.1), "192.0.2.85")

		// IPv4 failed and IPv6 could not be probed
		unprobedV6 := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hong Kong", "hk", 22.3, 114.2), "192.0.2.86", "2001:db8:86::1")
		addLagTestFailedProbes(ctx, unprobedV6, now, "192.0.2.86")
		addLagTestProbe(ctx, unprobedV6, "2001:db8:86::1", lagprobe.OutcomeUnprobed, now.Add(-time.Minute))

		// nothing could be probed
		allUnprobed := newLagServingTestExtender(t, ctx, places, lagTestPlace("Oslo", "no", 59.9, 10.8), "2001:db8:87::1")
		addLagTestProbe(ctx, allUnprobed, "2001:db8:87::1", lagprobe.OutcomeUnprobed, now.Add(-time.Minute))

		// failed, but not since the chain stopped
		stale := newLagServingTestExtender(t, ctx, places, lagTestPlace("Lima", "pe", -12.0, -77.0), "192.0.2.88")
		addLagTestProbe(ctx, stale, "192.0.2.88", lagprobe.OutcomeDial, now.Add(-20*time.Minute))
		addLagTestProbe(ctx, stale, "192.0.2.88", lagprobe.OutcomeDial, now.Add(-18*time.Minute))

		// failed at an address it has since left
		moved := newLagServingTestExtender(t, ctx, places, lagTestPlace("Quito", "ec", -0.2, -78.5), "192.0.2.89")
		addLagTestFailedProbes(ctx, moved, now, "192.0.2.189")

		// a restart loop whose probes pass, left out by the other rule
		looping := newLagServingTestExtender(t, ctx, places, lagTestPlace("Detroit", "us", 42.3, -83.0), "192.0.2.90", "2001:db8:90::1")
		addLagTestPasses(ctx, looping, now, []int{4, 6}, 110*time.Minute, 80*time.Minute, 50*time.Minute, 20*time.Minute)
		addLagTestProbe(ctx, looping, "192.0.2.90", lagprobe.OutcomeOk, now.Add(-time.Minute))
		addLagTestProbe(ctx, looping, "2001:db8:90::1", lagprobe.OutcomeOk, now.Add(-time.Minute))
		Testing_ResetLagServing()

		offered := lagTestSortedIds(healthy, halfDown, onceFailed, unknown, allUnprobed, stale, moved)
		result, err := LagExtenders(guest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, lagTestSampleIds(result), offered)

		// the tickets (A6) read the same set
		set := getLagServing(ctx, server.NowUtc(), settings.ExtenderSampleCache)
		connect.AssertEqual(t, set.get(stalled.extenderId), (*lagServingExtender)(nil))
		connect.AssertEqual(t, set.get(unprobedV6.extenderId), (*lagServingExtender)(nil))
		connect.AssertEqual(t, set.get(looping.extenderId), (*lagServingExtender)(nil))
		connect.AssertEqual(t, set.get(healthy.extenderId) != nil, true)
		connect.AssertEqual(t, lagTestSetIds(set), offered)

		// the probes go on, whatever the sample does (GetLagServingHosts)
		hostIds := []server.Id{}
		for _, host := range GetLagServingHosts(ctx, now) {
			hostIds = append(hostIds, host.ExtenderId)
		}
		for _, extender := range []*lagTestExtender{stalled, unprobedV6, looping, healthy} {
			connect.AssertEqual(t, slices.Contains(hostIds, extender.extenderId), true)
		}

		// a success on one family brings it back, at the next load
		addLagTestProbe(ctx, stalled, "2001:db8:81::1", lagprobe.OutcomeOk, now)
		result, err = LagExtenders(guest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, lagTestSampleIds(result), offered)
		Testing_ResetLagServing()
		result, err = LagExtenders(guest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, lagTestSampleIds(result), lagTestSortedIds(stalled, healthy, halfDown, onceFailed, unknown, allUnprobed, stale, moved))
	})
}

// A track's tickets leave out an extender the probes cannot reach (A6), and so
// does a reissue for a dot whose extender stopped answering after its track was
// issued (L4), which keeps the reissue for another dot.
func TestLagTrackLeavesOutUnreachableExtenders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		useIpAsnDb(t, "")
		stalled := newLagServingTestExtender(t, ctx, places, lagTestPlace("Saginaw", "us", 43.4, -83.9), "192.0.2.91", "2001:db8:91::1")
		steady := newLagServingTestExtender(t, ctx, places, lagTestPlace("Osaka", "jp", 34.7, 135.5), "192.0.2.92", "2001:db8:92::1")
		down := newLagServingTestExtender(t, ctx, places, lagTestPlace("Lima", "pe", -12.0, -77.0), "192.0.2.93")

		guest := newLagTestGuestSession(t, ctx, "198.51.100.30:1234")
		issuedAt := server.NowUtc().Truncate(time.Millisecond)
		addLagTestFailedProbes(ctx, down, issuedAt, "192.0.2.93")
		Testing_ResetLagServing()
		issued, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
			{ExtenderId: stalled.extenderId},
			{ExtenderId: down.extenderId},
			{ExtenderId: steady.extenderId},
		}}, guest, issuedAt, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, issued.Error, (*LagError)(nil))
		ticketIds := []server.Id{}
		for _, ticket := range issued.Tickets {
			ticketIds = append(ticketIds, ticket.ExtenderId)
		}
		slices.SortFunc(ticketIds, func(a server.Id, b server.Id) int {
			return a.Cmp(b)
		})
		connect.AssertEqual(t, ticketIds, lagTestSortedIds(stalled, steady))
		trackId := *issued.TrackId

		// then it stopped answering
		addLagTestFailedProbes(ctx, stalled, issuedAt, "192.0.2.91", "2001:db8:91::1")
		Testing_ResetLagServing()

		position := func(position int) *int {
			return &position
		}
		now := issuedAt.Add(5 * time.Minute)
		refused, err := lagTrack(&LagTrackArgs{TrackId: &trackId, Extenders: []*LagTrackExtenderArgs{
			{ExtenderId: stalled.extenderId, Position: position(0)},
		}}, guest, now, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refused.Error, &LagError{
			Code:    LagErrorCodeUnavailable,
			Message: "None of the extenders can be measured now.",
		})
		connect.AssertEqual(t, model.GetLagTrack(ctx, trackId).ReissueCount, 0)

		// the other dot, in the place its request gave it, reconnects with the
		// reissue the track kept
		reissued, err := lagTrack(&LagTrackArgs{TrackId: &trackId, Extenders: []*LagTrackExtenderArgs{
			{ExtenderId: steady.extenderId, Position: position(2)},
		}}, guest, now, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reissued.Error, (*LagError)(nil))
		connect.AssertEqual(t, len(reissued.Tickets), 1)
	})
}

// Replaces the probes' read with one that raises, for the rest of the test or
// until the returned function restores it.
func failLagTestProbeRead(t testing.TB) func() {
	t.Helper()
	previous := lagReadMeasurementProbes
	restore := func() {
		lagReadMeasurementProbes = previous
	}
	t.Cleanup(restore)
	lagReadMeasurementProbes = func(ctx context.Context, extenderIds []server.Id) map[server.Id][]*model.NetworkExtenderMeasurementProbe {
		server.Raise(errors.New("the probes query failed"))
		return nil
	}
	return restore
}

// When the probes cannot be read, every serving extender is offered, as before
// the rule, whether the read raises or the table itself is not there.
func TestLagExtendersOfferEveryExtenderWhenProbesAreUnreadable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		defer Testing_SetLagSettings(DefaultLagSettings())()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		now := server.NowUtc()
		guest := newLagTestGuestSession(t, ctx, "198.51.100.30:1234")

		stalled := newLagServingTestExtender(t, ctx, places, lagTestPlace("Saginaw", "us", 43.4, -83.9), "192.0.2.41", "2001:db8:41::1")
		addLagTestFailedProbes(ctx, stalled, now, "192.0.2.41", "2001:db8:41::1")
		healthy := newLagServingTestExtender(t, ctx, places, lagTestPlace("Paris", "fr", 48.9, 2.4), "192.0.2.42")

		restore := failLagTestProbeRead(t)
		Testing_ResetLagServing()
		result, err := LagExtenders(guest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, lagTestSampleIds(result), lagTestSortedIds(stalled, healthy))

		// with the read restored, the extender is left out
		restore()
		Testing_ResetLagServing()
		result, err = LagExtenders(guest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, lagTestSampleIds(result), lagTestSortedIds(healthy))

		// a read the database itself refuses
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_extender_measurement_probe RENAME TO network_extender_measurement_probe_gone`))
		})
		Testing_ResetLagServing()
		result, err = LagExtenders(guest)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_extender_measurement_probe_gone RENAME TO network_extender_measurement_probe`))
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, lagTestSampleIds(result), lagTestSortedIds(stalled, healthy))
	})
}

// A probes read that ends because the request loading the shared set went away
// raises again, whether it raised the cancellation or another error once the
// request's context had ended, and caches nothing; the next request loads the
// set afresh.
func TestLagServingIsNotCachedWhenTheProbeReadIsCancelled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		defer Testing_SetLagSettings(DefaultLagSettings())()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		now := server.NowUtc()

		stalled := newLagServingTestExtender(t, ctx, places, lagTestPlace("Saginaw", "us", 43.4, -83.9), "192.0.2.61", "2001:db8:61::1")
		addLagTestFailedProbes(ctx, stalled, now, "192.0.2.61", "2001:db8:61::1")
		healthy := newLagServingTestExtender(t, ctx, places, lagTestPlace("Paris", "fr", 48.9, 2.4), "192.0.2.62")

		previous := lagReadMeasurementProbes
		t.Cleanup(func() {
			lagReadMeasurementProbes = previous
		})
		cached := func() *lagServingSet {
			lagServingCache.Lock()
			defer lagServingCache.Unlock()
			return lagServingCache.set
		}
		load := func(loadCtx context.Context) (set *lagServingSet, raised any) {
			defer func() {
				raised = recover()
			}()
			return getLagServing(loadCtx, now, time.Minute), nil
		}

		for _, raise := range []func(readCtx context.Context){
			// the read raises the cancellation
			func(readCtx context.Context) {
				server.Raise(readCtx.Err())
			},
			// or another error, once the context has ended
			func(readCtx context.Context) {
				server.Raise(errors.New("conn closed"))
			},
		} {
			loadCtx, cancel := context.WithCancel(ctx)
			lagReadMeasurementProbes = func(readCtx context.Context, extenderIds []server.Id) map[server.Id][]*model.NetworkExtenderMeasurementProbe {
				// the request goes away during the read
				cancel()
				raise(readCtx)
				return nil
			}
			Testing_ResetLagServing()
			set, raised := load(loadCtx)
			cancel()
			if raised == nil {
				t.Fatalf("a cancelled read loaded %d extenders, want it raised again", len(set.extenders))
			}
			connect.AssertEqual(t, cached(), (*lagServingSet)(nil))
		}

		// the next request loads the set afresh, with the extender left out
		lagReadMeasurementProbes = previous
		set, raised := load(ctx)
		connect.AssertEqual(t, raised, nil)
		connect.AssertEqual(t, lagTestSetIds(set), lagTestSortedIds(healthy))
		connect.AssertEqual(t, cached() == set, true)

		// a read that fails while its request lives offers the set as it is,
		// and caches it
		failLagTestProbeRead(t)
		Testing_ResetLagServing()
		set, raised = load(ctx)
		connect.AssertEqual(t, raised, nil)
		connect.AssertEqual(t, lagTestSetIds(set), lagTestSortedIds(stalled, healthy))
		connect.AssertEqual(t, cached() == set, true)
	})
}

// With no extender serving, the rule reads nothing and answers.
func TestLagReachableServingOfNoExtenders(t *testing.T) {
	failLagTestProbeRead(t)
	set := &lagServingSet{extenders: []*lagServingExtender{}, byId: map[server.Id]*lagServingExtender{}}
	reachable, ok := lagReachableServing(context.Background(), set, time.Now())
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, reachable == set, true)
}

// The unreachable gauge counts the serving extenders in no restart loop that
// the rule leaves out, and the offered gauge those left. An extender both
// restarting and unreachable counts as restarting. When the probes cannot be
// read, every extender in no restart loop is offered and counted so, and the
// unreachable gauge goes stale rather than read 0.
func TestStatsRefreshLagCountsUnreachableExtenders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		defer Testing_SetLagSettings(DefaultLagSettings())()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		now := server.NowUtc()

		// with nothing left out
		healthy := newLagServingTestExtender(t, ctx, places, lagTestPlace("Paris", "fr", 48.9, 2.4), "192.0.2.72")
		addLagTestProbe(ctx, healthy, "192.0.2.72", lagprobe.OutcomeOk, now.Add(-time.Minute))
		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersOfferedGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersRestartingGauge.gauge), float64(0))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersUnreachableGauge.gauge), float64(0))

		stalled := newLagServingTestExtender(t, ctx, places, lagTestPlace("Saginaw", "us", 43.4, -83.9), "192.0.2.71", "2001:db8:71::1")
		addLagTestFailedProbes(ctx, stalled, now, "192.0.2.71", "2001:db8:71::1")
		stalledAgain := newLagServingTestExtender(t, ctx, places, lagTestPlace("Riga", "lv", 56.9, 24.1), "192.0.2.73")
		addLagTestFailedProbes(ctx, stalledAgain, now, "192.0.2.73")
		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersOfferedGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersRestartingGauge.gauge), float64(0))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersUnreachableGauge.gauge), float64(2))

		// in a restart loop as well
		addLagTestPasses(ctx, stalledAgain, now, []int{4, 6}, 110*time.Minute, 80*time.Minute, 50*time.Minute, 20*time.Minute)
		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersOfferedGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersRestartingGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersUnreachableGauge.gauge), float64(1))

		restore := failLagTestProbeRead(t)
		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersOfferedGauge.gauge), float64(2))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersRestartingGauge.gauge), float64(1))
		connect.AssertEqual(t, math.IsNaN(testutil.ToFloat64(statsLagExtendersUnreachableGauge.gauge)), true)

		// and it reads again once they can be read
		restore()
		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersUnreachableGauge.gauge), float64(1))
	})
}

// The rules steer a visitor from a bad dot to a good one. If together they would
// leave none, every extender that serves is offered. Two extenders is enough to
// show it, and is a deployment where it happens: both stalling, or one stalling and
// one restarting, would otherwise empty lag.dev. With a good one back, the rules
// apply again.
func TestLagExtendersOfferThemAllWhenTheRulesWouldLeaveNone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		defer Testing_SetLagSettings(DefaultLagSettings())()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		now := server.NowUtc()
		guest := newLagTestGuestSession(t, ctx, "198.51.100.30:1234")
		offered := func() []server.Id {
			Testing_ResetLagServing()
			result, err := LagExtenders(guest)
			connect.AssertEqual(t, err, nil)
			return lagTestSampleIds(result)
		}

		// no extender serves: nothing to offer, and no floor to apply
		connect.AssertEqual(t, offered(), []server.Id{})
		connect.AssertEqual(t, loadLagOffer(ctx, now).floored, false)

		a := newLagServingTestExtender(t, ctx, places, lagTestPlace("Saginaw", "us", 43.4, -83.9), "192.0.2.21", "2001:db8:21::1")
		b := newLagServingTestExtender(t, ctx, places, lagTestPlace("Paris", "fr", 48.9, 2.4), "192.0.2.22")
		both := lagTestSortedIds(a, b)
		connect.AssertEqual(t, offered(), both)
		connect.AssertEqual(t, loadLagOffer(ctx, now).floored, false)

		// one down: the rule applies
		addLagTestFailedProbes(ctx, b, now, "192.0.2.22")
		connect.AssertEqual(t, offered(), lagTestSortedIds(a))
		connect.AssertEqual(t, loadLagOffer(ctx, now).floored, false)
		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersOfferedGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersUnreachableGauge.gauge), float64(1))

		// both down: both offered, the tickets' set too, and the gauges say what the
		// rule found
		addLagTestFailedProbes(ctx, a, now, "192.0.2.21", "2001:db8:21::1")
		connect.AssertEqual(t, offered(), both)
		set := getLagServing(ctx, server.NowUtc(), time.Minute)
		connect.AssertEqual(t, set.get(a.extenderId) != nil, true)
		connect.AssertEqual(t, set.get(b.extenderId) != nil, true)
		connect.AssertEqual(t, loadLagOffer(ctx, now).floored, true)
		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersOfferedGauge.gauge), float64(2))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersRestartingGauge.gauge), float64(0))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersUnreachableGauge.gauge), float64(2))

		// one back on one family: the other is left out again
		addLagTestProbe(ctx, a, "2001:db8:21::1", lagprobe.OutcomeOk, now)
		connect.AssertEqual(t, offered(), lagTestSortedIds(a))
		connect.AssertEqual(t, loadLagOffer(ctx, now).floored, false)

		// and the two rules together: one restarting, the other unreachable
		addLagTestPasses(ctx, a, now, []int{4, 6}, 110*time.Minute, 80*time.Minute, 50*time.Minute, 20*time.Minute)
		connect.AssertEqual(t, offered(), both)
		connect.AssertEqual(t, loadLagOffer(ctx, now).floored, true)
		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersOfferedGauge.gauge), float64(2))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersRestartingGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersUnreachableGauge.gauge), float64(1))
	})
}

// A tripped guard and a stopped chain both leave the unreachable gauge at 0, so the
// probes have gauges of their own. A trip reads as unprobed rows. A stopped chain
// reads as no recent rows, every outcome at 0, and an age past what the sample
// reads. With no probe at all the age has no value, and an outcome this server does
// not know counts as other.
func TestStatsRefreshLagExportsTheProbesHealth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		defer Testing_SetLagSettings(DefaultLagSettings())()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		now := server.NowUtc().Truncate(time.Microsecond)
		outcome := func(name lagprobe.Outcome) float64 {
			return testutil.ToFloat64(statsLagExtenderProbesGauge.gauge.WithLabelValues(string(name)))
		}

		a := newLagServingTestExtender(t, ctx, places, lagTestPlace("Saginaw", "us", 43.4, -83.9), "192.0.2.11", "2001:db8:11::1")
		b := newLagServingTestExtender(t, ctx, places, lagTestPlace("Paris", "fr", 48.9, 2.4), "192.0.2.12")
		c := newLagServingTestExtender(t, ctx, places, lagTestPlace("Riga", "lv", 56.9, 24.1), "192.0.2.13")

		// a pass a minute ago: one of each, and IPv6 held to be the prober's fault
		addLagTestProbe(ctx, a, "192.0.2.11", lagprobe.OutcomeOk, now.Add(-time.Minute))
		addLagTestProbe(ctx, a, "2001:db8:11::1", lagprobe.OutcomeUnprobed, now.Add(-time.Minute))
		addLagTestProbe(ctx, b, "192.0.2.12", lagprobe.OutcomeTlsTimeout, now.Add(-time.Minute))
		addLagTestProbe(ctx, c, "192.0.2.13", lagprobe.Outcome("from_newer_build"), now.Add(-time.Minute))
		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, outcome(lagprobe.OutcomeOk), float64(1))
		connect.AssertEqual(t, outcome(lagprobe.OutcomeUnprobed), float64(1))
		connect.AssertEqual(t, outcome(lagprobe.OutcomeTlsTimeout), float64(1))
		connect.AssertEqual(t, outcome(lagprobe.OutcomeDial), float64(0))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtenderProbesGauge.gauge.WithLabelValues("other")), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtenderProbeAgeGauge.gauge), float64(60))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersUnreachableGauge.gauge), float64(0))

		// the chain stops: the unreachable gauge still reads 0, and these do not
		statsRefreshLag(ctx, now.Add(20*time.Minute))
		for _, name := range lagprobe.Outcomes {
			connect.AssertEqual(t, outcome(name), float64(0))
		}
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtenderProbeAgeGauge.gauge), (21 * time.Minute).Seconds())
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersUnreachableGauge.gauge), float64(0))

		// no probe at all, which a number would misstate
		model.RemoveNetworkExtenderMeasurementProbesBefore(ctx, now.Add(time.Hour))
		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, math.IsNaN(testutil.ToFloat64(statsLagExtenderProbeAgeGauge.gauge)), true)
		connect.AssertEqual(t, outcome(lagprobe.OutcomeOk), float64(0))
	})
}
