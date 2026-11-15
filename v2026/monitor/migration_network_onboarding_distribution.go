// The onboarding row's distribution column (server
// db_network_onboarding_distribution.go, mmm/ONBOARDING-RETENTION.md P6): the
// distribution each network's onboarding platform came with, which tells the
// App Store Mac from the direct-download Mac.
package monitor

import "github.com/urnetwork/server/v2026"

// One varchar(32) column on network_onboarding, not null with an empty-string
// default. The migration publishes no index or check. The default expression
// is the catalog's own text on PostgreSQL 18, as for 905's columns.
var networkOnboardingDistributionMigrationContract = migrationCurrentSuffix(server.NetworkOnboardingDistributionMigrationVersion(), "onboarding platform distribution column",
	snMainnetMigrationColumns("network_onboarding",
		snMainnetMigrationColumn{name: "distribution", kind: "character varying(32)", notNull: true, defaultExpression: "''::character varying"}))
