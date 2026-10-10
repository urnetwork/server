// Verify task adapters against durable effects and their published projections.
package work

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Each invocation uses actual serialization, claim, handback and Post. Replays
// must keep one future successor and may not add the same source work twice.
func runProjectionAuditTask(t testing.TB, owner *session.ClientSession, sample asyncMaintenanceCase, recurring bool) server.Id {
	ctx := owner.Ctx
	worker := startupClosureWorker(ctx, sample.target)
	defer worker.Close()
	id := sample.queue(owner)
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
		t.Fatal(sample.target.TargetFunctionName(), "did not finish", finished, retried, posts, err)
	}
	server.Db(ctx, func(conn server.PgConn) {
		var completed bool
		var pending int
		server.Raise(conn.QueryRow(ctx, `SELECT post_completed FROM finished_task WHERE task_id=$1`, id).Scan(&completed))
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name=$1 AND run_at>$2`,
			sample.target.TargetFunctionName(), server.NowUtc()).Scan(&pending))
		expected := 0
		if recurring {
			expected = 1
		}
		if !completed || pending != expected {
			t.Fatal(sample.target.TargetFunctionName(), "lost completion or recurrence", completed, pending)
		}
	})
	return id
}

// The task must replace a stale published value with the retained audit feed,
// not merely acknowledge the export or successfully schedule another run.
func TestExportStatsTaskPublishesCurrentAuditSnapshot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-export-inventory", server.NewId())
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_network_event(event_id,event_time,network_id,event_type)
				VALUES($1,$2,$3,'network_created')`, server.NewId(), server.NowUtc().Add(-48*time.Hour), networkId))
		})
		model.ExportStats(ctx, &model.Stats{Lookback: 90, NetworksSummary: 999})
		for range 2 {
			runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(ExportStats, ExportStatsPost, &ExportStatsArgs{}), true)
			stats := model.GetExportedStats(ctx, 90)
			if stats == nil || stats.Lookback != 90 || stats.NetworksSummary != 1 || stats.CreatedTime <= 0 {
				t.Fatal("export task did not publish the current audit snapshot")
			}
		}
	})
}

// The source has real public supply, a connected device, and closed metering
// blocks. Validate exported bytes, audit transitions and additive-rollup replay.
func TestAsyncProjectionTasksPublishSourcesOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		networkId, clientId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic-provider", "")
		model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{model.ProvideModePublic: make([]byte, 32)})
		handler := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "192.0.2.1:1", handler)
		server.Raise(err)
		region := &model.Location{LocationType: model.LocationTypeRegion, Region: "California", Country: "United States", CountryCode: "us"}
		model.CreateLocation(ctx, region)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability(client_id,network_id,update_block_number,
				region_location_id,country_location_id,client_address_hash_count,location_count,connected)
				VALUES($1,$2,1,$3,$4,1,1,true)`, clientId, networkId, region.LocationId, region.CountryLocationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_connection_reliability_score(client_id,lookback_index,
				independent_reliability_score,independent_reliability_weight,reliability_score,reliability_weight,
				min_block_number,max_block_number,region_location_id,country_location_id)
				VALUES($1,0,1,1,1,1,1,1,$2,$3)`, clientId, region.LocationId, region.CountryLocationId))
		})
		at := server.NowUtc().Add(-5 * model.ClientDataUsageBlockDuration)
		model.RecordClientDataUsage(ctx, clientId, 17, at)
		// Start just before this fixture's closed block; older empty retention
		// scans are already covered by the model rollup tests.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_data_usage_rollup(singleton_id,max_drained_block,update_time)
				VALUES(1,$1,$2)`, at.UnixNano()/int64(model.ClientDataUsageBlockDuration)-1, server.NowUtc()))
		})
		for range 2 {
			runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(ExportProvidersMap, ExportProvidersMapPost, &ExportProvidersMapArgs{}), true)
			wire := model.GetExportedProvidersMapJson(ctx)
			var regions map[string]map[string]*model.RegionProviders
			if wire == nil {
				t.Fatal("providers map task did not publish")
			}
			server.Raise(json.Unmarshal([]byte(*wire), &regions))
			if regions["us"]["California"] == nil || regions["us"]["California"].ProviderCount != 1 {
				t.Fatal("providers map lost or duplicated source", *wire)
			}
			runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(SweepProviderAuditEvents, SweepProviderAuditEventsPost, &SweepProviderAuditEventsArgs{}), true)
			runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(RollupClientDataUsage, RollupClientDataUsagePost, &RollupClientDataUsageArgs{}), true)
			server.Db(ctx, func(conn server.PgConn) {
				var providers, devices int
				var usage int64
				server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_provider_event WHERE device_id=$1),
					(SELECT count(*) FROM audit_device_event WHERE device_id=$1),
					(SELECT COALESCE(sum(used_byte_count),0) FROM network_client_data_usage WHERE client_id=$1)`, clientId).Scan(&providers, &devices, &usage))
				if providers != 1 || devices != 1 || usage != 17 {
					t.Fatal("task duplicated or lost source effects", providers, devices, usage)
				}
			})
		}
		server.Raise(model.DisconnectNetworkClient(ctx, connectionId))
		runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(SweepProviderAuditEvents, SweepProviderAuditEventsPost, &SweepProviderAuditEventsArgs{}), true)
		server.Db(ctx, func(conn server.PgConn) {
			var providers, devices int
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_provider_event WHERE device_id=$1),
				(SELECT count(*) FROM audit_device_event WHERE device_id=$1)`, clientId).Scan(&providers, &devices))
			if providers != 2 || devices != 2 {
				t.Fatal("disconnect transitions missing", providers, devices)
			}
		})
	})
}

// Exact source reconciliation initializes the clock once and repairs escrow
// drift without replaying a charge. These tasks publish before completing.
func TestAsyncClockAndEscrowReconciliation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		id, networkId := server.NewId(), server.NewId()
		now := server.NowUtc()
		balance := &model.TransferBalance{NetworkId: networkId,
			StartTime: now.Add(-time.Hour), EndTime: now.Add(time.Hour), StartBalanceByteCount: 1000, BalanceByteCount: 1000}
		model.AddTransferBalance(ctx, balance)
		balanceId := balance.BalanceId
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count)
				VALUES($1,$2,$2,$3,$3,100)`, id, networkId, server.NewId()))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,100)`, id, balanceId))
		})
		clock := asyncMaintenanceCase{target: task.NewTaskTarget(BackfillClock), queue: func(owner *session.ClientSession) server.Id {
			return task.ScheduleTask(BackfillClock, &BackfillClockArgs{}, owner, task.RunAt(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
		}}
		for range 2 {
			runProjectionAuditTask(t, owner, clock, false)
			value, ok := model.GetClock(ctx)
			if !ok || value.TotalTransferByteCount != "0" {
				t.Fatal("clock backfill failed on unclosed usage", value, ok)
			}
			runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(ReconcileNetEscrow, ReconcileNetEscrowPost, &ReconcileNetEscrowArgs{}), true)
			if count := model.Testing_NetEscrowByteCount(ctx, balanceId); count != 100 {
				t.Fatal("escrow reconciliation lost or doubled reservation", count)
			}
		}
	})
}
