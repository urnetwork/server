// The capability probe's prober shard against the database: its slot beside
// the URL probe's shards, a stale epoch of its own, and its cleanup.
package work

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/capprobe"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// The registry state of one prober shard epoch, "" for none.
func capabilityProbeShardTestState(t testing.TB, ctx context.Context, key model.ProberShardKey) string {
	t.Helper()
	state := ""
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT state FROM prober_shard_run WHERE task_id=$1 AND epoch=$2`, key.TaskId, key.Epoch)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&state))
			}
		})
	})
	return state
}

// The network a prober shard epoch owns.
func capabilityProbeShardTestNetworkId(t testing.TB, ctx context.Context, key model.ProberShardKey) server.Id {
	t.Helper()
	var networkId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT network_id FROM prober_shard_run WHERE task_id=$1 AND epoch=$2`, key.TaskId, key.Epoch).Scan(&networkId))
	})
	return networkId
}

// The network, client and balance rows a prober shard's network still holds.
func capabilityProbeShardTestRows(t testing.TB, ctx context.Context, networkId server.Id) int {
	t.Helper()
	var rows int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM network WHERE network_id=$1)+
			(SELECT count(*) FROM network_client WHERE network_id=$1)+
			(SELECT count(*) FROM transfer_balance WHERE network_id=$1)`, networkId).Scan(&rows))
	})
	return rows
}

// Drains and reaps a shard epoch a test left behind.
func capabilityProbeShardTestCleanup(t testing.TB, ctx context.Context, key model.ProberShardKey) {
	t.Helper()
	if err := model.DrainProberShard(ctx, key); err != nil {
		t.Fatal(err)
	}
	if deleted, err := model.ReapProberShard(ctx, key); err != nil || !deleted {
		t.Fatalf("shard %d epoch %s was not reaped: %t, %v", key.ShardIndex, key.Epoch, deleted, err)
	}
}

// A capability pass runs in prober shard slot 255 beside an active URL
// probe shard, in a network of its own. Taking the slot retires a stale
// epoch a previous execution of the probe's task left active, and leaves
// the URL shard's owner and identity as they were; the pass's own shard is
// drained and reaped when it returns, and its sweep stands.
func TestCapabilityProbeShardLifecycleBesideAUrlShard(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := capprobe.DefaultSettings()
		credit, err := capabilityProbeShardCredit(settings)
		if err != nil {
			t.Fatal(err)
		}
		urlKey := model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: 0, ShardCount: defaultProviderEgressProbeShardCount}
		urlOwner, err := model.BeginProberShard(ctx, urlKey, 64*1024, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		taskId := server.NewId()
		staleKey := model.ProberShardKey{TaskId: taskId, Epoch: server.NewId(), ShardIndex: capabilityProbeShardIndex, ShardCount: capabilityProbeShardCount}
		staleOwner, err := model.BeginProberShard(ctx, staleKey, credit, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		key := model.ProberShardKey{TaskId: taskId, Epoch: server.NewId(), ShardIndex: capabilityProbeShardIndex, ShardCount: capabilityProbeShardCount}
		sweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: server.NowUtc()}
		finishedSweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: sweep.StartedAt}
		pass := &capprobe.PassResult{Scanned: 1, Due: 1, Eligible: 1}
		finished := false
		var networkId server.Id
		next, result, err := runCapabilityProbeShard(ctx, key, settings, sweep, settings.MaxTime(), defaultProviderEgressProbeSettings("probe.example"), func(tunnelConfig providertunnel.Config) (capprobe.Sweep, *capprobe.PassResult, error) {
			finished = true
			if tunnelConfig.ClientCredentials == nil || tunnelConfig.ClientControl == nil || tunnelConfig.ByJwt == "" || tunnelConfig.ApiUrl != "https://api.probe.example" {
				t.Errorf("the pass's tunnels have no shard identity, credentials or control: %+v", tunnelConfig)
			}
			networkId = capabilityProbeShardTestNetworkId(t, ctx, key)
			if networkId == urlOwner.NetworkId || networkId == staleOwner.NetworkId {
				t.Error("the capability pass shares a network with another shard")
			}
			if state := capabilityProbeShardTestState(t, ctx, key); state != "active" {
				t.Errorf("the pass's shard is %q while it runs", state)
			}
			if state := capabilityProbeShardTestState(t, ctx, staleKey); state != "draining" {
				t.Errorf("the stale capability epoch is %q while the new one runs, want draining", state)
			}
			if _, err := model.ProberShardIdentity(ctx, staleOwner); !errors.Is(err, model.ErrProberShardRetired) {
				t.Errorf("the stale capability epoch still mints authority: %v", err)
			}
			if state := capabilityProbeShardTestState(t, ctx, urlKey); state != "active" {
				t.Errorf("the URL shard is %q beside the capability pass", state)
			}
			return finishedSweep, pass, nil
		})
		if err != nil || !finished || !reflect.DeepEqual(next, finishedSweep) || result != pass {
			t.Fatalf("the pass returned %+v, %+v, %v (finished %t)", next, result, err, finished)
		}
		if state := capabilityProbeShardTestState(t, ctx, key); state != "closed" {
			t.Fatalf("the pass's shard is %q after it, want closed", state)
		}
		if rows := capabilityProbeShardTestRows(t, ctx, networkId); rows != 0 {
			t.Fatalf("the pass's shard left %d network, client and balance rows", rows)
		}
		if state := capabilityProbeShardTestState(t, ctx, urlKey); state != "active" {
			t.Fatalf("the URL shard is %q after the capability pass", state)
		}
		if _, err := model.ProberShardIdentity(ctx, urlOwner); err != nil {
			t.Fatalf("the URL shard lost its identity beside the capability pass: %v", err)
		}
		capabilityProbeShardTestCleanup(t, ctx, urlKey)
		if deleted, err := model.ReapProberShard(ctx, staleKey); err != nil || !deleted {
			t.Fatalf("the stale capability epoch was not reaped: %t, %v", deleted, err)
		}
	})
}

// Root cause (review F3), through the database: a pass whose shard reap
// fails after the pass finished keeps its own sweep and result, counts no
// failed pass and one cleanup failure, and leaves the shard draining for the
// prober reaper, which reaps it once the cause is gone. The reap fails here
// because the shard's funding record changed under it, which the reaper
// refuses to delete.
func TestCapabilityProbeKeepsAFinishedPassWhenItsShardReapFails(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := capprobe.DefaultSettings()
		key := model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: capabilityProbeShardIndex, ShardCount: capabilityProbeShardCount}
		sweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: server.NowUtc()}
		finishedSweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: sweep.StartedAt}
		pass := &capprobe.PassResult{Scanned: 2, Due: 2, Eligible: 2, Started: 2, Recorded: 1}
		setFundingChanged := func(changed bool) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET pro=$3
					WHERE balance_id=(SELECT balance_id FROM prober_shard_run WHERE task_id=$1 AND epoch=$2)`, key.TaskId, key.Epoch, changed))
			})
		}
		failedLabels := map[string]string{"result": capprobe.PassResultFailed}
		failedBefore := capabilityProbeCounterValue(t, "urnetwork_capability_probe_passes_total", failedLabels)
		cleanupBefore := capabilityProbeCounterValue(t, "urnetwork_capability_probe_shard_cleanup_errors_total", map[string]string{})
		next, result, err := runCapabilityProbeShard(ctx, key, settings, sweep, settings.MaxTime(), defaultProviderEgressProbeSettings("probe.example"), func(providertunnel.Config) (capprobe.Sweep, *capprobe.PassResult, error) {
			setFundingChanged(true)
			return finishedSweep, pass, nil
		})
		if err != nil || !reflect.DeepEqual(next, finishedSweep) || result != pass {
			t.Fatalf("a finished pass whose reap failed returned %+v, %+v, %v; want its own sweep and result", next, result, err)
		}
		if failed := capabilityProbeCounterValue(t, "urnetwork_capability_probe_passes_total", failedLabels) - failedBefore; failed != 0 {
			t.Fatalf("a finished pass whose reap failed counted %v failed passes", failed)
		}
		if cleanup := capabilityProbeCounterValue(t, "urnetwork_capability_probe_shard_cleanup_errors_total", map[string]string{}) - cleanupBefore; cleanup != 1 {
			t.Fatalf("counted %v cleanup failures, want 1", cleanup)
		}
		if state := capabilityProbeShardTestState(t, ctx, key); state != "draining" {
			t.Fatalf("the shard whose reap failed is %q, want draining for the reaper", state)
		}
		networkId := capabilityProbeShardTestNetworkId(t, ctx, key)
		setFundingChanged(false)
		if deleted, err := model.ReapProberShard(ctx, key); err != nil || !deleted {
			t.Fatalf("the reaper did not reap the shard once its cause was gone: %t, %v", deleted, err)
		}
		if rows := capabilityProbeShardTestRows(t, ctx, networkId); rows != 0 {
			t.Fatalf("the reaped shard left %d network, client and balance rows", rows)
		}
	})
}
