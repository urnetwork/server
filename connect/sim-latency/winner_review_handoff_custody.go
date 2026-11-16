// Every original is independently retrievable by protected object version.
// Local paths and digest-only claims are never complete review custody.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Limits belong to this native capture owner, not the provider or legacy API.
type winnerHandoffCaptureSettings struct {
	FreezeLeaseDuration  time.Duration                   `json:"freeze_lease_duration"` // Positive one-time supervisor fence, ten seconds to five minutes; default five minutes, never a model clock.
	Capacity             winnerMultipassCapacitySettings `json:"capacity"`
	Spool                *winnerMultipassSpoolSettings   `json:"spool,omitempty"`        // Nil retains the full-local lifecycle; an explicit typed profile never lowers aggregate custody bounds.
	CleanupTimeout       time.Duration                   `json:"cleanup_timeout"`        // Positive failed-custody completion budget; default two minutes.
	ReceiptTimeout       time.Duration                   `json:"receipt_timeout"`        // Positive per-call protected retention budget; default two minutes.
	MaximumDocumentBytes int                             `json:"maximum_document_bytes"` // Positive native object cap; at most the separately approved 128 MiB archive bound.
}

func defaultWinnerHandoffCaptureSettings() winnerHandoffCaptureSettings {
	return winnerHandoffCaptureSettings{FreezeLeaseDuration: 5 * time.Minute, Capacity: defaultWinnerMultipassCapacitySettings(), CleanupTimeout: 2 * time.Minute, ReceiptTimeout: 2 * time.Minute, MaximumDocumentBytes: 128 * 1024 * 1024}
}

// The contained adapter supplies a real allocated reservation and observes the
// actual native cgroup limit. This receipt grants neither model nor DB authority.
type winnerHandoffCapacityReservation struct {
	CapacitySha256      string   `json:"capacity_sha256"`
	SpoolCapacitySha256 string   `json:"spool_capacity_sha256,omitempty"`
	ReservationPath     string   `json:"reservation_path"`
	ReservationInode    uint64   `json:"reservation_inode"`
	Device              uint64   `json:"device"`
	ReservedBytes       int64    `json:"reserved_bytes"`
	MemoryLimitBytes    int64    `json:"memory_limit_bytes"`
	MemoryCurrentBytes  int64    `json:"memory_current_bytes"`
	HostAvailableBytes  int64    `json:"host_available_bytes"`
	MemoryCgroup        string   `json:"memory_cgroup"`
	StoragePaths        []string `json:"storage_paths"`
}

// A descriptor binds exact bytes, archive version and retained-until time.
// The retaining implementation must authenticate exact-version readback first.
type winnerHandoffCustodyOriginal struct {
	Name      string                                  `json:"name"`
	Bytes     int64                                   `json:"bytes"`
	Retention model.CompetitionReviewHandoffRetention `json:"retention"`
}

// This is the authenticated wrapper's complete original census, including its
// manifest and receipt. Operation failures remain present and non-authorizing.
type winnerHandoffInvocationCustody struct {
	Phase                string                         `json:"phase"`
	Operation            string                         `json:"operation"`
	CallId               string                         `json:"call_id"`
	RequestSha256        string                         `json:"request_sha256"`
	ManifestSha256       string                         `json:"manifest_sha256"`
	WrapperReceiptSha256 string                         `json:"wrapper_receipt_sha256"`
	Originals            []winnerHandoffCustodyOriginal `json:"originals"`
	Failure              string                         `json:"failure,omitempty"`
	SpoolCommit          *winnerHandoffCustodyOriginal  `json:"spool_commit,omitempty"`
	SpoolReferencePath   string                         `json:"spool_reference_path,omitempty"`
	SpoolReferenceSha256 string                         `json:"spool_reference_sha256,omitempty"`
}

type winnerHandoffTokenization struct {
	Admission winnerMultipassAdmission       `json:"admission"`
	Custody   winnerHandoffInvocationCustody `json:"custody"`
}

// Qualification proves the transport release; run authority binds that exact
// release and fixed candidate attempt. Neither is an operator winner verdict.
type winnerHandoffProvenance struct {
	RunAuthorizationSha256    string `json:"run_authorization_sha256"`
	ReleaseSha256             string `json:"release_sha256"`
	QualificationReportSha256 string `json:"qualification_report_sha256"`
	CaptureAdmissionSha256    string `json:"capture_admission_sha256"`
}

// The completion and public private-evidence digest bind this entire manifest.
// Evidence and source bytes are separate protected objects, not omitted inputs.
type winnerHandoffCustodyManifest struct {
	Schema         int                              `json:"schema"`
	Kind           string                           `json:"kind"`
	HandoffId      string                           `json:"handoff_id"`
	PlanSha256     string                           `json:"plan_sha256"`
	BundleSha256   string                           `json:"bundle_sha256"`
	Provenance     winnerHandoffProvenance          `json:"provenance"`
	Capacity       winnerMultipassCapacity          `json:"capacity"`
	SpoolCapacity  *winnerMultipassSpoolCapacity    `json:"spool_capacity,omitempty"`
	Reservation    winnerHandoffCapacityReservation `json:"reservation"`
	Documents      []winnerHandoffCustodyOriginal   `json:"documents"`
	LeafAdmissions []winnerMultipassAdmission       `json:"leaf_admissions"`
	Invocations    []winnerHandoffInvocationCustody `json:"invocations"`
}

func validateWinnerHandoffReservation(capacity *winnerMultipassCapacity, reservation winnerHandoffCapacityReservation) error {
	return validateWinnerHandoffStorageReservation(capacity, nil, reservation)
}

// A spool receipt is a separate admitted lifecycle, never a reinterpretation of
// the aggregate original byte census. Memory remains the complete-review bound.
func validateWinnerHandoffStorageReservation(capacity *winnerMultipassCapacity, spool *winnerMultipassSpoolCapacity, reservation winnerHandoffCapacityReservation) error {
	required := int64(0)
	if capacity != nil {
		required = capacity.RequiredDurableBytes
	}
	if spool != nil {
		if capacity == nil {
			return errors.New("native spool has no aggregate resource census")
		}
		content, _ := json.Marshal(spool)
		aggregate, _ := json.Marshal(capacity)
		if spool.AggregateCapacitySha256 != winnerMultipassSha256(aggregate) || spool.RequiredMemoryBytes != capacity.RequiredMemoryBytes || reservation.SpoolCapacitySha256 != winnerMultipassSha256(content) {
			return errors.New("native spool reservation differs from its explicit aggregate/lifecycle census")
		}
		required = spool.RequiredLocalPeakBytes
	} else if reservation.SpoolCapacitySha256 != "" {
		return errors.New("full-local custody cannot silently use a smaller spool reservation")
	}
	content, err := json.Marshal(capacity)
	if err != nil || capacity == nil || required <= 0 || reservation.CapacitySha256 != winnerMultipassSha256(content) || reservation.ReservedBytes < required ||
		reservation.MemoryLimitBytes < capacity.RequiredMemoryBytes || reservation.MemoryLimitBytes > capacity.Settings.MaximumMemoryBytes || reservation.MemoryCurrentBytes < 0 ||
		reservation.MemoryLimitBytes-reservation.MemoryCurrentBytes < capacity.RequiredMemoryBytes || reservation.HostAvailableBytes < capacity.RequiredMemoryBytes ||
		!filepath.IsAbs(reservation.MemoryCgroup) || filepath.Clean(reservation.MemoryCgroup) != reservation.MemoryCgroup || reservation.ReservationInode == 0 ||
		!filepath.IsAbs(reservation.ReservationPath) || filepath.Clean(reservation.ReservationPath) != reservation.ReservationPath || len(reservation.StoragePaths) == 0 {
		return errors.New("native review lacks its actual bounded storage/memory reservation")
	}
	for _, path := range reservation.StoragePaths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("review storage reservation contains a noncanonical path")
		}
	}
	return nil
}

func validateWinnerHandoffOriginal(value winnerHandoffCustodyOriginal, minimumRetention time.Time, maximumBytes int64) error {
	if value.Name == "" || filepath.Base(value.Name) != value.Name || value.Name == "." || value.Bytes < 0 || value.Bytes > maximumBytes ||
		value.Retention.Key == "" || value.Retention.VersionId == "" || !validSha256(value.Retention.Sha256) || value.Retention.RetainUntil.Before(minimumRetention) {
		return errors.New("review original lacks bounded exact-version protected custody")
	}
	return nil
}

func validateWinnerHandoffInvocation(value winnerHandoffInvocationCustody, phase, operation string, call winnerMultipassCall, requestSha string, admission winnerMultipassAdmission, transportSha string, plan *winnerMultipassPlan) error {
	if value.Phase != phase || value.Operation != operation || value.CallId != call.Id || value.RequestSha256 != requestSha || value.Failure != "" || !validSha256(value.ManifestSha256) || !validSha256(value.WrapperReceiptSha256) || len(value.Originals) == 0 || len(value.Originals) > 40 {
		return errors.New("captured invocation lacks exact authenticated original census")
	}
	originals := map[string]winnerHandoffCustodyOriginal{}
	for _, original := range value.Originals {
		if _, exists := originals[original.Name]; exists {
			return errors.New("captured invocation repeats an original")
		}
		if err := validateWinnerHandoffOriginal(original, plan.Owner.Deadline, int64(defaultWinnerHandoffCaptureSettings().MaximumDocumentBytes)); err != nil {
			return err
		}
		originals[original.Name] = original
	}
	for name, digest := range map[string]string{"evidence-manifest.json": value.ManifestSha256, "wrapper-receipt.json": value.WrapperReceiptSha256} {
		if originals[name].Retention.Sha256 != digest {
			return errors.New("captured wrapper manifest or receipt is not protected")
		}
	}
	leaves, err := winnerMultipassJournalLeafLimits(operation, plan.Settings)
	if err != nil {
		return err
	}
	for name, limit := range leaves {
		original, found := originals[name]
		if !found || original.Bytes > int64(limit) {
			return errors.New("captured invocation omitted an original operation journal")
		}
	}
	if operation == "tokenize" && (originals["tokenize.request.json"].Retention.Sha256 != admission.TokenizerRequestSha256 || originals["tokenize.response.json"].Retention.Sha256 != admission.TokenizerResponseSha256) ||
		operation == "generate" && originals["transport.response"].Retention.Sha256 != transportSha {
		return errors.New("captured tokenizer or transport bytes differ from original provider evidence")
	}
	return nil
}

// Frozen leaves are counted before any draw and again before their own call.
// Synthesis requests are known only when their declared predecessors complete.
func winnerHandoffTokenizerCensus(plan *winnerMultipassPlan) int {
	count := len(plan.Calls)
	for _, call := range plan.Calls {
		if call.Stage == "scan" {
			count++
		}
	}
	return count
}

// Used for plan/bundle/evidence documents; wrapper originals take the same
// protected retaining hook only after independent containment authentication.
func retainWinnerHandoffDocument(ctx context.Context, name string, content []byte, maximum int, minimumRetention time.Time, retain func(context.Context, []byte, string) (*controller.CompetitionWinnerReviewEvidence, error)) (winnerHandoffCustodyOriginal, error) {
	if len(content) == 0 {
		return winnerHandoffCustodyOriginal{}, errors.New("native custody document is empty")
	}
	return retainWinnerHandoffOriginal(ctx, name, content, maximum, minimumRetention, retain)
}

// A zero-byte original is meaningful (empty stderr), unlike an absent document.
// It retains the empty SHA/version and is independently read back like any leaf.
func retainWinnerHandoffOriginal(ctx context.Context, name string, content []byte, maximum int, minimumRetention time.Time, retain func(context.Context, []byte, string) (*controller.CompetitionWinnerReviewEvidence, error)) (winnerHandoffCustodyOriginal, error) {
	if len(content) > maximum || retain == nil {
		return winnerHandoffCustodyOriginal{}, errors.New("native custody document exceeds its admitted archive object bound")
	}
	digest := winnerMultipassSha256(content)
	receipt, err := retain(ctx, content, digest)
	if err != nil {
		return winnerHandoffCustodyOriginal{}, err
	}
	if receipt == nil || receipt.Sha256 != digest {
		return winnerHandoffCustodyOriginal{}, errors.New("protected native document readback differs")
	}
	value := winnerHandoffCustodyOriginal{Name: name, Bytes: int64(len(content)), Retention: model.CompetitionReviewHandoffRetention{Key: receipt.Key, VersionId: receipt.VersionId, Sha256: receipt.Sha256, RetainUntil: receipt.RetainUntil}}
	return value, validateWinnerHandoffOriginal(value, minimumRetention, int64(maximum))
}

// Complete custody binds every materialized native input and every provider
// original. An incomplete manifest may be retained but never becomes a proposal.
func validateWinnerHandoffCustody(owner *model.CompetitionReviewHandoff, plan *winnerMultipassPlan, bundle winnerMultipassBundle, evidence winnerMultipassEvidence, custody winnerHandoffCustodyManifest) error {
	if owner == nil || plan == nil || custody.Schema != 1 || custody.Kind != "elected-winner-review-protected-custody/v1" || custody.HandoffId != owner.HandoffId.String() ||
		custody.Provenance.RunAuthorizationSha256 != owner.Args.AuthorizationSha256 || !validSha256(custody.Provenance.ReleaseSha256) || !validSha256(custody.Provenance.QualificationReportSha256) ||
		!validSha256(custody.Provenance.CaptureAdmissionSha256) ||
		custody.PlanSha256 != owner.Plan.PlanSha256 || custody.BundleSha256 != plan.BundleSha256 || len(custody.Documents) != 4 || len(custody.Invocations) != len(plan.Calls)+winnerHandoffTokenizerCensus(plan) {
		return errors.New("native review custody is incomplete or belongs to another owner")
	}
	capacity, err := estimateWinnerMultipassCapacity(plan, bundle, winnerHandoffTokenizerCensus(plan), custody.Capacity.Settings)
	if err != nil {
		return err
	}
	expectedCapacity, _ := json.Marshal(capacity)
	actualCapacity, _ := json.Marshal(custody.Capacity)
	if custody.SpoolCapacity != nil {
		expected, _, err := estimateWinnerMultipassSpoolCapacity(plan, bundle, winnerHandoffTokenizerCensus(plan), custody.Capacity.Settings, custody.SpoolCapacity.Settings)
		expectedBytes, _ := json.Marshal(expected)
		actualBytes, _ := json.Marshal(custody.SpoolCapacity)
		if err != nil || string(expectedBytes) != string(actualBytes) {
			return errors.New("native spool resource census changed")
		}
	}
	if string(expectedCapacity) != string(actualCapacity) || validateWinnerHandoffStorageReservation(capacity, custody.SpoolCapacity, custody.Reservation) != nil {
		return errors.New("native review resource census or reservation changed")
	}
	documents := map[string][]byte{}
	documents["plan.json"], _ = json.Marshal(plan)
	documents["bundle.json"], _ = json.Marshal(bundle)
	documents["core-evidence.json"], _ = json.Marshal(evidence)
	documents["capacity.json"], _ = json.Marshal(struct {
		Capacity    winnerMultipassCapacity          `json:"capacity"`
		Reservation winnerHandoffCapacityReservation `json:"reservation"`
		Spool       *winnerMultipassSpoolCapacity    `json:"spool,omitempty"`
	}{Capacity: custody.Capacity, Reservation: custody.Reservation, Spool: custody.SpoolCapacity})
	for _, original := range custody.Documents {
		content, found := documents[original.Name]
		if !found || original.Bytes != int64(len(content)) || original.Retention.Sha256 != winnerMultipassSha256(content) {
			return errors.New("native source, evidence or capacity document is not exactly protected")
		}
		if err := validateWinnerHandoffOriginal(original, owner.Deadline, int64(defaultWinnerHandoffCaptureSettings().MaximumDocumentBytes)); err != nil {
			return err
		}
		delete(documents, original.Name)
	}
	if len(evidence.Calls) != len(plan.Calls) {
		return errors.New("native review custody cannot replace missing model evidence")
	}
	for _, invocation := range custody.Invocations {
		if custody.SpoolCapacity != nil {
			if invocation.SpoolCommit == nil || validateWinnerHandoffOriginal(*invocation.SpoolCommit, owner.Deadline, winnerMultipassSpoolManifestBytes) != nil ||
				!filepath.IsAbs(invocation.SpoolReferencePath) || filepath.Clean(invocation.SpoolReferencePath) != invocation.SpoolReferencePath || !validSha256(invocation.SpoolReferenceSha256) {
				return errors.New("spooled invocation lost its protected compact commit/reference")
			}
		} else if invocation.SpoolCommit != nil || invocation.SpoolReferencePath != "" || invocation.SpoolReferenceSha256 != "" {
			return errors.New("full-local invocation unexpectedly changed custody lifecycle")
		}
	}
	if err := validateWinnerMultipassLeafAdmissions(plan, bundle, custody.LeafAdmissions); err != nil {
		return err
	}
	// The receipt order is part of the fixed protocol: every leaf preflight,
	// then each per-call admission immediately followed by its generation.
	invocationIndex, leafIndex := 0, 0
	for _, call := range plan.Calls {
		if call.Stage != "scan" {
			continue
		}
		payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
		if err != nil {
			return err
		}
		admission := custody.LeafAdmissions[leafIndex]
		if admission.CallId != call.Id {
			return errors.New("native leaf preflight order differs from the frozen call census")
		}
		if err := validateWinnerHandoffInvocation(custody.Invocations[invocationIndex], "leaf_preflight", "tokenize", call, winnerMultipassSha256(payload), admission, "", plan); err != nil {
			return err
		}
		invocationIndex++
		leafIndex++
	}
	for index, call := range plan.Calls {
		observation := evidence.Calls[index]
		requestSha := winnerMultipassSha256(observation.Request)
		for _, stage := range []struct{ phase, operation string }{{phase: "call_admission", operation: "tokenize"}, {phase: "capture", operation: "generate"}} {
			invocation := custody.Invocations[invocationIndex]
			if err := validateWinnerHandoffInvocation(invocation, stage.phase, stage.operation, call, requestSha, observation.Admission, observation.TransportResponseSha256, plan); err != nil {
				return err
			}
			if stage.operation == "generate" && invocation.WrapperReceiptSha256 != observation.TransportReceiptSha256 {
				return errors.New("native model evidence is not bound to its original containment receipt")
			}
			invocationIndex++
		}
	}
	return nil
}
