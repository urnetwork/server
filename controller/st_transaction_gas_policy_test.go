package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// All financial quantities and keys in this fixture are synthetic. Tests pass
// actual signed policy bytes through the same independent authority parser.
func stGasFixturePolicy(tb testing.TB, client *CoreStClient, edit func(*server.StOperatorGasPolicy)) (*server.StOperatorGasPolicy, *server.StOperatorGasAuthority, ed25519.PrivateKey) {
	tb.Helper()
	approvalKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x71}, ed25519.SeedSize))
	now := server.NowUtc()
	p := &server.StOperatorGasPolicy{Schema: server.StOperatorGasPolicySchema, Profile: client.cfg.Profile, ChainId: client.cfg.ChainId, GenesisHash: hexutil.Encode(client.cfg.GenesisHash[:]), NoId: client.cfg.NoId, Coordinator: strings.ToLower(client.cfg.ContractAddress.Hex()), PolicyHash: hexutil.Encode(client.cfg.PolicyHash[:]), ValidFrom: now.Add(-time.Hour).Unix(), ValidUntil: now.Add(24 * time.Hour).Unix(), MaximumGas: 100_000, MaximumFeePerGasWei: "1000", MaximumTipPerGasWei: "100", MaximumIntentLiabilityWei: "600000", MaximumLifetimeLiabilityWei: "1200000", MaximumIntentAttempts: 3, MaximumLifetimeAttempts: 12}
	for index, key := range []*ecdsa.PrivateKey{client.cfg.DepositKey, client.cfg.RootKey} {
		address := strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
		history, err := model.StOperatorGasAccountHistorySha256(context.Background(), p.ChainId, p.GenesisHash, address, p.MaximumLifetimeAttempts)
		if err != nil {
			tb.Fatal(err)
		}
		role := "deposit"
		if index == 1 {
			role = "root"
		}
		p.Accounts = append(p.Accounts, server.StOperatorGasPolicyAccount{Role: role, Address: address, InitialHistorySha256: history})
	}
	if edit != nil {
		edit(p)
	}
	a := &server.StOperatorGasAuthority{Schema: server.StOperatorGasAuthoritySchema, Profile: p.Profile, ChainId: p.ChainId, GenesisHash: p.GenesisHash, NoId: p.NoId, ApproverPublicKey: hex.EncodeToString(approvalKey.Public().(ed25519.PublicKey))}
	stGasSealFixturePolicy(tb, p, a, approvalKey)
	client.cfg.OperatorGasPolicy = p
	client.gasAuthority = func(context.Context) ([]byte, error) { return json.Marshal(a) }
	return p, a, approvalKey
}

func stGasSealFixturePolicy(tb testing.TB, p *server.StOperatorGasPolicy, a *server.StOperatorGasAuthority, key ed25519.PrivateKey) {
	tb.Helper()
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

func stGasFixtureSnapshot(tb testing.TB, p *server.StOperatorGasPolicy, amount string, attempts int64) {
	tb.Helper()
	snapshot, err := model.GetStOperatorGasBudgetSnapshot(context.Background(), p.Scope())
	if err != nil || snapshot == nil || snapshot.MaximumLiabilityWei != amount || snapshot.Attempts != attempts {
		tb.Fatalf("original gas ceiling/count changed: snapshot=%+v error=%v want=%s/%d", snapshot, err, amount, attempts)
	}
}

func stGasFixtureRestart(client *CoreStClient) *CoreStClient {
	return &CoreStClient{cfg: client.cfg, coordinator: client.coordinator, vault: client.vault, clients: client.clients, readHooks: client.readHooks, gasAuthority: client.gasAuthority, gasNow: client.gasNow}
}

func stGasCurrentLogical(tb testing.TB, client *CoreStClient) string {
	tb.Helper()
	logical, err := stTransactionLogicalKey(client.cfg, fmt.Sprintf("deposit:7:%d:3", client.cfg.NoId))
	if err != nil {
		tb.Fatal(err)
	}
	return logical
}

func TestStOperatorGasWriterReservesBeforeSigningAndKeepsFinalizedCeiling(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, _, _ := stGasFixturePolicy(tb, client, func(p *server.StOperatorGasPolicy) { p.MaximumLifetimeLiabilityWei = "600000" })
		signs := 0
		client.transactionSigner = func(ctx context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			signs++
			stGasFixtureSnapshot(tb, p, "600000", 1)
			intent := model.GetStTransactionIntent(ctx, stGasCurrentLogical(tb, client))
			pending, err := model.GetPendingStTransactionGasReservation(ctx, intent.IntentId)
			if err != nil || pending == nil || pending.SigningHash != strings.ToLower(signer.Hash(tx).Hex()) || len(model.GetStTransactionAttempts(ctx, intent.IntentId)) != 0 {
				tb.Fatalf("signer entered before exact durable reservation: %v", err)
			}
			return types.SignTx(tx, signer, key)
		}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		intent := model.GetStTransactionIntent(context.Background(), stGasCurrentLogical(tb, client))
		if signs != 1 || intent.Status != model.StTxFinalized || len(fixture.sent) != 1 || fixture.deposited.Int64() != 100 || fixture.source.Int64() != 10_000 {
			tb.Fatal("approved exact-edge writer changed principal or did not finalize once")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

func TestStOperatorGasWriterLostSignerResultRestoresOriginalBeforeQuote(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, _, _ := stGasFixturePolicy(tb, client, nil)
		lost := errors.New("synthetic signer result lost after producing signature")
		var original []byte
		client.transactionSigner = func(_ context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			signed, err := types.SignTx(tx, signer, key)
			if err != nil {
				return nil, err
			}
			original, _ = signed.MarshalBinary()
			return nil, lost
		}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, lost) {
			tb.Fatal("unknown signer result was not retained", err)
		}
		intent := model.GetStTransactionIntent(context.Background(), stGasCurrentLogical(tb, client))
		pending, err := model.GetPendingStTransactionGasReservation(context.Background(), intent.IntentId)
		if err != nil || pending == nil || intent.AttemptCount != 0 || len(fixture.sent) != 0 {
			tb.Fatal("lost signer result discarded unsigned original or sent", err)
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
		var quotes atomic.Int32
		url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(w http.ResponseWriter, call stEpochRPCRequest) bool {
			if call.Method == "eth_gasPrice" {
				quotes.Add(1)
				stWalletRecoveryReply(w, call, "0x186a0", false)
				return true
			}
			return false
		})
		restarted := stGasFixtureRestart(client)
		restarted.cfg.RpcUrls = []string{url}
		restarted.clients = map[string]*ethclient.Client{url: endpoint}
		restarted.transactionSigner = func(_ context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			signed, err := types.SignTx(tx, signer, key)
			if err != nil {
				return nil, err
			}
			raw, _ := signed.MarshalBinary()
			if !bytes.Equal(raw, original) {
				tb.Error("restart signed a different transaction after unknown result")
			}
			return signed, nil
		}
		if _, err := restarted.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatal("original unsigned reservation did not recover", err)
		}
		if quotes.Load() != 0 || len(fixture.sent) != 1 {
			tb.Fatal("unknown original was replaced by a fresh quote or duplicate send")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

func TestStOperatorGasUnknownSignatureCannotBeSupersededByAdvancedNonce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, _, _ := stGasFixturePolicy(tb, client, nil)
		calls := 0
		lost := errors.New("synthetic unknown signature")
		client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
			calls++
			return nil, lost
		}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, lost) {
			tb.Fatal(err)
		}
		fixture.accountNonce = 8
		restarted := stGasFixtureRestart(client)
		restarted.transactionSigner = client.transactionSigner
		if _, err := restarted.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err == nil {
			tb.Fatal("advanced nonce authorized another unknown signature")
		}
		intent := model.GetStTransactionIntent(context.Background(), stGasCurrentLogical(tb, client))
		if intent.Status != model.StTxPrepared || intent.AttemptCount != 0 || calls != 1 || len(fixture.sent) != 0 {
			tb.Fatal("advanced nonce erased pre-sign original liability")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

func TestStOperatorGasMissingPolicyCannotUnenrollOriginalAccount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, _, _ := stGasFixturePolicy(tb, client, nil)
		lost := errors.New("synthetic unsigned outcome held")
		client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
			return nil, lost
		}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, lost) {
			tb.Fatal(err)
		}
		client.cfg.OperatorGasPolicy = nil
		if _, err := stGasFixtureRestart(client).DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			tb.Fatal("removing signed policy restored legacy signing", err)
		}
		if len(fixture.sent) != 0 {
			tb.Fatal("policy removal broadcast an unbudgeted signature")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

func TestStOperatorGasExpiryDuringSigningKeepsReturnedSignature(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, _, _ := stGasFixturePolicy(tb, client, nil)
		var now atomic.Int64
		now.Store(server.NowUtc().Unix())
		client.gasNow = func() time.Time { return time.Unix(now.Load(), 0) }
		client.transactionSigner = func(_ context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			signed, err := types.SignTx(tx, signer, key)
			now.Store(p.ValidUntil)
			return signed, err
		}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err == nil {
			tb.Fatal("expired signer result was broadcast")
		}
		intent := model.GetStTransactionIntent(context.Background(), stGasCurrentLogical(tb, client))
		if intent.AttemptCount != 1 || len(model.GetStTransactionAttempts(context.Background(), intent.IntentId)) != 1 || len(fixture.sent) != 0 {
			tb.Fatal("expiry erased returned signature or sent after expiry")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
		attempt := model.GetCurrentStTransactionAttempt(context.Background(), intent.IntentId)
		stWalletRecoveryInclude(fixture, attempt)
		_, _ = stGasFixtureRestart(client).DepositCredit(context.Background(), 7, 1, big.NewInt(100))
		if model.GetStTransactionIntent(context.Background(), intent.LogicalKey).Status != model.StTxFinalized || len(fixture.sent) != 0 {
			tb.Fatal("expired policy blocked original receipt reconciliation")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

func TestStOperatorGasCancellationUsesSameNonceMaximumWithoutRebate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		old := stWalletRecoveryIntent(tb, client, 6, 99, 1)
		original := model.GetCurrentStTransactionAttempt(context.Background(), old.IntentId)
		p, _, _ := stGasFixturePolicy(tb, client, nil)
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		attempts := model.GetStTransactionAttempts(context.Background(), old.IntentId)
		if model.GetStTransactionIntent(context.Background(), old.LogicalKey).Status != model.StTxCanceled || len(attempts) != 2 || !bytes.Equal(attempts[1].RawTransaction, original.RawTransaction) || attempts[0].Kind != model.StTxAttemptCancellation || len(fixture.sent) != 2 {
			tb.Fatal("cancellation lost original nonce ownership")
		}
		stGasFixtureSnapshot(tb, p, "1200000", 3)
	})
}

func TestStOperatorGasLifetimeExhaustionAfterCancellationStopsNextNonce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		old := stWalletRecoveryIntent(tb, client, 6, 99, 1)
		p, _, _ := stGasFixturePolicy(tb, client, func(p *server.StOperatorGasPolicy) { p.MaximumLifetimeLiabilityWei = "1199999" })
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			tb.Fatal("lifetime exhaustion did not stop next nonce", err)
		}
		if model.GetStTransactionIntent(context.Background(), old.LogicalKey).Status != model.StTxCanceled || len(fixture.sent) != 1 || fixture.deposited.Sign() != 0 {
			tb.Fatal("cancellation invented rebate or repeated principal")
		}
		stGasFixtureSnapshot(tb, p, "600000", 2)
	})
}

func TestStOperatorGasWriterRejectsIndependentPinAndEnvelopeLimits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		for index, kind := range []string{"missing_authority", "changed_pin", "changed_signature", "gas_limit", "fee_limit", "intent_limit"} {
			client, _, fixture := newStDepositCustodyFixture(tb, uint64(index+1))
			fixture.staged.SetInt64(102)
			p, a, _ := stGasFixturePolicy(tb, client, func(p *server.StOperatorGasPolicy) {
				switch kind {
				case "gas_limit":
					p.MaximumGas = 59_999
				case "fee_limit":
					p.MaximumFeePerGasWei = "9"
					p.MaximumTipPerGasWei = "9"
				case "intent_limit":
					p.MaximumIntentLiabilityWei = "599999"
				}
			})
			switch kind {
			case "missing_authority":
				client.gasAuthority = func(context.Context) ([]byte, error) { return nil, errors.New("synthetic public authority absent") }
			case "changed_pin":
				a.PolicySha256 = strings.Repeat("a", 64)
			case "changed_signature":
				p.Signature = strings.Repeat("00", 64)
			}
			signs := 0
			client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
				signs++
				return nil, errors.New("unexpected signer entry")
			}
			if _, err := client.DepositCredit(context.Background(), 7, uint64(index+1), big.NewInt(100)); err == nil || signs != 0 || len(fixture.sent) != 0 {
				tb.Fatalf("%s reached signer without original allowance: %v", kind, err)
			}
		}
	})
}

func TestStOperatorGasInitialRetiredAccountRemainsInLifetime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		old := stWalletRecoveryIntent(tb, client, 6, 99, 1)
		oldAddress := old.FromAddress
		history, err := model.StOperatorGasAccountHistorySha256(context.Background(), client.cfg.ChainId, old.GenesisHash, oldAddress, 12)
		if err != nil {
			tb.Fatal(err)
		}
		client.cfg.DepositKey, err = crypto.HexToECDSA(fmt.Sprintf("%064x", 501))
		if err != nil {
			tb.Fatal(err)
		}
		p, _, _ := stGasFixturePolicy(tb, client, func(p *server.StOperatorGasPolicy) {
			p.MaximumLifetimeLiabilityWei = "600000"
			p.HistoricalAccounts = []server.StOperatorGasPolicyAccount{{Role: "retired_deposit", Address: oldAddress, InitialHistorySha256: history}}
		})
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			tb.Fatal("retired original account escaped lifetime ceiling", err)
		}
		if len(fixture.sent) != 0 || model.GetStTransactionIntent(context.Background(), old.LogicalKey).AttemptCount != 1 {
			tb.Fatal("retired key was discarded or conferred current signing authority")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

func TestStOperatorGasLegacyGenesisCannotBecomeEmptyHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		old := stWalletRecoveryIntent(tb, client, 6, 99, 1)
		p, a, key := stGasFixturePolicy(tb, client, nil)
		server.Tx(context.Background(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(context.Background(), `UPDATE st_transaction_intent SET genesis_hash='legacy' WHERE intent_id=$1`, old.IntentId))
		})
		empty := sha256.Sum256([]byte("urnetwork-operator-gas-history-v1\n"))
		p.Accounts[0].InitialHistorySha256 = hex.EncodeToString(empty[:])
		stGasSealFixturePolicy(tb, p, a, key)
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err == nil || !strings.Contains(err.Error(), "legacy network") {
			tb.Fatal("ambiguous legacy original was treated as empty network history", err)
		}
		if len(fixture.sent) != 0 {
			tb.Fatal("legacy-unknown history signed new liability")
		}
	})
}

func TestStOperatorGasLifetimeIncreaseKeepsOriginalRevertAllocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		fixture.fault = "deposit_revert"
		p, a, key := stGasFixturePolicy(tb, client, func(p *server.StOperatorGasPolicy) { p.MaximumLifetimeLiabilityWei = "600000" })
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err == nil {
			tb.Fatal("synthetic canonical revert not returned")
		}
		old := model.GetStTransactionIntent(context.Background(), stGasCurrentLogical(tb, client))
		if old.Status != model.StTxReverted {
			tb.Fatal("first generation was not canonically reverted")
		}
		prior := a.PolicySha256
		p.Revision = 1
		p.PreviousPolicySha256 = prior
		p.MaximumLifetimeLiabilityWei = "1200000"
		p.MaximumIntentLiabilityWei = "1200000"
		stGasSealFixturePolicy(tb, p, a, key)
		fixture.fault = ""
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			tb.Fatal("lifetime increase inflated original per-intent allocation", err)
		}
		// The refused successor takes no nonce: the reverted generation stays the latest.
		latest := model.GetStTransactionIntent(context.Background(), old.LogicalKey)
		if latest.Generation != 0 || latest.Status != model.StTxReverted || len(fixture.sent) != 1 || fixture.deposited.Sign() != 0 {
			tb.Fatal("revert successor escaped original cumulative allocation")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

// SV-1 (retries; the deposit key): a pending execution reservation never
// becomes a cancellation. Once the deposit's deadline has finalized,
// reconciliation asks for the cancellation that retires an expired deposit,
// but the reservation is signed as the execution it holds: the signer is asked
// once more, for the reserved bytes, and the intent's one attempt is that
// execution. The gas ledger holds what it held, and the deposit reverts
// without moving principal.
func TestStOperatorGasPendingExecutionCannotBecomeCancellation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, _, _ := stGasFixturePolicy(tb, client, nil)
		lost := errors.New("synthetic pending signer result")
		signs := 0
		client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
			signs++
			return nil, lost
		}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, lost) {
			tb.Fatal(err)
		}
		intent := model.GetStTransactionIntent(context.Background(), stGasCurrentLogical(tb, client))
		pending, err := model.GetPendingStTransactionGasReservation(context.Background(), intent.IntentId)
		if err != nil || pending == nil || pending.Kind != model.StTxAttemptExecution {
			tb.Fatal("fixture did not retain the pending execution", err)
		}
		stDepositCustodyExpire(fixture)
		client.transactionSigner = func(_ context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			signs++
			if strings.ToLower(signer.Hash(tx).Hex()) != pending.SigningHash {
				tb.Error("signer was asked for other bytes than the pending execution's")
			}
			return types.SignTx(tx, signer, key)
		}
		_, err = client.DepositCredit(context.Background(), 7, 1, big.NewInt(100))
		attempts := model.GetStTransactionAttempts(context.Background(), intent.IntentId)
		if len(attempts) != 1 || err == nil || !strings.Contains(err.Error(), "st: finalized transaction "+attempts[0].TxHash+" reverted") {
			tb.Fatal("expired pending execution was not signed as reserved", err)
		}
		sent, _, _, deposited, _ := stGasFixtureChain(fixture)
		if signs != 2 || attempts[0].Kind != model.StTxAttemptExecution || len(sent) != 1 || deposited != 0 {
			tb.Fatal("cancellation request rewrote pending original signature")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

// Reserves the execution of epoch 7's deposit at nonce 7 and stops before its
// signature, as a process stopped there leaves it.
func stGasReserveUnsignedDeposit(tb testing.TB, client *CoreStClient) (*model.StTransactionIntent, *model.StTransactionGasReservation) {
	tb.Helper()
	ctx := context.Background()
	stopped := errors.New("process stopped between the reservation and the signature")
	client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
		return nil, stopped
	}
	if _, err := client.DepositCredit(ctx, 7, client.cfg.NoId, big.NewInt(100)); !errors.Is(err, stopped) {
		tb.Fatalf("deposit: %v", err)
	}
	client.transactionSigner = nil
	intent := model.GetStTransactionIntent(ctx, stGasCurrentLogical(tb, client))
	pending, err := model.GetPendingStTransactionGasReservation(ctx, intent.IntentId)
	if intent.Nonce != 7 || intent.AttemptCount != 0 || pending == nil || pending.Kind != model.StTxAttemptExecution || err != nil {
		tb.Fatalf("deposit intent %+v, reservation %+v, %v", intent, pending, err)
	}
	return intent, pending
}

// Reads, under the fixture's lock, the transactions it was sent, its staged
// and source stake, the epoch's deposited principal and the coordinator's
// deposit nonce.
func stGasFixtureChain(fixture *stDepositCustodyRpc) (sent []*types.Transaction, staged, source, deposited, depositNonce int64) {
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	return append([]*types.Transaction(nil), fixture.sent...), fixture.staged.Int64(), fixture.source.Int64(), fixture.deposited.Int64(), fixture.depositNonce.Int64()
}

// Routes client through an endpoint at which the send run under stopCtx stops
// after its broadcast, as a process stopped there leaves it: its first receipt
// read after a broadcast is answered with no receipt and cancels stopCtx.
// resume ends the stop, and later sends see every receipt.
func stGasStopAfterBroadcast(tb testing.TB, client *CoreStClient, fixture *stDepositCustodyRpc) (stopCtx context.Context, resume func()) {
	tb.Helper()
	stopCtx, stop := context.WithCancel(context.Background())
	tb.Cleanup(stop)
	var stopping, broadcast atomic.Bool
	stopping.Store(true)
	url, endpoint := stWalletRecoveryEndpoint(tb, fixture, func(writer http.ResponseWriter, call stEpochRPCRequest) bool {
		switch {
		case !stopping.Load():
		case call.Method == "eth_sendRawTransaction":
			broadcast.Store(true)
		case call.Method == "eth_getTransactionReceipt" && broadcast.Load():
			stop()
			stWalletRecoveryReply(writer, call, nil, false)
			return true
		}
		return false
	})
	client.cfg.RpcUrls = []string{url}
	client.clients = map[string]*ethclient.Client{url: endpoint}
	return stopCtx, func() { stopping.Store(false) }
}

// SV-1 (retries; the deposit key): a deposit takes its nonce together with
// its first attempt's reservation, and the process may stop before the
// signature. Here epoch 7's deposit, whose deadline is block 1999, is reserved
// at nonce 7 and not signed, and block 2000, epoch 8's first, is finalized.
// The deposit is expired, and the reservation refuses the cancellation that
// retires an expired deposit. The deposit key's next send, epoch 8's deposit,
// signs the reserved deposit instead, once and exactly as reserved, and sends
// it. The coordinator reverts a deposit past its deadline before any stake
// moves, rolling back its reentrancy guard's write: no principal moves and the
// coordinator's deposit nonce stays 3, though the inclusion spends gas. That
// send reports the revert, the signature adds nothing to the gas ledger, and
// the nonce is free: the send after it deposits epoch 8's principal at nonce 8.
func TestStOperatorGasPendingExpiredDepositIsSignedOnceAndFreesTheDepositKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, _, _ := stGasFixturePolicy(tb, client, nil)
		intent, pending := stGasReserveUnsignedDeposit(tb, client)
		stDepositCustodyExpire(fixture)
		var signed []*types.Transaction
		client.transactionSigner = func(_ context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			result, err := types.SignTx(tx, signer, key)
			signed = append(signed, result)
			return result, err
		}

		_, err := client.DepositCredit(ctx, 8, 1, big.NewInt(100))
		sent, staged, source, deposited, depositNonce := stGasFixtureChain(fixture)
		if len(signed) != 1 || len(sent) != 1 || err == nil || !strings.Contains(err.Error(), "st: finalized transaction "+strings.ToLower(sent[0].Hash().Hex())+" reverted") {
			tb.Fatalf("send behind the expired deposit: %v, %d signed, %d sent", err, len(signed), len(sent))
		}
		signer := types.LatestSignerForChainID(new(big.Int).SetUint64(client.cfg.ChainId))
		if deposit := sent[0]; deposit.Hash() != signed[0].Hash() || deposit.Nonce() != 7 || !bytes.Equal(deposit.Data(), intent.Calldata) || strings.ToLower(signer.Hash(deposit).Hex()) != pending.SigningHash {
			tb.Fatalf("the deposit went out as nonce %d data %x, not as reserved", deposit.Nonce(), deposit.Data())
		}
		if staged != 102 || source != 10_000 || deposited != 0 || depositNonce != 3 {
			tb.Fatalf("the reverted deposit moved stake: %d staged, %d source, %d deposited, deposit nonce %d", staged, source, deposited, depositNonce)
		}
		requireLedgerV2Intent(tb, ctx, client.cfg, "deposit:7:1:3", model.StTxReverted, sent[0])
		if attempts := model.GetStTransactionAttempts(ctx, intent.IntentId); len(attempts) != 1 || attempts[0].Kind != model.StTxAttemptExecution {
			tb.Fatalf("expired deposit attempts %+v", attempts)
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)

		hash, err := client.DepositCredit(ctx, 8, 1, big.NewInt(100))
		sent, staged, _, deposited, depositNonce = stGasFixtureChain(fixture)
		if err != nil || len(signed) != 2 || signed[1].Nonce() != 8 || len(sent) != 2 || sent[1].Hash() != signed[1].Hash() || !strings.EqualFold(hash, sent[1].Hash().Hex()) || staged != 0 || deposited != 100 || depositNonce != 4 {
			tb.Fatalf("deposit on the freed key: %s, %v, %d signed, %d sent", hash, err, len(signed), len(sent))
		}
		requireLedgerV2Intent(tb, ctx, client.cfg, "deposit:8:1:3", model.StTxFinalized, sent[1])
		requireLedgerV2Intent(tb, ctx, client.cfg, "deposit:7:1:3", model.StTxReverted, sent[0])
		// Each deposit's 60,000 gas at 10 wei.
		stGasFixtureSnapshot(tb, p, "1200000", 2)
	})
}

// SV-1 (retries; the deposit key): until its deadline has passed at the
// finalized head, a deposit reserved and not signed keeps today's custody
// validation. Here epoch 7's deposit is reserved at nonce 7, and block 1998,
// the one before its deadline, is finalized. Its staging no longer holds
// principal plus the rounding allowance, so custody refuses the reserved
// execution: nothing is signed or sent, and the reservation and the gas ledger
// are as they were. Once the staging holds both again, the next send signs it
// exactly as reserved, and it deposits its principal at block 1999.
func TestStOperatorGasPendingDepositBeforeItsDeadlineKeepsCustodyValidation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, _, _ := stGasFixturePolicy(tb, client, nil)
		intent, pending := stGasReserveUnsignedDeposit(tb, client)
		fixture.stateLock.Lock()
		fixture.boundary.Block, fixture.boundary.Hash = 1_998, [32]byte(stTransactionReconcileBlockHash(1_998))
		fixture.staged.SetInt64(101)
		fixture.stateLock.Unlock()
		signs := 0
		client.transactionSigner = func(_ context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			signs++
			return types.SignTx(tx, signer, key)
		}
		if _, err := client.DepositCredit(ctx, 7, 1, big.NewInt(100)); err == nil || err.Error() != "st: deposit staging lacks principal plus exact reserve rounding allowance" {
			tb.Fatalf("reserved deposit with short staging: %v", err)
		}
		retained, err := model.GetPendingStTransactionGasReservation(ctx, intent.IntentId)
		sent, _, _, _, _ := stGasFixtureChain(fixture)
		if err != nil || retained == nil || retained.SigningHash != pending.SigningHash || signs != 0 || len(sent) != 0 {
			tb.Fatalf("custody refusal signed or dropped the reservation: %+v, %v, %d signed, %d sent", retained, err, signs, len(sent))
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)

		fixture.stateLock.Lock()
		fixture.staged.SetInt64(102)
		fixture.stateLock.Unlock()
		if _, err := client.DepositCredit(ctx, 7, 1, big.NewInt(100)); err != nil {
			tb.Fatalf("reserved deposit once staged: %v", err)
		}
		sent, staged, _, deposited, depositNonce := stGasFixtureChain(fixture)
		signer := types.LatestSignerForChainID(new(big.Int).SetUint64(client.cfg.ChainId))
		if signs != 1 || len(sent) != 1 || sent[0].Nonce() != 7 || strings.ToLower(signer.Hash(sent[0]).Hex()) != pending.SigningHash || staged != 0 || deposited != 100 || depositNonce != 4 {
			tb.Fatalf("reserved deposit once staged: %d signed, %d sent, %d staged, %d deposited, deposit nonce %d", signs, len(sent), staged, deposited, depositNonce)
		}
		requireLedgerV2Intent(tb, ctx, client.cfg, "deposit:7:1:3", model.StTxFinalized, sent[0])
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

// SV-1 (retries; the deposit key): the revert of an expired deposit's
// reserved execution is reported once across restarts. The process signs and
// broadcasts the reserved deposit, which is included, and stops before it
// reads the receipt: the deposit reverts while it is down. Of the restarted
// process's sends, exactly one reports the revert, and the others deposit
// epoch 8's principal once, at nonce 8.
func TestStOperatorGasRevertOfAPendingExpiredDepositIsReportedOnceAcrossRestarts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		stGasFixturePolicy(tb, client, nil)
		stGasReserveUnsignedDeposit(tb, client)
		stDepositCustodyExpire(fixture)
		stopCtx, resume := stGasStopAfterBroadcast(tb, client, fixture)
		_, err := client.DepositCredit(stopCtx, 8, 1, big.NewInt(100))
		sent, _, _, _, _ := stGasFixtureChain(fixture)
		if !errors.Is(err, context.Canceled) || len(sent) != 1 || sent[0].Nonce() != 7 {
			tb.Fatalf("the reserved deposit before the stop: %v, %d sent", err, len(sent))
		}
		deposit := sent[0]
		resume()

		reports, credited := 0, 0
		restarted := client
		for range 4 {
			restarted = stGasFixtureRestart(restarted)
			if _, err := restarted.DepositCredit(ctx, 8, 1, big.NewInt(100)); err == nil {
				credited++
			} else if strings.Contains(err.Error(), "st: finalized transaction "+strings.ToLower(deposit.Hash().Hex())+" reverted") {
				reports++
			} else {
				tb.Fatalf("deposit after a restart: %v", err)
			}
		}
		sent, _, _, deposited, _ := stGasFixtureChain(fixture)
		if reports != 1 || credited != 3 || len(sent) != 2 || sent[1].Nonce() != 8 || deposited != 100 {
			tb.Fatalf("%d reports of the revert, %d deposits, %d sent, %d deposited", reports, credited, len(sent), deposited)
		}
		requireLedgerV2Intent(tb, ctx, client.cfg, "deposit:7:1:3", model.StTxReverted, deposit)
		requireLedgerV2Intent(tb, ctx, client.cfg, "deposit:8:1:3", model.StTxFinalized, sent[1])
	})
}

// SV-1 (retries; the deposit key): signing an expired deposit's reserved
// execution respects the cap of three attempts. Epoch 7's deposit is signed
// at nonce 7 twice, with its 3,400,000-gas floor at 10 and 12 wei, neither
// sent, and its third attempt, at 14 wei, is reserved and not signed. Once its
// deadline has finalized, the deposit key's next send signs that third attempt
// exactly as reserved, which adds no reservation to the gas ledger; the node
// refuses its broadcast as underfunded, and the process stops. A cancellation
// would be a fourth signature, which the cap refuses, so the send after it
// signs nothing and sends the third attempt's stored bytes again: the deposit
// reverts without moving principal, that send reports the revert, and the
// send after it deposits epoch 8's principal at nonce 8.
func TestStOperatorGasPendingExpiredDepositAtTheAttemptCapSignsNothingMore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		client, rpcClient, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, _, _ := stGasFixturePolicy(tb, client, func(policy *server.StOperatorGasPolicy) {
			policy.MaximumGas, policy.MaximumIntentLiabilityWei, policy.MaximumLifetimeLiabilityWei = 3_500_000, "100000000", "1000000000"
		})
		intent, _ := stGasReserveUnsignedDeposit(tb, client)
		var previous *model.StTransactionAttempt
		for attempt := 1; attempt < stTxMaxAttempts; attempt++ {
			signed, err := client.buildTransactionAttempt(ctx, rpcClient, client.cfg.DepositKey, intent, previous, model.StTxAttemptExecution)
			if err != nil || signed.Attempt != attempt {
				tb.Fatalf("attempt %d: %+v, %v", attempt, signed, err)
			}
			intent.AttemptCount, previous = attempt, signed
		}
		stopped := errors.New("process stopped between the reservation and the signature")
		client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
			return nil, stopped
		}
		if _, err := client.buildTransactionAttempt(ctx, rpcClient, client.cfg.DepositKey, intent, previous, model.StTxAttemptExecution); !errors.Is(err, stopped) {
			tb.Fatalf("third attempt: %v", err)
		}
		client.transactionSigner = nil
		pending, err := model.GetPendingStTransactionGasReservation(ctx, intent.IntentId)
		if err != nil || pending == nil || pending.Attempt != stTxMaxAttempts || pending.Kind != model.StTxAttemptExecution {
			tb.Fatalf("third attempt reservation %+v, %v", pending, err)
		}
		// The nonce's charge is its largest attempt's, 3,400,000 gas at 14 wei.
		stGasFixtureSnapshot(tb, p, "47600000", 3)

		stDepositCustodyExpire(fixture)
		fixture.stateLock.Lock()
		fixture.balance = new(big.Int)
		fixture.stateLock.Unlock()
		stopCtx, resume := stGasStopAfterBroadcast(tb, client, fixture)
		_, err = client.DepositCredit(stopCtx, 8, 1, big.NewInt(100))
		attempts := model.GetStTransactionAttempts(ctx, intent.IntentId)
		sent, _, _, _, _ := stGasFixtureChain(fixture)
		if !errors.Is(err, context.Canceled) || len(attempts) != stTxMaxAttempts || attempts[0].Attempt != stTxMaxAttempts || attempts[0].Kind != model.StTxAttemptExecution || len(sent) != 0 {
			tb.Fatalf("the reserved third attempt: %v, %d attempts, %d sent", err, len(attempts), len(sent))
		}
		stGasFixtureSnapshot(tb, p, "47600000", 3)

		resume()
		fixture.stateLock.Lock()
		fixture.balance = nil
		fixture.stateLock.Unlock()
		_, err = client.DepositCredit(ctx, 8, 1, big.NewInt(100))
		sent, staged, source, deposited, depositNonce := stGasFixtureChain(fixture)
		if !errors.Is(err, errStTransactionAttemptLimit) || len(sent) != 1 || !strings.Contains(err.Error(), "st: finalized transaction "+attempts[0].TxHash+" reverted") {
			tb.Fatalf("send behind the third attempt: %v, %d sent", err, len(sent))
		}
		signer := types.LatestSignerForChainID(new(big.Int).SetUint64(client.cfg.ChainId))
		if third := sent[0]; !strings.EqualFold(third.Hash().Hex(), attempts[0].TxHash) || third.Nonce() != 7 || third.Gas() != stV1DepositGasFloor || third.GasPrice().Int64() != 14 || strings.ToLower(signer.Hash(third).Hex()) != pending.SigningHash {
			tb.Fatalf("sent nonce %d gas %d price %s, not the reserved third attempt", third.Nonce(), third.Gas(), third.GasPrice())
		}
		if after := model.GetStTransactionAttempts(ctx, intent.IntentId); len(after) != stTxMaxAttempts || staged != 102 || source != 10_000 || deposited != 0 || depositNonce != 3 {
			tb.Fatalf("%d attempts; %d staged, %d source, %d deposited, deposit nonce %d", len(after), staged, source, deposited, depositNonce)
		}
		requireLedgerV2Intent(tb, ctx, client.cfg, "deposit:7:1:3", model.StTxReverted, sent[0])
		stGasFixtureSnapshot(tb, p, "47600000", 3)

		hash, err := client.DepositCredit(ctx, 8, 1, big.NewInt(100))
		sent, _, _, deposited, _ = stGasFixtureChain(fixture)
		if err != nil || len(sent) != 2 || sent[1].Nonce() != 8 || !strings.EqualFold(hash, sent[1].Hash().Hex()) || deposited != 100 {
			tb.Fatalf("deposit on the freed key: %s, %v, %d sent", hash, err, len(sent))
		}
		// Epoch 8's deposit adds its 3,400,000-gas floor at 10 wei.
		stGasFixtureSnapshot(tb, p, "81600000", 4)
	})
}

func TestStOperatorGasMainnetAndUnknownProfilesRequirePolicy(t *testing.T) {
	for _, profile := range []string{"mainnet", "", "unknown"} {
		client := &CoreStClient{cfg: &StConfig{Profile: profile, Enabled: true, ChainId: 964}}
		if _, _, err := client.operatorGasAdmission(context.Background()); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			t.Fatalf("profile %q inferred unbounded legacy allowance: %v", profile, err)
		}
	}
}

func TestStOperatorGasLostBroadcastReplyNeverReleasesOrReservesTwice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		fixture.lostReply = true
		p, _, _ := stGasFixturePolicy(tb, client, nil)
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		if _, err := stGasFixtureRestart(client).DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatal(err)
		}
		if len(fixture.sent) != 1 || fixture.deposited.Int64() != 100 {
			tb.Fatal("lost reply repeated principal")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

func TestStOperatorGasCallerCancellationKeepsReturnedSignature(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, _, _ := stGasFixturePolicy(tb, client, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var original []byte
		client.transactionSigner = func(_ context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			signed, err := types.SignTx(tx, signer, key)
			if err == nil {
				original, _ = signed.MarshalBinary()
			}
			cancel()
			return signed, err
		}
		if _, err := client.DepositCredit(ctx, 7, 1, big.NewInt(100)); !errors.Is(err, context.Canceled) {
			tb.Fatal("returned signature ignored caller cancellation", err)
		}
		intent := model.GetStTransactionIntent(context.Background(), stGasCurrentLogical(tb, client))
		attempt := model.GetCurrentStTransactionAttempt(context.Background(), intent.IntentId)
		if attempt == nil || intent.AttemptCount != 1 || !bytes.Equal(attempt.RawTransaction, original) || len(fixture.sent) != 0 {
			tb.Fatal("caller cancellation lost an already-produced signature or authorized broadcast")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
		stWalletRecoveryInclude(fixture, attempt)
		if _, err := stGasFixtureRestart(client).DepositCredit(context.Background(), 7, 1, big.NewInt(100)); err != nil {
			tb.Fatal("restart could not reconcile canceled caller's original signature", err)
		}
		if len(fixture.sent) != 0 {
			tb.Fatal("original outcome reconciliation rebroadcast canceled caller's signature")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

func TestStOperatorGasTighterPolicyCannotResignPendingEnvelope(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, _, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, a, approval := stGasFixturePolicy(tb, client, nil)
		lost := errors.New("synthetic held signer outcome")
		client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
			return nil, lost
		}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, lost) {
			tb.Fatal(err)
		}
		intent := model.GetStTransactionIntent(context.Background(), stGasCurrentLogical(tb, client))
		original, err := model.GetPendingStTransactionGasReservation(context.Background(), intent.IntentId)
		if err != nil || original == nil {
			tb.Fatal("fixture did not retain original envelope", err)
		}
		p.PreviousPolicySha256 = a.PolicySha256
		p.Revision++
		p.MaximumFeePerGasWei = "9"
		p.MaximumTipPerGasWei = "9"
		stGasSealFixturePolicy(tb, p, a, approval)
		signs := 0
		restarted := stGasFixtureRestart(client)
		restarted.transactionSigner = func(_ context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			signs++
			return types.SignTx(tx, signer, key)
		}
		if _, err := restarted.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			tb.Fatal("new policy admitted pending bytes above its current fee cap", err)
		}
		pending, err := model.GetPendingStTransactionGasReservation(context.Background(), intent.IntentId)
		if err != nil || pending == nil || signs != 0 || len(fixture.sent) != 0 || !bytes.Equal(pending.UnsignedTransaction, original.UnsignedTransaction) {
			tb.Fatal("policy renewal erased or signed an unapproved pending envelope", err)
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

func TestStOperatorGasPendingKindIsCheckedAtActualSigningBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		client, rpcClient, fixture := newStDepositCustodyFixture(tb, 1)
		fixture.staged.SetInt64(102)
		p, _, _ := stGasFixturePolicy(tb, client, nil)
		lost := errors.New("synthetic held execution signer outcome")
		client.transactionSigner = func(context.Context, *types.Transaction, types.Signer, *ecdsa.PrivateKey) (*types.Transaction, error) {
			return nil, lost
		}
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, lost) {
			tb.Fatal(err)
		}
		intent := model.GetStTransactionIntent(context.Background(), stGasCurrentLogical(tb, client))
		signs := 0
		client.transactionSigner = func(_ context.Context, tx *types.Transaction, signer types.Signer, key *ecdsa.PrivateKey) (*types.Transaction, error) {
			signs++
			return types.SignTx(tx, signer, key)
		}
		if _, err := client.buildTransactionAttempt(context.Background(), rpcClient, client.cfg.DepositKey, intent, nil, model.StTxAttemptCancellation); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			tb.Fatal("cancellation request signed the still-valid pending execution", err)
		}
		if signs != 0 || len(fixture.sent) != 0 || len(model.GetStTransactionAttempts(context.Background(), intent.IntentId)) != 0 {
			tb.Fatal("pending kind changed at the real signer boundary")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}
