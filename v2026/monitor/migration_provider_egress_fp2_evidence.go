// Capability-aware FindProviders2's URL evidence columns (server
// db_provider_egress_fp2_evidence.go, FP2-PROTOCOL.md §4.1 and §12.3) belong
// to the migration catalog.
package monitor

import "github.com/urnetwork/server/v2026"

// Two smallint columns on provider_egress_health_history, not null with a
// constant 0 default: the failed later legs the ingest judged the provider's,
// and the post-quantum outcome. The migration publishes no index or check. The
// default expression is the catalog's own text on PostgreSQL 18, as for the
// URL source columns. Follow the migration's position after merges instead of
// pinning a number.
var providerEgressFp2EvidenceMigrationContract = migrationCurrentSuffix(
	server.ProviderEgressFp2EvidenceMigrationVersion(),
	"URL outcome failed later legs and post-quantum outcome",
	snMainnetMigrationColumns("provider_egress_health_history",
		snMainnetMigrationColumn{name: "failed_later_legs", kind: "smallint", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "pq_outcome", kind: "smallint", notNull: true, defaultExpression: "0"}))
