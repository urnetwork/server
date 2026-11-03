package controller

// sn_claim_relay_db_test.go — the claim relay on the real transaction engine,
// tables and Redis, against an RPC fixture. Run them with the private test
// database: TestDbClaimRelay.

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const claimRelayDbKey = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

// A relay's chain over an RPC fixture, as a restart builds it: nothing kept.
func newClaimRelayDbChain(t testing.TB, fixture *ledgerV2RPC, batcher common.Address) (*snClaimRelayCoreChain, *StConfig, func() *snClaimRelayCoreChain) {
	t.Helper()
	key, err := crypto.HexToECDSA(claimRelayDbKey)
	if err != nil {
		t.Fatal(err)
	}
	rootKey, err := crypto.HexToECDSA(strings.Repeat("3", 64))
	if err != nil {
		t.Fatal(err)
	}
	cfg := ledgerV2TestConfig()
	cfg.RootKey = rootKey
	listener := httptest.NewServer(fixture)
	t.Cleanup(listener.Close)
	cfg.RpcUrls = []string{listener.URL}
	relayer := &snClaimRelayKey{key: key, address: crypto.PubkeyToAddress(key.PublicKey), batcher: batcher}
	restart := func() *snClaimRelayCoreChain {
		snClaimRelayCoreChainLock.Lock()
		snClaimRelayCoreChains = map[string]*snClaimRelayCoreChain{}
		snClaimRelayCoreChainLock.Unlock()
		return newSnClaimRelayCoreChain(cfg, relayer)
	}
	return restart(), cfg, restart
}

func claimRelayDbClaim(index uint64, cumulative uint64) *snClaimRelayClaim {
	claim := claimRelayReceiptClaim(index, cumulative)
	return claim
}

func claimRelayDbSingle(chain *snClaimRelayCoreChain, claim *snClaimRelayClaim, gasLimit uint64) *snClaimRelaySend {
	to, calldata := chain.Claim(claim)
	return &snClaimRelaySend{To: to, Calldata: calldata, GasLimit: gasLimit, Claims: []*snClaimRelayClaim{claim}}
}

// What vault2 logs for a paid claim of this fixture's vault.
func claimRelayDbClaimedLog(t testing.TB, cfg *StConfig, claim *snClaimRelayClaim, caller common.Address) *types.Log {
	t.Helper()
	parsed, err := stabi.STSettlementVault2MetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	account := claim.Account
	event := parsed.Events["Claimed"]
	data, err := event.Inputs.NonIndexed().Pack(stBig(account.RootIndex), account.Cumulative.ToBig(), account.Cumulative.ToBig(), caller)
	if err != nil {
		t.Fatal(err)
	}
	return &types.Log{Address: cfg.VaultV2, Topics: []common.Hash{event.ID, ledgerV2Word(account.NoId), ledgerV2Word(account.Index), common.Hash(account.Coldkey)}, Data: data}
}

// The relay's account sends outside the operator gas policy, on its own client
// only, at the gas limit it planned and with its own close fee cap; what it spent
// is read back from the receipt, and the claim names the leaf's coldkey.
func TestDbClaimRelaySendsOutsideTheGasPolicy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		fixture.baseFee = claimRelayGwei(5)
		chain, cfg, _ := newClaimRelayDbChain(t, fixture, common.Address{})
		// a policy that admits nothing
		cfg.OperatorGasPolicy = &server.StOperatorGasPolicy{}
		claim := claimRelayDbClaim(5, 1_000)
		fixture.receiptHook = func(tx *types.Transaction, receipt *types.Receipt) {
			receipt.GasUsed = 155_000
			receipt.EffectiveGasPrice = claimRelayGwei(5)
			receipt.Logs = []*types.Log{claimRelayDbClaimedLog(t, cfg, claim, chain.relayer.address)}
		}

		// the operator's client refuses, as it should
		operator := &CoreStClient{cfg: cfg, clients: map[string]*ethclient.Client{}}
		if _, err := operator.sendLedgerV2(ctx, "v2relaytest:1", cfg.VaultV2, []byte{1, 2, 3, 4}); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			t.Fatalf("the operator's client under a bad policy: %v", err)
		}
		if len(fixture.sent) != 0 {
			t.Fatalf("sent %d", len(fixture.sent))
		}
		// and the relay's own client does not lend its exemption
		if _, _, err := chain.client.operatorGasAdmission(ctx, crypto.PubkeyToAddress(cfg.RootKey.PublicKey)); !errors.Is(err, model.ErrStOperatorGasAllowance) {
			t.Fatalf("the relay's client admitted the root key: %v", err)
		}

		send := claimRelayDbSingle(chain, claim, 198_000)
		outcome, err := chain.Send(ctx, send)
		if err != nil {
			t.Fatal(err)
		}
		if len(fixture.sent) != 1 {
			t.Fatalf("sent %d", len(fixture.sent))
		}
		tx := fixture.sent[0]
		if *tx.To() != cfg.VaultV2 || tx.Gas() != 198_000 {
			t.Fatalf("to %s gas %d", tx.To(), tx.Gas())
		}
		if tx.GasFeeCap().Cmp(big.NewInt(5_625_000_000)) != 0 || tx.GasTipCap().Sign() != 0 {
			t.Fatalf("fee cap %s tip %s", tx.GasFeeCap(), tx.GasTipCap())
		}
		from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
		if err != nil || from != chain.relayer.address {
			t.Fatalf("signed by %s (%v)", from, err)
		}
		claims, batch, err := snClaimRelayDecodeCalldata(tx.Data())
		if err != nil || batch || claims[0].Account.Coldkey != claim.Account.Coldkey {
			t.Fatalf("claims %+v %t %v", claims, batch, err)
		}
		if outcome.Griefed || outcome.Reverted || len(outcome.Items) != 1 || !outcome.Items[0].Paid || outcome.Items[0].AmountRao.Int64() != 1_000 {
			t.Fatalf("outcome %+v %+v", outcome, outcome.Items[0])
		}

		// the same claim again finds the same intent and sends nothing
		if outcome, err = chain.Send(ctx, send); err != nil || !outcome.Items[0].Paid || len(fixture.sent) != 1 {
			t.Fatalf("again: %+v, %v, sent %d", outcome, err, len(fixture.sent))
		}

		spend, err := chain.Spend(ctx, server.NowUtc().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if spend.SpentWei.Cmp(new(big.Int).Mul(big.NewInt(155_000), claimRelayGwei(5))) != 0 || spend.ClaimsPaid != 1 || spend.Griefed || len(spend.RevertedAccounts) != 0 {
			t.Fatalf("spend %+v", spend)
		}
		// a day that had not begun yet has spent nothing
		spend, err = chain.Spend(ctx, server.NowUtc().Add(time.Hour))
		if err != nil || spend.SpentWei.Sign() != 0 || spend.ClaimsPaid != 0 {
			t.Fatalf("a later day's spend %+v (%v)", spend, err)
		}
	})
}

// A claim whose outcome was unknown when the process stopped is finished by the
// next one, with the exact same bytes and nonce, and it costs the most it can
// until its receipt reads. Nothing is claimed twice.
func TestDbClaimRelayRestartResumesTheSameBytes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		fixture.baseFee = claimRelayGwei(5)
		fixture.withheld = 1
		chain, cfg, restart := newClaimRelayDbChain(t, fixture, common.Address{})
		claim := claimRelayDbClaim(5, 1_000)
		fixture.receiptHook = func(tx *types.Transaction, receipt *types.Receipt) {
			receipt.GasUsed = 155_000
			receipt.EffectiveGasPrice = claimRelayGwei(5)
			receipt.Logs = []*types.Log{claimRelayDbClaimedLog(t, cfg, claim, chain.relayer.address)}
		}
		send := claimRelayDbSingle(chain, claim, 198_000)
		stopping, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		if _, err := chain.Send(stopping, send); !errors.Is(err, errSnClaimRelayInFlight) {
			t.Fatalf("a stopped pass: %v", err)
		}
		if len(fixture.sent) != 1 {
			t.Fatalf("sent %d", len(fixture.sent))
		}
		first := fixture.sent[0]

		// meanwhile it counts at its limit and the next nonce is held
		chain = restart()
		spend, err := chain.Spend(ctx, server.NowUtc().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if most := new(big.Int).Mul(big.NewInt(198_000), big.NewInt(5_625_000_000)); spend.SpentWei.Cmp(most) != 0 || spend.ClaimsPaid != 0 {
			t.Fatalf("in flight spend %+v, want %s", spend, most)
		}

		if err := chain.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if len(fixture.sent) != 2 || fixture.sent[1].Hash() != first.Hash() || fixture.sent[1].Nonce() != first.Nonce() {
			t.Fatalf("sent %d, the second is another transaction", len(fixture.sent))
		}
		spend, err = chain.Spend(ctx, server.NowUtc().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if spend.SpentWei.Cmp(new(big.Int).Mul(big.NewInt(155_000), claimRelayGwei(5))) != 0 || spend.ClaimsPaid != 1 {
			t.Fatalf("spend after the receipt %+v", spend)
		}
		if outcome, err := chain.Send(ctx, send); err != nil || !outcome.Items[0].Paid || len(fixture.sent) != 2 {
			t.Fatalf("again: %+v, %v, sent %d", outcome, err, len(fixture.sent))
		}
	})
}

// An intent reserved and never signed is retired by the next process, not signed
// with the facts it was checked against long gone, and the claim is planned anew
// under its own name.
func TestDbClaimRelayUnsignedIntentIsRetired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		fixture.baseFee = claimRelayGwei(5)
		chain, cfg, restart := newClaimRelayDbChain(t, fixture, common.Address{})
		claim := claimRelayDbClaim(5, 1_000)
		fixture.receiptHook = func(tx *types.Transaction, receipt *types.Receipt) {
			receipt.GasUsed = 21_000
			if 0 < len(tx.Data()) {
				receipt.GasUsed = 155_000
				receipt.Logs = []*types.Log{claimRelayDbClaimedLog(t, cfg, claim, chain.relayer.address)}
			}
			receipt.EffectiveGasPrice = claimRelayGwei(5)
		}
		send := claimRelayDbSingle(chain, claim, 198_000)
		operation := snClaimRelayOperation(send, 0)
		logicalKey, err := stTransactionLogicalKey(cfg, operation)
		if err != nil {
			t.Fatal(err)
		}
		genesisHash := "0x" + hex.EncodeToString(cfg.GenesisHash[:])
		from := strings.ToLower(chain.relayer.address.Hex())
		calldataHash := strings.ToLower(crypto.Keccak256Hash(send.Calldata).Hex())
		model.ReserveStTransactionIntent(ctx, logicalKey, cfg.Profile, cfg.DeploymentId, cfg.DeploymentKey(), cfg.ChainId, genesisHash, from, strings.ToLower(send.To.Hex()), calldataHash, send.Calldata, 7)

		chain = restart()
		if err := chain.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if len(fixture.sent) != 1 {
			t.Fatalf("sent %d", len(fixture.sent))
		}
		retired := fixture.sent[0]
		if *retired.To() != chain.relayer.address || len(retired.Data()) != 0 || retired.Gas() != 21_000 || retired.Nonce() != 7 {
			t.Fatalf("the retirement is to %s with %d bytes at gas %d nonce %d", retired.To(), len(retired.Data()), retired.Gas(), retired.Nonce())
		}
		if intent := model.GetStTransactionIntent(ctx, logicalKey); intent == nil || intent.Status != model.StTxCanceled {
			t.Fatalf("intent %+v", intent)
		}
		// its self-transaction was paid for, and it was not a claim
		spend, err := chain.Spend(ctx, server.NowUtc().Add(-time.Hour))
		if err != nil || spend.SpentWei.Cmp(new(big.Int).Mul(big.NewInt(21_000), claimRelayGwei(5))) != 0 || spend.ClaimsPaid != 0 || spend.Griefed {
			t.Fatalf("spend %+v (%v)", spend, err)
		}

		outcome, err := chain.Send(ctx, send)
		if err != nil {
			t.Fatal(err)
		}
		if !outcome.Items[0].Paid || len(fixture.sent) != 2 || fixture.sent[1].Nonce() != 8 {
			t.Fatalf("outcome %+v, sent %d", outcome, len(fixture.sent))
		}
	})
}

// A signed claim someone else paid first is retired, not sent into a certain
// revert.
func TestDbClaimRelayClaimPaidFirstIsRetired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		fixture.baseFee = claimRelayGwei(5)
		fixture.withheld = 1
		chain, cfg, restart := newClaimRelayDbChain(t, fixture, common.Address{})
		claim := claimRelayDbClaim(5, 1_000)
		send := claimRelayDbSingle(chain, claim, 198_000)
		stopping, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		if _, err := chain.Send(stopping, send); !errors.Is(err, errSnClaimRelayInFlight) {
			t.Fatalf("a stopped pass: %v", err)
		}
		fixture.lock.Lock()
		fixture.reverts[string(send.Calldata)] = "NothingToClaim"
		fixture.lock.Unlock()
		fixture.receiptHook = func(tx *types.Transaction, receipt *types.Receipt) { receipt.GasUsed = 21_000 }

		chain = restart()
		if err := chain.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		last := fixture.sent[len(fixture.sent)-1]
		if *last.To() != chain.relayer.address || len(last.Data()) != 0 {
			t.Fatalf("the last transaction is to %s with %d bytes", last.To(), len(last.Data()))
		}
		logicalKey, err := stTransactionLogicalKey(cfg, snClaimRelayOperation(send, 0))
		if err != nil {
			t.Fatal(err)
		}
		if intent := model.GetStTransactionIntent(ctx, logicalKey); intent == nil || intent.Status != model.StTxCanceled {
			t.Fatalf("intent %+v", intent)
		}
	})
}

// A claim that reverts having burned its gas limit stops the day: the receipt says
// so through the durable intent, whichever process reads it.
func TestDbClaimRelayRevertedClaimStopsTheDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		fixture.baseFee = claimRelayGwei(5)
		chain, _, restart := newClaimRelayDbChain(t, fixture, common.Address{})
		claim := claimRelayDbClaim(5, 1_000)
		send := claimRelayDbSingle(chain, claim, 198_000)
		fixture.lock.Lock()
		fixture.needs[string(send.Calldata)] = 1_260_000
		fixture.lock.Unlock()
		outcome, err := chain.Send(ctx, send)
		if err != nil {
			t.Fatal(err)
		}
		if !outcome.Reverted || !outcome.Griefed || outcome.Items[0].Paid {
			t.Fatalf("outcome %+v", outcome)
		}
		chain = restart()
		spend, err := chain.Spend(ctx, server.NowUtc().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if !spend.Griefed || spend.RevertedAccounts["1/5"] == "" || spend.ClaimsPaid != 0 || spend.SpentWei.Sign() <= 0 {
			t.Fatalf("spend %+v", spend)
		}
	})
}

// A batch goes to the batcher at its sized limit, not an estimate, and its receipt
// says each item's outcome.
func TestDbClaimRelayBatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLedgerV2RPC(t)
		fixture.baseFee = claimRelayGwei(5)
		chain, cfg, _ := newClaimRelayDbChain(t, fixture, claimRelayTestBatcher)
		first, second := claimRelayDbClaim(1, 100), claimRelayDbClaim(2, 200)
		calldata := stClaimRelayTestBatcherAbi.PackClaimMany(cfg.VaultV2, snClaimRelayItems([]*snClaimRelayClaim{first, second}), big.NewInt(250_000))
		batcherLog := func(name string, values ...any) *types.Log {
			parsed, err := stabi.STClaimBatcherMetaData.ParseABI()
			if err != nil {
				t.Fatal(err)
			}
			data, err := parsed.Events[name].Inputs.NonIndexed().Pack(values...)
			if err != nil {
				t.Fatal(err)
			}
			return &types.Log{Address: claimRelayTestBatcher, Topics: []common.Hash{parsed.Events[name].ID}, Data: data}
		}
		fixture.receiptHook = func(tx *types.Transaction, receipt *types.Receipt) {
			receipt.GasUsed = 280_000
			receipt.EffectiveGasPrice = claimRelayGwei(5)
			receipt.Logs = []*types.Log{
				claimRelayDbClaimedLog(t, cfg, first, claimRelayTestBatcher),
				batcherLog("ClaimPaid", big.NewInt(0), big.NewInt(100)),
				batcherLog("ClaimRefused", big.NewInt(1), [4]byte{0xde, 0xad, 0xbe, 0xef}),
			}
		}
		send := &snClaimRelaySend{To: claimRelayTestBatcher, Calldata: calldata, GasLimit: 500_000, Claims: []*snClaimRelayClaim{first, second}, Batch: true}
		outcome, err := chain.Send(ctx, send)
		if err != nil {
			t.Fatal(err)
		}
		if tx := fixture.sent[0]; *tx.To() != claimRelayTestBatcher || tx.Gas() != 500_000 {
			t.Fatalf("to %s gas %d", tx.To(), tx.Gas())
		}
		if !outcome.Items[0].Paid || outcome.Items[1].Paid || outcome.Items[1].NotAttempted || outcome.Griefed {
			t.Fatalf("outcome %+v %+v", outcome.Items[0], outcome.Items[1])
		}
		spend, err := chain.Spend(ctx, server.NowUtc().Add(-time.Hour))
		if err != nil || spend.ClaimsPaid != 1 || spend.RevertedAccounts["1/2"] == "" || spend.RevertedAccounts["1/1"] != "" {
			t.Fatalf("spend %+v (%v)", spend, err)
		}
	})
}

// Claim now in Redis: one per coldkey per period, oldest first, and a refund gives
// the tap back.
func TestDbClaimRelayRequests(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := model.StDeploymentKey("945:" + server.NewId().String())
		first, second := claimRelayTestColdkey(1), claimRelayTestColdkey(2)
		now := server.NowUtc()
		if requested, until := model.GetSnClaimRelayRequest(ctx, key, first, now); requested || !until.IsZero() {
			t.Fatalf("no request yet: %t %s", requested, until)
		}
		added, until := model.AddSnClaimRelayRequest(ctx, key, first, now, snClaimRelayClaimNowPeriod)
		if !added || !until.Equal(now.Add(snClaimRelayClaimNowPeriod)) {
			t.Fatalf("first tap: %t %s", added, until)
		}
		added, until = model.AddSnClaimRelayRequest(ctx, key, first, now.Add(time.Minute), snClaimRelayClaimNowPeriod)
		if added || until.Before(now.Add(snClaimRelayClaimNowPeriod-time.Hour)) || until.After(now.Add(snClaimRelayClaimNowPeriod+time.Hour)) {
			t.Fatalf("second tap: %t %s", added, until)
		}
		if added, _ := model.AddSnClaimRelayRequest(ctx, key, second, now.Add(time.Second), snClaimRelayClaimNowPeriod); !added {
			t.Fatal("another coldkey's tap was refused")
		}
		requests := model.GetSnClaimRelayRequests(ctx, key, 10)
		if len(requests) != 2 || requests[0].Coldkey != first || requests[1].Coldkey != second {
			t.Fatalf("requests %+v", requests)
		}
		if requested, until := model.GetSnClaimRelayRequest(ctx, key, first, now); !requested || until.IsZero() {
			t.Fatalf("after the tap: %t %s", requested, until)
		}
		// a paid request keeps its limit
		model.FinishSnClaimRelayRequest(ctx, key, first, false)
		if requested, until := model.GetSnClaimRelayRequest(ctx, key, first, now); requested || until.IsZero() {
			t.Fatalf("after it was paid: %t %s", requested, until)
		}
		if added, _ := model.AddSnClaimRelayRequest(ctx, key, first, now, snClaimRelayClaimNowPeriod); added {
			t.Fatal("tapped again within the period")
		}
		// a refunded one does not
		model.FinishSnClaimRelayRequest(ctx, key, second, true)
		if requested, until := model.GetSnClaimRelayRequest(ctx, key, second, now); requested || !until.IsZero() {
			t.Fatalf("after the refund: %t %s", requested, until)
		}
		if added, _ := model.AddSnClaimRelayRequest(ctx, key, second, now, snClaimRelayClaimNowPeriod); !added {
			t.Fatal("the refund did not give the tap back")
		}
		// the limit runs out by itself
		short := claimRelayTestColdkey(3)
		if added, _ := model.AddSnClaimRelayRequest(ctx, key, short, now, 50*time.Millisecond); !added {
			t.Fatal("tap")
		}
		for deadline := time.Now().Add(10 * time.Second); ; {
			if added, _ := model.AddSnClaimRelayRequest(ctx, key, short, now, snClaimRelayClaimNowPeriod); added {
				break
			}
			if deadline.Before(time.Now()) {
				t.Fatal("the limit did not run out")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if status := model.GetSnClaimRelayStatus(ctx, key); status != nil {
			t.Fatalf("status %s", status)
		}
		model.SetSnClaimRelayStatus(ctx, key, []byte(`{"state":"running"}`))
		if status := model.GetSnClaimRelayStatus(ctx, key); string(status) != `{"state":"running"}` {
			t.Fatalf("status %s", status)
		}
	})
}

// claimRelayDbWorld is a pool with a finalized root on the real tables, and a chain
// fixture that answers what vault2 holds.
type claimRelayDbWorld struct {
	t       testing.TB
	ctx     context.Context
	fixture *ledgerV2RPC
	cfg     *StConfig
	chain   *snClaimRelayCoreChain
	leaves  []merkle.LedgerAccount
	paid    map[uint64]bool
}

func newClaimRelayDbWorld(t testing.TB, amounts ...*uint256.Int) *claimRelayDbWorld {
	t.Helper()
	ctx := context.Background()
	fixture := newLedgerV2RPC(t)
	fixture.baseFee = claimRelayGwei(5)
	fixture.balance = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	chain, cfg, _ := newClaimRelayDbChain(t, fixture, common.Address{})
	world := &claimRelayDbWorld{t: t, ctx: ctx, fixture: fixture, cfg: cfg, chain: chain, paid: map[uint64]bool{}}

	SetStConfig(cfg)
	t.Cleanup(func() { SetStConfig(nil) })
	t.Cleanup(claimRelayVaultEntry(t, "testnet-private-key: \""+claimRelayDbKey+"\"\n"))
	resetLedgerV2Caches()
	t.Cleanup(resetLedgerV2Caches)

	coldkeys := make([][32]byte, len(amounts))
	world.leaves = make([]merkle.LedgerAccount, len(amounts))
	for index, amount := range amounts {
		coldkeys[index] = claimRelayTestColdkey(index)
		world.leaves[index] = merkle.LedgerAccount{Coldkey: coldkeys[index], Cumulative: *amount}
	}
	tree, err := merkle.BuildLedgerSumTree(ctx, world.leaves)
	if err != nil {
		t.Fatal(err)
	}
	log := []string{}
	builder := newFakeLedgerBuilder(&log)
	ledgerHash := crypto.Keccak256Hash([]byte("claim relay ledger"))
	builder.ledgers[ledgerHash] = world.leaves
	SetStLedgerBuilder(builder)
	t.Cleanup(func() { SetStLedgerBuilder(nil) })

	root, total := tree.Root()
	rootIndex := uint64(0)
	key := cfg.DeploymentKey()
	model.SetStLedgerV2RootFinalized(ctx, key, &model.StLedgerV2Root{NoId: 1, Seq: 1, Epoch: 4, Root: root, Total: total, LeafCount: uint64(len(amounts)), LedgerHash: ledgerHash, RootIndex: &rootIndex}, 90)
	if err := model.RecordStLedgerV2Root(ctx, key, 1, 1, 0, coldkeys, nil); err != nil {
		t.Fatal(err)
	}

	// what the chain answers for the pass: the root, each claimed, the price
	parsed, err := stabi.STSettlementVault2MetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	rootOutput, err := parsed.Methods["roots"].Outputs.Pack(stabi.ISTSettlementVault2Root{Root: root, Total: total.ToBig(), LeafCount: uint64(len(amounts)), Epoch: 4, LedgerHash: [32]byte(ledgerHash)})
	if err != nil {
		t.Fatal(err)
	}
	fixture.results[string(stLedgerV2VaultAbi.PackRoots(stBig(1), stBig(0)))] = rootOutput
	for index := range amounts {
		fixture.results[string(stLedgerV2VaultAbi.PackClaimed(stBig(1), stBig(uint64(index))))] = make([]byte, 32)
	}
	priceCall := make([]byte, 36)
	copy(priceCall[:4], snClaimRelayGetAlphaPriceSelector)
	new(big.Int).SetUint64(cfg.Netuid).FillBytes(priceCall[4:])
	price := make([]byte, 32)
	claimRelayTestAlphaPrice.FillBytes(price)
	fixture.results[string(priceCall)] = price

	fixture.receiptHook = func(tx *types.Transaction, receipt *types.Receipt) {
		claims, _, err := snClaimRelayDecodeCalldata(tx.Data())
		if err != nil {
			t.Errorf("a transaction that is not a claim: %v", err)
			return
		}
		receipt.GasUsed = 155_000
		receipt.EffectiveGasPrice = claimRelayGwei(5)
		for _, item := range claims {
			index := item.Account.Index.Uint64()
			claim := &snClaimRelayClaim{Account: &snClaimRelayAccount{NoId: 1, Index: index, RootIndex: 0, Coldkey: item.Account.Coldkey, Cumulative: world.leaves[index].Cumulative}}
			receipt.Logs = append(receipt.Logs, claimRelayDbClaimedLog(t, cfg, claim, chain.relayer.address))
			word := make([]byte, 32)
			item.Account.Cumulative.FillBytes(word)
			fixture.results[string(stLedgerV2VaultAbi.PackClaimed(stBig(1), stBig(index)))] = word
			world.paid[index] = true
		}
	}
	SetStLedgerV2Client(chain.client)
	t.Cleanup(func() { SetStLedgerV2Client(nil) })
	return world
}

func (self *claimRelayDbWorld) session(coldkeyIndex int) *session.ClientSession {
	self.t.Helper()
	networkId := server.NewId()
	coldkey := claimRelayTestColdkey(coldkeyIndex)
	address, err := ss58.Encode(coldkey, ss58.BittensorPrefix)
	if err != nil {
		self.t.Fatal(err)
	}
	model.SetStWallet(self.ctx, networkId, address, coldkey)
	return session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: networkId, UserId: server.NewId(), NetworkName: "relay"})
}

// The whole relay on its real parts: a pass reads the account index and the tree,
// claims over the threshold, leaves the rest, and what it did reaches the api; and
// Claim now takes the rest, once.
func TestDbClaimRelayWholePass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		world := newClaimRelayDbWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao), claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
		ctx := world.ctx

		result := SnClaimRelayPass(ctx)
		if result.State != SnClaimRelayStateRunning {
			t.Fatalf("pass: %s", result)
		}
		if len(world.fixture.sent) != 1 || !world.paid[0] || world.paid[1] {
			t.Fatalf("sent %d, paid %v", len(world.fixture.sent), world.paid)
		}
		tx := world.fixture.sent[0]
		if *tx.To() != world.cfg.VaultV2 || tx.Gas() != 60_000 {
			t.Fatalf("to %s gas %d", tx.To(), tx.Gas())
		}
		claims, _, err := snClaimRelayDecodeCalldata(tx.Data())
		if err != nil || claims[0].Account.Coldkey != claimRelayTestColdkey(0) {
			t.Fatalf("claim %+v (%v)", claims, err)
		}
		status := snClaimRelayCurrentStatus(ctx, world.cfg, server.NowUtc())
		if status.State != SnClaimRelayStateRunning || status.ClaimsPaidToday != 1 || status.SpentTodayWei != new(big.Int).Mul(big.NewInt(155_000), claimRelayGwei(5)).String() {
			t.Fatalf("status %+v", status)
		}

		// the mirror catches up with the claim; nothing more is due
		model.UpsertStEvents(ctx, world.cfg.DeploymentKey(), []*model.StChainEvent{{BlockNumber: 95, LogIndex: 0, BlockHash: "0x" + strings.Repeat("1", 64), TxHash: tx.Hash().Hex(), Kind: "V2Claimed",
			DataJson: `{"no_id":"1","root_index":"0","index":"0","cumulative":"` + world.leaves[0].Cumulative.Dec() + `"}`}})
		if cumulatives := model.GetStLedgerV2ClaimedCumulatives(ctx, world.cfg.DeploymentKey(), 1); len(cumulatives) != 1 {
			t.Fatalf("mirror %v", cumulatives)
		}
		SnClaimRelayPass(ctx)
		if len(world.fixture.sent) != 1 {
			t.Fatalf("sent %d", len(world.fixture.sent))
		}

		// the api says why the other one waits, and that it can be tapped
		clientSession := world.session(1)
		answer, err := SnClaimRelay(clientSession)
		if err != nil || answer.Error != nil || answer.State != SnClaimRelayStateRunning || len(answer.Accounts) != 1 {
			t.Fatalf("answer %+v (%v)", answer, err)
		}
		account := answer.Accounts[0]
		if account.Reason != SnClaimRelayReasonUnderThreshold || !account.ClaimNowAvailable || account.Index != 1 {
			t.Fatalf("account %+v", account)
		}
		if answer.AutomaticThresholdRao < 2_959_000_000 || 2_960_000_000 < answer.AutomaticThresholdRao || answer.ClaimNowPeriodDays != 30 {
			t.Fatalf("answer %+v", answer)
		}

		// a coldkey of another network is not the caller's to tap
		other, err := ss58.Encode(claimRelayTestColdkey(0), ss58.BittensorPrefix)
		if err != nil {
			t.Fatal(err)
		}
		tap, err := SnClaimRelayClaimNow(&SnClaimRelayClaimNowArgs{ColdkeySs58: other}, clientSession)
		if err != nil || tap.Requested || tap.Reason != SnClaimRelayReasonNothingPending {
			t.Fatalf("another coldkey's tap %+v (%v)", tap, err)
		}

		tap, err = SnClaimRelayClaimNow(&SnClaimRelayClaimNowArgs{ColdkeySs58: account.ColdkeySs58}, clientSession)
		if err != nil || !tap.Requested {
			t.Fatalf("tap %+v (%v)", tap, err)
		}
		// asked for, and it says so
		answer, err = SnClaimRelay(clientSession)
		if err != nil || answer.Accounts[0].ClaimNowAvailable || answer.Accounts[0].ClaimNowReason != SnClaimRelayClaimNowRequested || answer.Accounts[0].Reason != SnClaimRelayReasonQueued {
			t.Fatalf("after the tap %+v (%v)", answer.Accounts[0], err)
		}
		if again, err := SnClaimRelayClaimNow(&SnClaimRelayClaimNowArgs{ColdkeySs58: account.ColdkeySs58}, clientSession); err != nil || again.Requested || again.Reason != SnClaimRelayClaimNowRequested {
			t.Fatalf("second tap %+v (%v)", again, err)
		}

		SnClaimRelayPass(ctx)
		if len(world.fixture.sent) != 2 || !world.paid[1] {
			t.Fatalf("sent %d, paid %v", len(world.fixture.sent), world.paid)
		}
		if requests := model.GetSnClaimRelayRequests(ctx, world.cfg.DeploymentKey(), 10); len(requests) != 0 {
			t.Fatalf("requests %+v", requests)
		}
		// paid, and the limit holds for 30 days
		if requested, until := model.GetSnClaimRelayRequest(ctx, world.cfg.DeploymentKey(), claimRelayTestColdkey(1), server.NowUtc()); requested || until.Before(server.NowUtc().Add(29*24*time.Hour)) {
			t.Fatalf("after the claim: %t %s", requested, until)
		}
		answer, err = SnClaimRelay(clientSession)
		if err != nil || answer.Accounts[0].Reason != SnClaimRelayReasonNothingPending || answer.Accounts[0].ClaimableRao != 0 {
			t.Fatalf("paid %+v (%v)", answer.Accounts[0], err)
		}
		if answer.Accounts[0].ClaimNowNextAtMillis < server.NowUtc().Add(29*24*time.Hour).UnixMilli() {
			t.Fatalf("next at %d", answer.Accounts[0].ClaimNowNextAtMillis)
		}
	})
}

// Without a key the whole pass reads and sends nothing, and the api and the status
// say so.
func TestDbClaimRelayNoKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		world := newClaimRelayDbWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
		ctx := world.ctx
		defer claimRelayVaultEntry(t, "")()
		world.fixture.lock.Lock()
		world.fixture.events = nil
		world.fixture.lock.Unlock()

		result := SnClaimRelayPass(ctx)
		if result.State != SnClaimRelayStateNoKey {
			t.Fatalf("pass: %s", result)
		}
		if len(world.fixture.sent) != 0 || len(world.fixture.takeEvents()) != 0 {
			t.Fatalf("sent %d", len(world.fixture.sent))
		}
		clientSession := world.session(0)
		answer, err := SnClaimRelay(clientSession)
		if err != nil || answer.State != SnClaimRelayStateNoKey || len(answer.Accounts) != 1 {
			t.Fatalf("answer %+v (%v)", answer, err)
		}
		account := answer.Accounts[0]
		if account.Reason != SnClaimRelayReasonRelayNotRunning || account.ClaimNowAvailable || account.ClaimNowReason != SnClaimRelayReasonRelayNotRunning {
			t.Fatalf("account %+v", account)
		}
		tap, err := SnClaimRelayClaimNow(&SnClaimRelayClaimNowArgs{ColdkeySs58: account.ColdkeySs58}, clientSession)
		if err != nil || tap.Requested || tap.Reason != SnClaimRelayReasonRelayNotRunning {
			t.Fatalf("tap %+v (%v)", tap, err)
		}
		if requests := model.GetSnClaimRelayRequests(ctx, world.cfg.DeploymentKey(), 10); len(requests) != 0 {
			t.Fatalf("requests %+v", requests)
		}
	})
}

// A status too old to believe reads as a relay that is not running.
func TestDbClaimRelayStaleStatus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		world := newClaimRelayDbWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
		SnClaimRelayPass(world.ctx)
		now := server.NowUtc()
		if status := snClaimRelayCurrentStatus(world.ctx, world.cfg, now); status.State != SnClaimRelayStateRunning {
			t.Fatalf("fresh: %+v", status)
		}
		if status := snClaimRelayCurrentStatus(world.ctx, world.cfg, now.Add(snClaimRelayStatusStale+time.Minute)); status.State != SnClaimRelayStateNotRunning {
			t.Fatalf("stale: %+v", status)
		}
	})
}

// A network's coldkeys are those of its consents, the replaced ones too: their
// accounts keep their balances, and the relay keeps claiming them.
func TestDbClaimRelayNetworkConsentsNameTheColdkeys(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		ctx := t.Context()
		firstKey, firstAddress := snNetworkWalletTestKey(t, 161)
		secondKey, secondAddress := snNetworkWalletTestKey(t, 162)
		first, _, _ := f.networkConsent(t, f.owner, firstKey, firstAddress, f.scope.Epoch)
		f.accept(t, f.owner, first)
		second, _, _ := f.networkConsent(t, f.owner, secondKey, secondAddress, f.scope.Epoch+1)
		f.accept(t, f.owner, second)

		coldkeys, err := model.GetNetworkWalletMappingConsentColdkeys(ctx, f.rpc.domain, f.owner.NetworkId)
		if err != nil {
			t.Fatal(err)
		}
		if len(coldkeys) != 2 || coldkeys[0] != secondKey.Public().Encode() || coldkeys[1] != firstKey.Public().Encode() {
			t.Fatalf("coldkeys %x", coldkeys)
		}
		if others, err := model.GetNetworkWalletMappingConsentColdkeys(ctx, f.rpc.domain, server.NewId()); err != nil || len(others) != 0 {
			t.Fatalf("another network's coldkeys %x (%v)", others, err)
		}
		clientSession := session.Testing_CreateClientSession(ctx, f.owner)
		listed, err := snClaimRelayNetworkColdkeys(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[[32]byte]int{}
		for _, coldkey := range listed {
			seen[coldkey] += 1
		}
		if seen[firstKey.Public().Encode()] != 1 || seen[secondKey.Public().Encode()] != 1 {
			t.Fatalf("listed %x", listed)
		}
	})
}
