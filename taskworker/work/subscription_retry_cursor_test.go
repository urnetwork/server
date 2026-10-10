// Keeps completed accounting-error pages moving without releasing their debt.
package work

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Orders two fully quiet, protected disputed rows for one-row page controls.
func newCloseRetryCursorPair(t testing.TB, ctx context.Context) (closeRetryFixture, closeRetryFixture) {
	t.Helper()
	first, second := newCloseRetryFixture(t, ctx), newCloseRetryFixture(t, ctx)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$3 WHERE contract_id IN ($1,$2)`,
			second.originId, second.companionId, time.Date(2020, time.January, 1, 0, 0, 1, 0, time.UTC)))
	})
	return first, second
}

// Reads an exact synthetic task and compares the identity metadata the retry
// must preserve. Arguments and failure scheduling are checked independently.
func readCloseRetryTask(t testing.TB, ctx context.Context, id server.Id) (CloseExpiredContractsArgs, string, int, string, time.Duration) {
	t.Helper()
	var raw, storedError, metadata string
	var errorCount int
	var runAt, released time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT args_json,coalesce(reschedule_error,''),reschedule_error_count,
            jsonb_build_array(function_name,run_once_key,run_priority,run_max_time_seconds,client_address_hash,client_address_port,client_by_jwt_json)::text,
            run_at,release_time FROM pending_task WHERE task_id=$1`, id).Scan(&raw, &storedError, &errorCount, &metadata, &runAt, &released))
	})
	var args CloseExpiredContractsArgs
	if json.Unmarshal([]byte(raw), &args) != nil {
		t.Fatal("retry wrote invalid arguments")
	}
	return args, storedError, errorCount, metadata, runAt.Sub(released)
}

// Advances test scheduling through durable state, never through sleeps.
func makeCloseRetryTaskDue(ctx context.Context, id server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=$1`, id, time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)))
	})
}

// The reportless Redis sibling owns a zero-byte debit until its real worker
// releases that reservation. Keep the companion reader purely observational:
// assert both reservations before this explicit handoff and one after replay.
func drainCloseRetryOriginDebits(t testing.TB, ctx context.Context, fixtures ...closeRetryFixture) {
	t.Helper()
	readCompanion := func(fixture closeRetryFixture) (snapshot string) {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_array(c.dispute,c.outcome,c.open,c.usage_unverified,c.provider_usage,
				e.settled,e.balance_byte_count,e.payout_byte_count,
				(SELECT jsonb_agg(jsonb_build_array(party,used_transfer_byte_count,close_time,checkpoint) ORDER BY party)
				 FROM contract_close WHERE contract_id=c.contract_id))::text
				FROM transfer_contract c JOIN transfer_escrow e USING(contract_id)
				WHERE c.contract_id=$1 AND e.balance_id=$2`, fixture.companionId, fixture.balanceId).Scan(&snapshot))
		})
		return
	}
	requireOrigin := func(fixture closeRetryFixture, settled bool) {
		shard := int(fixture.balanceId[15]) % model.TransferDebitShardCount
		var terminal, metadataSettled, redisReserved, exactJournal bool
		var reserved, payout, credit, debit model.ByteCount
		var journals int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT c.outcome=$3 AND NOT c.dispute AND NOT c.open,
				e.settled,e.redis_reserved,e.balance_byte_count,COALESCE(e.payout_byte_count,0),b.balance_byte_count,
				(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1),
				(SELECT COALESCE(sum(debit_byte_count),0) FROM transfer_debit_journal WHERE contract_id=$1),
				(SELECT COALESCE(bool_and(balance_id=$2 AND shard=$4 AND NOT applied AND debit_byte_count=0),true)
				 FROM transfer_debit_journal WHERE contract_id=$1)
				FROM transfer_contract c JOIN transfer_escrow e USING(contract_id)
				JOIN transfer_balance b USING(balance_id) WHERE c.contract_id=$1 AND e.balance_id=$2`,
				fixture.originId, fixture.balanceId, model.ContractOutcomeSettled, shard).Scan(
				&terminal, &metadataSettled, &redisReserved, &reserved, &payout, &credit, &journals, &debit, &exactJournal))
		})
		wantJournals, wantReserved := 1, 2*fixture.grant
		if settled {
			wantJournals, wantReserved = 0, fixture.grant
		}
		if !terminal || metadataSettled != settled || !redisReserved || reserved != fixture.grant || payout != 0 ||
			credit != 8*fixture.grant || journals != wantJournals || debit != 0 || !exactJournal ||
			model.Testing_NetEscrowByteCount(ctx, fixture.balanceId) != wantReserved {
			t.Fatal("reportless origin lost its exact zero-debit reservation owner", settled, journals, debit, credit)
		}
	}
	protected := map[server.Id]string{}
	for _, fixture := range fixtures {
		protected[fixture.companionId] = readCompanion(fixture)
		requireOrigin(fixture, false)
	}
	for pass := range 2 {
		// Join every observed origin before replay, so a bounded public
		// page cannot legitimately advance to another fixture's pending debt.
		for _, fixture := range fixtures {
			previous := fixture.balanceId
			for index := len(previous) - 1; ; index-- {
				if index < 0 {
					t.Fatal("synthetic balance has no preceding UUID")
				}
				if previous[index] != 0 {
					previous[index]--
					break
				}
				previous[index] = 255
			}
			shard := int(fixture.balanceId[15]) % model.TransferDebitShardCount
			result, err := model.FlushTransferDebits(ctx, shard, &previous, 1)
			if err != nil || result.Failed != 0 || result.Busy != 0 {
				t.Fatal("exact reportless debit owner or replay failed", result, err)
			}
			if pass == 0 {
				if result.Balances != 1 || result.Applied != 1 || result.Released != 1 || !result.More ||
					result.LastBalanceId == nil || *result.LastBalanceId != fixture.balanceId {
					t.Fatal("reportless debit page lost its exact balance or release", result)
				}
			} else if result.Applied != 0 || result.Released != 0 {
				t.Fatal("empty reportless debit replay repeated financial work", result)
			}
			requireOrigin(fixture, true)
			if readCompanion(fixture) != protected[fixture.companionId] {
				t.Fatal("reportless sibling debit changed the disputed companion")
			}
		}
	}
}

// The exact model and scheduler traverse two rejected pages, finish the empty
// tail and return to the protected head. Only normal completion invokes Post.
func TestCloseAccountingRetryPersistsContinuationAndReturnsToHead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first, second := newCloseRetryCursorPair(t, ctx)
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		var calls, postCalls atomic.Int32
		var page task.TaskFunction[*CloseExpiredContractsArgs, *CloseExpiredContractsResult]
		page = func(args *CloseExpiredContractsArgs, clientSession *session.ClientSession) (*CloseExpiredContractsResult, error) {
			calls.Add(1)
			c, next, err := model.ForceCloseOpenContractIdsPage(clientSession.Ctx, server.NowUtc().Add(-time.Hour), 1, 1, 1, 0, args.Cursor)
			return closeExpiredContractsPageResult(clientSession.Ctx, args, c, next, err)
		}
		post := func(args *CloseExpiredContractsArgs, result *CloseExpiredContractsResult, clientSession *session.ClientSession, tx server.PgTx) error {
			postCalls.Add(1)
			task.ScheduleTaskInTx(tx, page, &CloseExpiredContractsArgs{BlockSize: args.BlockSize, BlockIndex: args.BlockIndex, Cursor: result.Cursor}, clientSession, task.RunOnce("synthetic-retry-cursor"))
			return nil
		}
		target := task.NewTaskTargetWithPost(page, post)
		task.ScheduleTask(page, &CloseExpiredContractsArgs{BlockSize: 1, BlockIndex: 0, Cursor: nil}, clientSession, task.RunOnce("synthetic-retry-cursor"))
		worker := task.NewTaskWorkerWithDefaults(ctx)
		worker.AddTargets(target)
		readId := func() server.Id {
			var id server.Id
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`, `["synthetic-retry-cursor"]`).Scan(&id))
			})
			return id
		}
		id := readId()
		_, _, _, metadata, _ := readCloseRetryTask(t, ctx, id)
		for attempt, expected := range []server.Id{first.companionId, second.companionId} {
			makeCloseRetryTaskDue(ctx, id)
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 0 || len(retried) != 1 || retried[0] != id || len(posts) != 0 {
				t.Fatal("accounting page lost its failing durable task")
			}
			args, storedError, count, currentMetadata, delay := readCloseRetryTask(t, ctx, id)
			if args.Cursor == nil || args.Cursor.Dispute == nil || args.Cursor.Dispute.ContractId != expected || !strings.Contains(storedError, expected.String()) || count != attempt+1 || currentMetadata != metadata {
				t.Fatal("accounting retry lost cursor progress, error or identity")
			}
			if delay < 2*time.Second || 4*time.Second <= delay {
				t.Fatalf("completed raw continuation retry=%s, want bounded2–4s cadence", delay)
			}
		}
		drainCloseRetryOriginDebits(t, ctx, first, second)
		first.requireAccounting(t, ctx)
		second.requireAccounting(t, ctx)
		if calls.Load() != 2 || postCalls.Load() != 0 || len(task.GetFinishedTasks(ctx, id)) != 0 {
			t.Fatal("retry checkpoint reran model work or invoked a success post")
		}

		makeCloseRetryTaskDue(ctx, id)
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried) != 0 || len(posts) != 0 || postCalls.Load() != 1 {
			t.Fatal("empty pass tail did not finish through the normal post")
		}
		nextId := readId()
		args, _, count, _, _ := readCloseRetryTask(t, ctx, nextId)
		if nextId == id || args.Cursor != nil || count != 0 {
			t.Fatal("new pass did not reset to the protected head")
		}
		makeCloseRetryTaskDue(ctx, nextId)
		_, retried, _, err = worker.EvalTasks(1)
		_, storedError, _, _, _ := readCloseRetryTask(t, ctx, nextId)
		if err != nil || len(retried) != 1 || !strings.Contains(storedError, first.companionId.String()) || calls.Load() != 4 || postCalls.Load() != 1 {
			t.Fatal("protected first dispute was skipped on the next pass")
		}
		first.requireAccounting(t, ctx)
		second.requireAccounting(t, ctx)
	})
}

// A short last page can itself fail accounting. Its nil continuation must be
// persisted on the same failing task rather than retaining the prior position.
func TestCloseAccountingRetryResetsCursorOnRejectedPassEnd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first, second := newCloseRetryCursorPair(t, ctx)
		_, cursor, err := model.ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-time.Hour), 1, 1, 1, 0, nil)
		if err == nil || cursor == nil {
			t.Fatal("first protected page did not provide a continuation")
		}
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		page := func(args *CloseExpiredContractsArgs, clientSession *session.ClientSession) (*CloseExpiredContractsResult, error) {
			c, next, err := model.ForceCloseOpenContractIdsPage(clientSession.Ctx, server.NowUtc().Add(-time.Hour), 3, 1, 1, 0, args.Cursor)
			return closeExpiredContractsPageResult(clientSession.Ctx, args, c, next, err)
		}
		target := task.NewTaskTarget(page)
		task.ScheduleTask(page, &CloseExpiredContractsArgs{BlockSize: 1, BlockIndex: 0, Cursor: cursor}, clientSession, task.RunOnce("synthetic-retry-end"))
		var id server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`, `["synthetic-retry-end"]`).Scan(&id))
		})
		worker := task.NewTaskWorkerWithDefaults(ctx)
		worker.AddTargets(target)
		for attempt := 0; attempt < 2; attempt++ {
			makeCloseRetryTaskDue(ctx, id)
			finished, retried, _, err := worker.EvalTasks(1)
			args, storedError, count, _, _ := readCloseRetryTask(t, ctx, id)
			if err != nil || len(finished) != 0 || len(retried) != 1 || retried[0] != id || args.Cursor != nil || count != attempt+1 || !strings.Contains(storedError, second.companionId.String()) {
				t.Fatal("rejected last page failed to reset the same task to the head")
			}
			if attempt == 1 && !strings.Contains(storedError, first.companionId.String()) {
				t.Fatal("reset failed to revisit the earlier protected dispute")
			}
		}
		drainCloseRetryOriginDebits(t, ctx, first, second)
		first.requireAccounting(t, ctx)
		second.requireAccounting(t, ctx)
	})
}

// A returned cursor alone is never authority: canceled, joined and inconsistent
// model evidence must leave the failing task's prior arguments unchanged.
func TestCloseAccountingRetryRejectsAmbiguousProgress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first, _ := newCloseRetryCursorPair(t, ctx)
		c, next, accountingErr := model.ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-time.Hour), 1, 1, 1, 0, nil)
		if accountingErr == nil || next == nil {
			t.Fatal("control did not produce a completed accounting-error page")
		}
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		var scenario int
		page := func(args *CloseExpiredContractsArgs, clientSession *session.ClientSession) (*CloseExpiredContractsResult, error) {
			count, err, pageCtx := c, accountingErr, clientSession.Ctx
			switch scenario {
			case 0:
				err = errors.Join(accountingErr, errors.New("synthetic operational failure"))
			case 1:
				count++
			case 2:
				var stop context.CancelFunc
				pageCtx, stop = context.WithCancel(pageCtx)
				stop()
			case 3:
				result, err := closeExpiredContractsPageResult(pageCtx, args, count, next, err)
				clientSession.Cancel()
				return result, err
			}
			return closeExpiredContractsPageResult(pageCtx, args, count, next, err)
		}
		target := task.NewTaskTarget(page)
		task.ScheduleTask(page, &CloseExpiredContractsArgs{BlockSize: 1, BlockIndex: 0, Cursor: nil}, clientSession, task.RunOnce("synthetic-retry-ambiguous"))
		var id server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`, `["synthetic-retry-ambiguous"]`).Scan(&id))
		})
		worker := task.NewTaskWorkerWithDefaults(ctx)
		worker.AddTargets(target)
		for scenario = 0; scenario < 4; scenario++ {
			makeCloseRetryTaskDue(ctx, id)
			finished, retried, _, err := worker.EvalTasks(1)
			args, _, count, _, _ := readCloseRetryTask(t, ctx, id)
			if err != nil || len(finished) != 0 || len(retried) != 1 || retried[0] != id || args.Cursor != nil || count != scenario+1 {
				t.Fatal("ambiguous or canceled result advanced a task cursor")
			}
		}
		drainCloseRetryOriginDebits(t, ctx, first)
		first.requireAccounting(t, ctx)
	})
}
