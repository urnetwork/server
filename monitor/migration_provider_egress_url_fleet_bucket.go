// The fleet's URL outcomes by bucket of measured time (server
// db_provider_egress_url_fleet_bucket.go, model/provider_egress_url_overload.go)
// belong to the migration catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
var providerEgressUrlFleetBucketMigrationContract = migrationCurrentSuffix(
	server.ProviderEgressUrlFleetBucketVersion(),
	"provider URL outcome buckets of the fleet and their overload discount",
	migrationFp2Table("provider_egress_url_fleet_bucket"),
	snMainnetMigrationColumns("provider_egress_url_fleet_bucket",
		snMainnetMigrationColumn{name: "bucket_start", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "bucket_end", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "outcome_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "error_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "country_outcome_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "country_error_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "network_outcome_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "network_error_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "baseline_outcome_count", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "baseline_error_count", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "discounted", kind: "boolean", notNull: true},
		snMainnetMigrationColumn{name: "discounted_outcome_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "discounted_error_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "judge_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("provider_egress_url_fleet_bucket", "PRIMARY KEY (bucket_start)"))
