// The gift of privacy's Pro gift codes (mmm/GIFTOFPRIVACY.md §10.3, §10.6;
// model/gift_order_model.go, model/gift_redeem_model.go).
//
// A gift code is a transfer_balance_code row of kind 'pro_gift' (every code
// before is 'data', the default), granting pro_duration_seconds of Pro from its
// redemption, made by its gift_order and carrying the giver's network
// (referrer_network_id) when the giver earns referral credit. gift_order is one
// purchase of up to gift.max_quantity codes: pending until paid, keyed to its
// Stripe Checkout session or crypto invoice (each unique, so a payment finds
// one order), and on payment its purchase event base and the codes' redeem-by.
// The buyer's order token, which opens the order's status and cards, is kept
// only as its SHA-256 (order_token_hash).
// gift_order_recipient holds each friend's address and the code sent to it, in
// code order; gift_order_pdf the rendered cards, every card in part 'all' and
// one card per recipient in part 'r<index>'. Kinds, statuses, rails and
// deliveries are varchar, never ENUM.
//
// The migration adds four columns to transfer_balance_code: three nullable
// with no default and kind with a constant default, which PostgreSQL keeps in
// the catalog, so no row is rewritten or scanned. The five-second lock bound
// keeps their brief catalog lock from stalling behind a long reader of the
// codes. The three tables are new and empty, each created only when absent, and
// the columns are added only when absent, so a failed lock wait is safe to
// retry. Old binaries name the columns they read and write, so they neither see
// nor need the new ones. Apply before deploying the api and alt (redeem, check,
// checkout, the Stripe webhook) and the taskworker (fulfilment, cards, mail)
// that use them.
package server

// The migration's statement.
const GiftSchemaSql = `
	DO $gift$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		ALTER TABLE transfer_balance_code
			ADD COLUMN IF NOT EXISTS kind varchar(16) NOT NULL DEFAULT 'data',
			ADD COLUMN IF NOT EXISTS pro_duration_seconds bigint NULL,
			ADD COLUMN IF NOT EXISTS gift_order_id uuid NULL,
			ADD COLUMN IF NOT EXISTS referrer_network_id uuid NULL;
		IF to_regclass('public.gift_order') IS NULL THEN
			CREATE TABLE gift_order (
				gift_order_id uuid NOT NULL,
				order_token_hash bytea NOT NULL,
				create_time timestamp NOT NULL,
				status varchar(16) NOT NULL,
				paid_time timestamp NULL,
				refunded_time timestamp NULL,
				quantity integer NOT NULL,
				unit_price_cents bigint NOT NULL,
				total_cents bigint NOT NULL,
				rail varchar(16) NOT NULL,
				stripe_session_id varchar(128) NULL,
				crypto_intent_id uuid NULL,
				purchase_event_base varchar(128) NULL,
				redeem_by timestamp NULL,
				delivery varchar(16) NOT NULL,
				buyer_email varchar(256) NOT NULL,
				send_on date NULL,
				send_timezone varchar(64) NOT NULL,
				message text NOT NULL,
				signature text NOT NULL,
				language varchar(16) NOT NULL,
				season varchar(64) NOT NULL,
				bittensor_footer boolean NOT NULL,
				referrer_network_id uuid NULL,
				PRIMARY KEY (gift_order_id),
				UNIQUE (stripe_session_id),
				UNIQUE (crypto_intent_id)
			);
		END IF;
		IF to_regclass('public.gift_order_recipient') IS NULL THEN
			CREATE TABLE gift_order_recipient (
				gift_order_id uuid NOT NULL,
				recipient_index integer NOT NULL,
				email varchar(256) NOT NULL,
				balance_code_id uuid NULL,
				PRIMARY KEY (gift_order_id, recipient_index)
			);
		END IF;
		IF to_regclass('public.gift_order_pdf') IS NULL THEN
			CREATE TABLE gift_order_pdf (
				gift_order_id uuid NOT NULL,
				part varchar(16) NOT NULL,
				pdf bytea NOT NULL,
				create_time timestamp NOT NULL,
				PRIMARY KEY (gift_order_id, part)
			);
		END IF;
	END
	$gift$
`

var giftSchemaMigration = newSqlMigration(GiftSchemaSql)

// The migration's version: its position in the list, from 1.
func GiftSchemaVersion() int {
	for i, migration := range migrations {
		if migration == giftSchemaMigration {
			return i + 1
		}
	}
	return 0
}
