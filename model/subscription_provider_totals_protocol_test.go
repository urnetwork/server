// Provider projection amortizes transport while retaining each durable owner.
package model

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// A real transaction counts transport submissions and consumed batch replies.
// The optional close error models a missing batch acknowledgement before commit.
type providerProtocolTestTx struct {
	server.PgTx
	execs       int
	queries     int
	batches     int
	statements  int
	batchReads  int
	batchClosed bool
	closeErr    error
}

func (self *providerProtocolTestTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	self.execs++
	return self.PgTx.Exec(ctx, sql, args...)
}

func (self *providerProtocolTestTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	self.queries++
	return self.PgTx.Query(ctx, sql, args...)
}

func (self *providerProtocolTestTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	self.queries++
	return self.PgTx.QueryRow(ctx, sql, args...)
}

func (self *providerProtocolTestTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	self.batches++
	self.statements += batch.Len()
	return &providerProtocolTestBatch{BatchResults: self.PgTx.SendBatch(ctx, batch), owner: self}
}

// The wrapper does not invent successful SQL replies or hide the actual close.
type providerProtocolTestBatch struct {
	pgx.BatchResults
	owner *providerProtocolTestTx
}

func (self *providerProtocolTestBatch) Exec() (pgconn.CommandTag, error) {
	self.owner.batchReads++
	return self.BatchResults.Exec()
}

func (self *providerProtocolTestBatch) Close() error {
	err := self.BatchResults.Close()
	self.owner.batchClosed = true
	if err != nil {
		return err
	}
	return self.owner.closeErr
}

// Singleton, B4-with-one-held, and multi-provider bodies keep the locked read,
// then acknowledge one write transport before returning to the real commit owner.
func TestLegacyProviderTotalsPipelineAcknowledgesWholeProjection(t *testing.T) {
	for _, shape := range []struct {
		name      string
		contracts int
		providers int
	}{{"singleton", 1, 1}, {"three_claimed", 3, 1}, {"multi_provider", 1, 3}} {
		t.Run(shape.name, func(t *testing.T) {
			providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
				providerTotalsBatchWriteCounter(t, ctx)
				networkIds := make([]server.Id, shape.providers)
				payouts := map[server.Id]*contractPayout{}
				for index := range networkIds {
					networkIds[index] = server.NewId()
					payouts[networkIds[index]] = &contractPayout{payoutByteCount: 17, payout: 29}
				}
				ids := make([]server.Id, shape.contracts)
				for index := range ids {
					ids[index] = providerTotalsTestPublish(ctx, server.NewId(), payouts)
				}
				apply := func(tx server.PgTx) error {
					if len(ids) == 1 {
						return applyLegacyProviderTotalsInTx(ctx, tx, ids[0])
					}
					return applyLegacyProviderTotalsBatchInTx(ctx, tx, ids, networkIds[0])
				}
				for _, replay := range []bool{false, true} {
					server.Tx(ctx, func(tx server.PgTx) {
						observed := &providerProtocolTestTx{PgTx: tx}
						server.Raise(apply(observed))
						if observed.queries != 1 || observed.execs != 0 {
							t.Fatal("projection retained serial write round trips after its locked authority read", observed.queries, observed.execs)
						}
						if replay {
							if observed.batches != 0 {
								t.Fatal("applied replay submitted another provider credit")
							}
						} else if observed.batches != 1 || observed.statements != shape.providers+1 ||
							observed.batchReads != observed.statements || !observed.batchClosed {
							t.Fatal("projection returned before acknowledging every credit and marker reply")
						}
					}, server.TxReadCommitted, server.OptNoRetry())
				}
				for _, networkId := range networkIds {
					for _, id := range ids {
						requireProviderTotalsTestState(t, ctx, id, networkId, true, int64(17*len(ids)), int64(29*len(ids)))
					}
				}
				server.Db(ctx, func(conn server.PgConn) {
					var writes int
					server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM test_provider_total_write`).Scan(&writes))
					if writes != shape.providers {
						t.Fatal("projection or replay changed the exact number of provider writes", writes)
					}
				})
				providerQueueTestFinalize(t, ctx, task.GetTasks(ctx, ids...))
			})
		})
	}
}

// A missing final batch acknowledgement cannot commit even when every SQL
// statement executed. The ordinary retained owners recover exactly once.
func TestLegacyProviderTotalsPipelineCloseFailureRollsBackAll(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		ids := []server.Id{providerTotalsTestTask(ctx, server.NewId(), networkId), providerTotalsTestTask(ctx, server.NewId(), networkId)}
		lostReply := errors.New("synthetic provider batch reply loss")
		recovered := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				observed := &providerProtocolTestTx{PgTx: tx, closeErr: lostReply}
				server.Raise(applyLegacyProviderTotalsBatchInTx(ctx, observed, ids, networkId))
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		err, typed := recovered.(error)
		var phase *legacyProviderTotalsPhaseError
		if !typed || !errors.Is(err, lostReply) || !errors.As(err, &phase) || phase.phase != legacyProviderTotalsAppliedMarker {
			t.Fatal("projection committed without its complete batch reply", recovered)
		}
		for _, id := range ids {
			requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
		}
		providerQueueTestFinalize(t, ctx, task.GetTasks(ctx, ids...))
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count=34 AND provided_net_revenue_nano_cents=58
                FROM account_balance WHERE network_id=$1`, networkId).Scan(&exact))
			if !exact {
				t.Fatal("batch reply recovery lost or repeated an allocation")
			}
		})
	})
}

// A prepared snapshot supplies prospective keys, never permission to follow a
// replacement durable queue identity. The locked body rejects it before credits.
func TestLegacyProviderTotalsPreparedKeysRejectChangedSnapshotIdentity(t *testing.T) {
	for _, count := range []int{1, 3} {
		name := "singleton"
		if count > 1 {
			name = "three_claimed"
		}
		t.Run(name, func(t *testing.T) {
			providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
				networkId := server.NewId()
				ids := make([]server.Id, count)
				for index := range ids {
					ids[index] = providerTotalsTestTask(ctx, server.NewId(), networkId)
				}
				original := task.GetTasks(ctx, ids...)
				target := providerQueueTestTarget(original, ids)
				changedId := ids[len(ids)-1]
				replacement := task.RunOnce("synthetic-provider-snapshot-change", server.NewId()).String()
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_once_key=$2 WHERE task_id=$1`, changedId, replacement))
				}, server.TxReadCommitted, server.OptNoRetry())
				_, _, err := target.Run(ctx, original[ids[0]])
				var phase *legacyProviderTotalsPhaseError
				if !errors.As(err, &phase) || phase.phase != legacyProviderTotalsPendingRead {
					t.Fatal("stale prepared scope followed a replacement queue identity", err)
				}
				for _, id := range ids {
					requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
				}
				providerQueueTestFinalize(t, ctx, task.GetTasks(ctx, ids...))
			})
		})
	}
}

// The public task record collapses null to an empty string. That ambiguous
// snapshot must discover its exact durable key; null rows retain task-id owners.
func TestLegacyProviderTotalsPreparedEmptyKeyKeepsDurableDiscovery(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		ids := []server.Id{providerTotalsTestTask(ctx, server.NewId(), networkId), providerTotalsTestTask(ctx, server.NewId(), networkId)}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_once_key=NULL WHERE task_id=$1`, ids[0]))
		}, server.TxReadCommitted, server.OptNoRetry())
		original := task.GetTasks(ctx, ids...)
		if original[ids[0]].RunOnceKey != "" {
			t.Fatal("fixture did not retain the ambiguous task snapshot")
		}
		admitted := false
		key := task.PendingTaskOwnershipKey(ids[0], nil)
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted && slices.Contains(event.Keys, key) {
				admitted = true
			}
		})
		target := providerQueueTestTarget(original, ids)
		_, _, err := target.Run(observed, original[ids[0]])
		if err != nil || !admitted {
			t.Fatal("ambiguous snapshot bypassed durable null-key ownership", err)
		}
		for _, id := range ids {
			requireProviderTotalsTestState(t, ctx, id, networkId, true, 34, 58)
		}
		providerQueueTestFinalize(t, ctx, original)
	})
}
