// Deterministic ledger tests begin after the separate native proof boundary.
package model

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urfoundation/sn/v2026/nativefee"
	"github.com/urnetwork/server/v2026"
)

// These database tests start after native verification and exercise the private
// reducer. They assert no actual-runtime proof qualification; the public zero
// value refusal below separately ensures facts cannot manufacture ingress.
type stNativeFeeModelFixture struct {
	gas       *server.StOperatorGasPolicy
	gasRoot   *server.StOperatorGasAuthority
	gasSigner ed25519.PrivateKey
	policy    *server.StNativeFeeDenominationPolicy
	authority *server.StNativeFeeDenominationAuthority
	key       *ecdsa.PrivateKey
	approver  ed25519.PrivateKey
	intent    *StTransactionIntent
	attempt   *StTransactionAttempt
	statement nativefee.Statement
}

func newStNativeFeeModelFixture(tb testing.TB) *stNativeFeeModelFixture {
	tb.Helper()
	gas, gasRoot, key, gasApprover := stGasModelApproval(tb)
	gas.MaximumIntentLiabilityWei, gas.MaximumLifetimeLiabilityWei = "1800000", "2400000"
	stGasModelSeal(tb, gas, gasRoot, gasApprover, false)
	if err := AdmitStOperatorGasPolicy(tb.Context(), gas, gasRoot); err != nil {
		tb.Fatal(err)
	}
	intent := stGasModelIntent(gas, "synthetic-native-fee-original", []byte{1})
	unsigned := stGasModelTransaction(intent, 60_000, 10)
	if _, err := ReserveStTransactionGasAttempt(tb.Context(), gas, gasRoot, intent.IntentId, 1, StTxAttemptExecution, unsigned); err != nil {
		tb.Fatal(err)
	}
	attempt := stGasModelCommit(tb, intent, 1, unsigned, key)
	approver := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x69}, ed25519.SeedSize))
	hash := "sha256:" + strings.Repeat("d", 64)
	policy := &server.StNativeFeeDenominationPolicy{Schema: server.StNativeFeeDenominationSchema, Profile: gas.Profile, ChainId: gas.ChainId, GenesisHash: gas.GenesisHash, NoId: gas.NoId,
		NativeAuthority:   nativefee.Authority{Verifier: nativefee.Reference{Path: "/synthetic/native-fee-verifier", Sha256: hash}, NativePolicy: nativefee.NativePolicy{ApprovalPublicKey: "0x" + strings.Repeat("e", 64), Genesis: gas.GenesisHash, EvmChainId: gas.ChainId, EngineSha256: hash, CheckpointSha256: hash, ReviewSha256: hash, ProfileSha256: hash}},
		RuntimeCodeSha256: hash, FirstNativeBlock: 100, LastNativeBlock: 200, NativeUnit: "tao-rao", BudgetUnit: "evm-wei", WeiPerRaoNumerator: "10", WeiPerRaoDenominator: "1"}
	authority := &server.StNativeFeeDenominationAuthority{Schema: server.StNativeFeeDenominationAuthoritySchema, Profile: gas.Profile, ChainId: gas.ChainId, GenesisHash: gas.GenesisHash, NoId: gas.NoId, ApproverPublicKey: hex.EncodeToString(approver.Public().(ed25519.PublicKey))}
	value := &stNativeFeeModelFixture{gas: gas, gasRoot: gasRoot, gasSigner: gasApprover, policy: policy, authority: authority, key: key, approver: approver, intent: intent, attempt: attempt,
		statement: nativefee.Statement{Schema: nativefee.StatementSchema, Genesis: gas.GenesisHash, EvmChainId: gas.ChainId, RuntimeCodeSha256: hash, NativeBlockNumber: 101, NativeBlockHash: "0x" + strings.Repeat("f", 64), TransactionHash: attempt.TxHash, Sender: intent.FromAddress, Nonce: intent.Nonce, RawTransaction: bytes.Clone(attempt.RawTransaction), ReceiptStatus: 1, WithdrawalRao: "150", RefundRao: "50", DebitRao: "100"}}
	value.seal(tb)
	return value
}

func (self *stNativeFeeModelFixture) seal(tb testing.TB) {
	tb.Helper()
	message, err := self.policy.SigningBytes()
	if err != nil {
		tb.Fatal(err)
	}
	self.policy.Signature = hex.EncodeToString(ed25519.Sign(self.approver, message))
	self.authority.PolicySha256, err = self.policy.Digest()
	if err != nil {
		tb.Fatal(err)
	}
}

func (self *stNativeFeeModelFixture) settle(ctx context.Context) (*StTransactionNativeFeeSettlement, error) {
	return settleStTransactionNativeFee(ctx, self.intent.IntentId, self.policy, self.authority, self.statement)
}

func (self *stNativeFeeModelFixture) budget(tb testing.TB, maximum, charge, paid, outstanding string, attempts, settled int64) {
	tb.Helper()
	value, err := GetStOperatorGasBudgetSnapshot(tb.Context(), self.gas.Scope())
	if err != nil || value == nil || value.MaximumLiabilityWei != maximum || value.BudgetChargeWei != charge || value.PaidFeesWei != paid || value.OutstandingWei != outstanding || value.Attempts != attempts || value.SettledNonces != settled {
		tb.Fatalf("native fee budget differs: value=%+v error=%v, want retained=%s charge=%s paid=%s outstanding=%s attempts=%d settled=%d", value, err, maximum, charge, paid, outstanding, attempts, settled)
	}
}

func TestStNativeFeeSettlementOriginalDebitIsDurableAndIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		first, err := fixture.settle(tb.Context())
		if err != nil || first == nil || first.Reused || first.DebitWei != "1000" || first.OriginalCeilingWei != "600000" || first.ExceedsCeiling {
			tb.Fatal("original native debit was not settled exactly", first, err)
		}
		fixture.budget(tb, "600000", "1000", "1000", "0", 1, 1)
		// A new database transaction models a reply lost after durable commit.
		second, err := fixture.settle(tb.Context())
		retained, readErr := GetStTransactionNativeFeeSettlement(tb.Context(), fixture.intent.IntentId)
		if err != nil || readErr != nil || second == nil || !second.Reused || retained == nil || retained.StatementSha256 != first.StatementSha256 || retained.DebitWei != first.DebitWei {
			tb.Fatal("lost settlement acknowledgement changed original debit", second, retained, err, readErr)
		}
		fixture.budget(tb, "600000", "1000", "1000", "0", 1, 1)
		if original := GetCurrentStTransactionAttempt(tb.Context(), fixture.intent.IntentId); !bytes.Equal(original.RawTransaction, fixture.attempt.RawTransaction) {
			tb.Fatal("settlement rewrote the original signed transaction")
		}
	})
}

func TestStNativeFeeSettlementUnknownAndUnownedKeepFullCeiling(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		MarkStTransactionFinalized(tb.Context(), fixture.intent.IntentId, 1, fixture.attempt.TxHash, 10, "0x"+strings.Repeat("a", 64))
		for _, unowned := range []*nativefee.Verified{nil, {}} {
			if result, err := SettleStTransactionNativeFee(tb.Context(), fixture.intent.IntentId, fixture.policy, fixture.authority, unowned); err == nil || result != nil {
				tb.Fatal("receipt status or an unowned report released native fee liability", result, err)
			}
		}
		fixture.statement.RefundRao, fixture.statement.DebitRao = "", "150"
		if result, err := fixture.settle(tb.Context()); err == nil || result != nil {
			tb.Fatal("missing refund became a zero actual native fee", result, err)
		}
		fixture.budget(tb, "600000", "600000", "0", "600000", 1, 0)
	})
}

func TestStNativeFeeSettlementUsesNonceMaximumAcrossCandidates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		for number, price := range []int64{20, 30} {
			unsigned := stGasModelTransaction(fixture.intent, 60_000, price)
			if _, err := ReserveStTransactionGasAttempt(tb.Context(), fixture.gas, fixture.gasRoot, fixture.intent.IntentId, number+2, StTxAttemptExecution, unsigned); err != nil {
				tb.Fatal(err)
			}
			stGasModelCommit(tb, fixture.intent, number+2, unsigned, fixture.key)
		}
		fixture.budget(tb, "1800000", "1800000", "0", "1800000", 3, 0)
		if result, err := fixture.settle(tb.Context()); err != nil || result.OriginalCeilingWei != "1800000" || result.Attempt != 1 {
			tb.Fatal("older original winning candidate could not settle the full nonce", result, err)
		}
		fixture.budget(tb, "1800000", "1000", "1000", "0", 3, 1)
	})
}

func TestStNativeFeeSettlementHoldsAuthenticatedContradictoryOriginal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		if _, err := fixture.settle(tb.Context()); err != nil {
			tb.Fatal(err)
		}
		original := fixture.statement
		for _, change := range []func(*nativefee.Statement){
			func(value *nativefee.Statement) { value.NativeBlockHash = "0x" + strings.Repeat("a", 64) },
			func(value *nativefee.Statement) { value.WithdrawalRao, value.DebitRao = "160", "110" },
			func(value *nativefee.Statement) { value.ReceiptStatus = 0 },
		} {
			fixture.statement = original
			change(&fixture.statement)
			if result, err := fixture.settle(tb.Context()); err == nil || result != nil {
				tb.Fatal("changed original native outcome overwrote retained settlement", result, err)
			}
		}
		fixture.statement = original
		fixture.policy.WeiPerRaoNumerator = "1"
		fixture.seal(tb)
		if result, err := fixture.settle(tb.Context()); !errors.Is(err, ErrStNativeFeeConflict) || result != nil {
			tb.Fatal("later signed denomination cleared an authenticated contradiction", result, err)
		}
		fixture.budget(tb, "600000", "600000", "0", "600000", 1, 0)
		if err := AdmitStOperatorGasPolicy(tb.Context(), fixture.gas, fixture.gasRoot); !errors.Is(err, ErrStNativeFeeConflict) {
			tb.Fatal("authenticated conflict left the original scope spendable", err)
		}
	})
}

func TestStNativeFeeSettlementDenominationRefusalDoesNotRewriteOriginal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		if _, err := fixture.settle(tb.Context()); err != nil {
			tb.Fatal(err)
		}
		fixture.policy.WeiPerRaoNumerator = "1"
		fixture.seal(tb)
		if result, err := fixture.settle(tb.Context()); err == nil || result != nil || errors.Is(err, ErrStNativeFeeConflict) {
			tb.Fatal("denomination change rewrote original debit or invented a native contradiction", result, err)
		}
		fixture.budget(tb, "600000", "1000", "1000", "0", 1, 1)
		fixture.policy.WeiPerRaoDenominator = "3"
		fixture.seal(tb)
		if result, err := fixture.settle(tb.Context()); err == nil || result != nil || errors.Is(err, ErrStNativeFeeConflict) {
			tb.Fatal("same native outcome with fractional denomination invented a contradiction", result, err)
		}
		fixture.budget(tb, "600000", "1000", "1000", "0", 1, 1)
	})
}

// Contradictory native units may be unmappable to whole budget units. Null
// preserves that uncertainty; neither rounding nor an earlier credit survives.
func TestStNativeFeeSettlementFractionalContradictionRetainsUnknownExpense(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		fixture.policy.WeiPerRaoNumerator, fixture.policy.WeiPerRaoDenominator = "1", "2"
		fixture.seal(tb)
		if _, err := fixture.settle(tb.Context()); err != nil {
			tb.Fatal(err)
		}
		fixture.budget(tb, "600000", "50", "50", "0", 1, 1)
		fixture.statement.WithdrawalRao, fixture.statement.DebitRao = "151", "101"
		if result, err := fixture.settle(tb.Context()); result != nil || !errors.Is(err, ErrStNativeFeeConflict) {
			tb.Fatal("fractional contradictory native debit retained spendable credit", result, err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var mapped *string
			server.Raise(conn.QueryRow(tb.Context(), `SELECT maximum_debit_wei::text FROM st_operator_native_fee_hold WHERE intent_id=$1`, fixture.intent.IntentId).Scan(&mapped))
			if mapped != nil {
				tb.Fatal("unrepresentable native debit acquired an invented budget value", *mapped)
			}
		})
		fixture.budget(tb, "600000", "600000", "0", "600000", 1, 0)
		fixture.statement.WithdrawalRao, fixture.statement.DebitRao = "1800052", "1800002"
		if _, err := fixture.settle(tb.Context()); !errors.Is(err, ErrStNativeFeeConflict) {
			tb.Fatal("later exact debit cleared the original conflict", err)
		}
		fixture.budget(tb, "600000", "900001", "0", "900001", 1, 0)
		fixture.statement.WithdrawalRao, fixture.statement.DebitRao = "1800053", "1800003"
		if _, err := fixture.settle(tb.Context()); !errors.Is(err, ErrStNativeFeeConflict) {
			tb.Fatal("later fractional debit cleared known conflicting expense", err)
		}
		fixture.budget(tb, "600000", "900001", "0", "900001", 1, 0)
	})
}

// Denomination approval does not erase an owned native contradiction outside
// its runtime domain; it only prevents assigning that statement budget units.
func TestStNativeFeeSettlementRuntimeContradictionCannotKeepCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		if _, err := fixture.settle(tb.Context()); err != nil {
			tb.Fatal(err)
		}
		fixture.statement.RuntimeCodeSha256 = "sha256:" + strings.Repeat("e", 64)
		if result, err := fixture.settle(tb.Context()); result != nil || !errors.Is(err, ErrStNativeFeeConflict) {
			tb.Fatal("changed authenticated runtime retained an earlier native fee credit", result, err)
		}
		fixture.budget(tb, "600000", "600000", "0", "600000", 1, 0)
	})
}

// A changed original inclusion outside the signed denomination interval is
// still contradictory proof about the same exactly bound signed transaction.
func TestStNativeFeeSettlementIntervalContradictionCannotKeepCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		if _, err := fixture.settle(tb.Context()); err != nil {
			tb.Fatal(err)
		}
		fixture.statement.NativeBlockNumber = fixture.policy.LastNativeBlock + 1
		if result, err := fixture.settle(tb.Context()); result != nil || !errors.Is(err, ErrStNativeFeeConflict) {
			tb.Fatal("changed authenticated native interval retained an earlier fee credit", result, err)
		}
		fixture.budget(tb, "600000", "600000", "0", "600000", 1, 0)
	})
}

func TestStNativeFeeSettlementNewProofWrapperKeepsOriginalCustody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		first, err := fixture.settle(tb.Context())
		if err != nil {
			tb.Fatal(err)
		}
		fixture.statement.ProofHash, fixture.statement.RequestHash = "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
		fixture.statement.NativeFinalizedNumber, fixture.statement.NativeFinalizedHash = 105, "0x"+strings.Repeat("c", 64)
		if result, err := fixture.settle(tb.Context()); err != nil || result == nil || !result.Reused || result.StatementSha256 != first.StatementSha256 {
			tb.Fatal("same original outcome under a later proof wrapper lost idempotence", result, err)
		}
		fixture.budget(tb, "600000", "1000", "1000", "0", 1, 1)
	})
}

func TestStNativeFeeSettlementLaterFinalizedHeadKeepsOriginalReceipt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		fixture.statement.EvmBlockNumber, fixture.statement.EvmBlockHash = 95, "0x"+strings.Repeat("a", 64)
		MarkStTransactionMined(tb.Context(), fixture.intent.IntentId, 1, fixture.attempt.TxHash, 95, fixture.statement.EvmBlockHash)
		MarkStTransactionFinalized(tb.Context(), fixture.intent.IntentId, 1, fixture.attempt.TxHash, 100, "0x"+strings.Repeat("b", 64))
		if result, err := fixture.settle(tb.Context()); err != nil || result == nil {
			tb.Fatal("later finalized observation head was confused with receipt inclusion", result, err)
		}
		fixture.budget(tb, "600000", "1000", "1000", "0", 1, 1)
	})
}

func TestStNativeFeeSettlementConflictingRetainedReceiptCreatesHold(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		fixture.statement.EvmBlockNumber, fixture.statement.EvmBlockHash = 95, "0x"+strings.Repeat("a", 64)
		MarkStTransactionMined(tb.Context(), fixture.intent.IntentId, 1, fixture.attempt.TxHash, 95, "0x"+strings.Repeat("c", 64))
		MarkStTransactionFinalized(tb.Context(), fixture.intent.IntentId, 1, fixture.attempt.TxHash, 100, "0x"+strings.Repeat("b", 64))
		if result, err := fixture.settle(tb.Context()); !errors.Is(err, ErrStNativeFeeConflict) || result != nil {
			tb.Fatal("different retained receipt inclusion silently acquired fee credit", result, err)
		}
		fixture.budget(tb, "600000", "600000", "0", "600000", 1, 0)
		if err := RequireStTransactionGasUnsettled(tb.Context(), fixture.intent.IntentId); !errors.Is(err, ErrStOperatorGasAllowance) {
			tb.Fatal("retained receipt conflict left signing authorized", err)
		}
	})
}

func TestStNativeFeeSettlementMissingConflictOriginalsCannotKeepCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		if _, err := fixture.settle(tb.Context()); err != nil {
			tb.Fatal(err)
		}
		fixture.statement.WithdrawalRao, fixture.statement.DebitRao = "250050", "250000"
		missing := errors.New("synthetic original proof removed after owned invocation")
		result, err := settleStTransactionNativeFee(tb.Context(), fixture.intent.IntentId, fixture.policy, fixture.authority, fixture.statement, func(context.Context, server.PgTx, string) error { return missing })
		if !errors.Is(err, ErrStNativeFeeConflict) || result != nil {
			tb.Fatal("missing contradictory proof originals preserved spendable credit", result, err)
		}
		fixture.budget(tb, "600000", "2500000", "0", "2500000", 1, 0)
	})
}

func TestStNativeFeeSettlementCancellationAfterConflictPersistsHold(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		if _, err := fixture.settle(tb.Context()); err != nil {
			tb.Fatal(err)
		}
		fixture.statement.WithdrawalRao, fixture.statement.DebitRao = "160", "110"
		ctx, cancel := context.WithCancel(tb.Context())
		defer cancel()
		entered := false
		result, err := settleStTransactionNativeFeeOwned(ctx, fixture.intent.IntentId, fixture.policy, fixture.authority, fixture.statement, func(context.Context, server.PgTx, string) error {
			entered = true
			cancel()
			return context.Canceled
		})
		if !entered || result != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrStNativeFeeConflict) || !errors.Is(err, errStNativeFeeHoldCommitted) {
			tb.Fatal("caller cancellation erased an already authenticated contradiction", entered, result, err)
		}
		fixture.budget(tb, "600000", "600000", "0", "600000", 1, 0)
	})
}

func TestStNativeFeeSettlementLostSignatureCannotChangeOriginalReservation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		intent := stGasModelIntent(fixture.gas, "synthetic-native-fee-lost-signature", []byte{2})
		unsigned := stGasModelTransaction(intent, 60_000, 10)
		original, err := ReserveStTransactionGasAttempt(tb.Context(), fixture.gas, fixture.gasRoot, intent.IntentId, 1, StTxAttemptExecution, unsigned)
		if err != nil {
			tb.Fatal(err)
		}
		changed, err := types.SignTx(stGasModelTransaction(intent, 60_000, 20), types.LatestSignerForChainID(new(big.Int).SetUint64(intent.ChainId)), fixture.key)
		if err != nil {
			tb.Fatal(err)
		}
		raw, err := changed.MarshalBinary()
		if err != nil {
			tb.Fatal(err)
		}
		statement := fixture.statement
		statement.TransactionHash, statement.RawTransaction, statement.Nonce = strings.ToLower(changed.Hash().Hex()), raw, intent.Nonce
		if result, err := settleStTransactionNativeFee(tb.Context(), intent.IntentId, fixture.policy, fixture.authority, statement); err == nil || result != nil {
			tb.Fatal("recovery replaced the already-reserved unsigned transaction", result, err)
		}
		retained, err := GetPendingStTransactionGasReservation(tb.Context(), intent.IntentId)
		if err != nil || retained == nil || retained.SigningHash != original.SigningHash || !bytes.Equal(retained.UnsignedTransaction, original.UnsignedTransaction) || len(GetStTransactionAttempts(tb.Context(), intent.IntentId)) != 0 {
			tb.Fatal("refused recovery changed original reservation or signature census", retained, err)
		}
		fixture.budget(tb, "1200000", "1200000", "0", "1200000", 2, 0)
	})
}

func TestStNativeFeeSettlementCannotCreditAnotherNonceOrSignature(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		other := stGasModelIntent(fixture.gas, "synthetic-native-fee-other", []byte{2})
		unsigned := stGasModelTransaction(other, 60_000, 10)
		if _, err := ReserveStTransactionGasAttempt(tb.Context(), fixture.gas, fixture.gasRoot, other.IntentId, 1, StTxAttemptExecution, unsigned); err != nil {
			tb.Fatal(err)
		}
		stGasModelCommit(tb, other, 1, unsigned, fixture.key)
		if result, err := settleStTransactionNativeFee(tb.Context(), other.IntentId, fixture.policy, fixture.authority, fixture.statement); err == nil || result != nil {
			tb.Fatal("one native fee credited another original nonce", result, err)
		}
		fixture.statement.RawTransaction = bytes.Clone(fixture.statement.RawTransaction)
		fixture.statement.RawTransaction[len(fixture.statement.RawTransaction)-1] ^= 1
		if result, err := fixture.settle(tb.Context()); err == nil || result != nil {
			tb.Fatal("different signed bytes reused native fee authority", result, err)
		}
		fixture.budget(tb, "1200000", "1200000", "0", "1200000", 2, 0)
	})
}

func TestStNativeFeeSettlementConsumesNoAdditionalSigningAttempts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		if _, err := fixture.settle(tb.Context()); err != nil {
			tb.Fatal(err)
		}
		if err := RequireStTransactionGasUnsettled(tb.Context(), fixture.intent.IntentId); !errors.Is(err, ErrStOperatorGasAllowance) {
			tb.Fatal("settled nonce retained pending signing authority", err)
		}
		if _, err := ReserveStTransactionGasAttempt(tb.Context(), fixture.gas, fixture.gasRoot, fixture.intent.IntentId, 2, StTxAttemptExecution, stGasModelTransaction(fixture.intent, 60_000, 20)); !errors.Is(err, ErrStOperatorGasAllowance) {
			tb.Fatal("settled nonce reserved a fresh signature", err)
		}
		if err := ValidateStTransactionGasBroadcast(tb.Context(), fixture.gas, fixture.gasRoot, fixture.intent.IntentId, fixture.attempt); !errors.Is(err, ErrStOperatorGasAllowance) {
			tb.Fatal("settled nonce regained broadcast authority", err)
		}
		fixture.budget(tb, "600000", "1000", "1000", "0", 1, 1)
	})
}

func TestStNativeFeeSettlementVerifiedOverspendStaysCharged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		fixture.statement.WithdrawalRao, fixture.statement.RefundRao, fixture.statement.DebitRao = "250000", "0", "250000"
		if result, err := fixture.settle(tb.Context()); err != nil || result == nil || !result.ExceedsCeiling || result.DebitWei != "2500000" {
			tb.Fatal("verified native overspend was hidden by the old reservation", result, err)
		}
		fixture.budget(tb, "600000", "2500000", "2500000", "0", 1, 1)
		if err := AdmitStOperatorGasPolicy(tb.Context(), fixture.gas, fixture.gasRoot); !errors.Is(err, ErrStOperatorGasAllowance) {
			tb.Fatal("native overspend left the original spending allowance healthy", err)
		}
	})
}

func TestStNativeFeeSettlementCanceledAndRefusedWritesLeaveNoCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		ctx, cancel := context.WithCancel(tb.Context())
		cancel()
		if result, err := fixture.settle(ctx); !errors.Is(err, context.Canceled) || result != nil {
			tb.Fatal("canceled owner published native fee settlement", result, err)
		}
		// An exact database transition rejects the final insert after owner and
		// policy staging, proving their rollback without a scheduler race.
		server.Db(tb.Context(), func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(tb.Context(), `ALTER TABLE st_operator_native_fee_settlement ADD CONSTRAINT synthetic_native_fee_refusal CHECK (debit_wei <> 1000)`))
		})
		if result, err := fixture.settle(tb.Context()); err == nil || result != nil {
			tb.Fatal("refused durable settlement returned fee credit", result, err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var owners, policies, settlements int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT (SELECT COUNT(*) FROM st_operator_native_fee_owner),(SELECT COUNT(*) FROM st_operator_native_fee_policy),(SELECT COUNT(*) FROM st_operator_native_fee_settlement)`).Scan(&owners, &policies, &settlements))
			if owners != 0 || policies != 0 || settlements != 0 {
				tb.Fatal("refused final write retained partial fee authority", owners, policies, settlements)
			}
			server.RaisePgResult(conn.Exec(tb.Context(), `ALTER TABLE st_operator_native_fee_settlement DROP CONSTRAINT synthetic_native_fee_refusal`))
		})
		fixture.budget(tb, "600000", "600000", "0", "600000", 1, 0)
		if _, err := fixture.settle(tb.Context()); err != nil {
			tb.Fatal("identical original could not retry after rollback", err)
		}
	})
}

// A rejected original stream must roll back both its already-written chunks
// and the financial settlement. This fixture owns its bytes; shared exported
// native proof files remain immutable throughout qualification.
func TestStNativeFeeSettlementPartialOriginalCustodyRollsBackCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		original := bytes.Repeat([]byte{0x6d}, stNativeFeeOriginalChunkBytes+19)
		digest := sha256.Sum256(original)
		reference := nativefee.Reference{Path: "/synthetic/native-fee-original", Sha256: "sha256:" + strings.Repeat("0", 64)}
		entered := false
		retain := func(ctx context.Context, tx server.PgTx, statementHash string) error {
			entered = true
			return stNativeFeeRetainOriginal(ctx, tx, statementHash, "archive", reference, bytes.NewReader(original))
		}
		if result, err := settleStTransactionNativeFee(tb.Context(), fixture.intent.IntentId, fixture.policy, fixture.authority, fixture.statement, retain); !entered || err == nil || result != nil {
			tb.Fatal("changed original proof digest retained financial credit", entered, result, err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var objects, chunks, references, settlements int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT (SELECT COUNT(*) FROM st_operator_native_fee_original_object),(SELECT COUNT(*) FROM st_operator_native_fee_original_chunk),(SELECT COUNT(*) FROM st_operator_native_fee_original_reference),(SELECT COUNT(*) FROM st_operator_native_fee_settlement)`).Scan(&objects, &chunks, &references, &settlements))
			if objects != 0 || chunks != 0 || references != 0 || settlements != 0 {
				tb.Fatal("refused original custody left partial durable evidence", objects, chunks, references, settlements)
			}
		})
		fixture.budget(tb, "600000", "600000", "0", "600000", 1, 0)
		reference.Sha256 = "sha256:" + hex.EncodeToString(digest[:])
		result, err := settleStTransactionNativeFee(tb.Context(), fixture.intent.IntentId, fixture.policy, fixture.authority, fixture.statement, retain)
		if err != nil || result == nil {
			tb.Fatal("complete original could not retry after atomic custody refusal", result, err)
		}
		var reproduced bytes.Buffer
		if retained, err := WriteStNativeFeeOriginal(tb.Context(), result.StatementSha256, "archive", &reproduced); err != nil || retained == nil || *retained != reference || !bytes.Equal(reproduced.Bytes(), original) {
			tb.Fatal("complete original chunks did not reconstruct the admitted input", retained, err)
		}
		fixture.budget(tb, "600000", "1000", "1000", "0", 1, 1)
	})
}

func TestStNativeFeeSettlementConcurrentRetriesKeepOneDurableCharge(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		ctx, cancel := context.WithTimeout(tb.Context(), 30*time.Second)
		start := make(chan struct{})
		type result struct {
			value *StTransactionNativeFeeSettlement
			err   error
		}
		results := make(chan result, 2)
		var workers sync.WaitGroup
		for index := 0; index < 2; index++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				select {
				case <-start:
				case <-ctx.Done():
					results <- result{err: ctx.Err()}
					return
				}
				value, err := fixture.settle(ctx)
				results <- result{value: value, err: err}
			}()
		}
		joined := make(chan struct{})
		go func() { workers.Wait(); close(joined) }()
		defer func() {
			cancel()
			select {
			case <-joined:
			case <-time.After(10 * time.Second):
				tb.Error("native fee settlement workers did not join after cancellation")
			}
		}()
		close(start)
		select {
		case <-joined:
		case <-ctx.Done():
			tb.Fatal("native fee settlement workers exceeded their owner deadline", ctx.Err())
		}
		close(results)
		reused := 0
		for item := range results {
			if item.err != nil || item.value == nil {
				tb.Fatal("identical concurrent original settlement refused", item.err)
			}
			if item.value.Reused {
				reused++
			}
		}
		if reused != 1 {
			tb.Fatal("concurrent original did not elect one retained settlement", reused)
		}
		fixture.budget(tb, "600000", "1000", "1000", "0", 1, 1)
	})
}

func TestStNativeFeeSettlementCancellationKeepsActualNativeDebit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		to := common.HexToAddress(fixture.intent.FromAddress)
		unsigned := types.NewTx(&types.LegacyTx{Nonce: fixture.intent.Nonce, To: &to, Gas: 21_000, GasPrice: big.NewInt(20), Value: new(big.Int)})
		if _, err := ReserveStTransactionGasAttempt(tb.Context(), fixture.gas, fixture.gasRoot, fixture.intent.IntentId, 2, StTxAttemptCancellation, unsigned); err != nil {
			tb.Fatal(err)
		}
		signed, err := types.SignTx(unsigned, types.LatestSignerForChainID(new(big.Int).SetUint64(fixture.gas.ChainId)), fixture.key)
		if err != nil {
			tb.Fatal(err)
		}
		raw, err := signed.MarshalBinary()
		if err != nil {
			tb.Fatal(err)
		}
		price := "20"
		attempt := AddStTransactionAttempt(tb.Context(), &StTransactionAttempt{IntentId: fixture.intent.IntentId, Attempt: 2, Kind: StTxAttemptCancellation, TxHash: strings.ToLower(signed.Hash().Hex()), RawTransaction: raw, GasLimit: 21_000, GasPrice: &price})
		fixture.statement.TransactionHash, fixture.statement.RawTransaction = attempt.TxHash, bytes.Clone(raw)
		fixture.statement.WithdrawalRao, fixture.statement.RefundRao, fixture.statement.DebitRao = "110", "10", "100"
		if result, err := fixture.settle(tb.Context()); err != nil || result.Attempt != 2 || result.OriginalCeilingWei != "600000" || result.DebitWei != "1000" {
			tb.Fatal("nonce cancellation invented zero fees or used the latest smaller ceiling", result, err)
		}
		fixture.budget(tb, "600000", "1000", "1000", "0", 2, 1)
	})
}

func TestStNativeFeeSettlementOriginalGenerationAndUnknownSuccessorBothCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		fixture.gas.Revision, fixture.gas.PreviousPolicySha256 = 1, fixture.gasRoot.PolicySha256
		fixture.gas.MaximumIntentLiabilityWei, fixture.gas.MaximumLifetimeLiabilityWei = "601000", "601000"
		stGasModelSeal(tb, fixture.gas, fixture.gasRoot, fixture.gasSigner, false)
		if err := AdmitStOperatorGasPolicy(tb.Context(), fixture.gas, fixture.gasRoot); err != nil {
			tb.Fatal(err)
		}
		fixture.statement.ReceiptStatus = 0
		MarkStTransactionReverted(tb.Context(), fixture.intent.IntentId, 1, errors.New("synthetic original revert"))
		if _, err := fixture.settle(tb.Context()); err != nil {
			tb.Fatal(err)
		}
		next := stGasModelIntent(fixture.gas, fixture.intent.LogicalKey, fixture.intent.Calldata)
		if next.IntentId == fixture.intent.IntentId || next.Nonce == fixture.intent.Nonce {
			tb.Fatal("fixture did not retain a distinct original generation")
		}
		unsigned := stGasModelTransaction(next, 60_000, 10)
		if _, err := ReserveStTransactionGasAttempt(tb.Context(), fixture.gas, fixture.gasRoot, next.IntentId, 1, StTxAttemptExecution, unsigned); err != nil {
			tb.Fatal("admitted actual fee did not release its unused ceiling", err)
		}
		stGasModelCommit(tb, next, 1, unsigned, fixture.key)
		fixture.budget(tb, "1200000", "601000", "1000", "600000", 2, 1)
	})
}

func TestStNativeFeeSettlementRetiredAccountKeepsItsOriginalLifetime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		prior := fixture.gas.Accounts[0]
		prior.Role = "retired_deposit"
		key, err := crypto.HexToECDSA(strings.Repeat("0", 62) + "68")
		if err != nil {
			tb.Fatal(err)
		}
		fixture.gas.Revision, fixture.gas.PreviousPolicySha256 = 1, fixture.gasRoot.PolicySha256
		fixture.gas.Accounts[0].Address = strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
		fixture.gas.HistoricalAccounts = []server.StOperatorGasPolicyAccount{prior}
		stGasModelSeal(tb, fixture.gas, fixture.gasRoot, fixture.gasSigner, true)
		if err := AdmitStOperatorGasPolicy(tb.Context(), fixture.gas, fixture.gasRoot); err != nil {
			tb.Fatal(err)
		}
		if _, err := fixture.settle(tb.Context()); err != nil {
			tb.Fatal("retired signing account lost its exact native settlement", err)
		}
		fixture.budget(tb, "600000", "1000", "1000", "0", 1, 1)
	})
}

func TestStNativeFeeSettlementRefusesOutsideRuntimeAndNativeInterval(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		original := fixture.statement
		for _, change := range []func(*nativefee.Statement){
			func(value *nativefee.Statement) { value.NativeBlockNumber = 99 },
			func(value *nativefee.Statement) { value.NativeBlockNumber = 201 },
			func(value *nativefee.Statement) { value.RuntimeCodeSha256 = "sha256:" + strings.Repeat("e", 64) },
			func(value *nativefee.Statement) { value.Genesis = "0x" + strings.Repeat("e", 64) },
		} {
			fixture.statement = original
			change(&fixture.statement)
			if result, err := fixture.settle(tb.Context()); err == nil || result != nil {
				tb.Fatal("original runtime or native boundary escaped denomination approval", result, err)
			}
		}
		fixture.budget(tb, "600000", "600000", "0", "600000", 1, 0)
	})
}

// Pin an old actual repeatable-read snapshot before the completed settlement.
// The same row lock as gas allowance admission must observe an MVCC conflict,
// making the transaction owner retry before it can read obsolete fee totals.
func TestStNativeFeeSettlementInvalidatesWaitingAllowanceSnapshot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		ctx, cancel := context.WithTimeout(tb.Context(), 30*time.Second)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				tb.Fatal(err)
			}
			defer func() {
				cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				defer stop()
				_ = tx.Rollback(cleanup)
			}()
			var prior string
			server.Raise(tx.QueryRow(ctx, `SELECT current_policy_sha256 FROM st_operator_gas_budget WHERE scope_key=$1`, fixture.gas.Scope()).Scan(&prior))
			fixture.statement.WithdrawalRao, fixture.statement.RefundRao, fixture.statement.DebitRao = "250000", "0", "250000"
			if _, err := fixture.settle(ctx); err != nil {
				tb.Fatal(err)
			}
			err = tx.QueryRow(ctx, `SELECT current_policy_sha256 FROM st_operator_gas_budget WHERE scope_key=$1 FOR UPDATE`, fixture.gas.Scope()).Scan(&prior)
			var conflict *pgconn.PgError
			if !errors.As(err, &conflict) || conflict.Code != "40001" {
				tb.Fatal("old allowance snapshot could ignore a completed native overspend", err)
			}
		})
		if err := AdmitStOperatorGasPolicy(tb.Context(), fixture.gas, fixture.gasRoot); !errors.Is(err, ErrStOperatorGasAllowance) {
			tb.Fatal("fresh allowance snapshot did not retain actual native overspend", err)
		}
	})
}

// Pending signing locks both the shared budget and its own intent. The latter
// also participates in the write fence for callers already holding that lock.
func TestStNativeFeeSettlementInvalidatesWaitingSigningSnapshot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fixture := newStNativeFeeModelFixture(tb)
		ctx, cancel := context.WithTimeout(tb.Context(), 30*time.Second)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				tb.Fatal(err)
			}
			defer func() {
				cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				defer stop()
				_ = tx.Rollback(cleanup)
			}()
			var prior server.Id
			server.Raise(tx.QueryRow(ctx, `SELECT intent_id FROM st_transaction_intent WHERE intent_id=$1`, fixture.intent.IntentId).Scan(&prior))
			if _, err := fixture.settle(ctx); err != nil {
				tb.Fatal(err)
			}
			err = tx.QueryRow(ctx, `SELECT intent_id FROM st_transaction_intent WHERE intent_id=$1 FOR SHARE`, fixture.intent.IntentId).Scan(&prior)
			var conflict *pgconn.PgError
			if !errors.As(err, &conflict) || conflict.Code != "40001" {
				tb.Fatal("old signing snapshot could ignore the proved consumed nonce", err)
			}
		})
		if err := RequireStTransactionGasUnsettled(tb.Context(), fixture.intent.IntentId); !errors.Is(err, ErrStOperatorGasAllowance) {
			tb.Fatal("fresh signing snapshot reopened the native-settled nonce", err)
		}
	})
}
