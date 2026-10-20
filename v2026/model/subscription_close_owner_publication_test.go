// Exact registration and publication retain one committed financial handoff.
package model

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

func retainMissingCloseOwnerHints(t testing.TB, ctx context.Context, id server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
			SET payer_network_id=NULL,source_client_id=NULL,next_attempt_time=$2 WHERE contract_id=$1`,
			id, server.NowUtc().Add(-time.Hour)))
		var missing bool
		server.Raise(tx.QueryRow(ctx, `SELECT payer_network_id IS NULL AND source_client_id IS NULL
			AND next_attempt_time<clock_timestamp() AT TIME ZONE 'UTC'
			FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&missing))
		if !missing {
			t.Fatal("retained intent fixture did not preserve both missing hints and due state")
		}
	}, server.TxReadCommitted, server.OptNoRetry())
}

func exactClosePublicationState(t testing.TB, ctx context.Context, id server.Id, owner ContractCloseOwner) (registered, queued bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
			EXISTS(SELECT 1 FROM legacy_settlement_intent i JOIN transfer_contract c USING(contract_id)
				WHERE i.contract_id=$1 AND i.payer_network_id IS NOT DISTINCT FROM c.payer_network_id
				AND i.source_client_id=c.source_id),
			EXISTS(SELECT 1 FROM pending_task WHERE run_once_key=$2)`, id, owner.runOnce().String()).Scan(&registered, &queued))
	})
	return
}

// A genuine paid intent whose current owner page was invisible receives both
// routing hints and its one payer task, then the actual financial worker closes
// it. Registration itself leaves every accepted and financial byte unchanged.
func TestExactCloseOwnerPublicationSettlesRetainedPaidIntent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f, id := legacySettlementTestIntent(t, ctx)
		// Use the established paid-grant fixture so the unchanged positive
		// revenue oracle measures a funded payout as well as durable bytes.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		}, server.TxReadCommitted, server.OptNoRetry())
		retainMissingCloseOwnerHints(t, ctx, id)
		owner := ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: f.sourceNetworkId}
		before := legacyDispatchAuthorityState(ctx, id, f.balanceId)
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		var taskId server.Id
		start := server.NowUtc()
		withLegacyPayerQueueTestTx(ctx, []server.Id{owner.Id}, func(tx server.PgTx) {
			queued, err := QueueRegisteredLegacyCloseContractInTx(client, tx, id, owner)
			if err != nil || !queued {
				t.Fatal("exact retained payer did not register and publish", queued, err)
			}
			var registered bool
			server.Raise(tx.QueryRow(ctx, `SELECT payer_network_id=$2 AND source_client_id=$3
				FROM legacy_settlement_intent WHERE contract_id=$1`, id, owner.Id, f.sourceId).Scan(&registered))
			server.Raise(tx.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`, owner.runOnce().String()).Scan(&taskId))
			if !registered {
				t.Fatal("owner publication escaped the transaction that classified its intent")
			}
		})
		end := server.NowUtc()
		registered, queued := exactClosePublicationState(t, ctx, id, owner)
		if !registered || !queued || legacyDispatchAuthorityState(ctx, id, f.balanceId) != before {
			t.Fatal("exact handoff lost its queue or changed accepted authority, reports or money")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var runAt time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT run_at FROM pending_task WHERE task_id=$1`, taskId).Scan(&runAt))
			if runAt.Before(start.Add(30*time.Second)) || runAt.After(end.Add(30*time.Second)) {
				t.Fatal("exact registration replaced the ordinary collection window")
			}
		})
		// Cross only the test's durable eligibility clock; the real owner,
		// financial Run, completion and deferred accounting posts stay intact.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE task_id=$1`, taskId, server.NowUtc().Add(-time.Minute)))
		})
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(NewLegacyPayerSettlementTaskTarget())
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != taskId || len(retried)+len(posts) != 0 {
			t.Fatal("registered payer did not complete its actual financial turn", err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 11, 11)
	})
}

// A deferred refusal occurs after both writes were attempted. Neither hint
// registration nor its queue acknowledgement may survive that failed commit.
func TestExactCloseOwnerPublicationCommitFailureKeepsBothHintsMissing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		retainMissingCloseOwnerHints(t, ctx, id)
		owner := ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: f.sourceNetworkId}
		before := legacyDispatchAuthorityState(ctx, id, f.balanceId)
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE test_exact_close_refusal (run_once_key text PRIMARY KEY)`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO test_exact_close_refusal VALUES($1)`, owner.runOnce().String()))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_exact_close_refusal() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF EXISTS(SELECT 1 FROM test_exact_close_refusal WHERE run_once_key=NEW.run_once_key) THEN
					RAISE EXCEPTION 'synthetic exact owner commit refusal' USING ERRCODE='23514';
				END IF;
				RETURN NEW;
			END $$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE CONSTRAINT TRIGGER test_exact_close_refusal
				AFTER INSERT OR UPDATE ON pending_task DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW EXECUTE FUNCTION test_exact_close_refusal()`))
		})
		attempted := false
		failure := server.HandleError(func() {
			withLegacyPayerQueueTestTx(ctx, []server.Id{owner.Id}, func(tx server.PgTx) {
				queued, err := QueueRegisteredLegacyCloseContractInTx(client, tx, id, owner)
				server.Raise(err)
				attempted = queued
			})
		})
		registered, queued := exactClosePublicationState(t, ctx, id, owner)
		if !attempted || failure == nil || !strings.Contains(fmt.Sprint(failure), "synthetic exact owner commit refusal") ||
			registered || queued || legacyDispatchAuthorityState(ctx, id, f.balanceId) != before {
			t.Fatal("failed exact handoff donated routing, queue or financial authority", attempted, failure)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var missing bool
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NULL AND source_client_id IS NULL
				FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&missing))
			if !missing {
				t.Fatal("commit refusal retained a partially registered owner")
			}
		})
		server.Tx(ctx, func(tx server.PgTx) { server.RaisePgResult(tx.Exec(ctx, `DELETE FROM test_exact_close_refusal`)) })
		withLegacyPayerQueueTestTx(ctx, []server.Id{owner.Id}, func(tx server.PgTx) {
			queued, err := QueueRegisteredLegacyCloseContractInTx(client, tx, id, owner)
			if err != nil || !queued {
				t.Fatal("later exact handoff could not recover the retained intent", err)
			}
		})
		registered, queued = exactClosePublicationState(t, ctx, id, owner)
		if !registered || !queued || legacyDispatchAuthorityState(ctx, id, f.balanceId) != before {
			t.Fatal("recovered handoff lost atomic hints or changed financial state")
		}
	})
}

func TestExactCloseOwnerPublicationBusyAndMismatchedCustodyDoNotWrite(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		retainMissingCloseOwnerHints(t, ctx, id)
		owner := ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: f.sourceNetworkId}
		before := legacyDispatchAuthorityState(ctx, id, f.balanceId)
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		for _, relation := range []string{"legacy_settlement_intent", "transfer_contract"} {
			conn := acquireContractLifecycleTestConnection(t, ctx)
			tx, err := conn.Begin(ctx)
			server.Raise(err)
			func() {
				defer conn.Release()
				defer tx.Rollback(context.Background())
				var locked server.Id
				server.Raise(tx.QueryRow(ctx, `SELECT contract_id FROM `+relation+` WHERE contract_id=$1 FOR UPDATE`, id).Scan(&locked))
				withLegacyPayerQueueTestTx(ctx, []server.Id{owner.Id}, func(publication server.PgTx) {
					queued, err := QueueRegisteredLegacyCloseContractInTx(client, publication, id, owner)
					if err != nil || queued {
						t.Fatal("busy exact custody was not a read-only yield", relation, queued, err)
					}
				})
			}()
		}
		wrong := ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: f.destinationNetworkId}
		withLegacyPayerQueueTestTx(ctx, []server.Id{wrong.Id}, func(tx server.PgTx) {
			queued, err := QueueRegisteredLegacyCloseContractInTx(client, tx, id, wrong)
			if err != nil || queued {
				t.Fatal("changed actual owner used the originally admitted queue key", queued, err)
			}
		})
		// Model an owner that became unresolved after the child's successful
		// body. The handoff yields; a later body records any persistent error.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=$2 WHERE contract_id=$1`, id, server.Id{}))
		})
		unresolvedBefore := legacyDispatchAuthorityState(ctx, id, f.balanceId)
		withLegacyPayerQueueTestTx(ctx, []server.Id{owner.Id}, func(tx server.PgTx) {
			queued, err := QueueRegisteredLegacyCloseContractInTx(client, tx, id, owner)
			if err != nil || queued {
				t.Fatal("newly unresolved owner created a fatal handoff error or a wrong queue wake", queued, err)
			}
		})
		if legacyDispatchAuthorityState(ctx, id, f.balanceId) != unresolvedBefore {
			t.Fatal("unresolved exact handoff changed accepted authority or money")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=$2 WHERE contract_id=$1`, id, f.sourceNetworkId))
		})
		server.Tx(ctx, func(tx server.PgTx) {
			queued, err := QueueRegisteredLegacyCloseContractInTx(client, tx, id, owner)
			if err == nil || queued {
				t.Fatal("exact handoff acquired queue ownership after beginning its transaction")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		registered, queued := exactClosePublicationState(t, ctx, id, owner)
		_, wrongQueued := exactClosePublicationState(t, ctx, id, wrong)
		if registered || queued || wrongQueued || legacyDispatchAuthorityState(ctx, id, f.balanceId) != before {
			t.Fatal("refused exact publication changed routing, accepted authority or money")
		}
	})
}
