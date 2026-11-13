// The SN25 extender pool's migration (server db_st_ledger_v2_extenders.go, sn
// WHITEPAPER.md §8.6): four new tables, and six nullable columns on
// st_ledger_v2_epoch_rows.
package monitor

import "github.com/urnetwork/server/v2026"

// The keys every per-epoch extender table leads with.
func stLedgerV2ExtenderMigrationEpochColumns(columns ...snMainnetMigrationColumn) []snMainnetMigrationColumn {
	return append([]snMainnetMigrationColumn{
		{name: "deployment_key", kind: "character varying(96)", notNull: true},
		{name: "no_id", kind: "bigint", notNull: true},
		{name: "epoch", kind: "bigint", notNull: true},
	}, columns...)
}

// Durable ordinary tables with their columns, keys and checks, and the six
// epoch row columns, nullable with no default, in the catalog's own text on
// PostgreSQL 18. A release from before the columns names the columns it
// writes, so it inserts on and leaves them null: an epoch the amendment does
// not cover. A default would give such an epoch an extender component, and a
// required column would refuse its rows. The platform endpoint checks keep a
// registration to sidecar's three edges, the kinds the signed amendment entry
// covers, named by who made it, and ended only after it began by who ended it.
// Follow the migration's position after merges instead of pinning a number.
var stLedgerV2ExtenderMigrationContract = migrationCurrentSuffix(server.StLedgerV2ExtenderMigrationVersion(), "settlement v2 extender pool",
	migrationFp2Table("st_amendments"),
	snMainnetMigrationColumns("st_amendments",
		snMainnetMigrationColumn{name: "deployment_key", kind: "character varying(96)", notNull: true},
		snMainnetMigrationColumn{name: "raw", kind: "bytea", notNull: true},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("st_amendments", "PRIMARY KEY (deployment_key)"),

	migrationFp2Table("st_ledger_v2_extender_shadow"),
	snMainnetMigrationColumns("st_ledger_v2_extender_shadow", stLedgerV2ExtenderMigrationEpochColumns(
		snMainnetMigrationColumn{name: "report_ref", kind: "character varying(71)", notNull: true},
		snMainnetMigrationColumn{name: "report", kind: "bytea", notNull: true},
		snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true})...),
	snMainnetMigrationConstraints("st_ledger_v2_extender_shadow",
		"PRIMARY KEY (deployment_key, no_id, epoch)", "CHECK ((no_id > 0))", "CHECK ((epoch > 0))"),

	migrationFp2Table("st_platform_endpoint"),
	snMainnetMigrationColumns("st_platform_endpoint",
		snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "kind", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "register_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "registered_by", kind: "character varying(256)", notNull: true},
		snMainnetMigrationColumn{name: "revoke_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "revoked_by", kind: "character varying(256)"}),
	snMainnetMigrationConstraints("st_platform_endpoint",
		"PRIMARY KEY (client_id, register_time)",
		"CHECK (((kind)::text = ANY ((ARRAY['tlsedge'::character varying, 'smtpedge'::character varying, 'mxedge'::character varying])::text[])))",
		"CHECK (((registered_by)::text <> ''::text))",
		"CHECK (((revoke_time IS NULL) OR (register_time < revoke_time)))",
		"CHECK (((revoke_time IS NULL) = (revoked_by IS NULL)))"),

	migrationFp2Table("st_ledger_v2_extender_census"),
	snMainnetMigrationColumns("st_ledger_v2_extender_census", stLedgerV2ExtenderMigrationEpochColumns(
		snMainnetMigrationColumn{name: "census", kind: "bytea", notNull: true},
		snMainnetMigrationColumn{name: "census_rows", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "capture_time", kind: "timestamp without time zone", notNull: true})...),
	snMainnetMigrationConstraints("st_ledger_v2_extender_census",
		"PRIMARY KEY (deployment_key, no_id, epoch)", "CHECK ((no_id > 0))", "CHECK ((epoch > 0))", "CHECK ((census_rows >= 0))"),

	snMainnetMigrationColumns("st_ledger_v2_epoch_rows",
		snMainnetMigrationColumn{name: "extender_bytes", kind: "numeric(20,0)"},
		snMainnetMigrationColumn{name: "extender_census_chunks", kind: "text[]"},
		snMainnetMigrationColumn{name: "extender_intra_platform_bytes", kind: "numeric(20,0)"},
		snMainnetMigrationColumn{name: "extender_intra_network_bytes", kind: "numeric(20,0)"},
		snMainnetMigrationColumn{name: "extender_contracts", kind: "bigint"},
		snMainnetMigrationColumn{name: "extender_payers", kind: "bigint"}),
)
