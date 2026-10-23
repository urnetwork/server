// SIGNALS.md §22.0 and §22.1 agreed finalized checkpoint. Every family reader
// reads at one height P = min(both native finalized heads, both Evm finalized
// heads): both native endpoints must report the same block hash at P, both Evm
// endpoints the same Evm hash, both the same runtime state, and the runtime's
// own Ethereum.BlockHash(P) mapping must name that Evm hash. Contradictory
// finalized history is a violation; an unavailable or lagging endpoint is
// unknown. Only an approved, metadata-verified runtime binds storage layouts.
package monitor

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/crv4"
)

// The comparison source of every checkpoint finding.
const subnetEndpointComparison = "two-native-and-two-evm-endpoints"

// One agreed finalized checkpoint and the primary bound readers at it.
type subnetCheckpoint struct {
	number     uint64
	nativeHash types.Hash
	evm        subnetEvmBlock
	genesis    types.Hash
	runtime    *subnetVerifiedRuntime
	native     subnetNativeReader
	evmReader  subnetEvmReader
	chain      *crv4.Chain
}

// The checkpoint or the gate that prevented it, with the chain-family
// findings that only the §22.1 reader emits.
type subnetCheckpointResult struct {
	checkpoint *subnetCheckpoint
	gate       string
	findings   []finding
}

// Each endpoint's finalized head.
type subnetEndpointHead struct {
	hash   types.Hash
	number uint64
	reason string
}

func (self subnetCheckpointResult) withGate(gate string, findings ...finding) subnetCheckpointResult {
	self.gate = gate
	self.findings = append(self.findings, findings...)
	return self
}

// A proved checkpoint contradiction pages at once: the same height or block
// hash cannot have two finalized answers.
func subnetFinalityContradiction(scope subnetReplayScope, target, frame, reason, evidence string) finding {
	return subnetDecided(subnetChainFamily, subnetFinalityDivergenceClass, target, frame, scope, subnetDecision{
		state: "violated",
		issue: subnetReplayIssue{
			tier: tierPage, sustain: subnetProvedSustain,
			invariant: "one-finalized-history", expected: "agreeing-finalized-answers", observed: reason,
			symptom:   "Independent subnet endpoints contradict each other at one finalized checkpoint",
			mechanism: "Two enrolled endpoints, or one endpoint's runtime mapping, give different finalized answers for the same height or block hash. Finalized history cannot fork; one source is wrong or the deployment is reading a different chain, so no economic check is trusted at this checkpoint.",
			baseline:  "Both native endpoints agree on the block hash and runtime state at the agreed finalized height, both EVM endpoints agree on its EVM hash, and Ethereum.BlockHash at that native block names the same EVM hash.",
			action:    "Preserve both endpoint responses privately and investigate provider routing, indexing and node database state before attributing a chain fault. Do not move the reviewed pin to whichever answer is newer.",
			verify:    "Both endpoints return identical finalized answers at a fresh agreed checkpoint and the mapping check passes.",
		},
		evidence: evidence,
	})
}

// A lagging endpoint cannot stand in for a synced operational endpoint.
func subnetFinalityLag(scope subnetReplayScope, target, reason string, bound uint64, evidence string) finding {
	return subnetDecided(subnetChainFamily, subnetFinalityDivergenceClass, target, "lag", scope, subnetDecision{
		state: "lagging",
		issue: subnetReplayIssue{
			tier: tierWarn, sustain: subnetTransientSustain,
			invariant: "finalized-heads-within-lag", expected: fmt.Sprintf("at-most-%d-blocks-apart", bound), observed: reason,
			symptom:   "A subnet endpoint's finalized head lags its peer beyond the reviewed bound",
			mechanism: "The agreed checkpoint is the lower finalized head, so a stale or syncing endpoint delays every reader's observation. A lagging private node is not a substitute for a synced operational endpoint.",
			baseline:  "Every enrolled endpoint's finalized head is within the reviewed lag bound of its peer.",
			action:    "Check the lagging endpoint's sync and finality progress (see §17 for the owned nodes). Do not raise the lag bound to clear this warning.",
			verify:    "Both endpoints' finalized heads are within the bound on consecutive runs.",
		},
		evidence: evidence,
	})
}

// Runtime identity outside the reviewed signing authority.
func subnetRuntimeIdentityFinding(scope subnetReplayScope, frame, reason, evidence string) finding {
	return subnetDecided(subnetChainFamily, subnetRuntimeIdentityClass, "subnet-chain/runtime", frame, scope, subnetDecision{
		state: "unreviewed",
		issue: subnetReplayIssue{
			tier: tierWarn, sustain: subnetProvedSustain,
			invariant: "reviewed-runtime-identity", expected: "sn-yml-genesis-chain-id-and-a-reviewed-runtime-tuple", observed: reason,
			symptom:   "The live subnet runtime or chain identity is not the reviewed one",
			mechanism: "The finalized chain's genesis, EVM chain id or complete runtime tuple (spec name and version, transaction and state versions, Wasm code hash and metadata hash) does not match the code-owned sn catalog or a reviewed successor. A matching spec number alone is insufficient; storage layouts are not trusted until the runtime is reviewed.",
			baseline:  "Genesis and EVM chain id equal the sn.yml deployment, and the live runtime tuple exactly equals one approved artifact whose metadata hash was verified.",
			action:    "Route to runtime compatibility review and record the reviewed tuple through the owning approval; do not blindly add the observed tuple to the enrollment.",
			verify:    "The live tuple equals a reviewed artifact and every family reader reads at an approved checkpoint.",
		},
		evidence: evidence + " This passive reader cannot attribute a writer's runtime; a live writer acting under this runtime is an applied-weight or settlement receipt in §22.6/§22.9.",
	})
}

// Reads the agreed checkpoint. Read failures become unknown evidence; only a
// canceled parent returns an error.
func readSubnetCheckpoint(ctx context.Context, run *subnetRun, cache *subnetRuntimeCache) (subnetCheckpointResult, error) {
	result := subnetCheckpointResult{}
	natives := run.nativeReaders()
	evms := run.evmReaders()
	deployment := run.settings.Deployment
	var nativeHeads [2]subnetEndpointHead
	var evmHeads [2]subnetEvmBlock
	evmReasons := [2]string{}
	for i, native := range natives {
		hash, err := native.finalizedHead(ctx)
		if err == nil {
			nativeHeads[i].number, _, err = native.header(ctx, hash)
		}
		if canceled := subnetCanceled(ctx); canceled != nil {
			return result, canceled
		}
		nativeHeads[i].hash = hash
		if err != nil {
			nativeHeads[i].reason = native.endpoint.label + "-" + subnetReadReason(err)
		}
	}
	for i, evm := range evms {
		chainId, err := evm.chainId(ctx)
		if err == nil && chainId != deployment.ChainId {
			result.findings = append(result.findings, subnetRuntimeIdentityFinding(run.scopeAt(0, "complete", "evm-chain-id-against-sn-yml"), "evm-chain-id", evm.endpoint.label+"-chain-id-mismatch", "The EVM endpoint reports a chain id other than the sn.yml deployment."))
			return result.withGate("evm-chain-id-mismatch"), nil
		}
		if err == nil {
			evmHeads[i], err = evm.finalized(ctx)
		}
		if canceled := subnetCanceled(ctx); canceled != nil {
			return result, canceled
		}
		if err != nil {
			evmReasons[i] = evm.endpoint.label + "-" + subnetReadReason(err)
		}
	}
	unavailable := false
	for i := range natives {
		if nativeHeads[i].reason != "" {
			result.findings = append(result.findings, subnetUnknown(subnetChainFamily, subnetFinalityDivergenceClass, "subnet-chain/native", nativeHeads[i].reason, subnetTransientSustain, "a finalized head from both enrolled native endpoints"))
			unavailable = true
		}
		if evmReasons[i] != "" {
			result.findings = append(result.findings, subnetUnknown(subnetChainFamily, subnetFinalityDivergenceClass, "subnet-chain/evm", evmReasons[i], subnetTransientSustain, "a finalized block from both enrolled EVM endpoints"))
			unavailable = true
		}
	}
	if unavailable {
		result.findings = append(result.findings, subnetUnknown(subnetChainFamily, subnetRuntimeIdentityClass, "subnet-chain/runtime", "checkpoint-unavailable", subnetTransientSustain, "an agreed finalized checkpoint"))
		return result.withGate("checkpoint-unavailable"), nil
	}
	// A foreign genesis is an endpoint on another network, not a fork of this
	// one, so it is checked before any height is compared.
	genesis, ok := subnetParseHash(deployment.GenesisHash)
	if !ok {
		return result.withGate("deployment-identity-invalid"), nil
	}
	for _, native := range natives {
		observedGenesis, found, err := native.blockHash(ctx, 0)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return result, canceled
		}
		if err != nil || !found {
			reason := native.endpoint.label + "-genesis-unavailable"
			if err != nil {
				reason = native.endpoint.label + "-" + subnetReadReason(err)
			}
			return result.withGate("checkpoint-unavailable", subnetUnknown(subnetChainFamily, subnetRuntimeIdentityClass, "subnet-chain/runtime", reason, subnetTransientSustain, "the genesis hash from both native endpoints")), nil
		}
		if observedGenesis != genesis {
			result.findings = append(result.findings, subnetRuntimeIdentityFinding(run.scopeAt(0, "complete", "native-genesis-against-sn-yml"), "genesis", native.endpoint.label+"-genesis-mismatch", "The endpoint's genesis differs from the sn.yml deployment."))
			return result.withGate("genesis-mismatch"), nil
		}
	}
	number := min(nativeHeads[0].number, nativeHeads[1].number, evmHeads[0].number, evmHeads[1].number)
	lagEvidence := fmt.Sprintf("Agreed height %d; native finalized heads %d and %d; EVM finalized heads %d and %d.", number, nativeHeads[0].number, nativeHeads[1].number, evmHeads[0].number, evmHeads[1].number)
	lag := run.limits.maxFinalityLagBlocks
	if subnetAbsDifference(nativeHeads[0].number, nativeHeads[1].number) > lag {
		result.findings = append(result.findings, subnetFinalityLag(run.scopeAt(number, "complete", subnetEndpointComparison), "subnet-chain/native", "native-finalized-heads-apart", lag, lagEvidence))
	}
	if subnetAbsDifference(evmHeads[0].number, evmHeads[1].number) > lag || max(nativeHeads[0].number, nativeHeads[1].number) > min(evmHeads[0].number, evmHeads[1].number)+lag {
		result.findings = append(result.findings, subnetFinalityLag(run.scopeAt(number, "complete", subnetEndpointComparison), "subnet-chain/evm", "evm-finalized-behind", lag, lagEvidence))
	}

	// Both native endpoints answer the agreed height, and an endpoint whose
	// own finalized head is that height must name the same hash.
	var nativeHashes [2]types.Hash
	for i, native := range natives {
		hash, found, err := native.blockHash(ctx, number)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return result, canceled
		}
		if err != nil || !found {
			reason := native.endpoint.label + "-checkpoint-hash-unavailable"
			if err != nil {
				reason = native.endpoint.label + "-" + subnetReadReason(err)
			}
			return result.withGate("checkpoint-unavailable", subnetUnknown(subnetChainFamily, subnetFinalityDivergenceClass, "subnet-chain/native", reason, subnetTransientSustain, "the canonical hash at the agreed finalized height")), nil
		}
		if nativeHeads[i].number == number && hash != nativeHeads[i].hash {
			return result.withGate("finality-divergence", subnetFinalityContradiction(run.scopeAt(number, "contradicted", subnetEndpointComparison), "subnet-chain/native", "", native.endpoint.label+"-head-not-canonical", lagEvidence)), nil
		}
		nativeHashes[i] = hash
	}
	if nativeHashes[0] != nativeHashes[1] {
		return result.withGate("finality-divergence", subnetFinalityContradiction(run.scopeAt(number, "contradicted", subnetEndpointComparison), "subnet-chain/native", "", "native-hashes-differ-at-agreed-height", lagEvidence)), nil
	}
	var evmBlocks [2]subnetEvmBlock
	for i, evm := range evms {
		block, err := evm.blockAt(ctx, number)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return result, canceled
		}
		if err != nil {
			return result.withGate("checkpoint-unavailable", subnetUnknown(subnetChainFamily, subnetFinalityDivergenceClass, "subnet-chain/evm", evm.endpoint.label+"-"+subnetReadReason(err), subnetTransientSustain, "the EVM block at the agreed finalized height")), nil
		}
		if evmHeads[i].number == number && block.hash != evmHeads[i].hash {
			return result.withGate("finality-divergence", subnetFinalityContradiction(run.scopeAt(number, "contradicted", subnetEndpointComparison), "subnet-chain/evm", "", evm.endpoint.label+"-head-not-canonical", lagEvidence)), nil
		}
		evmBlocks[i] = block
	}
	if evmBlocks[0].hash != evmBlocks[1].hash {
		return result.withGate("finality-divergence", subnetFinalityContradiction(run.scopeAt(number, "contradicted", subnetEndpointComparison), "subnet-chain/evm", "", "evm-hashes-differ-at-agreed-height", lagEvidence)), nil
	}

	// The complete runtime state at the agreed block.
	var versions [2]crv4.RuntimeVersionIdentity
	var codeHashes [2]string
	for i, native := range natives {
		version, err := native.runtimeVersion(ctx, nativeHashes[0])
		if err == nil {
			versions[i] = version
			codeHashes[i], err = native.codeHash(ctx, nativeHashes[0])
		}
		if canceled := subnetCanceled(ctx); canceled != nil {
			return result, canceled
		}
		if err != nil {
			return result.withGate("checkpoint-unavailable", subnetUnknown(subnetChainFamily, subnetRuntimeIdentityClass, "subnet-chain/runtime", native.endpoint.label+"-"+subnetReadReason(err), subnetTransientSustain, "the runtime version and code hash from both native endpoints")), nil
		}
	}
	if versions[0] != versions[1] || codeHashes[0] != codeHashes[1] {
		return result.withGate("finality-divergence", subnetFinalityContradiction(run.scopeAt(number, "contradicted", subnetEndpointComparison), "subnet-chain/native", "runtime-state", "runtime-state-differs-at-agreed-hash", lagEvidence)), nil
	}
	approval := subnetApproveRuntime(versions[0], codeHashes[0], run.settings.ReviewedRuntimes)
	runtimeEvidence := fmt.Sprintf("Observed node runtime %s/%d/%d/%d at height %d.", versions[0].SpecName, versions[0].SpecVersion, versions[0].TransactionVersion, versions[0].StateVersion, number)
	if approval.status != subnetRuntimeApproved {
		result.findings = append(result.findings, subnetRuntimeIdentityFinding(run.scopeAt(number, "complete", "reviewed-runtime-artifacts"), "", "runtime-"+approval.status, runtimeEvidence))
		return result.withGate("runtime-" + approval.status), nil
	}
	runtime, status, err := cache.verify(ctx, natives[0], nativeHashes[0], approval.artifact)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return result, canceled
	}
	if err != nil {
		return result.withGate("runtime-metadata-unavailable", subnetUnknown(subnetChainFamily, subnetRuntimeIdentityClass, "subnet-chain/runtime", natives[0].endpoint.label+"-"+subnetReadReason(err), subnetTransientSustain, "the approved runtime's metadata at the agreed checkpoint")), nil
	}
	if status != subnetRuntimeApproved {
		result.findings = append(result.findings, subnetRuntimeIdentityFinding(run.scopeAt(number, "complete", "reviewed-runtime-artifacts"), "metadata", "runtime-metadata-conflict", runtimeEvidence))
		return result.withGate("runtime-conflict"), nil
	}

	// The runtime's own Evm mapping must name the agreed Evm hash.
	argument := make([]byte, 32)
	binary.LittleEndian.PutUint64(argument, number)
	for _, native := range natives {
		var mapped types.Hash
		_, err := subnetReadStorage(ctx, native.bound(runtime.metadata, genesis), nativeHashes[0], "Ethereum", "BlockHash", &mapped, argument)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return result, canceled
		}
		if err != nil {
			return result.withGate("checkpoint-unavailable", subnetUnknown(subnetChainFamily, subnetFinalityDivergenceClass, "subnet-chain/evm", native.endpoint.label+"-"+subnetReadReason(err), subnetTransientSustain, "the runtime's Ethereum.BlockHash mapping at the agreed checkpoint")), nil
		}
		if common.Hash(mapped) != evmBlocks[0].hash {
			return result.withGate("finality-divergence", subnetFinalityContradiction(run.scopeAt(number, "contradicted", subnetEndpointComparison), "subnet-chain/evm", "native-mapping", native.endpoint.label+"-mapping-differs", lagEvidence)), nil
		}
	}
	result.findings = append(result.findings,
		subnetHealthy(subnetChainFamily, subnetFinalityDivergenceClass, "subnet-chain/native"),
		subnetHealthy(subnetChainFamily, subnetFinalityDivergenceClass, "subnet-chain/evm"),
		subnetHealthy(subnetChainFamily, subnetRuntimeIdentityClass, "subnet-chain/runtime"),
	)
	run.checkpointNumber = number
	result.checkpoint = &subnetCheckpoint{
		number:     number,
		nativeHash: nativeHashes[0],
		evm:        evmBlocks[0],
		genesis:    genesis,
		runtime:    runtime,
		native:     natives[0],
		evmReader:  evms[0],
		chain:      natives[0].bound(runtime.metadata, genesis),
	}
	return result, nil
}

func subnetAbsDifference(a, b uint64) uint64 {
	if a < b {
		return b - a
	}
	return a - b
}

// The family gate for a reader that only consumes the checkpoint.
func subnetCheckpointGap(family subnetFamily, result subnetCheckpointResult) finding {
	sustain := subnetTransientSustain
	if result.gate == "runtime-unreviewed" || result.gate == "runtime-conflict" || result.gate == "evm-chain-id-mismatch" || result.gate == "genesis-mismatch" || result.gate == "deployment-identity-invalid" {
		sustain = subnetConfigurationSustain
	}
	return subnetCoverageGap(family, "checkpoint", result.gate, sustain)
}
