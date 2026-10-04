//go:build linux || darwin || freebsd

// An actual inherited stream remains open while its original SDK generations
// retire. A fresh payout reader must recover the birth from published custody.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	schnorrkel "github.com/ChainSafe/go-schnorrkel"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/urfoundation/sn/payoutartifact"
	snprotocol "github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
	"google.golang.org/protobuf/proto"
)

type providerWorkInheritedSdk struct {
	client    *connect.Client
	transport *providerWorkPublisherOob
	owner     payoutartifact.WholeWorkOwner
}

type providerWorkInheritedFixture struct {
	f          *providerWorkWindowFixture
	ctx        context.Context
	prior      *model.StPayoutArtifact
	original   payoutartifact.WholeWorkRetainedCreation
	contractId [16]byte
}

// The existing binding transport is called after the real SQL census returns.
// Its callback advances actual SDK effects, never supplies census or proof bytes.
type providerWorkInheritedPublishClient struct {
	*providerWorkRosterClient
	afterCensus func()
}

func (self *providerWorkInheritedPublishClient) BindingsAt(ctx context.Context, clients [][16]byte, epoch, start, end uint64) ([]*StFleetBindingState, error) {
	self.afterCensus()
	return self.providerWorkRosterClient.BindingsAt(ctx, clients, epoch, start, end)
}

// Real event timestamps use microseconds; the next second is an explicit
// header boundary, never a wait for asynchronous delivery or an assertion.
func providerWorkInheritedBoundary(t testing.TB, ctx context.Context) time.Time {
	t.Helper()
	boundary := server.NowUtc().Truncate(time.Second).Add(time.Second)
	select {
	case <-time.After(time.Until(boundary)):
		return boundary
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return time.Time{}
	}
}

// Each SDK owns a fresh generation and real pre-send custody. Reusing the
// registered client key cannot revive its old inventory generation.
func providerWorkInheritedNewSdk(t testing.TB, ctx context.Context, f *providerWorkWindowFixture, index, generation int) *providerWorkInheritedSdk {
	t.Helper()
	id := server.Id{byte(index + 1)}
	seed := bytes.Repeat([]byte{byte(164 + index)}, ed25519.SeedSize)
	key := ed25519.NewKeyFromSeed(seed)
	settings := connect.DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.Log = connect.NewNoopLogger()
	settings.EncryptionSettings.Mode = connect.EncryptionModeOff
	settings.ClientKeySeed = seed
	settings.ContractManagerSettings = connect.DefaultContractManagerSettingsNoNetworkEvents()
	settings.ContractManagerSettings.CloseReportDomainHash = f.domain
	settings.ContractManagerSettings.InitialContractTransferByteCount = 121
	scope := connect.OriginalContractStoreScope{DomainHash: f.domain, ClientId: [16]byte(id), PublicKey: [32]byte(key.Public().(ed25519.PublicKey)), SourceGeneration: [16]byte{byte(180 + 3*generation + index)}}
	settings.ContractManagerSettings.OriginalContractCapture = &connect.OriginalContractCaptureSettings{Directory: filepath.Join(t.TempDir(), "requests"), PublicKey: scope.PublicKey, SourceGeneration: scope.SourceGeneration}
	providerWorkPrepareCreationStore(t, settings.ContractManagerSettings.OriginalContractCapture.Directory, scope)
	transport := &providerWorkPublisherOob{ctx: ctx, clientId: id, directory: settings.ContractManagerSettings.OriginalContractCapture.Directory, settings: settings.ContractManagerSettings}
	client := connect.NewClient(ctx, connect.Id(id), transport, settings)
	t.Cleanup(func() {
		join, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.CloseAndWait(join); err != nil {
			t.Error(err)
		}
	})
	identity, err := client.ContractManager().OriginalWorkIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return &providerWorkInheritedSdk{client: client, transport: transport, owner: payoutartifact.WholeWorkOwner{ClientId: identity.ClientId, NetworkId: [16]byte{11}, Generation: identity.Generation, PublicKey: identity.PublicKey}}
}

// The real destination authenticates and enrolls each returned reservation.
func providerWorkInheritedCreate(t testing.TB, ctx context.Context, source, destination *providerWorkInheritedSdk, key connect.ContractKey) protocol.StoredContract {
	t.Helper()
	manager := source.client.ContractManager()
	manager.CreateContract(key, 0, 121)
	contract := manager.TakeContract(ctx, key, 0)
	if contract == nil {
		t.Fatal("actual inherited SDK request returned no reservation")
	}
	if !destination.client.ContractManager().Verify(contract.StoredContractHmac, contract.StoredContractBytes, contract.ProvideMode) {
		t.Fatal("actual inherited destination refused reservation")
	}
	var stored protocol.StoredContract
	if err := proto.Unmarshal(contract.StoredContractBytes, &stored); err != nil || len(stored.ContractId) != 16 || len(stored.StreamId) != 16 {
		t.Fatal("actual inherited reservation lost its stream", err)
	}
	return stored
}

func providerWorkInheritedClose(t testing.TB, source, destination *providerWorkInheritedSdk, contractId []byte) {
	t.Helper()
	for _, sdk := range []*providerWorkInheritedSdk{source, destination} {
		sdk.client.ContractManager().Close()
		sdk.client.ContractManager().CloseContract(connect.RequireIdFromBytes(contractId), 121, 0)
		sdk.transport.stateLock.Lock()
		err := sdk.transport.err
		sdk.transport.stateLock.Unlock()
		if err != nil {
			t.Fatal("actual inherited close ingress failed", err)
		}
	}
}

// A closed epoch needs its original prospective consent, including the root
// attestation and coldkey signature made before that epoch. Only those immutable
// historical bytes are seeded; the actual payout selector verifies them later.
func providerWorkInheritedWallet(t testing.TB, ctx context.Context, f *providerWorkWindowFixture, owner payoutartifact.WholeWorkOwner, userId server.Id, index int) payoutartifact.WholeWorkExpectedProvider {
	t.Helper()
	key, err := schnorrkel.NewMiniSecretKeyFromRaw([32]byte{byte(190 + index)})
	if err != nil {
		t.Fatal(err)
	}
	statement := snprotocol.WalletMappingStatement{Domain: f.authority.Domain, UserId: [16]byte(userId), ClientId: owner.ClientId, NetworkId: owner.NetworkId, Coldkey: key.Public().Encode(), Generation: 1, Nonce: [32]byte{byte(194 + index)}, IssuedAt: f.epoch.StartTime.Add(-10 * time.Minute).Unix(), ExpiresAt: f.epoch.StartTime.Add(-5 * time.Minute).Unix(), FromEpoch: f.epoch.Epoch, ThroughEpoch: f.epoch.Epoch + 1}
	boundary := snprotocol.ClientKeyEffectiveBoundary{Epoch: f.epoch.Epoch - 1, Block: f.epoch.Start.Block - 1, Hash: [32]byte{189}}
	if err := snprotocol.SignProspectiveWalletMapping(&statement, boundary, f.cfg.RootKey); err != nil {
		t.Fatal(err)
	}
	message, err := statement.Message()
	if err != nil {
		t.Fatal(err)
	}
	signature, err := key.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
	if err != nil {
		t.Fatal(err)
	}
	original := snprotocol.WalletMappingConsent{Message: message, Signature: signature.Encode()}
	_, head, err := snprotocol.VerifyWalletMappingConsent(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO wallet_mapping_challenge(nonce,domain_hash,client_id,generation,expires_at,message) VALUES($1,$2,$3,$4,$5,$6)`, statement.Nonce[:], f.domain[:], server.Id(owner.ClientId), statement.Generation, statement.ExpiresAt, message))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO wallet_mapping_consent(domain_hash,client_id,generation,original_hash,nonce,original,accepted_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, f.domain[:], server.Id(owner.ClientId), statement.Generation, head[:], statement.Nonce[:], raw, time.Unix(statement.IssuedAt+1, 0).UTC()))
	})
	return payoutartifact.WholeWorkExpectedProvider{ClientId: owner.ClientId, NetworkId: owner.NetworkId, WalletHeadHash: hex.EncodeToString(head[:]), WalletGeneration: statement.Generation}
}

// Both generations coexist in the first independently signed roster. The new
// one holds an actual open contract when the old stream origin is reconciled.
func newProviderWorkInheritedFixture(t testing.TB) *providerWorkInheritedFixture {
	t.Helper()
	f := newProviderWorkWindowFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	start := server.NowUtc().Truncate(time.Second).Add(-time.Hour)
	controllerPayoutSchedule(t, start)
	providerWorkSetClock(t, f, start, start.Add(2*time.Hour))
	signer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{161}, ed25519.SeedSize))
	authority := snprotocol.ProviderWorkSourceAuthority{DomainHash: f.domain, SourceId: server.Id{162}.String(), Generation: server.Id{163}.String(), PublicKey: [32]byte(signer.Public().(ed25519.PublicKey)), FromUnixMicro: start.Add(-time.Hour).UnixMicro(), ThroughUnixMicro: start.Add(3 * time.Hour).UnixMicro(), MaxEndpointEvents: 4096, MaxCohortMembers: 64, DirectoryPublicKeys: [][32]byte{}}
	source, err := model.NewProviderWorkSessionSource(authority, signer)
	if err != nil {
		t.Fatal(err)
	}
	ctx = model.WithProviderWorkSessionSource(ctx, source)
	f.authority.WorkSources = []snprotocol.ProviderWorkSourceAuthority{authority}
	root := crypto.PubkeyToAddress(f.cfg.RootKey.PublicKey)
	t.Cleanup(server.Vault.PushSimpleResource("provider_work.yml", []byte(fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\nauthority_signer: %s\nclient_key_root_signer: %s\nattribution_signer: %s\n", ProviderWorkPolicySchema, f.domain, f.authority.RequestPublicKey, root.Hex(), root.Hex(), root.Hex()))))
	network, userId := server.Id{11}, server.NewId()
	model.Testing_CreateNetwork(ctx, network, "inherited-publisher.example", userId)
	old := make([]*providerWorkInheritedSdk, 3)
	for index := range old {
		id := server.Id{byte(index + 1)}
		model.Testing_CreateDevice(ctx, network, server.NewId(), id, "synthetic", "synthetic")
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(164 + index)}, ed25519.SeedSize))
		if _, err := model.StoreStClientKeyRegistration(ctx, model.StClientKeyRegistrationInput{Domain: f.authority.Domain, DeploymentID: f.cfg.DeploymentId, ClientID: id, PublicKey: key.Public().(ed25519.PublicKey), Boundary: snprotocol.ClientKeyEffectiveBoundary{Block: 10, Hash: [32]byte{9}}, RootKey: f.cfg.RootKey, ArtifactKey: f.cfg.ArtifactKey, CreatedAt: start.Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
		old[index] = providerWorkInheritedNewSdk(t, ctx, f, index, 0)
		f.authority.ExpectedProviders = append(f.authority.ExpectedProviders, providerWorkInheritedWallet(t, ctx, f, old[index].owner, userId, index))
		providerWorkRetainActualCut(t, f, old[index].client.ContractManager(), "start")
	}
	secret := bytes.Repeat([]byte{167}, 32)
	old[1].client.ContractManager().LoadProvideSecretKeys(map[protocol.ProvideMode][]byte{protocol.ProvideMode_Network: secret})
	provided := make(chan error, 1)
	old[1].client.ContractManager().SetProvideModesWithOobAckCallback(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}, func(err error) { provided <- err })
	select {
	case err := <-provided:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	handler := model.CreateNetworkClientHandler(ctx)
	for index, id := range []server.Id{{1}, {2}} {
		if _, _, _, _, err := model.ConnectNetworkClientWithIpFamily(ctx, id, fmt.Sprintf("192.0.2.%d:12001", 20+index), handler, 4); err != nil {
			t.Fatal(err)
		}
	}
	origin := providerWorkInheritedCreate(t, ctx, old[0], old[1], connect.ContractKey{Destination: connect.DestinationId(old[1].client.ClientId()), IntermediaryIds: connect.RequireMultiHopId(old[2].client.ClientId())})
	current := []*providerWorkInheritedSdk{providerWorkInheritedNewSdk(t, ctx, f, 0, 1), providerWorkInheritedNewSdk(t, ctx, f, 1, 1), old[2]}
	current[1].client.ContractManager().LoadProvideSecretKeys(map[protocol.ProvideMode][]byte{protocol.ProvideMode_Network: secret})
	for _, sdk := range current[:2] {
		providerWorkRetainActualCut(t, f, sdk.client.ContractManager(), "start")
	}
	inherited := providerWorkInheritedCreate(t, ctx, current[0], current[1], connect.ContractKey{Destination: connect.DestinationId(current[1].client.ClientId())})
	if !bytes.Equal(origin.StreamId, inherited.StreamId) || bytes.Equal(origin.ContractId, inherited.ContractId) {
		t.Fatal("actual pair did not inherit its original live stream")
	}
	providerWorkInheritedClose(t, old[0], old[1], origin.ContractId)
	end := providerWorkInheritedBoundary(t, ctx)
	providerWorkSetClock(t, f, start, end)
	for _, sdk := range append(append([]*providerWorkInheritedSdk(nil), old...), current[:2]...) {
		f.authority.Owners = append(f.authority.Owners, sdk.owner)
		cut := providerWorkRetainActualCut(t, f, sdk.client.ContractManager(), "end")
		if !cut.Complete {
			t.Fatal("first actual SDK census is incomplete")
		}
	}
	slices.SortFunc(f.authority.Owners, func(a, b payoutartifact.WholeWorkOwner) int {
		if order := bytes.Compare(a.ClientId[:], b.ClientId[:]); order != 0 {
			return order
		}
		return bytes.Compare(a.Generation[:], b.Generation[:])
	})
	f.retainAuthority(t)
	// Freeze the actual SQL window while this contract is open, then complete
	// its original outcome at the binding transport boundary. That later signed
	// receipt dates openness without editing the earlier SQL or SDK snapshots.
	closes := 0
	client := &providerWorkInheritedPublishClient{providerWorkRosterClient: &providerWorkRosterClient{StClient: newStubStClient(&StEpochState{})}, afterCensus: func() {
		closes++
		providerWorkInheritedClose(t, current[0], current[1], inherited.ContractId)
	}}
	if _, _, err := stComputeReleasePayout(ctx, f.cfg, client, f.epoch.Epoch, start, end, f.epoch.Start.Block, f.epoch.End.Block, f.epoch); err != nil {
		t.Fatal("actual origin with an open inherited stream did not publish", err)
	}
	if closes != 1 {
		t.Fatal("actual first census did not own the original later close", closes)
	}
	prior := model.GetStPayoutArtifact(ctx, f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId)
	store, ok := server.LoadBlobStore()
	if prior == nil || !ok {
		t.Fatal("first immutable publication is absent")
	}
	artifact, _, err := startifact.Read(ctx, store, prior.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := stLoadProviderWorkAuthority(ctx, f.cfg, f.epoch)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := payoutartifact.VerifyWholeWorkInventory(ctx, artifact, approved.Expectation)
	if err != nil || verified == nil || !verified.Complete || !verified.AttributionComplete || verified.Open != 1 || verified.Credited != 1 || len(verified.RetainedCreations) != 1 || verified.RetainedCreations[0].Original.ContractId != [16]byte(origin.ContractId) {
		t.Fatal("original admission did not bind exactly one stream birth", verified, err)
	}
	for _, sdk := range old[:2] {
		if err := sdk.client.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// The next roster enrolls only the surviving generations. Neither its
	// proposed exclusions nor its SDK cuts contain the retired stream origin.
	f.epoch.Epoch++
	f.authority.Epoch = f.epoch.Epoch
	f.authority.Owners = []payoutartifact.WholeWorkOwner{current[0].owner, current[1].owner, current[2].owner}
	f.authority.PriorContracts = []payoutartifact.WholeWorkPriorContract{}
	f.epoch.StartTime, f.epoch.Start, f.epoch.StartHeader = end, f.epoch.End, bytes.Clone(f.epoch.EndHeader)
	f.authority.Start = f.authority.End
	for _, sdk := range current {
		cut := providerWorkRetainActualCut(t, f, sdk.client.ContractManager(), "start")
		for _, contract := range cut.Contracts {
			if contract.ContractId == [16]byte(origin.ContractId) {
				t.Fatal("retired SDK origin entered the replacement census")
			}
		}
	}
	nextEnd := providerWorkInheritedBoundary(t, ctx)
	header := &types.Header{Number: big.NewInt(40), Time: uint64(nextEnd.Unix()), Difficulty: big.NewInt(0), GasLimit: 1, Extra: []byte{3}}
	f.epoch.EndHeader, err = rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatal(err)
	}
	f.epoch.EndTime, f.epoch.End = nextEnd, snprotocol.ClientKeyEffectiveBoundary{Block: 40, Hash: [32]byte(header.Hash())}
	f.authority.End = payoutartifact.Boundary{Number: 40, Hash: header.Hash().Hex()}
	f.window.Start, f.window.End = end.Format(time.RFC3339Nano), nextEnd.Format(time.RFC3339Nano)
	for _, sdk := range current {
		providerWorkRetainActualCut(t, f, sdk.client.ContractManager(), "end")
	}
	f.retainAuthority(t)
	return &providerWorkInheritedFixture{f: f, ctx: ctx, prior: prior, original: verified.RetainedCreations[0], contractId: [16]byte(inherited.ContractId)}
}

// The real producer, fresh source acquisition and public companion each replay
// the original published artifact; no verified-result injection crosses them.
func TestProviderWorkInheritedStreamReopensRetiredCreationThroughPublicConsumer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkInheritedFixture(t)
		client := &providerWorkRosterClient{StClient: newStubStClient(&StEpochState{})}
		if _, _, err := stComputeReleasePayout(f.ctx, f.f.cfg, client, f.f.epoch.Epoch, f.f.epoch.StartTime, f.f.epoch.EndTime, f.f.epoch.Start.Block, f.f.epoch.End.Block, f.f.epoch); err != nil {
			t.Fatal("actual inherited payout lost retired creation", err)
		}
		record := model.GetStPayoutArtifact(f.ctx, f.f.cfg.DeploymentKey(), f.f.epoch.Epoch, f.f.cfg.NoId)
		store, ok := server.LoadBlobStore()
		if record == nil || !ok {
			t.Fatal("inherited publication is absent")
		}
		artifact, raw, err := startifact.Read(f.ctx, store, record.ContentHash)
		if err != nil {
			t.Fatal(err)
		}
		approved, err := stLoadProviderWorkAuthority(f.ctx, f.f.cfg, f.f.epoch)
		if err != nil {
			t.Fatal(err)
		}
		current := providerWorkPriorCurrent{artifact: artifact, inventory: artifact.ClosedWork.WholeInventory}
		expected, err := providerWorkPriorExpectation(f.ctx, approved.Authority, approved.Expectation, current)
		if err != nil || len(expected.PriorCreations) != 1 || expected.PriorCreations[0].Checkpoint != f.original.Checkpoint || expected.PriorCreations[0].Owner != f.original.Owner || !bytes.Equal(expected.PriorCreations[0].Original.OriginalCreation, f.original.Original.OriginalCreation) {
			t.Fatal("fresh reader did not reacquire the exact retired original", err)
		}
		if expected.PriorCreations[0].Owner.Generation == approved.Authority.Owners[0].Generation {
			t.Fatal("fixture did not retire the source SDK generation")
		}
		digest, err := providerWorkPolicyHash(strings.TrimPrefix(record.ContentHash, "sha256:"))
		if err != nil {
			t.Fatal(err)
		}
		first, err := ProviderWorkWindow(f.ctx, f.f.domain, f.f.epoch.Epoch, digest, [32]byte{})
		if err != nil {
			t.Fatal("public inherited companion refused original source", err)
		}
		witness, err := payoutartifact.DecodeWholeWorkInventory(f.ctx, first)
		if err != nil {
			t.Fatal(err)
		}
		verified, err := payoutartifact.VerifyWholeWorkInventoryWithWitness(f.ctx, artifact, witness, expected)
		if err != nil || verified == nil || !verified.Complete || !verified.AttributionComplete || verified.Contracts != 1 || verified.Credited != 1 || len(verified.ReconciledContracts) != 1 || verified.ReconciledContracts[0].ContractId != f.contractId {
			t.Fatal("retired stream origin was lost or recredited", verified, err)
		}
		if len(verified.ExpectedProviders) != 3 || verified.ExpectedProviders[0].UsageBytes != 0 || verified.ExpectedProviders[1].UsageBytes != 61 || verified.ExpectedProviders[2].UsageBytes != 60 {
			t.Fatal("inherited work changed original earning parties", verified.ExpectedProviders)
		}
		// Caller mutation cannot poison the next independently owned acquisition.
		expected.PriorCreations[0].Original.OriginalCreation[0] ^= 1
		second, err := ProviderWorkWindow(f.ctx, f.f.domain, f.f.epoch.Epoch, digest, [32]byte{})
		if err != nil || !bytes.Equal(first, second) {
			t.Fatal("fresh public owner borrowed a mutated dependency", err)
		}
		if _, _, err := stComputeReleasePayout(f.ctx, f.f.cfg, client, f.f.epoch.Epoch, time.Time{}, time.Time{}, 0, 0, nil); err != nil {
			t.Fatal("published restart attempted new source acquisition", err)
		}
		_, after, err := startifact.Read(f.ctx, store, record.ContentHash)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatal("restart rewrote original published work", err)
		}
	})
}

// Losing the exact published prior bytes defeats the retained locator and
// source-signed lookup; it never permits a partial current artifact.
func TestProviderWorkInheritedStreamMissingPriorCannotPublish(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkInheritedFixture(t)
		config, ok := server.LoadBlobStoreConfig()
		if !ok || !config.Local {
			t.Fatal("fixture does not own a local immutable artifact")
		}
		if err := os.Remove(filepath.Join(config.LocalPath, f.prior.ContentKey)); err != nil {
			t.Fatal(err)
		}
		client := &providerWorkRosterClient{StClient: newStubStClient(&StEpochState{})}
		if _, _, err := stComputeReleasePayout(f.ctx, f.f.cfg, client, f.f.epoch.Epoch, f.f.epoch.StartTime, f.f.epoch.EndTime, f.f.epoch.Start.Block, f.f.epoch.End.Block, f.f.epoch); !errors.Is(err, payoutartifact.ErrClosedWorkUnavailable) {
			t.Fatal("missing published creation did not retain unknown work", err)
		}
		if record := model.GetStPayoutArtifact(f.ctx, f.f.cfg.DeploymentKey(), f.f.epoch.Epoch, f.f.cfg.NoId); record != nil {
			t.Fatal("missing retired original published another artifact")
		}
	})
}

// Corrupt the physical content-addressed object after successful publication.
// The unchanged SQL locator and signed stream receipt cannot bless new bytes.
func TestProviderWorkInheritedStreamMutatedPriorCannotPublish(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkInheritedFixture(t)
		config, ok := server.LoadBlobStoreConfig()
		if !ok || !config.Local {
			t.Fatal("fixture does not own a local immutable artifact")
		}
		path := filepath.Join(config.LocalPath, f.prior.ContentKey)
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			t.Fatal(err)
		}
		raw[0] ^= 1
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		client := &providerWorkRosterClient{StClient: newStubStClient(&StEpochState{})}
		if _, _, err := stComputeReleasePayout(f.ctx, f.f.cfg, client, f.f.epoch.Epoch, f.f.epoch.StartTime, f.f.epoch.EndTime, f.f.epoch.Start.Block, f.f.epoch.End.Block, f.f.epoch); err == nil {
			t.Fatal("mutated published creation acquired prior authority")
		}
		if record := model.GetStPayoutArtifact(f.ctx, f.f.cfg.DeploymentKey(), f.f.epoch.Epoch, f.f.cfg.NoId); record != nil {
			t.Fatal("mutated retired original published another artifact")
		}
	})
}
