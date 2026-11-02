// The re-engagement program's win-back offers (server
// network_winback_offer_schema.go, mmm/reengagement/PLAN.md "THE STORES" 6).
package monitor

import "github.com/urnetwork/server"

// A durable ordinary table keyed by the offer, with eight NOT NULL columns
// and no defaults, the percent bounded by its check, and the network's offers
// in time order through the second index. The check and the index definition
// are the catalog's own text on PostgreSQL 18.
var networkWinbackOfferMigrationContract = migrationCurrentSuffix(server.NetworkWinbackOfferSchemaMigrationVersion(), "re-engagement win-back offers",
	migrationFp2Table("network_winback_offer"),
	snMainnetMigrationColumns("network_winback_offer",
		snMainnetMigrationColumn{name: "offer_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "store", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "plan", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "percent_off", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "issued_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "expires_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "redeemed_at", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "stripe_promotion_code_id", kind: "character varying(128)"},
		snMainnetMigrationColumn{name: "play_offer_tag", kind: "character varying(64)"},
		snMainnetMigrationColumn{name: "apple_offer_id", kind: "character varying(128)"}),
	snMainnetMigrationConstraints("network_winback_offer",
		"PRIMARY KEY (offer_id)",
		"CHECK (((0 < percent_off) AND (percent_off < 100)))"),
	migrationFp2Index("network_winback_offer", "network_winback_offer_network_issued", "network_id, issued_at", "", false))
