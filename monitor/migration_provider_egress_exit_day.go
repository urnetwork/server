package monitor

import "github.com/urnetwork/server"

// The per client and day record of probed exits that differ from every
// network their provider connects from (server provider_egress_exit_day_schema.go),
// versioned by its position.
var providerEgressExitDayMigrationContract = migrationCurrentSuffix(server.ProviderEgressExitDayMigrationVersion(), "provider exits that differ from the networks they connect from, per client and day",
	migrationFp2Table("provider_egress_exit_day"),
	snMainnetMigrationColumns("provider_egress_exit_day",
		snMainnetMigrationColumn{name: "day", kind: "date", notNull: true},
		snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "differs_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "cross_family_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "first_observed_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "last_observed_at", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("provider_egress_exit_day", "PRIMARY KEY (day, client_id)"),
)
