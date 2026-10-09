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

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
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
			scheduleOpenContractClosuresPage(owner, tx, &ScheduleOpenContractClosuresArgs{StartedAt: started})
			ScheduleOpenContractClosuresOnStartup(owner, tx)
		})
		key := task.RunOnce("schedule_open_contract_closures").String()
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

// The second page retains the first timestamp; a later startup never replaces
// its PK cursor or postpones an already published contract's earlier wake.
func TestStartupContractClosurePagesKeepCapAndEarliestWake(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		ids := newStartupClosureScanContracts(ctx, networkId, sourceId, destinationId, 10001)
		var stateLock sync.Mutex
		maxOwnedKeys := 0
		ownedKeys := map[server.PgOwnershipKey]bool{}
		ctx = server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted {
				stateLock.Lock()
				maxOwnedKeys = max(maxOwnedKeys, len(event.Keys))
				for _, key := range event.Keys {
					ownedKeys[key] = true
				}
				stateLock.Unlock()
			}
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		started := server.NowUtc().Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleOpenContractClosuresPage(owner, tx, &ScheduleOpenContractClosuresArgs{StartedAt: started})
		})
		worker := startupClosureWorker(ctx, NewStartupContractClosureTaskTarget())
		defer worker.Close()
		key := task.RunOnce("schedule_open_contract_closures").String()
		first := readExpiryRecoveryQueue(t, ctx)[key]
		evalStartupClosureTask(t, ctx, worker, first.id)
		queue := readExpiryRecoveryQueue(t, ctx)
		second, found := queue[key]
		var args ScheduleOpenContractClosuresArgs
		if !found || json.Unmarshal([]byte(second.args), &args) != nil || args.After == nil || *args.After != ids[9999] || !args.StartedAt.Equal(started) {
			t.Fatal("first keyset page did not publish exactly 10000 ordered contracts with its fixed cap")
		}
		if len(queue) != 10001 {
			t.Fatal("first keyset page did not commit exactly 10000 children and its continuation", len(queue))
		}
		if _, present := queue[task.RunOnce("close_scheduled_contract", ids[10000]).String()]; present {
			t.Fatal("first keyset page crossed its 10000-row bound")
		}
		stateLock.Lock()
		maxKeys := maxOwnedKeys
		ownedScan := ownedKeys[task.RunOnceOwnershipKey(task.RunOnce("schedule_open_contract_closures"))] &&
			ownedKeys[server.NewPgOwnershipKey("finished_task/task_id", first.id)]
		stateLock.Unlock()
		if maxKeys != 256 || !ownedScan {
			t.Fatal("10000-row page lost bounded child publication or the scanner completion owners", maxKeys, ownedScan)
		}
		// Deterministically remove earlier rows from the open set between
		// page reads. An increasing OFFSET would now miss the remaining row.
		for _, id := range ids[:2] {
			server.Raise(model.CloseContract(ctx, id, sourceId, 17, false))
			server.Raise(model.CloseContract(ctx, id, destinationId, 17, false))
			if _, terminal := model.GetContractClose(ctx, id); !terminal {
				t.Fatal("public report owner did not close the first-page fixture")
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleOpenContractClosuresPage(owner, tx, &ScheduleOpenContractClosuresArgs{StartedAt: started.Add(10 * time.Minute)})
		})
		coalesced := readExpiryRecoveryQueue(t, ctx)[key]
		if coalesced.id != second.id || coalesced.args != second.args || coalesced.runAt.After(second.runAt) {
			t.Fatal("duplicate startup discarded progress or postponed its request")
		}
		evalStartupClosureTask(t, ctx, worker, second.id)
		queue = readExpiryRecoveryQueue(t, ctx)
		for _, id := range ids {
			row, found := queue[task.RunOnce("close_scheduled_contract", id).String()]
			var closeArgs CloseScheduledContractArgs
			if !found || json.Unmarshal([]byte(row.args), &closeArgs) != nil || !closeArgs.Deadline.Equal(started.Add(model.DefaultContractExpiration)) || !row.runAt.Equal(closeArgs.Deadline) {
				t.Fatal("PK page omitted a contract or restarted its lifetime")
			}
		}
		// A later scan reaching this final row with a later timestamp and
		// stored expiry must keep the existing child's earlier args and wake.
		prior := queue[task.RunOnce("close_scheduled_contract", ids[10000]).String()]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, ids[10000], started.Add(4*time.Hour)))
		})
		result, err := ScheduleOpenContractClosures(&ScheduleOpenContractClosuresArgs{StartedAt: started.Add(20 * time.Minute), After: &ids[9999]}, owner)
		server.Raise(err)
		keys := []server.PgOwnershipKey{task.RunOnceOwnershipKey(task.RunOnce("schedule_open_contract_closures"))}
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			server.Raise(ScheduleOpenContractClosuresPost(&ScheduleOpenContractClosuresArgs{StartedAt: started.Add(20 * time.Minute)}, result, owner, tx))
		}, server.TxReadCommitted, server.OptNoRetry())
		after := readExpiryRecoveryQueue(t, ctx)[task.RunOnce("close_scheduled_contract", ids[10000]).String()]
		if after.id != prior.id || after.args != prior.args || !after.runAt.Equal(prior.runAt) {
			t.Fatal("later startup changed an earlier contract wake or its deadline authority")
		}
	})
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
// receives its real owner and registration wake; startup never edits its proof.
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
					(SELECT row_to_json(i) FROM legacy_settlement_intent i WHERE contract_id=$1),
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
			server.Raise(conn.QueryRow(ctx, `SELECT expiration_time FROM transfer_contract WHERE contract_id=$1`, id).Scan(&capped))
			if !capped.Equal(deadline) {
				t.Fatal("accepted intent did not receive its due retirement cap")
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

// The due child feeds the real registration dispatcher and source worker for
// a retained free intent; no synthetic financial settlement completes it.
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
					(SELECT row_to_json(i) FROM legacy_settlement_intent i WHERE contract_id=$1),
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
			var nonterminal, verified, missingProof, exactCap, unclassified bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL,NOT usage_unverified,provider_usage IS NULL,
				expiration_time=$2,EXISTS(SELECT 1 FROM legacy_settlement_intent
					WHERE contract_id=$1 AND source_client_id IS NULL AND payer_network_id IS NULL)
				FROM transfer_contract WHERE contract_id=$1`, id, deadline).Scan(&nonterminal, &verified, &missingProof, &exactCap, &unclassified))
			if !nonterminal || !verified || !missingProof || !exactCap || !unclassified {
				t.Fatal("child bypassed accepted source intent or rewrote its proof", nonterminal, verified, missingProof, exactCap, unclassified)
			}
		})
		if read() != before {
			t.Fatal("child changed accepted source intent, reports or provider proof")
		}
		dispatcher := startupClosureWorker(ctx, NewLegacySettlementDispatcherTaskTarget())
		defer dispatcher.Close()
		registration := readExpiryRecoveryQueue(t, ctx)[task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", shard)).String()]
		evalStartupClosureTask(t, ctx, dispatcher, registration.id)
		server.Db(ctx, func(conn server.PgConn) {
			var registered bool
			server.Raise(conn.QueryRow(ctx, `SELECT source_client_id=$2 AND payer_network_id IS NULL FROM legacy_settlement_intent WHERE contract_id=$1`, id, sourceId).Scan(&registered))
			if !registered {
				t.Fatal("normal registration did not make the retained source intent selectable")
			}
		})
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

// A refused later chunk retains earlier committed children and the same scan
// page. Retrying coalesces those exact keys before publishing the missing tail.
func TestStartupContractClosurePublicationRetryKeepsCommittedPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		ids := newStartupClosureScanContracts(ctx, networkId, sourceId, destinationId, 600)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		started := server.NowUtc().Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleOpenContractClosuresPage(owner, tx, &ScheduleOpenContractClosuresArgs{StartedAt: started})
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE startup_close_queue_refusal (run_once_key text PRIMARY KEY)`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO startup_close_queue_refusal VALUES($1)`, task.RunOnce("close_scheduled_contract", ids[256]).String()))
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
		key := task.RunOnce("schedule_open_contract_closures").String()
		initial := readExpiryRecoveryQueue(t, ctx)[key]
		worker := startupClosureWorker(ctx, NewStartupContractClosureTaskTarget())
		defer worker.Close()
		makeCloseRetryTaskDue(ctx, initial.id)
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != initial.id {
			t.Fatal("second queue chunk refusal did not retain the current scan page", finished, retried, posts, err)
		}
		partial := readExpiryRecoveryQueue(t, ctx)
		current, found := partial[key]
		if !found || current.id != initial.id || current.args != initial.args || len(partial) != 257 {
			t.Fatal("failed page advanced its cursor or lost the committed 256-child prefix", len(partial))
		}
		for index, id := range ids {
			_, found := partial[task.RunOnce("close_scheduled_contract", id).String()]
			if found != (index < 256) {
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
		if len(complete) != 600 {
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
