// The demand deposit's durable state (server st_demand_schema.go, sn/demand).
package monitor

import "github.com/urnetwork/server"

// Durable ordinary tables, each keyed from its first column and without column
// defaults: the funder's state, its events in time order through the one index
// beyond the keys, the last verified price sheet, and each settlement v2 epoch's
// usage statement, whose statement columns stay NULL until its rows are recorded.
var stDemandMigrationContract = migrationCurrentSuffix(server.StDemandSchemaMigrationVersion(), "demand deposit funder state, price sheet and epoch usage",
	migrationFp2Table("st_demand_state"),
	snMainnetMigrationColumns("st_demand_state",
		snMainnetMigrationColumn{name: "deployment_key", kind: "character varying(96)", notNull: true},
		snMainnetMigrationColumn{name: "version", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "state", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("st_demand_state", "PRIMARY KEY (deployment_key)"),
	migrationFp2Table("st_demand_event"),
	snMainnetMigrationColumns("st_demand_event",
		snMainnetMigrationColumn{name: "event_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "deployment_key", kind: "character varying(96)", notNull: true},
		snMainnetMigrationColumn{name: "event_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "epoch", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "kind", kind: "character varying(32)", notNull: true},
		snMainnetMigrationColumn{name: "data", kind: "jsonb", notNull: true}),
	snMainnetMigrationConstraints("st_demand_event", "PRIMARY KEY (event_id)"),
	migrationFp2Index("st_demand_event", "st_demand_event_deployment_time", "deployment_key, event_time", "", false),
	migrationFp2Table("st_demand_price_sheet"),
	snMainnetMigrationColumns("st_demand_price_sheet",
		snMainnetMigrationColumn{name: "deployment_key", kind: "character varying(96)", notNull: true},
		snMainnetMigrationColumn{name: "raw", kind: "bytea", notNull: true},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("st_demand_price_sheet", "PRIMARY KEY (deployment_key)"),
	migrationFp2Table("st_ledger_v2_epoch_usage"),
	snMainnetMigrationColumns("st_ledger_v2_epoch_usage",
		snMainnetMigrationColumn{name: "deployment_key", kind: "character varying(96)", notNull: true},
		snMainnetMigrationColumn{name: "no_id", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "epoch", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "total_users", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "users_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "total_usage_bytes", kind: "numeric(20,0)"},
		snMainnetMigrationColumn{name: "provider_rows", kind: "bigint"},
		snMainnetMigrationColumn{name: "content_hash", kind: "character varying(80)"},
		snMainnetMigrationColumn{name: "statement", kind: "bytea"},
		snMainnetMigrationColumn{name: "statement_time", kind: "timestamp without time zone"}),
	snMainnetMigrationConstraints("st_ledger_v2_epoch_usage",
		"CHECK ((epoch > 0))",
		"CHECK ((no_id > 0))",
		"CHECK ((total_users >= 0))",
		"PRIMARY KEY (deployment_key, no_id, epoch)"))
