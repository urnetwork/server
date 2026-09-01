// Escalation batteries: the perishable evidence-collection steps from the
// SIGNALS.md §5 playbooks, run once when a signal trips so the ticket arrives
// pre-loaded with the measurements a diagnostician would run first. Batteries
// are read-only and bounded; a battery error degrades to a note in the
// evidence, never a probe failure.
package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// batteryLatch runs a probe's escalation battery once per trip: the battery
// fires when its finding key transitions healthy -> broken, and a healthy tick
// re-arms it. While the key stays broken, the trip-time output is reattached
// to each subsequent finding (annotated with when it was collected), so ticket
// updates keep their evidence without re-landing battery load on an
// already-sick target every tick. One latch per probe instance; a probe never
// overlaps itself, but the lock keeps the map safe regardless.
type batteryLatch struct {
	lock sync.Mutex
	// finding key -> battery output captured when the key tripped; a key is
	// present exactly while its finding is broken (disarmed)
	tripped map[string]trippedBattery
	now     func() time.Time
}

type trippedBattery struct {
	at       time.Time
	evidence string
}

func newBatteryLatch() *batteryLatch {
	return &batteryLatch{
		tripped: map[string]trippedBattery{},
		now:     time.Now,
	}
}

// broken folds one broken tick of key, invoking battery only on the first
// broken tick since the key was last healthy. Returns the evidence to attach:
// fresh output on the trip, the cached trip-time output afterward.
func (self *batteryLatch) broken(key string, battery func() string) string {
	cached, alreadyTripped := func() (trippedBattery, bool) {
		self.lock.Lock()
		defer self.lock.Unlock()
		cached, ok := self.tripped[key]
		return cached, ok
	}()
	if alreadyTripped {
		return cached.evidence + fmt.Sprintf(
			"\n(battery collected once at trip %s; re-runs after a healthy tick re-arms)",
			cached.at.UTC().Format(time.RFC3339))
	}
	// run the battery outside the lock — batteries block (15s snapshot deltas)
	evidence := battery()
	func() {
		self.lock.Lock()
		defer self.lock.Unlock()
		self.tripped[key] = trippedBattery{at: self.now(), evidence: evidence}
	}()
	return evidence
}

// healthy folds one healthy tick of key, re-arming its battery so the next
// trip collects fresh evidence.
func (self *batteryLatch) healthy(key string) {
	self.lock.Lock()
	defer self.lock.Unlock()
	delete(self.tripped, key)
}

// planWallBattery is SIGNALS.md 5.8 steps 2–3, the discriminators for a
// query-plan cpu wall beyond the query_id grouping (which activeBattery
// already collects):
//   - a 15s pg_stat_statements counter delta for the top active query_ids
//     (current per-call mean far above lifetime mean = the step-change tell);
//   - a 15s idx_scan snapshot delta on transfer_contract (the intended pair
//     indexes at 0 while one wrong index takes all scans);
//   - the pg_stats landmine check on transfer_contract.open (n_distinct=1 /
//     mcv {v}@1.0 = the 2.3 rare-value poisoning).
//
// Two snapshots are taken 15s apart and diffed here — temp tables are
// unavailable to a read-only session, and this keeps no backend occupied in
// between.
func planWallBattery(ctx context.Context, env *probeEnv) string {
	parts := []string{}

	snapSql := `
		SELECT 'stmt', s.queryid::text, s.calls::text, round(s.total_exec_time)::text
		FROM pg_stat_statements s
		WHERE s.queryid IN (
			SELECT query_id FROM pg_stat_activity
			WHERE backend_type='client backend' AND state='active' AND query_id IS NOT NULL
			GROUP BY query_id ORDER BY count(*) DESC LIMIT 3
		)
		UNION ALL
		SELECT 'idx', indexrelname, idx_scan::text, '0'
		FROM pg_stat_user_indexes WHERE relname='transfer_contract';
	`
	type stmtSnap struct {
		calls   float64
		totalMs float64
	}
	parseSnap := func(rows []pgRow) (map[string]stmtSnap, map[string]float64) {
		stmtSnaps := map[string]stmtSnap{}
		idxScans := map[string]float64{}
		for _, r := range rows {
			switch r.str(0) {
			case "stmt":
				stmtSnaps[r.str(1)] = stmtSnap{calls: float64(atoiRow(r, 2)), totalMs: float64(atoiRow(r, 3))}
			case "idx":
				idxScans[r.str(1)] = float64(atoiRow(r, 2))
			}
		}
		return stmtSnaps, idxScans
	}

	rowsA, errA := env.runner.pg(ctx, snapSql)
	select {
	case <-ctx.Done():
		return "battery canceled"
	case <-time.After(15 * time.Second):
	}
	rowsB, errB := env.runner.pg(ctx, snapSql)
	if errA != nil || errB != nil {
		parts = append(parts, fmt.Sprintf("snapshot delta failed: %v %v", errA, errB))
	} else {
		stmtSnapsA, idxScansA := parseSnap(rowsA)
		stmtSnapsB, idxScansB := parseSnap(rowsB)
		parts = append(parts, "pg_stat_statements 15s delta (current vs lifetime ms/call):")
		for queryId, b := range stmtSnapsB {
			a, ok := stmtSnapsA[queryId]
			if !ok {
				continue
			}
			calls := b.calls - a.calls
			currentMs := 0.0
			if calls > 0 {
				currentMs = (b.totalMs - a.totalMs) / calls
			}
			lifetimeMs := 0.0
			if b.calls > 0 {
				lifetimeMs = b.totalMs / b.calls
			}
			parts = append(parts, fmt.Sprintf("  qid=%s calls_15s=%.0f current=%.1fms lifetime=%.1fms",
				queryId, calls, currentMs, lifetimeMs))
		}
		parts = append(parts, "transfer_contract idx_scan 15s delta:")
		type indexDelta struct {
			name  string
			delta float64
		}
		indexDeltas := []indexDelta{}
		for name, b := range idxScansB {
			indexDeltas = append(indexDeltas, indexDelta{name: name, delta: b - idxScansA[name]})
		}
		sort.Slice(indexDeltas, func(i, j int) bool { return indexDeltas[i].delta > indexDeltas[j].delta })
		for i, d := range indexDeltas {
			if i >= 6 {
				break
			}
			parts = append(parts, fmt.Sprintf("  %s = %.0f", d.name, d.delta))
		}
	}

	// the 2.3 rare-value landmine check
	rows, err := env.runner.pg(ctx, `
		SELECT n_distinct::text,
		       coalesce(most_common_vals::text, '-'),
		       coalesce(array_to_string(most_common_freqs, ','), '-')
		FROM pg_stats WHERE tablename='transfer_contract' AND attname='open';
	`)
	if err != nil {
		parts = append(parts, "pg_stats check failed: "+err.Error())
	} else if len(rows) > 0 {
		nDistinct := rows[0].str(0)
		verdict := "healthy (both values present)"
		if nDistinct == "1" {
			verdict = "landmine armed (2.3): rare value missing from stats — ANALYZE transfer_contract"
		}
		parts = append(parts, fmt.Sprintf("pg_stats transfer_contract.open: n_distinct=%s mcv=%s freqs=%s -> %s",
			nDistinct, rows[0].str(1), rows[0].str(2), verdict))
	}

	return strings.Join(parts, "\n")
}

// redisNodeBattery is the SIGNALS.md 5.2/5.4 evidence set for a sick redis
// node: memory attribution (dataset vs client buffers, the discriminator for
// why a node is growing), top client output buffers, and the accept-queue
// state on its port (recv-q pegged = event loop too busy to accept = wedge).
func redisNodeBattery(ctx context.Context, env *probeEnv, port int) string {
	h := env.cfg.hostByRole("redis-cluster")
	if h == nil {
		return ""
	}
	parts := []string{}

	info, err := env.runner.redis(ctx, h, port, "INFO", "memory")
	if err == nil {
		kv := parseRedisInfo(info)
		clientBytes := atof(kv["mem_clients_normal"]) + atof(kv["mem_clients_slaves"])
		parts = append(parts, fmt.Sprintf(
			"node %d memory: used=%s maxmemory=%s dataset=%.2fG clients=%.2fG frag=%s",
			port, kv["used_memory_human"], kv["maxmemory_human"],
			gb(atof(kv["used_memory_dataset"])), gb(clientBytes),
			kv["mem_fragmentation_ratio"]))
	} else {
		parts = append(parts, fmt.Sprintf("node %d INFO memory failed: %s", port, err))
	}

	// top clients by output buffer (a client > 32mb omem = stalled consumer)
	out, err := env.runner.shell(ctx, h, fmt.Sprintf(
		`redis-cli -p %d CLIENT LIST 2>/dev/null | awk '{for(i=1;i<=NF;i++){if($i ~ /^omem=/){split($i,a,"=");if(a[2]>1048576)print a[2], $2}}}' | sort -rn | head -3`,
		port))
	if err == nil && strings.TrimSpace(out) != "" {
		parts = append(parts, "clients with omem > 1mb (bytes addr):\n  "+strings.ReplaceAll(strings.TrimSpace(out), "\n", "\n  "))
	} else {
		parts = append(parts, "no clients with omem > 1mb")
	}

	// accept queue on the node's port
	out, err = env.runner.shell(ctx, h, fmt.Sprintf(`ss -lnt 2>/dev/null | awk '$4 ~ /:%d$/ {print "recv_q="$2" send_q="$3}'`, port))
	if err == nil && strings.TrimSpace(out) != "" {
		parts = append(parts, fmt.Sprintf("accept queue port %d: %s", port, strings.TrimSpace(out)))
	}

	return strings.Join(parts, "\n")
}

// redisConnectionBattery is the SIGNALS.md §3.5 discriminator for a node
// whose client count is far above its peers. It collapses CLIENT LIST on the
// Redis host before returning it, so the alert identifies whether the extra
// sockets are one source/process cohort repeatedly touching one hot slot or a
// fleet-wide reconnect storm without transferring thousands of raw client
// rows to the monitor.
func redisConnectionBattery(ctx context.Context, env *probeEnv, port int) string {
	h := env.cfg.hostByRole("redis-cluster")
	if h == nil {
		return ""
	}
	command := fmt.Sprintf(`redis_port=%d
marker_slot=$(redis-cli --raw -p "$redis_port" CLUSTER KEYSLOT client_reliability_stats_blocks 2>/dev/null)
marker_owner=$(redis-cli --raw -p "$redis_port" CLUSTER NODES 2>/dev/null | awk -v slot="$marker_slot" '
function owns(token, slot, range) {
  if (token ~ /^\[/) return 0
  if (token ~ /^[0-9]+$/) return token + 0 == slot
  if (token ~ /^[0-9]+-[0-9]+$/) {
    split(token, range, "-")
    return range[1] + 0 <= slot && slot <= range[2] + 0
  }
  return 0
}
$3 ~ /master/ {
  for (i=9; i<=NF; i++) if (owns($i, slot)) {
    address=$2
    sub(/@.*/, "", address)
    sub(/^.*:/, "", address)
    print address
    exit
  }
}')
owns_marker=false
[ "$marker_owner" = "$redis_port" ] && owns_marker=true
echo "reliability_marker_slot=$marker_slot reliability_marker_owner_port=$marker_owner queried_node_owns_reliability_marker=$owns_marker"

# The marker-free writer hashes 32 independent keys per minute. Resolve the
# current and previous minute onto this queried master so a pool enlarged by
# two colliding hot shards is distinguishable from a reconnect storm.
node_slot_tokens=$(redis-cli --raw -p "$redis_port" CLUSTER NODES 2>/dev/null | awk -v wanted="$redis_port" '
$3 ~ /master/ {
  address=$2
  sub(/@.*/, "", address)
  sub(/^.*:/, "", address)
  if (address == wanted) for (i=9; i<=NF; i++) if ($i !~ /^\[/) print $i
}')
slot_owned_by_queried_node() {
  wanted_slot=$1
  for token in $node_slot_tokens; do
    case "$token" in
      *-*) range_start=${token%%-*}; range_end=${token##*-} ;;
      *) range_start=$token; range_end=$token ;;
    esac
    if [ "$range_start" -le "$wanted_slot" ] 2>/dev/null &&
       [ "$wanted_slot" -le "$range_end" ] 2>/dev/null; then
      return 0
    fi
  done
  return 1
}
current_block=$(($(date +%%s) / 60))
previous_block=$((current_block - 1))
current_shards=0
previous_shards=0
for shard in $(seq 0 31); do
  current_slot=$(redis-cli --raw -p "$redis_port" CLUSTER KEYSLOT "client_reliability_stats.$current_block.$shard" 2>/dev/null)
  previous_slot=$(redis-cli --raw -p "$redis_port" CLUSTER KEYSLOT "client_reliability_stats.$previous_block.$shard" 2>/dev/null)
  slot_owned_by_queried_node "$current_slot" && current_shards=$((current_shards + 1))
  slot_owned_by_queried_node "$previous_slot" && previous_shards=$((previous_shards + 1))
done
echo "reliability_stats_current_block=$current_block current_reliability_shards_on_node=$current_shards previous_reliability_shards_on_node=$previous_shards reliability_shard_count=32"

redis-cli -p "$redis_port" CLIENT LIST 2>/dev/null | awk '
{
  source="-"; flags="-"; cmd="-"; lib="-"; idle=0; age=0
  for (i=1; i<=NF; i++) {
    split($i, value, "=")
    if (value[1] == "addr") { source=value[2]; sub(/:[0-9]+$/, "", source) }
    else if (value[1] == "flags") flags=value[2]
    else if (value[1] == "cmd") cmd=value[2]
    else if (value[1] == "lib-name") lib=value[2]
    else if (value[1] == "idle") idle=value[2]+0
    else if (value[1] == "age") age=value[2]+0
  }
  key="source=" source " flags=" flags " cmd=" cmd " lib=" lib
  count[key]++
  if (maxIdle[key] < idle) maxIdle[key]=idle
  if (maxAge[key] < age) maxAge[key]=age
}
END {
  for (key in count) print count[key], "max_idle_s=" maxIdle[key], "max_age_s=" maxAge[key], key
}' | sort -rn | head -20`, port)
	out, err := env.runner.shell(ctx, h, command)
	if err != nil {
		return fmt.Sprintf("node %d CLIENT LIST cohort aggregation failed: %s", port, err)
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Sprintf("node %d CLIENT LIST returned no cohorts", port)
	}
	return fmt.Sprintf("node %d CLIENT LIST top cohorts (count, idle/age, source, flags, last command, library):\n  %s",
		port, strings.ReplaceAll(strings.TrimSpace(out), "\n", "\n  "))
}

// logWindowBattery pulls the recent log window for a service around an
// incident (non-tail escalation read; the standing tailers are §3.7 of the
// design).
func logWindowBattery(ctx context.Context, env *probeEnv, service string, since time.Duration, query string) string {
	args := []string{"logs", env.cfg.env, service, "--since=" + since.String(), "--limit=200"}
	if query != "" {
		args = append(args, "--query="+query)
	}
	out, err := env.runner.warpctl(ctx, args...)
	if err != nil {
		return fmt.Sprintf("warpctl logs %s failed: %s", service, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 20 {
		lines = append(lines[:20], fmt.Sprintf("... (%d more)", len(lines)-20))
	}
	return strings.Join(lines, "\n")
}
