package model

import (
	"context"
	"net/netip"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/lagprobe"
)

// lag.dev's measurement api probes (lag.dev/PLAN.md A4). The taskworker's
// probe chain (taskworker/work/extender_measurement_probe_work.go) probes each
// published address of each extender the lag.dev sample serves, and writes
// here the latest probe of each extender and family. The sample reads them to
// leave out an extender its probes cannot reach
// (controller/lag_extender_reach.go).
//
// A row keeps the address it probed, so a family that moved to a new address
// starts over at its next probe, and until then the row is of an address the
// extender no longer has, which the sample reads as no probe at all. Activation
// does not touch these rows: the probes are the only writer.

// The latest probe of one family of one extender.
type NetworkExtenderMeasurementProbe struct {
	ExtenderId server.Id
	IpVersion  int
	// the address probed
	Ip        netip.Addr
	ProbeTime time.Time
	Outcome   lagprobe.Outcome
	// the latest probe of this address that succeeded, nil while none has
	SuccessTime *time.Time
	// this address's failures in a row, up to ProbeTime
	ConsecutiveFailures int
}

// One probe to record.
type NetworkExtenderMeasurementProbeResult struct {
	ExtenderId server.Id
	Ip         netip.Addr
	Outcome    lagprobe.Outcome
}

// RecordNetworkExtenderMeasurementProbes stores one pass's probes, each as its
// family's latest, at probeTime, in one statement:
//
//   - a success sets the success time and clears the failures;
//   - a failure adds one to the failures, or starts them at one when the row
//     is of another address or its probe is older than runGap, since probes
//     that far apart are not one after another;
//   - unprobed, a family the prober could not reach, says nothing about the
//     extender. It clears the failures, so a run counts only probes made one
//     after another, and keeps the success time.
//
// A row of another address keeps none of that address's history. Of two results
// for one family in a pass, the last is kept.
func RecordNetworkExtenderMeasurementProbes(
	ctx context.Context,
	results []*NetworkExtenderMeasurementProbeResult,
	probeTime time.Time,
	runGap time.Duration,
) {
	type familyKey struct {
		extenderId server.Id
		ipVersion  int
	}
	latest := map[familyKey]*NetworkExtenderMeasurementProbeResult{}
	for _, result := range results {
		latest[familyKey{extenderId: result.ExtenderId, ipVersion: server.IpVersionForAddr(result.Ip)}] = result
	}
	if len(latest) == 0 {
		return
	}
	extenderIds := make([]server.Id, 0, len(latest))
	ipVersions := make([]int16, 0, len(latest))
	ips := make([]string, 0, len(latest))
	outcomes := make([]string, 0, len(latest))
	for key, result := range latest {
		extenderIds = append(extenderIds, key.extenderId)
		ipVersions = append(ipVersions, int16(key.ipVersion))
		ips = append(ips, result.Ip.Unmap().WithZone("").String())
		outcomes = append(outcomes, string(result.Outcome))
	}

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_extender_measurement_probe AS probe (
				extender_id,
				ip_version,
				ip,
				probe_time,
				outcome,
				success_time,
				consecutive_failures
			)
			SELECT
				result.extender_id,
				result.ip_version,
				result.ip::inet,
				$5::timestamp,
				result.outcome,
				CASE WHEN result.outcome = $6::varchar THEN $5::timestamp END,
				CASE WHEN result.outcome IN ($6::varchar, $7::varchar) THEN 0 ELSE 1 END
			FROM unnest(
				$1::uuid[],
				$2::smallint[],
				$3::text[],
				$4::varchar[]
			) AS result(extender_id, ip_version, ip, outcome)
			ON CONFLICT (extender_id, ip_version) DO UPDATE
			SET
				ip = excluded.ip,
				probe_time = excluded.probe_time,
				outcome = excluded.outcome,
				success_time = CASE
					WHEN excluded.outcome = $6::varchar THEN excluded.probe_time
					WHEN probe.ip = excluded.ip THEN probe.success_time
				END,
				consecutive_failures = CASE
					WHEN excluded.outcome IN ($6::varchar, $7::varchar) THEN 0
					WHEN probe.ip = excluded.ip AND $8::timestamp <= probe.probe_time THEN probe.consecutive_failures + 1
					ELSE 1
				END
			`,
			extenderIds,
			ipVersions,
			ips,
			outcomes,
			probeTime.UTC(),
			string(lagprobe.OutcomeOk),
			string(lagprobe.OutcomeUnprobed),
			probeTime.Add(-runGap).UTC(),
		))
	})
}

// GetNetworkExtenderMeasurementProbes reads the latest probe of each family of
// each of the extenders, in one query. An extender with no probe is absent.
func GetNetworkExtenderMeasurementProbes(
	ctx context.Context,
	extenderIds []server.Id,
) map[server.Id][]*NetworkExtenderMeasurementProbe {
	probes := map[server.Id][]*NetworkExtenderMeasurementProbe{}
	if len(extenderIds) == 0 {
		return probes
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				extender_id,
				ip_version,
				ip,
				probe_time,
				outcome,
				success_time,
				consecutive_failures
			FROM network_extender_measurement_probe
			WHERE extender_id = ANY($1)
			ORDER BY extender_id, ip_version
			`,
			extenderIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				probe := scanNetworkExtenderMeasurementProbe(result)
				probes[probe.ExtenderId] = append(probes[probe.ExtenderId], probe)
			}
		})
	})
	return probes
}

// GetNetworkExtenderMeasurementProbesAt reads the probes stored at exactly
// probeTime that are still their family's latest: what is left of one pass, once
// any later probe has replaced part of it. The table holds at most two rows per
// extender that served in the retention, so this reads it all.
func GetNetworkExtenderMeasurementProbesAt(
	ctx context.Context,
	probeTime time.Time,
) []*NetworkExtenderMeasurementProbe {
	probes := []*NetworkExtenderMeasurementProbe{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				extender_id,
				ip_version,
				ip,
				probe_time,
				outcome,
				success_time,
				consecutive_failures
			FROM network_extender_measurement_probe
			WHERE probe_time = $1
			ORDER BY extender_id, ip_version
			`,
			probeTime.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				probes = append(probes, scanNetworkExtenderMeasurementProbe(result))
			}
		})
	})
	return probes
}

// One row of the columns both reads select, in their order.
func scanNetworkExtenderMeasurementProbe(result server.PgResult) *NetworkExtenderMeasurementProbe {
	probe := &NetworkExtenderMeasurementProbe{}
	var outcome string
	server.Raise(result.Scan(
		&probe.ExtenderId,
		&probe.IpVersion,
		&probe.Ip,
		&probe.ProbeTime,
		&outcome,
		&probe.SuccessTime,
		&probe.ConsecutiveFailures,
	))
	probe.Ip = probe.Ip.Unmap().WithZone("")
	probe.Outcome = lagprobe.Outcome(outcome)
	return probe
}

// What the probes' rows say of the chain, for the dashboards: what it found, and
// whether it still runs.
type NetworkExtenderMeasurementProbeStats struct {
	// the rows probed since the cut, by the outcome of their latest probe
	OutcomeCounts map[string]int64
	// the newest probe of any row, nil when the table is empty
	NewestProbeTime *time.Time
}

// GetNetworkExtenderMeasurementProbeStats reads the rows' outcomes and the newest
// probe, in one query over the small table.
func GetNetworkExtenderMeasurementProbeStats(ctx context.Context, since time.Time) *NetworkExtenderMeasurementProbeStats {
	stats := &NetworkExtenderMeasurementProbeStats{OutcomeCounts: map[string]int64{}}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				outcome,
				count(*) FILTER (WHERE $1 <= probe_time),
				max(probe_time)
			FROM network_extender_measurement_probe
			GROUP BY outcome
			`,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var outcome string
				var count int64
				var newest time.Time
				server.Raise(result.Scan(&outcome, &count, &newest))
				stats.OutcomeCounts[outcome] = count
				if stats.NewestProbeTime == nil || stats.NewestProbeTime.Before(newest) {
					stats.NewestProbeTime = &newest
				}
			}
		})
	})
	return stats
}

// RemoveNetworkExtenderMeasurementProbesBefore deletes every probe made before
// `before`, which are those of extenders the sample stopped serving, and
// returns how many.
func RemoveNetworkExtenderMeasurementProbesBefore(ctx context.Context, before time.Time) int64 {
	var removed int64
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_extender_measurement_probe WHERE probe_time < $1`,
			before.UTC(),
		))
		removed = tag.RowsAffected()
	})
	return removed
}
