// Native capture keeps main credentials in the parent and sends only one
// prepared model payload into the qualified containment boundary. No approval
// API, reward-delivery or source-publication operation exists in this module.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Independent transport qualification supplies these hooks. Tokenization is
// non-inference; Invoke authenticates actual wrapper/transport provenance and
// preserves every visible terminal, including malformed or uncertain output.
type winnerHandoffCaptureHooks struct {
	Provenance   winnerHandoffProvenance
	Now          func() time.Time
	Reserve      func(context.Context, *winnerMultipassCapacity) (winnerHandoffCapacityReservation, error)
	ReserveSpool func(context.Context, *winnerMultipassCapacity, *winnerMultipassSpoolCapacity) (winnerHandoffCapacityReservation, error)
	Admit        func(context.Context, string, winnerMultipassCall, []byte) (winnerHandoffTokenization, error)
	Invoke       func(context.Context, winnerMultipassCall, []byte, winnerMultipassAdmission, string) (winnerHandoffTransport, error)
	Claim        func(context.Context, model.CompetitionReviewHandoffCall) (*model.CompetitionReviewHandoffCall, error)
	Receipt      func(context.Context, model.CompetitionReviewHandoffCall) (*model.CompetitionReviewHandoffCall, error)
	Retain       func(context.Context, []byte, string) (*controller.CompetitionWinnerReviewEvidence, error)
	Complete     func(context.Context, model.CompetitionReviewHandoffCompletion) (*model.CompetitionReviewHandoffCompletion, error)
}

type winnerHandoffTransport struct {
	VisibleResponse          []byte
	TransportResponseSha256  string
	ContainmentReceiptSha256 string
	Custody                  winnerHandoffInvocationCustody
	// Publicly safe classification only; original diagnostic bytes stay private.
	Error string
}

// This report is private and always non-authorizing. A proposal is written to
// the durable pending ledger only after offline replay and protected retention.
type winnerHandoffCaptureResult struct {
	Status              string                                    `json:"status"`
	PromotionAuthorized bool                                      `json:"promotion_authorized"`
	Evidence            winnerMultipassEvidence                   `json:"evidence"`
	Custody             winnerHandoffCustodyManifest              `json:"custody"`
	Completion          *model.CompetitionReviewHandoffCompletion `json:"completion,omitempty"`
}

func validateWinnerHandoffIdentity(owner *model.CompetitionReviewHandoff, plan *winnerMultipassPlan, bundle winnerMultipassBundle) error {
	if owner == nil || plan == nil || model.ValidateCompetitionReviewHandoffArgs(owner.Args) != nil || model.ValidateCompetitionReviewHandoffPlan(owner.Plan) != nil {
		return errors.New("native handoff identity is incomplete")
	}
	if err := validateWinnerMultipassPlan(plan, bundle); err != nil {
		return err
	}
	planBytes, _ := json.Marshal(plan)
	providerBytes, _ := json.Marshal(plan.Provider)
	request, template, args := bundle.Request.Request, bundle.Request.Template, owner.Args
	if args.ProviderSha256 != winnerMultipassSha256(providerBytes) || owner.HandoffId.String() != plan.Owner.Id || !owner.StartedAt.Equal(plan.Owner.StartedAt) || !owner.Deadline.Equal(plan.Owner.Deadline) || owner.Deadline.Sub(owner.StartedAt) != time.Duration(args.TimeoutSeconds)*time.Second ||
		owner.Plan.PlanSha256 != winnerMultipassSha256(planBytes) || owner.Plan.BundleSha256 != plan.BundleSha256 || len(owner.Plan.CallIds) != len(plan.Calls) || template.HandoffId != owner.HandoffId.String() ||
		request.RoundId != args.RoundId || request.JobId != args.JobId || request.RewardRevision != args.RewardRevision || request.RewardDecisionSha256 != args.DecisionSha256 || request.SealSha256 != args.SealSha256 ||
		template.PatchSha256 != args.PatchSha256 || template.BaseSourceLockSha256 != args.BaseSourceLockSha256 || template.ScoreSha256 != args.ScoreSha256 || template.ArtifactManifestSha256 != args.ArtifactManifestSha256 || template.BaselineSha256 != args.BaselineSha256 {
		return errors.New("native handoff owner differs from exact source, decision, plan or clock")
	}
	for index, call := range plan.Calls {
		if owner.Plan.CallIds[index] != call.Id {
			return errors.New("native handoff call census differs")
		}
	}
	return nil
}

// Every leaf is counted against the final owner-bound plan before any draw.
// Future synthesis inputs cannot be known yet; each requires its exact count
// before its own durable claim, without changing the plan or clock.
func preflightWinnerHandoffLeaves(ctx context.Context, plan *winnerMultipassPlan, bundle winnerMultipassBundle, admit func(context.Context, winnerMultipassCall, []byte) (winnerMultipassAdmission, error)) ([]winnerMultipassAdmission, error) {
	if admit == nil {
		return nil, errors.New("qualified native tokenizer is unavailable")
	}
	admissions := []winnerMultipassAdmission{}
	for _, call := range plan.Calls {
		if call.Stage != "scan" {
			continue
		}
		payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
		if err != nil {
			return admissions, err
		}
		value, err := admit(ctx, call, payload)
		admissions = append(admissions, value)
		if err != nil {
			return admissions, err
		}
		if err := validateWinnerMultipassAdmission(plan, call, payload, value); err != nil {
			return admissions, err
		}
	}
	return admissions, validateWinnerMultipassLeafAdmissions(plan, bundle, admissions)
}

// Only a fresh private directory is accepted. A crash after the first DB claim
// is an explicit recovery hold, never permission to delete this directory and
// resample. The database refuses every repeated claim even under a new path.
func captureWinnerHandoff(ctx context.Context, directory string, owner *model.CompetitionReviewHandoff, plan *winnerMultipassPlan, bundle winnerMultipassBundle, settings winnerHandoffCaptureSettings, hooks winnerHandoffCaptureHooks) (result *winnerHandoffCaptureResult, resultErr error) {
	if owner == nil || plan == nil || hooks.Now == nil || settings.Spool == nil && hooks.Reserve == nil || settings.Spool != nil && hooks.ReserveSpool == nil || hooks.Admit == nil || hooks.Invoke == nil || hooks.Claim == nil || hooks.Receipt == nil || hooks.Retain == nil || hooks.Complete == nil ||
		hooks.Provenance.RunAuthorizationSha256 != owner.Args.AuthorizationSha256 || !validSha256(hooks.Provenance.ReleaseSha256) || !validSha256(hooks.Provenance.QualificationReportSha256) ||
		!validSha256(hooks.Provenance.CaptureAdmissionSha256) ||
		settings.CleanupTimeout <= 0 || settings.ReceiptTimeout <= 0 || settings.MaximumDocumentBytes <= 0 || settings.MaximumDocumentBytes > defaultWinnerHandoffCaptureSettings().MaximumDocumentBytes {
		return nil, errors.New("native capture lacks qualified ownership or transport hooks")
	}
	if err := validateWinnerHandoffIdentity(owner, plan, bundle); err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithDeadline(ctx, owner.Deadline)
	defer cancel()
	planBytes, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	if owner.HandoffId.String() != plan.Owner.Id || !owner.StartedAt.Equal(plan.Owner.StartedAt) || !owner.Deadline.Equal(plan.Owner.Deadline) || owner.Plan.PlanSha256 != winnerMultipassSha256(planBytes) || owner.Plan.BundleSha256 != plan.BundleSha256 || len(owner.Plan.CallIds) != len(plan.Calls) || bundle.Request.Template.HandoffId != owner.HandoffId.String() {
		return nil, errors.New("capture does not bind its exact durable owner")
	}
	for i, call := range plan.Calls {
		if owner.Plan.CallIds[i] != call.Id {
			return nil, errors.New("capture call census changed")
		}
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("capture requires a canonical new private directory")
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, err
	}
	result = &winnerHandoffCaptureResult{Status: "PENDING", Evidence: winnerMultipassEvidence{Version: winnerMultipassVersion, PlanSha256: owner.Plan.PlanSha256, BundleSha256: plan.BundleSha256, Calls: []winnerMultipassCallEvidence{}}}
	result.Custody = winnerHandoffCustodyManifest{Schema: 1, Kind: "elected-winner-review-protected-custody/v1", HandoffId: owner.HandoffId.String(), PlanSha256: owner.Plan.PlanSha256,
		BundleSha256: plan.BundleSha256, Provenance: hooks.Provenance, Documents: []winnerHandoffCustodyOriginal{}, LeafAdmissions: []winnerMultipassAdmission{}, Invocations: []winnerHandoffInvocationCustody{}}
	retainDocument := func(retentionCtx context.Context, name string, content []byte) error {
		original, err := retainWinnerHandoffDocument(retentionCtx, name, content, settings.MaximumDocumentBytes, owner.Deadline, hooks.Retain)
		if err == nil {
			result.Custody.Documents = append(result.Custody.Documents, original)
		}
		return err
	}
	// All return paths retain a non-authorizing local outcome, even when the
	// database/archive is unavailable. It is not automatically retried.
	defer func() {
		if resultErr != nil {
			result.Status = "FAILED_RETAINED"
			// Even failed/incomplete evidence is retained as an immutable terminal
			// observation, never an invitation to sample the same owner again.
			failedBytes, encodeErr := json.Marshal(result.Evidence)
			if encodeErr == nil {
				cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), settings.CleanupTimeout)
				found := false
				for _, document := range result.Custody.Documents {
					found = found || document.Name == "core-evidence.json"
				}
				if !found {
					encodeErr = retainDocument(cleanup, "core-evidence.json", failedBytes)
				}
				failedBytes, marshalErr := json.Marshal(result.Custody)
				var retained *controller.CompetitionWinnerReviewEvidence
				retainErr := errors.Join(encodeErr, marshalErr)
				if retainErr == nil && len(failedBytes) <= settings.MaximumDocumentBytes {
					retained, retainErr = hooks.Retain(cleanup, failedBytes, winnerMultipassSha256(failedBytes))
				}
				if retainErr == nil && retained != nil {
					result.Completion, retainErr = hooks.Complete(cleanup, model.CompetitionReviewHandoffCompletion{HandoffId: owner.HandoffId, PlanSha256: owner.Plan.PlanSha256,
						Evidence: model.CompetitionReviewHandoffRetention{Key: retained.Key, VersionId: retained.VersionId, Sha256: retained.Sha256, RetainUntil: retained.RetainUntil}, Failure: "capture_failed_or_unresolved"})
				}
				cleanupCancel()
				if retainErr != nil {
					resultErr = errors.Join(resultErr, errors.New("protected failed-evidence retention or durable completion unavailable; retain local originals"))
				}
			}
		}
		if result.Completion != nil {
			content, err := json.Marshal(result.Completion)
			if err == nil {
				err = writePrivateReviewFile(filepath.Join(directory, "completion.json"), content)
			}
			resultErr = errors.Join(resultErr, err)
		}
		content, err := json.Marshal(result)
		if err == nil {
			err = writePrivateReviewFile(filepath.Join(directory, "capture-result.json"), content)
		}
		if resultErr == nil {
			resultErr = err
		}
	}()
	capacity, err := estimateWinnerMultipassCapacity(plan, bundle, winnerHandoffTokenizerCensus(plan), settings.Capacity)
	if capacity != nil {
		result.Custody.Capacity = *capacity
	}
	if err != nil {
		return result, err
	}
	if capacity.SerializedEvidenceBytes > int64(settings.MaximumDocumentBytes) {
		return result, errors.New("fixed native evidence census cannot fit protected object bound")
	}
	if settings.Spool != nil {
		result.Custody.SpoolCapacity, _, err = estimateWinnerMultipassSpoolCapacity(plan, bundle, winnerHandoffTokenizerCensus(plan), settings.Capacity, *settings.Spool)
		if err == nil {
			result.Custody.Reservation, err = hooks.ReserveSpool(callCtx, capacity, result.Custody.SpoolCapacity)
		}
	} else {
		result.Custody.Reservation, err = hooks.Reserve(callCtx, capacity)
	}
	if err != nil {
		return result, err
	}
	if err := validateWinnerHandoffStorageReservation(capacity, result.Custody.SpoolCapacity, result.Custody.Reservation); err != nil {
		return result, err
	}
	capacityBytes, _ := json.Marshal(struct {
		Capacity    winnerMultipassCapacity          `json:"capacity"`
		Reservation winnerHandoffCapacityReservation `json:"reservation"`
		Spool       *winnerMultipassSpoolCapacity    `json:"spool,omitempty"`
	}{Capacity: *capacity, Reservation: result.Custody.Reservation, Spool: result.Custody.SpoolCapacity})
	if err := retainDocument(callCtx, "capacity.json", capacityBytes); err != nil {
		return result, err
	}
	if err := writePrivateReviewFile(filepath.Join(directory, "plan.json"), planBytes); err != nil {
		return result, err
	}
	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		return result, err
	}
	if err := writePrivateReviewFile(filepath.Join(directory, "bundle.json"), bundleBytes); err != nil {
		return result, err
	}
	for _, document := range []struct {
		name    string
		content []byte
	}{{name: "plan.json", content: planBytes}, {name: "bundle.json", content: bundleBytes}} {
		if err := retainDocument(callCtx, document.name, document.content); err != nil {
			return result, err
		}
	}
	admit := func(admissionCtx context.Context, phase string, call winnerMultipassCall, payload []byte) (winnerMultipassAdmission, error) {
		value, err := hooks.Admit(admissionCtx, phase, call, payload)
		result.Custody.Invocations = append(result.Custody.Invocations, value.Custody)
		if err == nil {
			err = validateWinnerHandoffInvocation(value.Custody, phase, "tokenize", call, winnerMultipassSha256(payload), value.Admission, "", plan)
		}
		return value.Admission, err
	}
	admissions, err := preflightWinnerHandoffLeaves(callCtx, plan, bundle, func(admissionCtx context.Context, call winnerMultipassCall, payload []byte) (winnerMultipassAdmission, error) {
		return admit(admissionCtx, "leaf_preflight", call, payload)
	})
	result.Custody.LeafAdmissions = admissions
	admissionBytes, marshalErr := json.Marshal(admissions)
	if marshalErr != nil {
		return result, marshalErr
	}
	if writeErr := writePrivateReviewFile(filepath.Join(directory, "leaf-admissions.json"), admissionBytes); writeErr != nil {
		return result, writeErr
	}
	if err != nil {
		return result, err
	}
	completed := map[string]winnerMultipassCallEvidence{}
	for index, call := range plan.Calls {
		if !hooks.Now().Before(owner.Deadline) {
			return result, errors.New("original handoff deadline expired; no renewal or draw")
		}
		payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, completed)
		if err != nil {
			return result, err
		}
		// The final preflight leaves the other member resident. Re-admit every
		// exact payload before its claim; generation never loads a model itself.
		admission, err := admit(callCtx, "call_admission", call, payload)
		if err != nil {
			return result, err
		}
		if err := validateWinnerMultipassAdmission(plan, call, payload, admission); err != nil {
			return result, err
		}
		claim := model.CompetitionReviewHandoffCall{HandoffId: owner.HandoffId, CallId: call.Id, Kind: "claim", RequestSha256: winnerMultipassSha256(payload)}
		claimed, err := hooks.Claim(callCtx, claim)
		if err != nil {
			return result, err
		}
		if claimed == nil || claimed.HandoffId != claim.HandoffId || claimed.CallId != claim.CallId || claimed.RequestSha256 != claim.RequestSha256 || claimed.RecordedAt.Before(owner.StartedAt) || !claimed.RecordedAt.Before(owner.Deadline) {
			return result, errors.New("provider call lacks its exact committed database claim")
		}
		prefix := fmt.Sprintf("call-%04d", index+1)
		claimBytes, _ := json.Marshal(claimed)
		if err := writePrivateReviewFile(filepath.Join(directory, prefix+".claim.json"), claimBytes); err != nil {
			return result, err
		}
		if err := writePrivateReviewFile(filepath.Join(directory, prefix+".request.json"), payload); err != nil {
			return result, err
		}
		transport, invokeErr := hooks.Invoke(callCtx, call, payload, admission, winnerMultipassSha256(claimBytes))
		result.Custody.Invocations = append(result.Custody.Invocations, transport.Custody)
		if invokeErr == nil {
			invokeErr = validateWinnerHandoffInvocation(transport.Custody, "capture", "generate", call, winnerMultipassSha256(payload), admission, transport.TransportResponseSha256, plan)
		}
		observation := winnerMultipassCallEvidence{CallId: call.Id, StartedAt: claimed.RecordedAt, CompletedAt: hooks.Now().UTC(), Request: payload, Admission: admission,
			VisibleResponse: transport.VisibleResponse, VisibleResponseSha256: winnerMultipassSha256(transport.VisibleResponse), TransportResponseSha256: transport.TransportResponseSha256, TransportReceiptSha256: transport.ContainmentReceiptSha256, Error: transport.Error}
		if invokeErr != nil && observation.Error == "" {
			observation.Error = "qualified_transport_failed"
		}
		if observation.Error == "" {
			var visible winnerModelResponse
			if decodeStrictJSONBytes(observation.VisibleResponse, &visible, "captured visible terminal") != nil {
				observation.Error = "malformed_visible_terminal"
			} else {
				for _, item := range visible.Output {
					if item.Type == "message" && item.Role == "assistant" {
						for _, part := range item.Content {
							if part.Type == "output_text" {
								observation.ProviderOutput += part.Text
							}
						}
					}
				}
				if decodeStrictJSONBytes([]byte(observation.ProviderOutput), &observation.Observation, "captured model observation") != nil {
					observation.Error = "malformed_model_observation"
				}
			}
		}
		result.Evidence.Calls = append(result.Evidence.Calls, observation)
		_, validationErr := validateWinnerMultipassCompletedPrefix(plan, bundle, result.Evidence)
		if validationErr != nil && observation.Error == "" {
			observation.Error = "native_call_evidence_validation_failed"
			result.Evidence.Calls[len(result.Evidence.Calls)-1] = observation
		}
		observationBytes, err := json.Marshal(observation)
		if err != nil {
			return result, err
		}
		if err := writePrivateReviewFile(filepath.Join(directory, prefix+".evidence.json"), observationBytes); err != nil {
			return result, err
		}
		// Retain receipt errors after expiry under a separate bounded cleanup
		// context. No cleanup operation may issue or retry a provider request.
		retentionCtx, retentionCancel := context.WithTimeout(context.WithoutCancel(ctx), settings.ReceiptTimeout)
		claim.Kind, claim.EvidenceSha256, claim.Failure = "receipt", winnerMultipassSha256(observationBytes), observation.Error
		_, receiptErr := hooks.Receipt(retentionCtx, claim)
		retentionCancel()
		if receiptErr != nil {
			return result, receiptErr
		}
		if observation.Error != "" || invokeErr != nil || validationErr != nil {
			return result, errors.New("original model call failed or is unresolved; exact evidence retained, no resampling")
		}
		completed[call.Id] = observation
	}
	judgment, err := validateWinnerMultipassEvidence(plan, bundle, result.Evidence)
	if err != nil {
		return result, err
	}
	evidenceBytes, err := json.Marshal(result.Evidence)
	if err != nil {
		return result, err
	}
	if err := writePrivateReviewFile(filepath.Join(directory, "complete-evidence.json"), evidenceBytes); err != nil {
		return result, err
	}
	if err := retainDocument(callCtx, "core-evidence.json", evidenceBytes); err != nil {
		return result, err
	}
	if err := validateWinnerHandoffCustody(owner, plan, bundle, result.Evidence, result.Custody); err != nil {
		return result, err
	}
	custodyBytes, err := json.Marshal(result.Custody)
	if err != nil || len(custodyBytes) > settings.MaximumDocumentBytes {
		return result, errors.New("complete custody manifest exceeds admitted object bound")
	}
	if err := writePrivateReviewFile(filepath.Join(directory, "custody.json"), custodyBytes); err != nil {
		return result, err
	}
	retained, err := retainWinnerHandoffDocument(callCtx, "custody.json", custodyBytes, settings.MaximumDocumentBytes, owner.Deadline, hooks.Retain)
	if err != nil {
		return result, err
	}
	completion := model.CompetitionReviewHandoffCompletion{HandoffId: owner.HandoffId, PlanSha256: owner.Plan.PlanSha256, Evidence: retained.Retention}
	broker := winnerSnowBrokerResult(bundle.Request, judgment, custodyBytes)
	if judgment.Decision == "approve" || judgment.Decision == "disqualify" {
		if err := model.ValidateCompetitionIntegrityReview(broker.Review, bundle.Request.Request.Policy); err != nil {
			completion.Failure = "native_final_proposal_invalid"
		} else {
			completion.Proposal = &broker.Review
			completion.ProposalSha256, _ = model.CompetitionAutomationDigest(broker.Review)
		}
	} else {
		completion.Failure = "model_judgment_uncertain"
	}
	result.Completion, err = hooks.Complete(callCtx, completion)
	if err != nil {
		return result, err
	}
	result.Status = "MODEL_FINDINGS_PENDING_OPERATOR"
	if completion.Proposal == nil {
		result.Status = "UNRESOLVED_RETAINED"
	}
	return result, nil
}

// Authenticated artifact identity is checked before constructing a fresh owner.
// Plan bytes, original full source and every retained visible call stay private.
func winnerHandoffPlan(owner model.CompetitionReviewHandoff, bundle winnerMultipassBundle, settings winnerMultipassSettings, provider winnerSnowProviderSettings, groups []winnerMultipassScanGroup) (*winnerMultipassPlan, winnerMultipassBundle, error) {
	bundle.Request.Request.StartedAt, bundle.Request.Request.Deadline = owner.StartedAt, owner.Deadline
	bundle.Request.Template.HandoffId = owner.HandoffId.String()
	bundle.Request.Template.IdempotencyKey = "handoff-" + owner.HandoffId.String()
	plan, err := prepareWinnerMultipass(bundle, winnerMultipassOwner{Id: owner.HandoffId.String(), StartedAt: owner.StartedAt, Deadline: owner.Deadline}, settings, provider)
	if err != nil {
		return nil, bundle, err
	}
	if groups != nil {
		if err := setWinnerMultipassScanGroups(plan, groups); err != nil {
			return nil, bundle, err
		}
	}
	return plan, bundle, validateWinnerMultipassPlan(plan, bundle)
}

// Byte-exact comparison, not a local JSON approval marker, binds the durable
// completion to evidence being presented for later explicit human import.
func validateWinnerHandoffCompletion(owner *model.CompetitionReviewHandoff, completion *model.CompetitionReviewHandoffCompletion, plan *winnerMultipassPlan, bundle winnerMultipassBundle, evidence winnerMultipassEvidence, custody winnerHandoffCustodyManifest) error {
	if err := validateWinnerHandoffIdentity(owner, plan, bundle); err != nil {
		return err
	}
	if owner == nil || completion == nil || completion.HandoffId != owner.HandoffId || completion.PlanSha256 != owner.Plan.PlanSha256 || completion.CompletedAt.Before(owner.StartedAt) || !completion.CompletedAt.Before(owner.Deadline) {
		return errors.New("handoff completion is absent, stale or outside its fixed clock")
	}
	judgment, err := validateWinnerMultipassEvidence(plan, bundle, evidence)
	if err != nil {
		return err
	}
	if err := validateWinnerHandoffCustody(owner, plan, bundle, evidence, custody); err != nil {
		return err
	}
	content, err := json.Marshal(custody)
	if err != nil {
		return err
	}
	if completion.Failure != "" || winnerMultipassSha256(content) != completion.Evidence.Sha256 || completion.Evidence.Key == "" || completion.Evidence.VersionId == "" {
		return errors.New("handoff completion evidence differs")
	}
	if completion.Proposal == nil {
		return errors.New("retained model findings remain unresolved")
	}
	actual := winnerSnowBrokerResult(bundle.Request, judgment, content)
	wantBytes, _ := json.Marshal(actual.Review)
	storedBytes, _ := json.Marshal(completion.Proposal)
	if !bytes.Equal(wantBytes, storedBytes) {
		return errors.New("pending proposal differs from original model findings")
	}
	if winnerMultipassSha256(storedBytes) != completion.ProposalSha256 {
		return errors.New("pending proposal digest differs")
	}
	return model.ValidateCompetitionIntegrityReview(actual.Review, bundle.Request.Request.Policy)
}
