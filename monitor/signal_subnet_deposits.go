// SIGNALS.md §22.3 operator demand deposits, conviction and reserve. At one
// agreed approved checkpoint the reader reconciles the reserve sink's
// principal with every operator's principal and cumulative conviction and
// with the staking precompile's own read of the reserve position, requires the
// reserve take to be zero and its custody to stay immutable across
// observations, pairs every Deposit and ConvictionAdded event with its
// ReservePrincipalAdded receipt over a bounded finalized frontier, replays the
// validators' exact deposit audit against the signed, committed prior-epoch
// payout artifact, or under settlement v2 the operator's signed usage statement,
// at the signed price sheet's rates for the epoch, and checks that applied native
// weight rows after the audit's native cycle give a noncompliant pool no weight.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/urfoundation/sn/demand/pricesheet"
	"github.com/urfoundation/sn/demand/usage"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
	"github.com/urfoundation/sn/validator"
)

const subnetDepositsRunBudget = 180 * time.Second

// The reserved incident classes of §22.3.
const (
	subnetDepositAttributionClass      = "subnet-deposit-attribution"
	subnetOperatorDepositMismatchClass = "subnet-operator-deposit-mismatch"
	subnetDepositPenaltyBypassClass    = "subnet-deposit-penalty-bypass"
	subnetReservePrincipalClass        = "subnet-reserve-principal"
	subnetReserveCreditDeltaClass      = "subnet-reserve-credit-delta"
	subnetReserveYieldClass            = "subnet-reserve-yield"
	subnetPriceSheetClass              = "subnet-price-sheet"
	subnetUsageStatementClass          = "subnet-usage-statement"
	subnetDepositLandingClass          = "subnet-deposit-landing"
	subnetCampaignHeadroomClass        = "subnet-campaign-headroom"
)

const (
	// How long a priced deposit may stay unpaid once its source totals exist: an
	// attempt converts within about 12 minutes and each send finalizes within 8, and
	// a refusal retries after 15. At 12 seconds a block this is 3 hours.
	subnetDepositLandingGraceBlocks = 900
	// A statement's chunk list is bounded by usage.MaxBytes, and its envelope adds
	// one small frame. A run's statements are few: one, and one more per rebuild.
	maximumSubnetUsageEnvelopeBytes = usage.MaxBytes + 8192
	maximumSubnetUsageHistoryBytes  = 64 * 1024
	maximumSubnetUsageObjects       = 8
)

var subnetDepositsFamily = subnetFamily{
	number: "22.3", key: "subnet-deposits", probeId: "subnet/deposits",
	title:   "operator demand deposits, conviction and reserve",
	classes: []string{subnetDepositAttributionClass, subnetOperatorDepositMismatchClass, subnetDepositPenaltyBypassClass, subnetReservePrincipalClass, subnetReserveCreditDeltaClass, subnetReserveYieldClass, subnetPriceSheetClass, subnetUsageStatementClass, subnetDepositLandingClass, subnetCampaignHeadroomClass},
	sources: "the agreed finalized checkpoint, the reserve sink and coordinator deposit views, the staking precompile, native Delegates, ChildkeyTake, Weights, LastUpdate and ValidatorPermit, Deposit, ConvictionAdded and ReservePrincipalAdded logs over a bounded frontier, the committed prior-epoch payout artifacts and usage statements at both API origins, the published price sheet under the deployment's pinned signer, vault2's pools, and the reviewed policy document and operator signers",
}

func NewSubnetDepositsSignal() Signal {
	return &signalAdapter{
		number: "22.3",
		key:    "subnet-deposits",
		name:   "Subnet operator demand deposits, conviction and reserve",
		probe:  newSubnetDepositsProbe(),
	}
}

// Probe-owned observation state: verified runtimes, the event frontier and
// the committed deposit ledger.
type subnetDepositsProbe struct {
	runtimes    *subnetRuntimeCache
	depositLogs *subnetLogFrontier
	ledger      *subnetDepositLedger
	latch       *subnetReplayLatch
	sheet       *subnetPriceSheetMemory
	totals      *subnetDepositTotalsMemory
	campaign    *subnetCampaignMemory
}

func newSubnetDepositsProbe() *subnetDepositsProbe {
	return &subnetDepositsProbe{runtimes: newSubnetRuntimeCache(), depositLogs: newSubnetLogFrontier(), ledger: &subnetDepositLedger{census: newSubnetDepositCensus()}, latch: newSubnetReplayLatch(), sheet: &subnetPriceSheetMemory{}, totals: &subnetDepositTotalsMemory{}, campaign: &subnetCampaignMemory{}}
}

// The block this probe first saw a usage statement of each source epoch, since a
// statement carries the epoch's end as its time, not when it was published. A restart
// only delays the landing check.
type subnetDepositTotalsMemory struct {
	stateLock sync.Mutex
	first     map[[2]uint64]uint64
}

func (self *subnetDepositTotalsMemory) firstSeen(run *subnetRun, noId, epoch, block uint64) uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	key := [2]uint64{noId, epoch}
	if first, ok := self.first[key]; ok {
		return first
	}
	run.afterPublish(func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.first == nil {
			self.first = map[[2]uint64]uint64{}
		}
		if _, ok := self.first[key]; !ok {
			self.first[key] = block
		}
		for known := range self.first {
			if known[1]+2 < epoch {
				delete(self.first, known)
			}
		}
	})
	return block
}

func (*subnetDepositsProbe) id() string               { return "subnet/deposits" }
func (*subnetDepositsProbe) tier() string             { return tierWarn }
func (*subnetDepositsProbe) cadence() time.Duration   { return 5 * time.Minute }
func (*subnetDepositsProbe) runBudget() time.Duration { return subnetDepositsRunBudget }

// The committed deposit census of every published frontier window.
type subnetDepositLedger struct {
	stateLock sync.Mutex
	census    *subnetDepositCensus
}

// A run's working copy; the ledger's own census is never shared.
func (self *subnetDepositLedger) copy() *subnetDepositCensus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.census.clone()
}

// Replaces the committed census with a published run's.
func (self *subnetDepositLedger) publish(census *subnetDepositCensus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.census = census
}

// The reserve as last observed, each operator's last nonce, the receipt
// chains and the per-epoch event totals. Principal never decreases, live
// stake never falls beyond runtime share rounding, and the reserve hotkey
// never changes.
type subnetDepositCensus struct {
	reserveKnown    bool
	reserveBlock    uint64
	liveStake       *big.Int
	principal       *big.Int
	reserveHotkey   [32]byte
	lastNonces      map[uint64]uint64
	receiptTotal    *big.Int
	operatorTotals  map[uint64]*big.Int
	surplus         *big.Int
	epochDeposits   map[[2]uint64]*big.Int
	epochConviction map[[2]uint64]*big.Int
}

func newSubnetDepositCensus() *subnetDepositCensus {
	return &subnetDepositCensus{lastNonces: map[uint64]uint64{}, operatorTotals: map[uint64]*big.Int{}, epochDeposits: map[[2]uint64]*big.Int{}, epochConviction: map[[2]uint64]*big.Int{}}
}

func subnetCopyAmount(value *big.Int) *big.Int {
	if value == nil {
		return nil
	}
	return new(big.Int).Set(value)
}

func (self *subnetDepositCensus) clone() *subnetDepositCensus {
	copy := newSubnetDepositCensus()
	copy.reserveKnown, copy.reserveBlock, copy.reserveHotkey = self.reserveKnown, self.reserveBlock, self.reserveHotkey
	copy.liveStake, copy.principal, copy.receiptTotal, copy.surplus = subnetCopyAmount(self.liveStake), subnetCopyAmount(self.principal), subnetCopyAmount(self.receiptTotal), subnetCopyAmount(self.surplus)
	for noId, nonce := range self.lastNonces {
		copy.lastNonces[noId] = nonce
	}
	for noId, total := range self.operatorTotals {
		copy.operatorTotals[noId] = subnetCopyAmount(total)
	}
	for key, total := range self.epochDeposits {
		copy.epochDeposits[key] = subnetCopyAmount(total)
	}
	for key, total := range self.epochConviction {
		copy.epochConviction[key] = subnetCopyAmount(total)
	}
	return copy
}

// Compares the reserve with its last observation and records the new one.
func (self *subnetDepositCensus) observeReserve(block uint64, liveStake, principal *big.Int, reserveHotkey [32]byte, allowance *big.Int) []string {
	reasons := []string{}
	if self.reserveKnown && self.reserveBlock < block {
		if new(big.Int).Add(liveStake, allowance).Cmp(self.liveStake) < 0 {
			reasons = append(reasons, "reserve-outflow")
		}
		if principal.Cmp(self.principal) < 0 {
			reasons = append(reasons, "principal-decreased")
		}
		if reserveHotkey != self.reserveHotkey {
			reasons = append(reasons, "reserve-hotkey-changed")
		}
	}
	if !self.reserveKnown || self.reserveBlock < block {
		self.reserveKnown, self.reserveBlock, self.reserveHotkey = true, block, reserveHotkey
		self.liveStake, self.principal = new(big.Int).Set(liveStake), new(big.Int).Set(principal)
	}
	return reasons
}

// Advances an operator's shared deposit and conviction nonce and its epoch
// totals by one event.
func (self *subnetDepositCensus) recordDeposit(event subnetDepositEvent) []string {
	reasons := []string{}
	if last, ok := self.lastNonces[event.noId]; ok && event.nonce != last+1 {
		if event.nonce <= last {
			reasons = append(reasons, "nonce-replayed")
		} else {
			reasons = append(reasons, "nonce-gap")
		}
	}
	self.lastNonces[event.noId] = event.nonce
	totals := self.epochDeposits
	if event.conviction {
		totals = self.epochConviction
	}
	key := [2]uint64{event.epoch, event.noId}
	if totals[key] == nil {
		totals[key] = new(big.Int)
	}
	totals[key].Add(totals[key], event.amount)
	return reasons
}

// One operator's event total of an epoch over every published window.
func (self *subnetDepositCensus) epochTotal(epoch, noId uint64, conviction bool) *big.Int {
	totals := self.epochDeposits
	if conviction {
		totals = self.epochConviction
	}
	if total := totals[[2]uint64{epoch, noId}]; total != nil {
		return total
	}
	return new(big.Int)
}

// Advances the receipt chains by one receipt in log order.
func (self *subnetDepositCensus) recordReceipt(receipt *stabi.STReserveSinkReservePrincipalAdded, allowance *big.Int) []string {
	reasons := []string{}
	if self.receiptTotal != nil && receipt.TotalPrincipal.Cmp(new(big.Int).Add(self.receiptTotal, receipt.Amount)) != 0 {
		reasons = append(reasons, "total-principal-chain")
	}
	noId := receipt.NoId.Uint64()
	if previous := self.operatorTotals[noId]; previous != nil && receipt.OperatorPrincipal.Cmp(new(big.Int).Add(previous, receipt.Amount)) != 0 {
		reasons = append(reasons, "operator-principal-chain")
	}
	if receipt.LiveStake.Cmp(receipt.TotalPrincipal) < 0 {
		reasons = append(reasons, "receipt-deficit")
	}
	surplus := new(big.Int).Sub(receipt.LiveStake, receipt.TotalPrincipal)
	if self.surplus != nil && new(big.Int).Add(surplus, allowance).Cmp(self.surplus) < 0 {
		reasons = append(reasons, "receipt-credit-short")
	}
	self.surplus = surplus
	self.receiptTotal = new(big.Int).Set(receipt.TotalPrincipal)
	self.operatorTotals[noId] = new(big.Int).Set(receipt.OperatorPrincipal)
	return reasons
}

// The last receipt's total principal, or nil before any receipt.
func (self *subnetDepositCensus) lastReceiptPrincipal() *big.Int {
	return self.receiptTotal
}

// The last nonce read for an operator.
func (self *subnetDepositCensus) lastNonce(noId uint64) (uint64, bool) {
	nonce, ok := self.lastNonces[noId]
	return nonce, ok
}

// Drops totals of epochs no counter can still check.
func (self *subnetDepositCensus) prune(currentEpoch uint64) {
	for _, totals := range []map[[2]uint64]*big.Int{self.epochDeposits, self.epochConviction} {
		for key := range totals {
			if key[0]+1 < currentEpoch {
				delete(totals, key)
			}
		}
	}
}

func (self *subnetDepositsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	return runSubnetFamily(ctx, env, subnetDepositsFamily, self.latch, self.observe)
}

// Deposit views of one operator at the checkpoint.
type subnetOperatorDeposits struct {
	noId                 uint64
	epochDeposits        *big.Int
	epochConvictionAdded *big.Int
	nextNonce            *big.Int
	commitment           stabi.RootCommitmentsOutput
	sourceVersion        stabi.STCoordinatorOperatorVersion
	sourceVersionErr     error
}

func (self *subnetDepositsProbe) observe(ctx context.Context, run *subnetRun) ([]finding, error) {
	checkpoint, state, gap, err := openSubnetState(ctx, run, self.runtimes, subnetDepositsFamily)
	if err != nil || checkpoint == nil {
		return gap, err
	}
	census := self.ledger.copy()
	run.afterPublish(func() { self.ledger.publish(census) })
	findings, err := observeSubnetReserve(ctx, run, checkpoint, state, census)
	if err != nil {
		return nil, err
	}
	deposits, sourceStart, sourceEnd, err := readSubnetOperatorDeposits(ctx, checkpoint, state)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		reason := "deposit-views-" + subnetReadReason(err)
		return append(findings,
			subnetUnknown(subnetDepositsFamily, subnetDepositAttributionClass, "subnet-deposits/events", reason, subnetTransientSustain, "coordinator deposit counters at the checkpoint"),
			subnetUnknown(subnetDepositsFamily, subnetOperatorDepositMismatchClass, "subnet-deposits/operators", reason, subnetTransientSustain, "coordinator deposit counters and root commitments at the checkpoint"),
			subnetUnknown(subnetDepositsFamily, subnetDepositPenaltyBypassClass, "subnet-deposits/operators", reason, subnetTransientSustain, "deposit audits to enforce"),
		), nil
	}
	events, err := self.observeDepositEvents(ctx, run, checkpoint, state, deposits, census)
	if err != nil {
		return nil, err
	}
	findings = append(findings, events...)
	audits, auditFindings, err := self.observeDepositAudits(ctx, run, checkpoint, state, deposits, sourceStart, sourceEnd)
	if err != nil {
		return nil, err
	}
	findings = append(findings, auditFindings...)
	penalties, err := observeSubnetDepositPenalties(ctx, run, checkpoint, state, audits)
	if err != nil {
		return nil, err
	}
	return append(findings, penalties...), nil
}

func subnetDepositsTarget(noId uint64) string {
	return fmt.Sprintf("subnet-deposits/no-%d", noId)
}

// The reserve: principal is covered by live stake read two ways, operator
// principal and conviction reconcile, custody is immutable and the take is
// zero.
func observeSubnetReserve(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState, census *subnetDepositCensus) ([]finding, error) {
	findings := []finding{}
	staking, err := abi.JSON(strings.NewReader(subnetStakingAbiJson))
	if err != nil {
		return nil, subnetReadFailure("staking-abi-invalid", err)
	}
	data, err := staking.Pack("getStake", state.reserveHotkey, state.sinkColdkey, new(big.Int).SetUint64(uint64(run.settings.Deployment.Netuid)))
	if err != nil {
		return nil, subnetReadFailure("staking-abi-invalid", err)
	}
	output, err := checkpoint.evmReader.call(ctx, subnetStakingPrecompile, data, checkpoint.evm)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return nil, canceled
	}
	var precompileStake *big.Int
	if err == nil {
		values, unpackErr := staking.Unpack("getStake", output)
		if unpackErr == nil && len(values) == 1 {
			precompileStake, _ = values[0].(*big.Int)
		}
	}
	if precompileStake == nil {
		findings = append(findings, subnetUnknown(subnetDepositsFamily, subnetReservePrincipalClass, "subnet-deposits/reserve", "staking-precompile-unavailable", subnetTransientSustain, "the staking precompile's read of the reserve position"))
	} else {
		reasons := []string{}
		if state.liveStake.Cmp(state.principal) < 0 {
			reasons = append(reasons, "reserve-deficit")
		}
		if precompileStake.Cmp(state.liveStake) != 0 {
			reasons = append(reasons, "live-stake-read-paths-differ")
		}
		sum := new(big.Int)
		for _, noId := range state.operatorIds {
			operator := state.operators[noId]
			sum.Add(sum, operator.operatorPrincipal)
			if operator.cumulativeConviction.Cmp(operator.operatorPrincipal) != 0 {
				reasons = append(reasons, fmt.Sprintf("conviction-principal-differs:%d", noId))
			}
		}
		if sum.Cmp(state.principal) != 0 {
			reasons = append(reasons, "operator-principal-sum")
		}
		reasons = append(reasons, census.observeReserve(checkpoint.number, state.liveStake, state.principal, state.reserveHotkey, subnetAllowance(state))...)
		if len(reasons) == 0 {
			findings = append(findings, subnetHealthy(subnetDepositsFamily, subnetReservePrincipalClass, "subnet-deposits/reserve"))
		} else {
			findings = append(findings, subnetDecided(subnetDepositsFamily, subnetReservePrincipalClass, "subnet-deposits/reserve", "", run.scope("complete", "reserve-sink-coordinator-and-staking-precompile"), subnetDecision{
				state: "violated",
				issue: subnetReplayIssue{
					tier: tierPage, sustain: subnetProvedSustain,
					invariant: "reserve-principal-conserved", expected: "live-stake-at-least-principal-and-every-ledger-agrees", observed: strings.Join(subnetSortedUnique(reasons), ","),
					symptom:   "The subnet reserve no longer covers or reconciles its recorded principal",
					mechanism: "At a finalized block the reserve sink's live stake is below its principal, the staking precompile and the sink disagree about the reserve position, operator principal and cumulative conviction do not reconcile, or the reserve lost stake, principal or its hotkey since the last observation. The sink has no withdrawal path, so any of these is a custody incident.",
					baseline:  "liveStake >= principal; the precompile reads the same live stake; the operator principals sum to principal and each equals that operator's cumulative conviction; live stake, principal and the reserve hotkey never regress.",
					action:    "Preserve both stake read paths and every ReservePrincipalAdded receipt; check the runtime share accounting before attribution. Never fund an escrow discrepancy from the reserve.",
					verify:    "A fresh checkpoint reconciles exactly and the incident identifies the first divergent receipt.",
				},
				evidence: fmt.Sprintf("Checkpoint height %d; %d operators. Stake and principal amounts are compared exactly in memory and not rendered.", checkpoint.number, len(state.operatorIds)),
			}))
		}
	}
	netuid := subnetNetuidArg(run.settings.Deployment.Netuid)
	var delegateTake, childkeyTake types.U16
	reads := []*subnetStorageRead{
		{pallet: "SubtensorModule", item: "Delegates", args: [][]byte{state.reserveHotkey[:]}, value: &delegateTake},
		{pallet: "SubtensorModule", item: "ChildkeyTake", args: [][]byte{state.reserveHotkey[:], netuid}, value: &childkeyTake},
	}
	if err := subnetReadStorageBatch(ctx, checkpoint.chain, checkpoint.nativeHash, reads); err != nil || reads[0].err != nil || reads[1].err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		return append(findings, subnetUnknown(subnetDepositsFamily, subnetReserveYieldClass, "subnet-deposits/reserve", "native-take-unavailable", subnetTransientSustain, "native Delegates and ChildkeyTake of the reserve hotkey")), nil
	}
	reasons := []string{}
	if uint16(delegateTake) != 0 {
		reasons = append(reasons, "reserve-delegate-take")
	}
	if uint16(childkeyTake) != 0 {
		reasons = append(reasons, "reserve-childkey-take")
	}
	if state.sinkColdkey != subnetMirroredColdkey(state.reserveSink) {
		reasons = append(reasons, "reserve-coldkey")
	}
	if len(reasons) == 0 {
		return append(findings, subnetHealthy(subnetDepositsFamily, subnetReserveYieldClass, "subnet-deposits/reserve")), nil
	}
	return append(findings, subnetDecided(subnetDepositsFamily, subnetReserveYieldClass, "subnet-deposits/reserve", "", run.scope("complete", "native-delegate-and-childkey-takes"), subnetDecision{
		state: "violated",
		issue: subnetReplayIssue{
			tier: tierPage, sustain: subnetProvedSustain,
			invariant: "reserve-yields-nothing", expected: "zero-takes-and-self-mirrored-sink-coldkey", observed: strings.Join(reasons, ","),
			symptom:   "The subnet reserve's yield is not compounding under its own coldkey",
			mechanism: "The reserve hotkey has a nonzero delegate or childkey take, or the sink's coldkey is not its own mirrored account. A take diverts the reserve's dividends; a changed coldkey changes who owns the compounding stake.",
			baseline:  "The reserve hotkey's delegate and childkey takes are zero and the sink coldkey is the sink's own mirrored account.",
			action:    "Route to custody incident response with the native take storage and the block; do not restake or adjust the reserve from the monitor.",
			verify:    "A fresh checkpoint shows zero takes under the sink's own coldkey.",
		},
		evidence: fmt.Sprintf("Checkpoint height %d. Deposits, voluntary conviction, rounding donations and yield are separate quantities; a low demand ratio is an economic signal, not this invariant.", checkpoint.number),
	})), nil
}

// Coordinator deposit counters for the current epoch and the source epoch's
// root commitment and operator version, with the source epoch's Evm
// boundaries.
func readSubnetOperatorDeposits(ctx context.Context, checkpoint *subnetCheckpoint, state *subnetDeploymentState) (map[uint64]*subnetOperatorDeposits, subnetEvmBlock, subnetEvmBlock, error) {
	coordinator := stabi.NewSTCoordinator()
	epoch := new(big.Int).SetUint64(state.currentEpoch)
	deposits := map[uint64]*subnetOperatorDeposits{}
	queue := &subnetViewQueue{}
	var sourceStartNumber, sourceEndNumber uint64
	hasSource := state.currentEpoch > 0
	if hasSource {
		source := new(big.Int).SetUint64(state.currentEpoch - 1)
		queue.add(state.coordinator, coordinator.PackEpochStartBlock(source), subnetDecodeUint64(&sourceStartNumber, coordinator.UnpackEpochStartBlock))
		queue.add(state.coordinator, coordinator.PackEpochEndBlock(source), subnetDecodeUint64(&sourceEndNumber, coordinator.UnpackEpochEndBlock))
	}
	for _, noId := range state.operatorIds {
		operator := &subnetOperatorDeposits{noId: noId}
		deposits[noId] = operator
		id := new(big.Int).SetUint64(noId)
		queue.add(state.coordinator, coordinator.PackEpochDeposits(epoch, id), subnetDecodeInto(&operator.epochDeposits, coordinator.UnpackEpochDeposits))
		queue.add(state.coordinator, coordinator.PackEpochConvictionAdded(epoch, id), subnetDecodeInto(&operator.epochConvictionAdded, coordinator.UnpackEpochConvictionAdded))
		queue.add(state.coordinator, coordinator.PackNextDepositNonce(id), subnetDecodeInto(&operator.nextNonce, coordinator.UnpackNextDepositNonce))
		if hasSource {
			queue.add(state.coordinator, coordinator.PackRootCommitments(new(big.Int).SetUint64(state.currentEpoch-1), id), subnetDecodeInto(&operator.commitment, coordinator.UnpackRootCommitments))
		}
	}
	if err := queue.run(ctx, checkpoint.evmReader, checkpoint.evm); err != nil {
		return nil, subnetEvmBlock{}, subnetEvmBlock{}, err
	}
	if !hasSource {
		return deposits, subnetEvmBlock{}, subnetEvmBlock{}, nil
	}
	// An operator not yet effective in the source epoch reverts operatorAt.
	calls := []subnetEvmCall{}
	for _, noId := range state.operatorIds {
		calls = append(calls, subnetEvmCall{to: state.coordinator, data: coordinator.PackOperatorAt(new(big.Int).SetUint64(noId), new(big.Int).SetUint64(state.currentEpoch-1))})
	}
	if err := checkpoint.evmReader.callBatch(ctx, calls, checkpoint.evm); err != nil {
		return nil, subnetEvmBlock{}, subnetEvmBlock{}, err
	}
	for i, noId := range state.operatorIds {
		operator := deposits[noId]
		if calls[i].err != nil {
			operator.sourceVersionErr = calls[i].err
			continue
		}
		operator.sourceVersion, operator.sourceVersionErr = coordinator.UnpackOperatorAt(calls[i].output)
	}
	if sourceEndNumber > checkpoint.number || sourceStartNumber >= sourceEndNumber {
		return nil, subnetEvmBlock{}, subnetEvmBlock{}, subnetReadFailure("source-epoch-boundaries-invalid", nil)
	}
	start, err := checkpoint.evmReader.blockAt(ctx, sourceStartNumber)
	if err != nil {
		return nil, subnetEvmBlock{}, subnetEvmBlock{}, err
	}
	end, err := checkpoint.evmReader.blockAt(ctx, sourceEndNumber)
	if err != nil {
		return nil, subnetEvmBlock{}, subnetEvmBlock{}, err
	}
	return deposits, start, end, nil
}

// One reserve receipt paired with its coordinator deposit event.
type subnetDepositEvent struct {
	log        ethtypes.Log
	conviction bool
	noId       uint64
	epoch      uint64
	amount     *big.Int
	nonce      uint64
	policyHash [32]byte
}

// The runtime share rounding allowance, or zero when the view is absent.
func subnetAllowance(state *subnetDeploymentState) *big.Int {
	if state.runtimeShareRoundingAllowance == nil {
		return new(big.Int)
	}
	return state.runtimeShareRoundingAllowance
}

func (self *subnetDepositsProbe) observeDepositEvents(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState, deposits map[uint64]*subnetOperatorDeposits, census *subnetDepositCensus) ([]finding, error) {
	target := "subnet-deposits/events"
	coordinatorAbi, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return nil, subnetReadFailure("coordinator-abi-invalid", err)
	}
	sinkAbi, err := stabi.STReserveSinkMetaData.ParseABI()
	if err != nil {
		return nil, subnetReadFailure("sink-abi-invalid", err)
	}
	plan := self.depositLogs.plan(checkpoint.number, run.settings.DeployBlock, run.limits)
	coordinatorLogs, err := subnetFetchLogs(ctx, checkpoint.evmReader, state.coordinator, [][]common.Hash{{coordinatorAbi.Events["Deposit"].ID, coordinatorAbi.Events["ConvictionAdded"].ID}}, plan, run.limits)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return nil, canceled
	}
	var sinkLogs []ethtypes.Log
	if err == nil {
		sinkLogs, err = subnetFetchLogs(ctx, checkpoint.evmReader, state.reserveSink, [][]common.Hash{{sinkAbi.Events["ReservePrincipalAdded"].ID}}, plan, run.limits)
	}
	if canceled := subnetCanceled(ctx); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		reason := "logs-" + subnetReadReason(err)
		return []finding{
			subnetUnknown(subnetDepositsFamily, subnetDepositAttributionClass, target, reason, subnetTransientSustain, "finalized deposit and reserve receipt logs over the frontier"),
			subnetUnknown(subnetDepositsFamily, subnetReserveCreditDeltaClass, "subnet-deposits/reserve-receipts", reason, subnetTransientSustain, "finalized reserve receipt logs over the frontier"),
		}, nil
	}
	coordinator := stabi.NewSTCoordinator()
	sink := stabi.NewSTReserveSink()
	attributionReasons := []string{}
	creditReasons := []string{}
	receipts := map[common.Hash][]*stabi.STReserveSinkReservePrincipalAdded{}
	for i := range sinkLogs {
		receipt, err := sink.UnpackReservePrincipalAddedEvent(&sinkLogs[i])
		if err != nil {
			creditReasons = append(creditReasons, "receipt-undecodable")
			continue
		}
		receipts[sinkLogs[i].TxHash] = append(receipts[sinkLogs[i].TxHash], receipt)
	}
	events := []subnetDepositEvent{}
	for i := range coordinatorLogs {
		log := coordinatorLogs[i]
		event := subnetDepositEvent{log: log}
		switch log.Topics[0] {
		case coordinatorAbi.Events["Deposit"].ID:
			deposit, err := coordinator.UnpackDepositEvent(&log)
			if err != nil {
				attributionReasons = append(attributionReasons, "event-undecodable")
				continue
			}
			event.noId, event.epoch, event.amount, event.nonce, event.policyHash = deposit.NoId.Uint64(), deposit.Epoch.Uint64(), deposit.Amount, deposit.Nonce.Uint64(), deposit.PolicyHash
		default:
			conviction, err := coordinator.UnpackConvictionAddedEvent(&log)
			if err != nil {
				attributionReasons = append(attributionReasons, "event-undecodable")
				continue
			}
			event.conviction = true
			event.noId, event.epoch, event.amount, event.nonce, event.policyHash = conviction.NoId.Uint64(), conviction.Epoch.Uint64(), conviction.Amount, conviction.Nonce.Uint64(), conviction.PolicyHash
		}
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].log.BlockNumber != events[j].log.BlockNumber {
			return events[i].log.BlockNumber < events[j].log.BlockNumber
		}
		return events[i].log.Index < events[j].log.Index
	})
	// Each deposit consumes exactly one receipt of its own transaction with
	// the same operator, epoch and amount; a leftover receipt is unattributed.
	consumed := map[*stabi.STReserveSinkReservePrincipalAdded]bool{}
	for _, event := range events {
		matched := false
		for _, receipt := range receipts[event.log.TxHash] {
			if !consumed[receipt] && receipt.NoId.Uint64() == event.noId && receipt.Epoch.Uint64() == event.epoch && receipt.Amount.Cmp(event.amount) == 0 && receipt.Raw.Index < event.log.Index {
				consumed[receipt] = true
				matched = true
				break
			}
		}
		if !matched {
			attributionReasons = append(attributionReasons, "deposit-without-receipt")
		}
		epoch, ok := subnetEpochAtBlock(state.policies, event.log.BlockNumber)
		if !ok || epoch != event.epoch {
			attributionReasons = append(attributionReasons, "deposit-epoch")
		}
		if policy, ok := subnetPolicyAtEpoch(state.policies, event.epoch); !ok || policy.PolicyHash != event.policyHash {
			attributionReasons = append(attributionReasons, "deposit-policy-hash")
		}
		if _, ok := state.operators[event.noId]; !ok {
			attributionReasons = append(attributionReasons, "deposit-unknown-operator")
		}
		attributionReasons = append(attributionReasons, census.recordDeposit(event)...)
	}
	for _, list := range receipts {
		for _, receipt := range list {
			if !consumed[receipt] {
				attributionReasons = append(attributionReasons, "receipt-without-deposit")
			}
		}
	}
	// The counters are cross-checks once the published frontier, read
	// contiguously from its origin, covers the whole current epoch.
	if plan.complete {
		covered := self.depositLogs.originBlock() <= subnetEpochStartBlock(state.policies, state.currentEpoch)
		for _, noId := range state.operatorIds {
			operator := deposits[noId]
			if last, ok := census.lastNonce(noId); ok && operator.nextNonce.Uint64() != last+1 {
				attributionReasons = append(attributionReasons, fmt.Sprintf("nonce-counter-differs:%d", noId))
			}
			if !covered {
				continue
			}
			for _, pair := range []struct {
				counted *big.Int
				counter *big.Int
				name    string
			}{{census.epochTotal(state.currentEpoch, noId, false), operator.epochDeposits, "epoch-deposit-counter"}, {census.epochTotal(state.currentEpoch, noId, true), operator.epochConvictionAdded, "epoch-conviction-counter"}} {
				if pair.counted.Cmp(pair.counter) != 0 {
					attributionReasons = append(attributionReasons, fmt.Sprintf("%s-differs:%d", pair.name, noId))
				}
			}
			if state.currentPolicy.EpochDepositCapRao != nil && operator.epochDeposits.Cmp(state.currentPolicy.EpochDepositCapRao) > 0 {
				attributionReasons = append(attributionReasons, fmt.Sprintf("epoch-cap-exceeded:%d", noId))
			}
		}
	}
	creditReasons = append(creditReasons, subnetReceiptCredit(sinkLogs, sink, state, plan, census)...)
	census.prune(state.currentEpoch)
	findings := []finding{}
	evidence := fmt.Sprintf("Checkpoint height %d; frontier read %d deposit events and %d reserve receipts. Amounts and funders are compared in memory and not rendered.", checkpoint.number, len(events), len(sinkLogs))
	if len(attributionReasons) != 0 {
		findings = append(findings, subnetDecided(subnetDepositsFamily, subnetDepositAttributionClass, target, "", run.scope("bounded-frontier", "reserve-receipt-events"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "deposit-receipt-attribution", expected: "one-same-transaction-receipt-per-deposit-and-sequential-nonces", observed: strings.Join(subnetSortedUnique(attributionReasons), ","),
				symptom:   "An accepted subnet deposit is misattributed or inconsistent with its counters",
				mechanism: "A finalized Deposit or ConvictionAdded event has no matching ReservePrincipalAdded receipt in its transaction, a receipt has no deposit, an event names the wrong epoch, policy or operator, an operator's shared nonce sequence repeats or skips, or the epoch counters disagree with the complete event census.",
				baseline:  "Each deposit or conviction event pairs one-to-one with its same-transaction reserve receipt, names the effective policy of its epoch, advances its operator's nonce by exactly one, and the complete epoch census reproduces the counters within the caps.",
				action:    "Preserve the call bytes, log indices, nonces and before/after stake of the affected transactions, including failed attempts; do not count voluntary conviction as epoch demand.",
				verify:    "A fresh frontier pairs every event and the counters reconcile.",
			},
			evidence: evidence,
		}))
	} else {
		findings = append(findings, subnetHealthy(subnetDepositsFamily, subnetDepositAttributionClass, target))
	}
	if len(creditReasons) != 0 {
		findings = append(findings, subnetDecided(subnetDepositsFamily, subnetReserveCreditDeltaClass, "subnet-deposits/reserve-receipts", "", run.scope("bounded-frontier", "reserve-receipt-events"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "receipt-credits-exact", expected: "receipts-add-exactly-their-amounts", observed: strings.Join(subnetSortedUnique(creditReasons), ","),
				symptom:   "A reserve receipt does not cover its full principal",
				mechanism: "Consecutive ReservePrincipalAdded receipts do not add exactly their amounts to the total or operator principal, or a receipt's live stake does not cover the total principal, or the stake-over-principal surplus fell between receipts. The contract allows only a pinned zero-to-two rao surplus per move; principal must never be reduced to hide a rounding loss.",
				baseline:  "Every receipt adds exactly its amount to the total and operator principal, its live stake covers the total principal, and the surplus never decreases beyond runtime share rounding.",
				action:    "Retain both destination share-pool transitions of the affected receipts and investigate the runtime share accounting before any repair.",
				verify:    "A fresh frontier's receipts chain exactly to the sink principal.",
			},
			evidence: evidence,
		}))
	} else {
		findings = append(findings, subnetHealthy(subnetDepositsFamily, subnetReserveCreditDeltaClass, "subnet-deposits/reserve-receipts"))
	}
	if !plan.complete {
		findings = append(findings, subnetUnknown(subnetDepositsFamily, subnetDepositAttributionClass, "subnet-deposits/deposit-events", "event-frontier-behind", subnetTransientSustain, "a deposit event census reaching the agreed checkpoint"))
	}
	if !plan.empty {
		run.afterPublish(func() { self.depositLogs.advance(plan.to) })
	}
	return findings, nil
}

// Receipts chain exactly in log order: each adds its amount to the total and
// to its operator's principal, keeps live stake over the total, and never
// lowers the surplus beyond runtime share rounding. The last receipt of a
// complete frontier equals the sink's principal.
func subnetReceiptCredit(logs []ethtypes.Log, sink *stabi.STReserveSink, state *subnetDeploymentState, plan subnetLogPlan, census *subnetDepositCensus) []string {
	ordered := append([]ethtypes.Log(nil), logs...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].BlockNumber != ordered[j].BlockNumber {
			return ordered[i].BlockNumber < ordered[j].BlockNumber
		}
		return ordered[i].Index < ordered[j].Index
	})
	reasons := []string{}
	for i := range ordered {
		receipt, err := sink.UnpackReservePrincipalAddedEvent(&ordered[i])
		if err != nil {
			continue
		}
		reasons = append(reasons, census.recordReceipt(receipt, subnetAllowance(state))...)
	}
	if total := census.lastReceiptPrincipal(); total != nil && plan.complete && total.Cmp(state.principal) != 0 {
		reasons = append(reasons, "receipts-differ-from-principal")
	}
	return reasons
}

// The active policy version of an epoch.
func subnetPolicyAtEpoch(policies []stabi.STCoordinatorPolicySnapshot, epoch uint64) (stabi.STCoordinatorPolicySnapshot, bool) {
	active := -1
	for i, policy := range policies {
		if policy.EffectiveEpoch <= epoch {
			active = i
		}
	}
	if active < 0 {
		return stabi.STCoordinatorPolicySnapshot{}, false
	}
	return policies[active], true
}

// The coordinator's epochStartBlock, replayed on its policy versions.
func subnetEpochStartBlock(policies []stabi.STCoordinatorPolicySnapshot, epoch uint64) uint64 {
	policy, ok := subnetPolicyAtEpoch(policies, epoch)
	if !ok {
		return 0
	}
	return policy.EffectiveBlock + (epoch-policy.EffectiveEpoch)*policy.EpochBlocks
}

// One operator's replayed deposit audit for the current epoch.
type subnetDepositAuditResult struct {
	noId    uint64
	audit   validator.DepositAudit
	decided bool
}

// Replays the validators' deposit audit for every on-chain operator at the rates
// in force for the epoch, against what settled its source epoch: the signed,
// committed payout artifact under v1, the signed usage statement under v2. Both
// underdeposit and overdeposit are mismatches; a pending source before its deadline
// is not.
func (self *subnetDepositsProbe) observeDepositAudits(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState, deposits map[uint64]*subnetOperatorDeposits, sourceStart, sourceEnd subnetEvmBlock) ([]subnetDepositAuditResult, []finding, error) {
	document, ok := run.settings.policyDocumentBytes()
	if !ok {
		return nil, []finding{subnetUnknown(subnetDepositsFamily, subnetOperatorDepositMismatchClass, "subnet-deposits/operators", "policy-document-not-enrolled", subnetConfigurationSustain, "the exact reviewed policy document with the deposit tiers and caps")}, nil
	}
	policy, err := protocol.ParsePolicy(document)
	if err != nil {
		return nil, []finding{subnetUnknown(subnetDepositsFamily, subnetOperatorDepositMismatchClass, "subnet-deposits/operators", "policy-document-invalid", subnetConfigurationSustain, "a valid reviewed policy document")}, nil
	}
	digest, err := policy.Hash()
	if err != nil || subnetHex32(digest) != run.settings.Deployment.PolicyHash {
		return nil, []finding{subnetUnknown(subnetDepositsFamily, subnetOperatorDepositMismatchClass, "subnet-deposits/operators", "policy-document-not-pinned", subnetConfigurationSustain, "the reviewed policy document whose hash the deployment pins")}, nil
	}
	signers := map[uint64]common.Address{}
	for _, operator := range run.settings.Operators {
		signers[operator.NoId] = common.HexToAddress(operator.ArtifactSigner)
	}
	deployment := run.settings.Deployment
	epoch := state.currentEpoch
	lag := policy.Deposit.UsageLagEpochs
	deadline := subnetEpochStartBlock(state.policies, epoch) + state.currentPolicy.RootCommitWindowBlocks
	results := []subnetDepositAuditResult{}
	findings := []finding{}
	sheet, err := self.sheet.read(ctx, run)
	if err != nil {
		return nil, nil, err
	}
	findings = append(findings, subnetPriceSheetFindings(run, checkpoint, sheet)...)
	// A sheet never verified, as on a host that cannot reach the published address or
	// after a restart during an outage, prices under the reviewed document, as validators
	// with no sheet do. Its unavailability is the sheet finding's alone.
	effective := pricesheet.Effective(sheet.sheet, epoch, policy.Deposit)
	inForce, scheduled := subnetDepositPriced(sheet.sheet, epoch, policy.Deposit)
	// The pools decide which settlement owned a source epoch, never whether a v1 root
	// exists, because the phase C sweep commits one on a v2 epoch. Nothing here reads
	// them while nothing is priced.
	var pools map[uint64]stabi.ISTSettlementVault2Pool
	poolsKnown := false
	if deployment.VaultV2 != "" && (inForce || scheduled) && lag <= epoch {
		pools, poolsKnown, err = readSubnetDepositV2Pools(ctx, checkpoint, common.HexToAddress(deployment.VaultV2), state.operatorIds)
		if err != nil {
			return nil, nil, err
		}
	}
	settledByV2 := func(noId, source uint64) bool {
		pool := pools[noId]
		return pool.Registered && pool.FirstEpoch <= source
	}
	statementOf := func(noId uint64) (subnetUsageStatementRead, error) {
		return readSubnetUsageStatement(ctx, run, noId, epoch-lag, signers[noId], sourceStart, sourceEnd)
	}
	mismatched := 0
	for _, noId := range state.operatorIds {
		operator := deposits[noId]
		target := subnetDepositsTarget(noId)
		observed := operator.epochDeposits
		conviction := new(big.Int).Sub(state.operators[noId].cumulativeConviction, operator.epochDeposits)
		conviction.Sub(conviction, operator.epochConvictionAdded)
		if conviction.Sign() < 0 {
			findings = append(findings, subnetUnknown(subnetDepositsFamily, subnetOperatorDepositMismatchClass, target, "conviction-underflow", subnetTransientSustain, "consistent cumulative conviction and epoch counters"))
			continue
		}
		v2Declared := deployment.VaultV2 != "" && (inForce || scheduled) && lag <= epoch
		var audit validator.DepositAudit
		totalsAt := uint64(0)
		switch {
		case effective.IsZeroPrice():
			audit = validator.ZeroPriceDepositAudit(epoch, max(epoch, lag)-lag, noId, observed, conviction)
			// Nothing is owed. Once a price is scheduled, the pipeline that price
			// will need is checked ahead of it.
			if v2Declared && scheduled && poolsKnown && settledByV2(noId, epoch-lag) {
				if _, reviewed := signers[noId]; reviewed {
					read, err := statementOf(noId)
					if err != nil {
						return nil, nil, err
					}
					findings = append(findings, subnetUsageStatementFinding(run, checkpoint, target, read, deadline, false))
				}
			}
		case epoch < lag:
			audit = validator.DepositAudit{NoID: noId, Epoch: epoch, Status: validator.DepositAuditBootstrap, Compliant: observed.Sign() == 0, Disposition: "zero_pool_weight_bootstrap"}
			if observed.Sign() != 0 {
				audit.Status = validator.DepositAuditMismatch
			}
		case v2Declared && !poolsKnown:
			findings = append(findings, subnetUnknown(subnetDepositsFamily, subnetOperatorDepositMismatchClass, target, "v2-pool-unavailable", subnetTransientSustain, "this operator's vault2 pool, which decides what settled the source epoch"))
			continue
		case v2Declared && settledByV2(noId, epoch-lag):
			if _, reviewed := signers[noId]; !reviewed {
				findings = append(findings, subnetUnknown(subnetDepositsFamily, subnetOperatorDepositMismatchClass, target, "artifact-signer-not-reviewed", subnetConfigurationSustain, "the reviewed artifact signer of this operator"))
				continue
			}
			read, err := statementOf(noId)
			if err != nil {
				return nil, nil, err
			}
			findings = append(findings, subnetUsageStatementFinding(run, checkpoint, target, read, deadline, true))
			switch {
			case read.statement != nil:
				audit = subnetEvaluateUsageStatement(read.statement, epoch, epoch-lag, noId, observed, conviction, effective)
				totalsAt = self.totals.firstSeen(run, noId, epoch-lag, checkpoint.evm.number)
			case read.status != "":
				audit = validator.FailedDepositAudit(epoch, epoch-lag, noId, observed, conviction, read.status, nil)
			case read.absent:
				audit = validator.FailedDepositAudit(epoch, epoch-lag, noId, observed, conviction, validator.DepositAuditUnavailablePending, nil)
				if checkpoint.evm.number > deadline {
					audit.Status = validator.DepositAuditUnavailable
				}
			default:
				findings = append(findings, subnetUnknown(subnetDepositsFamily, subnetOperatorDepositMismatchClass, target, read.reason, subnetTransientSustain, "the operator's signed usage statement at an enrolled API origin"))
				continue
			}
		case operator.commitment.CommitBlock == 0:
			audit = validator.FailedDepositAudit(epoch, epoch-lag, noId, observed, conviction, validator.DepositAuditUnavailablePending, nil)
			if checkpoint.evm.number > deadline {
				audit.Status = validator.DepositAuditUnavailable
			}
		default:
			signer, reviewed := signers[noId]
			if !reviewed {
				findings = append(findings, subnetUnknown(subnetDepositsFamily, subnetOperatorDepositMismatchClass, target, "artifact-signer-not-reviewed", subnetConfigurationSustain, "the reviewed artifact signer of this operator"))
				continue
			}
			if operator.sourceVersionErr != nil {
				findings = append(findings, subnetUnknown(subnetDepositsFamily, subnetOperatorDepositMismatchClass, target, "source-operator-unavailable", subnetTransientSustain, "the operator version at the source epoch"))
				continue
			}
			read, err := readSubnetCommittedArtifact(ctx, run, subnetArtifactContentHash(operator.commitment.ArtifactHash))
			if err != nil {
				return nil, nil, err
			}
			switch {
			case read.equivocation:
				audit = validator.FailedDepositAudit(epoch, epoch-lag, noId, observed, conviction, validator.DepositAuditEquivocation, nil)
			case read.artifact == nil:
				findings = append(findings, subnetUnknown(subnetDepositsFamily, subnetOperatorDepositMismatchClass, target, read.reason, subnetTransientSustain, "the committed prior-epoch payout artifact at an enrolled API origin"))
				continue
			default:
				audit = validator.EvaluateDepositArtifact(read.artifact, validator.DepositArtifactExpectation{
					DeploymentID: deployment.DeploymentId, ChainID: deployment.ChainId, GenesisHash: deployment.GenesisHash,
					Netuid: deployment.Netuid, Coordinator: deployment.coordinatorAddress(), SettlementVault: deployment.settlementVaultAddress(),
					PolicyHash: deployment.PolicyHash, Epoch: epoch - lag, NoID: noId, Signer: signer,
					Start:      payoutartifact.Boundary{Number: sourceStart.number, Hash: strings.ToLower(sourceStart.hash.Hex())},
					End:        payoutartifact.Boundary{Number: sourceEnd.number, Hash: strings.ToLower(sourceEnd.hash.Hex())},
					PayoutRoot: operator.commitment.PayoutRoot, ArtifactHash: operator.commitment.ArtifactHash,
					Committer: operator.commitment.Committer, RootSigner: operator.sourceVersion.RootSigner, CommitBlock: operator.commitment.CommitBlock,
				}, epoch, observed, conviction, effective)
				totalsAt = operator.commitment.CommitBlock
			}
		}
		audit.ObservedAtBlock = checkpoint.evm.number
		audit.ArtifactDeadlineBlock = deadline
		decided := audit.Status != validator.DepositAuditUnavailablePending
		results = append(results, subnetDepositAuditResult{noId: noId, audit: audit, decided: decided})
		if totalsAt != 0 {
			findings = append(findings, subnetDepositLandingFinding(run, checkpoint, target, audit, observed, totalsAt))
		}
		if audit.Compliant || audit.Status == validator.DepositAuditZeroPrice || audit.Status == validator.DepositAuditUnavailablePending {
			findings = append(findings, subnetHealthy(subnetDepositsFamily, subnetOperatorDepositMismatchClass, target))
			continue
		}
		mismatched++
		findings = append(findings, subnetDecided(subnetDepositsFamily, subnetOperatorDepositMismatchClass, target, "", run.scope("complete", "committed-payout-artifact-replay"), subnetDecision{
			state: fmt.Sprint(audit.Status),
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetConfigurationSustain,
				invariant: "operator-deposit-matches-signed-claim", expected: "compliant", observed: fmt.Sprint(audit.Disposition),
				symptom:   "An operator's demand deposit does not match its signed usage claim",
				mechanism: "The exact deposit audit validators run, replayed against the committed, signed prior-epoch payout artifact or usage statement at the rates in force for the epoch, found the observed deposit unequal to the required one, or found the claim invalid, equivocating or unavailable after its root window. Both underdeposit and overdeposit are mismatches; validators give such a pool zero weight.",
				baseline:  "Each operator's epoch deposit equals the required deposit for its signed prior-epoch usage, or its artifact is still pending before the root window closes, or the price in force is zero.",
				action:    "Notify the responsible operator with the private audit record. Do not treat a pre-deadline pending artifact or an authorized bootstrap rule as dishonesty.",
				verify:    "The operator's next epoch audit is compliant, or its artifact becomes available and verifies.",
			},
			evidence: fmt.Sprintf("Checkpoint EVM height %d; artifact deadline %d. This proves consistency with the signed claim, not a trustless usage oracle; required and observed amounts stay private.", checkpoint.evm.number, deadline),
			details:  fmt.Sprintf("disposition=%s epoch=%d source_epoch=%d", audit.Disposition, audit.Epoch, audit.SourceEpoch),
		}))
	}
	if mismatched != 0 && mismatched == len(state.operatorIds) && checkpoint.evm.number > deadline {
		findings = append(findings, subnetDecided(subnetDepositsFamily, subnetOperatorDepositMismatchClass, "subnet-deposits/operators", "", run.scope("complete", "committed-payout-artifact-replay"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "some-operator-deposit-compliant", expected: "at-least-one-compliant-operator", observed: fmt.Sprintf("%d-of-%d-failing", mismatched, len(state.operatorIds)),
				symptom:   "Every subnet operator failed its deposit audit after the root window",
				mechanism: "No operator has a compliant deposit audit after the root commit deadline, so every pool's tail weight is zeroed for the epoch.",
				baseline:  "At least one operator passes its deposit audit by the root commit deadline.",
				action:    "Page the operators' settlement owners; check artifact publication and the deposit path before the next epoch.",
				verify:    "At least one operator's audit is compliant on the next epoch.",
			},
			evidence: fmt.Sprintf("Checkpoint EVM height %d; artifact deadline %d.", checkpoint.evm.number, deadline),
			details:  fmt.Sprintf("failing_operators=%d", mismatched),
		}))
	}
	headroom, err := self.observeCampaignHeadroom(ctx, run, checkpoint, state, results, effective.IsZeroPrice())
	if err != nil {
		return nil, nil, err
	}
	return results, append(findings, headroom...), nil
}

// The campaign cap is a lifetime limit on every deposit's reserve, and a deposit past
// it reverts, after which validators give the pool no weight. The headroom left is
// judged in epochs of deposits at the latest required deposit, summed over the
// operators, each epoch under the cap of the version in force at it, so a raise that
// takes effect in time counts: under subnetCampaignWarnEpochs it warns, under
// subnetCampaignPageEpochs it pages. Each operator's latest requirement is this
// epoch's once its source totals exist, and its last one seen before that. At zero
// price nothing is required and nothing is read.
const (
	subnetCampaignWarnEpochs = 8
	subnetCampaignPageEpochs = 2
)

// Each operator's latest decided required deposit.
type subnetCampaignMemory struct {
	stateLock sync.Mutex
	required  map[uint64]*big.Int
}

func (self *subnetCampaignMemory) last() map[uint64]*big.Int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	last := map[uint64]*big.Int{}
	for noId, amount := range self.required {
		last[noId] = amount
	}
	return last
}

func (self *subnetCampaignMemory) remember(required map[uint64]*big.Int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.required = required
}

// subnetCampaignEpochsLeft counts the epochs, from this one on and at most limit, whose
// deposits of required each still fit under the campaign cap of the version in force at
// their epoch.
func subnetCampaignEpochsLeft(policies []stabi.STCoordinatorPolicySnapshot, epoch uint64, reserved *big.Int, required *big.Int, limit int) int {
	used := new(big.Int).Set(reserved)
	for left := range limit {
		version, ok := subnetPolicyAtEpoch(policies, epoch+uint64(left))
		if !ok || version.CampaignDepositCapRao == nil {
			return left
		}
		used.Add(used, required)
		if used.Cmp(version.CampaignDepositCapRao) > 0 {
			return left
		}
	}
	return limit
}

func (self *subnetDepositsProbe) observeCampaignHeadroom(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState, results []subnetDepositAuditResult, zeroPrice bool) ([]finding, error) {
	target := "subnet-deposits/campaign-headroom"
	if zeroPrice {
		run.afterPublish(func() { self.campaign.remember(nil) })
		return []finding{subnetHealthy(subnetDepositsFamily, subnetCampaignHeadroomClass, target)}, nil
	}
	// An operator not decided this run counts at its last known requirement, so one
	// whose source totals are late does not shrink the sum.
	latest := self.campaign.last()
	decided := false
	for _, result := range results {
		amount, ok := new(big.Int).SetString(result.audit.RequiredDepositRao, 10)
		// A requirement is known only once the audit has priced it: an audit decided
		// as unavailable or invalid says nothing about what the operator owes.
		if result.decided && ok && 0 < amount.Sign() {
			latest[result.noId], decided = amount, true
		}
	}
	if decided {
		run.afterPublish(func() { self.campaign.remember(latest) })
	}
	required := new(big.Int)
	for _, amount := range latest {
		required.Add(required, amount)
	}
	if required.Sign() == 0 {
		return []finding{subnetHealthy(subnetDepositsFamily, subnetCampaignHeadroomClass, target)}, nil
	}
	requiredEpoch := state.currentEpoch
	coordinator := stabi.NewSTCoordinator()
	calls := []subnetEvmCall{{to: state.coordinator, data: coordinator.PackCampaignReserved()}}
	err := checkpoint.evmReader.callBatch(ctx, calls, checkpoint.evm)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return nil, canceled
	}
	var reserved *big.Int
	if err == nil && calls[0].err == nil {
		reserved, err = coordinator.UnpackCampaignReserved(calls[0].output)
	}
	if err != nil || calls[0].err != nil || reserved == nil {
		return []finding{subnetUnknown(subnetDepositsFamily, subnetCampaignHeadroomClass, target, "campaign-reserved-unavailable", subnetTransientSustain, "the coordinator's campaignReserved at the agreed checkpoint")}, nil
	}
	epochs := subnetCampaignEpochsLeft(state.policies, state.currentEpoch, reserved, required, subnetCampaignWarnEpochs)
	if subnetCampaignWarnEpochs <= epochs {
		return []finding{subnetHealthy(subnetDepositsFamily, subnetCampaignHeadroomClass, target)}, nil
	}
	tier, sustain, invariant := tierWarn, subnetConfigurationSustain, fmt.Sprintf("campaign-headroom>=%d-epochs", subnetCampaignWarnEpochs)
	if epochs < subnetCampaignPageEpochs {
		tier, sustain, invariant = tierPage, subnetProvedSustain, fmt.Sprintf("campaign-headroom>=%d-epochs", subnetCampaignPageEpochs)
	}
	return []finding{subnetDecided(subnetDepositsFamily, subnetCampaignHeadroomClass, target, "", run.scope("complete", "coordinator-campaign-reserved"), subnetDecision{
		state: "violated",
		issue: subnetReplayIssue{
			tier: tier, sustain: sustain,
			invariant: invariant, expected: fmt.Sprintf("at-least-%d-epochs", subnetCampaignWarnEpochs), observed: fmt.Sprintf("%d-epochs", epochs),
			symptom:   "The coordinator's campaign deposit cap is close to used up",
			mechanism: "campaignDepositCapRao is a lifetime limit on the reserve every deposit adds to, and campaignReserved never goes down. Once a deposit would pass it, the deposit reverts, and validators give that pool no weight for the epoch.",
			baseline:  fmt.Sprintf("The campaign cap less campaignReserved covers at least %d epochs at the latest required deposit, summed over the operators, under the version in force at each epoch.", subnetCampaignWarnEpochs),
			action:    "The owner schedules a coordinator policy version with a higher campaign cap, effective before the headroom runs out. Lower prices only by a signed entry; never let the cap run out with a price in force.",
			verify:    "A fresh run shows the headroom at or above the warning level, or a scheduled version raising the cap in time.",
		},
		evidence: fmt.Sprintf("Checkpoint EVM height %d; the operators' latest required deposits as of epoch %d. Amounts stay private.", checkpoint.evm.number, requiredEpoch),
		details:  fmt.Sprintf("epochs_left=%d", epochs),
	})}, nil
}

// The price sheet itself: a file that does not verify pages at once, because
// operators and validators may then disagree on the rates. One that cannot be read is
// unknown until the last verified copy grows stale. Whether a signed cap fits the
// coordinator's is the chain family's deposit cap rule (subnetDepositCapsStop), which
// judges each entry against the version in force at its own epoch.
func subnetPriceSheetFindings(run *subnetRun, checkpoint *subnetCheckpoint, sheet subnetPriceSheetRead) []finding {
	if !sheet.pinned {
		return nil
	}
	target := "subnet-deposits/price-sheet"
	evidence := fmt.Sprintf("Checkpoint height %d. The signed entries are compared in memory; the file's address and contents are not rendered.", checkpoint.number)
	findings := []finding{}
	switch {
	case sheet.bad:
		findings = append(findings, subnetDecided(subnetDepositsFamily, subnetPriceSheetClass, target, "", run.scope("complete", "published-price-sheet"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "published-price-sheet-verifies", expected: "every-entry-signed-by-the-pinned-signer-and-extending-the-last-verified", observed: sheet.reason,
				symptom:   "The published demand deposit price sheet does not verify",
				mechanism: "A file is served at the published address that does not parse, carries an entry not signed by the deployment's pinned signer, or drops or changes a signed entry. Readers keep the last verified sheet, so a deposit is not paid from it, but a reader with no verified copy cannot price at all, and a real new entry may be hidden behind it.",
				baseline:  "The published file parses and every entry verifies under the pinned signer, keeping every entry already verified.",
				action:    "Treat the site as possibly compromised until the file is explained; publish the correctly signed file. Never sign to match a file you did not write.",
				verify:    "The published file verifies on a fresh run.",
			},
			evidence: evidence,
		}))
	case sheet.reason != "" && sheet.sheet != nil && subnetPriceSheetStaleAfter < run.started.Sub(sheet.lastVerified):
		findings = append(findings, subnetDecided(subnetDepositsFamily, subnetPriceSheetClass, target, "stale", run.scope("complete", "published-price-sheet"), subnetDecision{
			state: "stale",
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetConfigurationSustain,
				invariant: "published-price-sheet-fresh", expected: fmt.Sprintf("read-within-%s", subnetPriceSheetStaleAfter), observed: sheet.reason,
				symptom:   "The published demand deposit price sheet has not been readable for hours",
				mechanism: "Readers keep pricing from the last verified sheet while the published address is down, so nothing stops, but a new entry published meanwhile reaches no operator or validator.",
				baseline:  "The published file is readable at every cadence.",
				action:    "Restore the published address. Do not schedule a new price until readers can see it.",
				verify:    "The published file reads and verifies on a fresh run.",
			},
			evidence: evidence,
		}))
	case sheet.reason != "":
		findings = append(findings, subnetUnknown(subnetDepositsFamily, subnetPriceSheetClass, target, sheet.reason, subnetTransientSustain, "the published price sheet"))
	default:
		findings = append(findings, subnetHealthy(subnetDepositsFamily, subnetPriceSheetClass, target))
	}
	return findings
}

// One operator's vault2 pools at the checkpoint. A failed read is not an absent v2.
func readSubnetDepositV2Pools(ctx context.Context, checkpoint *subnetCheckpoint, vault common.Address, operators []uint64) (map[uint64]stabi.ISTSettlementVault2Pool, bool, error) {
	binding := stabi.NewSTSettlementVault2()
	calls := make([]subnetEvmCall, len(operators))
	for i, noId := range operators {
		calls[i] = subnetEvmCall{to: vault, data: binding.PackPools(new(big.Int).SetUint64(noId))}
	}
	if err := checkpoint.evmReader.callBatch(ctx, calls, checkpoint.evm); err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, false, canceled
		}
		return nil, false, nil
	}
	pools := map[uint64]stabi.ISTSettlementVault2Pool{}
	for i, noId := range operators {
		if calls[i].err != nil {
			return nil, false, nil
		}
		pool, err := binding.UnpackPools(calls[i].output)
		if err != nil {
			return nil, false, nil
		}
		pools[noId] = pool
	}
	return pools, true, nil
}

// One read of an operator's usage statements for an epoch at every origin. Statement
// is the one the audit uses. Absent means every origin answered and none holds one. A
// nonempty status is the audit's verdict; otherwise reason says why nothing is known.
type subnetUsageStatementRead struct {
	statement *usage.Statement
	absent    bool
	status    string
	reason    string
}

// The evidence routes' history answer.
type subnetUsageHistory struct {
	Schema  string `json:"schema"`
	Objects []struct {
		Key string `json:"key"`
	} `json:"objects"`
	More bool `json:"more"`
}

// The evidence kinds a statement is published under: version 1, and version 2 for an
// epoch whose committed header carries an extender component (sn WHITEPAPER.md
// §8.6.4).
var subnetUsageEvidenceKinds = []string{usage.EvidenceKind, usage.EvidenceKindV2}

// Reads the operator's statements through both origins, of either version. A
// statement counts only if it verifies under the reviewed artifact signer and names
// this deployment, operator, epoch and its finalized boundaries. Two that verify for
// the same chunks with different totals or extender pools, or of different versions,
// equivocate. Two for different chunks are a rebuild, which only the committed header
// can choose between, and so unknown here: validators read the ledger's rows, and the
// version its header calls for, and this reader does not repeat that.
func readSubnetUsageStatement(ctx context.Context, run *subnetRun, noId, epoch uint64, signer common.Address, start, end subnetEvmBlock) (subnetUsageStatementRead, error) {
	deployment := run.settings.Deployment
	runId := usage.RunID(noId, epoch)
	found := map[string]*usage.Statement{}
	invalid := false
	answered := 0
	for _, origin := range run.settings.ApiOrigins {
		complete := true
		for _, kind := range subnetUsageEvidenceKinds {
			raw, _, err := readSubnetApi(ctx, run, origin, "/sn/evidence/history", url.Values{"deployment_id": []string{deployment.DeploymentId}, "netuid": []string{fmt.Sprint(deployment.Netuid)}, "kind": []string{kind}, "run_id": []string{runId}, "limit": []string{fmt.Sprint(maximumSubnetUsageObjects)}}, maximumSubnetUsageHistoryBytes)
			if canceled := subnetCanceled(ctx); canceled != nil {
				return subnetUsageStatementRead{}, canceled
			}
			if err != nil {
				complete = false
				continue
			}
			var history subnetUsageHistory
			if err := json.Unmarshal(raw, &history); err != nil || history.More || len(history.Objects) > maximumSubnetUsageObjects {
				invalid = true
				complete = false
				continue
			}
			if !readSubnetUsageHistory(ctx, run, origin, history, runId, deployment, noId, epoch, signer, start, end, found, &invalid) {
				complete = false
			}
			if canceled := subnetCanceled(ctx); canceled != nil {
				return subnetUsageStatementRead{}, canceled
			}
		}
		if complete {
			answered++
		}
	}
	var chosen *usage.Statement
	for _, statement := range found {
		switch {
		case chosen == nil:
			chosen = statement
		case strings.Join(chosen.ProviderChunks, ",") != strings.Join(statement.ProviderChunks, ","):
			return subnetUsageStatementRead{reason: "usage-statement-rebuilt"}, nil
		case !subnetSameUsageStatement(chosen, statement):
			return subnetUsageStatementRead{status: validator.DepositAuditEquivocation}, nil
		}
	}
	switch {
	case chosen != nil:
		return subnetUsageStatementRead{statement: chosen}, nil
	case invalid:
		return subnetUsageStatementRead{status: validator.DepositAuditInvalid}, nil
	case answered == len(run.settings.ApiOrigins):
		return subnetUsageStatementRead{absent: true}, nil
	}
	return subnetUsageStatementRead{reason: "usage-statement-unavailable"}, nil
}

// Whether two verified statements for the same chunks attest the same usage: the
// same version, totals and, for version 2, extender pool.
func subnetSameUsageStatement(a, b *usage.Statement) bool {
	if a.Schema != b.Schema || a.TotalUsageBytes != b.TotalUsageBytes || a.TotalUsers != b.TotalUsers || a.ProviderRows != b.ProviderRows {
		return false
	}
	if a.ExtenderPool == nil || b.ExtenderPool == nil {
		return a.ExtenderPool == nil && b.ExtenderPool == nil
	}
	pa, pb := a.ExtenderPool, b.ExtenderPool
	return pa.IntraPlatformBytes == pb.IntraPlatformBytes && pa.IntraNetworkBytes == pb.IntraNetworkBytes && pa.ExtenderPoolBytes == pb.ExtenderPoolBytes &&
		pa.ExtenderPoolWeightBytes == pb.ExtenderPoolWeightBytes && pa.ExtenderPoolContracts == pb.ExtenderPoolContracts && pa.ExtenderPoolPayers == pb.ExtenderPoolPayers &&
		strings.Join(pa.ExtenderCensus, ",") == strings.Join(pb.ExtenderCensus, ",") && pa.ExtenderUnits == pb.ExtenderUnits &&
		pa.EligibleExtenders == pb.EligibleExtenders && strings.Join(pa.ExtenderChunks, ",") == strings.Join(pb.ExtenderChunks, ",")
}

// Reads one history answer's statements into found, and reports whether every
// object was read. A statement that does not verify or decode marks invalid.
func readSubnetUsageHistory(ctx context.Context, run *subnetRun, origin string, history subnetUsageHistory, runId string, deployment SubnetDeploymentIdentity, noId, epoch uint64, signer common.Address, start, end subnetEvmBlock, found map[string]*usage.Statement, invalid *bool) bool {
	complete := true
	for _, object := range history.Objects {
		contentHash := "sha256:" + strings.TrimSuffix(filepath.Base(object.Key), ".json")
		if _, seen := found[contentHash]; seen {
			continue
		}
		raw, etag, err := readSubnetApi(ctx, run, origin, "/sn/evidence", url.Values{"hash": []string{contentHash}}, maximumSubnetUsageEnvelopeBytes)
		if subnetCanceled(ctx) != nil {
			return false
		}
		if err != nil {
			complete = false
			continue
		}
		statement, err := decodeSubnetUsageEnvelope(raw, etag, contentHash, runId, deployment)
		if err == nil {
			err = checkSubnetUsageStatement(statement, deployment, noId, epoch, signer, start, end)
		}
		if err != nil {
			*invalid = true
			continue
		}
		found[contentHash] = statement
	}
	return complete
}

// The evidence envelope only routes a statement; the statement's own signature
// authenticates what the audit uses.
func decodeSubnetUsageEnvelope(raw []byte, etag, contentHash, runId string, deployment SubnetDeploymentIdentity) (*usage.Statement, error) {
	var envelope struct {
		DeploymentID string          `json:"deployment_id"`
		Netuid       uint16          `json:"netuid"`
		Kind         string          `json:"kind"`
		RunID        string          `json:"run_id"`
		Payload      json.RawMessage `json:"payload"`
		ContentHash  string          `json:"content_hash"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.DeploymentID != deployment.DeploymentId || envelope.Netuid != deployment.Netuid || !slices.Contains(subnetUsageEvidenceKinds, envelope.Kind) || envelope.RunID != runId || !strings.EqualFold(envelope.ContentHash, contentHash) || etag != `"`+envelope.ContentHash+`"` {
		return nil, errors.New("usage statement envelope is outside the requested identity")
	}
	statement, err := usage.Decode(envelope.Payload)
	if err != nil {
		return nil, err
	}
	// a statement is published under its own version's kind
	if statement.Kind() != envelope.Kind {
		return nil, errors.New("usage statement is published under another version's kind")
	}
	return statement, nil
}

func checkSubnetUsageStatement(statement *usage.Statement, deployment SubnetDeploymentIdentity, noId, epoch uint64, signer common.Address, start, end subnetEvmBlock) error {
	if err := statement.Verify(signer); err != nil {
		return err
	}
	if statement.DeploymentID != deployment.DeploymentId || statement.ChainID != deployment.ChainId || statement.Netuid != deployment.Netuid || statement.Coordinator != deployment.coordinatorAddress() || statement.VaultV2 != common.HexToAddress(deployment.VaultV2) || statement.NoID != noId || statement.Epoch != epoch ||
		!strings.EqualFold(statement.GenesisHash, deployment.GenesisHash) || !strings.EqualFold(statement.PolicyHash, deployment.PolicyHash) {
		return errors.New("usage statement deployment identity mismatch")
	}
	if statement.Start.Number != start.number || statement.End.Number != end.number || !strings.EqualFold(statement.Start.Hash, start.hash.Hex()) || !strings.EqualFold(statement.End.Hash, end.hash.Hex()) {
		return errors.New("usage statement finalized boundary mismatch")
	}
	return nil
}

// The same exact formula as validator.EvaluateDepositArtifact, on a verified statement.
func subnetEvaluateUsageStatement(statement *usage.Statement, epoch, source, noId uint64, observed, conviction *big.Int, policy protocol.DepositPolicy) validator.DepositAudit {
	audit := validator.FailedDepositAudit(epoch, source, noId, observed, conviction, validator.DepositAuditMismatch, nil)
	required, tier, err := protocol.RequiredDepositRao(statement.TotalUsageBytes, statement.TotalUsers, conviction, policy)
	if err != nil {
		audit.Status, audit.Error = validator.DepositAuditInvalid, fmt.Sprintf("canonical deposit formula: %v", err)
		return audit
	}
	audit.UsageBytes, audit.Users = statement.TotalUsageBytes, statement.TotalUsers
	audit.RateNumeratorRaoPerGiB, audit.RateNumeratorRaoPerUser, audit.RateDenominator = tier.RateNumeratorRaoPerGiB, tier.RateNumeratorRaoPerUser, tier.RateDenominator
	audit.RequiredDepositRao = required.String()
	if observed.Cmp(required) != 0 {
		audit.Error = "observed deposit does not equal the signed-usage requirement"
		return audit
	}
	audit.Status, audit.Compliant, audit.Disposition = validator.DepositAuditCompliant, true, "pool_weight_eligible"
	return audit
}

// A statement expected and missing after the source's commit deadline. It pages while
// a price is in force, because the pool then has no weight. Ahead of a scheduled price
// it warns, because the first priced deposit will need the same pipeline.
func subnetUsageStatementFinding(run *subnetRun, checkpoint *subnetCheckpoint, target string, read subnetUsageStatementRead, deadline uint64, priced bool) finding {
	if read.reason != "" {
		return subnetUnknown(subnetDepositsFamily, subnetUsageStatementClass, target, read.reason, subnetTransientSustain, "the operator's signed usage statement at an enrolled API origin")
	}
	if !read.absent || checkpoint.evm.number <= deadline {
		return subnetHealthy(subnetDepositsFamily, subnetUsageStatementClass, target)
	}
	tier, sustain := tierWarn, subnetConfigurationSustain
	if priced {
		tier, sustain = tierPage, subnetProvedSustain
	}
	return subnetDecided(subnetDepositsFamily, subnetUsageStatementClass, target, "", run.scope("complete", "operator-usage-statement"), subnetDecision{
		state: "violated",
		issue: subnetReplayIssue{
			tier: tier, sustain: sustain,
			invariant: "usage-statement-published-by-root-deadline", expected: "published", observed: "absent",
			symptom:   "An operator's usage statement is missing after its epoch's commit deadline",
			mechanism: "A settlement v2 source epoch has no signed payout artifact, so its usage statement is the only source of the next deposit's size. Without one the operator's deposit task waits and validators give the pool zero weight. The statement needs the epoch's users, captured at the boundary, and its provider rows, recorded when its root is built.",
			baseline:  "Every settlement v2 epoch's statement is published by its commit deadline, at any price.",
			action:    "Check the taskworker's usage steps and the epoch's provider rows, including an unrecoverable declaration or a pending rebuild. Do not sign a statement by hand.",
			verify:    "The statement reads and verifies at both origins on a fresh run.",
		},
		evidence: fmt.Sprintf("Checkpoint EVM height %d; commit deadline %d.", checkpoint.evm.number, deadline),
	})
}

// A priced deposit still unpaid hours after its source totals exist. Its pool has had
// no weight all that time, and this is the signal that it is not a passing delay.
func subnetDepositLandingFinding(run *subnetRun, checkpoint *subnetCheckpoint, target string, audit validator.DepositAudit, observed *big.Int, totalsAt uint64) finding {
	if audit.Status != validator.DepositAuditMismatch || observed.Sign() != 0 || checkpoint.evm.number <= totalsAt+subnetDepositLandingGraceBlocks {
		return subnetHealthy(subnetDepositsFamily, subnetDepositLandingClass, target)
	}
	return subnetDecided(subnetDepositsFamily, subnetDepositLandingClass, target, "", run.scope("complete", "operator-deposit-counter"), subnetDecision{
		state: "violated",
		issue: subnetReplayIssue{
			tier: tierPage, sustain: subnetProvedSustain,
			invariant: "priced-deposit-lands-after-its-totals", expected: fmt.Sprintf("landed-within-%d-blocks", subnetDepositLandingGraceBlocks), observed: "unpaid",
			symptom:   "A priced demand deposit has not landed hours after its source totals existed",
			mechanism: "The operator owes a deposit, its source totals are published, and the epoch's deposit counter is still zero. Until it lands validators give the pool zero weight at every steering pass. The usual causes are a refused conversion, an empty wallet, the price guard, or a blocked deposit key.",
			baseline:  "An operator's deposit lands within hours of its source totals existing.",
			action:    "Read `bringyourctl st status` and `st demand-status` for the refusal, and restore what it names. Do not send a deposit by hand of a size other than the one owed.",
			verify:    "The epoch's deposit counter equals the requirement on a fresh run.",
		},
		evidence: fmt.Sprintf("Checkpoint EVM height %d; source totals seen by block %d. Required and observed amounts stay private.", checkpoint.evm.number, totalsAt),
	})
}

// After the audit's native cycle, every applied validator row gives a
// noncompliant pool zero or no weight. Rows applied before that cycle are
// old weights, not a bypass.
func observeSubnetDepositPenalties(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState, audits []subnetDepositAuditResult) ([]finding, error) {
	noncompliant := []subnetDepositAuditResult{}
	for _, result := range audits {
		if result.decided && !result.audit.Compliant && result.audit.Status != validator.DepositAuditZeroPrice {
			noncompliant = append(noncompliant, result)
		}
	}
	if len(noncompliant) == 0 {
		return []finding{subnetHealthy(subnetDepositsFamily, subnetDepositPenaltyBypassClass, "subnet-deposits/operators")}, nil
	}
	netuid := subnetNetuidArg(run.settings.Deployment.Netuid)
	var permits []types.Bool
	var updates []types.U64
	var tempo types.U16
	var reveal types.U64
	shared := []*subnetStorageRead{
		{pallet: "SubtensorModule", item: "ValidatorPermit", args: [][]byte{netuid}, value: &permits},
		{pallet: "SubtensorModule", item: "LastUpdate", args: [][]byte{netuid}, value: &updates},
		{pallet: "SubtensorModule", item: "Tempo", args: [][]byte{netuid}, value: &tempo},
		{pallet: "SubtensorModule", item: "RevealPeriodEpochs", args: [][]byte{netuid}, value: &reveal},
	}
	unknown := func(reason string) []finding {
		return []finding{subnetUnknown(subnetDepositsFamily, subnetDepositPenaltyBypassClass, "subnet-deposits/operators", reason, subnetTransientSustain, "every permitted validator's applied weight row and update block")}
	}
	if err := subnetReadStorageBatch(ctx, checkpoint.chain, checkpoint.nativeHash, shared); err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		return unknown("native-" + subnetReadReason(err)), nil
	}
	for _, read := range shared {
		if read.err != nil {
			return unknown("native-" + subnetReadReason(read.err)), nil
		}
	}
	rows := map[uint16]*[]subnetWeightPair{}
	reads := []*subnetStorageRead{}
	for uid, permitted := range permits {
		if !bool(permitted) {
			continue
		}
		row := []subnetWeightPair{}
		rows[uint16(uid)] = &row
		reads = append(reads, &subnetStorageRead{pallet: "SubtensorModule", item: "Weights", args: [][]byte{netuid, subnetNetuidArg(uint16(uid))}, value: rows[uint16(uid)]})
	}
	if err := subnetReadStorageBatch(ctx, checkpoint.chain, checkpoint.nativeHash, reads); err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		return unknown("native-" + subnetReadReason(err)), nil
	}
	for _, read := range reads {
		if read.err != nil {
			return unknown("native-" + subnetReadReason(read.err)), nil
		}
	}
	deadline := subnetEpochStartBlock(state.policies, state.currentEpoch) + state.currentPolicy.RootCommitWindowBlocks
	cycle := deadline + (uint64(reveal)+1)*uint64(tempo)
	findings := []finding{}
	for _, result := range noncompliant {
		target := subnetDepositsTarget(result.noId)
		operator := state.operators[result.noId]
		if operator.poolErr != nil {
			findings = append(findings, subnetUnknown(subnetDepositsFamily, subnetDepositPenaltyBypassClass, target, "pool-unavailable", subnetTransientSustain, "this operator's vault pool UID"))
			continue
		}
		weighted := 0
		judged := 0
		for uid, row := range rows {
			if int(uid) >= len(updates) || uint64(updates[uid]) < cycle {
				continue
			}
			judged++
			for _, pair := range *row {
				if pair.Uid == operator.pool.Uid && pair.Weight != 0 {
					weighted++
				}
			}
		}
		if weighted == 0 {
			findings = append(findings, subnetHealthy(subnetDepositsFamily, subnetDepositPenaltyBypassClass, target))
			continue
		}
		findings = append(findings, subnetDecided(subnetDepositsFamily, subnetDepositPenaltyBypassClass, target, "", run.scope("complete", "applied-native-weights"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "noncompliant-pool-zero-weight", expected: "no-applied-row-weights-the-pool", observed: fmt.Sprintf("%d-rows-weight-the-pool", weighted),
				symptom:   "A noncompliant operator's pool still receives applied validator weight",
				mechanism: "After the deposit audit's native cycle, an applied validator row still gives positive weight to the pool of an operator whose replayed audit failed. Validators must zero a noncompliant pool; bad demand weighted into Yuma becomes miner emission.",
				baseline:  "Every validator row applied after the audit's commit-reveal cycle gives a noncompliant pool zero or no weight.",
				action:    "Identify the validator rows and their CRv4 commits; check the validators' deposit audits before attributing intent.",
				verify:    "Every row applied after the next eligible cycle gives the pool zero weight, or the operator's audit becomes compliant.",
			},
			evidence: fmt.Sprintf("Checkpoint height %d; rows applied at or after block %d are judged; earlier rows are old applied weights, not a bypass.", checkpoint.number, cycle),
			details:  fmt.Sprintf("status=%s rows_weighting_pool=%d rows_judged=%d", result.audit.Status, weighted, judged),
		}))
	}
	return findings, nil
}
