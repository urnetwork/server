// Prospective payout input and prior exclusions are verified using real SDK
// signatures, independently signed rosters, immutable blob publication and SQL.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/urfoundation/sn/v2026/payoutartifact"
	snprotocol "github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/startifact"
	"google.golang.org/protobuf/proto"
)

// Only chain binding reads are stubbed; original provider identity admission,
// wallet/reliability queries, artifact signing and publication remain real.
type providerWorkRosterClient struct {
	StClient
	clients [][16]byte
}

// A positional zero-exposure row must reach the actual binding owner too.
func (self *providerWorkRosterClient) BindingsAt(ctx context.Context, clients [][16]byte, epoch, start, end uint64) ([]*StFleetBindingState, error) {
	self.clients = append([][16]byte(nil), clients...)
	result := make([]*StFleetBindingState, len(clients))
	for index := range result {
		result[index] = &StFleetBindingState{}
	}
	return result, ctx.Err()
}

// Two independently listed SDK identities make idle provider rows observable.
func providerWorkRosterFixture(t testing.TB, f *providerWorkWindowFixture) []ed25519.PrivateKey {
	t.Helper()
	keys := []ed25519.PrivateKey{ed25519.NewKeyFromSeed(bytes.Repeat([]byte{141}, 32)), ed25519.NewKeyFromSeed(bytes.Repeat([]byte{142}, 32))}
	for index, key := range keys {
		owner := payoutartifact.WholeWorkOwner{ClientId: [16]byte{byte(index + 1)}, NetworkId: [16]byte{byte(index + 11)}, Generation: [16]byte{byte(index + 21)}, PublicKey: [32]byte(key.Public().(ed25519.PublicKey))}
		f.authority.Owners = append(f.authority.Owners, owner)
		provider := providerWorkRetainFixtureWallet(t, f.cfg, f.authority.Domain, f.epoch.Epoch, f.epoch.Start.Block, f.epoch.StartTime, owner.ClientId, owner.NetworkId)
		f.authority.ExpectedProviders = append(f.authority.ExpectedProviders, provider)
	}
	return keys
}

// Both endpoint requests are issued after their original committed clock;
// optional source records represent an actual retained canceled reservation.
func providerWorkRetainRosterCuts(t testing.TB, f *providerWorkWindowFixture, keys []ed25519.PrivateKey, record *protocol.OriginalWorkContract, previous bool) {
	t.Helper()
	for index, owner := range f.authority.Owners {
		for side, boundary := range []payoutartifact.Boundary{f.authority.Start, f.authority.End} {
			kind, clock := "start", f.epoch.StartTime
			if side == 1 {
				kind, clock = "end", f.epoch.EndTime
			}
			cut := protocol.OriginalWorkCut{DomainHash: f.domain, ClientId: owner.ClientId, Generation: owner.Generation, Epoch: f.epoch.Epoch, Block: boundary.Number, BlockHash: [32]byte(common.HexToHash(boundary.Hash)), Complete: true, Contracts: []protocol.OriginalWorkContract{}}
			if record != nil && index == 0 && (side == 1 || previous) {
				cut.Contracts = append(cut.Contracts, *record)
				cut.Revision = 1
			}
			cut, err := protocol.SignOriginalWorkCut(t.Context(), cut, keys[index])
			if err != nil {
				t.Fatal(err)
			}
			request, err := protocol.SignOriginalWorkRequest(protocol.OriginalWorkRequest{RequestId: [16]byte(server.NewId()), DomainHash: f.domain, ClientId: owner.ClientId, Generation: owner.Generation, PublicKey: owner.PublicKey, Epoch: f.epoch.Epoch, Kind: kind, Block: boundary.Number, BlockHash: cut.BlockHash, IssuedAtUnix: clock.Unix() + 1, ExpiresAtUnix: clock.Unix() + 601}, f.approver)
			if err != nil {
				t.Fatal(err)
			}
			requestRaw, err := request.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			cutRaw, err := cut.Bytes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := model.RetainProviderWorkRequest(t.Context(), requestRaw, f.authority.RequestPublicKey, f.domain, clock.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := model.RetainProviderWorkCut(t.Context(), protocol.OriginalWorkCutSubmission{Request: requestRaw, Cut: cutRaw}, f.authority.RequestPublicKey, f.domain); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The actual payout path must retain every independently expected idle provider
// before querying bindings or signing, without creating usage or eligibility.
func TestProviderWorkIdleRosterReachesActualPayoutAndBindings(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		keys := providerWorkRosterFixture(t, f)
		f.retainAuthority(t)
		providerWorkRetainRosterCuts(t, f, keys, nil, false)
		client := &providerWorkRosterClient{StClient: newStubStClient(&StEpochState{})}
		if _, leaves, err := stComputeReleasePayout(t.Context(), f.cfg, client, f.epoch.Epoch, f.epoch.StartTime, f.epoch.EndTime, f.epoch.Start.Block, f.epoch.End.Block, f.epoch); err != nil || leaves != 0 {
			t.Fatal("idle original roster did not reach production payout", leaves, err)
		}
		if len(client.clients) != 2 || client.clients[0] != f.authority.ExpectedProviders[0].ClientId || client.clients[1] != f.authority.ExpectedProviders[1].ClientId {
			t.Fatal("idle original roster did not reach actual binding query")
		}
		stored := model.GetStPayoutArtifact(t.Context(), f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId)
		store, ok := server.LoadBlobStore()
		if !ok || stored == nil {
			t.Fatal("idle roster publication absent")
		}
		artifact, _, err := startifact.Read(t.Context(), store, stored.ContentHash)
		if err != nil || len(artifact.Providers) != 2 || artifact.ClosedWork == nil || artifact.ClosedWork.WholeInventory == nil {
			t.Fatal("signed artifact omitted idle original provider universe", err)
		}
		for _, provider := range artifact.Providers {
			if provider.UsageBytes != 0 || provider.Eligible || provider.Assignments != 0 || provider.Confirmations != 0 {
				t.Fatal("zero roster row invented usage or eligibility")
			}
		}
	})
}

// An approved prospective window must not be irreversibly signed as legacy
// while its asynchronous original cuts are still arriving.
func TestProviderWorkApprovedPendingCutsDoNotPublishLegacyArtifact(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		keys := providerWorkRosterFixture(t, f)
		f.authority.ExpectedProviders = []payoutartifact.WholeWorkExpectedProvider{}
		f.retainAuthority(t)
		client := &providerWorkRosterClient{StClient: newStubStClient(&StEpochState{})}
		if _, _, err := stComputeReleasePayout(t.Context(), f.cfg, client, f.epoch.Epoch, f.epoch.StartTime, f.epoch.EndTime, f.epoch.Start.Block, f.epoch.End.Block, f.epoch); !errors.Is(err, payoutartifact.ErrClosedWorkUnavailable) {
			t.Fatal("approved pending capture did not retain pending state", err)
		}
		if prior := model.GetStPayoutArtifact(t.Context(), f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId); prior != nil {
			t.Fatal("pending original cuts published immutable legacy artifact")
		}
		providerWorkRetainRosterCuts(t, f, keys, nil, false)
		if _, _, err := stComputeReleasePayout(t.Context(), f.cfg, client, f.epoch.Epoch, f.epoch.StartTime, f.epoch.EndTime, f.epoch.Start.Block, f.epoch.End.Block, f.epoch); err != nil {
			t.Fatal("retained original cut delivery did not resume same payout", err)
		}
	})
}

// Usage rows may neither expand the signed universe nor change its network;
// absence of original authority keeps ordinary behavior explicitly unknown.
func TestProviderWorkRosterRefusesUnapprovedUsageAndForeignNetwork(t *testing.T) {
	approved := &stProviderWorkAuthority{Authority: payoutartifact.WholeWorkAuthority{ExpectedProviders: []payoutartifact.WholeWorkExpectedProvider{{ClientId: [16]byte{1}, NetworkId: [16]byte{2}}}}}
	for _, usages := range [][]*model.StProviderUsage{{{ClientId: server.Id{3}, NetworkId: server.Id{2}, PayoutByteCount: 1}}, {{ClientId: server.Id{1}, NetworkId: server.Id{3}, PayoutByteCount: 1}}, {{ClientId: server.Id{1}, NetworkId: server.Id{2}, PayoutByteCount: 1}, {ClientId: server.Id{1}, NetworkId: server.Id{2}, PayoutByteCount: 1}}} {
		if _, err := stProviderWorkUsages(t.Context(), approved, usages); !errors.Is(err, model.ErrProviderWorkConflict) {
			t.Fatal("observed rows reinterpreted signed provider universe", err)
		}
	}
	legacy := []*model.StProviderUsage{{ClientId: server.Id{3}, NetworkId: server.Id{4}, PayoutByteCount: 5}}
	if got, err := stProviderWorkUsages(t.Context(), nil, legacy); err != nil || len(got) != 1 || got[0] != legacy[0] {
		t.Fatal("missing original authority invented a provider roster", err)
	}
}

// A canceled zero-credit original avoids synthetic earning reports while
// retaining real SDK terminal-head, request, clock and complete roster proofs.
func providerWorkPublishCanceledPrior(t testing.TB, f *providerWorkWindowFixture, keys []ed25519.PrivateKey) (protocol.OriginalWorkContract, *payoutartifact.VerifiedWholeWorkInventory) {
	t.Helper()
	id := [16]byte{151}
	source, destination := f.authority.Owners[0], f.authority.Owners[1]
	stored, err := proto.Marshal(&protocol.StoredContract{ContractId: id[:], SourceId: source.ClientId[:], DestinationId: destination.ClientId[:], TransferByteCount: 100})
	if err != nil {
		t.Fatal(err)
	}
	head, err := protocol.SignOriginalCloseInventory(protocol.OriginalCloseInventory{DomainHash: f.domain, ClientId: source.ClientId, ContractId: id, ReportHash: [32]byte{152}, Sequence: 1, Terminal: true}, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	headRaw, err := head.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	record := protocol.OriginalWorkContract{ContractId: id, StoredContract: stored, LatestInventory: headRaw}
	f.retainAuthority(t)
	providerWorkRetainRosterCuts(t, f, keys, &record, false)
	closedAt := f.epoch.StartTime.Add(time.Minute).Format(time.RFC3339Nano)
	f.window.Records = []payoutartifact.ClosedWorkWindowRecord{{ContractId: fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), Disposition: "canceled", ClosedAt: &closedAt}}
	inventory, expected, err := stPrepareWholeWorkInventory(t.Context(), f.cfg, f.epoch, f.window)
	if err != nil || inventory == nil {
		t.Fatal("original canceled window failed preparation", err)
	}
	providers := make([]startifact.ProviderInput, 0, len(f.authority.ExpectedProviders))
	for _, provider := range f.authority.ExpectedProviders {
		providers = append(providers, startifact.ProviderInput{ClientID: provider.ClientId, NetworkID: provider.NetworkId, ExclusionReason: "missing_payout_wallet"})
	}
	census := &payoutartifact.ClosedWorkCensus{Schema: payoutartifact.ClosedWorkSchema, DeploymentId: f.cfg.DeploymentId, ChainId: f.cfg.ChainId, GenesisHash: fmt.Sprintf("0x%x", f.cfg.GenesisHash), Netuid: uint16(f.cfg.Netuid), Coordinator: f.cfg.ContractAddress, SettlementVault: f.cfg.SettlementVault, Epoch: f.epoch.Epoch, NoId: f.cfg.NoId, PolicyHash: fmt.Sprintf("0x%x", f.epoch.PolicyHash), Start: f.authority.Start, End: f.authority.End, WindowStart: f.window.Start, WindowEnd: f.window.End, Records: []payoutartifact.ClosedWorkRecord{}, WholeInventory: inventory}
	artifact, err := startifact.BuildWithContext(t.Context(), startifact.BuildInput{ClosedWork: census, DeploymentID: f.cfg.DeploymentId, ChainID: f.cfg.ChainId, GenesisHash: census.GenesisHash, Netuid: uint16(f.cfg.Netuid), Coordinator: f.cfg.ContractAddress, SettlementVault: f.cfg.SettlementVault, Epoch: f.epoch.Epoch, NoID: f.cfg.NoId, PolicyHash: census.PolicyHash, Start: f.authority.Start, End: f.authority.End, OperatorSnapshotHash: "sha256:" + strings.Repeat("01", 32), FleetSnapshotHash: "sha256:" + strings.Repeat("02", 32), Providers: providers, ReliabilityAMin: uint64(f.cfg.ReliabilityAMin), CreatedAt: f.epoch.EndTime})
	if err != nil {
		t.Fatal(err)
	}
	if err := startifact.Sign(artifact, f.cfg.ArtifactKey); err != nil {
		t.Fatal(err)
	}
	verified, err := payoutartifact.VerifyWholeWorkInventory(t.Context(), artifact, expected)
	if err != nil || !verified.Complete || len(verified.ReconciledContracts) != 1 {
		t.Fatal("original canceled window lacks reconstructed checkpoint", err)
	}
	if err := stRetainWholeWorkWindow(t.Context(), artifact, expected); err != nil {
		t.Fatal(err)
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("original blob owner absent")
	}
	published, err := startifact.Publish(t.Context(), store, artifact)
	if err != nil {
		t.Fatal(err)
	}
	model.AddStPayoutArtifact(t.Context(), f.cfg.DeploymentKey(), &model.StPayoutArtifact{Epoch: f.epoch.Epoch, NoId: f.cfg.NoId, ContentHash: published.ContentHash, ContentKey: published.ContentKey, HistoryKey: published.HistoryKey, PayoutRoot: artifact.PayoutRoot, CreateTime: f.epoch.EndTime})
	return record, verified
}

// A new independent authority proposes the earlier hash, but only actual
// original readback can populate the later window's prior expectations.
func TestProviderWorkPriorOriginalsReachActualLaterPayoutAndCompanion(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		keys := providerWorkRosterFixture(t, f)
		record, verified := providerWorkPublishCanceledPrior(t, f, keys)
		f.authority.PriorContracts = append([]payoutartifact.WholeWorkPriorContract(nil), verified.ReconciledContracts...)
		f.epoch.Epoch++
		f.authority.Epoch = f.epoch.Epoch
		for side, number := range []uint64{40, 50} {
			clock := f.epoch.EndTime.Add(time.Duration(side) * time.Hour)
			header := &types.Header{Number: new(big.Int).SetUint64(number), Time: uint64(clock.Unix())}
			raw, err := rlp.EncodeToBytes(header)
			if err != nil {
				t.Fatal(err)
			}
			boundary := payoutartifact.Boundary{Number: number, Hash: header.Hash().Hex()}
			if side == 0 {
				f.epoch.StartTime, f.epoch.StartHeader, f.epoch.Start = clock, raw, snprotocol.ClientKeyEffectiveBoundary{Block: number, Hash: [32]byte(header.Hash())}
				f.authority.Start = boundary
			} else {
				f.epoch.EndTime, f.epoch.EndHeader, f.epoch.End = clock, raw, snprotocol.ClientKeyEffectiveBoundary{Block: number, Hash: [32]byte(header.Hash())}
				f.authority.End = boundary
			}
		}
		f.retainAuthority(t)
		providerWorkRetainRosterCuts(t, f, keys, &record, true)
		client := &providerWorkRosterClient{StClient: newStubStClient(&StEpochState{})}
		if _, _, err := stComputeReleasePayout(t.Context(), f.cfg, client, f.epoch.Epoch, f.epoch.StartTime, f.epoch.EndTime, f.epoch.Start.Block, f.epoch.End.Block, f.epoch); err != nil {
			t.Fatal("actual later payout did not revalidate prior originals", err)
		}
		stored := model.GetStPayoutArtifact(t.Context(), f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId)
		if stored == nil {
			t.Fatal("later original artifact absent")
		}
		digest, err := providerWorkPolicyHash(strings.TrimPrefix(stored.ContentHash, "sha256:"))
		if err != nil {
			t.Fatal(err)
		}
		first, err := ProviderWorkWindow(t.Context(), f.domain, f.epoch.Epoch, digest, [32]byte{})
		if err != nil {
			t.Fatal("public later companion lost prior original closure", err)
		}
		second, err := ProviderWorkWindow(t.Context(), f.domain, f.epoch.Epoch, digest, [32]byte{})
		if err != nil || !bytes.Equal(first, second) {
			t.Fatal("fresh reader changed independently verified companion", err)
		}
		proposed := f.authority
		proposed.PriorContracts = append([]payoutartifact.WholeWorkPriorContract(nil), f.authority.PriorContracts...)
		proposed.PriorContracts[0].SourceInventoryHash[0] ^= 1
		proposed, err = payoutartifact.SignWholeWorkAuthority(t.Context(), proposed, f.cfg.RootKey)
		if err != nil {
			t.Fatal(err)
		}
		_, _, expected, err := LoadProviderWorkAuthorityPolicy()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := providerWorkPriorExpectation(t.Context(), proposed, expected); !errors.Is(err, payoutartifact.ErrClosedWorkIntegrity) || len(got.PriorContracts) != 0 {
			t.Fatal("new signature approved its own prior checkpoint", err)
		}
	})
}

// Source absence and I/O preserve unknown/transient ownership; neither can be
// filled from a valid newly signed proposal, including parent cancellation.
func TestProviderWorkPriorMissingAndCanceledSourceNeverInventsCheckpoint(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		f.authority.PriorContracts = []payoutartifact.WholeWorkPriorContract{{ContractId: [16]byte{155}, ReconciledEpoch: 1, InventoryHash: "sha256:" + strings.Repeat("03", 32), StoredContractHash: [32]byte{156}, SourceInventoryHash: [32]byte{157}}}
		f.retainAuthority(t)
		_, _, expected, err := LoadProviderWorkAuthorityPolicy()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := providerWorkPriorExpectation(t.Context(), f.authority, expected); !errors.Is(err, payoutartifact.ErrClosedWorkUnavailable) || len(got.PriorContracts) != 0 {
			t.Fatal("missing retained prior became a proposed checkpoint", err)
		}
		ctx, cancel := context.WithCancelCause(t.Context())
		reads := 0
		got, err := providerWorkPriorExpectationWithReader(ctx, f.authority, expected, func(context.Context, uint64) (providerWorkPriorOriginal, error) {
			reads++
			cancel(syscall.EIO)
			return providerWorkPriorOriginal{}, syscall.EIO
		})
		if reads != 1 || !errors.Is(err, syscall.EIO) || !errors.Is(err, context.Canceled) || errors.Is(err, payoutartifact.ErrClosedWorkIntegrity) || len(got.PriorContracts) != 0 {
			t.Fatal("canceled prior source invented a hard contradiction", reads, err)
		}
	})
}
