package controller

// sn_claim_relay_db_test.go — the claim relay on the real transaction engine,
// tables and Redis, against an RPC fixture. Run them with the private test
// database: TestDbClaimRelay.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urfoundation/sn/ctf"
	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

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

// Claim now in Redis: a queue, oldest first, where a coldkey keeps its place, and
// a limit that only a paid tap starts and that runs out by itself.
func TestDbClaimRelayRequests(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := model.StDeploymentKey("945:" + server.NewId().String())
		first, second, third := claimRelayTestColdkey(1), claimRelayTestColdkey(2), claimRelayTestColdkey(3)
		now := server.NowUtc()
		if requested, until := model.GetSnClaimRelayRequest(ctx, key, first, now); requested || !until.IsZero() {
			t.Fatalf("no request yet: %t %s", requested, until)
		}
		model.AddSnClaimRelayRequest(ctx, key, first, now)
		model.AddSnClaimRelayRequest(ctx, key, second, now.Add(time.Second))
		// tapped again later, it keeps its place
		model.AddSnClaimRelayRequest(ctx, key, first, now.Add(time.Minute))
		requests := model.GetSnClaimRelayRequests(ctx, key, 10)
		if len(requests) != 2 || requests[0].Coldkey != first || requests[1].Coldkey != second || !requests[0].Time.Equal(time.UnixMilli(now.UnixMilli()).UTC()) {
			t.Fatalf("requests %+v", requests)
		}
		if requested, until := model.GetSnClaimRelayRequest(ctx, key, first, now); !requested || !until.IsZero() {
			t.Fatalf("queued: %t %s", requested, until)
		}
		// paid, the limit starts and the request is done
		model.FinishSnClaimRelayRequest(ctx, key, first, true, now, snClaimRelayClaimNowPeriod)
		requested, until := model.GetSnClaimRelayRequest(ctx, key, first, now)
		if requested || until.Before(now.Add(snClaimRelayClaimNowPeriod-time.Hour)) || until.After(now.Add(snClaimRelayClaimNowPeriod+time.Hour)) {
			t.Fatalf("after it was paid: %t %s", requested, until)
		}
		// a running limit is not pushed back by another: it keeps its start
		model.FinishSnClaimRelayRequest(ctx, key, first, true, now.Add(10*24*time.Hour), snClaimRelayClaimNowPeriod)
		if _, again := model.GetSnClaimRelayRequest(ctx, key, first, now); !again.Equal(until) && again.Sub(until) > time.Minute {
			t.Fatalf("the limit moved from %s to %s", until, again)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			start, err := r.Get(ctx, fmt.Sprintf("sn_claim_relay_limit:%s:%s", key, hex.EncodeToString(first[:]))).Result()
			server.Raise(err)
			if start != fmt.Sprint(now.UnixMilli()) {
				t.Fatalf("the limit's start %s, want %d", start, now.UnixMilli())
			}
		})
		// nothing sent, nothing used up
		model.FinishSnClaimRelayRequest(ctx, key, second, false, now, snClaimRelayClaimNowPeriod)
		if requested, until := model.GetSnClaimRelayRequest(ctx, key, second, now); requested || !until.IsZero() {
			t.Fatalf("after nothing was sent: %t %s", requested, until)
		}
		if requests := model.GetSnClaimRelayRequests(ctx, key, 10); len(requests) != 0 {
			t.Fatalf("requests %+v", requests)
		}
		// the limit runs out by itself
		model.FinishSnClaimRelayRequest(ctx, key, third, true, now, 50*time.Millisecond)
		for deadline := time.Now().Add(10 * time.Second); ; {
			if _, until := model.GetSnClaimRelayRequest(ctx, key, third, server.NowUtc()); until.IsZero() {
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

// A paid claim's record in Redis: one per account of a deployment, the next one in
// place of the one before, kept about the Claim now period, and none for a record
// that does not decode.
func TestDbClaimRelayClaimRecords(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := model.StDeploymentKey("945:" + server.NewId().String())
		other := model.StDeploymentKey("945:" + server.NewId().String())
		if claim := model.GetSnClaimRelayClaim(ctx, key, 1, 7); claim != nil {
			t.Fatalf("no record yet: %+v", claim)
		}
		first := &model.SnClaimRelayClaim{TxHash: "0x" + strings.Repeat("1", 64), Cumulative: "100", AmountRao: "100"}
		model.SetSnClaimRelayClaim(ctx, key, 1, 7, first)
		if claim := model.GetSnClaimRelayClaim(ctx, key, 1, 7); claim == nil || *claim != *first {
			t.Fatalf("record %+v", claim)
		}
		// another account, pool or deployment has its own
		if model.GetSnClaimRelayClaim(ctx, key, 1, 8) != nil || model.GetSnClaimRelayClaim(ctx, key, 2, 7) != nil || model.GetSnClaimRelayClaim(ctx, other, 1, 7) != nil {
			t.Fatal("a record answers for another account")
		}
		second := &model.SnClaimRelayClaim{TxHash: "0x" + strings.Repeat("2", 64), Cumulative: "250", AmountRao: "150"}
		model.SetSnClaimRelayClaim(ctx, key, 1, 7, second)
		if claim := model.GetSnClaimRelayClaim(ctx, key, 1, 7); claim == nil || *claim != *second {
			t.Fatalf("replaced record %+v", claim)
		}
		redisKey := fmt.Sprintf("sn_claim_relay_claim:%s:%d/%d", key, 1, 7)
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.PTTL(ctx, redisKey).Result()
			server.Raise(err)
			if ttl < 29*24*time.Hour || 30*24*time.Hour < ttl {
				t.Fatalf("ttl %s", ttl)
			}
			server.Raise(r.Set(ctx, redisKey, "not a record", time.Hour).Err())
		})
		if claim := model.GetSnClaimRelayClaim(ctx, key, 1, 7); claim != nil {
			t.Fatalf("a record that does not decode: %+v", claim)
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
	// the ledger object store, once a capture the flag root is published
	store server.BlobStore
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

		// the claim it paid is named to the network it paid, with what it paid,
		// on the wire too
		paidAnswer, err := SnClaimRelay(world.session(0))
		if err != nil || paidAnswer.Error != nil || len(paidAnswer.Accounts) != 1 {
			t.Fatalf("paid answer %+v (%v)", paidAnswer, err)
		}
		if paidAccount := paidAnswer.Accounts[0]; paidAccount.ClaimTxHash != strings.ToLower(tx.Hash().Hex()) || paidAccount.ClaimAmountRao != int64(world.leaves[0].Cumulative.Uint64()) || paidAccount.ClaimableRao != 0 {
			t.Fatalf("paid account %+v, want %s", paidAccount, tx.Hash().Hex())
		}
		wire, err := json.Marshal(paidAnswer)
		if err != nil || !strings.Contains(string(wire), `"claim_tx_hash":"`+strings.ToLower(tx.Hash().Hex())+`"`) || !strings.Contains(string(wire), fmt.Sprintf(`"claim_amount_rao":%d`, world.leaves[0].Cumulative.Uint64())) {
			t.Fatalf("on the wire %s (%v)", wire, err)
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
		// nothing claimed yet, nothing named
		if account.ClaimTxHash != "" || account.ClaimAmountRao != 0 {
			t.Fatalf("an unclaimed account names a claim: %+v", account)
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
		// the tapped claim is named, the transaction the relay sent for it
		if tapped := world.fixture.sent[1]; answer.Accounts[0].ClaimTxHash != strings.ToLower(tapped.Hash().Hex()) || answer.Accounts[0].ClaimAmountRao != int64(world.leaves[1].Cumulative.Uint64()) {
			t.Fatalf("tapped claim %+v, want %s", answer.Accounts[0], tapped.Hash().Hex())
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

		coldkeys, err := model.GetNetworkWalletMappingConsentColdkeys(ctx, f.rpc.domain, f.owner.NetworkId, 8)
		if err != nil {
			t.Fatal(err)
		}
		if len(coldkeys) != 2 || coldkeys[0] != secondKey.Public().Encode() || coldkeys[1] != firstKey.Public().Encode() {
			t.Fatalf("coldkeys %x", coldkeys)
		}
		if newest, err := model.GetNetworkWalletMappingConsentColdkeys(ctx, f.rpc.domain, f.owner.NetworkId, 1); err != nil || len(newest) != 1 || newest[0] != secondKey.Public().Encode() {
			t.Fatalf("the newest coldkey %x (%v)", newest, err)
		}
		if others, err := model.GetNetworkWalletMappingConsentColdkeys(ctx, f.rpc.domain, server.NewId(), 8); err != nil || len(others) != 0 {
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

// publishCtfRoot gives the operator's capture the flag pool, 2^32 + 1, its
// finalized root rootIndex (seq rootIndex + 1) of epoch, published as the
// driver publishes one: the account chunks and a CTF header naming carry as the
// carry contract's coldkey. Its tree is read through that header, since the
// world's builder knows only pool 1's ledger. The account index records the
// accounts it adds, and vault2 answers the root and, for each account it adds,
// nothing claimed. It returns the ledger hash.
func (self *claimRelayDbWorld) publishCtfRoot(carry [32]byte, rootIndex uint64, epoch uint64, leaves []merkle.LedgerAccount) [32]byte {
	t := self.t
	t.Helper()
	if self.store == nil {
		self.store = server.NewLocalBlobStore(t.TempDir(), "test")
		SetStLedgerV2ObjectStore(self.store)
		t.Cleanup(func() { SetStLedgerV2ObjectStore(nil) })
	}
	chunks, err := ledger.EncodeAccountChunks(leaves)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]ledger.Ref, len(chunks))
	for index, chunk := range chunks {
		if _, err := PutStLedgerV2Object(self.ctx, chunk); err != nil {
			t.Fatal(err)
		}
		refs[index] = ledger.RefOf(chunk)
	}
	empty := ledger.RefOf(nil)
	header, err := ctf.EncodeHeader(&ctf.Header{
		NoID: self.cfg.NoId, Epoch: epoch, CarryColdkey: carry,
		Epochs:   []ctf.HeaderEpoch{{Epoch: epoch, Awards: empty, Pots: empty, Pending: empty}},
		Accounts: refs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutStLedgerV2Object(self.ctx, header); err != nil {
		t.Fatal(err)
	}
	ledgerHash := [32]byte(ledger.RefOf(header))
	tree, err := merkle.BuildLedgerSumTree(self.ctx, leaves)
	if err != nil {
		t.Fatal(err)
	}
	root, total := tree.Root()
	key := self.cfg.DeploymentKey()
	model.SetStLedgerV2RootFinalized(self.ctx, key, &model.StLedgerV2Root{NoId: claimRelayTestCtfPool, Seq: rootIndex + 1, Epoch: epoch, Root: root, Total: total, LeafCount: uint64(len(leaves)), LedgerHash: ledgerHash, RootIndex: &rootIndex}, 90+rootIndex)
	firstIndex := model.GetStLedgerV2AccountCount(self.ctx, key, claimRelayTestCtfPool)
	newColdkeys := [][32]byte{}
	for _, leaf := range leaves[firstIndex:] {
		newColdkeys = append(newColdkeys, leaf.Coldkey)
	}
	if err := model.RecordStLedgerV2Root(self.ctx, key, claimRelayTestCtfPool, rootIndex+1, firstIndex, newColdkeys, nil); err != nil {
		t.Fatal(err)
	}
	parsed, err := stabi.STSettlementVault2MetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	rootOutput, err := parsed.Methods["roots"].Outputs.Pack(stabi.ISTSettlementVault2Root{Root: root, Total: total.ToBig(), LeafCount: uint64(len(leaves)), Epoch: epoch, LedgerHash: ledgerHash})
	if err != nil {
		t.Fatal(err)
	}
	self.fixture.lock.Lock()
	defer self.fixture.lock.Unlock()
	self.fixture.results[string(stLedgerV2VaultAbi.PackRoots(stBig(claimRelayTestCtfPool), stBig(rootIndex)))] = rootOutput
	for index := firstIndex; index < uint64(len(leaves)); index++ {
		self.fixture.results[string(stLedgerV2VaultAbi.PackClaimed(stBig(claimRelayTestCtfPool), stBig(index)))] = make([]byte, 32)
	}
	return ledgerHash
}

// payBothPools answers each claim the relay sends, in either pool, as vault2
// pays it: a Claimed log in the receipt, and claimed reads the cumulative from
// then on. The function it returns lists the accounts paid, by pool and index,
// in order.
func (self *claimRelayDbWorld) payBothPools() func() []string {
	var paid []string
	self.fixture.lock.Lock()
	defer self.fixture.lock.Unlock()
	self.fixture.receiptHook = func(tx *types.Transaction, receipt *types.Receipt) {
		claims, _, err := snClaimRelayDecodeCalldata(tx.Data())
		if err != nil {
			self.t.Errorf("a transaction that is not a claim: %v", err)
			return
		}
		receipt.GasUsed = 155_000
		receipt.EffectiveGasPrice = claimRelayGwei(5)
		for _, item := range claims {
			noId, index := item.NoId.Uint64(), item.Account.Index.Uint64()
			cumulative, overflow := uint256.FromBig(item.Account.Cumulative)
			if overflow {
				self.t.Errorf("claim %d/%d of %s", noId, index, item.Account.Cumulative)
				return
			}
			claim := &snClaimRelayClaim{Account: &snClaimRelayAccount{NoId: noId, Index: index, RootIndex: item.RootIndex.Uint64(), Coldkey: item.Account.Coldkey, Cumulative: *cumulative}}
			receipt.Logs = append(receipt.Logs, claimRelayDbClaimedLog(self.t, self.cfg, claim, self.chain.relayer.address))
			word := make([]byte, 32)
			item.Account.Cumulative.FillBytes(word)
			self.fixture.results[string(stLedgerV2VaultAbi.PackClaimed(stBig(noId), stBig(index)))] = word
			paid = append(paid, snClaimRelayAccountKey(noId, index))
		}
	}
	return func() []string {
		self.fixture.lock.Lock()
		defer self.fixture.lock.Unlock()
		return slices.Clone(paid)
	}
}

// claimRelayDbNear reports whether a unix-milliseconds time is the limit's end,
// which the reads place by its remaining time to within a few milliseconds.
func claimRelayDbNear(millis int64, until time.Time) bool {
	return time.Duration(millis-until.UnixMilli()).Abs()*time.Millisecond <= time.Second
}

// With capture the flag on, GET /sn/claim-relay reads both of the operator's
// pools from the database: a coldkey's account in its pool and in its capture
// the flag pool, each a row of its own kind and label, whether or not
// partial=true is asked. Once the CTF pool's latest root cannot be read,
// partial=true names that pool in unavailable_pools beside the pool's row,
// while an old client, which never sends partial, gets the pool's row alone,
// as before the CTF pool, and no error. The pass claims the pool it can read
// and says the ledger is unavailable, naming the other, and a Claim now takes
// the readable pool's account and keeps its request for the other.
func TestDbClaimRelayRouteReadsBothPools(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		world := newClaimRelayDbWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao), claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
		setCtfPoolTestSwitches(t, world.cfg, true, true)
		ctx := world.ctx
		key := world.cfg.DeploymentKey()
		carry, holder := claimRelayTestColdkey(9), claimRelayTestColdkey(1)
		ctfLeaves := []merkle.LedgerAccount{
			// the pots, over the threshold: the carry's recycle claims them
			{Coldkey: carry, Cumulative: *claimRelayTestAlpha(4 * snClaimRelayAutomaticTaoRao)},
			{Coldkey: holder, Cumulative: *claimRelayTestAlpha(20 * snClaimRelayFloorTaoRao)},
		}
		world.publishCtfRoot(carry, 0, 9, ctfLeaves)
		paid := world.payBothPools()

		// the pass claims pool 1's account over the threshold, never the carry's
		if result := SnClaimRelayPass(ctx); result.State != SnClaimRelayStateRunning {
			t.Fatalf("pass: %s", result)
		}
		if got := paid(); !slices.Equal(got, []string{"1/0"}) {
			t.Fatalf("paid %v", got)
		}

		clientSession := world.session(1)
		for _, partial := range []bool{true, false} {
			answer, err := SnClaimRelayWithArgs(&SnClaimRelayArgs{Partial: partial}, clientSession)
			if err != nil || answer.Error != nil || answer.State != SnClaimRelayStateRunning || len(answer.Accounts) != 2 || len(answer.UnavailablePools) != 0 {
				t.Fatalf("partial %t: %+v (%v)", partial, answer, err)
			}
			pool, flag := answer.Accounts[0], answer.Accounts[1]
			if pool.NoId != 1 || pool.Kind != StLedgerV2PoolKindPool || pool.Label != "Pool 1" || pool.Index != 1 ||
				pool.ClaimableRao != int64(world.leaves[1].Cumulative.Uint64()) || pool.Reason != SnClaimRelayReasonUnderThreshold || !pool.ClaimNowAvailable {
				t.Fatalf("partial %t: the pool's account %+v", partial, pool)
			}
			if flag.NoId != int64(claimRelayTestCtfPool) || flag.Kind != StLedgerV2PoolKindCtf || flag.Label != "Capture the flag" || flag.Index != 1 || flag.ColdkeySs58 != pool.ColdkeySs58 ||
				flag.ClaimableRao != int64(ctfLeaves[1].Cumulative.Uint64()) || flag.Reason != SnClaimRelayReasonUnderThreshold || !flag.ClaimNowAvailable {
				t.Fatalf("partial %t: the capture the flag account %+v", partial, flag)
			}
			wire, err := json.Marshal(answer)
			if err != nil || !strings.Contains(string(wire), `"unavailable_pools":[]`) ||
				!strings.Contains(string(wire), fmt.Sprintf(`"no_id":%d,"kind":"ctf","label":"Capture the flag","index":1,`, claimRelayTestCtfPool)) {
				t.Fatalf("partial %t on the wire %s (%v)", partial, wire, err)
			}
		}

		// the CTF pool's next root names objects nobody published
		lost := uint64(1)
		model.SetStLedgerV2RootFinalized(ctx, key, &model.StLedgerV2Root{NoId: claimRelayTestCtfPool, Seq: 2, Epoch: 10, Root: [32]byte{0xcf}, Total: *uint256.NewInt(1), LeafCount: 2, LedgerHash: [32]byte{0xcf, 0x01}, RootIndex: &lost}, 95)
		resetLedgerV2Caches()
		answer, err := SnClaimRelayWithArgs(&SnClaimRelayArgs{Partial: true}, clientSession)
		if err != nil || answer.Error != nil || len(answer.Accounts) != 1 || answer.Accounts[0].NoId != 1 || len(answer.UnavailablePools) != 1 {
			t.Fatalf("partial, the CTF pool unreadable: %+v (%v)", answer, err)
		}
		if pool := answer.UnavailablePools[0]; pool.NoId != claimRelayTestCtfPool || pool.Kind != StLedgerV2PoolKindCtf || pool.Label != "Capture the flag" || pool.Vault != stLedgerV2Address(world.cfg.VaultV2) {
			t.Fatalf("the unavailable pool %+v", pool)
		}
		// an old client, which never sends partial: the pool's account as
		// before the CTF pool, and no error
		old, err := SnClaimRelay(clientSession)
		if err != nil || old.Error != nil || len(old.Accounts) != 1 || old.Accounts[0].NoId != 1 || len(old.UnavailablePools) != 0 || old.State != SnClaimRelayStateRunning {
			t.Fatalf("an old client's answer %+v (%v)", old, err)
		}
		if *old.Accounts[0] != *answer.Accounts[0] {
			t.Fatalf("an old client's account %+v, a partial answer's %+v", old.Accounts[0], answer.Accounts[0])
		}

		// the pass claims what it can read and names the pool it cannot
		SnClaimRelayPass(ctx)
		status := snClaimRelayCurrentStatus(ctx, world.cfg, server.NowUtc())
		if status.State != SnClaimRelayStateLedgerUnavailable || !slices.Equal(status.UnavailablePools, []uint64{claimRelayTestCtfPool}) {
			t.Fatalf("status %+v", status)
		}
		answer, err = SnClaimRelayWithArgs(&SnClaimRelayArgs{Partial: true}, clientSession)
		if err != nil || answer.State != SnClaimRelayStateLedgerUnavailable || len(answer.Accounts) != 1 || answer.Accounts[0].Reason != SnClaimRelayReasonUnderThreshold || !answer.Accounts[0].ClaimNowAvailable {
			t.Fatalf("the readable pool while the other is unavailable: %+v (%v)", answer, err)
		}
		tap, err := SnClaimRelayClaimNow(&SnClaimRelayClaimNowArgs{ColdkeySs58: answer.Accounts[0].ColdkeySs58}, clientSession)
		if err != nil || !tap.Requested {
			t.Fatalf("tap %+v (%v)", tap, err)
		}
		SnClaimRelayPass(ctx)
		if got := paid(); !slices.Equal(got, []string{"1/0", "1/1"}) {
			t.Fatalf("paid %v", got)
		}
		// paid, the coldkey's limit runs, and its request stays for the account
		// the pass cannot read
		now := server.NowUtc()
		if requested, until := model.GetSnClaimRelayRequest(ctx, key, holder, now); !requested || until.Before(now.Add(snClaimRelayClaimNowPeriod-time.Hour)) {
			t.Fatalf("after the readable pool's claim: requested %t until %s", requested, until)
		}
	})
}

// Claim now on the real tables and Redis, with capture the flag on, for a
// coldkey with an account in each of the operator's pools and no batcher: the
// first pass pays the pool's account, which starts the coldkey's 30-day limit,
// a Redis key of its own (model.StartSnClaimRelayLimit), while the request
// stays for the capture the flag account; the next pass pays that one and ends
// the request, and the limit keeps its first end. While it runs, the coldkey's
// new balance waits with the limit's reason and end, and a tap is refused with
// them; another coldkey taps freely.
func TestDbClaimRelayClaimNowLimitIsPerColdkey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
		world := newClaimRelayDbWorld(t, small, small)
		setCtfPoolTestSwitches(t, world.cfg, true, true)
		ctx := world.ctx
		key := world.cfg.DeploymentKey()
		carry, tapped, other := claimRelayTestColdkey(9), claimRelayTestColdkey(1), claimRelayTestColdkey(0)
		ctfLeaves := []merkle.LedgerAccount{
			{Coldkey: carry, Cumulative: *claimRelayTestAlpha(4 * snClaimRelayAutomaticTaoRao)},
			{Coldkey: tapped, Cumulative: *claimRelayTestAlpha(20 * snClaimRelayFloorTaoRao)},
		}
		world.publishCtfRoot(carry, 0, 9, ctfLeaves)
		paid := world.payBothPools()

		// nothing is due, and the carry's account is never the relay's to claim
		SnClaimRelayPass(ctx)
		if got := paid(); len(got) != 0 {
			t.Fatalf("paid %v", got)
		}
		clientSession := world.session(1)
		address, err := ss58.Encode(tapped, ss58.BittensorPrefix)
		if err != nil {
			t.Fatal(err)
		}
		if tap, err := SnClaimRelayClaimNow(&SnClaimRelayClaimNowArgs{ColdkeySs58: address}, clientSession); err != nil || !tap.Requested {
			t.Fatalf("tap %+v (%v)", tap, err)
		}

		// without a batcher each claim goes alone: the pool's account first
		SnClaimRelayPass(ctx)
		if got := paid(); !slices.Equal(got, []string{"1/1"}) {
			t.Fatalf("paid %v", got)
		}
		now := server.NowUtc()
		requested, until := model.GetSnClaimRelayRequest(ctx, key, tapped, now)
		if !requested || until.Before(now.Add(snClaimRelayClaimNowPeriod-time.Hour)) || now.Add(snClaimRelayClaimNowPeriod).Before(until) {
			t.Fatalf("after the first claim: requested %t until %s", requested, until)
		}
		// the limit is the coldkey's own key in Redis, holding its start and kept
		// for the period
		limitKey := fmt.Sprintf("sn_claim_relay_limit:%s:%s", key, hex.EncodeToString(tapped[:]))
		limit := func() (start string, ttl time.Duration) {
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				start, err = r.Get(ctx, limitKey).Result()
				server.Raise(err)
				ttl, err = r.PTTL(ctx, limitKey).Result()
				server.Raise(err)
			})
			return
		}
		limitStart, limitTtl := limit()
		if limitTtl < snClaimRelayClaimNowPeriod-time.Hour || snClaimRelayClaimNowPeriod < limitTtl {
			t.Fatalf("limit %s ttl %s", limitKey, limitTtl)
		}
		answer, err := SnClaimRelayWithArgs(&SnClaimRelayArgs{Partial: true}, clientSession)
		if err != nil || answer.Error != nil || len(answer.Accounts) != 2 {
			t.Fatalf("answer %+v (%v)", answer, err)
		}
		if pool := answer.Accounts[0]; pool.ClaimableRao != 0 || pool.Reason != SnClaimRelayReasonNothingPending || !claimRelayDbNear(pool.ClaimNowNextAtMillis, until) {
			t.Fatalf("the paid account %+v, limit until %s", pool, until)
		}
		if flag := answer.Accounts[1]; flag.Reason != SnClaimRelayReasonQueued || flag.ClaimNowReason != SnClaimRelayClaimNowRequested || flag.ClaimNowAvailable {
			t.Fatalf("the queued account %+v", flag)
		}

		SnClaimRelayPass(ctx)
		if got := paid(); !slices.Equal(got, []string{"1/1", claimRelayCtfKey(1)}) {
			t.Fatalf("paid %v", got)
		}
		if requests := model.GetSnClaimRelayRequests(ctx, key, 10); len(requests) != 0 {
			t.Fatalf("requests %+v", requests)
		}
		if requested, again := model.GetSnClaimRelayRequest(ctx, key, tapped, server.NowUtc()); requested || again.Sub(until).Abs() > time.Second {
			t.Fatalf("the limit moved from %s to %s (requested %t)", until, again, requested)
		}
		// the paid claim that ended the request kept the running limit: its
		// start, and the time left of it
		if start, ttl := limit(); start != limitStart || limitTtl < ttl {
			t.Fatalf("the limit started again: start %s ttl %s, was %s ttl %s", start, ttl, limitStart, limitTtl)
		}

		// the coldkey earns again in the capture the flag pool: the balance waits
		// for the limit, and a tap says until when
		more := claimRelayTestAlpha(15 * snClaimRelayFloorTaoRao)
		ctfLeaves[1].Cumulative.Add(&ctfLeaves[1].Cumulative, more)
		world.publishCtfRoot(carry, 1, 10, ctfLeaves)
		answer, err = SnClaimRelayWithArgs(&SnClaimRelayArgs{Partial: true}, clientSession)
		if err != nil || answer.Error != nil || len(answer.Accounts) != 2 {
			t.Fatalf("answer %+v (%v)", answer, err)
		}
		if flag := answer.Accounts[1]; flag.ClaimableRao != int64(more.Uint64()) || flag.ClaimNowAvailable || flag.ClaimNowReason != SnClaimRelayClaimNowLimit || !claimRelayDbNear(flag.ClaimNowNextAtMillis, until) {
			t.Fatalf("a new balance under the limit %+v, limit until %s", flag, until)
		}
		tap, err := SnClaimRelayClaimNow(&SnClaimRelayClaimNowArgs{ColdkeySs58: address}, clientSession)
		if err != nil || tap.Requested || tap.Reason != SnClaimRelayClaimNowLimit || !claimRelayDbNear(tap.NextAtMillis, until) {
			t.Fatalf("a tap under the limit %+v (%v)", tap, err)
		}
		// the limit is that coldkey's alone
		otherAddress, err := ss58.Encode(other, ss58.BittensorPrefix)
		if err != nil {
			t.Fatal(err)
		}
		if tap, err := SnClaimRelayClaimNow(&SnClaimRelayClaimNowArgs{ColdkeySs58: otherAddress}, world.session(0)); err != nil || !tap.Requested {
			t.Fatalf("another coldkey's tap %+v (%v)", tap, err)
		}
	})
}

// claimRelayDbKeys lists a JSON object's keys, sorted.
func claimRelayDbKeys(t testing.TB, raw []byte) []string {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// With capture the flag off (no st.yml ctf block, so no CTF root), the relay
// answers an old client, which never sends partial=true, as it did before the
// CTF pool: the fields it read then, and besides them only each account's kind
// and label and an empty unavailable_pools. A pass that cannot read the
// operator's pool ends there as it did, ledger_unavailable whatever else would
// stop it (here a low balance), and leaves every Claim now request queued; the
// old client reads the error and no balances, and the ledger route a 502. The
// pools_unavailable metric counts the pool.
func TestDbClaimRelayWithCtfOffAnswersAsBefore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
		world := newClaimRelayDbWorld(t, small, small)
		ctx := world.ctx
		key := world.cfg.DeploymentKey()
		if world.cfg.Ctf.On() {
			t.Fatalf("capture the flag is on: %+v", world.cfg.Ctf)
		}
		// an object store with nothing in it: no read leaves the test
		SetStLedgerV2ObjectStore(server.NewLocalBlobStore(t.TempDir(), "test"))
		t.Cleanup(func() { SetStLedgerV2ObjectStore(nil) })

		if result := SnClaimRelayPass(ctx); result.State != SnClaimRelayStateRunning {
			t.Fatalf("pass: %s", result)
		}
		clientSession := world.session(1)
		answer, err := SnClaimRelay(clientSession)
		if err != nil || answer.Error != nil || len(answer.Accounts) != 1 {
			t.Fatalf("answer %+v (%v)", answer, err)
		}
		account := answer.Accounts[0]
		if account.NoId != 1 || account.Kind != StLedgerV2PoolKindPool || account.Label != "Pool 1" || account.Index != 1 || account.Reason != SnClaimRelayReasonUnderThreshold || !account.ClaimNowAvailable {
			t.Fatalf("account %+v", account)
		}
		wire, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(wire, &top); err != nil {
			t.Fatal(err)
		}
		accounts := []json.RawMessage{}
		if err := json.Unmarshal(top["accounts"], &accounts); err != nil || len(accounts) != 1 || string(top["unavailable_pools"]) != "[]" {
			t.Fatalf("on the wire %s (%v)", wire, err)
		}
		// the answer's fields before capture the flag, and unavailable_pools
		if keys := claimRelayDbKeys(t, wire); !slices.Equal(keys, []string{"accounts", "automatic_threshold_rao", "automatic_threshold_tao_rao", "claim_now_period_days", "state", "transfer_floor_rao", "transfer_floor_tao_rao", "unavailable_pools"}) {
			t.Fatalf("answer fields %v", keys)
		}
		// an account's fields before capture the flag, and kind and label
		if keys := claimRelayDbKeys(t, accounts[0]); !slices.Equal(keys, []string{"claim_now_available", "claimable_rao", "coldkey_ss58", "index", "kind", "label", "no_id", "pending_rao", "reason", "value_tao_rao"}) {
			t.Fatalf("account fields %v", keys)
		}

		// the operator's pool's next root names objects nobody published, and the
		// relay's balance is low
		one := uint64(1)
		model.SetStLedgerV2RootFinalized(ctx, key, &model.StLedgerV2Root{NoId: 1, Seq: 2, Epoch: 5, Root: [32]byte{0xaf}, Total: *uint256.NewInt(1), LeafCount: 2, LedgerHash: [32]byte{0xaf, 0x01}, RootIndex: &one}, 95)
		resetLedgerV2Caches()
		world.fixture.lock.Lock()
		world.fixture.balance = big.NewInt(1)
		world.fixture.lock.Unlock()
		// requests of a coldkey the account index records and of one it does not
		now := server.NowUtc()
		model.AddSnClaimRelayRequest(ctx, key, claimRelayTestColdkey(1), now)
		model.AddSnClaimRelayRequest(ctx, key, claimRelayTestColdkey(5), now)

		result := SnClaimRelayPass(ctx)
		status := snClaimRelayCurrentStatus(ctx, world.cfg, server.NowUtc())
		if result.State != SnClaimRelayStateLedgerUnavailable || status.State != SnClaimRelayStateLedgerUnavailable || len(status.UnavailablePools) != 0 || !strings.Contains(status.Detail, "pool 1:") {
			t.Fatalf("pass %s, status %+v", result, status)
		}
		if gauge := testutil.ToFloat64(snClaimRelayPoolsUnavailable); status.PoolsUnavailable != 1 || gauge != 1 {
			t.Fatalf("pools unavailable %d, metric %v", status.PoolsUnavailable, gauge)
		}
		if requests := model.GetSnClaimRelayRequests(ctx, key, 10); len(requests) != 2 {
			t.Fatalf("requests %+v", requests)
		}
		old, err := SnClaimRelay(clientSession)
		if err != nil || old.Error == nil || len(old.Accounts) != 0 || len(old.UnavailablePools) != 0 || old.State != SnClaimRelayStateLedgerUnavailable {
			t.Fatalf("an old client's answer %+v (%v)", old, err)
		}
		if response := serveLedgerV2(SnLedgerAccount, "/sn/ledger/account?coldkey="+ledgerV2Hex(claimRelayTestColdkey(1))); response.Code != http.StatusBadGateway {
			t.Fatalf("the ledger route %d %s", response.Code, response.Body.String())
		}
	})
}

// With capture the flag on, once its pool has a root, a pass that cannot read
// the operator's pool still claims the CTF pool's accounts on the real tables
// and names the operator's pool unavailable; the request of a coldkey that
// pool's account index records waits for it, and one of a coldkey it does not
// record, with nothing readable to claim, is given back.
func TestDbClaimRelayClaimsTheCtfPoolWhileThePoolCannotBeRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		small := claimRelayTestAlpha(10 * snClaimRelayFloorTaoRao)
		world := newClaimRelayDbWorld(t, small, small)
		setCtfPoolTestSwitches(t, world.cfg, true, true)
		ctx := world.ctx
		key := world.cfg.DeploymentKey()
		carry := claimRelayTestColdkey(9)
		world.publishCtfRoot(carry, 0, 9, []merkle.LedgerAccount{
			{Coldkey: carry, Cumulative: *claimRelayTestAlpha(4 * snClaimRelayAutomaticTaoRao)},
			{Coldkey: claimRelayTestColdkey(0), Cumulative: *claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)},
		})
		paid := world.payBothPools()
		// the operator's pool's next root names objects nobody published
		one := uint64(1)
		model.SetStLedgerV2RootFinalized(ctx, key, &model.StLedgerV2Root{NoId: 1, Seq: 2, Epoch: 5, Root: [32]byte{0xaf}, Total: *uint256.NewInt(1), LeafCount: 2, LedgerHash: [32]byte{0xaf, 0x01}, RootIndex: &one}, 95)
		resetLedgerV2Caches()
		now := server.NowUtc()
		model.AddSnClaimRelayRequest(ctx, key, claimRelayTestColdkey(1), now)
		model.AddSnClaimRelayRequest(ctx, key, claimRelayTestColdkey(5), now)

		SnClaimRelayPass(ctx)
		status := snClaimRelayCurrentStatus(ctx, world.cfg, server.NowUtc())
		if status.State != SnClaimRelayStateLedgerUnavailable || !slices.Equal(status.UnavailablePools, []uint64{1}) || status.PoolsUnavailable != 1 {
			t.Fatalf("status %+v", status)
		}
		if got := paid(); !slices.Equal(got, []string{claimRelayCtfKey(1)}) {
			t.Fatalf("paid %v", got)
		}
		requests := model.GetSnClaimRelayRequests(ctx, key, 10)
		if len(requests) != 1 || requests[0].Coldkey != claimRelayTestColdkey(1) {
			t.Fatalf("requests %+v", requests)
		}
	})
}

// With capture the flag off the claim relay reads no capture the flag pool, so
// no fault of one can reach a client. Here every read of the operator's CTF
// pool fails: its finalized root names objects nobody published, and the
// account index records coldkey 1 there. Whichever switch is off (no ctf.yml or
// its api block off, or no st.yml ctf block), the pass still claims pool 1 and
// runs with no pool unavailable, an old client and a partial=true one both read
// coldkey 1's account in pool 1 whole, and a Claim now is taken and paid.
// Turned on, the same pool is named unavailable by the pass and by a
// partial=true answer, so a read of it would show, while an old client still
// reads pool 1's account alone, as before.
func TestDbClaimRelayReadsNoCtfPoolWithCtfOff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		world := newClaimRelayDbWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao), claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
		ctx := world.ctx
		key := world.cfg.DeploymentKey()
		// an object store with nothing in it: no read leaves the test
		SetStLedgerV2ObjectStore(server.NewLocalBlobStore(t.TempDir(), "test"))
		t.Cleanup(func() { SetStLedgerV2ObjectStore(nil) })
		zero := uint64(0)
		model.SetStLedgerV2RootFinalized(ctx, key, &model.StLedgerV2Root{NoId: claimRelayTestCtfPool, Seq: 1, Epoch: 9, Root: [32]byte{0xcf}, Total: *uint256.NewInt(1), LeafCount: 2, LedgerHash: [32]byte{0xcf, 0x01}, RootIndex: &zero}, 95)
		if err := model.RecordStLedgerV2Root(ctx, key, claimRelayTestCtfPool, 1, 0, [][32]byte{claimRelayTestColdkey(9), claimRelayTestColdkey(1)}, nil); err != nil {
			t.Fatal(err)
		}
		paid := world.payBothPools()
		clientSession := world.session(1)
		readsPool1 := func(name string, partial bool) *SnClaimRelayResult {
			t.Helper()
			answer, err := SnClaimRelayWithArgs(&SnClaimRelayArgs{Partial: partial}, clientSession)
			if err != nil || answer.Error != nil || len(answer.Accounts) != 1 || answer.Accounts[0].NoId != 1 || answer.Accounts[0].Index != 1 || len(answer.UnavailablePools) != 0 {
				t.Fatalf("%s, partial %t: %+v (%v)", name, partial, answer, err)
			}
			return answer
		}

		var answer *SnClaimRelayResult
		for _, off := range []struct {
			name   string
			stYml  bool
			ctfYml bool
		}{
			{"neither switch", false, false},
			{"no ctf.yml, or its api block off", true, false},
			{"no st.yml ctf block", false, true},
		} {
			setCtfPoolTestSwitches(t, world.cfg, off.stYml, off.ctfYml)
			result := SnClaimRelayPass(ctx)
			status := snClaimRelayCurrentStatus(ctx, world.cfg, server.NowUtc())
			if gauge := testutil.ToFloat64(snClaimRelayPoolsUnavailable); result.State != SnClaimRelayStateRunning || len(status.UnavailablePools) != 0 || status.PoolsUnavailable != 0 || gauge != 0 {
				t.Fatalf("%s: pass %s, status %+v, metric %v", off.name, result, status, gauge)
			}
			readsPool1(off.name, false)
			answer = readsPool1(off.name, true)
		}
		if got := paid(); !slices.Equal(got, []string{"1/0"}) {
			t.Fatalf("paid %v", got)
		}
		tap, err := SnClaimRelayClaimNow(&SnClaimRelayClaimNowArgs{ColdkeySs58: answer.Accounts[0].ColdkeySs58}, clientSession)
		if err != nil || !tap.Requested {
			t.Fatalf("tap %+v (%v)", tap, err)
		}
		if result := SnClaimRelayPass(ctx); result.State != SnClaimRelayStateRunning || !slices.Equal(paid(), []string{"1/0", "1/1"}) {
			t.Fatalf("the tapped pass %s, paid %v", result, paid())
		}

		// on, the same pool cannot be read
		setCtfPoolTestSwitches(t, world.cfg, true, true)
		SnClaimRelayPass(ctx)
		status := snClaimRelayCurrentStatus(ctx, world.cfg, server.NowUtc())
		if gauge := testutil.ToFloat64(snClaimRelayPoolsUnavailable); status.State != SnClaimRelayStateLedgerUnavailable || !slices.Equal(status.UnavailablePools, []uint64{claimRelayTestCtfPool}) || status.PoolsUnavailable != 1 || gauge != 1 {
			t.Fatalf("on: status %+v, metric %v", status, gauge)
		}
		readsPool1("on, an old client", false)
		if partial, err := SnClaimRelayWithArgs(&SnClaimRelayArgs{Partial: true}, clientSession); err != nil || partial.Error != nil || len(partial.Accounts) != 1 || len(partial.UnavailablePools) != 1 || partial.UnavailablePools[0].NoId != claimRelayTestCtfPool {
			t.Fatalf("on: a partial answer %+v (%v)", partial, err)
		}
	})
}

// Old apps on the claim relay through a fault of the capture the flag pool
// (owner, 2026-11-11). With capture the flag on and that pool unreadable, GET
// /sn/claim-relay without partial=true answers each network exactly as it does
// with capture the flag off, the operator's pool's rows and no error, and a
// Claim now of a coldkey whose only account is in that pool reads
// nothing_pending, as before the pool, never an error; a partial=true client
// sees the pool named unavailable. Two faults are tried in turn: a row of the
// pool's roots that does not read back from the database, then a latest root
// whose objects nobody published. In each the pass claims the operator's pool
// and names the other, and an old app still reads its answer as before. Once
// the pool reads again, its accounts are answered again. This test fails if an
// old client sees an error during a CTF pool fault.
func TestDbClaimRelayAnswersOldClientsThroughACtfPoolFault(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		world := newClaimRelayDbWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao), claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
		setCtfPoolTestSwitches(t, world.cfg, true, true)
		ctx := world.ctx
		key := world.cfg.DeploymentKey()
		carry, holder, flagOnly := claimRelayTestColdkey(9), claimRelayTestColdkey(1), claimRelayTestColdkey(7)
		world.publishCtfRoot(carry, 0, 9, []merkle.LedgerAccount{
			{Coldkey: carry, Cumulative: *claimRelayTestAlpha(4 * snClaimRelayAutomaticTaoRao)},
			{Coldkey: holder, Cumulative: *claimRelayTestAlpha(20 * snClaimRelayFloorTaoRao)},
			{Coldkey: flagOnly, Cumulative: *claimRelayTestAlpha(30 * snClaimRelayFloorTaoRao)},
		})
		paid := world.payBothPools()
		// the pass claims pool 1's account over the threshold, and nothing else
		if result := SnClaimRelayPass(ctx); result.State != SnClaimRelayStateRunning || !slices.Equal(paid(), []string{"1/0"}) {
			t.Fatalf("pass %s, paid %v", result, paid())
		}
		flagOnlySs58, err := ss58.Encode(flagOnly, ss58.BittensorPrefix)
		if err != nil {
			t.Fatal(err)
		}
		networks := []struct {
			name    string
			session *session.ClientSession
			// its accounts' pools while the CTF pool reads
			pools []int64
		}{
			{"pool 1 alone", world.session(0), []int64{1}},
			{"both pools", world.session(1), []int64{1, int64(claimRelayTestCtfPool)}},
			{"the CTF pool alone", world.session(7), []int64{int64(claimRelayTestCtfPool)}},
		}
		wire := func(answer *SnClaimRelayResult) string {
			raw, err := json.Marshal(answer)
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		}
		readable := func(name string) {
			t.Helper()
			for _, network := range networks {
				for _, partial := range []bool{false, true} {
					answer, err := SnClaimRelayWithArgs(&SnClaimRelayArgs{Partial: partial}, network.session)
					if err != nil || answer.Error != nil || len(answer.UnavailablePools) != 0 || len(answer.Accounts) != len(network.pools) {
						t.Fatalf("%s, the network with %s, partial %t: %+v (%v)", name, network.name, partial, answer, err)
					}
					for i, account := range answer.Accounts {
						if account.NoId != network.pools[i] {
							t.Fatalf("%s, the network with %s, partial %t: account %+v", name, network.name, partial, account)
						}
					}
				}
			}
		}
		readable("before any fault")

		// what an old app reads while the CTF pool cannot be read: what it reads
		// with capture the flag off, which is its answer before the CTF pool
		oldApps := func(name string) {
			t.Helper()
			setCtfPoolTestSwitches(t, world.cfg, false, false)
			want := map[string]string{}
			for _, network := range networks {
				answer, err := SnClaimRelay(network.session)
				if err != nil || answer.Error != nil {
					t.Fatalf("%s, capture the flag off, the network with %s: %+v (%v)", name, network.name, answer, err)
				}
				want[network.name] = wire(answer)
			}
			setCtfPoolTestSwitches(t, world.cfg, true, true)
			for _, network := range networks {
				old, err := SnClaimRelay(network.session)
				if err != nil || old.Error != nil {
					t.Fatalf("%s: an old app of the network with %s sees an error: %+v (%v)", name, network.name, old, err)
				}
				if got := wire(old); got != want[network.name] {
					t.Fatalf("%s: an old app of the network with %s reads %s\nwant, as before the CTF pool, %s", name, network.name, got, want[network.name])
				}
				partial, err := SnClaimRelayWithArgs(&SnClaimRelayArgs{Partial: true}, network.session)
				if err != nil || partial.Error != nil || len(partial.UnavailablePools) != 1 || partial.UnavailablePools[0].NoId != claimRelayTestCtfPool {
					t.Fatalf("%s: a partial answer to the network with %s %+v (%v)", name, network.name, partial, err)
				}
			}
			// a tap of the coldkey whose only account the CTF pool holds reads
			// nothing to claim, as before that pool, not an error
			tap, err := SnClaimRelayClaimNow(&SnClaimRelayClaimNowArgs{ColdkeySs58: flagOnlySs58}, networks[2].session)
			if err != nil || tap.Error != nil || tap.Requested || tap.Reason != SnClaimRelayReasonNothingPending {
				t.Fatalf("%s: a tap of the coldkey in the CTF pool alone %+v (%v)", name, tap, err)
			}
		}
		duringAFault := func(name string) {
			t.Helper()
			oldApps(name)
			// the pass claims the pool it can read and names the CTF pool
			SnClaimRelayPass(ctx)
			status := snClaimRelayCurrentStatus(ctx, world.cfg, server.NowUtc())
			if status.State != SnClaimRelayStateLedgerUnavailable || !slices.Equal(status.UnavailablePools, []uint64{claimRelayTestCtfPool}) {
				t.Fatalf("%s: status %+v", name, status)
			}
			oldApps(name + ", after a pass")
		}
		exec := func(sql string, args ...any) {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, sql, args...))
			}, server.OptReadWrite())
		}

		// a row of the pool's roots does not read back from the database
		total := model.GetLatestStLedgerV2FinalizedRoot(ctx, key, claimRelayTestCtfPool).Total.Dec()
		exec(`UPDATE st_ledger_v2_root SET total = -1 WHERE deployment_key = $1 AND no_id = $2 AND seq = 1`, string(key), int64(claimRelayTestCtfPool))
		duringAFault("a row of the pool's roots does not read back")
		exec(`UPDATE st_ledger_v2_root SET total = $3 WHERE deployment_key = $1 AND no_id = $2 AND seq = 1`, string(key), int64(claimRelayTestCtfPool), total)
		readable("the row reads back again")

		// the pool's latest root names objects nobody published
		lost := uint64(1)
		model.SetStLedgerV2RootFinalized(ctx, key, &model.StLedgerV2Root{NoId: claimRelayTestCtfPool, Seq: 2, Epoch: 10, Root: [32]byte{0xcf}, Total: *uint256.NewInt(1), LeafCount: 3, LedgerHash: [32]byte{0xcf, 0x01}, RootIndex: &lost}, 95)
		resetLedgerV2Caches()
		duringAFault("the pool's latest root names objects nobody published")
		// nothing but pool 1's due account was ever claimed
		if got := paid(); !slices.Equal(got, []string{"1/0"}) {
			t.Fatalf("paid %v", got)
		}
	})
}
