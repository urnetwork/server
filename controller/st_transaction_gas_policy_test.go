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
		latest := model.GetStTransactionIntent(context.Background(), old.LogicalKey)
		if latest.Generation != 1 || latest.AttemptCount != 0 || len(fixture.sent) != 1 || fixture.deposited.Sign() != 0 {
			tb.Fatal("revert successor escaped original cumulative allocation")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
	})
}

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
		fixture.boundary.Block = 2000
		fixture.boundary.Hash = [32]byte(stTransactionReconcileBlockHash(2000))
		if _, err := client.DepositCredit(context.Background(), 7, 1, big.NewInt(100)); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			tb.Fatal("expired unknown execution became cancellation", err)
		}
		if signs != 1 || len(fixture.sent) != 0 {
			tb.Fatal("cancellation request rewrote pending original signature")
		}
		stGasFixtureSnapshot(tb, p, "600000", 1)
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
