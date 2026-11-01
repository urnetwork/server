package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// Whether an extender the lag.dev sample could offer is stable now
// (lag.dev/PLAN.md A4). Two kinds of evidence say it is not.
//
// Its uptime probes: the latest probe of one of its active addresses failed
// (connect/EXTENDER.md C3). That is the address row's last probe time without
// a success at that time. An activation resets the failure count but leaves
// both times, so an extender that restarted after a failed probe stays
// unstable until its next probe answers.
//
// Its restarts: an extender that restarts runs an activation pass at once, one
// network_extender_activation row per family a few seconds apart. A burst of
// passes that each activated both families is a restart loop. What else
// starts a pass is not counted:
//
//   - a pass of one family. Connect re-posts the working family of a host
//     whose other family is refused on its backoff (10, 20, 40 minutes ...),
//     and such an extender often holds only that family's address, so its
//     passes cannot be told from a single-stack extender's; and a key many
//     clients activate posts from each of them. So an extender with one family
//     is unstable only by its probes;
//   - a pass soon after one of the extender's revocations, which re-activates
//     it once it reads that its probes withdrew it. The probes are what judge
//     that;
//   - connect's hourly address check, which starts a full pass when the caller
//     address flips between families. It starts at most one an hour, so a
//     burst window shorter than two hours never counts three of those.

// LagExtenderStabilityRule is what makes a burst of restarts.
type LagExtenderStabilityRule struct {
	// activation rows of one extender less than this apart are one pass
	PassGap time.Duration
	// BurstPasses passes that each activated both families, the first less
	// than BurstWindow before the last, are a burst
	BurstPasses int
	BurstWindow time.Duration
	// an extender is unstable until BurstHold after the last pass of a burst
	BurstHold time.Duration
	// a pass less than this after a revocation of the extender is its
	// re-activation, and is not counted
	RevocationWindow time.Duration
}

// LagExtenderStability is the evidence that an extender is unstable now.
type LagExtenderStability struct {
	// the latest pass that ended a burst less than BurstHold ago, nil when
	// none did
	BurstPassTime *time.Time
	// the latest probe of one of its active addresses failed
	ProbeFailed bool
}

// Unstable reports whether the extender should not be offered now.
func (self *LagExtenderStability) Unstable() bool {
	return self.ProbeFailed || self.BurstPassTime != nil
}

// GetLagExtenderStability reads the stability evidence of the given extenders
// at `now` in one query. An extender without evidence is absent.
//
// The passes are read from the activations of the last BurstHold plus
// BurstWindow; a counted pass ends a burst when the counted pass
// BurstPasses-1 before it started less than BurstWindow earlier.
func GetLagExtenderStability(
	ctx context.Context,
	extenderIds []server.Id,
	now time.Time,
	rule *LagExtenderStabilityRule,
) map[server.Id]*LagExtenderStability {
	stabilities := map[server.Id]*LagExtenderStability{}
	if len(extenderIds) == 0 {
		return stabilities
	}
	burstOffset := max(rule.BurstPasses-1, 0)
	activationStart := now.Add(-(rule.BurstHold + rule.BurstWindow + rule.PassGap))
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			WITH activation AS (
				SELECT
					extender_id,
					activate_time,
					ip_version,
					activate_time - lag(activate_time) OVER (
						PARTITION BY extender_id ORDER BY activate_time
					) AS since_previous
				FROM network_extender_activation
				WHERE
					extender_id = ANY($1) AND
					$2 < activate_time AND
					activate_time <= $3
			),
			activation_pass AS (
				SELECT
					extender_id,
					activate_time,
					ip_version,
					count(*) FILTER (
						WHERE since_previous IS NULL OR make_interval(secs => $4) <= since_previous
					) OVER (
						PARTITION BY extender_id ORDER BY activate_time
					) AS pass_number
				FROM activation
			),
			full_pass AS (
				SELECT
					extender_id,
					min(activate_time) AS pass_time
				FROM activation_pass
				GROUP BY extender_id, pass_number
				HAVING bool_or(ip_version = 4) AND bool_or(ip_version = 6)
			),
			revocation AS (
				SELECT
					extender_id,
					create_time
				FROM network_extender_publish
				WHERE
					(published_time IS NULL OR $8 < published_time) AND
					$8 < create_time AND
					kind = $10 AND
					extender_id = ANY($1)
			),
			restart_pass AS (
				SELECT
					extender_id,
					pass_time
				FROM full_pass
				WHERE NOT EXISTS (
					SELECT 1
					FROM revocation
					WHERE
						revocation.extender_id = full_pass.extender_id AND
						revocation.create_time <= full_pass.pass_time AND
						full_pass.pass_time - revocation.create_time < make_interval(secs => $9)
				)
			),
			burst_pass AS (
				SELECT
					extender_id,
					pass_time,
					pass_time - lag(pass_time, $6) OVER (
						PARTITION BY extender_id ORDER BY pass_time
					) AS burst_span
				FROM restart_pass
			),
			burst AS (
				SELECT
					extender_id,
					max(pass_time) AS burst_pass_time
				FROM burst_pass
				WHERE
					burst_span < make_interval(secs => $5) AND
					$7 < pass_time
				GROUP BY extender_id
			),
			probe_failure AS (
				SELECT DISTINCT extender_id
				FROM network_extender_address
				WHERE
					extender_id = ANY($1) AND
					active AND
					last_probe_time IS NOT NULL AND
					(last_probe_success_time IS NULL OR last_probe_success_time < last_probe_time)
			)
			SELECT
				COALESCE(burst.extender_id, probe_failure.extender_id),
				burst.burst_pass_time,
				probe_failure.extender_id IS NOT NULL
			FROM burst
			FULL OUTER JOIN probe_failure ON probe_failure.extender_id = burst.extender_id
			`,
			extenderIds,
			activationStart.UTC(),
			now.UTC(),
			rule.PassGap.Seconds(),
			rule.BurstWindow.Seconds(),
			burstOffset,
			now.Add(-rule.BurstHold).UTC(),
			activationStart.Add(-rule.RevocationWindow).UTC(),
			rule.RevocationWindow.Seconds(),
			NetworkExtenderPublishKindRevocation,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var extenderId server.Id
				stability := &LagExtenderStability{}
				server.Raise(result.Scan(
					&extenderId,
					&stability.BurstPassTime,
					&stability.ProbeFailed,
				))
				stabilities[extenderId] = stability
			}
		})
	})
	return stabilities
}

// Testing_AddNetworkExtenderActivation writes the activation history row of
// one family at activateTime, as an activation pass writes one for each family
// it activates. Test only.
func Testing_AddNetworkExtenderActivation(ctx context.Context, extenderId server.Id, ipVersion int, activateTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		insertNetworkExtenderActivationInTx(
			ctx,
			tx,
			extenderId,
			&NetworkExtenderActivation{IpVersion: ipVersion},
			activateTime,
		)
	})
}

// Testing_AddNetworkExtenderRevocation queues a revocation of the extender
// created at createTime, as the probes queue one when they withdraw its last
// address, not yet published. Test only.
func Testing_AddNetworkExtenderRevocation(ctx context.Context, extenderId server.Id, createTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		insertNetworkExtenderPublishInTx(
			ctx,
			tx,
			extenderId,
			NetworkExtenderPublishKindRevocation,
			[]byte{1},
			createTime,
		)
	})
}
