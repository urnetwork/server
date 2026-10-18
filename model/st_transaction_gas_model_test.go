package model

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/server"
)

func stGasModelApproval(tb testing.TB) (*server.StOperatorGasPolicy, *server.StOperatorGasAuthority, *ecdsa.PrivateKey, ed25519.PrivateKey) {
	tb.Helper()
	key, err := crypto.HexToECDSA(fmt.Sprintf("%064x", 101))
	if err != nil {
		tb.Fatal(err)
	}
	root, err := crypto.HexToECDSA(fmt.Sprintf("%064x", 102))
	if err != nil {
		tb.Fatal(err)
	}
	approval := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x62}, ed25519.SeedSize))
	now := server.NowUtc()
	p := &server.StOperatorGasPolicy{Schema: server.StOperatorGasPolicySchema, Profile: "testnet", ChainId: 945, GenesisHash: "0x" + strings.Repeat("a", 64), NoId: 1, Coordinator: "0x" + strings.Repeat("b", 40), PolicyHash: "0x" + strings.Repeat("c", 64), Accounts: []server.StOperatorGasPolicyAccount{{Role: "deposit", Address: strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())}, {Role: "root", Address: strings.ToLower(crypto.PubkeyToAddress(root.PublicKey).Hex())}}, ValidFrom: now.Add(-time.Hour).Unix(), ValidUntil: now.Add(24 * time.Hour).Unix(), MaximumGas: 3_000_000, MaximumFeePerGasWei: "1000", MaximumTipPerGasWei: "100", MaximumIntentLiabilityWei: "600000", MaximumLifetimeLiabilityWei: "600000", MaximumIntentAttempts: 8, MaximumLifetimeAttempts: 16}
	a := &server.StOperatorGasAuthority{Schema: server.StOperatorGasAuthoritySchema, Profile: p.Profile, ChainId: p.ChainId, GenesisHash: p.GenesisHash, NoId: p.NoId, ApproverPublicKey: hex.EncodeToString(approval.Public().(ed25519.PublicKey))}
	stGasModelSeal(tb, p, a, approval, true)
	return p, a, key, approval
}

func stGasModelSeal(tb testing.TB, p *server.StOperatorGasPolicy, a *server.StOperatorGasAuthority, key ed25519.PrivateKey, census bool) {
	tb.Helper()
	if census {
		for index := range p.Accounts {
			digest, err := StOperatorGasAccountHistorySha256(context.Background(), p.ChainId, p.GenesisHash, p.Accounts[index].Address, p.MaximumLifetimeAttempts)
			if err != nil {
				tb.Fatal(err)
			}
			p.Accounts[index].InitialHistorySha256 = digest
		}
	}
	encoded, err := p.SigningBytes()
	if err != nil {
		tb.Fatal(err)
	}
	p.Signature = hex.EncodeToString(ed25519.Sign(key, encoded))
	a.PolicySha256, err = p.Digest()
	if err != nil {
		tb.Fatal(err)
	}
}

func stGasModelIntent(p *server.StOperatorGasPolicy, logical string, data []byte) *StTransactionIntent {
	return ReserveStTransactionIntent(context.Background(), logical, p.Profile, "synthetic-gas-operator", StDeploymentKey(fmt.Sprintf("%d:%s", p.ChainId, p.Coordinator)), p.ChainId, p.GenesisHash, p.Accounts[0].Address, p.Coordinator, strings.ToLower(crypto.Keccak256Hash(data).Hex()), data, 7)
}

func stGasModelTransaction(intent *StTransactionIntent, gas uint64, price int64) *types.Transaction {
	to := common.HexToAddress(intent.ToAddress)
	return types.NewTx(&types.LegacyTx{Nonce: intent.Nonce, To: &to, Gas: gas, GasPrice: big.NewInt(price), Value: new(big.Int), Data: bytes.Clone(intent.Calldata)})
}

func stGasModelCommit(tb testing.TB, intent *StTransactionIntent, number int, tx *types.Transaction, key *ecdsa.PrivateKey) *StTransactionAttempt {
	tb.Helper()
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(new(big.Int).SetUint64(intent.ChainId)), key)
	if err != nil {
		tb.Fatal(err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		tb.Fatal(err)
	}
	price := signed.GasPrice().String()
	return AddStTransactionAttempt(context.Background(), &StTransactionAttempt{IntentId: intent.IntentId, Attempt: number, Kind: StTxAttemptExecution, TxHash: strings.ToLower(signed.Hash().Hex()), RawTransaction: raw, GasLimit: signed.Gas(), GasPrice: &price})
}

func TestStOperatorGasDistinctConcurrentNoncesShareLifetimeCeiling(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		p, a, _, _ := stGasModelApproval(tb)
		if err := AdmitStOperatorGasPolicy(context.Background(), p, a); err != nil {
			tb.Fatal(err)
		}
		intents := []*StTransactionIntent{stGasModelIntent(p, "synthetic-gas-a", []byte{1}), stGasModelIntent(p, "synthetic-gas-b", []byte{2})}
		if intents[0].Nonce == intents[1].Nonce {
			tb.Fatal("fixture failed to reserve distinct same-account nonces")
		}
		workerCtx, cancelWorkers := context.WithTimeout(tb.Context(), 30*time.Second)
		start := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		for _, intent := range intents {
			workers.Add(1)
			go func(intent *StTransactionIntent) {
				defer workers.Done()
				select {
				case <-start:
				case <-workerCtx.Done():
					results <- workerCtx.Err()
					return
				}
				_, err := ReserveStTransactionGasAttempt(workerCtx, p, a, intent.IntentId, 1, StTxAttemptExecution, stGasModelTransaction(intent, 60_000, 10))
				results <- err
			}(intent)
		}
		joined := make(chan struct{})
		go func() {
			workers.Wait()
			close(joined)
		}()
		defer func() {
			cancelWorkers()
			select {
			case <-joined:
			case <-time.After(10 * time.Second):
				tb.Error("operator gas reservation workers did not join after cancellation")
			}
		}()
		close(start)
		// The launch channel establishes the competing requests. These
		// deadlines only bound a broken database lock/cancellation path.
		select {
		case <-joined:
		case <-workerCtx.Done():
			tb.Fatal("operator gas reservation workers exceeded their parent deadline", workerCtx.Err())
		}
		close(results)
		success, refused := 0, 0
		for err := range results {
			if err == nil {
				success++
			} else if errors.Is(err, ErrStOperatorGasAllowance) {
				refused++
			} else {
				tb.Fatal(err)
			}
		}
		snapshot, err := GetStOperatorGasBudgetSnapshot(workerCtx, p.Scope())
		if err != nil || success != 1 || refused != 1 || snapshot == nil || snapshot.MaximumLiabilityWei != "600000" || snapshot.Attempts != 1 {
			tb.Fatalf("same-account concurrent writers exceeded lifetime: success=%d refused=%d snapshot=%+v error=%v", success, refused, snapshot, err)
		}
	})
}

func TestStOperatorGasInitialHistoryCountsEveryRevertedGeneration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		p, a, key, approval := stGasModelApproval(tb)
		p.MaximumIntentLiabilityWei = "2400000"
		p.MaximumLifetimeLiabilityWei = "2400000"
		p.MaximumIntentAttempts = 4
		first := stGasModelIntent(p, "synthetic-gas-generations", []byte{1})
		for number := 1; number <= 3; number++ {
			stGasModelCommit(tb, first, number, stGasModelTransaction(first, 60_000, int64(10*number)), key)
		}
		MarkStTransactionReverted(context.Background(), first.IntentId, 3, errors.New("synthetic canonical revert"))
		second := stGasModelIntent(p, first.LogicalKey, first.Calldata)
		stGasModelCommit(tb, second, 1, stGasModelTransaction(second, 60_000, 10), key)
		stGasModelSeal(tb, p, a, approval, true)
		if err := AdmitStOperatorGasPolicy(context.Background(), p, a); err != nil {
			tb.Fatal("finite cumulative approval could not adopt original generations", err)
		}
		snapshot, err := GetStOperatorGasBudgetSnapshot(context.Background(), p.Scope())
		if err != nil || snapshot.MaximumLiabilityWei != "2400000" || snapshot.Attempts != 4 {
			tb.Fatal("reverted generations were omitted from original ceiling", snapshot, err)
		}
	})
}

func TestStOperatorGasPolicyRevisionCannotDropOrMoveOldAccount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		p, a, _, approval := stGasModelApproval(tb)
		if err := AdmitStOperatorGasPolicy(context.Background(), p, a); err != nil {
			tb.Fatal(err)
		}
		old := p.Accounts[0]
		old.Role = "retired_deposit"
		next, err := crypto.HexToECDSA(fmt.Sprintf("%064x", 103))
		if err != nil {
			tb.Fatal(err)
		}
		prior := a.PolicySha256
		p.Revision = 1
		p.PreviousPolicySha256 = prior
		p.Accounts[0].Address = strings.ToLower(crypto.PubkeyToAddress(next.PublicKey).Hex())
		p.HistoricalAccounts = []server.StOperatorGasPolicyAccount{old}
		stGasModelSeal(tb, p, a, approval, true)
		if err := AdmitStOperatorGasPolicy(context.Background(), p, a); err != nil {
			tb.Fatal(err)
		}
		prior = a.PolicySha256
		p.Revision = 2
		p.PreviousPolicySha256 = prior
		p.HistoricalAccounts = nil
		stGasModelSeal(tb, p, a, approval, false)
		if err := AdmitStOperatorGasPolicy(context.Background(), p, a); err == nil {
			tb.Fatal("successor policy omitted original account census")
		}
		p.Revision = 0
		p.PreviousPolicySha256 = ""
		p.NoId = 2
		a.NoId = 2
		stGasModelSeal(tb, p, a, approval, false)
		if err := AdmitStOperatorGasPolicy(context.Background(), p, a); err == nil {
			tb.Fatal("new operator scope reset an enrolled wallet's lifetime")
		}
	})
}

func TestStOperatorGasInitialHistoryRejectsOversizedOriginalBeforeDecode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		p, _, _, _ := stGasModelApproval(tb)
		intent := stGasModelIntent(p, "synthetic-gas-oversized", []byte{1})
		price := "10"
		raw := bytes.Repeat([]byte{0x71}, stGasMaximumTransactionBytes+1)
		AddStTransactionAttempt(context.Background(), &StTransactionAttempt{IntentId: intent.IntentId, Attempt: 1, Kind: StTxAttemptExecution, TxHash: "0x" + strings.Repeat("1", 64), RawTransaction: raw, GasLimit: 60_000, GasPrice: &price})
		if _, err := StOperatorGasAccountHistorySha256(context.Background(), p.ChainId, p.GenesisHash, p.Accounts[0].Address, p.MaximumLifetimeAttempts); err == nil || !strings.Contains(err.Error(), "byte/count") {
			tb.Fatal("oversized history reached transaction decoding", err)
		}
		if !bytes.Equal(GetCurrentStTransactionAttempt(context.Background(), intent.IntentId).RawTransaction, raw) {
			tb.Fatal("bounded refusal trimmed original signature bytes")
		}
	})
}

func TestStOperatorGasSignedEncodingBoundPrecedesReservation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		p, a, key, approval := stGasModelApproval(tb)
		p.MaximumIntentLiabilityWei = "30000000"
		p.MaximumLifetimeLiabilityWei = "60000000"
		stGasModelSeal(tb, p, a, approval, false)
		if err := AdmitStOperatorGasPolicy(context.Background(), p, a); err != nil {
			tb.Fatal(err)
		}
		oversized := stGasModelIntent(p, "synthetic-gas-signature-width", bytes.Repeat([]byte{1}, stGasMaximumTransactionBytes-80))
		unsigned := stGasModelTransaction(oversized, 3_000_000, 10)
		raw, _ := unsigned.MarshalBinary()
		signed, err := types.SignTx(unsigned, types.LatestSignerForChainID(big.NewInt(945)), key)
		if err != nil {
			tb.Fatal(err)
		}
		signedRaw, _ := signed.MarshalBinary()
		if len(raw) > stGasMaximumTransactionBytes || len(signedRaw) <= stGasMaximumTransactionBytes {
			tb.Fatal("fixture does not cross only the signature encoding boundary")
		}
		if _, err := ReserveStTransactionGasAttempt(context.Background(), p, a, oversized.IntentId, 1, StTxAttemptExecution, unsigned); err == nil || !strings.Contains(err.Error(), "signed envelope") {
			tb.Fatal("signature encoding escaped retained byte limit", err)
		}
		snapshot, err := GetStOperatorGasBudgetSnapshot(context.Background(), p.Scope())
		if err != nil || snapshot.Attempts != 0 || snapshot.MaximumLiabilityWei != "0" {
			tb.Fatal("oversized envelope consumed an un-signable reservation", snapshot, err)
		}
		var widest [65]byte
		for index := 0; index < 64; index++ {
			widest[index] = 255
		}
		widest[64] = 1
		projected, err := unsigned.WithSignature(types.LatestSignerForChainID(big.NewInt(945)), widest[:])
		if err != nil {
			tb.Fatal(err)
		}
		projectedRaw, _ := projected.MarshalBinary()
		allowedSize := len(oversized.Calldata) - (len(projectedRaw) - stGasMaximumTransactionBytes)
		exact := stGasModelIntent(p, "synthetic-gas-signature-exact-bound", bytes.Repeat([]byte{1}, allowedSize))
		exactUnsigned := stGasModelTransaction(exact, 3_000_000, 10)
		projected, err = exactUnsigned.WithSignature(types.LatestSignerForChainID(big.NewInt(945)), widest[:])
		if err != nil {
			tb.Fatal(err)
		}
		projectedRaw, _ = projected.MarshalBinary()
		if len(projectedRaw) != stGasMaximumTransactionBytes {
			tb.Fatal("fixture did not reach exact retained signature byte boundary")
		}
		if _, err := ReserveStTransactionGasAttempt(context.Background(), p, a, exact.IntentId, 1, StTxAttemptExecution, exactUnsigned); err != nil {
			tb.Fatal("exact supported signed-byte boundary was refused", err)
		}
		snapshot, err = GetStOperatorGasBudgetSnapshot(context.Background(), p.Scope())
		if err != nil || snapshot.Attempts != 1 || snapshot.MaximumLiabilityWei != "30000000" {
			tb.Fatal("exact supported byte boundary did not reserve its full liability", snapshot, err)
		}
	})
}

func TestStOperatorGasUnsignedHistoryIsRefusedBeforeBodyLoading(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		p, _, _, _ := stGasModelApproval(tb)
		data := bytes.Repeat([]byte{0x71}, stGasMaximumTransactionBytes+1)
		intent := stGasModelIntent(p, "synthetic-gas-unsigned-oversized", data)
		if _, err := StOperatorGasAccountHistorySha256(context.Background(), p.ChainId, p.GenesisHash, p.Accounts[0].Address, p.MaximumLifetimeAttempts); err == nil || !strings.Contains(err.Error(), "before bounded census") {
			tb.Fatal("unsigned original reached body loading before refusal", err)
		}
		if retained := GetStTransactionIntent(context.Background(), intent.LogicalKey); retained.AttemptCount != 0 || !bytes.Equal(retained.Calldata, data) {
			tb.Fatal("unsigned bounded refusal removed original calldata")
		}
	})
}

func TestStOperatorGasEnvelopeChecksDynamicCapsAndUint256Product(t *testing.T) {
	p := &server.StOperatorGasPolicy{Schema: server.StOperatorGasPolicySchema, Profile: "testnet", ChainId: 945, GenesisHash: "0x" + strings.Repeat("a", 64), NoId: 1, Coordinator: "0x" + strings.Repeat("b", 40), PolicyHash: "0x" + strings.Repeat("c", 64), Accounts: []server.StOperatorGasPolicyAccount{{Role: "deposit", Address: "0x" + strings.Repeat("1", 40), InitialHistorySha256: strings.Repeat("2", 64)}, {Role: "root", Address: "0x" + strings.Repeat("3", 40), InitialHistorySha256: strings.Repeat("4", 64)}}, ValidFrom: 1, ValidUntil: 2, MaximumGas: 60_000, MaximumFeePerGasWei: "10", MaximumTipPerGasWei: "2", MaximumIntentLiabilityWei: "600000", MaximumLifetimeLiabilityWei: "600000", MaximumIntentAttempts: 3, MaximumLifetimeAttempts: 3}
	to := common.HexToAddress(p.Coordinator)
	good := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(945), Nonce: 7, To: &to, Gas: 60_000, GasTipCap: big.NewInt(2), GasFeeCap: big.NewInt(10), Value: new(big.Int)})
	if value, err := ValidateStTransactionGasEnvelope(p, good); err != nil || value.String() != "600000" {
		t.Fatal("exact dynamic envelope edge was refused", value, err)
	}
	badTip := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(945), Nonce: 7, To: &to, Gas: 60_000, GasTipCap: big.NewInt(3), GasFeeCap: big.NewInt(10), Value: new(big.Int)})
	if _, err := ValidateStTransactionGasEnvelope(p, badTip); !errors.Is(err, ErrStOperatorGasAllowance) {
		t.Fatal("dynamic tip escaped explicit cap", err)
	}
	wrongChain := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(946), Nonce: 7, To: &to, Gas: 60_000, GasTipCap: big.NewInt(2), GasFeeCap: big.NewInt(10), Value: new(big.Int)})
	if _, err := ValidateStTransactionGasEnvelope(p, wrongChain); !errors.Is(err, ErrStOperatorGasAllowance) {
		t.Fatal("typed envelope escaped approved chain", err)
	}
	price := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	overflow := types.NewTx(&types.LegacyTx{Nonce: 7, To: &to, Gas: 2, GasPrice: price, Value: new(big.Int)})
	if _, err := StTransactionGasLiability(overflow); err == nil {
		t.Fatal("gas product overflow gained wrapped allowance")
	}
}

// SV-1 (retries; §8.1 gas policy): under an operator gas policy a new intent
// takes its nonce only together with its first attempt's gas reservation.
// Without that attempt none is created. A first attempt the policy refuses,
// here above its maximum_gas and then above its liability, leaves no intent
// and takes no nonce: the next intent gets nonce 7. An admitted one leaves the
// intent and a pending reservation of the exact attempt at its nonce, and an
// existing intent is returned without a second reservation, with or without
// an attempt.
func TestStOperatorGasNewIntentTakesItsNonceOnlyWithItsFirstReservation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		p, a, _, _ := stGasModelApproval(tb)
		ctx := context.Background()
		reserve := func(logical string, data []byte, gas uint64) (*StTransactionIntent, error) {
			var unsigned func(uint64) *types.Transaction
			if gas != 0 {
				unsigned = func(nonce uint64) *types.Transaction {
					return stGasModelTransaction(&StTransactionIntent{Nonce: nonce, ToAddress: p.Coordinator, Calldata: data}, gas, 10)
				}
			}
			return ReserveStTransactionIntentGas(ctx, p, a, logical, p.Profile, "synthetic-gas-operator", StDeploymentKey(fmt.Sprintf("%d:%s", p.ChainId, p.Coordinator)), p.ChainId, p.GenesisHash, p.Accounts[0].Address, p.Coordinator,
				strings.ToLower(crypto.Keccak256Hash(data).Hex()), data, 7, unsigned)
		}
		if intent, err := reserve("synthetic-gas-admitted", []byte{2}, 0); !errors.Is(err, ErrStTransactionIntentNew) || intent != nil || GetStTransactionIntent(ctx, "synthetic-gas-admitted") != nil {
			tb.Fatalf("a new intent without its first attempt: %+v, %v", intent, err)
		}
		for _, refusal := range []struct {
			name string
			gas  uint64
		}{{"maximum_gas", 3_000_001}, {"liability", 60_001}} {
			if intent, err := reserve("synthetic-gas-refused-"+refusal.name, []byte{1}, refusal.gas); !errors.Is(err, ErrStOperatorGasAllowance) || intent != nil {
				tb.Fatalf("%s: intent %+v, %v", refusal.name, intent, err)
			}
			if GetStTransactionIntent(ctx, "synthetic-gas-refused-"+refusal.name) != nil {
				tb.Fatalf("%s: the refused attempt left an intent", refusal.name)
			}
		}
		intent, err := reserve("synthetic-gas-admitted", []byte{2}, 60_000)
		if err != nil || intent == nil || intent.Nonce != 7 || intent.Generation != 0 || intent.Status != StTxPrepared || intent.AttemptCount != 0 {
			tb.Fatalf("admitted intent %+v, %v", intent, err)
		}
		pending, err := GetPendingStTransactionGasReservation(ctx, intent.IntentId)
		want, _ := stGasModelTransaction(intent, 60_000, 10).MarshalBinary()
		if err != nil || pending == nil || pending.Attempt != 1 || pending.Kind != StTxAttemptExecution || !bytes.Equal(pending.UnsignedTransaction, want) || pending.MaximumLiabilityWei != "600000" {
			tb.Fatalf("admitted reservation %+v, %v", pending, err)
		}
		for _, gas := range []uint64{60_000, 0} {
			again, err := reserve("synthetic-gas-admitted", []byte{2}, gas)
			if err != nil || again == nil || again.IntentId != intent.IntentId {
				tb.Fatalf("existing intent %+v, %v", again, err)
			}
		}
		snapshot, err := GetStOperatorGasBudgetSnapshot(ctx, p.Scope())
		if err != nil || snapshot == nil || snapshot.Attempts != 1 || snapshot.MaximumLiabilityWei != "600000" {
			tb.Fatalf("budget %+v, %v", snapshot, err)
		}
	})
}

// SV-1 (§8.1 gas policy): the liability the policy still accepts for one more
// attempt of an operation's nonce is its per-operation cap, the lower of the
// one its first reservation recorded and the policy's, less the operation's
// other nonces' charges, and its lifetime cap less every other nonce's. The
// attempt's own nonce is left out, since its charge is its largest attempt's.
func TestStOperatorGasRemainingLiabilityLeavesOutTheAttemptsOwnNonce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		p, a, _, approval := stGasModelApproval(tb)
		p.MaximumLifetimeLiabilityWei = "1000000"
		stGasModelSeal(tb, p, a, approval, false)
		if err := AdmitStOperatorGasPolicy(ctx, p, a); err != nil {
			tb.Fatal(err)
		}
		remaining := func(step string, logical string, intentId server.Id, operation int64, lifetime int64) {
			tb.Helper()
			gotOperation, gotLifetime, err := StOperatorGasRemainingLiability(ctx, p, logical, intentId)
			if err != nil || gotOperation.Int64() != operation || gotLifetime.Int64() != lifetime {
				tb.Fatalf("%s: %v and %v, %v; want %d and %d", step, gotOperation, gotLifetime, err, operation, lifetime)
			}
		}
		remaining("before any reservation", "synthetic-remaining-a", server.Id{}, 600_000, 1_000_000)

		// a charges 200,000 wei and b 300,000, each at one nonce. a's second
		// attempt, smaller than its first, leaves its nonce's charge as it is.
		intents := map[string]*StTransactionIntent{}
		for _, reservation := range []struct {
			logical string
			attempt int
			gas     uint64
		}{{"synthetic-remaining-a", 1, 20_000}, {"synthetic-remaining-b", 1, 30_000}, {"synthetic-remaining-a", 2, 10_000}} {
			intent := intents[reservation.logical]
			if intent == nil {
				intent = stGasModelIntent(p, reservation.logical, []byte(reservation.logical))
				intents[reservation.logical] = intent
			}
			unsigned := stGasModelTransaction(intent, reservation.gas, 10)
			if _, err := ReserveStTransactionGasAttempt(ctx, p, a, intent.IntentId, reservation.attempt, StTxAttemptExecution, unsigned); err != nil {
				tb.Fatal(err)
			}
			key, err := crypto.HexToECDSA(fmt.Sprintf("%064x", 101))
			if err != nil {
				tb.Fatal(err)
			}
			if stGasModelCommit(tb, intent, reservation.attempt, unsigned, key) == nil {
				tb.Fatalf("%s attempt %d was not stored", reservation.logical, reservation.attempt)
			}
		}
		own := intents["synthetic-remaining-a"].IntentId
		remaining("a new nonce of a", "synthetic-remaining-a", server.Id{}, 400_000, 500_000)
		remaining("a's own nonce", "synthetic-remaining-a", own, 600_000, 700_000)
		remaining("an operation without a nonce", "synthetic-remaining-c", server.Id{}, 600_000, 500_000)

		// A revision that raises the per-intent cap leaves a the 600,000 its
		// first reservation recorded. One that lowers it binds, and one that
		// lowers either cap below the charges leaves nothing.
		for _, revision := range []struct {
			name      string
			operation string
			lifetime  string
			remaining [2]int64
		}{
			{"a raised per-intent cap", "700000", "1000000", [2]int64{400_000, 500_000}},
			{"a lowered per-intent cap", "500000", "1000000", [2]int64{300_000, 500_000}},
			{"a lifetime cap below the charges", "500000", "500000", [2]int64{300_000, 0}},
		} {
			p.Revision, p.PreviousPolicySha256 = p.Revision+1, a.PolicySha256
			p.MaximumIntentLiabilityWei, p.MaximumLifetimeLiabilityWei = revision.operation, revision.lifetime
			stGasModelSeal(tb, p, a, approval, false)
			if err := AdmitStOperatorGasPolicy(ctx, p, a); err != nil {
				tb.Fatalf("%s: %v", revision.name, err)
			}
			remaining("a new nonce of a under "+revision.name, "synthetic-remaining-a", server.Id{}, revision.remaining[0], revision.remaining[1])
		}
	})
}
