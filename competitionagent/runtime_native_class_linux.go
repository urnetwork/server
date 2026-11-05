// A piped core handler has no generic waiver. The prospective tracked class
// requires separate retained native qualification and root review, bound to
// the exact runtime/release/kernel. This file never creates accepted evidence.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
)

const runtimeNativeClassProfile = "tracked-create-core-one/v1"

// A v1 identity field length, not a configurable kernel or memory limit.
const runtimeNativeKernelReleaseBytes = 256

// A selection is static protected host authority, never a patched job identity.
// Its receipt is excluded from the canonical runtime digest to avoid a hash
// cycle; every other boundary/release field remains bound by that digest.
type RuntimeNativeClassSelection struct {
	Profile string                       `json:"profile"`
	Receipt Pin                          `json:"receipt"`
	Release RuntimeNativeReleaseIdentity `json:"release"`
}

// These are the same unpatched product/runtime inputs used for every job on
// one host. Per-job source commits and patch bytes retain their separate gate.
type RuntimeNativeReleaseIdentity struct {
	HostId                    string               `json:"host_id"`
	EvaluatorImageDigest      string               `json:"evaluator_image_digest"`
	SourceLockSha256          string               `json:"source_lock_sha256"`
	ConfigLocalSha256         string               `json:"config_local_sha256"`
	VaultLocalSha256          string               `json:"vault_local_sha256"`
	SubmissionBuilderSha256   string               `json:"submission_builder_sha256"`
	Agent                     Pin                  `json:"agent"`
	Docker                    Pin                  `json:"docker"`
	Compose                   Pin                  `json:"compose"`
	Buildx                    Pin                  `json:"buildx"`
	JsonFormatter             Pin                  `json:"json_formatter"`
	SubmissionBuilderIdentity Pin                  `json:"submission_builder_identity"`
	StockSeccompProfile       Pin                  `json:"stock_seccomp_profile"`
	CandidateCoreProfile      Pin                  `json:"candidate_core_profile"`
	Assets                    []RuntimeNativeAsset `json:"assets"`
}

// A finite logical asset name binds its actual installed protected file.
type RuntimeNativeAsset struct {
	Name string `json:"name"`
	Pin  Pin    `json:"pin"`
}

// Raw kernel bytes are hashed without whitespace normalization. Release is
// also explicit so a receipt cannot silently equate different distro builds.
type RuntimeNativeKernelIdentity struct {
	Release           string `json:"release"`
	VersionSha256     string `json:"version_sha256"`
	CorePatternSha256 string `json:"core_pattern_sha256"`
	CorePipeLimit     int    `json:"core_pipe_limit"`
}

// Acceptance selects two independently retained sealed documents. A native
// probe or self-reported pass document alone has no admission authority.
type runtimeNativeClassAcceptance struct {
	Schema                int                         `json:"schema"`
	Kind                  string                      `json:"kind"`
	Profile               string                      `json:"profile"`
	RuntimeIdentitySha256 string                      `json:"runtime_identity_sha256"`
	Kernel                RuntimeNativeKernelIdentity `json:"kernel"`
	Qualification         Pin                         `json:"qualification"`
	OperatorReview        Pin                         `json:"operator_review"`
}

// Every required trial has its own class-bound receipt. The retained artifact
// manifests are exact bounded files; interpretation/acceptance is independent
// operator review, not an automatic consequence of a probe exit status.
type runtimeNativeClassQualification struct {
	Schema                int                         `json:"schema"`
	Kind                  string                      `json:"kind"`
	Profile               string                      `json:"profile"`
	RuntimeIdentitySha256 string                      `json:"runtime_identity_sha256"`
	Kernel                RuntimeNativeKernelIdentity `json:"kernel"`
	Trials                []runtimeNativeTrial        `json:"trials"`
}

// A required trial name selects one immutable class-bound result record.
type runtimeNativeTrial struct {
	Name    string `json:"name"`
	Receipt Pin    `json:"receipt"`
}

// Result identity and artifact retention must match the reviewed class exactly.
type runtimeNativeTrialReceipt struct {
	Schema                int                         `json:"schema"`
	Kind                  string                      `json:"kind"`
	Profile               string                      `json:"profile"`
	RuntimeIdentitySha256 string                      `json:"runtime_identity_sha256"`
	Kernel                RuntimeNativeKernelIdentity `json:"kernel"`
	Trial                 string                      `json:"trial"`
	Outcome               string                      `json:"outcome"`
	ArtifactManifest      Pin                         `json:"artifact_manifest"`
}

// Only a separately pinned root review can accept the exact qualification.
// There is no writer, auto-accept command or caller-provided accepted flag.
type runtimeNativeClassReview struct {
	Schema                int                         `json:"schema"`
	Kind                  string                      `json:"kind"`
	Profile               string                      `json:"profile"`
	RuntimeIdentitySha256 string                      `json:"runtime_identity_sha256"`
	Kernel                RuntimeNativeKernelIdentity `json:"kernel"`
	QualificationSha256   string                      `json:"qualification_sha256"`
	Decision              string                      `json:"decision"`
	ReviewEvidence        Pin                         `json:"review_evidence"`
}

// Native implementations fix protected descriptor reads and the exact private
// daemon endpoint. Synthetic tests cannot change the public constructor.
type runtimeNativeClassHost interface {
	NativeClassRecord(Pin, int64, RuntimeResourceScope) ([]byte, error)
	NativeClassKernel(context.Context, RuntimeResourceBoundarySettings) (RuntimeNativeKernelIdentity, error)
	NativeClassImage(context.Context, RuntimeResourceBoundary, string, RuntimeResourceBoundarySettings) ([]byte, error)
}

// A successful receipt is necessary, not a renewable runtime/kernel lease.
// Every public admission loads it afresh; live peers recheck kernel bindings.
type runtimeNativeClassProof struct {
	identity      string
	kernel        RuntimeNativeKernelIdentity
	imageVerified bool
	acceptance    Pin
}

// The exact thirteen helper assets exercised by the prospective routing path.
// Builder identity additionally binds its validator, base Compose and legacy
// Dockerfile; those inputs cannot be substituted under its unchanged digest.
func runtimeNativeAssetNames() []string {
	return strings.Fields("host-self-check.sh container/subevaluation-ownership.sh container/evaluator-minimum-work.sh container/build-submission.sh container/prepare-evaluation-source.sh container/docker-id-map.sh container/network-boundary.sh container/retain-failure-evidence.sh container/compose.subevaluation.yml container/resource-boundary.sh container/evaluator-subevaluation.sh container/tracked-submission-build.sh container/Dockerfile.submission.tracked")
}

// Full-R9 statistical host admission is separate and remains required after
// physical class qualification. These trials do not weaken that later screen.
func runtimeNativeTrialNames() []string {
	return strings.Fields("matched-kernel-source core-limits-native-compat-abis inherited-exec-cgo tracked-build-stages candidate-probes-and-protected-checks helper-absence-and-join hard-exit-cleanup cpu-memory-disk-pressure kernel-journal-crash-storm management-recovery-pressure read-only-fixtures-and-source private-network-positive-negative")
}

// Static validation cannot qualify a host. It only prevents an open-ended
// selection, candidate-writable evidence or a partial runtime identity.
func (self *RuntimeNativeClassSelection) validate(boundary RuntimeResourceBoundary) error {
	if self == nil || self.Profile != runtimeNativeClassProfile || !runtimeNativePin(self.Receipt, boundary) || !namePattern.MatchString(self.Release.HostId) ||
		!strings.HasPrefix(self.Release.EvaluatorImageDigest, "sha256:") || !digestPattern.MatchString(strings.TrimPrefix(self.Release.EvaluatorImageDigest, "sha256:")) ||
		!digestPattern.MatchString(self.Release.SourceLockSha256) || !digestPattern.MatchString(self.Release.SubmissionBuilderSha256) ||
		!digestPattern.MatchString(self.Release.ConfigLocalSha256) || !digestPattern.MatchString(self.Release.VaultLocalSha256) {
		return errors.New("native runtime class omitted its closed static identity")
	}
	for _, pin := range []Pin{self.Release.Agent, self.Release.Docker, self.Release.Compose, self.Release.Buildx, self.Release.JsonFormatter,
		self.Release.SubmissionBuilderIdentity, self.Release.StockSeccompProfile, self.Release.CandidateCoreProfile} {
		if !runtimeNativePin(pin, boundary) {
			return errors.New("native runtime class selected an unsafe release pin")
		}
	}
	if len(self.Release.Assets) != len(runtimeNativeAssetNames()) {
		return errors.New("native runtime class omitted helper assets")
	}
	for index, name := range runtimeNativeAssetNames() {
		asset := self.Release.Assets[index]
		if asset.Name != name || !runtimeNativePin(asset.Pin, boundary) || filepath.Base(asset.Pin.Path) != filepath.Base(name) {
			return errors.New("native runtime class reordered, repeated or redirected helper assets")
		}
	}
	return nil
}

// Authority metadata never occupies the candidate quota or a noncanonical
// alias. Operator-aware ancestry is checked by the descriptor reader itself.
func runtimeNativePin(pin Pin, boundary RuntimeResourceBoundary) bool {
	return filepath.IsAbs(pin.Path) && filepath.Clean(pin.Path) == pin.Path && pin.Path != "/" &&
		!strings.ContainsAny(pin.Path, "\x00\r\n") && digestPattern.MatchString(pin.Sha256) && !pathWithin(boundary.CandidateRoot, pin.Path)
}

// There is no acceptance-pin hash cycle and no omission of runtime fields.
func runtimeNativeIdentity(boundary RuntimeResourceBoundary) (string, error) {
	if boundary.NativeClass == nil || boundary.NativeClass.validate(boundary) != nil {
		return "", errors.New("native runtime identity has no complete selection")
	}
	selection := *boundary.NativeClass
	selection.Receipt = Pin{}
	boundary.NativeClass = &selection
	content, err := json.Marshal(boundary)
	if err != nil {
		return "", err
	}
	return digestDockerBytes(content), nil
}

// Computes metadata only. It neither reads a host
// nor produces/accepts review evidence; qualification tools can use the exact
// Go structure encoding without inventing a different JSON canonicalization.
func RuntimeNativeClassIdentity(boundary RuntimeResourceBoundary) (string, error) {
	return runtimeNativeIdentity(boundary)
}

// A single total read budget includes every retained record/manifest, so a
// collection of individually bounded documents cannot amplify work unboundedly.
type runtimeNativeRecordReader struct {
	host        runtimeNativeClassHost
	boundary    RuntimeResourceBoundary
	scope       RuntimeResourceScope
	remaining   int64
	pinContents map[Pin][]byte
}

// Repeated exact pins share this one verification's bounded captured bytes.
func (self *runtimeNativeRecordReader) read(pin Pin, maximum int64) ([]byte, error) {
	if !runtimeNativePin(pin, self.boundary) || maximum <= 0 {
		return nil, errors.New("native class evidence has no finite protected identity")
	}
	if content, found := self.pinContents[pin]; found {
		if int64(len(content)) > maximum {
			return nil, errors.New("native class reused evidence exceeds its record bound")
		}
		return content, nil
	}
	if self.remaining < maximum {
		maximum = self.remaining
	}
	if maximum <= 0 {
		return nil, errors.New("native class exhausted its aggregate evidence budget")
	}
	content, err := self.host.NativeClassRecord(pin, maximum, self.scope)
	if err != nil || len(content) == 0 || int64(len(content)) > maximum || digestDockerBytes(content) != pin.Sha256 {
		return nil, errors.New("native class retained sealed evidence is absent or changed")
	}
	self.remaining -= int64(len(content))
	self.pinContents[pin] = content
	return content, nil
}

// Owned documents reject unknown/duplicate/case-aliased keys before typed use.
func (self *runtimeNativeRecordReader) document(pin Pin, maximum int64, output any) error {
	content, err := self.read(pin, maximum)
	if err != nil {
		return err
	}
	if validateDocumentKeys(content) != nil || decodeStrict(content, output) != nil {
		return errors.New("native class evidence is not a strict owned document")
	}
	return nil
}

// No input here comes from a signed job or candidate path. Selection was
// loaded from the protected boundary; independent evidence stays outside quota.
func prepareRuntimeNativeClass(ctx context.Context, boundary *RuntimeResourceBoundary, scope RuntimeResourceScope, settings RuntimeResourceBoundarySettings, host runtimeBoundaryHost) error {
	if boundary.NativeClass == nil {
		return nil
	}
	if settings.DocumentBytes <= 0 || settings.NativeBytes <= 0 || settings.DocumentBytes == math.MaxInt64 || settings.NativeBytes == math.MaxInt64 || settings.MaximumCorePipeLimit <= 0 {
		return errors.New("native class evidence omitted finite aggregate read bounds")
	}
	if err := boundary.NativeClass.validate(*boundary); err != nil {
		return err
	}
	owner, ok := host.(runtimeNativeClassHost)
	if !ok {
		return errors.New("native runtime class has no concrete sealed evidence/kernel owner")
	}
	identity, err := runtimeNativeIdentity(*boundary)
	if err != nil {
		return err
	}
	reader := &runtimeNativeRecordReader{host: owner, boundary: *boundary, scope: scope, remaining: settings.NativeBytes, pinContents: map[Pin][]byte{}}
	acceptance := runtimeNativeClassAcceptance{}
	if err := reader.document(boundary.NativeClass.Receipt, settings.DocumentBytes, &acceptance); err != nil {
		return err
	}
	if !runtimeNativeDocumentIdentity(acceptance.Schema, acceptance.Kind, "runtime-native-class-acceptance/v1", acceptance.Profile, acceptance.RuntimeIdentitySha256, identity) ||
		!validRuntimeNativeKernel(acceptance.Kernel, settings.MaximumCorePipeLimit) || acceptance.Qualification == acceptance.OperatorReview ||
		acceptance.Qualification.Path == acceptance.OperatorReview.Path || acceptance.Qualification.Sha256 == acceptance.OperatorReview.Sha256 {
		return errors.New("native class acceptance lacks separate exact qualification and review")
	}
	qualification := runtimeNativeClassQualification{}
	if err := reader.document(acceptance.Qualification, settings.DocumentBytes, &qualification); err != nil {
		return err
	}
	if !runtimeNativeDocumentIdentity(qualification.Schema, qualification.Kind, "runtime-native-class-qualification/v1", qualification.Profile, qualification.RuntimeIdentitySha256, identity) ||
		qualification.Kernel != acceptance.Kernel || len(qualification.Trials) != len(runtimeNativeTrialNames()) {
		return errors.New("native class qualification is stale, partial or belongs to another kernel")
	}
	for index, name := range runtimeNativeTrialNames() {
		trial := qualification.Trials[index]
		if trial.Name != name {
			return errors.New("native class qualification reordered or omitted a required trial")
		}
		receipt := runtimeNativeTrialReceipt{}
		if err := reader.document(trial.Receipt, settings.DocumentBytes, &receipt); err != nil {
			return err
		}
		if !runtimeNativeDocumentIdentity(receipt.Schema, receipt.Kind, "runtime-native-class-trial/v1", receipt.Profile, receipt.RuntimeIdentitySha256, identity) ||
			receipt.Kernel != acceptance.Kernel || receipt.Trial != name || receipt.Outcome != "pass" {
			return errors.New("native class trial does not positively match this exact reviewed class")
		}
		if _, err := reader.read(receipt.ArtifactManifest, settings.NativeBytes); err != nil {
			return err
		}
	}
	review := runtimeNativeClassReview{}
	if err := reader.document(acceptance.OperatorReview, settings.DocumentBytes, &review); err != nil {
		return err
	}
	if !runtimeNativeDocumentIdentity(review.Schema, review.Kind, "runtime-native-class-operator-review/v1", review.Profile, review.RuntimeIdentitySha256, identity) ||
		review.Kernel != acceptance.Kernel || review.QualificationSha256 != acceptance.Qualification.Sha256 || review.Decision != "accepted" ||
		review.ReviewEvidence.Path == acceptance.Qualification.Path || review.ReviewEvidence.Path == acceptance.OperatorReview.Path {
		return errors.New("native class has no separate root-pinned acceptance of its qualification")
	}
	if _, err := reader.read(review.ReviewEvidence, settings.NativeBytes); err != nil {
		return err
	}
	for _, pin := range []Pin{boundary.NativeClass.Release.Agent, boundary.NativeClass.Release.Docker, boundary.NativeClass.Release.Compose,
		boundary.NativeClass.Release.Buildx, boundary.NativeClass.Release.JsonFormatter, boundary.NativeClass.Release.SubmissionBuilderIdentity} {
		if err := host.Executable(pin); err != nil {
			return err
		}
	}
	for _, asset := range boundary.NativeClass.Release.Assets {
		if _, err := host.Document(asset.Pin, settings.NativeBytes); err != nil {
			return err
		}
	}
	profileSettings := DefaultCandidateCoreProfileSettings()
	stock, err := host.Document(boundary.NativeClass.Release.StockSeccompProfile, profileSettings.DocumentBytes)
	if err != nil {
		return err
	}
	derived, err := host.Document(boundary.NativeClass.Release.CandidateCoreProfile, profileSettings.DocumentBytes)
	if err != nil {
		return err
	}
	canonical, err := TransformCandidateCoreProfile(stock, profileSettings)
	if err != nil || !bytes.Equal(derived, canonical) {
		return errors.New("native class profile is not the exact restrictive stock derivation")
	}
	actual, err := owner.NativeClassKernel(ctx, settings)
	if err != nil || actual != acceptance.Kernel {
		return errors.New("native class kernel/build/core policy no longer matches qualification")
	}
	boundary.nativeClass = &runtimeNativeClassProof{identity: identity, kernel: actual, acceptance: boundary.NativeClass.Receipt}
	return ctx.Err()
}

// Equality of this complete tuple is required in acceptance, every trial and
// review; an approximate kernel prefix or semver is never sufficient.
func validRuntimeNativeKernel(kernel RuntimeNativeKernelIdentity, maximumCorePipeLimit int) bool {
	return kernel.Release != "" && len(kernel.Release) <= runtimeNativeKernelReleaseBytes && !strings.ContainsAny(kernel.Release, " /\\\x00\r\n\t") &&
		digestPattern.MatchString(kernel.VersionSha256) && digestPattern.MatchString(kernel.CorePatternSha256) && maximumCorePipeLimit > 0 && kernel.CorePipeLimit > 0 && kernel.CorePipeLimit <= maximumCorePipeLimit
}

// Closed record kinds share one exact runtime/profile/schema predicate.
func runtimeNativeDocumentIdentity(schema int, kind, expectedKind, profile, actualIdentity, expectedIdentity string) bool {
	return schema == Schema && kind == expectedKind && profile == runtimeNativeClassProfile && actualIdentity == expectedIdentity && digestPattern.MatchString(actualIdentity)
}

// A job cannot substitute another image, stock profile or helper release even
// when that other host/runtime class has valid independent qualification.
func validateRuntimeNativeClassBinding(config Config, boundary RuntimeResourceBoundary) error {
	if boundary.NativeClass == nil {
		return nil
	}
	if boundary.NativeClass.validate(boundary) != nil || boundary.nativeClass == nil || !boundary.nativeClass.imageVerified {
		return errors.New("agent runtime has no freshly admitted native class")
	}
	identity, err := runtimeNativeIdentity(boundary)
	if err != nil || identity != boundary.nativeClass.identity || boundary.NativeClass.Receipt != boundary.nativeClass.acceptance {
		return errors.New("agent runtime class changed after its exact acceptance was captured")
	}
	release := boundary.NativeClass.Release
	if release.HostId != config.HostId || release.EvaluatorImageDigest != config.EvaluatorImageDigest || release.SourceLockSha256 != config.SourceLockSha256 ||
		release.ConfigLocalSha256 != config.ConfigLocalSha256 || release.VaultLocalSha256 != config.VaultLocalSha256 ||
		release.SubmissionBuilderSha256 != config.SubmissionBuilderSha256 || release.Agent != config.Agent || release.Docker != config.Docker ||
		release.Compose != config.DockerRole.Compose || release.Buildx != config.DockerRole.Buildx || release.JsonFormatter != config.JsonFormatter ||
		release.SubmissionBuilderIdentity != config.SubmissionBuilderIdentity || release.StockSeccompProfile != config.DockerRole.StockSeccompProfile ||
		release.CandidateCoreProfile != config.DockerRole.CandidateCoreProfile {
		return errors.New("native class qualification selected another agent or frozen runtime release")
	}
	for _, asset := range release.Assets {
		expected := filepath.Join(filepath.Dir(config.Evaluator.Path), filepath.Base(asset.Name))
		if asset.Name == "host-self-check.sh" {
			expected = config.SelfCheck.Path
		}
		if asset.Pin.Path != expected {
			return errors.New("native class helper assets are not the agent's own installed release")
		}
		for name, pin := range map[string]Pin{"container/evaluator-subevaluation.sh": config.Evaluator, "host-self-check.sh": config.SelfCheck,
			"container/subevaluation-ownership.sh": config.SubevaluationOwnership, "container/resource-boundary.sh": config.DockerRole.ResourcePolicy,
			"container/Dockerfile.submission.tracked": config.DockerRole.Dockerfile} {
			if asset.Name == name && asset.Pin != pin {
				return errors.New("native class helper pin differs from the actual execution role")
			}
		}
	}
	return nil
}
