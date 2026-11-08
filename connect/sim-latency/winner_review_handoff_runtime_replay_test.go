// The normal release gate accepts only completed typed bootstrap captures whose
// original root claims and actual contained files still replay. Arbitrary pinned
// bytes, declared aliases or fabricated runtime digests cannot qualify context.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"time"
)

type winnerHandoffRuntimeCapture struct {
	Schema                 int                                    `json:"schema"`
	Kind                   string                                 `json:"kind"`
	BootstrapRelease       winnerMultipassQualificationRelease    `json:"bootstrap_release"`
	Owner                  winnerMultipassOwner                   `json:"owner"`
	OwnerClaim             currentQgQualificationPin              `json:"owner_claim"`
	Claim                  currentQgQualificationPin              `json:"claim"`
	Input                  []byte                                 `json:"input"`
	Output                 []byte                                 `json:"output"`
	Diagnostics            []byte                                 `json:"diagnostics"`
	Containment            *winnerMultipassContainmentReceipt     `json:"containment"`
	PhysicalAuthentication *winnerMultipassPhysicalAuthentication `json:"physical_authentication"`
	Custody                winnerHandoffInvocationCustody         `json:"protected_originals"`
	Error                  string                                 `json:"error"`
}

// Only offline tests inject these seams. The native entry below always opens the
// independent root claims and verifies actual inode/UID/cgroup/fence originals.
type winnerHandoffRuntimeReplayHooks struct {
	ReadClaim    func(currentQgQualificationPin) ([]byte, error)
	Authenticate func([]byte, []byte, []byte, winnerMultipassContainmentProfile) (*winnerMultipassContainmentReceipt, error)
	ReadOriginal func(*winnerMultipassContainmentReceipt, currentQgQualificationBinding) ([]byte, error)
}

func authenticateWinnerHandoffRuntimeEvidence(content []byte, normalRelease winnerMultipassQualificationRelease, member winnerSnowMember) error {
	return validateWinnerHandoffRuntimeEvidence(content, normalRelease, member, winnerHandoffRuntimeReplayHooks{
		ReadClaim: func(pin currentQgQualificationPin) ([]byte, error) {
			if err := currentQgQualificationPrivateRoot(filepath.Dir(pin.Path)); err != nil {
				return nil, err
			}
			return readCurrentQgQualificationPin(pin, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
		},
		Authenticate: func(input, output, diagnostics []byte, profile winnerMultipassContainmentProfile) (*winnerMultipassContainmentReceipt, error) {
			return authenticateWinnerMultipassContainment(input, output, diagnostics, false, profile)
		},
		ReadOriginal: func(receipt *winnerMultipassContainmentReceipt, binding currentQgQualificationBinding) ([]byte, error) {
			path, err := winnerMultipassContainmentArtifactPath(receipt, binding)
			if err != nil {
				return nil, err
			}
			return readCurrentQgQualificationPin(currentQgQualificationPin{Path: path, Sha256: binding.Sha256}, winnerMultipassCapturedLeafLimit(binding.Path, defaultWinnerMultipassSettings()), true)
		},
	})
}

func validateWinnerHandoffRuntimeEvidence(content []byte, normalRelease winnerMultipassQualificationRelease, member winnerSnowMember, hooks winnerHandoffRuntimeReplayHooks) error {
	if validateWinnerMultipassQualificationReleaseShape(normalRelease) != nil || !slices.Contains(defaultWinnerSnowProviderSettings().Members, member) || hooks.ReadClaim == nil || hooks.Authenticate == nil || hooks.ReadOriginal == nil || len(content) == 0 || len(content) > winnerMultipassSpoolCaptureBytes {
		return errors.New("runtime evidence has no exact observed release/member or native replay hooks")
	}
	if normalRelease.TokenizationProfiles[member.Name].RuntimeEvidenceSha256 != winnerMultipassSha256(content) {
		return errors.New("normal runtime profile is not the exact completed original capture")
	}
	var capture winnerHandoffRuntimeCapture
	if rejectWinnerReviewCaseAliases(content) != nil || decodeStrictJSONBytes(content, &capture, "typed original runtime capture") != nil || capture.Schema != 1 || capture.Kind != "winner-review-multipass-runtime-capture/v1" || capture.Error != "" || capture.Containment == nil || capture.PhysicalAuthentication == nil || validateWinnerMultipassRuntimeProbeReleaseShape(capture.BootstrapRelease) != nil {
		return errors.New("runtime evidence is partial, failed, untyped or self-declared")
	}
	canonical, _ := json.Marshal(capture)
	if !bytes.Equal(content, canonical) {
		return errors.New("runtime evidence is not the complete canonical original capture")
	}
	expectedRelease := normalRelease
	expectedRelease.TokenizationProfiles = capture.BootstrapRelease.TokenizationProfiles
	expectedRelease.DeploymentDeclaration = capture.BootstrapRelease.DeploymentDeclaration
	if !reflect.DeepEqual(expectedRelease, capture.BootstrapRelease) {
		return errors.New("runtime bootstrap changed release source, executable, container or qualification pins")
	}
	var envelope winnerMultipassBrokerEnvelope
	if decodeStrictJSONBytes(capture.Input, &envelope, "original runtime probe input") != nil || envelope.Kind != winnerMultipassRuntimeProbeKind || envelope.Call.Member != member || envelope.Owner != capture.Owner {
		return errors.New("runtime capture belongs to a different probe/member/owner")
	}
	releaseBytes, _ := json.Marshal(capture.BootstrapRelease)
	ownerRoot := "/var/lib/urnetwork/competition-review-multipass/qualification-owners"
	if !validSha256(capture.Owner.Id) || capture.OwnerClaim.Path != filepath.Join(ownerRoot, capture.Owner.Id+".runtime.json") || capture.Claim.Path != filepath.Join(ownerRoot, capture.Owner.Id+"."+member.Name+".runtime.json") {
		return errors.New("runtime capture lacks independent permanent fixed-namespace intent claims")
	}
	ownerBytes, err := hooks.ReadClaim(capture.OwnerClaim)
	if err != nil {
		return err
	}
	var ownerClaim struct {
		Schema              int                  `json:"schema"`
		Kind                string               `json:"kind"`
		ConfigurationSha256 string               `json:"configuration_sha256"`
		PermissionSha256    string               `json:"permission_sha256"`
		UserApprovalSha256  string               `json:"user_approval_sha256"`
		Owner               winnerMultipassOwner `json:"owner"`
		ReleaseSha256       string               `json:"release_sha256"`
	}
	if rejectWinnerReviewCaseAliases(ownerBytes) != nil || decodeStrictJSONBytes(ownerBytes, &ownerClaim, "independent runtime owner claim") != nil || winnerMultipassSha256(ownerBytes) != capture.OwnerClaim.Sha256 || ownerClaim.Schema != 1 || ownerClaim.Kind != "winner-review-multipass-runtime-owner/v1" || ownerClaim.Owner != capture.Owner || ownerClaim.ReleaseSha256 != winnerMultipassSha256(releaseBytes) || !validSha256(ownerClaim.ConfigurationSha256) || !validSha256(ownerClaim.PermissionSha256) || !validSha256(ownerClaim.UserApprovalSha256) {
		return errors.New("runtime owner claim changed the original permission/configuration/release clock")
	}
	ownerIdentity, _ := json.Marshal(struct {
		Approval string
		Release  string
	}{Approval: ownerClaim.UserApprovalSha256, Release: ownerClaim.ReleaseSha256})
	if capture.Owner.Id != winnerMultipassSha256(ownerIdentity) {
		return errors.New("runtime owner was reset under the same approved release")
	}
	claimBytes, err := hooks.ReadClaim(capture.Claim)
	if err != nil {
		return err
	}
	var claim struct {
		Schema      int                  `json:"schema"`
		Kind        string               `json:"kind"`
		Owner       winnerMultipassOwner `json:"owner"`
		Member      winnerSnowMember     `json:"member"`
		InputSha256 string               `json:"input_sha256"`
		StartedAt   time.Time            `json:"started_at"`
	}
	if rejectWinnerReviewCaseAliases(claimBytes) != nil || decodeStrictJSONBytes(claimBytes, &claim, "independent runtime call claim") != nil || winnerMultipassSha256(claimBytes) != capture.Claim.Sha256 || claim.Schema != 1 || claim.Kind != "winner-review-multipass-runtime-call/v1" || claim.Owner != capture.Owner || claim.Member != member || claim.InputSha256 != winnerMultipassSha256(capture.Input) || validateWinnerMultipassRuntimeProbeEnvelope(envelope, claim.StartedAt, defaultWinnerMultipassProviderLimits()) != nil {
		return errors.New("runtime call input or bounded operation differs from its original independent intent")
	}
	profile := winnerMultipassContainmentProfile{ImageId: normalRelease.ImageId, ProviderSettings: normalRelease.ProviderSettings, StateRoot: "/var/lib/urnetwork/competition-review-multipass/attempts"}
	receipt, err := hooks.Authenticate(capture.Input, capture.Output, capture.Diagnostics, profile)
	if err != nil {
		return err
	}
	if receipt == nil || !reflect.DeepEqual(receipt, capture.Containment) || receipt.Canceled || receipt.Result == nil || receipt.Result.Error != "" || receipt.Result.Admission != nil || receipt.Result.RuntimeObservation == nil || receipt.Result.StartedAt.Before(claim.StartedAt) {
		return errors.New("runtime original containment/result differs or failed after its durable intent")
	}
	proof := capture.PhysicalAuthentication
	receiptBytes, _ := json.Marshal(receipt)
	if proof.Schema != 1 || proof.Kind != "winner-review-physical-containment-authentication/v1" || proof.Protocol != winnerMultipassVersion || proof.ReleaseSha256 != winnerMultipassSha256(releaseBytes) || proof.NativeDriver != capture.BootstrapRelease.NativeDriver || proof.Profile != profile || proof.Canceled || proof.IndependentClaimSha256 != "" || proof.ObservedAt.Before(receipt.Result.CompletedAt) || proof.InputSha256 != winnerMultipassSha256(capture.Input) || proof.OutputSha256 != winnerMultipassSha256(capture.Output) || proof.ManifestSha256 != receipt.ManifestSha256 || proof.WrapperReceiptSha256 != receipt.WrapperReceiptSha256 || proof.AuthenticatedReceiptSha256 != winnerMultipassSha256(receiptBytes) {
		return errors.New("runtime physical authentication is not the exact pinned driver/original receipt")
	}
	custody := capture.Custody
	if custody.Phase != "runtime_probe" || custody.Operation != "tokenize" || custody.CallId != envelope.Call.Id || custody.RequestSha256 != winnerMultipassSha256(envelope.Payload) || custody.ManifestSha256 != receipt.ManifestSha256 || custody.WrapperReceiptSha256 != receipt.WrapperReceiptSha256 || custody.Failure != "" || custody.SpoolCommit != nil || custody.SpoolReferencePath != "" || custody.SpoolReferenceSha256 != "" || len(custody.Originals) != len(receipt.Artifacts)+1 {
		return errors.New("runtime custody omitted or relabeled permanent original artifacts")
	}
	originals := map[string]winnerHandoffCustodyOriginal{}
	for _, original := range custody.Originals {
		if _, duplicate := originals[original.Name]; duplicate {
			return errors.New("runtime custody repeats an original")
		}
		if err := validateWinnerHandoffOriginal(original, capture.Owner.Deadline, int64(defaultWinnerHandoffCaptureSettings().MaximumDocumentBytes)); err != nil {
			return err
		}
		originals[original.Name] = original
	}
	for _, binding := range receipt.Artifacts {
		original, found := originals[binding.Path]
		if !found || original.Bytes != binding.Bytes || original.Retention.Sha256 != binding.Sha256 {
			return errors.New("runtime protected descriptor differs from actual original manifest")
		}
		value, err := hooks.ReadOriginal(receipt, binding)
		if err != nil || int64(len(value)) != binding.Bytes || winnerMultipassSha256(value) != binding.Sha256 {
			return errors.New("runtime actual original disappeared or changed after physical authentication")
		}
	}
	wrapper, found := originals["wrapper-receipt.json"]
	if !found || wrapper.Bytes != int64(len(receipt.WrapperReceipt)) || wrapper.Retention.Sha256 != receipt.WrapperReceiptSha256 || winnerMultipassSha256(receipt.WrapperReceipt) != receipt.WrapperReceiptSha256 {
		return errors.New("runtime original wrapper receipt lost protected custody")
	}
	return nil
}
