// The onboarding results' purchase.started count (server
// onboarding_results_purchase_started_schema.go): the in-app offer's leading
// metric on onboarding_results_daily.
package monitor

import "github.com/urnetwork/server/v2026"

// One integer column, NOT NULL with a constant default. The contract pins the
// column alone: the table is the 648 artifact's. The default expression is the
// catalog's own text on PostgreSQL 18.
var onboardingResultsPurchaseStartedMigrationContract = migrationCurrentSuffix(server.OnboardingResultsPurchaseStartedMigrationVersion(), "onboarding results purchase started count",
	snMainnetMigrationColumns("onboarding_results_daily",
		snMainnetMigrationColumn{name: "purchase_started_7d", kind: "integer", notNull: true, defaultExpression: "0"}))
