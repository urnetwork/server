// Before a provider work roster or any signed wallet consent exists, the
// close must still pay a provider with usage to its account-history wallet.
// Keys, networks, clients and chain responses are synthetic.
package controller

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/validator"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// The core client against the policy fixture's RPC, with every provider
// unbound at both boundaries, which that RPC does not serve.
type stPayoutAccountWalletClient struct {
	*CoreStClient
}

func (self *stPayoutAccountWalletClient) BindingsAt(_ context.Context, clientIds [][16]byte, _ uint64, _ uint64, _ uint64) ([]*StFleetBindingState, error) {
	bindings := make([]*StFleetBindingState, len(clientIds))
	for index := range bindings {
		bindings[index] = &StFleetBindingState{}
	}
	return bindings, nil
}

// Epoch 1 of the policy fixture spans blocks 200 to 250, after the earning
// cutoff at block 100, with no roster, no signed consent and one provider
// whose settled usage and reliability make it eligible.
type stPayoutAccountWalletFixture struct {
	rpc       *stPayoutPolicyRpcFixture
	cfg       *StConfig
	client    *stPayoutAccountWalletClient
	networkId server.Id
	clientId  server.Id
}

func newStPayoutAccountWalletFixture(t testing.TB) *stPayoutAccountWalletFixture {
	t.Helper()
	if _, _, _, err := LoadProviderWorkAuthorityPolicy(); !errors.Is(err, server.ErrResourceNotFound) {
		t.Fatal("the test vault already provisions provider_work.yml", err)
	}
	ctx := t.Context()
	controllerPayoutSchedule(t, stPayoutPolicyTime(100))
	rpc, _, cfg, restart := newStPayoutPolicyFixture(t, 1)
	client := &stPayoutAccountWalletClient{CoreStClient: restart()}
	SetStClient(client)
	rpc.configure(275, "")
	f := &stPayoutAccountWalletFixture{rpc: rpc, cfg: cfg, client: client, networkId: server.NewId(), clientId: server.NewId()}
	testingCreatePaymentClient(ctx, f.networkId, f.clientId)
	stPayoutAccountWalletUsage(t, f.networkId, f.clientId, stPayoutPolicyTime(225), 3*1024*1024)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO verify_provider_stats (period_start, period_end, client_id, assignments, confirmations) VALUES ($1, $2, $3, 8, 8)`,
			stPayoutPolicyTime(205), stPayoutPolicyTime(245), f.clientId))
	})
	return f
}

// One settled contract crediting the provider, as the census reads it.
func stPayoutAccountWalletUsage(t testing.TB, networkId server.Id, clientId server.Id, closed time.Time, byteCount int64) {
	t.Helper()
	ctx := t.Context()
	sourceNetworkId, sourceId := server.NewId(), server.NewId()
	testingCreatePaymentClient(ctx, sourceNetworkId, sourceId)
	usage := fmt.Sprintf(`{"version":1,"byte_count":%d,"providers":[{"client_id":%q,"network_id":%q,"byte_count":%d}]}`, byteCount, clientId.String(), networkId.String(), byteCount)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,'settled',$9,true)`, server.NewId(), sourceNetworkId, sourceId, networkId, clientId, byteCount, closed.Add(-time.Minute), closed, usage))
	})
}

// An account-history wallet row as the wallet attach flow writes it, without a
// signed original, at an explicit time.
func stPayoutAccountWalletRow(t testing.TB, clientId server.Id, networkId server.Id, seed byte, setTime time.Time) [32]byte {
	t.Helper()
	ctx := t.Context()
	var coldkey [32]byte
	for index := range coldkey {
		coldkey[index] = seed + byte(index)
	}
	address, err := ss58.Encode(coldkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO st_provider_wallet_history (client_id, network_id, coldkey_ss58, coldkey_pubkey, set_time) VALUES ($1, $2, $3, $4, $5)`,
			clientId, networkId, address, coldkey[:], setTime))
	})
	return coldkey
}

// The root cause: without a roster and without signed consent, the provider
// was left unmapped and its leaf dropped, so its epoch paid nobody. The
// account-history wallet is now paid, and the published artifact, its leaf
// proof and the validator's own checks accept a leaf whose wallet has no
// signed original or consent resolution: its canonical decoder and
// closed-work check, as its artifact reader applies them, and its deposit
// audit against the committed root.
func TestStPayoutWithoutRosterPaysAccountHistoryWalletOfProviderWithoutEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		f := newStPayoutAccountWalletFixture(tb)
		coldkey := stPayoutAccountWalletRow(tb, f.clientId, f.networkId, 41, stPayoutPolicyTime(210))
		before := stPayoutWalletProviderCounts()
		root, count, err := stComputeReleasePayout(ctx, f.cfg, f.client, 1, stPayoutPolicyTime(200), stPayoutPolicyTime(250), 200, 250, nil)
		if err != nil || count != 1 || root == ([32]byte{}) {
			tb.Fatal("the account-history wallet of a provider without signed evidence was not paid", count, err)
		}
		if deltas := stPayoutWalletProviderDeltas(before); !maps.Equal(deltas, map[string]float64{StPayoutWalletSourceAccountHistory: 1}) {
			tb.Fatal("the account-history payout was not counted by source", deltas)
		}
		record := model.GetStPayoutArtifact(ctx, f.cfg.DeploymentKey(), 1, f.cfg.NoId)
		store, ok := server.LoadBlobStore()
		if record == nil || !ok || record.PayoutRoot != root {
			tb.Fatal("the account-history payout was not published")
		}
		artifact, raw, err := startifact.Read(ctx, store, record.ContentHash)
		if err != nil {
			tb.Fatal(err)
		}
		if len(artifact.Leaves) != 1 || artifact.Leaves[0].Coldkey != coldkey || artifact.Leaves[0].ShareBPS != 10_000 || artifact.Leaves[0].ClientID != [16]byte(f.clientId) {
			tb.Fatal("the leaf does not pay the account-history wallet the whole share", artifact.Leaves)
		}
		// the validator's artifact reader decodes canonically and, for a
		// closed-work census, verifies its reports
		if _, err := payoutartifact.DecodeWithContext(ctx, raw); err != nil {
			tb.Fatal("the validator's decoder refused the artifact", err)
		}
		if err := payoutartifact.VerifyWithContext(ctx, artifact); err != nil {
			tb.Fatal("the artifact with an account-history leaf did not verify", err)
		}
		if artifact.ClosedWork != nil {
			if _, err := payoutartifact.VerifyClosedWorkReports(ctx, artifact, common.Address{}); err != nil && !errors.Is(err, payoutartifact.ErrClosedWorkUnavailable) {
				tb.Fatal("the validator's closed-work check refused the artifact", err)
			}
		}
		// the validator's deposit audit of the next epoch, once the root is
		// committed, rebuilds the artifact and accepts its signed usage
		contentHash, err := hex.DecodeString(strings.TrimPrefix(artifact.ContentHash, "sha256:"))
		if err != nil || len(contentHash) != 32 {
			tb.Fatal("the artifact content hash is malformed", artifact.ContentHash, err)
		}
		rootSigner := crypto.PubkeyToAddress(f.cfg.RootKey.PublicKey)
		depositPolicy := protocol.DepositPolicy{EpochCapRaoPerOperator: 1_000_000, Tiers: []protocol.DepositTier{{RateNumeratorRaoPerGiB: 10, RateNumeratorRaoPerUser: 3, RateDenominator: 1}}}
		required, _, err := protocol.RequiredDepositRao(artifact.TotalUsageBytes, artifact.TotalUsers, big.NewInt(0), depositPolicy)
		if err != nil {
			tb.Fatal(err)
		}
		audit := validator.EvaluateDepositArtifact(artifact, validator.DepositArtifactExpectation{
			DeploymentID: f.cfg.DeploymentId, ChainID: f.cfg.ChainId, GenesisHash: fmt.Sprintf("0x%x", f.cfg.GenesisHash), Netuid: uint16(f.cfg.Netuid),
			Coordinator: f.cfg.ContractAddress, SettlementVault: f.cfg.SettlementVault, PolicyHash: fmt.Sprintf("0x%x", f.cfg.PolicyHash), Epoch: 1, NoID: f.cfg.NoId,
			Signer:     crypto.PubkeyToAddress(f.cfg.ArtifactKey.PublicKey),
			Start:      payoutartifact.Boundary{Number: 200, Hash: common.Hash(stPayoutPolicyBoundary(200).Hash).Hex()},
			End:        payoutartifact.Boundary{Number: 250, Hash: common.Hash(stPayoutPolicyBoundary(250).Hash).Hex()},
			PayoutRoot: root, ArtifactHash: [32]byte(contentHash), Committer: rootSigner, RootSigner: rootSigner, CommitBlock: 251,
		}, 2, required, big.NewInt(0), depositPolicy)
		if !audit.Compliant || audit.Status != validator.DepositAuditCompliant {
			tb.Fatal("the validator's deposit audit refused the artifact with an account-history leaf", audit.Status, audit.Error)
		}
		leaf := artifact.Leaves[0]
		if !merkle.Verify(artifact.PayoutRoot, merkle.PayoutLeaf(leaf.Coldkey, new(big.Int).SetUint64(leaf.ShareBPS)), leaf.Proof) {
			tb.Fatal("the vault's claim proof does not verify for the account-history leaf")
		}
		if resolutions := model.GetStPayoutWalletResolutions(ctx, f.cfg.DeploymentKey(), 1, f.cfg.NoId); len(resolutions) != 0 {
			tb.Fatal("an account-history wallet was recorded as a signed consent", resolutions)
		}
	})
}
