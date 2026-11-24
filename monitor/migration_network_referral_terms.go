package monitor

import "github.com/urnetwork/server"

// The referral terms' migration (server network_referral_terms_schema.go):
// the existing-link flag on each referral link, with false as the default of
// every link made after it, the referred networks' first link and
// qualification, and their app use per UTC day, versioned by its position.
var networkReferralTermsMigrationContract = migrationCurrentSuffix(server.NetworkReferralTermsSchemaMigrationVersion(), "referral terms: existing links, referred networks and their app use",
	snMainnetMigrationColumns("network_referral",
		snMainnetMigrationColumn{name: "existing_link", kind: "boolean", notNull: true, defaultExpression: "false"}),
	migrationFp2Table("network_referral_referee"),
	snMainnetMigrationColumns("network_referral_referee",
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "first_link_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "qualified_time", kind: "timestamp without time zone"}),
	snMainnetMigrationConstraints("network_referral_referee", "PRIMARY KEY (network_id)"),
	migrationFp2Table("network_referral_app_day"),
	snMainnetMigrationColumns("network_referral_app_day",
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "day", kind: "date", notNull: true},
		snMainnetMigrationColumn{name: "app_byte_count", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("network_referral_app_day", "PRIMARY KEY (network_id, day)"),
)
