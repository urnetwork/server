// Operator-requested corrections are idempotent obligations. The immutable
// earning window survives a safely canceled plan; a processor attempt is final.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Every invocation requires an explicit, stable operation identity and reason;
// the transition date itself never authorizes a bonus. Retry the same identity
// after an uncertain command result. The whole plan is checked before mutation.
func PayoutPlanApplyBonus(ctx context.Context, paymentPlanId server.Id, bonusNanoCents NanoCents, operationId server.Id, reason string) (returnErr error) {
	if bonusNanoCents <= 0 || operationId == (server.Id{}) || strings.TrimSpace(reason) == "" || len(reason) > 1024 {
		return errors.New("bonus requires a positive amount, operation id and bounded reason")
	}
	policy, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return err
	}
	if err := server.RequireProviderPayoutSchema(ctx); err != nil {
		return err
	}
	policyHash := ""
	if policy != nil {
		policyHash = policy.ConfigSha256
	}
	type bonusTarget struct {
		paymentId   server.Id
		networkId   server.Id
		eligible    bool
		latestClose *time.Time
		subsidyEnd  *time.Time
	}
	server.Tx(ctx, func(tx server.PgTx) {
		// The same operation cannot race itself. A hash collision only serializes
		// unrelated explicit corrections; payment locks remain the money fence.
		server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(620061,hashtext($1))`, operationId.String()))
		var priorCount int64
		var same bool
		server.Raise(tx.QueryRow(ctx, `SELECT COUNT(*),COALESCE(bool_and(payment_plan_id=$2 AND amount_nano_cents=$3 AND reason=$4),false)
			FROM account_payment_bonus WHERE operation_id=$1`, operationId, paymentPlanId, bonusNanoCents, reason).Scan(&priorCount, &same))
		if priorCount > 0 {
			if !same {
				returnErr = errors.New("bonus operation id already has different authority")
			}
			return
		}
		result, err := tx.Query(ctx, `SELECT payment_id,network_id,
			NOT completed AND NOT canceled AND circle_idempotency_key IS NULL AND payment_record IS NULL AND tx_hash IS NULL
			FROM account_payment WHERE payment_plan_id=$1 ORDER BY payment_id LIMIT 10001 FOR UPDATE`, paymentPlanId)
		targets := []bonusTarget{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var target bonusTarget
				server.Raise(result.Scan(&target.paymentId, &target.networkId, &target.eligible))
				targets = append(targets, target)
			}
		})
		if len(targets) == 0 || len(targets) > 10000 {
			returnErr = errors.New("bonus plan is empty or exceeds 10000-payment review bound")
			return
		}
		for index := range targets {
			target := &targets[index]
			if !target.eligible {
				returnErr = fmt.Errorf("payment %s has a retained submission or terminal state; no bonus applied", target.paymentId)
				return
			}
			target.latestClose, target.subsidyEnd, err = providerUsdcPaymentAttribution(ctx, tx, target.paymentId, policy)
			if err != nil {
				returnErr = err
				return
			}
		}
		for _, target := range targets {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment_bonus
				(operation_id,origin_payment_id,payment_id,payment_plan_id,network_id,amount_nano_cents,reason,latest_contract_close_time,subsidy_end_time,policy_sha256)
				VALUES($1,$2,$2,$3,$4,$5,$6,$7,$8,$9)`, operationId, target.paymentId, paymentPlanId, target.networkId, bonusNanoCents, reason, target.latestClose, target.subsidyEnd, policyHash))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET payout_nano_cents=payout_nano_cents+$2,
				bonus_payout_nano_cents=bonus_payout_nano_cents+$2 WHERE payment_id=$1`, target.paymentId, bonusNanoCents))
		}
	}, server.TxReadCommitted)
	return
}

// Only corrections already attached to safely canceled selected sweeps move.
// Their original amount/window/reason are immutable; one current payment owns
// each obligation. Missing or ambiguous earning authority remains retained.
func (self *PaymentPlanner) carryPaymentBonuses() {
	var cutoff *time.Time
	if self.transition != nil {
		cutoff = &self.transition.Cutoff
	}
	server.RaisePgResult(self.tx.Exec(self.ctx, `CREATE TEMPORARY TABLE temp_payment_bonus ON COMMIT DROP AS
		SELECT b.operation_id,b.origin_payment_id,b.payment_id,b.network_id,b.amount_nano_cents
		FROM account_payment_bonus b JOIN account_payment p ON p.payment_id=b.payment_id
		WHERE p.canceled AND NOT p.completed AND p.circle_idempotency_key IS NULL AND p.payment_record IS NULL AND p.tx_hash IS NULL
		AND ($1::timestamp IS NULL OR (b.latest_contract_close_time<$1 AND (b.subsidy_end_time IS NULL OR b.subsidy_end_time<=$1)))
		AND EXISTS (SELECT 1 FROM transfer_escrow_sweep s JOIN temp_account_payment selected
			ON selected.contract_id=s.contract_id AND selected.balance_id=s.balance_id AND selected.network_id=s.network_id
			WHERE s.payment_id=p.payment_id)`, cutoff))
	result, err := self.tx.Query(self.ctx, `SELECT network_id,SUM(amount_nano_cents)::bigint FROM temp_payment_bonus GROUP BY network_id`)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			var bonus NanoCents
			server.Raise(result.Scan(&networkId, &bonus))
			if payment := self.networkPayments[networkId]; payment != nil {
				if bonus <= 0 || payment.Payout > int64(^uint64(0)>>1)-bonus {
					panic("retained bonus amount overflow")
				}
				payment.BonusPayout = bonus
				payment.Payout += bonus
			}
		}
	})
}

// Publish the new payment before moving the correction. A concurrent re-plan
// can only move the original binding once; a mismatch rolls back its whole plan.
func (self *PaymentPlanner) assignPaymentBonuses() {
	var expected int64
	server.Raise(self.tx.QueryRow(self.ctx, `SELECT COUNT(*) FROM temp_payment_bonus b JOIN temp_payment_network_ids n ON n.network_id=b.network_id`).Scan(&expected))
	tag := server.RaisePgResult(self.tx.Exec(self.ctx, `UPDATE account_payment_bonus b SET payment_id=n.payment_id
		FROM temp_payment_bonus selected,temp_payment_network_ids n
		WHERE b.operation_id=selected.operation_id AND b.origin_payment_id=selected.origin_payment_id
		AND b.payment_id=selected.payment_id AND n.network_id=selected.network_id`))
	if tag.RowsAffected() != expected {
		panic("retained bonus assignment changed concurrently")
	}
	server.RaisePgResult(self.tx.Exec(self.ctx, `UPDATE account_payment p SET
		payout_nano_cents=p.payout_nano_cents+b.amount,bonus_payout_nano_cents=b.amount
		FROM (SELECT payment_id,SUM(amount_nano_cents)::bigint amount FROM account_payment_bonus
			WHERE payment_id IN (SELECT payment_id FROM temp_payment_network_ids) GROUP BY payment_id) b
		WHERE p.payment_id=b.payment_id`))
}
