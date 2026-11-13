// Payment holds are shared by row reaping and whole-partition retirement.
//
// USDC provider payouts ended (owner decisions 2026-11-12 and 2026-11-13:
// "settle closed rows, let's close out usdc entirely"). The close-out canceled
// each payment that was still pending and journaled it with one
// usdc_payout_closed event, keyed paid elsewhere ones included. A closed
// payment is settled for retention, as a completed one is: it holds no
// contract, whether or not an attempt of it reached Circle.
package server

import (
	"fmt"
)

// The journal event (audit_account_payment) of a payment the USDC close-out
// closed.
const UsdcPayoutClosedEventType = "usdc_payout_closed"

// The closure event id of the payment id expression paymentIdSql, a uuid
// expression: the md5 of a fixed prefix and the payment id's text. It is never
// the payment id itself, which a manual reimbursement or legacy replacement
// marker takes.
func UsdcPayoutClosedEventIdSql(paymentIdSql string) string {
	return fmt.Sprintf(`md5('%s/' || (%s)::text)::uuid`, UsdcPayoutClosedEventType, paymentIdSql)
}

// The payment of the payment id expression paymentIdSql holds its closure
// event. The probe is fenced by OFFSET 0, so it stays one primary key probe per
// row and the planner never turns it into a scan of the payment journal.
func UsdcPayoutClosedSql(paymentIdSql string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM audit_account_payment usdc_payout_closed
	WHERE usdc_payout_closed.event_id = %s
	AND usdc_payout_closed.payment_id = %s
	AND usdc_payout_closed.event_type = '%s' OFFSET 0)`,
		UsdcPayoutClosedEventIdSql(paymentIdSql),
		paymentIdSql,
		UsdcPayoutClosedEventType,
	)
}

// The account_payment row is settled for retention: completed, or canceled by
// the close-out. A closed payment that a writer unaware of the marker made
// pending again is not settled. References the account_payment relation/alias.
var AccountPaymentRetentionSettledSql = `(
	account_payment.completed OR
	(account_payment.canceled AND ` + UsdcPayoutClosedSql("account_payment.payment_id") + `)
)`

// References the account_payment relation/alias. A pending payment is an open
// obligation. A canceled external attempt (a Circle idempotency key, payment
// record or tx hash) still needs reconciliation, unless the close-out closed
// it. Completed payments can still need retention stamps.
var ContractPaymentRetentionHoldSql = `(
	account_payment.contract_retention_pending OR
	(
		NOT account_payment.completed AND
		(
			NOT account_payment.canceled OR
			(
				(
					account_payment.circle_idempotency_key IS NOT NULL OR
					account_payment.payment_record IS NOT NULL OR
					account_payment.tx_hash IS NOT NULL
				) AND
				NOT ` + UsdcPayoutClosedSql("account_payment.payment_id") + `
			)
		)
	)
)`
