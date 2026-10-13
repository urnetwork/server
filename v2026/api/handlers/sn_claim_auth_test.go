// Real authenticated Http requests resolve immutable provider contributions
// through Sql leaves; account wallets and aggregate representatives are decoys.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/merkle"
	"github.com/urfoundation/sn/v2026/ss58"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/startifact"
)

// Each serial fixture owns its synthetic deployment, local blobs and endpoint.
type snClaimAuthFixture struct {
	cfg      *controller.StConfig
	store    server.BlobStore
	blobRoot string
	endpoint *httptest.Server
}

// Keep the actual response bytes to check both claim fields and error framing.
type snClaimAuthResponse struct {
	statusCode int
	body       []byte
	result     controller.SnPoolClaimResult
}

// No chain client or production configuration is needed to read retained claims.
func newSnClaimAuthFixture(t testing.TB) *snClaimAuthFixture {
	t.Helper()
	artifactKey, err := crypto.HexToECDSA(strings.Repeat("31", 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &controller.StConfig{
		Enabled:         true,
		ChainId:         1945,
		GenesisHash:     [32]byte{0x11},
		DeploymentId:    "claim-auth-synthetic",
		PolicyHash:      [32]byte{0x12},
		ContractAddress: common.Address{0x21},
		SettlementVault: common.Address{0x22},
		Netuid:          7,
		NoId:            13,
		ArtifactKey:     artifactKey,
	}
	controller.SetStConfig(cfg)
	t.Cleanup(func() { controller.SetStConfig(nil) })
	blobRoot := t.TempDir()
	popBlobConfig := server.Vault.PushSimpleResource("minio.yml", []byte(fmt.Sprintf(
		"authority: local\npath: %s\nprefix: claim-auth\nmax_bytes: %d\n", blobRoot, 16*1024*1024,
	)))
	t.Cleanup(popBlobConfig)
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("synthetic claim artifact store unavailable")
	}
	endpoint := httptest.NewServer(http.HandlerFunc(SnPoolClaim))
	t.Cleanup(endpoint.Close)
	return &snClaimAuthFixture{cfg: cfg, store: store, blobRoot: blobRoot, endpoint: endpoint}
}

// Account and provider credentials have genuine active rows behind them.
func snClaimAuthAccount(t testing.TB) *session.ByJwt {
	t.Helper()
	networkId, userId := server.NewId(), server.NewId()
	networkName := "claim-auth-" + networkId.String()
	model.Testing_CreateNetwork(t.Context(), networkId, networkName, userId)
	return session.NewByJwt(networkId, userId, networkName, false, false)
}

// More than one provider may legitimately belong to the same account.
func snClaimAuthProvider(t testing.TB, account *session.ByJwt) *session.ByJwt {
	t.Helper()
	deviceId, clientId := server.NewId(), server.NewId()
	model.Testing_CreateDevice(t.Context(), account.NetworkId, deviceId, clientId, "claim-provider", "synthetic")
	return account.Client(deviceId, clientId)
}

// Ordinary rows have positive, equal reliability so usage controls their shares.
func snClaimAuthContribution(credential *session.ByJwt, coldkey [32]byte, usageBytes uint64) startifact.ProviderInput {
	return startifact.ProviderInput{
		ClientID:      [16]byte(*credential.ClientId),
		NetworkID:     [16]byte(credential.NetworkId),
		Coldkey:       coldkey,
		UsageBytes:    usageBytes,
		Assignments:   8,
		Confirmations: 8,
		Eligible:      true,
	}
}

// The current network projection is deliberately independent of payout history.
func snClaimAuthWallet(t testing.TB, account *session.ByJwt, coldkey [32]byte) string {
	t.Helper()
	encoded, err := ss58.Encode(coldkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	model.SetStWallet(t.Context(), account.NetworkId, encoded, coldkey)
	return encoded
}

// Canonical allocation, reliability and proofs are built by the production codec.
func (self *snClaimAuthFixture) build(t testing.TB, epoch uint64, providers []startifact.ProviderInput) *startifact.Artifact {
	t.Helper()
	artifact, err := startifact.Build(startifact.BuildInput{
		DeploymentID:         self.cfg.DeploymentId,
		GenesisHash:          fmt.Sprintf("0x%x", self.cfg.GenesisHash),
		PolicyHash:           fmt.Sprintf("0x%x", self.cfg.PolicyHash),
		ChainID:              self.cfg.ChainId,
		Netuid:               uint16(self.cfg.Netuid),
		Coordinator:          self.cfg.ContractAddress,
		SettlementVault:      self.cfg.SettlementVault,
		Epoch:                epoch,
		NoID:                 self.cfg.NoId,
		Start:                startifact.Boundary{Number: 100, Hash: "0x" + strings.Repeat("41", 32)},
		End:                  startifact.Boundary{Number: 200, Hash: "0x" + strings.Repeat("42", 32)},
		OperatorSnapshotHash: "sha256:" + strings.Repeat("43", 32),
		FleetSnapshotHash:    "sha256:" + strings.Repeat("44", 32),
		Providers:            providers,
		ReliabilityAMin:      8,
		CreatedAt:            time.Unix(1_700_000_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

// The retained Sql scope is explicit so foreign signed artifacts can be tested.
// A root fault is inserted originally, without bypassing the immutable-row guard.
func (self *snClaimAuthFixture) retain(t testing.TB, epoch uint64, artifact *startifact.Artifact, recordRoots ...[32]byte) *model.StPayoutArtifact {
	t.Helper()
	if err := startifact.Sign(artifact, self.cfg.ArtifactKey); err != nil {
		t.Fatal(err)
	}
	published, err := startifact.Publish(t.Context(), self.store, artifact)
	if err != nil {
		t.Fatal(err)
	}
	model.UpsertStEpoch(t.Context(), self.cfg.DeploymentKey(), &model.StEpoch{
		Epoch: epoch, StartBlock: 100, CommitDeadlineBlock: 200, TrailsDeadlineBlock: 210, FinalizeBlock: 220, Status: model.StEpochStatusFinalized,
	})
	clientIdNetworkIds := map[[16]byte]server.Id{}
	for _, provider := range artifact.Providers {
		clientIdNetworkIds[provider.ClientID] = server.Id(provider.NetworkID)
	}
	leaves := make([]*model.StPayoutLeaf, len(artifact.Leaves))
	for i, leaf := range artifact.Leaves {
		clientId := server.Id(leaf.ClientID)
		leaves[i] = &model.StPayoutLeaf{
			Epoch: epoch, NoId: self.cfg.NoId, ClientId: &clientId, NetworkId: clientIdNetworkIds[leaf.ClientID],
			Coldkey: leaf.Coldkey, ShareBps: int(leaf.ShareBPS), LeafIndex: i,
		}
	}
	model.SetStPayoutLeaves(t.Context(), self.cfg.DeploymentKey(), epoch, self.cfg.NoId, leaves)
	record := &model.StPayoutArtifact{
		Epoch: epoch, NoId: self.cfg.NoId, ContentHash: published.ContentHash, ContentKey: published.ContentKey,
		HistoryKey: published.HistoryKey, PayoutRoot: artifact.PayoutRoot, CreateTime: time.Unix(1_700_000_000, 0).UTC(),
	}
	if len(recordRoots) != 0 {
		if len(recordRoots) != 1 {
			t.Fatal("fixture accepts exactly one alternate retained root")
		}
		record.PayoutRoot = recordRoots[0]
	}
	model.AddStPayoutArtifact(t.Context(), self.cfg.DeploymentKey(), record)
	return record
}

// Every request traverses the actual handler and authentication with a deadline.
func (self *snClaimAuthFixture) request(t testing.TB, token string, query url.Values) snClaimAuthResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, self.endpoint.URL+"/sn/pool/claim?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := self.endpoint.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || len(body) > 64*1024 {
		t.Fatalf("claim response read: %v, close: %v, bytes: %d", readErr, closeErr, len(body))
	}
	result := snClaimAuthResponse{statusCode: response.StatusCode, body: body}
	if response.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &result.result); err != nil {
			t.Fatalf("claim response is not the Json wire shape: %v", err)
		}
	}
	return result
}

// Expected claims agree with the original signed tree and the public byte shape.
func snClaimAuthAssertClaim(t testing.TB, response snClaimAuthResponse, artifact *startifact.Artifact, coldkey [32]byte) {
	t.Helper()
	if response.statusCode != http.StatusOK || response.result.Error != nil {
		t.Fatalf("owned claim rejected: status=%d body=%s", response.statusCode, response.body)
	}
	var expected *startifact.Leaf
	for i := range artifact.Leaves {
		if artifact.Leaves[i].Coldkey == coldkey {
			expected = &artifact.Leaves[i]
			break
		}
	}
	if expected == nil || expected.ShareBPS == 0 {
		t.Fatal("fixture has no positive expected payout leaf")
	}
	artifactUri := ""
	if artifact.ContentHash != "" {
		artifactUri = "/sn/artifact?hash=" + artifact.ContentHash
	}
	result := response.result
	if result.Epoch != artifact.Epoch || len(result.NoId) != 32 || new(big.Int).SetBytes(result.NoId).Cmp(new(big.Int).SetUint64(artifact.NoID)) != 0 ||
		!bytes.Equal(result.Coldkey, coldkey[:]) || result.ShareBps != int(expected.ShareBPS) || !bytes.Equal(result.PayoutRoot, artifact.PayoutRoot[:]) ||
		result.ClaimOpenBlock != 220 || result.ContractAddress != artifact.SettlementVault.Hex() || result.SettlementVaultAddress != artifact.SettlementVault.Hex() ||
		result.ChainId != artifact.ChainID || result.ArtifactHash != artifact.ContentHash || result.ArtifactUri != artifactUri {
		t.Fatalf("claim differs from the original leaf or domain: %s", response.body)
	}
	if result.Proof == nil || len(result.Proof) != len(expected.Proof) {
		t.Fatalf("proof array shape differs: %s", response.body)
	}
	proof := make([][32]byte, len(result.Proof))
	for i, node := range result.Proof {
		if len(node) != 32 || !bytes.Equal(node, expected.Proof[i][:]) {
			t.Fatalf("proof node %d differs from retained evidence", i)
		}
		copy(proof[i][:], node)
	}
	if !merkle.Verify(artifact.PayoutRoot, merkle.PayoutLeaf(coldkey, big.NewInt(int64(result.ShareBps))), proof) {
		t.Fatal("public claim proof does not verify against its original root")
	}
}

// Held claims expose an error and no executable payout fields.
func snClaimAuthAssertHeld(t testing.TB, response snClaimAuthResponse) {
	t.Helper()
	result := response.result
	if response.statusCode != http.StatusOK || result.Error == nil || !strings.Contains(result.Error.Message, "no owned payout leaf") ||
		result.ShareBps != 0 || len(result.NoId) != 0 || len(result.Coldkey) != 0 || len(result.Proof) != 0 || len(result.PayoutRoot) != 0 ||
		result.ContractAddress != "" || result.SettlementVaultAddress != "" || result.ChainId != 0 || result.ClaimOpenBlock != 0 {
		t.Fatalf("unowned claim was not held without payout fields: status=%d body=%s", response.statusCode, response.body)
	}
}

// Same-account providers keep their own epoch wallets after the account rotates.
func TestSnPoolClaimProviderWalletsRemainDistinctAfterNetworkWalletRotation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		fixture := newSnClaimAuthFixture(tb)
		account := snClaimAuthAccount(tb)
		first, second := snClaimAuthProvider(tb, account), snClaimAuthProvider(tb, account)
		firstColdkey, secondColdkey := [32]byte{0x51}, [32]byte{0x52}
		artifact := fixture.build(tb, 0, []startifact.ProviderInput{
			snClaimAuthContribution(first, firstColdkey, 1), snClaimAuthContribution(second, secondColdkey, 3),
		})
		fixture.retain(tb, 0, artifact)
		firstToken, secondToken, accountToken := first.Testing_Sign(), second.Testing_Sign(), account.Testing_Sign()
		for _, currentColdkey := range [][32]byte{firstColdkey, secondColdkey, {0x53}} {
			snClaimAuthWallet(tb, account, currentColdkey)
			for _, value := range []struct {
				token   string
				coldkey [32]byte
			}{{token: firstToken, coldkey: firstColdkey}, {token: secondToken, coldkey: secondColdkey}} {
				response := fixture.request(tb, value.token, url.Values{"epoch": {"0"}})
				snClaimAuthAssertClaim(tb, response, artifact, value.coldkey)
			}
			snClaimAuthAssertHeld(tb, fixture.request(tb, accountToken, url.Values{"epoch": {"0"}}))
		}
		snClaimAuthAssertClaim(tb, fixture.request(tb, firstToken, nil), artifact, firstColdkey)
	})
}

// Sharing a paid coldkey cannot turn an absent or excluded contribution into work.
func TestSnPoolClaimProviderRequiresEligibleOriginalContribution(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		fixture := newSnClaimAuthFixture(tb)
		account, otherAccount := snClaimAuthAccount(tb), snClaimAuthAccount(tb)
		paid := snClaimAuthProvider(tb, account)
		paidColdkey := [32]byte{0x61}
		originalColdkey := snClaimAuthWallet(tb, account, paidColdkey)
		providers := []startifact.ProviderInput{snClaimAuthContribution(paid, paidColdkey, 8)}
		type rejectedProvider struct {
			fault      string
			credential *session.ByJwt
		}
		rejectedProviders := []rejectedProvider{}
		for _, fault := range []string{"unknown", "ineligible", "head-excluded", "zero-usage", "zero-reliability", "wrong-network"} {
			credential := snClaimAuthProvider(tb, account)
			rejectedProviders = append(rejectedProviders, rejectedProvider{fault: fault, credential: credential})
			provider := snClaimAuthContribution(credential, paidColdkey, 1)
			switch fault {
			case "unknown":
				continue
			case "ineligible":
				provider.Eligible = false
			case "head-excluded":
				provider.HeadExcluded = true
			case "zero-usage":
				provider.UsageBytes = 0
			case "zero-reliability":
				provider.Confirmations = 0
			case "wrong-network":
				provider.NetworkID = [16]byte(otherAccount.NetworkId)
			}
			providers = append(providers, provider)
		}
		artifact := fixture.build(tb, 7, providers)
		fixture.retain(tb, 7, artifact)
		snClaimAuthAssertClaim(tb, fixture.request(tb, paid.Testing_Sign(), url.Values{"epoch": {"7"}}), artifact, paidColdkey)
		for _, rejected := range rejectedProviders {
			tb.Logf("checking original contribution refusal: %s", rejected.fault)
			token := rejected.credential.Testing_Sign()
			snClaimAuthAssertHeld(tb, fixture.request(tb, token, url.Values{"epoch": {"7"}}))
			snClaimAuthAssertHeld(tb, fixture.request(tb, token, url.Values{"epoch": {"7"}, "legacy_coldkey": {originalColdkey}}))
		}
	})
}

// Two independently authenticated networks may share one coldkey aggregate even
// when a third contributing network is its informational Sql representative.
func TestSnPoolClaimSharedColdkeyContributorsIgnoreAggregateRepresentative(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		fixture := newSnClaimAuthFixture(tb)
		sharedColdkey := [32]byte{0x71}
		credentials := []*session.ByJwt{}
		providers := []startifact.ProviderInput{}
		for i := 0; i < 3; i++ {
			account := snClaimAuthAccount(tb)
			credential := snClaimAuthProvider(tb, account)
			credentials = append(credentials, credential)
			providers = append(providers, snClaimAuthContribution(credential, sharedColdkey, uint64(i+1)))
			snClaimAuthWallet(tb, account, [32]byte{byte(0x72 + i)})
		}
		artifact := fixture.build(tb, 8, providers)
		fixture.retain(tb, 8, artifact)
		leaves := model.GetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), 8, fixture.cfg.NoId)
		if len(leaves) != 1 || leaves[0].ClientId == nil || leaves[0].ShareBps != 10_000 {
			tb.Fatal("fixture did not retain one provider aggregate")
		}
		otherContributors := 0
		for _, credential := range credentials {
			if *credential.ClientId == *leaves[0].ClientId {
				continue
			}
			if credential.NetworkId == leaves[0].NetworkId {
				tb.Fatal("claimant unexpectedly belongs to representative network")
			}
			snClaimAuthAssertClaim(tb, fixture.request(tb, credential.Testing_Sign(), url.Values{"epoch": {"8"}}), artifact, sharedColdkey)
			otherContributors++
		}
		if otherContributors != 2 {
			tb.Fatalf("expected two other contributing networks, got %d", otherContributors)
		}
	})
}

// The signed provider row cannot fall back to a different paid coldkey if its
// own Sql projection is missing, even when that key is the current account wallet.
func TestSnPoolClaimProviderMissingOwnedLeafIsHeld(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		fixture := newSnClaimAuthFixture(tb)
		account := snClaimAuthAccount(tb)
		owner, other := snClaimAuthProvider(tb, account), snClaimAuthProvider(tb, account)
		ownerColdkey, otherColdkey := [32]byte{0x81}, [32]byte{0x82}
		snClaimAuthWallet(tb, account, otherColdkey)
		artifact := fixture.build(tb, 9, []startifact.ProviderInput{
			snClaimAuthContribution(owner, ownerColdkey, 1), snClaimAuthContribution(other, otherColdkey, 1),
		})
		fixture.retain(tb, 9, artifact)
		token := owner.Testing_Sign()
		snClaimAuthAssertClaim(tb, fixture.request(tb, token, url.Values{"epoch": {"9"}}), artifact, ownerColdkey)
		leaves := model.GetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), 9, fixture.cfg.NoId)
		retainedLeaves := []*model.StPayoutLeaf{}
		for _, leaf := range leaves {
			if leaf.Coldkey != ownerColdkey {
				retainedLeaves = append(retainedLeaves, leaf)
			}
		}
		model.SetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), 9, fixture.cfg.NoId, retainedLeaves)
		snClaimAuthAssertHeld(tb, fixture.request(tb, token, url.Values{"epoch": {"9"}}))
	})
}

// Legacy account reads explicitly select public committed bytes. The selector
// survives wallet rotation and conveys no historical ownership of the account.
func TestSnPoolClaimLegacyRequiresExplicitOriginalColdkeyAndNetworkOnlyLeaves(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		fixture := newSnClaimAuthFixture(tb)
		account, otherAccount := snClaimAuthAccount(tb), snClaimAuthAccount(tb)
		provider, otherProvider := snClaimAuthProvider(tb, account), snClaimAuthProvider(tb, otherAccount)
		originalColdkey, otherColdkey := [32]byte{0x91}, [32]byte{0x92}
		originalSelector := snClaimAuthWallet(tb, account, originalColdkey)
		rotatedSelector := snClaimAuthWallet(tb, account, [32]byte{0x93})
		artifact := fixture.build(tb, 10, []startifact.ProviderInput{
			snClaimAuthContribution(provider, originalColdkey, 3), snClaimAuthContribution(otherProvider, otherColdkey, 2),
		})
		// This pre-artifact compatibility state intentionally retains no original.
		model.UpsertStEpoch(tb.Context(), fixture.cfg.DeploymentKey(), &model.StEpoch{
			Epoch: 10, StartBlock: 100, CommitDeadlineBlock: 200, TrailsDeadlineBlock: 210, FinalizeBlock: 220, Status: model.StEpochStatusFinalized,
		})
		leaves := []*model.StPayoutLeaf{}
		for i, leaf := range artifact.Leaves {
			networkId := otherAccount.NetworkId
			if leaf.Coldkey == originalColdkey {
				networkId = account.NetworkId
			}
			leaves = append(leaves, &model.StPayoutLeaf{Epoch: 10, NoId: fixture.cfg.NoId, NetworkId: networkId, Coldkey: leaf.Coldkey, ShareBps: int(leaf.ShareBPS), LeafIndex: i})
		}
		model.SetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), 10, fixture.cfg.NoId, leaves)
		// Provider evidence in another pool does not change this pool's retained
		// network-only provenance or the operator id encoded by its public proof.
		model.SetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), 10, fixture.cfg.NoId+1, []*model.StPayoutLeaf{
			{Epoch: 10, NoId: fixture.cfg.NoId + 1, ClientId: otherProvider.ClientId, NetworkId: otherAccount.NetworkId, Coldkey: otherColdkey, ShareBps: 10_000, LeafIndex: 0},
		})
		query := url.Values{"epoch": {"10"}, "legacy_coldkey": {originalSelector}}
		for _, credential := range []*session.ByJwt{account, otherAccount} {
			response := fixture.request(tb, credential.Testing_Sign(), query)
			// Legacy claims have no immutable artifact coordinates to advertise.
			if response.result.ArtifactHash != "" || response.result.ArtifactUri != "" {
				tb.Fatalf("legacy read invented an original artifact: %s", response.body)
			}
			snClaimAuthAssertClaim(tb, response, artifact, originalColdkey)
		}
		snClaimAuthAssertHeld(tb, fixture.request(tb, account.Testing_Sign(), url.Values{"epoch": {"10"}, "legacy_coldkey": {rotatedSelector}}))
		// A current wallet that does have a leaf still cannot supply the missing
		// selector or grant a provider credential legacy compatibility.
		snClaimAuthWallet(tb, account, originalColdkey)
		snClaimAuthAssertHeld(tb, fixture.request(tb, account.Testing_Sign(), url.Values{"epoch": {"10"}}))
		snClaimAuthAssertHeld(tb, fixture.request(tb, provider.Testing_Sign(), query))
		snClaimAuthAssertHeld(tb, fixture.request(tb, provider.Testing_Sign(), url.Values{"epoch": {"10"}}))

		// A provider leaf anywhere in the selected pool disables compatibility,
		// including a different coldkey from the requested network-only leaf.
		for _, leaf := range leaves {
			if leaf.Coldkey == otherColdkey {
				leaf.ClientId = otherProvider.ClientId
			}
		}
		model.SetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), 10, fixture.cfg.NoId, leaves)
		snClaimAuthAssertHeld(tb, fixture.request(tb, account.Testing_Sign(), query))
	})
}

// Presence of an original artifact disables legacy reads even when historical
// Sql representatives are nil; a provider still resolves through that original.
func TestSnPoolClaimLegacyRefusesRetainedProviderArtifact(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		fixture := newSnClaimAuthFixture(tb)
		account := snClaimAuthAccount(tb)
		provider := snClaimAuthProvider(tb, account)
		coldkey := [32]byte{0xa1}
		selector := snClaimAuthWallet(tb, account, coldkey)
		artifact := fixture.build(tb, 11, []startifact.ProviderInput{snClaimAuthContribution(provider, coldkey, 1)})
		fixture.retain(tb, 11, artifact)
		leaves := model.GetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), 11, fixture.cfg.NoId)
		for _, leaf := range leaves {
			leaf.ClientId = nil
		}
		model.SetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), 11, fixture.cfg.NoId, leaves)
		snClaimAuthAssertClaim(tb, fixture.request(tb, provider.Testing_Sign(), url.Values{"epoch": {"11"}}), artifact, coldkey)
		query := url.Values{"epoch": {"11"}, "legacy_coldkey": {selector}}
		snClaimAuthAssertHeld(tb, fixture.request(tb, account.Testing_Sign(), query))
		snClaimAuthAssertHeld(tb, fixture.request(tb, provider.Testing_Sign(), query))
	})
}

// Missing or corrupt original bytes never downgrade to account-wallet selection.
func TestSnPoolClaimArtifactReadFailureNeverFallsBackToLegacy(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		fixture := newSnClaimAuthFixture(tb)
		account := snClaimAuthAccount(tb)
		provider := snClaimAuthProvider(tb, account)
		coldkey := [32]byte{0xb1}
		selector := snClaimAuthWallet(tb, account, coldkey)
		token := provider.Testing_Sign()
		for i, fault := range []string{"missing", "corrupt"} {
			epoch := uint64(20 + i)
			artifact := fixture.build(tb, epoch, []startifact.ProviderInput{snClaimAuthContribution(provider, coldkey, 1)})
			record := fixture.retain(tb, epoch, artifact)
			leaves := model.GetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), epoch, fixture.cfg.NoId)
			for _, leaf := range leaves {
				leaf.ClientId = nil
			}
			model.SetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), epoch, fixture.cfg.NoId, leaves)
			query := url.Values{"epoch": {strconv.FormatUint(epoch, 10)}}
			snClaimAuthAssertClaim(tb, fixture.request(tb, token, query), artifact, coldkey)
			artifactPath := filepath.Join(fixture.blobRoot, filepath.FromSlash(record.ContentKey))
			var err error
			if fault == "missing" {
				err = os.Remove(artifactPath)
			} else {
				err = os.WriteFile(artifactPath, []byte(`{"schema":"synthetic-corrupt-original"}`), 0o600)
			}
			if err != nil {
				tb.Fatal(err)
			}
			response := fixture.request(tb, token, query)
			if response.statusCode != http.StatusInternalServerError || !strings.Contains(string(response.body), "artifact integrity failure") {
				tb.Fatalf("%s original did not refuse the provider read: status=%d body=%s", fault, response.statusCode, response.body)
			}
			query.Set("legacy_coldkey", selector)
			snClaimAuthAssertHeld(tb, fixture.request(tb, token, query))
			snClaimAuthAssertHeld(tb, fixture.request(tb, account.Testing_Sign(), query))
		}
	})
}

// A valid signature is insufficient when the Sql pointer names another scope.
func TestSnPoolClaimRejectsArtifactOutsideOriginalScope(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		fixture := newSnClaimAuthFixture(tb)
		account := snClaimAuthAccount(tb)
		provider := snClaimAuthProvider(tb, account)
		coldkey := [32]byte{0xc1}
		snClaimAuthWallet(tb, account, coldkey)
		token := provider.Testing_Sign()
		providers := []startifact.ProviderInput{snClaimAuthContribution(provider, coldkey, 1)}
		healthy := fixture.build(tb, 30, providers)
		fixture.retain(tb, 30, healthy)
		snClaimAuthAssertClaim(tb, fixture.request(tb, token, url.Values{"epoch": {"30"}}), healthy, coldkey)
		for i, fault := range []string{"deployment", "chain", "genesis", "netuid", "coordinator", "vault", "epoch", "pool", "record-root"} {
			epoch := uint64(31 + i)
			artifact := fixture.build(tb, epoch, providers)
			switch fault {
			case "deployment":
				artifact.DeploymentID = "another-synthetic-deployment"
			case "chain":
				artifact.ChainID++
			case "genesis":
				artifact.GenesisHash = "0x" + strings.Repeat("c2", 32)
			case "netuid":
				artifact.Netuid++
			case "coordinator":
				artifact.Coordinator[0]++
			case "vault":
				artifact.SettlementVault[0]++
			case "epoch":
				artifact.Epoch++
			case "pool":
				artifact.NoID++
			}
			if fault == "record-root" {
				root := artifact.PayoutRoot
				root[0]++
				fixture.retain(tb, epoch, artifact, root)
			} else {
				fixture.retain(tb, epoch, artifact)
			}
			response := fixture.request(tb, token, url.Values{"epoch": {strconv.FormatUint(epoch, 10)}})
			if response.statusCode != http.StatusInternalServerError || !strings.Contains(string(response.body), "original claim scope") {
				tb.Fatalf("%s mismatch did not refuse the original scope: status=%d body=%s", fault, response.statusCode, response.body)
			}
		}
	})
}

// Locally valid proofs must still match the root retained beside the original.
func TestSnPoolClaimRejectsChangedPayoutLeafRoot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		fixture := newSnClaimAuthFixture(tb)
		account := snClaimAuthAccount(tb)
		provider := snClaimAuthProvider(tb, account)
		coldkey := [32]byte{0xd1}
		snClaimAuthWallet(tb, account, coldkey)
		artifact := fixture.build(tb, 50, []startifact.ProviderInput{snClaimAuthContribution(provider, coldkey, 1)})
		fixture.retain(tb, 50, artifact)
		token := provider.Testing_Sign()
		snClaimAuthAssertClaim(tb, fixture.request(tb, token, url.Values{"epoch": {"50"}}), artifact, coldkey)
		leaves := model.GetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), 50, fixture.cfg.NoId)
		leaves[0].ShareBps--
		model.SetStPayoutLeaves(tb.Context(), fixture.cfg.DeploymentKey(), 50, fixture.cfg.NoId, leaves)
		response := fixture.request(tb, token, url.Values{"epoch": {"50"}})
		if response.statusCode != http.StatusInternalServerError || !strings.Contains(string(response.body), "retained artifact root") {
			tb.Fatalf("changed Sql tree escaped the retained root: status=%d body=%s", response.statusCode, response.body)
		}
	})
}

// The real route refuses unsigned, invalid and revoked credentials before proof.
func TestSnPoolClaimRequiresValidActiveCredentials(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		fixture := newSnClaimAuthFixture(tb)
		account := snClaimAuthAccount(tb)
		provider := snClaimAuthProvider(tb, account)
		coldkey := [32]byte{0xe1}
		artifact := fixture.build(tb, 60, []startifact.ProviderInput{snClaimAuthContribution(provider, coldkey, 1)})
		fixture.retain(tb, 60, artifact)
		token := provider.Testing_Sign()
		query := url.Values{"epoch": {"60"}}
		snClaimAuthAssertClaim(tb, fixture.request(tb, token, query), artifact, coldkey)
		server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), `UPDATE network_client SET active = false WHERE client_id = $1`, *provider.ClientId))
		})
		for _, rejectedToken := range []string{"", "synthetic-invalid-credential", token} {
			response := fixture.request(tb, rejectedToken, query)
			if response.statusCode != http.StatusUnauthorized {
				tb.Fatalf("unauthenticated claim reached proof: status=%d body=%s", response.statusCode, response.body)
			}
		}
	})
}
