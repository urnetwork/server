package work

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The startup task and the pending sweep reach the same admission. Neither
// reinterprets a queued payment's earning time as the task's execution time.
func TestProviderTransitionStartupPreservesPending(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		owner := session.Testing_CreateClientSession(ctx, nil)
		t.Cleanup(owner.Cancel)
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		policy := fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", cutoff.Format(time.RFC3339), "0x"+strings.Repeat("11", 32))
		t.Cleanup(server.Config.PushSimpleResource("sn.yml", []byte(policy)))
		policyForPreparation, preparationErr := server.LoadProviderPayoutTransition(ctx)
		if preparationErr != nil {
			t.Fatal(preparationErr)
		}
		if _, err := server.PrepareProviderPayoutBoundary(ctx, policyForPreparation.ConfigSha256); err != nil {
			t.Fatal(err)
		}
		// This deliberately unbound prior payment cannot be newly sent, but
		// the task pipeline must keep it for attribution/reconciliation.
		paymentId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,payout_byte_count,payout_nano_cents,min_sweep_time)
		VALUES($1,$2,$3,0,100,$4)`, paymentId, server.NewId(), server.NewId(), cutoff.Add(-time.Hour)))
		})
		if _, err := ProcessPendingPayouts(&ProcessPendingPayoutsArgs{}, owner); err != nil {
			t.Fatal(err)
		}
		controller.SchedulePendingPayments(owner)
		payment, err := model.GetPayment(ctx, paymentId)
		if err != nil || payment == nil || payment.Completed || payment.Canceled || payment.PaymentRecord != nil {
			t.Fatal("task startup discarded or paid unresolved liability")
		}
	})
}
