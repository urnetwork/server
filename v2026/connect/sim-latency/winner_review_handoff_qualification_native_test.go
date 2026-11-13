// A genuine spool report is replayed through scoped exact-version archive
// reads. Compact pins never masquerade as full local provider capture records.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The complete group and resource digests come from the separately validated
// original report/plan. Every protected invocation must reproduce that scope.
func winnerHandoffQualificationSpoolResolver(release winnerMultipassQualificationRelease, groupSha, groupOwner, capacitySha string, read winnerHandoffSpoolReader) winnerMultipassQualificationSpoolResolver {
	return func(ctx context.Context, pin currentQgQualificationPin, compact []byte) (*winnerMultipassQualificationSpoolResolvedCapture, error) {
		if winnerMultipassSha256(compact) != pin.Sha256 || len(compact) > winnerMultipassSpoolManifestBytes || !validSha256(groupSha) || groupOwner == "" || !validSha256(capacitySha) {
			return nil, errors.New("qualification spool resolver lacks its exact bounded group/resource pin")
		}
		var reference winnerHandoffSpoolReference
		if rejectWinnerReviewCaseAliases(compact) != nil || decodeStrictJSONBytes(compact, &reference, "qualification compact spool reference") != nil {
			return nil, errors.New("qualification compact spool reference is malformed")
		}
		releaseBytes, _ := json.Marshal(release)
		binding := reference.Binding
		if binding.PlanSha256 != groupSha || binding.RunId != groupOwner || binding.ReleaseSha256 != winnerMultipassSha256(releaseBytes) || binding.NativeDriverSha256 != release.NativeDriver.Sha256 || binding.CapacitySha256 != capacitySha {
			return nil, errors.New("qualification compact spool reference changed its fixed group/release/resource scope")
		}
		profile := winnerMultipassContainmentProfile{ImageId: release.ImageId, ProviderSettings: release.ProviderSettings, StateRoot: "/var/lib/urnetwork/competition-review-multipass/attempts"}
		protected, err := loadWinnerHandoffSpool(ctx, pin.Path, binding, profile.StateRoot, read, winnerHandoffSpoolPortableVerifier(ctx, release, profile, read))
		if err != nil {
			return nil, err
		}
		referenceBytes, _ := json.Marshal(protected.reference)
		if !bytes.Equal(referenceBytes, compact) {
			return nil, errors.New("qualification compact original changed during protected resolution")
		}
		originals, err := replayWinnerHandoffSpool(ctx, protected.commit, read, winnerHandoffSpoolPortableVerifier(ctx, release, profile, read))
		if err != nil {
			return nil, err
		}
		captureBytes := originals["invocation-capture.json"]
		var capture winnerMultipassQualificationCapture
		if len(captureBytes) > winnerMultipassSpoolCaptureBytes || rejectWinnerReviewCaseAliases(captureBytes) != nil || decodeStrictJSONBytes(captureBytes, &capture, "original protected qualification capture") != nil ||
			capture.Invocation.Position != binding.InvocationPosition || capture.Invocation.CasePosition < 1 || capture.Invocation.CasePosition > 19 {
			return nil, errors.New("qualification full capture differs from its exact protected invocation")
		}
		var claim winnerMultipassQualificationClaim
		var envelope winnerMultipassBrokerEnvelope
		if decodeStrictJSONBytes(capture.ClaimContent, &claim, "original qualification claim") != nil || decodeStrictJSONBytes(capture.Input, &envelope, "original qualification envelope") != nil ||
			claim.GroupSha256 != groupSha || claim.Invocation != capture.Invocation || claim.Owner != envelope.Owner || envelope.BundleSha256 != binding.BundleSha256 ||
			envelope.Owner.Id != winnerMultipassSha256([]byte(fmt.Sprintf("%s/control-%02d", groupOwner, capture.Invocation.CasePosition))) ||
			capture.Containment == nil || capture.Containment.ManifestSha256 != protected.commit.Invocation.ManifestSha256 {
			return nil, errors.New("qualification protected capture substituted another group/case/claim")
		}
		independent, err := readCurrentQgQualificationPin(capture.Claim, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
		if err != nil || !bytes.Equal(independent, capture.ClaimContent) {
			return nil, errors.New("qualification independent native one-shot claim disappeared or changed")
		}
		physical := protected.commit.Physical
		physicalRef := winnerMultipassProtectedReference{Key: physical.Retention.Key, VersionId: physical.Retention.VersionId, Sha256: physical.Retention.Sha256, Bytes: physical.Bytes, RetainUntil: physical.Retention.RetainUntil}
		proof, err := loadWinnerMultipassPhysicalAuthentication(ctx, physicalRef, release, protected.commit.MinimumUntil, func(ctx context.Context, value winnerMultipassProtectedReference) ([]byte, error) {
			return read(ctx, winnerHandoffCustodyOriginal{Name: "physical-authentication.json", Bytes: value.Bytes, Retention: model.CompetitionReviewHandoffRetention{Key: value.Key, VersionId: value.VersionId, Sha256: value.Sha256, RetainUntil: value.RetainUntil}})
		})
		if err != nil {
			return nil, err
		}
		value := &winnerMultipassQualificationSpoolResolvedCapture{Commit: pin, Capture: &capture, Originals: map[string][]byte{}, IndependentGenerationClaim: originals["independent-generation-claim.json"], PhysicalAuthentication: proof}
		for _, original := range protected.commit.Invocation.Originals {
			value.Originals[original.Name] = originals[original.Name]
		}
		for _, original := range protected.commit.Extra {
			if original.Name == "invocation-capture.json" {
				value.CaptureReference = winnerMultipassProtectedReference{Key: original.Retention.Key, VersionId: original.Retention.VersionId, Sha256: original.Retention.Sha256, Bytes: original.Bytes, RetainUntil: original.Retention.RetainUntil}
			}
		}
		if err := validateWinnerMultipassResolvedSpoolCapture(value, envelope.Owner); err != nil {
			return nil, err
		}
		return value, nil
	}
}

// Spool mode accepts only its new typed report and scoped protected resolver.
// Full-local mode retains its old physical gate; neither accepts historical flat
// model reports or substitutes an absent local original with unchecked bytes.
func authenticateWinnerHandoffQualification(ctx context.Context, pin currentQgQualificationPin, release winnerMultipassQualificationRelease, spool bool, control *controller.Settings, args model.CompetitionReviewHandoffArgs) error {
	if !spool {
		_, err := authenticateWinnerMultipassQualification(pin, release)
		return err
	}
	content, err := readCurrentQgQualificationPin(pin, int64(defaultWinnerMultipassProviderLimits().MaximumResultBytes), true)
	if err != nil {
		return err
	}
	var report winnerMultipassQualificationSpoolReport
	if decodeStrictJSONBytes(content, &report, "protected-spool qualification report scope") != nil || report.Kind != "winner-review-multipass-qualification-spool-report/v1" || report.SpoolCapacity == nil || !reflect.DeepEqual(report.Release, release) {
		return errors.New("protected-spool candidate requires the exact new genuine qualification report")
	}
	capacityBytes, _ := json.Marshal(report.SpoolCapacity)
	read := winnerHandoffProtectedReader(control, &model.CompetitionReviewHandoff{Args: args})
	resolve := winnerHandoffQualificationSpoolResolver(release, report.PlanSha256, report.Owner.Id, winnerMultipassSha256(capacityBytes), read)
	_, err = authenticateWinnerMultipassSpoolQualification(pin, release, resolve)
	return err
}
