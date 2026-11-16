// The HTTP/3 place evidence (server db_provider_egress_h3_place.go,
// model/provider_egress_h3_place.go) belongs to the migration catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
var providerEgressH3PlaceMigrationContract = migrationCurrentSuffix(
	server.ProviderEgressH3PlaceVersion(),
	"provider HTTP/3 place evidence",
	migrationFp2Table("provider_egress_h3_place"),
	snMainnetMigrationColumns("provider_egress_h3_place",
		snMainnetMigrationColumn{name: "name", kind: "character varying(256)", notNull: true},
		snMainnetMigrationColumn{name: "country_code", kind: "character varying(2)", notNull: true},
		snMainnetMigrationColumn{name: "tally_day", kind: "date", notNull: true},
		snMainnetMigrationColumn{name: "network_ids", kind: "uuid[]", notNull: true, defaultExpression: "'{}'::uuid[]"},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("provider_egress_h3_place", "PRIMARY KEY (name, country_code, tally_day)"))
