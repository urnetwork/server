// Real maintenance entrypoints must release completed cohorts before later
// row waits, retain atomic callers, and reconcile the full selected populations.
package model

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// Bulk synthetic durable inputs avoid making fixture setup a connection churn
// workload. The subject remains the production maintenance entrypoint.
type clientLocationRefreshFixture struct {
	ctx        context.Context
	clientIds  []server.Id
	networkId  server.Id
	handlerId  server.Id
	locationId server.Id
	now        time.Time
	oldBlock   int64
}

// Every client begins with one live classified location and an old snapshot.
func newClientLocationRefreshFixture(t testing.TB, count int) *clientLocationRefreshFixture {
	t.Helper()
	ctx := context.Background()
	f := &clientLocationRefreshFixture{
		ctx: ctx, networkId: server.NewId(), handlerId: CreateNetworkClientHandler(ctx),
		locationId: server.NewId(), now: server.NowUtc().Truncate(time.Millisecond),
	}
	// The block stamp advances only after the refresh interval, even when values change.
	f.oldBlock = f.now.Add(-ClientLocationReliabilityRefreshInterval - ReliabilityBlockDuration).UnixMilli() / ReliabilityBlockDuration.Milliseconds()
	for range count {
		f.clientIds = append(f.clientIds, server.NewId())
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id)
			SELECT client_id,$2 FROM unnest($1::uuid[]) client_id`, f.clientIds, f.networkId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_connection
			(client_id,connection_id,connect_time,connection_host,connection_service,connection_block,
			 client_address_hash,handler_id,ip_version,ip_family_intent)
			SELECT client_id,client_id,$2,'fixture','connect','fixture',decode(repeat('01',32),'hex'),$3,4,4
			FROM unnest($1::uuid[]) client_id`, f.clientIds, f.now.Add(-time.Minute), f.handlerId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location
			(connection_id,client_id,network_id,city_location_id,region_location_id,country_location_id,
			 arin_quality_verified)
			SELECT client_id,client_id,$2,$3,$3,$3,true FROM unnest($1::uuid[]) client_id`, f.clientIds, f.networkId, f.locationId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability
			(client_id,network_id,update_block_number,city_location_id,region_location_id,country_location_id,
			 client_address_hash_count,location_count,connected)
			SELECT client_id,$2,$3,$4,$4,$4,1,1,true FROM unnest($1::uuid[]) client_id`, f.clientIds, f.networkId, f.oldBlock, f.locationId))
	}, server.TxReadCommitted)
	return f
}

// Observe the actual row-lock edge, not elapsed time as a stand-in for waiting.
func requireClientLocationRefreshWaiter(t testing.TB, ctx context.Context, holder server.PgTx) int {
	t.Helper()
	for {
		var pid int
		server.RaisePgResult(holder.Exec(ctx, `SELECT pg_stat_clear_snapshot()`))
		server.Raise(holder.QueryRow(ctx, `SELECT COALESCE(min(pid),0) FROM pg_stat_activity
			WHERE pg_backend_pid()=ANY(pg_blocking_pids(pid))`).Scan(&pid))
		if pid != 0 {
			return pid
		}
		select {
		case <-ctx.Done():
			t.Fatal("location page never reached held row", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A later page's blocked upsert cannot retain the already published prefix.
// The old fleet transaction reaches this same barrier with zero committed rows.
func TestClientLocationRefreshCommitsBeforeLaterPageWait(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newClientLocationRefreshFixture(t, clientLocationRefreshPageSize+1)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO provider_egress_probe_cycle
				(client_id,cycle_started_at,next_attempt_at,eligible)
				SELECT client_id,$2,$2,true FROM unnest($1::uuid[]) client_id`, f.clientIds, f.now))
		}, server.TxReadCommitted)
		ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
		defer cancel()
		conn := acquireContractLifecycleTestConnection(t, ctx)
		connectionReleased := false
		defer func() {
			if !connectionReleased {
				conn.Release()
			}
		}()
		holder, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer rollbackCloseReportTestTransaction(ctx, holder)
		server.RaisePgResult(holder.Exec(ctx, `UPDATE network_client_location_reliability
			SET max_net_type_score=max_net_type_score WHERE client_id=$1`, f.clientIds[len(f.clientIds)-1]))
		workerCtx, stopWorker := context.WithCancel(ctx)
		joined := make(chan error, 1)
		workerJoined := false
		go func() {
			var workerErr error
			server.HandleError(func() { UpdateClientLocationReliabilities(workerCtx, f.now.Add(-time.Hour), f.now) },
				func(err error) { workerErr = err })
			joined <- workerErr
		}()
		defer func() {
			stopWorker()
			rollbackCloseReportTestTransaction(ctx, holder)
			if !workerJoined {
				select {
				case <-joined:
				case <-time.After(35 * time.Second):
					t.Error("location refresh did not join")
				}
			}
		}()
		waiterPid := requireClientLocationRefreshWaiter(t, ctx, holder)
		var committed int
		server.Raise(holder.QueryRow(ctx, `SELECT count(*) FROM network_client_location_reliability
			WHERE network_id=$1 AND update_block_number=$2`, f.networkId, f.now.UnixMilli()/ReliabilityBlockDuration.Milliseconds()).Scan(&committed))
		if committed != clientLocationRefreshPageSize {
			t.Fatalf("held successor retained earlier publication: committed=%d want=%d", committed, clientLocationRefreshPageSize)
		}
		var untouchedSuccessor bool
		server.Raise(holder.QueryRow(ctx, `SELECT eligible FROM provider_egress_probe_cycle WHERE client_id=$1`, f.clientIds[len(f.clientIds)-1]).Scan(&untouchedSuccessor))
		if !untouchedSuccessor {
			t.Fatal("earlier cohort changed a later cohort's scheduling hint")
		}
		stopWorker()
		select {
		case err := <-joined:
			workerJoined = true
			if err == nil || workerCtx.Err() == nil {
				t.Fatal("canceled page returned success", err)
			}
		case <-ctx.Done():
			t.Fatal("canceled page did not return", ctx.Err())
		}
		var open bool
		server.RaisePgResult(holder.Exec(ctx, `SELECT pg_stat_clear_snapshot()`))
		server.Raise(holder.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE pid=$1 AND xact_start IS NOT NULL)`, waiterPid).Scan(&open))
		if open {
			t.Fatal("canceled page retained its database transaction")
		}
		server.Raise(holder.Rollback(ctx))
		conn.Release()
		connectionReleased = true
		UpdateClientLocationReliabilities(ctx, f.now.Add(-time.Hour), f.now)
		server.Db(ctx, func(read server.PgConn) {
			server.Raise(read.QueryRow(ctx, `SELECT count(*) FROM network_client_location_reliability
				WHERE network_id=$1 AND update_block_number=$2`, f.networkId, f.now.UnixMilli()/ReliabilityBlockDuration.Milliseconds()).Scan(&committed))
		})
		if committed != len(f.clientIds) {
			t.Fatal("fresh pass did not complete after the blocked page was released", committed)
		}
		if count := testutil.ToFloat64(clientLocationReliabilityPassClients.WithLabelValues("top_level")); count != float64(len(f.clientIds)) {
			t.Fatal("completed pass gauge contains only its last cohort", count)
		}
	})
}

// Compare the complete publication row, including generated validity and every
// measurement/security field. Cycle clocks are preexisting and must not change.
func clientLocationRefreshRows(ctx context.Context, queryer server.PgCanQuery) map[string]string {
	rows := map[string]string{}
	result, err := queryer.Query(ctx, `SELECT 'location:'||client_id::text, to_jsonb(location)::text
		FROM network_client_location_reliability location
		UNION ALL SELECT 'cycle:'||client_id::text, to_jsonb(cycle)::text FROM provider_egress_probe_cycle cycle`)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var key, value string
			server.Raise(result.Scan(&key, &value))
			rows[key] = value
		}
	})
	return rows
}

// A rolled-back invocation of the explicit atomic API supplies the independent
// publication oracle for dual-stack, orphan, offline and eligibility cohorts.
func TestClientLocationRefreshPagesMatchAtomicPublication(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newClientLocationRefreshFixture(t, clientLocationRefreshPageSize+7)
		ctx := f.ctx
		missingId, offlineId, orphanId := f.clientIds[2], f.clientIds[3], f.clientIds[4]
		staleCycleId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2
				WHERE client_id=ANY($1)`, []server.Id{missingId, offlineId}, f.now.Add(-time.Second)))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_location_reliability WHERE client_id=$1`, missingId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET connected=false WHERE client_id=$1`, offlineId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET handler_id=$2 WHERE client_id=$1`, orphanId, server.NewId()))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provide_key(client_id,provide_mode,secret_key)
				SELECT client_id,$2,decode('01','hex') FROM unnest($1::uuid[]) client_id`, f.clientIds, ProvideModePublic))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle(client_id,cycle_started_at,next_attempt_at,eligible,success_count)
				SELECT client_id,$2,$3,false,7 FROM unnest($1::uuid[]) client_id`, f.clientIds, f.now.Add(-time.Hour), f.now.Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle(client_id,cycle_started_at,next_attempt_at,eligible)
				VALUES ($1,$2,$2,true)`, staleCycleId, f.now))
			connectionId := server.NewId()
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_connection
				(client_id,connection_id,connect_time,connection_host,connection_service,connection_block,client_address_hash,handler_id,ip_version,ip_family_intent)
				VALUES ($1,$2,$3,'fixture','connect','fixture',decode(repeat('02',32),'hex'),$4,6,6)`, f.clientIds[0], connectionId, f.now, f.handlerId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location
				(connection_id,client_id,network_id,city_location_id,region_location_id,country_location_id,arin_quality_verified)
				VALUES ($1,$2,$3,$4,$4,$4,true)`, connectionId, f.clientIds[0], f.networkId, server.NewId()))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
				SELECT client_id,client_id,$2,1,1,'{}',false,true,$3 FROM unnest($1::uuid[]) client_id`, f.clientIds, f.now.Add(-time.Minute), SelectedProviderUrlProbePolicyVersion()))
		}, server.TxReadCommitted)
		rollback := errors.New("atomic location oracle rollback")
		var expected map[string]string
		func() {
			defer func() {
				if value := recover(); value != rollback {
					panic(value)
				}
			}()
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				UpdateClientLocationReliabilitiesInTx(tx, ctx, f.now.Add(-time.Hour), f.now, nil)
				expected = clientLocationRefreshRows(ctx, tx)
				panic(rollback)
			}, server.TxReadCommitted)
		}()
		UpdateClientLocationReliabilities(ctx, f.now.Add(-time.Hour), f.now)
		var actual map[string]string
		server.Db(ctx, func(conn server.PgConn) { actual = clientLocationRefreshRows(ctx, conn) })
		if !maps.Equal(expected, actual) {
			for key, want := range expected {
				if actual[key] != want {
					t.Errorf("page differs from atomic publication for fixture %s: %s != %s", key, actual[key], want)
				}
			}
			if len(expected) != len(actual) {
				t.Errorf("publication cardinality changed: %d != %d", len(actual), len(expected))
			}
		}
		row, found := readClientLocationReliabilityForTest(ctx, f.clientIds[0])
		if !found || !row.valid || !row.ipv4Proven || !row.ipv6Proven {
			t.Fatal("page split a client's whole dual-stack aggregation", row)
		}
		if testingClientLocationConnected(ctx, t, missingId) || testingClientLocationConnected(ctx, t, orphanId) {
			t.Fatal("missing fallback or orphan was advertised connected")
		}
	})
}

// Empty qualifying pages and equal timestamps still advance raw discovery;
// the inclusive first-page bounds cover the entire stored UUID domain.
func TestClientLocationRefreshDisconnectedRawProgressAndZeroBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newClientLocationRefreshFixture(t, 2*clientLocationRefreshPageSize+3)
		ctx := f.ctx
		minTime := f.now.Add(-time.Hour)
		zero := server.Id{}
		lastId := f.clientIds[len(f.clientIds)-1]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2
				WHERE client_id=ANY($1)`, f.clientIds, minTime))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET connected=false WHERE network_id=$1`, f.networkId))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_location_reliability WHERE client_id=$1`, lastId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id) VALUES ($1,$2)`, zero, f.networkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_connection
				(client_id,connection_id,connected,connect_time,disconnect_time,connection_host,connection_service,connection_block,client_address_hash,handler_id)
				VALUES ($1,$1,false,$2,$3,'fixture','connect','fixture',decode(repeat('01',32),'hex'),$4)`, zero, minTime.Add(-time.Minute), minTime, f.handlerId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location
				(connection_id,client_id,city_location_id,region_location_id,country_location_id)
				VALUES ($1,$1,$2,$2,$2)`, zero, f.locationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle(client_id,cycle_started_at,next_attempt_at,eligible)
				VALUES ($1,$2,$2,true)`, zero, minTime))
		}, server.TxReadCommitted)
		started := time.Now()
		UpdateClientLocationReliabilities(ctx, minTime, f.now)
		if testingClientLocationConnected(ctx, t, zero) || testingClientLocationConnected(ctx, t, lastId) {
			t.Fatal("disconnected discovery marked a boundary row connected")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var oldCount int
			var eligible bool
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client_location_reliability
				WHERE network_id=$1 AND update_block_number=$2`, f.networkId, f.oldBlock).Scan(&oldCount))
			if oldCount != len(f.clientIds)-1 {
				t.Fatal("existing offline rows had their expiration extended", oldCount)
			}
			server.Raise(conn.QueryRow(ctx, `SELECT eligible FROM provider_egress_probe_cycle WHERE client_id=$1`, zero).Scan(&eligible))
			if eligible {
				t.Fatal("zero-ID stale cycle escaped final reconciliation")
			}
		})
		t.Logf("equal-time raw fixture rows=%d completed=%s; this is fixture timing, not a Main work bound", len(f.clientIds)+1, time.Since(started))
	})
}

// The candidate merge's first page includes a stored zero client ID, just as
// the explicit atomic helper does; cursor sentinels cannot invent exclusions.
func TestClientLocationRefreshConnectedZeroBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newClientLocationRefreshFixture(t, 1)
		zero := server.Id{}
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO network_client(client_id,network_id) VALUES ($1,$2)`, zero, f.networkId))
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO network_client_connection
				(client_id,connection_id,connect_time,connection_host,connection_service,connection_block,client_address_hash,handler_id)
				VALUES ($1,$1,$2,'fixture','connect','fixture',decode(repeat('01',32),'hex'),$3)`, zero, f.now, f.handlerId))
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO network_client_location
				(connection_id,client_id,city_location_id,region_location_id,country_location_id)
				VALUES ($1,$1,$2,$2,$2)`, zero, f.locationId))
		}, server.TxReadCommitted)
		UpdateClientLocationReliabilities(f.ctx, f.now.Add(-time.Hour), f.now)
		if !testingClientLocationConnected(f.ctx, t, zero) {
			t.Fatal("first page omitted the lower-bound client")
		}
	})
}

// The fixed local budget, rather than the task's two-hour budget, ends a stuck
// page. The next transaction must not inherit those local settings.
func TestClientLocationRefreshDeadlineAndSettingsAreLocal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newClientLocationRefreshFixture(t, 1)
		ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
		defer cancel()
		conn := acquireContractLifecycleTestConnection(t, ctx)
		connectionReleased := false
		defer func() {
			if !connectionReleased {
				conn.Release()
			}
		}()
		holder, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer rollbackCloseReportTestTransaction(ctx, holder)
		server.RaisePgResult(holder.Exec(ctx, `UPDATE network_client_location_reliability SET connected=connected WHERE client_id=$1`, f.clientIds[0]))
		joined := make(chan error, 1)
		workerJoined := false
		defer func() {
			cancel()
			rollbackCloseReportTestTransaction(ctx, holder)
			if !workerJoined {
				select {
				case <-joined:
				case <-time.After(35 * time.Second):
					t.Error("deadline page did not join")
				}
			}
		}()
		go func() {
			var workerErr error
			server.HandleError(func() { UpdateClientLocationReliabilities(ctx, f.now.Add(-time.Hour), f.now) }, func(err error) { workerErr = err })
			joined <- workerErr
		}()
		waiterPid := requireClientLocationRefreshWaiter(t, ctx, holder)
		select {
		case err := <-joined:
			workerJoined = true
			if err == nil || ctx.Err() != nil {
				t.Fatal("page did not stop under its own deadline", err, ctx.Err())
			}
		case <-ctx.Done():
			t.Fatal("page inherited the outer task deadline", ctx.Err())
		}
		var open bool
		server.RaisePgResult(holder.Exec(ctx, `SELECT pg_stat_clear_snapshot()`))
		server.Raise(holder.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND xact_start IS NOT NULL)`, waiterPid).Scan(&open))
		if open {
			t.Fatal("timed-out page still owns a transaction")
		}
		server.Raise(holder.Rollback(ctx))
		conn.Release()
		connectionReleased = true
		var before, after string
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `SELECT current_setting('statement_timeout')||'/'||current_setting('idle_in_transaction_session_timeout')||'/'||current_setting('transaction_timeout')`).Scan(&before))
		})
		clientLocationRefreshTx(ctx, func(pageCtx context.Context, tx server.PgTx) {
			var statement, idle, transaction time.Duration
			var statementText, idleText, transactionText string
			server.Raise(tx.QueryRow(pageCtx, `SELECT current_setting('statement_timeout'),current_setting('idle_in_transaction_session_timeout'),current_setting('transaction_timeout')`).Scan(&statementText, &idleText, &transactionText))
			statement, err = time.ParseDuration(statementText)
			server.Raise(err)
			idle, err = time.ParseDuration(idleText)
			server.Raise(err)
			transaction, err = time.ParseDuration(transactionText)
			server.Raise(err)
			if statement != clientLocationRefreshPageTimeout || idle != clientLocationRefreshPageTimeout || transaction != clientLocationRefreshPageTimeout {
				t.Fatal("page server ceilings differ from the finite owner", statementText, idleText, transactionText)
			}
		})
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `SELECT current_setting('statement_timeout')||'/'||current_setting('idle_in_transaction_session_timeout')||'/'||current_setting('transaction_timeout')`).Scan(&after))
		})
		if before != after {
			t.Fatalf("page settings leaked: %s -> %s", before, after)
		}
	})
}

// All of a selected client's connections contribute even when its fanout is
// larger than a page. Page size limits clients, not their validity evidence.
func TestClientLocationRefreshHighFanoutKeepsWholeClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newClientLocationRefreshFixture(t, 1)
		connectionIds := []server.Id{}
		for range 4*clientLocationRefreshPageSize + 1 {
			connectionIds = append(connectionIds, server.NewId())
		}
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO network_client_connection
				(client_id,connection_id,connect_time,connection_host,connection_service,connection_block,client_address_hash,handler_id,ip_version,ip_family_intent)
				SELECT $2,connection_id,$3,'fixture','connect','fixture',decode(repeat('02',32),'hex'),$4,4,4
				FROM unnest($1::uuid[]) connection_id`, connectionIds, f.clientIds[0], f.now, f.handlerId))
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO network_client_location
				(connection_id,client_id,city_location_id,region_location_id,country_location_id,arin_quality_verified)
				SELECT connection_id,$2,$3,$3,$3,true FROM unnest($1::uuid[]) connection_id`, connectionIds, f.clientIds[0], f.locationId))
		}, server.TxReadCommitted)
		started := time.Now()
		UpdateClientLocationReliabilities(f.ctx, f.now.Add(-time.Hour), f.now)
		row, found := readClientLocationReliabilityForTest(f.ctx, f.clientIds[0])
		if !found || row.valid || row.hashCount != 2 || !row.connected || !row.ipv4Proven {
			t.Fatal("client fanout was truncated into false valid evidence", row)
		}
		t.Logf("whole-client fanout=%d completed=%s; larger skew still has the finite page deadline", len(connectionIds)+1, time.Since(started))
	})
}
