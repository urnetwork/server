package monitor

import (
	"context"
	"fmt"
	"time"
)

// Signal connection-orphans implements SIGNALS.md §2.16. It verifies that a
// durable open connection still has the ephemeral live handler which owns it.
func NewConnectionOrphansSignal() Signal {
	return &signalAdapter{
		number: "2.16", key: "connection-orphans", name: "Open connection handler ownership",
		probe: pgConnectionOrphansProbe{},
	}
}

type pgConnectionOrphansProbe struct{}

func (pgConnectionOrphansProbe) id() string             { return "pg/connection-orphans" }
func (pgConnectionOrphansProbe) tier() string           { return tierPage }
func (pgConnectionOrphansProbe) cadence() time.Duration { return time.Minute }

func (pgConnectionOrphansProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, `
		WITH connection_state AS MATERIALIZED (
		 SELECT
		  c.connect_time,
		  h.handler_id IS NULL AS orphan
		 FROM network_client_connection c
		 LEFT JOIN network_client_handler h USING (handler_id)
		 WHERE c.connected = true
		), handler_state AS MATERIALIZED (
		 SELECT
		  COUNT(*) AS handler_count,
		  COUNT(*) FILTER (WHERE heartbeat_time < now() - interval '2 minutes') AS stale_handler_count
		 FROM network_client_handler
		)
		SELECT
		 COUNT(*) AS connected_count,
		 COUNT(*) FILTER (WHERE orphan) AS orphan_count,
		 COUNT(*) FILTER (
		  WHERE orphan AND connect_time < now() - interval '2 minutes'
		 ) AS mature_orphan_count,
		 COALESCE(
		  extract(epoch FROM now() - MIN(connect_time) FILTER (WHERE orphan)),
		  0
		 )::bigint AS oldest_orphan_age_seconds,
		 handler_state.handler_count,
		 handler_state.stale_handler_count
		FROM connection_state
		CROSS JOIN handler_state
		GROUP BY handler_state.handler_count, handler_state.stale_handler_count;
	`)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) < 6 {
		return nil, fmt.Errorf("connection orphan query returned %d malformed rows", len(rows))
	}
	row := rows[0]
	connectedCount := atoiRow(row, 0)
	orphanCount := atoiRow(row, 1)
	matureOrphanCount := atoiRow(row, 2)
	oldestOrphanAgeSeconds := atoi64(row.str(3))
	if matureOrphanCount == 0 {
		return []finding{healthyFinding(
			"pg/connection-orphans", tierPage, "connection-orphans", pgTarget(env),
		)}, nil
	}

	return []finding{{
		probeId: "pg/connection-orphans", tier: tierPage,
		class: "connection-orphans", target: pgTarget(env), frame: "missing-handler", sustain: 2,
		symptom: fmt.Sprintf(
			"%d mature open connection rows have no owning handler (oldest %.1f days)",
			matureOrphanCount,
			float64(oldestOrphanAgeSeconds)/(24*60*60),
		),
		mechanism: "Handler rows are ephemeral while connection history is durable and there is intentionally no foreign key. The legacy cleanup selected expired handler IDs first, so it could close only connections whose handler still existed; a process loss, earlier deletion, or insertion race could leave an open row permanently invisible to every later cleanup. Those rows inflate connected supply and keep stale locations eligible.",
		baseline:  "Every connected row older than two handler-heartbeat intervals joins an existing handler; mature orphan count is zero.",
		observed: fmt.Sprintf(
			"connected_rows=%d orphan_rows=%d mature_orphan_rows=%d oldest_orphan_age_s=%d handler_rows=%s stale_handler_rows=%s",
			connectedCount,
			orphanCount,
			matureOrphanCount,
			oldestOrphanAgeSeconds,
			row.str(4),
			row.str(5),
		),
		evidence: "PostgreSQL left-joins the bounded current open set to handler primary keys and exports aggregate counts plus age only; no client, network, connection, or handler identifier leaves the database.",
		context:  "Fresh orphans younger than two minutes can exist briefly across the handler-cleanup cadence. This page requires older rows on two probes. Closing the durable row is necessary but provider publication also needs the §2.9 active top-level eligibility filter and a refreshed location snapshot.",
		action:   "Deploy every Taskworker from Server commit b7599962 or later. Let the existing singleton CloseExpiredNetworkClientHandlers task delete expired handlers and sweep every remaining open row whose handler is absent. Do not update connection rows, delete handlers, or clear score caches manually.",
		verify:   "All active Taskworkers have the required ancestry; two consecutive probes report zero mature orphans; a later location-reliability pass marks former orphan supply disconnected; the provider-eligibility marker is present; and open-set, destination-diversity, and child-churn signals recover without a manual restart.",
		playbook: "SIGNALS.md §2.16 and §2.9",
	}}, nil
}
