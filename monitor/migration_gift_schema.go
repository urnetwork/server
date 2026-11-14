// The gift of privacy's Pro gift codes (server gift_schema.go,
// mmm/GIFTOFPRIVACY.md §10.6) belong to the migration catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
// The code columns in the catalog's own text on PostgreSQL 18: without kind
// every redeem and check fails, and a different default would read the data
// codes sold before as gifts or as nothing; without the order keys one payment
// could fulfil two orders, or find none. The tables are durable ordinary
// tables keyed as published.
var giftSchemaMigrationContract = migrationCurrentSuffix(
	server.GiftSchemaVersion(),
	"gift orders and Pro gift codes",
	snMainnetMigrationColumns("transfer_balance_code",
		snMainnetMigrationColumn{name: "kind", kind: "character varying(16)", notNull: true, defaultExpression: "'data'::character varying"},
		snMainnetMigrationColumn{name: "pro_duration_seconds", kind: "bigint"},
		snMainnetMigrationColumn{name: "gift_order_id", kind: "uuid"},
		snMainnetMigrationColumn{name: "referrer_network_id", kind: "uuid"}),
	migrationFp2Table("gift_order"),
	snMainnetMigrationColumns("gift_order",
		snMainnetMigrationColumn{name: "gift_order_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "order_token_hash", kind: "bytea", notNull: true},
		snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "status", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "paid_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "refunded_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "quantity", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "unit_price_cents", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "total_cents", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "rail", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "stripe_session_id", kind: "character varying(128)"},
		snMainnetMigrationColumn{name: "crypto_intent_id", kind: "uuid"},
		snMainnetMigrationColumn{name: "purchase_event_base", kind: "character varying(128)"},
		snMainnetMigrationColumn{name: "redeem_by", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "delivery", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "buyer_email", kind: "character varying(256)", notNull: true},
		snMainnetMigrationColumn{name: "send_on", kind: "date"},
		snMainnetMigrationColumn{name: "send_timezone", kind: "character varying(64)", notNull: true},
		snMainnetMigrationColumn{name: "message", kind: "text", notNull: true},
		snMainnetMigrationColumn{name: "signature", kind: "text", notNull: true},
		snMainnetMigrationColumn{name: "language", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "season", kind: "character varying(64)", notNull: true},
		snMainnetMigrationColumn{name: "bittensor_footer", kind: "boolean", notNull: true},
		snMainnetMigrationColumn{name: "referrer_network_id", kind: "uuid"}),
	snMainnetMigrationConstraints("gift_order",
		"PRIMARY KEY (gift_order_id)",
		"UNIQUE (stripe_session_id)",
		"UNIQUE (crypto_intent_id)"),
	migrationFp2Table("gift_order_recipient"),
	snMainnetMigrationColumns("gift_order_recipient",
		snMainnetMigrationColumn{name: "gift_order_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "recipient_index", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "email", kind: "character varying(256)", notNull: true},
		snMainnetMigrationColumn{name: "balance_code_id", kind: "uuid"}),
	snMainnetMigrationConstraints("gift_order_recipient", "PRIMARY KEY (gift_order_id, recipient_index)"),
	migrationFp2Table("gift_order_pdf"),
	snMainnetMigrationColumns("gift_order_pdf",
		snMainnetMigrationColumn{name: "gift_order_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "part", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "pdf", kind: "bytea", notNull: true},
		snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("gift_order_pdf", "PRIMARY KEY (gift_order_id, part)"))
