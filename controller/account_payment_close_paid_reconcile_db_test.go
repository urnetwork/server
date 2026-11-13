// The sn-reconcile census and a keyed payment the USDC close-out closed. Every
// network, wallet and payment is synthetic.
package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The census reads canceled keyed payments as stuck rows (RC-1), and a send
// revives one: it un-cancels it and clears its key. A keyed payment the
// close-out closed (canceled, with its usdc_payout_closed event) had its
// obligation paid another way, so it is never a stuck row; the same payment
// without the event still is.
func TestSnReconcileCensusSkipsAClosedKeyedPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newCloseParkedTestFixture(t)
		networkId, walletId, _ := f.network("MATIC")
		created := server.NowUtc().Add(-20 * 24 * time.Hour)
		closedId := f.pending(networkId, &walletId, created, model.UsdToNanoCents(5))
		f.set(closedId, `circle_idempotency_key=$2,canceled=true,cancel_time=$3`, server.NewId(), created.Add(time.Hour))
		f.closedBefore(closedId)
		unclosedId := f.pending(networkId, &walletId, created, model.UsdToNanoCents(6))
		f.set(unclosedId, `circle_idempotency_key=$2,canceled=true,cancel_time=$3`, server.NewId(), created.Add(time.Hour))

		rows, err := model.GetSnReconcileStuckRows(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		stuck := map[server.Id]bool{}
		for _, row := range rows {
			stuck[row.PaymentId] = true
		}
		if stuck[closedId] {
			t.Fatal("a keyed payment the close-out closed is an sn-reconcile stuck row")
		}
		if !stuck[unclosedId] {
			t.Fatal("a canceled keyed payment without a closure is not a stuck row")
		}
	})
}
