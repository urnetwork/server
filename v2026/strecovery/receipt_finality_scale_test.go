// Codec tests exercise wire boundaries independently of certificate fixtures,
// including canonical SCALE lengths and both reviewed Frontier hash variants.
package strecovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"
)

// Every truncation is rejected; valid compact numbers retain their exact hash.
func TestReceiptFinalityScaleAuthenticatesCompleteCanonicalNativeHeaders(t *testing.T) {
	for _, number := range []uint32{0, 63, 64, 16383, 16384, 1<<30 - 1, 1 << 30, ^uint32(0)} {
		header := finalityTestNativeHeader(number, "0x"+strings.Repeat("7", 64), finalityTestDigest("test", []byte{1, 2, 3}), []byte{8})
		budget := nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes}
		decoded, err := budget.header("0x" + hex.EncodeToString(header.raw))
		if err != nil || decoded.identity != header.identity {
			t.Fatalf("canonical header %d differs: %v", number, err)
		}
		for end := 0; end < len(header.raw); end++ {
			budget := nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes}
			if _, err := budget.header("0x" + hex.EncodeToString(header.raw[:end])); err == nil {
				t.Fatalf("truncated native header admitted at %d/%d", end, len(header.raw))
			}
		}
		budget = nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes}
		if _, err := budget.header("0x" + hex.EncodeToString(append(header.raw, 0))); err == nil {
			t.Fatal("header trailing byte admitted")
		}
	}
	for _, encoded := range [][]byte{{1, 0}, {2, 0, 0, 0}, {3, 0, 0, 0, 0}, {7, 0, 0, 0, 0, 1}} {
		reader := finalityScaleReader{data: encoded}
		if _, err := reader.compact(); err == nil {
			t.Fatalf("noncanonical or oversized compact admitted: %x", encoded)
		}
	}
	for _, digest := range [][]byte{{2}, {4, 'f', 'r', 'o', 'n', 7}, append([]byte{0}, finalityTestCompact(maximumNativeFinalityDigestBytes+1)...)} {
		header := finalityTestNativeHeader(10, "0x"+strings.Repeat("7", 64), digest)
		budget := nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes}
		if _, err := budget.header("0x" + hex.EncodeToString(header.raw)); err == nil {
			t.Fatal("malformed or oversized digest admitted")
		}
	}
}

// Both post-log formats bind the same exact EVM hash. Pre-runtime logs, zero
// hashes, unsupported variants, wrong vector lengths and duplicate logs refuse.
func TestReceiptFinalityFrontierDigestProfileAndForgeryBoundaries(t *testing.T) {
	for _, fault := range []string{"variant3", "variant1", "missing", "duplicate", "zero-hash", "pre-runtime", "block-variant", "trailing", "vector-truncated", "vector-oversized"} {
		fixture := receiptFinalityTestFixture(t)
		hash, _ := hex.DecodeString(fixture.receipts.observations.EvmFinalized.Hash[2:])
		payload := append([]byte{3}, hash...)
		if fault == "variant1" || strings.HasPrefix(fault, "vector-") {
			payload[0] = 1
			payload = append(payload, 4)
			payload = append(payload, bytes.Repeat([]byte{0x71}, 32)...)
		}
		switch fault {
		case "zero-hash":
			copy(payload[1:33], make([]byte, 32))
		case "block-variant":
			payload[0] = 2
		case "trailing":
			payload = append(payload, 0)
		case "vector-truncated":
			payload = payload[:len(payload)-1]
		case "vector-oversized":
			payload = append(payload[:33], finalityTestCompact(2049)...)
		}
		digests := [][]byte{finalityTestDigest("fron", payload)}
		if fault == "pre-runtime" {
			digests[0][0] = 6
		}
		if fault == "duplicate" {
			digests = append(digests, digests[0])
		}
		if fault == "missing" {
			digests = nil
		}
		fixture.headers[3] = finalityTestNativeHeader(703, fixture.headers[2].identity.Hash, digests...)
		fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:], fixture.keys, 9)}
		fixture.bind()
		result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
		if fault == "variant3" || fault == "variant1" {
			if err != nil || result == nil {
				t.Fatalf("valid post-log refused: %s %v", fault, err)
			}
		} else if err == nil || result != nil {
			t.Fatalf("forged Frontier mapping admitted: %s", fault)
		}
	}
}

// Zero-delay changes are finalized by outgoing keys at the signal header.
// The exact u64 weight maximum is safe, while set-ID wraparound is terminal.
func TestReceiptFinalityZeroDelayAndArithmeticBoundaries(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	newKeys, authorities := finalityTestAuthorities(61)
	fixture.headers = fixture.headers[:1]
	fixture.headers = append(fixture.headers, finalityTestNativeHeader(701, fixture.headers[0].identity.Hash, finalityTestScheduled(authorities, 0)))
	fixture.headers = append(fixture.headers, finalityTestNativeHeader(702, fixture.headers[1].identity.Hash, finalityTestFrontier(fixture.receipts.observations.EvmFinalized.Hash)))
	fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:2], fixture.keys, 9), fixture.segment(fixture.headers[2:], newKeys, 10)}
	fixture.bind()
	if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err != nil || result.AuthorityTransitions != 1 {
		t.Fatalf("zero-delay handoff failed: %v", err)
	}
	fixture.checkpoint.SetId = ^uint64(0)
	fixture.proof.Segments[0] = fixture.segment(fixture.headers[1:2], fixture.keys, ^uint64(0))
	fixture.bind()
	if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil || !strings.Contains(err.Error(), "set ID overflows") {
		t.Fatalf("set ID overflow admitted: %v", err)
	}
	fixture = receiptFinalityTestFixture(t)
	fixture.checkpoint.Authorities[0].Weight = ^uint64(0) - 6
	fixture.bind()
	if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err != nil || result.Certificates[0].TotalWeight != ^uint64(0) {
		t.Fatalf("bounded full-width quorum overflowed: %v", err)
	}
}

// A descendant signed by the outgoing set cannot cross enactment, even when
// the commit target itself is the correct transition block.
func TestReceiptFinalityRejectsOldSetVoteBeyondDelayedEnactment(t *testing.T) {
	fixture, _ := receiptFinalityRotationFixture(t)
	fixture.proof.Segments[1].JustificationScale = finalityTestCertificate(fixture.headers[3], []*finalityTestHeader{fixture.headers[3], fixture.headers[4]}, []*finalityTestHeader{fixture.headers[4]}, []int{0, 1}, fixture.keys, 17, 9)
	if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil || !strings.Contains(err.Error(), "enactment boundary") {
		t.Fatalf("old-set descendant vote crossed handoff: %v", err)
	}
}
