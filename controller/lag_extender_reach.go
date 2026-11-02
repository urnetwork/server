package controller

import (
	"context"
	"net/netip"
	"runtime/debug"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/lagprobe"
	"github.com/urnetwork/server/model"
)

// The lag.dev sample (lag.dev/PLAN.md A4) leaves out an extender whose
// measurement api the probes cannot reach, and the tickets (A6) leave it out
// with it, since both read the same serving set. The probes
// (taskworker/work/extender_measurement_probe_work.go, lagprobe) make what a
// page makes first, at each address the extender's name publishes, so they
// test what a measurement uses and nothing else, which the uptime probes do
// not: those forward through the extender, and failed for extenders whose
// measurement api answered.
//
// An extender is left out while, on every family it publishes and the probes
// could reach, its latest probes failed lagExtenderUnreachableRule's number of
// times in a row. One success on one family brings it back at the next load of
// the set. This combines with the restart loop rule (lag_extender_stability.go)
// and does not replace it: an extender that is down shows here within minutes,
// and the loop rule holds out one that keeps stalling and restarting, between
// and ahead of stalls too short for two probes to see.
//
// Everything the probes do not know offers the extender, as before the rule:
//
//   - a family with no probe, whose row is of another address, or whose latest
//     probe is ExtenderMeasurementProbeMaxAge old: a new name, a moved address,
//     or the probe chain stopped. So if the chain stops, every extender is
//     offered again within that long;
//   - a table that cannot be read (readLagMeasurementProbes);
//   - an extender with no family the probes could reach. A family recorded
//     unprobed counts for neither side, so an extender whose other family
//     failed is still left out, where the taskworker cannot probe IPv6: its
//     name's IPv4 failing is what a stall on both families looks like from
//     there.

const (
	// How old a probe may be and still count. The chain runs every two
	// minutes (ExtenderMeasurementProbeTimeout), so this is seven passes in a
	// row missed, longer than a deployment leaves it stopped.
	ExtenderMeasurementProbeMaxAge = 15 * time.Minute
)

// What leaves an extender out.
type lagExtenderReachRule struct {
	// the failures in a row on each probed family that leave an extender out
	ConsecutiveFailures int
	// a probe this old or older counts as none
	ProbeMaxAge time.Duration
}

// Two in a row, not one. A probe that fails once is too often not the
// extender: one handshake that stuttered, or the prober's own network for a
// moment, which takes many at once. Polling the 30 offered extenders every 30 s
// from a laptop for 2.4 hours on 2026-11-02, the only extenders down for longer
// than one poll were two of Saginaw's stalls. Of the 39 down for one poll, 35
// came in two moments when 16 and 32 of the 37 addresses failed together.
//
// The second probe costs little, since it comes about 30 s after the first
// rather than at the next pass (work.ExtenderMeasurementRecheck). Replayed through
// this rule, in those 2.4 hours it leaves out extenders that answer for under one
// extender-minute, where one failure does for 7. It leaves out on average 50% of
// a 3 minute stall and 81% of an 8 minute one, where one failure leaves out 67%
// and 87%, and two failures a whole pass apart 8% and 61%. On a poll of
// 2026-11-01, with extenders down throughout and two of Saginaw's stalls, it left
// out 93% of the time they were down, one failure 97%, and two a pass apart 84%.
var lagExtenderUnreachableRule = lagExtenderReachRule{
	ConsecutiveFailures: 2,
	ProbeMaxAge:         ExtenderMeasurementProbeMaxAge,
}

// Reads the extenders' probes. A variable so a test can make the read fail.
var lagReadMeasurementProbes = model.GetNetworkExtenderMeasurementProbes

// Whether the rule leaves out an extender publishing these addresses, given
// the latest probe of each of its families, at `now`.
func lagExtenderUnreachable(
	ips []netip.Addr,
	probes []*model.NetworkExtenderMeasurementProbe,
	now time.Time,
	rule *lagExtenderReachRule,
) bool {
	probed := false
	for _, ip := range ips {
		ip = ip.Unmap().WithZone("")
		var probe *model.NetworkExtenderMeasurementProbe
		for _, candidate := range probes {
			if candidate.IpVersion == server.IpVersionForAddr(ip) {
				probe = candidate
				break
			}
		}
		if probe == nil || probe.Ip != ip || rule.ProbeMaxAge <= now.Sub(probe.ProbeTime) {
			// nothing is known of this family as it is now
			return false
		}
		if probe.Outcome == lagprobe.OutcomeUnprobed {
			continue
		}
		if probe.ConsecutiveFailures < rule.ConsecutiveFailures {
			return false
		}
		probed = true
	}
	return probed
}

// The extenders of a serving set the probes can reach at `now`, in one read,
// and whether the read answered. The read runs with the set's load, so an
// extender is left out, and comes back, within extender_sample_cache of the
// probe that decides it. If the read fails, the set is offered as it is.
func lagReachableServing(ctx context.Context, set *lagServingSet, now time.Time) (*lagServingSet, bool) {
	if len(set.extenders) == 0 {
		return set, true
	}
	extenderIds := make([]server.Id, 0, len(set.extenders))
	for _, extender := range set.extenders {
		extenderIds = append(extenderIds, extender.ExtenderId)
	}
	probes, ok := readLagMeasurementProbes(ctx, extenderIds)
	if !ok {
		return set, false
	}

	reachable := &lagServingSet{
		loadTime:  set.loadTime,
		extenders: []*lagServingExtender{},
		byId:      map[server.Id]*lagServingExtender{},
	}
	for _, extender := range set.extenders {
		if lagExtenderUnreachable(extender.Ips, probes[extender.ExtenderId], now, &lagExtenderUnreachableRule) {
			if glog.V(1) {
				glog.Infof("[lag]extender %s's measurement api is not reachable and it is not offered\n", extender.ExtenderId)
			}
			continue
		}
		reachable.extenders = append(reachable.extenders, extender)
		reachable.byId[extender.ExtenderId] = extender
	}
	return reachable, true
}

// The latest probes of the extenders, or false when they cannot be read,
// logged once. A read that raised because ctx ended raises again: its caller
// went away, the read did not fail, and the set the caller was loading, which
// is shared, must not be cached unfiltered on its account.
func readLagMeasurementProbes(ctx context.Context, extenderIds []server.Id) (probes map[server.Id][]*model.NetworkExtenderMeasurementProbe, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if server.IsDoneError(recovered) || ctx.Err() != nil {
				panic(recovered)
			}
			glog.Errorf(
				"[lag]the extenders' measurement api probes are not readable, offering every serving extender: %s\n",
				server.ErrorJson(recovered, debug.Stack()),
			)
			probes = nil
			ok = false
		}
	}()
	return lagReadMeasurementProbes(ctx, extenderIds), true
}
