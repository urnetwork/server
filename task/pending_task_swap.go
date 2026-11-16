// The table swap profile of pending_task (server.SwapTable;
// PARTITION-CANDIDATES.md §4.4): the queue's live rows are copied into a fresh
// twin during a taskworker stop, and the bloated file is dropped later.
//
// The swap needs every taskworker stopped. Copied rows carry the swap's
// transaction id, and an ordinary completion runs in a repeatable read
// transaction (task_completion.go): one whose snapshot predates the swap would
// find no row for its fenced delete, raise errTaskClaimOwnership, and its task
// would run again after its lease. Heartbeats, lease extensions and retries are
// fenced the same way.
//
// The preflight proves no taskworker is running from the database's side:
//   - a claimed task's owner holds a session advisory lock on its id
//     (taskAdvisoryLockKey) for as long as it runs, and PostgreSQL drops it
//     with the owner's connection, so a leased row whose lock some session
//     holds has a live owner;
//   - a live owner heartbeats claim_time every ReleaseTimeout/3, so a leased
//     row that heartbeat within ReleaseTimeout was owned moments ago.
//
// A leased row whose lock no session holds and whose heartbeat is older is a
// stopped worker's claim. It is copied as it is, keeps its claim generation
// and lease, and is claimed again once the lease ends, exactly as without the
// swap.
package task

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

const pendingTaskSwapTable = "pending_task"

var pendingTaskSwapProfile = server.RegisterTableSwapProfile(&server.TableSwapProfile{
	Table: pendingTaskSwapTable,
	Stop: "every taskworker stopped, units and containers (docker ps lists none), under the settlement gate; " +
		"producers in the other services keep writing through the swap",
	Preflight:      pendingTaskSwapPreflight,
	LockedCheckSql: pendingTaskSwapLockedCheckSql,
})

// One row per leased task: its id and function, its heartbeat's age and its
// lease's time left in seconds, whether a live session holds its advisory
// lock (taskAdvisoryLockKey, computed by the database), and whether it
// heartbeat within ReleaseTimeout. A lease ahead puts the row's
// available_block ahead too, so the poll order index bounds the read to rows
// not yet available.
func pendingTaskSwapLeasesSql() string {
	return fmt.Sprintf(`
	WITH leased AS (
		SELECT task_id, function_name, claim_time, release_time,
			('x' || substr(replace(task_id::text, '-', ''), 1, 16))::bit(64)::bigint
				# ('x' || substr(replace(task_id::text, '-', ''), 17, 16))::bit(64)::bigint
				# (%d)::bigint AS lock_key
		FROM pending_task
		WHERE available_block > extract(epoch FROM now() AT TIME ZONE 'UTC')::bigint
			AND release_time > now() AT TIME ZONE 'UTC'
	), held AS (
		SELECT DISTINCT (classid::bigint << 32) | objid::bigint AS lock_key
		FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 1 AND granted AND pid <> pg_backend_pid()
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
	)
	SELECT
		leased.task_id,
		leased.function_name,
		extract(epoch FROM now() AT TIME ZONE 'UTC' - leased.claim_time)::float8 AS heartbeat_age,
		extract(epoch FROM leased.release_time - now() AT TIME ZONE 'UTC')::float8 AS lease_left,
		held.lock_key IS NOT NULL AS owned,
		leased.claim_time > now() AT TIME ZONE 'UTC' - make_interval(secs => %d) AS fresh
	FROM leased LEFT JOIN held ON held.lock_key = leased.lock_key
	ORDER BY leased.task_id`,
		int64(taskAdvisoryLockNamespace),
		int64(ReleaseTimeout/time.Second),
	)
}

// The check the swap script runs under pending_task's lock, where no claim,
// heartbeat or completion can change a row: it raises while a task has a live
// owner or a fresh heartbeat.
func pendingTaskSwapLockedCheckSql() string {
	return fmt.Sprintf(`DO $table_swap$
DECLARE
	owned_count bigint;
	fresh_count bigint;
	sample_text text;
BEGIN
	SELECT
		count(*) FILTER (WHERE leases.owned),
		count(*) FILTER (WHERE leases.fresh AND NOT leases.owned),
		left(string_agg(leases.task_id::text || ' ' || leases.function_name, '; ') FILTER (WHERE leases.owned OR leases.fresh), 500)
	INTO owned_count, fresh_count, sample_text
	FROM (%s) leases;
	IF 0 < owned_count THEN
		RAISE EXCEPTION 'a taskworker is running: %% task(s) have a live owner holding their advisory lock: %%', owned_count, sample_text;
	END IF;
	IF 0 < fresh_count THEN
		RAISE EXCEPTION 'a taskworker ran moments ago: %% task(s) heartbeat within %ds: %%', fresh_count, sample_text;
	END IF;
END
$table_swap$`, pendingTaskSwapLeasesSql(), int64(ReleaseTimeout/time.Second))
}

// The live owners and recent heartbeats among the leased rows, before the
// swap takes its lock: refused while any exists, with the leases reported.
func pendingTaskSwapPreflight(ctx context.Context, conn server.PgQuerier) (findings []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			if recoveredErr, ok := r.(error); ok {
				err = recoveredErr
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	type lease struct {
		taskId       server.Id
		functionName string
		heartbeatAge time.Duration
		leaseLeft    time.Duration
		owned        bool
		fresh        bool
	}
	leases := []*lease{}
	result, queryErr := conn.Query(ctx, pendingTaskSwapLeasesSql())
	server.WithPgResult(result, queryErr, func() {
		for result.Next() {
			l := &lease{}
			var heartbeatSeconds, leftSeconds float64
			server.Raise(result.Scan(&l.taskId, &l.functionName, &heartbeatSeconds, &leftSeconds, &l.owned, &l.fresh))
			l.heartbeatAge = time.Duration(heartbeatSeconds * float64(time.Second))
			l.leaseLeft = time.Duration(leftSeconds * float64(time.Second))
			leases = append(leases, l)
		}
	})
	if len(leases) == 0 {
		findings = append(findings, "pending_task: no leased row")
		return
	}
	owned := []string{}
	fresh := []string{}
	var longestLease time.Duration
	for _, l := range leases {
		describe := fmt.Sprintf("%s %s (heartbeat %s ago, lease ends in %s)", l.taskId, l.functionName,
			l.heartbeatAge.Round(time.Second), l.leaseLeft.Round(time.Second))
		if l.owned {
			owned = append(owned, describe)
		} else if l.fresh {
			fresh = append(fresh, describe)
		}
		longestLease = max(longestLease, l.leaseLeft)
	}
	findings = append(findings, fmt.Sprintf(
		"pending_task: %d leased row(s), the last lease ending in %s; %d with a live owner, %d heartbeat within %s; "+
			"a stopped worker's lease is copied as it is and the task is claimed again once it ends",
		len(leases), longestLease.Round(time.Second), len(owned), len(fresh), ReleaseTimeout,
	))
	sample := func(descriptions []string) string {
		if 5 < len(descriptions) {
			return strings.Join(descriptions[:5], "; ") + fmt.Sprintf("; and %d more", len(descriptions)-5)
		}
		return strings.Join(descriptions, "; ")
	}
	switch {
	case 0 < len(owned):
		err = fmt.Errorf("a taskworker is running: %d task(s) have a live owner holding their advisory lock: %s", len(owned), sample(owned))
	case 0 < len(fresh):
		err = fmt.Errorf("a taskworker ran moments ago: %d task(s) heartbeat within %s: %s", len(fresh), ReleaseTimeout, sample(fresh))
	}
	return
}
