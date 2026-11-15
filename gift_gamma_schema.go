// Gift orders paid with Gamma (mmm/GIFTOFPRIVACY.md §10.2, mmm/BTCPAY.md §18;
// controller/gift_gamma_controller.go, model/gift_order_model.go).
//
// gift_order gains gamma_intent_id: the Pay with Gamma intent that pays the
// order, set in the transaction that records the intent, so the biller can
// never charge an intent its credit cannot find the order of. It is nullable,
// like stripe_session_id and crypto_intent_id, and unique like them, so one
// spend fulfils at most one order. The 30-day unlink sets it back to NULL
// (model.UnlinkGammaGiftOrders). An order of rail gamma is new: the column
// rail is varchar, so nothing constrains it in the database.
//
// The column is added only when absent, nullable with no default, which
// PostgreSQL keeps in the catalog: no row is rewritten. Its unique constraint
// builds an index over gift_order, which holds only the orders sold since gift
// sales opened, and is added only when absent; both run under a five-second
// lock bound, so a failed lock wait is safe to retry. Old binaries name the
// columns they read and write, so they neither see nor need it, and never
// store an order of rail gamma. Apply before deploying the api and alt (POST
// /pay/gift/gamma-intent) and the taskworker (the Gamma biller's credit, the
// gift retention's unlink) that use it.
package server

// The migration's statement.
const GiftGammaSchemaSql = `
	DO $gift_gamma$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		ALTER TABLE gift_order
			ADD COLUMN IF NOT EXISTS gamma_intent_id uuid NULL;
		IF NOT EXISTS (
			SELECT 1
			FROM pg_constraint
			WHERE
				conrelid = 'public.gift_order'::regclass AND
				conname = 'gift_order_gamma_intent_id_key'
		) THEN
			ALTER TABLE gift_order
				ADD CONSTRAINT gift_order_gamma_intent_id_key UNIQUE (gamma_intent_id);
		END IF;
	END
	$gift_gamma$
`

var giftGammaSchemaMigration = newSqlMigration(GiftGammaSchemaSql)

// The migration's version: its position in the list, from 1.
func GiftGammaSchemaVersion() int {
	for i, migration := range migrations {
		if migration == giftGammaSchemaMigration {
			return i + 1
		}
	}
	return 0
}
