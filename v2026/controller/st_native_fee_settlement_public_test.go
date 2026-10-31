// These roots use the actual global controller, sealed native verifier and
// public SQL settlement. They load no operator EVM key or caller-written report.
package controller

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/nativefee"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type nativeFeeControllerManifest struct {
	Schema            string              `json:"schema"`
	Authority         nativefee.Authority `json:"authority"`
	Request           nativefee.Reference `json:"request"`
	TransactionHash   string              `json:"transaction_hash"`
	RawTransaction    []byte              `json:"raw_transaction"`
	Sender            string              `json:"sender"`
	Nonce             uint64              `json:"nonce"`
	RuntimeCodeSha256 string              `json:"runtime_code_sha256"`
	NativeBlockNumber uint64              `json:"native_block_number"`
	ReceiptStatus     uint64              `json:"receipt_status"`
}

// Qualification independently selects the persistent exported originals and
// the exact mainnet.test image. Its nested runtime peer is explicitly synthetic.
func nativeFeeControllerProofFixture(tb testing.TB, mode string) (nativeFeeControllerManifest, *model.StTransactionIntent, *server.StOperatorGasPolicy) {
	tb.Helper()
	directory := os.Getenv("URNETWORK_NATIVE_FEE_SETTLEMENT_FIXTURE_DIRECTORY")
	verifier := os.Getenv("URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER")
	verifierHash := os.Getenv("URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER_SHA256")
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || !filepath.IsAbs(verifier) || len(verifierHash) != 71 {
		tb.Fatal("public native fee controller test requires selected persistent proof fixtures and mainnet.test path/SHA256")
	}
	file, err := os.Open(filepath.Join(directory, mode+".json"))
	if err != nil {
		tb.Fatal(err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 256*1024+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(raw) > 256*1024 {
		tb.Fatal("native fee fixture manifest is unavailable or overbound", readErr, closeErr)
	}
	var manifest nativeFeeControllerManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		tb.Fatal(err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF || manifest.Schema != "urnetwork-native-fee-settlement-test-fixture-v1" || manifest.Authority.Verifier.Path != verifier || manifest.Authority.Verifier.Sha256 != verifierHash {
		tb.Fatal("native fee controller fixture differs from selected verifier", err)
	}
	var signed types.Transaction
	if err := signed.UnmarshalBinary(manifest.RawTransaction); err != nil {
		tb.Fatal(err)
	}
	maximum, err := model.StTransactionGasLiability(&signed)
	if err != nil || maximum.Cmp(big.NewInt(750)) <= 0 || signed.Type() != types.LegacyTxType || signed.To() == nil {
		tb.Fatal("original signature does not exercise retained fee liability", err)
	}
	now := server.NowUtc()
	gas := &server.StOperatorGasPolicy{Schema: server.StOperatorGasPolicySchema, Profile: "testnet", ChainId: manifest.Authority.NativePolicy.EvmChainId, GenesisHash: manifest.Authority.NativePolicy.Genesis, NoId: 1, Coordinator: strings.ToLower(signed.To().Hex()), PolicyHash: "0x" + strings.Repeat("41", 32),
		Accounts: []server.StOperatorGasPolicyAccount{{Role: "deposit", Address: manifest.Sender}, {Role: "root", Address: "0x" + strings.Repeat("42", 20)}}, ValidFrom: now.Add(-time.Hour).Unix(), ValidUntil: now.Add(24 * time.Hour).Unix(), MaximumGas: signed.Gas(), MaximumFeePerGasWei: signed.GasPrice().String(), MaximumTipPerGasWei: signed.GasPrice().String(), MaximumIntentLiabilityWei: maximum.String(), MaximumLifetimeLiabilityWei: maximum.String(), MaximumIntentAttempts: 2, MaximumLifetimeAttempts: 4}
	intent := model.ReserveStTransactionIntent(tb.Context(), "synthetic-native-fee-controller", gas.Profile, "synthetic-native-fee-export", model.StDeploymentKey("synthetic-native-fee-controller"), gas.ChainId, gas.GenesisHash, manifest.Sender, gas.Coordinator, strings.ToLower(crypto.Keccak256Hash(signed.Data()).Hex()), signed.Data(), manifest.Nonce)
	if intent.Nonce != manifest.Nonce || strings.ToLower(signed.Hash().Hex()) != manifest.TransactionHash {
		tb.Fatal("controller fixture changed original signature")
	}
	price := signed.GasPrice().String()
	if attempt := model.AddStTransactionAttempt(tb.Context(), &model.StTransactionAttempt{IntentId: intent.IntentId, Attempt: 1, Kind: model.StTxAttemptExecution, TxHash: manifest.TransactionHash, RawTransaction: bytes.Clone(manifest.RawTransaction), GasLimit: signed.Gas(), GasPrice: &price}); attempt == nil {
		tb.Fatal("original signed transaction was not retained")
	}
	for index := range gas.Accounts {
		digest, err := model.StOperatorGasAccountHistorySha256(tb.Context(), gas.ChainId, gas.GenesisHash, gas.Accounts[index].Address, gas.MaximumLifetimeAttempts)
		if err != nil {
			tb.Fatal(err)
		}
		gas.Accounts[index].InitialHistorySha256 = digest
	}
	gasKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x73}, ed25519.SeedSize))
	gasRoot := &server.StOperatorGasAuthority{Schema: server.StOperatorGasAuthoritySchema, Profile: gas.Profile, ChainId: gas.ChainId, GenesisHash: gas.GenesisHash, NoId: gas.NoId, ApproverPublicKey: hex.EncodeToString(gasKey.Public().(ed25519.PublicKey))}
	stGasSealFixturePolicy(tb, gas, gasRoot, gasKey)
	if err := model.AdmitStOperatorGasPolicy(tb.Context(), gas, gasRoot); err != nil {
		tb.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x74}, ed25519.SeedSize))
	policy := &server.StNativeFeeDenominationPolicy{Schema: server.StNativeFeeDenominationSchema, Profile: gas.Profile, ChainId: gas.ChainId, GenesisHash: gas.GenesisHash, NoId: gas.NoId, NativeAuthority: manifest.Authority, RuntimeCodeSha256: manifest.RuntimeCodeSha256, FirstNativeBlock: manifest.NativeBlockNumber, LastNativeBlock: manifest.NativeBlockNumber, NativeUnit: "tao-rao", BudgetUnit: "evm-wei", WeiPerRaoNumerator: "1", WeiPerRaoDenominator: "1"}
	message, err := policy.SigningBytes()
	if err != nil {
		tb.Fatal(err)
	}
	policy.Signature = hex.EncodeToString(ed25519.Sign(key, message))
	digest, err := policy.Digest()
	if err != nil {
		tb.Fatal(err)
	}
	authority := &server.StNativeFeeDenominationAuthority{Schema: server.StNativeFeeDenominationAuthoritySchema, Profile: gas.Profile, ChainId: gas.ChainId, GenesisHash: gas.GenesisHash, NoId: gas.NoId, ApproverPublicKey: hex.EncodeToString(key.Public().(ed25519.PublicKey)), PolicySha256: digest}
	for name, value := range map[string]any{"native-fee-denomination-policy.yml": policy, "native-fee-denomination-authority.yml": authority} {
		encoded, err := json.Marshal(value)
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(server.Config.PushSimpleResource(name, encoded))
	}
	// The complete actual path must work with no parseable signer configuration.
	tb.Cleanup(server.Vault.PushSimpleResource("st.yml", []byte("signer configuration must not be loaded: [")))
	return manifest, intent, gas
}

// Actual global dispatch releases one exact native fee and retains all originals.
func TestNativeFeeControllerPublicProofSettlesWithoutSignerConfiguration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		manifest, intent, gas := nativeFeeControllerProofFixture(tb, "pair")
		result, err := SettleNativeTransactionFee(tb.Context(), intent.IntentId, manifest.Request, manifest.TransactionHash, 2*time.Minute)
		if err != nil || result == nil || result.DebitRao != "750" || result.DebitWei != "750" {
			tb.Fatal("global public-only controller did not settle actual owned proof", result, err)
		}
		budget, err := model.GetStOperatorGasBudgetSnapshot(tb.Context(), gas.Scope())
		if err != nil || budget == nil || budget.MaximumLiabilityWei != gas.MaximumLifetimeLiabilityWei || budget.BudgetChargeWei != "750" || budget.PaidFeesWei != "750" || budget.OutstandingWei != "0" || budget.Attempts != 1 || budget.SettledNonces != 1 {
			tb.Fatal("controller changed retained liability or released duplicate credit", budget, err)
		}
		for _, kind := range []string{"request", "approval", "archive", "receipt_collection", "checkpoint", "finality_proof", "replay_job"} {
			digest := sha256.New()
			reference, err := model.WriteStNativeFeeOriginal(tb.Context(), result.StatementSha256, kind, digest)
			if err != nil || reference == nil || reference.Sha256 != "sha256:"+hex.EncodeToString(digest.Sum(nil)) {
				tb.Fatal("controller lost complete original proof custody", kind, err)
			}
		}
	})
}

// Missing original refund never reaches durable credit, through the same API.
func TestNativeFeeControllerPublicUnknownRefundKeepsOriginalCeiling(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		manifest, intent, gas := nativeFeeControllerProofFixture(tb, "missing")
		if result, err := SettleNativeTransactionFee(tb.Context(), intent.IntentId, manifest.Request, manifest.TransactionHash, 2*time.Minute); err == nil || result != nil {
			tb.Fatal("unknown native refund gained controller settlement authority", result, err)
		}
		budget, err := model.GetStOperatorGasBudgetSnapshot(tb.Context(), gas.Scope())
		if err != nil || budget == nil || budget.BudgetChargeWei != gas.MaximumLifetimeLiabilityWei || budget.PaidFeesWei != "0" || budget.OutstandingWei != gas.MaximumLifetimeLiabilityWei || budget.Attempts != 1 || budget.SettledNonces != 0 {
			tb.Fatal("unknown original proof released retained liability", budget, err)
		}
	})
}
