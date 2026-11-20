// Exercise the remaining external-service adapters without contacting providers.
package work

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Missing store credentials are an explicit skipped result. Internal x402
// recovery still runs; task completion and the next hourly run must persist.
func TestPaymentReconcileTaskPersistsSkippedStoresAndRecurrence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, name := range []string{"stripe.yml", "apple.yml", "google.yml", "helius.yml"} {
			t.Cleanup(server.Vault.PushSimpleResource(name, []byte("{}\n")))
		}
		t.Cleanup(server.Config.PushSimpleResource("play.yml", []byte("{}\n")))
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		id := runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(PaymentReconcile, PaymentReconcilePost, &PaymentReconcileArgs{}), true)
		var result PaymentReconcileResult
		server.Raise(json.Unmarshal([]byte(task.GetFinishedTasks(ctx, id)[id].ResultJson), &result))
		if result.RunId == (server.Id{}) || len(result.SkippedStores) != 4 || result.Errors != 0 || result.Credited != 0 {
			t.Fatal("payment reconciliation adapter lost its source result", result)
		}
	})
}

// An empty initial sync completes and recurs. Cancellation is still an error
// at the real serialized target boundary instead of a false empty success.
func TestInitialProductSyncTaskEmptyBatchAndCancellation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		sample := newAsyncMaintenanceCase(SyncInitialProductUpdates, SyncInitialProductUpdatesPost, &SyncInitialProductUpdatesArgs{})
		runProjectionAuditTask(t, owner, sample, true)
		id := sample.queue(owner)
		queued := task.GetTasks(ctx, id)[id]
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := sample.target.Run(canceled, queued); err == nil {
			t.Fatal("canceled product sync acknowledged success")
		}
	})
}

// Empty evidence leaves the configured sites in their initial probation.
// There are no promotion candidates, so the actual task requires no HTTP calls.
func TestEgressDestinationRefreshTaskSeedsAndReplaysCatalog(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testRefreshCatalog+testRefreshCatalogSize)))
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		for range 2 {
			runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(RefreshEgressDestinations, RefreshEgressDestinationsPost, &RefreshEgressDestinationsArgs{}), true)
			server.Db(ctx, func(conn server.PgConn) {
				var count int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM provider_egress_destination WHERE active`).Scan(&count))
				if count != 5 {
					t.Fatal("refresh adapter lost or duplicated configured sites", count)
				}
			})
		}
	})
}

// Retention runs even with all external analytics providers disabled. A
// malformed configuration returns an error, preserving the queued retry.
func TestWebSearchAnalyticsTaskRetentionAndConfigurationFailure(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		t.Cleanup(server.Config.PushSimpleResource("analytics.yml", []byte("enabled: true\nsearch:\n  enabled: true\n")))
		t.Cleanup(server.Vault.PushSimpleResource("analytics.yml", []byte("{}\n")))
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO web_search_analytics(row_key,provider,site,period_start,period_end,search_type,query,path,region,device,clicks,impressions,average_position,update_time)
				VALUES(repeat('a',64),'synthetic','synthetic.example','2000-01-01','2000-01-02','web','synthetic old query','/','','',1,1000000,1,'2000-01-02'),
				(repeat('b',64),'synthetic','synthetic.example','2099-01-01','2099-01-02','web','synthetic retained query','/','','',1,1000000,1,'2099-01-02')`))
		})
		for range 2 {
			runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(WebSearchAnalytics, WebSearchAnalyticsPost, &WebSearchAnalyticsArgs{}), true)
			server.Db(ctx, func(conn server.PgConn) {
				var correct bool
				server.Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(row_key=repeat('b',64)) FROM web_search_analytics`).Scan(&correct))
				if !correct {
					t.Fatal("analytics task lost retained source or kept expired rows")
				}
			})
		}
		pop := server.Config.PushSimpleResource("analytics.yml", []byte("enabled: [\n"))
		_, err := WebSearchAnalytics(&WebSearchAnalyticsArgs{}, owner)
		pop()
		if err == nil {
			t.Fatal("invalid analytics configuration acknowledged")
		}
	})
}
