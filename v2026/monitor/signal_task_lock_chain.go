// Direct lock edges distinguish blocked task writes from harmless transaction
// age. Source visibility and owner attribution are independent requirements.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §1.3e observes current pending-task writes and their direct blockers.
func NewTaskLockChainSignal() Signal {
	return &signalAdapter{number: "1.3e", key: "task-lock-chain", name: "Task queue write lock chain", probe: taskLockChainProbe{}}
}

// Stateless observations use PostgreSQL's lock waitstart, not query age.
type taskLockChainProbe struct{}

func (taskLockChainProbe) id() string             { return "pg/task-lock-chain" }
func (taskLockChainProbe) tier() string           { return tierPage }
func (taskLockChainProbe) cadence() time.Duration { return time.Minute }

// Application task writers use these direct statement forms. A relation lock
// independently confirms the target; SQL text is never returned by the source.
const taskLockWritePattern = `^[[:space:]]*(INSERT[[:space:]]+INTO|UPDATE|DELETE[[:space:]]+FROM)[[:space:]]+(public[.])?pending_task([[:space:](]|$)`

const taskLockChainQuery = `
WITH observed AS MATERIALIZED (SELECT clock_timestamp() AS at),
activity AS MATERIALIZED (
    SELECT pid, backend_start, xact_start, state_change, state, wait_event_type, query, client_addr
    FROM pg_stat_activity WHERE datname = current_database()
), locks AS MATERIALIZED (SELECT pid, relation, mode, granted, waitstart FROM pg_locks),
writers AS MATERIALIZED (
    SELECT activity.*,
           (SELECT min(waitstart) FROM locks WHERE locks.pid = activity.pid AND NOT granted) AS wait_start
    FROM activity
    WHERE state = 'active' AND wait_event_type = 'Lock'
      AND query ~* '` + taskLockWritePattern + `'
      AND EXISTS (
          SELECT 1 FROM locks WHERE locks.pid = activity.pid
            AND relation = to_regclass('public.pending_task')
            AND mode IN ('RowExclusiveLock', 'ShareRowExclusiveLock', 'ExclusiveLock', 'AccessExclusiveLock')
      )
), edges AS MATERIALIZED (
    SELECT writer.pid AS blocked_pid, blocker.pid AS blocker_pid,
           extract(epoch FROM writer.backend_start)::bigint AS blocked_backend_start,
           extract(epoch FROM blocker.backend_start)::bigint AS blocker_backend_start,
           floor(extract(epoch FROM observed.at-writer.wait_start))::bigint AS wait_s,
           CASE WHEN blocker.state = 'idle in transaction'
                THEN floor(extract(epoch FROM observed.at-blocker.state_change))::bigint ELSE 0 END AS idle_s,
           coalesce(floor(extract(epoch FROM observed.at-blocker.xact_start))::bigint, 0) AS xact_s,
           coalesce(blocker.state, 'unavailable') AS blocker_state,
           coalesce(host(blocker.client_addr), '') AS blocker_address,
           blocker.pid IS NOT NULL AS blocker_visible
    FROM writers AS writer
    CROSS JOIN observed
    CROSS JOIN LATERAL unnest(pg_blocking_pids(writer.pid)) AS blocking(pid)
    LEFT JOIN activity AS blocker ON blocker.pid = blocking.pid
)
SELECT extract(epoch FROM observed.at)::bigint,
       pg_has_role(current_user, 'pg_read_all_stats', 'USAGE'),
       (SELECT count(*) FROM writers), (SELECT count(*) FROM edges),
       coalesce((SELECT jsonb_agg(sample) FROM (
           SELECT * FROM edges ORDER BY wait_s DESC NULLS FIRST, blocked_pid, blocker_pid LIMIT 32
       ) AS sample), '[]'::jsonb)
FROM observed;
`

// Optional numeric pointers distinguish missing evidence from a measured zero.
type taskLockEdge struct {
	BlockedPid          int64  `json:"blocked_pid"`
	BlockerPid          int64  `json:"blocker_pid"`
	BlockedBackendStart int64  `json:"blocked_backend_start"`
	BlockerBackendStart int64  `json:"blocker_backend_start"`
	WaitSeconds         *int64 `json:"wait_s"`
	IdleSeconds         *int64 `json:"idle_s"`
	XactSeconds         *int64 `json:"xact_s"`
	BlockerState        string `json:"blocker_state"`
	BlockerAddress      string `json:"blocker_address"`
	BlockerVisible      *bool  `json:"blocker_visible"`
}

// A raced-away or prepared-transaction blocker is unknown, not a healthy edge.
func parseTaskLockChain(rows []pgRow, now time.Time) ([]taskLockEdge, bool, error) {
	invalid := fmt.Errorf("task lock chain: incomplete or invalid observation")
	if len(rows) != 1 || len(rows[0]) != 5 {
		return nil, false, invalid
	}
	observed, err := strconv.ParseInt(rows[0].str(0), 10, 64)
	if err != nil || observed <= 0 || now.Sub(time.Unix(observed, 0)).Abs() > 30*time.Second || rows[0].str(1) != "t" {
		return nil, false, invalid
	}
	writers, err := strconv.ParseInt(rows[0].str(2), 10, 64)
	if err != nil || writers < 0 {
		return nil, false, invalid
	}
	count, err := strconv.ParseInt(rows[0].str(3), 10, 64)
	if err != nil || count < 0 || (writers == 0 && count != 0) {
		return nil, false, invalid
	}
	var edges []taskLockEdge
	decoder := json.NewDecoder(strings.NewReader(rows[0].str(4)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&edges); err != nil || edges == nil || int64(len(edges)) != min(count, 32) {
		return nil, false, invalid
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, false, invalid
	}
	complete := count <= 32 && (writers == 0 || count >= writers)
	seenPidPairs := map[[2]int64]bool{}
	blockedPids := map[int64]bool{}
	for _, edge := range edges {
		if edge.BlockedPid <= 0 || edge.BlockedBackendStart <= 0 || edge.BlockedBackendStart > observed ||
			edge.WaitSeconds == nil || *edge.WaitSeconds < 0 || edge.IdleSeconds == nil || *edge.IdleSeconds < 0 ||
			edge.XactSeconds == nil || *edge.XactSeconds < 0 || edge.BlockerVisible == nil {
			return nil, false, invalid
		}
		blockedPids[edge.BlockedPid] = true
		if !*edge.BlockerVisible {
			complete = false
			continue
		}
		key := [2]int64{edge.BlockedPid, edge.BlockerPid}
		if edge.BlockerPid <= 0 || edge.BlockerPid == edge.BlockedPid || edge.BlockerBackendStart <= 0 ||
			edge.BlockerBackendStart > observed || *edge.IdleSeconds > *edge.XactSeconds || seenPidPairs[key] {
			return nil, false, invalid
		}
		seenPidPairs[key] = true
		switch edge.BlockerState {
		case "active", "idle", "idle in transaction", "idle in transaction (aborted)", "fastpath function call", "disabled":
		default:
			return nil, false, invalid
		}
		if edge.BlockerState != "idle in transaction" && *edge.IdleSeconds != 0 {
			return nil, false, invalid
		}
	}
	if int64(len(blockedPids)) > writers {
		return nil, false, invalid
	}
	complete = complete && int64(len(blockedPids)) == writers
	return edges, complete, nil
}

// Confirmed edges can alert during truncated coverage; only complete evidence
// clears a preceding fault. Owner mapping never confers termination authority.
func (taskLockChainProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	host := env.cfg.hostByRole("pg-primary")
	if host == nil {
		return nil, fmt.Errorf("task lock chain: no enabled database host")
	}
	rows, err := env.runner.pg(ctx, taskLockChainQuery)
	var edges []taskLockEdge
	complete := false
	if err == nil {
		edges, complete, err = parseTaskLockChain(rows, env.now())
	}
	findings := []finding{}
	if err != nil || !complete {
		findings = append(findings, finding{
			probeId: "pg/task-lock-chain", tier: tierWarn, class: "task-lock-chain-unavailable", target: host.name, sustain: 2,
			symptom:   "Current task-write blocking coverage is incomplete.",
			mechanism: "The direct source failed, lacks pg_read_all_stats visibility, is stale or malformed, raced a disappearing blocker, or exceeded the 32-edge detail bound.",
			baseline:  "Fresh complete statistics with every direct task-write blocker visible.", observed: "source_status=unavailable_or_partial",
			action: "Restore direct statistics visibility; retain confirmed lock findings and resolve missing ownership separately. Missing evidence is not an unlocked queue.",
			verify: "Two fresh complete observations succeed; a prior task-lock finding clears only on complete evidence.", playbook: "SIGNALS.md §1.3e",
		})
	} else {
		findings = append(findings, healthyFinding("pg/task-lock-chain", tierWarn, "task-lock-chain-unavailable", host.name))
	}
	if err != nil {
		return findings, nil
	}
	var worst *taskLockEdge
	for index := range edges {
		edge := &edges[index]
		if *edge.BlockerVisible && edge.BlockerState == "idle in transaction" &&
			*edge.WaitSeconds >= 60 && *edge.IdleSeconds >= 60 &&
			(worst == nil || min(*edge.WaitSeconds, *edge.IdleSeconds) > min(*worst.WaitSeconds, *worst.IdleSeconds)) {
			worst = edge
		}
	}
	if worst == nil {
		if complete {
			findings = append(findings, healthyFinding("pg/task-lock-chain", tierWarn, "task-write-idle-blocker", host.name))
		}
		return findings, nil
	}
	tier := tierWarn
	if *worst.WaitSeconds >= 300 && *worst.IdleSeconds >= 300 {
		tier = tierPage
	}
	findings = append(findings, finding{
		probeId: "pg/task-lock-chain", tier: tier, class: "task-write-idle-blocker", target: host.name, sustain: 2,
		symptom:   "A pending-task write is waiting behind an idle transaction.",
		mechanism: "A direct pg_blocking_pids edge joins the queued write's current ungranted lock to its idle-in-transaction blocker. This can obstruct scheduling or lease updates well before the generic 30-minute zombie threshold.",
		baseline:  "WARN when both continuous lock wait and blocker idle time reach 60s; PAGE at 300s, sustained for two one-minute observations.",
		observed:  fmt.Sprintf("wait_s=%d blocker_idle_s=%d blocker_xact_s=%d detail_edges=%d complete=%t", *worst.WaitSeconds, *worst.IdleSeconds, *worst.XactSeconds, len(edges), complete),
		evidence:  fmt.Sprintf("blocked_pid=%d blocked_backend_start=%d blocker_pid=%d blocker_backend_start=%d owner_host=%s; task identifiers, addresses and SQL withheld", worst.BlockedPid, worst.BlockedBackendStart, worst.BlockerPid, worst.BlockerBackendStart, reliabilityTaskSourceHost(env.cfg, worst.BlockerAddress)),
		context:   "An old transaction without this edge is not a fault here. Query age is not lock-wait age. A lease age, due time, or advisory lock alone does not prove a dead worker. Direct edges omit deeper chains and nonstandard task-write SQL; owner host does not prove service generation, especially behind NAT or pooling.",
		action:    "Re-read the exact backend start times and lock chain; attest the socket, owning process/service generation and same-attempt task progress. Inspect cancellation/commit and bounded goroutine evidence. Do not mass-terminate, restart PostgreSQL, or kill an advisory owner from age alone; any targeted termination needs operator authorization and fresh identity proof.",
		verify:    "Two complete fresh observations have no qualifying edge; separately verify closer checkpoints and aged-open drainage. A cleared lock does not prove that maintenance throughput recovered.", playbook: "SIGNALS.md §1.3e",
	})
	return findings, nil
}
