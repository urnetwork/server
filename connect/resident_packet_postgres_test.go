package connect

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Offers a borrowed callback frame and releases the caller's original owner.
func offerResidentPacket(t testing.TB, resident *Resident, source, destination server.Id) []byte {
	t.Helper()
	message := clientconnect.MessagePoolGet(1200)
	witness := retainResidentPoolWitness(message)
	resident.handleClientForward(clientconnect.TransferPath{
		SourceId: clientconnect.Id(source), DestinationId: clientconnect.Id(destination),
	}, message)
	clientconnect.MessagePoolReturn(message)
	return witness
}

// Prefill the real callback queues before their sole consumers start. The
// baseline reads once for every denied packet; admission reduces each resident's
// same-pair cohort to one read without waiting, retrying, or losing pool owners.
func TestResidentPacketDeniedCohortHasOneCheckPerPair(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(server.WithoutPostgres(context.Background()))
		defer cancel()
		const residentCount, packetCount = 16, 128
		var calls atomic.Int32
		var residents []*Resident
		var witnesses [][]byte
		var ledgers []*residentPayloadLedger
		for range residentCount {
			settings := DefaultExchangeSettings()
			settings.ResidentForwardQueueShardCount = 1
			resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
			resident.residentContractManager = newResidentContractManager(resident.ctx, resident.cancel, resident.clientId, settings)
			resident.residentContractManager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool {
				calls.Add(1)
				return false
			})
			ledger := &residentPayloadLedger{}
			resident.exchange.payloadOwnerLedger = ledger
			ledgers = append(ledgers, ledger)
			shard := &resident.forwardIngress[0]
			shard.startOnce.Do(func() { shard.queue = make(chan residentForwardIngress, settings.ResidentForwardQueueSize) })
			destination := server.NewId()
			for range packetCount {
				witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, destination))
			}
			if len(shard.queue) != packetCount {
				t.Fatal("callback cohort was not completely admitted before consumption")
			}
			residents = append(residents, resident)
		}
		for _, resident := range residents {
			resident.callbackWorkers.Add(1)
			go func() {
				defer resident.callbackWorkers.Done()
				resident.runClientForwardIngress(resident.forwardIngress[0].queue)
			}()
		}
		synctest.Wait()
		if got := calls.Load(); got != residentCount {
			t.Errorf("denied packet checks=%d, want %d for %d queued packets", got, residentCount, residentCount*packetCount)
		}
		for index, resident := range residents {
			if len(resident.forwards) != 0 || len(resident.forwardIngress[0].queue) != 0 {
				t.Error("denied cohort created a forward or retained queued work")
			}
			if snapshot := ledgers[index].snapshot(); !snapshot.Complete || snapshot.Groups[residentPayloadForwardIngress].Messages != 0 || snapshot.Groups[residentPayloadForwardOutput].Messages != 0 {
				t.Errorf("denied payload owners remained: %+v", snapshot)
			}
		}
		cancel()
		for _, resident := range residents {
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}
		requireResidentPoolOwnersReturned(t, witnesses, "rate-refused callback cohort")
		if server.PacketPostgresAttempts(ctx) != 0 {
			t.Fatal("packet cohort attempted PostgreSQL")
		}
		t.Logf("residents=%d queued_packets=%d checks=%d postgres_attempts=0", residentCount, residentCount*packetCount, calls.Load())
	})
}

// Wrong authenticated source is rejected before authorization or lazy worker
// creation, even when the destination could otherwise have an open contract.
func TestResidentPacketWrongSourceDoesNotReadAuthorization(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(server.WithoutPostgres(context.Background()))
		defer cancel()
		settings := DefaultExchangeSettings()
		resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
		resident.residentContractManager = newResidentContractManager(resident.ctx, resident.cancel, resident.clientId, settings)
		var calls atomic.Int32
		resident.residentContractManager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool { calls.Add(1); return true })
		witness := offerResidentPacket(t, resident, server.NewId(), server.NewId())
		synctest.Wait()
		if calls.Load() != 0 || len(resident.forwards) != 0 {
			t.Fatal("wrong-source packet reached authorization")
		}
		for shardIndex := range resident.forwardIngress {
			if resident.forwardIngress[shardIndex].queue != nil {
				t.Fatal("wrong source started a callback worker")
			}
		}
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		requireResidentPoolOwnerReturned(t, witness, "wrong-source refusal")
		if server.PacketPostgresAttempts(ctx) != 0 {
			t.Fatal("wrong-source packet attempted PostgreSQL")
		}
	})
}

// The Redis wire key is part of the packet projection contract, using synthetic
// client ids only. Model lifecycle tests separately prove how it is populated.
func residentPacketHoleKey(source, destination server.Id) string {
	pair := model.NewUnorderedTransferPair(source, destination)
	return fmt.Sprintf("contract-hole:v2:{%s:%s}:count", pair.A, pair.B)
}

// Drives the unmocked Redis authorization and destination reader through the
// actual callback/consumer/forward path. A sentinel on the same FIFO worker is
// the completion barrier; the only wrapper records real model read attempts.
func testResidentPacketRedisProjection(t testing.TB, value string, active bool, mutate ...func(context.Context, server.Id, server.Id)) {
	testResidentPacketRedisProjectionWithFallback(t, value, active, nil, mutate...)
}

// The same acquisition tripwire covers a configured bridge: authoritative
// positive and negative evidence must not consult its source helper.
func testResidentPacketRedisProjectionWithFallback(t testing.TB, value string, active bool, fallback *residentContractFallback, mutate ...func(context.Context, server.Id, server.Id)) {
	t.Helper()
	postgresAttempts, stopPostgresTripwire := server.DenyPostgresForTest(t)
	defer stopPostgresTripwire()
	ctx, cancel := context.WithCancel(server.WithoutPostgres(context.Background()))
	defer cancel()
	settings := DefaultExchangeSettings()
	settings.ResidentForwardQueueShardCount = 1
	resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
	defer func() {
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		if got := postgresAttempts(); got != 0 {
			t.Errorf("process packet PostgreSQL attempts=%d", got)
		}
	}()
	resident.residentContractManager = newResidentContractManagerWithFallback(resident.ctx, resident.cancel, resident.clientId, settings, fallback)
	ledger := &residentPayloadLedger{}
	resident.exchange.payloadOwnerLedger = ledger
	destination, sentinel := server.NewId(), server.NewId()
	if value != "" {
		server.Redis(ctx, func(client server.RedisClient) {
			residentPacketSeedHole(ctx, client, resident.clientId, destination, value)
		})
	}
	for _, change := range mutate {
		change(ctx, resident.clientId, destination)
	}
	barrier, release := make(chan struct{}), make(chan struct{})
	defaultRead := resident.residentContractManager.readContract
	var calls atomic.Int32
	resident.residentContractManager.readContract = func(ctx context.Context, source, target server.Id) residentContractAllowance {
		if target == sentinel {
			close(barrier)
			<-release
			return residentContractAllowance{}
		} else {
			calls.Add(1)
		}
		return defaultRead(ctx, source, target)
	}
	var witnesses [][]byte
	for range 32 {
		witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, destination))
	}
	witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, sentinel))
	select {
	case <-barrier:
	case <-resident.ctx.Done():
		close(release)
		_ = resident.CloseAndWait(context.Background())
		t.Fatalf("packet worker stopped before its FIFO barrier: postgres_attempts=%d", server.PacketPostgresAttempts(ctx))
	case <-time.After(5 * time.Second):
		cancel()
		close(release)
		_ = resident.CloseAndWait(context.Background())
		t.Fatal("Redis packet callback did not reach its FIFO completion barrier")
	}
	resident.stateLock.RLock()
	forward := resident.forwards[destination]
	resident.stateLock.RUnlock()
	if active && (forward == nil || calls.Load() != 1) {
		t.Error("positive hole did not establish one reusable live forward")
	}
	if !active && forward != nil {
		t.Error("missing or malformed hole established a forward")
	}
	if calls.Load() == 0 {
		t.Error("real Redis projection reader was not reached")
	}
	close(release)
	cancel()
	if err := resident.CloseAndWait(context.Background()); err != nil {
		t.Error(err)
	}
	requireResidentPoolOwnersReturned(t, witnesses, "Redis projection callback")
	if got := server.PacketPostgresAttempts(ctx); got != 0 {
		t.Errorf("packet PostgreSQL attempts=%d", got)
	}
	if snapshot := ledger.snapshot(); !snapshot.Complete || snapshot.Groups[residentPayloadForwardIngress].Messages != 0 || snapshot.Groups[residentPayloadForwardOutput].Messages != 0 {
		t.Errorf("packet projection retained payload owners: %+v", snapshot)
	}
}

func TestResidentPacketRedisPositiveHasNoPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) { testResidentPacketRedisProjection(t, "1", true) })
}

func TestResidentPacketRedisMissingHasNoPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) { testResidentPacketRedisProjection(t, "", false) })
}

func TestResidentPacketRedisMalformedHasNoPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) { testResidentPacketRedisProjection(t, "synthetic-invalid-count", false) })
}

func TestResidentPacketRedisZeroHasNoPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) { testResidentPacketRedisProjection(t, "0", false) })
}

func TestResidentPacketRedisPersistentHasNoPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testResidentPacketRedisProjection(t, "1", false, func(ctx context.Context, source, destination server.Id) {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Persist(ctx, residentPacketHoleKey(source, destination)).Err())
			})
		})
	})
}

func TestResidentPacketRedisExpiredHasNoPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testResidentPacketRedisProjection(t, "1", false, func(ctx context.Context, source, destination server.Id) {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.PExpireAt(ctx, residentPacketHoleKey(source, destination), time.Unix(1, 0)).Err())
			})
		})
	})
}

func TestResidentPacketRedisWrongPairHasNoPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testResidentPacketRedisProjection(t, "1", false, func(ctx context.Context, source, destination server.Id) {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Del(ctx, residentPacketHoleKey(source, destination)).Err())
				residentPacketSeedHole(ctx, client, source, server.NewId(), "1")
			})
		})
	})
}

// A real Redis command error must deny through the same callback pipeline;
// malformed backing type cannot trigger a hidden SQL recovery path.
func TestResidentPacketRedisReadErrorHasNoPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testResidentPacketRedisProjection(t, "1", false, func(ctx context.Context, source, destination server.Id) {
			server.Redis(ctx, func(client server.RedisClient) {
				key := residentPacketHoleKey(source, destination)
				server.Raise(client.Del(ctx, key).Err())
				server.Raise(client.RPush(ctx, key, "synthetic wrong Redis type").Err())
			})
		})
	})
}

// Uses both real Redis reads, the real callback shard and forward socket pump.
// A synthetic resident and net.Pipe peer prove successful payload delivery and
// reuse, rather than only observing a forward parked on a missing destination.
func TestResidentPacketRedisPositiveDeliversAndReusesSocket(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		postgresAttempts, stopPostgresTripwire := server.DenyPostgresForTest(t)
		defer stopPostgresTripwire()
		ctx, cancel := context.WithCancel(server.WithoutPostgres(context.Background()))
		defer cancel()
		settings := DefaultExchangeSettings()
		settings.ExchangePingTimeout = time.Hour
		// Interval boundaries have separate virtual-clock controls. This fixture
		// isolates actual Redis and socket I/O from machine scheduling delays.
		settings.ContractManagerCheckTimeout = time.Hour
		resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
		resident.residentContractManager = newResidentContractManager(resident.ctx, resident.cancel, resident.clientId, settings)
		ledger := &residentPayloadLedger{}
		resident.exchange.payloadOwnerLedger = ledger
		destination, destinationResident := server.NewId(), server.NewId()
		var socketWorkers sync.WaitGroup
		defer func() {
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			socketWorkers.Wait()
			if got := postgresAttempts(); got != 0 {
				t.Errorf("successful packet PostgreSQL attempts=%d", got)
			}
		}()
		server.Redis(ctx, func(client server.RedisClient) {
			residentPacketSeedHole(ctx, client, resident.clientId, destination, "1")
		})
		if !model.NominateResident(ctx, nil, &model.NetworkClientResident{
			ClientId: destination, InstanceId: server.NewId(), ResidentId: destinationResident,
			ResidentHost: "packet-destination.example", ResidentInternalPorts: []int{1},
		}, time.Minute) {
			t.Fatal("synthetic Redis resident was not nominated")
		}
		delivered := make(chan string, 2)
		var dials, reads atomic.Int32
		defaultRead := resident.residentContractManager.readContract
		resident.residentContractManager.readContract = func(ctx context.Context, source, target server.Id) residentContractAllowance {
			reads.Add(1)
			return defaultRead(ctx, source, target)
		}
		settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			local, remote := net.Pipe()
			socketWorkers.Add(1)
			go func() {
				defer socketWorkers.Done()
				defer remote.Close()
				buffer := NewDefaultExchangeBuffer(settings)
				header, err := buffer.ReadHeader(ctx, remote)
				if err != nil {
					t.Error(err)
					return
				}
				if header.Op != ExchangeOpForward || header.ClientId != destination || header.ResidentId != destinationResident {
					t.Error("forward did not use the exact Redis resident")
					return
				}
				if err := buffer.WriteHeader(ctx, remote, header); err != nil {
					t.Error(err)
					return
				}
				for {
					message, err := buffer.ReadMessage(remote)
					if err != nil {
						if !isExpectedExchangeFixtureCloseError(err) {
							t.Error(err)
						}
						return
					}
					value := string(message)
					clientconnect.MessagePoolReturn(message)
					delivered <- value
				}
			}()
			return local, nil
		}
		var witnesses [][]byte
		for index := range 2 {
			value := fmt.Sprintf("synthetic ordinary packet %d", index)
			message := clientconnect.MessagePoolCopy([]byte(value))
			witnesses = append(witnesses, retainResidentPoolWitness(message))
			resident.handleClientForward(clientconnect.TransferPath{
				SourceId: clientconnect.Id(resident.clientId), DestinationId: clientconnect.Id(destination),
			}, message)
			clientconnect.MessagePoolReturn(message)
			select {
			case got := <-delivered:
				if got != value {
					t.Fatal("forward changed the packet payload")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("real Redis-authorized packet did not reach the socket peer")
			}
		}
		if reads.Load() != 1 || dials.Load() != 1 {
			t.Fatalf("healthy forwarding did not reuse allowance/socket: reads=%d dials=%d", reads.Load(), dials.Load())
		}
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		socketWorkers.Wait()
		requireResidentPoolOwnersReturned(t, witnesses, "delivered Redis-authorized socket packets")
		if snapshot := ledger.snapshot(); !snapshot.Complete || snapshot.Groups[residentPayloadForwardIngress].Messages != 0 || snapshot.Groups[residentPayloadForwardOutput].Messages != 0 {
			t.Errorf("delivered packet owners remained after join: %+v", snapshot)
		}
	})
}

// A live forward cannot indefinitely bypass the five-second authorization
// bound. A resumable checkpoint keeps its allowance, while a later final close
// drops offers without renewing activity or retaining their payload owners.
func TestResidentPacketLiveForwardRechecksAndRevokes(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(server.WithoutPostgres(context.Background()))
		defer cancel()
		settings := DefaultExchangeSettings()
		resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
		ledger := &residentPayloadLedger{}
		resident.exchange.payloadOwnerLedger = ledger
		destination := server.NewId()
		forward := NewResidentForward(ctx, resident.exchange, destination)
		defer forward.Close()
		resident.forwards[destination] = forward
		var calls atomic.Int32
		var active atomic.Bool
		active.Store(true)
		resident.residentContractManager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool { calls.Add(1); return active.Load() })
		first := offerResidentPacket(t, resident, resident.clientId, destination)
		synctest.Wait()
		if len(forward.send) != 1 || calls.Load() != 1 {
			t.Fatal("live forward bypassed its initial authorization")
		}
		// A model checkpoint still has an open/resumable hole; expire the old
		// cache so this offer must observe that positive projection again.
		time.Sleep(settings.ContractManagerCheckTimeout)
		resumed := offerResidentPacket(t, resident, resident.clientId, destination)
		synctest.Wait()
		if len(forward.send) != 2 || calls.Load() != 2 {
			t.Fatal("checkpoint-resumable allowance did not pass a fresh check")
		}
		acceptedAt := forward.lastActivityNanos.Load()
		active.Store(false)
		time.Sleep(settings.ContractManagerCheckTimeout)
		refused := offerResidentPacket(t, resident, resident.clientId, destination)
		synctest.Wait()
		if len(forward.send) != 2 || calls.Load() != 3 || forward.lastActivityNanos.Load() != acceptedAt {
			t.Fatal("final-close refusal accepted or renewed live forward work")
		}
		requireResidentPoolOwnerReturned(t, refused, "revoked live-forward offer")
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		forward.Close() // This test owns the held consumer instead of Run.
		requireResidentPoolOwnersReturned(t, [][]byte{first, resumed}, "accepted live-forward shutdown")
		if server.PacketPostgresAttempts(ctx) != 0 {
			t.Fatal("live packet allowance attempted PostgreSQL")
		}
		if snapshot := ledger.snapshot(); !snapshot.Complete || snapshot.Groups[residentPayloadForwardIngress].Messages != 0 || snapshot.Groups[residentPayloadForwardOutput].Messages != 0 {
			t.Errorf("live-forward revocation retained payload owners: %+v", snapshot)
		}
	})
}

// The existing destination-count limit still refuses a new pair before any
// Redis read, while an already admitted destination remains independently usable.
func TestResidentPacketForwardLimitDoesNotReadNewPair(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(server.WithoutPostgres(context.Background()))
		defer cancel()
		settings := DefaultExchangeSettings()
		settings.MaxConcurrentForwardsPerResident = 1
		resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
		destination := server.NewId()
		forward := NewResidentForward(ctx, resident.exchange, destination)
		defer forward.Close()
		resident.forwards[destination] = forward
		var calls atomic.Int32
		resident.residentContractManager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool { calls.Add(1); return true })
		refused := offerResidentPacket(t, resident, resident.clientId, server.NewId())
		synctest.Wait()
		if calls.Load() != 0 || len(resident.forwards) != 1 {
			t.Fatal("new-pair destination limit reached authorization")
		}
		requireResidentPoolOwnerReturned(t, refused, "forward-count limit")
		accepted := offerResidentPacket(t, resident, resident.clientId, destination)
		synctest.Wait()
		if calls.Load() != 1 || len(forward.send) != 1 {
			t.Fatal("destination limit blocked the existing authorized forward")
		}
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		forward.Close()
		requireResidentPoolOwnerReturned(t, accepted, "existing destination shutdown")
		if server.PacketPostgresAttempts(ctx) != 0 {
			t.Fatal("destination limit attempted PostgreSQL")
		}
	})
}

// Unknown destinations consume only the existing resident's finite destination
// budget. A healthy cached pair remains reusable at that cap, and a different
// resident keeps its own budget. Expiry admits new pairs without a retry queue.
func TestResidentPacketDeniedPairHistoryHasOwnerBound(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(server.WithoutPostgres(context.Background()))
		defer cancel()
		settings := DefaultExchangeSettings()
		settings.ResidentForwardQueueShardCount = 1
		settings.MaxConcurrentForwardsPerResident = 4
		resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
		healthy := server.NewId()
		forward := NewResidentForward(ctx, resident.exchange, healthy)
		defer forward.Close()
		resident.forwards[healthy] = forward
		var calls atomic.Int32
		resident.residentContractManager.readContract = residentContractReadForTest(func(_ context.Context, _, target server.Id) bool {
			calls.Add(1)
			return target == healthy
		})
		var witnesses [][]byte
		witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, healthy))
		synctest.Wait()
		for range 64 {
			witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, server.NewId()))
		}
		synctest.Wait()
		if got := calls.Load(); got != 4 || len(resident.residentContractManager.checkLimiters) != 4 {
			t.Fatalf("denied history exceeded owner bound: reads=%d pairs=%d", got, len(resident.residentContractManager.checkLimiters))
		}
		witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, healthy))
		synctest.Wait()
		if len(forward.send) != 2 || calls.Load() != 4 {
			t.Fatal("denied history blocked or reread an existing healthy pair")
		}
		other := newResidentCallbackLifecycleFixture(t, ctx, settings)
		var otherCalls atomic.Int32
		other.residentContractManager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool { otherCalls.Add(1); return false })
		witnesses = append(witnesses, offerResidentPacket(t, other, other.clientId, server.NewId()))
		synctest.Wait()
		if otherCalls.Load() != 1 {
			t.Fatal("one resident's denied history consumed another resident's budget")
		}
		time.Sleep(time.Second)
		witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, server.NewId()))
		synctest.Wait()
		if calls.Load() != 5 || len(resident.residentContractManager.checkLimiters) != 2 {
			t.Fatal("expired denied slots were not reclaimed beside the healthy pair")
		}
		for _, owner := range []*Resident{resident, other} {
			if err := owner.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}
		forward.Close()
		requireResidentPoolOwnersReturned(t, witnesses, "finite denied-pair history")
	})
}

// Drives the exact one-second retry boundary through the real callback worker;
// rejected offers never slide the interval or retain a future retry owner.
func TestResidentPacketDeniedPairRechecksAtExactInterval(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(server.WithoutPostgres(context.Background()))
		defer cancel()
		resident := newResidentCallbackLifecycleFixture(t, ctx, DefaultExchangeSettings())
		destination := server.NewId()
		var calls atomic.Int32
		resident.residentContractManager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool { calls.Add(1); return false })
		var witnesses [][]byte
		witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, destination))
		synctest.Wait()
		time.Sleep(time.Second - time.Nanosecond)
		witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, destination))
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("ordinary packet repeated a denied check before one second")
		}
		time.Sleep(time.Nanosecond)
		witnesses = append(witnesses, offerResidentPacket(t, resident, resident.clientId, destination))
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("refused packet moved the exact one-second admission boundary")
		}
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		requireResidentPoolOwnersReturned(t, witnesses, "exact interval callback refusal")
	})
}

// The positive refresh is a resident-owned worker. Shutdown cancels its read
// and cannot finish before that reader releases even after callbacks are idle.
func TestResidentPacketCloseJoinsPositiveRefresh(t *testing.T) {
	warmForwardDemandPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(server.WithoutPostgres(context.Background()))
		defer cancel()
		resident := newResidentCallbackLifecycleFixture(t, ctx, DefaultExchangeSettings())
		destination := server.NewId()
		forward := NewResidentForward(ctx, resident.exchange, destination)
		defer forward.Close()
		resident.forwards[destination] = forward
		var calls atomic.Int32
		started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		resident.residentContractManager.readContract = residentContractReadForTest(func(ctx context.Context, _, _ server.Id) bool {
			if calls.Add(1) == 1 {
				return true
			}
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-release
			return true
		})
		first := offerResidentPacket(t, resident, resident.clientId, destination)
		synctest.Wait()
		time.Sleep(resident.exchange.settings.ContractManagerCheckTimeout / 2)
		second := offerResidentPacket(t, resident, resident.clientId, destination)
		<-started
		synctest.Wait()
		if len(forward.send) != 2 {
			t.Fatal("fresh positive packet waited for the independent Redis refresh")
		}
		closed := make(chan error, 1)
		go func() { closed <- resident.CloseAndWait(context.Background()) }()
		<-cancelled
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("resident shutdown did not join its positive refresh")
		default:
		}
		close(release)
		if err := <-closed; err != nil {
			t.Error(err)
		}
		if len(resident.residentContractManager.activeReads) != 0 || resident.residentContractManager.HasActiveContract(resident.clientId, destination) {
			t.Fatal("closed manager retained or admitted a read")
		}
		forward.Close()
		requireResidentPoolOwnersReturned(t, [][]byte{first, second}, "joined positive refresh")
	})
}
