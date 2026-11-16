// The Shopify orders paid with Solana Pay (server
// solana_shopify_order_schema.go, controller/solana_shopify_order.go) belong
// to the migration catalog.
package monitor

import "github.com/urnetwork/server/v2026"

// Follow the migration's position after merges instead of pinning a number.
var solanaShopifyOrderPaymentMigrationContract = migrationCurrentSuffix(
	server.SolanaShopifyOrderPaymentVersion(),
	"Solana Shopify order payment",
	migrationFp2Table("solana_shopify_order_payment"),
	snMainnetMigrationColumns("solana_shopify_order_payment",
		snMainnetMigrationColumn{name: "tx_signature", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "order_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "order_name", kind: "character varying(64)", notNull: true},
		snMainnetMigrationColumn{name: "payment_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "received_usd", kind: "double precision", notNull: true},
		snMainnetMigrationColumn{name: "order_total_usd", kind: "double precision", notNull: true},
		snMainnetMigrationColumn{name: "transaction_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "leg", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "record_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("solana_shopify_order_payment", "PRIMARY KEY (tx_signature)", "UNIQUE (order_id)"))
