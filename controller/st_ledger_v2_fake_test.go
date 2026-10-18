package controller

// st_ledger_v2_fake_test.go — an in-memory vault2, STSettlement2 and v1 epoch
// clock, an in-memory ledger builder, and a blob store that logs writes, for
// the settlement v2 driver tests. Epoch e ends at block e·epochBlocks. Every
// write enforces the contract rules the driver relies on, so a driver that
// sent a call out of order would see the contract's refusal.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/merkle"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

type fakeLedgerV2Chain struct {
	log *[]string

	head       uint64
	paused     bool
	registered bool
	firstEpoch uint64
	// coordinator is the STSettlement2 the vault is bound to.
	coordinator common.Address
	// emission[e] reaches the pool hotkey at end(e); a close captures all of it.
	emission     map[uint64]uint64
	epochBlocks  uint64
	window       uint64
	challenge    uint64
	lastFunded   uint64
	lastCaptured uint64
	funded       map[uint64]uint256.Int
	captured     uint256.Int
	roots        []*StLedgerV2FinalizedRoot
	pending      *StLedgerV2Pending
	seq          uint64
	lastCommit   uint64
	approvals    uint64
	quorum       uint64
	// demands holds DemandState by (seq, rootIndex, index).
	demands map[[3]uint64]uint8
	// claimed is vault2's claimed by (noId, index).
	claimed map[[2]uint64]uint256.Int
	// fail queues the outcomes of an operation's next calls (nil succeeds).
	fail map[string][]error
	// latest is the latest block, where a send's eth_call and eth_estimateGas
	// run; a close or commit executes in the block after it (R battery). Below
	// head it reads as head.
	latest uint64
	// afterClose moves the head after a close, as a slow inclusion would.
	afterClose uint64
	// closeLag hides a close from the funding state, as a lagging head would.
	closeLag bool
}

func newFakeLedgerV2Chain(log *[]string) *fakeLedgerV2Chain {
	return &fakeLedgerV2Chain{
		log: log, registered: true, firstEpoch: 5, coordinator: common.HexToAddress(testLedgerV2Settlement),
		epochBlocks: 50_400, window: 1_200, challenge: 14_400, quorum: 1,
		lastFunded: 4, lastCaptured: 4, emission: map[uint64]uint64{}, funded: map[uint64]uint256.Int{},
		demands: map[[3]uint64]uint8{}, claimed: map[[2]uint64]uint256.Int{}, fail: map[string][]error{},
	}
}

func (self *fakeLedgerV2Chain) end(epoch uint64) uint64 {
	return epoch * self.epochBlocks
}

func (self *fakeLedgerV2Chain) failed(operation string) error {
	queue := self.fail[operation]
	if len(queue) == 0 {
		return nil
	}
	self.fail[operation] = queue[1:]
	return queue[0]
}

// failNext makes the next call of an operation fail.
func (self *fakeLedgerV2Chain) failNext(operation string, err error) {
	self.fail[operation] = []error{err}
}

func (self *fakeLedgerV2Chain) LedgerV2Pool(ctx context.Context, noId uint64) (bool, uint64, error) {
	if err := self.failed("pool"); err != nil {
		return false, 0, err
	}
	return self.registered, self.firstEpoch, nil
}

func (self *fakeLedgerV2Chain) LedgerV2Coordinator(ctx context.Context) (common.Address, error) {
	if err := self.failed("coordinator"); err != nil {
		return common.Address{}, err
	}
	return self.coordinator, nil
}

func (self *fakeLedgerV2Chain) LedgerV2State(ctx context.Context, noId uint64) (*StLedgerV2State, error) {
	if err := self.failed("state"); err != nil {
		return nil, err
	}
	state := &StLedgerV2State{HeadBlock: self.head, Paused: self.paused, Registered: self.registered, FirstEpoch: self.firstEpoch,
		LastFundedEpoch: self.lastFunded, RootCount: uint64(len(self.roots)), LastCommitBlock: self.lastCommit,
		Quorum: self.quorum, ChallengeBlocks: self.challenge}
	if len(self.roots) != 0 {
		latest := *self.roots[len(self.roots)-1]
		state.Latest = &latest
	}
	if self.pending != nil {
		pending := *self.pending
		state.Pending, state.Approvals = &pending, self.approvals
	}
	return state, nil
}

func (self *fakeLedgerV2Chain) LedgerV2EpochWindow(ctx context.Context, epoch uint64) (uint64, uint64, error) {
	if err := self.failed("window"); err != nil {
		return 0, 0, err
	}
	return self.end(epoch), self.window, nil
}

func (self *fakeLedgerV2Chain) LedgerV2FundedThrough(ctx context.Context, noId uint64, epoch uint64) (uint256.Int, error) {
	if err := self.failed("funded"); err != nil {
		return uint256.Int{}, err
	}
	return self.funded[epoch], nil
}

// The block a close or commit sent now executes in.
func (self *fakeLedgerV2Chain) executionBlock() uint64 {
	return max(self.head, self.latest) + 1
}

func (self *fakeLedgerV2Chain) LedgerV2LatestBlock(ctx context.Context) (uint64, error) {
	if err := self.failed("latest"); err != nil {
		return 0, err
	}
	return max(self.head, self.latest), nil
}

func (self *fakeLedgerV2Chain) LedgerV2Root(ctx context.Context, noId uint64, rootIndex uint64) (*StLedgerV2FinalizedRoot, error) {
	if err := self.failed("root"); err != nil {
		return nil, err
	}
	if uint64(len(self.roots)) <= rootIndex {
		return nil, errors.New("roots reverts")
	}
	root := *self.roots[rootIndex]
	return &root, nil
}

func (self *fakeLedgerV2Chain) LedgerV2Demand(ctx context.Context, noId uint64, seq uint64, rootIndex uint64, index uint64) (uint8, error) {
	if err := self.failed("demand"); err != nil {
		return 0, err
	}
	return self.demands[[3]uint64{seq, rootIndex, index}], nil
}

func (self *fakeLedgerV2Chain) LedgerV2Claimed(ctx context.Context, noId uint64, index uint64) (uint256.Int, error) {
	if err := self.failed("claimed"); err != nil {
		return uint256.Int{}, err
	}
	return self.claimed[[2]uint64{noId, index}], nil
}

func (self *fakeLedgerV2Chain) LedgerV2Close(ctx context.Context, epoch uint64, noId uint64) (string, error) {
	*self.log = append(*self.log, fmt.Sprintf("close %d", epoch))
	if err := self.failed("close"); err != nil {
		return "", err
	}
	if self.paused {
		return "", errors.New("SettlementPaused")
	}
	if block := self.executionBlock(); block < self.end(epoch) || self.end(epoch)+self.window < block {
		return "", errors.New("InvalidWindow")
	}
	if epoch <= self.lastFunded {
		return "", errors.New("InvalidTransition")
	}
	for k := self.lastFunded + 1; k < epoch; k++ {
		self.funded[k] = self.captured
		*self.log = append(*self.log, fmt.Sprintf("defer %d", k))
	}
	for k := self.lastCaptured + 1; k <= epoch; k++ {
		self.captured.AddUint64(&self.captured, self.emission[k])
	}
	if self.closeLag {
		return fmt.Sprintf("0xclose%d", epoch), nil
	}
	self.funded[epoch], self.lastFunded, self.lastCaptured = self.captured, epoch, epoch
	self.head += self.afterClose
	return fmt.Sprintf("0xclose%d", epoch), nil
}

func (self *fakeLedgerV2Chain) LedgerV2Commit(ctx context.Context, epoch uint64, noId uint64, prevRootCount uint64, c *StLedgerV2Commitment) (string, error) {
	*self.log = append(*self.log, fmt.Sprintf("commit %d prev %d", epoch, prevRootCount))
	if err := self.failed("commit"); err != nil {
		return "", err
	}
	lastRootEpoch, finalizedTotal, lastLeafCount := self.firstEpoch-1, uint256.Int{}, uint64(0)
	if len(self.roots) != 0 {
		last := self.roots[len(self.roots)-1]
		lastRootEpoch, finalizedTotal, lastLeafCount = last.Epoch, last.Total, last.LeafCount
	}
	funded := self.funded[epoch]
	switch {
	case self.paused:
		return "", errors.New("SettlementPaused")
	case self.executionBlock() < self.end(epoch) || self.end(epoch)+self.window < self.executionBlock():
		return "", errors.New("InvalidWindow")
	case self.pending != nil && self.head < self.end(self.pending.Epoch+1):
		return "", errors.New("PendingRootLive")
	case self.lastCommit != 0 && self.head < self.lastCommit+self.challenge:
		return "", errors.New("CommitTooSoon")
	case prevRootCount != uint64(len(self.roots)):
		return "", errors.New("StaleRootCount")
	case epoch <= lastRootEpoch || self.lastFunded < epoch:
		return "", errors.New("EpochOutOfRange")
	case !c.Total.Eq(&funded):
		return "", errors.New("TotalMismatch")
	case !c.Total.Gt(&finalizedTotal):
		return "", errors.New("TotalNotIncreased")
	case c.LeafCount == 0 || c.LeafCount < lastLeafCount:
		return "", errors.New("InvalidLeafCount")
	case c.Root == [32]byte{} || c.LedgerHash == [32]byte{}:
		return "", errors.New("InvalidRoot")
	}
	self.seq++
	self.pending = &StLedgerV2Pending{Seq: self.seq, Root: c.Root, Total: c.Total, LeafCount: c.LeafCount, Epoch: epoch, LedgerHash: c.LedgerHash, CommittedBlock: self.head}
	self.lastCommit, self.approvals = self.head, 0
	return fmt.Sprintf("0xcommit%d", epoch), nil
}

func (self *fakeLedgerV2Chain) LedgerV2Finalize(ctx context.Context, epoch uint64, noId uint64) (string, error) {
	*self.log = append(*self.log, fmt.Sprintf("finalize %d", epoch))
	if err := self.failed("finalize"); err != nil {
		return "", err
	}
	switch {
	case self.paused:
		return "", errors.New("SettlementPaused")
	case self.pending == nil:
		return "", errors.New("NoPendingRoot")
	case self.approvals < self.quorum:
		return "", errors.New("QuorumNotReached")
	case self.head < self.pending.CommittedBlock+self.challenge:
		return "", errors.New("ChallengeWindowOpen")
	case self.pending.OpenDemands != 0:
		return "", errors.New("DemandOpen")
	}
	p := self.pending
	self.roots = append(self.roots, &StLedgerV2FinalizedRoot{Index: uint64(len(self.roots)), Root: p.Root, Total: p.Total, LeafCount: p.LeafCount, Epoch: p.Epoch, LedgerHash: p.LedgerHash})
	self.pending = nil
	return fmt.Sprintf("0xfinalize%d", epoch), nil
}

// demand opens a demand on the pending root for an account of the latest
// finalized root.
func (self *fakeLedgerV2Chain) demand(index uint64) {
	self.demandAt(uint64(len(self.roots)-1), index)
}

// demandAt opens a demand on the pending root for an account of the finalized
// root rootIndex.
func (self *fakeLedgerV2Chain) demandAt(rootIndex uint64, index uint64) {
	self.demands[[3]uint64{self.pending.Seq, rootIndex, index}] = StLedgerV2DemandOpen
	self.pending.OpenDemands++
}

// The revert vault2 returns for a named error, with its selector as data.
func fakeLedgerV2Revert(name string) error {
	return ledgerV2DataError{ledgerV2ErrorData(name)}
}

func (self *fakeLedgerV2Chain) LedgerV2Answer(ctx context.Context, noId uint64, seq uint64, answer *StLedgerV2Answer) (string, error) {
	*self.log = append(*self.log, fmt.Sprintf("answer %d %d %d", seq, answer.RootIndex, answer.Index))
	if err := self.failed("answer"); err != nil {
		return "", err
	}
	if self.pending == nil || len(self.roots) == 0 {
		return "", fakeLedgerV2Revert("NoPendingRoot")
	}
	if uint64(len(self.roots)) <= answer.RootIndex {
		return "", errors.New("UnknownRoot")
	}
	f, p := self.roots[answer.RootIndex], self.pending
	if !merkle.VerifyLedgerProof(f.Root, &f.Total, f.LeafCount, answer.Index, answer.Account.Coldkey, &answer.Account.Cumulative, answer.Proof) {
		return "", errors.New("InvalidProof")
	}
	fault, _, err := merkle.WalkLedgerPath(p.Root, &p.Total, p.LeafCount, answer.Index, answer.Account.Coldkey, &answer.Account.Cumulative, answer.Path)
	if err != nil {
		return "", errors.New("MalformedPath")
	}
	if fault != merkle.LedgerFaultNone {
		self.pending = nil
		return fmt.Sprintf("0xdeleted%d", seq), nil
	}
	key := [3]uint64{p.Seq, answer.RootIndex, answer.Index}
	if self.demands[key] != StLedgerV2DemandOpen {
		return "", fakeLedgerV2Revert("NoFault")
	}
	self.demands[key] = StLedgerV2DemandAnswered
	p.OpenDemands--
	return fmt.Sprintf("0xanswer%d", answer.Index), nil
}

// fakeLedgerBuilder allocates each covered epoch's funding plus carry to the
// epoch's earners in equal shares, the remainder to the first; an epoch with
// no earners, or declared null, carries.
type fakeLedgerBuilder struct {
	log     *[]string
	earners map[uint64][][32]byte
	ledgers map[[32]byte][]merkle.LedgerAccount
	fail    error
	inputs  []*StLedgerV2BuildInput
	builds  []*StLedgerV2Build
	// onBuild runs during a build; mutate changes a finished build.
	onBuild func()
	mutate  func(*StLedgerV2Build)
	// Builder faults the driver must refuse.
	nullUnrecorded bool
	changeRecorded bool
	shortTotal     bool
}

func newFakeLedgerBuilder(log *[]string) *fakeLedgerBuilder {
	return &fakeLedgerBuilder{log: log, earners: map[uint64][][32]byte{}, ledgers: map[[32]byte][]merkle.LedgerAccount{}}
}

func (self *fakeLedgerBuilder) Build(ctx context.Context, input *StLedgerV2BuildInput) (*StLedgerV2Build, error) {
	*self.log = append(*self.log, fmt.Sprintf("build %d", input.Epoch))
	self.inputs = append(self.inputs, input)
	if self.onBuild != nil {
		self.onBuild()
	}
	if err := self.fail; err != nil {
		self.fail = nil
		return nil, err
	}
	var accounts []merkle.LedgerAccount
	if input.Previous != nil {
		previous, ok := self.ledgers[input.Previous.LedgerHash]
		if !ok {
			return nil, errors.New("fake builder: unknown previous ledger")
		}
		accounts = slices.Clone(previous)
	}
	positions := map[[32]byte]int{}
	for i, account := range accounts {
		positions[account.Coldkey] = i
	}
	build := &StLedgerV2Build{}
	added := map[[32]byte]*uint256.Int{}
	var carry uint256.Int
	for _, covered := range input.Epochs {
		providers := &StLedgerV2EpochProviders{Epoch: covered.Epoch, ProviderChunks: []string{}}
		earners := self.earners[covered.Epoch]
		switch {
		case covered.Rows != nil && covered.Rows.Null():
			providers.ProviderChunks, providers.Null, earners = nil, true, nil
		case covered.Rows != nil:
			providers.ProviderChunks = covered.Rows.ProviderChunks
			if self.changeRecorded {
				providers.ProviderChunks = append(slices.Clone(providers.ProviderChunks), startifact.LedgerObjectRef([]byte("changed")))
			}
		case self.nullUnrecorded:
			providers.ProviderChunks, providers.Null = nil, true
		case len(earners) != 0:
			chunk := []byte(fmt.Sprintf("provider rows of epoch %d: %x", covered.Epoch, earners))
			build.Chunks = append(build.Chunks, chunk)
			providers.ProviderChunks = []string{startifact.LedgerObjectRef(chunk)}
		}
		build.Epochs = append(build.Epochs, providers)
		amount := new(uint256.Int).Add(&covered.Funding, &carry)
		if len(earners) == 0 {
			carry = *amount
			continue
		}
		carry.Clear()
		share, remainder := new(uint256.Int), new(uint256.Int)
		share.DivMod(amount, uint256.NewInt(uint64(len(earners))), remainder)
		for i, coldkey := range earners {
			delta := new(uint256.Int).Set(share)
			if i == 0 {
				delta.Add(delta, remainder)
			}
			if delta.IsZero() {
				continue
			}
			build.Amounts = append(build.Amounts, &model.StLedgerV2Amount{Epoch: covered.Epoch, Coldkey: coldkey, Amount: *delta})
			if position, ok := positions[coldkey]; ok {
				accounts[position].Cumulative.Add(&accounts[position].Cumulative, delta)
			} else if sum, ok := added[coldkey]; ok {
				sum.Add(sum, delta)
			} else {
				added[coldkey] = delta
			}
		}
	}
	if !carry.IsZero() {
		return nil, ErrStLedgerV2NoRoot
	}
	newColdkeys := make([][32]byte, 0, len(added))
	for coldkey := range added {
		newColdkeys = append(newColdkeys, coldkey)
	}
	sort.Slice(newColdkeys, func(a, b int) bool { return string(newColdkeys[a][:]) < string(newColdkeys[b][:]) })
	for _, coldkey := range newColdkeys {
		accounts = append(accounts, merkle.LedgerAccount{Coldkey: coldkey, Cumulative: *added[coldkey]})
	}
	if self.shortTotal {
		accounts[0].Cumulative.SubUint64(&accounts[0].Cumulative, 1)
	}
	accountChunk := []byte(fmt.Sprintf("accounts of epoch %d: %v", input.Epoch, accounts))
	build.Chunks = append(build.Chunks, accountChunk)
	build.Accounts = accounts
	header, err := json.Marshal(map[string]any{"no_id": input.NoId, "epoch": input.Epoch, "epochs": build.Epochs, "accounts": []string{startifact.LedgerObjectRef(accountChunk)}})
	if err != nil {
		return nil, err
	}
	build.Header = header
	self.ledgers[sha256.Sum256(header)] = slices.Clone(accounts)
	if self.mutate != nil {
		self.mutate(build)
	}
	self.builds = append(self.builds, build)
	return build, nil
}

func (self *fakeLedgerBuilder) Accounts(ctx context.Context, ledgerHash [32]byte) ([]merkle.LedgerAccount, error) {
	*self.log = append(*self.log, fmt.Sprintf("accounts %x", ledgerHash[:4]))
	accounts, ok := self.ledgers[ledgerHash]
	if !ok {
		return nil, errors.New("fake builder: unknown ledger")
	}
	return slices.Clone(accounts), nil
}

// loggingBlobStore records each put-if-absent by the key's first 8 digest digits.
type loggingBlobStore struct {
	server.BlobStore
	log *[]string
}

func (self *loggingBlobStore) PutIfAbsent(ctx context.Context, key string, localPath string, contentType string) (bool, error) {
	*self.log = append(*self.log, "put "+path.Base(key)[:8])
	return self.BlobStore.PutIfAbsent(ctx, key, localPath, contentType)
}

// refusingBlobStore refuses every write, or only writes whose key contains only.
type refusingBlobStore struct {
	server.BlobStore
	only string
}

func (self *refusingBlobStore) PutIfAbsent(ctx context.Context, key string, localPath string, contentType string) (bool, error) {
	if self.only != "" && !strings.Contains(key, self.only) {
		return self.BlobStore.PutIfAbsent(ctx, key, localPath, contentType)
	}
	return false, errors.New("put refused")
}

func ledgerV2PutEntry(object []byte) string {
	return "put " + strings.TrimPrefix(startifact.LedgerObjectRef(object), "sha256:")[:8]
}

// ledgerV2Fixture wires a fake chain, a fake builder and a logging blob store
// into the driver under a v2 st config.
type ledgerV2Fixture struct {
	t       testing.TB
	ctx     context.Context
	cfg     *StConfig
	log     []string
	chain   *fakeLedgerV2Chain
	builder *fakeLedgerBuilder
	store   server.BlobStore
	alerts  *[]string
}

func newLedgerV2Fixture(t testing.TB) *ledgerV2Fixture {
	f := &ledgerV2Fixture{t: t, ctx: context.Background(), cfg: ledgerV2TestConfig()}
	f.chain, f.builder = newFakeLedgerV2Chain(&f.log), newFakeLedgerBuilder(&f.log)
	SetStConfig(f.cfg)
	SetStLedgerV2Client(f.chain)
	SetStLedgerBuilder(f.builder)
	f.store = &loggingBlobStore{BlobStore: server.NewLocalBlobStore(t.TempDir(), "test"), log: &f.log}
	SetStLedgerV2ObjectStore(f.store)
	resetLedgerV2Caches()
	f.alerts = captureLedgerV2Alerts(t)
	t.Cleanup(func() {
		SetStConfig(nil)
		SetStLedgerV2Client(nil)
		SetStLedgerBuilder(nil)
		SetStLedgerV2ObjectStore(nil)
		resetLedgerV2Caches()
	})
	return f
}

func resetLedgerV2Caches() {
	stLedgerV2Pools.Clear()
	stLedgerV2Windows.Clear()
	stLedgerV2PruneChecks.Clear()
	stLedgerV2TreeLock.Lock()
	stLedgerV2Trees, stLedgerV2TreeOrder = map[[32]byte]*stLedgerV2Tree{}, nil
	stLedgerV2TreeLock.Unlock()
}

func (self *ledgerV2Fixture) run(kind string, epoch uint64, seq uint64, index uint64) *StPublishOutcome {
	self.t.Helper()
	outcome, err := StLedgerV2RunStep(self.ctx, &StLedgerV2Step{Kind: kind, Epoch: epoch, Seq: seq, Index: index})
	if err != nil {
		self.t.Fatalf("%s step: %v", kind, err)
	}
	return outcome
}

// takeLog returns and clears the shared call log.
func (self *ledgerV2Fixture) takeLog() []string {
	log := self.log
	self.log = nil
	return log
}

// publishes returns "kind status" for an epoch's v2 publish rows, in order.
func (self *ledgerV2Fixture) publishes(epoch uint64) []string {
	rows := model.GetStPublishes(self.ctx, self.cfg.DeploymentKey(), epoch)
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].CreateTime.Before(rows[b].CreateTime) })
	out := []string{}
	for _, row := range rows {
		out = append(out, row.Kind+" "+row.Status)
	}
	return out
}

func ledgerV2Coldkey(marker byte) [32]byte {
	var coldkey [32]byte
	coldkey[0] = marker
	return coldkey
}
