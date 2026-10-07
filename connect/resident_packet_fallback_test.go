package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Real callback entry with the bridge configured still has zero PostgreSQL
// acquisition for either authoritative Redis result, including known denial.
func TestResidentPacketFallbackRedisPositiveAndNegativeHaveNoPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, sample := range []struct {
			name, value string
			active      bool
		}{{name: "positive", value: "1", active: true}, {name: "negative", value: "0", active: false}} {
			t.Logf("case=%s", sample.name)
			testResidentPacketRedisProjectionWithFallback(t, sample.value, sample.active, newResidentContractFallback())
		}
	})
}

// Unknown/error evidence drops at the real callback when the exchange budget
// is full. Neither capacity refusal nor the FIFO barrier may reach PostgreSQL.
func TestResidentPacketFallbackSaturationHasNoPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, malformed := range []bool{false, true} {
			t.Logf("redis_error_case=%t", malformed)
			func() {
				fallback := newResidentContractFallback()
				for range residentContractFallbackMaxConcurrent {
					fallback.slots <- struct{}{}
				}
				defer func() {
					for range residentContractFallbackMaxConcurrent {
						<-fallback.slots
					}
				}()
				testResidentPacketRedisProjectionWithFallback(t, "", false, fallback, func(ctx context.Context, source, destination server.Id) {
					if malformed {
						server.Redis(ctx, func(client server.RedisClient) {
							server.Raise(client.RPush(ctx, residentPacketHoleKey(source, destination), "synthetic wrong type").Err())
						})
					}
				})
			}()
		}
	})
}

// Synthetic durable rows model an older writer that did not publish Redis. All
// packet-time source reads below use the real exported resumable SQL predicate.
func insertResidentFallbackContract(ctx context.Context, source, destination server.Id) server.Id {
	id := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count)
 VALUES($1,$2,$3,$4,$5,0)`, id, server.NewId(), source, server.NewId(), destination))
	})
	return id
}

// One real callback FIFO contains a whole packet cohort. A held live-forward
// consumer records acceptance without introducing another socket owner; a
// separate FIFO sentinel proves every offered packet has been processed.
func testResidentPacketSourceFallback(t testing.TB, caseName string, active bool, prepare func(context.Context, server.Id, server.Id)) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := DefaultExchangeSettings()
	settings.ResidentForwardQueueShardCount = 1
	resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
	ledger := &residentPayloadLedger{}
	resident.exchange.payloadOwnerLedger = ledger
	destination, sentinel := server.NewId(), server.NewId()
	if prepare != nil {
		prepare(ctx, resident.clientId, destination)
	}
	postgresAttempts, stopPostgresTripwire := server.DenyPostgresForTest(t)
	defer stopPostgresTripwire()
	fallback := newResidentContractFallback()
	var sourceReads atomic.Int32
	fallback.readContract = func(ctx context.Context, source, target server.Id) (model.ContractHoleStatus, time.Time, error) {
		sourceReads.Add(1)
		sourceCtx, revoke := server.PermitPostgresAcquisitionForTest(t, ctx)
		defer revoke()
		return model.ReadResumableContractLease(sourceCtx, source, target)
	}
	resident.residentContractManager = newResidentContractManagerWithFallback(resident.ctx, resident.cancel, resident.clientId, settings, fallback)
	forward := NewResidentForward(ctx, resident.exchange, destination)
	resident.forwards[destination] = forward
	barrier, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(release) })
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		forward.Close() // The test owns the held forward consumer.
	}()
	defaultRead := resident.residentContractManager.readContract
	resident.residentContractManager.readContract = func(ctx context.Context, source, target server.Id) residentContractAllowance {
		if target == sentinel {
			close(barrier)
			<-release
			return residentContractAllowance{}
		}
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
		t.Fatalf("%s: source callback stopped before completion: packet_pg_attempts=%d", caseName, server.PacketPostgresAttempts(resident.residentContractManager.ctx))
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: source callback cohort did not reach its FIFO barrier", caseName)
	}
	wantAccepted := 0
	if active {
		wantAccepted = packets
	}
	if got := len(forward.send); got != wantAccepted || sourceReads.Load() != 1 {
		t.Fatalf("%s: cohort accepted=%d source_reads=%d, want accepted=%d source_reads=1", caseName, got, sourceReads.Load(), wantAccepted)
	}
	if got := server.PacketPostgresAttempts(resident.residentContractManager.ctx); got != 0 {
		t.Fatalf("%s: bridge escaped through an ordinary packet dependency: attempts=%d", caseName, got)
	}
	releaseOnce.Do(func() { close(release) })
	if err := resident.CloseAndWait(context.Background()); err != nil {
		t.Error(err)
	}
	forward.Close()
	requireResidentPoolOwnersReturned(t, witnesses, "bounded source-fallback cohort")
	if got := postgresAttempts(); got != 1 {
		t.Errorf("%s: source callback process acquisitions=%d, want only the permitted source acquisition", caseName, got)
	}
	if snapshot := ledger.snapshot(); !snapshot.Complete || snapshot.Groups[residentPayloadForwardIngress].Messages != 0 || snapshot.Groups[residentPayloadForwardOutput].Messages != 0 || len(fallback.slots) != 0 {
		t.Fatalf("%s: source fallback retained a payload or concurrency owner", caseName)
	}
}

// Checkpoint permission and either direction survive the rollout. Final close,
// wrong pairs and settled outcomes refuse; expired Redis evidence is rechecked.
func TestResidentPacketFallbackUsesExactResumableSource(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, sample := range []struct {
			name    string
			active  bool
			prepare func(context.Context, server.Id, server.Id)
		}{
			{name: "missing", active: false, prepare: nil},
			{name: "open", active: true, prepare: func(ctx context.Context, source, destination server.Id) {
				insertResidentFallbackContract(ctx, source, destination)
			}},
			{name: "absolute_expired", active: false, prepare: func(ctx context.Context, source, destination server.Id) {
				id := insertResidentFallbackContract(ctx, source, destination)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, time.Now().UTC().Add(-time.Hour)))
				})
			}},
			{name: "absolute_live", active: true, prepare: func(ctx context.Context, source, destination server.Id) {
				id := insertResidentFallbackContract(ctx, source, destination)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, time.Now().UTC().Add(time.Hour)))
				})
			}},
			{name: "reverse", active: true, prepare: func(ctx context.Context, source, destination server.Id) {
				insertResidentFallbackContract(ctx, destination, source)
			}},
			{name: "checkpoint", active: true, prepare: func(ctx context.Context, source, destination server.Id) {
				id := insertResidentFallbackContract(ctx, source, destination)
				server.Raise(model.CloseContract(ctx, id, source, 0, true))
				// Remove the lifecycle projection to model a pre-publication writer.
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.Del(ctx, residentPacketHoleKey(source, destination)).Err())
				})
			}},
			{name: "final_close", active: false, prepare: func(ctx context.Context, source, destination server.Id) {
				id := insertResidentFallbackContract(ctx, source, destination)
				server.Raise(model.CloseContract(ctx, id, source, 0, false))
			}},
			{name: "disputed", active: false, prepare: func(ctx context.Context, source, destination server.Id) {
				id := insertResidentFallbackContract(ctx, source, destination)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, id))
				})
			}},
			{name: "settled", active: false, prepare: func(ctx context.Context, source, destination server.Id) {
				id := insertResidentFallbackContract(ctx, source, destination)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract
 SET outcome=$2, close_time=now() AT TIME ZONE 'UTC',
     provider_usage='{"version":1,"byte_count":0,"providers":[]}'::jsonb
 WHERE contract_id=$1`, id, model.ContractOutcomeSettled))
				})
			}},
			{name: "empty_close_metadata", active: true, prepare: func(ctx context.Context, source, destination server.Id) {
				id := insertResidentFallbackContract(ctx, source, destination)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'',0,false)`, id))
				})
			}},
			{name: "wrong_source", active: false, prepare: func(ctx context.Context, source, destination server.Id) {
				insertResidentFallbackContract(ctx, server.NewId(), destination)
			}},
			{name: "wrong_destination", active: false, prepare: func(ctx context.Context, source, destination server.Id) {
				insertResidentFallbackContract(ctx, source, server.NewId())
			}},
			{name: "expired_redis", active: false, prepare: func(ctx context.Context, source, destination server.Id) {
				server.Redis(ctx, func(client server.RedisClient) {
					key := residentPacketHoleKey(source, destination)
					residentPacketSeedHole(ctx, client, source, destination, "1")
					server.Raise(client.PExpireAt(ctx, key, time.Unix(1, 0)).Err())
				})
			}},
			{name: "redis_command_error", active: true, prepare: func(ctx context.Context, source, destination server.Id) {
				insertResidentFallbackContract(ctx, source, destination)
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.RPush(ctx, residentPacketHoleKey(source, destination), "synthetic wrong type").Err())
				})
			}},
		} {
			t.Logf("case=%s", sample.name)
			testResidentPacketSourceFallback(t, sample.name, sample.active, sample.prepare)
		}
	})
}

// A real source read held behind a PostgreSQL table-lock barrier must cancel
// and drain the callback cohort before that independent lock owner releases.
// The test is local-fixture only; the bridge must not inherit the model's 5 s
// source ceiling or retain the remaining 31 packets as retry work.
func TestResidentPacketFallbackBlockedSourceCancelsBeforeBarrierRelease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		locked, release := make(chan struct{}), make(chan struct{})
		finished := make(chan struct{})
		var barrierFailure any
		go func() {
			defer close(finished)
			barrierFailure = server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `LOCK TABLE transfer_contract IN ACCESS EXCLUSIVE MODE`))
					close(locked)
					select {
					case <-ctx.Done():
					case <-release:
					}
				}, server.OptNoRetry())
			})
		}()
		defer func() {
			close(release)
			<-finished
			if barrierFailure != nil {
				t.Errorf("local SQL barrier owner failed: %v", barrierFailure)
			}
		}()
		select {
		case <-locked:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("local SQL barrier did not acquire its lock")
		}
		testResidentPacketSourceFallback(t, "blocked_source", false, nil)
		select {
		case <-finished:
			t.Fatal("source barrier ended before the packet cohort drained")
		default:
		}
	})
}
