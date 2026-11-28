// The price tier evidence's migration (server price_tier_evidence_schema.go):
// what decided the price tier of each crypto invoice, Gamma intent and Solana
// Pay intent, kept on the intent's row.
package monitor

import "github.com/urnetwork/server/v2026"

// One nullable jsonb column, without a default, on each of the three intent
// tables. The migration publishes no index or check.
var priceTierEvidenceMigrationContract = migrationCurrentSuffix(server.PriceTierEvidenceSchemaMigrationVersion(), "price tier evidence on the purchase intents",
	snMainnetMigrationColumns("crypto_payment_intent",
		snMainnetMigrationColumn{name: "price_tier_evidence", kind: "jsonb"}),
	snMainnetMigrationColumns("gamma_payment_intent",
		snMainnetMigrationColumn{name: "price_tier_evidence", kind: "jsonb"}),
	snMainnetMigrationColumns("solana_payment_intent",
		snMainnetMigrationColumn{name: "price_tier_evidence", kind: "jsonb"}))
