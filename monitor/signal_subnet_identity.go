// SIGNALS.md §22.2 pool registration, validator identity and fleet ownership.
// At one agreed approved checkpoint the reader discovers the operator census
// from the coordinator itself and compares it with the reviewed census, then
// checks each active pool's custody in both directions of the native Uid
// mapping and under the immutable vault coldkey, the escrow registration,
// every reviewed validator's identity, permit and stake, every native fleet
// commitment against its coordinator mirror, and every accepted fleet binding
// over a bounded frontier against its original client and hotkey signatures.
package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
)

const subnetIdentityRunBudget = 180 * time.Second

// The reserved incident classes of §22.2.
const (
	subnetPoolCustodyIdentityClass  = "subnet-pool-custody-identity"
	subnetValidatorEligibilityClass = "subnet-validator-eligibility"
	subnetFleetBindingInvalidClass  = "subnet-fleet-binding-invalid"
	subnetCommitmentMirrorLagClass  = "subnet-commitment-mirror-lag"
)

var subnetIdentityFamily = subnetFamily{
	number: "22.2", key: "subnet-identity", probeId: "subnet/identity",
	title:   "pool registration, validator identity and fleet ownership",
	classes: []string{subnetPoolCustodyIdentityClass, subnetValidatorEligibilityClass, subnetFleetBindingInvalidClass, subnetCommitmentMirrorLagClass},
	sources: "the agreed finalized checkpoint, the coordinator operator census and vault pools, native Keys, Uids, Owner, ValidatorPermit, TotalHotkeyAlpha and StakeThreshold, native fleet commitments, coordinator commitment mirrors, FleetBound transactions over a bounded frontier, and the reviewed operator and validator census",
}

func NewSubnetIdentitySignal() Signal {
	return &signalAdapter{
		number: "22.2",
		key:    "subnet-identity",
		name:   "Subnet pool registration, validator identity and fleet ownership",
		probe:  newSubnetIdentityProbe(),
	}
}

type subnetIdentityProbe struct {
	runtimes    *subnetRuntimeCache
	bindingLogs *subnetLogFrontier
	latch       *subnetReplayLatch
}

func newSubnetIdentityProbe() *subnetIdentityProbe {
	return &subnetIdentityProbe{runtimes: newSubnetRuntimeCache(), bindingLogs: newSubnetLogFrontier(), latch: newSubnetReplayLatch()}
}

func (*subnetIdentityProbe) id() string               { return "subnet/identity" }
func (*subnetIdentityProbe) tier() string             { return tierWarn }
func (*subnetIdentityProbe) cadence() time.Duration   { return 5 * time.Minute }
func (*subnetIdentityProbe) runBudget() time.Duration { return subnetIdentityRunBudget }

func (self *subnetIdentityProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	return runSubnetFamily(ctx, env, subnetIdentityFamily, self.latch, self.observe)
}

func (self *subnetIdentityProbe) observe(ctx context.Context, run *subnetRun) ([]finding, error) {
	checkpoint, state, gap, err := openSubnetState(ctx, run, self.runtimes, subnetIdentityFamily)
	if err != nil || checkpoint == nil {
		return gap, err
	}
	findings, err := observeSubnetPoolCustody(ctx, run, checkpoint, state)
	if err != nil {
		return nil, err
	}
	validators, err := observeSubnetValidatorEligibility(ctx, run, checkpoint)
	if err != nil {
		return nil, err
	}
	findings = append(findings, validators...)
	mirrors, err := observeSubnetCommitmentMirrors(ctx, run, checkpoint, state)
	if err != nil {
		return nil, err
	}
	findings = append(findings, mirrors...)
	bindings, err := self.observeFleetBindings(ctx, run, checkpoint, state)
	if err != nil {
		return nil, err
	}
	return append(findings, bindings...), nil
}

func subnetOperatorTarget(noId uint64) string {
	return fmt.Sprintf("subnet-identity/no-%d", noId)
}

// Every active pool is the coordinator's registered pool hotkey, holds its
// vault Uid in both directions of the native mapping, is owned by the
// immutable vault coldkey, and keeps its roles distinct.
func observeSubnetPoolCustody(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState) ([]finding, error) {
	netuid := subnetNetuidArg(run.settings.Deployment.Netuid)
	findings := []finding{}
	reviewed := map[uint64]bool{}
	for _, operator := range run.settings.Operators {
		reviewed[operator.NoId] = true
	}
	censusReasons := []string{}
	for noId := range reviewed {
		if _, ok := state.operators[noId]; !ok {
			censusReasons = append(censusReasons, fmt.Sprintf("reviewed-operator-unregistered:%d", noId))
		}
	}
	for _, noId := range state.operatorIds {
		if !reviewed[noId] {
			censusReasons = append(censusReasons, fmt.Sprintf("unreviewed-operator:%d", noId))
		}
	}
	if len(censusReasons) != 0 {
		sort.Strings(censusReasons)
		findings = append(findings, subnetDecided(subnetIdentityFamily, subnetPoolCustodyIdentityClass, "subnet-identity/census", "census", run.scope("complete", "reviewed-operator-census"), subnetDecision{
			state: "census-mismatch",
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetConfigurationSustain,
				invariant: "reviewed-operator-census", expected: "exactly-the-reviewed-operators", observed: strings.Join(censusReasons, ","),
				symptom:   "The on-chain operator census differs from the reviewed census",
				mechanism: "The coordinator's own operatorCount/operatorIdAt census at the checkpoint and the reviewed operator census disagree. An unreviewed operator has no reviewed artifact signer, so its deposit audit cannot be replayed; a reviewed operator missing on chain is not registered.",
				baseline:  "The coordinator registers exactly the reviewed operators.",
				action:    "Reconcile the reviewed census with the registration history through governance; do not infer an operator's signer from its own artifacts.",
				verify:    "The on-chain census equals the reviewed census on a fresh run.",
			},
			evidence: fmt.Sprintf("Checkpoint height %d; %d on-chain operators; %d reviewed operators.", checkpoint.number, len(state.operatorIds), len(reviewed)),
		}))
	}
	type poolCheck struct {
		operator *subnetOperatorState
		key      types.AccountID
		uid      types.U16
		owner    types.AccountID
		reads    []*subnetStorageRead
	}
	checks := []*poolCheck{}
	reads := []*subnetStorageRead{}
	for _, noId := range state.operatorIds {
		operator := state.operators[noId]
		if operator.versionErr != nil || operator.poolErr != nil {
			findings = append(findings, subnetUnknown(subnetIdentityFamily, subnetPoolCustodyIdentityClass, subnetOperatorTarget(noId), "operator-views-unavailable", subnetTransientSustain, "operatorAt and pools for this operator at the checkpoint"))
			continue
		}
		if !operator.version.Active {
			continue
		}
		check := &poolCheck{operator: operator}
		check.reads = []*subnetStorageRead{
			{pallet: "SubtensorModule", item: "Keys", args: [][]byte{netuid, subnetNetuidArg(operator.pool.Uid)}, value: &check.key},
			{pallet: "SubtensorModule", item: "Uids", args: [][]byte{netuid, operator.pool.Hotkey[:]}, value: &check.uid},
			{pallet: "SubtensorModule", item: "Owner", args: [][]byte{operator.pool.Hotkey[:]}, value: &check.owner},
		}
		reads = append(reads, check.reads...)
		checks = append(checks, check)
	}
	var escrowUid types.U16
	var escrowOwner, escrowKey types.AccountID
	escrowReads := []*subnetStorageRead{
		{pallet: "SubtensorModule", item: "Uids", args: [][]byte{netuid, state.escrowHotkey[:]}, value: &escrowUid},
		{pallet: "SubtensorModule", item: "Owner", args: [][]byte{state.escrowHotkey[:]}, value: &escrowOwner},
	}
	reads = append(reads, escrowReads...)
	if err := subnetReadStorageBatch(ctx, checkpoint.chain, checkpoint.nativeHash, reads); err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		return append(findings, subnetUnknown(subnetIdentityFamily, subnetPoolCustodyIdentityClass, "subnet-identity/pools", "native-"+subnetReadReason(err), subnetTransientSustain, "native Keys, Uids and Owner for every pool at the checkpoint")), nil
	}
	poolHotkeys := map[[32]byte]uint64{}
	depositHotkeys := map[[32]byte]uint64{}
	for _, check := range checks {
		operator := check.operator
		target := subnetOperatorTarget(operator.noId)
		failed := false
		for _, read := range check.reads {
			if read.err != nil {
				findings = append(findings, subnetUnknown(subnetIdentityFamily, subnetPoolCustodyIdentityClass, target, "native-"+subnetReadReason(read.err), subnetTransientSustain, "native Keys, Uids and Owner for this pool"))
				failed = true
				break
			}
		}
		if failed {
			continue
		}
		reasons := []string{}
		version := operator.version
		pool := operator.pool
		if pool.Hotkey != version.PoolHotkey {
			reasons = append(reasons, "vault-pool-hotkey-differs")
		}
		if [32]byte(check.key) != pool.Hotkey {
			reasons = append(reasons, "uid-reassigned")
		}
		if !check.reads[1].present || uint16(check.uid) != pool.Uid {
			reasons = append(reasons, "pool-uid-mismatch")
		}
		if [32]byte(check.owner) != state.vaultColdkey {
			reasons = append(reasons, "pool-foreign-owner")
		}
		roles := [][32]byte{pool.Hotkey, version.DepositHotkey, state.escrowHotkey, state.reserveHotkey}
		if subnetDuplicateKeys(roles) || version.Coldkey == state.vaultColdkey || version.Coldkey == state.sinkColdkey || version.Coldkey == state.coordinatorColdkey || version.DepositSigner == version.RootSigner {
			reasons = append(reasons, "role-overlap")
		}
		if other, ok := poolHotkeys[pool.Hotkey]; ok && other != operator.noId {
			reasons = append(reasons, "duplicate-pool-hotkey")
		}
		if other, ok := depositHotkeys[version.DepositHotkey]; ok && other != operator.noId {
			reasons = append(reasons, "duplicate-deposit-hotkey")
		}
		poolHotkeys[pool.Hotkey] = operator.noId
		depositHotkeys[version.DepositHotkey] = operator.noId
		if !pool.Active {
			// The vault's own flag stops capture for this pool; ownership is
			// unchanged, so it is not a custody identity breach.
			findings = append(findings, subnetDecided(subnetIdentityFamily, subnetPoolCustodyIdentityClass, target, "pool-inactive", run.scope("complete", "coordinator-and-settlement-vault"), subnetDecision{
				state: "pool-inactive",
				issue: subnetReplayIssue{
					tier: tierWarn, sustain: subnetConfigurationSustain,
					invariant: "active-operator-pool-active", expected: "active", observed: "inactive",
					symptom:   "An active operator's vault pool is inactive",
					mechanism: "The coordinator lists the operator as active but the settlement vault marks its pool inactive, so the pool's emission is not captured. Ownership is unchanged; an upgraded coordinator or a pool migration can set this flag.",
					baseline:  "Every active operator's vault pool is active.",
					action:    "Confirm whether the pool is migrating; otherwise restore capture through governance.",
					verify:    "The vault pool is active, or the operator is deactivated, on a fresh run.",
				},
				evidence: fmt.Sprintf("Checkpoint height %d; operator %d.", checkpoint.number, operator.noId),
			}))
		}
		if len(reasons) == 0 {
			findings = append(findings, subnetHealthy(subnetIdentityFamily, subnetPoolCustodyIdentityClass, target))
			continue
		}
		findings = append(findings, subnetCustodyViolation(run.scope("complete", "coordinator-vault-and-native-registry"), target, reasons, fmt.Sprintf("Checkpoint height %d; operator %d; vault UID %d.", checkpoint.number, operator.noId, pool.Uid)))
	}
	escrowReasons := []string{}
	for _, read := range escrowReads {
		if read.err != nil {
			return append(findings, subnetUnknown(subnetIdentityFamily, subnetPoolCustodyIdentityClass, "subnet-identity/escrow", "native-"+subnetReadReason(read.err), subnetTransientSustain, "native Uids, Keys and Owner of the escrow hotkey")), nil
		}
	}
	if !state.escrowRegistered {
		escrowReasons = append(escrowReasons, "escrow-unregistered")
	}
	if [32]byte(escrowOwner) != state.vaultColdkey {
		escrowReasons = append(escrowReasons, "escrow-foreign-owner")
	}
	if !escrowReads[0].present {
		escrowReasons = append(escrowReasons, "escrow-uid-absent")
	} else {
		keyRead := &subnetStorageRead{pallet: "SubtensorModule", item: "Keys", args: [][]byte{netuid, subnetNetuidArg(uint16(escrowUid))}, value: &escrowKey}
		if err := subnetReadStorageBatch(ctx, checkpoint.chain, checkpoint.nativeHash, []*subnetStorageRead{keyRead}); err != nil || keyRead.err != nil {
			if canceled := subnetCanceled(ctx); canceled != nil {
				return nil, canceled
			}
			return append(findings, subnetUnknown(subnetIdentityFamily, subnetPoolCustodyIdentityClass, "subnet-identity/escrow", "native-keys-unavailable", subnetTransientSustain, "native Keys of the escrow UID")), nil
		}
		if [32]byte(escrowKey) != state.escrowHotkey {
			escrowReasons = append(escrowReasons, "escrow-uid-reassigned")
		}
	}
	if len(escrowReasons) == 0 {
		findings = append(findings, subnetHealthy(subnetIdentityFamily, subnetPoolCustodyIdentityClass, "subnet-identity/escrow"))
	} else {
		findings = append(findings, subnetCustodyViolation(run.scope("complete", "coordinator-vault-and-native-registry"), "subnet-identity/escrow", escrowReasons, fmt.Sprintf("Checkpoint height %d; escrow registered=%t.", checkpoint.number, state.escrowRegistered)))
	}
	return findings, nil
}

func subnetCustodyViolation(scope subnetReplayScope, target string, reasons []string, evidence string) finding {
	return subnetDecided(subnetIdentityFamily, subnetPoolCustodyIdentityClass, target, "", scope, subnetDecision{
		state: "violated",
		issue: subnetReplayIssue{
			tier: tierPage, sustain: subnetProvedSustain,
			invariant: "pool-custody-identity", expected: "registered-pool-hotkey-uid-both-ways-vault-coldkey-owner-distinct-roles", observed: strings.Join(subnetSortedUnique(reasons), ","),
			symptom:   "A subnet pool or escrow custody identity is not the registered one",
			mechanism: "At a finalized block the vault pool, the coordinator's operator version and the native UID mapping disagree, the hotkey is owned by a coldkey other than the immutable vault, or a pool, deposit, escrow, reserve or participant role is shared. Emission captured from such a pool can be credited to the wrong operator or leave custody.",
			baseline:  "Each active operator's vault pool equals its registered pool hotkey, holds its vault UID in both Keys and Uids, is owned by the vault coldkey, and every custody role is distinct; the escrow is registered under the vault coldkey.",
			action:    "Reconstruct the registration and rotation boundary from finalized events before any repair; never credit a reused UID to its former operator.",
			verify:    "A fresh checkpoint shows the registered pool identity in both directions under the vault coldkey.",
		},
		evidence: evidence + " Hotkeys and coldkeys are compared in memory and not rendered.",
	})
}

func subnetDuplicateKeys(keys [][32]byte) bool {
	seen := map[[32]byte]bool{}
	for _, key := range keys {
		if key == ([32]byte{}) || seen[key] {
			return true
		}
		seen[key] = true
	}
	return false
}

// Each reviewed validator still holds its hotkey's Uid, its reviewed coldkey,
// a validator permit and the stake threshold at the checkpoint.
func observeSubnetValidatorEligibility(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint) ([]finding, error) {
	netuid := subnetNetuidArg(run.settings.Deployment.Netuid)
	var permits []types.Bool
	var threshold types.U64
	shared := []*subnetStorageRead{
		{pallet: "SubtensorModule", item: "ValidatorPermit", args: [][]byte{netuid}, value: &permits},
		{pallet: "SubtensorModule", item: "StakeThreshold", value: &threshold},
	}
	type validatorCheck struct {
		uid   types.U16
		key   types.AccountID
		owner types.AccountID
		stake types.U64
		reads []*subnetStorageRead
	}
	checks := make([]*validatorCheck, len(run.settings.Validators))
	reads := append([]*subnetStorageRead(nil), shared...)
	for i, validator := range run.settings.Validators {
		hotkey, _ := subnetParseHash(validator.Hotkey)
		check := &validatorCheck{}
		check.reads = []*subnetStorageRead{
			{pallet: "SubtensorModule", item: "Uids", args: [][]byte{netuid, hotkey[:]}, value: &check.uid},
			{pallet: "SubtensorModule", item: "Owner", args: [][]byte{hotkey[:]}, value: &check.owner},
			{pallet: "SubtensorModule", item: "TotalHotkeyAlpha", args: [][]byte{hotkey[:], netuid}, value: &check.stake},
		}
		checks[i] = check
		reads = append(reads, check.reads...)
	}
	if err := subnetReadStorageBatch(ctx, checkpoint.chain, checkpoint.nativeHash, reads); err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		return []finding{subnetUnknown(subnetIdentityFamily, subnetValidatorEligibilityClass, "subnet-identity/validators", "native-"+subnetReadReason(err), subnetTransientSustain, "native validator identity, permit and stake at the checkpoint")}, nil
	}
	for _, read := range shared {
		if read.err != nil {
			return []finding{subnetUnknown(subnetIdentityFamily, subnetValidatorEligibilityClass, "subnet-identity/validators", "native-"+subnetReadReason(read.err), subnetTransientSustain, "the ValidatorPermit vector and StakeThreshold at the checkpoint")}, nil
		}
	}
	findings := []finding{}
	keyReads := []*subnetStorageRead{}
	for i, check := range checks {
		if check.reads[0].err == nil && check.reads[0].present {
			read := &subnetStorageRead{pallet: "SubtensorModule", item: "Keys", args: [][]byte{netuid, subnetNetuidArg(uint16(check.uid))}, value: &checks[i].key}
			keyReads = append(keyReads, read)
		}
	}
	if err := subnetReadStorageBatch(ctx, checkpoint.chain, checkpoint.nativeHash, keyReads); err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		return []finding{subnetUnknown(subnetIdentityFamily, subnetValidatorEligibilityClass, "subnet-identity/validators", "native-"+subnetReadReason(err), subnetTransientSustain, "native Keys of each validator UID")}, nil
	}
	for i, validator := range run.settings.Validators {
		target := subnetValidatorTarget(subnetIdentityFamily, i)
		check := checks[i]
		failed := false
		for _, read := range check.reads {
			if read.err != nil {
				findings = append(findings, subnetUnknown(subnetIdentityFamily, subnetValidatorEligibilityClass, target, "native-"+subnetReadReason(read.err), subnetTransientSustain, "this validator's native identity, permit and stake"))
				failed = true
				break
			}
		}
		if failed {
			continue
		}
		hotkey, _ := subnetParseHash(validator.Hotkey)
		coldkey, _ := subnetParseHash(validator.Coldkey)
		reasons := []string{}
		if !check.reads[0].present {
			reasons = append(reasons, "validator-unregistered")
		} else {
			uid := int(check.uid)
			if [32]byte(check.key) != [32]byte(hotkey) {
				reasons = append(reasons, "uid-mapping-inconsistent")
			}
			if uid >= len(permits) || !bool(permits[uid]) {
				reasons = append(reasons, "permit-absent")
			}
		}
		if [32]byte(check.owner) != [32]byte(coldkey) {
			reasons = append(reasons, "coldkey-differs")
		}
		if uint64(check.stake) < uint64(threshold) {
			reasons = append(reasons, "stake-below-threshold")
		}
		if len(reasons) == 0 {
			findings = append(findings, subnetHealthy(subnetIdentityFamily, subnetValidatorEligibilityClass, target))
			continue
		}
		findings = append(findings, subnetDecided(subnetIdentityFamily, subnetValidatorEligibilityClass, target, "", run.scope("complete", "reviewed-validator-census"), subnetDecision{
			state: "ineligible",
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetConfigurationSustain,
				invariant: "reviewed-validator-eligible", expected: "registered-permitted-reviewed-owner-and-threshold-stake", observed: strings.Join(reasons, ","),
				symptom:   "A reviewed subnet validator is not eligible at the checkpoint",
				mechanism: "The validator's hotkey no longer holds a UID with a validator permit, its owner is not the reviewed coldkey, or its stake is below the native threshold. Its weights would not count, and evidence signed under a changed identity cannot be attributed to the reviewed validator.",
				baseline:  "Each reviewed validator's hotkey holds its UID in both directions, is owned by its reviewed coldkey, has a permit and meets the stake threshold.",
				action:    "Check the validator's registration, permit ranking and stake; restore the reviewed identity or update the reviewed census only through review.",
				verify:    "The validator is registered with a permit and sufficient stake under its reviewed coldkey on a fresh run.",
			},
			evidence: fmt.Sprintf("Checkpoint height %d; validator ordinal %d. Accepting evidence from an ineligible identity is a validator-evidence anchor finding in §22.8.", checkpoint.number, i+1),
		}))
	}
	return findings, nil
}

// One native fleet commitment and its coordinator mirror.
type subnetCommitmentObservation struct {
	hotkey    [32]byte
	hash      [32]byte
	block     uint64
	mirror    stabi.MirroredCommitmentsOutput
	mirrorErr error
}

// Every native fleet commitment is mirrored by the coordinator with the same
// hash, block and canonical block hash, within the reviewed lag and before
// the policy's commitment age expires.
func observeSubnetCommitmentMirrors(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState) ([]finding, error) {
	target := "subnet-identity/commitments"
	unknown := func(reason string) []finding {
		return []finding{subnetUnknown(subnetIdentityFamily, subnetCommitmentMirrorLagClass, target, reason, subnetTransientSustain, "every native fleet commitment and its coordinator mirror at the checkpoint")}
	}
	netuid := subnetNetuidArg(run.settings.Deployment.Netuid)
	prefix, err := subnetStorageMapPrefix(checkpoint.chain.Meta, crv4.CommitmentsPalletName, "CommitmentOf", netuid)
	if err != nil {
		return unknown("commitment-prefix-" + subnetReadReason(err)), nil
	}
	keys := [][]byte{}
	var start []byte
	for len(keys) < run.limits.maxCommitments {
		page := min(256, run.limits.maxCommitments-len(keys)+1)
		part, err := checkpoint.native.keysPaged(ctx, prefix, page, start, checkpoint.nativeHash)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		if err != nil {
			return unknown("commitment-keys-" + subnetReadReason(err)), nil
		}
		keys = append(keys, part...)
		if len(part) < page {
			break
		}
		start = part[len(part)-1]
	}
	if len(keys) > run.limits.maxCommitments {
		return unknown("commitment-census-exceeds-bound"), nil
	}
	observations := []*subnetCommitmentObservation{}
	reads := []*subnetStorageRead{}
	raws := make([]subnetRawStorageValue, len(keys))
	for i, key := range keys {
		if len(key) < len(prefix)+32 {
			return unknown("commitment-key-invalid"), nil
		}
		var hotkey [32]byte
		copy(hotkey[:], key[len(key)-32:])
		derived, err := types.CreateStorageKey(checkpoint.chain.Meta, crv4.CommitmentsPalletName, "CommitmentOf", netuid, hotkey[:])
		if err != nil || codec.HexEncodeToString(derived) != codec.HexEncodeToString(key) {
			return unknown("commitment-key-not-derived"), nil
		}
		observations = append(observations, &subnetCommitmentObservation{hotkey: hotkey})
		reads = append(reads, &subnetStorageRead{pallet: crv4.CommitmentsPalletName, item: "CommitmentOf", args: [][]byte{netuid, hotkey[:]}, value: &raws[i]})
	}
	if err := subnetReadStorageBatch(ctx, checkpoint.chain, checkpoint.nativeHash, reads); err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		return unknown("commitments-" + subnetReadReason(err)), nil
	}
	fleet := []*subnetCommitmentObservation{}
	legacy := 0
	for i, observation := range observations {
		if reads[i].err != nil || !reads[i].present {
			return unknown("commitment-value-unavailable"), nil
		}
		hash, block, err := crv4.DecodeFleetCommitmentRegistrationWithBlockV454(raws[i])
		if err != nil {
			legacy++
			continue
		}
		observation.hash, observation.block = hash, block
		fleet = append(fleet, observation)
	}
	coordinator := stabi.NewSTCoordinator()
	calls := make([]subnetEvmCall, len(fleet))
	for i, observation := range fleet {
		calls[i] = subnetEvmCall{to: state.coordinator, data: coordinator.PackMirroredCommitments(observation.hotkey)}
	}
	if err := checkpoint.evmReader.callBatch(ctx, calls, checkpoint.evm); err != nil {
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		return unknown("mirrors-" + subnetReadReason(err)), nil
	}
	for i, observation := range fleet {
		if calls[i].err != nil {
			return unknown("mirror-" + subnetReadReason(calls[i].err)), nil
		}
		observation.mirror, observation.mirrorErr = coordinator.UnpackMirroredCommitments(calls[i].output)
		if observation.mirrorErr != nil {
			return unknown("mirror-output-invalid"), nil
		}
	}
	pageReasons := []string{}
	warnReasons := []string{}
	maxAge := state.currentPolicy.CommitmentMaxAgeBlocks
	for _, observation := range fleet {
		mirror := observation.mirror
		switch {
		case observation.block > checkpoint.number:
			return unknown("commitment-after-checkpoint"), nil
		case mirror.FinalizedBlock > observation.block:
			pageReasons = append(pageReasons, "mirror-newer-than-native")
		case mirror.FinalizedBlock == observation.block && mirror.CommitmentHash != observation.hash:
			pageReasons = append(pageReasons, "mirror-conflicts")
		case mirror.FinalizedBlock == observation.block:
			canonical, found, err := checkpoint.native.blockHash(ctx, observation.block)
			if canceled := subnetCanceled(ctx); canceled != nil {
				return nil, canceled
			}
			if err != nil || !found {
				return unknown("mirror-block-hash-unavailable"), nil
			}
			if [32]byte(canonical) != mirror.FinalizedBlockHash {
				pageReasons = append(pageReasons, "mirror-block-not-canonical")
			}
		default:
			age := checkpoint.number - observation.block
			if maxAge != 0 && age > maxAge {
				pageReasons = append(pageReasons, "mirror-deadline-missed")
			} else if age > run.limits.maxFinalityLagBlocks*16 {
				warnReasons = append(warnReasons, "mirror-stale")
			}
		}
	}
	evidence := fmt.Sprintf("Checkpoint height %d; %d native fleet commitments; %d other commitments; policy commitment age bound %d blocks.", checkpoint.number, len(fleet), legacy, maxAge)
	findings := []finding{}
	if len(pageReasons) != 0 {
		findings = append(findings, subnetDecided(subnetIdentityFamily, subnetCommitmentMirrorLagClass, target, "", run.scope("complete", "native-commitments-and-coordinator-mirror"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "commitment-mirror-consistent", expected: "mirror-equals-native-commitment-before-expiry", observed: strings.Join(subnetSortedUnique(pageReasons), ","),
				symptom:   "A coordinator commitment mirror contradicts native finalized state",
				mechanism: "A mirrored commitment names a newer block, a different hash or a non-canonical block hash than the native commitment, or a valid commitment expired before it was mirrored. A false mirror can admit a binding no fleet authorized; an expired one drops valid membership.",
				baseline:  "Every native fleet commitment is mirrored with its exact hash, block and canonical block hash before the policy's commitment age expires.",
				action:    "Preserve the native inclusion and the EVM mirror receipt; investigate the commitment oracle before any binding relies on the mirror.",
				verify:    "Every fleet commitment's mirror matches native finalized state on a fresh run.",
			},
			evidence: evidence,
		}))
	}
	if len(warnReasons) != 0 {
		findings = append(findings, subnetDecided(subnetIdentityFamily, subnetCommitmentMirrorLagClass, target, "lag", run.scope("complete", "native-commitments-and-coordinator-mirror"), subnetDecision{
			state: "lagging",
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetTransientSustain,
				invariant: "commitment-mirrored-within-lag", expected: fmt.Sprintf("mirrored-within-%d-blocks", run.limits.maxFinalityLagBlocks*16), observed: strings.Join(subnetSortedUnique(warnReasons), ","),
				symptom:   "A native fleet commitment is not yet mirrored by the coordinator",
				mechanism: "A finalized fleet commitment has waited beyond the reviewed lag without a coordinator mirror. Its fleet cannot bind until the commitment oracle mirrors it.",
				baseline:  "The commitment oracle mirrors each finalized fleet commitment within the reviewed lag.",
				action:    "Check the commitment oracle's progress and funding; do not mirror manually from the monitor.",
				verify:    "The commitment is mirrored on a later run.",
			},
			evidence: evidence,
		}))
	}
	if len(findings) == 0 {
		findings = append(findings, subnetHealthy(subnetIdentityFamily, subnetCommitmentMirrorLagClass, target))
	}
	return findings, nil
}

// Every FleetBound event over the bounded frontier is re-verified from its
// original calldata: domain, client Ed25519 consent, hotkey sr25519
// authorization, generation, validity span and future effective epoch.
func (self *subnetIdentityProbe) observeFleetBindings(ctx context.Context, run *subnetRun, checkpoint *subnetCheckpoint, state *subnetDeploymentState) ([]finding, error) {
	target := "subnet-identity/bindings"
	coordinatorAbi, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		return nil, subnetReadFailure("coordinator-abi-invalid", err)
	}
	bound := coordinatorAbi.Events["FleetBound"]
	method, ok := coordinatorAbi.Methods["bindFleetMember"]
	if !ok {
		return nil, subnetReadFailure("coordinator-abi-invalid", nil)
	}
	plan := self.bindingLogs.plan(checkpoint.number, run.settings.DeployBlock, run.limits)
	logs, err := subnetFetchLogs(ctx, checkpoint.evmReader, state.coordinator, [][]common.Hash{{bound.ID}}, plan, run.limits)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return []finding{subnetUnknown(subnetIdentityFamily, subnetFleetBindingInvalidClass, target, "logs-"+subnetReadReason(err), subnetTransientSustain, "finalized FleetBound events over the frontier")}, nil
	}
	coordinator := stabi.NewSTCoordinator()
	pageReasons := []string{}
	indirect := 0
	for i := range logs {
		event, err := coordinator.UnpackFleetBoundEvent(&logs[i])
		if err != nil {
			pageReasons = append(pageReasons, "event-undecodable")
			continue
		}
		to, input, blockHash, err := checkpoint.evmReader.transactionInput(ctx, logs[i].TxHash)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, canceled
		}
		if err != nil {
			return []finding{subnetUnknown(subnetIdentityFamily, subnetFleetBindingInvalidClass, target, "transaction-"+subnetReadReason(err), subnetTransientSustain, "the original bindFleetMember calldata of every accepted binding")}, nil
		}
		if blockHash != logs[i].BlockHash {
			pageReasons = append(pageReasons, "transaction-block-differs")
			continue
		}
		if to != state.coordinator || len(input) < 4 || string(input[:4]) != string(method.ID) {
			indirect++
			continue
		}
		binding, clientSignature, hotkeySignature, err := subnetDecodeBindFleetMember(method, input[4:])
		if err != nil {
			pageReasons = append(pageReasons, "calldata-undecodable")
			continue
		}
		pageReasons = append(pageReasons, subnetFleetBindingReasons(binding, clientSignature, hotkeySignature, event, logs[i].BlockNumber, run.settings.Deployment, state)...)
	}
	findings := []finding{}
	evidence := fmt.Sprintf("Checkpoint height %d; frontier read %d accepted bindings; %d sent through a contract wallet whose calldata is not the direct call.", checkpoint.number, len(logs), indirect)
	if len(pageReasons) != 0 {
		findings = append(findings, subnetDecided(subnetIdentityFamily, subnetFleetBindingInvalidClass, target, "", run.scope("bounded-frontier", "original-binding-transactions"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "accepted-binding-verifies", expected: "client-consent-hotkey-authorization-domain-and-epochs-verify", observed: strings.Join(subnetSortedUnique(pageReasons), ","),
				symptom:   "The coordinator accepted an invalid fleet binding",
				mechanism: "An accepted FleetBound binding fails its client Ed25519 consent, its hotkey sr25519 authorization, its chain/netuid/coordinator domain, its generation or validity span, or its future effective epoch when re-verified from the original calldata. Such a binding can credit a fleet that never authorized it.",
				baseline:  "Every accepted binding verifies under both signatures and the deployment domain, takes effect only in a future epoch and stays within the policy's validity span.",
				action:    "Preserve the transaction and its event, quarantine the binding's credit, and investigate the coordinator's verification before relying on fleet membership.",
				verify:    "Every accepted binding re-verifies on a fresh run and the incident attributes the accepted one.",
			},
			evidence: evidence + " Rejected hostile requests emit no event and are not counted here.",
		}))
	}
	if indirect != 0 {
		findings = append(findings, subnetUnknown(subnetIdentityFamily, subnetFleetBindingInvalidClass, target, "binding-calldata-indirect", subnetTransientSustain, "decodable direct bindFleetMember calldata for every accepted binding"))
	}
	if !plan.complete {
		findings = append(findings, subnetUnknown(subnetIdentityFamily, subnetFleetBindingInvalidClass, "subnet-identity/binding-events", "event-frontier-behind", subnetTransientSustain, "a FleetBound census reaching the agreed checkpoint"))
	}
	if len(findings) == 0 {
		findings = append(findings, subnetHealthy(subnetIdentityFamily, subnetFleetBindingInvalidClass, target))
	}
	if !plan.empty {
		run.afterPublish(func() { self.bindingLogs.advance(plan.to) })
	}
	return findings, nil
}

// The exact calldata arguments of bindFleetMember.
func subnetDecodeBindFleetMember(method abi.Method, data []byte) (protocol.FleetBinding, []byte, []byte, error) {
	values, err := method.Inputs.Unpack(data)
	if err != nil || len(values) != 3 {
		return protocol.FleetBinding{}, nil, nil, subnetReadFailure("calldata-invalid", err)
	}
	binding, ok := abi.ConvertType(values[0], new(stabi.STCoordinatorFleetBinding)).(*stabi.STCoordinatorFleetBinding)
	clientSignature, clientOk := values[1].([]byte)
	hotkeySignature, hotkeyOk := values[2].([]byte)
	if !ok || !clientOk || !hotkeyOk {
		return protocol.FleetBinding{}, nil, nil, subnetReadFailure("calldata-invalid", nil)
	}
	return protocol.FleetBinding{
		ChainID: binding.ChainId, Netuid: binding.Netuid, Coordinator: [20]byte(binding.Coordinator),
		FleetID: binding.FleetId, Hotkey: binding.Hotkey, ClientID: binding.ClientId, ClientKey: binding.ClientKey,
		Generation: binding.Generation, ValidFromEpoch: binding.ValidFromEpoch, ValidToEpoch: binding.ValidToEpoch,
		CommitmentHash: binding.CommitmentHash,
	}, clientSignature, hotkeySignature, nil
}

// The reasons one accepted binding fails re-verification.
func subnetFleetBindingReasons(binding protocol.FleetBinding, clientSignature, hotkeySignature []byte, event *stabi.STCoordinatorFleetBound, block uint64, deployment SubnetDeploymentIdentity, state *subnetDeploymentState) []string {
	reasons := []string{}
	if err := binding.Validate(); err != nil {
		return []string{"binding-shape"}
	}
	if binding.ChainID != deployment.ChainId || binding.Netuid != deployment.Netuid || common.Address(binding.Coordinator) != state.coordinator {
		reasons = append(reasons, "binding-domain")
	}
	if binding.ClientID != event.ClientId || binding.FleetID != event.FleetId || binding.Hotkey != event.Hotkey || binding.Generation != event.Generation || binding.ValidFromEpoch != event.ValidFromEpoch || binding.ValidToEpoch != event.ValidToEpoch {
		reasons = append(reasons, "binding-event-differs")
	}
	if !binding.VerifyClient(clientSignature) {
		reasons = append(reasons, "client-signature")
	}
	if !binding.VerifyHotkey(hotkeySignature) {
		reasons = append(reasons, "hotkey-signature")
	}
	epoch, ok := subnetEpochAtBlock(state.policies, block)
	if !ok || binding.ValidFromEpoch <= epoch {
		reasons = append(reasons, "binding-not-future-effective")
	}
	validity := uint64(0)
	for _, policy := range state.policies {
		if policy.EffectiveEpoch <= binding.ValidFromEpoch {
			validity = policy.MaximumBindingValidityEpochs
		}
	}
	if validity == 0 || binding.ValidToEpoch-binding.ValidFromEpoch+1 > validity {
		reasons = append(reasons, "binding-validity-span")
	}
	return reasons
}
