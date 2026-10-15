// Native execution reads derive their root from the same verified parent/child
// finality proof. Values remain raw storage facts; runtime/provider semantics
// require the separately approved original layout and unmodified execution.
package strecovery

import (
	"context"
	"errors"
)

const NativeExecutionStorageSchema = "urnetwork-native-execution-storage-v1"
const MaximumNativeExecutionStorageReads = 4*4096 + 4

// A compressed trie has fewer branch nodes than leaves; separately hashed
// values add one node each. The native profile reserves twice that structural
// 3*reads+1 bound, while retaining its independent64MiB decoded-byte ceiling.
const MaximumNativeExecutionStorageNodes = 2 * (3*MaximumNativeExecutionStorageReads + 1)

// Omitted blobs are unavailable evidence, never authenticated absence.
var ErrNativeStorageIncomplete = errors.New("native storage proof is incomplete")

// Four facts per possible UID plus runtime/subnet identity fit the accepted
// 4096 UID profile. Existing receipt witnesses keep their original32-read bound.
// Receipt proofs retain8192nodes; native proofs have a separate finite node
// count. Both preserve64MiB total decoded input, not per-key allowances.
type NativeExecutionStorageWitness struct {
	Schema           string                `json:"schema"`
	FinalityHash     string                `json:"finality_hash"`
	RootRole         string                `json:"root_role"`
	NativeStateBlock ObservedBlockIdentity `json:"native_state_block"`
	Reads            []NativeStorageRead   `json:"reads"`
	Nodes            []string              `json:"nodes"`
}

type NativeExecutionStorage struct {
	Schema           string                   `json:"schema"`
	WitnessHash      string                   `json:"witness_hash"`
	RootRole         string                   `json:"root_role"`
	NativeStateBlock ObservedBlockIdentity    `json:"native_state_block"`
	NativeStateRoot  string                   `json:"native_state_root"`
	Reads            []NativeStorageRead      `json:"reads"`
	Finality         *NativeExecutionFinality `json:"finality"`
}

func (self *NativeExecutionFinalityProof) Hash() string { return objectDigest(self) }

// Finality is freshly checked; caller-provided roots or prior boolean reports
// cannot select storage. Missing nodes refuse, distinct from proven absence.
func VerifyNativeExecutionStorage(ctx context.Context, genesis string, checkpoint *NativeFinalityCheckpoint, proof *NativeExecutionFinalityProof, witness *NativeExecutionStorageWitness) (*NativeExecutionStorage, error) {
	if ctx == nil {
		return nil, errors.New("native execution storage context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if proof == nil || witness == nil || witness.Schema != NativeExecutionStorageSchema || witness.FinalityHash != proof.Hash() || len(witness.Reads) == 0 || len(witness.Reads) > MaximumNativeExecutionStorageReads || len(witness.Nodes) > MaximumNativeExecutionStorageNodes {
		return nil, errors.New("native execution storage witness context or profile bound differs")
	}
	if witness.RootRole != NativeStorageRootParentExecution && witness.RootRole != NativeStorageRootChildPostState {
		return nil, errors.New("native execution storage root role is unsupported")
	}
	finality, err := VerifyNativeExecutionFinality(ctx, genesis, checkpoint, proof)
	if err != nil {
		return nil, err
	}
	block, root := finality.Parent, finality.ParentStateRoot
	if witness.RootRole == NativeStorageRootChildPostState {
		block, root = finality.Child, finality.ChildStateRoot
	}
	if witness.NativeStateBlock != block {
		return nil, errors.New("native execution storage boundary differs from exact certified header")
	}
	reads, err := verifyBoundedNativeStorageReads(ctx, root, witness.Nodes, witness.Reads, MaximumNativeExecutionStorageReads, MaximumNativeExecutionStorageNodes)
	if err != nil {
		return nil, err
	}
	return &NativeExecutionStorage{Schema: NativeExecutionStorageSchema, WitnessHash: objectDigest(witness), RootRole: witness.RootRole, NativeStateBlock: block, NativeStateRoot: root, Reads: reads, Finality: finality}, nil
}
