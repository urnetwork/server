package controller

// §6.10.1's Service contracts through CreateContract on the private stack (PROTO-M5-04; C9, D-PROTO-9, D-PROTO-15,
// I-12): an urnetwork-edge client and a sidecar get Service only while a Find's pair lives, in both directions and
// for an edge's companion, ending at the earlier of the grant's expiry and the hour; a missing or ended pair, a
// sidecar holding no registration and two edge ends get nothing, and never Network (§22.5's §8 row); the decision
// takes one Redis read and no PostgreSQL acquisition; and a Service contract's settlement debits no balance. The pair
// is written as SOCK's Find writes it (model.SocketStore.SetServicePair).

import (
	"bytes"
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The edge network and a sidecar's network, with balances, and sidecar.yml naming the edge network until the test
// ends.
type serviceTestNets struct {
	t                testing.TB
	ctx              context.Context
	edgeNetworkId    server.Id
	sidecarNetworkId server.Id
}

func newServiceTestNets(t testing.TB, ctx context.Context) *serviceTestNets {
	t.Helper()
	self := &serviceTestNets{
		t:                t,
		ctx:              ctx,
		edgeNetworkId:    server.NewId(),
		sidecarNetworkId: server.NewId(),
	}
	for _, networkId := range []server.Id{self.edgeNetworkId, self.sidecarNetworkId} {
		model.Testing_CreateNetwork(ctx, networkId, "service-"+networkId.String(), server.NewId())
		server.Raise(model.AddBasicTransferBalance(ctx, networkId, 256*1024*1024, server.NowUtc(), server.NowUtc().Add(time.Hour)))
	}
	t.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  urnetwork_edge: " + self.edgeNetworkId.String() + "\n"))
	return self
}

// A client of networkId providing provideModes.
func (self *serviceTestNets) client(networkId server.Id, provideModes ...model.ProvideMode) server.Id {
	self.t.Helper()
	clientId := server.NewId()
	model.Testing_CreateDevice(self.ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
	secretKeys := map[model.ProvideMode][]byte{}
	for _, provideMode := range provideModes {
		secretKeys[provideMode] = bytes.Repeat([]byte{byte(provideMode) + 11}, 32)
	}
	model.SetProvide(self.ctx, clientId, secretKeys)
	return clientId
}

// Records the pair as an edge's Find does: field dialer of {svc_<listener>}, holding the grant's expiry.
func (self *serviceTestNets) pair(listenerId server.Id, dialerId server.Id, grantExpire time.Time) {
	self.t.Helper()
	wrote, _, err := model.NewSocketStore(nil, nil).SetServicePair(self.ctx, connect.Id(listenerId), connect.Id(dialerId), uint64(grantExpire.UnixMilli()), 15*time.Minute, 1)
	if err != nil || !wrote {
		self.t.Fatalf("pair: wrote %t, %v", wrote, err)
	}
}

// CreateContract's result for request from source.
func serviceTestCreate(t testing.TB, ctx context.Context, source server.Id, destination server.Id, companion bool) *protocol.CreateContractResult {
	t.Helper()
	frames, err := CreateContract(ctx, source, &protocol.CreateContract{
		DestinationId:     destination.Bytes(),
		TransferByteCount: 1024 * 1024,
		Companion:         companion,
	}, connect.DefaultContractManagerSettings())
	if err != nil || len(frames) != 1 {
		t.Fatalf("create contract: %d frames, %v", len(frames), err)
	}
	message, err := connect.FromFrame(frames[0])
	returnConnectControlFrames(frames)
	if err != nil {
		t.Fatal(err)
	}
	return message.(*protocol.CreateContractResult)
}

// The Service contract result holds, its stored bytes parsed; fails t on any other result.
func serviceTestIssued(t testing.TB, name string, result *protocol.CreateContractResult) *protocol.StoredContract {
	t.Helper()
	if result.Error != nil || result.Contract == nil {
		t.Fatalf("%s: no contract: %+v", name, result)
	}
	if result.Contract.ProvideMode != protocol.ProvideMode_Service {
		t.Fatalf("%s: mode %s, want Service", name, result.Contract.ProvideMode)
	}
	storedContract := &protocol.StoredContract{}
	if err := proto.Unmarshal(result.Contract.StoredContractBytes, storedContract); err != nil {
		t.Fatal(err)
	}
	return storedContract
}

// Whether result is the NoPermission refusal.
func serviceTestRefused(result *protocol.CreateContractResult) bool {
	return result.Contract == nil && result.Error != nil && *result.Error == protocol.ContractError_NoPermission
}

// An edge and a sidecar holding a registration get Service both ways while the pair lives, an edge's companion
// settling as Service rather than Stream, each sealed with its own source's network; none before the pair.
func TestCreateContractServicePair(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		nets := newServiceTestNets(t, ctx)
		edge := nets.client(nets.edgeNetworkId, model.ProvideModeService)
		sidecarClient := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModeService)

		before := rejectionCount(t, "internal", "service_no_pair", "false")
		for _, pair := range [][2]server.Id{{edge, sidecarClient}, {sidecarClient, edge}} {
			if result := serviceTestCreate(t, ctx, pair[0], pair[1], false); !serviceTestRefused(result) {
				t.Fatalf("%s->%s with no pair: %+v", pair[0], pair[1], result)
			}
		}
		connect.AssertEqual(t, rejectionCount(t, "internal", "service_no_pair", "false"), before+2)

		nets.pair(sidecarClient, edge, server.NowUtc().Add(2*time.Hour))
		forward := serviceTestIssued(t, "the edge to the sidecar", serviceTestCreate(t, ctx, edge, sidecarClient, false))
		connect.AssertEqual(t, forward.SourceNetworkId, nets.edgeNetworkId.Bytes())
		connect.AssertEqual(t, forward.ContractClass, protocol.ContractClass_ContractClassData)
		reply := serviceTestIssued(t, "the sidecar to the edge", serviceTestCreate(t, ctx, sidecarClient, edge, false))
		connect.AssertEqual(t, reply.SourceNetworkId, nets.sidecarNetworkId.Bytes())
		// an edge's reply to the sidecar's contract is Service, with the forward contract's seals
		companion := serviceTestIssued(t, "the edge's companion", serviceTestCreate(t, ctx, edge, sidecarClient, true))
		connect.AssertEqual(t, companion.SourceNetworkId, nets.sidecarNetworkId.Bytes())

		// the grant is two hours off, so the contract's own hour ends it
		for name, storedContract := range map[string]*protocol.StoredContract{"forward": forward, "reply": reply, "companion": companion} {
			life := time.UnixMilli(*storedContract.ExpirationTimeUnixMilli).Sub(server.NowUtc())
			if life <= model.DefaultContractExpiration-time.Minute || model.DefaultContractExpiration < life {
				t.Errorf("%s: ends in %v, want the hour", name, life)
			}
		}
	})
}

// A grant that ends inside the hour ends the contract with it, and an ended grant gets none.
func TestCreateContractServiceDeadline(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		nets := newServiceTestNets(t, ctx)
		edge := nets.client(nets.edgeNetworkId, model.ProvideModeService)
		soon := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModeService)
		ended := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModeService)

		grantExpire := server.NowUtc().Add(10 * time.Minute).Truncate(time.Millisecond)
		nets.pair(soon, edge, grantExpire)
		storedContract := serviceTestIssued(t, "a grant ending in 10 min", serviceTestCreate(t, ctx, edge, soon, false))
		expiration := time.UnixMilli(*storedContract.ExpirationTimeUnixMilli)
		if grantExpire.Before(expiration) || expiration.Before(grantExpire.Add(-5*time.Second)) {
			t.Errorf("ends at %v, want the grant's %v", expiration, grantExpire)
		}

		nets.pair(ended, edge, server.NowUtc().Add(-time.Second))
		for _, pair := range [][2]server.Id{{edge, ended}, {ended, edge}} {
			if result := serviceTestCreate(t, ctx, pair[0], pair[1], false); !serviceTestRefused(result) {
				t.Errorf("%s->%s past the grant: %+v", pair[0], pair[1], result)
			}
		}
	})
}

// §22.5's §8 row, the edge half: no Network-mode edge contract. A sidecar that provides Network and Public but no
// Service, or holds no pair, gets nothing from an edge, never Network or Public; two clients of the edge network get
// nothing, though they share a network.
func TestCreateContractServiceNeverNetwork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		nets := newServiceTestNets(t, ctx)
		edge := nets.client(nets.edgeNetworkId, model.ProvideModeService, model.ProvideModeNetwork)
		otherEdge := nets.client(nets.edgeNetworkId, model.ProvideModeService, model.ProvideModeNetwork)
		unregistered := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModePublic, model.ProvideModeStream)
		unpaired := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork, model.ProvideModePublic, model.ProvideModeService)

		// a pair, but the sidecar advertises no Service: its key is missing, so nothing is signed
		nets.pair(unregistered, edge, server.NowUtc().Add(time.Hour))
		for _, pair := range [][2]server.Id{{edge, unregistered}, {edge, unpaired}, {unpaired, edge}, {edge, otherEdge}, {otherEdge, edge}} {
			for _, companion := range []bool{false, true} {
				result := serviceTestCreate(t, ctx, pair[0], pair[1], companion)
				if !serviceTestRefused(result) {
					t.Errorf("%s->%s companion %t: %+v", pair[0], pair[1], companion, result)
				}
			}
		}
	})
}

// The decision holds I-12's line: one Redis read of the pair and no PostgreSQL acquisition, with the relationship's
// facts already read; a contract with no edge end reads nothing.
func TestServiceContractDecisionReadsOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		nets := newServiceTestNets(t, ctx)
		edge := nets.client(nets.edgeNetworkId, model.ProvideModeService)
		sidecarClient := nets.client(nets.sidecarNetworkId, model.ProvideModeService)
		peer := nets.client(nets.sidecarNetworkId, model.ProvideModeNetwork)
		nets.pair(sidecarClient, edge, server.NowUtc().Add(time.Hour))
		edgeDetails := model.GetProvideRelationshipDetails(ctx, edge, sidecarClient)
		peerDetails := model.GetProvideRelationshipDetails(ctx, sidecarClient, peer)
		config := server.CurrentSidecarConfig()

		reads := 0
		read := getSocketServicePair
		getSocketServicePair = func(ctx context.Context, listenerClientId connect.Id, dialerClientId connect.Id) (uint64, bool, error) {
			reads += 1
			return read(ctx, listenerClientId, dialerClientId)
		}
		defer func() {
			getSocketServicePair = read
		}()

		attempts, stop := server.DenyPostgresForTest(t)
		forward := decideServiceContract(ctx, edge, sidecarClient, &edgeDetails, config, server.NowUtc())
		forwardReads := reads
		none := decideServiceContract(ctx, sidecarClient, peer, &peerDetails, config, server.NowUtc())
		stop()
		connect.AssertEqual(t, attempts(), uint64(0))
		connect.AssertEqual(t, forward.outcome, contractServiceIssue)
		connect.AssertEqual(t, forwardReads, 1)
		connect.AssertEqual(t, none.outcome, contractServiceNone)
		connect.AssertEqual(t, reads, 1)
	})
}

// A Service contract has no escrow or payout: creating and settling one debits neither network (§20's leases bill
// the bytes).
func TestCreateContractServiceSettlesNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		nets := newServiceTestNets(t, ctx)
		edge := nets.client(nets.edgeNetworkId, model.ProvideModeService)
		sidecarClient := nets.client(nets.sidecarNetworkId, model.ProvideModeService)
		nets.pair(sidecarClient, edge, server.NowUtc().Add(time.Hour))
		balances := map[server.Id]model.ByteCount{}
		for _, networkId := range []server.Id{nets.edgeNetworkId, nets.sidecarNetworkId} {
			balances[networkId] = model.GetActiveTransferBalanceByteCount(ctx, networkId)
		}

		storedContract := serviceTestIssued(t, "the edge to the sidecar", serviceTestCreate(t, ctx, edge, sidecarClient, false))
		contractId := server.RequireIdFromBytes(storedContract.ContractId)
		for _, networkId := range []server.Id{nets.edgeNetworkId, nets.sidecarNetworkId} {
			connect.AssertEqual(t, model.GetOpenTransferByteCount(ctx, networkId), model.ByteCount(0))
		}
		server.Raise(model.CloseContract(ctx, contractId, edge, 64*1024, false))
		server.Raise(model.CloseContract(ctx, contractId, sidecarClient, 64*1024, false))
		for networkId, before := range balances {
			connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), before)
			connect.AssertEqual(t, model.GetOpenTransferByteCount(ctx, networkId), model.ByteCount(0))
		}
	})
}

// A grant's deadline is the earlier of its expiry and the hour; 0 is no grant expiry, and an ended grant none.
func TestServiceContractDeadline(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000).UTC()
	deadline, live := model.ServiceContractDeadline(now, 0)
	connect.AssertEqual(t, live, true)
	connect.AssertEqual(t, deadline, now.Add(model.DefaultContractExpiration))
	deadline, live = model.ServiceContractDeadline(now, uint64(now.Add(2*time.Hour).UnixMilli()))
	connect.AssertEqual(t, live, true)
	connect.AssertEqual(t, deadline, now.Add(model.DefaultContractExpiration))
	deadline, live = model.ServiceContractDeadline(now, uint64(now.Add(10*time.Minute).UnixMilli()))
	connect.AssertEqual(t, live, true)
	connect.AssertEqual(t, deadline, now.Add(10*time.Minute))
	_, live = model.ServiceContractDeadline(now, uint64(now.UnixMilli()))
	connect.AssertEqual(t, live, false)
}
