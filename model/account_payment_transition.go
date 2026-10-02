// A queued task is not earning authority. Re-read its immutable completed
// contracts and subsidy window before submitting a legacy provider payment.
package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/server"
)

var ErrProviderLegacyReliabilityWindow = errors.New("provider legacy reliability attribution unavailable for exact earning window; retained for retry")

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
	policy, err := server.LoadProviderPayoutTransition(ctx)
	if err != nil || policy == nil {
		return err
	}
	var allowed bool
	var queryErr error
	server.Db(ctx, func(conn server.PgConn) {
		queryErr = conn.QueryRow(ctx, `SELECT
			NOT p.completed AND NOT p.canceled AND p.payout_nano_cents > 0
			AND p.subsidy_payout_nano_cents >= 0 AND p.reliability_subsidy_nano_cents >= 0
			AND EXISTS (SELECT 1 FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id)
			AND NOT EXISTS (SELECT 1 FROM transfer_escrow_sweep s
				LEFT JOIN transfer_contract c ON c.contract_id=s.contract_id
				WHERE s.payment_id=p.payment_id AND
				(c.close_time IS NULL OR c.close_time >= $2 OR c.outcome IS NULL OR
				 c.outcome NOT IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination') OR
				 s.network_id <> p.network_id OR s.payout_net_revenue_nano_cents < 0))
			AND p.payout_nano_cents = p.subsidy_payout_nano_cents + p.reliability_subsidy_nano_cents +
				(SELECT COALESCE(SUM(s.payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id)
			AND ((p.subsidy_payout_nano_cents=0 AND p.reliability_subsidy_nano_cents=0) OR
				EXISTS (SELECT 1 FROM subsidy_payment sp WHERE sp.payment_plan_id=p.payment_plan_id
				AND sp.start_time < sp.end_time AND sp.end_time <= $2))
			FROM account_payment p WHERE p.payment_id=$1`, paymentId, policy.Cutoff).Scan(&allowed)
	})
	if queryErr != nil {
		return fmt.Errorf("provider USDC earning authority unavailable: %w", queryErr)
	}
	if !allowed {
		return errors.New("provider USDC payment has unresolved or post-cutoff earning attribution; retained without submission")
	}
	return nil
}
