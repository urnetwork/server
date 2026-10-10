// Startup's simple PK pass publishes real queue rows with exact deadlines.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

func newStartupClosureFreeClients(ctx context.Context) (networkId, sourceId, destinationId server.Id) {
	networkId, sourceId, destinationId = server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "synthetic-startup-close", server.NewId())
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "synthetic-source", "synthetic")
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "synthetic-destination", "synthetic")
	return
}

// Only selector volume is synthetic; the existing shape tests retain public
// creation. Public report owners still perform the boundary's actual closes.
func newStartupClosureScanContracts(ctx context.Context, networkId, sourceId, destinationId server.Id, count int) []server.Id {
	ids := make([]server.Id, count)
	for index := range ids {
		ids[index] = server.NewId()
	}
	slices.SortFunc(ids, func(a, b server.Id) int { return a.Cmp(b) })
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
			contract_id,source_network_id,source_id,destination_network_id,destination_id,
			transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
			SELECT contract_id,$2,$3,$2,$4,100,true,$5,NULL FROM unnest($1::uuid[]) AS candidate(contract_id)`,
			ids, networkId, sourceId, destinationId, server.NowUtc()))
	})
	return ids
}

func startupClosureWorker(ctx context.Context, targets ...task.Target) *task.TaskWorker {
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	worker.AddTargets(targets...)
	return worker
}

// The caller explicitly crosses integer claim eligibility; the target and its
// real completion/Post still run without timer sleeps or replaced callbacks.
func evalStartupClosureTask(t testing.TB, ctx context.Context, worker *task.TaskWorker, id server.Id) {
	t.Helper()
	var function string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT function_name FROM pending_task WHERE task_id=$1`, id).Scan(&function))
	})
	if function != NewScheduledContractClosureTaskTarget().TargetFunctionName() {
		makeCloseRetryTaskDue(ctx, id)
	}
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
		t.Fatal("startup closure task did not finish its real queue handoff", finished, retried, posts, err)
	}
}

// Generated open is false for disputes. Reports, legacy intent presence,
// missing expiration and future explicit expiration must not hide any row.
func TestStartupContractClosureSchedulesEveryNonterminalShape(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		started := server.NowUtc().Truncate(time.Microsecond)
		wanted := map[server.Id]time.Time{}
		var closedId server.Id
		for index := range 8 {
			id, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
			server.Raise(err)
			if index > 0 {
				server.Raise(model.CloseContract(ctx, id, sourceId, 17, index != 2 && index != 7))
			}
			if index == 3 {
				server.Raise(model.CloseContract(ctx, id, destinationId, 17, true))
			}
			if index == 4 {
				model.SetContractDispute(ctx, id, true)
			}
			if index == 7 {
				server.Raise(model.CloseContract(ctx, id, destinationId, 17, false))
				closedId = id
				continue
			}
			var expiration *time.Time
			deadline := started.Add(model.DefaultContractExpiration)
			switch index % 4 {
			case 1:
				value := started.Add(-time.Minute)
				expiration = &value
				deadline = value
			case 2:
				value := started.Add(10 * time.Minute)
				expiration = &value
				deadline = value
			case 3:
				value := started.Add(3 * time.Hour)
				expiration = &value
				deadline = value
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, expiration))
				if index == 5 {
					// Historical source intents can lack registration hints. The
					// scanner must enumerate them before any owner classification.
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome)
						VALUES($1,$2,'settled')`, id, int(id[15])%model.LegacySettlementShardCount))
				}
			})
			wanted[id] = deadline
		}
		server.Tx(ctx, func(tx server.PgTx) {
			task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures, &ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started}, owner,
				task.RunOnce("schedule_open_contract_closures_on_startup"), task.RunAt(server.NowUtc()))
			ScheduleOpenContractClosuresOnStartup(owner, tx)
		})
		key := task.RunOnce("schedule_open_contract_closures_on_startup").String()
		initial, found := readExpiryRecoveryQueue(t, ctx)[key]
		if !found {
			t.Fatal("startup did not create its stable enumerator")
		}
		worker := startupClosureWorker(ctx, NewStartupContractClosureTaskTarget())
		defer worker.Close()
		evalStartupClosureTask(t, ctx, worker, initial.id)
		queue := readExpiryRecoveryQueue(t, ctx)
		for id, deadline := range wanted {
			row, found := queue[task.RunOnce("close_scheduled_contract", id).String()]
			var args CloseScheduledContractArgs
			if !found || json.Unmarshal([]byte(row.args), &args) != nil || args.ContractId != id || !args.Private ||
				!args.Deadline.Equal(deadline) || !row.runAt.Equal(deadline) || row.function != NewScheduledContractClosureTaskTarget().TargetFunctionName() {
				t.Fatal("startup omitted a nonterminal shape or changed its exact requested wake", deadline, row.runAt)
			}
		}
		if _, found := queue[task.RunOnce("close_scheduled_contract", closedId).String()]; found {
			t.Fatal("startup scheduled a terminal contract")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE outcome IS NULL AND NOT usage_unverified AND provider_usage IS NULL`).Scan(&untouched))
			if untouched != len(wanted) {
				t.Fatal("enumeration changed contract proof or outcomes")
			}
		})
	})
}

// One real task traverses multiple bounded pages. Closing earlier contracts
// before the next read cannot shift later rows out of the increasing PK pass.
func TestStartupContractClosurePagesKeepCapAndEarliestWake(t *testing.T) {
	for _, count := range []int{1025, 2049} {
		env := server.DefaultTestEnv()
		env.RerunCount = 0
		env.Run(t, func(t testing.TB) {
			baseCtx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 2*time.Minute)
			defer cancel()
			networkId, sourceId, destinationId := newStartupClosureFreeClients(baseCtx)
			ids := newStartupClosureScanContracts(baseCtx, networkId, sourceId, destinationId, count)
			childKeys := map[server.PgOwnershipKey]bool{}
			for _, id := range ids {
				childKeys[task.RunOnceOwnershipKey(task.RunOnce("close_scheduled_contract", id))] = true
			}
			var stateLock sync.Mutex
			maxOwnedKeys, published := 0, 0
			closedPrefix := false
			var closeErr error
			ownedKeys := map[server.PgOwnershipKey]bool{}
			ctx := server.Testing_WithPgOwnershipObservation(baseCtx, func(event server.PgOwnershipEvent) {
				stateLock.Lock()
				publication := len(event.Keys) > 0
				for _, key := range event.Keys {
					publication = publication && childKeys[key]
				}
				if event.Kind == server.PgOwnershipAdmitted {
					maxOwnedKeys = max(maxOwnedKeys, len(event.Keys))
					for _, key := range event.Keys {
						ownedKeys[key] = true
					}
				}
				closeNow := false
				if event.Kind == server.PgOwnershipReleased && publication {
					published += len(event.Keys)
					if published == 1024 && !closedPrefix {
						closedPrefix, closeNow = true, true
					}
				}
				stateLock.Unlock()
				if closeNow {
					// Release observation is synchronous after the real chunk
					// commits and before this invocation reads the next page.
					var observedErr error
					server.HandleError(func() {
						for _, id := range ids[:2] {
							server.Raise(model.CloseContract(baseCtx, id, sourceId, 17, false))
							server.Raise(model.CloseContract(baseCtx, id, destinationId, 17, false))
						}
					}, func(err error) { observedErr = err })
					stateLock.Lock()
					closeErr = observedErr
					stateLock.Unlock()
				}
			})
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			started := server.NowUtc().Truncate(time.Microsecond)
			server.Tx(ctx, func(tx server.PgTx) {
				task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures, &ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started}, owner,
					task.RunOnce("schedule_open_contract_closures_on_startup"), task.RunAt(server.NowUtc()))
			})
			worker := startupClosureWorker(ctx, NewStartupContractClosureTaskTarget())
			defer worker.Close()
			key := task.RunOnce("schedule_open_contract_closures_on_startup").String()
			first := readExpiryRecoveryQueue(t, ctx)[key]
			evalStartupClosureTask(t, ctx, worker, first.id)
			queue := readExpiryRecoveryQueue(t, ctx)
			if _, found := queue[key]; found || len(queue) != count {
				t.Fatal("single startup invocation left a page task or omitted children", count, len(queue))
			}
			stateLock.Lock()
			maxKeys, closed, boundaryErr := maxOwnedKeys, closedPrefix, closeErr
			ownedScan := ownedKeys[task.RunOnceOwnershipKey(task.RunOnce("schedule_open_contract_closures_on_startup"))] &&
				ownedKeys[server.NewPgOwnershipKey("finished_task/task_id", first.id)]
			stateLock.Unlock()
			if maxKeys != 256 || !ownedScan || !closed || boundaryErr != nil {
				t.Fatal("single scan lost bounded publication, completion ownership or its real page boundary", maxKeys, ownedScan, closed, boundaryErr)
			}
			for _, id := range ids[:2] {
				if _, terminal := model.GetContractClose(baseCtx, id); !terminal {
					t.Fatal("public report owners did not close the earlier page rows")
				}
			}
			for _, id := range ids {
				row, found := queue[task.RunOnce("close_scheduled_contract", id).String()]
				var args CloseScheduledContractArgs
				if !found || json.Unmarshal([]byte(row.args), &args) != nil || args.ContractId != id ||
					!args.Deadline.Equal(started.Add(model.DefaultContractExpiration)) || !row.runAt.Equal(args.Deadline) {
					t.Fatal("ordered loop omitted a contract or restarted its fallback lifetime")
				}
			}
			server.Db(ctx, func(conn server.PgConn) {
				var emptyResult, complete bool
				var maxSeconds int
				server.Raise(conn.QueryRow(ctx, `SELECT result_json::jsonb='{}'::jsonb,post_completed,run_max_time_seconds
					FROM finished_task WHERE task_id=$1`, first.id).Scan(&emptyResult, &complete, &maxSeconds))
				if !emptyResult || !complete || maxSeconds != int(task.DefaultMaxTime/time.Second) {
					t.Fatal("scanner retained page payloads or replaced ordinary task timing", emptyResult, complete, maxSeconds)
				}
			})
			lastId := ids[len(ids)-1]
			prior := queue[task.RunOnce("close_scheduled_contract", lastId).String()]
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, lastId, started.Add(4*time.Hour)))
			})
			_, err := ScheduleOpenContractClosures(&ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started.Add(20 * time.Minute)}, owner)
			server.Raise(err)
			after := readExpiryRecoveryQueue(t, ctx)[task.RunOnce("close_scheduled_contract", lastId).String()]
			if after.id != prior.id || after.args != prior.args || !after.runAt.Equal(prior.runAt) {
				t.Fatal("later startup replaced a stable child or postponed its earlier wake")
			}
			earlier := started.Add(30 * time.Minute)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, lastId, earlier))
			})
			_, err = ScheduleOpenContractClosures(&ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started.Add(40 * time.Minute)}, owner)
			server.Raise(err)
			after = readExpiryRecoveryQueue(t, ctx)[task.RunOnce("close_scheduled_contract", lastId).String()]
			if after.id != prior.id || after.args != prior.args || !after.runAt.Equal(earlier) {
				t.Fatal("earlier explicit expiry did not advance the same child's wake")
			}
		})
	}
}

// A genuinely future request is unclaimable and its direct target call keeps
// both contract state and its exact future child deadline intact.
func TestScheduledContractClosureFutureWakeRetainsDeadline(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		id, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
		server.Raise(err)
		server.Raise(model.CloseContract(ctx, id, sourceId, 17, true))
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		deadline := server.NowUtc().Truncate(time.Microsecond).Add(time.Hour)
		key := task.RunOnce("close_scheduled_contract", id)
		server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(key)}, func(tx server.PgTx) {
			scheduleContractClose(owner, tx, &CloseScheduledContractArgs{Private: true, ScheduledContractClose: ScheduledContractClose{ContractId: id, Deadline: deadline}})
		}, server.TxReadCommitted, server.OptNoRetry())
		initial := readExpiryRecoveryQueue(t, ctx)[key.String()]
		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(retried)+len(posts) != 0 {
			t.Fatal("future close was admitted before its requested wake", err)
		}
		result, err := CloseScheduledContract(&CloseScheduledContractArgs{Private: true, ScheduledContractClose: ScheduledContractClose{ContractId: id, Deadline: deadline}}, owner)
		if err != nil || result.RetryAt == nil || !result.RetryAt.Equal(deadline) {
			t.Fatal("future direct call acquired retirement authority", err)
		}
		next, found := readExpiryRecoveryQueue(t, ctx)[key.String()]
		if !found || !next.runAt.Equal(deadline) || next.args != initial.args {
			t.Fatal("future wake lost exact future close")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND NOT usage_unverified AND provider_usage IS NULL
				AND (SELECT count(*)=1 AND bool_and(checkpoint AND used_transfer_byte_count=17) FROM contract_close WHERE contract_id=$1)
				FROM transfer_contract WHERE contract_id=$1`, id).Scan(&untouched))
			if !untouched {
				t.Fatal("early child wake changed active report custody")
			}
		})
	})
}

// A retained intent with unclassified owner hints and a malformed report still
// receives its real owner and registration wake; only routing hints may change.
func TestScheduledContractClosureWakesUnclassifiedIntentWithoutProofEdits(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		id, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
		server.Raise(err)
		server.Raise(model.CloseContract(ctx, id, sourceId, 17, true))
		deadline := server.NowUtc().Truncate(time.Microsecond).Add(-time.Minute)
		shard := int(id[15]) % model.LegacySettlementShardCount
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
				VALUES($1,$2,'settled',$3)`, id, shard, deadline))
			// Current inserts are classified by the database trigger. Retain an
			// older unclassified row through its supported NULL-hint update seam.
			var classified, unclassified bool
			server.Raise(tx.QueryRow(ctx, `SELECT source_client_id=$2 AND payer_network_id IS NULL
				FROM legacy_settlement_intent WHERE contract_id=$1`, id, sourceId).Scan(&classified))
			if !classified {
				t.Fatal("current insert did not classify the synthetic source intent")
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
				SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=$1`, id))
			server.Raise(tx.QueryRow(ctx, `SELECT source_client_id IS NULL AND payer_network_id IS NULL
				FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&unclassified))
			if !unclassified {
				t.Fatal("retained source fixture did not preserve missing owner hints")
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=-1 WHERE contract_id=$1`, id))
		})
		read := func() (raw string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_array(outcome,usage_unverified,provider_usage,
					(SELECT to_jsonb(i)-'payer_network_id'-'source_client_id' FROM legacy_settlement_intent i WHERE contract_id=$1),
					(SELECT jsonb_agg(row_to_json(r) ORDER BY party) FROM contract_close r WHERE contract_id=$1))::text
					FROM transfer_contract WHERE contract_id=$1`, id).Scan(&raw))
			})
			return
		}
		before := read()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		key := task.RunOnce("close_scheduled_contract", id)
		server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(key)}, func(tx server.PgTx) {
			scheduleContractClose(owner, tx, &CloseScheduledContractArgs{Private: true, ScheduledContractClose: ScheduledContractClose{ContractId: id, Deadline: deadline}})
		}, server.TxReadCommitted, server.OptNoRetry())
		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		wakeBefore := server.NowUtc()
		evalStartupClosureTask(t, ctx, worker, readExpiryRecoveryQueue(t, ctx)[key.String()].id)
		wakeAfter := server.NowUtc()
		queue := readExpiryRecoveryQueue(t, ctx)
		actual, actualFound := queue[task.RunOnce("flush_legacy_source_settlements", sourceId).String()]
		registration, registrationFound := queue[task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", shard)).String()]
		if !actualFound || !registrationFound || actual.runAt.Before(wakeBefore.Add(30*time.Second)) || actual.runAt.After(wakeAfter.Add(30*time.Second)) || registration.runAt.After(wakeAfter) || before != read() {
			t.Fatal("accepted malformed intent lost wake or changed proof/authority")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var capped time.Time
			var registered bool
			server.Raise(conn.QueryRow(ctx, `SELECT expiration_time,
				EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1
					AND source_client_id=$2 AND payer_network_id IS NULL)
				FROM transfer_contract WHERE contract_id=$1`, id, sourceId).Scan(&capped, &registered))
			if !capped.Equal(deadline) || !registered {
				t.Fatal("accepted intent did not receive its due cap and exact routing hints")
			}
		})
	})
}

// RunOnce preserves the old JSON while merging an earlier wake. The actual
// registered target must use that earlier request, not reschedule stale args.
func TestScheduledContractClosureEarlierDuplicateClosesAtMinimum(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		id, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
		server.Raise(err)
		server.Raise(model.CloseContract(ctx, id, sourceId, 17, true))
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		later := server.NowUtc().Truncate(time.Microsecond).Add(time.Hour)
		earlier := later.Add(-61 * time.Minute)
		key := task.RunOnce("close_scheduled_contract", id)
		publish := func(deadline time.Time) {
			server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(key)}, func(tx server.PgTx) {
				scheduleContractClose(owner, tx, &CloseScheduledContractArgs{Private: true, ScheduledContractClose: ScheduledContractClose{ContractId: id, Deadline: deadline}})
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		publish(later)
		first := readExpiryRecoveryQueue(t, ctx)[key.String()]
		publish(earlier)
		merged := readExpiryRecoveryQueue(t, ctx)[key.String()]
		if merged.id != first.id || merged.args != first.args || !merged.runAt.Equal(earlier) {
			t.Fatal("fixture did not exercise ordinary RunOnce's earlier wake with retained args")
		}
		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		evalStartupClosureTask(t, ctx, worker, merged.id)
		closed, terminal := model.GetContractClose(ctx, id)
		if !terminal || closed.Outcome != model.ContractOutcomeSettled {
			t.Fatal("earlier duplicate woke but kept stale later close authority")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT expiration_time=$2
				AND (SELECT count(*)=2 AND bool_and(NOT checkpoint AND used_transfer_byte_count=17) FROM contract_close WHERE contract_id=$1)
				AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1)
				AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
				AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
				FROM transfer_contract WHERE contract_id=$1`, id, earlier).Scan(&exact))
			if !exact {
				t.Fatal("earlier request changed source-free bytes or failed to retain its exact cap")
			}
		})
	})
}

// The due child classifies a retained free intent with its owner wake. The
// real source worker can close it before the compatibility dispatcher runs.
func TestScheduledContractClosureRegistersAndClosesRetainedSourceIntent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		id, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
		server.Raise(err)
		server.Raise(model.CloseContract(ctx, id, sourceId, 17, true))
		deadline := server.NowUtc().Truncate(time.Microsecond).Add(-time.Minute)
		shard := int(id[15]) % model.LegacySettlementShardCount
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, server.NowUtc().Add(3*time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
				VALUES($1,$2,'settled',$3)`, id, shard, deadline))
			// Current inserts are classified by the database trigger. Retain an
			// older unclassified row through its supported NULL-hint update seam.
			var classified, unclassified bool
			server.Raise(tx.QueryRow(ctx, `SELECT source_client_id=$2 AND payer_network_id IS NULL
				FROM legacy_settlement_intent WHERE contract_id=$1`, id, sourceId).Scan(&classified))
			if !classified {
				t.Fatal("current insert did not classify the synthetic source intent")
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
				SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=$1`, id))
			server.Raise(tx.QueryRow(ctx, `SELECT source_client_id IS NULL AND payer_network_id IS NULL
				FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&unclassified))
			if !unclassified {
				t.Fatal("retained source fixture did not preserve missing owner hints")
			}
		})
		read := func() (raw string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_array(outcome,usage_unverified,provider_usage,
					(SELECT to_jsonb(i)-'payer_network_id'-'source_client_id' FROM legacy_settlement_intent i WHERE contract_id=$1),
					(SELECT jsonb_agg(row_to_json(r) ORDER BY party) FROM contract_close r WHERE contract_id=$1))::text
					FROM transfer_contract WHERE contract_id=$1`, id).Scan(&raw))
			})
			return
		}
		before := read()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		key := task.RunOnce("close_scheduled_contract", id)
		server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(key)}, func(tx server.PgTx) {
			scheduleContractClose(owner, tx, &CloseScheduledContractArgs{Private: true, ScheduledContractClose: ScheduledContractClose{ContractId: id, Deadline: deadline}})
		}, server.TxReadCommitted, server.OptNoRetry())
		child := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer child.Close()
		evalStartupClosureTask(t, ctx, child, readExpiryRecoveryQueue(t, ctx)[key.String()].id)
		server.Db(ctx, func(conn server.PgConn) {
			var nonterminal, verified, missingProof, exactCap, registered bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL,NOT usage_unverified,provider_usage IS NULL,
				expiration_time=$2,EXISTS(SELECT 1 FROM legacy_settlement_intent
					WHERE contract_id=$1 AND source_client_id=$3 AND payer_network_id IS NULL)
				FROM transfer_contract WHERE contract_id=$1`, id, deadline, sourceId).Scan(&nonterminal, &verified, &missingProof, &exactCap, &registered))
			if !nonterminal || !verified || !missingProof || !exactCap || !registered {
				t.Fatal("child lost atomic source registration or rewrote accepted proof", nonterminal, verified, missingProof, exactCap, registered)
			}
		})
		if read() != before {
			t.Fatal("child changed accepted source intent, reports or provider proof")
		}
		// Compatibility discovery has not run; the exact child owns this
		// classification and publication, while its ordinary shard wake remains.
		if _, found := readExpiryRecoveryQueue(t, ctx)[task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", shard)).String()]; !found {
			t.Fatal("exact child lost the ordinary registration successor")
		}
		sourceWorker := startupClosureWorker(ctx, model.NewLegacySourceSettlementTaskTarget())
		defer sourceWorker.Close()
		source := readExpiryRecoveryQueue(t, ctx)[task.RunOnce("flush_legacy_source_settlements", sourceId).String()]
		evalStartupClosureTask(t, ctx, sourceWorker, source.id)
		closed, terminal := model.GetContractClose(ctx, id)
		if !terminal || closed.Outcome != model.ContractOutcomeSettled {
			t.Fatal("registered source worker did not close its capped partial intent")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT usage_unverified
				AND (SELECT count(*)=2 AND bool_and(NOT checkpoint AND used_transfer_byte_count=17) FROM contract_close WHERE contract_id=$1)
				AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
				AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1)
				AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
				AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
				FROM transfer_contract WHERE contract_id=$1`, id).Scan(&exact))
			if !exact {
				t.Fatal("source continuation changed positive bytes or acquired financial custody")
			}
		})
	})
}

// A refusal on the second page retains the complete first page and its first
// chunk. A retry rescans from the head and coalesces that committed prefix.
func TestStartupContractClosurePublicationRetryKeepsCommittedPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		ids := newStartupClosureScanContracts(ctx, networkId, sourceId, destinationId, 2049)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		started := server.NowUtc().Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures, &ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started}, owner,
				task.RunOnce("schedule_open_contract_closures_on_startup"), task.RunAt(server.NowUtc()))
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE startup_close_queue_refusal (run_once_key text PRIMARY KEY)`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO startup_close_queue_refusal VALUES($1)`, task.RunOnce("close_scheduled_contract", ids[1280]).String()))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION startup_close_queue_refuse() RETURNS trigger LANGUAGE plpgsql AS $body$
				BEGIN
					IF EXISTS(SELECT 1 FROM startup_close_queue_refusal WHERE run_once_key=NEW.run_once_key) THEN
						RAISE EXCEPTION 'synthetic startup close queue refusal';
					END IF;
					RETURN NEW;
				END $body$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER startup_close_queue_refuse BEFORE INSERT ON pending_task
				FOR EACH ROW EXECUTE FUNCTION startup_close_queue_refuse()`))
		})
		key := task.RunOnce("schedule_open_contract_closures_on_startup").String()
		initial := readExpiryRecoveryQueue(t, ctx)[key]
		worker := startupClosureWorker(ctx, NewStartupContractClosureTaskTarget())
		defer worker.Close()
		makeCloseRetryTaskDue(ctx, initial.id)
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != initial.id {
			t.Fatal("second-page chunk refusal did not retain the original all-page task", finished, retried, posts, err)
		}
		partial := readExpiryRecoveryQueue(t, ctx)
		current, found := partial[key]
		if !found || current.id != initial.id || current.args != initial.args || len(partial) != 1281 {
			t.Fatal("failed page advanced its cursor or lost the committed 1280-child prefix", len(partial))
		}
		for index, id := range ids {
			_, found := partial[task.RunOnce("close_scheduled_contract", id).String()]
			if found != (index < 1280) {
				t.Fatal("failed chunk changed the exact committed prefix", index)
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER startup_close_queue_refuse ON pending_task`))
			server.RaisePgResult(tx.Exec(ctx, `DROP FUNCTION startup_close_queue_refuse()`))
			server.RaisePgResult(tx.Exec(ctx, `DROP TABLE startup_close_queue_refusal`))
		})
		evalStartupClosureTask(t, ctx, worker, initial.id)
		complete := readExpiryRecoveryQueue(t, ctx)
		if len(complete) != 2049 {
			t.Fatal("retried page did not publish the entire bounded tail before EOF", len(complete))
		}
		for index, id := range ids {
			key := task.RunOnce("close_scheduled_contract", id).String()
			row, found := complete[key]
			var args CloseScheduledContractArgs
			if !found || json.Unmarshal([]byte(row.args), &args) != nil || args.ContractId != id ||
				!args.Deadline.Equal(started.Add(model.DefaultContractExpiration)) || !row.runAt.Equal(args.Deadline) {
				t.Fatal("retried page lost a child or changed its original deadline", index)
			}
			if prior, existed := partial[key]; existed && (row.id != prior.id || row.args != prior.args || !row.runAt.Equal(prior.runAt)) {
				t.Fatal("retry replaced an already committed child or delayed its wake", index)
			}
		}
	})
}
