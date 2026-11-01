// The §22.7 settlement state census. One finalized evm checkpoint pins every
// read: the coordinator and vault links, the vault's exact counters and live
// escrow stake, the coordinator's own operator census, and each operator's
// entitlement, root commitment and operator version for every epoch since
// the first policy. When the pin declares settlement v2, the same checkpoint
// also pins each operator's vault2 pool and vault2's coldkey, which decide
// the epochs v1 no longer settles. Discovery starts from the reviewed pin and
// the chain, never from the producer's mirrored rows.
package monitor

import (
	"context"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urfoundation/sn/stabi"
)

// Vault v1 entitlement states (STSettlementVault.EpochStatus); protocol facts.
const (
	subnetSettlementUnset      uint8 = 0
	subnetSettlementFunded     uint8 = 1
	subnetSettlementFinalized  uint8 = 2
	subnetSettlementRootMissed uint8 = 3
	subnetSettlementCarried    uint8 = 4
)

// The vault's fixed share denominator (STSettlementVault.BPS); a protocol fact.
const subnetSettlementBps = 10000

// Parsed abi definitions of the pinned release contracts.
type subnetSettlementContracts struct {
	coordinator *abi.ABI
	vault       *abi.ABI
	evidence    *abi.ABI
}

var subnetSettlementAbis = sync.OnceValues(func() (subnetSettlementContracts, error) {
	coordinator, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return subnetSettlementContracts{}, err
	}
	vault, err := stabi.STSettlementVaultMetaData.ParseABI()
	if err != nil {
		return subnetSettlementContracts{}, err
	}
	evidence, err := stabi.STValidatorEvidenceMetaData.ParseABI()
	if err != nil {
		return subnetSettlementContracts{}, err
	}
	return subnetSettlementContracts{coordinator: coordinator, vault: vault, evidence: evidence}, nil
})

// Exact vault counters and constants at the checkpoint.
type subnetSettlementVault struct {
	totalCaptured         *big.Int
	totalPaid             *big.Int
	pendingFunding        *big.Int
	outstandingLiability  *big.Int
	escrowAccounted       *big.Int
	liveEscrowStake       *big.Int
	conservationHolds     bool
	minimumClaimTtlBlocks uint64
	minimumTransferTaoRao uint64
}

// One coordinator operator and its current vault carry.
type subnetSettlementOperator struct {
	noId  uint64
	carry *big.Int
}

// One epoch's effective policy and clock. Epochs before the first policy have
// no policy and therefore cannot hold settlement state.
type subnetSettlementEpoch struct {
	epoch       uint64
	policyKnown bool
	policy      stabi.STCoordinatorPolicySnapshot
	endBlock    uint64
	expiryFloor uint64
}

// One epoch-by-operator row of settlement state.
type subnetSettlementEntry struct {
	epoch         uint64
	noId          uint64
	clock         *subnetSettlementEpoch
	operatorKnown bool
	operator      stabi.STCoordinatorOperatorVersion
	entitlement   stabi.STSettlementVaultEntitlement
	root          stabi.RootCommitmentsOutput
	// Set by readSubnetSettlementCutover: the settlement version that owns
	// the row, whether it is the operator's planned v1 sweep epoch at or after
	// the cutover, and whether its root is the sanctioned v1 carry sweep.
	cutover         subnetSettlementCutover
	plannedSweep    bool
	sanctionedSweep bool
}

// Which settlement version owns one v1 row. From an operator's registered
// vault2 pool firstEpoch, the v1 driver closes and commits nothing for that
// operator (VERSION2-FINAL.md SV-4), except one owner-approved carry sweep.
type subnetSettlementCutover uint8

const (
	// v1 settles the row: no declared v2, no registered pool, or an epoch
	// before the pool's first v2 epoch.
	subnetSettlementBeforeCutover subnetSettlementCutover = iota
	// v2 settles the row.
	subnetSettlementAfterCutover
	// The vault2 read failed, so the owner of the row is unknown.
	subnetSettlementCutoverUnknown
)

// The v2 facts the v1 rules depend on, read at the census checkpoint when the
// pin declares settlement v2: each operator's vault2 pool and the one leaf the
// sanctioned carry sweep commits. A failed read leaves the cutover unknown
// with a public reason; it is never read as an absent v2.
type subnetSettlementV2Cutover struct {
	declared  bool
	known     bool
	reason    string
	sweepLeaf [32]byte
	pools     map[uint64]stabi.ISTSettlementVault2Pool
	// Post-cutover v1 roots of each operator in the census.
	roots map[uint64]int
	// The enrolled planned sweep epochs that agree with the chain, and the
	// public reason when an enrolled one contradicts it.
	planned    map[uint64]uint64
	planReason string
}

// The settlement version that owns one epoch of one operator.
func (self *subnetSettlementV2Cutover) of(epoch uint64, noId uint64) subnetSettlementCutover {
	switch {
	case !self.declared:
		return subnetSettlementBeforeCutover
	case !self.known:
		return subnetSettlementCutoverUnknown
	}
	pool := self.pools[noId]
	if !pool.Registered || epoch < pool.FirstEpoch {
		return subnetSettlementBeforeCutover
	}
	return subnetSettlementAfterCutover
}

// The complete or explicitly partial state census at one checkpoint.
type subnetSettlementState struct {
	checkpoint    subnetReplayEvmCheckpoint
	currentEpoch  uint64
	paused        bool
	escrowHotkey  [32]byte
	vault         subnetSettlementVault
	operatorCount uint64
	operators     []*subnetSettlementOperator
	epochs        []*subnetSettlementEpoch
	entries       []*subnetSettlementEntry
	// Every epoch and every operator was read, so whole-history sums are exact.
	censusComplete bool
	censusReason   string
	cutover        subnetSettlementV2Cutover
}

// Reads the census. A link that differs from the pin is not this family's
// incident (§22.1 owns governance drift), but it makes every verdict unknown.
func readSubnetSettlementState(ctx context.Context, rpc *subnetReplayRpc, pin *SubnetReplayPin, settings *SubnetReplaySettings) (*subnetSettlementState, error) {
	contracts, err := subnetSettlementAbis()
	if err != nil {
		return nil, newSubnetReplayUnknown("binding-unavailable", err)
	}
	checkpoint, err := readSubnetReplayEvmCheckpoint(ctx, rpc, pin)
	if err != nil {
		return nil, err
	}
	state := &subnetSettlementState{checkpoint: checkpoint, censusComplete: true}

	coordinatorVault := newSubnetReplayView(contracts.coordinator, pin.coordinator, "settlementVault")
	coordinatorNetuid := newSubnetReplayView(contracts.coordinator, pin.coordinator, "netuid")
	currentEpoch := newSubnetReplayView(contracts.coordinator, pin.coordinator, "currentEpoch")
	operatorCount := newSubnetReplayView(contracts.coordinator, pin.coordinator, "operatorCount")
	paused := newSubnetReplayView(contracts.coordinator, pin.coordinator, "paused")
	vaultCoordinator := newSubnetReplayView(contracts.vault, pin.settlementVault, "coordinator")
	vaultNetuid := newSubnetReplayView(contracts.vault, pin.settlementVault, "netuid")
	escrowHotkey := newSubnetReplayView(contracts.vault, pin.settlementVault, "escrowHotkey")
	counterNames := []string{"totalCaptured", "totalPaid", "pendingFunding", "outstandingLiability", "escrowAccounted", "liveEscrowStake"}
	counters := make([]*subnetReplayView, len(counterNames))
	for i, name := range counterNames {
		counters[i] = newSubnetReplayView(contracts.vault, pin.settlementVault, name)
	}
	holds := newSubnetReplayView(contracts.vault, pin.settlementVault, "conservationHolds")
	claimTtl := newSubnetReplayView(contracts.vault, pin.settlementVault, "minimumClaimTTLBlocks")
	transferFloor := newSubnetReplayView(contracts.vault, pin.settlementVault, "minimumTransferTaoRao")
	views := append([]*subnetReplayView{coordinatorVault, coordinatorNetuid, currentEpoch, operatorCount, paused, vaultCoordinator, vaultNetuid, escrowHotkey, holds, claimTtl, transferFloor}, counters...)
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, views...); err != nil {
		return nil, err
	}
	linkedVault, err := subnetReplayValue[common.Address](coordinatorVault, 0)
	if err != nil {
		return nil, err
	}
	linkedCoordinator, err := subnetReplayValue[common.Address](vaultCoordinator, 0)
	if err != nil {
		return nil, err
	}
	coordinatorSubnet, err := subnetReplayValue[uint16](coordinatorNetuid, 0)
	if err != nil {
		return nil, err
	}
	vaultSubnet, err := subnetReplayValue[uint16](vaultNetuid, 0)
	if err != nil {
		return nil, err
	}
	if linkedVault != pin.settlementVault || linkedCoordinator != pin.coordinator || coordinatorSubnet != pin.netuid || vaultSubnet != pin.netuid {
		return nil, newSubnetReplayUnknown("deployment-link-mismatch", nil)
	}
	epoch, err := subnetReplayValue[*big.Int](currentEpoch, 0)
	if err != nil {
		return nil, err
	}
	count, err := subnetReplayValue[*big.Int](operatorCount, 0)
	if err != nil {
		return nil, err
	}
	if !epoch.IsUint64() || !count.IsUint64() {
		return nil, newSubnetReplayUnknown("view-shape-invalid", nil)
	}
	state.currentEpoch = epoch.Uint64()
	state.operatorCount = count.Uint64()
	if state.paused, err = subnetReplayValue[bool](paused, 0); err != nil {
		return nil, err
	}
	if state.escrowHotkey, err = subnetReplayValue[[32]byte](escrowHotkey, 0); err != nil {
		return nil, err
	}
	values := make([]*big.Int, len(counters))
	for i, view := range counters {
		if values[i], err = subnetReplayValue[*big.Int](view, 0); err != nil {
			return nil, err
		}
	}
	state.vault = subnetSettlementVault{
		totalCaptured: values[0], totalPaid: values[1], pendingFunding: values[2],
		outstandingLiability: values[3], escrowAccounted: values[4], liveEscrowStake: values[5],
	}
	if state.vault.conservationHolds, err = subnetReplayValue[bool](holds, 0); err != nil {
		return nil, err
	}
	if state.vault.minimumClaimTtlBlocks, err = subnetReplayValue[uint64](claimTtl, 0); err != nil {
		return nil, err
	}
	if state.vault.minimumTransferTaoRao, err = subnetReplayValue[uint64](transferFloor, 0); err != nil {
		return nil, err
	}

	// The coordinator is the independent operator census; a larger census is
	// read up to its bound and every whole-history identity becomes unknown.
	readable := state.operatorCount
	if uint64(settings.MaximumOperators) < readable {
		readable = uint64(settings.MaximumOperators)
		state.censusComplete = false
		state.censusReason = "operator-census-bound"
	}
	epochCount := state.currentEpoch + 1
	firstEpoch := uint64(0)
	if 0 < readable && uint64(settings.MaximumCensusEntries) < epochCount*readable {
		window := uint64(settings.MaximumCensusEntries) / readable
		if window == 0 {
			window = 1
		}
		firstEpoch = epochCount - window
		state.censusComplete = false
		state.censusReason = "epoch-census-bound"
	}
	idViews := make([]*subnetReplayView, readable)
	for i := range idViews {
		idViews[i] = newSubnetReplayView(contracts.coordinator, pin.coordinator, "operatorIdAt", new(big.Int).SetUint64(uint64(i)))
	}
	policyViews := []*subnetReplayView{}
	endViews := []*subnetReplayView{}
	for e := firstEpoch; e <= state.currentEpoch; e++ {
		clock := &subnetSettlementEpoch{epoch: e}
		state.epochs = append(state.epochs, clock)
		policyViews = append(policyViews, newSubnetReplayView(contracts.coordinator, pin.coordinator, "policyAt", new(big.Int).SetUint64(e)).revertible())
		endViews = append(endViews, newSubnetReplayView(contracts.coordinator, pin.coordinator, "epochEndBlock", new(big.Int).SetUint64(e)).revertible())
	}
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, append(append(idViews, policyViews...), endViews...)...); err != nil {
		return nil, err
	}
	seenOperators := map[uint64]bool{}
	for _, view := range idViews {
		id, err := subnetReplayValue[*big.Int](view, 0)
		if err != nil {
			return nil, err
		}
		if !id.IsUint64() || id.Sign() == 0 || seenOperators[id.Uint64()] {
			return nil, newSubnetReplayUnknown("operator-census-invalid", nil)
		}
		seenOperators[id.Uint64()] = true
		state.operators = append(state.operators, &subnetSettlementOperator{noId: id.Uint64()})
	}
	for i, clock := range state.epochs {
		if policyViews[i].revert != nil || endViews[i].revert != nil {
			if !policyViews[i].revertedWith(contracts.coordinator, "InvalidPolicy") || !endViews[i].revertedWith(contracts.coordinator, "InvalidPolicy") {
				return nil, newSubnetReplayUnknown("policy-read-reverted", nil)
			}
			continue
		}
		clock.policyKnown = true
		if clock.policy, err = subnetReplayValue[stabi.STCoordinatorPolicySnapshot](policyViews[i], 0); err != nil {
			return nil, err
		}
		end, err := subnetReplayValue[*big.Int](endViews[i], 0)
		if err != nil {
			return nil, err
		}
		if !end.IsUint64() || clock.policy.EpochBlocks == 0 {
			return nil, newSubnetReplayUnknown("policy-shape-invalid", nil)
		}
		clock.endBlock = end.Uint64()
	}

	// Rows: entitlement, root commitment and operator version per epoch and
	// operator, plus each operator's carry.
	type rowViews struct {
		entry       *subnetSettlementEntry
		entitlement *subnetReplayView
		root        *subnetReplayView
		operator    *subnetReplayView
	}
	rows := []rowViews{}
	rowRequests := []*subnetReplayView{}
	for _, clock := range state.epochs {
		if !clock.policyKnown {
			continue
		}
		for _, operator := range state.operators {
			epochArgument := new(big.Int).SetUint64(clock.epoch)
			idArgument := new(big.Int).SetUint64(operator.noId)
			row := rowViews{
				entry:       &subnetSettlementEntry{epoch: clock.epoch, noId: operator.noId, clock: clock},
				entitlement: newSubnetReplayView(contracts.vault, pin.settlementVault, "entitlement", epochArgument, idArgument),
				root:        newSubnetReplayView(contracts.coordinator, pin.coordinator, "rootCommitments", epochArgument, idArgument),
				operator:    newSubnetReplayView(contracts.coordinator, pin.coordinator, "operatorAt", idArgument, epochArgument).revertible(),
			}
			rows = append(rows, row)
			rowRequests = append(rowRequests, row.entitlement, row.root, row.operator)
		}
	}
	carryViews := make([]*subnetReplayView, len(state.operators))
	for i, operator := range state.operators {
		carryViews[i] = newSubnetReplayView(contracts.vault, pin.settlementVault, "carry", new(big.Int).SetUint64(operator.noId))
	}
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, append(rowRequests, carryViews...)...); err != nil {
		return nil, err
	}
	for i, operator := range state.operators {
		if operator.carry, err = subnetReplayValue[*big.Int](carryViews[i], 0); err != nil {
			return nil, err
		}
	}
	expiryEpochs := map[uint64]*subnetSettlementEpoch{}
	for _, row := range rows {
		if row.entry.entitlement, err = subnetReplayValue[stabi.STSettlementVaultEntitlement](row.entitlement, 0); err != nil {
			return nil, err
		}
		if row.entry.root, err = subnetSettlementRootOf(row.root); err != nil {
			return nil, err
		}
		if row.operator.revert != nil {
			if !row.operator.revertedWith(contracts.coordinator, "UnknownOperator") {
				return nil, newSubnetReplayUnknown("operator-read-reverted", nil)
			}
		} else {
			row.entry.operatorKnown = true
			if row.entry.operator, err = subnetReplayValue[stabi.STCoordinatorOperatorVersion](row.operator, 0); err != nil {
				return nil, err
			}
		}
		if row.entry.entitlement.Status > subnetSettlementCarried || row.entry.entitlement.Funded == nil || row.entry.entitlement.Total == nil || row.entry.entitlement.Claimed == nil {
			return nil, newSubnetReplayUnknown("entitlement-shape-invalid", nil)
		}
		if row.entry.entitlement.Status == subnetSettlementFinalized || row.entry.entitlement.Status == subnetSettlementCarried {
			expiryEpochs[row.entry.epoch] = row.entry.clock
		}
		state.entries = append(state.entries, row.entry)
	}

	// The claim expiry floor is the start of the first epoch after claim ttl
	// and grace, minus one block, exactly as the coordinator computes it.
	expiryViews := []*subnetReplayView{}
	expiryClocks := []*subnetSettlementEpoch{}
	for _, clock := range state.epochs {
		if expiryEpochs[clock.epoch] == nil {
			continue
		}
		next := clock.epoch + clock.policy.ClaimTTLEpochs + clock.policy.ClaimGraceEpochs + 1
		expiryViews = append(expiryViews, newSubnetReplayView(contracts.coordinator, pin.coordinator, "epochStartBlock", new(big.Int).SetUint64(next)))
		expiryClocks = append(expiryClocks, clock)
	}
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, expiryViews...); err != nil {
		return nil, err
	}
	for i, clock := range expiryClocks {
		start, err := subnetReplayValue[*big.Int](expiryViews[i], 0)
		if err != nil {
			return nil, err
		}
		if !start.IsUint64() || start.Uint64() == 0 {
			return nil, newSubnetReplayUnknown("policy-shape-invalid", nil)
		}
		clock.expiryFloor = start.Uint64() - 1
	}
	if firstEpoch != 0 && state.censusComplete {
		state.censusComplete = false
		state.censusReason = "epoch-census-bound"
	}
	return state, nil
}

// rootCommitments returns four outputs rather than one tuple.
func subnetSettlementRootOf(view *subnetReplayView) (stabi.RootCommitmentsOutput, error) {
	payoutRoot, err := subnetReplayValue[[32]byte](view, 0)
	if err != nil {
		return stabi.RootCommitmentsOutput{}, err
	}
	artifactHash, err := subnetReplayValue[[32]byte](view, 1)
	if err != nil {
		return stabi.RootCommitmentsOutput{}, err
	}
	committer, err := subnetReplayValue[common.Address](view, 2)
	if err != nil {
		return stabi.RootCommitmentsOutput{}, err
	}
	commitBlock, err := subnetReplayValue[uint64](view, 3)
	if err != nil {
		return stabi.RootCommitmentsOutput{}, err
	}
	return stabi.RootCommitmentsOutput{PayoutRoot: payoutRoot, ArtifactHash: artifactHash, Committer: committer, CommitBlock: commitBlock}, nil
}

// Reads the settlement v2 cutover at the census checkpoint and classifies
// every row against it and the enrolled planned sweep epochs. A gap with a
// public reason leaves the cutover unknown, so no row that depends on it can
// read as settled; any other error is returned.
func readSubnetSettlementCutover(ctx context.Context, rpc *subnetReplayRpc, pin *SubnetReplayPin, state *subnetSettlementState, planned map[uint64]uint64) error {
	state.cutover = subnetSettlementV2Cutover{declared: pin.settlementV2Declared, roots: map[uint64]int{}, planned: map[uint64]uint64{}}
	if pin.settlementV2Declared {
		sweepLeaf, pools, err := readSubnetSettlementV2Pools(ctx, rpc, pin, state.checkpoint, state.operators)
		if reason := subnetReplayUnknownReason(err); reason != "" {
			state.cutover.reason = reason
		} else if err != nil {
			return err
		} else {
			state.cutover.known = true
			state.cutover.sweepLeaf = sweepLeaf
			state.cutover.pools = pools
		}
	}
	// A plan that names an operator outside a complete census, or an epoch
	// before its registered pool's first v2 epoch, cannot be the sweep. It is
	// dropped and reported, never applied to another epoch.
	census := map[uint64]bool{}
	for _, operator := range state.operators {
		census[operator.noId] = true
	}
	completeOperators := uint64(len(state.operators)) == state.operatorCount
	for noId, epoch := range planned {
		pool := state.cutover.pools[noId]
		if state.cutover.known && ((completeOperators && !census[noId]) || (pool.Registered && epoch < pool.FirstEpoch)) {
			state.cutover.planReason = "planned-sweep-inconsistent"
			continue
		}
		state.cutover.planned[noId] = epoch
	}
	for _, entry := range state.entries {
		entry.cutover = state.cutover.of(entry.epoch, entry.noId)
		plannedEpoch, hasPlan := state.cutover.planned[entry.noId]
		entry.plannedSweep = hasPlan && plannedEpoch == entry.epoch && entry.cutover != subnetSettlementBeforeCutover
		if entry.cutover == subnetSettlementAfterCutover && entry.root.PayoutRoot != [32]byte{} {
			state.cutover.roots[entry.noId] += 1
		}
	}
	// A planned operator's sweep is sanctioned only at its planned epoch;
	// without a plan, only as the operator's single post-cutover root.
	for _, entry := range state.entries {
		if entry.cutover != subnetSettlementAfterCutover || entry.root.PayoutRoot != state.cutover.sweepLeaf {
			continue
		}
		if plannedEpoch, hasPlan := state.cutover.planned[entry.noId]; hasPlan {
			entry.sanctionedSweep = entry.epoch == plannedEpoch
		} else {
			entry.sanctionedSweep = state.cutover.roots[entry.noId] == 1
		}
	}
	return nil
}

// The pinned v2 pair's links, vault2's coldkey and each operator's pool in
// one batch at the checkpoint. The links are checked as §22.7.1 checks them:
// both v2 contracts must name each other, the v1 coordinator and the pinned
// subnet before any pool is trusted.
func readSubnetSettlementV2Pools(ctx context.Context, rpc *subnetReplayRpc, pin *SubnetReplayPin, checkpoint subnetReplayEvmCheckpoint, operators []*subnetSettlementOperator) ([32]byte, map[uint64]stabi.ISTSettlementVault2Pool, error) {
	v2, err := subnetRootDeadlineV2Abis()
	if err != nil {
		return [32]byte{}, nil, newSubnetReplayUnknown("binding-unavailable", err)
	}
	type addressLink struct {
		view *subnetReplayView
		want common.Address
	}
	links := []addressLink{
		{newSubnetReplayView(v2.settlement, pin.settlementV2, "v1Coordinator"), pin.coordinator},
		{newSubnetReplayView(v2.settlement, pin.settlementV2, "vault"), pin.settlementVaultV2},
		{newSubnetReplayView(v2.vault, pin.settlementVaultV2, "coordinator"), pin.settlementV2},
	}
	netuid := newSubnetReplayView(v2.vault, pin.settlementVaultV2, "netuid")
	selfColdkey := newSubnetReplayView(v2.vault, pin.settlementVaultV2, "selfColdkey")
	views := []*subnetReplayView{netuid, selfColdkey}
	for _, link := range links {
		views = append(views, link.view)
	}
	poolViews := make([]*subnetReplayView, len(operators))
	for i, operator := range operators {
		poolViews[i] = newSubnetReplayView(v2.vault, pin.settlementVaultV2, "pools", new(big.Int).SetUint64(operator.noId))
		views = append(views, poolViews[i])
	}
	if err := readSubnetReplayViews(ctx, rpc, checkpoint, views...); err != nil {
		return [32]byte{}, nil, err
	}
	for _, link := range links {
		got, err := subnetReplayValue[common.Address](link.view, 0)
		if err != nil {
			return [32]byte{}, nil, err
		}
		if got != link.want {
			return [32]byte{}, nil, newSubnetReplayUnknown("deployment-link-mismatch", nil)
		}
	}
	subnet, err := subnetReplayValue[uint16](netuid, 0)
	if err != nil {
		return [32]byte{}, nil, err
	}
	if subnet != pin.netuid {
		return [32]byte{}, nil, newSubnetReplayUnknown("deployment-link-mismatch", nil)
	}
	coldkey, err := subnetReplayValue[[32]byte](selfColdkey, 0)
	if err != nil {
		return [32]byte{}, nil, err
	}
	if coldkey == [32]byte{} {
		return [32]byte{}, nil, newSubnetReplayUnknown("self-coldkey-invalid", nil)
	}
	pools := map[uint64]stabi.ISTSettlementVault2Pool{}
	for i, operator := range operators {
		pool, err := subnetReplayValue[stabi.ISTSettlementVault2Pool](poolViews[i], 0)
		if err != nil {
			return [32]byte{}, nil, err
		}
		// vault2 registers a pool only with a nonzero hotkey and first epoch.
		if pool.Registered != (pool.FirstEpoch != 0) || pool.Registered != (pool.Hotkey != [32]byte{}) {
			return [32]byte{}, nil, newSubnetReplayUnknown("pool-shape-invalid", nil)
		}
		pools[operator.noId] = pool
	}
	return subnetSettlementPayoutLeaf(coldkey, subnetSettlementBps), pools, nil
}

// vault1's payoutLeaf(coldkey, shareBps), computed independently of the
// producer: keccak256(bytes.concat(keccak256(abi.encode(bytes32 coldkey,
// uint256 shareBps)))). The sweep's single-leaf tree has this leaf as root.
func subnetSettlementPayoutLeaf(coldkey [32]byte, shareBps uint64) [32]byte {
	encoded := make([]byte, 64)
	copy(encoded[:32], coldkey[:])
	new(big.Int).SetUint64(shareBps).FillBytes(encoded[32:])
	return [32]byte(crypto.Keccak256(crypto.Keccak256(encoded)))
}
