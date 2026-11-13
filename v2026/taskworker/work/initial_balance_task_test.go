// The one-shot backfill must expose a failed grant, and retries must not mint twice.
package work

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A returned grant error used to be discarded. Healthy networks still receive
// their grant while the task retains a retryable error for the failed network.
func TestInitialBalanceBackfillRetainsGrantErrorsAndCancellation(t *testing.T) {
	ids := []server.Id{server.NewId(), server.NewId()}
	failure := errors.New("synthetic grant failure")
	calls := 0
	err := backfillInitialTransferBalances(t.Context(), ids, func(context.Context, server.Id) error {
		calls++
		if calls == 1 {
			return failure
		}
		return nil
	})
	if calls != 2 || !errors.Is(err, failure) {
		t.Fatal("failed grant was acknowledged or healthy work was abandoned", calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls = 0
	err = backfillInitialTransferBalances(ctx, ids, func(context.Context, server.Id) error {
		calls++
		cancel()
		return failure
	})
	if calls != 1 || !errors.Is(err, context.Canceled) || !errors.Is(err, failure) {
		t.Fatal("cancellation lost the error or started another grant", calls, err)
	}
}

// Exercise discovery, grant commit, real task handback and replay together.
func TestInitialBalanceBackfillTaskGrantsOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		config := model.Pro()
		previous := *config
		defer func() { *config = previous }()
		config.Free.Data, config.Free.DataPeriod = 100, 24*time.Hour
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-initial-grant", server.NewId())
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		sample := newAsyncMaintenanceCase(BackfillInitialTransferBalance, BackfillInitialTransferBalancePost, &BackfillInitialTransferBalanceArgs{})
		worker := startupClosureWorker(ctx, sample.target)
		defer worker.Close()
		for range 2 {
			id := sample.queue(owner)
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
				t.Fatal("backfill did not complete its real task", finished, retried, posts, err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var count int
				var amount int64
				server.Raise(conn.QueryRow(ctx, `SELECT count(*),COALESCE(sum(start_balance_byte_count),0) FROM transfer_balance WHERE network_id=$1`, networkId).Scan(&count, &amount))
				if count != 1 || amount != 100 {
					t.Fatal("initial grant was absent or duplicated", count, amount)
				}
			})
		}
	})
}
