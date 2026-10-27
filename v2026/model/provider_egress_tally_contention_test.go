// Real database lock barriers verify additive tally concurrency and rollback.
package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// A nontransactional sequence counts actual attempts of the production place
// upsert, including attempts rolled back by PostgreSQL serialization conflicts.
func testingTallyAttemptCounter(ctx context.Context) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE synthetic_tally_attempts;
			CREATE FUNCTION synthetic_count_tally_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN PERFORM nextval('synthetic_tally_attempts'); RETURN NEW; END $$;
			CREATE TRIGGER synthetic_tally_attempt BEFORE INSERT ON provider_egress_place_tally
			FOR EACH ROW EXECUTE FUNCTION synthetic_count_tally_attempt()`))
	})
}

// Committing an already-locked place while a real tally call waits must not
// replay the whole additive transaction. The observed database lock dependency
// is the barrier; elapsed time and goroutine scheduling are not the verdict.
func TestProviderEgressRunTallyConcurrentPlaceDoesNotReplay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		var worker sync.WaitGroup
		defer worker.Wait()
		defer cancel()
		day := time.Date(2026, time.October, 5, 0, 0, 0, 0, time.UTC)
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "ZZ", Region: "Synthetic place"}, Healthy: true}
		loads := []ProviderEgressSiteLoad{{Name: "synthetic.example", Ok: true, Healthy: true}}
		AddProviderEgressRunTally(ctx, day, run, loads)
		testingTallyAttemptCounter(ctx)
		finished := make(chan any, 1)
		server.Tx(ctx, func(tx server.PgTx) {
			var holderPid int
			server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_place_tally
				SET update_time=update_time+interval '1 microsecond'
				WHERE tally_day=$1 AND country_code='zz' AND region=$2`, day, run.Place.Region))
			worker.Add(1)
			go func() {
				defer worker.Done()
				finished <- server.HandleError(func() { AddProviderEgressRunTally(ctx, day, run, loads) })
			}()
			server.Db(ctx, func(conn server.PgConn) {
				for {
					var blocked bool
					server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (
						SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
						AND $1=ANY(pg_blocking_pids(pid))
						AND query LIKE '%INSERT INTO provider_egress_place_tally%')`, holderPid).Scan(&blocked))
					if blocked {
						return
					}
				}
			})
		}, server.OptNoRetry())
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
		var attempts int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT last_value FROM synthetic_tally_attempts`).Scan(&attempts))
		})
		places := GetProviderEgressPlaceTallies(ctx, day)
		sites := GetProviderEgressSiteTallies(ctx, day)
		if len(places) != 1 || places[0].RunCount != 2 || places[0].HealthyRunCount != 2 ||
			len(sites) != 1 || sites[0].LoadCount != 2 || sites[0].HealthyLoadCount != 2 {
			t.Fatalf("whole-run counters changed: places=%+v sites=%+v", places, sites)
		}
		if attempts != 1 {
			t.Fatalf("one forced concurrent place update caused %d tally transaction attempts; want 1", attempts)
		}
	})
}

// A later site's write failure must undo both the place increment and
// an earlier site's successful increment in the same run.
func TestProviderEgressRunTallySiteFailureRollsBackWholeRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		day := time.Date(2026, time.October, 5, 0, 0, 0, 0, time.UTC)
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "ZZ", Region: "Synthetic place"}, Healthy: true}
		loads := []ProviderEgressSiteLoad{
			{Name: "a-synthetic.example", Ok: true, Healthy: true},
			{Name: "z-synthetic.example", Ok: true, Healthy: true},
		}
		AddProviderEgressRunTally(ctx, day, run, loads)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_late_site_failure()
				RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
				IF NEW.name='z-synthetic.example' THEN
					RAISE EXCEPTION 'synthetic later site failure' USING ERRCODE='P0001';
				END IF; RETURN NEW; END $$;
				CREATE TRIGGER synthetic_late_site_failure BEFORE INSERT ON provider_egress_site_tally
				FOR EACH ROW EXECUTE FUNCTION synthetic_late_site_failure()`))
		})
		failure := server.HandleError(func() { AddProviderEgressRunTally(ctx, day, run, loads) })
		err, ok := failure.(error)
		var pgErr *pgconn.PgError
		if !ok || !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
			t.Fatalf("later site write failure=%v, want synthetic P0001", failure)
		}
		places := GetProviderEgressPlaceTallies(ctx, day)
		sites := GetProviderEgressSiteTallies(ctx, day)
		if len(places) != 1 || places[0].RunCount != 1 || places[0].HealthyRunCount != 1 || len(sites) != 2 {
			t.Fatalf("failed run changed counters: places=%+v sites=%+v", places, sites)
		}
		for _, site := range sites {
			if site.LoadCount != 1 || site.HealthyLoadCount != 1 {
				t.Fatalf("failed run changed a site counter: %+v", site)
			}
		}
	})
}
