package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog"
)

// SIGNALS.md §1.3b maps to signal_pool_retention.go and
// signal_pool_retention_test.go. It measures PostgreSQL's local pool-owned
// reserve directly; configuration deployment remains an xops responsibility.
func NewPoolRetentionSignal() Signal {
	return &signalAdapter{
		number: "1.3b",
		key:    "pool-retention",
		name:   "PgBouncer idle-backend retention",
		probe:  poolRetentionProbe{},
	}
}

type poolRetentionProbe struct{}

const (
	poolRetentionWarnFraction = 0.50
	poolRetentionIdleSeconds  = 600
)

func (poolRetentionProbe) id() string             { return "pg/pool-retention" }
func (poolRetentionProbe) tier() string           { return tierWarn }
func (poolRetentionProbe) cadence() time.Duration { return 30 * time.Second }

// poolRetentionQuery deliberately exports no SQL text, PIDs, customer
// identifiers, or credentials. Loopback client backends are candidates rather
// than assumed PgBouncer owners; the action requires a host socket census to
// prove that final attribution.
const poolRetentionQuery = `
	WITH settings AS MATERIALIZED (
		SELECT max(setting::int) FILTER (WHERE name = 'max_connections')
		           - coalesce(max(setting::int) FILTER (WHERE name = 'superuser_reserved_connections'), 0)
		           - coalesce(max(setting::int) FILTER (WHERE name = 'reserved_connections'), 0)
		           AS normal_ceiling
		FROM pg_settings
		WHERE name IN ('max_connections', 'superuser_reserved_connections', 'reserved_connections')
	), activity AS MATERIALIZED (
		SELECT state, state_change,
		       (client_addr <<= inet '127.0.0.0/8' OR client_addr = inet '::1') AS loopback
		FROM pg_stat_activity
		WHERE backend_type = 'client backend'
	)
	SELECT settings.normal_ceiling::text,
	       count(*)::text,
	       count(*) FILTER (WHERE loopback)::text,
	       count(*) FILTER (WHERE loopback AND state = 'idle')::text,
	       count(*) FILTER (WHERE loopback AND state = 'active')::text,
	       count(*) FILTER (WHERE loopback AND state LIKE 'idle in transaction%')::text,
	       count(*) FILTER (
	           WHERE loopback AND state = 'idle'
	             AND state_change <= now() - interval '600 seconds'
	       )::text,
	       coalesce(extract(epoch FROM max(now() - state_change) FILTER (
	           WHERE loopback AND state = 'idle'
	       )), 0)::bigint::text
	FROM settings
	CROSS JOIN activity
	GROUP BY settings.normal_ceiling;
`

type poolRetentionState struct {
	normalCeiling int
	totalClients  int
	localClients  int
	localIdle     int
	localActive   int
	localIdleTx   int
	agedIdle      int
	oldestIdleSec int
}

func parsePoolRetentionRows(rows []pgRow) (poolRetentionState, error) {
	if len(rows) != 1 {
		return poolRetentionState{}, fmt.Errorf("PostgreSQL pool-retention query returned %d rows, want 1", len(rows))
	}
	if len(rows[0]) != 8 {
		return poolRetentionState{}, fmt.Errorf("PostgreSQL pool-retention query returned %d columns, want 8", len(rows[0]))
	}
	values := make([]int, 8)
	names := []string{
		"normal ceiling", "total clients", "loopback clients", "loopback idle",
		"loopback active", "loopback idle-in-transaction", "aged loopback idle", "oldest loopback idle",
	}
	for i, name := range names {
		value, err := strconv.Atoi(rows[0].str(i))
		if err != nil || value < 0 {
			return poolRetentionState{}, fmt.Errorf("invalid PostgreSQL pool-retention %s %q", name, rows[0].str(i))
		}
		values[i] = value
	}
	state := poolRetentionState{
		normalCeiling: values[0],
		totalClients:  values[1],
		localClients:  values[2],
		localIdle:     values[3],
		localActive:   values[4],
		localIdleTx:   values[5],
		agedIdle:      values[6],
		oldestIdleSec: values[7],
	}
	if state.normalCeiling <= 0 || state.localClients > state.totalClients ||
		state.localIdle+state.localActive+state.localIdleTx > state.localClients ||
		state.agedIdle > state.localIdle {
		return poolRetentionState{}, fmt.Errorf("inconsistent PostgreSQL pool-retention summary: %+v", state)
	}
	return state, nil
}

func poolRetentionContext(state poolRetentionState) string {
	aged := fmt.Sprintf(
		"%d loopback idle backend(s) have remained continuously idle for at least %d seconds.",
		state.agedIdle, poolRetentionIdleSeconds,
	)
	if state.agedIdle == 0 {
		aged = fmt.Sprintf(
			"No loopback idle backend in this snapshot is yet continuously idle for %d seconds; that distinguishes a young post-peak or recurring-demand cohort from proved long-idle retention, but it does not restore the consumed admission reserve.",
			poolRetentionIdleSeconds,
		)
	}
	return strings.Join([]string{
		aged,
		"Loopback is an attribution candidate, not an assumption: confirm owners with one privileged socket census or PgBouncer SHOW POOLS before changing the envelope.",
		"The 2026-09-01 production control found 32 live PgBouncer shards, every live file set to default_pool_size=20, min_pool_size=8, server_lifetime=3600, and server_idle_timeout=0, with 16-20 established PostgreSQL backends per shard and 608 total. Zero disables idle draining; 32*20 permits 640 retained server connections while the warm floor is 32*8=256.",
		"Xops commit 31ae1e7 sets server_idle_timeout=600 through the consolidated run-dbs.sh --pgbouncer-only path; there is no separate run-pgbouncer.sh.",
		"The 2026-09-02 production controls found all 32 live files already set to 600 and observed two fleet-wide refill cohorts drain near that boundary. In the later control, loopback clients fell from 589 to 366 and total client backends from 684 to 461 in 32 seconds as the oldest idle cohort advanced from 574 to 593 seconds; the following sample's oldest age reset to 462 seconds. A direct systemd census then found all 32 PgBouncer units still active with their August 9 process starts and NRestarts=0. That proves active timeout draining for the current configuration without a reload, restart, or session termination.",
		"A young refill must first receive one complete idle-timeout interval before another deployment or pool-size change is justified.",
	}, " ")
}

func poolRetentionMechanism(state poolRetentionState) string {
	base := "Independent transaction-pool processes each enforce a per-process server pool. Their aggregate idle server connections still consume PostgreSQL admission slots. A zero server_idle_timeout can retain peak occupancy until server_lifetime, while a nonzero timeout should contract each continuously unused server connection toward the configured warm floor."
	if state.agedIdle == 0 {
		return fmt.Sprintf(
			"%s No loopback idle backend in this snapshot has yet crossed the %d-second drain interval, so the observation is compatible with an expected young refill or recurring demand and does not prove that idle draining is disabled.",
			base, poolRetentionIdleSeconds,
		)
	}
	return fmt.Sprintf(
		"%s The %d loopback idle backend(s) continuously idle beyond the %d-second drain interval require owner and live-setting attribution: after that proof they can expose disabled, ineffective, or mismatched draining, but loopback age alone does not select among those causes.",
		base, state.agedIdle, poolRetentionIdleSeconds,
	)
}

func (poolRetentionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, poolRetentionQuery)
	if err != nil {
		return nil, err
	}
	state, err := parsePoolRetentionRows(rows)
	if err != nil {
		return nil, err
	}
	idleFraction := float64(state.localIdle) / float64(state.normalCeiling)
	observed := fmt.Sprintf(
		"normal_role_ceiling=%d total_client_backends=%d loopback_clients=%d loopback_idle=%d loopback_active=%d loopback_idle_in_tx=%d loopback_idle_at_least_%ds=%d oldest_loopback_idle_s=%d loopback_idle_ceiling_pct=%.1f",
		state.normalCeiling, state.totalClients, state.localClients, state.localIdle,
		state.localActive, state.localIdleTx, poolRetentionIdleSeconds, state.agedIdle,
		state.oldestIdleSec, 100*idleFraction,
	)
	target := pgTarget(env)
	glog.Infof("[monitor]PostgreSQL local pool retention on %s: %s\n", target, observed)
	if idleFraction < poolRetentionWarnFraction {
		return []finding{healthyFinding("pg/pool-retention", tierWarn, "pgbouncer-idle-retention", target)}, nil
	}
	return []finding{{
		probeId: "pg/pool-retention", tier: tierWarn,
		class: "pgbouncer-idle-retention", target: target, sustain: 10,
		symptom: fmt.Sprintf(
			"%d idle loopback backends consume %.1f%% of PostgreSQL's ordinary-role ceiling",
			state.localIdle, 100*idleFraction,
		),
		mechanism: poolRetentionMechanism(state),
		baseline:  "Idle loopback backends consume less than 50% of the ordinary-role ceiling, excess pools drain toward their configured warm minimum after 600 idle seconds, and §1.3a retains more than 25% total headroom.",
		observed:  observed,
		evidence: fmt.Sprintf(
			"loopback_idle=%d of local_clients=%d; aged_idle=%d; oldest_idle_s=%d; total_clients=%d of normal_ceiling=%d",
			state.localIdle, state.localClients, state.agedIdle, state.oldestIdleSec,
			state.totalClients, state.normalCeiling,
		),
		context:  poolRetentionContext(state),
		action:   "First read the selected pool settings from every live shard and take one privileged socket census; do not infer PgBouncer ownership from loopback alone. If server_idle_timeout is zero, after protected database maintenance is empty and with explicit operational authorization, deploy a clean Xops descendant of 31ae1e7 with xops/main/ansible/run-dbs.sh --pgbouncer-only; it reloads only changed PgBouncer units and requires their PIDs to remain unchanged. If 600 is already effective and no idle backend is yet 600 seconds old, make no change and observe one complete timeout interval. If the reserve remains consumed after that interval, use SHOW POOLS/STATS and wait metrics to bound default_pool_size, or add database hardware as part of an explicit connection-and-memory budget. Do not restart PostgreSQL/PgBouncer, terminate sessions, or raise max_connections to silence this alert.",
		verify:   "All 32 live shard files report server_idle_timeout=600; the isolated deployment proves every PgBouncer PID is unchanged across reload; after more than 600 seconds outside a demand peak, excess pools contract toward the 256-connection warm floor, ordinary-role headroom stays above 25% for ten minutes, and neither query_wait_timeout nor rejected server login recurs.",
		playbook: "SIGNALS.md §1.3b and §1.3a",
	}}, nil
}
