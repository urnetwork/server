// A classified refusal must not park healthy due rows behind its bounded page.
package work

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// The first real 256-row dispute page contains one underfunded refusal and
// active legacy peers. A healthy absolute-deadline expiry is the next row.
// Observe the ordinary evaluator's durable retry before admitting its next run.
func TestCloseExpiredAccountingContinuationKeepsDueTailPrompt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), time.Minute)
		defer cancel()
		fixture := newCloseRetryFixture(t, ctx)
		var sourceNetworkId, sourceId, destinationNetworkId, destinationId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT source_network_id,source_id,destination_network_id,destination_id
				FROM transfer_contract WHERE contract_id=$1`, fixture.originId).Scan(&sourceNetworkId, &sourceId, &destinationNetworkId, &destinationId))
		})
		healthy, err := model.CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 100)
		server.Raise(err)
		server.Raise(model.CloseContract(ctx, healthy.ContractId, sourceId, 17, true))
		server.Raise(model.CloseContract(ctx, healthy.ContractId, destinationId, 17, true))
		model.SetContractDispute(ctx, healthy.ContractId, true)
		protectedIds := make([]server.Id, 255)
		for index := range protectedIds {
			protectedIds[index] = server.NewId()
		}
		slices.SortFunc(protectedIds, server.Id.Cmp)
		protectedTime := time.Date(2020, time.January, 1, 0, 1, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			// NULL peers remain governed by the quiet period, independently of
			// the healthy tail's immutable deadline and fresh delivered reports.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,
				transfer_byte_count,usage_origin_is_source,create_time,expiration_time,dispute)
				SELECT id,$2,$3,$4,$5,$2,100,true,$6,NULL,true FROM unnest($1::uuid[]) row(id)`,
				protectedIds, sourceNetworkId, sourceId, destinationNetworkId, destinationId, protectedTime))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				SELECT id,party,17,$2,true FROM unnest($1::uuid[]) row(id)
				CROSS JOIN (VALUES ('source'),('destination')) parties(party)`, protectedIds, server.NowUtc()))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=$3 WHERE contract_id=$1`,
				healthy.ContractId, protectedTime.Add(time.Minute), server.NowUtc().Add(-time.Minute)))
		}, server.TxReadCommitted, server.OptNoRetry())
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { ScheduleCloseExpiredContracts(owner, tx, 0, false) })
		target := task.NewTaskTargetWithPost(CloseExpiredContracts, CloseExpiredContractsPost)
		var taskId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `UPDATE pending_task SET run_at=$2,reschedule_error_count=16
				WHERE function_name=$1 RETURNING task_id`, target.TargetFunctionName(), time.Unix(1, 0).UTC()).Scan(&taskId))
		}, server.TxReadCommitted, server.OptNoRetry())
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != taskId {
			t.Fatal("classified expiry page lost its exact retry owner", finished, retried, posts, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var raw, diagnostic string
			var runAt, released time.Time
			var errorCount int
			server.Raise(conn.QueryRow(ctx, `SELECT args_json,run_at,release_time,reschedule_error_count,reschedule_error
				FROM pending_task WHERE task_id=$1`, taskId).Scan(&raw, &runAt, &released, &errorCount, &diagnostic))
			var args CloseExpiredContractsArgs
			server.Raise(json.Unmarshal([]byte(raw), &args))
			if args.Sweep == nil || args.Sweep.Historical == nil || args.Sweep.Historical.Dispute == nil ||
				args.Sweep.Historical.Dispute.ContractId != protectedIds[len(protectedIds)-1] ||
				!args.Sweep.Historical.Dispute.CreateTime.Equal(protectedTime) {
				t.Fatal("classified expiry page lost its completed raw continuation", raw)
			}
			if errorCount != 17 || !strings.Contains(diagnostic, "Escrow does not have enough value") {
				t.Fatal("continuation hid its underfunded accounting refusal", errorCount, diagnostic)
			}
			if delay := runAt.Sub(released); delay < 2*time.Second || 4*time.Second <= delay {
				t.Fatalf("classified expiry continuation parked a due tail for %s; want bounded2–4s retry", delay)
			}
		})
		if _, terminal := model.GetContractClose(ctx, healthy.ContractId); terminal {
			t.Fatal("first bounded page crossed its protected raw head")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=$1`, taskId, time.Unix(1, 0).UTC()))
		}, server.TxReadCommitted, server.OptNoRetry())
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != taskId || len(retried)+len(posts) != 0 {
			t.Fatal("expiry continuation did not finish the healthy tail", finished, retried, posts, err)
		}
		if close, terminal := model.GetContractClose(ctx, healthy.ContractId); !terminal || close.Outcome != model.ContractOutcomeSettled {
			t.Fatal("fresh checkpoints extended the healthy tail's absolute deadline")
		}
		debits, err := model.FlushTransferDebits(ctx, int(fixture.balanceId[15])%model.TransferDebitShardCount, nil, 1)
		if err != nil || debits.Failed != 0 {
			t.Fatal("healthy tail lost its independent debit owner", debits, err)
		}
		fixture.requireAccounting(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var credit, payout model.ByteCount
			var protected int
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, fixture.balanceId).Scan(&credit))
			server.Raise(conn.QueryRow(ctx, `SELECT payout_byte_count FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2`, healthy.ContractId, fixture.balanceId).Scan(&payout))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1)
				AND outcome IS NULL AND dispute AND NOT usage_unverified AND expiration_time IS NULL`, protectedIds).Scan(&protected))
			if credit != 8*fixture.grant-17 || payout != 17 || protected != len(protectedIds) {
				t.Fatal("healthy continuation changed a protected peer or exact delivered debit", credit, payout, protected)
			}
		})
	})
}
