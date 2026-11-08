// Protected spool capacity is a separate lifecycle contract. It does not
// reinterpret the immutable full-local aggregate receipt or authorize deletion.
package main

import (
	"encoding/json"
	"errors"
)

const winnerMultipassSpoolLifecycle = "protected-exact-version-spool/v1"

// These independent finite bounds include wrapper result/claim/proof copies.
// The native lifecycle must enforce them before committing/evicting a record.
const winnerMultipassSpoolManifestBytes = 1024 * 1024
const winnerMultipassSpoolCaptureBytes = 32 * 1024 * 1024

type winnerMultipassSpoolSettings struct {
	LifecycleVersion      string `json:"lifecycle_version"`
	MaximumLocalPeakBytes int64  `json:"maximum_local_peak_bytes"`
	MaximumProtectedBytes int64  `json:"maximum_protected_bytes"`
}

// FullLocalAggregateBytes retains its original meaning and original receipt
// digest. Smaller LocalPeakBytes is valid only for the independently qualified
// protected-readback/commit/owned-cleanup lifecycle, never for full-local capture.
type winnerMultipassSpoolCapacity struct {
	Version                      string                       `json:"version"`
	Settings                     winnerMultipassSpoolSettings `json:"settings"`
	AggregateCapacitySha256      string                       `json:"aggregate_capacity_sha256"`
	PlanSha256                   string                       `json:"plan_sha256"`
	BundleSha256                 string                       `json:"bundle_sha256,omitempty"`
	GenerationCalls              int                          `json:"generation_calls"`
	TokenizerInvocations         int                          `json:"tokenizer_invocations"`
	CancellationInvocations      int                          `json:"cancellation_invocations"`
	Invocations                  int                          `json:"invocations"`
	ProtectedCopies              int                          `json:"protected_copies"`
	FullLocalAggregateBytes      int64                        `json:"full_local_aggregate_bytes"`
	ProtectedBytes               int64                        `json:"protected_bytes"`
	PermanentDocumentsBytes      int64                        `json:"permanent_documents_bytes"`
	CompactManifestBytes         int64                        `json:"compact_manifest_bytes"`
	MaximumCompactManifestBytes  int64                        `json:"maximum_compact_manifest_bytes"`
	MaximumCaptureRecordBytes    int64                        `json:"maximum_capture_record_bytes"`
	MaximumConcurrentInvocations int                          `json:"maximum_concurrent_invocations"`
	ActiveInvocationBytes        int64                        `json:"active_invocation_bytes"`
	RecoveryWorkspaceBytes       int64                        `json:"recovery_workspace_bytes"`
	RequiredLocalPeakBytes       int64                        `json:"required_local_peak_bytes"`
	RequiredMemoryBytes          int64                        `json:"required_memory_bytes"`
}

// Includes the original member state, captured attempt, one serialized capture
// record, and every configured simultaneous staging/readback copy. It is a
// peak for one invocation; the lifecycle must finish custody/cleanup before next.
func winnerMultipassSpoolInvocationCapacity(plan *winnerMultipassPlan, call winnerMultipassCall, operation string, payloadBytes int, settings winnerMultipassCapacitySettings) (int64, error) {
	if settings.TemporaryCopies <= 0 || settings.TemporaryCopies > 4 {
		return 0, errors.New("protected spool invocation lacks bounded staging copies")
	}
	if _, err := winnerMultipassInvocationCapacity(plan, call, operation, payloadBytes, settings); err != nil {
		return 0, err
	}
	envelope, err := winnerMultipassEnvelopeCapacity(plan, call, payloadBytes)
	if err != nil {
		return 0, err
	}
	original, capture, err := winnerMultipassInvocationStorage(envelope, operation, plan.Settings)
	if err != nil {
		return 0, err
	}
	return original + int64(1+settings.TemporaryCopies)*capture + winnerMultipassSpoolCaptureBytes + winnerMultipassSpoolManifestBytes, nil
}

// Finalize a receipt only after all exact census-specific terms are computed.
// Both storage budgets must be explicit; there is deliberately no permissive
// default or fallback that converts a full-local capacity failure into admission.
func finishWinnerMultipassSpoolCapacity(receipt *winnerMultipassSpoolCapacity) error {
	settings := receipt.Settings
	if settings.LifecycleVersion != winnerMultipassSpoolLifecycle || settings.MaximumLocalPeakBytes <= 0 || settings.MaximumProtectedBytes <= 0 ||
		!validSha256(receipt.AggregateCapacitySha256) || !validSha256(receipt.PlanSha256) || receipt.GenerationCalls < 1 || receipt.TokenizerInvocations < receipt.GenerationCalls ||
		receipt.Invocations != receipt.GenerationCalls+receipt.TokenizerInvocations+receipt.CancellationInvocations || receipt.CancellationInvocations < 0 || receipt.CancellationInvocations > 1 ||
		receipt.ProtectedCopies <= 0 || receipt.ProtectedCopies > 4 || receipt.FullLocalAggregateBytes <= 0 || receipt.ProtectedBytes <= 0 || receipt.PermanentDocumentsBytes <= 0 || receipt.ActiveInvocationBytes <= 0 || receipt.RequiredMemoryBytes <= 0 {
		return errors.New("protected spool capacity lacks an exact bounded original census/lifecycle")
	}
	receipt.Version = winnerMultipassVersion
	receipt.MaximumCompactManifestBytes = winnerMultipassSpoolManifestBytes
	receipt.MaximumCaptureRecordBytes = winnerMultipassSpoolCaptureBytes
	receipt.MaximumConcurrentInvocations = 1
	receipt.CompactManifestBytes = int64(receipt.Invocations) * winnerMultipassSpoolManifestBytes
	// Whole capture records and custody commits are independently protected in
	// addition to every raw leaf counted by the original aggregate receipt.
	receipt.ProtectedBytes += int64(receipt.Invocations*receipt.ProtectedCopies) * (winnerMultipassSpoolCaptureBytes + winnerMultipassSpoolManifestBytes)
	receipt.RecoveryWorkspaceBytes = receipt.ActiveInvocationBytes + winnerMultipassSpoolCaptureBytes + winnerMultipassSpoolManifestBytes
	receipt.RequiredLocalPeakBytes = receipt.PermanentDocumentsBytes + receipt.CompactManifestBytes + receipt.ActiveInvocationBytes + receipt.RecoveryWorkspaceBytes
	if receipt.RequiredLocalPeakBytes > settings.MaximumLocalPeakBytes || receipt.ProtectedBytes > settings.MaximumProtectedBytes {
		return errors.New("protected spool exceeds its separately declared local-peak or protected-storage budget")
	}
	return nil
}

// AggregateSettings still bounds ALL originals/copies exactly as before. The
// returned full receipt must be retained without mutation alongside this one.
func estimateWinnerMultipassSpoolCapacity(plan *winnerMultipassPlan, bundle winnerMultipassBundle, tokenizers int, aggregateSettings winnerMultipassCapacitySettings, spoolSettings winnerMultipassSpoolSettings) (*winnerMultipassSpoolCapacity, *winnerMultipassCapacity, error) {
	aggregate, err := estimateWinnerMultipassCapacity(plan, bundle, tokenizers, aggregateSettings)
	if err != nil {
		return nil, aggregate, err
	}
	encoded, err := json.Marshal(aggregate)
	if err != nil {
		return nil, aggregate, err
	}
	result := &winnerMultipassSpoolCapacity{Settings: spoolSettings, AggregateCapacitySha256: winnerMultipassSha256(encoded), PlanSha256: aggregate.PlanSha256, BundleSha256: aggregate.BundleSha256,
		GenerationCalls: aggregate.GenerationCalls, TokenizerInvocations: aggregate.TokenizerInvocations, Invocations: aggregate.GenerationCalls + aggregate.TokenizerInvocations, ProtectedCopies: aggregateSettings.ProtectedCopies,
		FullLocalAggregateBytes: aggregate.RequiredDurableBytes, ProtectedBytes: aggregate.ProtectedCopyBytes, PermanentDocumentsBytes: aggregate.DocumentAndTemporaryBytes, RequiredMemoryBytes: aggregate.RequiredMemoryBytes}
	for _, call := range plan.Calls {
		payloadBytes := plan.Settings.MaximumRequestBytes
		if call.Stage == "scan" {
			payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
			if err != nil {
				return result, aggregate, err
			}
			payloadBytes = len(payload)
		}
		for _, operation := range []string{"tokenize", "generate"} {
			peak, err := winnerMultipassSpoolInvocationCapacity(plan, call, operation, payloadBytes, aggregateSettings)
			if err != nil {
				return result, aggregate, err
			}
			result.ActiveInvocationBytes = max(result.ActiveInvocationBytes, peak)
		}
	}
	return result, aggregate, finishWinnerMultipassSpoolCapacity(result)
}
