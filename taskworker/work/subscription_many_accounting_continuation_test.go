// Many completed accounting refusals keep durable scan progress and liabilities.
package work

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The production task sees 128 independent underfunded disputes alongside one
// verified close and 255 protected raw rows. Their complete accounting witness
// must persist the 256-row open cursor, reach the healthy tail next, and revisit
// every still-reserved dispute on the next pass without changing money or reports.
func TestCloseExpiredManyAccountingRejectionsKeepCursorCountsAndCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		oldBase, oldCap := task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout
		task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = 2*time.Second, time.Hour
		t.Cleanup(func() { task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = oldBase, oldCap })
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 5*time.Minute)
		defer cancel()
		fixtures := make([]closeRetryFixture, 128)
		companionIds := make([]server.Id, len(fixtures))
		for index := range fixtures {
			fixtures[index] = newCloseRetryFixture(t, ctx)
			companionIds[index] = fixtures[index].companionId
		}
		// Join the independent zero-debit origins before the target starts.
		// This leaves one grant owner per rejected row and cannot introduce a
		// scheduling-dependent ownership-busy error into the accounting page.
		cutoff := server.NowUtc().Add(-time.Hour)
		count, _, err := model.ForceCloseOpenContractIdsPage(ctx, cutoff, 256, 8, 1, 0,
			&model.ContractExpiryCursor{ScanBefore: cutoff, DisputeDone: true})
		if err != nil || count != int64(len(fixtures)) {
			t.Fatal("synthetic origin preparation failed", count, err)
		}
		drainCloseRetryOriginDebits(t, ctx, fixtures...)
		for _, fixture := range fixtures {
			fixture.requireAccounting(t, ctx)
		}
		readAccounting := func() (snapshot string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(
					c.contract_id,c.dispute,c.outcome,c.open,
					e.settled,e.redis_reserved,e.balance_byte_count,e.payout_byte_count,b.balance_byte_count,
					(SELECT jsonb_agg(jsonb_build_array(party,used_transfer_byte_count,close_time,checkpoint) ORDER BY party)
					 FROM contract_close WHERE contract_id=c.contract_id),
					(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=c.contract_id),
					(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=c.contract_id)
				) ORDER BY c.contract_id)::text
				FROM transfer_contract c JOIN transfer_escrow e USING(contract_id)
				JOIN transfer_balance b USING(balance_id) WHERE c.contract_id=ANY($1::uuid[])`, companionIds).Scan(&snapshot))
			})
			return
		}
		beforeAccounting := readAccounting()
		networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-many-accounting-tail", server.NewId())
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "synthetic-source", "synthetic")
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "synthetic-destination", "synthetic")
		first, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
		server.Raise(err)
		tail, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
		server.Raise(err)
		protectedIds := make([]server.Id, 255)
		for index := range protectedIds {
			protectedIds[index] = server.NewId()
		}
		slices.SortFunc(protectedIds, server.Id.Cmp)
		created := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL WHERE contract_id=$1`, first, created.Add(30*time.Second)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL WHERE contract_id=$1`, tail, created.Add(2*time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
				SELECT id,$2,$3,$2,$4,100,true,$5,$6 FROM unnest($1::uuid[]) row(id)`,
				protectedIds, networkId, sourceId, destinationId, created.Add(time.Minute), server.NowUtc().Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				SELECT id,party,0,$2,true FROM unnest($1::uuid[]) row(id)
				CROSS JOIN (VALUES ('source'),('destination')) parties(party)`, protectedIds, server.NowUtc()))
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { ScheduleCloseExpiredContracts(owner, tx, 0, false) })
		target := &closeRetryObservedTarget{
			Target: task.NewTaskTargetWithPost(CloseExpiredContracts, CloseExpiredContractsPost), observed: make(chan error, 1),
		}
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
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		var retainedProofs string
		requireRejections := func(verified int64) {
			var observed error
			select {
			case observed = <-target.observed:
			default:
				t.Fatal("real accounting target did not return its failure")
			}
			var accounting *model.ForceCloseAccountingError
			if !errors.As(observed, &accounting) || accounting.VerifiedCloseCount() != verified ||
				accounting.AccountingRejectionCount() != int64(len(fixtures)) || accounting.QuarantinedAccountingRejectionCount() != 0 ||
				strings.Count(observed.Error(), "Escrow does not have enough value") != len(fixtures) {
				t.Fatal("many-row accounting receipt changed verified, rejected or quarantined counts", observed)
			}
			for _, id := range companionIds {
				if !strings.Contains(observed.Error(), id.String()) {
					t.Fatal("completed page omitted a protected accounting refusal")
				}
			}
			if readAccounting() != beforeAccounting {
				t.Fatal("many-row checkpoint changed reserved accounting, reports, credit or custody")
			}
			// Ordinary expiry commits original usage proof before trying the
			// disputed financial close. Require that exact legitimate first
			// transition, then preserve the same bytes across the next pass.
			var proofs string
			var proved int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE c.usage_unverified
					AND c.provider_usage->>'version'='1' AND c.provider_usage->'expiry'=jsonb_build_object(
						'capacity',c.transfer_byte_count,'reports',
						(SELECT jsonb_object_agg(party,jsonb_build_object('byte_count',used_transfer_byte_count,'checkpoint',checkpoint))
						 FROM contract_close WHERE contract_id=c.contract_id))),
					jsonb_agg(jsonb_build_array(c.contract_id,c.usage_unverified,c.provider_usage) ORDER BY c.contract_id)::text
					FROM transfer_contract c WHERE c.contract_id=ANY($1::uuid[])`, companionIds).Scan(&proved, &proofs))
			})
			if proved != len(fixtures) || retainedProofs != "" && proofs != retainedProofs {
				t.Fatal("accounting revisit lost or rewrote the exact retained original-report proof", proved)
			}
			retainedProofs = proofs
		}
		makeCloseRetryTaskDue(ctx, id)
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != id {
			t.Fatal("many accounting refusals lost the same failing task", finished, retried, posts, err)
		}
		args, diagnostic, errorCount, metadata, delay := readCloseRetryTask(t, ctx, id)
		if args.Sweep == nil || args.Sweep.Historical == nil || args.Sweep.Historical.Open == nil ||
			args.Sweep.Historical.Open.ContractId != protectedIds[len(protectedIds)-1] || !args.Sweep.Historical.DisputeDone {
			t.Fatal("128 independently completed accounting refusals pinned the completed production raw page")
		}
		requireRejections(1)
		if errorCount != 17 || !strings.Contains(diagnostic, "Escrow does not have enough value") || metadata != originalMetadata ||
			delay < 2*time.Second || 4*time.Second <= delay || len(task.GetFinishedTasks(ctx, id)) != 0 {
			t.Fatal("many-row accounting progress changed task identity, failure visibility or bounded cadence", errorCount, delay)
		}
		if close, terminal := model.GetContractClose(ctx, first); !terminal || close.Outcome != model.ContractOutcomeSettled {
			t.Fatal("many refused peers prevented the independently verified first-page close")
		}
		if _, terminal := model.GetContractClose(ctx, tail); terminal {
			t.Fatal("accounting first page crossed its production raw boundary")
		}
		makeCloseRetryTaskDue(ctx, id)
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
			t.Fatal("accounting cursor did not reach the next healthy raw page", finished, retried, posts, err)
		}
		if observed := <-target.observed; observed != nil {
			t.Fatal("healthy accounting tail unexpectedly failed", observed)
		}
		if close, terminal := model.GetContractClose(ctx, tail); !terminal || close.Outcome != model.ContractOutcomeSettled {
			t.Fatal("many accounting refusals starved the healthy expired tail")
		}
		nextId := readId()
		if nextId == id {
			t.Fatal("successful tail did not publish the ordinary next pass")
		}
		makeCloseRetryTaskDue(ctx, nextId)
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != nextId {
			t.Fatal("next pass lost still-reserved accounting rows", finished, retried, posts, err)
		}
		requireRejections(0)
		_, diagnostic, errorCount, _, delay = readCloseRetryTask(t, ctx, nextId)
		if errorCount != 1 || strings.Count(diagnostic, "Escrow does not have enough value") != len(fixtures) ||
			delay < time.Minute || 5*time.Minute <= delay {
			t.Fatal("revisited idle accounting page lost its failure count, diagnostics or normal cadence", errorCount, delay)
		}
		for _, fixture := range fixtures {
			fixture.requireAccounting(t, ctx)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var protected int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[])
				AND outcome IS NULL AND NOT dispute AND NOT usage_unverified AND expiration_time>statement_timestamp()`, protectedIds).Scan(&protected))
			if protected != len(protectedIds) {
				t.Fatal("accounting continuation changed an active protected row")
			}
		})
	})
}
