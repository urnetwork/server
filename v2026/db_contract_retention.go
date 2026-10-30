// Payment holds are shared by row reaping and whole-partition retirement.
package server

// References the account_payment relation/alias. A canceled external attempt
// still needs reconciliation; completed payments can still need retention stamps.
const ContractPaymentRetentionHoldSql = `(
	account_payment.contract_retention_pending OR
	(
		NOT account_payment.completed AND
		(
			NOT account_payment.canceled OR
			account_payment.circle_idempotency_key IS NOT NULL OR
			account_payment.payment_record IS NOT NULL OR
			account_payment.tx_hash IS NOT NULL
		)
	)
)`
