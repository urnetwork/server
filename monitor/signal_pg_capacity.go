package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog"
)

// SIGNALS.md §1.3a maps to signal_pg_capacity.go and
// signal_pg_capacity_test.go. The probe uses PostgreSQL's direct 5432 path so
// a saturated PgBouncer shard cannot hide the server's actual client ceiling.
func NewPgCapacitySignal() Signal {
	return &signalAdapter{
		number: "1.3a",
		key:    "pg-capacity",
		name:   "PostgreSQL client-slot capacity",
		probe:  pgCapacityProbe{},
	}
}

type pgCapacityProbe struct{}

const (
	pgCapacityWarnFraction = 0.75
	pgCapacityPageFraction = 0.90
	pgCapacityPageSlots    = 64
)

func (pgCapacityProbe) id() string             { return "pg/client-capacity" }
func (pgCapacityProbe) tier() string           { return tierPage }
func (pgCapacityProbe) cadence() time.Duration { return 30 * time.Second }

// pgCapacityQuery deliberately returns only aggregate state and ten bounded
// owner groups. It never exports query text, customer identifiers, or secrets.
// The current direct observation session remains in the count: it consumes a
// real slot and makes the headroom estimate conservative by one connection.
const pgCapacityQuery = `
	WITH settings AS MATERIALIZED (
		SELECT max(setting::int) FILTER (WHERE name = 'max_connections') AS max_connections,
		       coalesce(max(setting::int) FILTER (WHERE name = 'superuser_reserved_connections'), 0) AS super_reserved,
		       coalesce(max(setting::int) FILTER (WHERE name = 'reserved_connections'), 0) AS role_reserved
		FROM pg_settings
		WHERE name IN ('max_connections', 'superuser_reserved_connections', 'reserved_connections')
	), activity AS MATERIALIZED (
		SELECT coalesce(nullif(application_name, ''), '(unset)') AS application_name,
		       coalesce(nullif(usename, ''), '(unset)') AS role_name,
		       coalesce(client_addr::text, 'local') AS client_address,
		       coalesce(nullif(state, ''), '(unset)') AS connection_state,
		       wait_event_type,
		       wait_event,
		       state_change,
		       backend_start
		FROM pg_stat_activity
		WHERE backend_type = 'client backend'
	), summary AS (
		SELECT settings.max_connections,
		       settings.super_reserved,
		       settings.role_reserved,
		       settings.max_connections - settings.super_reserved - settings.role_reserved AS normal_ceiling,
		       count(*)::int AS total_clients,
		       count(*) FILTER (WHERE connection_state = 'active')::int AS active_clients,
		       count(*) FILTER (WHERE connection_state = 'idle')::int AS idle_clients,
		       count(*) FILTER (WHERE connection_state LIKE 'idle in transaction%')::int AS idle_in_tx_clients
		FROM settings
		CROSS JOIN activity
		GROUP BY settings.max_connections, settings.super_reserved, settings.role_reserved
	), owners AS (
		SELECT application_name, role_name, client_address, connection_state,
		       count(*)::int AS clients,
		       coalesce(string_agg(DISTINCT coalesce(wait_event_type, '-') || ':' || coalesce(wait_event, '-'), ','), '-') AS waits,
		       coalesce(round(extract(epoch FROM max(now() - state_change))), 0)::bigint AS oldest_state_s,
		       coalesce(round(extract(epoch FROM max(now() - backend_start))), 0)::bigint AS oldest_backend_s
		FROM activity
		GROUP BY application_name, role_name, client_address, connection_state
		ORDER BY clients DESC, application_name, role_name, client_address, connection_state
		LIMIT 10
	), output AS (
		SELECT 0 AS sort_key, 'summary'::text AS kind,
		       max_connections::text AS value_1,
		       super_reserved::text AS value_2,
		       role_reserved::text AS value_3,
		       normal_ceiling::text AS value_4,
		       total_clients::text AS value_5,
		       active_clients::text AS value_6,
		       idle_clients::text AS value_7,
		       idle_in_tx_clients::text AS value_8,
		       current_setting('work_mem')::text AS value_9,
		       current_setting('shared_buffers')::text AS value_10
		FROM summary
		UNION ALL
		SELECT 1, 'owner',
		       left(translate(application_name, E'|\n\r\t', '    '), 80),
		       left(translate(role_name, E'|\n\r\t', '    '), 80),
		       left(translate(client_address, E'|\n\r\t', '    '), 80),
		       left(translate(connection_state, E'|\n\r\t', '    '), 80),
		       clients::text,
		       left(translate(waits, E'|\n\r\t', '    '), 120),
		       oldest_state_s::text, oldest_backend_s::text, '', ''
		FROM owners
	)
	SELECT kind, value_1, value_2, value_3, value_4, value_5,
	       value_6, value_7, value_8, value_9, value_10
	FROM output
	ORDER BY sort_key, value_5::int DESC NULLS LAST, value_1;
`

type pgCapacityState struct {
	maxConnections int
	superReserved  int
	roleReserved   int
	normalCeiling  int
	totalClients   int
	activeClients  int
	idleClients    int
	idleInTx       int
	workMem        string
	sharedBuffers  string
}

type pgCapacityOwner struct {
	application string
	role        string
	address     string
	state       string
	clients     int
	waits       string
	oldestState int
	oldestAge   int
}

func parsePgCapacityNonnegative(row pgRow, column int, name string) (int, error) {
	value, err := strconv.Atoi(row.str(column))
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid PostgreSQL capacity %s %q", name, row.str(column))
	}
	return value, nil
}

func parsePgCapacityRows(rows []pgRow) (pgCapacityState, []pgCapacityOwner, error) {
	var state pgCapacityState
	owners := make([]pgCapacityOwner, 0, 10)
	foundSummary := false
	for _, row := range rows {
		if len(row) != 11 {
			return pgCapacityState{}, nil, fmt.Errorf("PostgreSQL capacity query returned %d columns, want 11", len(row))
		}
		switch row.str(0) {
		case "summary":
			if foundSummary {
				return pgCapacityState{}, nil, fmt.Errorf("PostgreSQL capacity query returned multiple summary rows")
			}
			foundSummary = true
			values := []*int{
				&state.maxConnections,
				&state.superReserved,
				&state.roleReserved,
				&state.normalCeiling,
				&state.totalClients,
				&state.activeClients,
				&state.idleClients,
				&state.idleInTx,
			}
			names := []string{"max_connections", "superuser_reserved_connections", "reserved_connections", "normal ceiling", "total clients", "active clients", "idle clients", "idle-in-transaction clients"}
			for i := range values {
				value, err := parsePgCapacityNonnegative(row, i+1, names[i])
				if err != nil {
					return pgCapacityState{}, nil, err
				}
				*values[i] = value
			}
			state.workMem = boundedPgCapacityLabel(row.str(9), 40)
			state.sharedBuffers = boundedPgCapacityLabel(row.str(10), 40)
		case "owner":
			clients, err := parsePgCapacityNonnegative(row, 5, "owner clients")
			if err != nil {
				return pgCapacityState{}, nil, err
			}
			oldestState, err := parsePgCapacityNonnegative(row, 7, "owner oldest state")
			if err != nil {
				return pgCapacityState{}, nil, err
			}
			oldestAge, err := parsePgCapacityNonnegative(row, 8, "owner oldest backend")
			if err != nil {
				return pgCapacityState{}, nil, err
			}
			owners = append(owners, pgCapacityOwner{
				application: boundedPgCapacityLabel(row.str(1), 80),
				role:        boundedPgCapacityLabel(row.str(2), 80),
				address:     boundedPgCapacityLabel(row.str(3), 80),
				state:       boundedPgCapacityLabel(row.str(4), 80),
				clients:     clients,
				waits:       boundedPgCapacityLabel(row.str(6), 120),
				oldestState: oldestState,
				oldestAge:   oldestAge,
			})
		default:
			return pgCapacityState{}, nil, fmt.Errorf("PostgreSQL capacity query returned unknown row kind %q", row.str(0))
		}
	}
	if !foundSummary {
		return pgCapacityState{}, nil, fmt.Errorf("PostgreSQL capacity query returned no summary row")
	}
	if state.maxConnections <= 0 || state.normalCeiling <= 0 ||
		state.normalCeiling != state.maxConnections-state.superReserved-state.roleReserved {
		return pgCapacityState{}, nil, fmt.Errorf(
			"invalid PostgreSQL capacity ceiling max=%d super_reserved=%d role_reserved=%d normal=%d",
			state.maxConnections, state.superReserved, state.roleReserved, state.normalCeiling,
		)
	}
	return state, owners, nil
}

func boundedPgCapacityLabel(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r == '|' || r == '\n' || r == '\r' || r == '\t' || r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, strings.TrimSpace(value))
	if value == "" {
		value = "(unset)"
	}
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

func postgresTooManyClients(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "too many clients already")
}

func pgCapacityEvidence(owners []pgCapacityOwner) string {
	if len(owners) == 0 {
		return "No owner groups were returned; preserve the aggregate result and repeat the direct observation."
	}
	lines := []string{"Top client-backend owner groups (bounded to ten; no query text):"}
	for _, owner := range owners {
		lines = append(lines, fmt.Sprintf(
			"application=%s role=%s address=%s state=%s clients=%d waits=%s oldest_state_s=%d oldest_backend_s=%d",
			owner.application, owner.role, owner.address, owner.state, owner.clients, owner.waits,
			owner.oldestState, owner.oldestAge,
		))
	}
	return strings.Join(lines, "\n")
}

func pgCapacityAction() string {
	return "First split active, young idle-in-transaction, idle, and starting owners. Correlate active and young idle-in-transaction cohorts with wait events and completed statement or COMMIT latency in PostgreSQL logs; after a client deadline, PgBouncer can discard an uncertain server session and open a replacement while the old backend unwinds. Compare PgBouncer logs, or SHOW POOLS where administrative access exists, across every active shard and with direct-maintenance owners. Remove the upstream database stall, transaction leak, or continuously retaining owner before changing the pool envelope. Do not infer a leak from a later idle recovery snapshot. Do not raise max_connections first, restart PostgreSQL/PgBouncer, or mass-terminate sessions: this host's large work_mem makes a blind slot increase a memory-risk change and termination can interrupt durable maintenance."
}

func (pgCapacityProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := pgTarget(env)
	rows, err := env.runner.pg(ctx, pgCapacityQuery)
	if err != nil {
		if !postgresTooManyClients(err) {
			return nil, err
		}
		return []finding{{
			probeId: "pg/client-capacity", tier: tierPage,
			class: "pg-client-capacity", target: target, sustain: 1,
			symptom:   fmt.Sprintf("PostgreSQL on %s rejected the direct capacity observation because no eligible client slot was available", target),
			mechanism: "PostgreSQL reached max_connections after accounting for reserved slots. PgBouncer's server_login_retry can cache and fan this rejection across application requests, but the direct 5432 rejection proves the server ceiling itself is exhausted at this instant.",
			baseline:  "Direct 5432 accepts the read-only observation and ordinary roles retain more than 25% normal-role connection headroom.",
			observed:  "direct_connection_result=too_many_clients_already server_login_retry_possible=true capacity_values=unavailable",
			evidence:  "The transport error contained PostgreSQL's canonical `too many clients already` result. Its remaining text is deliberately omitted because it can contain command framing rather than additional capacity evidence.",
			context:   "Application logs can repeat this one rejection as Unexpected error, route recovery, and goroutine-shaped JSON. Raw panic-class volume is diagnostic amplification, not a count of unique rejected PostgreSQL sessions. In the 2026-09-01 production control, repeatedly retried legacy concurrent reindex work caused WAL waits and 60-66-second COMMIT latency; application deadlines then lost client sessions while PgBouncer opened replacements. PostgreSQL did not restart, and the later idle cohort was recovery turnover rather than proof of an idle-session leak.",
			action:    pgCapacityAction(),
			verify:    "For ten minutes through the triggering workload, direct 5432 remains observable, ordinary-role headroom stays above 25%, completed COMMIT latency and WAL waits return to their ordinary band, and neither pg-client-capacity nor query_wait_timeout recurs.",
			playbook:  "SIGNALS.md §1.3a, §1.5, and §2.11",
		}}, nil
	}

	state, owners, err := parsePgCapacityRows(rows)
	if err != nil {
		return nil, err
	}
	remaining := state.normalCeiling - state.totalClients
	usedFraction := float64(state.totalClients) / float64(state.normalCeiling)
	observed := fmt.Sprintf(
		"max_connections=%d superuser_reserved_connections=%d reserved_connections=%d normal_role_ceiling=%d total_client_backends=%d normal_role_slots_remaining=%d normal_role_used_pct=%.1f active=%d idle=%d idle_in_tx=%d work_mem=%s shared_buffers=%s",
		state.maxConnections, state.superReserved, state.roleReserved, state.normalCeiling,
		state.totalClients, remaining, 100*usedFraction, state.activeClients, state.idleClients,
		state.idleInTx, state.workMem, state.sharedBuffers,
	)
	glog.Infof("[monitor]PostgreSQL client capacity on %s: %s\n", target, observed)
	glog.Infof("[monitor]PostgreSQL client capacity owners on %s:\n%s\n", target, pgCapacityEvidence(owners))

	severity := tierWarn
	sustain := 2
	if remaining <= pgCapacityPageSlots || usedFraction >= pgCapacityPageFraction {
		severity = tierPage
		sustain = 1
	}
	if usedFraction < pgCapacityWarnFraction {
		return []finding{healthyFinding("pg/client-capacity", tierPage, "pg-client-capacity", target)}, nil
	}
	return []finding{{
		probeId: "pg/client-capacity", tier: severity,
		class: "pg-client-capacity", target: target, sustain: sustain,
		symptom: fmt.Sprintf(
			"PostgreSQL on %s uses %.1f%% of the ordinary-role client ceiling (%d slots remain)",
			target, 100*usedFraction, remaining,
		),
		mechanism: "Ordinary application roles are admitted only below max_connections minus superuser_reserved_connections and reserved_connections. All existing client backends consume that threshold. Independent PgBouncer processes enforce local pools, so their aggregate server connections plus direct maintenance sessions can exhaust PostgreSQL even when each shard is individually within its own limit.",
		baseline:  "Ordinary roles retain more than 25% of the normal-role ceiling; active, idle, and idle-in-transaction owners are separately attributable.",
		observed:  observed,
		evidence:  pgCapacityEvidence(owners),
		context:   "The summary includes this read-only direct probe as one real client. Capacity is not query load: a high idle count and a high active count require different root-cause work. Application pg-client-capacity log volume is diagnostic amplification and can repeat several times per failed request. The 2026-09-01 production control tied the rejected-login wave to repeatedly retried legacy concurrent reindex work, WAL waits, 60-66-second COMMIT latency, client deadline loss, and replacement overlap; PostgreSQL did not restart, and a later idle recovery cohort did not establish retention as the cause.",
		action:    pgCapacityAction(),
		verify:    "For ten minutes through the triggering workload, ordinary-role headroom stays above 25%, direct 5432 remains observable, completed COMMIT latency and WAL waits return to their ordinary band, and neither pg-client-capacity nor query_wait_timeout recurs.",
		playbook:  "SIGNALS.md §1.3a, §1.5, and §2.11",
	}}, nil
}
