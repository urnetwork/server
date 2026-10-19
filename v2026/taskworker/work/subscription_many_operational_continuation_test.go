// Many completed row failures cannot pin the real expiry task before its tail.
package work

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// A 128-row failing prefix exceeds one error graph budget before a healthy
// expired tail. The production 256-row boundary must persist on the same failed
// task at the scan cadence, not in backoff, reach the tail next, and revisit
// every unresolved row on the next pass.
func TestCloseExpiredManyOperationalVisitsKeepFailureAndReachNextRawPage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 120*time.Second)
		defer cancel()
		networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-expiry-visit", server.NewId())
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "synthetic-source", "synthetic")
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "synthetic-destination", "synthetic")
		first, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
		server.Raise(err)
		tail, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
		server.Raise(err)
		failedIds := make([]server.Id, 128)
		failedIds[0] = first
		for index := 1; index < len(failedIds); index++ {
			failedIds[index] = server.NewId()
		}
		protectedIds := make([]server.Id, 128)
		for index := range protectedIds {
			protectedIds[index] = server.NewId()
		}
		slices.SortFunc(protectedIds, server.Id.Cmp)
		created := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL WHERE contract_id=$1`, first, created))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL WHERE contract_id=$1`, tail, created.Add(2*time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
				SELECT id,$2,$3,$2,$4,100,true,$5,NULL FROM unnest($1::uuid[]) row(id)`,
				failedIds[1:], networkId, sourceId, destinationId, created))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,
				transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
				SELECT id,$2,$3,$2,$4,$2,100,true,$5,$6 FROM unnest($1::uuid[]) row(id)`,
				protectedIds, networkId, sourceId, destinationId, created.Add(time.Minute), server.NowUtc().Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				SELECT id,party,0,$2,true FROM unnest($1::uuid[]) row(id)
				CROSS JOIN (VALUES ('source'),('destination')) parties(party)`, protectedIds, server.NowUtc()))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
				CREATE SEQUENCE synthetic_task_expiry_proof_attempts;
				CREATE FUNCTION synthetic_task_expiry_proof_failure() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					IF NEW.create_time='%s'::timestamp THEN
						PERFORM nextval('synthetic_task_expiry_proof_attempts');
						RAISE EXCEPTION USING ERRCODE='53200',MESSAGE='synthetic task proof failure';
					END IF;
					RETURN NEW;
				END;
				$$;
				CREATE TRIGGER synthetic_task_expiry_proof_failure
				BEFORE UPDATE OF usage_unverified ON transfer_contract
				FOR EACH ROW WHEN (NOT OLD.usage_unverified AND NEW.usage_unverified)
				EXECUTE FUNCTION synthetic_task_expiry_proof_failure();`, created.Format(time.RFC3339))))
		}, server.TxReadCommitted, server.OptNoRetry())
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { ScheduleCloseExpiredContracts(owner, tx, 0, false) })
		target := NewCloseExpiredContractsTaskTarget()
		readId := func() (id server.Id) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE function_name=$1`, target.TargetFunctionName()).Scan(&id))
			})
			return
		}
		id := readId()
		_, _, _, originalMetadata, _ := readCloseRetryTask(t, ctx, id)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET reschedule_error_count=16 WHERE task_id=$1`, id))
		})
		makeCloseRetryTaskDue(ctx, id)
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		readMetrics := func() (failed, succeeded, databaseErrors float64) {
			families, err := prometheus.DefaultGatherer.Gather()
			server.Raise(err)
			for _, family := range families {
				if family.GetName() != "urnetwork_taskworker_executions_total" && family.GetName() != "urnetwork_taskworker_execution_errors_total" {
					continue
				}
				for _, metric := range family.Metric {
					labels := map[string]string{}
					for _, label := range metric.Label {
						labels[label.GetName()] = label.GetValue()
					}
					if labels["task"] != "work.CloseExpiredContracts" {
						continue
					}
					value := metric.GetCounter().GetValue()
					if family.GetName() == "urnetwork_taskworker_execution_errors_total" && labels["cause"] == "postgres_other" {
						databaseErrors += value
					} else if labels["outcome"] == "failed" {
						failed += value
					} else if labels["outcome"] == "succeeded" {
						succeeded += value
					}
				}
			}
			return
		}
		failedBefore, succeededBefore, causesBefore := readMetrics()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != id {
			t.Fatal("completed failed visit became a success or lost the same retry owner", finished, retried, posts, err)
		}
		args, diagnostic, errorCount, metadata, delay := readCloseRetryTask(t, ctx, id)
		if args.Sweep == nil || args.Sweep.Historical == nil || args.Sweep.Historical.Open == nil ||
			args.Sweep.Historical.Open.ContractId != protectedIds[len(protectedIds)-1] {
			t.Fatal("128 independently completed proof failures pinned the completed production raw page")
		}
		if failed, succeeded, causes := readMetrics(); failed != failedBefore+1 || succeeded != succeededBefore || causes != causesBefore+1 {
			t.Fatal("real many-row checkpoint suppressed its failure metrics", failed, succeeded, causes)
		}
		if errorCount != 17 || !strings.Contains(diagnostic, "synthetic task proof failure") || metadata != originalMetadata ||
			delay < 2*time.Second || 4*time.Second <= delay || len(task.GetFinishedTasks(ctx, id)) != 0 {
			t.Fatal("raw progress changed failure visibility, continuation cadence, identity or success history", errorCount, delay)
		}
		if _, terminal := model.GetContractClose(ctx, tail); terminal {
			t.Fatal("first failed raw page crossed its 256-row boundary")
		}
		makeCloseRetryTaskDue(ctx, id)
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
			t.Fatal("continued scan failed to reach its next raw page", finished, retried, posts, err)
		}
		if close, terminal := model.GetContractClose(ctx, tail); !terminal || close.Outcome != model.ContractOutcomeSettled {
			t.Fatal("persistent failed head starved the healthy expired tail")
		}
		nextId := readId()
		if nextId == id {
			t.Fatal("completed pass failed to publish a new normal scan")
		}
		makeCloseRetryTaskDue(ctx, nextId)
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != nextId {
			t.Fatal("next pass lost the still-unresolved failing head", finished, retried, posts, err)
		}
		_, diagnostic, errorCount, _, _ = readCloseRetryTask(t, ctx, nextId)
		if errorCount != 1 || !strings.Contains(diagnostic, "synthetic task proof failure") {
			t.Fatal("new pass suppressed its repeated original failure")
		}
		if failed, succeeded, causes := readMetrics(); failed != failedBefore+2 || succeeded != succeededBefore+1 || causes != causesBefore+2 {
			t.Fatal("next-pass revisit lost failure or successful-tail execution metrics", failed, succeeded, causes)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var attempts, protected, retained int
			server.Raise(conn.QueryRow(ctx, `SELECT last_value FROM synthetic_task_expiry_proof_attempts`).Scan(&attempts))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract c
				WHERE c.contract_id=ANY($1::uuid[]) AND c.outcome IS NULL AND NOT c.dispute AND NOT c.usage_unverified
				AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent i WHERE i.contract_id=c.contract_id)
				AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal j WHERE j.contract_id=c.contract_id)
				AND NOT EXISTS(SELECT 1 FROM contract_close r WHERE r.contract_id=c.contract_id)`, failedIds).Scan(&retained))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[])
				AND outcome IS NULL AND NOT dispute AND NOT usage_unverified AND expiration_time>statement_timestamp()`, protectedIds).Scan(&protected))
			if attempts != 2*len(failedIds) || retained != len(failedIds) || protected != len(protectedIds) {
				t.Fatal("many-row checkpoint lost failed custody, crossed an active peer, or skipped its next pass", attempts, retained, protected)
			}
		})
	})
}
