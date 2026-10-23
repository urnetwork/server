// SIGNALS.md §22.1--§22.5 typed contract state at the agreed checkpoint. The
// coordinator, settlement vault and reserve sink views are read in bounded
// hash-pinned batches. The operator census is discovered from the
// coordinator's own operatorCount/operatorIdAt at that block, never only from
// a producer feed, and is compared with the reviewed census by each reader.
package monitor

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/stabi"
)

// Contract census bounds; a larger on-chain census is unknown, not truncated.
const maximumSubnetPolicyVersions = 64
const maximumSubnetOperators = 64

// One on-chain operator at the current epoch with its vault pool.
type subnetOperatorState struct {
	noId                 uint64
	version              stabi.STCoordinatorOperatorVersion
	versionErr           error
	pool                 stabi.PoolsOutput
	poolErr              error
	operatorPrincipal    *big.Int
	cumulativeConviction *big.Int
}

// Contract state read at one checkpoint.
type subnetDeploymentState struct {
	coordinator common.Address
	vault       common.Address
	reserveSink common.Address

	coordinatorNetuid             uint16
	coordinatorColdkey            [32]byte
	coordinatorVault              common.Address
	coordinatorSink               common.Address
	owner                         common.Address
	guardian                      common.Address
	activeGuardian                common.Address
	pendingGuardian               common.Address
	pendingGuardianEpoch          uint64
	commitmentOracle              common.Address
	activeCommitmentOracle        common.Address
	pendingCommitmentOracle       common.Address
	pendingCommitmentOracleEpoch  uint64
	paused                        bool
	validatorEvidence             common.Address
	currentEpoch                  uint64
	policies                      []stabi.STCoordinatorPolicySnapshot
	currentPolicy                 stabi.STCoordinatorPolicySnapshot
	operatorIds                   []uint64
	operators                     map[uint64]*subnetOperatorState
	vaultCoordinator              common.Address
	vaultNetuid                   uint16
	vaultColdkey                  [32]byte
	escrowHotkey                  [32]byte
	escrowRegistered              bool
	sinkRecorder                  common.Address
	sinkNetuid                    uint16
	sinkColdkey                   [32]byte
	reserveHotkey                 [32]byte
	liveStake                     *big.Int
	principal                     *big.Int
	reserveRoundingAllowanceRao   *big.Int
	runtimeShareRoundingAllowance *big.Int
}

// Typed view calls queued for one batch; each decodes after the batch.
type subnetViewQueue struct {
	calls    []subnetEvmCall
	decoders []func([]byte) error
}

func (self *subnetViewQueue) add(to common.Address, data []byte, decode func([]byte) error) {
	self.calls = append(self.calls, subnetEvmCall{to: to, data: data})
	self.decoders = append(self.decoders, decode)
}

// Runs the batch; the first failed call's fixed reason fails the stage.
func (self *subnetViewQueue) run(ctx context.Context, reader subnetEvmReader, at subnetEvmBlock) error {
	if err := reader.callBatch(ctx, self.calls, at); err != nil {
		return err
	}
	for i, call := range self.calls {
		if call.err != nil {
			return call.err
		}
		if err := self.decoders[i](call.output); err != nil {
			return subnetReadFailure("call-output-invalid", err)
		}
	}
	return nil
}

// Binds an abigen unpacker to its destination.
func subnetDecodeInto[T any](target *T, unpack func([]byte) (T, error)) func([]byte) error {
	return func(output []byte) error {
		value, err := unpack(output)
		if err != nil {
			return err
		}
		*target = value
		return nil
	}
}

// Binds a uint256 unpacker that must fit in uint64.
func subnetDecodeUint64(target *uint64, unpack func([]byte) (*big.Int, error)) func([]byte) error {
	return func(output []byte) error {
		value, err := unpack(output)
		if err != nil {
			return err
		}
		if value == nil || value.Sign() < 0 || !value.IsUint64() {
			return subnetReadFailure("call-output-out-of-range", nil)
		}
		*target = value.Uint64()
		return nil
	}
}

// Reads the deployment's contract state. The reserve sink address is the
// reviewed one; whether the coordinator names the same sink is a governance
// check, not an assumption.
func readSubnetDeploymentState(ctx context.Context, checkpoint *subnetCheckpoint, settings *SubnetCorrectnessSettings) (*subnetDeploymentState, error) {
	coordinator := stabi.NewSTCoordinator()
	vault := stabi.NewSTSettlementVault()
	sink := stabi.NewSTReserveSink()
	state := &subnetDeploymentState{
		coordinator: settings.Deployment.coordinatorAddress(),
		vault:       settings.Deployment.settlementVaultAddress(),
		reserveSink: common.HexToAddress(settings.Governance.ReserveSink),
		operators:   map[uint64]*subnetOperatorState{},
	}
	reader := checkpoint.evmReader
	at := checkpoint.evm
	var policyCount, operatorCount uint64
	first := &subnetViewQueue{}
	first.add(state.coordinator, coordinator.PackNetuid(), subnetDecodeInto(&state.coordinatorNetuid, coordinator.UnpackNetuid))
	first.add(state.coordinator, coordinator.PackSelfColdkey(), subnetDecodeInto(&state.coordinatorColdkey, coordinator.UnpackSelfColdkey))
	first.add(state.coordinator, coordinator.PackSettlementVault(), subnetDecodeInto(&state.coordinatorVault, coordinator.UnpackSettlementVault))
	first.add(state.coordinator, coordinator.PackReserveSink(), subnetDecodeInto(&state.coordinatorSink, coordinator.UnpackReserveSink))
	first.add(state.coordinator, coordinator.PackOwner(), subnetDecodeInto(&state.owner, coordinator.UnpackOwner))
	first.add(state.coordinator, coordinator.PackGuardian(), subnetDecodeInto(&state.guardian, coordinator.UnpackGuardian))
	first.add(state.coordinator, coordinator.PackActiveGuardian(), subnetDecodeInto(&state.activeGuardian, coordinator.UnpackActiveGuardian))
	first.add(state.coordinator, coordinator.PackPendingGuardian(), subnetDecodeInto(&state.pendingGuardian, coordinator.UnpackPendingGuardian))
	first.add(state.coordinator, coordinator.PackPendingGuardianEpoch(), subnetDecodeInto(&state.pendingGuardianEpoch, coordinator.UnpackPendingGuardianEpoch))
	first.add(state.coordinator, coordinator.PackCommitmentOracle(), subnetDecodeInto(&state.commitmentOracle, coordinator.UnpackCommitmentOracle))
	first.add(state.coordinator, coordinator.PackActiveCommitmentOracle(), subnetDecodeInto(&state.activeCommitmentOracle, coordinator.UnpackActiveCommitmentOracle))
	first.add(state.coordinator, coordinator.PackPendingCommitmentOracle(), subnetDecodeInto(&state.pendingCommitmentOracle, coordinator.UnpackPendingCommitmentOracle))
	first.add(state.coordinator, coordinator.PackPendingCommitmentOracleEpoch(), subnetDecodeInto(&state.pendingCommitmentOracleEpoch, coordinator.UnpackPendingCommitmentOracleEpoch))
	first.add(state.coordinator, coordinator.PackPaused(), subnetDecodeInto(&state.paused, coordinator.UnpackPaused))
	first.add(state.coordinator, coordinator.PackValidatorEvidence(), subnetDecodeInto(&state.validatorEvidence, coordinator.UnpackValidatorEvidence))
	first.add(state.coordinator, coordinator.PackCurrentEpoch(), subnetDecodeUint64(&state.currentEpoch, coordinator.UnpackCurrentEpoch))
	first.add(state.coordinator, coordinator.PackPolicyCount(), subnetDecodeUint64(&policyCount, coordinator.UnpackPolicyCount))
	first.add(state.coordinator, coordinator.PackOperatorCount(), subnetDecodeUint64(&operatorCount, coordinator.UnpackOperatorCount))
	first.add(state.coordinator, coordinator.PackRESERVEROUNDINGALLOWANCERAO(), subnetDecodeInto(&state.reserveRoundingAllowanceRao, coordinator.UnpackRESERVEROUNDINGALLOWANCERAO))
	first.add(state.coordinator, coordinator.PackRUNTIMESHAREROUNDINGALLOWANCERAO(), subnetDecodeInto(&state.runtimeShareRoundingAllowance, coordinator.UnpackRUNTIMESHAREROUNDINGALLOWANCERAO))
	first.add(state.vault, vault.PackCoordinator(), subnetDecodeInto(&state.vaultCoordinator, vault.UnpackCoordinator))
	first.add(state.vault, vault.PackNetuid(), subnetDecodeInto(&state.vaultNetuid, vault.UnpackNetuid))
	first.add(state.vault, vault.PackSelfColdkey(), subnetDecodeInto(&state.vaultColdkey, vault.UnpackSelfColdkey))
	first.add(state.vault, vault.PackEscrowHotkey(), subnetDecodeInto(&state.escrowHotkey, vault.UnpackEscrowHotkey))
	first.add(state.vault, vault.PackEscrowRegistered(), subnetDecodeInto(&state.escrowRegistered, vault.UnpackEscrowRegistered))
	first.add(state.reserveSink, sink.PackRecorder(), subnetDecodeInto(&state.sinkRecorder, sink.UnpackRecorder))
	first.add(state.reserveSink, sink.PackNetuid(), subnetDecodeInto(&state.sinkNetuid, sink.UnpackNetuid))
	first.add(state.reserveSink, sink.PackSelfColdkey(), subnetDecodeInto(&state.sinkColdkey, sink.UnpackSelfColdkey))
	first.add(state.reserveSink, sink.PackReserveHotkey(), subnetDecodeInto(&state.reserveHotkey, sink.UnpackReserveHotkey))
	first.add(state.reserveSink, sink.PackLiveStake(), subnetDecodeInto(&state.liveStake, sink.UnpackLiveStake))
	first.add(state.reserveSink, sink.PackPrincipal(), subnetDecodeInto(&state.principal, sink.UnpackPrincipal))
	if err := first.run(ctx, reader, at); err != nil {
		return nil, err
	}
	if policyCount == 0 || policyCount > maximumSubnetPolicyVersions {
		return nil, subnetReadFailure("policy-census-out-of-bounds", nil)
	}
	if operatorCount > maximumSubnetOperators {
		return nil, subnetReadFailure("operator-census-out-of-bounds", nil)
	}
	second := &subnetViewQueue{}
	state.policies = make([]stabi.STCoordinatorPolicySnapshot, policyCount)
	for i := uint64(0); i < policyCount; i++ {
		second.add(state.coordinator, coordinator.PackPolicyByIndex(new(big.Int).SetUint64(i)), subnetDecodeInto(&state.policies[i], coordinator.UnpackPolicyByIndex))
	}
	second.add(state.coordinator, coordinator.PackPolicyAt(new(big.Int).SetUint64(state.currentEpoch)), subnetDecodeInto(&state.currentPolicy, coordinator.UnpackPolicyAt))
	state.operatorIds = make([]uint64, operatorCount)
	for i := uint64(0); i < operatorCount; i++ {
		second.add(state.coordinator, coordinator.PackOperatorIdAt(new(big.Int).SetUint64(i)), subnetDecodeUint64(&state.operatorIds[i], coordinator.UnpackOperatorIdAt))
	}
	if err := second.run(ctx, reader, at); err != nil {
		return nil, err
	}
	// Per-operator views keep their own errors: an operator effective only in
	// a later epoch reverts operatorAt at the current one.
	calls := []subnetEvmCall{}
	epoch := new(big.Int).SetUint64(state.currentEpoch)
	for _, noId := range state.operatorIds {
		if _, duplicate := state.operators[noId]; duplicate || noId == 0 {
			return nil, subnetReadFailure("operator-census-invalid", nil)
		}
		state.operators[noId] = &subnetOperatorState{noId: noId}
		id := new(big.Int).SetUint64(noId)
		calls = append(calls,
			subnetEvmCall{to: state.coordinator, data: coordinator.PackOperatorAt(id, epoch)},
			subnetEvmCall{to: state.vault, data: vault.PackPools(id)},
			subnetEvmCall{to: state.reserveSink, data: sink.PackOperatorPrincipal(id)},
			subnetEvmCall{to: state.coordinator, data: coordinator.PackCumulativeConviction(id)},
		)
	}
	if err := reader.callBatch(ctx, calls, at); err != nil {
		return nil, err
	}
	for i, noId := range state.operatorIds {
		operator := state.operators[noId]
		base := 4 * i
		if calls[base].err == nil {
			operator.version, operator.versionErr = coordinator.UnpackOperatorAt(calls[base].output)
		} else {
			operator.versionErr = calls[base].err
		}
		if calls[base+1].err == nil {
			operator.pool, operator.poolErr = vault.UnpackPools(calls[base+1].output)
		} else {
			operator.poolErr = calls[base+1].err
		}
		for j, target := range []**big.Int{&operator.operatorPrincipal, &operator.cumulativeConviction} {
			call := calls[base+2+j]
			if call.err != nil {
				return nil, call.err
			}
			unpack := sink.UnpackOperatorPrincipal
			if j == 1 {
				unpack = coordinator.UnpackCumulativeConviction
			}
			value, err := unpack(call.output)
			if err != nil || value == nil || value.Sign() < 0 {
				return nil, subnetReadFailure("call-output-invalid", err)
			}
			*target = value
		}
	}
	return state, nil
}

// Opens the checkpoint and contract state shared by the families that only
// consume them. A failed gate is one coverage finding for the family.
func openSubnetState(ctx context.Context, run *subnetRun, cache *subnetRuntimeCache, family subnetFamily) (*subnetCheckpoint, *subnetDeploymentState, []finding, error) {
	result, err := readSubnetCheckpoint(ctx, run, cache)
	if err != nil {
		return nil, nil, nil, err
	}
	if result.checkpoint == nil {
		return nil, nil, []finding{subnetCheckpointGap(family, result)}, nil
	}
	state, err := readSubnetDeploymentState(ctx, result.checkpoint, run.settings)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return nil, nil, nil, canceled
	}
	if err != nil {
		return nil, nil, []finding{subnetCoverageGap(family, "deployment-state", subnetReadReason(err), subnetTransientSustain)}, nil
	}
	return result.checkpoint, state, nil, nil
}

// The settlement epoch containing a block under the coordinator's own
// boundary rule.
func subnetEpochAtBlock(policies []stabi.STCoordinatorPolicySnapshot, block uint64) (uint64, bool) {
	active := -1
	for i, policy := range policies {
		if policy.EffectiveBlock <= block {
			active = i
		}
	}
	if active < 0 || policies[active].EpochBlocks == 0 {
		return 0, false
	}
	policy := policies[active]
	return policy.EffectiveEpoch + (block-policy.EffectiveBlock)/policy.EpochBlocks, true
}
