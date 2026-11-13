// Real same-nonce attempts prove that one endpoint response cannot erase
// pending evidence or hide another canonical candidate from reconciliation.
package controller

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Keep both actual persisted candidates, in the same newest-first order used
// by production recovery, so status assertions cannot inspect only the winner.
type stReceiptCandidateObservation struct {
	terminal     bool
	pending      bool
	hash         string
	err          error
	intent       *model.StTransactionIntent
	attempts     []*model.StTransactionAttempt
	expectedHash string
	invalidIndex int
	validIndex   int
}

// The nonce never changes. Both fee attempts are genuinely signed and stored;
// only one serialized receipt's transactionHash is deliberately substituted.
func observeStReceiptCandidates(t testing.TB, invalidFirst bool, finalized bool, fault string) stReceiptCandidateObservation {
	t.Helper()
	ctx := context.Background()
	cfg := stTransactionReconcileConfig()
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte("inert same-nonce receipt fixture")))
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	to := cfg.ContractAddress
	calldata := []byte{4, 5, 6}
	intent := model.ReserveStTransactionIntent(ctx, "receipt-candidates", "testnet", cfg.DeploymentId, cfg.DeploymentKey(), cfg.ChainId, "0x"+hex.EncodeToString(cfg.GenesisHash[:]), strings.ToLower(from.Hex()), strings.ToLower(to.Hex()), crypto.Keccak256Hash(calldata).Hex(), calldata, 7)
	transactions := make([]*types.Transaction, 2)
	for index := range transactions {
		price := big.NewInt(int64(100 + index*20))
		transaction, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: 7, To: &to, Gas: 50000, GasPrice: price, Value: new(big.Int), Data: calldata}), types.LatestSignerForChainID(big.NewInt(945)), key)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := transaction.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		priceString := price.String()
		attempt := model.AddStTransactionAttempt(ctx, &model.StTransactionAttempt{IntentId: intent.IntentId, Attempt: index + 1, Kind: model.StTxAttemptExecution, TxHash: strings.ToLower(transaction.Hash().Hex()), RawTransaction: raw, GasLimit: transaction.Gas(), GasPrice: &priceString})
		if attempt == nil {
			t.Fatal("real same-nonce attempt was not stored")
		}
		model.MarkStTransactionBroadcast(ctx, intent.IntentId, attempt.Attempt)
		transactions[index] = transaction
	}
	// Database iteration is newest first, unlike the preparation order above.
	attempts := model.GetStTransactionAttempts(ctx, intent.IntentId)
	if len(attempts) != 2 || attempts[0].Attempt != 2 || attempts[1].Attempt != 1 {
		t.Fatal("actual candidate order differs")
	}
	result := stReceiptCandidateObservation{invalidIndex: 1, validIndex: 0}
	if invalidFirst {
		result.invalidIndex, result.validIndex = 0, 1
	}
	receipts := map[string]*types.Receipt{}
	for index, attempt := range attempts {
		height := uint64(99)
		if index == result.validIndex && !finalized {
			height = 101
		}
		receipt := &types.Receipt{Type: types.LegacyTxType, Status: types.ReceiptStatusSuccessful, TxHash: common.HexToHash(attempt.TxHash), BlockHash: stTransactionReconcileBlockHash(height), BlockNumber: new(big.Int).SetUint64(height), GasUsed: 50000, CumulativeGasUsed: 50000, EffectiveGasPrice: big.NewInt(100), Logs: []*types.Log{}}
		if index == result.invalidIndex {
			if fault == "orphan" {
				receipt.BlockHash = common.Hash{0xfa}
			} else {
				receipt.TxHash[0] ^= 1
			}
		} else {
			result.expectedHash = attempt.TxHash
		}
		receipts[attempt.TxHash] = receipt
	}
	fixture := &stTransactionReconcileRPC{t: t, finalizedBlock: 100, finalizedNonce: 7, receipts: receipts}
	client, rpcClient := newStTransactionReconcileClient(t, fixture, cfg)
	result.terminal, result.pending, result.hash, result.err = client.observeTransactionAttempts(ctx, rpcClient, intent, attempts)
	result.intent = model.GetStTransactionIntent(ctx, intent.LogicalKey)
	result.attempts = model.GetStTransactionAttempts(ctx, intent.IntentId)
	return result
}

// A later malformed predecessor cannot clear the pending flag established by
// an earlier real nonfinal inclusion and authorize premature fee replacement.
func TestStReceiptCandidatesPreserveEarlierPendingEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		result := observeStReceiptCandidates(tb, false, false, "malformed")
		if result.err == nil || result.terminal || !result.pending || result.hash != "" {
			tb.Fatalf("later invalid receipt erased earlier pending evidence: terminal%t pending%t error%v", result.terminal, result.pending, result.err)
		}
		if result.intent == nil || result.intent.Status != model.StTxMined || len(result.attempts) != 2 || result.attempts[result.validIndex].Status != model.StTxMined || result.attempts[result.invalidIndex].Status != model.StTxAttemptReplaced || result.attempts[result.invalidIndex].InclusionBlock != nil {
			tb.Fatal("later invalid receipt changed actual candidate storage")
		}
	})
}

// A malformed newest fee attempt must not hide an older real pending candidate.
func TestStReceiptCandidatesContinueToLaterPendingEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		result := observeStReceiptCandidates(tb, true, false, "malformed")
		if result.err == nil || result.terminal || !result.pending || result.hash != "" {
			tb.Fatalf("earlier invalid receipt hid later pending evidence: terminal%t pending%t error%v", result.terminal, result.pending, result.err)
		}
		if result.intent == nil || result.intent.Status != model.StTxMined || len(result.attempts) != 2 || result.attempts[result.validIndex].Status != model.StTxMined || result.attempts[result.invalidIndex].Status != model.StTxBroadcast || result.attempts[result.invalidIndex].InclusionBlock != nil {
			tb.Fatal("earlier invalid receipt changed actual candidate storage")
		}
	})
}

// An independently exact canonical older attempt is decisive despite an invalid
// response for its newer competitor. Reconciliation must finalize the actual winner.
func TestStReceiptCandidatesFindExactCanonicalWinnerAfterInvalidResponse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		result := observeStReceiptCandidates(tb, true, true, "malformed")
		if result.err != nil || !result.terminal || result.pending || result.hash != result.expectedHash {
			tb.Fatalf("earlier invalid receipt hid exact canonical winner: terminal%t pending%t hash%s error%v", result.terminal, result.pending, result.hash, result.err)
		}
		if result.intent == nil || result.intent.Status != model.StTxFinalized || result.intent.CurrentTxHash == nil || *result.intent.CurrentTxHash != result.expectedHash || len(result.attempts) != 2 || result.attempts[result.validIndex].Status != model.StTxFinalized || result.attempts[result.invalidIndex].InclusionBlock != nil {
			tb.Fatal("canonical winner was not preserved in actual durable state")
		}
	})
}

// A stale receipt for the newest fee candidate cannot hide the exact older
// winner and let account reconciliation classify the nonce as unknown.
func TestStReceiptCandidatesContinuePastOrphanToCanonicalWinner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		result := observeStReceiptCandidates(tb, true, true, "orphan")
		if result.err != nil || !result.terminal || result.pending || result.hash != result.expectedHash {
			tb.Fatalf("orphan receipt hid exact canonical winner: terminal%t pending%t hash%s error%v", result.terminal, result.pending, result.hash, result.err)
		}
		if result.intent == nil || result.intent.Status != model.StTxFinalized || len(result.attempts) != 2 || result.attempts[result.validIndex].Status != model.StTxFinalized || result.attempts[result.invalidIndex].Status != model.StTxUncertain || result.attempts[result.invalidIndex].InclusionBlock != nil {
			tb.Fatal("orphan candidate changed canonical winner storage")
		}
	})
}

// Removing a later orphan's inclusion must not erase pending evidence already
// found on another signed candidate. Both durable per-attempt facts remain.
func TestStReceiptCandidatesPreservePendingBeforeOrphan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		result := observeStReceiptCandidates(tb, false, false, "orphan")
		if result.err == nil || result.terminal || !result.pending || result.hash != "" {
			tb.Fatalf("orphan receipt erased earlier pending candidate: terminal%t pending%t error%v", result.terminal, result.pending, result.err)
		}
		if result.intent == nil || result.intent.Status != model.StTxUncertain || len(result.attempts) != 2 || result.attempts[result.validIndex].Status != model.StTxMined || result.attempts[result.invalidIndex].Status != model.StTxUncertain || result.attempts[result.invalidIndex].InclusionBlock != nil {
			tb.Fatal("orphan cleanup erased another candidate's durable inclusion")
		}
	})
}

// A first orphan cannot truncate the finite census before a real nonfinal
// candidate is observed. The caller must continue waiting without replacement.
func TestStReceiptCandidatesFindPendingAfterOrphan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		result := observeStReceiptCandidates(tb, true, false, "orphan")
		if result.err == nil || result.terminal || !result.pending || result.hash != "" {
			tb.Fatalf("orphan receipt hid later pending candidate: terminal%t pending%t error%v", result.terminal, result.pending, result.err)
		}
		if result.intent == nil || result.intent.Status != model.StTxMined || len(result.attempts) != 2 || result.attempts[result.validIndex].Status != model.StTxMined || result.attempts[result.invalidIndex].Status != model.StTxUncertain || result.attempts[result.invalidIndex].InclusionBlock != nil {
			tb.Fatal("orphan-first scan lost actual pending storage")
		}
	})
}
