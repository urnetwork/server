// A genuine release gate replays retained original calls and all nineteen
// controls. A label, copied flat report or model list is never qualification.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
)

// The exact root-owned claim binds its original payload/admission, fixed group,
// operation ordinal and half-open owner/call clock before provider dispatch.
func validateWinnerMultipassQualificationClaim(content []byte, plan *winnerMultipassQualificationPlan, capture winnerMultipassQualificationCapture) error {
	var claim winnerMultipassQualificationClaim
	if rejectWinnerReviewCaseAliases(content) != nil || decodeStrictJSONBytes(content, &claim, "original multi-pass qualification claim") != nil {
		return errors.New("qualification original claim malformed")
	}
	var envelope winnerMultipassBrokerEnvelope
	if decodeStrictJSONBytes(capture.Input, &envelope, "claimed original envelope") != nil {
		return errors.New("qualification claim input malformed")
	}
	planBytes, _ := json.Marshal(plan)
	admissionBytes, _ := json.Marshal(envelope.Admission)
	if claim.Schema != 1 || claim.GroupSha256 != winnerMultipassSha256(planBytes) || !reflect.DeepEqual(claim.Invocation, capture.Invocation) || claim.Owner != envelope.Owner || claim.PayloadSha256 != winnerMultipassSha256(envelope.Payload) || claim.AdmissionSha256 != winnerMultipassSha256(admissionBytes) || claim.CallDeadline != envelope.CallDeadline ||
		claim.StartedAt.Before(plan.Owner.StartedAt) || !claim.StartedAt.Before(plan.Owner.Deadline) || !claim.CallDeadline.After(claim.StartedAt) || claim.CallDeadline.After(plan.Owner.Deadline) || claim.CallDeadline.Sub(claim.StartedAt) > defaultWinnerMultipassProviderLimits().GenerationTimeout ||
		capture.Claim.Sha256 != winnerMultipassSha256(content) || envelope.Operation == "generate" && envelope.AuthorizedClaimSha256 != capture.Claim.Sha256 {
		return errors.New("qualification claim substituted its owner, input, operation or deadline")
	}
	if capture.Containment != nil && capture.Containment.Result != nil && capture.Containment.Result.StartedAt.Before(claim.StartedAt) {
		return errors.New("provider input preceded its original durable claim")
	}
	return nil
}

// Read root-owned original bytes, not a convenience copy or selected transcript.
// Native callers separately authenticate this release's source/binary/build/unit
// pins before capture; the report must exactly match those expected same pins.
func authenticateWinnerMultipassQualification(reportPin currentQgQualificationPin, expectedRelease winnerMultipassQualificationRelease) (*winnerMultipassQualificationReport, error) {
	if err := validateWinnerMultipassQualificationReleaseShape(expectedRelease); err != nil {
		return nil, err
	}
	content, err := readCurrentQgQualificationPin(reportPin, int64(defaultWinnerMultipassProviderLimits().MaximumResultBytes), true)
	if err != nil {
		return nil, err
	}
	var report winnerMultipassQualificationReport
	if rejectWinnerReviewCaseAliases(content) != nil || decodeStrictJSONBytes(content, &report, "original complete-coverage qualification report") != nil {
		return nil, errors.New("new multi-pass qualification report malformed")
	}
	if report.Schema != 1 || report.Kind != "winner-review-multipass-qualification-report/v1" || report.Protocol != winnerMultipassVersion || report.Mode != "genuine_provider" || !report.Complete || !report.Qualified || report.Error != "" || report.Plan.Sha256 != report.PlanSha256 || !reflect.DeepEqual(report.Release, expectedRelease) {
		return nil, errors.New("qualification report is partial, offline, historical, failed or for a different release")
	}
	planBytes, err := readCurrentQgQualificationPin(report.Plan, 8*int64(defaultWinnerMultipassSettings().MaximumBundleBytes), true)
	if err != nil {
		return nil, err
	}
	var plan winnerMultipassQualificationPlan
	if decodeStrictJSONBytes(planBytes, &plan, "original complete-coverage qualification plan") != nil || validateWinnerMultipassQualificationPlan(&plan) != nil || !reflect.DeepEqual(plan.Release, expectedRelease) || plan.Owner != report.Owner || len(report.Captures) != len(plan.Invocations) || len(report.CaseEvidence) != 18 || report.Capacity == nil || report.Capacity.Cases[2] == nil {
		return nil, errors.New("qualification report omitted or substituted its fixed plan, owner or complete census")
	}
	canonicalPlan, _ := json.Marshal(plan)
	if !bytes.Equal(planBytes, canonicalPlan) {
		return nil, errors.New("qualification plan pin is not the exact canonical precommit")
	}
	preparationCtx, preparationCancel := context.WithTimeout(context.Background(), defaultWinnerMultipassProviderLimits().TokenizerTimeout)
	inputs, inputsErr := authenticateWinnerMultipassQualificationInputs(preparationCtx, expectedRelease)
	preparationCancel()
	if inputsErr != nil {
		return nil, inputsErr
	}
	if err := validateWinnerMultipassQualificationOriginalInputs(&plan, inputs); err != nil {
		return nil, err
	}
	capacity, err := estimateWinnerMultipassQualificationCapacity(&plan, report.Capacity.Cases[2].Settings)
	if err != nil || !reflect.DeepEqual(capacity, report.Capacity) {
		return nil, errors.New("qualification aggregate resource bounds were omitted or changed")
	}
	profile := winnerMultipassContainmentProfile{ImageId: expectedRelease.ImageId, ProviderSettings: expectedRelease.ProviderSettings, StateRoot: "/var/lib/urnetwork/competition-review-multipass/attempts"}
	state := newWinnerMultipassQualificationState(&plan)
	for index, invocation := range plan.Invocations {
		captureBytes, err := readCurrentQgQualificationPin(report.Captures[index], 32*1024*1024, true)
		if err != nil {
			return nil, err
		}
		var capture winnerMultipassQualificationCapture
		if rejectWinnerReviewCaseAliases(captureBytes) != nil || decodeStrictJSONBytes(captureBytes, &capture, "original complete-coverage invocation capture") != nil {
			return nil, errors.New("qualification capture is not its exact original record")
		}
		claimBytes, err := readCurrentQgQualificationPin(capture.Claim, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(claimBytes, capture.ClaimContent) {
			return nil, errors.New("retained qualification capture replaced its independent root intent")
		}
		if err := validateWinnerMultipassQualificationClaim(claimBytes, &plan, capture); err != nil {
			return nil, err
		}
		if err := applyWinnerMultipassQualificationCapture(&plan, invocation, capture, state, func(input, output, diagnostics []byte, canceled bool) (*winnerMultipassContainmentReceipt, error) {
			return authenticateWinnerMultipassContainment(input, output, diagnostics, canceled, profile)
		}); err != nil {
			return nil, err
		}
	}
	delete(state.Evidence, 1)
	if err := validateWinnerMultipassQualificationOutcomes(&plan, state.Evidence); err != nil {
		return nil, err
	}
	for _, item := range plan.Cases[1:] {
		pin, found := report.CaseEvidence[item.Position]
		if !found {
			return nil, errors.New("qualification complete-case custody is absent")
		}
		bound := capacity.Cases[item.Position].SerializedEvidenceBytes + 1024*1024
		actual, err := readCurrentQgQualificationPin(pin, bound, true)
		if err != nil {
			return nil, err
		}
		original, _ := json.Marshal(state.Evidence[item.Position])
		if !bytes.Equal(original, actual) {
			return nil, errors.New("qualification retained case evidence is not reconstructed from every original invocation")
		}
	}
	return &report, nil
}

// A root-owned native adapter can use this explicit descendant check when
// retaining the new authenticator's fixed leaves. It is not path-only custody:
// callers still verify content hash, size, private owner and protected readback.
func winnerMultipassContainmentArtifactPath(receipt *winnerMultipassContainmentReceipt, binding currentQgQualificationBinding) (string, error) {
	if receipt == nil || binding.Path == "" || filepath.Base(binding.Path) != binding.Path || !validSha256(binding.Sha256) || binding.Bytes < 0 || !filepath.IsAbs(receipt.AttemptDirectory) || filepath.Clean(receipt.AttemptDirectory) != receipt.AttemptDirectory {
		return "", errors.New("invalid exact containment artifact descriptor")
	}
	found := false
	for _, original := range receipt.Artifacts {
		if original == binding {
			found = true
			break
		}
	}
	if !found {
		return "", errors.New("artifact was not authenticated by this exact containment receipt")
	}
	return filepath.Join(receipt.AttemptDirectory, binding.Path), nil
}
