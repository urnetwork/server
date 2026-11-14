// The existing independently signed GRANDPA fixtures are consumed through both
// public adapters. Native-only proofs never acquire receipt or runtime authority.
package strecovery

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func nativeExecutionTestProof(f *receiptFinalityFixture, child int) *NativeExecutionFinalityProof {
	return &NativeExecutionFinalityProof{Schema: NativeExecutionFinalitySchema, CheckpointHash: f.checkpoint.Hash(), Parent: f.headers[child-1].identity, Child: f.headers[child].identity, Segments: f.proof.Segments}
}

func TestNativeExecutionFinalityMatchesReceiptKernelWithoutGrantingAuthority(t *testing.T) {
	f := receiptFinalityTestFixture(t)
	before := []string{objectDigest(f.collection), f.checkpoint.Hash(), objectDigest(f.proof)}
	native, err := VerifyNativeExecutionFinality(t.Context(), f.checkpoint.Genesis, f.checkpoint, nativeExecutionTestProof(f, 3))
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := VerifyReceiptFinality(t.Context(), f.receipts.archive, f.collection, f.checkpoint, f.proof)
	if err != nil {
		t.Fatal(err)
	}
	if native.Parent != f.headers[2].identity || native.Child != receipts.NativeFinalized || native.Certified != receipts.NativeFinalized || native.ChildStateRoot != receipts.NativeStateRoot || !reflect.DeepEqual(native.Certificates, receipts.Certificates) || native.NextCheckpoint.SetId != receipts.NextSetId || !reflect.DeepEqual(native.NextCheckpoint.Authorities, receipts.NextAuthorities) || receipts.FinalityAuthenticated || receipts.NativeEvmCommitmentVerified != true || receipts.SpendingAuthorized {
		t.Fatal("native adapter changed the original receipt proof or invented authority")
	}
	if !reflect.DeepEqual(before, []string{objectDigest(f.collection), f.checkpoint.Hash(), objectDigest(f.proof)}) {
		t.Fatal("native verifier mutated borrowed original authority")
	}
}

func TestNativeExecutionFinalityDescendantHandoffRetainsCertifiedRestart(t *testing.T) {
	f, keys := receiptFinalityRotationFixture(t)
	proof := nativeExecutionTestProof(f, 2)
	result, err := VerifyNativeExecutionFinality(t.Context(), f.checkpoint.Genesis, f.checkpoint, proof)
	if err != nil {
		t.Fatal(err)
	}
	if result.Child.Number != 702 || result.Certified.Number != 704 || result.NextCheckpoint.SetId != 10 || result.NextCheckpoint.PendingChange != nil || result.NextCheckpoint.HeaderScale != "0x"+hex.EncodeToString(f.headers[4].raw) || result.AuthorityTransitions != 1 {
		t.Fatal("later authority state was attached to an earlier selected child", result)
	}
	child := finalityTestNativeHeader(705, f.headers[4].identity.Hash)
	next := &NativeExecutionFinalityProof{Schema: NativeExecutionFinalitySchema, CheckpointHash: result.NextCheckpoint.Hash(), Parent: f.headers[4].identity, Child: child.identity, Segments: []GrandpaFinalitySegment{f.segment([]*finalityTestHeader{child}, keys, 10)}}
	resumed, err := VerifyNativeExecutionFinality(t.Context(), f.checkpoint.Genesis, &result.NextCheckpoint, next)
	if err != nil || resumed == nil || resumed.Child.Number != 705 {
		t.Fatal("actual certified authority state could not continue", err)
	}
	wrong := result.NextCheckpoint
	wrong.HeaderScale = result.ChildHeaderScale
	next.CheckpointHash = wrong.Hash()
	if value, err := VerifyNativeExecutionFinality(t.Context(), f.checkpoint.Genesis, &wrong, next); err == nil || value != nil {
		t.Fatal("later authorities admitted with an earlier selected head")
	}
}

func TestNativeExecutionFinalityRequiresParentQuorumAndExactProof(t *testing.T) {
	for _, fault := range []string{"parent", "child", "checkpoint", "votes", "signature", "missing", "count", "cancel"} {
		f := receiptFinalityTestFixture(t)
		proof := nativeExecutionTestProof(f, 3)
		ctx, cancel := context.WithCancel(t.Context())
		switch fault {
		case "parent":
			proof.Parent.Hash = "0x" + strings.Repeat("8", 64)
		case "child":
			proof.Child.Hash = "0x" + strings.Repeat("8", 64)
		case "checkpoint":
			proof.CheckpointHash = "sha256:" + strings.Repeat("8", 64)
		case "votes":
			proof.Segments[0].JustificationScale = finalityTestCertificate(f.headers[3], nil, nil, []int{1, 2, 3}, f.keys, 17, 9)
		case "signature":
			raw, _ := hex.DecodeString(proof.Segments[0].JustificationScale[2:])
			raw[81] ^= 1
			proof.Segments[0].JustificationScale = "0x" + hex.EncodeToString(raw)
		case "missing":
			proof.Segments[0].Headers = proof.Segments[0].Headers[1:]
		case "count":
			proof.Segments = make([]GrandpaFinalitySegment, maximumGrandpaCertificates+1)
		case "cancel":
			cancel()
		}
		value, err := VerifyNativeExecutionFinality(ctx, f.checkpoint.Genesis, f.checkpoint, proof)
		cancel()
		if err == nil || value != nil || fault == "cancel" && !errors.Is(err, context.Canceled) {
			t.Fatal("incomplete or contradictory native proof escaped", fault, err)
		}
		if errors.Is(err, ErrNativeFinalityConflict) != (fault != "cancel") {
			t.Fatal("pure verification confused unavailable ownership with contradictory evidence", fault, err)
		}
	}
}

func TestNativeExecutionFinalityDoesNotBypassReceiptFrontierCommitment(t *testing.T) {
	f := receiptFinalityTestFixture(t)
	f.headers[3] = finalityTestNativeHeader(703, f.headers[2].identity.Hash)
	f.proof.Segments = []GrandpaFinalitySegment{f.segment(f.headers[1:], f.keys, 9)}
	f.bind()
	if value, err := VerifyNativeExecutionFinality(t.Context(), f.checkpoint.Genesis, f.checkpoint, nativeExecutionTestProof(f, 3)); err != nil || value == nil {
		t.Fatal("native-only block required a fabricated EVM receipt mapping", err)
	}
	if value, err := VerifyReceiptFinality(t.Context(), f.receipts.archive, f.collection, f.checkpoint, f.proof); err == nil || value != nil {
		t.Fatal("native adapter disabled original receipt Frontier authority")
	}
}

func TestNativeExecutionStoragePreservesFullUidAndOriginalReceiptProfiles(t *testing.T) {
	vector := nativeStorageCase(t, "layout1-empty-trie")
	f, _ := nativeStorageFinalityFixture(t, vector)
	proof := nativeExecutionTestProof(f, 3)
	reads := make([]NativeStorageRead, MaximumNativeExecutionStorageReads)
	for index := range reads {
		reads[index] = NativeStorageRead{Key: fmt.Sprintf("0x%08x", index)}
	}
	witness := &NativeExecutionStorageWitness{Schema: NativeExecutionStorageSchema, FinalityHash: proof.Hash(), RootRole: NativeStorageRootChildPostState, NativeStateBlock: proof.Child, Nodes: vector.Nodes, Reads: reads}
	result, err := VerifyNativeExecutionStorage(t.Context(), f.checkpoint.Genesis, f.checkpoint, proof, witness)
	if err != nil || result == nil || len(result.Reads) != 4*4096+4 {
		t.Fatal("accepted UID profile was silently narrowed", err)
	}
	if _, err := verifyNativeStorageReads(t.Context(), vector.Root, vector.Nodes, reads[:33]); err == nil {
		t.Fatal("old receipt32-read bound changed")
	}
	witness.Reads = append(reads, NativeStorageRead{Key: "0xffffffffff"})
	if value, err := VerifyNativeExecutionStorage(t.Context(), f.checkpoint.Genesis, f.checkpoint, proof, witness); err == nil || value != nil {
		t.Fatal("native read-count overflow returned partial evidence")
	}
	witness.Reads = reads
	empty := "0x"
	witness.Reads[0].Value = &empty
	if value, err := VerifyNativeExecutionStorage(t.Context(), f.checkpoint.Genesis, f.checkpoint, proof, witness); err == nil || value != nil {
		t.Fatal("proven absence became present empty storage")
	}
}

func TestNativeExecutionStorageRefusesUnboundRootAndIncompleteWitness(t *testing.T) {
	vector := nativeStorageCase(t, "layout1-empty-trie")
	f, _ := nativeStorageFinalityFixture(t, vector)
	proof := nativeExecutionTestProof(f, 3)
	for _, fault := range []string{"role", "boundary", "finality", "duplicate", "cancel"} {
		witness := &NativeExecutionStorageWitness{Schema: NativeExecutionStorageSchema, FinalityHash: proof.Hash(), RootRole: NativeStorageRootChildPostState, NativeStateBlock: proof.Child, Nodes: vector.Nodes, Reads: vector.Reads}
		ctx, cancel := context.WithCancel(t.Context())
		switch fault {
		case "role":
			witness.RootRole = "caller_supplied_root"
		case "boundary":
			witness.NativeStateBlock = proof.Parent
		case "finality":
			witness.FinalityHash = "sha256:" + strings.Repeat("8", 64)
		case "duplicate":
			witness.Reads = []NativeStorageRead{vector.Reads[0], vector.Reads[0]}
		case "cancel":
			cancel()
		}
		value, err := VerifyNativeExecutionStorage(ctx, f.checkpoint.Genesis, f.checkpoint, proof, witness)
		cancel()
		if err == nil || value != nil {
			t.Fatal("unbound native read became a provider fact", fault)
		}
	}
}
