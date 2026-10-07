// Final-policy controls retain both real constructors and the real Redis
// reader. An eligible durable contract must never reopen a packet SQL path.
package connect

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A held consumer makes live-forward reuse observable without another socket
// owner. The sentinel follows every packet through the same callback FIFO.
func testResidentPacketRedisOnlyConstruction(t testing.TB, value string, active, blockSource bool, mutate ...func(context.Context, server.Id, server.Id)) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := DefaultExchangeSettings()
	settings.EnableNetworkPeers = false
	settings.KeyEventDelivery.Enabled = false
	settings.ContractHoleCompatibilityFallback = true
	settings.ResidentForwardQueueShardCount = 1
	ledger := &residentPayloadLedger{}
	settings.payloadOwnerLedger = ledger
	exchange := NewExchange(ctx, "synthetic-resident.example", "connect", "synthetic", nil, nil, settings)
	defer exchange.Close()
	resident := NewResident(ctx, exchange, server.NewId(), server.NewId(), server.NewId())
	defer func() {
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	destination, sentinel := server.NewId(), server.NewId()
	insertResidentFallbackContract(ctx, resident.clientId, destination)
	status, _, err := model.ReadResumableContractLease(ctx, resident.clientId, destination)
	if err != nil || status != model.ContractHolePositive {
		t.Fatalf("durable setup did not establish a positive control: status=%d error=%v", status, err)
	}
	if value != "" {
		server.Redis(ctx, func(client server.RedisClient) {
			residentPacketSeedHole(ctx, client, resident.clientId, destination, value)
		})
	}
	for _, change := range mutate {
		change(ctx, resident.clientId, destination)
	}
	if blockSource {
		locked, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var barrierFailure any
		go func() {
			defer close(finished)
			barrierFailure = server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `LOCK TABLE transfer_contract IN ACCESS EXCLUSIVE MODE`))
					close(locked)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}, server.OptNoRetry())
			})
		}()
		defer func() {
			close(release)
			<-finished
			if barrierFailure != nil {
				t.Errorf("independent source barrier failed: %v", barrierFailure)
			}
		}()
		select {
		case <-locked:
		case <-finished:
			t.Fatal("independent source barrier ended before acquiring its lock")
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("independent source barrier did not acquire its lock")
		}
	}
	postgresAttempts, stopPostgresTripwire := server.DenyPostgresForTest(t)
	defer stopPostgresTripwire()
	forward := NewResidentForward(ctx, exchange, destination)
	resident.forwards[destination] = forward
	barrier, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(release) })
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		forward.Close()
		exchange.Close()
		if !exchange.WaitForIdle(context.Background()) {
			t.Error("exchange owners did not join")
		}
		if postgresAttempts() != 0 || server.PacketPostgresAttempts(resident.residentContractManager.ctx) != 0 {
			t.Errorf("Redis-only constructed packet path attempted PostgreSQL: process=%d packet=%d", postgresAttempts(), server.PacketPostgresAttempts(resident.residentContractManager.ctx))
		}
	}()
	defaultRead := resident.residentContractManager.readContract
	var redisReads atomic.Int32
	resident.residentContractManager.readContract = func(ctx context.Context, source, target server.Id) residentContractAllowance {
		if target == sentinel {
			close(barrier)
			<-release
			return residentContractAllowance{}
		}
		redisReads.Add(1)
		return defaultRead(ctx, source, target)
	}
	const packets = 32
	var witnesses [][]byte
	for range packets {
		witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, destination))
	}
	witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, sentinel))
	select {
	case <-barrier:
	case <-resident.ctx.Done():
		t.Fatal("constructed packet worker stopped before its FIFO barrier")
	case <-time.After(5 * time.Second):
		t.Fatal("constructed packet cohort did not reach its FIFO barrier")
	}
	wantAccepted := 0
	if active {
		wantAccepted = packets
	}
	if got := len(forward.send); got != wantAccepted || redisReads.Load() != 1 {
		t.Fatalf("constructed cohort accepted=%d Redis reads=%d, want accepted=%d reads=1", got, redisReads.Load(), wantAccepted)
	}
	releaseOnce.Do(func() { close(release) })
	if err := resident.CloseAndWait(context.Background()); err != nil {
		t.Error(err)
	}
	forward.Close()
	requireResidentPoolOwnersReturned(t, witnesses, "constructed Redis-only callback cohort")
	if snapshot := ledger.snapshot(); !snapshot.Complete || snapshot.Groups[residentPayloadForwardIngress].Messages != 0 || snapshot.Groups[residentPayloadForwardOutput].Messages != 0 {
		t.Fatal("constructed Redis-only cohort retained payload ownership")
	}
}

// Durable positive permission does not rescue an unknown Redis projection.
// Cases include command errors, mismatched evidence and all TTL boundaries.
func TestResidentPacketRedisOnlyConstructorsRejectUnknownWithoutPostgres(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, sample := range []struct {
			name   string
			value  string
			mutate func(context.Context, server.Id, server.Id)
		}{
			{name: "missing"},
			{name: "expired", value: "1", mutate: func(ctx context.Context, source, destination server.Id) {
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.PExpireAt(ctx, residentPacketHoleKey(source, destination), time.Unix(1, 0)).Err())
				})
			}},
			{name: "persistent", value: "1", mutate: func(ctx context.Context, source, destination server.Id) {
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.Persist(ctx, residentPacketHoleKey(source, destination)).Err())
				})
			}},
			{name: "overlong", value: "1", mutate: func(ctx context.Context, source, destination server.Id) {
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.PExpire(ctx, residentPacketHoleKey(source, destination), 2*model.DefaultContractExpiration).Err())
				})
			}},
			{name: "missing_members", value: "1", mutate: func(ctx context.Context, source, destination server.Id) {
				server.Redis(ctx, func(client server.RedisClient) {
					memberKey := strings.TrimSuffix(residentPacketHoleKey(source, destination), ":count") + ":members"
					server.Raise(client.Del(ctx, memberKey).Err())
				})
			}},
			{name: "command_error", mutate: func(ctx context.Context, source, destination server.Id) {
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.RPush(ctx, residentPacketHoleKey(source, destination), "synthetic wrong type").Err())
				})
			}},
		} {
			t.Logf("case=%s", sample.name)
			var changes []func(context.Context, server.Id, server.Id)
			if sample.mutate != nil {
				changes = append(changes, sample.mutate)
			}
			testResidentPacketRedisOnlyConstruction(t, sample.value, false, false, changes...)
		}
	})
}

// Both authoritative results remain Redis-only with the production constructor
// wiring; a live forward cannot bypass a known zero.
func TestResidentPacketRedisOnlyConstructorsUsePositiveAndZero(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		testResidentPacketRedisOnlyConstruction(t, "1", true, false)
		testResidentPacketRedisOnlyConstruction(t, "0", false, false)
	})
}

// An independent durable lock remains held until after the actual packet/read
// owners join. Neither missing nor positive Redis evidence waits for that lock.
func TestResidentPacketRedisOnlyConstructorsIgnoreBlockedSource(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		testResidentPacketRedisOnlyConstruction(t, "", false, true)
		testResidentPacketRedisOnlyConstruction(t, "1", true, true)
	})
}

// A closed real resident refuses another callback even if Redis remains
// positive; its borrowed payload and all owned workers are reclaimed.
func TestResidentPacketRedisOnlyConstructorsCancelWithoutPostgres(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		settings := DefaultExchangeSettings()
		settings.EnableNetworkPeers = false
		settings.KeyEventDelivery.Enabled = false
		settings.ContractHoleCompatibilityFallback = true
		exchange := NewExchange(ctx, "synthetic-resident.example", "connect", "synthetic", nil, nil, settings)
		defer exchange.Close()
		resident := NewResident(ctx, exchange, server.NewId(), server.NewId(), server.NewId())
		destination := server.NewId()
		server.Redis(ctx, func(client server.RedisClient) {
			residentPacketSeedHole(ctx, client, resident.clientId, destination, "1")
		})
		postgresAttempts, stopPostgresTripwire := server.DenyPostgresForTest(t)
		defer stopPostgresTripwire()
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		witness := offerResidentPacket(t, resident, resident.clientId, destination)
		if resident.residentContractManager.HasActiveContract(resident.clientId, destination) {
			t.Fatal("closed resident reused a positive projection")
		}
		exchange.Close()
		if !exchange.WaitForIdle(context.Background()) {
			t.Fatal("closed exchange owners did not join")
		}
		requireResidentPoolOwnerReturned(t, witness, "closed constructed Redis-only resident")
		if postgresAttempts() != 0 || server.PacketPostgresAttempts(resident.residentContractManager.ctx) != 0 {
			t.Fatal("closed constructed resident attempted PostgreSQL")
		}
	})
}
