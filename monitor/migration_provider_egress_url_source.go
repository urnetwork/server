// The catalog source and probation state of each URL outcome (server
// db_provider_egress_url_source.go, D3) belong to the migration catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
var providerEgressUrlSourceMigrationContract = migrationCurrentSuffix(
	server.ProviderEgressUrlSourceVersion(),
	"URL outcome catalog source and probation",
	snMainnetMigrationColumns("provider_egress_health_history",
		snMainnetMigrationColumn{name: "url_source", kind: "smallint", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "url_probation", kind: "boolean", notNull: true, defaultExpression: "false"}))
