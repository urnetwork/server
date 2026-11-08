// Physical capture and protected portable replay are different trust boundaries.
// Rehydrated files are never presented as original inode, UID or process facts.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Native archive adapters must verify COMPLIANCE retention and exact-version
// readback. A content digest without a retained version is not protected custody.
type winnerMultipassProtectedReference struct {
	Key         string    `json:"key"`
	VersionId   string    `json:"version_id"`
	Sha256      string    `json:"sha256"`
	Bytes       int64     `json:"bytes"`
	RetainUntil time.Time `json:"retain_until"`
}

// This is a statement by the exact trusted driver after actual physical checks,
// not by the provider. Its protected exact-version provenance is required before
// a portable reader may rely on those prior filesystem observations.
type winnerMultipassPhysicalAuthentication struct {
	Schema                     int                               `json:"schema"`
	Kind                       string                            `json:"kind"`
	Protocol                   string                            `json:"protocol"`
	ReleaseSha256              string                            `json:"release_sha256"`
	NativeDriver               currentQgQualificationPin         `json:"native_driver"`
	Profile                    winnerMultipassContainmentProfile `json:"profile"`
	ObservedAt                 time.Time                         `json:"observed_at"`
	InputSha256                string                            `json:"input_sha256"`
	OutputSha256               string                            `json:"output_sha256"`
	ManifestSha256             string                            `json:"manifest_sha256"`
	WrapperReceiptSha256       string                            `json:"wrapper_receipt_sha256"`
	AuthenticatedReceiptSha256 string                            `json:"authenticated_receipt_sha256"`
	IndependentClaimSha256     string                            `json:"independent_generation_claim_sha256,omitempty"`
	Canceled                   bool                              `json:"canceled"`
}

// Unexported fields cannot be populated by decoding an untrusted JSON flag.
// Only the exact protected-readback constructor below produces this capability.
type winnerMultipassAuthenticatedPhysicalProof struct {
	proof     winnerMultipassPhysicalAuthentication
	retention winnerMultipassProtectedReference
	validated bool
}

// Read exact private originals only from the original wrapper-owned paths.
func readWinnerMultipassPhysicalOriginals(input, diagnostics []byte, settings currentQgQualificationSettings) (map[string][]byte, []byte, currentQgQualificationWrapperReceipt, error) {
	var envelope winnerMultipassBrokerEnvelope
	var receipt currentQgQualificationWrapperReceipt
	if decodeStrictJSONBytes(input, &envelope, "original physical input") != nil || validateWinnerMultipassSettings(envelope.Settings) != nil {
		return nil, nil, receipt, errors.New("original physical input is malformed")
	}
	lines := bytes.Split(bytes.TrimSpace(diagnostics), []byte("\n"))
	if len(lines) == 0 || decodeStrictJSONBytes(lines[len(lines)-1], &receipt, "original physical wrapper receipt") != nil || receipt.Schema != 1 || receipt.Kind != "winner-review-container-receipt" || !validSha256(receipt.Owner) ||
		!receipt.CleanupVerified || filepath.Dir(receipt.AttemptDirectory) != settings.StateRoot || !strings.HasPrefix(receipt.AttemptDirectory, settings.StateRoot+"/invocation.") {
		return nil, nil, receipt, errors.New("original physical receipt escaped its owned state root")
	}
	for _, path := range []string{settings.StateRoot, receipt.AttemptDirectory} {
		if err := currentQgQualificationPrivateRoot(path); err != nil {
			return nil, nil, receipt, err
		}
	}
	manifest, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: filepath.Join(receipt.AttemptDirectory, "evidence-manifest.json"), Sha256: receipt.ManifestSha256}, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
	if err != nil {
		return nil, nil, receipt, err
	}
	var value struct {
		Schema    int                             `json:"schema"`
		Kind      string                          `json:"kind"`
		Artifacts []currentQgQualificationBinding `json:"artifacts"`
	}
	if decodeStrictJSONBytes(manifest, &value, "original physical manifest") != nil || value.Schema != 1 || value.Kind != "winner-review-container-evidence" || len(value.Artifacts) > 26 {
		return nil, nil, receipt, errors.New("original physical manifest is malformed")
	}
	originals := map[string][]byte{"evidence-manifest.json": manifest}
	for _, binding := range value.Artifacts {
		if _, exists := originals[binding.Path]; exists || filepath.Base(binding.Path) != binding.Path || binding.Bytes < 0 || binding.Bytes > winnerMultipassCapturedLeafLimit(binding.Path, envelope.Settings) || winnerMultipassCapturedLeafLimit(binding.Path, envelope.Settings) <= 0 {
			return nil, nil, receipt, errors.New("original physical manifest has an unknown, duplicate or oversized leaf")
		}
		content, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: filepath.Join(receipt.AttemptDirectory, binding.Path), Sha256: binding.Sha256}, winnerMultipassCapturedLeafLimit(binding.Path, envelope.Settings), true)
		if err != nil || int64(len(content)) != binding.Bytes {
			return nil, nil, receipt, errors.New("original physical leaf changed")
		}
		originals[binding.Path] = content
	}
	var claim []byte
	if envelope.Operation == "generate" {
		directory := filepath.Join(filepath.Dir(settings.StateRoot), "generation-claims")
		if err := currentQgQualificationPrivateRoot(directory); err != nil {
			return nil, nil, receipt, err
		}
		key := winnerMultipassSha256([]byte(envelope.Owner.Id + "/" + envelope.Call.Id))
		claim, err = readCurrentQgQualificationPin(currentQgQualificationPin{Path: filepath.Join(directory, key+".json"), Sha256: winnerMultipassSha256(originals["attempt-claim.json"])}, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
		if err != nil {
			return nil, nil, receipt, err
		}
	}
	return originals, claim, receipt, nil
}

// Capture keeps every previous physical check. Shared byte validation is pure;
// filesystem ownership, exact state census and durable claim are still observed.
func authenticateWinnerMultipassContainmentInternal(input, output, diagnostics []byte, canceled bool, image string, settings currentQgQualificationSettings, profile currentQgQualificationProviderProfile) (string, error) {
	originals, claim, receipt, err := readWinnerMultipassPhysicalOriginals(input, diagnostics, settings)
	if err != nil {
		return "", err
	}
	digest, err := validateWinnerMultipassContainmentBytes(input, output, diagnostics, canceled, image, settings, profile, originals, claim)
	if err != nil {
		return "", err
	}
	if canceled {
		if err := validateCurrentQgSnowQualificationStateCensus(filepath.Join(receipt.AttemptDirectory, "member-state"), 0); err != nil {
			return "", err
		}
	} else {
		var envelope winnerMultipassBrokerEnvelope
		var result winnerMultipassBrokerResult
		if decodeStrictJSONBytes(input, &envelope, "physically authenticated envelope") != nil || decodeStrictJSONBytes(output, &result, "physically authenticated result") != nil {
			return "", errors.New("physical journal input/output changed")
		}
		if err := validateWinnerMultipassJournalFilesystem(originals, receipt.AttemptDirectory, envelope, result); err != nil {
			return "", err
		}
	}
	return digest, nil
}

// The creator refuses a different executable even when supplied a plausible
// driver hash. Native release authentication remains separately mandatory.
func createWinnerMultipassPhysicalAuthentication(input, output, diagnostics []byte, canceled bool, profile winnerMultipassContainmentProfile, release winnerMultipassQualificationRelease) (*winnerMultipassPhysicalAuthentication, *winnerMultipassContainmentReceipt, []byte, error) {
	if err := validateWinnerMultipassQualificationReleaseShape(release); err != nil {
		return nil, nil, nil, err
	}
	executable, err := os.Executable()
	if err != nil || executable != release.NativeDriver.Path || verifySourceSupervisorProgram(executable, release.NativeDriver.Sha256) != nil || profile.ImageId != release.ImageId || profile.ProviderSettings != release.ProviderSettings {
		return nil, nil, nil, errors.New("physical authentication is not executing the pinned native release driver")
	}
	if err := validateCurrentQgQualificationRuntime(); err != nil {
		return nil, nil, nil, err
	}
	receipt, err := authenticateWinnerMultipassContainment(input, output, diagnostics, canceled, profile)
	if err != nil {
		return nil, nil, nil, err
	}
	settings := defaultCurrentQgQualificationSettings()
	settings.StateRoot = profile.StateRoot
	_, claim, _, err := readWinnerMultipassPhysicalOriginals(input, diagnostics, settings)
	if err != nil {
		return nil, nil, nil, err
	}
	releaseBytes, _ := json.Marshal(release)
	receiptBytes, _ := json.Marshal(receipt)
	proof := &winnerMultipassPhysicalAuthentication{Schema: 1, Kind: "winner-review-physical-containment-authentication/v1", Protocol: winnerMultipassVersion, ReleaseSha256: winnerMultipassSha256(releaseBytes), NativeDriver: release.NativeDriver, Profile: profile, ObservedAt: time.Now().UTC(),
		InputSha256: winnerMultipassSha256(input), OutputSha256: winnerMultipassSha256(output), ManifestSha256: receipt.ManifestSha256, WrapperReceiptSha256: receipt.WrapperReceiptSha256, AuthenticatedReceiptSha256: winnerMultipassSha256(receiptBytes), Canceled: canceled}
	if len(claim) != 0 {
		proof.IndependentClaimSha256 = winnerMultipassSha256(claim)
	}
	return proof, receipt, claim, nil
}

// The callback must perform actual protected exact-version COMPLIANCE readback.
// No local file, hash-only record or self-asserted physical boolean is accepted.
func loadWinnerMultipassPhysicalAuthentication(ctx context.Context, reference winnerMultipassProtectedReference, release winnerMultipassQualificationRelease, minimumRetention time.Time, readback func(context.Context, winnerMultipassProtectedReference) ([]byte, error)) (*winnerMultipassAuthenticatedPhysicalProof, error) {
	if validateWinnerMultipassQualificationReleaseShape(release) != nil || readback == nil || reference.Key == "" || reference.VersionId == "" || !validSha256(reference.Sha256) || reference.Bytes <= 0 || reference.Bytes > int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes) || minimumRetention.IsZero() || reference.RetainUntil.Before(minimumRetention) {
		return nil, errors.New("portable physical proof lacks protected exact-version custody")
	}
	content, err := readback(ctx, reference)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != reference.Bytes || winnerMultipassSha256(content) != reference.Sha256 {
		return nil, errors.New("protected physical proof readback differs from its exact retained version")
	}
	var proof winnerMultipassPhysicalAuthentication
	if rejectWinnerReviewCaseAliases(content) != nil || decodeStrictJSONBytes(content, &proof, "protected original physical authentication") != nil {
		return nil, errors.New("protected physical proof is malformed")
	}
	releaseBytes, _ := json.Marshal(release)
	if proof.Schema != 1 || proof.Kind != "winner-review-physical-containment-authentication/v1" || proof.Protocol != winnerMultipassVersion || proof.ReleaseSha256 != winnerMultipassSha256(releaseBytes) || proof.NativeDriver != release.NativeDriver || proof.Profile.ImageId != release.ImageId || proof.Profile.ProviderSettings != release.ProviderSettings || proof.ObservedAt.IsZero() ||
		!validSha256(proof.InputSha256) || !validSha256(proof.OutputSha256) || !validSha256(proof.ManifestSha256) || !validSha256(proof.WrapperReceiptSha256) || !validSha256(proof.AuthenticatedReceiptSha256) || proof.IndependentClaimSha256 != "" && !validSha256(proof.IndependentClaimSha256) {
		return nil, errors.New("protected physical proof belongs to another release or lacks original identities")
	}
	return &winnerMultipassAuthenticatedPhysicalProof{proof: proof, retention: reference, validated: true}, nil
}

// Pure replay authenticates the immutable proof/manifest/census/raw bytes and
// repeats every semantic transport/container/journal check. Actual historical
// filesystem observations are relied on only through prior protected proof.
func replayWinnerMultipassPortableContainment(originals map[string][]byte, independentClaim []byte, authenticated *winnerMultipassAuthenticatedPhysicalProof, profile winnerMultipassContainmentProfile) (*winnerMultipassContainmentReceipt, error) {
	return replayWinnerMultipassPortableContainmentForProfile(originals, independentClaim, authenticated, profile, currentQgSnowQualificationProfile(profile.ProviderSettings))
}

// The provider-profile seam exists for reserved-address offline guard fixtures;
// production callers above always derive the fixed Snow-only LAN boundary.
func replayWinnerMultipassPortableContainmentForProfile(originals map[string][]byte, independentClaim []byte, authenticated *winnerMultipassAuthenticatedPhysicalProof, profile winnerMultipassContainmentProfile, provider currentQgQualificationProviderProfile) (*winnerMultipassContainmentReceipt, error) {
	if authenticated == nil || !authenticated.validated || authenticated.retention.VersionId == "" || !reflect.DeepEqual(authenticated.proof.Profile, profile) {
		return nil, errors.New("portable containment lacks protected prior physical authentication")
	}
	proof := authenticated.proof
	input, output, diagnostics := originals["input.json"], originals["broker.stdout"], originals["wrapper-receipt.json"]
	if proof.InputSha256 != winnerMultipassSha256(input) || proof.OutputSha256 != winnerMultipassSha256(output) || proof.WrapperReceiptSha256 != winnerMultipassSha256(diagnostics) || proof.ManifestSha256 != winnerMultipassSha256(originals["evidence-manifest.json"]) ||
		(len(independentClaim) == 0) != (proof.IndependentClaimSha256 == "") || len(independentClaim) != 0 && winnerMultipassSha256(independentClaim) != proof.IndependentClaimSha256 {
		return nil, errors.New("portable original input/output/manifest/claim changed")
	}
	bound := map[string][]byte{}
	for name, content := range originals {
		if name != "wrapper-receipt.json" {
			bound[name] = content
		}
	}
	settings := defaultCurrentQgQualificationSettings()
	settings.StateRoot = profile.StateRoot
	if _, err := validateWinnerMultipassContainmentBytes(input, output, diagnostics, proof.Canceled, profile.ImageId, settings, provider, bound, independentClaim); err != nil {
		return nil, err
	}
	var wrapper currentQgQualificationWrapperReceipt
	var manifest struct {
		Schema    int                             `json:"schema"`
		Kind      string                          `json:"kind"`
		Artifacts []currentQgQualificationBinding `json:"artifacts"`
	}
	if decodeStrictJSONBytes(diagnostics, &wrapper, "portable original wrapper receipt") != nil || decodeStrictJSONBytes(bound["evidence-manifest.json"], &manifest, "portable original manifest") != nil {
		return nil, errors.New("portable originals are malformed")
	}
	result := &winnerMultipassContainmentReceipt{AttemptDirectory: wrapper.AttemptDirectory, ManifestSha256: wrapper.ManifestSha256, WrapperReceiptSha256: proof.WrapperReceiptSha256, WrapperReceipt: append([]byte{}, diagnostics...), Artifacts: manifest.Artifacts, Canceled: proof.Canceled}
	result.Artifacts = append(result.Artifacts, currentQgQualificationBinding{Path: "evidence-manifest.json", Sha256: wrapper.ManifestSha256, Bytes: int64(len(bound["evidence-manifest.json"]))})
	if !proof.Canceled {
		result.Result = &winnerMultipassBrokerResult{}
		if err := decodeStrictJSONBytes(output, result.Result, "portable original result"); err != nil {
			return nil, err
		}
	}
	receiptBytes, _ := json.Marshal(result)
	if winnerMultipassSha256(receiptBytes) != proof.AuthenticatedReceiptSha256 {
		return nil, errors.New("portable reconstructed receipt differs from original physical authentication")
	}
	return result, nil
}
