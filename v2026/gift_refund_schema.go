// What refunds, disputes and orphaned crypto payments did to a gift order's
// codes (mmm/GIFTOFPRIVACY.md §5 decision 16; model/gift_refund_model.go).
//
// gift_code_void holds one row per code that a refund, a dispute or an orphaned
// crypto payment voided (or, for a redeemed code, whose year it ended): the
// cause, its source (the refund or the dispute), and when. It is what lets a
// won dispute restore exactly the codes that dispute voided and the years it
// ended, never a refund's, and a crypto payment credited again restore exactly
// what its orphaning took. gift_order_refund holds each refund of an order
// once, by its id, with its amount: their sum is the order's cumulative
// refunded amount, which decides how many codes the refunds void, so a refund
// delivered again, or the next refund of the same charge, never voids twice.
//
// The tables are new and empty, each created with its index only when absent,
// under a five-second lock bound, so a failed lock wait is safe to retry.
// Nothing existing is touched. Apply before deploying the api and alt (the
// Stripe webhook's refunds and disputes) and the taskworker (the crypto
// reconcile and the operator's refund) that use them.
package server

// The migration's statement.
const GiftRefundSchemaSql = `
	DO $gift_refund$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		IF to_regclass('public.gift_code_void') IS NULL THEN
			CREATE TABLE gift_code_void (
				balance_code_id uuid NOT NULL,
				gift_order_id uuid NOT NULL,
				cause varchar(16) NOT NULL,
				source_id varchar(128) NOT NULL,
				ended_year boolean NOT NULL,
				void_time timestamp NOT NULL,
				PRIMARY KEY (balance_code_id)
			);
			CREATE INDEX gift_code_void_order ON gift_code_void (gift_order_id, cause, source_id);
		END IF;
		IF to_regclass('public.gift_order_refund') IS NULL THEN
			CREATE TABLE gift_order_refund (
				refund_id varchar(128) NOT NULL,
				gift_order_id uuid NOT NULL,
				amount_cents bigint NOT NULL,
				refund_time timestamp NOT NULL,
				PRIMARY KEY (refund_id)
			);
			CREATE INDEX gift_order_refund_order ON gift_order_refund (gift_order_id);
		END IF;
	END
	$gift_refund$
`

var giftRefundSchemaMigration = newSqlMigration(GiftRefundSchemaSql)

// The migration's version: its position in the list, from 1.
func GiftRefundSchemaVersion() int {
	for i, migration := range migrations {
		if migration == giftRefundSchemaMigration {
			return i + 1
		}
	}
	return 0
}
