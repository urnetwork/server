package server

// What decided the price tier of each purchase the server prices itself
// (controller/price_tier_signals.go): the tier, whether country signals
// decided it, the country the client claimed, and each signal's country and
// tier, as json. The crypto invoices (USDC on Solana Pay among them), the
// Gamma intents and the plans bought through the old Solana Pay endpoint
// keep it on their row; a Stripe subscription keeps it in its metadata,
// without the countries. One nullable jsonb column on each of the three
// tables, without a default, added only when absent under a five-second lock
// bound: nothing is rewritten, rows made before it have none, and the
// running release neither reads nor writes it.
const priceTierEvidenceSchemaSql = `
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE crypto_payment_intent ADD COLUMN IF NOT EXISTS price_tier_evidence jsonb NULL;
		ALTER TABLE gamma_payment_intent ADD COLUMN IF NOT EXISTS price_tier_evidence jsonb NULL;
		ALTER TABLE solana_payment_intent ADD COLUMN IF NOT EXISTS price_tier_evidence jsonb NULL;
	`

// The catalog monitor follows this immutable DDL wherever merges place it.
func PriceTierEvidenceSchemaMigrationVersion() int {
	return sqlMigrationVersion(priceTierEvidenceSchemaSql)
}
