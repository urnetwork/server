package monitor

import (
	"context"
	"fmt"
	"time"
)

// Signal connection-orphans implements SIGNALS.md §2.16. It verifies that a
// durable open connection retains handler ownership. Stale SQL heartbeats leave
// the readers' separate liveness judgement unobserved by this aggregate.
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

func (pgConnectionOrphansProbe) check(ctx context.Context, env *probeEnv) (result []finding, resultErr error) {
	defer finishHandlerLeaseObservation(ctx, &result, &resultErr)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := env.runner.pg(ctx, `
		WITH connection_state AS MATERIALIZED (
		 SELECT
		  c.connect_time,
		  h.handler_id IS NULL AS orphan,
		  h.handler_id IS NOT NULL AND h.heartbeat_time < now() - interval '2 minutes' AS stale_owner
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
		 (SELECT handler_count FROM handler_state) AS handler_count,
		 (SELECT stale_handler_count FROM handler_state) AS stale_handler_count,
		 COUNT(*) FILTER (WHERE stale_owner) AS stale_open_connection_count
		FROM connection_state;
	`)
	if err := handlerLeaseQueryError(ctx, err); err != nil {
		return nil, err
	}
	values, err := pgAggregateIntegers(rows, 7)
	if err != nil {
		return nil, err
	}
	connectedCount, orphanCount, matureOrphanCount := values[0], values[1], values[2]
	oldestOrphanAgeSeconds, handlerCount, staleHandlerCount := values[3], values[4], values[5]
	staleOpenCount := values[6]
	if connectedCount < orphanCount || orphanCount < matureOrphanCount || handlerCount < staleHandlerCount || staleOpenCount > connectedCount-orphanCount || staleOpenCount > 0 && staleHandlerCount == 0 {
		return nil, fmt.Errorf("invalid response for connection orphan aggregate: inconsistent counts")
	}
	findings := []finding{}
	if staleOpenCount > 0 {
		findings = append(findings, handlerLeaseCoverageFinding("pg/connection-orphans", pgTarget(env), "open-connections", staleOpenCount))
	}
	if matureOrphanCount == 0 {
		if staleOpenCount == 0 {
			findings = append(findings, healthyFinding("pg/connection-orphans", tierPage, "connection-orphans", pgTarget(env)))
		}
		return findings, nil
	}

	return append(findings, finding{
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
			"connected_rows=%d orphan_rows=%d mature_orphan_rows=%d oldest_orphan_age_s=%d handler_rows=%d stale_handler_rows=%d stale_owned_open_rows=%d",
			connectedCount,
			orphanCount,
			matureOrphanCount,
			oldestOrphanAgeSeconds,
			handlerCount,
			staleHandlerCount,
			staleOpenCount,
		),
		evidence: "PostgreSQL left-joins the bounded current open set to handler primary keys and exports aggregate counts plus age only; no client, network, connection, or handler identifier leaves the database.",
		context:  "Fresh orphans younger than two minutes can exist briefly across the handler-cleanup cadence. This page requires older rows on two probes. Closing the durable row is necessary but provider publication also needs the §2.9 active top-level eligibility filter and a refreshed location snapshot.",
		action:   "Verify the running Taskworker and Connect ancestry: b7599962 repaired missing-handler sweeping, while the October 19 reaper protection was reported as 53e858563 and needs independent cohort verification. Retain the existing singleton cleanup and planned-maintenance safeguards for unknown or mixed generations. Do not update connection rows, delete handlers, or clear score caches manually.",
		verify:   "All active Taskworkers have the required ancestry; two consecutive probes report zero mature orphans; a later location-reliability pass marks former orphan supply disconnected; the provider-eligibility marker is present; and open-set, destination-diversity, and child-churn signals recover without a manual restart.",
		playbook: "SIGNALS.md §2.16 and §2.9",
	}), nil
}

// SQL lease age does not observe the readers' separate liveness judgement.
// Reuse the established visibility class so independent incident findings remain.
func handlerLeaseCoverageFinding(probeId, target, domain string, staleOpen int64) finding {
	f := cannotObserveFinding(target, &observationStateUnavailableError{})
	f.probeId, f.frame = probeId, "handler-lease-coverage"
	f.symptom = fmt.Sprintf("Reader judgement is unobserved for %d open rows with stale SQL handler heartbeats", staleOpen)
	f.mechanism = "These open rows retain an existing handler whose SQL heartbeat is older than 120 seconds. Current readers also accept stale handlers judged live by the shared writer-stamp or fleet-fallback policy. This SQL aggregate does not observe that judgement, its cache or deployed adoption; SQL age alone proves neither exclusion nor an ended process."
	f.baseline = "Relevant open rows retain handler ownership; a fresh SQL heartbeat or an applicable judged-live stale handler can satisfy current readers. This aggregate observes only the SQL branch. Unused stale handlers do not consume this coverage."
	f.observed = fmt.Sprintf("coverage_domain=%s stale_owned_open_rows=%d handler_lease_seconds=120 reader_judgement=unobserved read_side_exclusion=unproven process_liveness=unknown", domain, staleOpen)
	f.evidence = "One existing PostgreSQL aggregate counts joined rows without exporting handler, client or connection identities. It reads no writer stamps or consumer judgement cache and does not override judged-live ownership."
	f.context = "Location, subscriber, named-subscriber, audit and ARIN readers share the stale-handler judgement. Older readers or request-path readers without a usable cached judgement can still take only the fresh-heartbeat branch; background judgement failure is separate incomplete work. Missing-handler and complete empty-export findings remain independently valid."
	f.action = "Correlate the database/PgBouncer path with the applicable reader judgement, its freshness and actual API/Taskworker adoption. Keep §5.13 safeguards for unqualified or mixed generations. Do not delete handlers, close connections or rewrite location/cache state from SQL heartbeat age alone."
	f.verify = "Fresh SQL heartbeats can restore this aggregate's narrow coverage. When SQL remains stale, independently qualified reader judgement, cache freshness and deployed adoption may establish read-side recovery; this SQL-only probe leaves that authority unobserved and must not overrule it. Verify transport and selection behavior separately."
	f.playbook = "SIGNALS.md §2.16, §2.9 and §5.13"
	return f
}

// Preserve both ended-parent and original SQL causes without exposing source text.
func handlerLeaseQueryError(ctx context.Context, sourceErr error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		if sourceErr != nil && sourceErr != contextErr {
			return &signalContextError{contextErr: contextErr, sourceErr: sourceErr}
		}
		return contextErr
	}
	return sourceErr
}

// Parsing and reduction belong to the owner too, including unscoped adapters.
func finishHandlerLeaseObservation(ctx context.Context, findings *[]finding, resultErr *error) {
	if contextErr := ctx.Err(); contextErr != nil {
		*findings = nil
		if *resultErr != nil && *resultErr != contextErr {
			*resultErr = &signalContextError{contextErr: contextErr, sourceErr: *resultErr}
		} else {
			*resultErr = contextErr
		}
	}
}
