// These roots cross the actual owned verifier into the public durable ledger.
// Receipt and GRANDPA verification are real; the exported nested runtime peer
// is synthetic and does not qualify a deployed production runtime or its units.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
)

// Only the independently selected exporter image supplies these original pins.
type stNativeFeePublicManifest struct {
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

// The exact exporter image and persistent original directory are selected by
// qualification. Missing assets fail; an unexecuted fixture is never a skip.
func stNativeFeePublicFixture(tb testing.TB, mode string, pending bool) (*stNativeFeeModelFixture, stNativeFeePublicManifest) {
	tb.Helper()
	directory := os.Getenv("URNETWORK_NATIVE_FEE_SETTLEMENT_FIXTURE_DIRECTORY")
	verifier := os.Getenv("URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER")
	verifierHash := os.Getenv("URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER_SHA256")
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || !filepath.IsAbs(verifier) || len(verifierHash) != 71 {
		tb.Fatal("native fee public proof tests require the independently selected persistent exporter directory and verifier path/SHA256")
	}
	file, err := os.Open(filepath.Join(directory, mode+".json"))
	if err != nil {
		tb.Fatal(err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 256*1024+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(raw) > 256*1024 {
		tb.Fatal("native fee original test manifest is unavailable or too large", readErr, closeErr)
	}
	var manifest stNativeFeePublicManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		tb.Fatal(err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF || manifest.Schema != "urnetwork-native-fee-settlement-test-fixture-v1" || manifest.Authority.Verifier.Path != verifier || manifest.Authority.Verifier.Sha256 != verifierHash {
		tb.Fatal("native fee original fixture differs from selected source image", err)
	}
	var signed types.Transaction
	if err := signed.UnmarshalBinary(manifest.RawTransaction); err != nil {
		tb.Fatal(err)
	}
	maximum, err := StTransactionGasLiability(&signed)
	if err != nil || maximum.Cmp(big.NewInt(750)) <= 0 || signed.Type() != types.LegacyTxType || signed.To() == nil {
		tb.Fatal("exported original does not exercise a larger reserved fee ceiling", err)
	}
	gas, gasRoot, _, gasSigner := stGasModelApproval(tb)
	gas.ChainId, gas.GenesisHash, gas.Coordinator = manifest.Authority.NativePolicy.EvmChainId, manifest.Authority.NativePolicy.Genesis, strings.ToLower(signed.To().Hex())
	gas.Accounts[0].Address = manifest.Sender
	gas.MaximumGas, gas.MaximumFeePerGasWei, gas.MaximumTipPerGasWei = signed.Gas(), signed.GasPrice().String(), signed.GasPrice().String()
	gas.MaximumIntentLiabilityWei, gas.MaximumLifetimeLiabilityWei = maximum.String(), maximum.String()
	gasRoot.ChainId, gasRoot.GenesisHash = gas.ChainId, gas.GenesisHash
	if pending {
		stGasModelSeal(tb, gas, gasRoot, gasSigner, true)
		if err := AdmitStOperatorGasPolicy(tb.Context(), gas, gasRoot); err != nil {
			tb.Fatal(err)
		}
	}
	intent := ReserveStTransactionIntent(tb.Context(), "synthetic-native-fee-public", gas.Profile, "synthetic-native-fee-export", StDeploymentKey("synthetic-native-fee-public"), gas.ChainId, gas.GenesisHash, manifest.Sender, gas.Coordinator, strings.ToLower(crypto.Keccak256Hash(signed.Data()).Hex()), signed.Data(), manifest.Nonce)
	if intent.Nonce != manifest.Nonce || strings.ToLower(signed.Hash().Hex()) != manifest.TransactionHash {
		tb.Fatal("fixture changed original signed nonce or transaction")
	}
	price := signed.GasPrice().String()
	attempt := &StTransactionAttempt{IntentId: intent.IntentId, Attempt: 1, Kind: StTxAttemptExecution, TxHash: manifest.TransactionHash, RawTransaction: bytes.Clone(manifest.RawTransaction), GasLimit: signed.Gas(), GasPrice: &price}
	if pending {
		unsigned := types.NewTx(&types.LegacyTx{Nonce: signed.Nonce(), To: signed.To(), Value: new(big.Int).Set(signed.Value()), Gas: signed.Gas(), GasPrice: new(big.Int).Set(signed.GasPrice()), Data: bytes.Clone(signed.Data())})
		if _, err := ReserveStTransactionGasAttempt(tb.Context(), gas, gasRoot, intent.IntentId, 1, StTxAttemptExecution, unsigned); err != nil {
			tb.Fatal(err)
		}
	} else {
		AddStTransactionAttempt(tb.Context(), attempt)
		stGasModelSeal(tb, gas, gasRoot, gasSigner, true)
		if err := AdmitStOperatorGasPolicy(tb.Context(), gas, gasRoot); err != nil {
			tb.Fatal(err)
		}
	}
	approver := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x69}, ed25519.SeedSize))
	policy := &server.StNativeFeeDenominationPolicy{Schema: server.StNativeFeeDenominationSchema, Profile: gas.Profile, ChainId: gas.ChainId, GenesisHash: gas.GenesisHash, NoId: gas.NoId, NativeAuthority: manifest.Authority, RuntimeCodeSha256: manifest.RuntimeCodeSha256, FirstNativeBlock: manifest.NativeBlockNumber, LastNativeBlock: manifest.NativeBlockNumber, NativeUnit: "tao-rao", BudgetUnit: "evm-wei", WeiPerRaoNumerator: "1", WeiPerRaoDenominator: "1"}
	authority := &server.StNativeFeeDenominationAuthority{Schema: server.StNativeFeeDenominationAuthoritySchema, Profile: gas.Profile, ChainId: gas.ChainId, GenesisHash: gas.GenesisHash, NoId: gas.NoId, ApproverPublicKey: hex.EncodeToString(approver.Public().(ed25519.PublicKey))}
	fixture := &stNativeFeeModelFixture{gas: gas, gasRoot: gasRoot, gasSigner: gasSigner, policy: policy, authority: authority, approver: approver, intent: intent, attempt: attempt}
	fixture.seal(tb)
	return fixture, manifest
}

func TestStNativeFeeSettlementPublicOwnedProofRetainsAllOriginals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, manifest := stNativeFeePublicFixture(tb, "pair", false)
		ctx, cancel := context.WithTimeout(tb.Context(), 2*time.Minute)
		defer cancel()
		verified, err := nativefee.Invoke(ctx, manifest.Authority, manifest.Request, manifest.TransactionHash, time.Minute)
		if err != nil {
			tb.Fatal("actual native verifier did not return its owned outcome", err)
		}
		result, err := SettleStTransactionNativeFee(ctx, fixture.intent.IntentId, fixture.policy, fixture.authority, verified)
		if err != nil || result == nil || result.DebitRao != "750" || result.DebitWei != "750" || result.Reused {
			tb.Fatal("actual owned original proof did not settle its exact reserved nonce", result, err)
		}
		fixture.budget(tb, fixture.gas.MaximumLifetimeLiabilityWei, "750", "750", "0", 1, 1)
		originals := verified.Facts().Originals
		if len(originals) != 7 {
			tb.Fatal("actual original proof did not carry its complete source closure")
		}
		for _, original := range originals {
			digest := sha256.New()
			reference, err := WriteStNativeFeeOriginal(ctx, result.StatementSha256, original.Kind, digest)
			if err != nil || reference == nil || *reference != original.Reference || "sha256:"+hex.EncodeToString(digest.Sum(nil)) != original.Reference.Sha256 {
				tb.Fatal("durable original proof could not be independently reconstructed", original.Kind, reference, err)
			}
		}
		if retry, err := SettleStTransactionNativeFee(ctx, fixture.intent.IntentId, fixture.policy, fixture.authority, verified); err != nil || retry == nil || !retry.Reused || retry.StatementSha256 != result.StatementSha256 {
			tb.Fatal("actual original proof retry lost durable idempotence", retry, err)
		}
		fixture.budget(tb, fixture.gas.MaximumLifetimeLiabilityWei, "750", "750", "0", 1, 1)
	})
}

func TestStNativeFeeSettlementPublicMissingRefundKeepsFullCeiling(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, manifest := stNativeFeePublicFixture(tb, "missing", false)
		ctx, cancel := context.WithTimeout(tb.Context(), 2*time.Minute)
		defer cancel()
		verified, err := nativefee.Invoke(ctx, manifest.Authority, manifest.Request, manifest.TransactionHash, time.Minute)
		if err == nil || verified != nil {
			tb.Fatal("actual missing refund was promoted to a verified zero", verified, err)
		}
		if result, err := SettleStTransactionNativeFee(ctx, fixture.intent.IntentId, fixture.policy, fixture.authority, verified); err == nil || result != nil {
			tb.Fatal("public settlement credited an unobserved native fee", result, err)
		}
		fixture.budget(tb, fixture.gas.MaximumLifetimeLiabilityWei, fixture.gas.MaximumLifetimeLiabilityWei, "0", fixture.gas.MaximumLifetimeLiabilityWei, 1, 0)
	})
}

func TestStNativeFeeSettlementPublicProofRecoversLostSignerResult(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, manifest := stNativeFeePublicFixture(tb, "pair", true)
		if len(GetStTransactionAttempts(tb.Context(), fixture.intent.IntentId)) != 0 {
			tb.Fatal("recovery fixture already retained a signature")
		}
		ctx, cancel := context.WithTimeout(tb.Context(), 2*time.Minute)
		defer cancel()
		verified, err := nativefee.Invoke(ctx, manifest.Authority, manifest.Request, manifest.TransactionHash, time.Minute)
		if err != nil {
			tb.Fatal(err)
		}
		result, err := SettleStTransactionNativeFee(ctx, fixture.intent.IntentId, fixture.policy, fixture.authority, verified)
		if err != nil || result == nil || result.DebitWei != "750" {
			tb.Fatal("owned original proof could not recover the lost signer result", result, err)
		}
		retained := GetCurrentStTransactionAttempt(tb.Context(), fixture.intent.IntentId)
		if retained == nil || retained.TxHash != manifest.TransactionHash || !bytes.Equal(retained.RawTransaction, manifest.RawTransaction) {
			tb.Fatal("recovery did not retain the exact already-executed signature")
		}
		fixture.budget(tb, fixture.gas.MaximumLifetimeLiabilityWei, "750", "750", "0", 1, 1)
	})
}

// Both conflicting native statements come from actual owned invocations. The
// signed ratio admits 750/2 exactly while the later 751/2 stays unknown.
func TestStNativeFeeSettlementPublicFractionalConflictRevokesCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture, manifest := stNativeFeePublicFixture(tb, "pair", false)
		fixture.policy.WeiPerRaoDenominator = "2"
		fixture.seal(tb)
		ctx, cancel := context.WithTimeout(tb.Context(), 3*time.Minute)
		defer cancel()
		verified, err := nativefee.Invoke(ctx, manifest.Authority, manifest.Request, manifest.TransactionHash, time.Minute)
		if err != nil {
			tb.Fatal(err)
		}
		if result, err := SettleStTransactionNativeFee(ctx, fixture.intent.IntentId, fixture.policy, fixture.authority, verified); err != nil || result == nil || result.DebitWei != "375" {
			tb.Fatal("exact approved original native ratio did not settle", result, err)
		}
		fixture.budget(tb, fixture.gas.MaximumLifetimeLiabilityWei, "375", "375", "0", 1, 1)
		path := filepath.Join(os.Getenv("URNETWORK_NATIVE_FEE_SETTLEMENT_FIXTURE_DIRECTORY"), "fractional-conflict.json")
		file, err := os.Open(path)
		if err != nil {
			tb.Fatal(err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 256*1024+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(raw) > 256*1024 {
			tb.Fatal("fractional native conflict manifest unavailable", readErr, closeErr)
		}
		var contradiction stNativeFeePublicManifest
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&contradiction); err != nil {
			tb.Fatal(err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF || contradiction.Schema != manifest.Schema || contradiction.Authority != manifest.Authority || contradiction.TransactionHash != manifest.TransactionHash || !bytes.Equal(contradiction.RawTransaction, manifest.RawTransaction) {
			tb.Fatal("fractional proof did not preserve exact selected original authority and signature", err)
		}
		changed, err := nativefee.Invoke(ctx, contradiction.Authority, contradiction.Request, contradiction.TransactionHash, time.Minute)
		if err != nil || changed == nil || changed.Facts().DebitRao != "751" {
			tb.Fatal("actual contradictory native proof did not reach owned result", changed, err)
		}
		if result, err := SettleStTransactionNativeFee(ctx, fixture.intent.IntentId, fixture.policy, fixture.authority, changed); result != nil || !errors.Is(err, ErrStNativeFeeConflict) {
			tb.Fatal("actual fractional native contradiction kept previous credit", result, err)
		}
		var heldStatement string
		server.Db(ctx, func(conn server.PgConn) {
			var mapped *string
			server.Raise(conn.QueryRow(ctx, `SELECT first_statement_sha256,maximum_debit_wei::text FROM st_operator_native_fee_hold WHERE intent_id=$1`, fixture.intent.IntentId).Scan(&heldStatement, &mapped))
			if mapped != nil {
				tb.Fatal("actual fractional native debit acquired an invented mapped expense", *mapped)
			}
		})
		for _, original := range changed.Facts().Originals {
			digest := sha256.New()
			reference, err := WriteStNativeFeeOriginal(ctx, heldStatement, original.Kind, digest)
			if err != nil || reference == nil || *reference != original.Reference || "sha256:"+hex.EncodeToString(digest.Sum(nil)) != original.Reference.Sha256 {
				tb.Fatal("contradictory actual native proof lost durable original custody", original.Kind, reference, err)
			}
		}
		fixture.budget(tb, fixture.gas.MaximumLifetimeLiabilityWei, fixture.gas.MaximumLifetimeLiabilityWei, "0", fixture.gas.MaximumLifetimeLiabilityWei, 1, 0)
		if err := RequireStTransactionGasUnsettled(ctx, fixture.intent.IntentId); !errors.Is(err, ErrStOperatorGasAllowance) {
			tb.Fatal("fractional native contradiction left signing authorized", err)
		}
	})
}
