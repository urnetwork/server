// The native adapter is the only boundary that receives main DB/archive access.
// Its child receives one prepared tool-free payload, never those credentials,
// source-repository mounts, an operator token or permission to execute a patch.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Archive versions, not temporary local paths, own these original bytes. This
// helper is called only after the qualified authenticator has accepted a receipt.
func retainWinnerHandoffContainment(ctx context.Context, phase string, envelope winnerMultipassBrokerEnvelope, receipt *winnerMultipassContainmentReceipt, retain func(context.Context, []byte, string) (*controller.CompetitionWinnerReviewEvidence, error)) (winnerHandoffInvocationCustody, error) {
	value := winnerHandoffInvocationCustody{Phase: phase, Operation: envelope.Operation, CallId: envelope.Call.Id, RequestSha256: winnerMultipassSha256(envelope.Payload), Originals: []winnerHandoffCustodyOriginal{}}
	if receipt == nil || receipt.Canceled || receipt.Result == nil || !validSha256(receipt.ManifestSha256) || !validSha256(receipt.WrapperReceiptSha256) || len(receipt.Artifacts) == 0 || len(receipt.Artifacts) > 32 {
		return value, errors.New("native custody has no independently authenticated complete attempt")
	}
	value.ManifestSha256, value.WrapperReceiptSha256, value.Failure = receipt.ManifestSha256, receipt.WrapperReceiptSha256, receipt.Result.Error
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
func captureWinnerHandoffNative(ctx context.Context, directory string, owner *model.CompetitionReviewHandoff, plan *winnerMultipassPlan, bundle winnerMultipassBundle, release winnerMultipassQualificationRelease, settings winnerHandoffCaptureSettings, storagePaths []string, control *controller.Settings, store controller.PostgresStore) (result *winnerHandoffCaptureResult, resultErr error) {
	if err := validateWinnerHandoffIdentity(owner, plan, bundle); err != nil {
		return nil, err
	}
	profile := winnerMultipassContainmentProfile{ImageId: release.ImageId, ProviderSettings: release.ProviderSettings, StateRoot: "/var/lib/urnetwork/competition-review-multipass/attempts"}
	for _, required := range []string{directory, os.TempDir(), profile.StateRoot, filepath.Dir(profile.StateRoot)} {
		if !slices.Contains(storagePaths, required) {
			return nil, errors.New("native storage reservation omits an actual wrapper, capture or archive staging root")
		}
	}
	var storage *winnerHandoffStorage
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
		if err != nil {
			return nil, custody, err
		}
		if err := storage.Consume(allowance); err != nil {
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
		receipt, authErr := authenticateWinnerMultipassContainment(input, output, diagnostics, false, profile)
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
		custody, retainErr := retainWinnerHandoffContainment(retentionCtx, phase, envelope, receipt, retain)
		if receipt.Result == nil || receipt.Result.Error != "" {
			return receipt.Result, custody, errors.Join(invokeErr, retainErr, errors.New("original contained operation failed or is unresolved"))
		}
		if operation == "tokenize" && invokeErr == nil && retainErr == nil {
			resident = call.Member.Name
		}
		return receipt.Result, custody, errors.Join(invokeErr, retainErr)
	}
	hooks := winnerHandoffCaptureHooks{Now: func() time.Time { return time.Now().UTC() }, Retain: retain,
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
