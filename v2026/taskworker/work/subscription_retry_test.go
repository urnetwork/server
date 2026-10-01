// Drives the real force-close target and task evaluator against synthetic state.
package work

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// An origin is the valid sibling; its reverse companion records unequal,
// over-grant usage and therefore becomes a genuinely underfunded dispute.
type closeRetryFixture struct {
	originId    server.Id
	companionId server.Id
	balanceId   server.Id
	grant       model.ByteCount
}

// All identities and accounts are generated within DefaultTestEnv. No live
// address, credential, host, configuration or network endpoint is a fixture.
func newCloseRetryFixture(t testing.TB, ctx context.Context) closeRetryFixture {
	t.Helper()
	payerNetworkId, providerNetworkId := server.NewId(), server.NewId()
	sourceId, destinationId := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, payerNetworkId, "synthetic-close-payer-"+payerNetworkId.String(), server.NewId())
	model.Testing_CreateNetwork(ctx, providerNetworkId, "synthetic-close-provider-"+providerNetworkId.String(), server.NewId())
	server.Tx(ctx, func(tx server.PgTx) {
		for clientId, networkId := range map[server.Id]server.Id{sourceId: payerNetworkId, destinationId: providerNetworkId} {
			server.RaisePgResult(tx.Exec(ctx, `
                INSERT INTO network_client (client_id, network_id, active, create_time, auth_time)
                VALUES ($1,$2,true,$3,$3)
            `, clientId, networkId, server.NowUtc()))
		}
	})
	const grant = model.ByteCount(32 * 1024 * 1024)
	model.AddBasicTransferBalance(ctx, payerNetworkId, 8*grant, server.NowUtc(), server.NowUtc().Add(24*time.Hour))
	balances := model.GetActiveTransferBalances(ctx, payerNetworkId)
	if len(balances) != 1 {
		t.Fatal("synthetic payer did not have exactly one balance")
	}
	originId, _, err := model.CreateContract(ctx, payerNetworkId, sourceId, providerNetworkId, destinationId, grant)
	if err != nil {
		t.Fatal("synthetic origin creation failed")
	}
	companionId, _, err := model.CreateCompanionContract(ctx, providerNetworkId, destinationId, payerNetworkId, sourceId, grant, time.Hour)
	if err != nil {
		t.Fatal("synthetic companion creation failed")
	}
	if model.CloseContract(ctx, companionId, destinationId, 0, false) != nil || model.CloseContract(ctx, companionId, sourceId, 4*grant, false) != nil {
		t.Fatal("synthetic over-grant closes did not enter dispute")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$3 WHERE contract_id IN ($1,$2)`,
			originId, companionId, time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)))
	})
	return closeRetryFixture{originId: originId, companionId: companionId, balanceId: balances[0].BalanceId, grant: grant}
}

// A real completed sibling must not release or clamp the disputed companion's
// reserved bytes; persisted reports remain untouched for accounting review.
func (self closeRetryFixture) requireAccounting(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
            SELECT c.dispute, c.outcome IS NULL, c.open, e.settled,
                   e.balance_byte_count, coalesce(e.payout_byte_count,0),
                   (SELECT count(*) FROM contract_close WHERE contract_id=c.contract_id),
                   (SELECT sum(used_transfer_byte_count) FROM contract_close WHERE contract_id=c.contract_id),
                   (SELECT count(*) FROM contract_close WHERE contract_id=c.contract_id AND checkpoint)
            FROM transfer_contract c JOIN transfer_escrow e ON e.contract_id=c.contract_id
            WHERE c.contract_id=$1
        `, self.companionId)
		server.WithPgResult(rows, err, func() {
			if !rows.Next() {
				t.Fatal("reserved companion disappeared")
			}
			var dispute, nonfinal, open, settled bool
			var reserved, payout, usage int64
			var partyCount, checkpointCount int
			server.Raise(rows.Scan(&dispute, &nonfinal, &open, &settled, &reserved, &payout, &partyCount, &usage, &checkpointCount))
			if !dispute || !nonfinal || open || settled || reserved != self.grant || payout != 0 || partyCount != 2 || usage != 4*self.grant || checkpointCount != 0 || rows.Next() {
				t.Fatal("retry altered the protected disputed accounting state")
			}
		})
	})
	if close, closed := model.GetContractClose(ctx, self.originId); !closed || close.Outcome != model.ContractOutcomeSettled {
		t.Fatal("unrelated origin sibling failed to finalize")
	}
	if model.Testing_NetEscrowByteCount(ctx, self.balanceId) != self.grant {
		t.Fatal("the completed sibling changed the disputed companion reservation")
	}
}

// At error count16 the old evaluator adds30–90min despite completed siblings.
// Assert persisted run_at minus release_time, not scheduler timing or a sleep.
func TestCloseExpiredAccountingRejectionKeepsTaskAndIdleCadence(t *testing.T) {
	testCloseExpiredAccountingRejectionKeepsTaskAndIdleCadence(t, false)
}

// The second rejected row is terminal and unpaid after the existing quarantine.
// Its retained diagnostic must not push the shared task back to hour-scale delay.
func TestCloseExpiredVerifiedQuarantineKeepsTaskAndIdleCadence(t *testing.T) {
	testCloseExpiredAccountingRejectionKeepsTaskAndIdleCadence(t, true)
}

// Both paths drive the real target/evaluator and persisted retry timestamps;
// no wall-clock waiting or injected retry wrapper supplies the expected result.
func testCloseExpiredAccountingRejectionKeepsTaskAndIdleCadence(t *testing.T, quarantineOrigin bool) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		oldBase, oldCap := task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout
		task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = 2*time.Second, time.Hour
		t.Cleanup(func() { task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = oldBase, oldCap })
		ctx := context.Background()
		fixture := newCloseRetryFixture(t, ctx)
		if quarantineOrigin {
			var sourceId, destinationId server.Id
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT source_id,destination_id FROM transfer_contract WHERE contract_id=$1`, fixture.originId).Scan(&sourceId, &destinationId))
			})
			if model.CloseContract(ctx, fixture.originId, sourceId, 2*fixture.grant, true) != nil ||
				model.CloseContract(ctx, fixture.originId, destinationId, 2*fixture.grant, true) != nil {
				t.Fatal("synthetic over-grant checkpoint pair failed")
			}
		}
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { ScheduleCloseExpiredContracts(clientSession, tx, 0, false) })
		target := task.NewTaskTargetWithPost(CloseExpiredContracts, CloseExpiredContractsPost)
		var taskId server.Id
		var scheduledMetadata string
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `
                SELECT task_id, jsonb_build_array(function_name, args_json, run_once_key,
                    run_priority, run_max_time_seconds, client_address_hash,
                    client_address_port, client_by_jwt_json)::text
                FROM pending_task WHERE function_name=$1
            `, target.TargetFunctionName())
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("scheduled synthetic closer missing")
				}
				server.Raise(rows.Scan(&taskId, &scheduledMetadata))
				if rows.Next() {
					t.Fatal("synthetic closer had multiple scheduler blocks")
				}
			})
		})
		worker := task.NewTaskWorkerWithDefaults(ctx)
		worker.AddTargets(target)
		for pass := 0; pass < 2; pass++ {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
                    UPDATE pending_task SET run_at=$2, release_time=$2, reschedule_error_count=$3 WHERE task_id=$1
                `, taskId, time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC), 16+pass))
			})
			finished, rescheduled, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 0 || len(posts) != 0 || len(rescheduled) != 1 || rescheduled[0] != taskId {
				t.Fatal("partial accounting failure was lost, finished, or replaced")
			}
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `
                    SELECT run_at, release_time, reschedule_error_count, reschedule_error,
                        jsonb_build_array(function_name, args_json, run_once_key,
                            run_priority, run_max_time_seconds, client_address_hash,
                            client_address_port, client_by_jwt_json)::text
                    FROM pending_task WHERE task_id=$1
                `, taskId)
				server.WithPgResult(rows, err, func() {
					if !rows.Next() {
						t.Fatal("rejected task was removed")
					}
					var runAt, releaseAt time.Time
					var errorCount int
					var storedError string
					var currentMetadata string
					server.Raise(rows.Scan(&runAt, &releaseAt, &errorCount, &storedError, &currentMetadata))
					if currentMetadata != scheduledMetadata {
						t.Error("retry changed durable task arguments, key, priority, deadline or session metadata")
					}
					if errorCount != 17+pass || !strings.Contains(storedError, "Escrow does not have enough value") || !strings.Contains(storedError, "contract remained non-final") {
						t.Error("accounting error/count visibility was weakened")
					}
					if quarantineOrigin && pass == 0 && strings.Count(storedError, "Escrow does not have enough value") != 2 {
						t.Error("terminal quarantine diagnostic was discarded")
					}
					if delay := runAt.Sub(releaseAt); delay < time.Minute || 5*time.Minute <= delay {
						t.Errorf("isolated accounting rejection retry=%s, want existing1–5min idle cadence", delay)
					}
				})
			})
			fixture.requireAccounting(t, ctx)
			if quarantineOrigin {
				server.Db(ctx, func(conn server.PgConn) {
					var settled bool
					var payout int64
					server.Raise(conn.QueryRow(ctx, `SELECT settled,coalesce(payout_byte_count,0) FROM transfer_escrow WHERE contract_id=$1`, fixture.originId).Scan(&settled, &payout))
					if settled || payout != 0 {
						t.Error("verified quarantine gained financial settlement authority")
					}
				})
			}
			if len(task.GetFinishedTasks(ctx, taskId)) != 0 {
				t.Fatal("failed batch was falsely recorded as a successful task")
			}
		}
	})
}

// Pure progress boundaries need no thousands-row fixture. The source retains
// two independent scan caps; selected or rejected counts never enter this input.
func TestCloseExpiredAccountingRetryCadenceUsesVerifiedProgress(t *testing.T) {
	for _, verified := range []int64{0, 1, 6249, 6250, 24999, 25000, 50000} {
		for _, unit := range []float64{0, 0.5, 1} {
			delay := closeExpiredContractsRetryDelay(verified, unit)
			low, high := time.Minute, 5*time.Minute
			if 6250 <= verified {
				low, high = 2*time.Second, 4*time.Second
			}
			if delay < low || high <= delay {
				t.Errorf("verified=%d unit=%g: delay=%s outside [%s,%s)", verified, unit, delay, low, high)
			}
		}
	}
	if closeExpiredContractsMaxCount != 25000 || closeExpiredContractsParallel != 92 || DefaultCloseExpiredContractsBlockSize != 1 {
		t.Fatal("accounting isolation changed the scan cap, worker pool, or task partitioning")
	}
}
