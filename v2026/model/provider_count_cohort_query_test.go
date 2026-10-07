// Real export calls must not fetch unrelated historical eligibility failures.
// This file also compiles against the predecessor for the causal regression.
package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Run statements only inside the test environment's disposable database.
func cohortTestExec(ctx context.Context, query string, args ...any) {
	server.Db(ctx, func(c server.PgConn) { server.RaisePgResult(c.Exec(ctx, query, args...)) })
}

// Repeatable synthetic identities make SQL plan inputs deterministic.
func cohortTestId(n int) server.Id {
	return server.RequireParseId(fmt.Sprintf("00000000-0000-0000-0000-%012x", n))
}

// This is the production population and evidence shape, with a dense historical
// exception population. It does not reproduce Main's full cardinality or CPU.
func cohortTestTables(ctx context.Context) {
	cohortTestExec(ctx, `
CREATE TABLE location(location_id uuid PRIMARY KEY, location_type text NOT NULL, location_name text NOT NULL, city_location_id uuid, region_location_id uuid, country_location_id uuid, country_code text NOT NULL);
CREATE TABLE location_group(location_group_id uuid PRIMARY KEY, location_group_name text NOT NULL, promoted bool NOT NULL);
CREATE TABLE location_group_member(location_group_id uuid, location_id uuid, PRIMARY KEY(location_group_id, location_id));
CREATE TABLE network_client(client_id uuid PRIMARY KEY, active bool NOT NULL DEFAULT true, source_client_id uuid);
CREATE TABLE provide_key(client_id uuid, provide_mode int, PRIMARY KEY(client_id, provide_mode));
CREATE TABLE network_client_location_reliability(client_id uuid PRIMARY KEY, network_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000042', connected bool NOT NULL DEFAULT false, valid bool NOT NULL DEFAULT false, city_location_id uuid, region_location_id uuid, country_location_id uuid, max_net_type_score int NOT NULL DEFAULT 1, max_net_type_score_speed int NOT NULL DEFAULT 1, min_relative_latency_ms int NOT NULL DEFAULT 5, max_bytes_per_second bigint NOT NULL DEFAULT 104857600, has_latency_test bool NOT NULL DEFAULT true, has_speed_test bool NOT NULL DEFAULT true, egress_index smallint, egress_quality bool, arin_risk bool NOT NULL DEFAULT false, arin_non_quality bool NOT NULL DEFAULT false, ipv4_proven bool NOT NULL DEFAULT true, ipv6_proven bool NOT NULL DEFAULT false, payload text NOT NULL DEFAULT '');
CREATE INDEX network_client_location_reliability_arin_exceptions ON network_client_location_reliability(client_id) INCLUDE(arin_risk,arin_non_quality) WHERE arin_risk OR arin_non_quality;
CREATE TABLE client_connection_reliability_score(client_id uuid NOT NULL,lookback_index int NOT NULL,reliability_weight float8 NOT NULL DEFAULT 1,independent_reliability_weight float8 NOT NULL,PRIMARY KEY(client_id,lookback_index));
CREATE TABLE provider_egress_health(client_id uuid PRIMARY KEY,measured_at timestamp NOT NULL DEFAULT now(),reputation_failed_names text NOT NULL DEFAULT '',tls_authentication_failure bool NOT NULL DEFAULT false,legacy_tls_authentication_failure bool NOT NULL DEFAULT false);
CREATE TABLE provider_egress_url_security(client_id uuid NOT NULL,url_key text NOT NULL,tls_failure bool NOT NULL,PRIMARY KEY(client_id,url_key));
CREATE TABLE provider_egress_location(client_id uuid PRIMARY KEY,country_code text,observed_at timestamp NOT NULL);
CREATE TABLE provider_egress_health_history(client_id uuid NOT NULL,measured_at timestamp NOT NULL,ok_count int NOT NULL,total_count int NOT NULL,class_results jsonb NOT NULL,url_probe_policy_version int NOT NULL,PRIMARY KEY(client_id,measured_at));
CREATE TABLE exclude_network_client_location(network_id uuid,client_location_id uuid);
INSERT INTO network_client_location_reliability(client_id,arin_risk,arin_non_quality,payload)
SELECT lpad(to_hex(n),32,'0')::uuid,n%5=0,n%97=0,repeat('x',128) FROM generate_series(10000,259999)n;
INSERT INTO client_connection_reliability_score(client_id,lookback_index,independent_reliability_weight) SELECT lpad(to_hex(n),32,'0')::uuid,l,.2 FROM generate_series(10000,11599)n CROSS JOIN generate_series(1,3)l;
ANALYZE network_client_location_reliability; ANALYZE client_connection_reliability_score;
`)
}

// A two-client live cohort cannot justify returning the fixture's more than
// 50,000 historical failures from each of the three unchanged export callers.
func TestProviderCountCohortExportQueriesBounded(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		var pgss bool
		server.Db(ctx, func(c server.PgConn) {
			server.Raise(c.QueryRow(ctx, `SELECT current_setting('shared_preload_libraries') LIKE '%pg_stat_statements%'`).Scan(&pgss))
		})
		if !pgss {
			t.Skip("requires an isolated pg_stat_statements fixture")
		}
		cohortTestTables(ctx)
		country := cohortTestId(1000000)
		cohortTestExec(ctx, `INSERT INTO location VALUES($1,'country','Synthetic',NULL,NULL,NULL,'zz')`, country)
		for n := 1; n <= 2; n++ {
			id := cohortTestId(n)
			cohortTestExec(ctx, `INSERT INTO network_client(client_id) VALUES($1)`, id)
			cohortTestExec(ctx, `INSERT INTO provide_key VALUES($1,$2)`, id, ProvideModePublic)
			cohortTestExec(ctx, `INSERT INTO network_client_location_reliability(client_id,connected,valid,city_location_id,region_location_id,country_location_id,arin_risk) VALUES($1,true,true,$2,$2,$2,$3)`, id, country, n == 2)
		}
		cohortTestExec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements; SELECT pg_stat_statements_reset()`)
		CountProviderEgress(ctx)
		if err := UpdateClientLocations(ctx, time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := UpdateClientScores(ctx, time.Hour, 2); err != nil {
			t.Fatal(err)
		}
		var calls, rows, unscoped int64
		server.Db(ctx, func(c server.PgConn) {
			server.Raise(c.QueryRow(ctx, `SELECT coalesce(sum(calls),0)::bigint,coalesce(sum(rows),0)::bigint,coalesce(sum(calls) FILTER(WHERE query NOT LIKE '%ANY($1)%'),0)::bigint FROM pg_stat_statements WHERE query LIKE 'WITH failed_reliability%'`).Scan(&calls, &rows, &unscoped))
		})
		if calls != 3 || rows != 3 || unscoped != 0 {
			t.Fatalf("export eligibility escaped captured cohort: calls=%d returned_rows=%d unscoped_calls=%d; want 3,3,0", calls, rows, unscoped)
		}
		t.Logf("cohort work: live_clients=2 historical_rollups=250000 dense_exceptions_over=50000 calls=%d returned_rows=%d unscoped_calls=%d", calls, rows, unscoped)
	})
}
