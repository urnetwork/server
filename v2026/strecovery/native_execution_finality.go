// Native execution finality uses the same weighted GRANDPA verifier as receipt
// recovery. No receipt collection or Frontier mapping is invented for a native
// block. The initial checkpoint, runtime and provider policy need independent
// approval; a successful proof never grants its own anchor authority.
package strecovery

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/urnetwork/server/v2026"
)

const NativeExecutionFinalitySchema = "urnetwork-native-execution-finality-v1"

// Only complete decoded evidence or a pure cryptographic verification may add
// this marker. Missing bytes, failed reads and canceled ownership cannot.
var ErrNativeFinalityConflict = errors.New("native finality evidence conflicts")

// Verification is pure except for cancellation. Walk every cause so a joined
// contradiction cannot disappear merely because cancellation is also present.
func nativeFinalityVerificationError(err error) error {
	if err == nil {
		return err
	}
	// An incomplete cause graph cannot invent a contradiction. A concrete
	// verifier refusal already observed beside it still retains precedence.
	for _, node := range server.InspectErrorCauses(err).Nodes {
		if node.Leaf && node.Err != context.Canceled && node.Err != context.DeadlineExceeded {
			return errors.Join(ErrNativeFinalityConflict, err)
		}
	}
	return err
}

func nativeFinalityCancellationOnly(err error) bool {
	inspection := server.InspectErrorCauses(err)
	if !inspection.Complete {
		return false
	}
	seen := false
	for _, node := range inspection.Nodes {
		if node.Leaf {
			seen = true
			if node.Err != context.Canceled && node.Err != context.DeadlineExceeded {
				return false
			}
		}
	}
	return seen
}

// Decode the bounded independently approved anchor without granting it trust.
// Its caller must authenticate the complete checkpoint in a separate policy.
func NativeExecutionCheckpointIdentity(ctx context.Context, genesis string, checkpoint *NativeFinalityCheckpoint) (result ObservedBlockIdentity, resultErr error) {
	defer func() { resultErr = nativeFinalityVerificationError(resultErr) }()
	if ctx == nil {
		return ObservedBlockIdentity{}, errors.New("native execution checkpoint context is absent")
	}
	if err := ctx.Err(); err != nil {
		return ObservedBlockIdentity{}, err
	}
	header, err := checkpoint.validateGenesis(genesis, &nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes})
	if err != nil {
		return ObservedBlockIdentity{}, err
	}
	return header.identity, nil
}

// The boundary is the executed child. A later certificate may prove it by exact
// ancestry, while parent and child remain separately bound to the replay job.
type NativeExecutionFinalityProof struct {
	Schema         string                   `json:"schema"`
	CheckpointHash string                   `json:"checkpoint_hash"`
	Parent         ObservedBlockIdentity    `json:"parent"`
	Child          ObservedBlockIdentity    `json:"child"`
	Segments       []GrandpaFinalitySegment `json:"segments"`
}

// The result is relative to the caller's independently pinned checkpoint. The
// returned authority state belongs to Certified, possibly later than Child;
// callers cannot use that later checkpoint to skip unobserved economic blocks.
type NativeExecutionFinality struct {
	Schema               string                     `json:"schema"`
	CheckpointHash       string                     `json:"checkpoint_hash"`
	ProofHash            string                     `json:"proof_hash"`
	Parent               ObservedBlockIdentity      `json:"parent"`
	Child                ObservedBlockIdentity      `json:"child"`
	Certified            ObservedBlockIdentity      `json:"certified"`
	ParentHeaderScale    string                     `json:"parent_header_scale"`
	ChildHeaderScale     string                     `json:"child_header_scale"`
	ParentStateRoot      string                     `json:"parent_state_root"`
	ChildStateRoot       string                     `json:"child_state_root"`
	Certificates         []GrandpaCertificateResult `json:"certificates"`
	AuthorityTransitions int                        `json:"authority_transitions"`
	NextCheckpoint       NativeFinalityCheckpoint   `json:"next_checkpoint"`
}

// All output is owned and conditional on the original checkpoint; no partial
// verification escapes on cancellation, ancestry gaps or unsupported handoffs.
func VerifyNativeExecutionFinality(ctx context.Context, genesis string, checkpoint *NativeFinalityCheckpoint, proof *NativeExecutionFinalityProof) (result *NativeExecutionFinality, resultErr error) {
	defer func() { resultErr = nativeFinalityVerificationError(resultErr) }()
	if ctx == nil {
		return nil, errors.New("native execution finality context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if checkpoint == nil || proof == nil || proof.Schema != NativeExecutionFinalitySchema || proof.CheckpointHash != checkpoint.Hash() || proof.Parent.Number >= uint64(^uint32(0)) || proof.Parent.Number+1 != proof.Child.Number || !canonicalHex(proof.Parent.Hash, 32) || !canonicalHex(proof.Child.Hash, 32) {
		return nil, errors.New("native execution finality boundary or original checkpoint differs")
	}
	window, err := verifyNativeFinalityWindow(ctx, genesis, checkpoint, proof.Child, proof.Segments, true)
	if err != nil {
		return nil, err
	}
	if window.parent == nil || window.parent.identity != proof.Parent {
		return nil, errors.New("native execution finality parent differs from certified ancestry")
	}
	var pending *GrandpaScheduledChange
	if window.pending != nil {
		value := *window.pending
		value.Authorities = slices.Clone(value.Authorities)
		pending = &value
	}
	return &NativeExecutionFinality{Schema: NativeExecutionFinalitySchema, CheckpointHash: checkpoint.Hash(), ProofHash: objectDigest(proof), Parent: window.parent.identity, Child: window.boundary.identity, Certified: window.tip.identity, ParentHeaderScale: window.parentScale, ChildHeaderScale: window.boundaryScale, ParentStateRoot: window.parent.stateRoot, ChildStateRoot: window.boundary.stateRoot, Certificates: window.certificates, AuthorityTransitions: window.transitions,
		NextCheckpoint: NativeFinalityCheckpoint{Schema: NativeFinalityCheckpointSchema, CodecProfile: checkpoint.CodecProfile, Genesis: genesis, HeaderScale: window.tipScale, SetId: window.setId, Authorities: slices.Clone(window.authorities), LiveState: "live", PendingChange: pending}}, nil
}

type nativeFinalityWindow struct {
	anchor, parent, boundary, tip        *nativeFinalityHeader
	parentScale, boundaryScale, tipScale string
	setId                                uint64
	authorities                          []GrandpaAuthority
	pending                              *GrandpaScheduledChange
	certificates                         []GrandpaCertificateResult
	transitions                          int
}

// One invocation shares the original finite encoded-byte/header/certificate
// budget across the complete ancestry and every justification, including votes.
func verifyNativeFinalityWindow(ctx context.Context, genesis string, checkpoint *NativeFinalityCheckpoint, boundary ObservedBlockIdentity, segments []GrandpaFinalitySegment, descendants bool) (*nativeFinalityWindow, error) {
	if ctx == nil || !canonicalHex(genesis, 32) || !canonicalHex(boundary.Hash, 32) || len(segments) == 0 || len(segments) > maximumGrandpaCertificates {
		return nil, errors.New("native finality context, boundary or certificate count differs")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	budget := &nativeFinalityBudget{remaining: MaximumReceiptFinalityBytes}
	anchor, err := checkpoint.validateGenesis(genesis, budget)
	if err != nil {
		return nil, err
	}
	if boundary.Number <= anchor.identity.Number || boundary.Number-anchor.identity.Number >= maximumNativeFinalityHeaders {
		return nil, errors.New("native finality selected boundary is outside the rolling checkpoint interval")
	}
	cursor := anchor
	cursorScale := checkpoint.HeaderScale
	var parent *nativeFinalityHeader
	var parentScale, boundaryScale string
	var boundaryHeader *nativeFinalityHeader
	setId, authorities, pending := checkpoint.SetId, checkpoint.Authorities, checkpoint.PendingChange
	certificates := make([]GrandpaCertificateResult, 0, len(segments))
	transitions := 0
	for index, segment := range segments {
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
			if header.identity.Number != cursor.identity.Number+1 || header.parent != cursor.identity.Hash ||
				header.identity.Number-anchor.identity.Number >= maximumNativeFinalityHeaders ||
				!descendants && header.identity.Number > boundary.Number {
				return nil, errors.New("native finality header ancestry is disconnected or outside the collection boundary")
			}
			if header.identity.Number == boundary.Number {
				if header.identity != boundary {
					return nil, errors.New("native finality certified ancestry differs from the exact collection boundary")
				}
				boundaryHeader = header
				parent, parentScale, boundaryScale = cursor, cursorScale, encoded
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
			cursor, cursorScale = header, encoded
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
	if boundaryHeader == nil || !descendants && cursor.identity != boundary {
		return nil, errors.New("native finality certificate chain does not cover the exact collection boundary")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &nativeFinalityWindow{anchor: anchor, parent: parent, boundary: boundaryHeader, tip: cursor, parentScale: parentScale, boundaryScale: boundaryScale, tipScale: cursorScale, setId: setId, authorities: authorities, pending: pending, certificates: certificates, transitions: transitions}, nil
}
