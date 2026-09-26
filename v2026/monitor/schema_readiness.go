package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Only compile-time table/column/type names enter these contracts. This is a
// query-readiness check, not full migration or running-service attestation.
type probeSchemaColumn struct {
	table    string
	column   string
	dataType string
}

type probeSchemaContract struct {
	probeId          string
	class            string
	section          string
	minimumMigration int
	columns          []probeSchemaColumn
}

// Adds only fixed, paired column/type prerequisites to the owning contract.
func (self *probeSchemaContract) table(name string, columns ...string) {
	if len(columns)%2 != 0 {
		panic("invalid compiled probe schema")
	}
	for i := 0; i < len(columns); i += 2 {
		self.columns = append(self.columns, probeSchemaColumn{table: name, column: columns[i], dataType: columns[i+1]})
	}
}

// Keeps the shared current-provider eligibility prerequisites explicit.
func providerEligibilitySchema(self *probeSchemaContract) {
	self.table("network_client_location_reliability", "client_id", "uuid", "connected", "boolean", "valid", "boolean")
	self.table("network_client", "client_id", "uuid", "active", "boolean", "source_client_id", "uuid")
	self.table("provide_key", "client_id", "uuid", "provide_mode", "integer")
}

// Includes destination storage, both tallies and every secondary data query.
func egressSitePoolSchemaContract() probeSchemaContract {
	schema := probeSchemaContract{probeId: egressSitePoolProbeId, class: "egress-site-pool-schema-unavailable", section: "2.19b", minimumMigration: 713}
	schema.table("provider_egress_destination",
		"name", "character varying", "class", "character varying", "active", "boolean", "probation", "boolean",
		"retired_time", "timestamp without time zone", "retire_count", "integer",
		"above_retire_since", "timestamp without time zone", "failure_share", "real", "sample_count", "integer", "incompatible", "jsonb")
	schema.table("provider_egress_site_tally",
		"tally_day", "date", "name", "character varying", "country_code", "character varying", "region", "character varying",
		"load_count", "integer", "failure_count", "integer", "healthy_load_count", "integer", "healthy_failure_count", "integer")
	schema.table("provider_egress_place_tally",
		"tally_day", "date", "country_code", "character varying", "region", "character varying", "run_count", "integer", "echo_failure_count", "integer")
	schema.table("pending_task", "function_name", "character varying", "reschedule_error", "text", "reschedule_error_count", "integer", "run_at", "timestamp without time zone")
	providerEligibilitySchema(&schema)
	schema.table("provider_blackhole_check", "client_id", "uuid", "next_due_at", "timestamp without time zone", "consecutive_failures", "integer", "failure", "character varying")
	schema.table("provider_egress_health", "class_results", "jsonb", "measured_at", "timestamp without time zone")
	return schema
}

// Requires the derived table, hourly tally and scheduler/partition witnesses.
func derivedLocationsSchemaContract() probeSchemaContract {
	schema := probeSchemaContract{probeId: "pg/derived-locations", class: "derived-locations-schema-unavailable", section: "2.19c", minimumMigration: 716}
	schema.table("derived_location",
		"crossed_region", "boolean", "crossed_country", "boolean", "peer_count", "integer",
		"residual_km", "real", "reputation", "real", "update_time", "timestamp without time zone")
	schema.table("network_ping_hour_tally",
		"hour", "timestamp without time zone", "cosign", "smallint", "cosign_reason", "smallint", "relayed", "boolean",
		"ping_count", "bigint", "zero_rtt_count", "bigint", "beyond_half_planet_count", "bigint")
	schema.table("pending_task", "function_name", "character varying", "reschedule_error_count", "integer", "run_at", "timestamp without time zone")
	schema.table("network_extender", "active", "boolean")
	schema.table("network_client_connection", "client_id", "uuid", "connected", "boolean")
	schema.table("provide_key", "client_id", "uuid", "provide_mode", "integer")
	// Even nullable partition catalog reads require the relation to exist
	// before they can be interpreted. This witness does not impose partition
	// shape or read ping rows; those semantics remain in the existing reducer.
	schema.table("network_ping", "create_time", "timestamp without time zone")
	return schema
}

// Requires streak semantics; schema absence must not restore the legacy rule.
func hmacCutoverSchemaContract() probeSchemaContract {
	schema := probeSchemaContract{probeId: "pg/hmac-cutover", class: "hmac-cutover-schema-unavailable", section: "2.24", minimumMigration: 708}
	providerEligibilitySchema(&schema)
	schema.table("network_client", "network_id", "uuid", "description", "character varying")
	schema.table("provider_blackhole_check",
		"client_id", "uuid", "checked_at", "timestamp without time zone", "ok", "boolean",
		"failure", "character varying", "consecutive_failures", "integer", "first_failed_at", "timestamp without time zone")
	return schema
}

// PostgreSQL analyzes absent relation/column references before CASE executes.
// Query only metadata first, never optional data objects or strict regclass
// casts. Exact data types catch incompatible partial rollout/lookalike schemas.
// Defaults, constraints and the append-only head remain the job of §8.9.
func probeSchemaQuery(schema probeSchemaContract) string {
	if len(schema.columns) == 0 || len(schema.columns) > 96 {
		panic("invalid compiled probe schema size")
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	values := make([]string, 0, len(schema.columns))
	for _, column := range schema.columns {
		values = append(values, "("+quote(column.table)+", "+quote(column.column)+", "+quote(column.dataType)+")")
	}
	return "/* monitor-schema-readiness */\nSELECT NOT EXISTS (\n" +
		" SELECT 1 FROM (VALUES " + strings.Join(values, ",\n") + ") AS required(table_name, column_name, data_type)\n" +
		" WHERE NOT EXISTS (\n" +
		"  SELECT 1 FROM information_schema.columns AS actual\n" +
		"  WHERE actual.table_schema = 'public' AND actual.table_name = required.table_name\n" +
		"    AND actual.column_name = required.column_name AND actual.data_type = required.data_type\n" +
		" )\n);\n"
}

// Accepts one catalog boolean and preserves read, shape and cancellation errors.
func probeSchemaReady(ctx context.Context, env *probeEnv, schema probeSchemaContract) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	rows, err := env.runner.pg(ctx, probeSchemaQuery(schema))
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return false, fmt.Errorf("probe schema preflight returned an invalid shape")
	}
	ready, err := strconv.ParseBool(rows[0].str(0))
	if err != nil {
		return false, fmt.Errorf("probe schema preflight returned an invalid readiness value")
	}
	return ready, nil
}

// Reports readiness unknown without issuing or resolving dependent observations.
func probeSchemaUnavailableFinding(schema probeSchemaContract, target string) finding {
	return finding{
		probeId: schema.probeId, tier: tierWarn, class: schema.class, target: target, sustain: 1,
		symptom:   "The monitor cannot observe this signal because its required database schema is unavailable.",
		mechanism: "A bounded catalog-only preflight did not confirm all required public tables, columns and query-compatible types. The dependent data queries were not issued. The monitor can run ahead of a deliberately staged schema/service rollout; this is readiness unknown, not proof of a running service outage.",
		baseline:  "Every data-column prerequisite is visible before issuing the owning probe's unchanged data queries; full migration coherence and deployed artifacts are checked separately.",
		observed:  fmt.Sprintf("schema_ready=false signal_readiness=unknown required_through_migration=%d required_columns=%d dependent_data_queries=0", schema.minimumMigration, len(schema.columns)),
		evidence:  "One read-only metadata query returned false. No dependent table rows, Mimir counters or Redis history were read by this signal.",
		context:   "Missing schema, incompatible types or catalog visibility restrictions can produce this warning. A numeric migration head alone cannot prove object presence; conversely, column presence does not attest defaults, constraints, writers, running binaries or healthy data. No healthy finding for the dependent signal or legacy HMAC fallback is emitted, so absence cannot resolve an existing incident.",
		action:    "Correlate §8.9 physical artifacts and successful migration head with exact deployed artifacts and the approved rollout plan. Do not apply migrations merely to clear this warning, change verdict rules or infer a service outage from staged readiness.",
		verify:    "The physical preflight is ready and dependent PostgreSQL queries return validly. This clears only schema readiness; independent Mimir or Redis visibility may remain unknown, and no feature recovery is inferred. Preflight read errors, malformed output and cancellation still fail closed.",
		playbook:  "SIGNALS.md §" + schema.section + " and §8.9",
	}
}
