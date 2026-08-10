package model

// Storage for the hourly payment reconciliation task (UPGRADE.md §8): the
// audit-event trail, the per-store listing watermark, and the read/repair
// helpers the reconcilers share. The crediting itself never lives here -- the
// reconcilers credit through the same idempotent controller paths the webhooks
// use.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/server"
)

// The reconciliation audit actions. Every run writes at least a heartbeat;
// every repair, skip, and per-object failure is its own row. A dry run
// (bringyourctl payments reconcile --dry-run) records the would_ forms
// instead of repairing: same evidence and details the real repair would
// carry, tagged dry_run = true.
const (
	PaymentReconcileActionCredited     = "credited"
	PaymentReconcileActionEnded        = "ended"
	PaymentReconcileActionWouldCredit  = "would_credit"
	PaymentReconcileActionWouldEnd     = "would_end"
	PaymentReconcileActionSkippedStore = "skipped_store"
	PaymentReconcileActionHeartbeat    = "heartbeat"
	PaymentReconcileActionError        = "error"
)

// Webhook-written operator events (UPGRADE.md §2 S7/S11) that join the
// reconciler's audit stream. Their run_id is a fresh provenance id, not a
// reconcile run: the store's REAL-TIME refund/revocation handlers and the
// stripe email fallback write these as they happen.
const (
	// a store refund clawed back (or tried to claw back) what it granted
	PaymentReconcileActionRefunded = "refunded"
	// a dispute was opened -- the funds are withdrawn, treated like a refund
	PaymentReconcileActionDisputed = "disputed"
	// a store revoked an entitlement (Apple REFUND/REVOKE, Play
	// SUBSCRIPTION_REVOKED) and the entitlement was ended
	PaymentReconcileActionRevoked = "revoked"
	// a refund/dispute whose charge could not be mapped to anything we
	// granted: recorded for the operator instead of guessing what to claw
	PaymentReconcileActionRefundUnmatched = "refund_unmatched"
	// stripeHandleInvoicePaid resolved the network by the LEGACY customer
	// email fallback (S11) -- every use is surfaced until it can be retired
	PaymentReconcileActionEmailFallback = "email_fallback"
)

// PaymentReconcileStoreAll is the store label for run-level rows (heartbeat).
const PaymentReconcileStoreAll = "all"

type PaymentReconciliationEvent struct {
	EventId   server.Id
	RunId     server.Id
	Store     string
	NetworkId *server.Id
	Action    string
	// the store-object id the action acted on (invoice id, transaction id,
	// purchase token, tx signature); "" for run-level rows
	Evidence string
	// free-form context, stored as json
	Details map[string]any
	// true on every event a dry-run pass writes (including its heartbeat and
	// error rows), so operator queries over real repairs exclude dry runs by
	// the column's default false
	DryRun    bool
	EventTime time.Time
}

// AddPaymentReconciliationEvent appends one audit row. The audit trail must
// never turn a completed repair into a failed run, so callers log-and-continue
// on error rather than aborting.
func AddPaymentReconciliationEvent(
	ctx context.Context,
	event *PaymentReconciliationEvent,
) (err error) {
	server.Tx(ctx, func(tx server.PgTx) {
		err = AddPaymentReconciliationEventInTx(tx, ctx, event)
	})
	return
}

// AddPaymentReconciliationEventInTx is AddPaymentReconciliationEvent inside a
// caller-owned tx -- used by the refund/revocation webhook handlers so the
// operator event commits (or rolls back) atomically with the clawback and its
// idempotency-ledger row.
func AddPaymentReconciliationEventInTx(
	tx server.PgTx,
	ctx context.Context,
	event *PaymentReconciliationEvent,
) (err error) {
	var detailsJson *string
	if 0 < len(event.Details) {
		if detailsBytes, jsonErr := json.Marshal(event.Details); jsonErr == nil {
			details := string(detailsBytes)
			detailsJson = &details
		}
	}
	var evidence *string
	if event.Evidence != "" {
		evidence = &event.Evidence
	}

	event.EventId = server.NewId()
	event.EventTime = server.NowUtc()

	_, err = tx.Exec(
		ctx,
		`
		INSERT INTO payment_reconciliation_event
		(event_id, run_id, store, network_id, action, evidence, details, dry_run, event_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`,
		event.EventId,
		event.RunId,
		event.Store,
		event.NetworkId,
		event.Action,
		evidence,
		detailsJson,
		event.DryRun,
		event.EventTime,
	)
	return
}

// GetPaymentReconciliationEventsByAction reads one store's audit rows for one
// action since a time, oldest first, bounded by limit and excluding dry runs.
// This is how the stripe reconciler leg and the CLI summary surface
// webhook-written events (email_fallback) since the last watermark.
func GetPaymentReconciliationEventsByAction(
	ctx context.Context,
	store string,
	action string,
	since time.Time,
	limit int,
) []*PaymentReconciliationEvent {
	events := []*PaymentReconciliationEvent{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT event_id, run_id, network_id,
			       COALESCE(evidence, ''), details, event_time
			FROM payment_reconciliation_event
			WHERE store = $1
			  AND action = $2
			  AND event_time >= $3
			  AND NOT dry_run
			ORDER BY event_time ASC, event_id ASC
			LIMIT $4
			`,
			store,
			action,
			since,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				event := &PaymentReconciliationEvent{
					Store:  store,
					Action: action,
				}
				var detailsJson *string
				server.Raise(result.Scan(
					&event.EventId,
					&event.RunId,
					&event.NetworkId,
					&event.Evidence,
					&detailsJson,
					&event.EventTime,
				))
				if detailsJson != nil {
					json.Unmarshal([]byte(*detailsJson), &event.Details)
				}
				events = append(events, event)
			}
		})
	})
	return events
}

// GetPaymentReconciliationEvents reads one run's audit rows back, oldest first.
func GetPaymentReconciliationEvents(
	ctx context.Context,
	runId server.Id,
) []*PaymentReconciliationEvent {
	events := []*PaymentReconciliationEvent{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT event_id, store, network_id, action,
			       COALESCE(evidence, ''), details, dry_run, event_time
			FROM payment_reconciliation_event
			WHERE run_id = $1
			ORDER BY event_time ASC, event_id ASC
			`,
			runId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				event := &PaymentReconciliationEvent{
					RunId: runId,
				}
				var detailsJson *string
				server.Raise(result.Scan(
					&event.EventId,
					&event.Store,
					&event.NetworkId,
					&event.Action,
					&event.Evidence,
					&detailsJson,
					&event.DryRun,
					&event.EventTime,
				))
				if detailsJson != nil {
					json.Unmarshal([]byte(*detailsJson), &event.Details)
				}
				events = append(events, event)
			}
		})
	})
	return events
}

// GetPaymentReconcileWatermark returns the store's incremental-listing
// watermark. ok = false means the store has never completed a run.
func GetPaymentReconcileWatermark(
	ctx context.Context,
	store string,
) (watermarkTime time.Time, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT watermark_time FROM payment_reconciliation_watermark
			WHERE store = $1
			`,
			store,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&watermarkTime))
				ok = true
			}
		})
	})
	return
}

// SetPaymentReconcileWatermark advances (upserts) the store's watermark. Only
// called after a store's reconcile pass completes without error, so a failed
// pass re-examines the same window next run.
func SetPaymentReconcileWatermark(
	ctx context.Context,
	store string,
	watermarkTime time.Time,
) (err error) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err = tx.Exec(
			ctx,
			`
			INSERT INTO payment_reconciliation_watermark (store, watermark_time, update_time)
			VALUES ($1, $2, $3)
			ON CONFLICT (store) DO UPDATE
			SET watermark_time = $2, update_time = $3
			`,
			store,
			watermarkTime,
			server.NowUtc(),
		)
	})
	return
}

// ReconcileSubscriptionRenewal is one subscription_renewal row as the
// reconcilers iterate it: enough to name the store object (purchase token /
// transaction id) and the entitlement window it bought.
type ReconcileSubscriptionRenewal struct {
	NetworkId     server.Id
	StartTime     time.Time
	EndTime       time.Time
	PurchaseToken string
	TransactionId string
}

// GetReconcileSubscriptionRenewals returns the market's supporter renewals
// that are active now or ended after minEndTime (the ±48h reconcile window:
// recently-expired rows are where a missed renewal or missed revocation
// hides), newest window first, bounded by limit.
func GetReconcileSubscriptionRenewals(
	ctx context.Context,
	market SubscriptionMarket,
	minEndTime time.Time,
	limit int,
) []*ReconcileSubscriptionRenewal {
	renewals := []*ReconcileSubscriptionRenewal{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_id, start_time, end_time,
			       COALESCE(purchase_token, ''), COALESCE(transaction_id, '')
			FROM subscription_renewal
			WHERE market = $1
			  AND subscription_type = $2
			  AND end_time > $3
			ORDER BY end_time DESC
			LIMIT $4
			`,
			market,
			SubscriptionTypeSupporter,
			minEndTime,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				renewal := &ReconcileSubscriptionRenewal{}
				server.Raise(result.Scan(
					&renewal.NetworkId,
					&renewal.StartTime,
					&renewal.EndTime,
					&renewal.PurchaseToken,
					&renewal.TransactionId,
				))
				renewals = append(renewals, renewal)
			}
		})
	})
	return renewals
}

// EndReconciledEntitlement is the "store says it is already over" repair:
// refund, revocation, or an expiry the end-of-period task missed. It ends --
// AT now, never retroactively -- the market's active supporter renewals and
// the pro transfer balances those renewals granted, then refreshes the pro
// cache. The balances are matched by their exact window end: every store
// crediting path writes the renewal and its balance with the SAME end_time,
// so this claws back only what the ended subscription granted and leaves
// balances from other purchases (other markets, data codes) untouched.
//
// Callers must apply the cancelled ≠ expired rule BEFORE calling: a
// cancel-at-period-end with time remaining is paid-through and is never
// ended here.
func EndReconciledEntitlement(
	ctx context.Context,
	networkId server.Id,
	market SubscriptionMarket,
	now time.Time,
) (ended bool, err error) {
	server.Tx(ctx, func(tx server.PgTx) {
		ended = 0 < len(endReconciledEntitlementInTx(tx, ctx, &networkId, market, nil, "", now))
	})

	if ended {
		// the entitlement changed under the network -- refresh the pro cache so
		// the downgrade is visible immediately rather than after ProCacheTtl
		UpdateProNetwork(ctx, networkId)
	}
	return
}

// EndReconciledEntitlementForTransactions ends the market's active supporter
// renewals for the given store transaction ids (Stripe invoice ids, Apple
// transaction ids) and the pro balances they granted -- the
// EndReconciledEntitlement shape scoped to specific purchases, for the
// real-time refund/revocation handlers. Returns the networks whose
// entitlement was ended (their pro cache is refreshed here).
func EndReconciledEntitlementForTransactions(
	ctx context.Context,
	market SubscriptionMarket,
	transactionIds []string,
	now time.Time,
) (endedNetworkIds []server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		endedNetworkIds = EndReconciledEntitlementForTransactionsInTx(tx, ctx, market, transactionIds, now)
	})
	for _, networkId := range endedNetworkIds {
		UpdateProNetwork(ctx, networkId)
	}
	return
}

// EndReconciledEntitlementForTransactionsInTx is the caller-owns-the-tx form,
// so a webhook handler can gate the clawback on its idempotency ledger inside
// the SAME tx (the stripe_refund ledger, the apple_notification ledger). The
// caller must refresh the pro cache (UpdateProNetwork) for each returned
// network after commit.
func EndReconciledEntitlementForTransactionsInTx(
	tx server.PgTx,
	ctx context.Context,
	market SubscriptionMarket,
	transactionIds []string,
	now time.Time,
) []server.Id {
	if len(transactionIds) == 0 {
		// an empty scope must never mean "the whole market"
		return nil
	}
	return endReconciledEntitlementInTx(tx, ctx, nil, market, transactionIds, "", now)
}

// EndReconciledEntitlementForPurchaseToken is the purchase-token-scoped form
// (Play SUBSCRIPTION_REVOKED). The network is derived from the renewal rows
// themselves -- a revoked token can be 410-Gone at the store, so the rows are
// the only reliable map back to the network. Refreshes the pro cache for each
// ended network.
func EndReconciledEntitlementForPurchaseToken(
	ctx context.Context,
	market SubscriptionMarket,
	purchaseToken string,
	now time.Time,
) (endedNetworkIds []server.Id) {
	if purchaseToken == "" {
		// an empty scope must never mean "the whole market"
		return nil
	}
	server.Tx(ctx, func(tx server.PgTx) {
		endedNetworkIds = endReconciledEntitlementInTx(tx, ctx, nil, market, nil, purchaseToken, now)
	})
	for _, networkId := range endedNetworkIds {
		UpdateProNetwork(ctx, networkId)
	}
	return
}

// endReconciledEntitlementInTx is the shared core: it ends -- AT now, never
// retroactively -- the market's active supporter renewals matching the scope
// (networkId nil-able; transactionIds nil = no transaction filter;
// purchaseToken "" = no token filter) and the pro transfer balances those
// renewals granted, matched per network by identical window end. Naturally
// idempotent: every predicate requires end_time > now, so a second delivery
// of the same refund/revocation finds nothing left to end. Returns the
// distinct networks whose entitlement was ended.
func endReconciledEntitlementInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId *server.Id,
	market SubscriptionMarket,
	transactionIds []string,
	purchaseToken string,
	now time.Time,
) []server.Id {
	networkEndTimes := map[server.Id][]time.Time{}
	result, queryErr := tx.Query(
		ctx,
		`
		SELECT network_id, end_time FROM subscription_renewal
		WHERE subscription_type = $1
		  AND market = $2
		  AND end_time > $3
		  AND ($4::uuid IS NULL OR network_id = $4)
		  AND ($5::varchar[] IS NULL OR transaction_id = ANY($5))
		  AND ($6::varchar = '' OR purchase_token = $6)
		`,
		SubscriptionTypeSupporter,
		market,
		now,
		networkId,
		transactionIds,
		purchaseToken,
	)
	server.WithPgResult(result, queryErr, func() {
		for result.Next() {
			var rowNetworkId server.Id
			var endTime time.Time
			server.Raise(result.Scan(&rowNetworkId, &endTime))
			networkEndTimes[rowNetworkId] = append(networkEndTimes[rowNetworkId], endTime)
		}
	})
	if len(networkEndTimes) == 0 {
		return nil
	}

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		UPDATE subscription_renewal
		SET end_time = $3
		WHERE subscription_type = $1
		  AND market = $2
		  AND end_time > $3
		  AND ($4::uuid IS NULL OR network_id = $4)
		  AND ($5::varchar[] IS NULL OR transaction_id = ANY($5))
		  AND ($6::varchar = '' OR purchase_token = $6)
		`,
		SubscriptionTypeSupporter,
		market,
		now,
		networkId,
		transactionIds,
		purchaseToken,
	))

	endedNetworkIds := []server.Id{}
	for endedNetworkId, endTimes := range networkEndTimes {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE transfer_balance
			SET end_time = $2
			WHERE network_id = $1
			  AND pro
			  AND end_time = ANY($3::timestamp[])
			  AND end_time > $2
			`,
			endedNetworkId,
			now,
			endTimes,
		))
		endedNetworkIds = append(endedNetworkIds, endedNetworkId)
	}
	return endedNetworkIds
}

// GetStripeInvoiceNetworkId reads the stripe_invoice credit ledger: ok = true
// means the invoice already credited (and to which network). The stripe
// reconciler pre-checks this before spending store API budget on an invoice
// the webhook already handled.
func GetStripeInvoiceNetworkId(
	ctx context.Context,
	invoiceId string,
) (networkId server.Id, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_id FROM stripe_invoice
			WHERE invoice_id = $1
			`,
			invoiceId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
				ok = true
			}
		})
	})
	return
}

// IsAppleTransactionCredited reads the apple_subscription_transaction credit
// ledger: true means this transaction already granted its entitlement (the
// apple reconciler's pre-check before crediting a store-reported renewal).
func IsAppleTransactionCredited(
	ctx context.Context,
	transactionId string,
) (credited bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT 1 FROM apple_subscription_transaction
			WHERE transaction_id = $1
			`,
			transactionId,
		)
		server.WithPgResult(result, err, func() {
			credited = result.Next()
		})
	})
	return
}
