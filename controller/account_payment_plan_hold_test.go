// A payment of a plan whose staged sweep assignment is pending is held without
// a processor attempt, and advances once the plan's last batch commits.
package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestAdvancePaymentHoldsPaymentOfPendingPlanAssignment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `INSERT INTO payment_plan_assignment (payment_plan_id, create_time, sweep_count)
				VALUES ($1, $2, 1)`, payment.PaymentPlanId, server.NowUtc()))
		})
		sends := 0
		controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			if err := requireCircleProviderPayment(ctx); err != nil {
				return nil, err
			}
			sends++
			return &CreateTransferTransactionResult{Id: "synthetic-plan-released-transfer", State: "INITIATED"}, nil
		})
		args := &AdvancePaymentArgs{PaymentId: payment.PaymentId}

		result, err := AdvancePayment(args, owner)
		if err == nil || !strings.Contains(err.Error(), "pending its plan's sweep assignment") || result.Complete || result.Canceled || sends != 0 {
			t.Fatal("payment of a pending plan was not held", result, err, sends)
		}
		held, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || held.CircleIdempotencyKey != nil || held.PaymentRecord != nil || held.Canceled {
			t.Fatal("held payment changed", held, err)
		}

		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE payment_plan_assignment SET assigned_sweep_count = 1, complete_time = $2
				WHERE payment_plan_id = $1`, payment.PaymentPlanId, server.NowUtc()))
		})
		result, err = AdvancePayment(args, owner)
		if err != nil || result.Complete || result.Canceled || sends != 1 {
			t.Fatal("payment of a completed plan did not advance", result, err, sends)
		}
		released, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || released.PaymentRecord == nil || *released.PaymentRecord != "synthetic-plan-released-transfer" {
			t.Fatal("released payment did not record its processor attempt", released, err)
		}
	})
}
