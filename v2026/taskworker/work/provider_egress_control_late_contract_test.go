package work

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"google.golang.org/protobuf/proto"
)

type privateLateContractFixture struct {
	control *providerEgressControl
	grant   server.Id
	peer    server.Id
	token   string
}

func newPrivateLateContractFixture(t testing.TB, ctx context.Context) privateLateContractFixture {
	t.Helper()
	return newPrivateLateContractFixtureWithCredit(t, ctx, 1024*1024*1024)
}

func newPrivateLateContractFixtureWithCredit(t testing.TB, ctx context.Context, credit model.ByteCount) privateLateContractFixture {
	t.Helper()
	shard, err := model.BeginProberShard(ctx, model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: 0, ShardCount: 8}, credit, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := model.ProberShardIdentity(ctx, shard)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := newProviderEgressCredentials(identity)
	if err != nil {
		t.Fatal(err)
	}
	source := connect.Id(shard.ClientId)
	minted, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &source})
	if err != nil {
		t.Fatal(err)
	}
	peer, network, user := server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, network, "late-contract-provider", user)
	model.Testing_CreateDevice(ctx, network, server.NewId(), peer, "provider", "fixture")
	model.SetProvide(ctx, peer, map[model.ProvideMode][]byte{model.ProvideModePublic: bytes.Repeat([]byte{17}, 32)})
	notifications := model.NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings())
	t.Cleanup(notifications.Close)
	local, err := newProviderEgressControl(credentials, notifications)
	if err != nil {
		t.Fatal(err)
	}
	return privateLateContractFixture{control: local, grant: shard.BalanceId, peer: peer, token: minted.ByClientJwt}
}

func privateLateContractStrategy(ctx context.Context) *connect.ClientStrategy {
	settings := connect.DefaultClientStrategySettings()
	settings.EnableNormal, settings.EnableResilient = false, false
	settings.AltUrl, settings.ExtenderDirectory, settings.InternalDohDomains = "", nil, nil
	settings.RequestTimeout = 5 * time.Second
	return connect.NewClientStrategy(ctx, settings)
}

func privateLateStoredContract(result *connect.ConnectControlResult) (*protocol.StoredContract, error) {
	if result == nil || result.Error != nil {
		return nil, errors.New("controller did not return a successful pack")
	}
	data, err := connect.DecodeBase64(base64.StdEncoding, result.Pack)
	if err != nil {
		return nil, err
	}
	defer connect.MessagePoolReturn(data)
	pack := &protocol.Pack{}
	if err := proto.Unmarshal(data, pack); err != nil {
		return nil, err
	}
	if len(pack.Frames) == 0 {
		return nil, nil // processed close has no response frame
	}
	if len(pack.Frames) != 1 {
		return nil, errors.New("expected one contract response")
	}
	message, err := connect.FromFrame(pack.Frames[0])
	if err != nil {
		return nil, err
	}
	created, ok := message.(*protocol.CreateContractResult)
	if !ok || created.Error != nil || created.Contract == nil {
		return nil, errors.New("controller did not allocate a contract")
	}
	stored := &protocol.StoredContract{}
	if err := proto.Unmarshal(created.Contract.StoredContractBytes, stored); err != nil {
		return nil, err
	}
	return stored, nil
}

func privateLateOneConnection(t testing.TB) func() {
	t.Helper()
	pop := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
	server.PgReset()
	return func() { pop(); server.PgReset() }
}

func privateLateAssertPool(t testing.TB) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_pg_pool_connections" {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["pool"] == "default" && labels["state"] == "maximum" {
				if metric.GetGauge().GetValue() != 1 {
					t.Fatal("control did not use the one-connection pool")
				}
				return
			}
		}
	}
	t.Fatal("actual pool maximum is unavailable")
}

// The real private controller durably allocates, then a deterministic barrier
// delivers its known result only after cancellation and closed OOB admission.
// Cleanup must join through the same authenticated controller, write only the
// requester's zero-use close, and leave provider accounting authoritative.
func TestPrivateProviderLateCommittedContractClosesRequesterOnly(t *testing.T) {
	privateProviderLateCommittedContractClosesRequesterOnly(t, 1024*1024*1024, false)
}

// The configured URL-probe grant also qualifies for indexed selected-first
// allocation. Counter evidence prevents a large-credit fixture from silently
// exercising the ordinary fallback tested above.
func TestPrivateProviderLateCommittedSelectedGrantClosesRequesterOnly(t *testing.T) {
	args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("late-contract.example"), 0)
	credit, err := providerUrlProbeShardCredit(args)
	if err != nil {
		t.Fatal(err)
	}
	privateProviderLateCommittedContractClosesRequesterOnly(t, credit, true)
}

func privateProviderLateCommittedContractClosesRequesterOnly(t *testing.T, credit model.ByteCount, selected bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newPrivateLateContractFixtureWithCredit(t, ctx, credit)
		restore := privateLateOneConnection(t)
		defer restore()
		selectedBefore := urlFundingSelectedGrants(t)
		ownerCtx, cancelOwner := context.WithCancel(ctx)
		defer cancelOwner()
		strategy := privateLateContractStrategy(ctx)
		defer strategy.Close()
		committed := make(chan *protocol.StoredContract, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		var calls atomic.Int64
		local := privatePassControl(func(callCtx context.Context, token string, args *connect.ConnectControlArgs) (*connect.ConnectControlResult, error) {
			calls.Add(1)
			result, err := fixture.control.ConnectControl(callCtx, token, args)
			if err != nil {
				return result, err
			}
			stored, err := privateLateStoredContract(result)
			if err != nil {
				return nil, err
			}
			if stored != nil {
				committed <- stored
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return result, nil
		})
		owner := connect.NewApiOutOfBandControlWithLocalControl(ownerCtx, strategy, fixture.token, "https://no-network.invalid", local)
		defer owner.Close()
		frame, err := connect.ToFrame(&protocol.CreateContract{DestinationId: fixture.peer.Bytes(), TransferByteCount: 4096}, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan privateOobReply, 1)
		owner.SendControl([]*protocol.Frame{frame}, func(frames []*protocol.Frame, err error) {
			if len(frames) != 0 {
				err = errors.New("canceled result became usable")
			}
			done <- privateOobReply{err: err}
		})
		var stored *protocol.StoredContract
		select {
		case stored = <-committed:
		case reply := <-done:
			t.Fatal("create did not reach its committed barrier", reply.err)
		case <-ctx.Done():
			t.Fatal("create could not complete through one database connection")
		}
		selectedWant := float64(0)
		if selected {
			selectedWant = 1
		}
		if got := urlFundingSelectedGrants(t) - selectedBefore; got != selectedWant {
			t.Fatalf("unexpected selected-first allocation count: got=%v want=%v", got, selectedWant)
		}
		contract, err := server.IdFromBytes(stored.ContractId)
		if err != nil || stored.TransferByteCount <= 0 {
			t.Fatal("committed contract identity or reservation missing", err)
		}
		if got := model.Testing_NetEscrowByteCount(ctx, fixture.grant); got != model.ByteCount(stored.TransferByteCount) {
			t.Fatal("commit did not publish its exact reservation")
		}
		cancelOwner()
		owner.Close()
		unblock()
		if err := owner.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
		if reply := <-done; !errors.Is(reply.err, context.Canceled) {
			t.Fatal("cancellation was not retained", reply.err)
		}
		var source, destination int
		var sourceUsed model.ByteCount
		var checkpoint bool
		var outcome *string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM contract_close WHERE contract_id=$1 AND party='source'),
				(SELECT count(*) FROM contract_close WHERE contract_id=$1 AND party='destination'),
				COALESCE((SELECT used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party='source'),-1),
				COALESCE((SELECT checkpoint FROM contract_close WHERE contract_id=$1 AND party='source'),true),
				outcome FROM transfer_contract WHERE contract_id=$1`, contract).Scan(&source, &destination, &sourceUsed, &checkpoint, &outcome))
		})
		if calls.Load() != 2 || source != 1 || destination != 0 || sourceUsed != 0 || checkpoint || outcome != nil {
			t.Fatalf("late cleanup lost requester-only accounting: calls=%d source=%d destination=%d source_used=%d checkpoint=%t terminal=%t", calls.Load(), source, destination, sourceUsed, checkpoint, outcome != nil)
		}
		if got := model.Testing_NetEscrowByteCount(ctx, fixture.grant); got != model.ByteCount(stored.TransferByteCount) {
			t.Fatal("requester cleanup prematurely released provider reservation")
		}
		if err := model.CloseContract(ctx, contract, fixture.peer, 0, false); err != nil {
			t.Fatal(err)
		}
		if closed, ok := model.GetContractClose(ctx, contract); !ok || closed.Outcome != model.ContractOutcomeSettled {
			t.Fatal("independent provider close did not use normal settlement")
		}
		if got := model.Testing_NetEscrowByteCount(ctx, fixture.grant); got != 0 {
			t.Fatal("normal two-party settlement retained the reservation")
		}
		privateLateAssertPool(t)
	})
}

// Eight simultaneous real private allocations share one payer and one pool
// connection. A helper that acquires a second connection inside an owning
// transaction cannot pass this control. It does not model Main query costs.
func TestPrivateProviderContractLoadedSingleConnection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newPrivateLateContractFixture(t, ctx)
		restore := privateLateOneConnection(t)
		defer restore()
		strategy := privateLateContractStrategy(ctx)
		defer strategy.Close()
		owner := connect.NewApiOutOfBandControlWithLocalControl(ctx, strategy, fixture.token, "https://no-network.invalid", fixture.control)
		defer owner.CloseAndWait(ctx)
		start := make(chan struct{})
		done := make(chan error, 8)
		for range 8 {
			go func() {
				<-start
				reply := privateOobSend(ctx, owner, &protocol.CreateContract{DestinationId: fixture.peer.Bytes(), TransferByteCount: 4096})
				if reply.err != nil || len(reply.messages) != 1 {
					done <- fmt.Errorf("concurrent create failed: %w", reply.err)
					return
				}
				created, ok := reply.messages[0].(*protocol.CreateContractResult)
				if !ok || created.Error != nil || created.Contract == nil {
					done <- errors.New("concurrent allocation rejected")
					return
				}
				stored := &protocol.StoredContract{}
				if err := proto.Unmarshal(created.Contract.StoredContractBytes, stored); err != nil {
					done <- err
					return
				}
				contract, err := server.IdFromBytes(stored.ContractId)
				if err != nil {
					done <- err
					return
				}
				reply = privateOobSend(ctx, owner, &protocol.CloseContract{ContractId: stored.ContractId})
				if reply.err != nil {
					done <- reply.err
					return
				}
				done <- model.CloseContract(ctx, contract, fixture.peer, 0, false)
			}()
		}
		close(start)
		for range 8 {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
		if err := owner.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
		privateLateAssertPool(t)
		var total, settled int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE outcome='settled') FROM transfer_contract WHERE destination_id=$1`, fixture.peer).Scan(&total, &settled))
		})
		if total != 8 || settled != 8 || model.Testing_NetEscrowByteCount(ctx, fixture.grant) != 0 {
			t.Fatal("concurrent one-connection lifecycles did not settle exactly eight contracts")
		}
	})
}
