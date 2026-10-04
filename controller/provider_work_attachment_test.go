//go:build linux || darwin || freebsd

// One actual SDK generation retains its pre-send request, server admission and
// close heads before the real payout publisher transports the live originals.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

// The OOB owns frame buffers through the synchronous server callback. Its
// observation records bytes already durable at the real pre-send boundary.
type providerWorkPublisherOob struct {
	ctx       context.Context
	clientId  server.Id
	directory string
	settings  *connect.ContractManagerSettings
	stateLock sync.Mutex
	request   []byte
	reply     []byte
	err       error
}

// Only client key registration uses the separately installed original model
// history below. Creation, provide registration and close use actual ingress.
func (self *providerWorkPublisherOob) SendControl(frames []*protocol.Frame, callback connect.OobResultFunction) {
	defer func() {
		for _, frame := range frames {
			connect.MessagePoolReturn(frame.MessageBytes)
		}
	}()
	if len(frames) == 1 && (frames[0].MessageType == protocol.MessageType_TransferClientKey || frames[0].MessageType == protocol.MessageType_TransferProvidePing || frames[0].MessageType == protocol.MessageType_TransferControlPing) {
		callback(nil, nil)
		return
	}
	if len(frames) == 1 && frames[0].MessageType == protocol.MessageType_TransferCreateContract {
		frameRaw, err := proto.Marshal(frames[0])
		var request []byte
		if err == nil {
			request, err = providerWorkReadPresend(self.ctx, self.directory, frameRaw)
		}
		self.stateLock.Lock()
		self.request, self.err = request, errors.Join(self.err, err)
		self.stateLock.Unlock()
		if err != nil {
			callback(nil, err)
			return
		}
	}
	replies, err := ConnectControlFrames(self.ctx, self.clientId, frames, self.settings)
	defer returnConnectControlFrames(replies)
	var reply []byte
	if err == nil && len(replies) == 1 && replies[0].MessageType == protocol.MessageType_TransferCreateContractResult {
		reply, err = proto.Marshal(replies[0])
	}
	self.stateLock.Lock()
	if reply != nil {
		self.reply = reply
	}
	self.err = errors.Join(self.err, err)
	self.stateLock.Unlock()
	callback(replies, err)
}

// Reading by exact signed content checks custody before any server reservation
// exists. Neither the callback nor the test recreates the missing request.
func providerWorkReadPresend(ctx context.Context, directory string, frame []byte) ([]byte, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "request-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		original, err := protocol.DecodeOriginalContractRequest(ctx, raw)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(original.RequestFrame, frame) {
			return raw, nil
		}
	}
	return nil, errors.New("actual SDK request was not durable before server ingress")
}

// Headers are original fixture clock inputs. Their canonical bytes and exact
// hashes remain consistent with the actual SQL event window.
func providerWorkSetClock(t testing.TB, f *providerWorkWindowFixture, start, end time.Time) {
	t.Helper()
	for index, clock := range []time.Time{start, end} {
		number := uint64(20 + 10*index)
		header := &types.Header{Number: new(big.Int).SetUint64(number), Time: uint64(clock.Unix()), Difficulty: big.NewInt(0), GasLimit: 1, Extra: []byte{byte(index + 1)}}
		raw, err := rlp.EncodeToBytes(header)
		if err != nil {
			t.Fatal(err)
		}
		boundary := payoutartifact.Boundary{Number: number, Hash: header.Hash().Hex()}
		if index == 0 {
			f.epoch.StartTime, f.epoch.StartHeader, f.epoch.Start = clock, raw, snprotocol.ClientKeyEffectiveBoundary{Block: number, Hash: [32]byte(header.Hash())}
			f.authority.Start = boundary
		} else {
			f.epoch.EndTime, f.epoch.EndHeader, f.epoch.End = clock, raw, snprotocol.ClientKeyEffectiveBoundary{Block: number, Hash: [32]byte(header.Hash())}
			f.authority.End = boundary
		}
	}
	f.window.Start, f.window.End = start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano)
}

// Requests authorize the actual returned SDK cut. Intake verifies both original
// signatures and retains exact bytes; no test constructs or signs a cut.
func providerWorkRetainActualCut(t testing.TB, f *providerWorkWindowFixture, manager *connect.ContractManager, kind string) protocol.OriginalWorkCut {
	t.Helper()
	identity, err := manager.OriginalWorkIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	boundary := f.epoch.Start
	if kind == "end" {
		boundary = f.epoch.End
	}
	issued := server.NowUtc()
	request, err := protocol.SignOriginalWorkRequest(protocol.OriginalWorkRequest{RequestId: [16]byte(server.NewId()), DomainHash: f.domain, ClientId: identity.ClientId, Generation: identity.Generation, PublicKey: identity.PublicKey, Epoch: f.epoch.Epoch, Kind: kind, Block: boundary.Block, BlockHash: boundary.Hash, IssuedAtUnix: issued.Unix(), ExpiresAtUnix: issued.Add(5 * time.Minute).Unix()}, f.approver)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := request.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.RetainProviderWorkRequest(t.Context(), raw, f.authority.RequestPublicKey, f.domain, issued); err != nil {
		t.Fatal(err)
	}
	cut, err := manager.OriginalWorkCut(t.Context(), f.epoch.Epoch, boundary.Block, boundary.Hash)
	if err != nil {
		t.Fatal(err)
	}
	cutRaw, err := cut.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := model.RetainProviderWorkCut(t.Context(), protocol.OriginalWorkCutSubmission{Request: raw, Cut: cutRaw}, f.authority.RequestPublicKey, f.domain)
	if err != nil || receipt.CutHash != sha256.Sum256(cutRaw) {
		t.Fatal("actual SDK cut intake lost original identity", err)
	}
	return cut
}

// This test owns only a newly created empty root. Runtime receives its original
// physical checkpoint and cannot recreate missing custody after construction.
func providerWorkPrepareCreationStore(t testing.TB, directory string, scope connect.OriginalContractStoreScope) {
	t.Helper()
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	lease, err := os.OpenFile(filepath.Join(directory, connect.OriginalContractStoreLeaseName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(lease.Sync(), lease.Close()); err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	raw, err := connect.BuildFreshOriginalContractStoreCheckpoint(t.Context(), root, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Fsetxattr(int(root.Fd()), connect.OriginalContractStoreAttribute, raw, 0); err != nil {
		t.Fatal(err)
	}
	if err := root.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := connect.ValidateOriginalContractStore(t.Context(), directory, scope); err != nil {
		t.Fatal(err)
	}
}

// This single root follows nonempty original work through SDK custody, actual
// stream creation and closes, production payout signing and public verification.
func TestProviderWorkActualSdkStreamPublishesCompleteNonemptyAttribution(t *testing.T) {
	providerWorkActualSdkStreamPublication(t, false)
}

// Ordinary traffic still completes when the optional signer was unavailable at
// live admission. Later signer recovery cannot invent that original history.
func TestProviderWorkActualPublisherWaitsForMissingLiveSessionOriginal(t *testing.T) {
	providerWorkActualSdkStreamPublication(t, true)
}

// Both paths use the same genuine SDK and database producers. The sole fault
// removes the optional signer from live connection admission, before exposure.
func providerWorkActualSdkStreamPublication(t *testing.T, missingSessionOriginal bool, continuations ...func(testing.TB, context.Context, *providerWorkWindowFixture, *payoutartifact.Artifact)) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		ctx, stop := context.WithTimeout(t.Context(), 90*time.Second)
		defer stop()
		start := server.NowUtc().Truncate(time.Second).Add(-time.Hour)
		controllerPayoutSchedule(t, start)
		providerWorkSetClock(t, f, start, start.Add(2*time.Hour))
		signer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{161}, 32))
		sourceAuthority := snprotocol.ProviderWorkSourceAuthority{DomainHash: f.domain, SourceId: server.Id{162}.String(), Generation: server.Id{163}.String(), PublicKey: [32]byte(signer.Public().(ed25519.PublicKey)), FromUnixMicro: start.Add(-time.Hour).UnixMicro(), ThroughUnixMicro: start.Add(3 * time.Hour).UnixMicro(), MaxEndpointEvents: 4096, MaxCohortMembers: 64, DirectoryPublicKeys: [][32]byte{}}
		source, err := model.NewProviderWorkSessionSource(sourceAuthority, signer)
		if err != nil {
			t.Fatal(err)
		}
		ctx = model.WithProviderWorkSessionSource(ctx, source)
		f.authority.WorkSources = []snprotocol.ProviderWorkSourceAuthority{sourceAuthority}
		root := crypto.PubkeyToAddress(f.cfg.RootKey.PublicKey)
		t.Cleanup(server.Vault.PushSimpleResource("provider_work.yml", []byte(fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\nauthority_signer: %s\nclient_key_root_signer: %s\nattribution_signer: %s\n", ProviderWorkPolicySchema, f.domain, f.authority.RequestPublicKey, root.Hex(), root.Hex(), root.Hex()))))
		networkId := server.Id{11}
		model.Testing_CreateNetwork(ctx, networkId, "actual-publisher.example", server.NewId())
		var clients []*connect.Client
		var transports []*providerWorkPublisherOob
		t.Cleanup(func() {
			join, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, client := range clients {
				if err := client.CloseAndWait(join); err != nil {
					t.Error(err)
				}
			}
		})
		for index := 0; index < 3; index++ {
			id := server.Id{byte(index + 1)}
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), id, "synthetic", "synthetic")
			seed := bytes.Repeat([]byte{byte(164 + index)}, 32)
			key := ed25519.NewKeyFromSeed(seed)
			if _, err := model.StoreStClientKeyRegistration(ctx, model.StClientKeyRegistrationInput{Domain: f.authority.Domain, DeploymentID: f.cfg.DeploymentId, ClientID: id, PublicKey: key.Public().(ed25519.PublicKey), Boundary: snprotocol.ClientKeyEffectiveBoundary{Block: 10, Hash: [32]byte{9}}, RootKey: f.cfg.RootKey, ArtifactKey: f.cfg.ArtifactKey, CreatedAt: start.Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			settings := connect.DefaultClientSettings()
			settings.ControlPingTimeout = 0
			settings.Log = connect.NewNoopLogger()
			settings.EncryptionSettings.Mode = connect.EncryptionModeOff
			settings.ClientKeySeed = seed
			settings.ContractManagerSettings = connect.DefaultContractManagerSettingsNoNetworkEvents()
			settings.ContractManagerSettings.CloseReportDomainHash = f.domain
			settings.ContractManagerSettings.InitialContractTransferByteCount = 121
			scope := connect.OriginalContractStoreScope{DomainHash: f.domain, ClientId: [16]byte(id), PublicKey: [32]byte(key.Public().(ed25519.PublicKey)), SourceGeneration: [16]byte{byte(174 + index)}}
			settings.ContractManagerSettings.OriginalContractCapture = &connect.OriginalContractCaptureSettings{Directory: filepath.Join(t.TempDir(), "requests"), PublicKey: scope.PublicKey, SourceGeneration: scope.SourceGeneration}
			providerWorkPrepareCreationStore(t, settings.ContractManagerSettings.OriginalContractCapture.Directory, scope)
			transport := &providerWorkPublisherOob{ctx: ctx, clientId: id, directory: settings.ContractManagerSettings.OriginalContractCapture.Directory, settings: settings.ContractManagerSettings}
			client := connect.NewClient(ctx, connect.Id(id), transport, settings)
			clients, transports = append(clients, client), append(transports, transport)
			identity, err := client.ContractManager().OriginalWorkIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f.authority.Owners = append(f.authority.Owners, payoutartifact.WholeWorkOwner{ClientId: identity.ClientId, NetworkId: [16]byte(networkId), Generation: identity.Generation, PublicKey: identity.PublicKey})
			provider := providerWorkRetainFixtureWallet(t, f.cfg, f.authority.Domain, f.epoch.Epoch, f.epoch.Start.Block, f.epoch.StartTime, identity.ClientId, [16]byte(networkId))
			f.authority.ExpectedProviders = append(f.authority.ExpectedProviders, provider)
			cut := providerWorkRetainActualCut(t, f, client.ContractManager(), "start")
			if !cut.Complete || len(cut.Contracts) != 0 {
				t.Fatal("actual start cut is not empty")
			}
		}
		destination := clients[1].ContractManager()
		destination.LoadProvideSecretKeys(map[protocol.ProvideMode][]byte{protocol.ProvideMode_Network: bytes.Repeat([]byte{167}, 32)})
		provided := make(chan error, 1)
		destination.SetProvideModesWithOobAckCallback(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}, func(err error) { provided <- err })
		select {
		case err := <-provided:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionCtx := ctx
		if missingSessionOriginal {
			connectionCtx = model.WithProviderWorkSessionSource(ctx, nil)
		}
		for index, id := range []server.Id{{1}, {2}} {
			if _, _, _, _, err := model.ConnectNetworkClientWithIpFamily(connectionCtx, id, fmt.Sprintf("192.0.2.%d:12001", 20+index), handlerId, 4); err != nil {
				t.Fatal(err)
			}
		}
		contractKey := connect.ContractKey{Destination: connect.DestinationId(clients[1].ClientId()), IntermediaryIds: connect.RequireMultiHopId(clients[2].ClientId())}
		manager := clients[0].ContractManager()
		manager.CreateContract(contractKey, 0, 121)
		contract := manager.TakeContract(ctx, contractKey, 0)
		if contract == nil {
			t.Fatal("actual SDK did not admit its Server contract")
		}
		var stored protocol.StoredContract
		if err := proto.Unmarshal(contract.StoredContractBytes, &stored); err != nil {
			t.Fatal(err)
		}
		if len(stored.StreamId) != 16 || contract.ProvideMode != protocol.ProvideMode_Network {
			t.Fatal("actual ingress missed private stream", &stored)
		}
		if !destination.Verify(contract.StoredContractHmac, contract.StoredContractBytes, contract.ProvideMode) {
			t.Fatal("actual destination refused original reservation")
		}
		contractId := connect.RequireIdFromBytes(stored.ContractId)
		// Close the manager's admission before exercising its supported cleanup
		// delivery. Both exact SDK-generated reports reach actual Server ingress.
		for _, client := range clients[:2] {
			client.ContractManager().Close()
			client.ContractManager().CloseContract(contractId, 121, 0)
		}
		for _, transport := range transports {
			transport.stateLock.Lock()
			err := transport.err
			transport.stateLock.Unlock()
			if err != nil {
				t.Fatal("actual control ingress failed", err)
			}
		}
		// SQL events use microseconds while these original headers use seconds.
		// Wait only for the next clock boundary, never for a goroutine outcome.
		end := server.NowUtc().Truncate(time.Second).Add(time.Second)
		timer := time.NewTimer(time.Until(end))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		providerWorkSetClock(t, f, start, end)
		for index, client := range clients {
			cut := providerWorkRetainActualCut(t, f, client.ContractManager(), "end")
			if !cut.Complete {
				t.Fatal("actual end cut incomplete")
			}
			if index < 2 && len(cut.Contracts) != 1 || index == 2 && len(cut.Contracts) != 0 {
				t.Fatal("actual SDK inventory differs from ingress", index, len(cut.Contracts))
			}
			if index == 0 {
				admission, err := protocol.DecodeOriginalContractAdmission(ctx, cut.Contracts[0].OriginalCreation)
				if err != nil {
					t.Fatal(err)
				}
				transports[0].stateLock.Lock()
				matches := bytes.Equal(admission.Request, transports[0].request) && bytes.Equal(admission.ResultFrame, transports[0].reply)
				transports[0].stateLock.Unlock()
				if !matches {
					t.Fatal("actual SDK cut lost its durable request or actual Server reply")
				}
			}
		}
		f.retainAuthority(t)
		originals, err := model.ListProviderWorkOriginals(ctx, []server.Id{server.Id(contractId)})
		if err != nil || !missingSessionOriginal && len(originals) < 6 {
			t.Fatal("live producer originals incomplete", len(originals), err)
		}
		client := &providerWorkRosterClient{StClient: newStubStClient(&StEpochState{})}
		if missingSessionOriginal {
			if _, _, err := stComputeReleasePayout(ctx, f.cfg, client, f.epoch.Epoch, start, end, f.epoch.Start.Block, f.epoch.End.Block, f.epoch); !errors.Is(err, payoutartifact.ErrClosedWorkUnavailable) {
				t.Fatal("actual publisher did not retain missing live originals as pending", err)
			}
			if record := model.GetStPayoutArtifact(ctx, f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId); record != nil {
				t.Fatal("missing live original published an immutable artifact")
			}
			return
		}
		if _, _, err := stComputeReleasePayout(ctx, f.cfg, client, f.epoch.Epoch, start, end, f.epoch.Start.Block, f.epoch.End.Block, f.epoch); err != nil {
			t.Fatal("actual nonempty original payout did not publish", err)
		}
		record := model.GetStPayoutArtifact(ctx, f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId)
		if record == nil {
			t.Fatal("actual nonempty payout artifact absent")
		}
		store, ok := server.LoadBlobStore()
		if !ok {
			t.Fatal("actual blob store absent")
		}
		artifact, raw, err := startifact.Read(ctx, store, record.ContentHash)
		if err != nil {
			t.Fatal(err)
		}
		if artifact.ClosedWork == nil || artifact.ClosedWork.WholeInventory == nil || len(artifact.ClosedWork.Records) != 1 {
			t.Fatal("actual publication lost nonempty work")
		}
		if len(artifact.ClosedWork.WholeInventory.AttributionOriginals) != len(originals) {
			t.Fatal("actual publication omitted retained participant originals")
		}
		approved, err := stLoadProviderWorkAuthority(ctx, f.cfg, f.epoch)
		if err != nil || approved == nil {
			t.Fatal(err)
		}
		expected := approved.Expectation
		verified, err := payoutartifact.VerifyWholeWorkInventory(ctx, artifact, expected)
		if err != nil || verified == nil || !verified.Complete || !verified.AttributionComplete || verified.Credited != 1 {
			t.Fatal("actual public payout did not prove complete nonempty attribution", verified, err)
		}
		if len(verified.ExpectedProviders) != 3 || verified.ExpectedProviders[0].UsageBytes != 0 || verified.ExpectedProviders[1].UsageBytes != 61 || verified.ExpectedProviders[2].UsageBytes != 60 {
			t.Fatal("original source/destination/stream earning parties differ", verified.ExpectedProviders)
		}
		digest, err := providerWorkPolicyHash(strings.TrimPrefix(record.ContentHash, "sha256:"))
		if err != nil {
			t.Fatal(err)
		}
		companion, err := ProviderWorkWindow(ctx, f.domain, f.epoch.Epoch, digest, [32]byte{})
		if err != nil {
			t.Fatal("actual public companion failed", err)
		}
		witness, err := payoutartifact.DecodeWholeWorkInventory(ctx, companion)
		if err != nil {
			t.Fatal(err)
		}
		public, err := payoutartifact.VerifyWholeWorkInventoryWithWitness(ctx, artifact, witness, expected)
		if err != nil || public == nil || !public.Complete || !public.AttributionComplete {
			t.Fatal("public original companion lost complete attribution", err)
		}
		if _, _, err := stComputeReleasePayout(ctx, f.cfg, client, f.epoch.Epoch, time.Time{}, time.Time{}, 0, 0, nil); err != nil {
			t.Fatal("published restart requested new evidence", err)
		}
		_, after, err := startifact.Read(ctx, store, record.ContentHash)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatal("restart changed original signed artifact", err)
		}
		for _, continuation := range continuations {
			continuation(t, ctx, f, artifact)
		}
		witness.AttributionOriginals = nil
		missing, err := payoutartifact.VerifyWholeWorkInventoryWithWitness(ctx, artifact, witness, expected)
		if err == nil && missing != nil && missing.AttributionComplete {
			t.Fatal("omitted original pool retained full attribution")
		}
	})
}
