package controller

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The lag.dev sample (lag.dev/PLAN.md A4) leaves out an extender that is in a
// restart loop, and the tickets (A6) leave it out with it, since both read the
// same serving set: three activation passes that each activated both families,
// within 90 minutes, the last less than two hours ago
// (model.GetLagExtenderRestartLoops). An extender that stalls answers tcp but
// not tls until it restarts, so a ticket issued in the minutes before a
// restart gets no result far more often than another.
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
//
// An extender's uptime probes do not decide: a probe forwards through the
// extender to the api host, which a measurement does not use, and on Main
// most of the minutes a failed latest probe would have left an extender out,
// its measurement api answered. An outage is left to the probes' revocation,
// as before, and the page's spares.
var lagExtenderRestartLoopRule = model.LagExtenderRestartLoopRule{
	PassGap:          30 * time.Second,
	BurstPasses:      3,
	BurstWindow:      90 * time.Minute,
	BurstHold:        2 * time.Hour,
	RevocationWindow: 30 * time.Minute,
}

// Reads which extenders are in a restart loop. A variable so a test can make
// the read fail.
var lagReadRestartLoops = model.GetLagExtenderRestartLoops

// The extenders of a serving set that are not in a restart loop at `now`, in
// one read. The read runs with the set's load, so an extender whose loop
// starts is left out within extender_sample_cache. If the loops cannot be
// read, the set is offered as it is, as it was before there was a rule.
func lagStableServing(ctx context.Context, set *lagServingSet, now time.Time) *lagServingSet {
	if len(set.extenders) == 0 {
		return set
	}
	extenderIds := make([]server.Id, 0, len(set.extenders))
	for _, extender := range set.extenders {
		extenderIds = append(extenderIds, extender.ExtenderId)
	}
	var loops map[server.Id]time.Time
	if recovered := server.HandleError(func() {
		loops = lagReadRestartLoops(ctx, extenderIds, now, &lagExtenderRestartLoopRule)
	}); recovered != nil {
		glog.Errorf("[lag]the extenders' restart loops are not readable, offering every serving extender: %v\n", recovered)
		return set
	}

	stable := &lagServingSet{
		loadTime:  set.loadTime,
		extenders: []*lagServingExtender{},
		byId:      map[server.Id]*lagServingExtender{},
	}
	for _, extender := range set.extenders {
		if burstPassTime, ok := loops[extender.ExtenderId]; ok {
			if glog.V(1) {
				glog.Infof(
					"[lag]extender %s is in a restart loop and not offered: its latest burst ended %s\n",
					extender.ExtenderId,
					burstPassTime.Format(time.RFC3339),
				)
			}
			continue
		}
		stable.extenders = append(stable.extenders, extender)
		stable.byId[extender.ExtenderId] = extender
	}
	return stable
}
