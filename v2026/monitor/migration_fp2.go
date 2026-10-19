// Exact catalog contracts for the URL-probe and ARIN rollout, without row scans.
package monitor

import (
	"fmt"
	"strings"
)

// Missing future relations yield empty evidence rather than failed casts.
const migrationFp2CatalogQuery = `fp2_column_artifact AS (
	SELECT relation.relname::text AS table_name, attribute.attname::text AS column_name,
		format_type(attribute.atttypid,attribute.atttypmod) AS data_type,
		attribute.attnotnull AS not_null, attribute.attgenerated::text AS generated,
		regexp_replace(pg_get_expr(defaults.adbin,defaults.adrelid),'[[:space:]]+',' ','g') AS default_expression
	FROM pg_attribute AS attribute
	JOIN pg_class AS relation ON relation.oid=attribute.attrelid
	JOIN pg_namespace AS namespace ON namespace.oid=relation.relnamespace
	LEFT JOIN pg_attrdef AS defaults ON defaults.adrelid=attribute.attrelid AND defaults.adnum=attribute.attnum
	WHERE namespace.nspname='public' AND attribute.attnum>0 AND NOT attribute.attisdropped
	AND relation.relname IN ('network_client_location','network_client_location_reliability',
		'provider_egress_health','provider_egress_health_history','provider_egress_probe_cycle','provider_egress_url_security',
		'provider_url_probe_run','client_reliability_running','client_reliability_running_window')
)`

// Expected fields are source-owned literals; SQL never incorporates live input.
type migrationFp2Column struct {
	name, kind, defaultExpression, generated string
	notNull                                  bool
}

// Match only the published columns so later additive migrations remain valid.
func migrationFp2Columns(table string, columns ...migrationFp2Column) string {
	guards := make([]string, 0, len(columns))
	for _, column := range columns {
		defaultExpression := "NULL"
		if column.defaultExpression != "" {
			defaultExpression = "'" + strings.ReplaceAll(column.defaultExpression, "'", "''") + "'"
		}
		guards = append(guards, fmt.Sprintf(`EXISTS (SELECT 1 FROM fp2_column_artifact
			WHERE table_name='%s' AND column_name='%s' AND data_type='%s'
			AND not_null=%t AND generated='%s' AND default_expression IS NOT DISTINCT FROM %s)`,
			table, column.name, column.kind, column.notNull, column.generated, defaultExpression))
	}
	return "(" + strings.Join(guards, " AND ") + ")"
}

// Complete definitions also pin key order, uniqueness, method, and predicate.
func migrationFp2Index(table, name, keys, predicate string, unique bool) string {
	definition := "CREATE "
	if unique {
		definition += "UNIQUE "
	}
	definition += "INDEX " + name + " ON public." + table + " USING btree (" + keys + ")"
	predicateSql := "NULL"
	if predicate != "" {
		definition += " WHERE " + predicate
		predicateSql = "'" + strings.ReplaceAll(predicate, "'", "''") + "'"
	}
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM index_artifact
		WHERE table_name = '%s' AND index_name = '%s' AND definition = '%s'
		AND predicate_definition IS NOT DISTINCT FROM %s AND indisvalid AND indisready)`,
		table, name, strings.ReplaceAll(definition, "'", "''"), predicateSql)
}

// Validated constraints cannot be replaced by similarly named no-op checks.
func migrationFp2Constraint(table, kind, definition string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM constraint_artifact
		WHERE table_name='%s' AND constraint_type='%s' AND definition='%s' AND validated)`,
		table, kind, strings.ReplaceAll(definition, "'", "''"))
}

// New evidence belongs in durable ordinary tables, not look-alike views.
func migrationFp2Table(table string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM pg_class WHERE oid=to_regclass('public.%s')
		AND relkind='r' AND relpersistence='p')`, table)
}

// The v729 replacement must retain the columns originally published at v726.
var migrationFp2UrlColumns = migrationFp2Columns("provider_egress_health_history",
	migrationFp2Column{name: "url_probe", kind: "boolean", notNull: true, defaultExpression: "false"}) + " AND " +
	migrationFp2Columns("provider_egress_probe_cycle",
		migrationFp2Column{name: "outcome_count", kind: "bigint", notNull: true, defaultExpression: "0"})

// v741 adds the measured-run quota lookup; the existing success projection is
// retained and must not be silently redefined during a rolling deployment.
var migrationUrlProbeMeasuredQuotaArtifactQuery = migrationFp2Index("provider_egress_health_history",
	"provider_egress_health_history_url_run", "client_id, measured_at DESC",
	"(url_probe AND (url_probe_policy_version = 1) AND (total_count = 1) AND ((ok_count = 0) OR (ok_count = 1)))", false)

// Order is the migration's positional row protocol: versions722 through730.
var migrationFp2ArtifactQueries = []string{
	// 722: both stored address facts and the serving rollup must exist.
	"(" + migrationFp2Columns("network_client_location",
		migrationFp2Column{name: "arin_risk", kind: "boolean", notNull: true, defaultExpression: "false"},
		migrationFp2Column{name: "arin_non_quality", kind: "boolean", notNull: true, defaultExpression: "false"}) + " AND " +
		migrationFp2Columns("network_client_location_reliability",
			migrationFp2Column{name: "arin_risk", kind: "boolean", notNull: true, defaultExpression: "false"},
			migrationFp2Column{name: "arin_non_quality", kind: "boolean", notNull: true, defaultExpression: "false"}) + ")",
	// 723: immutable receipt identity, bounded history reads, and durable queue.
	"(" + migrationFp2Table("provider_egress_health_history") + " AND " +
		migrationFp2Columns("provider_egress_health_history",
			migrationFp2Column{name: "run_id", kind: "uuid", notNull: true},
			migrationFp2Column{name: "client_id", kind: "uuid", notNull: true},
			migrationFp2Column{name: "measured_at", kind: "timestamp without time zone", notNull: true},
			migrationFp2Column{name: "ok_count", kind: "integer", notNull: true},
			migrationFp2Column{name: "total_count", kind: "integer", notNull: true},
			migrationFp2Column{name: "class_results", kind: "jsonb", notNull: true},
			migrationFp2Column{name: "tls_authentication_failure", kind: "boolean", notNull: true},
			migrationFp2Column{name: "cycle_started_at", kind: "timestamp without time zone"}) + " AND " +
		migrationFp2Constraint("provider_egress_health_history", "p", "PRIMARY KEY (run_id)") + " AND " +
		migrationFp2Constraint("provider_egress_health_history", "c", "CHECK ((ok_count >= 0))") + " AND " +
		migrationFp2Constraint("provider_egress_health_history", "c", "CHECK ((total_count >= ok_count))") + " AND " +
		migrationFp2Index("provider_egress_health_history", "provider_egress_health_history_pkey", "run_id", "", true) + " AND " +
		migrationFp2Index("provider_egress_health_history", "provider_egress_health_history_client_time", "client_id, measured_at", "", false) + " AND " +
		migrationFp2Index("provider_egress_health_history", "provider_egress_health_history_time", "measured_at", "", false) + " AND " +
		migrationFp2Table("provider_egress_probe_cycle") + " AND " +
		migrationFp2Columns("provider_egress_probe_cycle",
			migrationFp2Column{name: "client_id", kind: "uuid", notNull: true},
			migrationFp2Column{name: "cycle_started_at", kind: "timestamp without time zone", notNull: true},
			migrationFp2Column{name: "success_count", kind: "integer", notNull: true, defaultExpression: "0"},
			migrationFp2Column{name: "error_count", kind: "integer", notNull: true, defaultExpression: "0"},
			migrationFp2Column{name: "latest_result_at", kind: "timestamp without time zone"},
			migrationFp2Column{name: "next_attempt_at", kind: "timestamp without time zone", notNull: true}) + " AND " +
		migrationFp2Constraint("provider_egress_probe_cycle", "p", "PRIMARY KEY (client_id)") + " AND " +
		migrationFp2Constraint("provider_egress_probe_cycle", "c", "CHECK ((success_count >= 0))") + " AND " +
		migrationFp2Constraint("provider_egress_probe_cycle", "c", "CHECK ((error_count >= 0))") + " AND " +
		migrationFp2Index("provider_egress_probe_cycle", "provider_egress_probe_cycle_pkey", "client_id", "", true) + " AND " +
		migrationFp2Index("provider_egress_probe_cycle", "provider_egress_probe_cycle_next_attempt", "next_attempt_at, client_id", "", false) + ")",
	// 724: independent security ordering cannot inherit an implicit timestamp.
	migrationFp2Columns("provider_egress_health", migrationFp2Column{name: "security_measured_at", kind: "timestamp without time zone"}),
	// 725: the queue head must exclude ineligible rows through its partial index.
	"(" + migrationFp2Columns("provider_egress_probe_cycle", migrationFp2Column{name: "eligible", kind: "boolean", notNull: true, defaultExpression: "false"}) + " AND " +
		migrationFp2Index("provider_egress_probe_cycle", "provider_egress_probe_cycle_eligible_next_attempt", "next_attempt_at, client_id", "eligible", false) + ")",
	// 726: this exact pre-version predicate is superseded at version729.
	"(" + migrationFp2UrlColumns + " AND " + migrationFp2Index("provider_egress_health_history", "provider_egress_health_history_url_success",
		"client_id, measured_at DESC", "(url_probe AND (ok_count = 1))", false) + ")",
	// 727: per-URL security identity and retained unknown-target quarantine.
	"(" + migrationFp2Table("provider_egress_url_security") + " AND " +
		migrationFp2Columns("provider_egress_url_security",
			migrationFp2Column{name: "client_id", kind: "uuid", notNull: true},
			migrationFp2Column{name: "url_key", kind: "character varying(64)", notNull: true},
			migrationFp2Column{name: "destination", kind: "jsonb", notNull: true},
			migrationFp2Column{name: "measured_at", kind: "timestamp without time zone", notNull: true},
			migrationFp2Column{name: "tls_failure", kind: "boolean", notNull: true}) + " AND " +
		migrationFp2Constraint("provider_egress_url_security", "p", "PRIMARY KEY (client_id, url_key)") + " AND " +
		migrationFp2Index("provider_egress_url_security", "provider_egress_url_security_pkey", "client_id, url_key", "", true) + " AND " +
		migrationFp2Index("provider_egress_url_security", "provider_egress_url_security_unresolved", "client_id, url_key", "tls_failure", false) + " AND " +
		migrationFp2Columns("provider_egress_health", migrationFp2Column{name: "legacy_tls_authentication_failure", kind: "boolean", notNull: true, defaultExpression: "false"}) + " AND " +
		migrationFp2Columns("provider_egress_health_history", migrationFp2Column{name: "url_probe_evidence", kind: "jsonb"}) + ")",
	// 728: pin generated storage and the exact normalized modulo1024 expression.
	"(" + migrationFp2Columns("provider_egress_probe_cycle", migrationFp2Column{name: "slot_id", kind: "smallint", generated: "s",
		defaultExpression: "(((hashtext((client_id)::text) % 1024) + 1024) % 1024)"}) + " AND " +
		migrationFp2Index("provider_egress_probe_cycle", "provider_egress_probe_cycle_slot_next_attempt", "slot_id, next_attempt_at, client_id", "eligible", false) + ")",
	// 729: selected version must precede the bounded latest-success lookup.
	"(" + migrationFp2UrlColumns + " AND " +
		migrationFp2Columns("provider_egress_health_history", migrationFp2Column{name: "url_probe_policy_version", kind: "integer", notNull: true, defaultExpression: "0"}) + " AND " +
		migrationFp2Index("provider_egress_health_history", "provider_egress_health_history_url_success", "client_id, measured_at DESC",
			"(url_probe AND (url_probe_policy_version = 1) AND (ok_count = 1))", false) + ")",
	// 730: legacy false flags never fabricate a database-generation attestation.
	migrationFp2Columns("network_client_location",
		migrationFp2Column{name: "arin_lookup_at", kind: "timestamp without time zone"},
		migrationFp2Column{name: "arin_database_build_epoch", kind: "bigint", notNull: true, defaultExpression: "0"}),
}
