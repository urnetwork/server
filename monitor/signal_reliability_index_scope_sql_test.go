package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Genuine local catalogs and the unchanged emitted query flow through native
// COPY/CSV and public Run. No shadow catalogs, migrated product rows or sleeps.
func TestReliabilityIndexNativeRelationScopeAndPhysicalContract(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		server.Db(tb.Context(), func(conn server.PgConn) {
			var projected []Row
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				projected = pgGroupedActualSqlRows(tb, conn, query)
				return projected, nil
			}}
			check := func(status, class string) {
				tb.Helper()
				alerts, err := NewReliabilityIndexSignal().Run(context.Background(), syntheticSettings(source))
				if err != nil {
					tb.Fatal("native index-scope public evaluation failed")
				}
				if class == "" {
					if len(alerts) != 0 {
						tb.Fatal("qualified native partitioned contract became anomalous")
					}
				} else if len(alerts) != 1 || alerts[0].Class != class {
					tb.Fatal("native relation scope supplied wrong public class")
				}
				// This assertion follows the public causal check: baseline absent state
				// fails as false health before any new projection-width expectation.
				if len(projected) != 1 || len(projected[0]) != 9 || projected[0][8] != status {
					tb.Fatal("native relation status projection differs")
				}
			}
			exec := func(sql string) { tb.Helper(); server.RaisePgResult(conn.Exec(tb.Context(), sql)) }
			check("absent", "cannot-observe")
			exec(`CREATE SCHEMA synthetic_other; CREATE TABLE synthetic_other.client_reliability(value integer);`)
			check("wrong-schema", "cannot-observe")
			exec(`DROP TABLE synthetic_other.client_reliability; DROP SCHEMA synthetic_other; CREATE TABLE public.client_reliability(value integer);`)
			check("plain", "cannot-observe")
			exec(`DROP TABLE public.client_reliability; CREATE VIEW public.client_reliability AS SELECT 1 AS value;`)
			check("unsupported", "cannot-observe")
			exec(`DROP VIEW public.client_reliability;
CREATE TABLE public.client_reliability(valid boolean,block_number bigint,client_address_hash bytea,network_id uuid,client_id uuid) PARTITION BY RANGE(block_number);
CREATE TABLE public.synthetic_reliability_child PARTITION OF public.client_reliability FOR VALUES FROM (0) TO (10);
CREATE INDEX client_reliability_valid_bnch_net_client ON public.client_reliability USING btree(valid,block_number,client_address_hash) INCLUDE(network_id,client_id);`)
			check("partitioned", "")
			exec(`CREATE INDEX client_reliability_valid_block_number_client_address_hash ON public.client_reliability(valid,block_number,client_address_hash);`)
			check("partitioned", "reliability-index-drift")
			exec(`DROP INDEX public.client_reliability_valid_bnch_net_client;`)
			check("partitioned", "reliability-index-drift")
			exec(`CREATE INDEX client_reliability_valid_bnch_net_client ON ONLY public.client_reliability USING btree(valid,block_number,client_address_hash) INCLUDE(network_id,client_id);`)
			check("partitioned", "reliability-index-drift")
			exec(`DROP INDEX public.client_reliability_valid_block_number_client_address_hash;`)
			check("partitioned", "reliability-index-drift")
			exec(`DROP INDEX public.client_reliability_valid_bnch_net_client; CREATE INDEX client_reliability_valid_bnch_net_client ON public.client_reliability(valid);`)
			check("partitioned", "reliability-index-drift")
		}, server.OptReadWrite())
	})
}

// A prior genuine drift is never cleared across absent, wrong-schema, plain or
// unsupported states; only qualified partitioned health completes the clear.
func TestReliabilityIndexNativeScopeCannotClearNotifierIncident(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		server.Db(tb.Context(), func(conn server.PgConn) {
			exec := func(sql string) { tb.Helper(); server.RaisePgResult(conn.Exec(tb.Context(), sql)) }
			partitioned := `CREATE TABLE public.client_reliability(valid boolean,block_number bigint,client_address_hash bytea,network_id uuid,client_id uuid) PARTITION BY RANGE(block_number);
CREATE TABLE public.synthetic_reliability_child PARTITION OF public.client_reliability FOR VALUES FROM (0) TO (10);
CREATE INDEX client_reliability_valid_block_number_client_address_hash ON public.client_reliability(valid,block_number,client_address_hash);`
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) { return pgGroupedActualSqlRows(tb, conn, query), nil }}
			webhook, url := newFakeSlackWebhook(t)
			harness := newSlackTestHarness()
			notifier := NewSlackAlertNotifier(harness.settings(url))
			signal := NewReliabilityIndexSignal()
			// Direct real public executions feed the actual notifier in deterministic
			// order; two per state exceed the existing clear window without timers.
			observe := func() {
				alerts, err := signal.Run(context.Background(), syntheticSettings(source))
				if err != nil {
					tb.Fatal("native notifier source evaluation failed")
				}
				notifier.Handle(signal, SignalExecution{Alerts: alerts})
			}
			exec(partitioned)
			observe()
			observe()
			exec(`DROP TABLE public.client_reliability CASCADE;`)
			observe()
			observe()
			exec(`CREATE SCHEMA synthetic_other; CREATE TABLE synthetic_other.client_reliability(value integer);`)
			observe()
			observe()
			exec(`DROP TABLE synthetic_other.client_reliability; DROP SCHEMA synthetic_other; CREATE TABLE public.client_reliability(value integer);`)
			observe()
			observe()
			exec(`DROP TABLE public.client_reliability; CREATE VIEW public.client_reliability AS SELECT 1 AS value;`)
			observe()
			observe()
			exec(`DROP VIEW public.client_reliability;`)
			exec(partitioned)
			observe()
			observe()
			closeSlackNotifier(t, notifier)
			texts := webhook.posted()
			identity := "pg/reliability-index · reliability-index-drift"
			if countSlackPosts(texts, "[WARN] raised", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != 0 {
				tb.Fatal("unobserved native scope cleared or reraised original drift")
			}
			// A fresh notifier proves real healthy partitioned state can clear.
			webhook2, url2 := newFakeSlackWebhook(t)
			notifier2 := NewSlackAlertNotifier(harness.settings(url2))
			alerts, err := signal.Run(context.Background(), syntheticSettings(source))
			if err != nil {
				tb.Fatal("native healthy control setup failed")
			}
			notifier2.Handle(signal, SignalExecution{Alerts: alerts})
			exec(`DROP INDEX public.client_reliability_valid_block_number_client_address_hash; CREATE INDEX client_reliability_valid_bnch_net_client ON public.client_reliability USING btree(valid,block_number,client_address_hash) INCLUDE(network_id,client_id);`)
			for i := 0; i < 2; i++ {
				alerts, err = signal.Run(context.Background(), syntheticSettings(source))
				if err != nil {
					tb.Fatal("native healthy control source failed")
				}
				notifier2.Handle(signal, SignalExecution{Alerts: alerts})
			}
			closeSlackNotifier(t, notifier2)
			if countSlackPosts(webhook2.posted(), "[CLEARED]", identity) != 1 {
				tb.Fatal("qualified native index health did not clear")
			}
		}, server.OptReadWrite())
	})
}

// Projection grammar cannot manufacture an off-scope or healthy classification.
func TestReliabilityIndexRelationStatusRejectsMalformedProjection(t *testing.T) {
	for _, row := range []Row{
		{"f", "f", "f", "f", "", "0", "0", "0", "private-status-sentinel"},
		{"t", "f", "f", "f", "", "0", "0", "0", "plain"},
		{"f", "f", "f", "f", "", "0", "0", "0", "partitioned"},
		{"f", "f", "f", "f", "", "0", "0", "0"},
	} {
		settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{row}, nil }})
		_, err := NewReliabilityIndexSignal().Run(context.Background(), settings)
		if err == nil {
			t.Fatal("unqualified relation projection supplied a state")
		}
		if strings.Contains(err.Error(), "private-status-sentinel") {
			t.Fatal("new relation-status parser exposed rejected input")
		}
	}
}
