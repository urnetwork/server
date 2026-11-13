// The Shopify orders paid with Solana Pay into our USDC receivers
// (controller/solana_shopify_order.go, model/solana_shopify_order_model.go):
// a payment the Helius webhook or the Solana reconcile's receiver leg
// recognized, by its Shopify order record, as a URnode order of our Shopify
// store, kept here instead of as an unfulfilled payment, so it is never
// credited and never pages. One payment per signature, and one per order.
//
// The migration only creates the table, empty: no existing table is locked
// or rewritten. The five-second lock bound keeps a catalog wait from stalling
// behind a long reader, and a table that exists already is left as it is, so
// a failed lock wait is safe to retry. Old binaries never read it. Apply
// before deploying the api and alt (the webhook) and the taskworker (the
// reconcile) that write and read it.
package server

// The migration's statement.
const SolanaShopifyOrderPaymentSql = `
	DO $shopify_order$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		IF to_regclass('public.solana_shopify_order_payment') IS NULL THEN
			CREATE TABLE solana_shopify_order_payment (
				tx_signature varchar(128) NOT NULL,
				order_id varchar(128) NOT NULL,
				order_name varchar(64) NOT NULL,
				payment_id varchar(128) NOT NULL,
				received_usd double precision NOT NULL,
				order_total_usd double precision NOT NULL,
				transaction_time timestamp NULL,
				leg varchar(16) NOT NULL,
				record_time timestamp NOT NULL,
				PRIMARY KEY (tx_signature),
				UNIQUE (order_id)
			);
		END IF;
	END
	$shopify_order$
`

var solanaShopifyOrderPaymentMigration = newSqlMigration(SolanaShopifyOrderPaymentSql)

// The migration's version: its position in the list, from 1.
func SolanaShopifyOrderPaymentVersion() int {
	for i, migration := range migrations {
		if migration == solanaShopifyOrderPaymentMigration {
			return i + 1
		}
	}
	return 0
}
