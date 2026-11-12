package controller

// sn_claim_relay_test.go — the claim relay's policy, with its chain, clock, gas
// and stores injected. Nothing here reads a database, Redis or a network. A
// "restart" is a new relay over the same world: the world holds only what lasts
// across one, the chain, the durable intents and Redis.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server/model"
)

const claimRelayDbKey = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

const (
	claimRelayTestUsedGas     = uint64(155_000)
	claimRelayTestEstimateGas = uint64(165_000)
	// the griefed claim of sn's R-3 rehearsal
	claimRelayTestGriefedGas = uint64(1_260_000)
	claimRelayTestBatchGas   = uint64(125_000)
)

var (
	claimRelayTestGwei       = big.NewInt(1_000_000_000)
	claimRelayTestVault      = common.HexToAddress("0x00000000000000000000000000000000000000a2")
	claimRelayTestBatcher    = common.HexToAddress("0x00000000000000000000000000000000000000b2")
	claimRelayTestRelayer    = common.HexToAddress("0x00000000000000000000000000000000000000c2")
	claimRelayTestAlphaPrice = big.NewInt(4_224_000_000_000_000)
	claimRelayTestMonday     = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
)

func claimRelayGwei(gwei int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(gwei), claimRelayTestGwei)
}

// The rao of α worth a value in rao of TAO at the world's default price.
func claimRelayTestAlpha(valueTaoRao uint64) *uint256.Int {
	amount := snClaimRelayAlphaOf(valueTaoRao, claimRelayTestAlphaPrice)
	value, _ := uint256.FromBig(amount)
	return value
}

type claimRelayWorldTx struct {
	createTime time.Time
	send       *snClaimRelaySend
	gasUsed    uint64
	costWei    *big.Int
	// the fee cap an unfinished one was signed at
	feeCapWei *big.Int
	griefed   bool
	outcome   *snClaimRelayOutcome
	final     bool
}

// The capture the flag pool of operator 1, beside its pool 1.
const claimRelayTestCtfPool = uint64(1<<32 + 1)

// The world's maps key an account of pool 1 by its index and an account of the
// capture the flag pool by its index past claimRelayTestCtfSlot.
const claimRelayTestCtfSlot = uint64(1 << 20)

func claimRelaySlot(account *snClaimRelayAccount) uint64 {
	if account.NoId == claimRelayTestCtfPool {
		return claimRelayTestCtfSlot + account.Index
	}
	return account.Index
}

// claimRelayWorld is the chain, the transaction intents, the mirror and Redis.
type claimRelayWorld struct {
	t       testing.TB
	nowTime time.Time

	leaves []merkle.LedgerAccount
	tree   *merkle.LedgerSumTree
	// the capture the flag pool's accounts, none unless a test adds them
	ctfLeaves []merkle.LedgerAccount
	ctfTree   *merkle.LedgerSumTree
	// one pool's ledger cannot be read; ledgerErr fails both
	poolLedgerErr error
	ctfLedgerErr  error
	// a pool whose tree cannot be read for a claim's proof, by no_id
	proofErrs map[uint64]error
	// the account index of a pool that cannot be read cannot be read either
	indexUnreadable bool

	chainClaimed  map[uint64]*uint256.Int
	mirrorClaimed map[uint64]*uint256.Int

	balanceWei    *big.Int
	gasPriceWei   *big.Int
	feeCapWei     *big.Int
	alphaPriceWei *big.Int

	signerErr    error
	chainErr     error
	reconcileErr error
	factsErr     error
	spendErr     error
	claimedErr   error
	ledgerErr    error
	sendErr      error
	batchErr     error
	badProofs    map[uint64]bool

	estimates map[uint64]uint64
	refusals  map[uint64]error
	gasUsed   map[uint64]uint64
	reverts   map[uint64]bool
	// a batch's refused items, by account index
	batchRefusals map[uint64][4]byte
	batching      bool
	// the most claims the batcher fits in a batch, 0 for all
	batchMost int

	txs        []*claimRelayWorldTx
	requests   map[[32]byte]time.Time
	limits     map[[32]byte]time.Time
	records    map[string]*snClaimRelayPaid
	recorded   int
	status     *SnClaimRelayStatus
	banned     map[[32]byte]bool
	results    map[string]int
	chainBuilt int
	estimated  []uint64
	published  int
}

func newClaimRelayWorld(t testing.TB, amounts ...*uint256.Int) *claimRelayWorld {
	t.Helper()
	world := &claimRelayWorld{
		t: t, nowTime: claimRelayTestMonday,
		chainClaimed: map[uint64]*uint256.Int{}, mirrorClaimed: map[uint64]*uint256.Int{},
		balanceWei: claimRelayGwei(100_000_000), gasPriceWei: claimRelayGwei(5), feeCapWei: big.NewInt(5_625_000_000), alphaPriceWei: claimRelayTestAlphaPrice,
		badProofs: map[uint64]bool{}, estimates: map[uint64]uint64{}, refusals: map[uint64]error{}, gasUsed: map[uint64]uint64{}, reverts: map[uint64]bool{},
		batchRefusals: map[uint64][4]byte{},
		requests:      map[[32]byte]time.Time{}, limits: map[[32]byte]time.Time{}, banned: map[[32]byte]bool{}, results: map[string]int{},
		records: map[string]*snClaimRelayPaid{},
	}
	world.setLeaves(amounts...)
	return world
}

func claimRelayTestColdkey(index int) [32]byte {
	return [32]byte(crypto.Keccak256([]byte(fmt.Sprintf("claim relay test coldkey %d", index))))
}

func (self *claimRelayWorld) setLeaves(amounts ...*uint256.Int) {
	self.t.Helper()
	self.leaves = make([]merkle.LedgerAccount, len(amounts))
	for index, amount := range amounts {
		self.leaves[index] = merkle.LedgerAccount{Coldkey: claimRelayTestColdkey(index), Cumulative: *amount}
	}
	if len(amounts) == 0 {
		self.tree = nil
		return
	}
	tree, err := merkle.BuildLedgerSumTree(context.Background(), self.leaves)
	if err != nil {
		self.t.Fatal(err)
	}
	self.tree = tree
}

// addCtf gives the coldkey of pool 1's account coldkeyIndex an account in the
// capture the flag pool, at the pool's next index, and returns that index.
func (self *claimRelayWorld) addCtf(coldkeyIndex int, amount *uint256.Int) uint64 {
	self.t.Helper()
	self.ctfLeaves = append(self.ctfLeaves, merkle.LedgerAccount{Coldkey: claimRelayTestColdkey(coldkeyIndex), Cumulative: *amount})
	tree, err := merkle.BuildLedgerSumTree(context.Background(), self.ctfLeaves)
	if err != nil {
		self.t.Fatal(err)
	}
	self.ctfTree = tree
	return uint64(len(self.ctfLeaves) - 1)
}

func (self *claimRelayWorld) treeOf(account *snClaimRelayAccount) *merkle.LedgerSumTree {
	if account.NoId == claimRelayTestCtfPool {
		return self.ctfTree
	}
	return self.tree
}

// A new relay over this world: nothing but the world lasts.
func (self *claimRelayWorld) relay() *snClaimRelay {
	return &snClaimRelay{
		now: func() time.Time { return self.nowTime },
		signer: func() (*snClaimRelaySigner, error) {
			if self.signerErr != nil {
				return nil, self.signerErr
			}
			return &snClaimRelaySigner{Address: claimRelayTestRelayer}, nil
		},
		chain: func(*snClaimRelaySigner) (snClaimRelayChain, error) {
			self.chainBuilt += 1
			if self.chainErr != nil {
				return nil, self.chainErr
			}
			return self, nil
		},
		ledger:       self,
		requests:     self,
		claimRecords: self,
		lastStatus:   func(context.Context) *SnClaimRelayStatus { return self.status },
		publish: func(_ context.Context, status *SnClaimRelayStatus) {
			self.status = status
			self.published += 1
		},
		claimResults: func(result string, count int) { self.results[result] += count },
		banned:       func(coldkey [32]byte) bool { return self.banned[coldkey] },
	}
}

func (self *claimRelayWorld) pass() *SnClaimRelayStatus {
	self.t.Helper()
	return self.relay().pass(context.Background())
}

// Passes, each on a restarted relay, until one has nothing more to send now.
func (self *claimRelayWorld) passes() *SnClaimRelayStatus {
	self.t.Helper()
	for range 200 {
		status := self.pass()
		if !status.Again {
			return status
		}
	}
	self.t.Fatal("the relay never stopped")
	return nil
}

// snClaimRelayChain

func (self *claimRelayWorld) Reconcile(context.Context) error { return self.reconcileErr }

func (self *claimRelayWorld) Facts(context.Context) (*snClaimRelayFacts, error) {
	if self.factsErr != nil {
		return nil, self.factsErr
	}
	return &snClaimRelayFacts{BalanceWei: self.balanceWei, GasPriceWei: self.gasPriceWei, FeeCapWei: self.feeCapWei, AlphaPriceWei: self.alphaPriceWei}, nil
}

// The durable intents as the engine's Spend reads them, counted by the same add:
// from the day before on, newest first, a final one with its receipt and an
// unfinished one at the most its attempt can cost.
func (self *claimRelayWorld) Spend(_ context.Context, since time.Time) (*snClaimRelaySpend, error) {
	if self.spendErr != nil {
		return nil, self.spendErr
	}
	spend := &snClaimRelaySpend{SpentWei: new(big.Int), RevertedAccounts: map[string]string{}}
	for index := len(self.txs) - 1; 0 <= index; index -= 1 {
		tx := self.txs[index]
		if tx.createTime.Before(since.Add(-snClaimRelayPaidLookback)) {
			continue
		}
		intent := &model.StTransactionIntent{Status: model.StTxBroadcast, CreateTime: tx.createTime}
		var attempts []*model.StTransactionAttempt
		var receipt *snClaimRelayReceipt
		if tx.final {
			intent.Status = model.StTxFinalized
			receipt = &snClaimRelayReceipt{spentWei: tx.costWei, outcome: tx.outcome}
			for _, claim := range tx.send.Claims {
				receipt.accounts = append(receipt.accounts, claim.Account.key())
			}
		} else {
			feeCap := tx.feeCapWei.String()
			attempts = []*model.StTransactionAttempt{{GasLimit: tx.send.GasLimit, GasFeeCap: &feeCap}}
		}
		spend.add(intent, attempts, receipt, !tx.createTime.Before(since))
	}
	return spend, nil
}

func (self *claimRelayWorld) claimedOf(index uint64) *uint256.Int {
	if claimed := self.chainClaimed[index]; claimed != nil {
		return claimed
	}
	return new(uint256.Int)
}

func (self *claimRelayWorld) Claimed(_ context.Context, account *snClaimRelayAccount) (*uint256.Int, error) {
	if self.claimedErr != nil {
		return nil, self.claimedErr
	}
	return self.claimedOf(claimRelaySlot(account)), nil
}

func (self *claimRelayWorld) VerifyProof(_ context.Context, account *snClaimRelayAccount, proof []merkle.LedgerNode) (bool, error) {
	if self.badProofs[claimRelaySlot(account)] {
		return false, nil
	}
	tree := self.treeOf(account)
	root, total := tree.Root()
	return merkle.VerifyLedgerProof(root, &total, tree.LeafCount(), account.Index, account.Coldkey, &account.Cumulative, proof), nil
}

func (self *claimRelayWorld) estimateOf(index uint64) uint64 {
	if estimate, ok := self.estimates[index]; ok {
		return estimate
	}
	return claimRelayTestEstimateGas
}

func (self *claimRelayWorld) Estimate(_ context.Context, claim *snClaimRelayClaim) (uint64, error) {
	slot := claimRelaySlot(claim.Account)
	self.estimated = append(self.estimated, slot)
	if err := self.refusals[slot]; err != nil {
		return 0, err
	}
	return self.estimateOf(slot), nil
}

func (self *claimRelayWorld) Claim(claim *snClaimRelayClaim) (common.Address, []byte) {
	item := snClaimRelayItem(claim)
	return claimRelayTestVault, stLedgerV2VaultAbi.PackClaim(item.NoId, item.RootIndex, item.Account, item.Proof)
}

func (self *claimRelayWorld) Batching(context.Context) bool { return self.batching }

func (self *claimRelayWorld) SizeBatch(_ context.Context, claims []*snClaimRelayClaim, gasPerClaim uint64) (*snClaimRelayBatch, error) {
	if self.batchErr != nil {
		return nil, self.batchErr
	}
	if !self.batching {
		return nil, nil
	}
	if 0 < self.batchMost && self.batchMost < len(claims) {
		claims = claims[:self.batchMost]
	}
	// the last item needs its whole cap; the 20% is the tools' headroom
	need := uint64(len(claims)-1)*claimRelayTestBatchGas + gasPerClaim + gasPerClaim/63 + 20_000
	return &snClaimRelayBatch{
		Count: len(claims), To: claimRelayTestBatcher, GasLimit: need + need/5,
		Calldata: stClaimRelayTestBatcherAbi.PackClaimMany(claimRelayTestVault, snClaimRelayItems(claims), new(big.Int).SetUint64(gasPerClaim)),
	}, nil
}

var stClaimRelayTestBatcherAbi = stabi.NewSTClaimBatcher()

// One transaction, final at once, unless sendErr says otherwise. A transaction
// that needs more gas than its limit burns the limit and reverts, as a claim
// whose hotkey list grew after its estimate does.
func (self *claimRelayWorld) Send(_ context.Context, send *snClaimRelaySend) (*snClaimRelayOutcome, error) {
	if self.sendErr != nil {
		if errors.Is(self.sendErr, errSnClaimRelayInFlight) {
			// in flight: signed and sent, but not final, so it may cost its limit
			self.txs = append(self.txs, &claimRelayWorldTx{createTime: self.nowTime, send: send, gasUsed: send.GasLimit,
				costWei: snClaimRelayLiability(send.GasLimit, self.feeCapWei), feeCapWei: self.feeCapWei, outcome: &snClaimRelayOutcome{}})
		}
		return nil, self.sendErr
	}
	outcome := &snClaimRelayOutcome{TxHash: fmt.Sprintf("0x%064x", len(self.txs)+1)}
	need := uint64(0)
	paid := []*snClaimRelayClaim{}
	if send.Batch {
		need = 21_000
	}
	griefed := false
	for _, claim := range send.Claims {
		index := claimRelaySlot(claim.Account)
		used, ok := self.gasUsed[index]
		if !ok {
			used = claimRelayTestUsedGas
			if send.Batch {
				used = claimRelayTestBatchGas
			}
		}
		need += used
		griefed = griefed || snClaimRelayGriefingGas < used
		if self.reverts[index] && !send.Batch {
			outcome.Reverted = true
			outcome.Items = append(outcome.Items, &snClaimRelayItemOutcome{Reason: "reverted"})
			continue
		}
		if selector, refused := self.batchRefusals[index]; refused && send.Batch {
			griefed = griefed || selector == [4]byte{}
			outcome.Items = append(outcome.Items, &snClaimRelayItemOutcome{Reason: fmt.Sprintf("0x%x", selector)})
			continue
		}
		outcome.Items = append(outcome.Items, &snClaimRelayItemOutcome{Paid: true, AmountRao: claim.Account.claimable().ToBig(),
			NoId: claim.Account.NoId, Index: claim.Account.Index, Cumulative: claim.Account.Cumulative.ToBig()})
		paid = append(paid, claim)
	}
	if send.GasLimit < need {
		// out of gas: nothing paid, the whole limit gone
		need = send.GasLimit
		griefed = true
		outcome.Reverted = true
		paid = nil
		for index := range outcome.Items {
			outcome.Items[index] = &snClaimRelayItemOutcome{Reason: "out of gas"}
		}
	}
	for _, claim := range paid {
		self.chainClaimed[claimRelaySlot(claim.Account)] = &claim.Account.Cumulative
		// the mirror follows within seconds
		self.mirrorClaimed[claimRelaySlot(claim.Account)] = &claim.Account.Cumulative
	}
	outcome.GasUsed, outcome.Griefed = need, griefed
	self.txs = append(self.txs, &claimRelayWorldTx{createTime: self.nowTime, send: send, gasUsed: need, costWei: snClaimRelayLiability(need, self.gasPriceWei), griefed: griefed, outcome: outcome, final: true})
	return outcome, nil
}

// snClaimRelayLedger

// Pool 1's accounts, then the capture the flag pool's, as the core ledger reads
// them. A pool that cannot be read is named with the coldkeys its account index
// records, unless the index cannot be read either; no pool with a finalized
// root read is an error, with the read naming the pools that failed. A pool
// has a root once a test gives it an account.
func (self *claimRelayWorld) Accounts(context.Context) (*snClaimRelayLedgerRead, error) {
	read := &snClaimRelayLedgerRead{Accounts: []*snClaimRelayAccount{}}
	pools := []struct {
		noId      uint64
		rootIndex uint64
		leaves    []merkle.LedgerAccount
		rooted    bool
		err       error
	}{{1, 3, self.leaves, 0 < len(self.leaves), errors.Join(self.ledgerErr, self.poolLedgerErr)}, {claimRelayTestCtfPool, 0, self.ctfLeaves, 0 < len(self.ctfLeaves), errors.Join(self.ledgerErr, self.ctfLedgerErr)}}
	rooted := 0
	for _, pool := range pools {
		if pool.err != nil {
			read.Unavailable = append(read.Unavailable, pool.noId)
			if !self.indexUnreadable {
				if read.UnavailableColdkeys == nil {
					read.UnavailableColdkeys = map[[32]byte]bool{}
				}
				for _, leaf := range pool.leaves {
					read.UnavailableColdkeys[leaf.Coldkey] = true
				}
			}
			continue
		}
		if pool.rooted {
			rooted += 1
		}
		for index := range pool.leaves {
			account := &snClaimRelayAccount{NoId: pool.noId, Index: uint64(index), RootIndex: pool.rootIndex, Coldkey: pool.leaves[index].Coldkey, Cumulative: pool.leaves[index].Cumulative}
			if claimed := self.mirrorClaimed[claimRelaySlot(account)]; claimed != nil {
				account.Claimed = *claimed
			}
			read.Accounts = append(read.Accounts, account)
		}
	}
	if 0 < len(read.Unavailable) && rooted == 0 {
		return read, errors.Join(self.ledgerErr, self.poolLedgerErr, self.ctfLedgerErr)
	}
	if self.indexUnreadable {
		read.UnavailableColdkeys = nil
	}
	return read, nil
}

func (self *claimRelayWorld) Proof(_ context.Context, account *snClaimRelayAccount) ([]merkle.LedgerNode, error) {
	if err := self.proofErrs[account.NoId]; err != nil {
		return nil, err
	}
	return self.treeOf(account).Proof(account.Index)
}

// snClaimRelayRequests

func (self *claimRelayWorld) List(_ context.Context, limit int) []*snClaimRelayRequest {
	requests := []*snClaimRelayRequest{}
	for coldkey, at := range self.requests {
		requests = append(requests, &snClaimRelayRequest{Coldkey: coldkey, Time: at})
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Time.Before(requests[j].Time) })
	return requests[:min(limit, len(requests))]
}

func (self *claimRelayWorld) Finish(_ context.Context, coldkey [32]byte, startLimit bool) {
	delete(self.requests, coldkey)
	if _, limited := self.limits[coldkey]; startLimit && !limited {
		self.limits[coldkey] = self.nowTime.Add(snClaimRelayClaimNowPeriod)
	}
}

func (self *claimRelayWorld) Limit(_ context.Context, coldkey [32]byte) {
	if _, limited := self.limits[coldkey]; !limited {
		self.limits[coldkey] = self.nowTime.Add(snClaimRelayClaimNowPeriod)
	}
}

// snClaimRelayClaimRecords, as Redis keeps them: one per account, the last write
// wins.

func (self *claimRelayWorld) Record(_ context.Context, paid *snClaimRelayPaid) {
	self.records[snClaimRelayAccountKey(paid.NoId, paid.Index)] = paid
	self.recorded += 1
}

// A tap, as the api takes it: refused while the coldkey's limit runs, and a
// coldkey already queued keeps its place.
func (self *claimRelayWorld) tap(index int) bool {
	coldkey := claimRelayTestColdkey(index)
	if until, limited := self.limits[coldkey]; limited && self.nowTime.Before(until) {
		return false
	}
	if _, queued := self.requests[coldkey]; !queued {
		self.requests[coldkey] = self.nowTime
	}
	return true
}

func (self *claimRelayWorld) limited(index int) bool {
	until, limited := self.limits[claimRelayTestColdkey(index)]
	return limited && self.nowTime.Before(until)
}

// Inspection

func (self *claimRelayWorld) claimsSent() []*snClaimRelayClaim {
	claims := []*snClaimRelayClaim{}
	for _, tx := range self.txs {
		claims = append(claims, tx.send.Claims...)
	}
	return claims
}

func (self *claimRelayWorld) spentSince(since time.Time, until time.Time) *big.Int {
	spent := new(big.Int)
	for _, tx := range self.txs {
		if !tx.createTime.Before(since) && tx.createTime.Before(until) {
			spent.Add(spent, tx.costWei)
		}
	}
	return spent
}

func requireClaimRelayState(t testing.TB, status *SnClaimRelayStatus, state string) {
	t.Helper()
	if status.State != state {
		t.Fatalf("state %s (%s), want %s", status.State, status.Detail, state)
	}
}

func claimRelayRepeat(amount *uint256.Int, count int) []*uint256.Int {
	amounts := make([]*uint256.Int, count)
	for index := range amounts {
		// distinct, so the order is the balance's and not an accident
		amounts[index] = new(uint256.Int).Add(amount, uint256.NewInt(uint64(count-index)))
	}
	return amounts
}

// With no relayer key the relay does nothing, and says so: no chain client is
// built, so nothing can be read or sent, and the state reaches the status field
// and the metrics.
func TestClaimRelayNoKeySendsNothing(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayTestAlpha(100*snClaimRelayAutomaticTaoRao))
	world.signerErr = errSnClaimRelayNoKey
	world.tap(0)
	status := world.pass()
	requireClaimRelayState(t, status, SnClaimRelayStateNoKey)
	if world.chainBuilt != 0 || len(world.txs) != 0 || len(world.estimated) != 0 {
		t.Fatalf("no key built %d chains, sent %d, estimated %d", world.chainBuilt, len(world.txs), len(world.estimated))
	}
	if world.published == 0 || world.status.State != SnClaimRelayStateNoKey {
		t.Fatalf("the status does not say so: %+v", world.status)
	}
	observeSnClaimRelayPass(status)
	if value := testutil.ToFloat64(snClaimRelayState.WithLabelValues(SnClaimRelayStateNoKey)); value != 1 {
		t.Fatalf("the no_key state metric is %v", value)
	}
	if value := testutil.ToFloat64(snClaimRelayState.WithLabelValues(SnClaimRelayStateRunning)); value != 0 {
		t.Fatalf("the running state metric is %v", value)
	}
}

// A key that cannot be used stops the relay as no key does, and says which.
func TestClaimRelayBadKeySendsNothing(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayTestAlpha(100*snClaimRelayAutomaticTaoRao))
	world.signerErr = errors.New("claim relay: the vault entry's private_key is not a key")
	status := world.pass()
	requireClaimRelayState(t, status, SnClaimRelayStateBadKey)
	if world.chainBuilt != 0 || len(world.txs) != 0 {
		t.Fatalf("a bad key built %d chains and sent %d", world.chainBuilt, len(world.txs))
	}
}

func claimRelayVaultEntry(t testing.TB, entry string) func() {
	t.Helper()
	previous := snClaimRelayVaultBytes
	snClaimRelayVaultBytes = func() ([]byte, error) {
		if entry == "" {
			return nil, errSnClaimRelayNoKey
		}
		return []byte(entry), nil
	}
	return func() { snClaimRelayVaultBytes = previous }
}

// The vault entry: absent or empty is no key, and nothing unusable is used. The
// error never quotes the entry.
func TestClaimRelayKeyFromTheVaultEntry(t *testing.T) {
	const hexKey = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
	key, err := crypto.HexToECDSA(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.PubkeyToAddress(key.PublicKey).Hex()
	rootKey, err := crypto.HexToECDSA(strings.Repeat("3", 64))
	if err != nil {
		t.Fatal(err)
	}
	mainnet := &StConfig{Profile: "mainnet", RootKey: rootKey}
	testnet := &StConfig{Profile: "testnet", RootKey: rootKey}
	for _, c := range []struct {
		name    string
		cfg     *StConfig
		entry   string
		noKey   bool
		refused string
		batcher common.Address
	}{
		{name: "absent", cfg: mainnet, entry: "", noKey: true},
		{name: "empty key", cfg: mainnet, entry: "address: \"\"\nprivate_key: \"\"\n", noKey: true},
		{name: "another profile's key", cfg: mainnet, entry: "testnet-private-key: \"" + hexKey + "\"\n", noKey: true},
		{name: "the other way", cfg: testnet, entry: "private_key: \"" + hexKey + "\"\n", noKey: true},
		{name: "not yaml", cfg: mainnet, entry: "private_key: [" + hexKey, refused: "does not parse"},
		{name: "not a key", cfg: mainnet, entry: "private_key: \"not-hex\"\n", refused: "not a key"},
		{name: "wrong address", cfg: mainnet, entry: "address: \"0x00000000000000000000000000000000000000c2\"\nprivate_key: \"" + hexKey + "\"\n", refused: "not the key of address"},
		{name: "a role key", cfg: mainnet, entry: "private_key: \"" + strings.Repeat("3", 64) + "\"\n", refused: "role keys"},
		{name: "bad batcher", cfg: mainnet, entry: "private_key: \"" + hexKey + "\"\nbatcher: \"nope\"\n", refused: "not an address"},
		{name: "mainnet", cfg: mainnet, entry: "address: \"" + address + "\"\nprivate_key: \"" + hexKey + "\"\n", batcher: common.HexToAddress(snClaimRelayMainnetBatcher)},
		{name: "mainnet with its batcher", cfg: mainnet, entry: "private_key: \"0x" + hexKey + "\"\nbatcher: \"0x00000000000000000000000000000000000000b2\"\n", batcher: claimRelayTestBatcher},
		{name: "testnet names its batcher", cfg: testnet, entry: "testnet-private-key: \"" + hexKey + "\"\ntestnet-batcher: \"0x00000000000000000000000000000000000000b2\"\n", batcher: claimRelayTestBatcher},
		{name: "testnet without one", cfg: testnet, entry: "testnet-private-key: \"" + hexKey + "\"\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer claimRelayVaultEntry(t, c.entry)()
			relayer, err := loadSnClaimRelayKey(c.cfg)
			switch {
			case c.noKey:
				if !errors.Is(err, errSnClaimRelayNoKey) {
					t.Fatalf("err %v, want no key", err)
				}
			case c.refused != "":
				if err == nil || !strings.Contains(err.Error(), c.refused) || errors.Is(err, errSnClaimRelayNoKey) {
					t.Fatalf("err %v, want %q", err, c.refused)
				}
				if strings.Contains(err.Error(), hexKey) || strings.Contains(err.Error(), "not-hex") {
					t.Fatalf("the error quotes the entry: %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if relayer.address.Hex() != address || relayer.batcher != c.batcher {
					t.Fatalf("relayer %s batcher %s", relayer.address, relayer.batcher)
				}
			}
		})
	}
}

// Each claim names the leaf's own coldkey and cumulative, with a proof of them
// against the root, sent to vault2. vault2 pays that coldkey whoever sends it.
func TestClaimRelayClaimsNameTheLeafColdkey(t *testing.T) {
	for _, batching := range []bool{false, true} {
		world := newClaimRelayWorld(t, claimRelayRepeat(claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao), 5)...)
		world.batching = batching
		world.passes()
		if len(world.claimsSent()) != 5 {
			t.Fatalf("batching %t: sent %d claims", batching, len(world.claimsSent()))
		}
		root, total := world.tree.Root()
		seen := map[uint64]bool{}
		for _, tx := range world.txs {
			claims, batch, err := snClaimRelayDecodeCalldata(tx.send.Calldata)
			if err != nil {
				t.Fatal(err)
			}
			if batch != tx.send.Batch || batch && tx.send.To != claimRelayTestBatcher || !batch && tx.send.To != claimRelayTestVault {
				t.Fatalf("a claim goes to %s, batch %t", tx.send.To, batch)
			}
			for _, claim := range claims {
				index := claim.Account.Index.Uint64()
				leaf := world.leaves[index]
				cumulative, _ := uint256.FromBig(claim.Account.Cumulative)
				if claim.Account.Coldkey != leaf.Coldkey || !cumulative.Eq(&leaf.Cumulative) || claim.NoId.Uint64() != 1 || claim.RootIndex.Uint64() != 3 {
					t.Fatalf("claim %d names coldkey %x cumulative %s, the leaf %x %s", index, claim.Account.Coldkey, cumulative, leaf.Coldkey, leaf.Cumulative.Dec())
				}
				proof := make([]merkle.LedgerNode, len(claim.Proof))
				for i, node := range claim.Proof {
					sum, _ := uint256.FromBig(node.Sum)
					proof[i] = merkle.LedgerNode{Hash: node.Hash, Sum: *sum}
				}
				if !merkle.VerifyLedgerProof(root, &total, world.tree.LeafCount(), index, claim.Account.Coldkey, cumulative, proof) {
					t.Fatalf("claim %d's proof does not verify", index)
				}
				seen[index] = true
			}
		}
		if len(seen) != 5 {
			t.Fatalf("claims reached %d accounts", len(seen))
		}
	}
}

// A proof that does not verify is never sent, and the rest go on.
func TestClaimRelayNeverSendsAProofThatDoesNotVerify(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayRepeat(claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao), 3)...)
	world.badProofs[0] = true
	world.passes()
	if sent := world.claimsSent(); len(sent) != 2 || sent[0].Account.Index == 0 || sent[1].Account.Index == 0 {
		t.Fatalf("sent %d claims", len(sent))
	}
	if world.results[SnClaimRelayResultRefused] != 1 {
		t.Fatalf("results %v", world.results)
	}
}

// The threshold is in TAO value: the same α waits at a lower price and goes at a
// higher one, and a balance goes at exactly 0.0125 TAO and not a rao under it.
func TestClaimRelayThresholdIsInTaoValue(t *testing.T) {
	exactly := claimRelayTestAlpha(snClaimRelayAutomaticTaoRao)
	under := new(uint256.Int).Sub(exactly, uint256.NewInt(1_000_000))

	world := newClaimRelayWorld(t, under)
	status := world.passes()
	requireClaimRelayState(t, status, SnClaimRelayStateRunning)
	if len(world.txs) != 0 || status.OverThreshold != 0 || status.ClaimsPending != 0 {
		t.Fatalf("under the threshold: sent %d, over %d, pending %d", len(world.txs), status.OverThreshold, status.ClaimsPending)
	}

	// the same α, when the price rises
	world.alphaPriceWei = new(big.Int).Mul(claimRelayTestAlphaPrice, big.NewInt(2))
	world.passes()
	if len(world.txs) != 1 {
		t.Fatalf("over the threshold at the higher price: sent %d", len(world.txs))
	}

	world = newClaimRelayWorld(t, exactly)
	world.passes()
	if len(world.txs) != 1 {
		t.Fatalf("exactly at the threshold: sent %d", len(world.txs))
	}

	// and α alone says nothing: more α, a lower price
	world = newClaimRelayWorld(t, new(uint256.Int).Mul(exactly, uint256.NewInt(2)))
	world.alphaPriceWei = new(big.Int).Div(claimRelayTestAlphaPrice, big.NewInt(4))
	world.passes()
	if len(world.txs) != 0 {
		t.Fatalf("worth half the threshold: sent %d", len(world.txs))
	}
}

// The mirror of claims can lag. A claim is sized by the chain's own read, and one
// the chain has already paid, or that the read puts under the threshold, is not
// sent.
func TestClaimRelayTheChainNotTheMirrorDecides(t *testing.T) {
	amount := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	world := newClaimRelayWorld(t, amount, amount)
	world.chainClaimed[0] = amount
	world.chainClaimed[1] = new(uint256.Int).Sub(amount, uint256.NewInt(10))
	status := world.passes()
	requireClaimRelayState(t, status, SnClaimRelayStateRunning)
	if len(world.txs) != 0 {
		t.Fatalf("sent %d claims of accounts the chain has paid", len(world.txs))
	}
}

// Accounts go largest first, and every one of them is reached.
func TestClaimRelayLargestFirst(t *testing.T) {
	small, large := claimRelayTestAlpha(snClaimRelayAutomaticTaoRao), claimRelayTestAlpha(20*snClaimRelayAutomaticTaoRao)
	world := newClaimRelayWorld(t, small, large, new(uint256.Int).Add(small, large))
	world.passes()
	sent := world.claimsSent()
	if len(sent) != 3 || sent[0].Account.Index != 2 || sent[1].Account.Index != 1 || sent[2].Account.Index != 0 {
		t.Fatalf("order %v", sent)
	}
}

// The daily cap holds, whatever a claim costs, when claims' gas is close to their
// limit, and at any gas price under the ceiling.
func TestClaimRelayDailyCapHolds(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	for _, c := range []struct {
		name     string
		gwei     int64
		estimate uint64
		used     uint64
		batching bool
		atLeast  int
	}{
		{name: "planning figures", gwei: 5, estimate: claimRelayTestEstimateGas, used: claimRelayTestUsedGas, atLeast: 11},
		{name: "at the ceiling", gwei: 10, estimate: claimRelayTestEstimateGas, used: claimRelayTestUsedGas, atLeast: 5},
		{name: "dear claims", gwei: 10, estimate: 350_000, used: 350_000, atLeast: 2},
		{name: "batched", gwei: 5, estimate: claimRelayTestEstimateGas, used: claimRelayTestBatchGas, batching: true, atLeast: 12},
	} {
		t.Run(c.name, func(t *testing.T) {
			world := newClaimRelayWorld(t, claimRelayRepeat(over, 60)...)
			world.gasPriceWei = claimRelayGwei(c.gwei)
			world.feeCapWei = snClaimRelayFeeCap(claimRelayGwei(c.gwei), new(big.Int))
			world.batching = c.batching
			for index := range world.leaves {
				world.estimates[uint64(index)] = c.estimate
				world.gasUsed[uint64(index)] = c.used
			}
			status := world.passes()
			requireClaimRelayState(t, status, SnClaimRelayStateCapReached)
			dayStart := snClaimRelayDayStart(world.nowTime)
			spent := world.spentSince(dayStart, dayStart.Add(24*time.Hour))
			if 0 < spent.Cmp(snClaimRelayDailyCapWei) {
				t.Fatalf("spent %s, over the cap %s", spent, snClaimRelayDailyCapWei)
			}
			if sent := len(world.claimsSent()); sent < c.atLeast || 60 <= sent {
				t.Fatalf("sent %d claims, want at least %d and some left", sent, c.atLeast)
			}
			// nothing more goes today, however many passes come
			sent := len(world.txs)
			for range 5 {
				requireClaimRelayState(t, world.pass(), SnClaimRelayStateCapReached)
			}
			if len(world.txs) != sent {
				t.Fatalf("sent past the cap: %d more", len(world.txs)-sent)
			}
		})
	}
}

// A transaction whose worst case does not fit what is left of the day is not
// sent, even when its expected cost would.
func TestClaimRelayCapCountsTheWorstCaseOfTheNextClaim(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
	// spent all but what this claim is expected to cost
	expected := snClaimRelayLiability(claimRelayTestUsedGas, world.gasPriceWei)
	world.txs = append(world.txs, &claimRelayWorldTx{createTime: world.nowTime, send: &snClaimRelaySend{},
		costWei: new(big.Int).Sub(snClaimRelayDailyCapWei, expected), outcome: &snClaimRelayOutcome{}, final: true})
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateCapReached)
	if len(world.txs) != 1 {
		t.Fatalf("sent a claim whose limit does not fit the day")
	}
}

// The day's last claim must fit by what its replacements can cost too: one wei short
// of that and it waits for the next day.
func TestClaimRelayCapCountsTheReplacementsOfTheNextClaim(t *testing.T) {
	// 198,000 gas at 5.625 gwei, raised twice by 12.5%
	worst := big.NewInt(1_409_589_843_750_000)
	for _, c := range []struct {
		left  *big.Int
		sends bool
	}{
		{worst, true},
		{new(big.Int).Sub(worst, big.NewInt(1)), false},
		// what the first attempt alone can cost fits, and is not enough
		{snClaimRelayLiability(198_000, big.NewInt(5_625_000_000)), false},
	} {
		world := newClaimRelayWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
		world.txs = append(world.txs, &claimRelayWorldTx{createTime: world.nowTime, send: &snClaimRelaySend{},
			costWei: new(big.Int).Sub(snClaimRelayDailyCapWei, c.left), outcome: &snClaimRelayOutcome{}, final: true})
		status := world.pass()
		if sent := len(world.txs) == 2; sent != c.sends {
			t.Fatalf("with %s wei left: sent %t (%s), want %t", c.left, sent, status.State, c.sends)
		}
		if !c.sends {
			requireClaimRelayState(t, status, SnClaimRelayStateCapReached)
		}
	}
}

// The relay's replacement is the engine's 12.5% bump of its last attempt, never a
// quote that rose since, so a base fee that jumps cannot take a transaction past
// what the cap was checked against. Any other client's replacement follows the
// quote as before.
func TestClaimRelayReplacementIsTheBumpAlone(t *testing.T) {
	fixture := newLedgerV2RPC(t)
	fixture.baseFee = claimRelayGwei(50)
	client, rpcClient, cfg := newLedgerV2RPCClient(t, fixture)
	relayKey, err := crypto.HexToECDSA(claimRelayDbKey)
	if err != nil {
		t.Fatal(err)
	}
	relayer := crypto.PubkeyToAddress(relayKey.PublicKey)
	calldata := claimRelaySingleIntent(claimRelayReceiptClaim(5, 1_000)).Calldata
	feeCap, tip := "5625000000", "0"
	previous := &model.StTransactionAttempt{Attempt: 1, Kind: model.StTxAttemptExecution, GasLimit: 198_000, GasFeeCap: &feeCap, GasTipCap: &tip}
	intent := &model.StTransactionIntent{Calldata: calldata}
	envelope := func(relay bool) *stTransactionEnvelope {
		t.Helper()
		client.claimRelay = nil
		if relay {
			client.claimRelay = newStClaimRelayEngine(relayer)
		}
		result, err := client.transactionEnvelope(context.Background(), rpcClient, intent, relayer, cfg.VaultV2, calldata, 0, previous, model.StTxAttemptExecution, nil)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	relay := envelope(true)
	if relay.legacy || relay.gasLimit != 198_000 || relay.fee.Cmp(big.NewInt(6_328_125_000)) != 0 || relay.tip.Sign() != 0 {
		t.Fatalf("the relay's replacement: gas %d fee %s tip %s", relay.gasLimit, relay.fee, relay.tip)
	}
	operator := envelope(false)
	if operator.fee.Cmp(claimRelayGwei(100)) != 0 {
		t.Fatalf("another client's replacement: fee %s", operator.fee)
	}
	// and two of them stay inside the worst case
	once := relay.fee
	second := stReplacementFee(&[]string{once.String()}[0], nil)
	if most := snClaimRelayWorstCase(198_000, big.NewInt(5_625_000_000)); 0 < snClaimRelayLiability(198_000, second).Cmp(most) {
		t.Fatalf("a second replacement at %s may cost more than %s", second, most)
	}
}

// A restart forgets nothing: spend is read back from what the account sent. The
// same claims and the same cap, with a new relay before every pass.
func TestClaimRelayCapHoldsAcrossARestart(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	straight := newClaimRelayWorld(t, claimRelayRepeat(over, 40)...)
	relay := straight.relay()
	for range 100 {
		if !relay.pass(context.Background()).Again {
			break
		}
	}
	restarted := newClaimRelayWorld(t, claimRelayRepeat(over, 40)...)
	restarted.passes()
	if len(straight.txs) != len(restarted.txs) || len(restarted.txs) == 0 {
		t.Fatalf("a restart changed the claims: %d against %d", len(restarted.txs), len(straight.txs))
	}
	dayStart := snClaimRelayDayStart(restarted.nowTime)
	if spent := restarted.spentSince(dayStart, dayStart.Add(24*time.Hour)); 0 < spent.Cmp(snClaimRelayDailyCapWei) {
		t.Fatalf("spent %s over the cap", spent)
	}
	requireClaimRelayState(t, restarted.relay().pass(context.Background()), SnClaimRelayStateCapReached)
}

// A claim whose outcome was not known when the process stopped counts at the most
// it can cost, and nothing new is sent over it.
func TestClaimRelayInFlightClaimCountsAtItsLimit(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayRepeat(claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao), 3)...)
	world.sendErr = errSnClaimRelayInFlight
	status := world.pass()
	requireClaimRelayState(t, status, SnClaimRelayStateInFlight)
	if status.Again {
		t.Fatal("a pass with a claim in flight asks for another at once")
	}
	if len(world.txs) != 1 {
		t.Fatalf("sent %d", len(world.txs))
	}
	spend, err := world.Spend(context.Background(), snClaimRelayDayStart(world.nowTime))
	if err != nil || spend.SpentWei.Cmp(snClaimRelayLiability(snClaimRelayGasLimit(claimRelayTestEstimateGas), world.feeCapWei)) != 0 {
		t.Fatalf("spent %v (%v)", spend, err)
	}
	// While reconciliation says it is not final, nothing else goes.
	world.sendErr = nil
	world.reconcileErr = errors.New("still not final")
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateInFlight)
	if len(world.txs) != 1 {
		t.Fatalf("sent %d while one was in flight", len(world.txs))
	}
}

// Past the cap, claims wait for the next UTC day and then go. Each day has its own
// cap, and a transaction belongs to the day it was created.
func TestClaimRelayCapHoldsAcrossADayBoundary(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	world := newClaimRelayWorld(t, claimRelayRepeat(over, 40)...)
	world.nowTime = time.Date(2026, 11, 9, 23, 59, 30, 0, time.UTC)
	requireClaimRelayState(t, world.passes(), SnClaimRelayStateCapReached)
	firstDay := len(world.txs)
	if firstDay == 0 {
		t.Fatal("nothing sent on the first day")
	}

	// still the same day to the second
	world.nowTime = time.Date(2026, 11, 9, 23, 59, 59, 999, time.UTC)
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateCapReached)
	if len(world.txs) != firstDay {
		t.Fatalf("sent before the day turned")
	}

	world.nowTime = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	requireClaimRelayState(t, world.passes(), SnClaimRelayStateCapReached)
	if len(world.txs) != 2*firstDay {
		t.Fatalf("the second day sent %d, the first %d", len(world.txs)-firstDay, firstDay)
	}
	for _, day := range []time.Time{time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)} {
		if spent := world.spentSince(day, day.Add(24*time.Hour)); spent.Sign() == 0 || 0 < spent.Cmp(snClaimRelayDailyCapWei) {
			t.Fatalf("day %s spent %s", day, spent)
		}
	}
	// nothing was claimed twice
	seen := map[uint64]bool{}
	for _, claim := range world.claimsSent() {
		if seen[claim.Account.Index] {
			t.Fatalf("account %d claimed twice", claim.Account.Index)
		}
		seen[claim.Account.Index] = true
	}
}

// A day's clock is UTC, whatever zone the process's clock is in.
func TestClaimRelayDayIsUtc(t *testing.T) {
	zone := time.FixedZone("east", 9*60*60)
	if start := snClaimRelayDayStart(time.Date(2026, 11, 10, 8, 0, 0, 0, zone)); !start.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day start %s", start)
	}
}

// A preflight over the limit stops the pass before anything is sent, batched or
// not, and the relay goes on when the estimate comes back.
func TestClaimRelayGriefingEstimateStopsIt(t *testing.T) {
	for _, batching := range []bool{false, true} {
		over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
		world := newClaimRelayWorld(t, claimRelayRepeat(over, 3)...)
		world.batching = batching
		for index := range world.leaves {
			world.estimates[uint64(index)] = claimRelayTestGriefedGas
		}
		status := world.pass()
		requireClaimRelayState(t, status, SnClaimRelayStateGriefingStop)
		if len(world.txs) != 0 {
			t.Fatalf("sent %d at a griefed estimate", len(world.txs))
		}
		if status.LastEstimateGas != claimRelayTestGriefedGas || !strings.Contains(status.Detail, "1260000") {
			t.Fatalf("the status does not say why: %+v", status)
		}
		// a claim among those kept for the batch is enough
		world.estimates[0], world.estimates[1] = claimRelayTestEstimateGas, claimRelayTestEstimateGas
		requireClaimRelayState(t, world.pass(), map[bool]string{false: SnClaimRelayStateRunning, true: SnClaimRelayStateGriefingStop}[batching])
		if batching && len(world.txs) != 0 {
			t.Fatalf("sent %d in a batch with a griefed claim", len(world.txs))
		}
		if !batching && len(world.txs) != 1 {
			t.Fatalf("sent %d", len(world.txs))
		}
		for index := range world.leaves {
			delete(world.estimates, uint64(index))
		}
		world.passes()
		if len(world.claimsSent()) != 3 {
			t.Fatalf("did not go on: sent %d", len(world.claimsSent()))
		}
	}
	// the limit itself passes, one over does not
	world := newClaimRelayWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
	world.estimates[0] = snClaimRelayGriefingGas + 1
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateGriefingStop)
	world.estimates[0] = snClaimRelayGriefingGas
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateRunning)
	if len(world.txs) != 1 {
		t.Fatalf("sent %d at the limit", len(world.txs))
	}
}

// A claim whose estimate was fine and whose gas jumped on chain stops the rest of
// the day, across a restart, and the day after decides by its own estimates.
func TestClaimRelayGriefingReceiptStopsTheDay(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	for _, c := range []struct {
		name string
		used uint64
	}{
		// over the limit it was signed with: the list grew after the estimate, so
		// the claim ran out of gas and burned its limit
		{name: "out of gas", used: claimRelayTestGriefedGas},
		// within a higher limit: paid, at the jumped price
		{name: "paid at a jump", used: snClaimRelayGriefingGas + 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			world := newClaimRelayWorld(t, claimRelayRepeat(over, 4)...)
			// the largest, which goes first
			world.gasUsed[0] = c.used
			if c.name == "paid at a jump" {
				world.estimates[0] = snClaimRelayGriefingGas
			}
			status := world.pass()
			requireClaimRelayState(t, status, SnClaimRelayStateGriefingStop)
			if len(world.txs) != 1 {
				t.Fatalf("sent %d", len(world.txs))
			}
			delete(world.gasUsed, 0)
			delete(world.estimates, 0)
			// the estimates look normal again, but the day has stopped, on a new
			// relay too
			for range 3 {
				requireClaimRelayState(t, world.relay().pass(context.Background()), SnClaimRelayStateGriefingStop)
			}
			if len(world.txs) != 1 {
				t.Fatalf("sent %d after the stop", len(world.txs))
			}
			// the next day goes by its estimates: a high one holds the stop
			world.nowTime = world.nowTime.Add(24 * time.Hour)
			for index := range world.leaves {
				world.estimates[uint64(index)] = claimRelayTestGriefedGas
			}
			requireClaimRelayState(t, world.pass(), SnClaimRelayStateGriefingStop)
			if len(world.txs) != 1 {
				t.Fatalf("sent %d the next day", len(world.txs))
			}
			// and normal ones end it
			world.estimates = map[uint64]uint64{}
			requireClaimRelayState(t, world.passes(), SnClaimRelayStateRunning)
			for index := range world.leaves {
				if world.claimedOf(uint64(index)).IsZero() {
					t.Fatalf("account %d is still unpaid", index)
				}
			}
		})
	}
}

// A griefed batch stops the day too: an item that ran out of its gas says so.
func TestClaimRelayGriefedBatchStopsTheDay(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayRepeat(claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao), 3)...)
	world.batching = true
	world.batchRefusals[0] = [4]byte{}
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateGriefingStop)
	world.batchRefusals = map[uint64][4]byte{}
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateGriefingStop)
	if len(world.txs) != 1 {
		t.Fatalf("sent %d", len(world.txs))
	}
}

// One refused preflight leaves the rest alone, costs nothing, and is tried again
// after a while.
func TestClaimRelayRefusedPreflightLeavesTheRestAlone(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	for _, batching := range []bool{false, true} {
		world := newClaimRelayWorld(t, claimRelayRepeat(over, 4)...)
		world.batching = batching
		// the largest, which is tried first
		world.refusals[0] = errors.New("execution reverted")
		status := world.passes()
		requireClaimRelayState(t, status, SnClaimRelayStateRunning)
		sent := world.claimsSent()
		if len(sent) != 3 {
			t.Fatalf("batching %t: sent %d", batching, len(sent))
		}
		for _, claim := range sent {
			if claim.Account.Index == 0 {
				t.Fatal("sent the refused claim")
			}
		}
		if world.results[SnClaimRelayResultRefused] != 1 || world.results[SnClaimRelayResultPaid] != 3 {
			t.Fatalf("results %v", world.results)
		}
		if world.status.Refusals["1/0"] == nil {
			t.Fatalf("refusals %v", world.status.Refusals)
		}

		// not asked again at once, across a restart
		before := len(world.estimated)
		world.pass()
		if after := len(world.estimated); after != before {
			t.Fatalf("estimated %d more within the hour", after-before)
		}
		// and again after the wait, when it goes
		delete(world.refusals, 0)
		world.nowTime = world.nowTime.Add(snClaimRelayRefusalRetry)
		world.passes()
		if len(world.claimsSent()) != 4 {
			t.Fatalf("batching %t: sent %d after the wait", batching, len(world.claimsSent()))
		}
	}
}

// A claim that reverts on chain leaves the rest alone and waits for the next UTC
// day, across a restart: it passed its preflight and it cost gas.
func TestClaimRelayRevertedClaimWaitsForTheNextDay(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayRepeat(claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao), 3)...)
	world.reverts[0] = true
	world.passes()
	sent := world.claimsSent()
	if len(sent) != 3 {
		t.Fatalf("sent %d", len(sent))
	}
	if world.results[SnClaimRelayResultRefused] != 1 || world.results[SnClaimRelayResultPaid] != 2 {
		t.Fatalf("results %v", world.results)
	}
	if world.status.Refusals["1/0"] == nil {
		t.Fatalf("refusals %v", world.status.Refusals)
	}
	world.status = nil // a lost status: the intents still say it reverted
	world.pass()
	if len(world.claimsSent()) != 3 {
		t.Fatalf("sent %d the same day", len(world.claimsSent()))
	}
	delete(world.reverts, 0)
	world.nowTime = world.nowTime.Add(24 * time.Hour)
	world.passes()
	if sent := world.claimsSent(); len(sent) != 4 || sent[3].Account.Index != 0 {
		t.Fatalf("sent %v the next day", sent)
	}
}

// One item a batch refuses leaves the batch's others paid.
func TestClaimRelayRefusedBatchItemLeavesTheRestAlone(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayRepeat(claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao), 4)...)
	world.batching = true
	world.batchRefusals[1] = [4]byte{0xde, 0xad, 0xbe, 0xef}
	status := world.passes()
	requireClaimRelayState(t, status, SnClaimRelayStateRunning)
	if world.results[SnClaimRelayResultPaid] != 3 || world.results[SnClaimRelayResultRefused] != 1 {
		t.Fatalf("results %v", world.results)
	}
	if !world.claimedOf(1).IsZero() || world.claimedOf(0).IsZero() || world.claimedOf(2).IsZero() || world.claimedOf(3).IsZero() {
		t.Fatal("the batch paid the wrong accounts")
	}
}

// A batch is sized by the most it can cost, against the day and the balance. It
// shrinks to what fits, and alone is the last resort.
func TestClaimRelayBatchFitsTheFunds(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	world := newClaimRelayWorld(t, claimRelayRepeat(over, 32)...)
	world.batching = true
	world.pass()
	if len(world.txs) != 1 || !world.txs[0].send.Batch {
		t.Fatalf("first transaction %+v", world.txs)
	}
	first := world.txs[0].send
	liability := snClaimRelayWorstCase(first.GasLimit, world.feeCapWei)
	if 0 < liability.Cmp(snClaimRelayDailyCapWei) {
		t.Fatalf("a batch of %d may cost %s, over the cap", len(first.Claims), liability)
	}
	if len(first.Claims) < 2 || 32 <= len(first.Claims) {
		t.Fatalf("a batch of %d", len(first.Claims))
	}

	// a balance that covers one claim sends one claim
	world = newClaimRelayWorld(t, claimRelayRepeat(over, 3)...)
	world.batching = true
	world.balanceWei = snClaimRelayWorstCase(snClaimRelayGasLimit(claimRelayTestEstimateGas), world.feeCapWei)
	world.pass()
	if len(world.txs) != 1 || world.txs[0].send.Batch {
		t.Fatalf("transactions %+v", world.txs[0].send)
	}

	// a batch that cannot be sized goes alone
	world = newClaimRelayWorld(t, claimRelayRepeat(over, 3)...)
	world.batching = true
	world.batchErr = errors.New("the batcher is unavailable")
	world.passes()
	if len(world.txs) != 3 {
		t.Fatalf("sent %d alone", len(world.txs))
	}
}

// Without a batcher a pass estimates no more than the claim it sends.
func TestClaimRelayEstimatesOnlyWhatItCanSend(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayRepeat(claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao), 10)...)
	world.pass()
	if len(world.estimated) != 1 {
		t.Fatalf("estimated %d for one claim", len(world.estimated))
	}
}

// A pass verifies a bounded number of accounts, however far the mirror is behind, and
// says more is due.
func TestClaimRelayVerifiesABoundedNumberInAPass(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	world := newClaimRelayWorld(t, claimRelayRepeat(over, snClaimRelayPreflightsPerPass+10)...)
	// the chain has paid every one of them; the mirror does not know yet
	for index := range world.leaves {
		world.chainClaimed[uint64(index)] = &world.leaves[index].Cumulative
	}
	reads := 0
	counting := &claimRelayCountingChain{claimRelayWorld: world, reads: &reads}
	relay := world.relay()
	relay.chain = func(*snClaimRelaySigner) (snClaimRelayChain, error) { return counting, nil }
	status := relay.pass(context.Background())
	requireClaimRelayState(t, status, SnClaimRelayStateRunning)
	if reads != snClaimRelayPreflightsPerPass {
		t.Fatalf("read %d in one pass", reads)
	}
	if len(world.txs) != 0 {
		t.Fatalf("sent %d", len(world.txs))
	}
}

type claimRelayCountingChain struct {
	*claimRelayWorld
	reads *int
}

func (self *claimRelayCountingChain) Claimed(ctx context.Context, account *snClaimRelayAccount) (*uint256.Int, error) {
	*self.reads += 1
	return self.claimRelayWorld.Claimed(ctx, account)
}

// Over the ceiling nothing goes, and the claims wait.
func TestClaimRelayGasPriceCeiling(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
	world.gasPriceWei = new(big.Int).Add(snClaimRelayGasPriceCeilingWei, big.NewInt(1))
	world.feeCapWei = world.gasPriceWei
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateGasPriceHigh)
	if len(world.txs) != 0 {
		t.Fatalf("sent %d over the ceiling", len(world.txs))
	}
	world.gasPriceWei = snClaimRelayGasPriceCeilingWei
	world.feeCapWei = world.gasPriceWei
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateRunning)
	if len(world.txs) != 1 {
		t.Fatalf("sent %d at the ceiling", len(world.txs))
	}
}

// A balance short of the next claim's limit sends nothing, and says so even when
// nothing is due, so the apps do not offer a tap that cannot be honored.
func TestClaimRelayLowBalance(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
	// the claim's limit at its fee cap, through both replacements
	needed := big.NewInt(1_409_589_843_750_000)
	if worst := snClaimRelayWorstCase(snClaimRelayGasLimit(claimRelayTestEstimateGas), world.feeCapWei); worst.Cmp(needed) != 0 {
		t.Fatalf("worst case %s", worst)
	}
	world.balanceWei = new(big.Int).Sub(needed, big.NewInt(1))
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateLowBalance)
	if len(world.txs) != 0 {
		t.Fatal("sent with too little")
	}
	world.balanceWei = needed
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateRunning)
	if len(world.txs) != 1 {
		t.Fatal("did not send with enough")
	}

	world = newClaimRelayWorld(t)
	world.balanceWei = new(big.Int)
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateLowBalance)
}

// Any chain or ledger read failing sends nothing, and the state says which.
func TestClaimRelayFailedReadsFailClosed(t *testing.T) {
	failure := errors.New("read failed")
	for _, c := range []struct {
		name  string
		fail  func(*claimRelayWorld)
		state string
	}{
		{"not configured", func(world *claimRelayWorld) { world.chainErr = failure }, SnClaimRelayStateNotConfigured},
		{"reconcile", func(world *claimRelayWorld) { world.reconcileErr = failure }, SnClaimRelayStateInFlight},
		{"facts", func(world *claimRelayWorld) { world.factsErr = failure }, SnClaimRelayStateChainUnavailable},
		{"spend", func(world *claimRelayWorld) { world.spendErr = failure }, SnClaimRelayStateChainUnavailable},
		{"ledger", func(world *claimRelayWorld) { world.ledgerErr = failure }, SnClaimRelayStateLedgerUnavailable},
		{"claimed", func(world *claimRelayWorld) { world.claimedErr = failure }, SnClaimRelayStateChainUnavailable},
	} {
		world := newClaimRelayWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
		c.fail(world)
		requireClaimRelayState(t, world.pass(), c.state)
		if len(world.txs) != 0 {
			t.Fatalf("%s: sent %d", c.name, len(world.txs))
		}
	}
	world := newClaimRelayWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
	world.sendErr = failure
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateSendFailed)
}

// Nothing to claim, no root yet: a pass is quiet and running.
func TestClaimRelayNothingDue(t *testing.T) {
	world := newClaimRelayWorld(t)
	status := world.pass()
	requireClaimRelayState(t, status, SnClaimRelayStateRunning)
	if status.Again || len(world.estimated) != 0 {
		t.Fatalf("status %+v, estimated %d", status, len(world.estimated))
	}
}

// A banned coldkey gets no claim of ours, automatic or tapped.
func TestClaimRelayBannedColdkeySendsNothing(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	world := newClaimRelayWorld(t, over, over)
	world.banned[claimRelayTestColdkey(0)] = true
	world.tap(0)
	world.passes()
	if sent := world.claimsSent(); len(sent) != 1 || sent[0].Account.Index != 1 {
		t.Fatalf("sent %d", len(sent))
	}
	if _, queued := world.requests[claimRelayTestColdkey(0)]; queued || world.limited(0) {
		t.Fatal("the banned coldkey's request stayed queued, or used up its limit")
	}
}

// Claim now sends a balance under the automatic threshold once it clears the floor,
// before the larger balances that do not need a tap, and the limit stays used.
func TestClaimRelayClaimNowGoesFirst(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayTestAlpha(10*snClaimRelayFloorTaoRao), claimRelayTestAlpha(100*snClaimRelayAutomaticTaoRao))
	status := world.pass()
	if sent := world.claimsSent(); len(sent) != 1 || sent[0].Account.Index != 1 {
		t.Fatalf("before the tap: %v", sent)
	}
	if status.ClaimsPending != 1 {
		t.Fatalf("pending %d", status.ClaimsPending)
	}
	if !world.tap(0) {
		t.Fatal("the first tap was refused")
	}
	world.passes()
	sent := world.claimsSent()
	if len(sent) != 2 || sent[1].Account.Index != 0 || !sent[1].Requested {
		t.Fatalf("after the tap: %v", sent)
	}
	coldkey := claimRelayTestColdkey(0)
	if _, queued := world.requests[coldkey]; queued {
		t.Fatal("a paid request stayed queued")
	}
	if !world.limited(0) {
		t.Fatal("a paid tap under the threshold did not start the limit")
	}
	if world.tap(0) {
		t.Fatal("tapped again within the limit")
	}
	world.nowTime = world.nowTime.Add(snClaimRelayClaimNowPeriod)
	if !world.tap(0) {
		t.Fatal("the limit did not end after its period")
	}
}

// A tap on a balance the relay would claim by itself moves it ahead and uses
// nothing up.
func TestClaimRelayClaimNowOverTheThresholdUsesNoLimit(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayTestAlpha(100*snClaimRelayAutomaticTaoRao), claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
	world.tap(1)
	world.pass()
	if sent := world.claimsSent(); len(sent) != 1 || sent[0].Account.Index != 1 || !sent[0].Requested || sent[0].UnderThreshold {
		t.Fatalf("sent %+v", sent)
	}
	if _, queued := world.requests[claimRelayTestColdkey(1)]; queued || world.limited(1) {
		t.Fatalf("queued %t limited %t", queued, world.limited(1))
	}
}

// A tapped claim goes ahead of a larger automatic one when both wait.
func TestClaimRelayClaimNowIsAheadOfTheThreshold(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayTestAlpha(100*snClaimRelayAutomaticTaoRao), claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
	world.tap(1)
	world.pass()
	if sent := world.claimsSent(); len(sent) != 1 || sent[0].Account.Index != 1 {
		t.Fatalf("sent %v", sent)
	}
}

// Taps go oldest first.
func TestClaimRelayClaimNowOldestFirst(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayRepeat(claimRelayTestAlpha(10*snClaimRelayFloorTaoRao), 3)...)
	world.tap(2)
	world.nowTime = world.nowTime.Add(time.Second)
	world.tap(0)
	world.nowTime = world.nowTime.Add(time.Second)
	world.tap(1)
	world.passes()
	sent := world.claimsSent()
	if len(sent) != 3 || sent[0].Account.Index != 2 || sent[1].Account.Index != 0 || sent[2].Account.Index != 1 {
		t.Fatalf("sent %v", sent)
	}
}

// Claim now shares the day's cap: past it a tapped claim waits for the next day,
// and goes first then.
func TestClaimRelayClaimNowWaitsForTheCap(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	amounts := append(claimRelayRepeat(over, 30), claimRelayTestAlpha(10*snClaimRelayFloorTaoRao))
	world := newClaimRelayWorld(t, amounts...)
	requireClaimRelayState(t, world.passes(), SnClaimRelayStateCapReached)
	sent := len(world.claimsSent())
	world.tap(30)
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateCapReached)
	if len(world.claimsSent()) != sent {
		t.Fatal("a tap went past the cap")
	}
	if _, queued := world.requests[claimRelayTestColdkey(30)]; !queued {
		t.Fatal("the tap was dropped at the cap")
	}
	world.nowTime = world.nowTime.Add(24 * time.Hour)
	world.pass()
	if claims := world.claimsSent(); claims[sent].Account.Index != 30 {
		t.Fatalf("the next day's first claim is %d", claims[sent].Account.Index)
	}
}

// A claim under the floor reverts, so none is ever sent, whatever the tap said
// when it was made. The tap is given back: nothing was sent for it.
func TestClaimRelayNeverSendsBelowTheFloor(t *testing.T) {
	amount := claimRelayTestAlpha(2 * snClaimRelayFloorTaoRao)
	world := newClaimRelayWorld(t, amount)
	world.tap(0)
	// the price halves, and a little more, after the tap
	world.alphaPriceWei = new(big.Int).Div(claimRelayTestAlphaPrice, big.NewInt(3))
	status := world.pass()
	requireClaimRelayState(t, status, SnClaimRelayStateRunning)
	coldkey := claimRelayTestColdkey(0)
	if len(world.txs) != 0 || len(world.estimated) != 0 {
		t.Fatalf("sent %d under the floor, estimated %d", len(world.txs), len(world.estimated))
	}
	if _, queued := world.requests[coldkey]; queued {
		t.Fatal("the request stayed queued")
	}
	if world.limited(0) {
		t.Fatal("a tap with nothing sent used up the limit")
	}
	// so it can be tapped again
	world.alphaPriceWei = claimRelayTestAlphaPrice
	if !world.tap(0) {
		t.Fatal("the tap was used up by a claim never sent")
	}
	world.passes()
	if len(world.txs) != 1 {
		t.Fatalf("sent %d", len(world.txs))
	}
}

// The chain's read decides the floor too: a request whose claim the chain's read
// puts under it is given back, not sent.
func TestClaimRelayFloorHoldsOnTheChainsOwnRead(t *testing.T) {
	amount := claimRelayTestAlpha(2 * snClaimRelayFloorTaoRao)
	world := newClaimRelayWorld(t, amount)
	world.chainClaimed[0] = new(uint256.Int).Sub(amount, claimRelayTestAlpha(snClaimRelayFloorTaoRao/2))
	world.tap(0)
	world.pass()
	coldkey := claimRelayTestColdkey(0)
	if _, queued := world.requests[coldkey]; len(world.txs) != 0 || queued || world.limited(0) {
		t.Fatalf("sent %d, queued %t, limited %t", len(world.txs), queued, world.limited(0))
	}
}

// A request for an account someone else has since paid is done, and uses nothing up.
func TestClaimRelayClaimNowOfAPaidAccount(t *testing.T) {
	amount := claimRelayTestAlpha(2 * snClaimRelayFloorTaoRao)
	world := newClaimRelayWorld(t, amount)
	world.tap(0)
	world.chainClaimed[0] = amount
	world.pass()
	coldkey := claimRelayTestColdkey(0)
	if len(world.txs) != 0 {
		t.Fatalf("sent %d", len(world.txs))
	}
	if _, queued := world.requests[coldkey]; queued {
		t.Fatal("the request stayed queued")
	}
	if world.limited(0) {
		t.Fatal("a claim someone else paid used up the tap")
	}
}

// A request for a coldkey no root holds is dropped, and uses nothing up.
func TestClaimRelayClaimNowOfNoAccount(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayTestAlpha(2*snClaimRelayFloorTaoRao))
	unknown := claimRelayTestColdkey(9)
	world.requests[unknown] = world.nowTime
	world.pass()
	if _, queued := world.requests[unknown]; queued || world.limited(9) {
		t.Fatalf("queued %t limited %t", queued, world.limited(9))
	}
}

// claimRelayLedgerEntry is the ledger's answer for one account.
func claimRelayLedgerEntry(cumulative *uint256.Int, claimed *uint256.Int, pending *uint256.Int) *StLedgerV2AccountEntry {
	rootIndex := uint64(3)
	entry := &StLedgerV2AccountEntry{NoId: 1, Index: 7, RootIndex: &rootIndex, Cumulative: cumulative.Dec(), Claimed: claimed.Dec()}
	if pending != nil {
		text := pending.Dec()
		entry.PendingCumulative = &text
	}
	return entry
}

// What the api says about one balance, for every state of the relay.
func TestClaimRelayAccountViewSaysWhy(t *testing.T) {
	now := claimRelayTestMonday
	underThreshold := claimRelayTestAlpha(snClaimRelayAutomaticTaoRao / 2)
	overThreshold := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	underFloor := claimRelayTestAlpha(snClaimRelayFloorTaoRao / 2)
	zero := new(uint256.Int)
	limitUntil := now.Add(10 * 24 * time.Hour)
	refused := map[string]*SnClaimRelayRefusal{"1/7": {Time: now, Until: now.Add(time.Hour), Reason: "refused"}}
	expired := map[string]*SnClaimRelayRefusal{"1/7": {Time: now.Add(-2 * time.Hour), Until: now.Add(-time.Hour), Reason: "refused"}}
	for _, c := range []struct {
		name      string
		state     string
		balance   *uint256.Int
		requested bool
		limit     time.Time
		refusals  map[string]*SnClaimRelayRefusal
		reason    string
		available bool
		nowReason string
	}{
		{name: "nothing", state: SnClaimRelayStateRunning, balance: zero, reason: SnClaimRelayReasonNothingPending, nowReason: SnClaimRelayReasonNothingPending},
		{name: "under the threshold", state: SnClaimRelayStateRunning, balance: underThreshold, reason: SnClaimRelayReasonUnderThreshold, available: true},
		{name: "under the floor", state: SnClaimRelayStateRunning, balance: underFloor, reason: SnClaimRelayReasonUnderThreshold, nowReason: SnClaimRelayClaimNowUnderFloor},
		{name: "over the threshold", state: SnClaimRelayStateRunning, balance: overThreshold, reason: SnClaimRelayReasonQueued, available: true},
		{name: "requested", state: SnClaimRelayStateRunning, balance: underThreshold, requested: true, limit: limitUntil, reason: SnClaimRelayReasonQueued, nowReason: SnClaimRelayClaimNowRequested},
		{name: "limit", state: SnClaimRelayStateRunning, balance: underThreshold, limit: limitUntil, reason: SnClaimRelayReasonUnderThreshold, nowReason: SnClaimRelayClaimNowLimit},
		{name: "limit over", state: SnClaimRelayStateRunning, balance: underThreshold, limit: now, reason: SnClaimRelayReasonUnderThreshold, available: true},
		{name: "cap", state: SnClaimRelayStateCapReached, balance: overThreshold, reason: SnClaimRelayReasonCapReached, available: true},
		{name: "cap, under the threshold", state: SnClaimRelayStateCapReached, balance: underThreshold, reason: SnClaimRelayReasonUnderThreshold, available: true},
		{name: "in flight", state: SnClaimRelayStateInFlight, balance: overThreshold, reason: SnClaimRelayReasonQueued, available: true},
		{name: "refused", state: SnClaimRelayStateRunning, balance: overThreshold, refusals: refused, reason: SnClaimRelayReasonRefused, nowReason: SnClaimRelayReasonRefused},
		{name: "refusal over", state: SnClaimRelayStateRunning, balance: overThreshold, refusals: expired, reason: SnClaimRelayReasonQueued, available: true},
		{name: "no key", state: SnClaimRelayStateNoKey, balance: overThreshold, reason: SnClaimRelayReasonRelayNotRunning, nowReason: SnClaimRelayReasonRelayNotRunning},
		{name: "bad key", state: SnClaimRelayStateBadKey, balance: overThreshold, reason: SnClaimRelayReasonRelayNotRunning, nowReason: SnClaimRelayReasonRelayNotRunning},
		{name: "no pass", state: SnClaimRelayStateNotRunning, balance: overThreshold, reason: SnClaimRelayReasonRelayNotRunning, nowReason: SnClaimRelayReasonRelayNotRunning},
		{name: "griefing", state: SnClaimRelayStateGriefingStop, balance: overThreshold, reason: SnClaimRelayReasonRelayPaused, nowReason: SnClaimRelayReasonRelayPaused},
		{name: "gas", state: SnClaimRelayStateGasPriceHigh, balance: overThreshold, reason: SnClaimRelayReasonRelayPaused, nowReason: SnClaimRelayReasonRelayPaused},
		{name: "low balance", state: SnClaimRelayStateLowBalance, balance: overThreshold, reason: SnClaimRelayReasonRelayPaused, nowReason: SnClaimRelayReasonRelayPaused},
		{name: "chain", state: SnClaimRelayStateChainUnavailable, balance: overThreshold, reason: SnClaimRelayReasonRelayPaused, nowReason: SnClaimRelayReasonRelayPaused},
	} {
		t.Run(c.name, func(t *testing.T) {
			status := &SnClaimRelayStatus{State: c.state, PassTime: now, AlphaPriceWei: claimRelayTestAlphaPrice.String(), Refusals: c.refusals}
			account, err := snClaimRelayAccountView(status, "ss58", claimRelayLedgerEntry(c.balance, zero, nil), c.requested, false, c.limit, nil, now)
			if err != nil {
				t.Fatal(err)
			}
			if account.Reason != c.reason || account.ClaimNowAvailable != c.available || account.ClaimNowReason != c.nowReason {
				t.Fatalf("reason %q available %t now %q, want %q %t %q", account.Reason, account.ClaimNowAvailable, account.ClaimNowReason, c.reason, c.available, c.nowReason)
			}
			if account.ClaimableRao != int64(c.balance.Uint64()) {
				t.Fatalf("claimable %d", account.ClaimableRao)
			}
			if now.Before(c.limit) && account.ClaimNowNextAtMillis != c.limit.UnixMilli() {
				t.Fatalf("next at %d, want %d", account.ClaimNowNextAtMillis, c.limit.UnixMilli())
			}
		})
	}
}

// A balance's amounts: what a claim would pay, its value, and what is still in a
// root that is not final.
func TestClaimRelayAccountViewAmounts(t *testing.T) {
	now := claimRelayTestMonday
	status := &SnClaimRelayStatus{State: SnClaimRelayStateRunning, PassTime: now, AlphaPriceWei: claimRelayTestAlphaPrice.String()}
	cumulative, claimed, pending := uint256.NewInt(10_000_000_000), uint256.NewInt(4_000_000_000), uint256.NewInt(12_000_000_000)
	account, err := snClaimRelayAccountView(status, "ss58", claimRelayLedgerEntry(cumulative, claimed, pending), false, false, time.Time{}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if account.ClaimableRao != 6_000_000_000 || account.PendingRao != 2_000_000_000 || account.ValueTaoRao != 25_344_000 || account.Index != 7 || account.NoId != 1 {
		t.Fatalf("account %+v", account)
	}
	if account.Reason != SnClaimRelayReasonQueued {
		t.Fatalf("reason %s", account.Reason)
	}
	// an account only in a root that is not final has nothing to claim yet
	rootless := &StLedgerV2AccountEntry{NoId: 1, Index: 7, Cumulative: "0", Claimed: "0"}
	text := "5"
	rootless.PendingCumulative = &text
	account, err = snClaimRelayAccountView(status, "ss58", rootless, false, false, time.Time{}, nil, now)
	if err != nil || account.Reason != SnClaimRelayReasonNothingPending || account.PendingRao != 5 {
		t.Fatalf("account %+v (%v)", account, err)
	}
}

// The api names the relay's claim while it is the account's latest: vault2's
// claimed is still the cumulative that claim paid up to. A later claim by anyone,
// a record that does not read, or none, names nothing.
func TestClaimRelayAccountViewNamesTheRelaysClaim(t *testing.T) {
	now := claimRelayTestMonday
	status := &SnClaimRelayStatus{State: SnClaimRelayStateRunning, PassTime: now, AlphaPriceWei: claimRelayTestAlphaPrice.String()}
	hash := "0x" + strings.Repeat("ab", 32)
	record := &model.SnClaimRelayClaim{TxHash: hash, Cumulative: "4000000000", AmountRao: "3000000000"}
	view := func(cumulative uint64, claimed uint64, record *model.SnClaimRelayClaim) *SnClaimRelayAccount {
		t.Helper()
		account, err := snClaimRelayAccountView(status, "ss58", claimRelayLedgerEntry(uint256.NewInt(cumulative), uint256.NewInt(claimed), nil), false, false, time.Time{}, record, now)
		if err != nil {
			t.Fatal(err)
		}
		return account
	}

	// claimed up to it: the claimed state, with what it paid
	account := view(4_000_000_000, 4_000_000_000, record)
	if account.ClaimTxHash != hash || account.ClaimAmountRao != 3_000_000_000 || account.ClaimableRao != 0 || account.Reason != SnClaimRelayReasonNothingPending {
		t.Fatalf("claimed %+v", account)
	}
	// a root since adds a balance: the claim is still the latest one
	account = view(10_000_000_000, 4_000_000_000, record)
	if account.ClaimTxHash != hash || account.ClaimAmountRao != 3_000_000_000 || account.ClaimableRao != 6_000_000_000 {
		t.Fatalf("after a new root %+v", account)
	}
	// someone claimed after it, with the gas key or another relayer
	if account = view(10_000_000_000, 10_000_000_000, record); account.ClaimTxHash != "" || account.ClaimAmountRao != 0 {
		t.Fatalf("after a later claim %+v", account)
	}
	// none, and records that do not read
	for _, c := range []struct {
		name   string
		record *model.SnClaimRelayClaim
	}{
		{"none", nil},
		{"uppercase hash", &model.SnClaimRelayClaim{TxHash: "0x" + strings.Repeat("AB", 32), Cumulative: "4000000000", AmountRao: "3000000000"}},
		{"short hash", &model.SnClaimRelayClaim{TxHash: "0xabcd", Cumulative: "4000000000", AmountRao: "3000000000"}},
		{"no 0x", &model.SnClaimRelayClaim{TxHash: strings.Repeat("ab", 33), Cumulative: "4000000000", AmountRao: "3000000000"}},
		{"cumulative", &model.SnClaimRelayClaim{TxHash: hash, Cumulative: "4e9", AmountRao: "3000000000"}},
		{"amount", &model.SnClaimRelayClaim{TxHash: hash, Cumulative: "4000000000", AmountRao: "-1"}},
	} {
		if account := view(4_000_000_000, 4_000_000_000, c.record); account.ClaimTxHash != "" || account.ClaimAmountRao != 0 {
			t.Fatalf("%s: %+v", c.name, account)
		}
	}
	// an account never claimed names nothing, whatever a record says
	if account = view(4_000_000_000, 0, &model.SnClaimRelayClaim{TxHash: hash, Cumulative: "0", AmountRao: "0"}); account.ClaimTxHash != "" {
		t.Fatalf("never claimed %+v", account)
	}
	// an amount past int64 saturates rather than wraps
	huge := new(uint256.Int).Lsh(uint256.NewInt(1), 70)
	account = view(4_000_000_000, 4_000_000_000, &model.SnClaimRelayClaim{TxHash: hash, Cumulative: "4000000000", AmountRao: huge.Dec()})
	if account.ClaimTxHash != hash || account.ClaimAmountRao != math.MaxInt64 {
		t.Fatalf("huge amount %+v", account)
	}
}

// Each claim the relay paid is recorded from its receipt, the latest of each
// account in place of the one before, and nothing for a claim that did not pay
// or was not sent.
func TestClaimRelayRecordsWhatItPaid(t *testing.T) {
	over := claimRelayTestAlpha(2 * snClaimRelayAutomaticTaoRao)
	under := claimRelayTestAlpha(snClaimRelayAutomaticTaoRao / 2)
	world := newClaimRelayWorld(t, over, under, over)
	world.reverts[2] = true
	world.passes()
	sentFor := func(index uint64) *claimRelayWorldTx {
		t.Helper()
		var found *claimRelayWorldTx
		for _, tx := range world.txs {
			for _, claim := range tx.send.Claims {
				if claim.Account.Index == index {
					found = tx
				}
			}
		}
		if found == nil {
			t.Fatalf("account %d was not sent", index)
		}
		return found
	}
	first := sentFor(0)
	sentFor(2)
	record := world.records["1/0"]
	if record == nil || record.TxHash != first.outcome.TxHash || record.Cumulative.Cmp(over.ToBig()) != 0 || record.AmountRao.Cmp(over.ToBig()) != 0 {
		t.Fatalf("record %+v, want %s", record, first.outcome.TxHash)
	}
	// under the threshold and not sent, and sent and reverted
	if world.records["1/1"] != nil || world.records["1/2"] != nil || len(world.records) != 1 {
		t.Fatalf("records %v", world.records)
	}

	// a new root raises account 0, and the relay claims it again: its record is
	// the new claim's
	twice := new(uint256.Int).Add(over, over)
	world.setLeaves(twice, under, over)
	world.passes()
	if second := sentFor(0); second == first {
		t.Fatal("account 0 was not claimed again")
	}
	record = world.records["1/0"]
	if record == nil || record.TxHash != sentFor(0).outcome.TxHash || record.Cumulative.Cmp(twice.ToBig()) != 0 || record.AmountRao.Cmp(over.ToBig()) != 0 {
		t.Fatalf("record after the second claim %+v", record)
	}
}

// A claim whose outcome was only final after midnight is still recorded, since
// paid claims are read from the day before on, without counting toward the new
// day. One from before that is not read again.
func TestClaimRelayRecordsAClaimFinalAfterMidnight(t *testing.T) {
	world := newClaimRelayWorld(t, claimRelayTestAlpha(2*snClaimRelayAutomaticTaoRao))
	world.nowTime = time.Date(2026, 11, 9, 23, 59, 0, 0, time.UTC)
	world.sendErr = errSnClaimRelayInFlight
	requireClaimRelayState(t, world.pass(), SnClaimRelayStateInFlight)
	if len(world.txs) != 1 || len(world.records) != 0 {
		t.Fatalf("sent %d, records %v", len(world.txs), world.records)
	}

	// the next pass, after midnight, finds it final
	tx := world.txs[0]
	claim := tx.send.Claims[0]
	tx.final = true
	tx.costWei = snClaimRelayLiability(claimRelayTestUsedGas, world.gasPriceWei)
	tx.outcome = &snClaimRelayOutcome{TxHash: "0x" + strings.Repeat("cd", 32), GasUsed: claimRelayTestUsedGas, Items: []*snClaimRelayItemOutcome{
		{Paid: true, AmountRao: claim.Account.claimable().ToBig(), NoId: claim.Account.NoId, Index: claim.Account.Index, Cumulative: claim.Account.Cumulative.ToBig()},
	}}
	world.chainClaimed[0] = &claim.Account.Cumulative
	world.mirrorClaimed[0] = &claim.Account.Cumulative
	world.sendErr = nil
	world.nowTime = time.Date(2026, 11, 10, 0, 5, 0, 0, time.UTC)
	status := world.pass()
	if record := world.records["1/0"]; record == nil || record.TxHash != tx.outcome.TxHash || record.Cumulative.Cmp(claim.Account.Cumulative.ToBig()) != 0 {
		t.Fatalf("record %+v", record)
	}
	// it belongs to the day it was created
	if status.ClaimsPaidToday != 0 || status.SpentTodayWei != "0" || len(world.txs) != 1 {
		t.Fatalf("today paid %d, spent %s, sent %d", status.ClaimsPaidToday, status.SpentTodayWei, len(world.txs))
	}

	// two days on, it is not read again
	delete(world.records, "1/0")
	world.nowTime = time.Date(2026, 11, 11, 0, 5, 0, 0, time.UTC)
	world.pass()
	if len(world.records) != 0 {
		t.Fatalf("records %v", world.records)
	}
}

// What one intent adds: today's spend, count and refusals, and the paid claims of
// the day before too, the latest of each account only, newest first.
func TestClaimRelaySpendAddsTheDayAndThePaidClaims(t *testing.T) {
	price := "5000000000"
	attempts := []*model.StTransactionAttempt{{GasLimit: 200_000, GasPrice: &price}}
	most := snClaimRelayMostAttemptCost(attempts)
	intent := func(status string) *model.StTransactionIntent {
		return &model.StTransactionIntent{Status: status}
	}
	paidItem := func(index uint64, cumulative int64, amount int64) *snClaimRelayItemOutcome {
		return &snClaimRelayItemOutcome{Paid: true, NoId: 1, Index: index, Cumulative: big.NewInt(cumulative), AmountRao: big.NewInt(amount)}
	}
	receipt := func(spent int64, hash string, accounts []string, items ...*snClaimRelayItemOutcome) *snClaimRelayReceipt {
		return &snClaimRelayReceipt{spentWei: big.NewInt(spent), accounts: accounts, outcome: &snClaimRelayOutcome{TxHash: hash, Items: items}}
	}
	reverted := receipt(50, "0x06", []string{"1/6"}, &snClaimRelayItemOutcome{Reason: "the transaction reverted"})
	reverted.outcome.Reverted = true
	yesterday := receipt(999, "0x07", []string{"1/7", "1/8"}, paidItem(7, 50, 50), &snClaimRelayItemOutcome{Reason: "refused"})
	yesterday.outcome.Griefed = true

	spend := &snClaimRelaySpend{SpentWei: new(big.Int), RevertedAccounts: map[string]string{}}
	// newest nonce first, as the intents read
	spend.add(intent(model.StTxFinalized), attempts, receipt(100, "0x05b", []string{"1/5"}, paidItem(5, 300, 100)), true)
	spend.add(intent(model.StTxReverted), attempts, reverted, true)
	spend.add(intent(model.StTxFinalized), attempts, receipt(70, "0x05a", []string{"1/5"}, paidItem(5, 200, 200)), true)
	spend.add(intent(model.StTxBroadcast), attempts, nil, true)
	spend.add(intent(model.StTxSuperseded), attempts, nil, true)
	// final, and its receipt could not be read
	spend.add(intent(model.StTxFinalized), attempts, nil, true)
	spend.add(intent(model.StTxFinalized), attempts, yesterday, false)
	spend.add(intent(model.StTxBroadcast), attempts, nil, false)
	spend.add(intent(model.StTxFinalized), attempts, nil, false)

	want := new(big.Int).Add(big.NewInt(100+50+70), new(big.Int).Mul(most, big.NewInt(2)))
	if spend.SpentWei.Cmp(want) != 0 || spend.ClaimsPaid != 2 || spend.Griefed {
		t.Fatalf("spent %s (want %s), paid %d, griefed %t", spend.SpentWei, want, spend.ClaimsPaid, spend.Griefed)
	}
	if len(spend.RevertedAccounts) != 1 || spend.RevertedAccounts["1/6"] != "the transaction reverted" {
		t.Fatalf("reverted %v", spend.RevertedAccounts)
	}
	if len(spend.Paid) != 2 {
		t.Fatalf("paid %d", len(spend.Paid))
	}
	if paid := spend.Paid[0]; paid.NoId != 1 || paid.Index != 5 || paid.TxHash != "0x05b" || paid.Cumulative.Int64() != 300 || paid.AmountRao.Int64() != 100 {
		t.Fatalf("latest of account 5 %+v", paid)
	}
	if paid := spend.Paid[1]; paid.Index != 7 || paid.TxHash != "0x07" || paid.Cumulative.Int64() != 50 {
		t.Fatalf("the day before's %+v", paid)
	}
}

// The states every metric and class has to know about are complete.
func TestClaimRelayEveryStateHasAClass(t *testing.T) {
	moving := map[string]bool{SnClaimRelayStateRunning: true, SnClaimRelayStateCapReached: true, SnClaimRelayStateInFlight: true}
	notRunning := map[string]bool{SnClaimRelayStateNoKey: true, SnClaimRelayStateBadKey: true, SnClaimRelayStateNotConfigured: true, SnClaimRelayStateNotRunning: true}
	for _, state := range snClaimRelayStates {
		class := snClaimRelayClassOf(state)
		switch {
		case moving[state] && class != snClaimRelayClassMoving, notRunning[state] && class != snClaimRelayClassNotRunning, !moving[state] && !notRunning[state] && class != snClaimRelayClassPaused:
			t.Errorf("state %s has class %d", state, class)
		}
	}
	if snClaimRelayClassOf("anything else") != snClaimRelayClassNotRunning {
		t.Error("an unknown state must not read as moving")
	}
}

// Value and α conversions at the price the plan quotes.
func TestClaimRelayConversions(t *testing.T) {
	if value := snClaimRelayValueTaoRao(uint256.NewInt(1_000_000_000), claimRelayTestAlphaPrice); value.Cmp(big.NewInt(4_224_000)) != 0 {
		t.Fatalf("1 α is worth %s rao of TAO", value)
	}
	// about 3 α today, as the plan says
	if alpha := snClaimRelayAlphaOf(snClaimRelayAutomaticTaoRao, claimRelayTestAlphaPrice); alpha.Cmp(big.NewInt(2_959_000_000)) < 0 || alpha.Cmp(big.NewInt(2_960_000_000)) > 0 {
		t.Fatalf("the threshold is %s rao of α", alpha)
	}
	// 1.9 α at sn's planning price
	if alpha := snClaimRelayAlphaOf(snClaimRelayAutomaticTaoRao, big.NewInt(6_587_000_000_000_000)); alpha.Cmp(big.NewInt(1_897_000_000)) < 0 || alpha.Cmp(big.NewInt(1_899_000_000)) > 0 {
		t.Fatalf("the threshold is %s rao of α at the planning price", alpha)
	}
	if alpha := snClaimRelayAlphaOf(snClaimRelayFloorTaoRao, nil); alpha.Sign() != 0 {
		t.Fatalf("without a price: %s", alpha)
	}
	if !snClaimRelayClearsFloor(big.NewInt(105_000)) || snClaimRelayClearsFloor(big.NewInt(104_999)) {
		t.Fatal("the floor's margin moved")
	}
	if !snClaimRelayOverThreshold(big.NewInt(12_500_000)) || snClaimRelayOverThreshold(big.NewInt(12_499_999)) {
		t.Fatal("the threshold moved")
	}
	if snClaimRelayDailyCapWei.Cmp(big.NewInt(10_000_000_000_000_000)) != 0 || 1e-12 < math.Abs(snClaimRelayWeiFloat(snClaimRelayDailyCapWei)-0.01) {
		t.Fatal("the cap moved")
	}
}

// claimRelayReceiptFixture is a receipt as snClaimRelayReadReceipt reads it.
func claimRelayClaimedLog(t *testing.T, noId uint64, rootIndex uint64, index uint64, coldkey [32]byte, cumulative uint64, amount uint64, caller common.Address) *types.Log {
	t.Helper()
	return ledgerV2EventLog(t, &stabi.STSettlementVault2MetaData, claimRelayTestVault, "Claimed",
		[]common.Hash{ledgerV2Word(noId), ledgerV2Word(index), common.Hash(coldkey)},
		new(big.Int).SetUint64(rootIndex), new(big.Int).SetUint64(cumulative), new(big.Int).SetUint64(amount), caller)
}

func claimRelayBatcherLog(t *testing.T, name string, values ...any) *types.Log {
	t.Helper()
	return ledgerV2EventLog(t, &stabi.STClaimBatcherMetaData, claimRelayTestBatcher, name, nil, values...)
}

func claimRelayReceiptClaim(index uint64, cumulative uint64) *snClaimRelayClaim {
	return &snClaimRelayClaim{Account: &snClaimRelayAccount{NoId: 1, Index: index, RootIndex: 3, Coldkey: claimRelayTestColdkey(int(index)), Cumulative: *uint256.NewInt(cumulative)}}
}

func claimRelaySingleIntent(claim *snClaimRelayClaim) *model.StTransactionIntent {
	item := snClaimRelayItem(claim)
	return &model.StTransactionIntent{IntentKey: "k", Calldata: stLedgerV2VaultAbi.PackClaim(item.NoId, item.RootIndex, item.Account, item.Proof)}
}

func claimRelayBatchIntent(claims ...*snClaimRelayClaim) *model.StTransactionIntent {
	return &model.StTransactionIntent{IntentKey: "k", Calldata: stClaimRelayTestBatcherAbi.PackClaimMany(claimRelayTestVault, snClaimRelayItems(claims), big.NewInt(250_000))}
}

func claimRelayAttempt(gasLimit uint64) *model.StTransactionAttempt {
	price := "5000000000"
	return &model.StTransactionAttempt{Kind: model.StTxAttemptExecution, GasLimit: gasLimit, GasPrice: &price}
}

// A receipt decides what was paid, and it has to name the leaf's coldkey, the
// cumulative and the caller. Spend is the gas used at the price paid.
func TestClaimRelayReceiptOfASingleClaim(t *testing.T) {
	claim := claimRelayReceiptClaim(5, 1_000)
	intent := claimRelaySingleIntent(claim)
	attempt := claimRelayAttempt(198_000)
	coldkey := claim.Account.Coldkey
	read := func(gasUsed uint64, status uint64, price *big.Int, logs ...*types.Log) *snClaimRelayReceipt {
		return snClaimRelayReadReceipt(claimRelayTestRelayer, claimRelayTestVault, claimRelayTestBatcher, intent, attempt, &types.Receipt{Status: status, GasUsed: gasUsed, EffectiveGasPrice: price, Logs: logs})
	}
	good := claimRelayClaimedLog(t, 1, 3, 5, coldkey, 1_000, 1_000, claimRelayTestRelayer)
	receipt := read(155_000, types.ReceiptStatusSuccessful, claimRelayGwei(5), good)
	if receipt.spentWei.Cmp(new(big.Int).Mul(big.NewInt(155_000), claimRelayGwei(5))) != 0 {
		t.Fatalf("spent %s", receipt.spentWei)
	}
	if receipt.outcome == nil || receipt.outcome.Griefed || len(receipt.outcome.Items) != 1 || !receipt.outcome.Items[0].Paid || receipt.outcome.Items[0].AmountRao.Int64() != 1_000 {
		t.Fatalf("outcome %+v", receipt.outcome)
	}
	if len(receipt.accounts) != 1 || receipt.accounts[0] != "1/5" {
		t.Fatalf("accounts %v", receipt.accounts)
	}
	// a paid claim names its account and what vault2 now records as its claimed
	if item := receipt.outcome.Items[0]; item.NoId != 1 || item.Index != 5 || item.Cumulative == nil || item.Cumulative.Int64() != 1_000 {
		t.Fatalf("paid item %+v", item)
	}
	// a node that does not report the price: what was signed stands in
	if receipt := read(155_000, types.ReceiptStatusSuccessful, nil, good); receipt.spentWei.Cmp(new(big.Int).Mul(big.NewInt(155_000), claimRelayGwei(5))) != 0 {
		t.Fatalf("spent %s without a reported price", receipt.spentWei)
	}
	for _, c := range []struct {
		name string
		log  *types.Log
	}{
		{"another coldkey", claimRelayClaimedLog(t, 1, 3, 5, claimRelayTestColdkey(9), 1_000, 1_000, claimRelayTestRelayer)},
		{"another cumulative", claimRelayClaimedLog(t, 1, 3, 5, coldkey, 999, 999, claimRelayTestRelayer)},
		{"another root", claimRelayClaimedLog(t, 1, 2, 5, coldkey, 1_000, 1_000, claimRelayTestRelayer)},
		{"another caller", claimRelayClaimedLog(t, 1, 3, 5, coldkey, 1_000, 1_000, claimRelayTestBatcher)},
		{"nothing paid", claimRelayClaimedLog(t, 1, 3, 5, coldkey, 1_000, 0, claimRelayTestRelayer)},
		{"another account", claimRelayClaimedLog(t, 1, 3, 6, coldkey, 1_000, 1_000, claimRelayTestRelayer)},
	} {
		if receipt := read(155_000, types.ReceiptStatusSuccessful, claimRelayGwei(5), c.log); receipt.outcome.Items[0].Paid {
			t.Fatalf("%s reads as paid", c.name)
		}
	}
	if receipt := read(155_000, types.ReceiptStatusSuccessful, claimRelayGwei(5), good, good); receipt.outcome.Items[0].Paid {
		t.Fatal("two events read as paid")
	}
	if receipt := read(155_000, types.ReceiptStatusSuccessful, claimRelayGwei(5)); receipt.outcome.Items[0].Paid {
		t.Fatal("no event reads as paid")
	}
	// a log of another address proves nothing
	foreign := *good
	foreign.Address = common.Address{0x99}
	if receipt := read(155_000, types.ReceiptStatusSuccessful, claimRelayGwei(5), &foreign); receipt.outcome.Items[0].Paid {
		t.Fatal("a foreign log reads as paid")
	}
}

// What says griefing in a single claim's receipt: gas over the limit, or a revert
// that burned the transaction's whole gas limit.
func TestClaimRelayReceiptGriefing(t *testing.T) {
	claim := claimRelayReceiptClaim(5, 1_000)
	intent := claimRelaySingleIntent(claim)
	good := claimRelayClaimedLog(t, 1, 3, 5, claim.Account.Coldkey, 1_000, 1_000, claimRelayTestRelayer)
	read := func(gasLimit uint64, gasUsed uint64, status uint64, logs ...*types.Log) *snClaimRelayOutcome {
		return snClaimRelayReadReceipt(claimRelayTestRelayer, claimRelayTestVault, claimRelayTestBatcher, intent, claimRelayAttempt(gasLimit), &types.Receipt{Status: status, GasUsed: gasUsed, EffectiveGasPrice: claimRelayGwei(5), Logs: logs}).outcome
	}
	if outcome := read(480_000, snClaimRelayGriefingGas, types.ReceiptStatusSuccessful, good); outcome.Griefed {
		t.Fatal("gas at the limit reads as griefed")
	}
	if outcome := read(480_000, snClaimRelayGriefingGas+1, types.ReceiptStatusSuccessful, good); !outcome.Griefed || !outcome.Items[0].Paid {
		t.Fatalf("gas over the limit: %+v", outcome)
	}
	if outcome := read(198_000, 198_000, types.ReceiptStatusFailed); !outcome.Griefed || !outcome.Reverted || outcome.Items[0].Paid {
		t.Fatalf("a burned limit: %+v", outcome)
	}
	// someone claimed first: an ordinary revert, with gas left
	if outcome := read(198_000, 40_000, types.ReceiptStatusFailed); outcome.Griefed || !outcome.Reverted {
		t.Fatalf("an ordinary revert: %+v", outcome)
	}
}

// A batch's receipt, item by item: a refused item leaves the others paid, and an
// item out of its gas says griefing.
func TestClaimRelayReceiptOfABatch(t *testing.T) {
	first, second, third := claimRelayReceiptClaim(1, 100), claimRelayReceiptClaim(2, 200), claimRelayReceiptClaim(3, 300)
	intent := claimRelayBatchIntent(first, second, third)
	attempt := claimRelayAttempt(1_000_000)
	read := func(gasUsed uint64, logs ...*types.Log) *snClaimRelayReceipt {
		return snClaimRelayReadReceipt(claimRelayTestRelayer, claimRelayTestVault, claimRelayTestBatcher, intent, attempt, &types.Receipt{Status: types.ReceiptStatusSuccessful, GasUsed: gasUsed, EffectiveGasPrice: claimRelayGwei(5), Logs: logs})
	}
	paid := func(claim *snClaimRelayClaim, item int64, amount uint64) []*types.Log {
		return []*types.Log{
			claimRelayClaimedLog(t, 1, 3, claim.Account.Index, claim.Account.Coldkey, claim.Account.Cumulative.Uint64(), amount, claimRelayTestBatcher),
			claimRelayBatcherLog(t, "ClaimPaid", big.NewInt(item), new(big.Int).SetUint64(amount)),
		}
	}
	logs := paid(first, 0, 100)
	logs = append(logs, claimRelayBatcherLog(t, "ClaimRefused", big.NewInt(1), [4]byte{0xde, 0xad, 0xbe, 0xef}))
	logs = append(logs, paid(third, 2, 300)...)
	receipt := read(400_000, logs...)
	outcome := receipt.outcome
	if outcome.Griefed || len(outcome.Items) != 3 || !outcome.Items[0].Paid || outcome.Items[1].Paid || outcome.Items[1].NotAttempted || !outcome.Items[2].Paid {
		t.Fatalf("outcome %+v", outcome)
	}
	if strings.Join(receipt.accounts, " ") != "1/1 1/2 1/3" {
		t.Fatalf("accounts %v", receipt.accounts)
	}
	if item := outcome.Items[2]; item.NoId != 1 || item.Index != 3 || item.Cumulative == nil || item.Cumulative.Int64() != 300 {
		t.Fatalf("paid item %+v", item)
	}

	// items the gas did not reach are simply due again
	logs = paid(first, 0, 100)
	logs = append(logs, claimRelayBatcherLog(t, "ClaimsNotAttempted", big.NewInt(1)))
	outcome = read(150_000, logs...).outcome
	if outcome.Griefed || !outcome.Items[0].Paid || !outcome.Items[1].NotAttempted || !outcome.Items[2].NotAttempted {
		t.Fatalf("outcome %+v", outcome)
	}

	// no revert data is a claim out of its gas
	logs = paid(first, 0, 100)
	logs = append(logs, claimRelayBatcherLog(t, "ClaimRefused", big.NewInt(1), [4]byte{}))
	logs = append(logs, paid(third, 2, 300)...)
	if outcome = read(400_000, logs...).outcome; !outcome.Griefed {
		t.Fatalf("outcome %+v", outcome)
	}

	// gas a claim over the limit
	logs = paid(first, 0, 100)
	logs = append(logs, paid(second, 1, 200)...)
	logs = append(logs, paid(third, 2, 300)...)
	if outcome = read(3*snClaimRelayGriefingGas, logs...).outcome; outcome.Griefed {
		t.Fatalf("at the limit: %+v", outcome)
	}
	if outcome = read(3*snClaimRelayGriefingGas+3, logs...).outcome; !outcome.Griefed {
		t.Fatalf("over the limit: %+v", outcome)
	}

	// the batcher's word needs vault2's: paid to another coldkey is not paid
	logs = []*types.Log{
		claimRelayClaimedLog(t, 1, 3, 1, claimRelayTestColdkey(9), 100, 100, claimRelayTestBatcher),
		claimRelayBatcherLog(t, "ClaimPaid", big.NewInt(0), big.NewInt(100)),
		claimRelayBatcherLog(t, "ClaimsNotAttempted", big.NewInt(1)),
	}
	if outcome = read(150_000, logs...).outcome; outcome.Items[0].Paid {
		t.Fatalf("outcome %+v", outcome.Items[0])
	}
	// and a batcher report that cannot be read proves nothing
	if outcome = read(150_000).outcome; outcome.Items[0].Paid || outcome.Items[0].NotAttempted {
		t.Fatalf("outcome %+v", outcome.Items[0])
	}
}

// A retired nonce's self-transaction only paid.
func TestClaimRelayReceiptOfACancellation(t *testing.T) {
	attempt := claimRelayAttempt(21_000)
	attempt.Kind = model.StTxAttemptCancellation
	receipt := snClaimRelayReadReceipt(claimRelayTestRelayer, claimRelayTestVault, claimRelayTestBatcher, &model.StTransactionIntent{}, attempt, &types.Receipt{Status: types.ReceiptStatusSuccessful, GasUsed: 21_000, EffectiveGasPrice: claimRelayGwei(5)})
	if receipt.outcome != nil || receipt.spentWei.Cmp(new(big.Int).Mul(big.NewInt(21_000), claimRelayGwei(5))) != 0 {
		t.Fatalf("receipt %+v", receipt)
	}
}

// Calldata decodes back to the claims it was packed from, and nothing else is a
// claim.
func TestClaimRelayDecodeCalldata(t *testing.T) {
	first, second := claimRelayReceiptClaim(1, 100), claimRelayReceiptClaim(2, 200)
	claims, batch, err := snClaimRelayDecodeCalldata(claimRelaySingleIntent(first).Calldata)
	if err != nil || batch || len(claims) != 1 || claims[0].Account.Index.Uint64() != 1 || claims[0].Account.Coldkey != first.Account.Coldkey {
		t.Fatalf("single: %+v %t %v", claims, batch, err)
	}
	claims, batch, err = snClaimRelayDecodeCalldata(claimRelayBatchIntent(first, second).Calldata)
	if err != nil || !batch || len(claims) != 2 || claims[1].Account.Index.Uint64() != 2 || claims[1].Account.Coldkey != second.Account.Coldkey {
		t.Fatalf("batch: %+v %t %v", claims, batch, err)
	}
	for _, calldata := range [][]byte{nil, {1, 2}, {1, 2, 3, 4, 5}, claimRelayBatchIntent(first).Calldata[:40], claimRelaySingleIntent(first).Calldata[:40]} {
		if _, _, err := snClaimRelayDecodeCalldata(calldata); err == nil {
			t.Fatalf("decoded %x", calldata)
		}
	}
}

// Worst-case cost of an intent's attempts: the largest of its same-nonce attempts,
// by fee cap or by price.
func TestClaimRelayMostAttemptCost(t *testing.T) {
	feeCap, tip, price := "6000000000", "0", "5000000000"
	cost := snClaimRelayMostAttemptCost([]*model.StTransactionAttempt{
		{GasLimit: 200_000, GasFeeCap: &feeCap, GasTipCap: &tip},
		{GasLimit: 100_000, GasPrice: &price},
		{GasLimit: 999_999},
	})
	if cost.Cmp(big.NewInt(1_200_000_000_000_000)) != 0 {
		t.Fatalf("cost %s", cost)
	}
}

// A retried operation has its own name, and a batch's name follows its content.
func TestClaimRelayOperationNames(t *testing.T) {
	single := &snClaimRelaySend{Claims: []*snClaimRelayClaim{claimRelayReceiptClaim(5, 1)}}
	if name := snClaimRelayOperation(single, 2); name != "v2relay:1:5:3:2" {
		t.Fatalf("name %s", name)
	}
	first := &snClaimRelaySend{Batch: true, Calldata: []byte{1}}
	second := &snClaimRelaySend{Batch: true, Calldata: []byte{2}}
	if snClaimRelayOperation(first, 0) == snClaimRelayOperation(second, 0) || snClaimRelayOperation(first, 0) == snClaimRelayOperation(first, 1) {
		t.Fatal("batch names collide")
	}
	if 96 < len(snClaimRelayOperation(first, 1000)) || 96 < len(snClaimRelayOperation(&snClaimRelaySend{Claims: []*snClaimRelayClaim{{Account: &snClaimRelayAccount{NoId: 1 << 62, Index: 1 << 62, RootIndex: 1 << 62}}}}, 1000)) {
		t.Fatal("an operation name is over the engine's limit")
	}
}

// Only the relay's own client escapes the operator gas policy, for its own account.
func TestClaimRelayEngineExemption(t *testing.T) {
	var none *stClaimRelayEngine
	if none.exemptFromOperatorGasPolicy([]common.Address{claimRelayTestRelayer}) || none.plannedGasLimit([]byte{1}) != 0 || none.retiresUnsigned(nil) {
		t.Fatal("a client without the relay's engine behaves as the relay's")
	}
	engine := newStClaimRelayEngine(claimRelayTestRelayer)
	if !engine.exemptFromOperatorGasPolicy([]common.Address{claimRelayTestRelayer}) {
		t.Fatal("the relayer is not exempt on its own client")
	}
	if engine.exemptFromOperatorGasPolicy([]common.Address{claimRelayTestVault}) || engine.exemptFromOperatorGasPolicy([]common.Address{claimRelayTestRelayer, claimRelayTestVault}) || engine.exemptFromOperatorGasPolicy(nil) {
		t.Fatal("another account is exempt")
	}
	engine.plan([]byte{1}, 7)
	if engine.plannedGasLimit([]byte{1}) != 7 || engine.plannedGasLimit([]byte{2}) != 0 {
		t.Fatal("planned gas limits")
	}
	engine.unplan([]byte{1})
	if engine.plannedGasLimit([]byte{1}) != 0 {
		t.Fatal("unplanned gas limit")
	}
	if !engine.retiresUnsigned(nil) || engine.retiresUnsigned([]*model.StTransactionAttempt{{}}) {
		t.Fatal("only an unsigned intent is retired")
	}
}

// The relay's fee cap is close to the price, so its worst case is small.
func TestClaimRelayFeeCap(t *testing.T) {
	if cap := snClaimRelayFeeCap(claimRelayGwei(5), big.NewInt(0)); cap.Cmp(big.NewInt(5_625_000_000)) != 0 {
		t.Fatalf("fee cap %s", cap)
	}
	if cap := snClaimRelayFeeCap(claimRelayGwei(8), claimRelayGwei(1)); cap.Cmp(claimRelayGwei(10)) != 0 {
		t.Fatalf("fee cap %s", cap)
	}
	// What one claim at the planning figures may cost, against the day.
	if most := snClaimRelayLiability(snClaimRelayGasLimit(claimRelayTestEstimateGas), snClaimRelayFeeCap(claimRelayGwei(5), new(big.Int))); most.Cmp(snClaimRelayDailyCapWei) >= 0 {
		t.Fatalf("one claim may cost %s", most)
	}
}

// Refusals carried in the status are bounded, and go oldest first.
func TestClaimRelayRefusalsAreBounded(t *testing.T) {
	now := claimRelayTestMonday
	status := &SnClaimRelayStatus{Refusals: map[string]*SnClaimRelayRefusal{}}
	for index := range snClaimRelayRefusalsLimit + 3 {
		status.Refusals[snClaimRelayAccountKey(1, uint64(index))] = &SnClaimRelayRefusal{Time: now.Add(time.Duration(index) * time.Second), Until: now.Add(time.Hour)}
	}
	snClaimRelayLimitRefusals(status)
	if len(status.Refusals) != snClaimRelayRefusalsLimit || status.Refusals["1/0"] != nil || status.Refusals["1/1"] != nil || status.Refusals["1/2"] != nil || status.Refusals["1/3"] == nil {
		t.Fatalf("%d refusals", len(status.Refusals))
	}
	live := snClaimRelayLiveRefusals(map[string]*SnClaimRelayRefusal{"a": {Until: now.Add(time.Second)}, "b": {Until: now}, "c": nil}, now)
	if len(live) != 1 || live["a"] == nil {
		t.Fatalf("live %v", live)
	}
}

// A pass's metrics carry its amounts, and a counter its state.
func TestClaimRelayMetricsOfAPass(t *testing.T) {
	before := testutil.ToFloat64(snClaimRelayPasses.WithLabelValues(SnClaimRelayStateCapReached))
	observeSnClaimRelayPass(&SnClaimRelayStatus{
		State: SnClaimRelayStateCapReached, PassTime: claimRelayTestMonday, BalanceWei: "500000000000000000", SpentTodayWei: "9300000000000000",
		GasPriceWei: "5000000000", AlphaPriceWei: "4224000000000000", ClaimsPending: 31, OverThreshold: 30, ClaimsPaidToday: 12, LastEstimateGas: 165_000,
		Refusals: map[string]*SnClaimRelayRefusal{"1/1": {}},
	})
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"passes", testutil.ToFloat64(snClaimRelayPasses.WithLabelValues(SnClaimRelayStateCapReached)) - before, 1},
		{"state", testutil.ToFloat64(snClaimRelayState.WithLabelValues(SnClaimRelayStateCapReached)), 1},
		{"balance", testutil.ToFloat64(snClaimRelayBalanceTao), 0.5},
		{"spent", testutil.ToFloat64(snClaimRelaySpentTodayTao), 0.0093},
		{"cap", testutil.ToFloat64(snClaimRelayDailyCapTao), 0.01},
		{"gas price", testutil.ToFloat64(snClaimRelayGasPriceGwei), 5},
		{"price", testutil.ToFloat64(snClaimRelayAlphaPriceTao), 0.004224},
		{"pending", testutil.ToFloat64(snClaimRelayClaimsPending), 31},
		{"over", testutil.ToFloat64(snClaimRelayOverThresholdGauge), 30},
		{"paid", testutil.ToFloat64(snClaimRelayClaimsPaidToday), 12},
		{"refused", testutil.ToFloat64(snClaimRelayRefusalsGauge), 1},
		{"estimate", testutil.ToFloat64(snClaimRelayLastEstimateGas), 165_000},
	} {
		if math.Abs(c.got-c.want) > 1e-12 {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}
