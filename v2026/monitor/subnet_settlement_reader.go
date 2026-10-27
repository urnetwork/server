// The §22.7 settlement state census. One finalized evm checkpoint pins every
// read: the coordinator and vault links, the vault's exact counters and live
// escrow stake, the coordinator's own operator census, and each operator's
// entitlement, root commitment and operator version for every epoch since
// the first policy. Discovery starts from the reviewed pin and the chain,
// never from the producer's mirrored rows.
package monitor

import (
	"context"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/v2026/stabi"
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
