package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Own one fixture connection so the test can prove that a session setting is
// restored on that same backend. The fixture's original setting is restored
// even when an assertion fails; no production pool setting is changed.
func testingUrlCensusJitConnection(t testing.TB, use func(context.Context, server.PgConn)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	server.Db(ctx, func(conn server.PgConn) {
		var original string
		server.Raise(conn.QueryRow(ctx, "SHOW jit").Scan(&original))
		if original != "on" && original != "off" {
			panic("unexpected fixture JIT setting")
		}
		defer func() {
			if !conn.Conn().IsClosed() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
				defer cleanupCancel()
				server.RaisePgResult(conn.Exec(cleanupCtx, "SET jit="+original))
			}
		}()
		server.RaisePgResult(conn.Exec(ctx, "SET jit=on"))
		use(ctx, conn)
	}, server.OptNoRetry())
}

func testingCheckUrlCensusJitRestored(ctx context.Context, conn server.PgConn, want string) {
	var actual string
	server.Raise(conn.QueryRow(ctx, "SHOW jit").Scan(&actual))
	if actual != want || conn.Conn().PgConn().TxStatus() != 'I' {
		panic(fmt.Sprintf("census setting/transaction leaked: jit=%s status=%c", actual, conn.Conn().PgConn().TxStatus()))
	}
}

func TestProviderUrlProbeFleetJitScopeAndBackend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingUrlCensusJitConnection(t, func(ctx context.Context, conn server.PgConn) {
			var before int
			server.Raise(conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&before))
			for _, initial := range []string{"on", "off"} {
				server.RaisePgResult(conn.Exec(ctx, "SET jit="+initial))
				providerUrlProbeFleetRead(ctx, conn, func(tx server.PgTx) {
					var jit, readOnly, isolation string
					var backend int
					server.Raise(tx.QueryRow(ctx, `SELECT current_setting('jit'),current_setting('transaction_read_only'),
						current_setting('transaction_isolation'),pg_backend_pid()`).Scan(&jit, &readOnly, &isolation, &backend))
					if jit != "off" || readOnly != "on" || isolation != "read committed" || backend != before {
						panic(fmt.Sprintf("census transaction scope differs: jit=%s readonly=%s isolation=%s same_backend=%t", jit, readOnly, isolation, backend == before))
					}
				})
				testingCheckUrlCensusJitRestored(ctx, conn, initial)
				var after int
				server.Raise(conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&after))
				if after != before {
					panic("success control did not reuse the same backend")
				}
			}
			testingUrlCensusCachedJitScope(ctx, conn)
		})
	})
}

// A cached generic plan prepared while JIT is on must also honor the local
// setting. The connection and prepared statement stay the same across all
// three executions; the fixture restores its session setting on every exit.
func testingUrlCensusCachedJitScope(ctx context.Context, conn server.PgConn) {
	var originalCost string
	server.Raise(conn.QueryRow(ctx, "SHOW jit_above_cost").Scan(&originalCost))
	name := fmt.Sprintf("url_census_jit_%d", time.Now().UnixNano())
	prepared := false
	defer func() {
		if !conn.Conn().IsClosed() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cleanupCancel()
			if prepared {
				server.Raise(conn.Conn().Deallocate(cleanupCtx, name))
			}
			server.RaisePgResult(conn.Exec(cleanupCtx, "SELECT set_config('jit_above_cost',$1,false)", originalCost))
		}
	}()
	server.RaisePgResult(conn.Exec(ctx, "SET jit=on"))
	server.RaisePgResult(conn.Exec(ctx, "SET jit_above_cost=0"))
	_, err := conn.Conn().Prepare(ctx, name, "SELECT SUM(i) FROM generate_series(1,100) AS sample(i)")
	server.Raise(err)
	prepared = true
	explain := "EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE " + pgx.Identifier{name}.Sanitize()
	checkPlan := func(query func(string, ...any) pgx.Row, wantJit bool) {
		var raw []byte
		server.Raise(query(explain).Scan(&raw))
		if len(raw) > 64*1024 {
			panic("cached census JIT plan exceeds fixture bound")
		}
		var plans []struct {
			JIT struct{ Functions int }
		}
		server.Raise(json.Unmarshal(raw, &plans))
		if len(plans) != 1 || (plans[0].JIT.Functions > 0) != wantJit {
			panic("cached census plan did not honor the scoped JIT setting")
		}
	}
	checkPlan(func(query string, args ...any) pgx.Row { return conn.QueryRow(ctx, query, args...) }, true)
	providerUrlProbeFleetRead(ctx, conn, func(tx server.PgTx) {
		checkPlan(func(query string, args ...any) pgx.Row { return tx.QueryRow(ctx, query, args...) }, false)
	})
	testingCheckUrlCensusJitRestored(ctx, conn, "on")
	checkPlan(func(query string, args ...any) pgx.Row { return conn.QueryRow(ctx, query, args...) }, true)
	var generic, custom int64
	server.Raise(conn.QueryRow(ctx, "SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name=$1", name).Scan(&generic, &custom))
	if generic != 3 || custom != 0 {
		panic("cached census JIT control did not reuse one generic statement")
	}
}

func TestProviderUrlProbeFleetJitFailuresRestoreAndRetainCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingUrlCensusJitConnection(t, func(ctx context.Context, conn server.PgConn) {
			sentinel := errors.New("synthetic census decode failure")
			failure := server.HandleError(func() {
				providerUrlProbeFleetRead(ctx, conn, func(server.PgTx) { panic(sentinel) })
			})
			if failure != sentinel {
				panic("census cleanup replaced the original non-SQL failure")
			}
			testingCheckUrlCensusJitRestored(ctx, conn, "on")
			failure = server.HandleError(func() {
				providerUrlProbeFleetRead(ctx, conn, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, "SELECT 1/0"))
				})
			})
			err, ok := failure.(error)
			var pgErr *pgconn.PgError
			if !ok || !errors.As(err, &pgErr) || pgErr.Code != "22012" {
				panic("census cleanup replaced the exact PostgreSQL error")
			}
			testingCheckUrlCensusJitRestored(ctx, conn, "on")
		})
	})
}

func TestProviderUrlProbeFleetJitCancellationIsBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingUrlCensusJitConnection(t, func(ctx context.Context, conn server.PgConn) {
			readCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			defer cancel()
			entered := false
			started := time.Now()
			failure := server.HandleError(func() {
				providerUrlProbeFleetRead(readCtx, conn, func(tx server.PgTx) {
					entered = true
					server.RaisePgResult(tx.Exec(readCtx, "SELECT pg_sleep(30)"))
				})
			})
			if !entered || failure == nil || !errors.Is(readCtx.Err(), context.DeadlineExceeded) || time.Since(started) > 8*time.Second {
				panic("census cancellation did not retain the finite read owner")
			}
			if !conn.Conn().IsClosed() {
				testingCheckUrlCensusJitRestored(ctx, conn, "on")
			}
		})
	})
}

// This closes the missing adverse JIT comparison from the original packet.
// It uses the unchanged census SQL and the same 2m-row background, then runs
// the actual production helper under its original ten-second model deadline.
func TestUrlProbeCensusAdverseJitAndOwnedRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlCensusDeadlineFleet(t, now)
		testingAddUrlCensusHistoryBackground(t, now)
		query := providerUrlProbeFleetSql(1)
		before, beforeErr := testingUrlCensusBoundedValues(t, query, now)
		if beforeErr != nil || len(before) != 22 {
			t.Fatalf("adverse census baseline failed: %v", beforeErr)
		}
		for _, mode := range []string{"custom", "generic"} {
			for index, jit := range []string{"on", "off", "off", "on"} {
				label := fmt.Sprintf("adverse-current-%s-%d", jit, index)
				plan, err := testingUrlCensusAlternativeExplain(t, label, mode, jit, query, now)
				if err != nil {
					t.Errorf("adverse %s %s failed: %v", mode, label, err)
					continue
				}
				work, workErr := testingCheckUrlCensusAlternativeWork(plan.testingUrlCompletedPlan, testingUrlCensusProviderCount, 2000000)
				t.Logf("adverse JIT %s %s work=%+v planning_ms=%.3f execution_ms=%.3f jit_functions=%d jit_ms=%.3f",
					mode, label, work, plan.PlanningTime, plan.ExecutionTime, plan.JIT.Functions, plan.JIT.Timing.Total)
				if workErr != nil {
					t.Errorf("adverse %s %s: %v", mode, label, workErr)
				}
			}
		}
		readCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		observation := server.NewDbReadObservation()
		started := time.Now()
		fleet := GetProviderUrlProbeFleetObserved(readCtx, now, observation)
		elapsed := time.Since(started)
		read := observation.Snapshot()
		if readCtx.Err() != nil || read.AcquireSucceeded != 1 || read.QuerySucceeded != 1 || read.Rows != 1 ||
			fleet.Eligible != 76000 || fleet.MatureEligible != 76000 || fleet.MatureQuotaComplete != 75920 ||
			fleet.MatureRunsNeeded != 80 || fleet.WarmingEligible != 0 || fleet.EligibilityAgeUnknown != 0 ||
			fleet.MatureDeficitDiagnostics.Selected != 80 || fleet.MatureDeficitDiagnostics.RunsNeeded != 80 {
			t.Fatalf("owned JIT census changed exact quota/deadline/phases: fleet=%+v read=%+v", fleet, read)
		}
		t.Logf("actual owned JIT-off census wall=%s acquire=%s query=%s", elapsed, read.AcquireDuration, read.QueryDuration)
		after, afterErr := testingUrlCensusBoundedValues(t, query, now)
		if afterErr != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("owned read changed exact22 values: %v", afterErr)
		}
	})
}
