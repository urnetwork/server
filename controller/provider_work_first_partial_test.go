//go:build linux || darwin || freebsd

// Two genuine SDK closes cross one independently prepared earning boundary.
// Full original epoch evidence survives selection, signing and public readback.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/payoutartifact"
	snprotocol "github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// The cutoff is the second actual immutable close instant, prepared before the
// first earning-policy admission. Neither SQL originals nor their clocks move.
func TestProviderWorkFirstPartialSdkEpochPublishesOnlyNewEarnings(t *testing.T) {
	providerWorkFirstPartialRun(t, false)
}

// The same epoch with the second earning provider paid through its network's
// consent: the roster pins no provider chain for it and the network chain for
// its network. The leaf pays the network coldkey and the retained resolution
// records the network mode.
func TestProviderWorkFirstPartialSdkEpochPaysNetworkConsent(t *testing.T) {
	providerWorkFirstPartialRun(t, true)
}

// With networkMode, client 3 earns through the network consent.
func providerWorkFirstPartialRun(t *testing.T, networkMode bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		// Match the synthetic deployment identity in controllerPayoutSchedule.
		// Admission below requires a separate synthetic reviewed declaration;
		// this fixture supplies no production identity or operator send.
		f.cfg.Profile, f.cfg.ChainId, f.cfg.Netuid = "mainnet", 964, 25
		copy(f.cfg.GenesisHash[:], bytes.Repeat([]byte{0x11}, 32))
		f.authority.Domain.ChainID, f.authority.Domain.Netuid, f.authority.Domain.GenesisHash = f.cfg.ChainId, uint16(f.cfg.Netuid), f.cfg.GenesisHash
		var err error
		f.domain, err = f.authority.Domain.Digest()
		if err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithTimeout(t.Context(), 90*time.Second)
		defer stop()
		start := server.NowUtc().Truncate(time.Second).Add(-time.Hour)
		providerWorkSetClock(t, f, start, start.Add(2*time.Hour))
		signer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{181}, 32))
		sourceAuthority := snprotocol.ProviderWorkSourceAuthority{DomainHash: f.domain, SourceId: server.Id{182}.String(), Generation: server.Id{183}.String(), PublicKey: [32]byte(signer.Public().(ed25519.PublicKey)), FromUnixMicro: start.Add(-time.Hour).UnixMicro(), ThroughUnixMicro: start.Add(3 * time.Hour).UnixMicro(), MaxEndpointEvents: 4096, MaxCohortMembers: 64, DirectoryPublicKeys: [][32]byte{}}
		source, err := model.NewProviderWorkSessionSource(sourceAuthority, signer)
		if err != nil {
			t.Fatal(err)
		}
		ctx = model.WithProviderWorkSessionSource(ctx, source)
		f.authority.WorkSources = []snprotocol.ProviderWorkSourceAuthority{sourceAuthority}
		root := crypto.PubkeyToAddress(f.cfg.RootKey.PublicKey)
		t.Cleanup(server.Vault.PushSimpleResource("provider_work.yml", []byte(fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\nauthority_signer: %s\nclient_key_root_signer: %s\nattribution_signer: %s\n", ProviderWorkPolicySchema, f.domain, f.authority.RequestPublicKey, root.Hex(), root.Hex(), root.Hex()))))
		networkId := server.Id{11}
		model.Testing_CreateNetwork(ctx, networkId, "first-partial-publisher.example", server.NewId())
		var networkColdkey [32]byte
		if networkMode {
			var networkWallet payoutartifact.WholeWorkNetworkWallet
			networkWallet, networkColdkey = providerWorkRetainFixtureNetworkWallet(t, f.cfg, f.authority.Domain, f.epoch.Epoch, f.epoch.Start.Block, start, [16]byte(networkId))
			f.authority.NetworkWallets = []payoutartifact.WholeWorkNetworkWallet{networkWallet}
		}
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
			seed := bytes.Repeat([]byte{byte(184 + index)}, 32)
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
			settings.ContractManagerSettings.StandardContractTransferByteCount = 122
			scope := connect.OriginalContractStoreScope{DomainHash: f.domain, ClientId: [16]byte(id), PublicKey: [32]byte(key.Public().(ed25519.PublicKey)), SourceGeneration: [16]byte{byte(194 + index)}}
			settings.ContractManagerSettings.OriginalContractCapture = &connect.OriginalContractCaptureSettings{Directory: filepath.Join(providerWorkPhysicalTempDir(t), "requests"), PublicKey: scope.PublicKey, SourceGeneration: scope.SourceGeneration}
			providerWorkPrepareCreationStore(t, settings.ContractManagerSettings.OriginalContractCapture.Directory, scope)
			transport := &providerWorkPublisherOob{ctx: ctx, clientId: id, directory: settings.ContractManagerSettings.OriginalContractCapture.Directory, settings: settings.ContractManagerSettings}
			client := connect.NewClient(ctx, connect.Id(id), transport, settings)
			clients, transports = append(clients, client), append(transports, transport)
			identity, err := client.ContractManager().OriginalWorkIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f.authority.Owners = append(f.authority.Owners, payoutartifact.WholeWorkOwner{ClientId: identity.ClientId, NetworkId: [16]byte(networkId), Generation: identity.Generation, PublicKey: identity.PublicKey})
			if networkMode && index == 2 {
				f.authority.ExpectedProviders = append(f.authority.ExpectedProviders, payoutartifact.WholeWorkExpectedProvider{ClientId: identity.ClientId, NetworkId: [16]byte(networkId)})
			} else {
				f.authority.ExpectedProviders = append(f.authority.ExpectedProviders, providerWorkRetainFixtureWallet(t, f.cfg, f.authority.Domain, f.epoch.Epoch, f.epoch.Start.Block, start, identity.ClientId, [16]byte(networkId)))
			}
			cut := providerWorkRetainActualCut(t, f, client.ContractManager(), "start")
			if !cut.Complete || len(cut.Contracts) != 0 {
				t.Fatal("actual first-partial start cut is not empty")
			}
		}
		destination := clients[1].ContractManager()
		destination.LoadProvideSecretKeys(map[protocol.ProvideMode][]byte{protocol.ProvideMode_Network: bytes.Repeat([]byte{187}, 32)})
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
		for index, id := range []server.Id{{1}, {2}} {
			if _, _, _, _, err := model.ConnectNetworkClientWithIpFamily(ctx, id, fmt.Sprintf("192.0.2.%d:12001", 40+index), handlerId, 4); err != nil {
				t.Fatal(err)
			}
		}
		type originalClose struct {
			id                    server.Id
			request, reply, usage []byte
			closed                time.Time
		}
		originals := make([]originalClose, 2)
		manager := clients[0].ContractManager()
		contractKey := connect.ContractKey{Destination: connect.DestinationId(clients[1].ClientId()), IntermediaryIds: connect.RequireMultiHopId(clients[2].ClientId())}
		for index := range originals {
			manager.CreateContract(contractKey, uint64(index), connect.ByteCount(121+index))
			contract := manager.TakeContract(ctx, contractKey, -1)
			if contract == nil {
				t.Fatal("actual SDK first-partial contract admission absent", index)
			}
			var stored protocol.StoredContract
			if err := proto.Unmarshal(contract.StoredContractBytes, &stored); err != nil {
				t.Fatal(err)
			}
			// The actual server retains its own minimum and two-hop reservation;
			// only the later completed-byte reports select 119 and 121 earnings.
			if len(stored.StreamId) != 16 || stored.TransferByteCount < uint64(121+index) || stored.TransferByteCount > 2*uint64(MaxContractTransferByteCount) || contract.ProvideMode != protocol.ProvideMode_Network || !destination.Verify(contract.StoredContractHmac, contract.StoredContractBytes, contract.ProvideMode) {
				t.Fatal("actual first-partial stream reservation refused", index)
			}
			originals[index].id = server.Id(connect.RequireIdFromBytes(stored.ContractId))
			transports[0].stateLock.Lock()
			originals[index].request, originals[index].reply = bytes.Clone(transports[0].request), bytes.Clone(transports[0].reply)
			transports[0].stateLock.Unlock()
		}
		if originals[0].id == originals[1].id {
			t.Fatal("two SDK admissions reused one original contract")
		}
		// Closing admission selects the supported synchronous cleanup delivery.
		// Every report and terminal time still comes from the production ingress.
		for _, client := range clients[:2] {
			client.ContractManager().Close()
		}
		for index := range originals {
			for _, client := range clients[:2] {
				client.ContractManager().CloseContract(connect.Id(originals[index].id), connect.ByteCount(119+2*index), 0)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var outcome string
				server.Raise(conn.QueryRow(ctx, `SELECT outcome,close_time,provider_usage FROM transfer_contract WHERE contract_id=$1`, originals[index].id).Scan(&outcome, &originals[index].closed, &originals[index].usage))
				if outcome != "settled" || len(originals[index].usage) == 0 {
					t.Fatal("actual SDK reports did not settle immutable original", index, outcome)
				}
			})
		}
		for _, transport := range transports {
			transport.stateLock.Lock()
			err := transport.err
			transport.stateLock.Unlock()
			if err != nil {
				t.Fatal("actual first-partial control ingress failed", err)
			}
		}
		cutoff := originals[1].closed
		if !start.Before(originals[0].closed) || !originals[0].closed.Before(cutoff) {
			t.Fatal("serial original closes did not bracket the synthetic cutoff")
		}
		controllerPayoutSchedule(t, cutoff)
		if err := stPayoutAdmission(ctx, f.cfg); err == nil || !strings.Contains(err.Error(), "mainnet deployment readiness is blocked") {
			t.Fatal("unreviewed first-partial fixture unexpectedly acquired mainnet admission", err)
		}
		// The readiness declaration changes admission, not the already prepared
		// earning boundary. Bind it to this fixture's exact synthetic identity.
		policy, err := server.LoadProviderPayoutEarningPolicy(ctx)
		if err != nil || policy == nil {
			t.Fatal("first-partial earning policy is unavailable", err)
		}
		f.cfg.LaunchReadinessSha256 = strings.Repeat("ab", 32)
		policy.Mainnet = stPayoutIdentity(f.cfg)
		policy.Mainnet.Activation = "reviewed"
		declaration, err := yaml.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(server.Config.PushSimpleResource("sn.yml", declaration))
		if err := stPayoutAdmission(ctx, f.cfg); err != nil {
			t.Fatal("synthetic reviewed first-partial declaration was not admitted", err)
		}
		// Only the original header clock needs the next full second. This wait
		// does not establish completion, which the synchronous reads proved above.
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
			if !cut.Complete || index < 2 && len(cut.Contracts) != 2 || index == 2 && len(cut.Contracts) != 0 {
				t.Fatal("actual first-partial end cut omitted original work", index)
			}
			if index == 0 {
				for _, record := range cut.Contracts {
					admission, err := protocol.DecodeOriginalContractAdmission(ctx, record.OriginalCreation)
					if err != nil {
						t.Fatal(err)
					}
					matched := false
					for _, original := range originals {
						if record.ContractId == [16]byte(original.id) {
							matched = bytes.Equal(admission.Request, original.request) && bytes.Equal(admission.ResultFrame, original.reply)
						}
					}
					if !matched {
						t.Fatal("first-partial cut replaced a pre-send request or server reply")
					}
				}
			}
		}
		f.retainAuthority(t)
		// Independently seeded verification exposure is wholly after cutoff;
		// this fixture exercises usage custody, not challenge execution.
		model.UpsertVerifyProviderStats(ctx, []*model.VerifyProviderStatsRow{{PeriodStart: cutoff, PeriodEnd: end, ClientId: server.Id{2}, Assignments: 8, Confirmations: 8}, {PeriodStart: cutoff, PeriodEnd: end, ClientId: server.Id{3}, Assignments: 8, Confirmations: 8}})
		client := &providerWorkRosterClient{StClient: newStubStClient(&StEpochState{})}
		_, leaves, err := stComputeReleasePayout(ctx, f.cfg, client, f.epoch.Epoch, start, end, f.epoch.Start.Block, f.epoch.End.Block, f.epoch)
		if err != nil || leaves != 2 {
			t.Fatal("actual first-partial original payout did not publish two earning leaves", leaves, err)
		}
		record := model.GetStPayoutArtifact(ctx, f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId)
		store, ok := server.LoadBlobStore()
		if !ok || record == nil {
			t.Fatal("first-partial immutable publication absent")
		}
		artifact, raw, err := startifact.Read(ctx, store, record.ContentHash)
		if err != nil {
			t.Fatal(err)
		}
		census := artifact.ClosedWork
		if census == nil || census.WholeInventory == nil || len(census.Records) != 2 || census.Count != 2 || len(census.WholeInventory.Window.Records) != 2 || census.WindowStart != start.Format(time.RFC3339Nano) || census.WindowEnd != end.Format(time.RFC3339Nano) {
			t.Fatal("publisher clipped the original first-partial epoch census")
		}
		for _, original := range originals {
			matched := false
			for _, row := range census.Records {
				if row.ContractId == [16]byte(original.id) {
					matched = bytes.Equal(row.Original, original.usage) && row.ClosedAt == original.closed.Format(time.RFC3339Nano)
				}
			}
			if !matched {
				t.Fatal("first-partial selection omitted or reconstructed original settlement", original.id)
			}
		}
		approved, _, err := stLoadProviderWorkAuthority(ctx, f.cfg, f.epoch)
		if err != nil || approved == nil || approved.Expectation.EarningSelection == nil {
			t.Fatal("independently prepared earning selection unavailable", err)
		}
		expected := approved.Expectation
		selection := expected.EarningSelection
		if !selection.StartTime.Equal(cutoff) || census.EarningStart != cutoff.Format(time.RFC3339Nano) || census.EarningSelectionHash != selection.PolicyHash {
			t.Fatal("publication borrowed or changed the independent earning boundary")
		}
		verified, err := payoutartifact.VerifyWholeWorkInventory(ctx, artifact, expected)
		if err != nil || verified == nil || !verified.Complete || !verified.AttributionComplete || verified.Contracts != 2 || verified.Credited != 2 || len(verified.ReconciledContracts) != 2 || verified.Reports == nil || verified.Reports.ClosedWork.UsageBytes != 121 {
			t.Fatal("first-partial public verifier lost full originals or exact new earnings", verified, err)
		}
		if len(artifact.Providers) != 3 || artifact.Providers[0].UsageBytes != 0 || artifact.Providers[1].UsageBytes != 61 || artifact.Providers[2].UsageBytes != 60 || artifact.TotalUsageBytes != 121 || artifact.EligibleUsageBytes != 121 || len(artifact.Leaves) != 2 || artifact.Leaves[0].ClientID != ([16]byte{2}) || artifact.Leaves[1].ClientID != ([16]byte{3}) || artifact.Leaves[0].ShareBPS != 5041 || artifact.Leaves[1].ShareBPS != 4959 {
			t.Fatal("pre-cutoff completed work changed first-partial payout leaves", artifact.Providers, artifact.Leaves)
		}
		if err := payoutartifact.VerifyWithContext(ctx, artifact); err != nil {
			t.Fatal("public first-partial artifact signature or payout proof failed", err)
		}
		resolutions := model.GetStPayoutWalletResolutions(ctx, f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId)
		if len(resolutions) != 3 {
			t.Fatal("the publication did not retain every provider's wallet resolution", resolutions)
		}
		for index, resolution := range resolutions {
			mode := snprotocol.EarningWalletModeProvider
			if networkMode && index == 2 {
				mode = snprotocol.EarningWalletModeNetwork
			}
			if resolution.ClientId != (server.Id{byte(index + 1)}) || resolution.NetworkId != networkId || resolution.Mode != mode || resolution.Coldkey != artifact.Providers[index].Coldkey {
				t.Fatal("a retained resolution differs from the published provider wallet", index, resolution)
			}
		}
		if networkMode && (artifact.Leaves[1].Coldkey != networkColdkey || resolutions[2].HeadGeneration != 1 || resolutions[2].ConsentHash != resolutions[2].HeadHash) {
			t.Fatal("the network-mode provider was not paid to the network consent", artifact.Leaves[1])
		}
		if !networkMode && artifact.Leaves[1].Coldkey == networkColdkey {
			t.Fatal("provider mode paid a network coldkey")
		}
		digest, err := providerWorkPolicyHash(strings.TrimPrefix(record.ContentHash, "sha256:"))
		if err != nil {
			t.Fatal(err)
		}
		companion, err := ProviderWorkWindow(ctx, f.domain, f.epoch.Epoch, digest, [32]byte{})
		if err != nil {
			t.Fatal("first-partial exact-hash public companion failed", err)
		}
		witness, err := payoutartifact.DecodeWholeWorkInventory(ctx, companion)
		if err != nil {
			t.Fatal(err)
		}
		public, err := payoutartifact.VerifyWholeWorkInventoryWithWitness(ctx, artifact, witness, expected)
		if err != nil || public == nil || !public.Complete || !public.AttributionComplete || public.Reports == nil || public.Reports.ClosedWork.UsageBytes != 121 {
			t.Fatal("public companion reinterpreted original first-partial earnings", err)
		}
		changedExpected := expected
		changedSelection := *selection
		changedSelection.StartTime = cutoff.Add(time.Microsecond)
		changedExpected.EarningSelection = &changedSelection
		if _, err := payoutartifact.VerifyWholeWorkInventoryWithWitness(ctx, artifact, witness, changedExpected); !errors.Is(err, payoutartifact.ErrClosedWorkIntegrity) {
			t.Error("public consumer accepted a changed independent earning filter", err)
		}
		// A valid publisher signature and internally rebuilt payout leaves cannot
		// authorize an alternate policy which recredits the 119 earlier bytes.
		proposal, err := startifact.DecodeWithContext(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		policy, err = server.LoadProviderPayoutEarningPolicy(ctx)
		if err != nil || policy == nil {
			t.Fatal(err)
		}
		alternate := *policy
		alternate.Cutoff, alternate.CutoffUtc = start, start.Format(time.RFC3339Nano)
		alternateHash, err := alternate.EarningIdentitySha256()
		if err != nil {
			t.Fatal(err)
		}
		proposal.ClosedWork.EarningStart, proposal.ClosedWork.EarningSelectionHash = alternate.CutoffUtc, "sha256:"+alternateHash
		providers := append([]startifact.ProviderInput(nil), artifact.Providers...)
		providers[1].UsageBytes, providers[2].UsageBytes = 121, 119
		forged, err := startifact.BuildWithContext(ctx, startifact.BuildInput{ClosedWork: proposal.ClosedWork, DeploymentID: artifact.DeploymentID, ChainID: artifact.ChainID, GenesisHash: artifact.GenesisHash, Netuid: artifact.Netuid, Coordinator: artifact.Coordinator, SettlementVault: artifact.SettlementVault, Epoch: artifact.Epoch, NoID: artifact.NoID, PolicyHash: artifact.PolicyHash, Start: artifact.Start, End: artifact.End, OperatorSnapshotHash: artifact.OperatorSnapshotHash, FleetSnapshotHash: artifact.FleetSnapshotHash, Providers: providers, TotalUsers: artifact.TotalUsers, ReliabilityAMin: artifact.ReliabilityAMin, CreatedAt: end})
		if err != nil {
			t.Fatal(err)
		}
		if err := startifact.Sign(forged, f.cfg.ArtifactKey); err != nil {
			t.Fatal(err)
		}
		if err := payoutartifact.VerifyWithContext(ctx, forged); err != nil || forged.TotalUsageBytes != 240 || forged.PayoutRoot == artifact.PayoutRoot {
			t.Fatal("recredit control did not reach a valid changed publisher proposal", err)
		}
		if _, err := payoutartifact.VerifyWholeWorkInventoryWithWitness(ctx, forged, witness, expected); !errors.Is(err, payoutartifact.ErrClosedWorkIntegrity) {
			t.Error("publisher proposal recredited pre-cutoff work under an unapproved filter", err)
		}
		if _, _, err := stComputeReleasePayout(ctx, f.cfg, client, f.epoch.Epoch, time.Time{}, time.Time{}, 0, 0, nil); err != nil {
			t.Fatal("first-partial restart requested replacement evidence", err)
		}
		_, retained, err := startifact.Read(ctx, store, record.ContentHash)
		if err != nil || !bytes.Equal(raw, retained) {
			t.Fatal("restart or rejected proposal changed the original publication", err)
		}
	})
}
