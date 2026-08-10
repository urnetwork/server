package work

// Hourly payment reconciliation -- the lost-webhook safety net (UPGRADE.md
// §8). The task is a thin shell: the store iteration, budgets, audit rows,
// and both repair directions live in controller.RunPaymentReconciliation.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type PaymentReconcileArgs struct {
}

type PaymentReconcileResult struct {
	RunId         server.Id `json:"run_id"`
	Credited      int       `json:"credited"`
	Ended         int       `json:"ended"`
	Errors        int       `json:"errors"`
	SkippedStores []string  `json:"skipped_stores,omitempty"`
}

func SchedulePaymentReconcile(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		PaymentReconcile,
		&PaymentReconcileArgs{},
		clientSession,
		task.RunOnce("payment_reconciliation"),
		task.RunAt(server.NowUtc().Add(1*time.Hour)),
		task.MaxTime(30*time.Minute),
	)
}

func PaymentReconcile(
	paymentReconcile *PaymentReconcileArgs,
	clientSession *session.ClientSession,
) (*PaymentReconcileResult, error) {
	result, err := controller.RunPaymentReconciliation(clientSession)
	if err != nil {
		// includes ErrPaymentReconcileRunInProgress when a manual
		// `bringyourctl payments reconcile` run holds the run-level advisory
		// lock: the task errors and the task system reschedules it with
		// backoff, retrying after the manual run finishes
		return nil, err
	}
	glog.Infof(
		"[reconcile]run %s: credited %d, ended %d, errors %d, skipped %v\n",
		result.RunId, result.Credited, result.Ended, result.Errors, result.SkippedStores,
	)
	return &PaymentReconcileResult{
		RunId:         result.RunId,
		Credited:      result.Credited,
		Ended:         result.Ended,
		Errors:        result.Errors,
		SkippedStores: result.SkippedStores,
	}, nil
}

func PaymentReconcilePost(
	paymentReconcile *PaymentReconcileArgs,
	paymentReconcileResult *PaymentReconcileResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	SchedulePaymentReconcile(clientSession, tx)
	return nil
}
