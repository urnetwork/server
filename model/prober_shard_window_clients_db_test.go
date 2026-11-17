package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// On by default, paced at the code's fastest rate a test can wait for.
func proberWindowTestSettings() *ProberShardRetentionSettings {
	settings := proberRetentionTestSettings()
	settings.MaxWindowShardsPerMinute = 60000
	settings.DefaultMaxWindowShardsPerMinute = settings.MaxWindowShardsPerMinute
	return settings
}

// The rows keyed by a window that a pass leaves: a TLS certificate and a
// location fold, which a post-quantum turn's connected window has, and the
// role, provider intent and provide key an older pass's windows kept.
func proberWindowTestDependents(t testing.TB, ctx context.Context, owner *ProberShardOwner, windowId server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_tls_certificate (client_id, tls_certificate_pem, set_time) VALUES ($1, '\x00', $2)`, windowId, now))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability (client_id, update_block_number, network_id) VALUES ($1, 1, $2)`, windowId, owner.NetworkId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_role (client_id, role) VALUES ($1, 'probe')`, windowId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_provider_intent (client_id, create_time) VALUES ($1, $2)`, windowId, now))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provide_key (client_id, provide_mode, secret_key) VALUES ($1, $2, '\x01')`, windowId, ProvideModeNetwork))
	})
}

// The rows of each table keyed by the clients.
func proberWindowTestRows(t testing.TB, ctx context.Context, clientIds []server.Id) map[string]int {
	t.Helper()
	counts := map[string]int{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, table := range append([]string{"network_client"}, proberShardWindowDependentTables...) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE client_id=ANY($1)`, clientIds).Scan(&count))
			counts[table] = count
		}
	})
	return counts
}

// The window cursor just past the pass.
func proberWindowTestCursor(t testing.TB, ctx context.Context, owner *ProberShardOwner) ProberShardWindowCursor {
	t.Helper()
	cursor := ProberShardWindowCursor{TaskId: owner.Key.TaskId, Epoch: owner.Key.Epoch}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT create_time FROM prober_shard_run WHERE task_id=$1 AND epoch=$2`, owner.Key.TaskId, owner.Key.Epoch).Scan(&cursor.CreateTime))
	})
	return cursor
}

func proberWindowTestSameCursor(a ProberShardWindowCursor, b ProberShardWindowCursor) bool {
	return a.CreateTime.Equal(b.CreateTime) && a.TaskId == b.TaskId && a.Epoch == b.Epoch
}

// The root cause: every window client a pass mints stayed until its pass's
// generation dropped, eight days later, with its location fold and
// certificate, although nothing reads a retired window once its pass settled.
// The window step deletes them, and the rows keyed by them, an hour past the
// pass's deadline. The pass's own client, identity, contracts and registry row
// stay for its generation, whose drop still removes the rest whole.
func TestProberShardWindowStepRemovesASettledPassWindowsAndKeepsItsIdentity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		windows := []server.Id{}
		for range 3 {
			windows = append(windows, proberRetentionTestChild(t, ctx, owner))
		}
		contractId := proberRetentionTestOrigin(t, ctx, owner, windows[0], peer, 0)
		proberRetentionTestFlushDebits(t, ctx, owner)
		// the pass ends as a URL pass does: drained and settled
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))
		proberWindowTestDependents(t, ctx, owner, windows[0])
		settings := proberWindowTestSettings()
		before := proberWindowTestRows(t, ctx, windows)
		connect.AssertEqual(t, before["network_client"], 3)

		// within the hour past its deadline nothing goes
		report, next, err := RemoveProberShardWindows(ctx, owner.Deadline.Add(settings.WindowMinDeadlineAge), ProberShardWindowCursor{}, settings)
		if err != nil || !report.CaughtUp || report.Clients != 0 || len(report.Shards) != 0 || !proberWindowTestSameCursor(next, ProberShardWindowCursor{}) {
			t.Fatalf("a pass inside the hour past its deadline was taken up: %+v %+v %v", report, next, err)
		}
		if rows := proberWindowTestRows(t, ctx, windows); rows["network_client"] != 3 {
			t.Fatalf("windows went inside the hour: %v", rows)
		}

		// an hour past its deadline its windows go with their rows
		later := owner.Deadline.Add(settings.WindowMinDeadlineAge + time.Minute)
		report, next, err = RemoveProberShardWindows(ctx, later, ProberShardWindowCursor{}, settings)
		if err != nil || report.Shards[proberShardWindowRemoved] != 1 || report.Clients != 3 || !report.CaughtUp {
			t.Fatalf("the settled pass kept its windows: %+v %v", report, err)
		}
		if !proberWindowTestSameCursor(next, proberWindowTestCursor(t, ctx, owner)) {
			t.Fatalf("the cursor did not move past the pass: %+v", next)
		}
		after := proberWindowTestRows(t, ctx, windows)
		for table, count := range after {
			if count != 0 {
				t.Fatalf("the window step left %d %s rows of the windows", count, table)
			}
		}
		connect.AssertEqual(t, report.Deleted["network_client"], int64(3))
		for _, table := range proberShardWindowDependentTables {
			connect.AssertEqual(t, report.Deleted[table], int64(before[table]))
		}
		// the pass's identity, its own client and its contract stay
		if n, c, b := shardTestRows(t, ctx, owner); n != 1 || c != 1 || b != 1 {
			t.Fatalf("the window step deleted the identity: network=%d clients=%d balances=%d", n, c, b)
		}
		if rows := proberWindowTestRows(t, ctx, []server.Id{owner.ClientId}); rows["network_client"] != 1 {
			t.Fatal("the window step deleted the pass's own client")
		}
		if registered, state, _ := proberRetentionTestRegistered(t, ctx, owner.Key); !registered || state != "draining" {
			t.Fatalf("the pass's registry row is %s, want draining", state)
		}
		if rows := proberRetentionTestContractRows(t, ctx, []server.Id{contractId}); rows["transfer_contract"] != 1 || rows["transfer_escrow"] != 1 {
			t.Fatalf("the window step changed the pass's contract: %v", rows)
		}

		// the next run resumes past it and finds nothing
		report, again, err := RemoveProberShardWindows(ctx, later, next, settings)
		if err != nil || !report.CaughtUp || len(report.Shards) != 0 || !proberWindowTestSameCursor(again, next) {
			t.Fatalf("a run past the cleared pass took something up: %+v %+v %v", report, again, err)
		}

		// its generation still drops the rest whole
		dropReport, err := proberRetentionTestRun(ctx, proberRetentionTestLater(), proberRetentionTestSettings())
		if err != nil || dropReport.Shards[proberShardRetentionShardDropped] != 1 {
			t.Fatalf("the generation did not drop the pass: %+v %v", dropReport, err)
		}
		connect.AssertEqual(t, dropReport.Deleted["network_client"], int64(1))
		if n, c, b := shardTestRows(t, ctx, owner); n != 0 || c != 0 || b != 0 {
			t.Fatalf("the drop left network=%d clients=%d balances=%d", n, c, b)
		}
	})
}

// A due pass with an unresolved contract is waited at, and the passes after it
// with it, until WindowSkipAge; then it is passed over and its windows wait
// for its generation's drop, while the passes after it lose theirs.
func TestProberShardWindowStepWaitsAtAnUnsettledPassThenPassesItOver(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		peer := newEscrowSelectionTestClients(t, ctx)
		unsettled := shardTestOwner(t, ctx, shardTestKey(0))
		unsettledWindow := proberRetentionTestChild(t, ctx, unsettled)
		if _, err := CreateTransferEscrow(ctx, unsettled.NetworkId, unsettledWindow, peer.providerNetworkId, peer.providerId, 1024); err != nil {
			t.Fatal(err)
		}
		server.Raise(DrainProberShard(ctx, unsettled.Key))
		settled := shardTestOwner(t, ctx, shardTestKey(1))
		settledWindow := proberRetentionTestChild(t, ctx, settled)
		server.Raise(DrainProberShard(ctx, settled.Key))
		server.Raise(SettleProberShard(ctx, settled.Key))
		settings := proberWindowTestSettings()

		due := settled.Deadline.Add(settings.WindowMinDeadlineAge + time.Minute)
		report, next, err := RemoveProberShardWindows(ctx, due, ProberShardWindowCursor{}, settings)
		if err != nil || report.Shards[proberShardWindowWaiting] != 1 || report.Shards[proberShardWindowRemoved] != 0 ||
			!report.CaughtUp || !report.Idle() || !proberWindowTestSameCursor(next, ProberShardWindowCursor{}) {
			t.Fatalf("the step did not wait at the unsettled pass: %+v %+v %v", report, next, err)
		}
		if rows := proberWindowTestRows(t, ctx, []server.Id{unsettledWindow, settledWindow}); rows["network_client"] != 2 {
			t.Fatalf("a window went while the step waited: %v", rows)
		}

		past := unsettled.Deadline.Add(settings.WindowSkipAge + time.Minute)
		report, next, err = RemoveProberShardWindows(ctx, past, next, settings)
		if err != nil || report.Shards[proberShardWindowSkipped] != 1 || report.Shards[proberShardWindowRemoved] != 1 || report.Clients != 1 {
			t.Fatalf("the step did not pass the unsettled pass over: %+v %v", report, err)
		}
		if !proberWindowTestSameCursor(next, proberWindowTestCursor(t, ctx, settled)) {
			t.Fatalf("the cursor is not past the settled pass: %+v", next)
		}
		if rows := proberWindowTestRows(t, ctx, []server.Id{unsettledWindow}); rows["network_client"] != 1 {
			t.Fatal("the unsettled pass lost its window")
		}
		if rows := proberWindowTestRows(t, ctx, []server.Id{settledWindow}); rows["network_client"] != 0 {
			t.Fatal("the settled pass after it kept its window")
		}
	})
}

// A live pass past its deadline, which the settle sweep has not fenced, is
// waited at and then passed over, and keeps its windows either way.
func TestProberShardWindowStepNeverTakesALivePass(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		live := shardTestOwner(t, ctx, shardTestKey(0))
		window := proberRetentionTestChild(t, ctx, live)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false, deactivate_time=$2 WHERE client_id=$1`, window, server.NowUtc()))
		})
		settings := proberWindowTestSettings()

		report, next, err := RemoveProberShardWindows(ctx, live.Deadline.Add(2*time.Hour), ProberShardWindowCursor{}, settings)
		if err != nil || report.Shards[proberShardWindowWaiting] != 1 || !proberWindowTestSameCursor(next, ProberShardWindowCursor{}) {
			t.Fatalf("the step did not wait at the live pass: %+v %+v %v", report, next, err)
		}
		report, next, err = RemoveProberShardWindows(ctx, live.Deadline.Add(settings.WindowSkipAge+time.Minute), next, settings)
		if err != nil || report.Shards[proberShardWindowSkipped] != 1 || report.Clients != 0 ||
			!proberWindowTestSameCursor(next, proberWindowTestCursor(t, ctx, live)) {
			t.Fatalf("the step did not pass the live pass over: %+v %+v %v", report, next, err)
		}
		if rows := proberWindowTestRows(t, ctx, []server.Id{window}); rows["network_client"] != 1 {
			t.Fatal("a live pass lost a window")
		}
		if registered, state, _ := proberRetentionTestRegistered(t, ctx, live.Key); !registered || state != "active" {
			t.Fatalf("the live pass is %s, want active", state)
		}
	})
}

// A window with a connected connection, or with a hosted proxy resource, stays
// for the drop, which refuses such a pass; the pass's other windows go.
func TestProberShardWindowStepKeepsConnectedAndHostedWindows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		connected := proberRetentionTestChild(t, ctx, owner)
		handlerId := CreateNetworkClientHandler(ctx)
		if _, _, _, _, err := ConnectNetworkClient(ctx, connected, "192.0.2.1:0", handlerId); err != nil {
			t.Fatal(err)
		}
		hosted := proberRetentionTestChild(t, ctx, owner)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO proxy_device_config (proxy_id, client_id, instance_id, config_json) VALUES ($1, $2, $3, '{}')`,
				server.NewId(), hosted, server.NewId()))
		})
		plain := proberRetentionTestChild(t, ctx, owner)
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))
		settings := proberWindowTestSettings()

		report, _, err := RemoveProberShardWindows(ctx, owner.Deadline.Add(settings.WindowMinDeadlineAge+time.Minute), ProberShardWindowCursor{}, settings)
		if err != nil || report.Shards[proberShardWindowRemoved] != 1 || report.Clients != 1 {
			t.Fatalf("the step did not remove the plain window alone: %+v %v", report, err)
		}
		if rows := proberWindowTestRows(t, ctx, []server.Id{connected, hosted}); rows["network_client"] != 2 {
			t.Fatal("the step deleted a connected or hosted window")
		}
		if rows := proberWindowTestRows(t, ctx, []server.Id{plain}); rows["network_client"] != 0 {
			t.Fatal("the step kept the plain window")
		}
	})
}

// A pass with more windows than a batch loses them over several transactions
// in one run.
func TestProberShardWindowStepDeletesInBatches(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		windows := []server.Id{}
		for range 5 {
			windows = append(windows, proberRetentionTestChild(t, ctx, owner))
		}
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))
		settings := proberWindowTestSettings()
		settings.WindowBatchSize = 2

		report, next, err := RemoveProberShardWindows(ctx, owner.Deadline.Add(settings.WindowMinDeadlineAge+time.Minute), ProberShardWindowCursor{}, settings)
		if err != nil || report.Shards[proberShardWindowRemoved] != 1 || report.Clients != 5 ||
			!proberWindowTestSameCursor(next, proberWindowTestCursor(t, ctx, owner)) {
			t.Fatalf("the batched removal stopped short: %+v %+v %v", report, next, err)
		}
		if rows := proberWindowTestRows(t, ctx, windows); rows["network_client"] != 0 {
			t.Fatalf("batches left windows: %v", rows)
		}
	})
}

// A pass whose registry row another transaction holds ends the run without
// waiting, and the next run, once the row is free, removes its windows.
func TestProberShardWindowStepEndsAtAHeldPass(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		window := proberRetentionTestChild(t, ctx, owner)
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))
		settings := proberWindowTestSettings()
		later := owner.Deadline.Add(settings.WindowMinDeadlineAge + time.Minute)

		held := make(chan struct{})
		release := make(chan struct{})
		holderDone := make(chan struct{})
		go func() {
			defer close(holderDone)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM prober_shard_run WHERE task_id=$1 AND epoch=$2 FOR UPDATE`, owner.Key.TaskId, owner.Key.Epoch))
				close(held)
				<-release
			})
		}()
		<-held
		report, next, err := RemoveProberShardWindows(ctx, later, ProberShardWindowCursor{}, settings)
		close(release)
		<-holderDone
		if err != nil || report.Shards[proberShardWindowBusy] != 1 || report.Stop != proberShardRetentionStopBusy || report.Idle() ||
			!proberWindowTestSameCursor(next, ProberShardWindowCursor{}) {
			t.Fatalf("a held pass did not end the run: %+v %+v %v", report, next, err)
		}
		if rows := proberWindowTestRows(t, ctx, []server.Id{window}); rows["network_client"] != 1 {
			t.Fatal("a held pass lost its window")
		}
		report, _, err = RemoveProberShardWindows(ctx, later, next, settings)
		if err != nil || report.Shards[proberShardWindowRemoved] != 1 || report.Clients != 1 {
			t.Fatalf("the freed pass kept its window: %+v %v", report, err)
		}
	})
}

// The control: the code default is off; a dry run counts the windows a run
// would delete and changes nothing, the cursor included; windows off stops
// the step in the on mode; on removes.
func TestProberShardWindowStepControl(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		controlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		windows := []server.Id{proberRetentionTestChild(t, ctx, owner), proberRetentionTestChild(t, ctx, owner)}
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(SettleProberShard(ctx, owner.Key))
		settings := DefaultProberShardRetentionSettings()
		settings.DefaultMaxWindowShardsPerMinute = settings.MaxWindowShardsPerMinute
		later := owner.Deadline.Add(settings.WindowMinDeadlineAge + time.Minute)
		defer func() {
			removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			server.Raise(RemoveProberShardRetentionControl(removeCtx))
		}()

		report, next, err := RemoveProberShardWindows(ctx, later, ProberShardWindowCursor{}, settings)
		if err != nil || report.Stop != proberShardRetentionStopOff || len(report.Shards) != 0 || !report.Idle() {
			t.Fatalf("the code default took something up: %+v %v", report, err)
		}

		server.Raise(SetProberShardRetentionControl(controlCtx, map[string]string{ProberShardRetentionControlFieldMode: "dry_run"}))
		report, next, err = RemoveProberShardWindows(ctx, later, ProberShardWindowCursor{}, settings)
		if err != nil || report.Stop != proberShardRetentionStopDryRun || report.Shards[proberShardWindowDryRun] != 1 ||
			report.Eligible != 2 || report.Clients != 0 || len(report.Deleted) != 0 || !proberWindowTestSameCursor(next, ProberShardWindowCursor{}) {
			t.Fatalf("the dry run did not count the windows alone: %+v %+v %v", report, next, err)
		}
		if rows := proberWindowTestRows(t, ctx, windows); rows["network_client"] != 2 {
			t.Fatal("the dry run deleted a window")
		}

		server.Raise(SetProberShardRetentionControl(controlCtx, map[string]string{
			ProberShardRetentionControlFieldMode:    "on",
			ProberShardRetentionControlFieldWindows: "off",
		}))
		report, _, err = RemoveProberShardWindows(ctx, later, ProberShardWindowCursor{}, settings)
		if err != nil || report.Stop != proberShardRetentionStopOff || len(report.Shards) != 0 {
			t.Fatalf("windows off took something up: %+v %v", report, err)
		}

		server.Raise(SetProberShardRetentionControl(controlCtx, map[string]string{ProberShardRetentionControlFieldWindows: "on"}))
		report, _, err = RemoveProberShardWindows(ctx, later, ProberShardWindowCursor{}, settings)
		if err != nil || report.Shards[proberShardWindowRemoved] != 1 || report.Clients != 2 {
			t.Fatalf("windows on did not remove: %+v %v", report, err)
		}
		if rows := proberWindowTestRows(t, ctx, windows); rows["network_client"] != 0 {
			t.Fatal("windows on kept a window")
		}
	})
}
