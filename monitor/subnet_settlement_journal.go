// The §22.7 settlement event journal: an incremental, retained completeness
// frontier over the vault's finalized logs. The replay must provably start
// before the vault existed (no code at the block before the enrolled start),
// every log block must be the canonical block at its height, and the frontier
// never passes the run's finalized checkpoint. While it is behind, every
// event-dependent class stays unknown; a complete replay reconstructs the
// counters independently and compares them with the getters.
package monitor

import (
	"context"
	"math/big"
	"sort"
	"strconv"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/urfoundation/sn/stabi"
)

// Entitlement coordinates.
type subnetSettlementKey struct {
	epoch uint64
	noId  uint64
}

// One accepted claim leaf: coordinates plus claimant.
type subnetSettlementClaimKey struct {
	key     subnetSettlementKey
	coldkey [32]byte
}

// An event-level counterexample proved while replaying, with coordinates.
type subnetSettlementJournalIssue struct {
	issueClass string
	target     string
	frame      string
	invariant  string
	expected   string
	observed   string
	block      uint64
}

// Replay accumulators. All values are exact; claimant keys stay private.
type subnetSettlementReplay struct {
	captureEvents  map[subnetSettlementKey]int
	deferEvents    map[subnetSettlementKey]int
	deferTx        map[subnetSettlementKey]common.Hash
	dustEpochs     map[subnetSettlementKey]bool
	capturedAmount map[subnetSettlementKey]*big.Int
	capturedTotal  *big.Int
	finalizedTotal map[subnetSettlementKey]*big.Int
	claimedAmount  map[subnetSettlementKey]*big.Int
	claimKeys      map[subnetSettlementClaimKey]bool
	credit         map[[32]byte]*big.Int
	paidTotal      *big.Int
	carry          map[uint64]*big.Int
	coordinatorSet bool
	issues         []subnetSettlementJournalIssue
}

func newSubnetSettlementReplay() *subnetSettlementReplay {
	return &subnetSettlementReplay{
		captureEvents: map[subnetSettlementKey]int{}, deferEvents: map[subnetSettlementKey]int{},
		deferTx: map[subnetSettlementKey]common.Hash{}, dustEpochs: map[subnetSettlementKey]bool{},
		capturedAmount: map[subnetSettlementKey]*big.Int{}, capturedTotal: new(big.Int),
		finalizedTotal: map[subnetSettlementKey]*big.Int{}, claimedAmount: map[subnetSettlementKey]*big.Int{},
		claimKeys: map[subnetSettlementClaimKey]bool{}, credit: map[[32]byte]*big.Int{},
		paidTotal: new(big.Int), carry: map[uint64]*big.Int{},
	}
}

// The frontier and its accumulators for one enrolled vault and start block.
// Safe for concurrent use; the lock is never held across a read.
type subnetSettlementJournal struct {
	stateLock sync.Mutex
	vault     common.Address
	fromBlock uint64
	proven    bool
	nextBlock uint64
	replay    *subnetSettlementReplay
}

func newSubnetSettlementJournal() *subnetSettlementJournal {
	return &subnetSettlementJournal{}
}

// The frontier position for this run, resetting on a changed enrollment.
func (self *subnetSettlementJournal) Position(vault common.Address, fromBlock uint64) (proven bool, nextBlock uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.vault != vault || self.fromBlock != fromBlock {
		self.vault, self.fromBlock, self.proven, self.nextBlock = vault, fromBlock, false, fromBlock
		self.replay = newSubnetSettlementReplay()
	}
	return self.proven, self.nextBlock
}

// Records that the enrolled start precedes the vault's deployment.
func (self *subnetSettlementJournal) Prove(vault common.Address, fromBlock uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.vault == vault && self.fromBlock == fromBlock {
		self.proven = true
	}
}

// Commits contiguous, canonical, ordered windows after the run's checkpoint
// recheck and advances the frontier. Windows that do not start at the
// frontier are discarded, so a concurrent or stale run cannot double apply.
func (self *subnetSettlementJournal) Apply(vault common.Address, fromBlock uint64, windows []*subnetReplayLogWindow, decoder *subnetSettlementDecoder) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.vault != vault || self.fromBlock != fromBlock || !self.proven || len(windows) == 0 || self.nextBlock != windows[0].from {
		return false
	}
	for i, window := range windows {
		if 0 < i && windows[i-1].to+1 != window.from {
			return false
		}
	}
	for _, window := range windows {
		for i := range window.logs {
			decoder.apply(self.replay, &window.logs[i])
		}
		self.nextBlock = window.to + 1
	}
	return true
}

// A consistent copy of the replay for evaluation outside the lock.
func (self *subnetSettlementJournal) Snapshot() (uint64, *subnetSettlementReplay) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.replay == nil {
		return self.nextBlock, newSubnetSettlementReplay()
	}
	return self.nextBlock, self.replay.clone()
}

func (self *subnetSettlementReplay) clone() *subnetSettlementReplay {
	copy := newSubnetSettlementReplay()
	for key, value := range self.captureEvents {
		copy.captureEvents[key] = value
	}
	for key, value := range self.deferEvents {
		copy.deferEvents[key] = value
	}
	for key, value := range self.deferTx {
		copy.deferTx[key] = value
	}
	for key, value := range self.dustEpochs {
		copy.dustEpochs[key] = value
	}
	for key, value := range self.capturedAmount {
		copy.capturedAmount[key] = new(big.Int).Set(value)
	}
	copy.capturedTotal.Set(self.capturedTotal)
	for key, value := range self.finalizedTotal {
		copy.finalizedTotal[key] = new(big.Int).Set(value)
	}
	for key, value := range self.claimedAmount {
		copy.claimedAmount[key] = new(big.Int).Set(value)
	}
	for key, value := range self.claimKeys {
		copy.claimKeys[key] = value
	}
	for key, value := range self.credit {
		copy.credit[key] = new(big.Int).Set(value)
	}
	copy.paidTotal.Set(self.paidTotal)
	for key, value := range self.carry {
		copy.carry[key] = new(big.Int).Set(value)
	}
	copy.coordinatorSet = self.coordinatorSet
	copy.issues = append([]subnetSettlementJournalIssue(nil), self.issues...)
	return copy
}

// Decodes vault events by topic with the pinned binding. Unknown topics are
// ignored; a known topic that does not decode is a journal issue.
type subnetSettlementDecoder struct {
	binding *stabi.STSettlementVault
	topics  map[common.Hash]string
	shapes  map[string]subnetSettlementEventShape
	minimum uint64
}

// Exact log shape of one event: every vault event input is one static word.
type subnetSettlementEventShape struct {
	topics    int
	dataBytes int
}

func newSubnetSettlementDecoder(contracts subnetSettlementContracts, minimumTransferTaoRao uint64) *subnetSettlementDecoder {
	topics := map[common.Hash]string{}
	shapes := map[string]subnetSettlementEventShape{}
	for _, name := range []string{"CoordinatorFixed", "EmissionCaptured", "EmissionDeferred", "EmissionDustDeferred", "EntitlementFinalized", "RootMissed", "Claimed", "ClaimPaymentDeferred", "ClaimPaid", "EntitlementExpired"} {
		event := contracts.vault.Events[name]
		topics[event.ID] = name
		shape := subnetSettlementEventShape{topics: 1}
		for _, input := range event.Inputs {
			if input.Indexed {
				shape.topics += 1
			} else {
				shape.dataBytes += 32
			}
		}
		shapes[name] = shape
	}
	return &subnetSettlementDecoder{binding: stabi.NewSTSettlementVault(), topics: topics, shapes: shapes, minimum: minimumTransferTaoRao}
}

func (self *subnetSettlementDecoder) apply(replay *subnetSettlementReplay, log *types.Log) {
	name, known := self.topics[log.Topics[0]]
	if !known {
		return
	}
	shape := self.shapes[name]
	issue := func(issueClass, target, frame, invariant, expected, observed string) {
		replay.issues = append(replay.issues, subnetSettlementJournalIssue{issueClass: issueClass, target: target, frame: frame, invariant: invariant, expected: expected, observed: observed, block: log.BlockNumber})
	}
	malformed := func() {
		issue("subnet-vault-conservation", "subnet-settlement/vault", "journal-decode", "every-known-vault-event-decodes", "decodable-event", name)
	}
	key := func(epoch, noId *big.Int) (subnetSettlementKey, bool) {
		if !epoch.IsUint64() || !noId.IsUint64() {
			return subnetSettlementKey{}, false
		}
		return subnetSettlementKey{epoch: epoch.Uint64(), noId: noId.Uint64()}, true
	}
	frameOf := func(coordinates subnetSettlementKey, condition string) string {
		return subnetSettlementEpochFrame(coordinates.epoch, condition)
	}
	// The binding skips empty data, which would leave nil amounts; the exact
	// shape check refuses that before any field is read.
	if len(log.Topics) != shape.topics || len(log.Data) != shape.dataBytes {
		malformed()
		return
	}
	switch name {
	case "CoordinatorFixed":
		replay.coordinatorSet = true
	case "EmissionCaptured":
		event, err := self.binding.UnpackEmissionCapturedEvent(log)
		if err != nil {
			malformed()
			return
		}
		coordinates, ok := key(event.Epoch, event.NoId)
		if !ok {
			malformed()
			return
		}
		replay.captureEvents[coordinates] += 1
		if 1 < replay.captureEvents[coordinates]+replay.deferEvents[coordinates] {
			issue("subnet-capture-delta", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "duplicate-capture"), "one-capture-or-deferral-per-epoch", "1", strconv.Itoa(replay.captureEvents[coordinates]+replay.deferEvents[coordinates]))
		}
		replay.capturedAmount[coordinates] = new(big.Int).Set(event.Amount)
		replay.capturedTotal.Add(replay.capturedTotal, event.Amount)
	case "EmissionDeferred":
		event, err := self.binding.UnpackEmissionDeferredEvent(log)
		if err != nil {
			malformed()
			return
		}
		coordinates, ok := key(event.Epoch, event.NoId)
		if !ok {
			malformed()
			return
		}
		replay.deferEvents[coordinates] += 1
		replay.deferTx[coordinates] = log.TxHash
		if 1 < replay.captureEvents[coordinates]+replay.deferEvents[coordinates] {
			issue("subnet-capture-delta", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "duplicate-capture"), "one-capture-or-deferral-per-epoch", "1", strconv.Itoa(replay.captureEvents[coordinates]+replay.deferEvents[coordinates]))
		}
		if _, ok := replay.capturedAmount[coordinates]; !ok {
			replay.capturedAmount[coordinates] = new(big.Int)
		}
	case "EmissionDustDeferred":
		event, err := self.binding.UnpackEmissionDustDeferredEvent(log)
		if err != nil {
			malformed()
			return
		}
		coordinates, ok := key(event.Epoch, event.NoId)
		if !ok {
			malformed()
			return
		}
		// The dust path emits its deferral first in the same transaction.
		if replay.deferTx[coordinates] != log.TxHash {
			issue("subnet-capture-delta", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "dust-without-deferral"), "dust-deferral-pairs-with-zero-funding", "same-transaction-deferral", "absent")
		}
		floor := new(big.Int).SetUint64(event.MinimumTransferTaoRao)
		if event.TaoEquivalentRao.Cmp(floor) >= 0 || event.MinimumTransferTaoRao != self.minimum {
			issue("subnet-capture-delta", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "dust-above-floor"), "taoEquivalent<minimumTransferTaoRao", floor.String(), event.TaoEquivalentRao.String())
		}
		replay.dustEpochs[coordinates] = true
	case "EntitlementFinalized":
		event, err := self.binding.UnpackEntitlementFinalizedEvent(log)
		if err != nil {
			malformed()
			return
		}
		coordinates, ok := key(event.Epoch, event.NoId)
		if !ok {
			malformed()
			return
		}
		if _, duplicate := replay.finalizedTotal[coordinates]; duplicate {
			issue("subnet-payout-root", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "duplicate-finalization"), "one-finalization-per-epoch", "1", "2")
		}
		replay.finalizedTotal[coordinates] = new(big.Int).Set(event.Total)
		funded := replay.capturedAmount[coordinates]
		if funded == nil {
			funded = new(big.Int)
		}
		consumed := new(big.Int).Sub(event.Total, funded)
		carry := replay.carryOf(coordinates.noId)
		if consumed.Cmp(carry) != 0 {
			issue("subnet-carry-isolation", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "finalized-carry"), "finalized-total=funded+prior-same-operator-carry", carry.String(), consumed.String())
		}
		carry.SetInt64(0)
	case "RootMissed":
		event, err := self.binding.UnpackRootMissedEvent(log)
		if err != nil {
			malformed()
			return
		}
		coordinates, ok := key(event.Epoch, event.NoId)
		if !ok {
			malformed()
			return
		}
		funded := replay.capturedAmount[coordinates]
		if funded == nil {
			funded = new(big.Int)
		}
		if event.Carried.Cmp(funded) != 0 {
			issue("subnet-carry-isolation", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "missed-carry"), "root-missed-carries-exact-funding", funded.String(), event.Carried.String())
		}
		replay.carryOf(coordinates.noId).Add(replay.carryOf(coordinates.noId), event.Carried)
	case "EntitlementExpired":
		event, err := self.binding.UnpackEntitlementExpiredEvent(log)
		if err != nil {
			malformed()
			return
		}
		coordinates, ok := key(event.Epoch, event.NoId)
		if !ok {
			malformed()
			return
		}
		carry := replay.carryOf(coordinates.noId)
		carry.Add(carry, event.Unclaimed)
		if event.OperatorCarry.Cmp(carry) != 0 {
			issue("subnet-carry-isolation", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "expired-carry"), "expiry-adds-remainder-to-same-operator-carry", carry.String(), event.OperatorCarry.String())
		}
	case "Claimed":
		event, err := self.binding.UnpackClaimedEvent(log)
		if err != nil {
			malformed()
			return
		}
		coordinates, ok := key(event.Epoch, event.NoId)
		if !ok {
			malformed()
			return
		}
		leaf := subnetSettlementClaimKey{key: coordinates, coldkey: event.Coldkey}
		if replay.claimKeys[leaf] {
			issue("subnet-claim-correctness", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "duplicate-claim"), "one-claim-per-leaf", "1", "2")
		}
		replay.claimKeys[leaf] = true
		total, finalized := replay.finalizedTotal[coordinates]
		if !finalized {
			issue("subnet-claim-correctness", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "claim-before-finalization"), "claims-follow-finalization", "finalized", "not-finalized")
		} else if event.ShareBps.Sign() <= 0 || 0 < event.ShareBps.Cmp(big.NewInt(subnetSettlementBps)) {
			issue("subnet-claim-correctness", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "share-out-of-range"), "0<shareBps<=10000", "within-range", "out-of-range")
		} else {
			expected := new(big.Int).Div(new(big.Int).Mul(total, event.ShareBps), big.NewInt(subnetSettlementBps))
			if expected.Cmp(event.Amount) != 0 {
				issue("subnet-claim-correctness", subnetSettlementOperatorTarget(coordinates.noId), frameOf(coordinates, "claim-amount"), "amount=floor(total*shareBps/10000)", "exact-share", "different-amount")
			}
		}
		claimed, ok := replay.claimedAmount[coordinates]
		if !ok {
			claimed = new(big.Int)
			replay.claimedAmount[coordinates] = claimed
		}
		claimed.Add(claimed, event.Amount)
		replay.creditOf(event.Coldkey).Add(replay.creditOf(event.Coldkey), event.Amount)
	case "ClaimPaymentDeferred":
		event, err := self.binding.UnpackClaimPaymentDeferredEvent(log)
		if err != nil {
			malformed()
			return
		}
		if event.CreditAlphaRao.Cmp(replay.creditOf(event.Coldkey)) != 0 {
			issue("subnet-claim-credit-payment", "subnet-settlement/vault", "deferral-credit", "deferred-amount=accumulated-claimant-credit", "accumulated-credit", "different-amount")
		}
	case "ClaimPaid":
		event, err := self.binding.UnpackClaimPaidEvent(log)
		if err != nil {
			malformed()
			return
		}
		credit := replay.creditOf(event.Coldkey)
		if event.Amount.Sign() <= 0 || event.Amount.Cmp(credit) != 0 {
			issue("subnet-claim-credit-payment", "subnet-settlement/vault", "payment-credit", "paid-amount=accumulated-claimant-credit", "accumulated-credit", "different-amount")
		}
		credit.SetInt64(0)
		replay.paidTotal.Add(replay.paidTotal, event.Amount)
	}
}

func (self *subnetSettlementReplay) carryOf(noId uint64) *big.Int {
	value, ok := self.carry[noId]
	if !ok {
		value = new(big.Int)
		self.carry[noId] = value
	}
	return value
}

func (self *subnetSettlementReplay) creditOf(coldkey [32]byte) *big.Int {
	value, ok := self.credit[coldkey]
	if !ok {
		value = new(big.Int)
		self.credit[coldkey] = value
	}
	return value
}

// Proves the enrolled start precedes the vault: the canonical block before
// it has no vault code. Pinned by that block's hash with requireCanonical.
func proveSubnetSettlementJournalStart(ctx context.Context, rpc *subnetReplayRpc, address common.Address, fromBlock uint64) error {
	if fromBlock == 0 {
		return newSubnetReplayUnknown("journal-start-unproven", nil)
	}
	var before *subnetReplayRpcBlock
	call := &subnetReplayRpcCall{method: "eth_getBlockByNumber", params: []any{hexutil.EncodeUint64(fromBlock - 1), false}, result: &before}
	if err := rpc.call(ctx, call); err != nil {
		return err
	}
	if err := subnetReplayRpcFailed(call); err != nil {
		return err
	}
	identity, ok := before.identity()
	if !ok || identity.number != fromBlock-1 {
		return newSubnetReplayUnknown("journal-start-unproven", nil)
	}
	var code *hexutil.Bytes
	codeCall := &subnetReplayRpcCall{method: "eth_getCode", params: []any{address, map[string]any{"blockHash": identity.hash, "requireCanonical": true}}, result: &code}
	if err := rpc.call(ctx, codeCall); err != nil {
		return err
	}
	if err := subnetReplayRpcFailed(codeCall); err != nil {
		return err
	}
	if code == nil || len(*code) != 0 {
		return newSubnetReplayUnknown("journal-start-after-deployment", nil)
	}
	return nil
}

// Reads the next bounded windows toward the checkpoint and verifies every
// log block is canonical. Nothing is committed here: the caller applies the
// windows to a snapshot for evaluation and commits them only after the
// checkpoint recheck, so the retained cursor never advances past unproven
// history.
func readSubnetSettlementJournalWindows(ctx context.Context, rpc *subnetReplayRpc, journal *subnetSettlementJournal, address common.Address, fromBlock uint64, checkpoint subnetReplayEvmCheckpoint, settings *SubnetReplaySettings) ([]*subnetReplayLogWindow, error) {
	proven, next := journal.Position(address, fromBlock)
	if checkpoint.number+1 < next {
		return nil, newSubnetReplayUnknown("finality-regressed", nil)
	}
	if !proven {
		if err := proveSubnetSettlementJournalStart(ctx, rpc, address, fromBlock); err != nil {
			return nil, err
		}
		journal.Prove(address, fromBlock)
	}
	windows := []*subnetReplayLogWindow{}
	for start := next; start <= checkpoint.number && len(windows) < settings.LogMaximumRangesPerRun; start += settings.LogRangeBlocks {
		end := start + settings.LogRangeBlocks - 1
		if checkpoint.number < end {
			end = checkpoint.number
		}
		windows = append(windows, &subnetReplayLogWindow{from: start, to: end})
	}
	if len(windows) == 0 {
		return nil, nil
	}
	if err := readSubnetReplayLogs(ctx, rpc, address, settings.LogMaximumEntriesPerRange, windows...); err != nil {
		return nil, err
	}
	if err := verifySubnetReplayLogBlocks(ctx, rpc, windows...); err != nil {
		return nil, err
	}
	return windows, nil
}

// Every log must come from the canonical block at its height.
func verifySubnetReplayLogBlocks(ctx context.Context, rpc *subnetReplayRpc, windows ...*subnetReplayLogWindow) error {
	expected := map[uint64]common.Hash{}
	for _, window := range windows {
		for _, log := range window.logs {
			if hash, ok := expected[log.BlockNumber]; ok && hash != log.BlockHash {
				return newSubnetReplayUnknown("log-block-conflict", nil)
			}
			expected[log.BlockNumber] = log.BlockHash
		}
	}
	numbers := make([]uint64, 0, len(expected))
	for number := range expected {
		numbers = append(numbers, number)
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	blocks := make([]*subnetReplayRpcBlock, len(numbers))
	calls := make([]*subnetReplayRpcCall, len(numbers))
	for i, number := range numbers {
		calls[i] = &subnetReplayRpcCall{method: "eth_getBlockByNumber", params: []any{hexutil.EncodeUint64(number), false}, result: &blocks[i]}
	}
	if err := rpc.call(ctx, calls...); err != nil {
		return err
	}
	if err := subnetReplayRpcFailed(calls...); err != nil {
		return err
	}
	for i, number := range numbers {
		identity, ok := blocks[i].identity()
		if !ok || identity.number != number || identity.hash != expected[number] {
			return newSubnetReplayUnknown("log-block-not-canonical", nil)
		}
	}
	return nil
}

// Converts replay counterexamples and compares the reconstructed counters with
// the state census read at the same checkpoint. Claimant credit is compared
// through bounded claimCredit views supplied by the caller in coldkey order.
func evaluateSubnetSettlementJournal(state *subnetSettlementState, replay *subnetSettlementReplay, credits map[[32]byte]*big.Int, settings *SubnetReplaySettings, scope subnetReplayScope) []finding {
	findings := []finding{}
	journalIssue := func(target, frame, invariant, expected, observed string) subnetReplayIssue {
		return subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: invariant, expected: expected, observed: observed,
			symptom:   "The vault's finalized event journal contradicts a settlement invariant",
			mechanism: "Replaying every finalized vault event from before deployment reconstructs capture, finalization, claim, payment, expiry and carry exactly; this event sequence violates the named invariant.",
			baseline:  "Each epoch is captured or deferred once, carry moves only within one operator, each leaf is claimed once at its exact share, and each payment equals the claimant's accumulated credit.",
			action:    "Preserve the receipts at the named coordinates and route to custody incident response; never resend, reverse or correct a transaction to clear this alert.",
			verify:    "Independent replay from deployment attributes the event and a recorded disposition closes it.",
		}
	}
	for _, issue := range replay.issues {
		frame := issue.frame
		value := journalIssue(issue.target, frame, issue.invariant, issue.expected, issue.observed)
		value.deadline = "event_block=" + strconv.FormatUint(issue.block, 10)
		switch issue.issueClass {
		case "subnet-capture-delta":
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-capture-delta", issue.target, frame, scope, subnetSettlementPlaybook, value))
		case "subnet-carry-isolation":
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-carry-isolation", issue.target, frame, scope, subnetSettlementPlaybook, value))
		case "subnet-claim-correctness":
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-claim-correctness", issue.target, frame, scope, subnetSettlementPlaybook, value))
		case "subnet-claim-credit-payment":
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-claim-credit-payment", issue.target, frame, scope, subnetSettlementPlaybook, value))
		case "subnet-payout-root":
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-payout-root", issue.target, frame, scope, subnetSettlementPlaybook, value))
		default:
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-vault-conservation", issue.target, frame, scope, subnetSettlementPlaybook, value))
		}
	}
	if !replay.coordinatorSet {
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-vault-conservation", "subnet-settlement/vault", "journal-coordinator", scope, subnetSettlementPlaybook, journalIssue("subnet-settlement/vault", "journal-coordinator", "journal-contains-coordinator-fixed", "1", "0")))
	}
	if replay.capturedTotal.Cmp(state.vault.totalCaptured) != 0 {
		value := journalIssue("subnet-settlement/vault", "journal-captured", "totalCaptured=sum(EmissionCaptured.amount)", replay.capturedTotal.String(), state.vault.totalCaptured.String())
		value.delta = subnetReplayDelta(replay.capturedTotal, state.vault.totalCaptured)
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-vault-conservation", "subnet-settlement/vault", "journal-captured", scope, subnetSettlementPlaybook, value))
	}
	if replay.paidTotal.Cmp(state.vault.totalPaid) != 0 {
		value := journalIssue("subnet-settlement/vault", "journal-paid", "totalPaid=sum(ClaimPaid.amount)", replay.paidTotal.String(), state.vault.totalPaid.String())
		value.delta = subnetReplayDelta(replay.paidTotal, state.vault.totalPaid)
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-claim-credit-payment", "subnet-settlement/vault", "journal-paid", scope, subnetSettlementPlaybook, value))
	}
	for _, entry := range state.entries {
		coordinates := subnetSettlementKey{epoch: entry.epoch, noId: entry.noId}
		target := subnetSettlementOperatorTarget(entry.noId)
		value := entry.entitlement
		events := replay.captureEvents[coordinates] + replay.deferEvents[coordinates]
		if value.Status != subnetSettlementUnset && events == 0 {
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-capture-delta", target, subnetSettlementEpochFrame(entry.epoch, "funded-without-event"), scope, subnetSettlementPlaybook, journalIssue(target, "", "status>=funded-requires-capture-or-deferral-event", "1", "0")))
		}
		if value.Status == subnetSettlementUnset && 0 < events {
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-capture-delta", target, subnetSettlementEpochFrame(entry.epoch, "event-without-funding"), scope, subnetSettlementPlaybook, journalIssue(target, "", "capture-event-sets-funded-status", "status>=funded", "status=unset")))
		}
		if captured, ok := replay.capturedAmount[coordinates]; ok && captured.Cmp(value.Funded) != 0 {
			issue := journalIssue(target, "", "funded=EmissionCaptured.amount", captured.String(), value.Funded.String())
			issue.delta = subnetReplayDelta(captured, value.Funded)
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-capture-delta", target, subnetSettlementEpochFrame(entry.epoch, "captured-amount"), scope, subnetSettlementPlaybook, issue))
		}
		claimed := replay.claimedAmount[coordinates]
		if claimed == nil {
			claimed = new(big.Int)
		}
		if claimed.Cmp(value.Claimed) != 0 {
			issue := journalIssue(target, "", "claimed=sum(Claimed.amount)", claimed.String(), value.Claimed.String())
			issue.delta = subnetReplayDelta(claimed, value.Claimed)
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-claim-correctness", target, subnetSettlementEpochFrame(entry.epoch, "journal-claimed"), scope, subnetSettlementPlaybook, issue))
		}
	}
	for _, operator := range state.operators {
		carry := replay.carry[operator.noId]
		if carry == nil {
			carry = new(big.Int)
		}
		if carry.Cmp(operator.carry) != 0 {
			target := subnetSettlementOperatorTarget(operator.noId)
			issue := journalIssue(target, "", "carry(no)=replayed-same-operator-carry", carry.String(), operator.carry.String())
			issue.delta = subnetReplayDelta(carry, operator.carry)
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-carry-isolation", target, "journal-carry", scope, subnetSettlementPlaybook, issue))
		}
		// Persistent dust deferral: count the newest closed epochs of this
		// operator, skipping unclosed ones, until one was not dust-deferred.
		consecutive := 0
		for i := len(state.entries) - 1; 0 <= i; i-- {
			entry := state.entries[i]
			if entry.noId != operator.noId || entry.entitlement.Status == subnetSettlementUnset {
				continue
			}
			if !replay.dustEpochs[subnetSettlementKey{epoch: entry.epoch, noId: operator.noId}] {
				break
			}
			consecutive += 1
		}
		if 0 < settings.DustDeferralWarnEpochs && settings.DustDeferralWarnEpochs <= consecutive {
			target := subnetSettlementOperatorTarget(operator.noId)
			findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-capture-delta", target, "persistent-dust-deferral", scope, subnetSettlementPlaybook, subnetReplayIssue{
				tier: tierWarn, sustain: 2, invariant: "dust-deferral-is-transient",
				expected: "fewer-than-" + strconv.Itoa(settings.DustDeferralWarnEpochs) + "-consecutive-dust-epochs", observed: strconv.Itoa(consecutive) + "-consecutive-dust-epochs",
				symptom:   "An operator's pool emission has been deferred as dust for consecutive epochs",
				mechanism: "A capture below the runtime transfer floor records zero funding and leaves the stake on the pool for the next timely boundary; persistent deferral after the floor should be feasible needs an explanation.",
				baseline:  "Dust deferral is occasional; accumulated pool stake crosses the transfer floor and is captured.",
				action:    "Compare the pool stake and alpha price at the deferral boundaries with the immutable transfer floor; investigate before assuming low demand.",
				verify:    "A later epoch of this operator captures its accumulated stake.",
			}))
		}
	}
	coldkeys := make([][32]byte, 0, len(replay.credit))
	for coldkey, credit := range replay.credit {
		if credit.Sign() != 0 {
			coldkeys = append(coldkeys, coldkey)
		}
	}
	mismatches := 0
	for _, coldkey := range coldkeys {
		observed, ok := credits[coldkey]
		if ok && observed.Cmp(replay.credit[coldkey]) != 0 {
			mismatches += 1
		}
	}
	if 0 < mismatches {
		findings = append(findings, subnetReplayFinding(subnetSettlementProbeId, "subnet-claim-credit-payment", "subnet-settlement/vault", "claimant-credit", scope, subnetSettlementPlaybook, subnetReplayIssue{
			tier: tierPage, sustain: 1, invariant: "claimCredit(coldkey)=replayed-unpaid-credit",
			expected: "0-claimants-differ", observed: strconv.Itoa(mismatches) + "-claimants-differ",
			symptom:   "Durable claim credit differs from the replayed unpaid credit of some claimants",
			mechanism: "Claimed credit survives expiry and is permissionlessly retryable until paid; a getter that differs from the replay means credit was lost, redirected or paid without a ClaimPaid event.",
			baseline:  "Every claimant's claimCredit equals its replayed claims minus its replayed payments.",
			action:    "Preserve the claimant receipts privately (claimant identities stay restricted) and route to custody incident response.",
			verify:    "Independent replay attributes each difference and a recorded disposition closes it.",
		}))
	}
	return findings
}

// The bounded claimant set whose current credit is compared, in a stable order.
func subnetSettlementCreditColdkeys(replay *subnetSettlementReplay, maximum int) ([][32]byte, bool) {
	coldkeys := make([][32]byte, 0, len(replay.credit))
	for coldkey, credit := range replay.credit {
		if credit.Sign() != 0 {
			coldkeys = append(coldkeys, coldkey)
		}
	}
	sort.Slice(coldkeys, func(i, j int) bool { return string(coldkeys[i][:]) < string(coldkeys[j][:]) })
	if maximum < len(coldkeys) {
		return coldkeys[:maximum], false
	}
	return coldkeys, true
}
