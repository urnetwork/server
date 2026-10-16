// Advisory ownership qualifies timestamp lag; it does not prove a task's
// execution health or authorize releasing its session.
package monitor

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// Read only the oldest bounded queue prefix and one current lock snapshot.
// The XOR key is the task package's session-ownership namespace. Identifiers
// remain inside PostgreSQL; only aggregate counts and ages leave the source.
const taskDueOwnershipQuery = `
WITH observed AS MATERIALIZED (SELECT clock_timestamp() AS at),
due AS MATERIALIZED (
    SELECT available_block, run_priority, run_max_time_seconds,
           floor(extract(epoch FROM now()-greatest(run_at, release_time)))::bigint AS age_s,
           (('x'||substr(replace(task_id::text,'-',''),1,16))::bit(64)
            # ('x'||substr(replace(task_id::text,'-',''),17,16))::bit(64)
            # x'75726e7461736b31')::bigint AS ownership_key
    FROM pending_task
    WHERE available_block <= floor(extract(epoch FROM now()))::bigint
      AND greatest(run_at, release_time) <= now()
    ORDER BY available_block, run_priority DESC, run_max_time_seconds DESC
    LIMIT 257
), sample AS MATERIALIZED (
    SELECT * FROM due
    ORDER BY available_block, run_priority DESC, run_max_time_seconds DESC
    LIMIT 256
), owners AS MATERIALIZED (
    SELECT DISTINCT classid::bigint AS key_high, objid::bigint AS key_low
    FROM pg_locks
    WHERE locktype='advisory' AND granted AND objsubid=1
      AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
)
SELECT extract(epoch FROM (SELECT at FROM observed))::bigint,
       count(*), count(owners.key_high),
       coalesce(max(age_s) FILTER (WHERE owners.key_high IS NOT NULL),0),
       coalesce(max(age_s) FILTER (WHERE owners.key_high IS NULL),0),
       (SELECT count(*) > 256 FROM due)
FROM sample LEFT JOIN owners
  ON owners.key_high=((sample.ownership_key >> 32) & 4294967295)
 AND owners.key_low=(sample.ownership_key & 4294967295);
`

// Failure keeps ownership explicitly unknown while the independently observed
// timestamp lag remains visible. Absence of an advisory owner is not proof of
// claimability: row locks, target admission and source races remain separate.
func taskDueOwnershipEvidence(ctx context.Context, env *probeEnv) string {
	unknown := "due_ownership=unknown; timestamp lag alone does not establish whether a task is claimed or whether the task plane stopped"
	rows, err := env.runner.pg(ctx, taskDueOwnershipQuery)
	if err != nil || len(rows) != 1 || len(rows[0]) != 6 {
		return unknown
	}
	values := [5]int64{}
	for index := range values {
		value, err := strconv.ParseInt(rows[0].str(index), 10, 64)
		if err != nil || value < 0 {
			return unknown
		}
		values[index] = value
	}
	observed, count, held, heldAge, unownedAge := values[0], values[1], values[2], values[3], values[4]
	partial := rows[0].str(5)
	if observed == 0 || env.now().Sub(time.Unix(observed, 0)).Abs() > 30*time.Second ||
		count > 256 || held > count || (partial != "t" && partial != "f") ||
		(partial == "t" && count != 256) || (held == 0 && heldAge != 0) || (held == count && unownedAge != 0) {
		return unknown
	}
	return fmt.Sprintf("due_ownership=observed sampled_due=%d sampled_advisory_held=%d sampled_without_advisory_owner=%d oldest_held_s=%d oldest_without_owner_s=%d prefix_truncated=%t; a held session can outlive timestamp freshness; no observed advisory owner does not prove claimability or a dead worker",
		count, held, count-held, heldAge, unownedAge, partial == "t")
}
