package server

// The failed-renewal ledger of Stripe subscription invoices
// (controller/subscription_stripe_dunning.go). One row per invoice whose
// off-session renewal charge failed, keyed by the invoice, so the notices it
// owes the account are decided once however often and in whatever order Stripe
// delivers its events:
//
//   - first_notice_time and final_notice_time are set in the transaction that
//     owes each notice (and adds it to the account message outbox). A set time
//     is never owed again, so an invoice gets at most the two notices.
//   - resolve_time ends the failure: the invoice was paid, voided or marked
//     uncollectible, or its subscription ended. The in-app past-due notice
//     reads the open rows of a network, and the hosted invoice link is cleared
//     on resolve because nothing needs it afterwards.
//
// A new table: nothing is rewritten, and older binaries never read it.
const stripeInvoicePaymentFailureSchemaSql = `
	CREATE TABLE stripe_invoice_payment_failure (
		invoice_id varchar(128) NOT NULL,
		network_id uuid NOT NULL,
		customer_id varchar(128) NOT NULL,
		subscription_id varchar(128) NOT NULL,
		hosted_invoice_url varchar(2048) NOT NULL,
		attempt_count integer NOT NULL CHECK (attempt_count >= 0),
		first_fail_time timestamp NOT NULL,
		last_fail_time timestamp NOT NULL,
		first_notice_time timestamp NULL,
		final_notice_time timestamp NULL,
		resolve_time timestamp NULL,
		resolve_reason varchar(32) NULL,

		PRIMARY KEY (invoice_id)
	);

	CREATE INDEX stripe_invoice_payment_failure_network_open
		ON stripe_invoice_payment_failure (network_id, last_fail_time)
		WHERE resolve_time IS NULL;

	CREATE INDEX stripe_invoice_payment_failure_subscription_open
		ON stripe_invoice_payment_failure (subscription_id)
		WHERE resolve_time IS NULL;
`

// The version of the migration that creates the ledger in this binary, found by
// its SQL, so the monitor's contract follows the migration to whatever position
// a merge gives it. Zero when the binary lacks it.
func StripeInvoicePaymentFailureMigrationVersion() int {
	for index, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && sql.sql == stripeInvoicePaymentFailureSchemaSql {
			return index + 1
		}
	}
	return 0
}
