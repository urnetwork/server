// SIGNALS.md §22.5 top-200 promotion, zero weight, demotion and tier
// isolation. The reader replays the coordinator's fleet binding census from
// the deployment block over a bounded finalized frontier, then judges every
// permitted validator's applied native row independently: positive head
// weight only to bound fleets, at most the policy's head limit, never to the
// validator itself. It checks that no client is paid by both tiers, using the
// coordinator's own bindingAt at each committed payout artifact's boundary
// blocks, that head incentive accrues to the fleet's own keys rather than a
// custody or operator key or a reassigned Uid, and how many head UIDs change
// hands. The validators' selection replay and realized pay need decision and
// pay ledgers no monitor-readable source publishes; those stay unknown.
package monitor

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
)

const subnetHeadRunBudget = 180 * time.Second

// The protocol's own bound on the head fleet count.
const maximumSubnetHeadFleets = 200

// The reserved incident classes of §22.5.
const (
	subnetHeadSelectionClass      = "subnet-head-selection"
	subnetHeadWeightBypassClass   = "subnet-head-weight-bypass"
	subnetPromotionChurnClass     = "subnet-promotion-churn"
	subnetTierDoublePayClass      = "subnet-tier-double-pay"
	subnetHeadNativePayoutClass   = "subnet-head-native-payout"
	subnetPromotionEconomicsClass = "subnet-promotion-economics"
)

var subnetHeadFamily = subnetFamily{
	number: "22.5", key: "subnet-head", probeId: "subnet/head",
	title:   "top-200 promotion, zero weight, demotion and tier isolation",
	classes: []string{subnetHeadSelectionClass, subnetHeadWeightBypassClass, subnetPromotionChurnClass, subnetTierDoublePayClass, subnetHeadNativePayoutClass, subnetPromotionEconomicsClass},
	sources: "the agreed finalized checkpoint, the coordinator's FleetBound, FleetBindingRevoked and FleetBindingCleaned census from the deployment block, its bindingAt at each payout artifact's boundary blocks, native ValidatorPermit, Weights, LastUpdate, Keys, Owner, Incentive and SubnetOwnerHotkey, the committed prior-epoch payout artifacts at both API origins, and the reviewed policy, validator census and treasury recipients",
}

func NewSubnetHeadSignal() Signal {
	return &signalAdapter{
		number: "22.5",
		key:    "subnet-head",
		name:   "Subnet top-200 promotion, zero weight, demotion and tier isolation",
		probe:  newSubnetHeadProbe(),
	}
}

// Probe-owned observation state: verified runtimes, the binding frontier,
// the committed binding census and the last observed head Uid owners.
type subnetHeadProbe struct {
	runtimes    *subnetRuntimeCache
	bindingLogs *subnetLogFrontier
	ledger      *subnetBindingLedger
}

func newSubnetHeadProbe() *subnetHeadProbe {
	return &subnetHeadProbe{runtimes: newSubnetRuntimeCache(), bindingLogs: newSubnetLogFrontier(), ledger: &subnetBindingLedger{census: newSubnetBindingCensus()}}
}

func (*subnetHeadProbe) id() string               { return "subnet/head" }
func (*subnetHeadProbe) tier() string             { return tierWarn }
func (*subnetHeadProbe) cadence() time.Duration   { return 5 * time.Minute }
func (*subnetHeadProbe) runBudget() time.Duration { return subnetHeadRunBudget }

// One binding version, replayed from its finalized events.
type subnetBindingVersion struct {
	fleetId    [32]byte
	hotkey     [32]byte
	uid        uint16
	generation uint64
	validFrom  uint64
	validTo    uint64
	cleaned    bool
	cleanedAt  uint64
}

// The committed binding census of every published frontier window.
type subnetBindingLedger struct {
	stateLock sync.Mutex
	census    *subnetBindingCensus
}

func (self *subnetBindingLedger) copy() *subnetBindingCensus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.census.clone()
}

func (self *subnetBindingLedger) publish(census *subnetBindingCensus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.census = census
}

// Every client's binding versions in event order, and the head Uid owners
// last observed. It applies the coordinator's own rules: a revocation ends a
// version before its effective epoch, a cleanup ends the latest version at
// its cleanup epoch, and bindingAt reads the latest version already valid.
type subnetBindingCensus struct {
	versions  map[[16]byte][]subnetBindingVersion
	headOwner map[uint16][32]byte
}

func newSubnetBindingCensus() *subnetBindingCensus {
	return &subnetBindingCensus{versions: map[[16]byte][]subnetBindingVersion{}, headOwner: map[uint16][32]byte{}}
}

func (self *subnetBindingCensus) clone() *subnetBindingCensus {
	copy := newSubnetBindingCensus()
	for client, versions := range self.versions {
		copy.versions[client] = append([]subnetBindingVersion(nil), versions...)
	}
	for uid, hotkey := range self.headOwner {
		copy.headOwner[uid] = hotkey
	}
	return copy
}

func (self *subnetBindingCensus) bind(event *stabi.STCoordinatorFleetBound) {
	self.versions[event.ClientId] = append(self.versions[event.ClientId], subnetBindingVersion{
		fleetId: event.FleetId, hotkey: event.Hotkey, uid: event.Uid, generation: event.Generation, validFrom: event.ValidFromEpoch, validTo: event.ValidToEpoch,
	})
}

// False when no version has the revoked generation: the census is incomplete.
func (self *subnetBindingCensus) revoke(event *stabi.STCoordinatorFleetBindingRevoked) bool {
	versions := self.versions[event.ClientId]
	for i := len(versions) - 1; 0 <= i; i-- {
		if versions[i].generation == event.Generation && event.EffectiveEpoch > 0 {
			versions[i].validTo = event.EffectiveEpoch - 1
			return true
		}
	}
	return false
}

func (self *subnetBindingCensus) clean(event *stabi.STCoordinatorFleetBindingCleaned) bool {
	versions := self.versions[event.ClientId]
	if len(versions) == 0 {
		return false
	}
	versions[len(versions)-1].cleaned = true
	versions[len(versions)-1].cleanedAt = event.CleanedAtEpoch
	return true
}

// bindingAt without its live Uid test: the version and its validity.
func (self *subnetBindingCensus) boundAt(client [16]byte, epoch uint64) (subnetBindingVersion, bool) {
	versions := self.versions[client]
	for i := len(versions) - 1; 0 <= i; i-- {
		version := versions[i]
		if epoch < version.validFrom {
			continue
		}
		return version, epoch <= version.validTo && (!version.cleaned || epoch < version.cleanedAt)
	}
	return subnetBindingVersion{}, false
}

// Every Uid a binding valid at one of the epochs names, with its hotkeys.
func (self *subnetBindingCensus) boundUids(epochs ...uint64) map[uint16]map[[32]byte]bool {
	uids := map[uint16]map[[32]byte]bool{}
	for client := range self.versions {
		for _, epoch := range epochs {
			if version, active := self.boundAt(client, epoch); active {
				if uids[version.uid] == nil {
					uids[version.uid] = map[[32]byte]bool{}
				}
				uids[version.uid][version.hotkey] = true
			}
		}
	}
	return uids
}

func (self *subnetBindingCensus) everBound(client [16]byte) bool {
	return len(self.versions[client]) != 0
}

// Records the current head Uid owners; returns how many changed hands.
func (self *subnetBindingCensus) observeHeadOwners(owners map[uint16][32]byte) uint64 {
	changed := uint64(0)
	for uid, hotkey := range owners {
		if previous, ok := self.headOwner[uid]; ok && previous != hotkey {
			changed++
		}
		self.headOwner[uid] = hotkey
	}
	return changed
}

func (self *subnetHeadProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	gate, err := openSubnetRun(ctx, env, subnetHeadFamily)
	if err != nil || gate.run == nil {
		return gate.findings, err
	}
	run := gate.run
	findings, err := self.observe(ctx, run)
	if err != nil {
		run.closer()
		return nil, err
	}
	return run.close(ctx, env, subnetHeadFamily, findings)
}

func (self *subnetHeadProbe) observe(ctx context.Context, run *subnetRun) ([]finding, error) {
	checkpoint, state, gap, err := openSubnetState(ctx, run, self.runtimes, subnetHeadFamily)
	if err != nil || checkpoint == nil {
		return gap, err
	}
	findings := []finding{
		subnetUnknown(subnetHeadFamily, subnetPromotionEconomicsClass, "subnet-head/economics", "pay-ledger-unpublished", subnetConfigurationSustain, "an aligned mature per-epoch ledger of realized head and tail pay"),
	}
	census := self.ledger.copy()
	run.afterPublish(func() { self.ledger.publish(census) })
	complete, err := self.advanceBindings(ctx, run, checkpoint, state, census)
	if err != nil {
		return nil, err
	}
	policy := subnetEnrolledPolicy(run.settings)
	rows, err := observeSubnetHeadRows(ctx, run, checkpoint, state, census, complete, policy)
	if err != nil {
		return nil, err
	}
	findings = append(findings, rows...)
	tiers, err := observeSubnetTierIsolation(ctx, run, checkpoint, state, census, complete)
	if err != nil {
		return nil, err
	}
	return append(findings, tiers...), nil
}

// Advances the binding census over the frontier; reports whether it is a
// complete replay from the deployment block to the checkpoint.
func (self *subnetHeadProbe) advanceBindings(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState, census *subnetBindingCensus) (bool, error) {
	coordinatorAbi, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return false, subnetReadFailure("coordinator-abi-invalid", err)
	}
	limits := run.limits
	// The census must replay every binding since deployment.
	limits.initialLogWindowBlocks = math.MaxUint64
	plan := self.bindingLogs.plan(checkpoint.number, run.settings.DeployBlock, limits)
	topics := []common.Hash{coordinatorAbi.Events["FleetBound"].ID, coordinatorAbi.Events["FleetBindingRevoked"].ID, coordinatorAbi.Events["FleetBindingCleaned"].ID}
	logs, err := subnetFetchLogs(ctx, checkpoint.evmReader, state.coordinator, [][]common.Hash{topics}, plan, limits)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return false, canceled
	}
	if err != nil {
		return false, nil
	}
	sort.Slice(logs, func(i, j int) bool {
		if logs[i].BlockNumber != logs[j].BlockNumber {
			return logs[i].BlockNumber < logs[j].BlockNumber
		}
		return logs[i].Index < logs[j].Index
	})
	coordinator := stabi.NewSTCoordinator()
	consistent := true
	for i := range logs {
		log := &logs[i]
		switch log.Topics[0] {
		case topics[0]:
			event, err := coordinator.UnpackFleetBoundEvent(log)
			if err != nil {
				consistent = false
				continue
			}
			census.bind(event)
		case topics[1]:
			event, err := coordinator.UnpackFleetBindingRevokedEvent(log)
			consistent = consistent && err == nil && census.revoke(event)
		default:
			event, err := coordinator.UnpackFleetBindingCleanedEvent(log)
			consistent = consistent && err == nil && census.clean(event)
		}
	}
	if !plan.empty {
		run.afterPublish(func() { self.bindingLogs.advance(plan.to) })
	}
	return plan.complete && !plan.truncated && consistent, nil
}

// Per-validator row evidence.
type subnetHeadRow struct {
	uid     uint16
	pairs   []subnetWeightPair
	updated uint64
}

// Judges every permitted validator's applied row: positive weight only to a
// pool, a bound fleet, a reviewed treasury recipient or the subnet owner,
// never to the validator itself, and at most the policy's head limit. Then
// checks head incentive custody and head Uid churn.
func observeSubnetHeadRows(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState, census *subnetBindingCensus, complete bool, policy *protocol.Policy) ([]finding, error) {
	netuid := subnetNetuidArg(run.settings.Deployment.Netuid)
	var permits []types.Bool
	var updates []types.U64
	var incentives []types.U16
	var ownerHotkey types.AccountID
	shared := []*subnetStorageRead{
		{pallet: "SubtensorModule", item: "ValidatorPermit", args: [][]byte{netuid}, value: &permits},
		{pallet: "SubtensorModule", item: "LastUpdate", args: [][]byte{netuid}, value: &updates},
		{pallet: "SubtensorModule", item: "Incentive", args: [][]byte{netuid}, value: &incentives},
		{pallet: "SubtensorModule", item: "SubnetOwnerHotkey", args: [][]byte{netuid}, value: &ownerHotkey},
	}
	unknown := func(reason string) []finding {
		return []finding{
			subnetUnknown(subnetHeadFamily, subnetHeadWeightBypassClass, "subnet-head/weights", reason, subnetTransientSustain, "every permitted validator's applied row and the bound head census"),
			subnetUnknown(subnetHeadFamily, subnetHeadSelectionClass, "subnet-head/selection", reason, subnetTransientSustain, "every permitted validator's applied row"),
			subnetUnknown(subnetHeadFamily, subnetHeadNativePayoutClass, "subnet-head/payout", reason, subnetTransientSustain, "native incentive and head UID ownership"),
			subnetUnknown(subnetHeadFamily, subnetPromotionChurnClass, "subnet-head/churn", reason, subnetTransientSustain, "head UID ownership across observations"),
		}
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
	rows := []*subnetHeadRow{}
	reads := []*subnetStorageRead{}
	for uid, permitted := range permits {
		if !bool(permitted) {
			continue
		}
		row := &subnetHeadRow{uid: uint16(uid)}
		if uid < len(updates) {
			row.updated = uint64(updates[uid])
		}
		rows = append(rows, row)
		reads = append(reads, &subnetStorageRead{pallet: "SubtensorModule", item: "Weights", args: [][]byte{netuid, subnetNetuidArg(uint16(uid))}, value: &row.pairs})
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
	// The UIDs any row owns or names, or any incentive pays, resolved to their
	// hotkeys.
	named := map[uint16]bool{}
	for _, row := range rows {
		named[row.uid] = true
		for _, pair := range row.pairs {
			named[pair.Uid] = true
		}
	}
	for uid, incentive := range incentives {
		if uint16(incentive) != 0 {
			named[uint16(uid)] = true
		}
	}
	keys := map[uint16]*types.AccountID{}
	keyReads := []*subnetStorageRead{}
	for uid := range named {
		key := new(types.AccountID)
		keys[uid] = key
		keyReads = append(keyReads, &subnetStorageRead{pallet: "SubtensorModule", item: "Keys", args: [][]byte{netuid, subnetNetuidArg(uid)}, value: key})
	}
	if err := subnetReadStorageBatch(ctx, checkpoint.chain, checkpoint.nativeHash, keyReads); err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		return unknown("native-" + subnetReadReason(err)), nil
	}
	pools := map[uint16]bool{}
	custody := map[[32]byte]bool{state.escrowHotkey: true, state.reserveHotkey: true}
	custodyColdkeys := map[[32]byte]bool{state.vaultColdkey: true, state.sinkColdkey: true, state.coordinatorColdkey: true}
	for _, operator := range state.operators {
		if operator.poolErr == nil {
			pools[operator.pool.Uid] = true
			custody[operator.pool.Hotkey] = true
		}
		if operator.versionErr == nil {
			custody[operator.version.DepositHotkey] = true
			custodyColdkeys[operator.version.Coldkey] = true
		}
	}
	treasury := map[[32]byte]bool{}
	for _, hotkey := range run.settings.TreasuryHotkeys {
		key, _ := subnetParseHash(hotkey)
		treasury[[32]byte(key)] = true
	}
	reviewedUids := map[uint16]int{}
	for i, validator := range run.settings.Validators {
		hotkey, _ := subnetParseHash(validator.Hotkey)
		for _, row := range rows {
			if key, ok := keys[row.uid]; ok && [32]byte(*key) == [32]byte(hotkey) {
				reviewedUids[row.uid] = i
			}
		}
	}
	// A row decided in an earlier epoch is judged against that epoch's census
	// as well as the current one.
	bound := census.boundUids(state.currentEpoch, max(state.currentEpoch, 1)-1)
	bypassReasons := map[string][]string{}
	selectionReasons := []string{}
	unexplained := 0
	for _, row := range rows {
		target := "subnet-head/other-validators"
		if index, ok := reviewedUids[row.uid]; ok {
			target = subnetValidatorTarget(subnetHeadFamily, index)
		}
		heads := 0
		for _, pair := range row.pairs {
			if pair.Weight == 0 {
				continue
			}
			key, _ := keys[pair.Uid]
			switch {
			case pair.Uid == row.uid:
				bypassReasons[target] = append(bypassReasons[target], "self-weight")
			case pools[pair.Uid]:
			case bound[pair.Uid] != nil:
				heads++
			case key != nil && (treasury[[32]byte(*key)] || [32]byte(*key) == [32]byte(ownerHotkey)):
			case len(treasury) == 0:
				unexplained++
			default:
				bypassReasons[target] = append(bypassReasons[target], "unbound-uid-weighted")
			}
		}
		limit := maximumSubnetHeadFleets
		if policy != nil {
			limit = int(policy.Steering.MaximumHeadFleets)
		}
		if heads > limit {
			selectionReasons = append(selectionReasons, fmt.Sprintf("head-limit-exceeded:%s", strings.TrimPrefix(target, "subnet-head/")))
		}
	}
	findings := []finding{}
	evidence := fmt.Sprintf("Checkpoint height %d; %d permitted validator rows; %d bound head UIDs in the current or previous epoch; binding census complete=%t.", checkpoint.number, len(rows), len(bound), complete)
	switch {
	case !complete:
		findings = append(findings, subnetUnknown(subnetHeadFamily, subnetHeadWeightBypassClass, "subnet-head/weights", "binding-census-incomplete", subnetTransientSustain, "a complete replay of every fleet binding since deployment"))
	case unexplained != 0:
		findings = append(findings, subnetUnknown(subnetHeadFamily, subnetHeadWeightBypassClass, "subnet-head/weights", "treasury-recipients-not-enrolled", subnetConfigurationSustain, "the reviewed treasury recipients of the owner recycle row"))
	case len(bypassReasons) == 0:
		findings = append(findings, subnetHealthy(subnetHeadFamily, subnetHeadWeightBypassClass, "subnet-head/weights"))
	}
	if complete {
		targets := []string{}
		for target := range bypassReasons {
			targets = append(targets, target)
		}
		sort.Strings(targets)
		for _, target := range targets {
			findings = append(findings, finding{
				probeId: subnetHeadFamily.probeId, tier: tierPage, class: subnetHeadWeightBypassClass,
				target: target, sustain: subnetProvedSustain,
				symptom:   "A validator's applied row pays head weight outside the bound fleets",
				mechanism: "An applied native weight row gives positive weight to a UID that is no operator pool, no fleet bound in the current or previous epoch, no reviewed treasury recipient and not the subnet owner, or to the validator itself. Registration alone never establishes eligibility.",
				baseline:  "Every positive weight goes to a pool, a bound head fleet, a reviewed treasury recipient or the subnet owner, and no validator weights itself.",
				observed:  "state=violated reasons=" + strings.Join(subnetSortedUnique(bypassReasons[target]), ","),
				evidence:  evidence + " An earlier valid cycle's still-pending emission is not new weight; only the applied row is judged.",
				action:    "Identify the row's CRv4 commit and its validator's head decision before attribution; distinguish a not-yet-live winning challenger from an unbound claimant.",
				verify:    "The validator's next applied row weights only eligible UIDs.",
				playbook:  "SIGNALS.md §22.5",
			})
		}
	}
	if len(selectionReasons) != 0 && complete {
		findings = append(findings, finding{
			probeId: subnetHeadFamily.probeId, tier: tierPage, class: subnetHeadSelectionClass,
			target: "subnet-head/selection", sustain: subnetProvedSustain,
			symptom:   "A validator weights more head fleets than the policy allows",
			mechanism: "An applied row gives positive weight to more bound head fleets than the reviewed policy's head limit. Selection must choose at most that many fleets by split-adjusted breadth.",
			baseline:  "Every applied row weights at most the policy's head limit of bound fleets.",
			observed:  "state=violated reasons=" + strings.Join(subnetSortedUnique(selectionReasons), ","),
			evidence:  evidence,
			action:    "Replay the validator's selection from its retained measurement before attribution.",
			verify:    "The validator's next applied row is within the head limit.",
			playbook:  "SIGNALS.md §22.5",
		})
	} else {
		findings = append(findings, subnetUnknown(subnetHeadFamily, subnetHeadSelectionClass, "subnet-head/selection", "validator-decision-unpublished", subnetConfigurationSustain, "each validator's signed release measurement, which carries its full candidate census, split-adjusted breadth, EMA and tie break"))
	}
	findings = append(findings, observeSubnetHeadPayout(ctx, run, checkpoint, census, complete, incentives, keys, pools, bound, custody, custodyColdkeys, treasury, [32]byte(ownerHotkey))...)
	return findings, nil
}

// Head incentive accrues to the bound fleet's own hotkey and a coldkey that is
// no custody or operator key; a bound Uid whose hotkey changed is reused.
func observeSubnetHeadPayout(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, census *subnetBindingCensus, complete bool, incentives []types.U16, keys map[uint16]*types.AccountID, pools map[uint16]bool, bound map[uint16]map[[32]byte]bool, custody, custodyColdkeys, treasury map[[32]byte]bool, ownerHotkey [32]byte) []finding {
	payoutTarget := "subnet-head/payout"
	churnTarget := "subnet-head/churn"
	if !complete {
		return []finding{
			subnetUnknown(subnetHeadFamily, subnetHeadNativePayoutClass, payoutTarget, "binding-census-incomplete", subnetTransientSustain, "a complete replay of every fleet binding since deployment"),
			subnetUnknown(subnetHeadFamily, subnetPromotionChurnClass, churnTarget, "binding-census-incomplete", subnetTransientSustain, "a complete replay of every fleet binding since deployment"),
		}
	}
	netuid := subnetNetuidArg(run.settings.Deployment.Netuid)
	heads := []uint16{}
	for uid := range bound {
		if !pools[uid] {
			heads = append(heads, uid)
		}
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i] < heads[j] })
	owners := map[uint16]*types.AccountID{}
	reads := []*subnetStorageRead{}
	for _, uid := range heads {
		key := keys[uid]
		if key == nil {
			key = new(types.AccountID)
			keys[uid] = key
			reads = append(reads, &subnetStorageRead{pallet: "SubtensorModule", item: "Keys", args: [][]byte{netuid, subnetNetuidArg(uid)}, value: key})
		}
	}
	if err := subnetReadStorageBatch(ctx, checkpoint.chain, checkpoint.nativeHash, reads); err != nil {
		return []finding{subnetUnknown(subnetHeadFamily, subnetHeadNativePayoutClass, payoutTarget, "native-"+subnetReadReason(err), subnetTransientSustain, "native Keys of every bound head UID")}
	}
	reads = reads[:0]
	for _, uid := range heads {
		owner := new(types.AccountID)
		owners[uid] = owner
		key := [32]byte(*keys[uid])
		reads = append(reads, &subnetStorageRead{pallet: "SubtensorModule", item: "Owner", args: [][]byte{key[:]}, value: owner})
	}
	if err := subnetReadStorageBatch(ctx, checkpoint.chain, checkpoint.nativeHash, reads); err != nil {
		return []finding{subnetUnknown(subnetHeadFamily, subnetHeadNativePayoutClass, payoutTarget, "native-"+subnetReadReason(err), subnetTransientSustain, "native Owner of every bound head hotkey")}
	}
	payoutReasons := []string{}
	headOwners := map[uint16][32]byte{}
	for _, uid := range heads {
		hotkey := [32]byte(*keys[uid])
		headOwners[uid] = hotkey
		paid := int(uid) < len(incentives) && uint16(incentives[uid]) != 0
		if !paid {
			continue
		}
		if !bound[uid][hotkey] {
			payoutReasons = append(payoutReasons, "head-uid-reassigned")
		}
		if custody[hotkey] || custodyColdkeys[[32]byte(*owners[uid])] || treasury[hotkey] || hotkey == ownerHotkey {
			payoutReasons = append(payoutReasons, "head-incentive-to-custody-key")
		}
	}
	findings := []finding{}
	evidence := fmt.Sprintf("Checkpoint height %d; %d bound head UIDs. Keys and coldkeys are compared in memory and not rendered.", checkpoint.number, len(heads))
	if len(payoutReasons) != 0 {
		findings = append(findings, finding{
			probeId: subnetHeadFamily.probeId, tier: tierPage, class: subnetHeadNativePayoutClass,
			target: payoutTarget, sustain: subnetProvedSustain,
			symptom:   "Head incentive accrues to the wrong recipient",
			mechanism: "A bound head UID with positive native incentive is now held by a hotkey its binding does not name, or by an operator, escrow, reserve or pool hotkey, or by a hotkey owned by a custody or operator coldkey. Head emission is native and belongs to the selected fleet's own keys.",
			baseline:  "Every paid head UID is held by its bound fleet hotkey under a coldkey that is no custody or operator key.",
			observed:  "state=violated reasons=" + strings.Join(subnetSortedUnique(payoutReasons), ","),
			evidence:  evidence + " Registration funding, user transfers, delegation, dividends and share rounding are separate from incentive.",
			action:    "Preserve the UID's registration and binding history; reconstruct the reassignment boundary before any attribution.",
			verify:    "Every paid head UID is held by its bound fleet's own keys on a fresh run.",
			playbook:  "SIGNALS.md §22.5",
		})
	} else {
		findings = append(findings, subnetHealthy(subnetHeadFamily, subnetHeadNativePayoutClass, payoutTarget))
	}
	changed := census.observeHeadOwners(headOwners)
	if changed > run.limits.maxHeadChurnPerRun {
		findings = append(findings, finding{
			probeId: subnetHeadFamily.probeId, tier: tierWarn, class: subnetPromotionChurnClass,
			target: churnTarget, sustain: subnetTransientSustain,
			symptom:   "Head UIDs are changing hands faster than the reviewed bound",
			mechanism: "Between consecutive observations more bound head UIDs changed hotkey than the reviewed churn bound. Unproductive promotion and demotion churn burns registration cost and dips coverage-driven scores.",
			baseline:  "Head UID ownership changes no faster than the reviewed bound between observations.",
			observed:  fmt.Sprintf("state=churning changed_head_uids=%d bound=%d", changed, run.limits.maxHeadChurnPerRun),
			evidence:  evidence + " Authorized burn ceilings and the promotion SLO need the fleets' authorization records, which are not monitor-readable.",
			action:    "Review immunity, the emission floor and registration timing with the head selection owners; governance decides any change.",
			verify:    "Churn returns within the bound over later observations.",
			playbook:  "SIGNALS.md §22.5",
		})
	} else {
		findings = append(findings, subnetHealthy(subnetHeadFamily, subnetPromotionChurnClass, churnTarget))
	}
	return findings
}

// Every tail provider of every committed source-epoch payout artifact must be
// unbound at both of its boundary blocks by the coordinator's own bindingAt;
// a head-excluded provider must be bound at one of them.
func observeSubnetTierIsolation(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState, census *subnetBindingCensus, complete bool) ([]finding, error) {
	findings := []finding{}
	if state.currentEpoch == 0 {
		return []finding{subnetHealthy(subnetHeadFamily, subnetTierDoublePayClass, "subnet-head/tiers")}, nil
	}
	source := state.currentEpoch - 1
	coordinator := stabi.NewSTCoordinator()
	calls := []subnetEvmCall{}
	for _, noId := range state.operatorIds {
		calls = append(calls, subnetEvmCall{to: state.coordinator, data: coordinator.PackRootCommitments(new(big.Int).SetUint64(source), new(big.Int).SetUint64(noId))})
	}
	if err := checkpoint.evmReader.callBatch(ctx, calls, checkpoint.evm); err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		return []finding{subnetUnknown(subnetHeadFamily, subnetTierDoublePayClass, "subnet-head/tiers", "commitments-"+subnetReadReason(err), subnetTransientSustain, "the committed source-epoch payout roots")}, nil
	}
	for i, noId := range state.operatorIds {
		target := fmt.Sprintf("subnet-head/no-%d", noId)
		if calls[i].err != nil {
			findings = append(findings, subnetUnknown(subnetHeadFamily, subnetTierDoublePayClass, target, "commitment-"+subnetReadReason(calls[i].err), subnetTransientSustain, "this operator's committed source-epoch payout root"))
			continue
		}
		commitment, err := coordinator.UnpackRootCommitments(calls[i].output)
		if err != nil {
			findings = append(findings, subnetUnknown(subnetHeadFamily, subnetTierDoublePayClass, target, "commitment-invalid", subnetTransientSustain, "this operator's committed source-epoch payout root"))
			continue
		}
		if commitment.CommitBlock == 0 {
			findings = append(findings, subnetHealthy(subnetHeadFamily, subnetTierDoublePayClass, target))
			continue
		}
		read, err := readSubnetCommittedArtifact(ctx, run, subnetArtifactContentHash(commitment.ArtifactHash))
		if err != nil {
			return nil, err
		}
		if read.artifact == nil {
			findings = append(findings, subnetUnknown(subnetHeadFamily, subnetTierDoublePayClass, target, read.reason, subnetTransientSustain, "the committed source-epoch payout artifact at an enrolled API origin"))
			continue
		}
		reasons, unknownReason, err := subnetArtifactTierReasons(ctx, run, checkpoint, state, read.artifact, census, complete)
		if err != nil {
			return nil, err
		}
		switch {
		case unknownReason != "":
			findings = append(findings, subnetUnknown(subnetHeadFamily, subnetTierDoublePayClass, target, unknownReason, subnetTransientSustain, "bindingAt at the artifact's boundary blocks for every candidate provider"))
		case len(reasons) == 0:
			findings = append(findings, subnetHealthy(subnetHeadFamily, subnetTierDoublePayClass, target))
		default:
			tier := tierWarn
			for _, reason := range reasons {
				if reason == "head-client-in-tail" {
					tier = tierPage
				}
			}
			findings = append(findings, finding{
				probeId: subnetHeadFamily.probeId, tier: tier, class: subnetTierDoublePayClass,
				target: target, sustain: subnetProvedSustain,
				symptom:   "A client is assigned to the wrong payout tier",
				mechanism: "The coordinator's bindingAt at the artifact's own boundary blocks shows a tail provider bound to an active head fleet, so it is paid as both a head miner and a pool provider, or shows a head-excluded provider bound at neither boundary, so it is paid by neither tier.",
				baseline:  "Every provider active in a head fleet at either boundary is head-excluded from every tail artifact, and every head-excluded provider is bound at a boundary.",
				observed:  fmt.Sprintf("state=violated reasons=%s epoch=%d", strings.Join(subnetSortedUnique(reasons), ","), source),
				evidence:  fmt.Sprintf("Checkpoint height %d; artifact boundaries %d to %d. Client identities stay private.", checkpoint.number, read.artifact.Start.Number, read.artifact.End.Number),
				action:    "Compare the binding history, the client-level payout inputs and native ownership before any repayment; do not compare aggregated coldkey totals only.",
				verify:    "The next committed artifact excludes exactly the boundary-active head clients.",
				playbook:  "SIGNALS.md §22.5",
			})
		}
	}
	return findings, nil
}

// The tier reasons of one artifact, or why it cannot be decided.
func subnetArtifactTierReasons(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState, artifact *payoutartifact.Artifact, census *subnetBindingCensus, complete bool) ([]string, string, error) {
	candidates := []payoutartifact.ProviderInput{}
	for _, provider := range artifact.Providers {
		tail := provider.Eligible && !provider.HeadExcluded
		if complete && tail && !census.everBound(provider.ClientID) {
			continue
		}
		if tail || provider.HeadExcluded {
			candidates = append(candidates, provider)
		}
	}
	if len(candidates) > run.limits.maxArtifactProviders {
		return nil, "artifact-candidates-exceed-bound", nil
	}
	start, startOk := subnetParseHash(strings.ToLower(artifact.Start.Hash))
	end, endOk := subnetParseHash(strings.ToLower(artifact.End.Hash))
	if !startOk || !endOk {
		return nil, "artifact-boundaries-invalid", nil
	}
	coordinator := stabi.NewSTCoordinator()
	epoch := new(big.Int).SetUint64(artifact.Epoch)
	active := make([]bool, len(candidates))
	for _, boundary := range []subnetEvmBlock{{number: artifact.Start.Number, hash: common.Hash(start)}, {number: artifact.End.Number, hash: common.Hash(end)}} {
		calls := make([]subnetEvmCall, len(candidates))
		for i, provider := range candidates {
			calls[i] = subnetEvmCall{to: state.coordinator, data: coordinator.PackBindingAt(provider.ClientID, epoch)}
		}
		if err := checkpoint.evmReader.callBatch(ctx, calls, boundary); err != nil {
			if canceled := subnetCanceled(ctx); canceled != nil {
				return nil, "", canceled
			}
			return nil, "boundary-state-" + subnetReadReason(err), nil
		}
		for i := range candidates {
			if calls[i].err != nil {
				return nil, "boundary-state-" + subnetReadReason(calls[i].err), nil
			}
			result, err := coordinator.UnpackBindingAt(calls[i].output)
			if err != nil {
				return nil, "boundary-state-invalid", nil
			}
			active[i] = active[i] || result.Active
		}
	}
	reasons := []string{}
	for i, provider := range candidates {
		switch {
		case provider.HeadExcluded && !active[i]:
			reasons = append(reasons, "head-excluded-unbound")
		case !provider.HeadExcluded && active[i]:
			reasons = append(reasons, "head-client-in-tail")
		}
	}
	return reasons, "", nil
}
