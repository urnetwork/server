package controller

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The lag.dev sample (lag.dev/PLAN.md A4) leaves out an extender that is in a
// restart loop, and the tickets (A6) leave it out with it, since both read the
// same serving set: four activation passes that each activated both families,
// within two hours, the last less than two hours ago
// (model.GetLagExtenderRestartLoops). An extender that stalls answers tcp but
// not tls until it restarts, so a ticket issued in the minutes before a
// restart gets no result far more often than another.
//
// The thresholds come from replays of Main's activations of the 48 hours to
// 2026-11-01 22:45 UTC:
//
//   - the rows of one pass are 0.5 to 15 seconds apart, the operator probing
//     back each family in turn, and passes are minutes apart, so a 30 second
//     gap groups them;
//   - connect's timers start full passes as well as restarts do: the hourly
//     address check when the caller address flips between families, and a
//     backoff retry once the refused family is accepted. They start at most
//     one an hour, 0, 10 or 30 minutes past the hour's check, so three can
//     span 90 minutes, and on Main three did, a fraction of a second either
//     side of it; four span at least 150, so four within 120 minutes leave
//     the timers out with a margin;
//   - the probes revoke an extender after about half an hour of failures,
//     and it re-activates once it reads that. Not counting a pass less than
//     30 minutes after a revocation leaves out every such cycle fast enough
//     to make a burst, since a slower one is over an hour long;
//   - with these, an extender in a loop like Saginaw's (01a17c4f) is left out
//     for 81% of the loop, including the minutes before 22 of its 31
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
	BurstPasses:      4,
	BurstWindow:      2 * time.Hour,
	BurstHold:        2 * time.Hour,
	RevocationWindow: 30 * time.Minute,
}

// Reads which extenders are in a restart loop. A variable so a test can make
// the read fail.
var lagReadRestartLoops = model.GetLagExtenderRestartLoops

// The extenders of a serving set that are not in a restart loop at `now`, in
// one read, and whether the read answered. The read runs with the set's load,
// so an extender whose loop starts is left out within extender_sample_cache.
// If the read fails, the set is offered as it is, as it was before there was a
// rule.
func lagStableServing(ctx context.Context, set *lagServingSet, now time.Time) (*lagServingSet, bool) {
	if len(set.extenders) == 0 {
		return set, true
	}
	extenderIds := make([]server.Id, 0, len(set.extenders))
	for _, extender := range set.extenders {
		extenderIds = append(extenderIds, extender.ExtenderId)
	}
	loops, ok := readLagRestartLoops(ctx, extenderIds, now)
	if !ok {
		return set, false
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
	return stable, true
}

// The restart loops of the extenders, or false when they cannot be read,
// logged once. A read that raised because ctx ended raises again: its caller
// went away, the read did not fail, and the set the caller was loading, which
// is shared, must not be cached unfiltered on its account.
func readLagRestartLoops(ctx context.Context, extenderIds []server.Id, now time.Time) (loops map[server.Id]time.Time, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if server.IsDoneError(recovered) || ctx.Err() != nil {
				panic(recovered)
			}
			glog.Errorf(
				"[lag]the extenders' restart loops are not readable, offering every serving extender: %s\n",
				server.ErrorJson(recovered, debug.Stack()),
			)
			loops = nil
			ok = false
		}
	}()
	return lagReadRestartLoops(ctx, extenderIds, now, &lagExtenderRestartLoopRule), true
}
