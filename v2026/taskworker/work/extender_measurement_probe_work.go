package work

import (
	"context"
	mathrand "math/rand/v2"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/lagprobe"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
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
//
// An address that failed is probed once more about 30 seconds later, by a task of
// its own that the pass's Post schedules, and that counts as a probe like any
// other. The rule takes two failures in a row, so an extender that stalls leaves
// the sample about 30 seconds to two and a half minutes in, where two passes
// alone would take 2 to 4 minutes and miss most of the 3 to 8 minute stalls seen
// on Main. Almost every other failure is over by then, so the second failure
// still tells them apart. The wait is not spent in the pass: a task waiting in a
// worker's slot would hold that slot, and hold up the rest of the worker when an
// isolated task is next (task.TaskWorker). A recheck has no guard against this
// host's own network (lagprobe.Recheck): a moment of it that falls on one gives
// what failed once a false second failure, until the next pass.
//
// The taskworker hosts reach IPv6. Main's uptime probes run in the same
// processes and dial every address directly, about 30% of them IPv6. None of
// the 259 passes of the 24 hours to 2026-11-02 09:46Z failed more than 22% of
// its addresses, so no process that ran one lacked IPv6, and all 65 active IPv6
// addresses have passed. A host that can't reach a family is told apart anyway.
// An address it has no route to, and every failure of a family most of whose
// probes failed in a pass, is recorded unprobed, which the sample doesn't count
// against an extender (lagprobe.ProbeAll). Failed, those would take every extender
// of the family out of the sample. A tripped guard therefore leaves
// lag_extenders_unreachable at 0, as a stopped chain does, so each has a signal of
// its own: lag_probe_guard_trips_total, and lag_extender_probes and
// lag_extender_probe_age_seconds (controller/stats_collector_lag.go).
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

	// How long after a pass its failures are probed again.
	ExtenderMeasurementRecheckTimeout = 30 * time.Second

	// How long the row of a family no longer probed is kept: the week an
	// extender that went away may come back in with its certificate
	// (controller.ExtenderMeasurementAwayGrace). Its latest success and outcome
	// are all there is of it to read, and the table holds at most two rows per
	// extender that served in that week.
	ExtenderMeasurementProbeRetention = controller.ExtenderMeasurementAwayGrace
)

// Makes one pass of probes, and one recheck. Variables so a test can answer as it
// needs, without an extender behind every address. Production never replaces
// them; the probe itself is proved against in-process servers in package lagprobe.
var (
	probeExtenderMeasurementApis   = lagprobe.ProbeAll
	recheckExtenderMeasurementApis = lagprobe.Recheck
)

// Passes that took a family for one this host cannot reach (lagprobe.ProbeAll).
// Its extenders are offered as if they had not been probed, so nothing else counts
// them. Bounded by the two families, and read from grafana/dashboards/lag-dev.json.
var extenderMeasurementProbeGuardTripsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "lag",
	Name:      "probe_guard_trips_total",
	Help:      "Passes of the lag.dev measurement api probes in which most of a family failed, at least three addresses, so the prober held it to be its own fault and recorded the failures unprobed, by family",
}, []string{"ip_family"})

func init() {
	prometheus.MustRegister(extenderMeasurementProbeGuardTripsTotal)
	for _, family := range []string{"ipv4", "ipv6"} {
		extenderMeasurementProbeGuardTripsTotal.WithLabelValues(family)
	}
}

// The family label of an ip version, as the lag gauges name it.
func extenderMeasurementProbeFamilyLabel(ipVersion int) string {
	if ipVersion == 6 {
		return "ipv6"
	}
	return "ipv4"
}

type ExtenderMeasurementProbeArgs struct {
}

type ExtenderMeasurementProbeResult struct {
	// the time a pass's probes are stored at, which its recheck finds them by;
	// zero when it probed nothing, and for a recheck
	PassTime time.Time `json:"pass_time"`
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
	// as the column stores it, so the recheck finds the pass by this time
	probeExtenderMeasurementHosts(clientSession.Ctx, server.NowUtc().Truncate(time.Microsecond), result)
	return result, nil
}

// Re-arms the chain, and has what failed probed again in about 30 seconds.
func ExtenderMeasurementProbePost(
	_ *ExtenderMeasurementProbeArgs,
	result *ExtenderMeasurementProbeResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleExtenderMeasurementProbe(clientSession, tx)
	if result != nil && 0 < result.Failed {
		scheduleExtenderMeasurementRecheck(clientSession, tx, result.PassTime)
	}
	return nil
}

// One pass as the clock reads `now`, into `result`: every published address of
// every serving extender, stored as of `now`.
//
// The store comes last. A task that raises runs again, and a pass that had stored
// its failures would count one stutter twice, seconds apart, with no recheck
// between them.
func probeExtenderMeasurementHosts(ctx context.Context, now time.Time, result *ExtenderMeasurementProbeResult) {
	result.PassTime = now
	result.Removed = int(model.RemoveNetworkExtenderMeasurementProbesBefore(ctx, now.Add(-ExtenderMeasurementProbeRetention)))

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
	storeExtenderMeasurementProbes(ctx, now, extenderIds, targets, pass, result)
}

type ExtenderMeasurementRecheckArgs struct {
	// the pass whose failures to probe again
	PassTime time.Time `json:"pass_time"`
}

func scheduleExtenderMeasurementRecheck(clientSession *session.ClientSession, tx server.PgTx, passTime time.Time) {
	task.ScheduleTaskInTx(
		tx,
		ExtenderMeasurementRecheck,
		&ExtenderMeasurementRecheckArgs{PassTime: passTime},
		clientSession,
		task.RunOnce("extender_measurement_recheck", passTime.UnixMicro()),
		task.RunAt(server.NowUtc().Add(ExtenderMeasurementRecheckTimeout)),
	)
}

// ExtenderMeasurementRecheck probes the failures of one pass once more (A4). It
// has no Post: what fails here is probed again by the next pass, so one failure
// asks for one more probe and no more.
func ExtenderMeasurementRecheck(
	args *ExtenderMeasurementRecheckArgs,
	clientSession *session.ClientSession,
) (*ExtenderMeasurementProbeResult, error) {
	result := &ExtenderMeasurementProbeResult{}
	config, err := controller.EnvExtenderConfig()
	if err != nil || !config.Measurement.Enabled {
		return result, nil
	}
	recheckExtenderMeasurementProbes(
		clientSession.Ctx,
		config.Measurement.MeasurementZone(),
		args.PassTime,
		server.NowUtc().Truncate(time.Microsecond),
		result,
	)
	return result, nil
}

// Probes again what is still a failure of the pass at passTime, as the clock
// reads `now`, into `result`. A row a later pass has replaced is not the pass's,
// so a recheck that runs too late finds nothing to do, rather than probing a new
// failure seconds after it. The addresses are the ones probed, read from their
// rows: an extender that stops serving or moves within these seconds costs one more
// probe, and its row is of an address the sample no longer reads.
//
// There is no family rule here, and it could not fire if there were: a pass that
// did not trip it leaves at most half of a family to probe again, or fewer than
// three (lagprobe.Recheck). So what fails here stands as a failure, whatever its
// cause.
func recheckExtenderMeasurementProbes(
	ctx context.Context,
	zone string,
	passTime time.Time,
	now time.Time,
	result *ExtenderMeasurementProbeResult,
) {
	extenderIds := []server.Id{}
	targets := []lagprobe.Target{}
	for _, probe := range model.GetNetworkExtenderMeasurementProbesAt(ctx, passTime) {
		if !probe.Outcome.Failed() {
			continue
		}
		extenderIds = append(extenderIds, probe.ExtenderId)
		targets = append(targets, lagprobe.Target{
			Name: controller.ExtenderMeasurementName(probe.ExtenderId, zone),
			Addr: netip.AddrPortFrom(probe.Ip, controller.ExtenderMeasurementPort),
		})
	}
	if len(targets) == 0 {
		return
	}
	pass := recheckExtenderMeasurementApis(ctx, targets, lagprobe.DefaultSettings())
	storeExtenderMeasurementProbes(ctx, now, extenderIds, targets, pass, result)
}

// Stores what one round of probes found, as of `now`, and counts and logs it.
// extenderIds[i] is the extender of targets[i].
func storeExtenderMeasurementProbes(
	ctx context.Context,
	now time.Time,
	extenderIds []server.Id,
	targets []lagprobe.Target,
	pass *lagprobe.Pass,
	result *ExtenderMeasurementProbeResult,
) {
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

	for _, ipVersion := range pass.UnreachableIpVersions {
		extenderMeasurementProbeGuardTripsTotal.WithLabelValues(extenderMeasurementProbeFamilyLabel(ipVersion)).Inc()
		// the extenders are offered as if nothing had been probed
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
