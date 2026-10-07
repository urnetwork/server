// Legacy subsidy/reliability is already an allocated obligation. Re-planning
// only its raw sweeps would lose those components after the window advanced.
package model

import (
	"errors"

	"github.com/urnetwork/server/v2026"
)

const legacyPaymentRecoveryLimit = 1024

// Recover the same original row only with an exact, complete pre-cutoff census.
// Invalid originals stay visible for review and do not repeatedly occupy the
// bounded candidate batch. Locked owners and unrelated payments keep running.
func (self *PaymentPlanner) recoverLegacyComponents() {
	if self.transition == nil {
		return
	}
	server.Raise(server.RequireProviderPayoutSchema(self.ctx))
	result, err := self.tx.Query(self.ctx, `SELECT payment_id FROM account_payment
		WHERE canceled AND NOT completed AND circle_idempotency_key IS NULL AND payment_record IS NULL AND tx_hash IS NULL
		AND NOT attribution_review_required AND (subsidy_payout_nano_cents>0 OR reliability_subsidy_nano_cents>0)
		ORDER BY payment_id LIMIT 1024 FOR UPDATE SKIP LOCKED`)
	ids := []server.Id{}
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var id server.Id
			server.Raise(result.Scan(&id))
			ids = append(ids, id)
		}
	})
	for _, id := range ids {
		_, _, err := inspectProviderUsdcPaymentAttribution(self.ctx, self.tx, id, self.transition, true)
		if err != nil {
			if !errors.Is(err, ErrProviderUsdcAttributionUnresolved) {
				panic(err)
			}
			server.RaisePgResult(self.tx.Exec(self.ctx, `UPDATE account_payment SET attribution_review_required=true WHERE payment_id=$1`, id))
			self.unresolvedLegacyPaymentCount++
			continue
		}
		// Preserve original amounts, points, plan, sweeps and cancellation time.
		// Only the safely failed lifecycle flag changes; no new earning exists.
		server.RaisePgResult(self.tx.Exec(self.ctx, `UPDATE account_payment SET canceled=false WHERE payment_id=$1`, id))
		server.RaisePgResult(self.tx.Exec(self.ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details)
			VALUES($1,$2,'legacy_components_restored',json_build_object('policy_sha256',$3::text,'original_obligation',true)::text)`, server.NewId(), id, self.transition.ConfigSha256))
		self.restoredLegacyPaymentCount++
	}
}
