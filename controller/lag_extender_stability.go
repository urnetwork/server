package controller

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The lag.dev sample (lag.dev/PLAN.md A4) leaves out an extender that is
// unstable now, and the tickets (A6) leave it out with it, since both read the
// same serving set. An extender is unstable when the latest uptime probe of
// one of its active addresses failed, or while it is in a restart loop: three
// activation passes that each activated both families, within 90 minutes, the
// last less than two hours ago (model.GetLagExtenderStability). An extender
// that stalls answers tcp but not tls until it restarts, so a ticket issued in
// the minutes before a restart gets no result far more often than another.
//
// The thresholds come from a replay of Main's activations of the 48 hours to
// 2026-11-01 22:45 UTC:
//
//   - the rows of one pass are 0.5 to 15 seconds apart, the operator probing
//     back each family in turn, and passes are minutes apart, so a 30 second
//     gap groups them;
//   - connect's hourly address check starts at most one full pass an hour, so
//     no 90 minute window holds three of those, while 105 and 120 minute
//     windows caught healthy extenders whose full passes were mostly those;
//   - the probes revoke an extender after about half an hour of failures,
//     and it re-activates once it reads that. Not counting a pass less than
//     30 minutes after a revocation leaves out every such cycle fast enough
//     to make a burst, since a slower one is over an hour long;
//   - with these, an extender in a loop like Saginaw's (01a17c4f) is left out
//     for 91% of the loop, including the minutes before 25 of its 31
//     restarts, and no offered extender out of a loop was left out at all.
//
// The hold bridges the gaps between a loop's restarts, which went up to two
// hours, and ends the exclusion two hours after a loop's last burst.
var lagExtenderStabilityRule = model.LagExtenderStabilityRule{
	PassGap:          30 * time.Second,
	BurstPasses:      3,
	BurstWindow:      90 * time.Minute,
	BurstHold:        2 * time.Hour,
	RevocationWindow: 30 * time.Minute,
}

// Reads the stability evidence of extenders. A variable so a test can make
// the read fail.
var lagReadExtenderStability = model.GetLagExtenderStability

// The extenders of a serving set that are stable at `now`, in one read. The
// read runs with the set's load, so an extender that becomes unstable is left
// out within extender_sample_cache. If the stability cannot be read, the set
// is offered as it is, as it was before there was a stability rule.
func lagStableServing(ctx context.Context, set *lagServingSet, now time.Time) *lagServingSet {
	if len(set.extenders) == 0 {
		return set
	}
	extenderIds := make([]server.Id, 0, len(set.extenders))
	for _, extender := range set.extenders {
		extenderIds = append(extenderIds, extender.ExtenderId)
	}
	var stabilities map[server.Id]*model.LagExtenderStability
	if recovered := server.HandleError(func() {
		stabilities = lagReadExtenderStability(ctx, extenderIds, now, &lagExtenderStabilityRule)
	}); recovered != nil {
		glog.Errorf("[lag]the extenders' stability is not readable, offering every serving extender: %v\n", recovered)
		return set
	}

	stable := &lagServingSet{
		loadTime:  set.loadTime,
		extenders: []*lagServingExtender{},
		byId:      map[server.Id]*lagServingExtender{},
	}
	for _, extender := range set.extenders {
		if stability := stabilities[extender.ExtenderId]; stability != nil && stability.Unstable() {
			if glog.V(1) {
				glog.Infof(
					"[lag]extender %s is unstable and not offered: probe failed=%t, restart burst=%v\n",
					extender.ExtenderId,
					stability.ProbeFailed,
					stability.BurstPassTime,
				)
			}
			continue
		}
		stable.extenders = append(stable.extenders, extender)
		stable.byId[extender.ExtenderId] = extender
	}
	return stable
}
