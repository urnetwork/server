// Resource preflight is a prospective refusal boundary, not an allocation or
// deletion mechanism. The native owner must reserve actual storage separately.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// Original wrapper attempts and member-state copies are always counted. These
// additional copies describe the actual native protected-custody implementation.
type winnerMultipassCapacitySettings struct {
	ProtectedCopies     int   `json:"protected_copies"`      // Positive extra durable copies of captured originals; default one.
	TemporaryCopies     int   `json:"temporary_copies"`      // Positive simultaneous protected-write/readback staging copies; default one.
	MaximumDurableBytes int64 `json:"maximum_durable_bytes"` // Positive prospectively reserved bytes; default 8 GiB, never proof of free disk.
	MaximumMemoryBytes  int64 `json:"maximum_memory_bytes"`  // Positive native process allowance; default and hard ceiling 8 GiB.
}

// Defaults require the caller to prove real capacity, not trust this number.
func defaultWinnerMultipassCapacitySettings() winnerMultipassCapacitySettings {
	return winnerMultipassCapacitySettings{ProtectedCopies: 1, TemporaryCopies: 1, MaximumDurableBytes: 8 * 1024 * 1024 * 1024, MaximumMemoryBytes: 8 * 1024 * 1024 * 1024}
}

// Pin this complete receipt alongside the owner before its first generation
// claim. It is deliberately separate from actual filesystem reservation proof.
type winnerMultipassCapacity struct {
	Version                   string                          `json:"version"`
	PlanSha256                string                          `json:"plan_sha256"`
	BundleSha256              string                          `json:"bundle_sha256"`
	Settings                  winnerMultipassCapacitySettings `json:"settings"`
	GenerationCalls           int                             `json:"generation_calls"`
	TokenizerInvocations      int                             `json:"tokenizer_invocations"`
	WrapperOriginalBytes      int64                           `json:"wrapper_original_bytes"`
	ProtectedCopyBytes        int64                           `json:"protected_copy_bytes"`
	TemporaryBytes            int64                           `json:"temporary_bytes"`
	CoreEvidenceBytes         int64                           `json:"core_evidence_bytes"`
	SerializedEvidenceBytes   int64                           `json:"serialized_evidence_bytes"`
	RequiredDurableBytes      int64                           `json:"required_durable_bytes"`
	InvocationAllowanceBytes  int64                           `json:"invocation_allowance_bytes"`
	DocumentAndTemporaryBytes int64                           `json:"document_and_temporary_bytes"`
	RequiredMemoryBytes       int64                           `json:"required_memory_bytes"`
}

// Every named artifact has a prospective bound; the new wrapper must authenticate
// the complete leaf census and enforce these independently of provider success.
func winnerMultipassJournalLeafLimits(operation string, settings winnerMultipassSettings) (map[string]int, error) {
	if err := validateWinnerMultipassSettings(settings); err != nil {
		return nil, err
	}
	limits := defaultWinnerMultipassProviderLimits()
	leaves := map[string]int{"call.started.json": limits.MaximumEnvelopeBytes, "call.complete.json": limits.MaximumResultBytes}
	switch operation {
	case "tokenize":
		leaves["tokenize.request.json"] = settings.MaximumRequestBytes
		leaves["tokenize.response.json"] = limits.MaximumTokenizerBytes
	case "generate":
		leaves["transport.response"] = settings.MaximumResponseBytes
	default:
		return nil, errors.New("unknown contained operation has no custody census")
	}
	return leaves, nil
}

// JSON []byte fields encode at exactly this upper bound; +2 accounts for quotes.
func winnerMultipassEncodedBytes(size int64) int64 { return 4*((size+2)/3) + 2 }

// Metadata is known before inference. Fill all admission digests and numeric
// fields at conservative serialized lengths, rather than guessing an 8-MiB
// envelope for a small exact scan payload. Synthesis requests retain their cap.
func winnerMultipassEnvelopeCapacity(plan *winnerMultipassPlan, call winnerMultipassCall, payloadBytes int) (int64, error) {
	digest := strings.Repeat("0", 64)
	profile := plan.Settings.TokenizationProfiles[call.Member.Name]
	admission := winnerMultipassAdmission{CallId: call.Id, RequestSha256: digest, Model: call.Member.Model, Revision: call.Member.Revision,
		TokenizerContractSha256: digest, RenderedPromptSha256: digest, TokenizerRequestSha256: digest, TokenizerResponseSha256: digest,
		MaximumContextTokens: profile.MaximumContextTokens, InputTokens: int(^uint(0) >> 1), OutputTokens: plan.Settings.MaximumOutputTokens, NoTruncation: true}
	envelope := winnerMultipassBrokerEnvelope{Schema: 1, Kind: "winner-review-multipass-call/v1", Operation: "generate", Owner: plan.Owner,
		CallDeadline: time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC), PlanSha256: digest, BundleSha256: digest, Call: call,
		Payload: []byte{}, Settings: plan.Settings, Admission: &admission, AuthorizedClaimSha256: digest}
	content, err := json.Marshal(envelope)
	if err != nil {
		return 0, err
	}
	bound := int64(len(content)) + winnerMultipassEncodedBytes(int64(payloadBytes))
	if bound > int64(defaultWinnerMultipassProviderLimits().MaximumEnvelopeBytes) {
		return 0, errors.New("planned envelope cannot fit the separately qualified stdin profile")
	}
	return bound, nil
}

// One shared formula serves both aggregate reservation and per-operation release
// of an owned allocation. It never deletes an original transcript or authorizes
// an extra call. Original state and its captured copy are separate retained bytes.
func winnerMultipassInvocationStorage(envelope int64, operation string, settings winnerMultipassSettings) (original, capture int64, err error) {
	limits := defaultWinnerMultipassProviderLimits()
	metadata := int64(32*limits.MaximumMetadataBytes + 32*4096)
	stdout := int64(limits.MaximumResultBytes)
	state := envelope + stdout
	switch operation {
	case "generate":
		state += int64(settings.MaximumResponseBytes)
	case "tokenize":
		state += int64(settings.MaximumRequestBytes + limits.MaximumTokenizerBytes)
	default:
		return 0, 0, errors.New("unknown operation has no reserved capacity")
	}
	capture = envelope + stdout + metadata + state
	return capture + state, capture, nil
}

// The host may consume at most this allowance before the exact planned operation
// writes. Synthesis uses its actual payload here, never more than its predeclared
// MaximumRequestBytes allowance. The caller must separately enforce invocation
// census and retain the document/temporary residual until protected completion.
func winnerMultipassInvocationCapacity(plan *winnerMultipassPlan, call winnerMultipassCall, operation string, exactPayloadBytes int, settings winnerMultipassCapacitySettings) (int64, error) {
	if plan == nil || validateWinnerMultipassSettings(plan.Settings) != nil || exactPayloadBytes <= 0 || exactPayloadBytes > plan.Settings.MaximumRequestBytes || settings.ProtectedCopies <= 0 || settings.ProtectedCopies > 4 {
		return 0, errors.New("invalid planned invocation capacity")
	}
	found := false
	for _, entry := range plan.Calls {
		if reflect.DeepEqual(entry, call) {
			found = true
			break
		}
	}
	if !found {
		return 0, errors.New("unplanned call cannot consume a reserved allocation")
	}
	envelope, err := winnerMultipassEnvelopeCapacity(plan, call, exactPayloadBytes)
	if err != nil {
		return 0, err
	}
	original, capture, err := winnerMultipassInvocationStorage(envelope, operation, plan.Settings)
	if err != nil {
		return 0, err
	}
	return original + int64(settings.ProtectedCopies)*capture, nil
}

// This calculator performs no I/O. TokenizerInvocations is the explicit future
// census: include final-leaf preflight, synthesis admission and any planned
// re-admission, but never silently spend additional probes under this receipt.
// Previous preparation probes already occupy disk and reduce the host's actual
// free-space reservation; their originals remain immutable separate evidence.
func estimateWinnerMultipassCapacity(plan *winnerMultipassPlan, bundle winnerMultipassBundle, tokenizerInvocations int, settings winnerMultipassCapacitySettings) (*winnerMultipassCapacity, error) {
	if err := validateWinnerMultipassPlan(plan, bundle); err != nil {
		return nil, err
	}
	if settings.ProtectedCopies <= 0 || settings.ProtectedCopies > 4 || settings.TemporaryCopies <= 0 || settings.TemporaryCopies > 4 ||
		settings.MaximumDurableBytes <= 0 || settings.MaximumMemoryBytes <= 0 || settings.MaximumMemoryBytes > defaultWinnerMultipassCapacitySettings().MaximumMemoryBytes ||
		tokenizerInvocations < len(plan.Calls) || tokenizerInvocations > 4*len(plan.Units)+3*len(plan.Calls) {
		return nil, errors.New("capacity preflight lacks bounded explicit copies, tokenizer census or native memory budget")
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		return nil, err
	}
	result := &winnerMultipassCapacity{Version: winnerMultipassVersion, PlanSha256: winnerMultipassSha256(planBytes), BundleSha256: plan.BundleSha256,
		Settings: settings, GenerationCalls: len(plan.Calls), TokenizerInvocations: tokenizerInvocations}
	limits := defaultWinnerMultipassProviderLimits()
	raw := int64(plan.Settings.MaximumResponseBytes)
	tokenReply := int64(limits.MaximumTokenizerBytes)
	visible := int64(limits.MaximumVisibleTerminalBytes)
	observation := int64(plan.Settings.MaximumObservationBytes)
	captureBytes, maxEnvelope := int64(0), int64(0)
	for _, call := range plan.Calls {
		requestBytes := plan.Settings.MaximumRequestBytes
		if call.Stage == "scan" {
			payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
			if err != nil {
				return nil, err
			}
			requestBytes = len(payload)
		}
		envelope, err := winnerMultipassEnvelopeCapacity(plan, call, requestBytes)
		if err != nil {
			return nil, err
		}
		maxEnvelope = max(maxEnvelope, envelope)
		generationOriginal, generationCapture, err := winnerMultipassInvocationStorage(envelope, "generate", plan.Settings)
		if err != nil {
			return nil, err
		}
		tokenOriginal, tokenCapture, err := winnerMultipassInvocationStorage(envelope, "tokenize", plan.Settings)
		if err != nil {
			return nil, err
		}
		result.WrapperOriginalBytes += generationOriginal + tokenOriginal
		captureBytes += generationCapture + tokenCapture
		// ProviderOutput and decoded Observation are independent in-memory copies;
		// JSON text may expand each byte sixfold, unlike []byte base64 fields.
		result.CoreEvidenceBytes += int64(requestBytes) + visible + 2*observation + 16384
		result.SerializedEvidenceBytes += winnerMultipassEncodedBytes(int64(requestBytes)) + winnerMultipassEncodedBytes(visible) + 7*observation + 16384
	}
	extra := int64(tokenizerInvocations - len(plan.Calls))
	tokenOriginal, tokenCapture, err := winnerMultipassInvocationStorage(maxEnvelope, "tokenize", plan.Settings)
	if err != nil {
		return nil, err
	}
	result.WrapperOriginalBytes += extra * tokenOriginal
	captureBytes += extra * tokenCapture
	// Source/plan originals, native evidence and small immutable custody manifests
	// are retained in addition to the captured wrapper leaves, never instead.
	nativeDocuments := int64(len(planBytes)+len(bundleBytes)) + result.SerializedEvidenceBytes + 1024*1024
	result.ProtectedCopyBytes = int64(settings.ProtectedCopies) * (captureBytes + nativeDocuments)
	result.TemporaryBytes = int64(settings.TemporaryCopies) * (nativeDocuments + max(raw, max(tokenReply, int64(limits.MaximumEnvelopeBytes))))
	fixedReserve := int64(64 * 1024 * 1024)
	result.RequiredDurableBytes = result.WrapperOriginalBytes + result.ProtectedCopyBytes + result.TemporaryBytes + nativeDocuments + fixedReserve
	result.InvocationAllowanceBytes = result.WrapperOriginalBytes + int64(settings.ProtectedCopies)*captureBytes
	result.DocumentAndTemporaryBytes = result.RequiredDurableBytes - result.InvocationAllowanceBytes
	// Reserve simultaneous evidence encoding/readback plus source-artifact copies,
	// tokenizer IDs/JSON buffers, and 256 MiB for native runtime/DB overhead.
	result.RequiredMemoryBytes = result.CoreEvidenceBytes + 3*result.SerializedEvidenceBytes + 4*int64(len(bundleBytes)+len(planBytes)) + int64(plan.Settings.MaximumBundleBytes) + 128*1024*1024 + 256*1024*1024
	if result.RequiredDurableBytes > settings.MaximumDurableBytes || result.RequiredMemoryBytes > settings.MaximumMemoryBytes {
		return result, fmt.Errorf("fixed complete-review capacity exceeds reserved budget: durable %d/%d bytes, native memory %d/%d bytes", result.RequiredDurableBytes, settings.MaximumDurableBytes, result.RequiredMemoryBytes, settings.MaximumMemoryBytes)
	}
	return result, nil
}
