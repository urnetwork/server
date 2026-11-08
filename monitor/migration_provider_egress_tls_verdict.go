// The TLS finding verdict columns and the legacy flag's clearing columns
// (server db_provider_egress_tls_verdict.go, D1) belong to the migration
// catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
var providerEgressTlsVerdictMigrationContract = migrationCurrentSuffix(
	server.ProviderEgressTlsVerdictVersion(),
	"exact-URL TLS finding verdicts",
	snMainnetMigrationColumns("provider_egress_url_security",
		snMainnetMigrationColumn{name: "tls_verdict", kind: "smallint", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "tls_verdict_measured_at", kind: "timestamp without time zone"}),
	snMainnetMigrationColumns("provider_egress_health",
		snMainnetMigrationColumn{name: "legacy_tls_recheck_from", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "legacy_tls_cleared_at", kind: "timestamp without time zone"}))
