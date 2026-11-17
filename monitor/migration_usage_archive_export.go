// The usage archive's export ledger (server
// db_provider_usage_archive_export.go): one row per verified copy of a week of
// st_provider_usage_archive stored off the primary, which the exporter writes
// and an early drop of the week stamps.
package monitor

import "github.com/urnetwork/server"

// A durable ordinary table keyed by the export, unique by the stored file's
// SHA-256, with no defaults, and the leaf's exports by row count through the
// second index.
var usageArchiveExportLedgerMigrationContract = migrationCurrentSuffix(server.ProviderUsageArchiveExportLedgerMigrationVersion(), "usage archive export ledger",
	migrationFp2Table("st_provider_usage_archive_export"),
	snMainnetMigrationColumns("st_provider_usage_archive_export",
		snMainnetMigrationColumn{name: "export_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "week_start", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "leaf_name", kind: "character varying(64)", notNull: true},
		snMainnetMigrationColumn{name: "lower_contract_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "upper_contract_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "row_count", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "null_close_count", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "min_contract_id", kind: "uuid"},
		snMainnetMigrationColumn{name: "max_contract_id", kind: "uuid"},
		snMainnetMigrationColumn{name: "min_close_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "max_close_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "row_digest", kind: "numeric", notNull: true},
		snMainnetMigrationColumn{name: "copy_sha256", kind: "character(64)", notNull: true},
		snMainnetMigrationColumn{name: "copy_bytes", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "file_name", kind: "text", notNull: true},
		snMainnetMigrationColumn{name: "file_sha256", kind: "character(64)", notNull: true},
		snMainnetMigrationColumn{name: "file_bytes", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "location", kind: "text", notNull: true},
		snMainnetMigrationColumn{name: "export_host", kind: "text", notNull: true},
		snMainnetMigrationColumn{name: "snapshot_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "verified_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "dropped_time", kind: "timestamp without time zone"}),
	snMainnetMigrationConstraints("st_provider_usage_archive_export",
		"PRIMARY KEY (export_id)",
		"UNIQUE (file_sha256)"),
	migrationFp2Index("st_provider_usage_archive_export", "st_provider_usage_archive_export_leaf", "leaf_name, row_count", "", false))
