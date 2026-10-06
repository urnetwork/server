package work

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Mandatory release regression: two independent processes, distinct actual
// derived clients in ONE funded network, the same grant, simultaneous admission.
// The separate held-row regression forces each historical serializer directly.
func TestContractCreationSameNetworkLargeNContentionFree(t *testing.T) {
	count := 512
	if value := os.Getenv("URNETWORK_CONTRACT_CONTENTION_CLIENTS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 256 || n > 4096 || n%2 != 0 {
			t.Fatal("invalid contention workload size")
		}
		count = n
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 110*time.Second)
		defer cancel()
		f := newPrivateLoadFixtureCount(t, ctx, 0, 0, count)
		// Enough durable credit for >10 times both complete waves.
		if 260*1024*model.Gib < int64(count)*2*128*model.Mib*10 {
			t.Fatal("fixture headroom insufficient")
		}
		var peerNetwork server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT network_id FROM network_client WHERE client_id=$1`, f.peer).Scan(&peerNetwork))
		})
		peers := make([]server.Id, count/2)
		for i := range peers {
			peers[i] = server.NewId()
			model.Testing_CreateDevice(ctx, peerNetwork, server.NewId(), peers[i], "synthetic destination", "fixture")
			model.SetProvide(ctx, peers[i], map[model.ProvideMode][]byte{model.ProvideModePublic: bytes.Repeat([]byte{29}, 32)})
		}
		// Exercise production's unconditional default; no test-only mode switch.
		pop := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 16\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		child := privateLoadStartPeerProcess(t, ctx, 100*time.Second)
		defer child.close(t)
		encoder, decoder := child.encoder, child.decoder
		config := privateLoadProcessConfig{PG: server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName).Bytes(), Redis: server.Vault.RequireSimpleResource("redis.yml").Bytes(), Owner: f.owner, Peer: f.peer, Tokens: f.tokens[count/2:], Clients: f.clients[count/2:], PoolSize: 16, Peers: peers}
		if err := encoder.Encode(config); err != nil {
			t.Fatal(err)
		}
		child.requireReady(t)
		local := f
		local.tokens = f.tokens[:count/2]
		local.clients = f.clients[:count/2]
		local.peers = peers
		observe := privateLoadStartObserver(ctx, t)
		for wave := range 2 {
			localReady, release := make(chan struct{}), make(chan struct{})
			localResult := make(chan privateLoadProcessReport, 1)
			if wave == 1 {
				local.peers = nil
			} // Shared destination control remains healthy too.
			go func() { localResult <- privateLoadCreateWaveBarrier(ctx, t, local, localReady, release) }()
			command := "prepare_create"
			if wave == 1 {
				command = "prepare_create_shared"
			}
			if err := encoder.Encode(command); err != nil {
				t.Fatal(err)
			}
			child.requireReady(t)
			<-localReady
			if err := encoder.Encode("go"); err != nil {
				t.Fatal(err)
			}
			close(release)
			left := <-localResult
			var right privateLoadProcessReport
			if err := decoder.Decode(&right); err != nil {
				t.Fatal("child result missing")
			}
			if left.Completed+right.Completed != count || left.Failed+right.Failed != 0 {
				t.Fatalf("wave %d completed=%d failed=%d classes=%v/%v", wave, left.Completed+right.Completed, left.Failed+right.Failed, left.Errors, right.Errors)
			}
			overlapStart := left.Started
			if right.Started.After(overlapStart) {
				overlapStart = right.Started
			}
			overlapEnd := left.Started.Add(left.Elapsed)
			if right.Started.Add(right.Elapsed).Before(overlapEnd) {
				overlapEnd = right.Started.Add(right.Elapsed)
			}
			if !overlapStart.Before(overlapEnd) {
				t.Fatal("independent processes did not overlap")
			}
			if left.Elapsed > 3*time.Second || right.Elapsed > 3*time.Second {
				t.Fatal("contract admission throughput regression")
			}
			t.Logf("clients=%d processes=2 wave=%d completed=%d latency=%s/%s overlap=%s", count, wave, count, left.Elapsed, right.Elapsed, overlapEnd.Sub(overlapStart))
		}
		activity := observe()
		for _, key := range []string{"balance_lock_peak", "cache_lock_peak", "census_lock_peak"} {
			if activity[key] != 0 {
				t.Errorf("financial database contention %s=%v", key, activity[key])
			}
		}
		t.Logf("bounded_activity=%v", activity)
		if err := encoder.Encode("stop"); err != nil {
			t.Fatal(err)
		}
		if err := child.wait(); err != nil {
			t.Fatal("child did not join")
		}
		var durable int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE balance_id=$1 AND redis_reserved`, f.owner.BalanceId).Scan(&durable))
		})
		if durable != 2*count {
			t.Fatalf("durable contract reservations=%d want=%d", durable, 2*count)
		}
		if got := model.Testing_NetEscrowByteCount(ctx, f.owner.BalanceId); got != int64(2*count)*128*model.Mib {
			t.Fatalf("Redis reservations differ from committed workload")
		}
	})
}
