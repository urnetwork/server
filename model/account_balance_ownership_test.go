// Account ownership is shared by real provider and paid-completion entry points.
package model

import (
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

type accountOwnershipTestRun struct {
	done chan struct{}
	err  error
}

func startAccountOwnershipTest(run func()) *accountOwnershipTestRun {
	result := &accountOwnershipTestRun{done: make(chan struct{})}
	go func() {
		defer close(result.done)
		server.HandleError(run, func(err error) { result.err = err })
	}()
	return result
}

func (self *accountOwnershipTestRun) join(t testing.TB, ctx context.Context) error {
	t.Helper()
	select {
	case <-self.done:
		return self.err
	case <-ctx.Done():
		t.Fatal("account ownership participant did not join", ctx.Err())
		return ctx.Err()
	}
}

func accountOwnershipTestPayment(ctx context.Context, networkId server.Id, bytes, revenue int64, record string) *AccountPayment {
	id := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,wallet_id,
          payout_byte_count,payout_nano_cents,min_sweep_time,payment_record,circle_idempotency_key)
          VALUES($1,$2,$3,NULL,$4,$5,$6,$7,$8)`, id, server.NewId(), networkId, bytes, revenue, server.NowUtc(), record, server.NewId()))
	}, server.TxReadCommitted, server.OptNoRetry())
	payment, err := GetPayment(ctx, id)
	server.Raise(err)
	return payment
}

func accountOwnershipTestWorker(ctx context.Context) *task.TaskWorker {
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	worker.AddTargets(NewLegacyProviderTotalsTaskTarget())
	return worker
}

func evalAccountOwnershipTestWorker(worker *task.TaskWorker, expected server.Id) {
	finished, retried, posts, err := worker.EvalTasks(1)
	server.Raise(err)
	if len(finished) != 1 || finished[0] != expected || len(retried)+len(posts) != 0 {
		panic(errors.New("registered provider did not finalize its exact owner once"))
	}
}

func accountOwnershipRequireFinished(t testing.TB, ctx context.Context, original map[server.Id]*task.Task) {
	t.Helper()
	ids := make([]server.Id, 0, len(original))
	for id := range original {
		ids = append(ids, id)
	}
	if len(task.GetTasks(ctx, ids...)) != 0 {
		t.Fatal("account ownership left a durable provider pending")
	}
	finished := task.GetFinishedTasks(ctx, ids...)
	if len(finished) != len(ids) {
		t.Fatal("account ownership lost a finished provider identity")
	}
	for id, before := range original {
		after := finished[id]
		if after == nil || !after.PostCompleted || after.ResultJson != "{}" || after.RunStartTime.IsZero() || after.RunEndTime.IsZero() || after.RunEndTime.Before(after.RunStartTime) {
			t.Fatal("account ownership lost exact durable handback")
		}
		want, err := decodeLegacyProviderTotals(before.ArgsJson)
		server.Raise(err)
		got, err := decodeLegacyProviderTotals(after.ArgsJson)
		server.Raise(err)
		want.Applied = true
		if !reflect.DeepEqual(want, got) {
			t.Fatal("account ownership changed immutable provider allocation")
		}
	}
}

// A real registered provider owns the account row at a held write acknowledgement.
// The fixture hook keeps SQL timeouts unchanged. Both paid APIs and a valid
// three-provider payload queue before SQL; an independent provider completes.
func TestAccountBalanceOwnerCoversProvidedAndBothPaidWriters(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkIds := []server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId()}
		hot := networkIds[0]
		holderId := providerTotalsTestTask(ctx, server.NewId(), hot)
		multiId := providerTotalsTestPublish(ctx, server.NewId(), map[server.Id]*contractPayout{
			hot: {payoutByteCount: 31, payout: 43}, networkIds[1]: {payoutByteCount: 3, payout: 5}, networkIds[2]: {payoutByteCount: 7, payout: 11},
		})
		independentId := providerTotalsTestTask(ctx, server.NewId(), networkIds[3])
		original := task.GetTasks(ctx, holderId, multiId, independentId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=ANY($1)`,
				[]server.Id{holderId, multiId, independentId}, server.NowUtc().Add(time.Hour)))
		}, server.TxReadCommitted, server.OptNoRetry())
		firstPayment := accountOwnershipTestPayment(ctx, hot, 7, 11, "synthetic-first-completion")
		secondPayment := accountOwnershipTestPayment(ctx, hot, 13, 19, "synthetic-observed-completion")
		entered, releaseOwner := make(chan uint32, 1), make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseOwner) }) }
		defer release()
		providerTotalsBatchDue(ctx, []server.Id{holderId})
		var reruns atomic.Int32
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		holderCtx := context.WithValue(ctx, accountBalanceWriteTestKey{}, func(writeCtx context.Context, tx server.PgTx, networkId server.Id) {
			if networkId != hot {
				panic(errors.New("fixture held the wrong provider write"))
			}
			var pid uint32
			server.Raise(tx.QueryRow(writeCtx, `SELECT pg_backend_pid()`).Scan(&pid))
			entered <- pid
			select {
			case <-releaseOwner:
			case <-writeCtx.Done():
				server.Raise(writeCtx.Err())
			}
		})
		holderWorker := accountOwnershipTestWorker(holderCtx)
		defer holderWorker.Close()
		holder := startAccountOwnershipTest(func() { evalAccountOwnershipTestWorker(holderWorker, holderId) })
		defer func() { release(); holder.join(t, ctx) }()
		var heldPid uint32
		select {
		case heldPid = <-entered:
		case <-holder.done:
			t.Fatal("registered provider never reached its actual account write", holder.err)
		case <-ctx.Done():
			t.Fatal("provider write acknowledgement did not arrive", ctx.Err())
		}
		key := server.NewPgOwnershipKey("account_balance", hot)
		var held atomic.Bool
		held.Store(true)
		waiting := make(chan int, 3)
		var premature atomic.Int32
		observe := func(slot int) context.Context {
			var once sync.Once
			return server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
				if !slices.Contains(event.Keys, key) {
					return // Queue-only finalization is a separate ownership phase.
				}
				if event.Kind == server.PgOwnershipWaiting {
					once.Do(func() { waiting <- slot })
				}
				if event.Kind == server.PgOwnershipAdmitted && held.Load() {
					premature.Add(1)
				}
			})
		}
		firstCtx, secondCtx, multiCtx := observe(0), observe(1), observe(2)
		first := startAccountOwnershipTest(func() {
			server.Raise(CompletePayment(firstCtx, firstPayment.PaymentId, "synthetic-receipt", "synthetic-hash-one"))
		})
		second := startAccountOwnershipTest(func() {
			complete, canceled, err := ApplyProviderPaymentOutcome(secondCtx, secondPayment, "COMPLETE", "synthetic-receipt", "synthetic-hash-two", false)
			server.Raise(err)
			if !complete || canceled {
				panic(errors.New("real payment outcome lost complete state"))
			}
		})
		defer func() { release(); first.join(t, ctx); second.join(t, ctx) }()
		providerTotalsBatchDue(ctx, []server.Id{multiId})
		multiWorker := accountOwnershipTestWorker(multiCtx)
		defer multiWorker.Close()
		multi := startAccountOwnershipTest(func() { evalAccountOwnershipTestWorker(multiWorker, multiId) })
		defer func() { release(); multi.join(t, ctx) }()
		seen := map[int]bool{}
		for len(seen) != 3 {
			select {
			case slot := <-waiting:
				seen[slot] = true
			case <-holder.done:
				t.Fatal("real held provider ended before all conflicting writers queued", holder.err)
			case <-first.done:
				t.Fatal("first paid writer entered SQL instead of queuing", first.err)
			case <-second.done:
				t.Fatal("observed paid writer entered SQL instead of queuing", second.err)
			case <-multi.done:
				t.Fatal("three-provider writer bypassed complete key admission", multi.err)
			case <-ctx.Done():
				t.Fatal("conflicting writers never acknowledged ownership admission", ctx.Err())
			case <-time.After(time.Millisecond):
				server.Db(ctx, func(conn server.PgConn) {
					var blocked bool
					server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
                      WHERE datname=current_database() AND $1::integer=ANY(pg_blocking_pids(pid)))`, heldPid).Scan(&blocked))
					if blocked {
						t.Fatal("an actual account writer waited on the held provider business transaction")
					}
				}, server.OptNoRetry())
			}
		}
		providerTotalsBatchDue(ctx, []server.Id{independentId})
		independentWorker := accountOwnershipTestWorker(ctx)
		defer independentWorker.Close()
		evalAccountOwnershipTestWorker(independentWorker, independentId)
		server.Db(ctx, func(conn server.PgConn) {
			var stillHeld bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND state='idle in transaction' AND backend_xid IS NOT NULL)`, heldPid).Scan(&stillHeld))
			if !stillHeld || premature.Load() != 0 {
				t.Fatal("conflicting business entered while actual provider remained held")
			}
		}, server.OptNoRetry())
		held.Store(false)
		release()
		for _, run := range []*accountOwnershipTestRun{holder, multi, first, second} {
			if err := run.join(t, ctx); err != nil {
				t.Fatal("account writer failed after release", err)
			}
		}
		if reruns.Load() != 0 {
			t.Fatal("common account writers automatically replayed transactions")
		}
		accountOwnershipRequireFinished(t, ctx, original)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
            EXISTS(SELECT 1 FROM account_balance WHERE network_id=$1 AND provided_byte_count=48 AND provided_net_revenue_nano_cents=72 AND paid_byte_count=20 AND paid_net_revenue_nano_cents=30)
            AND EXISTS(SELECT 1 FROM account_balance WHERE network_id=$2 AND provided_byte_count=3 AND provided_net_revenue_nano_cents=5)
            AND EXISTS(SELECT 1 FROM account_balance WHERE network_id=$3 AND provided_byte_count=7 AND provided_net_revenue_nano_cents=11)
            AND EXISTS(SELECT 1 FROM account_balance WHERE network_id=$4 AND provided_byte_count=17 AND provided_net_revenue_nano_cents=29)
            AND (SELECT count(*) FROM account_payment WHERE payment_id=ANY($5) AND completed AND NOT canceled AND contract_retention_pending)=2`,
				networkIds[0], networkIds[1], networkIds[2], networkIds[3], []server.Id{firstPayment.PaymentId, secondPayment.PaymentId}).Scan(&exact))
			if !exact {
				t.Fatal("paid/provided exact accounting or completion markers changed")
			}
		}, server.OptNoRetry())
		if err := CompletePayment(ctx, firstPayment.PaymentId, "replayed-receipt", "synthetic-hash-one"); err == nil {
			t.Fatal("paid replay was acknowledged twice")
		}
		if _, _, err := ApplyProviderPaymentOutcome(ctx, secondPayment, "COMPLETE", "replayed-receipt", "synthetic-hash-two", false); !errors.Is(err, ErrProviderPaymentAttemptChanged) {
			t.Fatal("attempt replay bypassed retained completion", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT paid_byte_count=20 AND paid_net_revenue_nano_cents=30 FROM account_balance WHERE network_id=$1`, hot).Scan(&exact))
			if !exact {
				t.Fatal("replayed completion repeated paid accounting")
			}
		}, server.OptNoRetry())
	})
}

// A durable network change between the unlocked admission read and BEGIN must
// refuse every credit. Exact amounts remain authoritative only after row lock.
func TestAccountBalanceOwnerRejectsChangedDurableProviderMembership(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId, changedNetworkId := server.NewId(), server.NewId()
		id := providerTotalsTestTask(ctx, server.NewId(), networkId)
		providerTotalsBatchDue(ctx, []server.Id{id})
		var mutated atomic.Bool
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind != server.PgOwnershipAdmitted || !mutated.CompareAndSwap(false, true) {
				return
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=jsonb_set(args_json::jsonb,'{totals,0,network_id}',to_jsonb($2::uuid))::text WHERE task_id=$1`, id, changedNetworkId))
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		worker := accountOwnershipTestWorker(observed)
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != id || !mutated.Load() {
			t.Fatal("changed membership did not retain its exact durable owner", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM account_balance WHERE network_id=ANY($1))
              AND NOT (SELECT (args_json::jsonb->>'applied')::boolean FROM pending_task WHERE task_id=$2)`, []server.Id{networkId, changedNetworkId}, id).Scan(&untouched))
			if !untouched {
				t.Fatal("stale admitted keys authorized a different account write")
			}
		}, server.OptNoRetry())
	})
}

// Historical review-only completion has no account identity and performs no
// paid credit. Preserve that synchronous lifecycle while owning its payment row.
func TestAccountBalanceOwnerKeepsReviewOnlyHistoricalCompletion(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		id := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,wallet_id,
              payout_byte_count,payout_nano_cents,min_sweep_time,attribution_review_required)
              VALUES($1,$2,NULL,NULL,7,11,$3,true)`, id, server.NewId(), server.NowUtc()))
		}, server.TxReadCommitted, server.OptNoRetry())
		server.Raise(CompletePayment(ctx, id, "synthetic-review-receipt", "synthetic-review-hash"))
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT completed AND attribution_review_required AND NOT contract_retention_pending
             AND network_id IS NULL AND NOT EXISTS(SELECT 1 FROM account_balance) FROM account_payment WHERE payment_id=$1`, id).Scan(&exact))
			if !exact {
				t.Fatal("review-only completion invented an account projection")
			}
		}, server.OptNoRetry())
	})
}

// The accounting commit is real and only its caller's reply is lost. The exact
// applied marker remains the authority for a subsequent registered task owner.
func TestAccountBalanceOwnerLostReplyRetainsExactProviderReplay(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		networkId := server.NewId()
		id := providerTotalsTestTask(ctx, server.NewId(), networkId)
		original := task.GetTasks(ctx, id)
		calls := 0
		err := runLegacyProviderTotalsTxWithOwner(ctx, func(tx server.PgTx) error {
			calls++
			return applyLegacyProviderTotalsWithOwnershipInTx(ctx, tx, id, []server.Id{networkId})
		}, func(ctx context.Context, callback func(server.PgTx), options ...any) {
			server.OwnedTx(ctx, accountBalanceOwnershipKeys([]server.Id{networkId}), callback, options...)
			panic(io.ErrUnexpectedEOF)
		})
		if !errors.Is(err, io.ErrUnexpectedEOF) || calls != 1 {
			t.Fatal("unknown caller reply repeated or hid provider accounting", err, calls)
		}
		var phase *legacyProviderTotalsPhaseError
		if !errors.As(err, &phase) || phase.phase != legacyProviderTotalsCommit {
			t.Fatal("lost reply changed commit-phase diagnostics", err)
		}
		requireProviderTotalsTestState(t, ctx, id, networkId, true, 17, 29)
		providerTotalsBatchDue(ctx, []server.Id{id})
		worker := accountOwnershipTestWorker(ctx)
		defer worker.Close()
		evalAccountOwnershipTestWorker(worker, id)
		accountOwnershipRequireFinished(t, ctx, original)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count=17 AND provided_net_revenue_nano_cents=29
              AND (SELECT count(*) FROM test_provider_total_write WHERE network_id=$1)=1 FROM account_balance WHERE network_id=$1`, networkId).Scan(&exact))
			if !exact {
				t.Fatal("recovery repeated an acknowledged provider credit")
			}
		}, server.OptNoRetry())
	})
}

// Completing a payment cannot survive failure of its paired account credit,
// and a serialization-class refusal cannot replay the common owner's body.
func TestAccountBalanceOwnerPaymentFailureRollsBackWithoutRetry(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		payment := accountOwnershipTestPayment(ctx, networkId, 7, 11, "synthetic-refused-completion")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE test_owned_paid_attempt;
              CREATE FUNCTION test_owned_paid_refusal() RETURNS trigger LANGUAGE plpgsql AS $$
              BEGIN
                IF NEW.paid_byte_count > 0 THEN
                  PERFORM nextval('test_owned_paid_attempt');
                  RAISE EXCEPTION 'synthetic paid refusal' USING ERRCODE='40001';
                END IF;
                RETURN NEW;
              END $$;
              CREATE TRIGGER test_owned_paid_refusal BEFORE INSERT OR UPDATE ON account_balance
              FOR EACH ROW EXECUTE FUNCTION test_owned_paid_refusal()`))
		}, server.TxReadCommitted, server.OptNoRetry())
		var reruns atomic.Int32
		observed := server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		var err error
		server.HandleError(func() {
			server.Raise(CompletePayment(observed, payment.PaymentId, "synthetic-receipt", "synthetic-hash"))
		}, func(cause error) { err = cause })
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "40001" || reruns.Load() != 0 {
			t.Fatal("paid refusal lost its cause or reran financial SQL", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT NOT completed AND complete_time IS NULL
              AND NOT EXISTS(SELECT 1 FROM account_balance WHERE network_id=$2)
              AND (SELECT is_called AND last_value=1 FROM test_owned_paid_attempt)
              FROM account_payment WHERE payment_id=$1`, payment.PaymentId, networkId).Scan(&untouched))
			if !untouched {
				t.Fatal("failed paid credit leaked completion or automatically replayed")
			}
		}, server.OptNoRetry())
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER test_owned_paid_refusal ON account_balance`))
		}, server.OptNoRetry())
		server.Raise(CompletePayment(ctx, payment.PaymentId, "synthetic-receipt", "synthetic-hash"))
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT paid_byte_count=7 AND paid_net_revenue_nano_cents=11
              AND EXISTS(SELECT 1 FROM account_payment WHERE payment_id=$2 AND completed AND contract_retention_pending)
              FROM account_balance WHERE network_id=$1`, networkId, payment.PaymentId).Scan(&exact))
			if !exact {
				t.Fatal("explicit recovery lost exact payment/account atomicity")
			}
		}, server.OptNoRetry())
	})
}
