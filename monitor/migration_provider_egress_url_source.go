// The catalog source and probation state of each URL outcome, and the country
// sites' graduation evidence and state (server db_provider_egress_url_source.go,
// D3) belong to the migration catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
var providerEgressUrlSourceMigrationContract = migrationCurrentSuffix(
	server.ProviderEgressUrlSourceVersion(),
	"URL outcome catalog source and probation, and country site graduation",
	snMainnetMigrationColumns("provider_egress_health_history",
		snMainnetMigrationColumn{name: "url_source", kind: "smallint", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "url_probation", kind: "boolean", notNull: true, defaultExpression: "false"}),
	migrationFp2Table("provider_egress_country_site"),
	snMainnetMigrationColumns("provider_egress_country_site",
		snMainnetMigrationColumn{name: "name", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "country_code", kind: "character varying(2)", notNull: true},
		snMainnetMigrationColumn{name: "url", kind: "character varying(4096)", notNull: true},
		snMainnetMigrationColumn{name: "scored", kind: "boolean", notNull: true, defaultExpression: "false"},
		snMainnetMigrationColumn{name: "probation_since", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "graduated_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "demoted_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "demote_count", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("provider_egress_country_site", "PRIMARY KEY (name, country_code)"),
	migrationFp2Table("provider_egress_country_site_tally"),
	snMainnetMigrationColumns("provider_egress_country_site_tally",
		snMainnetMigrationColumn{name: "tally_day", kind: "date", notNull: true},
		snMainnetMigrationColumn{name: "name", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "country_code", kind: "character varying(2)", notNull: true},
		snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "paired_load_count", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "paired_ok_count", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "expected_ok_sum", kind: "double precision", notNull: true, defaultExpression: "0"}),
	snMainnetMigrationConstraints("provider_egress_country_site_tally", "PRIMARY KEY (tally_day, name, country_code, client_id)"),
	// each row is updated about six times a day: the free space keeps those
	// updates on their page
	migrationFillfactor("provider_egress_country_site_tally", 70))
