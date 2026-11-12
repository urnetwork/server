// Disposable PostgreSQL instruments only reviewed transaction-local config or
// the final slot UPDATE. Production SQL stays unchanged; actual server wait,
// cancellation and rollback acknowledgements prove three returned failures.
package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Each barrier has one independently owned maintenance connection; neither
// the observed transaction nor its rollback needs to acquire another one.
type competitionHeartbeatPrefixPgBarrier struct {
	pid     int32
	release chan struct{}
	done    chan struct{}
	err     error
	once    sync.Once
}

func (self *competitionHeartbeatPrefixPgBarrier) unlock() {
	self.once.Do(func() { close(self.release) })
}

// A live original claim is renewed, never rerun or given a replacement clock.
func TestCompetitionHeartbeatPrefixRetryPgThreeRollbacksThenFourthSuccess(t *testing.T) {
	testCompetitionHeartbeatReturnedRollbackPg(t, false)
}

// Both exact source graphs share actual server-wait/cancellation/rollback and
// cleanup ownership. Only isolated instrumentation and proof position differ.
func testCompetitionHeartbeatReturnedRollbackPg(t *testing.T, finalSlot bool) {
	t.Helper()
	for _, historicalCap := range []bool{true, false} {
		env := server.DefaultTestEnv()
		env.RerunCount = 0
		env.Run(t, func(tb testing.TB) {
			watchdog, cancelWatchdog := context.WithTimeout(tb.Context(), time.Minute)
			defer cancelWatchdog()
			pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 4\n"))
			server.PgReset()
			defer func() { pop(); server.PgReset() }()
			fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
			selected := fixture.selected(0)
			const worker = "synthetic-prefix-heartbeat-worker"
			job, err := fixture.store.Claim(watchdog, selected, worker, testWorkerImageDigest())
			if err != nil || job == nil {
				tb.Fatal("genuine distributed prefix claim failed", err)
			}
			before := readCompetitionDistributedMergedHeartbeat(watchdog, job.JobId)
			fixture.clock.Advance(time.Second)
			class, firstObject := int32(183302), int32(833021)
			const applicationName = "synthetic-prefix-deadline"
			// The generated local test database is the entire installation
			// scope. No role default or production callback hook is changed.
			server.Tx(watchdog, func(tx server.PgTx) {
				var database string
				server.Raise(tx.QueryRow(watchdog, "SELECT current_database()").Scan(&database))
				if !strings.HasPrefix(database, "test_") {
					panic("prefix instrumentation requires its isolated generated test database")
				}
				if finalSlot {
					server.RaisePgResult(tx.Exec(watchdog, `CREATE SEQUENCE synthetic_returned_slot_calls;
					 CREATE FUNCTION synthetic_returned_slot_latency() RETURNS trigger AS $body$
					 DECLARE call_number bigint;
					 BEGIN
					  IF NEW.job_id::text=TG_ARGV[0] AND NEW.lease_expires_at>OLD.lease_expires_at THEN
					   call_number:=nextval('synthetic_returned_slot_calls'::regclass);
					   IF call_number<=3 THEN
					    PERFORM set_config('lock_timeout','0',true);
					    PERFORM set_config('application_name',TG_ARGV[3],true);
					    PERFORM pg_advisory_xact_lock(TG_ARGV[1]::integer,(TG_ARGV[2]::integer+call_number)::integer);
					   END IF;
					  END IF;
					  RETURN NEW;
					 END $body$ LANGUAGE plpgsql;`))
					server.RaisePgResult(tx.Exec(watchdog, fmt.Sprintf(`CREATE TRIGGER synthetic_returned_slot_latency
					 BEFORE UPDATE OF lease_expires_at ON competition_execution_slot FOR EACH ROW
					 EXECUTE FUNCTION synthetic_returned_slot_latency('%s','%d','%d','%s')`, job.JobId.String(), class, firstObject, applicationName)))
					return
				}
				server.RaisePgResult(tx.Exec(watchdog, fmt.Sprintf(`CREATE SCHEMA synthetic_prefix;
				 CREATE SEQUENCE synthetic_prefix.calls;
				 CREATE FUNCTION synthetic_prefix.set_config(setting_name text,new_value text,is_local boolean) RETURNS text AS $body$
				 DECLARE call_number bigint;
				 BEGIN
				  IF setting_name='urnetwork.competition_v2_instrument_approval' THEN
				   call_number:=pg_catalog.nextval('synthetic_prefix.calls'::pg_catalog.regclass);
				   IF call_number<=3 THEN
				    PERFORM pg_catalog.set_config('lock_timeout','0',true);
				    PERFORM pg_catalog.set_config('application_name','%s',true);
				    PERFORM pg_catalog.pg_advisory_xact_lock(%d::integer,(%d+call_number)::integer);
				   END IF;
				  END IF;
				  RETURN pg_catalog.set_config(setting_name,new_value,is_local);
				 END $body$ LANGUAGE plpgsql;`, applicationName, class, firstObject)))
				server.RaisePgResult(tx.Exec(watchdog, "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" SET search_path=synthetic_prefix,pg_catalog,public"))
			})
			server.PgReset()
			barriers := []*competitionHeartbeatPrefixPgBarrier{}
			operationCtx, cancelOperation := context.WithCancel(watchdog)
			joined, ownerStarted := make(chan struct{}), false
			// Installed before the first goroutine: even partial barrier
			// setup failure releases and physically joins every issued owner.
			defer func() {
				cancelOperation()
				for _, barrier := range barriers {
					barrier.unlock()
				}
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(watchdog), 40*time.Second)
				defer cancel()
				if ownerStarted {
					select {
					case <-joined:
					case <-cleanup.Done():
						tb.Error("real prefix owner did not physically join")
					}
				}
				for _, barrier := range barriers {
					select {
					case <-barrier.done:
						if barrier.err != nil && watchdog.Err() == nil {
							tb.Error("owned prefix barrier failed", barrier.err)
						}
					case <-cleanup.Done():
						tb.Error("owned prefix barrier did not physically join")
					}
				}
			}()
			for ordinal := 1; ordinal <= 3; ordinal++ {
				barrier := &competitionHeartbeatPrefixPgBarrier{release: make(chan struct{}), done: make(chan struct{})}
				barriers = append(barriers, barrier)
				held := make(chan int32, 1)
				object := firstObject + int32(ordinal)
				go func() {
					defer close(barrier.done)
					barrier.err = captureDatabaseError(func() {
						server.MaintenanceTx(watchdog, func(tx server.PgTx) {
							var pid int32
							server.Raise(tx.QueryRow(watchdog, `SELECT pg_backend_pid() FROM pg_advisory_xact_lock($1::integer,$2::integer)`, class, object).Scan(&pid))
							held <- pid
							select {
							case <-barrier.release:
							case <-watchdog.Done():
								panic(watchdog.Err())
							}
						}, server.OptNoRetry())
					})
				}()
				select {
				case barrier.pid = <-held:
				case <-barrier.done:
					tb.Fatal("owned prefix barrier failed before admission", barrier.err)
				case <-watchdog.Done():
					tb.Fatal("owned prefix barrier did not acknowledge its actual lock")
				}
			}
			type dispatch struct {
				ordinal int
				ctx     context.Context
				stopped <-chan struct{}
			}
			type failure struct {
				ordinal int
				err     error
			}
			type result struct {
				ack   time.Time
				calls int
				err   error
			}
			dispatched, failed, done := make(chan dispatch, 1), make(chan failure, 1), make(chan result, 1)
			allowRetry := make(chan struct{}, 3)
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			settings.AcquireRetry.AttemptTimeout = 2 * time.Second
			if historicalCap {
				settings.DistributedReturnedRollbackMaximumAttempts = settings.LeaseRetry.MaximumAttempts
			}
			originalWait := settings.LeaseRetry.Wait
			settings.LeaseRetry.Wait = func(ctx context.Context, delay time.Duration) error {
				select {
				case <-allowRetry:
				case <-ctx.Done():
					return ctx.Err()
				}
				return originalWait(ctx, delay)
			}
			ownerStarted = true
			go func() {
				defer close(joined)
				calls := 0
				ack, err := retryCompetitionJobHeartbeatOperation(operationCtx, *job.LeaseExpiresAt, time.Now().Add(time.Minute), settings, func(attempt context.Context) error {
					calls++
					actual := withCompetitionJobAuthority(attempt, job)
					if calls <= 3 {
						witness := &competitionHeartbeatPgDeadlineContext{Context: actual, stopped: make(chan struct{})}
						actual = witness
						select {
						case dispatched <- dispatch{ordinal: calls, ctx: actual, stopped: witness.stopped}:
						case <-actual.Done():
							return actual.Err()
						}
					}
					err := fixture.store.Heartbeat(actual, selected, worker, job.JobId)
					if err != nil {
						select {
						case failed <- failure{ordinal: calls, err: err}:
						case <-operationCtx.Done():
						}
					}
					return err
				})
				done <- result{ack: ack, calls: calls, err: err}
			}()
			for ordinal := 1; ordinal <= 3; ordinal++ {
				var actual dispatch
				select {
				case actual = <-dispatched:
					if actual.ordinal != ordinal {
						tb.Fatal("prefix attempt dispatch changed order")
					}
				case returned := <-done:
					tb.Fatal("owner returned before its third proved rollback", ordinal, returned.err)
				case <-watchdog.Done():
					tb.Fatal("prefix attempt was not dispatched")
				}
				for {
					pid, err := competitionHeartbeatCallbackWaitingBackend(watchdog, barriers[ordinal-1].pid, applicationName)
					if err != nil {
						tb.Fatal(err)
					}
					if pid != 0 {
						break
					}
					select {
					case returned := <-done:
						tb.Fatal("prefix returned before its actual server wait", returned.err)
					case <-watchdog.Done():
						tb.Fatal("exact supervisor configuration did not reach its advisory barrier")
					case <-time.After(20 * time.Millisecond):
					}
				}
				select {
				case <-actual.ctx.Done():
				case <-watchdog.Done():
					tb.Fatal("actual prefix owner deadline did not expire")
				}
				select {
				case <-actual.stopped:
				case <-watchdog.Done():
					tb.Fatal("actual statement stop did not acknowledge its deadline")
				}
				barriers[ordinal-1].unlock()
				var returned failure
				select {
				case returned = <-failed:
				case <-watchdog.Done():
					tb.Fatal("actual prefix statement/rollback/release did not return")
				}
				proof, ok := returned.err.(competitionHeartbeatDatabaseError)
				if returned.ordinal != ordinal || !ok || proof.snapshot.Transaction.CommitAttempts != 0 ||
					proof.snapshot.Transaction.RollbackAcknowledged != 1 || proof.snapshot.Transaction.ReleaseReturned != 1 || proof.snapshot.Transaction.LastRelease.Disposition != server.DbTxReleaseIdle {
					tb.Fatal("actual heartbeat failed to produce its exact returned rollback proof", ordinal, competitionHeartbeatFailureDetails(returned.err))
				}
				if finalSlot {
					if !competitionHeartbeatDistributedSlotUpdateDeadline(returned.err) || competitionHeartbeatDistributedPreBusinessDeadline(returned.err) ||
						proof.snapshot.Step != competitionHeartbeatDatabaseSlotUpdate || proof.snapshot.SqlStatements != 12 || len(proof.snapshot.Statements) != 12 ||
						proof.snapshot.Statements[10].Step != competitionHeartbeatDatabaseSlotUpdate || proof.snapshot.Statements[10].Succeeded ||
						proof.snapshot.Statements[11].Step != competitionHeartbeatDatabaseJobUpdate || !proof.snapshot.Statements[11].Succeeded {
						tb.Fatal("actual final-slot rollback lost its exact failed slot and completed job UPDATE proof", competitionHeartbeatFailureDetails(returned.err))
					}
				} else if !competitionHeartbeatDistributedPreBusinessDeadline(returned.err) || proof.snapshot.Step != competitionHeartbeatDatabaseSupervisorAuthority || proof.snapshot.SqlStatements != 8 ||
					len(proof.snapshot.Statements) != 8 || proof.snapshot.Statements[7].Ordinal != 2 || proof.snapshot.Statements[7].Kind != "exec" {
					tb.Fatal("actual prefix lost its exact failed configuration proof", competitionHeartbeatFailureDetails(returned.err))
				}
				if after := readCompetitionDistributedMergedHeartbeat(watchdog, job.JobId); !reflect.DeepEqual(before, after) {
					tb.Fatal("rolled-back prefix changed the original committed custody", before, after)
				}
				allowRetry <- struct{}{}
			}
			select {
			case returned := <-done:
				if historicalCap {
					if returned.err == nil || returned.calls != 3 || !returned.ack.IsZero() {
						tb.Fatal("real old three-call cap did not reproduce premature handback", returned.calls, returned.err)
					}
				} else if returned.err != nil || returned.calls != 4 || returned.ack.IsZero() {
					tb.Fatal("real fourth prefix heartbeat lost its original reserve", returned.calls, returned.err)
				}
			case <-watchdog.Done():
				tb.Fatal("bounded original prefix owner did not finish")
			}
			after := readCompetitionDistributedMergedHeartbeat(watchdog, job.JobId)
			if historicalCap {
				if !reflect.DeepEqual(before, after) {
					tb.Fatal("refused old owner mutated its original custody")
				}
			} else if !after.jobLease.After(before.jobLease) || !after.jobLease.Equal(after.slotLease) || !after.startedAt.Equal(before.startedAt) || !after.deadline.Equal(before.deadline) || after.fence != before.fence || after.attempt != before.attempt {
				tb.Fatal("fourth prefix heartbeat changed original clock/fence/attempt or atomicity", before, after)
			}
		})
	}
}
