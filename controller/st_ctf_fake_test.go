package controller

// st_ctf_fake_test.go — an in-memory STSettlement2Ctf, carry contract and
// driver records for the capture the flag driver tests, beside the provider
// pool's fake vault2 (st_ledger_v2_fake_test.go), which here is the CTF pool
// P = 2^32 + 1. Writes enforce the contract rules the driver relies on: the
// census window, write-once commitments, a recycle only of a final root's
// carry account. No test here uses the database or the network.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/amendment"
	"github.com/urfoundation/sn/ctf"
	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/merkle"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const ctfTestNoId = 1

var (
	ctfTestPoolId = ctf.PoolID(ctfTestNoId)
	ctfTestCarry  = common.HexToAddress("0x00000000000000000000000000000000000000c7")
)

// Real quicknet pulses, read from drand's public API on 2026-11-11.
var ctfTestPulses = map[uint64]string{
	1000: "b44679b9a59af2ec876b1a6b1ad52ea9b1615fc3982b19576350f93447cb1125e342b73a8dd2bacbe47e4b6b63ed5e39",
	1001: "b33bf3667cbd5a82de3a24b4e0e9fe5513cc1a0e840368c6e31f5fcfa79bea03f73896b25883abf2853d10337fb8fa41",
	1002: "ab066f9c12dd6de1336fca0f925192fb0c72a771c3e4c82ede1fd362c1a770f9eb05843c6308ce2530b53a99c0281a6e",
	1003: "b104c82771698f45fd8dcfead083d482694c31ab519bcef077f126f3736fe98c8392fd5d45d88aeb76b56ccfcb0296d7",
	1004: "a40658b820c0f8c10207524179a2031ba9537688a0d04e4851b58026be9a341fee3b96fb48ffad28483d84b40a5864aa",
}

func ctfTestPulse(t testing.TB, round uint64) *ctf.Pulse {
	t.Helper()
	signature, err := hex.DecodeString(ctfTestPulses[round])
	if err != nil || len(signature) != ctf.PulseSignatureBytes {
		t.Fatalf("no test pulse for round %d", round)
	}
	return &ctf.Pulse{Round: round, Signature: signature}
}

// ctfTestCommitTime is a census block time whose draw round is round.
func ctfTestCommitTime(round uint64) uint64 {
	return ctf.RoundTime(round) - ctf.DrawDelay
}

type fakeCtfCensus struct {
	hash  [32]byte
	block uint64
}

// fakeCtfChain is the CTF pool's vault2 and STSettlement2 (the embedded
// fake), the provider pool's vault2 (providers), STSettlement2Ctf's census
// commitments and the carry contract.
type fakeCtfChain struct {
	*fakeLedgerV2Chain
	providers *fakeLedgerV2Chain

	census map[uint64]fakeCtfCensus
	// times holds blocks' EVM timestamps; others read as 12 s a block.
	times map[uint64]uint64
	// roundOf, when set, fixes the draw round of an epoch's commitment.
	roundOf  map[uint64]uint64
	carry    StCtfCarry
	recycled map[uint64]uint256.Int
	// held is the carry contract's stake on vault2's escrow hotkey.
	held uint256.Int
}

func newFakeCtfChain(log *[]string) *fakeCtfChain {
	pool := newFakeLedgerV2Chain(log)
	pool.firstEpoch, pool.lastFunded, pool.lastCaptured = 8, 7, 7
	providers := newFakeLedgerV2Chain(&[]string{})
	providers.firstEpoch, providers.lastFunded, providers.lastCaptured = 4, 3, 3
	return &fakeCtfChain{
		fakeLedgerV2Chain: pool, providers: providers,
		census: map[uint64]fakeCtfCensus{}, times: map[uint64]uint64{}, roundOf: map[uint64]uint64{}, recycled: map[uint64]uint256.Int{},
		carry: StCtfCarry{Vault: common.HexToAddress(testLedgerV2Vault), PoolId: ctfTestPoolId, Coldkey: stCtfCarryColdkey(ctfTestCarry)},
	}
}

// pool routes a read to the provider pool's fake or the CTF pool's.
func (self *fakeCtfChain) pool(noId uint64) *fakeLedgerV2Chain {
	if noId == ctfTestNoId {
		return self.providers
	}
	return self.fakeLedgerV2Chain
}

func (self *fakeCtfChain) LedgerV2Pool(ctx context.Context, noId uint64) (bool, uint64, error) {
	return self.pool(noId).LedgerV2Pool(ctx, noId)
}

func (self *fakeCtfChain) LedgerV2State(ctx context.Context, noId uint64) (*StLedgerV2State, error) {
	state, err := self.pool(noId).LedgerV2State(ctx, noId)
	if state != nil {
		state.HeadBlock = self.head
	}
	return state, err
}

func (self *fakeCtfChain) LedgerV2FundedThrough(ctx context.Context, noId uint64, epoch uint64) (uint256.Int, error) {
	return self.pool(noId).LedgerV2FundedThrough(ctx, noId, epoch)
}

func (self *fakeCtfChain) requirePool(noId uint64) error {
	if noId != ctfTestPoolId {
		return fmt.Errorf("a write to pool %d, not the capture the flag pool", noId)
	}
	return nil
}

func (self *fakeCtfChain) LedgerV2Close(ctx context.Context, epoch uint64, noId uint64) (string, error) {
	if err := self.requirePool(noId); err != nil {
		return "", err
	}
	return self.fakeLedgerV2Chain.LedgerV2Close(ctx, epoch, noId)
}

func (self *fakeCtfChain) LedgerV2Commit(ctx context.Context, epoch uint64, noId uint64, prevRootCount uint64, c *StLedgerV2Commitment) (string, error) {
	if err := self.requirePool(noId); err != nil {
		return "", err
	}
	return self.fakeLedgerV2Chain.LedgerV2Commit(ctx, epoch, noId, prevRootCount, c)
}

func (self *fakeCtfChain) LedgerV2Finalize(ctx context.Context, epoch uint64, noId uint64) (string, error) {
	if err := self.requirePool(noId); err != nil {
		return "", err
	}
	return self.fakeLedgerV2Chain.LedgerV2Finalize(ctx, epoch, noId)
}

func (self *fakeCtfChain) CtfCensus(ctx context.Context, noId uint64, epoch uint64) ([32]byte, uint64, error) {
	if err := self.failed("ctfcensus"); err != nil {
		return [32]byte{}, 0, err
	}
	if noId != ctfTestNoId {
		return [32]byte{}, 0, fmt.Errorf("ctfCensus of operator %d", noId)
	}
	census := self.census[epoch]
	return census.hash, census.block, nil
}

func (self *fakeCtfChain) CtfBlockTime(ctx context.Context, block uint64) (uint64, error) {
	if err := self.failed("blocktime"); err != nil {
		return 0, err
	}
	if at, ok := self.times[block]; ok {
		return at, nil
	}
	return 1_700_000_000 + 12*block, nil
}

func (self *fakeCtfChain) CtfCarry(ctx context.Context, carry common.Address) (*StCtfCarry, error) {
	if carry != ctfTestCarry {
		return nil, fmt.Errorf("no contract at %s", carry.Hex())
	}
	read := self.carry
	return &read, nil
}

func (self *fakeCtfChain) CtfRecycledFor(ctx context.Context, carry common.Address, epoch uint64) (uint256.Int, error) {
	if carry != ctfTestCarry {
		return uint256.Int{}, fmt.Errorf("no contract at %s", carry.Hex())
	}
	return self.recycled[epoch], nil
}

func (self *fakeCtfChain) CtfCommitCensus(ctx context.Context, epoch uint64, noId uint64, hash [32]byte) (string, error) {
	*self.log = append(*self.log, fmt.Sprintf("census %d", epoch))
	if err := self.failed("commitcensus"); err != nil {
		return "", err
	}
	block := self.executionBlock()
	switch {
	case noId != ctfTestNoId:
		return "", fmt.Errorf("commitCtfCensus of operator %d", noId)
	case self.paused:
		return "", fakeLedgerV2Revert("SettlementPaused")
	case !self.registered || epoch < self.firstEpoch:
		return "", fakeLedgerV2Revert("UnknownPool")
	case hash == [32]byte{}:
		return "", fakeLedgerV2Revert("InvalidCensusHash")
	case block < self.end(epoch) || self.end(epoch)+self.window/2 < block:
		return "", fakeLedgerV2Revert("InvalidWindow")
	case self.census[epoch].hash != [32]byte{}:
		return "", fakeLedgerV2Revert("CensusAlreadyCommitted")
	}
	self.census[epoch] = fakeCtfCensus{hash: hash, block: block}
	if round, ok := self.roundOf[epoch]; ok {
		self.times[block] = ctfTestCommitTime(round)
	}
	return fmt.Sprintf("0xcensus%d", epoch), nil
}

func (self *fakeCtfChain) CtfRecycle(ctx context.Context, carry common.Address, poolId uint64, rootIndex uint64, index uint64, account *merkle.LedgerAccount, proof []merkle.LedgerNode) (string, error) {
	*self.log = append(*self.log, fmt.Sprintf("recycle root %d", rootIndex))
	if err := self.failed("recycle"); err != nil {
		return "", err
	}
	if carry != ctfTestCarry || poolId != ctfTestPoolId {
		return "", errors.New("recycle of another carry")
	}
	if uint64(len(self.roots)) <= rootIndex {
		return "", fakeLedgerV2Revert("UnknownRoot")
	}
	if account != nil {
		root := self.roots[rootIndex]
		if account.Coldkey != self.carry.Coldkey {
			return "", fakeLedgerV2Revert("InvalidColdkey")
		}
		if !merkle.VerifyLedgerProof(root.Root, &root.Total, root.LeafCount, index, account.Coldkey, &account.Cumulative, proof) {
			return "", fakeLedgerV2Revert("InvalidProof")
		}
		claimed := self.claimed[[2]uint64{ctfTestPoolId, index}]
		if claimed.Lt(&account.Cumulative) {
			var due uint256.Int
			due.Sub(&account.Cumulative, &claimed)
			self.held.Add(&self.held, &due)
			self.claimed[[2]uint64{ctfTestPoolId, index}] = account.Cumulative
		}
	}
	if self.held.IsZero() {
		return "", fakeLedgerV2Revert("NothingToRecycle")
	}
	// The next close captures it with the epoch's emission.
	epoch := self.lastFunded + 1
	recycled := self.recycled[epoch]
	recycled.Add(&recycled, &self.held)
	self.recycled[epoch] = recycled
	self.emission[epoch] += self.held.Uint64()
	self.held.Clear()
	return fmt.Sprintf("0xrecycle%d", rootIndex), nil
}

// fakeCtfRecords keeps the driver's records in memory. The mirror's rows are
// copied from the fake chain by mirror.
type fakeCtfRecords struct {
	publishes   map[uint64][]*fakeCtfPublish
	commitments map[uint64]*model.CtfCensusCommitment
	draws       map[uint64]*model.CtfDraw
	results     map[uint64]*model.CtfEpochResult
	awards      map[uint64][]*model.CtfAward
	ledgers     map[[32]byte]*model.CtfLedger
	pending     *model.StLedgerV2Root
	roots       []*model.StLedgerV2Root
	demands     []*model.StLedgerV2Demand
	funding     map[uint64]*model.StLedgerV2Funding
	mirrored    uint64
	recorded    map[uint64]fakeCtfRecorded
	next        int
}

type fakeCtfPublish struct {
	id      server.Id
	kind    string
	outcome *StPublishOutcome
}

type fakeCtfRecorded struct {
	firstIndex  uint64
	newColdkeys [][32]byte
	amounts     []*model.StLedgerV2Amount
}

func newFakeCtfRecords() *fakeCtfRecords {
	return &fakeCtfRecords{publishes: map[uint64][]*fakeCtfPublish{}, commitments: map[uint64]*model.CtfCensusCommitment{}, draws: map[uint64]*model.CtfDraw{},
		results: map[uint64]*model.CtfEpochResult{}, awards: map[uint64][]*model.CtfAward{}, ledgers: map[[32]byte]*model.CtfLedger{},
		funding: map[uint64]*model.StLedgerV2Funding{}, recorded: map[uint64]fakeCtfRecorded{}}
}

func (self *fakeCtfRecords) Outcome(ctx context.Context, epoch uint64, kind string) (string, string) {
	status, reason := "", ""
	for _, publish := range self.publishes[epoch] {
		if publish.kind == kind && publish.outcome != nil {
			switch publish.outcome.Status {
			case model.StPublishStatusConfirmed, model.StPublishStatusSkipped, model.StPublishStatusMissed:
				status, reason = publish.outcome.Status, publish.outcome.Reason
			}
		}
	}
	return status, reason
}

func (self *fakeCtfRecords) Publish(ctx context.Context, epoch uint64, kind string) server.Id {
	id := server.NewId()
	self.publishes[epoch] = append(self.publishes[epoch], &fakeCtfPublish{id: id, kind: kind})
	return id
}

func (self *fakeCtfRecords) Resolve(ctx context.Context, publishId server.Id, outcome *StPublishOutcome) {
	for _, publishes := range self.publishes {
		for _, publish := range publishes {
			if publish.id == publishId {
				publish.outcome = outcome
			}
		}
	}
}

// rows returns "kind status" for an epoch's publish rows, in order.
func (self *fakeCtfRecords) rows(epoch uint64) []string {
	out := []string{}
	for _, publish := range self.publishes[epoch] {
		status := "pending"
		if publish.outcome != nil {
			status = publish.outcome.Status
		}
		out = append(out, publish.kind+" "+status)
	}
	return out
}

func (self *fakeCtfRecords) SetCommitment(ctx context.Context, commitment *model.CtfCensusCommitment) bool {
	existing := self.commitments[commitment.Epoch]
	if existing != nil && existing.CensusHash != commitment.CensusHash {
		return false
	}
	updated := *commitment
	if existing != nil {
		if updated.TxHash == "" {
			updated.TxHash = existing.TxHash
		}
		if updated.CommitBlock == nil {
			updated.CommitBlock, updated.CommitBlockTime, updated.Round = existing.CommitBlock, existing.CommitBlockTime, existing.Round
		}
	}
	self.commitments[commitment.Epoch] = &updated
	return true
}

func (self *fakeCtfRecords) CreateDraw(ctx context.Context, draw *model.CtfDraw) bool {
	if self.draws[draw.Epoch] != nil {
		return false
	}
	self.draws[draw.Epoch] = draw
	return true
}

func (self *fakeCtfRecords) EpochResults(ctx context.Context, first uint64, last uint64) map[uint64]*model.CtfEpochResult {
	out := map[uint64]*model.CtfEpochResult{}
	for k := first; k <= last; k++ {
		if result := self.results[k]; result != nil {
			out[k] = result
		}
	}
	return out
}

func (self *fakeCtfRecords) CreateEpochResult(ctx context.Context, result *model.CtfEpochResult, awards []*model.CtfAward, pots []*model.CtfPot, pending []*model.CtfPendingAward) bool {
	if self.results[result.Epoch] != nil {
		return false
	}
	if len(pots) != ctf.NumPots {
		panic(fmt.Sprintf("epoch %d result with %d pots", result.Epoch, len(pots)))
	}
	self.results[result.Epoch], self.awards[result.Epoch] = result, awards
	return true
}

func (self *fakeCtfRecords) CreateLedger(ctx context.Context, ledger *model.CtfLedger) bool {
	if self.ledgers[ledger.LedgerHash] != nil {
		return false
	}
	self.ledgers[ledger.LedgerHash] = ledger
	return true
}

func (self *fakeCtfRecords) SetLedgerStatus(ctx context.Context, epoch uint64, ledgerHash [32]byte, update *model.CtfLedgerStatusUpdate) bool {
	ledger := self.ledgers[ledgerHash]
	if ledger == nil || ledger.Epoch != epoch || update == nil {
		return false
	}
	ledger.Status = update.Status
	if update.Seq != nil {
		ledger.Seq = update.Seq
	}
	if update.RootIndex != nil {
		ledger.RootIndex = update.RootIndex
	}
	return true
}

func (self *fakeCtfRecords) PendingRoot(ctx context.Context, poolId uint64) *model.StLedgerV2Root {
	return self.pending
}

func (self *fakeCtfRecords) Root(ctx context.Context, poolId uint64, seq uint64) *model.StLedgerV2Root {
	for _, root := range self.roots {
		if root.Seq == seq {
			return root
		}
	}
	return nil
}

func (self *fakeCtfRecords) FinalizedRoot(ctx context.Context, poolId uint64, rootIndex uint64) *model.StLedgerV2Root {
	for _, root := range self.roots {
		if root.RootIndex != nil && *root.RootIndex == rootIndex {
			return root
		}
	}
	return nil
}

func (self *fakeCtfRecords) LatestFinalizedRoot(ctx context.Context, poolId uint64) *model.StLedgerV2Root {
	if len(self.roots) == 0 {
		return nil
	}
	return self.roots[len(self.roots)-1]
}

func (self *fakeCtfRecords) NextUnrecordedRoot(ctx context.Context, poolId uint64) *model.StLedgerV2Root {
	for _, root := range self.roots {
		if root.RecordedTime == nil {
			return root
		}
	}
	return nil
}

func (self *fakeCtfRecords) RootsFromEpoch(ctx context.Context, poolId uint64, epoch uint64) []*model.StLedgerV2Root {
	out := []*model.StLedgerV2Root{}
	for _, root := range append(slices.Clone(self.roots), self.pending) {
		if root != nil && epoch <= root.Epoch {
			out = append(out, root)
		}
	}
	return out
}

func (self *fakeCtfRecords) OpenDemands(ctx context.Context, poolId uint64, seq uint64) []*model.StLedgerV2Demand {
	return self.demands
}

func (self *fakeCtfRecords) Funding(ctx context.Context, poolId uint64, epoch uint64) *model.StLedgerV2Funding {
	return self.funding[epoch]
}

func (self *fakeCtfRecords) MirroredBlock(ctx context.Context) uint64 {
	return self.mirrored
}

func (self *fakeCtfRecords) RecordRoot(ctx context.Context, poolId uint64, seq uint64, firstIndex uint64, newColdkeys [][32]byte, amounts []*model.StLedgerV2Amount) error {
	root := self.Root(ctx, poolId, seq)
	if root == nil || root.RootIndex == nil {
		return fmt.Errorf("%w: seq %d is not a finalized root", model.ErrStLedgerV2Record, seq)
	}
	now := time.Now()
	root.RecordedTime = &now
	self.recorded[seq] = fakeCtfRecorded{firstIndex: firstIndex, newColdkeys: newColdkeys, amounts: amounts}
	return nil
}

// mirror copies the fake chain's roots into the records, as the event mirror
// would: the finalized roots at their finalization block, the pending root at
// its commit block.
func (self *fakeCtfRecords) mirror(chain *fakeCtfChain) {
	for i, root := range chain.roots {
		if i < len(self.roots) {
			continue
		}
		index, statusBlock := uint64(i), chain.head
		seq := uint64(i + 1)
		if self.pending != nil && self.pending.Epoch == root.Epoch {
			seq = self.pending.Seq
		}
		self.roots = append(self.roots, &model.StLedgerV2Root{NoId: ctfTestPoolId, Seq: seq, Epoch: root.Epoch, Root: root.Root, Total: root.Total,
			LeafCount: root.LeafCount, LedgerHash: root.LedgerHash, Status: model.StLedgerV2RootFinalized, StatusBlock: &statusBlock, RootIndex: &index})
	}
	self.pending = nil
	if p := chain.pending; p != nil {
		commit := p.CommittedBlock
		self.pending = &model.StLedgerV2Root{NoId: ctfTestPoolId, Seq: p.Seq, Epoch: p.Epoch, Root: p.Root, Total: p.Total, LeafCount: p.LeafCount,
			LedgerHash: p.LedgerHash, CommitBlock: &commit, Status: model.StLedgerV2RootPending}
	}
	for k, through := range chain.funded {
		if self.funding[k] == nil {
			self.funding[k] = &model.StLedgerV2Funding{NoId: ctfTestPoolId, Epoch: k, FundedThrough: through, BlockNumber: chain.head}
		}
	}
	self.mirrored = chain.head
}

// ctfTestCandidate is a candidate whose client id and unit are named by
// markers.
func ctfTestCandidate(role ctf.Role, country string, unit byte, client byte) ctf.Candidate {
	c, _ := ctf.ParseCountry(country)
	candidate := ctf.Candidate{Role: role, Country: c, Family: ctf.FamilyIPv4}
	candidate.Unit[0], candidate.Unit[31] = 0x0a, unit
	candidate.ClientID[0], candidate.ClientID[15] = 0x0c, client
	return candidate
}

// ctfTestColdkey is the payout coldkey of a test client.
func ctfTestColdkey(clientId [16]byte) [32]byte {
	var coldkey [32]byte
	coldkey[0] = 0xc0
	copy(coldkey[16:], clientId[:])
	return coldkey
}

// ctfTestCensusOf encodes a census of the test operator with every
// candidate's coldkey.
func ctfTestCensusOf(t testing.TB, epoch uint64, candidates ...ctf.Candidate) *StCtfCensus {
	t.Helper()
	encoded, err := ctf.EncodeCensus(&ctf.Census{NoID: ctfTestNoId, Epoch: epoch, Candidates: candidates, Excluded: []ctf.Exclusion{
		{Country: ctf.Country{'i', 's'}, Role: ctf.RoleProvider, Reason: ctf.ReasonPresence, Count: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	coldkeys := map[[16]byte][32]byte{}
	for _, candidate := range candidates {
		coldkeys[candidate.ClientID] = ctfTestColdkey(candidate.ClientID)
	}
	return &StCtfCensus{Epoch: epoch, Hash: encoded.Hash, Objects: encoded.Objects, Coldkeys: coldkeys, Published: true}
}

// The test census: three provider units in Iceland, one in Turkmenistan
// with two members, and two extender units in Germany.
func ctfTestCandidates() []ctf.Candidate {
	return []ctf.Candidate{
		ctfTestCandidate(ctf.RoleProvider, "is", 1, 1),
		ctfTestCandidate(ctf.RoleProvider, "is", 2, 2),
		ctfTestCandidate(ctf.RoleProvider, "is", 3, 3),
		ctfTestCandidate(ctf.RoleProvider, "tm", 4, 4),
		ctfTestCandidate(ctf.RoleProvider, "tm", 4, 5),
		ctfTestCandidate(ctf.RoleExtender, "de", 6, 6),
		ctfTestCandidate(ctf.RoleExtender, "de", 7, 7),
	}
}

// ctfTestEntryLine is the amendment entry v2 the test deployment configures.
func ctfTestEntryLine(t testing.TB, fromEpoch uint64) string {
	t.Helper()
	line, err := amendment.NewCaptureTheFlag(945, 25, ctfTestNoId, fromEpoch, ctfTestCarry).Line()
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// ctfFixture is a configured live driver over the fakes.
type ctfFixture struct {
	t       testing.TB
	ctx     context.Context
	cfg     *StConfig
	log     []string
	chain   *fakeCtfChain
	records *fakeCtfRecords
	census  map[uint64]*StCtfCensus
	alerts  *[]string
	bound   map[[16]byte]bool
}

func newCtfFixture(t testing.TB, mode StCtfMode) *ctfFixture {
	f := &ctfFixture{t: t, ctx: context.Background(), cfg: ledgerV2TestConfig(), records: newFakeCtfRecords(), census: map[uint64]*StCtfCensus{}, bound: map[[16]byte]bool{}}
	f.cfg.Netuid = 25
	f.cfg.Ctf = StCtfConfig{Mode: mode, CarryAddress: ctfTestCarry, PotCaptureAlertRao: stCtfDefaultPotCaptureAlertRao, DrandUrls: []string{"http://drand.invalid"},
		AmendmentLines: []string{ctfTestEntryLine(t, 8)}}
	f.chain = newFakeCtfChain(&f.log)
	SetStConfig(f.cfg)
	SetStLedgerV2Client(f.chain)
	SetStCtfClient(f.chain)
	setStCtfRecords(f.records)
	SetStLedgerV2ObjectStore(server.NewLocalBlobStore(t.TempDir(), "test"))
	signer, admission, bound, source := stCtfAmendmentSigner, stCtfAdmission, stCtfBoundFor, stCtfCensusSource
	stCtfCensusSource = func(ctx context.Context, epoch uint64) (*StCtfCensus, error) {
		return f.census[epoch], nil
	}
	stCtfAmendmentSigner = func(uint64, uint16) (common.Address, bool) { return common.Address{}, false }
	stCtfAdmission = func(context.Context, *StConfig) error { return nil }
	stCtfBoundFor = func(ctx context.Context, cfg *StConfig, epoch uint64, clientIds [][16]byte) (map[[16]byte]bool, error) {
		return f.bound, nil
	}
	resetCtfCaches()
	for round := range ctfTestPulses {
		stCtfPulses.Store(round, ctfTestPulse(t, round))
	}
	f.alerts = captureLedgerV2Alerts(t)
	t.Cleanup(func() {
		SetStConfig(nil)
		SetStLedgerV2Client(nil)
		SetStCtfClient(nil)
		setStCtfRecords(nil)
		SetStLedgerV2ObjectStore(nil)
		stCtfAmendmentSigner, stCtfAdmission, stCtfBoundFor, stCtfCensusSource = signer, admission, bound, source
		resetCtfCaches()
	})
	return f
}

func resetCtfCaches() {
	resetLedgerV2Caches()
	stCtfPools.Clear()
	stCtfCarries.Clear()
	stCtfPulses.Clear()
	stCtfAmendmentHolders.Clear()
	stCtfTreeLock.Lock()
	stCtfTrees, stCtfTreeOrder = map[[32]byte]*stLedgerV2Tree{}, nil
	stCtfTreeLock.Unlock()
}

func (self *ctfFixture) run(kind string, epoch uint64) *StPublishOutcome {
	self.t.Helper()
	return self.runStep(&StCtfDriverStep{Kind: kind, Epoch: epoch})
}

func (self *ctfFixture) runStep(step *StCtfDriverStep) *StPublishOutcome {
	self.t.Helper()
	outcome, err := StCtfDriverRunStep(self.ctx, step)
	if err != nil {
		self.t.Fatalf("%s step: %v", step.Kind, err)
	}
	return outcome
}

func (self *ctfFixture) takeLog() []string {
	log := self.log
	self.log = nil
	return log
}

func (self *ctfFixture) sync(epoch uint64) []string {
	self.t.Helper()
	steps, err := StCtfDriverSync(self.ctx, &StEpochState{Epoch: epoch, HeadBlock: self.chain.head, HeadBlockTime: time.Now().UTC()})
	if err != nil {
		self.t.Fatal(err)
	}
	out := []string{}
	for _, step := range steps {
		out = append(out, fmt.Sprintf("%s %d", step.Kind, step.Epoch))
	}
	return out
}

// commitRound fixes the draw round of an epoch's coming census commitment.
func (self *ctfFixture) commitRound(epoch uint64, round uint64) {
	self.chain.roundOf[epoch] = round
}

// verify audits the CTF pool's pending root as the validator does, from the
// fake chain's facts and the published objects.
func (self *ctfFixture) verify(previous *ledger.Root) *ctf.Result {
	self.t.Helper()
	chain := self.chain
	pending := chain.pending
	in := ctf.VerifyInput{NoID: ctfTestNoId, ChainID: self.cfg.ChainId, Netuid: uint16(self.cfg.Netuid), FirstEpoch: chain.firstEpoch,
		CarryColdkey: stCtfCarryColdkey(ctfTestCarry), Previous: previous, FundedThrough: map[uint64]uint256.Int{}, Epochs: map[uint64]ctf.EpochFacts{},
		Pending: ledger.Root{Epoch: pending.Epoch, Root: pending.Root, Total: pending.Total, LeafCount: pending.LeafCount, LedgerHash: pending.LedgerHash},
		Fetcher: stCtfObjectFetcher{}, Custody: ledger.NewMemoryCustody(), MaxFetchBytes: 1 << 30}
	first := chain.firstEpoch
	if previous != nil {
		first = previous.Epoch + 1
	}
	for k := first; k <= pending.Epoch; k++ {
		in.FundedThrough[k] = chain.funded[k]
		facts := ctf.EpochFacts{Recycled: chain.recycled[k]}
		if census := chain.census[k]; census.hash != [32]byte{} {
			at, _ := chain.CtfBlockTime(self.ctx, census.block)
			facts.Census = &ctf.CensusCommitment{Hash: census.hash, Block: census.block, Time: at}
			facts.Pulse = ctfTestPulse(self.t, ctf.DrawRound(at))
		}
		in.Epochs[k] = facts
	}
	result, err := ctf.Verify(self.ctx, in)
	if err != nil {
		self.t.Fatal(err)
	}
	return result
}
