// Requested expiry wakes retain observed deadlines without bypassing owners.
package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Synthetic source-only rows retain recent checkpoints until their hard
// deadline, including a disputed NULL-expiration row and a reportless row.
func newCloseDeadlineContracts(t testing.TB, ctx context.Context) []server.Id {
	t.Helper()
	networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "synthetic-expiry-deadline", server.NewId())
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "synthetic-source", "synthetic")
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "synthetic-destination", "synthetic")
	ids := make([]server.Id, 3)
	for index := range ids {
		id, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
		server.Raise(err)
		if index < 2 {
			server.Raise(model.CloseContract(ctx, id, sourceId, 17, true))
		}
		ids[index] = id
	}
	return ids
}

// The real task and evaluator must request the exact earlier deadline instead
// of random 1–5 minute polling. No sleep makes a live row eligible: the second
// run advances only the synthetic persisted expiration and normal task clock.
func TestCloseExpiredDeadlineRequestsExactWakeThenNormalClosure(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		ids := newCloseDeadlineContracts(t, ctx)
		now := server.NowUtc().Truncate(time.Microsecond)
		deadline := now.Add(45 * time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, ids[0], deadline))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL,create_time=$2,dispute=true WHERE contract_id=$1`,
				ids[1], deadline.Add(5*time.Second-time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, ids[2], deadline.Add(10*time.Second)))
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { ScheduleCloseExpiredContracts(owner, tx, 0, false) })
		key := task.RunOnce("close_expired_contracts_1_0").String()
		initial, found := readExpiryRecoveryQueue(t, ctx)[key]
		if !found {
			t.Fatal("normal expiry owner was not scheduled")
		}
		makeCloseRetryTaskDue(ctx, initial.id)
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(task.NewTaskTargetWithPost(CloseExpiredContracts, CloseExpiredContractsPost))
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != initial.id || len(retried)+len(posts) != 0 {
			t.Fatal("live expiry scan failed its ordinary completion", err)
		}
		row, found := readExpiryRecoveryQueue(t, ctx)[key]
		var args CloseExpiredContractsArgs
		if !found || !row.runAt.Equal(deadline) || json.Unmarshal([]byte(row.args), &args) != nil ||
			args.Cursor != nil || args.Sweep != nil || args.NextExpiration != nil {
			t.Fatal("EOF did not request the observed deadline and consume its pass hint")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var protected int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[])
				AND outcome IS NULL AND NOT usage_unverified AND provider_usage IS NULL`, ids).Scan(&protected))
			if protected != len(ids) {
				t.Fatal("scheduling a future wake changed live contract custody")
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=CASE WHEN expiration_time IS NULL THEN NULL ELSE $2::timestamp END,
				create_time=$3 WHERE contract_id=ANY($1::uuid[])`, ids, now.Add(-time.Minute), now.Add(-61*time.Minute)))
		})
		makeCloseRetryTaskDue(ctx, row.id)
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != row.id || len(retried)+len(posts) != 0 {
			t.Fatal("due live rows did not return through the normal close owner", err)
		}
		for _, id := range ids {
			if closed, terminal := model.GetContractClose(ctx, id); !terminal || closed.Outcome != model.ContractOutcomeSettled {
				t.Fatal("observed deadline row did not settle through ordinary expiry")
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var financial int
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1::uuid[]))+
				(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=ANY($1::uuid[]))+
				(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=ANY($1::uuid[]))`, ids).Scan(&financial))
			if financial != 0 {
				t.Fatal("source-only deadline closure created financial records")
			}
		})
		row, found = readExpiryRecoveryQueue(t, ctx)[key]
		if !found || json.Unmarshal([]byte(row.args), &args) != nil || args.NextExpiration != nil || !row.runAt.After(server.NowUtc()) {
			t.Fatal("consumed expiration kept the completed empty pass spinning")
		}
	})
}

// The existing RunOnce owner keeps both its fixed cursor and an earlier wake.
// Once EOF consumes an overdue observation, a new head pass must forget it.
func TestCloseExpiredDeadlineKeepsRunOnceMinimumAndConsumesOverdueHint(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		now := server.NowUtc().Truncate(time.Microsecond)
		early, later := now.Add(20*time.Second), now.Add(40*time.Second)
		cursor := &model.ContractExpiryCursor{ScanBefore: now.Add(-time.Hour), DisputeDone: true}
		sweep := &model.ContractExpirySweepCursor{Historical: cursor, RecentAfter: cursor.ScanBefore}
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleCloseExpiredContractsPageWithExpiration(owner, tx, 0, true, cursor, sweep, &early)
		})
		key := task.RunOnce("close_expired_contracts_1_0").String()
		first := readExpiryRecoveryQueue(t, ctx)[key]
		var saved CloseExpiredContractsArgs
		if json.Unmarshal([]byte(first.args), &saved) != nil || saved.NextExpiration == nil || !saved.NextExpiration.Equal(early) ||
			saved.Sweep == nil || !saved.Sweep.RecentAfter.Equal(cursor.ScanBefore) || !first.runAt.Equal(early) {
			t.Fatal("bounded continuation lost its exact deadline or fixed epoch")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleCloseExpiredContractsPageWithExpiration(owner, tx, 0, true, nil, nil, &later)
		})
		merged := readExpiryRecoveryQueue(t, ctx)[key]
		if merged.id != first.id || merged.args != first.args || !merged.runAt.Equal(early) || merged.generation != first.generation+1 {
			t.Fatal("later idle publication replaced custody or postponed the earliest RunOnce wake")
		}
		task.RemovePendingTask(ctx, first.id)
		overdue := now.Add(-time.Hour)
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(CloseExpiredContractsPost(&CloseExpiredContractsArgs{BlockSize: 1},
				&CloseExpiredContractsResult{NextExpiration: &overdue}, owner, tx))
		})
		after := server.NowUtc()
		queued := readExpiryRecoveryQueue(t, ctx)[key]
		var next CloseExpiredContractsArgs
		if json.Unmarshal([]byte(queued.args), &next) != nil || next.NextExpiration != nil || next.Sweep != nil || next.Cursor != nil ||
			queued.runAt.Before(before) || queued.runAt.After(after) {
			t.Fatal("overdue observation did not request one immediate head pass")
		}
		// Persisted EOF retries may still contain the old observation. The
		// normal head pass discards it even if no row supplies a replacement.
		result, err := CloseExpiredContracts(&CloseExpiredContractsArgs{BlockSize: 1, NextExpiration: &overdue}, owner)
		if err != nil || result.NextExpiration != nil || result.Full || result.Cursor != nil || result.Sweep != nil {
			t.Fatal("new head pass reused an expired inherited hint", err)
		}
	})
}

// Accounting EOF remains a real failed task with its existing retry cadence.
// Its observed deadline must survive durable args, rather than being silently
// dropped by the separate accounting checkpoint constructor.
func TestCloseExpiredDeadlineAccountingEofRetainsHintAndFailure(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		fixture := newCloseRetryFixture(t, ctx)
		ids := newCloseDeadlineContracts(t, ctx)
		deadline := server.NowUtc().Truncate(time.Microsecond).Add(45 * time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=ANY($1::uuid[])`, ids, deadline))
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { ScheduleCloseExpiredContracts(owner, tx, 0, false) })
		key := task.RunOnce("close_expired_contracts_1_0").String()
		initial := readExpiryRecoveryQueue(t, ctx)[key]
		makeCloseRetryTaskDue(ctx, initial.id)
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(task.NewTaskTargetWithPost(CloseExpiredContracts, CloseExpiredContractsPost))
		finished, retried, posts, err := worker.EvalTasks(1)
		args, diagnostic, errorCount, _, delay := readCloseRetryTask(t, ctx, initial.id)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != initial.id ||
			args.Cursor != nil || args.Sweep != nil || args.NextExpiration == nil || !args.NextExpiration.Equal(deadline) ||
			diagnostic == "" || errorCount != 1 || delay < time.Minute || 5*time.Minute <= delay {
			t.Fatal("accounting EOF lost its exact deadline, failure or ordinary retry policy", err, delay)
		}
		drainCloseRetryOriginDebits(t, ctx, fixture)
		fixture.requireAccounting(t, ctx)
	})
}
