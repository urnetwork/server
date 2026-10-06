package work

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"google.golang.org/protobuf/proto"
)

// This is an offline causal control, not a Main throughput benchmark. It uses
// the production private credential/controller path, a single actual-sized
// shard grant, and distinct derived clients. No provider carrier is simulated.
type privateLoadFixture struct {
	closeOwner func()
	control    *providerEgressControl
	owner      *model.ProberShardOwner
	peer       server.Id
	tokens     []string
	clients    []server.Id
	peers      []server.Id
}

// Close joins notification workers before their TestEnv restores Redis resources.
func (self privateLoadFixture) Close() {
	if self.closeOwner != nil {
		self.closeOwner()
	}
}

func newPrivateLoadFixture(t testing.TB, ctx context.Context, index, history int) privateLoadFixture {
	return newPrivateLoadFixtureCount(t, ctx, index, history, 64)
}

func newPrivateLoadFixtureCount(t testing.TB, ctx context.Context, index, history, count int) privateLoadFixture {
	t.Helper()
	owner, err := model.BeginProberShard(ctx, model.ProberShardKey{
		TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: index, ShardCount: 8,
	}, 260*1024*model.Gib, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := model.ProberShardIdentity(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := newProviderEgressCredentials(identity)
	if err != nil {
		t.Fatal(err)
	}
	f := privateLoadFixture{owner: owner, peer: server.NewId()}
	parent := connect.Id(owner.ClientId)
	for range count {
		minted, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &parent})
		if err != nil {
			t.Fatal(err)
		}
		claims, err := jwt.ParseByJwtForAudience(ctx, minted.ByClientJwt, jwt.ByJwtAudienceApi)
		if err != nil || claims.ClientId == nil {
			t.Fatal("derived credential lacks its actual client", err)
		}
		f.tokens = append(f.tokens, minted.ByClientJwt)
		f.clients = append(f.clients, *claims.ClientId)
	}
	peerNetwork, peerUser := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, peerNetwork, fmt.Sprintf("load-control-provider-%d", index), peerUser)
	model.Testing_CreateDevice(ctx, peerNetwork, server.NewId(), f.peer, "load provider", "fixture")
	model.SetProvide(ctx, f.peer, map[model.ProvideMode][]byte{model.ProvideModePublic: bytes.Repeat([]byte{29}, 32)})
	if history > 0 {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
				SELECT md5($5::uuid::text||'-loaded-history-'||n)::uuid,$1,$2,$3,$4,$1,1
				FROM generate_series(1,$6)n`, owner.NetworkId, f.clients[0], peerNetwork, f.peer, owner.BalanceId, history))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
				SELECT md5($1::uuid::text||'-loaded-history-'||n)::uuid,$1,1 FROM generate_series(1,$2)n`, owner.BalanceId, history))
		})
	}
	if history > 0 {
		// Synthetic legacy SQL history needs the same targeted mirror publication as legacy production.
		// This is fixture preparation, outside the measured native admission workload.
		_, count := model.ReconcileNetEscrowForNetwork(ctx, owner.NetworkId, true)
		if count != 1 {
			t.Fatal("synthetic legacy history did not resolve its one grant")
		}
	}
	notifications := model.NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings())
	f.control, f.closeOwner, err = privateLoadFinishControl(func() (*providerEgressControl, error) { return newProviderEgressControl(credentials, notifications) }, notifications.Close)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

type privateLoadProtocolRefusal struct {
	reason       protocol.ContractError
	requestError error
}

func (self *privateLoadProtocolRefusal) Error() string { return "bounded contract protocol refusal" }

func privateLoadCall(ctx context.Context, f privateLoadFixture, token string, message proto.Message) (*protocol.StoredContract, error) {
	frame, err := connect.ToFrame(message, connect.DefaultProtocolVersion)
	if err != nil {
		return nil, err
	}
	defer connect.MessagePoolReturn(frame.MessageBytes)
	pack, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
	if err != nil {
		return nil, err
	}
	result, err := f.control.ConnectControl(ctx, token, &connect.ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(pack)})
	if err != nil {
		return nil, err
	}
	if result == nil || result.Error != nil {
		return nil, errors.New("controller returned an error result")
	}
	data, err := base64.StdEncoding.DecodeString(result.Pack)
	if err != nil {
		return nil, err
	}
	response := &protocol.Pack{}
	if err := proto.Unmarshal(data, response); err != nil {
		return nil, err
	}
	if len(response.Frames) == 0 {
		return nil, nil
	}
	if len(response.Frames) != 1 {
		return nil, errors.New("controller returned a non-single response")
	}
	decoded, err := connect.FromFrame(response.Frames[0])
	if err != nil {
		return nil, err
	}
	created, ok := decoded.(*protocol.CreateContractResult)
	if !ok {
		return nil, errors.New("controller returned a non-contract response")
	}
	if created.Error != nil {
		return nil, &privateLoadProtocolRefusal{reason: *created.Error, requestError: ctx.Err()}
	}
	if created.Contract == nil {
		return nil, errors.New("controller returned no contract")
	}
	stored := &protocol.StoredContract{}
	if err := proto.Unmarshal(created.Contract.StoredContractBytes, stored); err != nil {
		return nil, err
	}
	return stored, nil
}

// Missing finite counter outcomes are valid zero; the pool must be opened and
// its configured maximum independently observed before using any timing data.
func privateLoadMetric(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matched := 0
			for _, label := range metric.Label {
				if value, ok := labels[label.GetName()]; ok && value == label.GetValue() {
					matched++
				}
			}
			if matched == len(labels) {
				if metric.Counter != nil {
					return metric.GetCounter().GetValue()
				}
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func privateLoadStats(t testing.TB) map[string]float64 {
	t.Helper()
	values := map[string]float64{}
	for _, name := range []string{"admission", "creation", "settlement", "refresh"} {
		for _, result := range []string{"reused", "reloaded"} {
			values[name+"_"+result] = privateLoadMetric(t, "urnetwork_net_escrow_"+name+"_snapshot_total", map[string]string{"result": result})
		}
	}
	for _, operation := range []string{"reserve", "reserve-owned", "reserve-owned-full"} {
		values["native_reserved"] += privateLoadMetric(t, "urnetwork_redis_contract_reservation_total", map[string]string{"operation": operation, "result": "accepted"})
	}
	for _, result := range []string{"selected_first", "fallback", "error"} {
		values[result] = privateLoadMetric(t, "urnetwork_prober_grant_selection_total", map[string]string{"result": result})
	}
	values["pool_acquires"] = privateLoadMetric(t, "urnetwork_pg_pool_acquires_total", map[string]string{"pool": "default", "outcome": "acquired"})
	values["pool_wait_seconds"] = privateLoadMetric(t, "urnetwork_pg_pool_acquire_duration_seconds_total", map[string]string{"pool": "default"})
	return values
}

func privateLoadAssertAccounting(t testing.TB, ctx context.Context, f privateLoadFixture, want model.ByteCount) {
	t.Helper()
	state, err := privateLoadReadAccounting(ctx, f)
	if err != nil {
		t.Fatal("accounting source read failed", err)
	}
	if err := state.validate(); err != nil {
		t.Fatal(err)
	}
	if state.exact != want {
		t.Fatalf("financial state mismatch: exact=%d expected=%d", state.exact, want)
	}
}

func TestPrivateProviderLoadedSelectedGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
		defer cancel()
		for arm, settings := range []struct{ pool, history int }{{1, 0}, {1, 10001}, {16, 0}, {16, 10001}} {
			func() {
				f := newPrivateLoadFixture(t, ctx, arm, settings.history)
				defer f.Close()
				pop := server.Config.PushSimpleResource("db.yml", []byte(fmt.Sprintf("min_connections: 0\nmax_connections: %d\n", settings.pool)))
				server.PgReset()
				defer func() { pop(); server.PgReset() }()
				before := privateLoadStats(t)
				var allocated int64
				var created []*protocol.StoredContract
				for _, workers := range []int{1, 8, 64} {
					start := make(chan struct{})
					durations := make([]time.Duration, workers)
					contracts := make([]*protocol.StoredContract, workers)
					errs := make([]error, workers)
					var wg sync.WaitGroup
					for index := range workers {
						wg.Add(1)
						go func() {
							defer wg.Done()
							<-start
							requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
							defer requestCancel()
							began := time.Now()
							contracts[index], errs[index] = privateLoadCall(requestCtx, f, f.tokens[index], &protocol.CreateContract{
								DestinationId: f.peer.Bytes(), TransferByteCount: uint64(connect.DefaultContractManagerSettings().StandardContractTransferByteCount),
							})
							durations[index] = time.Since(began)
						}()
					}
					began := time.Now()
					close(start)
					wg.Wait()
					elapsed := time.Since(began)
					for index, err := range errs {
						if err != nil || contracts[index] == nil {
							t.Fatalf("pool=%d history=%d workers=%d finite create failed: %v", settings.pool, settings.history, workers, err)
						}
						allocated += int64(contracts[index].TransferByteCount)
					}
					created = contracts // Final wave has exactly one per token.
					slices.Sort(durations)
					t.Logf("pool=%d history=%d workers=%d succeeded=%d total=%s p50=%s p95=%s max=%s", settings.pool, settings.history, workers, workers, elapsed, durations[(workers-1)/2], durations[(workers-1)*95/100], durations[workers-1])
				}
				after := privateLoadStats(t)
				for key := range after {
					after[key] -= before[key]
				}
				if got := privateLoadMetric(t, "urnetwork_pg_pool_connections", map[string]string{"pool": "default", "state": "maximum"}); got != float64(settings.pool) {
					t.Fatalf("observed pool maximum=%v, expected%d", got, settings.pool)
				}
				privateLoadAssertNativeAdmission(t, ctx, f, after, 73)
				want := model.ByteCount(int64(settings.history) + allocated)
				privateLoadAssertAccounting(t, ctx, f, want)
				t.Logf("pool=%d history=%d create_counters=%v exact_reserved=%d", settings.pool, settings.history, after, want)
				// Create another wave while requester closes and independent provider
				// acknowledgments compete for the same balance and pool connections.
				start := make(chan struct{})
				results := make(chan error, len(created))
				newContracts := make(chan *protocol.StoredContract, len(created))
				createErrors := make(chan error, len(created))
				before = privateLoadStats(t)
				for index, stored := range created {
					go func() {
						<-start
						requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
						defer requestCancel()
						contract, err := privateLoadCall(requestCtx, f, f.tokens[index], &protocol.CreateContract{
							DestinationId: f.peer.Bytes(), TransferByteCount: uint64(connect.DefaultContractManagerSettings().StandardContractTransferByteCount),
						})
						newContracts <- contract
						createErrors <- err
					}()
					go func() {
						<-start
						closeCtx, closeCancel := context.WithTimeout(ctx, 10*time.Second)
						defer closeCancel()
						if _, err := privateLoadCall(closeCtx, f, f.tokens[index], &protocol.CloseContract{ContractId: stored.ContractId}); err != nil {
							results <- err
							return
						}
						id, err := server.IdFromBytes(stored.ContractId)
						if err == nil {
							err = model.CloseContract(closeCtx, id, f.peer, 0, false)
						}
						results <- err
					}()
				}
				began := time.Now()
				close(start)
				settleErr := privateLoadJoinResults(len(created), results)
				var createErr error
				for range created {
					contract, err := <-newContracts, <-createErrors
					if err != nil {
						createErr = errors.Join(createErr, err)
					} else if contract == nil {
						createErr = errors.Join(createErr, errors.New("creation returned no contract"))
					} else {
						want += model.ByteCount(contract.TransferByteCount)
					}
				}
				if err := errors.Join(settleErr, createErr); err != nil {
					t.Fatal("overlapping owners failed after joining", err)
				}
				after = privateLoadStats(t)
				for key := range after {
					after[key] -= before[key]
				}
				for _, stored := range created {
					want -= model.ByteCount(stored.TransferByteCount)
				}
				privateLoadAssertAccounting(t, ctx, f, want)
				t.Logf("pool=%d history=%d settled=%d concurrent_created=%d elapsed=%s combined_counters=%v exact_reserved=%d", settings.pool, settings.history, len(created), len(created), time.Since(began), after, want)
			}()
		}
	})
}
