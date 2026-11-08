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
	Capacity             winnerMultipassCapacitySettings `json:"capacity"`
	CleanupTimeout       time.Duration                   `json:"cleanup_timeout"`        // Positive failed-custody completion budget; default two minutes.
	ReceiptTimeout       time.Duration                   `json:"receipt_timeout"`        // Positive per-call protected retention budget; default two minutes.
	MaximumDocumentBytes int                             `json:"maximum_document_bytes"` // Positive native object cap; at most the separately approved 128 MiB archive bound.
}

func defaultWinnerHandoffCaptureSettings() winnerHandoffCaptureSettings {
	return winnerHandoffCaptureSettings{Capacity: defaultWinnerMultipassCapacitySettings(), CleanupTimeout: 2 * time.Minute, ReceiptTimeout: 2 * time.Minute, MaximumDocumentBytes: 128 * 1024 * 1024}
}

// The contained adapter supplies a real allocated reservation and observes the
// actual native cgroup limit. This receipt grants neither model nor DB authority.
type winnerHandoffCapacityReservation struct {
	CapacitySha256     string   `json:"capacity_sha256"`
	ReservationPath    string   `json:"reservation_path"`
	ReservationInode   uint64   `json:"reservation_inode"`
	Device             uint64   `json:"device"`
	ReservedBytes      int64    `json:"reserved_bytes"`
	MemoryLimitBytes   int64    `json:"memory_limit_bytes"`
	MemoryCurrentBytes int64    `json:"memory_current_bytes"`
	HostAvailableBytes int64    `json:"host_available_bytes"`
	MemoryCgroup       string   `json:"memory_cgroup"`
	StoragePaths       []string `json:"storage_paths"`
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
	Operation            string                         `json:"operation"`
	CallId               string                         `json:"call_id"`
	RequestSha256        string                         `json:"request_sha256"`
	ManifestSha256       string                         `json:"manifest_sha256"`
	WrapperReceiptSha256 string                         `json:"wrapper_receipt_sha256"`
	Originals            []winnerHandoffCustodyOriginal `json:"originals"`
	Failure              string                         `json:"failure,omitempty"`
}

type winnerHandoffTokenization struct {
	Admission winnerMultipassAdmission       `json:"admission"`
	Custody   winnerHandoffInvocationCustody `json:"custody"`
}

// The completion and public private-evidence digest bind this entire manifest.
// Evidence and source bytes are separate protected objects, not omitted inputs.
type winnerHandoffCustodyManifest struct {
	Schema       int                              `json:"schema"`
	Kind         string                           `json:"kind"`
	HandoffId    string                           `json:"handoff_id"`
	PlanSha256   string                           `json:"plan_sha256"`
	BundleSha256 string                           `json:"bundle_sha256"`
	Capacity     winnerMultipassCapacity          `json:"capacity"`
	Reservation  winnerHandoffCapacityReservation `json:"reservation"`
	Documents    []winnerHandoffCustodyOriginal   `json:"documents"`
	Invocations  []winnerHandoffInvocationCustody `json:"invocations"`
}

func validateWinnerHandoffReservation(capacity *winnerMultipassCapacity, reservation winnerHandoffCapacityReservation) error {
	content, err := json.Marshal(capacity)
	if err != nil || capacity == nil || reservation.CapacitySha256 != winnerMultipassSha256(content) || reservation.ReservedBytes < capacity.RequiredDurableBytes ||
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

func validateWinnerHandoffInvocation(value winnerHandoffInvocationCustody, operation string, call winnerMultipassCall, requestSha string, admission winnerMultipassAdmission, transportSha string, plan *winnerMultipassPlan) error {
	if value.Operation != operation || value.CallId != call.Id || value.RequestSha256 != requestSha || value.Failure != "" || !validSha256(value.ManifestSha256) || !validSha256(value.WrapperReceiptSha256) || len(value.Originals) == 0 || len(value.Originals) > 40 {
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

// Used for plan/bundle/evidence documents; wrapper originals take the same
// protected retaining hook only after independent containment authentication.
func retainWinnerHandoffDocument(ctx context.Context, name string, content []byte, maximum int, minimumRetention time.Time, retain func(context.Context, []byte, string) (*controller.CompetitionWinnerReviewEvidence, error)) (winnerHandoffCustodyOriginal, error) {
	if len(content) == 0 || len(content) > maximum || retain == nil {
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
		custody.PlanSha256 != owner.Plan.PlanSha256 || custody.BundleSha256 != plan.BundleSha256 || len(custody.Documents) != 4 || len(custody.Invocations) != 2*len(plan.Calls) {
		return errors.New("native review custody is incomplete or belongs to another owner")
	}
	capacity, err := estimateWinnerMultipassCapacity(plan, bundle, len(plan.Calls), custody.Capacity.Settings)
	if err != nil {
		return err
	}
	expectedCapacity, _ := json.Marshal(capacity)
	actualCapacity, _ := json.Marshal(custody.Capacity)
	if string(expectedCapacity) != string(actualCapacity) || validateWinnerHandoffReservation(capacity, custody.Reservation) != nil {
		return errors.New("native review resource census or reservation changed")
	}
	documents := map[string][]byte{}
	documents["plan.json"], _ = json.Marshal(plan)
	documents["bundle.json"], _ = json.Marshal(bundle)
	documents["core-evidence.json"], _ = json.Marshal(evidence)
	documents["capacity.json"], _ = json.Marshal(struct {
		Capacity    winnerMultipassCapacity          `json:"capacity"`
		Reservation winnerHandoffCapacityReservation `json:"reservation"`
	}{Capacity: custody.Capacity, Reservation: custody.Reservation})
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
	invocations := map[string]winnerHandoffInvocationCustody{}
	for _, invocation := range custody.Invocations {
		key := invocation.Operation + ":" + invocation.CallId
		if _, exists := invocations[key]; exists {
			return errors.New("native review custody repeats a tokenizer or generation invocation")
		}
		invocations[key] = invocation
	}
	if len(evidence.Calls) != len(plan.Calls) {
		return errors.New("native review custody cannot replace missing model evidence")
	}
	for index, call := range plan.Calls {
		observation := evidence.Calls[index]
		requestSha := winnerMultipassSha256(observation.Request)
		for _, operation := range []string{"tokenize", "generate"} {
			invocation, found := invocations[operation+":"+call.Id]
			if !found {
				return errors.New("native review is missing a tokenizer or generation original")
			}
			if err := validateWinnerHandoffInvocation(invocation, operation, call, requestSha, observation.Admission, observation.TransportResponseSha256, plan); err != nil {
				return err
			}
			if operation == "generate" && invocation.WrapperReceiptSha256 != observation.TransportReceiptSha256 {
				return errors.New("native model evidence is not bound to its original containment receipt")
			}
		}
	}
	return nil
}
