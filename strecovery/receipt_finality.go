// Offline native finality proofs join GRANDPA certificates and the native
// Frontier digest to an exact receipt collection. The initial checkpoint and
// runtime debit semantics remain independent, explicitly missing authorities.
package strecovery

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

const NativeFinalityCheckpointSchema = "urnetwork-native-finality-checkpoint-v1"
const ReceiptFinalityProofSchema = "urnetwork-operator-receipt-finality-proof-v1"
const ReceiptFinalityReconciliationSchema = "urnetwork-operator-receipt-finality-reconciliation-v1"
const NativeFinalityCodecProfile = "subtensor-u32-blake2-grandpa-ed25519-scheduled-v1"
const NativeFinalityRuntimeSource = "67dcf7f791dc495064c293f080a0702cb433e51e"
const NativeFinalitySdkSource = "cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a"
const MaximumReceiptFinalityBytes = 16 * 1024 * 1024
const MaximumNativeCheckpointBytes = 1024 * 1024
const maximumGrandpaCertificates = 64

// Weight is the consensus voting weight, not a count of keys. Public keys are
// Ed25519 application keys, never account addresses or private signing seeds.
type GrandpaAuthority struct {
	PublicKey string `json:"public_key"`
	Weight    uint64 `json:"weight"`
}

// A checkpoint may retain a previously signaled, not-yet-enacted change.
// ScheduledAt and EnactmentNumber are native u32 heights, not EVM heights.
type GrandpaScheduledChange struct {
	ScheduledAt     uint64             `json:"scheduled_at"`
	EnactmentNumber uint64             `json:"enactment_number"`
	Authorities     []GrandpaAuthority `json:"authorities"`
}

// The complete post-finalization authority state is an independent trust input.
// LiveState must be "live" with no pending force/pause/resume/disabled state.
// A file pin or successful certificate never approves this state or genesis.
type NativeFinalityCheckpoint struct {
	Schema        string                  `json:"schema"`
	CodecProfile  string                  `json:"codec_profile"`
	Genesis       string                  `json:"genesis"`
	HeaderScale   string                  `json:"header_scale"`
	SetId         uint64                  `json:"set_id"`
	Authorities   []GrandpaAuthority      `json:"authorities"`
	LiveState     string                  `json:"live_state"`
	PendingChange *GrandpaScheduledChange `json:"pending_change"`
}

// This content digest binds the decoded checkpoint for proof association. It
// is separate from a file-byte SHA pin and from independent approval of either.
func (self *NativeFinalityCheckpoint) Hash() string { return objectDigest(self) }

// Headers are ascending, consecutive descendants of the previous certificate
// (or checkpoint); the last is the exact commit target. A handoff segment ends
// at enactment, certified by the outgoing set. No boundary is supplied as JSON.
type GrandpaFinalitySegment struct {
	Headers            []string `json:"headers"`
	JustificationScale string   `json:"justification_scale"`
}

// Both independent input identities are bound before cryptographic work. The
// collection's old external mapping label is retained but never trusted here.
type ReceiptFinalityProof struct {
	Schema         string                   `json:"schema"`
	CollectionHash string                   `json:"collection_hash"`
	CheckpointHash string                   `json:"checkpoint_hash"`
	Segments       []GrandpaFinalitySegment `json:"segments"`
}

// These are proof facts relative to the pinned checkpoint, not chain admission.
// The unchanged receipt report preserves exact histories and null actual fees.
type ReceiptFinalityReconciliation struct {
	Schema                           string                           `json:"schema"`
	Admission                        string                           `json:"admission"`
	CodecProfile                     string                           `json:"codec_profile"`
	CodecRuntimeSource               string                           `json:"codec_runtime_source"`
	CodecSdkSource                   string                           `json:"codec_sdk_source"`
	CheckpointHash                   string                           `json:"checkpoint_hash"`
	ProofHash                        string                           `json:"proof_hash"`
	CollectionHash                   string                           `json:"collection_hash"`
	NativeCheckpoint                 ObservedBlockIdentity            `json:"native_checkpoint"`
	NativeFinalized                  ObservedBlockIdentity            `json:"native_finalized"`
	NativeStateRoot                  string                           `json:"native_state_root"`
	EvmFinalized                     ObservedBlockIdentity            `json:"evm_finalized"`
	FrontierPostLogVariant           uint8                            `json:"frontier_post_log_variant"`
	Certificates                     []GrandpaCertificateResult       `json:"certificates"`
	AuthorityTransitions             int                              `json:"authority_transitions"`
	NextSetId                        uint64                           `json:"next_set_id"`
	NextAuthorities                  []GrandpaAuthority               `json:"next_authorities"`
	PendingChange                    *GrandpaScheduledChange          `json:"pending_change"`
	GrandpaCertificatesVerified      bool                             `json:"grandpa_certificates_verified"`
	NativeHeaderAncestryVerified     bool                             `json:"native_header_ancestry_verified"`
	NativeEvmCommitmentVerified      bool                             `json:"native_evm_commitment_verified"`
	AuthorityCheckpointAuthenticated bool                             `json:"authority_checkpoint_authenticated"`
	GenesisAuthenticated             bool                             `json:"genesis_authenticated"`
	RuntimeSourceAuthenticated       bool                             `json:"runtime_source_authenticated"`
	FinalityAuthenticated            bool                             `json:"finality_authenticated"`
	CanonicalReceiptsReconciled      bool                             `json:"canonical_receipts_reconciled"`
	ActualFeesReconciled             bool                             `json:"actual_fees_reconciled"`
	SpendingAuthorized               bool                             `json:"spending_authorized"`
	MissingAuthorities               []string                         `json:"missing_authorities"`
	Receipts                         *ReceiptCommitmentReconciliation `json:"receipt_commitments"`
}

// Strict private loading rejects duplicate/unknown fields, shared files and
// byte substitution. Approval is deliberately absent from this input schema.
func LoadNativeFinalityCheckpoint(ctx context.Context, reference FileReference) (*NativeFinalityCheckpoint, error) {
	if ctx == nil || !canonicalDigest(reference.Sha256) {
		return nil, errors.New("native finality checkpoint requires a context and exact byte pin")
	}
	raw, err := readPrivateFile(ctx, reference.Path, MaximumNativeCheckpointBytes)
	if err != nil {
		return nil, err
	}
	if digest(raw) != reference.Sha256 {
		return nil, errors.New("native finality checkpoint differs from its byte pin")
	}
	var checkpoint NativeFinalityCheckpoint
	if err := decodeJson(raw, &checkpoint); err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

// Proof bytes are archival input; loading creates no network or signing port.
func LoadReceiptFinalityProof(ctx context.Context, reference FileReference) (*ReceiptFinalityProof, error) {
	if ctx == nil || !canonicalDigest(reference.Sha256) {
		return nil, errors.New("native finality proof requires a context and exact byte pin")
	}
	raw, err := readPrivateFile(ctx, reference.Path, MaximumReceiptFinalityBytes)
	if err != nil {
		return nil, err
	}
	if digest(raw) != reference.Sha256 {
		return nil, errors.New("native finality proof differs from its byte pin")
	}
	var proof ReceiptFinalityProof
	if err := decodeJson(raw, &proof); err != nil {
		return nil, err
	}
	return &proof, nil
}

// A rolling checkpoint includes active and pending authority state. The
// verifier checks internal consistency; independently establishing that state
// (including absence of other pending signals) remains an explicit requirement.
func (self *NativeFinalityCheckpoint) validate(archive *Archive, budget *nativeFinalityBudget) (*nativeFinalityHeader, error) {
	if self == nil || self.Schema != NativeFinalityCheckpointSchema || self.CodecProfile != NativeFinalityCodecProfile ||
		self.Genesis != archive.Selection.Genesis || !canonicalHex(self.Genesis, 32) || self.LiveState != "live" {
		return nil, errors.New("native finality checkpoint schema, genesis, codec or live state differs")
	}
	if _, _, err := grandpaAuthorityWeights(self.Authorities); err != nil {
		return nil, err
	}
	header, err := budget.header(self.HeaderScale)
	if err != nil {
		return nil, err
	}
	if header.identity.Number == 0 && header.identity.Hash != self.Genesis {
		return nil, errors.New("native finality genesis checkpoint hash differs")
	}
	pending := self.PendingChange
	if pending != nil {
		if pending.ScheduledAt > header.identity.Number || pending.EnactmentNumber <= header.identity.Number || pending.EnactmentNumber > uint64(^uint32(0)) {
			return nil, errors.New("native finality checkpoint pending change is outside the future enactment interval")
		}
		if _, _, err := grandpaAuthorityWeights(pending.Authorities); err != nil {
			return nil, err
		}
	}
	change, err := header.scheduledChange()
	if err != nil {
		return nil, err
	}
	if change != nil {
		if change.EnactmentNumber == header.identity.Number {
			if self.SetId == 0 || pending != nil || !slices.Equal(change.Authorities, self.Authorities) {
				return nil, errors.New("native finality checkpoint does not describe its post-enactment authority state")
			}
		} else if pending == nil || objectDigest(change) != objectDigest(pending) {
			return nil, errors.New("native finality checkpoint omits or changes its header's pending authority signal")
		}
	}
	return header, nil
}

// Inputs are borrowed immutable values. All headers, certificates and weighted
// sets are bounded per call; cancellation returns no partial report. Existing
// receipt verification is mandatory, including original signed transaction and
// receipt trie proofs. A valid signature never authorizes its own trust anchor.
func VerifyReceiptFinality(ctx context.Context, archive *Archive, collection *ReceiptCollection, checkpoint *NativeFinalityCheckpoint, proof *ReceiptFinalityProof) (*ReceiptFinalityReconciliation, error) {
	if ctx == nil {
		return nil, errors.New("native finality verification context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	receipts, err := VerifyReceiptCollection(ctx, archive, collection)
	if err != nil {
		return nil, err
	}
	budget := &nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes}
	anchor, err := checkpoint.validate(archive, budget)
	if err != nil {
		return nil, err
	}
	if proof == nil || proof.Schema != ReceiptFinalityProofSchema || proof.CheckpointHash != checkpoint.Hash() || proof.CollectionHash != collection.ContentHash || len(proof.Segments) == 0 || len(proof.Segments) > maximumGrandpaCertificates {
		return nil, errors.New("native finality proof context or certificate count differs")
	}
	boundary := collection.Observations.NativeFinalized
	if boundary.Number <= anchor.identity.Number || boundary.Number-anchor.identity.Number >= maximumNativeFinalityHeaders {
		return nil, errors.New("native finality selected boundary is outside the rolling checkpoint interval")
	}
	cursor := anchor
	setId, authorities, pending := checkpoint.SetId, checkpoint.Authorities, checkpoint.PendingChange
	certificates := make([]GrandpaCertificateResult, 0, len(proof.Segments))
	transitions := 0
	for index, segment := range proof.Segments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(segment.Headers) == 0 || len(segment.Headers) > maximumNativeFinalityHeaders-budget.headers {
			return nil, errors.New("native finality segment header count differs")
		}
		for _, encoded := range segment.Headers {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			header, err := budget.header(encoded)
			if err != nil {
				return nil, err
			}
			if header.identity.Number != cursor.identity.Number+1 || header.parent != cursor.identity.Hash || header.identity.Number > boundary.Number {
				return nil, errors.New("native finality header ancestry is disconnected or outside the collection boundary")
			}
			if pending != nil && header.identity.Number > pending.EnactmentNumber {
				return nil, errors.New("native finality segment skips the outgoing authority enactment certificate")
			}
			change, err := header.scheduledChange()
			if err != nil {
				return nil, err
			}
			if change != nil {
				if pending != nil {
					return nil, errors.New("native finality scheduled authority changes overlap")
				}
				pending = change
			}
			cursor = header
		}
		justification, err := budget.justification(segment.JustificationScale)
		if err != nil {
			return nil, err
		}
		certificate, err := verifyGrandpaCertificate(ctx, justification, cursor, setId, authorities, pending)
		if err != nil {
			return nil, fmt.Errorf("native finality segment %d: %w", index, err)
		}
		certificates = append(certificates, certificate)
		if pending != nil && cursor.identity.Number == pending.EnactmentNumber {
			if setId == ^uint64(0) {
				return nil, errors.New("native finality authority set ID overflows")
			}
			setId++
			authorities, pending = pending.Authorities, nil
			transitions++
		}
	}
	if cursor.identity != boundary {
		return nil, errors.New("native finality certificate chain does not end at the exact collection boundary")
	}
	evmHash, variant, err := cursor.frontierHash()
	if err != nil {
		return nil, err
	}
	// VerifyReceiptCollection already hashes the final exact RLP15 header and
	// checks its own EVM number. Native numbers never select an EVM candidate.
	if evmHash != collection.Observations.EvmFinalized.Hash {
		return nil, errors.New("native finality Frontier commitment differs from the collection EVM boundary")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var nextPending *GrandpaScheduledChange
	if pending != nil {
		copy := *pending
		copy.Authorities = slices.Clone(pending.Authorities)
		nextPending = &copy
	}
	return &ReceiptFinalityReconciliation{Schema: ReceiptFinalityReconciliationSchema, Admission: "unapproved_checkpoint_proof", CodecProfile: NativeFinalityCodecProfile,
		CodecRuntimeSource: NativeFinalityRuntimeSource, CodecSdkSource: NativeFinalitySdkSource, CheckpointHash: checkpoint.Hash(), ProofHash: objectDigest(proof), CollectionHash: collection.ContentHash,
		NativeCheckpoint: anchor.identity, NativeFinalized: cursor.identity, NativeStateRoot: cursor.stateRoot, EvmFinalized: collection.Observations.EvmFinalized, FrontierPostLogVariant: variant,
		Certificates: certificates, AuthorityTransitions: transitions, NextSetId: setId, NextAuthorities: slices.Clone(authorities), PendingChange: nextPending,
		GrandpaCertificatesVerified: true, NativeHeaderAncestryVerified: true, NativeEvmCommitmentVerified: true, Receipts: receipts,
		MissingAuthorities: []string{"independent genesis and initial checkpoint approval, including active set ID, weighted keys, live voter state and any pending transition", "deployed runtime/code and Frontier digest semantics admission for the verified interval", "authenticated native runtime transaction debit/refund evidence; actual fees remain null", "account nonce state proofs, service adoption and live custody/restart qualification; no recovery spend is authorized"}}, nil
}
