// Class admission uses only synthetic receipts, injected kernel/image facts
// and private regular files. No native core, manager, daemon or sysctl runs.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The extra class facts are explicit and independent from protected documents.
type runtimeNativeClassTestHost struct {
	*runtimeBoundaryTestHost
	kernel RuntimeNativeKernelIdentity
	image  []byte
}

// The fake retains the production hash/byte checks without root file access.
func (self *runtimeNativeClassTestHost) NativeClassRecord(pin Pin, maximum int64, _ RuntimeResourceScope) ([]byte, error) {
	self.events = append(self.events, "sealed:"+pin.Path)
	return self.Document(pin, maximum)
}

// No fact is discovered from the executing machine's procfs.
func (self *runtimeNativeClassTestHost) NativeClassKernel(ctx context.Context, _ RuntimeResourceBoundarySettings) (RuntimeNativeKernelIdentity, error) {
	self.events = append(self.events, "native-kernel")
	if self.fail == "native-kernel" {
		return RuntimeNativeKernelIdentity{}, errors.New("synthetic kernel read failure")
	}
	return self.kernel, ctx.Err()
}

// A synthetic daemon response can fail independently from protected metadata.
func (self *runtimeNativeClassTestHost) NativeClassImage(ctx context.Context, _ RuntimeResourceBoundary, _ string, _ RuntimeResourceBoundarySettings) ([]byte, error) {
	self.events = append(self.events, "native-image")
	if self.fail == "native-image" {
		return nil, errors.New("synthetic image read failure")
	}
	return append([]byte(nil), self.image...), ctx.Err()
}

// Rebinding changes only this fixture's root-authority records; no test ever
// publishes a real accepted receipt or writes an installed host configuration.
type runtimeNativeClassTest struct {
	*runtimeBoundaryTest
	host          *runtimeNativeClassTestHost
	acceptance    runtimeNativeClassAcceptance
	qualification runtimeNativeClassQualification
	review        runtimeNativeClassReview
	trials        []runtimeNativeTrialReceipt
}

// Every authority and image is generated synthetic fixture data only.
func newRuntimeNativeClassTest(t *testing.T) *runtimeNativeClassTest {
	t.Helper()
	base := newRuntimeBoundaryTest(t)
	host := &runtimeNativeClassTestHost{runtimeBoundaryTestHost: base.host}
	fixture := &runtimeNativeClassTest{runtimeBoundaryTest: base, host: host}
	executable := Pin{Path: "/opt/synthetic-release/agent", Sha256: strings.Repeat("b", 64)}
	stock := testCandidateCoreStockProfile()
	derived, err := TransformCandidateCoreProfile(stock, DefaultCandidateCoreProfileSettings())
	if err != nil {
		t.Fatal(err)
	}
	release := RuntimeNativeReleaseIdentity{HostId: "synthetic-executor", EvaluatorImageDigest: "sha256:" + strings.Repeat("c", 64),
		SourceLockSha256: strings.Repeat("d", 64), ConfigLocalSha256: strings.Repeat("e", 64), VaultLocalSha256: strings.Repeat("f", 64), SubmissionBuilderSha256: strings.Repeat("a", 64),
		Agent: executable, Docker: executable, Compose: executable, Buildx: executable, JsonFormatter: executable, SubmissionBuilderIdentity: executable,
		StockSeccompProfile: base.document("stock.json", stock), CandidateCoreProfile: base.document("candidate.json", derived)}
	for _, name := range runtimeNativeAssetNames() {
		pin := base.document(filepath.Base(name), []byte("synthetic helper "+name+"\n"))
		release.Assets = append(release.Assets, RuntimeNativeAsset{Name: name, Pin: pin})
	}
	base.boundary.NativeClass = &RuntimeNativeClassSelection{Profile: runtimeNativeClassProfile, Receipt: Pin{Path: "/etc/synthetic-private-engine/accepted.json", Sha256: strings.Repeat("1", 64)}, Release: release}
	pattern := []byte("|/synthetic/core-handler %P %u %g\n")
	host.kernel = RuntimeNativeKernelIdentity{Release: "6.8.0-synthetic-build", VersionSha256: digestDockerBytes([]byte("synthetic exact kernel version\n")),
		CorePatternSha256: digestDockerBytes(pattern), CorePipeLimit: 10}
	host.facts.CorePattern, host.facts.CorePatternAfter = pattern, append([]byte(nil), pattern...)
	host.facts.NativeKernel, host.facts.NativeKernelAfter = host.kernel, host.kernel
	host.image, err = json.Marshal(map[string]any{"Id": release.EvaluatorImageDigest, "Os": "linux", "Architecture": "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := runtimeNativeIdentity(base.boundary)
	if err != nil {
		t.Fatal(err)
	}
	fixture.acceptance = runtimeNativeClassAcceptance{Schema: Schema, Kind: "runtime-native-class-acceptance/v1", Profile: runtimeNativeClassProfile, RuntimeIdentitySha256: identity, Kernel: host.kernel}
	fixture.qualification = runtimeNativeClassQualification{Schema: Schema, Kind: "runtime-native-class-qualification/v1", Profile: runtimeNativeClassProfile, RuntimeIdentitySha256: identity, Kernel: host.kernel}
	fixture.review = runtimeNativeClassReview{Schema: Schema, Kind: "runtime-native-class-operator-review/v1", Profile: runtimeNativeClassProfile, RuntimeIdentitySha256: identity, Kernel: host.kernel, Decision: "accepted",
		ReviewEvidence: base.document("review-evidence.txt", []byte("synthetic independent review evidence, never operational authority\n"))}
	for _, name := range runtimeNativeTrialNames() {
		fixture.trials = append(fixture.trials, runtimeNativeTrialReceipt{Schema: Schema, Kind: "runtime-native-class-trial/v1", Profile: runtimeNativeClassProfile, RuntimeIdentitySha256: identity,
			Kernel: host.kernel, Trial: name, Outcome: "pass", ArtifactManifest: base.document(name+".manifest", []byte("synthetic retained artifact manifest "+name+"\n"))})
	}
	fixture.rebind(t)
	return fixture
}

// The test document store hashes its current bytes like the protected reader.
func (self *runtimeNativeClassTest) object(t *testing.T, name string, value any) Pin {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return self.document(name, content)
}

// This helper is fixture-only. Production contains no corresponding accept or
// qualification producer, and the class never consumes a job's patch identity.
func (self *runtimeNativeClassTest) rebind(t *testing.T) {
	t.Helper()
	self.qualification.Trials = nil
	for _, trial := range self.trials {
		self.qualification.Trials = append(self.qualification.Trials, runtimeNativeTrial{Name: trial.Trial, Receipt: self.object(t, trial.Trial+".json", trial)})
	}
	self.acceptance.Qualification = self.object(t, "qualification.json", self.qualification)
	self.review.QualificationSha256 = self.acceptance.Qualification.Sha256
	self.acceptance.OperatorReview = self.object(t, "operator-review.json", self.review)
	self.boundary.NativeClass.Receipt = self.object(t, "accepted.json", self.acceptance)
	self.commit()
}

// Real admission ordering runs with injected native boundaries only.
func (self *runtimeNativeClassTest) verify() (*RuntimeResourceBoundary, error) {
	return verifyRuntimeResourceBoundary(context.Background(), self.pin, self.scope, self.settings, self.host)
}

// A positive synthetic class exercises the complete production ordering,
// including image readback, retained documents and the final namespace recheck.
func TestRuntimeNativeClassRequiresCompleteIndependentEvidence(t *testing.T) {
	fixture := newRuntimeNativeClassTest(t)
	boundary, err := fixture.verify()
	if err != nil || boundary == nil || boundary.nativeClass == nil || !boundary.nativeClass.imageVerified {
		t.Fatal("complete synthetic native class was not admitted", err)
	}
	defer boundary.Close()
	if !strings.Contains(strings.Join(fixture.host.events, "\n"), "native-image\nnetwork") {
		t.Fatal("native class image did not precede final network verification")
	}
	if err := validateRuntimeBoundaryCorePolicy(*boundary, fixture.host.facts.CorePattern, fixture.settings); err != nil {
		t.Fatal("reviewed exact core policy lost its static class identity", err)
	}
	boundary.NativeClass.Receipt.Sha256 = strings.Repeat("0", 64)
	if validateRuntimeBoundaryCorePolicy(*boundary, fixture.host.facts.CorePattern, fixture.settings) == nil {
		t.Fatal("replacing a captured acceptance pin retained its old core proof")
	}
}

// A pass document is not its own root review; missing, partial, mixed-class or
// negative evidence cannot become a piped-handler fallback.
func TestRuntimeNativeClassRejectsMissingPartialAndSelfReportedEvidence(t *testing.T) {
	for _, fault := range []string{"missing-review", "same-review", "pending-review", "unknown-trial", "missing-trial", "reordered-trial", "wrong-review-qualification", "missing-manifest", "missing-review-evidence", "wrong-trial-kernel"} {
		fixture := newRuntimeNativeClassTest(t)
		switch fault {
		case "pending-review":
			fixture.review.Decision = "pending"
		case "unknown-trial":
			fixture.trials[0].Outcome = "unknown"
		case "missing-trial":
			fixture.trials = fixture.trials[1:]
		case "reordered-trial":
			fixture.trials[0], fixture.trials[1] = fixture.trials[1], fixture.trials[0]
		case "wrong-trial-kernel":
			fixture.trials[0].Kernel.Release = "6.8.0-other-build"
		}
		fixture.rebind(t)
		switch fault {
		case "missing-review":
			delete(fixture.host.documents, fixture.acceptance.OperatorReview.Path)
		case "same-review":
			fixture.acceptance.OperatorReview = fixture.acceptance.Qualification
			fixture.boundary.NativeClass.Receipt = fixture.object(t, "accepted.json", fixture.acceptance)
			fixture.commit()
		case "wrong-review-qualification":
			fixture.review.QualificationSha256 = strings.Repeat("0", 64)
			fixture.acceptance.OperatorReview = fixture.object(t, "operator-review.json", fixture.review)
			fixture.boundary.NativeClass.Receipt = fixture.object(t, "accepted.json", fixture.acceptance)
			fixture.commit()
		case "missing-manifest":
			delete(fixture.host.documents, fixture.trials[0].ArtifactManifest.Path)
		case "missing-review-evidence":
			delete(fixture.host.documents, fixture.review.ReviewEvidence.Path)
		}
		if boundary, err := fixture.verify(); err == nil || boundary != nil {
			t.Fatal("incomplete native class evidence acquired admission", fault, err)
		}
	}
}

// A class-specific provider cannot be silently substituted with a generic
// protected-document fake or omitted while keeping the new selection enabled.
func TestRuntimeNativeClassRequiresConcreteClassFactsAndExactAssets(t *testing.T) {
	fixture := newRuntimeNativeClassTest(t)
	if boundary, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.runtimeBoundaryTest.host); err == nil || boundary != nil {
		t.Fatal("generic runtime host silently waived native class ownership", err)
	}
	for _, fault := range []string{"missing", "reordered", "alias-name", "inside-quota"} {
		fixture := newRuntimeNativeClassTest(t)
		switch fault {
		case "missing":
			fixture.boundary.NativeClass.Release.Assets = fixture.boundary.NativeClass.Release.Assets[1:]
		case "reordered":
			assets := fixture.boundary.NativeClass.Release.Assets
			assets[0], assets[1] = assets[1], assets[0]
		case "alias-name":
			fixture.boundary.NativeClass.Release.Assets[0].Name = fixture.boundary.NativeClass.Release.Assets[1].Name
		case "inside-quota":
			fixture.boundary.NativeClass.Receipt.Path = filepath.Join(fixture.boundary.CandidateRoot, "accepted.json")
		}
		fixture.commit()
		if boundary, err := fixture.verify(); err == nil || boundary != nil {
			t.Fatal("partial or candidate-writable native class authority was accepted", fault, err)
		}
	}
}

// Canonical identity excludes exactly the receipt's own pin, not another
// daemon, allocation, profile, source release or immutable image field.
func TestRuntimeNativeClassBindsEveryRuntimeReleaseDimension(t *testing.T) {
	for _, mutate := range []func(*RuntimeResourceBoundary){
		func(value *RuntimeResourceBoundary) { value.Dockerd.Sha256 = strings.Repeat("0", 64) },
		func(value *RuntimeResourceBoundary) { value.Runc.Sha256 = strings.Repeat("0", 64) },
		func(value *RuntimeResourceBoundary) { value.Containerd.Sha256 = strings.Repeat("0", 64) },
		func(value *RuntimeResourceBoundary) { value.ContainerdShim.Sha256 = strings.Repeat("0", 64) },
		func(value *RuntimeResourceBoundary) { value.EngineUnit.Sha256 = strings.Repeat("0", 64) },
		func(value *RuntimeResourceBoundary) { value.EngineConfig.Sha256 = strings.Repeat("0", 64) },
		func(value *RuntimeResourceBoundary) { value.ContainerdConfig.Sha256 = strings.Repeat("0", 64) },
		func(value *RuntimeResourceBoundary) { value.NetworkPolicy.Sha256 = strings.Repeat("0", 64) },
		func(value *RuntimeResourceBoundary) { value.ContainerRootUid++ },
		func(value *RuntimeResourceBoundary) { value.ContainerRootGid++ },
		func(value *RuntimeResourceBoundary) { value.ManagementCpuSet = "6,8" },
		func(value *RuntimeResourceBoundary) { value.NativeClass.Release.HostId = "another-synthetic-executor" },
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.SourceLockSha256 = strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.ConfigLocalSha256 = strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.VaultLocalSha256 = strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.SubmissionBuilderSha256 = strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.StockSeccompProfile.Sha256 = strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.CandidateCoreProfile.Sha256 = strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) { value.NativeClass.Release.Agent.Sha256 = strings.Repeat("0", 64) },
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.Docker.Sha256 = strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.Compose.Sha256 = strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.Buildx.Sha256 = strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.JsonFormatter.Sha256 = strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.SubmissionBuilderIdentity.Sha256 = strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.EvaluatorImageDigest = "sha256:" + strings.Repeat("0", 64)
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.Assets[0].Pin.Sha256 = strings.Repeat("0", 64)
		},
	} {
		fixture := newRuntimeNativeClassTest(t)
		mutate(&fixture.boundary)
		fixture.commit()
		if boundary, err := fixture.verify(); err == nil || boundary != nil {
			t.Fatal("changed runtime identity retained its old acceptance", err)
		}
	}
	for index := range runtimeNativeAssetNames() {
		fixture := newRuntimeNativeClassTest(t)
		fixture.boundary.NativeClass.Release.Assets[index].Pin.Sha256 = strings.Repeat("0", 64)
		fixture.commit()
		if boundary, err := fixture.verify(); err == nil || boundary != nil {
			t.Fatal("one changed helper asset retained old native acceptance", index, err)
		}
	}
	fixture := newRuntimeNativeClassTest(t)
	before, err := runtimeNativeIdentity(fixture.boundary)
	if err != nil {
		t.Fatal(err)
	}
	fixture.boundary.NativeClass.Receipt.Sha256 = strings.Repeat("0", 64)
	after, err := runtimeNativeIdentity(fixture.boundary)
	if err != nil || before != after {
		t.Fatal("acceptance pin formed a circular runtime identity", err)
	}
	fixture.commit()
	if boundary, err := fixture.verify(); err == nil || boundary != nil {
		t.Fatal("excluding the receipt from identity also disabled its actual pin", err)
	}
}

// Exact build and raw handler/limit are checked independently at preparation
// and both kernel snapshots. No changed tuple can reuse an old acceptance.
func TestRuntimeNativeClassKernelChangesNeverUseFallback(t *testing.T) {
	for _, fault := range []string{"initial-release", "initial-version", "initial-limit", "snapshot-release", "snapshot-after", "raw-pattern", "kernel-read"} {
		fixture := newRuntimeNativeClassTest(t)
		switch fault {
		case "initial-release":
			fixture.host.kernel.Release = "6.8.0-other-build"
		case "initial-version":
			fixture.host.kernel.VersionSha256 = strings.Repeat("0", 64)
		case "initial-limit":
			fixture.host.kernel.CorePipeLimit++
		case "snapshot-release":
			fixture.host.facts.NativeKernel.Release = "6.8.0-other-build"
		case "snapshot-after":
			fixture.host.facts.NativeKernelAfter.CorePipeLimit++
		case "raw-pattern":
			fixture.host.facts.CorePattern = []byte("|/other/core-handler %P\n")
			fixture.host.facts.CorePatternAfter = append([]byte(nil), fixture.host.facts.CorePattern...)
		case "kernel-read":
			fixture.host.fail = "native-kernel"
		}
		if boundary, err := fixture.verify(); err == nil || boundary != nil {
			t.Fatal("changed native kernel class retained admission", fault, err)
		}
	}
}

// The legacy non-piped filename still needs no new receipt or class provider;
// selecting an unreviewed pipe never gains that legacy branch's admission.
func TestRuntimeNativeClassPreservesLegacyFilenameBranch(t *testing.T) {
	fixture := newRuntimeBoundaryTest(t)
	boundary, err := verifyRuntimeResourceBoundary(context.Background(), fixture.pin, fixture.scope, fixture.settings, fixture.host)
	if err != nil || boundary.NativeClass != nil {
		t.Fatal("legacy exact filename policy acquired a new class requirement", err)
	}
	boundary.Close()
	for _, pattern := range [][]byte{nil, []byte("|/synthetic/handler %P\n"), []byte("core.%P\r\n"), []byte("/tmp/core.%P\n")} {
		if validateRuntimeBoundaryCorePolicy(RuntimeResourceBoundary{}, pattern, fixture.settings) == nil {
			t.Fatal("legacy branch admitted another core policy", string(pattern))
		}
	}
}

// The exact image must be read from the authenticated private engine after
// namespace capture; failure physically closes that captured namespace receipt.
func TestRuntimeNativeClassImageFailureClosesCapturedNamespace(t *testing.T) {
	for _, content := range [][]byte{
		[]byte(`{"Id":"sha256:` + strings.Repeat("0", 64) + `","Os":"linux","Architecture":"amd64"}`),
		[]byte(`{"Id":"sha256:` + strings.Repeat("c", 64) + `","Os":"linux","Architecture":"arm64"}`),
		[]byte(`{"Id":"sha256:` + strings.Repeat("c", 64) + `","Os":"other","Architecture":"amd64"}`),
		[]byte(`{"Id":"sha256:` + strings.Repeat("c", 64) + `","Id":"other","Os":"linux","Architecture":"amd64"}`),
	} {
		fixture := newRuntimeNativeClassTest(t)
		fixture.host.image = content
		if boundary, err := fixture.verify(); err == nil || boundary != nil || !fixture.host.namespace.closed {
			t.Fatal("unqualified image retained a captured namespace", err)
		}
	}
}

// Each receipt is strict owned data. An attacker cannot append authority to
// otherwise valid JSON or use a collection of manifests to disable its budget.
func TestRuntimeNativeClassRejectsAmbiguousAndOverBudgetRecords(t *testing.T) {
	for _, fault := range []string{"duplicate", "case-alias", "unknown", "trailing", "aggregate"} {
		fixture := newRuntimeNativeClassTest(t)
		content := fixture.host.documents[fixture.boundary.NativeClass.Receipt.Path]
		switch fault {
		case "duplicate":
			content = bytes.Replace(content, []byte(`"schema":1`), []byte(`"schema":1,"schema":1`), 1)
		case "case-alias":
			content = bytes.Replace(content, []byte(`"schema":1`), []byte(`"Schema":1`), 1)
		case "unknown":
			content = append([]byte(`{"allow_pipe":true,`), content[1:]...)
		case "trailing":
			content = append(content, []byte("{}")...)
		case "aggregate":
			fixture.settings.NativeBytes = 1024
		}
		fixture.boundary.NativeClass.Receipt = fixture.document("accepted.json", content)
		fixture.commit()
		if boundary, err := fixture.verify(); err == nil || boundary != nil {
			t.Fatal("ambiguous or unbounded native class acquired admission", fault, err)
		}
	}
}

// The test role is a finite static configuration only. It is never passed to
// NewAgent, a service manager or any native root-loading entrypoint.
func runtimeNativeClassTestConfig(boundary RuntimeResourceBoundary) Config {
	release := boundary.NativeClass.Release
	config := Config{HostId: release.HostId, EvaluatorImageDigest: release.EvaluatorImageDigest, SourceLockSha256: release.SourceLockSha256,
		ConfigLocalSha256: release.ConfigLocalSha256, VaultLocalSha256: release.VaultLocalSha256, SubmissionBuilderSha256: release.SubmissionBuilderSha256,
		Agent: release.Agent, Docker: release.Docker, JsonFormatter: release.JsonFormatter, SubmissionBuilderIdentity: release.SubmissionBuilderIdentity,
		DockerSocketPath: boundary.PrivateSocketPath, WorkDirectory: boundary.WorkRoot,
		DockerRole: DockerRoleConfig{ScratchDirectory: boundary.ScratchRoot, Compose: release.Compose, Buildx: release.Buildx,
			StockSeccompProfile: release.StockSeccompProfile, CandidateCoreProfile: release.CandidateCoreProfile}}
	for _, asset := range release.Assets {
		switch asset.Name {
		case "container/evaluator-subevaluation.sh":
			config.Evaluator = asset.Pin
		case "host-self-check.sh":
			config.SelfCheck = asset.Pin
		case "container/subevaluation-ownership.sh":
			config.SubevaluationOwnership = asset.Pin
		case "container/resource-boundary.sh":
			config.DockerRole.ResourcePolicy = asset.Pin
		case "container/Dockerfile.submission.tracked":
			config.DockerRole.Dockerfile = asset.Pin
		}
	}
	return config
}

// Another accepted class is not permission to substitute this role's image,
// source checkpoint, fixtures, signing executable or exact helper profiles.
func TestRuntimeNativeClassRechecksActualAgentReleaseBinding(t *testing.T) {
	fixture := newRuntimeNativeClassTest(t)
	boundary, err := fixture.verify()
	if err != nil {
		t.Fatal(err)
	}
	defer boundary.Close()
	config := runtimeNativeClassTestConfig(*boundary)
	if err := validateCleanupRuntimeBinding(config, boundary); err != nil {
		t.Fatal("matching static agent/native class was rejected", err)
	}
	for _, mutate := range []func(*Config){
		func(value *Config) { value.HostId = "another-synthetic-executor" },
		func(value *Config) { value.EvaluatorImageDigest = "sha256:" + strings.Repeat("0", 64) },
		func(value *Config) { value.SourceLockSha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.ConfigLocalSha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.VaultLocalSha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.SubmissionBuilderSha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.Agent.Sha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.Docker.Sha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.DockerRole.Compose.Sha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.DockerRole.Buildx.Sha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.JsonFormatter.Sha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.SubmissionBuilderIdentity.Sha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.DockerRole.StockSeccompProfile.Sha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.DockerRole.CandidateCoreProfile.Sha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.Evaluator.Sha256 = strings.Repeat("0", 64) },
		func(value *Config) { value.DockerRole.ResourcePolicy.Sha256 = strings.Repeat("0", 64) },
	} {
		changed := config
		mutate(&changed)
		if err := validateCleanupRuntimeBinding(changed, boundary); err == nil {
			t.Fatal("agent substituted another identity under an accepted class")
		}
	}
	boundary.NativeClass.Release.Assets[0].Pin.Sha256 = strings.Repeat("0", 64)
	if validateRuntimeBoundaryCorePolicy(*boundary, fixture.host.facts.CorePattern, fixture.settings) == nil {
		t.Fatal("mutating an already admitted selection retained its core-policy proof")
	}
	if validateCleanupRuntimeBinding(config, boundary) == nil {
		t.Fatal("mutating an already admitted selection retained its agent-binding proof")
	}
}

// Actual sensitive reads reject relative symlinks and hardlinks, not only
// manifest spelling. Both hardlink names deliberately share one fixture FS.
func TestRuntimeNativeClassSealedRecordUsesRawNoFollowAndSingleInode(t *testing.T) {
	root := t.TempDir()
	content := []byte("synthetic retained root review\n")
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(real, "receipt")
	if err := os.WriteFile(path, content, 0400); err != nil {
		t.Fatal(err)
	}
	pin := Pin{Path: path, Sha256: digestDockerBytes(content)}
	if got, err := readRuntimeNativeRecord(pin, 4096, RuntimeResourceScope{}, false); err != nil || !bytes.Equal(got, content) {
		t.Fatal("sealed fixture record rejected", err)
	}
	leaf := filepath.Join(real, "leaf")
	if err := os.Symlink("receipt", leaf); err != nil {
		t.Fatal(err)
	}
	ancestor := filepath.Join(root, "alias")
	if err := os.Symlink("real", ancestor); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{leaf, filepath.Join(ancestor, "receipt")} {
		if _, err := readRuntimeNativeRecord(Pin{Path: alias, Sha256: pin.Sha256}, 4096, RuntimeResourceScope{}, false); err == nil {
			t.Fatal("relative alias was followed at the actual record reader", alias)
		}
	}
	other := filepath.Join(root, "other-link")
	if err := os.Link(path, other); err != nil {
		t.Fatal(err)
	}
	first, firstErr := os.Stat(path)
	second, secondErr := os.Stat(other)
	if firstErr != nil || secondErr != nil || !os.SameFile(first, second) || first.Sys().(*syscall.Stat_t).Nlink != 2 {
		t.Fatal("hardlink fixture did not retain the same two-link inode", firstErr, secondErr)
	}
	if _, err := readRuntimeNativeRecord(pin, 4096, RuntimeResourceScope{}, false); err == nil {
		t.Fatal("multiply linked review was accepted")
	}
}

// Read-only is necessary but not sufficient: a wrong digest, a writable leaf,
// a FIFO, or a file beyond the read owner's cap all refuse without stream I/O.
func TestRuntimeNativeClassSealedRecordRejectsModesTypesAndBounds(t *testing.T) {
	for _, fault := range []string{"mode", "digest", "fifo", "bound"} {
		root := t.TempDir()
		path, content := filepath.Join(root, "record"), []byte("synthetic retained metadata\n")
		if fault == "fifo" {
			if err := syscall.Mkfifo(path, 0400); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, content, 0400); err != nil {
			t.Fatal(err)
		}
		pin, maximum := Pin{Path: path, Sha256: digestDockerBytes(content)}, int64(4096)
		switch fault {
		case "mode":
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
		case "digest":
			pin.Sha256 = strings.Repeat("0", 64)
		case "bound":
			maximum = int64(len(content) - 1)
		}
		if _, err := readRuntimeNativeRecord(pin, maximum, RuntimeResourceScope{}, false); err == nil {
			t.Fatal("unsafe sealed record was accepted", fault)
		}
	}
}
