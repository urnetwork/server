// SIGNALS.md §22.3 operator demand deposits, conviction and reserve. At one
// agreed approved checkpoint the reader reconciles the reserve sink's
// principal with every operator's principal and cumulative conviction and
// with the staking precompile's own read of the reserve position, requires the
// reserve take to be zero and its custody to stay immutable across
// observations, pairs every Deposit and ConvictionAdded event with its
// ReservePrincipalAdded receipt over a bounded finalized frontier, replays the
// validators' exact deposit audit against the signed, committed prior-epoch
// payout artifact, and checks that applied native weight rows after the
// audit's native cycle give a noncompliant pool no weight.
package monitor

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"

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
)

var subnetDepositsFamily = subnetFamily{
	number: "22.3", key: "subnet-deposits", probeId: "subnet/deposits",
	title:   "operator demand deposits, conviction and reserve",
	classes: []string{subnetDepositAttributionClass, subnetOperatorDepositMismatchClass, subnetDepositPenaltyBypassClass, subnetReservePrincipalClass, subnetReserveCreditDeltaClass, subnetReserveYieldClass},
	sources: "the agreed finalized checkpoint, the reserve sink and coordinator deposit views, the staking precompile, native Delegates, ChildkeyTake, Weights, LastUpdate and ValidatorPermit, Deposit, ConvictionAdded and ReservePrincipalAdded logs over a bounded frontier, the committed prior-epoch payout artifacts at both API origins, and the reviewed policy document and operator signers",
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
}

func newSubnetDepositsProbe() *subnetDepositsProbe {
	return &subnetDepositsProbe{runtimes: newSubnetRuntimeCache(), depositLogs: newSubnetLogFrontier(), ledger: &subnetDepositLedger{census: newSubnetDepositCensus()}, latch: newSubnetReplayLatch()}
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
	audits, auditFindings, err := observeSubnetDepositAudits(ctx, run, checkpoint, state, deposits, sourceStart, sourceEnd)
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

// Replays the validators' deposit audit for every on-chain operator against
// the signed, committed prior-epoch payout artifact and the reviewed policy.
// Both underdeposit and overdeposit are mismatches; a pending artifact before
// its deadline is not.
func observeSubnetDepositAudits(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState, deposits map[uint64]*subnetOperatorDeposits, sourceStart, sourceEnd subnetEvmBlock) ([]subnetDepositAuditResult, []finding, error) {
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
		var audit validator.DepositAudit
		switch {
		case policy.Deposit.IsZeroPrice():
			audit = validator.ZeroPriceDepositAudit(epoch, max(epoch, lag)-lag, noId, observed, conviction)
		case epoch < lag:
			audit = validator.DepositAudit{NoID: noId, Epoch: epoch, Status: validator.DepositAuditBootstrap, Compliant: observed.Sign() == 0, Disposition: "zero_pool_weight_bootstrap"}
			if observed.Sign() != 0 {
				audit.Status = validator.DepositAuditMismatch
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
				}, epoch, observed, conviction, policy.Deposit)
			}
		}
		audit.ObservedAtBlock = checkpoint.evm.number
		audit.ArtifactDeadlineBlock = deadline
		decided := audit.Status != validator.DepositAuditUnavailablePending
		results = append(results, subnetDepositAuditResult{noId: noId, audit: audit, decided: decided})
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
				mechanism: "The exact deposit audit validators run, replayed against the committed, signed prior-epoch payout artifact, found the observed deposit unequal to the required one, or found the artifact invalid, equivocating or unavailable after its root window. Both underdeposit and overdeposit are mismatches; validators give such a pool zero weight.",
				baseline:  "Each operator's epoch deposit equals the required deposit for its signed prior-epoch usage, or its artifact is still pending before the root window closes, or the reviewed policy is zero-price.",
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
	return results, findings, nil
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
