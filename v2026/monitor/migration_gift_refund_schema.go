// What refunds, disputes and orphaned crypto payments did to a gift order's
// codes (server gift_refund_schema.go, model/gift_refund_model.go) belongs to
// the migration catalog.
package monitor

import "github.com/urnetwork/server/v2026"

// Follow the migration's position after merges instead of pinning a number.
// Without a void's cause and source a won dispute could restore a refund's
// voids, and without one row per code a code could be voided twice over;
// without one row per refund a refund delivered again would void more codes.
// The tables are durable ordinary tables keyed and indexed as published.
var giftRefundSchemaMigrationContract = migrationCurrentSuffix(
	server.GiftRefundSchemaVersion(),
	"gift code voids and order refunds",
	migrationFp2Table("gift_code_void"),
	snMainnetMigrationColumns("gift_code_void",
		snMainnetMigrationColumn{name: "balance_code_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "gift_order_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "cause", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "source_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "ended_year", kind: "boolean", notNull: true},
		snMainnetMigrationColumn{name: "void_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("gift_code_void", "PRIMARY KEY (balance_code_id)"),
	migrationFp2Index("gift_code_void", "gift_code_void_order", "gift_order_id, cause, source_id", "", false),
	migrationFp2Table("gift_order_refund"),
	snMainnetMigrationColumns("gift_order_refund",
		snMainnetMigrationColumn{name: "refund_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "gift_order_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "amount_cents", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "refund_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("gift_order_refund", "PRIMARY KEY (refund_id)"),
	migrationFp2Index("gift_order_refund", "gift_order_refund_order", "gift_order_id", "", false))
