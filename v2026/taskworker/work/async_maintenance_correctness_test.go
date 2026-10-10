// Exercise maintenance bodies through real queue admission and completion.
// A successful cleanup must preserve live data and rearm exactly one successor.
package work

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Typed production functions still pass through the real serialization adapter.
type asyncMaintenanceCase struct {
	target task.Target
	queue  func(*session.ClientSession) server.Id
}

// Keep fixture scheduling separate from each production Post's recurrence.
func newAsyncMaintenanceCase[A, R any](body func(A, *session.ClientSession) (R, error), post func(A, R, *session.ClientSession, server.PgTx) error, args A) asyncMaintenanceCase {
	target := task.NewTaskTargetWithPost(body, post)
	return asyncMaintenanceCase{target: target, queue: func(owner *session.ClientSession) server.Id {
		return task.ScheduleTask(body, args, owner, task.RunOnce("synthetic_maintenance_audit", target.TargetFunctionName()),
			task.RunAt(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
	}}
}

// Retained compatibility targets are deliberate one-shot no-ops. They must
// complete old rows, including their Post, without reviving a retired chain.
func TestRetiredCompatibilityTasksCompleteWithoutSuccessors(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		for _, sample := range []asyncMaintenanceCase{
			newAsyncMaintenanceCase(RemoveLocationLookupResults, RemoveLocationLookupResultsPost, &RemoveLocationLookupResultsArgs{}),
			newAsyncMaintenanceCase(SetMissingConnectionLocations, SetMissingConnectionLocationsPost, &SetMissingConnectionLocationsArgs{}),
			newAsyncMaintenanceCase(UpdateClientReliabilityScores, UpdateClientReliabilityScoresPost, &UpdateClientReliabilityScoresArgs{}),
			newAsyncMaintenanceCase(UpdateNetworkReliabilityWindow, UpdateNetworkReliabilityWindowPost, &UpdateNetworkReliabilityWindowArgs{}),
			newAsyncMaintenanceCase(WarmNetworkGetProviderLocations, WarmNetworkGetProviderLocationsPost, &WarmNetworkGetProviderLocationsArgs{}),
			newAsyncMaintenanceCase(RefreshGeolocationSourcePins, RefreshGeolocationSourcePinsPost, &RefreshGeolocationSourcePinsArgs{}),
		} {
			worker := startupClosureWorker(ctx, sample.target)
			id := sample.queue(owner)
			finished, retried, posts, err := worker.EvalTasks(1)
			worker.Close()
			if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
				t.Fatal("compatibility task failed to drain", sample.target.TargetFunctionName(), finished, retried, posts, err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var completed bool
				var pending int
				server.Raise(conn.QueryRow(ctx, `SELECT post_completed FROM finished_task WHERE task_id=$1`, id).Scan(&completed))
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task`).Scan(&pending))
				if !completed || pending != 0 {
					t.Fatal("retired task revived work or skipped its Post", sample.target.TargetFunctionName(), pending)
				}
			})
		}
	})
}

// Old and retained rows coexist in every fixture. Calling the real task twice
// must remove only eligible data and keep one future normal successor.
func TestMaintenanceTasksCommitSelectiveCleanupAndRecurrence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		for _, sample := range []struct {
			asyncMaintenanceCase
			seed   string
			verify string
		}{
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveOldProvideKeyChanges, RemoveOldProvideKeyChangesPost, &RemoveOldProvideKeyChangesArgs{}),
				seed:   `INSERT INTO provide_key_change(client_id,change_time) VALUES($1,'2000-01-01'),($2,'2099-01-01')`,
				verify: `SELECT count(*)=1 AND bool_and(client_id=$2) FROM provide_key_change WHERE client_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveOldSearchProviderStats, RemoveOldSearchProviderStatsPost, &RemoveOldSearchProviderStatsArgs{}),
				seed:   `INSERT INTO search_provider_stats(period_start,period_end,client_id,match_count) VALUES('2000-01-01','2000-01-02',$1,1),('2099-01-01','2099-01-02',$2,1)`,
				verify: `SELECT count(*)=1 AND bool_and(client_id=$2) FROM search_provider_stats WHERE client_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveExpiredBulkClientRemovalQuota, RemoveExpiredBulkClientRemovalQuotaPost, &RemoveExpiredBulkClientRemovalQuotaArgs{}),
				seed: `INSERT INTO bulk_client_removal_quota(bulk_client_removal_quota_id,network_id,client_count,bucket_start)
					VALUES($1,$1,5,'2000-01-01'),($2,$2,7,'2099-01-01')`,
				verify: `SELECT count(*)=1 AND sum(client_count)=7 FROM bulk_client_removal_quota WHERE network_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(CleanupExpiredPaymentIntents, CleanupExpiredPaymentIntentsPost, &CleanupExpiredPaymentIntentsArgs{}),
				seed: `INSERT INTO solana_payment_intent(payment_reference,network_id,expires_at,tx_signature)
					VALUES('synthetic-old',$1,'2000-01-01',NULL),('synthetic-paid',$1,'2000-01-01','synthetic-signature'),('synthetic-live',$2,'2099-01-01',NULL)`,
				verify: `SELECT count(*)=2 AND bool_and(payment_reference IN ('synthetic-paid','synthetic-live'))
					FROM solana_payment_intent WHERE network_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveExpiredAuthCodes, RemoveExpiredAuthCodesPost, &RemoveExpiredAuthCodesArgs{}),
				seed: `WITH codes AS (
					INSERT INTO auth_code(auth_code_id,network_id,user_id,auth_code,create_time,end_time,uses,remaining_uses)
					VALUES($1,$1,$1,'synthetic-old','2000-01-01','2000-01-02',1,1),
					($2,$2,$2,'synthetic-live','2099-01-01','2099-01-02',1,1) RETURNING auth_code_id)
					INSERT INTO auth_code_role(auth_code_id,role) SELECT auth_code_id,'synthetic-role' FROM codes`,
				verify: `SELECT (SELECT count(*)=1 AND bool_and(auth_code_id=$2) FROM auth_code WHERE auth_code_id IN ($1,$2))
					AND (SELECT count(*)=1 AND bool_and(auth_code_id=$2) FROM auth_code_role WHERE auth_code_id IN ($1,$2))`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveExpiredAuthCodes, RemoveExpiredAuthCodesPost, &RemoveExpiredAuthCodesArgs{}),
				seed: `INSERT INTO user_auth_verify(user_auth_verify_id,user_id,verify_time,verify_code)
					VALUES($1,$1,'2000-01-01','old'),($2,$2,'2099-01-01','live')`,
				verify: `SELECT count(*)=1 AND bool_and(user_auth_verify_id=$2) FROM user_auth_verify WHERE user_auth_verify_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveExpiredAuthAttempts, RemoveExpiredAuthAttemptsPost, &RemoveExpiredAuthAttemptsArgs{}),
				seed: `INSERT INTO user_auth_attempt(user_auth_attempt_id,success,attempt_time)
					VALUES($1,false,'2000-01-01'),($2,false,'2099-01-01')`,
				verify: `SELECT count(*)=1 AND bool_and(user_auth_attempt_id=$2) FROM user_auth_attempt WHERE user_auth_attempt_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(CloseExpiredNetworkClientHandlers, CloseExpiredNetworkClientHandlersPost, &CloseExpiredNetworkClientHandlersArgs{}),
				seed:   `INSERT INTO network_client_handler(handler_id,heartbeat_time) VALUES($1,'2000-01-01'),($2,'2099-01-01')`,
				verify: `SELECT count(*)=1 AND bool_and(handler_id=$2) FROM network_client_handler WHERE handler_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveOldClientLocationReliabilities, RemoveOldClientLocationReliabilitiesPost, &RemoveOldClientLocationReliabilitiesArgs{}),
				seed: `INSERT INTO network_client_location_reliability(client_id,network_id,update_block_number)
					VALUES($1,$1,0),($2,$2,900000000000)`,
				verify: `SELECT count(*)=1 AND bool_and(client_id=$2) FROM network_client_location_reliability WHERE client_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveOldNetworkReliabilityWindow, RemoveOldNetworkReliabilityWindowPost, &RemoveOldNetworkReliabilityWindowArgs{}),
				seed:   `INSERT INTO network_connection_reliability_window(network_id,bucket_number) VALUES($1,0),($2,900000000000)`,
				verify: `SELECT count(*)=1 AND bool_and(network_id=$2) FROM network_connection_reliability_window WHERE network_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveOldClientReliabilityStats, RemoveOldClientReliabilityStatsPost, &RemoveOldClientReliabilityStatsArgs{}),
				seed: `INSERT INTO client_reliability(block_number,client_address_hash,network_id,client_id)
					VALUES(0,'synthetic-old',$1,$1),(900000000000,'synthetic-live',$2,$2)`,
				verify: `SELECT count(*)=1 AND bool_and(client_id=$2) FROM client_reliability WHERE client_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveOldAuditNetworkEvents, RemoveOldAuditNetworkEventsPost, &RemoveOldAuditNetworkEventsArgs{}),
				seed: `INSERT INTO audit_network_event(event_id,event_time,network_id,event_type)
					VALUES($1,'2000-01-01',$1,'network_created'),($2,'2099-01-01',$2,'network_created')`,
				verify: `SELECT count(*)=1 AND bool_and(event_id=$2) FROM audit_network_event WHERE event_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveExpiredWalletNonces, RemoveExpiredWalletNoncesPost, &RemoveExpiredWalletNoncesArgs{}),
				seed:   `INSERT INTO auth_wallet_nonce(nonce,expire_time) VALUES($1::text,'2000-01-01'),($2::text,'2099-01-01')`,
				verify: `SELECT count(*)=1 AND bool_and(nonce=$2::text) FROM auth_wallet_nonce WHERE nonce IN ($1::text,$2::text)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveExpiredProviderEgressLocations, RemoveExpiredProviderEgressLocationsPost, &RemoveExpiredProviderEgressLocationsArgs{}),
				seed: `WITH locations AS (INSERT INTO provider_egress_location(client_id,location_id,country_code,observed_at,update_time)
					VALUES($1,$1,'US','2000-01-01','2000-01-01'),($2,$2,'US','2099-01-01','2099-01-01'))
					INSERT INTO provider_egress_probe_attempt(client_id,attempt_at,update_time)
					VALUES($1,'2000-01-01','2000-01-01'),($2,'2099-01-01','2099-01-01')`,
				verify: `SELECT (SELECT count(*)=1 AND bool_and(client_id=$2) FROM provider_egress_location WHERE client_id IN ($1,$2))
					AND (SELECT count(*)=1 AND bool_and(client_id=$2) FROM provider_egress_probe_attempt WHERE client_id IN ($1,$2))`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveOldExtenderLatencies, RemoveOldExtenderLatenciesPost, &RemoveOldExtenderLatenciesArgs{}),
				seed: `INSERT INTO network_extender_latency(latency_id,extender_id,client_id,probe_nonce,rtt_ms,probe_time,create_time)
					VALUES($1,$1,$1,'old',1,'2000-01-01','2000-01-01'),($2,$2,$2,'live',1,'2099-01-01','2099-01-01')`,
				verify: `SELECT count(*)=1 AND bool_and(latency_id=$2) FROM network_extender_latency WHERE latency_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(CancelHungAccountPayments, CancelHungAccountPaymentsPost, &CancelHungAccountPaymentsArgs{}),
				seed: `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,payout_byte_count,payout_nano_cents,min_sweep_time,create_time,payment_record)
					VALUES($1,$1,$1,1,1,'2000-01-01','2000-01-01',NULL),($2,$2,$2,1,1,'2000-01-01','2000-01-01','submitted')`,
				verify: `SELECT count(*)=2 AND bool_and(canceled=(payment_id=$1)) FROM account_payment WHERE payment_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveCompletedContracts, RemoveCompletedContractsPost, &RemoveCompletedContractsArgs{}),
				seed: `INSERT INTO transfer_contract(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,outcome,create_time,close_time,reap_time)
					VALUES($1,$1,$1,$1,$1,1,'canceled','2000-01-01','2000-01-01','2000-01-01'),($2,$2,$2,$2,$2,1,'canceled','2099-01-01','2099-01-01','2099-01-01')`,
				verify: `SELECT count(*)=1 AND bool_and(contract_id=$2) FROM transfer_contract WHERE contract_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveDisconnectedNetworkClients, RemoveDisconnectedNetworkClientsPost, &RemoveDisconnectedNetworkClientsArgs{}),
				seed: `INSERT INTO network_client(client_id,network_id,active,create_time,auth_time,deactivate_time)
					VALUES($1,$1,false,'2000-01-01','2000-01-01','2000-01-01'),($2,$2,true,'2099-01-01','2099-01-01',NULL)`,
				verify: `SELECT count(*)=1 AND bool_and(client_id=$2) FROM network_client WHERE client_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(SweepOrphanNetworkClientData, SweepOrphanNetworkClientDataPost, &SweepOrphanNetworkClientDataArgs{}),
				seed: `WITH parent AS (INSERT INTO network_client_connection(connection_id,client_id,connect_time,connection_host,connection_service,connection_block)
					VALUES($2,$2,'2099-01-01','synthetic','synthetic','synthetic'))
					INSERT INTO network_client_speed(connection_id,bytes_per_second) VALUES($1,1),($2,1)`,
				verify: `SELECT count(*)=1 AND bool_and(connection_id=$2) FROM network_client_speed WHERE connection_id IN ($1,$2)`},
			{asyncMaintenanceCase: newAsyncMaintenanceCase(RemoveOldAuditEvents, RemoveOldAuditEventsPost, &RemoveOldAuditEventsArgs{}),
				seed: `WITH providers AS (INSERT INTO audit_provider_event(event_id,event_time,network_id,device_id,event_type,country_name,region_name,city_name)
					VALUES($1,'2000-01-01',$1,$1,'provider_online','','',''),($2,'2099-01-01',$2,$2,'provider_online','','','')),
					extenders AS (INSERT INTO audit_extender_event(event_id,event_time,network_id,extender_id,event_type)
					VALUES($1,'2000-01-01',$1,$1,'extender_online'),($2,'2099-01-01',$2,$2,'extender_online')),
					devices AS (INSERT INTO audit_device_event(event_id,event_time,network_id,device_id,event_type)
					VALUES($1,'2000-01-01',$1,$1,'device_added'),($2,'2099-01-01',$2,$2,'device_added'))
					INSERT INTO audit_contract_event(event_id,event_time,contract_id,client_network_id,client_device_id,provider_network_id,provider_device_id,event_type)
					VALUES($1,'2000-01-01',$1,$1,$1,$1,$1,'contract_closed_success'),($2,'2099-01-01',$2,$2,$2,$2,$2,'contract_closed_success')`,
				verify: `SELECT count(*)=4 AND bool_and(event_id=$2) FROM (
					SELECT event_id FROM audit_provider_event WHERE event_id IN ($1,$2) UNION ALL
					SELECT event_id FROM audit_extender_event WHERE event_id IN ($1,$2) UNION ALL
					SELECT event_id FROM audit_device_event WHERE event_id IN ($1,$2) UNION ALL
					SELECT event_id FROM audit_contract_event WHERE event_id IN ($1,$2)) retained`},
		} {
			oldId, retainedId := server.NewId(), server.NewId()
			server.Tx(ctx, func(tx server.PgTx) { server.RaisePgResult(tx.Exec(ctx, sample.seed, oldId, retainedId)) })
			worker := startupClosureWorker(ctx, sample.target)
			for range 2 {
				id := sample.queue(owner)
				finished, retried, posts, err := worker.EvalTasks(1)
				if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
					t.Fatal("maintenance body or Post failed", sample.target.TargetFunctionName(), finished, retried, posts, err)
				}
				server.Db(ctx, func(conn server.PgConn) {
					var correct, future bool
					var pending int
					server.Raise(conn.QueryRow(ctx, sample.verify, oldId, retainedId).Scan(&correct))
					server.Raise(conn.QueryRow(ctx, `SELECT count(*),COALESCE(bool_and(run_at>$2),false)
						FROM pending_task WHERE function_name=$1`, sample.target.TargetFunctionName(), server.NowUtc()).Scan(&pending, &future))
					if !correct || pending != 1 || !future {
						t.Fatal("successful cleanup lost live data or recurrence", sample.target.TargetFunctionName(), correct, pending, future)
					}
				})
			}
			worker.Close()
		}
	})
}
