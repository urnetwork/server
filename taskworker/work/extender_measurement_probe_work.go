package work

import (
	"context"
	mathrand "math/rand/v2"
	"net/netip"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/lagprobe"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The lag.dev measurement api probes (lag.dev/PLAN.md A4).
//
// Every two minutes each published address of each extender the lag.dev sample
// serves is probed as a page would use it (lagprobe), and the latest probe of
// each extender and family is stored. The sample reads them to leave out an
// extender that can't be reached (controller/lag_extender_reach.go). An
// extender the sample leaves out is still probed, whether for this or for a
// restart loop, so it comes back with its first success.
//
// Two minutes is the shortest of the two to five the plan allows. A pass is
// about 60 to 100 handshakes among 30 to 60 extenders, a few seconds of network
// wait, and the extender answers with its cheapest refusal, before admission.
// The rule takes two failures in a row, so the cadence is what decides how soon
// an extender that stalls for 2 to 15 minutes leaves the sample, 2 to 4 minutes
// in, and how soon it returns.
//
// The taskworker hosts reach IPv6. Main's uptime probes run in the same
// processes and dial every address directly, about 30% of them IPv6. None of
// the 259 passes of the 24 hours to 2026-11-02 09:46Z failed more than 22% of
// its addresses, so no process that ran one lacked IPv6, and all 65 active IPv6
// addresses have passed. A host that can't reach a family is told apart anyway.
// An address it has no route to, and every failure of a family most of whose
// probes failed, is recorded unprobed, which the sample doesn't count against an
// extender (lagprobe.ProbeAll). Failed, those would take every extender of the
// family out of the sample.
//
// A failed probe is this task's data, not its error, as for the uptime probes:
// returning one would skip the Post and put the chain on a backoff, and stale
// probes offer every extender again. The task's limit stays the default two
// minutes, which lagprobe's budget and timeouts fit in, so it needs no worker
// of its own. Like the other measurement chains, it does nothing while the
// measurement block is disabled.

const (
	// How often the probes run.
	ExtenderMeasurementProbeTimeout = 2 * time.Minute

	// How long the row of a family no longer probed is kept: the week an
	// extender that went away may come back in with its certificate
	// (controller.ExtenderMeasurementAwayGrace). Its latest success and outcome
	// are all there is of it to read, and the table holds at most two rows per
	// extender that served in that week.
	ExtenderMeasurementProbeRetention = controller.ExtenderMeasurementAwayGrace
)

// Makes one pass of probes. A variable so a test can answer as it needs,
// without an extender behind every address. Production never replaces it; the
// probe itself is proved against in-process servers in package lagprobe.
var probeExtenderMeasurementApis = lagprobe.ProbeAll

type ExtenderMeasurementProbeArgs struct {
}

type ExtenderMeasurementProbeResult struct {
	// counts only, as for the uptime probes; which extender failed is in the
	// table and the log
	Probed   int `json:"probed"`
	Ok       int `json:"ok"`
	Failed   int `json:"failed"`
	Unprobed int `json:"unprobed"`
	// addresses the pass's budget or its end left unprobed, with no outcome
	Skipped int `json:"skipped"`
	Removed int `json:"removed"`
}

func ScheduleExtenderMeasurementProbe(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		ExtenderMeasurementProbe,
		&ExtenderMeasurementProbeArgs{},
		clientSession,
		task.RunOnce("extender_measurement_probe"),
		task.RunAt(server.NowUtc().Add(ExtenderMeasurementProbeTimeout)),
	)
}

// ExtenderMeasurementProbe is one pass of the probe chain (A4).
func ExtenderMeasurementProbe(
	_ *ExtenderMeasurementProbeArgs,
	clientSession *session.ClientSession,
) (*ExtenderMeasurementProbeResult, error) {
	result := &ExtenderMeasurementProbeResult{}
	config, err := controller.EnvExtenderConfig()
	if err != nil || !config.Measurement.Enabled {
		return result, nil
	}
	probeExtenderMeasurementHosts(clientSession.Ctx, server.NowUtc(), result)
	return result, nil
}

func ExtenderMeasurementProbePost(
	_ *ExtenderMeasurementProbeArgs,
	_ *ExtenderMeasurementProbeResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleExtenderMeasurementProbe(clientSession, tx)
	return nil
}

// One pass as the clock reads `now`, into `result`: every published address of
// every serving extender, stored as of `now`.
func probeExtenderMeasurementHosts(ctx context.Context, now time.Time, result *ExtenderMeasurementProbeResult) {
	extenderIds := []server.Id{}
	targets := []lagprobe.Target{}
	for _, host := range controller.GetLagServingHosts(ctx, now) {
		for _, ip := range host.Ips {
			extenderIds = append(extenderIds, host.ExtenderId)
			targets = append(targets, lagprobe.Target{
				Name: host.Name,
				Addr: netip.AddrPortFrom(ip, controller.ExtenderMeasurementPort),
			})
		}
	}
	// a pass whose budget runs out leaves a different part of the list to the
	// next one
	mathrand.Shuffle(len(targets), func(i, j int) {
		extenderIds[i], extenderIds[j] = extenderIds[j], extenderIds[i]
		targets[i], targets[j] = targets[j], targets[i]
	})

	pass := probeExtenderMeasurementApis(ctx, targets, lagprobe.DefaultSettings())

	records := make([]*model.NetworkExtenderMeasurementProbeResult, 0, len(targets))
	for i, probe := range pass.Results {
		if probe == nil {
			result.Skipped += 1
			continue
		}
		result.Probed += 1
		switch {
		case probe.Outcome == lagprobe.OutcomeOk:
			result.Ok += 1
		case probe.Outcome == lagprobe.OutcomeUnprobed:
			result.Unprobed += 1
		default:
			result.Failed += 1
		}
		if probe.Outcome != lagprobe.OutcomeOk && glog.V(1) {
			glog.Infof(
				"[extendermeasurement]extender %s ipv%d probe %s: %v\n",
				extenderIds[i],
				targets[i].IpVersion(),
				probe.Outcome,
				probe.Err,
			)
		}
		records = append(records, &model.NetworkExtenderMeasurementProbeResult{
			ExtenderId: extenderIds[i],
			Ip:         targets[i].Addr.Addr(),
			Outcome:    probe.Outcome,
		})
	}
	model.RecordNetworkExtenderMeasurementProbes(ctx, records, now, controller.ExtenderMeasurementProbeMaxAge)
	result.Removed = int(model.RemoveNetworkExtenderMeasurementProbesBefore(ctx, now.Add(-ExtenderMeasurementProbeRetention)))

	for _, ipVersion := range pass.UnreachableIpVersions {
		// nothing else says so: the extenders are offered as if nothing had
		// been probed
		glog.Errorf(
			"[extendermeasurement]most ipv%d measurement api probes failed, so this host is taken not to reach ipv%d and they are recorded unprobed\n",
			ipVersion,
			ipVersion,
		)
	}
	if 0 < result.Failed+result.Unprobed+result.Skipped {
		glog.Infof(
			"[extendermeasurement]probed %d addresses: %d ok, %d failed, %d unprobed; %d not reached\n",
			result.Probed,
			result.Ok,
			result.Failed,
			result.Unprobed,
			result.Skipped,
		)
	}
}
