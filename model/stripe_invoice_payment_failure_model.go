// The failed-renewal ledger of Stripe subscription invoices
// (db_stripe_invoice_payment_failure.go). The controller records each failed
// renewal charge here and decides, on the row it locks, which payment-failed
// notice the invoice owes (controller/subscription_stripe_dunning.go). The row
// is keyed by the invoice, so a webhook that is delivered again, late or out of
// order decides on the same row: each notice time is set once, and a resolved
// row owes nothing more.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// Why a failed invoice stopped failing.
const (
	StripeInvoicePaymentFailureResolvePaid              = "paid"
	StripeInvoicePaymentFailureResolveVoided            = "voided"
	StripeInvoicePaymentFailureResolveUncollectible     = "uncollectible"
	StripeInvoicePaymentFailureResolveSubscriptionEnded = "subscription_ended"
)

// One failed renewal invoice.
type StripeInvoicePaymentFailure struct {
	InvoiceId      string
	NetworkId      server.Id
	CustomerId     string
	SubscriptionId string
	// the invoice's Stripe-hosted payment page; cleared on resolve
	HostedInvoiceUrl string
	// the invoice's attempt count at the latest failure recorded
	AttemptCount  int
	FirstFailTime time.Time
	LastFailTime  time.Time
	// when each notice was owed; nil until then
	FirstNoticeTime *time.Time
	FinalNoticeTime *time.Time
	// nil while the invoice fails
	ResolveTime   *time.Time
	ResolveReason string
}

const stripeInvoicePaymentFailureColumns = `
	invoice_id,
	network_id,
	customer_id,
	subscription_id,
	hosted_invoice_url,
	attempt_count,
	first_fail_time,
	last_fail_time,
	first_notice_time,
	final_notice_time,
	resolve_time,
	COALESCE(resolve_reason, '')
`

// Scans one row of stripeInvoicePaymentFailureColumns.
func scanStripeInvoicePaymentFailure(result server.PgResult) *StripeInvoicePaymentFailure {
	failure := &StripeInvoicePaymentFailure{}
	server.Raise(result.Scan(
		&failure.InvoiceId,
		&failure.NetworkId,
		&failure.CustomerId,
		&failure.SubscriptionId,
		&failure.HostedInvoiceUrl,
		&failure.AttemptCount,
		&failure.FirstFailTime,
		&failure.LastFailTime,
		&failure.FirstNoticeTime,
		&failure.FinalNoticeTime,
		&failure.ResolveTime,
		&failure.ResolveReason,
	))
	return failure
}

// Records a failed attempt of the invoice at `failTime` and returns the row,
// locked for the caller's transaction, so concurrent deliveries of the invoice's
// events decide one after the other. The first record fixes the network,
// customer and subscription; a later one only raises the attempt count and the
// last failure time, and refreshes the hosted invoice link of an open row.
func RecordStripeInvoicePaymentFailureInTx(
	ctx context.Context,
	tx server.PgTx,
	failure *StripeInvoicePaymentFailure,
	failTime time.Time,
) (recorded *StripeInvoicePaymentFailure) {
	result, err := tx.Query(
		ctx,
		`
			INSERT INTO stripe_invoice_payment_failure (
				invoice_id,
				network_id,
				customer_id,
				subscription_id,
				hosted_invoice_url,
				attempt_count,
				first_fail_time,
				last_fail_time
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
			ON CONFLICT (invoice_id) DO UPDATE SET
				attempt_count = GREATEST(stripe_invoice_payment_failure.attempt_count, EXCLUDED.attempt_count),
				last_fail_time = GREATEST(stripe_invoice_payment_failure.last_fail_time, EXCLUDED.last_fail_time),
				hosted_invoice_url = CASE
					WHEN stripe_invoice_payment_failure.resolve_time IS NULL THEN EXCLUDED.hosted_invoice_url
					ELSE stripe_invoice_payment_failure.hosted_invoice_url
				END
			RETURNING `+stripeInvoicePaymentFailureColumns,
		failure.InvoiceId,
		failure.NetworkId,
		failure.CustomerId,
		failure.SubscriptionId,
		failure.HostedInvoiceUrl,
		failure.AttemptCount,
		failTime,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			recorded = scanStripeInvoicePaymentFailure(result)
		}
	})
	return
}

// Records that the invoice owes its first notice at `noticeTime`.
func SetStripeInvoicePaymentFailureFirstNoticeInTx(ctx context.Context, tx server.PgTx, invoiceId string, noticeTime time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE stripe_invoice_payment_failure
			SET first_notice_time = $2
			WHERE invoice_id = $1 AND first_notice_time IS NULL
		`,
		invoiceId,
		noticeTime,
	))
}

// Records that the invoice owes its final notice at `noticeTime`. The final
// notice also stands for the first, so an invoice whose first decided failure
// is already its last owes one notice, and a late copy of an earlier failure
// owes none.
func SetStripeInvoicePaymentFailureFinalNoticeInTx(ctx context.Context, tx server.PgTx, invoiceId string, noticeTime time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE stripe_invoice_payment_failure
			SET
				final_notice_time = $2,
				first_notice_time = COALESCE(first_notice_time, $2)
			WHERE invoice_id = $1 AND final_notice_time IS NULL
		`,
		invoiceId,
		noticeTime,
	))
}

// Resolves the invoice's open failure. Returns the network of the row it
// resolved, or nil when the invoice has no open failure.
func ResolveStripeInvoicePaymentFailureInTx(
	ctx context.Context,
	tx server.PgTx,
	invoiceId string,
	reason string,
	resolveTime time.Time,
) (networkId *server.Id) {
	result, err := tx.Query(
		ctx,
		`
			UPDATE stripe_invoice_payment_failure
			SET
				resolve_time = $3,
				resolve_reason = $2,
				hosted_invoice_url = ''
			WHERE invoice_id = $1 AND resolve_time IS NULL
			RETURNING network_id
		`,
		invoiceId,
		reason,
		resolveTime,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var resolvedNetworkId server.Id
			server.Raise(result.Scan(&resolvedNetworkId))
			networkId = &resolvedNetworkId
		}
	})
	return
}

// Resolves every open failure of the subscription. Returns the rows it
// resolved, by invoice.
func ResolveStripeSubscriptionPaymentFailuresInTx(
	ctx context.Context,
	tx server.PgTx,
	subscriptionId string,
	reason string,
	resolveTime time.Time,
) (invoiceIdNetworkIds map[string]server.Id) {
	invoiceIdNetworkIds = map[string]server.Id{}
	result, err := tx.Query(
		ctx,
		`
			UPDATE stripe_invoice_payment_failure
			SET
				resolve_time = $3,
				resolve_reason = $2,
				hosted_invoice_url = ''
			WHERE subscription_id = $1 AND resolve_time IS NULL
			RETURNING invoice_id, network_id
		`,
		subscriptionId,
		reason,
		resolveTime,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var invoiceId string
			var networkId server.Id
			server.Raise(result.Scan(&invoiceId, &networkId))
			invoiceIdNetworkIds[invoiceId] = networkId
		}
	})
	return
}

// The network's open failure whose latest failure is at or after
// `minLastFailTime`, the most recent first, or nil. A failure that no event
// resolved (a webhook that never arrived) stops counting once its last failure
// is older than that.
func GetOpenStripeInvoicePaymentFailure(
	ctx context.Context,
	networkId server.Id,
	minLastFailTime time.Time,
) (failure *StripeInvoicePaymentFailure) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+stripeInvoicePaymentFailureColumns+`
				FROM stripe_invoice_payment_failure
				WHERE
					network_id = $1 AND
					resolve_time IS NULL AND
					$2 <= last_fail_time
				ORDER BY last_fail_time DESC, invoice_id
				LIMIT 1
			`,
			networkId,
			minLastFailTime,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				failure = scanStripeInvoicePaymentFailure(result)
			}
		})
	})
	return
}

// The invoice's row, open or resolved, or nil.
func GetStripeInvoicePaymentFailure(ctx context.Context, invoiceId string) (failure *StripeInvoicePaymentFailure) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+stripeInvoicePaymentFailureColumns+`
				FROM stripe_invoice_payment_failure
				WHERE invoice_id = $1
			`,
			invoiceId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				failure = scanStripeInvoicePaymentFailure(result)
			}
		})
	})
	return
}

// The email address of the network's account, read on the caller's
// transaction, or "" when the account has none (a wallet, seedphrase or phone
// account) or the network is gone. In order: the admin's own sign-in address
// (network_user, the address every other account notice goes to) when it is an
// email, then a verified email-and-password sign-in, then an SSO sign-in
// address (Google, Apple), newest first within each.
func GetAccountEmailInTx(ctx context.Context, tx server.PgTx, networkId server.Id) string {
	var adminUserId *server.Id
	var adminUserAuth *string
	result, err := tx.Query(
		ctx,
		`
			SELECT
				network.admin_user_id,
				network_user.user_auth
			FROM network
			INNER JOIN network_user ON network_user.user_id = network.admin_user_id
			WHERE network.network_id = $1
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&adminUserId, &adminUserAuth))
		}
	})
	if adminUserId == nil {
		return ""
	}
	if normalUserAuth, userAuthType := NormalUserAuthV1(adminUserAuth); userAuthType == UserAuthTypeEmail {
		return *normalUserAuth
	}

	email := ""
	result, err = tx.Query(
		ctx,
		`
			SELECT user_auth, 0 AS source, create_time
			FROM network_user_auth_password
			WHERE user_id = $1 AND auth_type = $2 AND verified

			UNION ALL

			SELECT user_auth, 1 AS source, create_time
			FROM network_user_auth_sso
			WHERE user_id = $1

			ORDER BY source, create_time DESC
		`,
		*adminUserId,
		UserAuthTypeEmail,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var userAuth string
			var source int
			var createTime time.Time
			server.Raise(result.Scan(&userAuth, &source, &createTime))
			if email != "" {
				continue
			}
			if normalUserAuth, userAuthType := NormalUserAuth(userAuth); userAuthType == UserAuthTypeEmail {
				email = normalUserAuth
			}
		}
	})
	return email
}
