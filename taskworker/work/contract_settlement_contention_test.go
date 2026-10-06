package work

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Both closes use the real authenticated owner and current public model path.
func privateLoadSettleWave(ctx context.Context, t testing.TB, f privateLoadFixture, contracts []*protocol.StoredContract, ready chan<- struct{}, release <-chan struct{}) privateLoadProcessReport {
	start := make(chan struct{})
	results := make(chan error, len(contracts))
	for index, stored := range contracts {
		go func() {
			<-start
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			results <- server.HandleError1(func() error {
				if _, err := privateLoadCall(bounded, f, f.tokens[index], &protocol.CloseContract{ContractId: stored.ContractId, AckedByteCount: 11}); err != nil {
					return err
				}
				id, err := server.IdFromBytes(stored.ContractId)
				if err != nil {
					return err
				}
				destination := f.peer
				if len(f.peers) > 0 {
					destination = f.peers[index%len(f.peers)]
				}
				return model.CloseContract(bounded, id, destination, 11, false)
			}, func(err error) error { return err })
		}()
	}
	ready <- struct{}{}
	<-release
	report := privateLoadProcessReport{Started: time.Now()}
	close(start)
	for range contracts {
		if err := <-results; err != nil {
			report.Failed++
		} else {
			report.Completed++
		}
	}
	report.Elapsed = time.Since(report.Started)
	return report
}

// Mandatory same-network regression: two independent processes settle 512
// funded contracts simultaneously while a third transaction holds their grant.
// Completion under that lock proves no hidden process-local serializer can
// turn a shared row queue into a misleading low-contention sample.
func TestContractSettlementSameNetworkLargeNContentionFree(t *testing.T) {
	count := 512
	if value := os.Getenv("URNETWORK_CONTRACT_CONTENTION_CLIENTS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 256 || n > 4096 || n%2 != 0 {
			t.Fatal("invalid settlement workload size")
		}
		count = n
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 110*time.Second)
		defer cancel()
		f := newPrivateLoadFixtureCount(t, ctx, 0, 0, count)
		child := privateLoadStartPeerProcess(t, ctx, 100*time.Second)
		defer child.close(t)
		encoder, decoder := child.encoder, child.decoder
		config := privateLoadProcessConfig{PG: server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName).Bytes(), Redis: server.Vault.RequireSimpleResource("redis.yml").Bytes(), Owner: f.owner, Peer: f.peer, Tokens: f.tokens[count/2:], Clients: f.clients[count/2:], PoolSize: 16}
		server.Raise(encoder.Encode(config))
		child.requireReady(t)
		local := f
		local.tokens = f.tokens[:count/2]
		local.clients = f.clients[:count/2]
		server.Raise(encoder.Encode("create"))
		created := privateLoadCreateWave(ctx, t, local)
		var remoteCreated privateLoadProcessReport
		server.Raise(decoder.Decode(&remoteCreated))
		if created.Completed+remoteCreated.Completed != count || created.Failed+remoteCreated.Failed != 0 {
			t.Fatal("fully funded preparation failed")
		}
		conn, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.owner.BalanceId))
		localReady, release := make(chan struct{}), make(chan struct{})
		localResult := make(chan privateLoadProcessReport, 1)
		go func() { localResult <- privateLoadSettleWave(ctx, t, local, created.Contracts, localReady, release) }()
		server.Raise(encoder.Encode("prepare_settle"))
		child.requireReady(t)
		<-localReady
		observe := privateLoadStartObserver(ctx, t)
		server.Raise(encoder.Encode("go"))
		close(release)
		left := <-localResult
		var right privateLoadProcessReport
		server.Raise(decoder.Decode(&right))
		activity := observe()
		// Keep the grant held until both complete and durable journal rows are read.
		if left.Completed+right.Completed != count || left.Failed+right.Failed != 0 {
			t.Fatalf("settlement queued on shared grant: completed=%d failed=%d", left.Completed+right.Completed, left.Failed+right.Failed)
		}
		overlapStart := left.Started
		if right.Started.After(overlapStart) {
			overlapStart = right.Started
		}
		overlapEnd := left.Started.Add(left.Elapsed)
		if end := right.Started.Add(right.Elapsed); end.Before(overlapEnd) {
			overlapEnd = end
		}
		if !overlapStart.Before(overlapEnd) {
			t.Fatal("independent settlement owners did not overlap")
		}
		if left.Elapsed > 3*time.Second || right.Elapsed > 3*time.Second {
			t.Fatal("same-network settlement throughput regressed")
		}
		for _, key := range []string{"balance_lock_peak", "cache_lock_peak", "census_lock_peak"} {
			if activity[key] != 0 {
				t.Fatal("shared financial wait in current settlement", key, activity[key])
			}
		}
		var pending int
		var consumed, credit model.ByteCount
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),sum(debit_byte_count) FROM transfer_debit_journal WHERE balance_id=$1 AND NOT applied`, f.owner.BalanceId).Scan(&pending, &consumed))
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.owner.BalanceId).Scan(&credit))
		})
		if pending != count || consumed != model.ByteCount(11*count) || credit != 260*1024*model.Gib {
			t.Fatal("pending exact consumption was not durable", pending, consumed, credit)
		}
		t.Logf("settlement clients=%d processes=2 completed=%d elapsed=%s/%s overlap=%s activity=%v", count, count, left.Elapsed, right.Elapsed, overlapEnd.Sub(overlapStart), activity)
		server.Raise(held.Rollback(ctx))
		for shard := range model.TransferDebitShardCount {
			for page := 0; page < 1+count/512; page++ {
				result, err := model.FlushTransferDebits(ctx, shard, nil, 64)
				server.Raise(err)
				if result.Failed != 0 {
					t.Fatal("writeback failed", result)
				}
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.owner.BalanceId).Scan(&credit))
		})
		if credit != 260*1024*model.Gib-model.ByteCount(count*11) || model.Testing_NetEscrowByteCount(ctx, f.owner.BalanceId) != 0 {
			t.Fatal("batched settlement accounting differs", credit)
		}
		server.Raise(encoder.Encode("stop"))
		server.Raise(child.wait())
	})
}
