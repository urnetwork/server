package monitor

import (
	"context"
	"fmt"
	"time"
)

// SIGNALS.md §2.2 maps to signal_wait_events.go and signal_wait_events_test.go.
func NewWaitEventsSignal() Signal {
	return &signalAdapter{number: "2.2", key: "wait-events", name: "Active-query wait events", probe: pgWaitEventProbe{}}
}

type pgWaitEventProbe struct{}

func (pgWaitEventProbe) id() string             { return "pg/wait-events" }
func (pgWaitEventProbe) tier() string           { return tierWarn }
func (pgWaitEventProbe) cadence() time.Duration { return 5 * time.Minute }

func (pgWaitEventProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := pgTarget(env)
	rows, err := env.runner.pg(ctx, `
		SELECT coalesce(wait_event_type,'-'), coalesce(wait_event,'-'), count(*),
		       round(extract(epoch FROM max(clock_timestamp()-query_start)))::int,
		       (array_agg(left(regexp_replace(query,'\s+',' ','g'),160)
		          ORDER BY query_start, pid))[1] AS oldest_sample,
		       (array_agg(pid ORDER BY query_start, pid))[1] AS oldest_pid,
		       (array_agg(coalesce(query_id::text,'unknown') ORDER BY query_start, pid))[1] AS oldest_query_id,
		       (array_agg(coalesce(nullif(application_name,''),'-') ORDER BY query_start, pid))[1] AS oldest_application,
		       (array_agg(coalesce(client_addr::text,'local') ORDER BY query_start, pid))[1] AS oldest_client
		FROM pg_stat_activity
		WHERE backend_type='client backend' AND state='active' AND wait_event IS NOT NULL
		GROUP BY 1,2 HAVING count(*) >= 5 OR max(clock_timestamp()-query_start) > interval '1 minute'
		ORDER BY 3 DESC;
	`)
	if err != nil {
		return nil, err
	}
	findings := []finding{}
	for _, row := range rows {
		wait := row.str(0) + ":" + row.str(1)
		oldestSeconds := atoi(row.str(3))
		if wait == "Client:ClientRead" && oldestSeconds < 60 {
			// A busy client fleet continuously produces a handful of active
			// ClientRead samples while BEGIN/COMMIT and ordinary queries hand the
			// protocol back to the caller.  Those rows are normally only
			// milliseconds old and their PIDs rotate between samples.  Count alone
			// therefore does not prove a client-side stall for this wait class; the
			// existing one-minute age branch in the SQL is the discriminator.
			continue
		}
		if wait == "Lock:virtualxid" && expectedConcurrentReindex(row.str(4), oldestSeconds) {
			// DbMaintenance gives CONCURRENTLY reindex operations two hours.
			// They commonly wait on a pre-existing virtual transaction while
			// remaining healthy maintenance; active-queries applies the same
			// bound, so wait-events must not contradict it after one minute.
			continue
		}
		findings = append(findings, finding{
			probeId: "pg/wait-events", tier: tierWarn,
			class: "wait-event-cluster", target: target, frame: wait, sustain: 2,
			symptom:   fmt.Sprintf("%s has %s active waiter(s), oldest %ss", wait, row.str(2), row.str(3)),
			mechanism: waitEventMeaning(wait),
			baseline:  "No non-ClientRead wait event is shared by five active client backends, and no individual active command remains on one wait event for more than one minute. ClientRead count alone is healthy until its oldest command reaches one minute.",
			observed:  fmt.Sprintf("wait=%s active=%s oldest_s=%s count_guard=5 age_guard_s=60 oldest_pid=%s oldest_query_id=%s", wait, row.str(2), row.str(3), row.str(5), row.str(6)),
			evidence:  fmt.Sprintf("oldest waiter snapshot: pid=%s query_id=%s application=%s client=%s\nsample query: %s", row.str(5), row.str(6), row.str(7), row.str(8), row.str(4)),
			context:   "The PID, query ID, application, client address, and SQL sample all come from the same oldest waiter in this class. They remain the attribution snapshot even if the command completes before a follow-up pg_stat_activity query.",
			action:    waitEventAction(wait),
			verify:    "Fewer than five active backends share the wait, no individual command remains on it beyond one minute on consecutive samples, and the attributed query completes inside its historical band.",
			playbook:  "SIGNALS.md §2.2",
		})
	}
	if len(findings) == 0 {
		findings = append(findings, healthyFinding("pg/wait-events", tierWarn, "wait-event-cluster", target))
	}
	return findings, nil
}

func waitEventMeaning(wait string) string {
	switch wait {
	case "LWLock:WALWrite":
		return "Backends are clustered on WAL writes; forced checkpoints or storage pressure can serialize otherwise unrelated queries."
	case "IPC:MessageQueueReceive":
		return "Parallel-query workers are waiting on their message queues; attribute the parent query before treating backend count as organic load."
	case "Client:ClientRead":
		return "PostgreSQL is waiting for clients mid-protocol; inspect the pool/client path rather than query execution."
	case "Client:ClientWrite":
		return "PostgreSQL cannot finish sending a result because the client is not currently reading its socket. A bounded consumer phase can clear on its own; recurrence can pin the query and transaction behind client-side backpressure."
	case "Lock:virtualxid":
		return "A statement is waiting for an older virtual transaction to finish; concurrent index maintenance does this normally, but an over-bound wait means the blocker must be attributed."
	case "IO:DataFileRead":
		return "A backend is waiting for a relation data page to be read from storage. One bounded cold or large scan can do this; recurrence or a cluster points to a query-plan, cache-residency, or storage-pressure boundary."
	default:
		return "A persistent wait class is holding active backends and must be attributed before remediation."
	}
}

func waitEventAction(wait string) string {
	if wait == "Lock:virtualxid" {
		return "Use pg_blocking_pids to identify the older transaction. Allow expected concurrent reindex maintenance within its two-hour bound; investigate the blocker before canceling either backend."
	}
	if wait == "Client:ClientWrite" {
		return "Attribute the sample query and current PID, then inspect the owning client's result-consumption path and pool connection before changing PostgreSQL. Do not cancel a singleton that clears between samples."
	}
	if wait == "IO:DataFileRead" {
		return "Attribute the sample query and current PID in pg_stat_activity, compare its completed history and storage metrics, and require recurrence before changing its plan or storage. Do not cancel one bounded read solely from a one-shot sample."
	}
	return "Use the wait class to choose the next discriminator; correlate WAL waits with checkpoint cadence and client waits with the calling route."
}
