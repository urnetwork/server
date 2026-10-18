// A queued task is not earning authority. Re-read its immutable completed
// contracts and subsidy window before submitting a legacy provider payment.
package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

var ErrProviderLegacyReliabilityWindow = errors.New("provider legacy reliability attribution unavailable for exact earning window; retained for retry")
var ErrProviderUsdcAttributionUnresolved = errors.New("provider USDC payment has unresolved or post-cutoff earning attribution; retained without submission")

// A confirmed processor transfer is reconciled even when a historical gross
// correction is unexplained. Persist the exception without authorizing money.
func RetainProviderPaymentAttributionReview(ctx context.Context, paymentId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET attribution_review_required=true WHERE payment_id=$1`, paymentId))
	})
}

// This bounded diagnostic is a lower-bound sample, never a complete debt
// inventory. Both drivers preserve the planner's unpaid/canceled indexes.
const paymentTransitionUnresolvedSampleSql = `
	WITH candidates AS MATERIALIZED (
		(SELECT contract_id,payout_net_revenue_nano_cents FROM transfer_escrow_sweep WHERE payment_id IS NULL LIMIT 1024)
		UNION ALL
		(SELECT s.contract_id,s.payout_net_revenue_nano_cents FROM account_payment p
		 JOIN transfer_escrow_sweep s ON s.payment_id=p.payment_id
		 WHERE p.canceled AND NOT p.completed AND p.circle_idempotency_key IS NULL AND p.payment_record IS NULL AND p.tx_hash IS NULL LIMIT 1024)
	)
	SELECT COUNT(*), COALESCE(SUM(s.payout_net_revenue_nano_cents),0)::bigint
	FROM candidates s LEFT JOIN transfer_contract c ON c.contract_id=s.contract_id
	WHERE c.close_time IS NULL OR c.outcome IS NULL OR c.outcome NOT IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')`

// Ambiguous payments remain pending, with their processor idempotency key
// intact. Existing PaymentRecord reconciliation does not call this admission.
func RequireProviderUsdcPayment(ctx context.Context, paymentId server.Id) error {
	policy, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil || policy == nil {
		return err
	}
	var admissionErr error
	server.Db(ctx, func(conn server.PgConn) {
		_, _, admissionErr = providerUsdcPaymentAttribution(ctx, conn, paymentId, policy)
	})
	return admissionErr
}

// The same exact component census admits a send and records an explicitly
// requested adjustment. A nil policy here still requires original provenance.
func providerUsdcPaymentAttribution(ctx context.Context, query server.PgCanQuery, paymentId server.Id, policy *server.ProviderPayoutTransition) (latestClose, subsidyEnd *time.Time, returnErr error) {
	return inspectProviderUsdcPaymentAttribution(ctx, query, paymentId, policy, false)
}

// Canceled recovery additionally requires the complete original sweep byte
// census. Query failures never become evidence that the old debt is invalid.
func inspectProviderUsdcPaymentAttribution(ctx context.Context, query server.PgCanQuery, paymentId server.Id, policy *server.ProviderPayoutTransition, allowCanceled bool) (latestClose, subsidyEnd *time.Time, returnErr error) {
	return inspectProviderUsdcPaymentAttributionWithReview(ctx, query, paymentId, policy, allowCanceled, false)
}

// Only the locked replacement release may ignore its own review hold. It still
// requires the exact same contract, amount, bonus and subsidy provenance.
func inspectProviderUsdcPaymentAttributionWithReview(ctx context.Context, query server.PgCanQuery, paymentId server.Id, policy *server.ProviderPayoutTransition, allowCanceled, allowReviewHeld bool) (latestClose, subsidyEnd *time.Time, returnErr error) {
	var cutoff *time.Time
	if policy != nil {
		cutoff = &policy.Cutoff
	}
	var allowed bool
	result, err := query.Query(ctx, `SELECT
			NOT p.completed AND (NOT p.canceled OR $3) AND (NOT p.attribution_review_required OR $4) AND p.payout_nano_cents >= 0
			AND p.subsidy_payout_nano_cents >= 0 AND p.reliability_subsidy_nano_cents >= 0
			AND attribution.sweep_count > 0
			AND (NOT $3 OR p.payout_byte_count=attribution.payout_byte_count)
			AND NOT attribution.invalid
			AND p.payout_nano_cents = p.subsidy_payout_nano_cents::numeric + p.reliability_subsidy_nano_cents + p.bonus_payout_nano_cents +
				attribution.payout_net_revenue_nano_cents
			AND p.bonus_payout_nano_cents = (SELECT COALESCE(SUM(b.amount_nano_cents),0) FROM account_payment_bonus b WHERE b.payment_id=p.payment_id)
			AND NOT EXISTS (SELECT 1 FROM account_payment_bonus b WHERE b.payment_id=p.payment_id AND
				(b.network_id<>p.network_id OR ($2::timestamp IS NOT NULL AND
				 (b.latest_contract_close_time >= $2 OR b.subsidy_end_time > $2))))
			AND ((p.subsidy_payout_nano_cents=0 AND p.reliability_subsidy_nano_cents=0) OR
				EXISTS (SELECT 1 FROM subsidy_payment sp WHERE sp.payment_plan_id=p.payment_plan_id
				AND sp.start_time < sp.end_time AND ($2::timestamp IS NULL OR sp.end_time <= $2))),
			attribution.latest_close,
			(SELECT MAX(sp.end_time) FROM subsidy_payment sp WHERE sp.payment_plan_id=p.payment_plan_id AND (p.subsidy_payout_nano_cents>0 OR p.reliability_subsidy_nano_cents>0))
			FROM account_payment p
			CROSS JOIN LATERAL (
				SELECT COUNT(*) AS sweep_count,
					COALESCE(SUM(s.payout_byte_count),0) AS payout_byte_count,
					COALESCE(SUM(s.payout_net_revenue_nano_cents),0) AS payout_net_revenue_nano_cents,
					COALESCE(BOOL_OR(c.close_time IS NULL OR ($2::timestamp IS NOT NULL AND c.close_time >= $2) OR
						c.outcome IS NULL OR c.outcome NOT IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination') OR
						s.network_id <> p.network_id OR s.payout_net_revenue_nano_cents < 0),false) AS invalid,
					MAX(c.close_time) AS latest_close
				FROM transfer_escrow_sweep s
				-- Keep contract reads keyed to the selected sweeps. Flattening this
				-- join can choose a census of unrelated historical contracts.
				LEFT JOIN LATERAL (
					SELECT c.close_time,c.outcome FROM transfer_contract c
					WHERE c.contract_id=s.contract_id OFFSET 0
				) c ON true
				WHERE s.payment_id=p.payment_id
			) attribution
			WHERE p.payment_id=$1`, paymentId, cutoff, allowCanceled, allowReviewHeld)
	if err != nil {
		return nil, nil, fmt.Errorf("provider USDC earning authority unavailable: %w", err)
	}
	defer result.Close()
	if result.Next() {
		err = result.Scan(&allowed, &latestClose, &subsidyEnd)
	}
	if err = errors.Join(err, result.Err()); err != nil {
		return nil, nil, fmt.Errorf("provider USDC earning authority unavailable: %w", err)
	}
	if !allowed || latestClose == nil {
		return nil, nil, ErrProviderUsdcAttributionUnresolved
	}
	return latestClose, subsidyEnd, nil
}
