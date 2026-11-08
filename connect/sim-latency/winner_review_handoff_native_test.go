// The native adapter is the only boundary that receives main DB/archive access.
// Its child receives one prepared tool-free payload, never those credentials,
// source-repository mounts, an operator token or permission to execute a patch.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The final protected model observations must match every original DB receipt,
// not only a valid-looking local completion file or provider wrapper manifest.
func validateWinnerHandoffJournal(owner *model.CompetitionReviewHandoff, plan *winnerMultipassPlan, evidence winnerMultipassEvidence, calls []model.CompetitionReviewHandoffCall) error {
	if owner == nil || plan == nil || len(calls) != 2*len(plan.Calls) || len(evidence.Calls) != len(plan.Calls) {
		return errors.New("native handoff lacks its complete original DB call journal")
	}
	for index, call := range plan.Calls {
		claim, receipt, observation := calls[2*index], calls[2*index+1], evidence.Calls[index]
		content, _ := json.Marshal(observation)
		requestSha := winnerMultipassSha256(observation.Request)
		if claim.HandoffId != owner.HandoffId || receipt.HandoffId != owner.HandoffId || claim.CallId != call.Id || receipt.CallId != call.Id || observation.CallId != call.Id || claim.Kind != "claim" || receipt.Kind != "receipt" ||
			claim.RequestSha256 != requestSha || receipt.RequestSha256 != requestSha || receipt.EvidenceSha256 != winnerMultipassSha256(content) || claim.Failure != "" || receipt.Failure != "" || !claim.RecordedAt.Equal(observation.StartedAt) ||
			claim.RecordedAt.Before(owner.StartedAt) || !claim.RecordedAt.Before(owner.Deadline) || receipt.RecordedAt.Before(claim.RecordedAt) || !receipt.RecordedAt.Before(owner.Deadline) {
			return errors.New("native model evidence differs from its exact ordered DB claim/receipt")
		}
	}
	return nil
}

// Reload every protected original and replay physical containment while its
// original local attempt still exists. Eviction is intentionally not supported
// by this path; a later portable replay must preserve the same original proofs.
func reloadWinnerHandoffProtectedCustody(ctx context.Context, control *controller.Settings, owner *model.CompetitionReviewHandoff, plan *winnerMultipassPlan, custody winnerHandoffCustodyManifest, completion model.CompetitionReviewHandoffRetention, release winnerMultipassQualificationRelease, calls []model.CompetitionReviewHandoffCall) error {
	manifest, err := json.Marshal(custody)
	if err != nil {
		return err
	}
	stored, err := controller.LoadCompetitionReviewHandoffEvidence(ctx, control, owner.Args.RoundId, owner.Args.JobId, completion, int64(len(manifest)))
	if err != nil || !bytes.Equal(stored, manifest) {
		return errors.New("protected completion manifest is not its exact retained version")
	}
	for _, document := range custody.Documents {
		if _, err := controller.LoadCompetitionReviewHandoffEvidence(ctx, control, owner.Args.RoundId, owner.Args.JobId, document.Retention, document.Bytes); err != nil {
			return err
		}
	}
	claimShas := map[string]string{}
	for _, call := range calls {
		if call.Kind == "claim" {
			content, _ := json.Marshal(call)
			claimShas[call.CallId] = winnerMultipassSha256(content)
		}
	}
	profile := winnerMultipassContainmentProfile{ImageId: release.ImageId, ProviderSettings: release.ProviderSettings, StateRoot: "/var/lib/urnetwork/competition-review-multipass/attempts"}
	for ordinal, invocation := range custody.Invocations {
		originals := map[string][]byte{}
		bindings := map[string]winnerHandoffCustodyOriginal{}
		for _, original := range invocation.Originals {
			content, err := controller.LoadCompetitionReviewHandoffEvidence(ctx, control, owner.Args.RoundId, owner.Args.JobId, original.Retention, original.Bytes)
			if err != nil {
				return err
			}
			originals[original.Name], bindings[original.Name] = content, original
		}
		var envelope winnerMultipassBrokerEnvelope
		if decodeStrictJSONBytes(originals["input.json"], &envelope, "protected original model input") != nil || envelope.Owner != plan.Owner || envelope.PlanSha256 != owner.Plan.PlanSha256 || envelope.BundleSha256 != plan.BundleSha256 ||
			envelope.Operation != invocation.Operation || envelope.Call.Id != invocation.CallId || winnerMultipassSha256(envelope.Payload) != invocation.RequestSha256 ||
			envelope.Operation == "generate" && envelope.AuthorizedClaimSha256 != claimShas[envelope.Call.Id] {
			return errors.New("protected original input differs from the exact owner/plan/DB claim")
		}
		if !slices.ContainsFunc(plan.Calls, func(call winnerMultipassCall) bool { return reflect.DeepEqual(call, envelope.Call) }) {
			return errors.New("protected input contains an unplanned member call")
		}
		if custody.SpoolCapacity != nil {
			capacityBytes, _ := json.Marshal(custody.SpoolCapacity)
			binding := winnerHandoffSpoolBinding{RunId: plan.Owner.Id, InvocationPosition: ordinal + 1, PlanSha256: owner.Plan.PlanSha256, BundleSha256: plan.BundleSha256,
				ReleaseSha256: custody.Provenance.ReleaseSha256, NativeDriverSha256: release.NativeDriver.Sha256, CapacitySha256: winnerMultipassSha256(capacityBytes)}
			if err := reloadWinnerHandoffSpoolInvocation(ctx, invocation, binding, release, profile, winnerHandoffProtectedReader(control, owner)); err != nil {
				return err
			}
			continue
		}
		receipt, err := authenticateWinnerMultipassContainment(originals["input.json"], originals["broker.stdout"], originals["wrapper-receipt.json"], false, profile)
		if err != nil || receipt.ManifestSha256 != invocation.ManifestSha256 || receipt.WrapperReceiptSha256 != invocation.WrapperReceiptSha256 || len(receipt.Artifacts)+1 != len(originals) {
			return errors.New("protected original containment does not replay exactly")
		}
		for _, binding := range receipt.Artifacts {
			original, found := bindings[binding.Path]
			if !found || original.Bytes != binding.Bytes || original.Retention.Sha256 != binding.Sha256 {
				return errors.New("protected containment original census differs from its authenticated manifest")
			}
		}
	}
	return nil
}

// Archive versions, not temporary local paths, own these original bytes. This
// helper is called only after the qualified authenticator has accepted a receipt.
func retainWinnerHandoffContainment(ctx context.Context, phase string, envelope winnerMultipassBrokerEnvelope, receipt *winnerMultipassContainmentReceipt, retain func(context.Context, []byte, string) (*controller.CompetitionWinnerReviewEvidence, error)) (winnerHandoffInvocationCustody, error) {
	value := winnerHandoffInvocationCustody{Phase: phase, Operation: envelope.Operation, CallId: envelope.Call.Id, RequestSha256: winnerMultipassSha256(envelope.Payload), Originals: []winnerHandoffCustodyOriginal{}}
	if receipt == nil || !receipt.Canceled && receipt.Result == nil || receipt.Canceled && receipt.Result != nil || !validSha256(receipt.ManifestSha256) || !validSha256(receipt.WrapperReceiptSha256) || len(receipt.Artifacts) == 0 || len(receipt.Artifacts) > 32 {
		return value, errors.New("native custody has no independently authenticated complete attempt")
	}
	value.ManifestSha256, value.WrapperReceiptSha256 = receipt.ManifestSha256, receipt.WrapperReceiptSha256
	if receipt.Result != nil {
		value.Failure = receipt.Result.Error
	}
	seen := map[string]bool{}
	for _, binding := range receipt.Artifacts {
		path, err := winnerMultipassContainmentArtifactPath(receipt, binding)
		if err != nil || seen[binding.Path] {
			return value, errors.New("authenticated wrapper original census is ambiguous")
		}
		seen[binding.Path] = true
		limit := winnerMultipassCapturedLeafLimit(binding.Path, envelope.Settings)
		content, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: path, Sha256: binding.Sha256}, limit, true)
		if err != nil || int64(len(content)) != binding.Bytes {
			return value, errors.New("authenticated wrapper original changed before protected custody")
		}
		original, err := retainWinnerHandoffOriginal(ctx, binding.Path, content, int(limit), envelope.Owner.Deadline, retain)
		if err != nil {
			return value, err
		}
		value.Originals = append(value.Originals, original)
	}
	if winnerMultipassSha256(receipt.WrapperReceipt) != receipt.WrapperReceiptSha256 {
		return value, errors.New("authenticated wrapper receipt bytes changed")
	}
	original, err := retainWinnerHandoffDocument(ctx, "wrapper-receipt.json", receipt.WrapperReceipt, defaultWinnerMultipassProviderLimits().MaximumMetadataBytes, envelope.Owner.Deadline, retain)
	if err == nil {
		value.Originals = append(value.Originals, original)
	}
	return value, err
}

// One process owns one fixed capture. No resumption, retry, operator import or
// local-cache eviction is implicit. Close releases only the empty reservation.
func captureWinnerHandoffNative(ctx context.Context, directory string, owner *model.CompetitionReviewHandoff, plan *winnerMultipassPlan, bundle winnerMultipassBundle, release winnerMultipassQualificationRelease, qualification, authorization, captureAdmission currentQgQualificationPin, settings winnerHandoffCaptureSettings, storagePaths []string, control *controller.Settings, store controller.PostgresStore) (result *winnerHandoffCaptureResult, resultErr error) {
	if err := validateWinnerHandoffIdentity(owner, plan, bundle); err != nil {
		return nil, err
	}
	if err := authenticateWinnerMultipassReleaseArtifacts(release); err != nil {
		return nil, err
	}
	if err := authenticateWinnerHandoffQualification(ctx, qualification, release, settings.Spool != nil, control, owner.Args); err != nil {
		return nil, err
	}
	authorityBytes, err := readCurrentQgQualificationPin(authorization, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
	if err != nil {
		return nil, err
	}
	provenance, err := validateWinnerHandoffRunAuthorization(authorityBytes, authorization, owner.ActorId, owner.Args, release, qualification, plan.ScanGroups, settings, storagePaths, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	admissionBytes, err := readCurrentQgQualificationPin(captureAdmission, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
	if err != nil {
		return nil, err
	}
	admission, err := validateWinnerHandoffCaptureAdmission(admissionBytes, owner, plan, bundle, provenance, settings, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	for _, pin := range admission.ResourceProofs {
		if _, err := readCurrentQgQualificationPin(pin, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true); err != nil {
			return nil, err
		}
	}
	provenance.CaptureAdmissionSha256 = captureAdmission.Sha256
	actual, calls, completion, err := store.ReviewHandoff(ctx, control, owner.Args.RoundId, owner.HandoffId)
	if err != nil || !reflect.DeepEqual(actual, owner) || len(calls) != 0 || completion != nil {
		return nil, errors.New("native capture requires its exact existing DB owner with no previous claim or terminal outcome")
	}
	profile := winnerMultipassContainmentProfile{ImageId: release.ImageId, ProviderSettings: release.ProviderSettings, StateRoot: "/var/lib/urnetwork/competition-review-multipass/attempts"}
	for _, required := range []string{directory, os.TempDir(), profile.StateRoot, filepath.Dir(profile.StateRoot)} {
		if !slices.Contains(storagePaths, required) {
			return nil, errors.New("native storage reservation omits an actual wrapper, capture or archive staging root")
		}
	}
	var storage *winnerHandoffStorage
	var spoolCapacity *winnerMultipassSpoolCapacity
	spoolDirectory := filepath.Join(directory, "protected-spool")
	defer func() {
		if storage != nil {
			resultErr = errors.Join(resultErr, storage.Close())
		}
	}()
	retain := func(retentionCtx context.Context, content []byte, digest string) (*controller.CompetitionWinnerReviewEvidence, error) {
		return controller.RetainCompetitionReviewHandoffEvidence(retentionCtx, control, owner.Args.RoundId, owner.Args.JobId, content, digest)
	}
	resident, ordinal := "", 0
	// Every attempt is fully joined and retained before this function returns.
	invoke := func(callCtx context.Context, phase, operation string, call winnerMultipassCall, payload []byte, admission *winnerMultipassAdmission, claimSha string) (*winnerMultipassBrokerResult, winnerHandoffInvocationCustody, error) {
		custody := winnerHandoffInvocationCustody{Phase: phase, Operation: operation, CallId: call.Id, RequestSha256: winnerMultipassSha256(payload), Originals: []winnerHandoffCustodyOriginal{}}
		if storage == nil || operation == "generate" && (admission == nil || !validSha256(claimSha) || resident != call.Member.Name) {
			return nil, custody, errors.New("native invocation lacks reserved capacity, exact claim or resident model")
		}
		allowance, err := winnerMultipassInvocationCapacity(plan, call, operation, len(payload), settings.Capacity)
		if settings.Spool != nil {
			allowance, err = winnerMultipassSpoolInvocationCapacity(plan, call, operation, len(payload), settings.Capacity)
		}
		if err != nil {
			return nil, custody, err
		}
		if settings.Spool != nil {
			if spoolCapacity == nil || allowance > spoolCapacity.ActiveInvocationBytes {
				return nil, custody, errors.New("native invocation exceeds its fixed single-invocation spool window")
			}
			err = storage.Borrow(allowance)
		} else {
			err = storage.Consume(allowance)
		}
		if err != nil {
			return nil, custody, err
		}
		limits := defaultWinnerMultipassProviderLimits()
		timeout := limits.GenerationTimeout
		if operation == "tokenize" {
			timeout = limits.TokenizerTimeout
		}
		deadline := time.Now().UTC().Add(timeout)
		if owner.Deadline.Before(deadline) {
			deadline = owner.Deadline
		}
		envelope := winnerMultipassBrokerEnvelope{Schema: 1, Kind: "winner-review-multipass-call/v1", Operation: operation, Owner: plan.Owner, CallDeadline: deadline,
			PlanSha256: owner.Plan.PlanSha256, BundleSha256: plan.BundleSha256, Call: call, Payload: payload, Settings: plan.Settings, Admission: admission, AuthorizedClaimSha256: claimSha,
			AllowModelLoad: operation == "tokenize" && resident != call.Member.Name}
		input, err := json.Marshal(envelope)
		if err != nil {
			return nil, custody, err
		}
		bounded, cancel := context.WithDeadline(callCtx, deadline)
		output, diagnostics, invokeErr := invokeWinnerMultipassBroker(bounded, release.Wrapper, input)
		cancel()
		ordinal++
		var receipt *winnerMultipassContainmentReceipt
		var physical *winnerMultipassPhysicalAuthentication
		var independentClaim []byte
		var authErr error
		if settings.Spool != nil {
			physical, receipt, independentClaim, authErr = createWinnerMultipassPhysicalAuthentication(input, output, diagnostics, false, profile, release)
		} else {
			receipt, authErr = authenticateWinnerMultipassContainment(input, output, diagnostics, false, profile)
		}
		retentionCtx, retentionCancel := context.WithTimeout(context.WithoutCancel(ctx), settings.ReceiptTimeout)
		defer retentionCancel()
		if authErr != nil {
			// Failed authentication never discards the original bounded host I/O.
			// The raw wrapper attempt remains in place for scoped investigation.
			failure, _ := json.Marshal(struct {
				Input, Output, Diagnostics []byte
			}{Input: input, Output: output, Diagnostics: diagnostics})
			name := fmt.Sprintf("unverified-invocation-%04d.json", ordinal)
			writeErr := writePrivateReviewFile(filepath.Join(directory, name), failure)
			original, retainErr := retainWinnerHandoffDocument(retentionCtx, name, failure, settings.MaximumDocumentBytes, owner.Deadline, retain)
			if retainErr == nil {
				custody.Originals = append(custody.Originals, original)
			}
			custody.Failure = "containment_authentication_failed"
			return nil, custody, errors.Join(invokeErr, authErr, writeErr, retainErr)
		}
		var retainErr error
		if settings.Spool != nil {
			capacityBytes, _ := json.Marshal(spoolCapacity)
			captureBytes, encodeErr := json.Marshal(struct {
				Input, Output, Diagnostics []byte
				Receipt                    *winnerMultipassContainmentReceipt
			}{Input: input, Output: output, Diagnostics: diagnostics, Receipt: receipt})
			if encodeErr != nil {
				return receipt.Result, custody, encodeErr
			}
			binding := winnerHandoffSpoolBinding{RunId: plan.Owner.Id, InvocationPosition: ordinal, PlanSha256: owner.Plan.PlanSha256, BundleSha256: plan.BundleSha256,
				ReleaseSha256: provenance.ReleaseSha256, NativeDriverSha256: release.NativeDriver.Sha256, CapacitySha256: winnerMultipassSha256(capacityBytes)}
			custody, retainErr = protectWinnerHandoffSpoolInvocation(retentionCtx, spoolDirectory, phase, binding, envelope, receipt, physical, independentClaim, captureBytes, release, retain, winnerHandoffProtectedReader(control, owner))
			if retainErr == nil {
				retainErr = storage.Restore()
			}
		} else {
			custody, retainErr = retainWinnerHandoffContainment(retentionCtx, phase, envelope, receipt, retain)
		}
		if receipt.Result == nil || receipt.Result.Error != "" {
			return receipt.Result, custody, errors.Join(invokeErr, retainErr, errors.New("original contained operation failed or is unresolved"))
		}
		if operation == "tokenize" && invokeErr == nil && retainErr == nil {
			resident = call.Member.Name
		}
		return receipt.Result, custody, errors.Join(invokeErr, retainErr)
	}
	hooks := winnerHandoffCaptureHooks{Now: func() time.Time { return time.Now().UTC() }, Retain: retain, Provenance: provenance,
		ReserveSpool: func(_ context.Context, capacity *winnerMultipassCapacity, spool *winnerMultipassSpoolCapacity) (winnerHandoffCapacityReservation, error) {
			if err := os.Mkdir(spoolDirectory, 0700); err != nil {
				return winnerHandoffCapacityReservation{}, err
			}
			var proof winnerHandoffCapacityReservation
			var err error
			storage, proof, err = reserveWinnerHandoffStorageForLifecycle(directory, storagePaths, capacity, spool)
			if err == nil {
				err = storage.Consume(spool.PermanentDocumentsBytes + spool.CompactManifestBytes)
			}
			spoolCapacity = spool
			return proof, err
		},
		Reserve: func(_ context.Context, capacity *winnerMultipassCapacity) (winnerHandoffCapacityReservation, error) {
			var proof winnerHandoffCapacityReservation
			var err error
			storage, proof, err = reserveWinnerHandoffStorage(directory, storagePaths, capacity)
			if err == nil {
				err = storage.Consume(capacity.DocumentAndTemporaryBytes)
			}
			return proof, err
		},
		Admit: func(admissionCtx context.Context, phase string, call winnerMultipassCall, payload []byte) (winnerHandoffTokenization, error) {
			result, custody, err := invoke(admissionCtx, phase, "tokenize", call, payload, nil, "")
			value := winnerHandoffTokenization{Custody: custody}
			if result != nil && result.Admission != nil {
				value.Admission = *result.Admission
			}
			return value, err
		},
		Invoke: func(callCtx context.Context, call winnerMultipassCall, payload []byte, admission winnerMultipassAdmission, claimSha string) (winnerHandoffTransport, error) {
			result, custody, err := invoke(callCtx, "capture", "generate", call, payload, &admission, claimSha)
			value := winnerHandoffTransport{Custody: custody, ContainmentReceiptSha256: custody.WrapperReceiptSha256}
			if result != nil {
				value.VisibleResponse, value.TransportResponseSha256, value.Error = result.VisibleResponse, result.TransportResponseSha256, result.Error
			}
			return value, err
		},
		Claim: func(claimCtx context.Context, value model.CompetitionReviewHandoffCall) (*model.CompetitionReviewHandoffCall, error) {
			return store.RecordReviewHandoffCall(claimCtx, control, owner.Args.RoundId, owner.ActorId, value)
		},
		Receipt: func(receiptCtx context.Context, value model.CompetitionReviewHandoffCall) (*model.CompetitionReviewHandoffCall, error) {
			return store.RecordReviewHandoffCall(receiptCtx, control, owner.Args.RoundId, owner.ActorId, value)
		},
		Complete: func(completionCtx context.Context, value model.CompetitionReviewHandoffCompletion) (*model.CompetitionReviewHandoffCompletion, error) {
			return store.CompleteReviewHandoff(completionCtx, control, owner.Args.RoundId, owner.ActorId, value)
		},
	}
	return captureWinnerHandoff(ctx, directory, owner, plan, bundle, settings, hooks)
}
