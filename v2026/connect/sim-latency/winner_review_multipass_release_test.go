// The native gate binds actual compiled source bytes and original test events
// to its installed executable. A caller-authored accepted flag, old flat report
// or model alias cannot replace a complete new release qualification.
package main

import (
	"bytes"
	"embed"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Source bytes are immutable members of this exact test-driver executable.
// Embedding also closes the census when a new owned source/test file is added.
//
//go:embed winner_review_multipass*.go winner_review_handoff*.go
var winnerMultipassCompiledSources embed.FS

// There is deliberately no Accepted/Qualified boolean. Each original log and
// every compiled owned source must authenticate and satisfy the closed census.
type winnerMultipassOfflineReceipt struct {
	Schema             int                         `json:"schema"`
	Kind               string                      `json:"kind"`
	SourceCommit       string                      `json:"source_commit"`
	NativeDriverSha256 string                      `json:"native_driver_sha256"`
	TrustedCliSha256   string                      `json:"trusted_cli_sha256"`
	WrapperSha256      string                      `json:"wrapper_sha256"`
	GoVersion          string                      `json:"go_version"`
	Sources            []currentQgQualificationPin `json:"sources"`
	OrdinaryLogs       []currentQgQualificationPin `json:"ordinary_logs"`
	RaceLogs           []currentQgQualificationPin `json:"race_logs"`
}

// The names come from compiled test source, not a supplied pass-count claim.
func winnerMultipassCompiledCensus() (map[string][]byte, []string, error) {
	sources, tests := map[string][]byte{}, []string{}
	entries, err := fs.ReadDir(winnerMultipassCompiledSources, ".")
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		content, err := winnerMultipassCompiledSources.ReadFile(entry.Name())
		if err != nil {
			return nil, nil, err
		}
		sources[entry.Name()] = content
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), content, 0)
		if err != nil {
			return nil, nil, err
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && (strings.HasPrefix(function.Name.Name, "TestWinnerMultipass") || strings.HasPrefix(function.Name.Name, "TestWinnerHandoff") || function.Name.Name == "TestElectedWinnerReviewHandoffCommand") {
				tests = append(tests, function.Name.Name)
			}
		}
	}
	slices.Sort(tests)
	if len(sources) == 0 || len(tests) == 0 || len(slices.Compact(append([]string{}, tests...))) != len(tests) {
		return nil, nil, errors.New("compiled native qualification census is empty or duplicated")
	}
	return sources, tests, nil
}

// The injected reader is only a deterministic offline-test seam. Actual callers
// supply the private pinned reader after root-owned release authentication.
func validateWinnerMultipassOfflineReceipt(receipt winnerMultipassOfflineReceipt, release winnerMultipassQualificationRelease, read func(currentQgQualificationPin) ([]byte, error)) error {
	if read == nil || receipt.Schema != 1 || receipt.Kind != "winner-review-multipass-offline-qualification/v1" || receipt.SourceCommit != release.SourceCommit || receipt.NativeDriverSha256 != release.NativeDriver.Sha256 ||
		receipt.TrustedCliSha256 != release.TrustedCli.Sha256 || receipt.WrapperSha256 != release.Wrapper.Sha256 || receipt.GoVersion != "go1.27.3" || len(receipt.OrdinaryLogs) == 0 || len(receipt.RaceLogs) == 0 || len(receipt.OrdinaryLogs) > 3 || len(receipt.RaceLogs) > 3 {
		return errors.New("native offline qualification lacks its exact source/executable and ordinary/race evidence")
	}
	sources, tests, err := winnerMultipassCompiledCensus()
	if err != nil {
		return err
	}
	if len(receipt.Sources) != len(sources) {
		return errors.New("native offline qualification omitted a compiled source")
	}
	for _, pin := range receipt.Sources {
		name := filepath.Base(pin.Path)
		expected, found := sources[name]
		actual, err := read(pin)
		if !found || err != nil || !bytes.Equal(expected, actual) || winnerMultipassSha256(actual) != pin.Sha256 {
			return errors.New("qualified source bytes differ from the compiled closed census")
		}
		delete(sources, name)
	}
	seenLogs := map[string]bool{}
	for _, pin := range append(append([]currentQgQualificationPin{}, receipt.OrdinaryLogs...), receipt.RaceLogs...) {
		content, err := read(pin)
		if err != nil || seenLogs[pin.Sha256] || winnerMultipassSha256(content) != pin.Sha256 || validateCurrentQgUnitLog(content, tests) != nil {
			return errors.New("native qualification omitted, repeated, skipped or failed an exact top-level test")
		}
		seenLogs[pin.Sha256] = true
	}
	return nil
}

// Authenticate installed release bytes and original evidence before genuine
// qualification or candidate capture. Fixture/preparation replay is a separate
// required semantic gate, not replaced by any byte identity checked here.
func authenticateWinnerMultipassReleaseArtifacts(release winnerMultipassQualificationRelease) error {
	if err := validateWinnerMultipassQualificationReleaseShape(release); err != nil {
		return err
	}
	return authenticateWinnerMultipassInstalledArtifacts(release, false)
}

// Bootstrap authenticates the same installed source, ordinary/race evidence,
// image and fixed provider, but permits only absent runtime observations. It
// cannot be used by a normal review call or issue generation admission.
func authenticateWinnerMultipassRuntimeProbeArtifacts(release winnerMultipassQualificationRelease) error {
	if err := validateWinnerMultipassRuntimeProbeReleaseShape(release); err != nil {
		return err
	}
	return authenticateWinnerMultipassInstalledArtifacts(release, true)
}

// Both callers enforce their mutually exclusive release shapes first. The
// boolean selects only the declaration format and completed-observation gate.
func authenticateWinnerMultipassInstalledArtifacts(release winnerMultipassQualificationRelease, runtimeProbe bool) error {
	if err := validateCurrentQgQualificationRuntime(); err != nil {
		return err
	}
	settings := defaultCurrentQgQualificationSettings()
	settings.MaximumBytes = settings.MaximumExecutableBytes
	for _, pin := range []currentQgQualificationPin{release.TrustedCli, release.NativeDriver, release.Wrapper} {
		if _, err := currentQgRootPublicPin(pin, settings, 0555); err != nil {
			return err
		}
		if err := verifySourceSupervisorProgram(pin.Path, pin.Sha256); err != nil {
			return err
		}
	}
	self, err := os.Executable()
	if err != nil || self != release.NativeDriver.Path {
		return errors.New("running native driver is not the independently pinned release executable")
	}
	configuration, err := currentQgRootPublicPin(release.Configuration, settings, 0)
	if err != nil {
		return err
	}
	var deployment struct {
		ImageId                string `json:"image_id"`
		ProviderSettingsFile   string `json:"provider_settings_file"`
		ProviderSettingsSha256 string `json:"provider_settings_sha256"`
	}
	if decodeCurrentQgSnowQualificationProof(configuration, &deployment) != nil || deployment.ImageId != release.ImageId || deployment.ProviderSettingsFile != release.ProviderSettings.Path || deployment.ProviderSettingsSha256 != release.ProviderSettings.Sha256 {
		return errors.New("installed multi-pass wrapper configuration differs from its qualified release")
	}
	if _, err := currentQgRootPublicPin(release.ProviderSettings, settings, 0444); err != nil {
		return err
	}
	if _, err := loadWinnerSnowProviderSettings(release.ProviderSettings.Path, release.ProviderSettings.Sha256); err != nil {
		return err
	}
	read := func(pin currentQgQualificationPin) ([]byte, error) {
		return readCurrentQgQualificationPin(pin, settings.MaximumBytes, true)
	}
	imageBytes, err := read(release.Image)
	if err != nil {
		return err
	}
	var image currentQgQualificationImage
	if decodeStrictJSONBytes(imageBytes, &image, "new multi-pass scratch image qualification") != nil || image.Schema != 1 || image.Kind != "winner-review-multipass-scratch-image-qualification/v1" || !image.Accepted || image.ImageId != release.ImageId ||
		image.SourceCommit != release.SourceCommit || image.BinarySha256 != release.TrustedCli.Sha256 || image.GoVersion != "go1.27.3" || image.PolicyModel != model.CompetitionSnowWinnerReviewModel || image.ProviderModel != model.CompetitionSnowWinnerReviewModel || len(image.Evidence) == 0 {
		return errors.New("new multi-pass scratch image lacks its exact release identity")
	}
	for _, pin := range image.Evidence {
		if _, err := read(pin); err != nil {
			return err
		}
	}
	content, err := read(release.OfflineQualification)
	if err != nil {
		return err
	}
	var offline winnerMultipassOfflineReceipt
	if err := decodeStrictJSONBytes(content, &offline, "new multi-pass original offline qualification"); err != nil {
		return err
	}
	if err := validateWinnerMultipassOfflineReceipt(offline, release, read); err != nil {
		return err
	}
	// Deployment observations establish declared checkpoint provenance only.
	// The fixed per-member actual runtime context/rendering proof remains pinned.
	content, err = read(release.DeploymentDeclaration)
	if err != nil {
		return err
	}
	var declaration struct {
		Schema                 int                                           `json:"schema"`
		Kind                   string                                        `json:"kind"`
		CheckpointEvidenceKind string                                        `json:"checkpoint_evidence_kind"`
		Members                []winnerSnowMember                            `json:"members"`
		TokenizationProfiles   map[string]winnerMultipassTokenizationProfile `json:"tokenization_profiles"`
		Evidence               []currentQgQualificationPin                   `json:"evidence"`
	}
	declarationKind := "winner-review-multipass-deployment-declaration/v1"
	if runtimeProbe {
		declarationKind = "winner-review-multipass-bootstrap-deployment-declaration/v1"
	}
	if decodeStrictJSONBytes(content, &declaration, "fixed provider deployment declaration") != nil || declaration.Schema != 1 || declaration.Kind != declarationKind || declaration.CheckpointEvidenceKind != release.CheckpointEvidenceKind ||
		!reflect.DeepEqual(declaration.Members, defaultWinnerSnowProviderSettings().Members) || !reflect.DeepEqual(declaration.TokenizationProfiles, release.TokenizationProfiles) || len(declaration.Evidence) == 0 {
		return errors.New("new provider declaration omitted or changed its exact runtime/checkpoint provenance")
	}
	runtimePins := map[string][]byte{}
	for _, pin := range declaration.Evidence {
		original, err := read(pin)
		if err != nil {
			return err
		}
		runtimePins[pin.Sha256] = original
	}
	if !runtimeProbe {
		for _, profile := range release.TokenizationProfiles {
			original, found := runtimePins[profile.RuntimeEvidenceSha256]
			if !found {
				return errors.New("runtime tokenization profile has no retained original declaration evidence")
			}
			if err := authenticateWinnerHandoffRuntimeEvidence(original, release, profile.Member); err != nil {
				return err
			}
		}
		if err := validateWinnerHandoffRuntimePair(runtimePins, release); err != nil {
			return err
		}
	}
	return nil
}

// Source embedding must include this gate and the complete native/model core;
// omissions are detected even if a caller offers a plausible pass count.
func TestWinnerMultipassCompiledQualificationCensusIsClosed(t *testing.T) {
	sources, tests, err := winnerMultipassCompiledCensus()
	if err != nil || len(sources["winner_review_multipass_release_test.go"]) == 0 || len(sources["winner_review_handoff_native_test.go"]) == 0 || !slices.Contains(tests, "TestWinnerMultipassCompiledQualificationCensusIsClosed") || !slices.Contains(tests, "TestElectedWinnerReviewHandoffCommand") {
		t.Fatal("compiled qualification census is incomplete", err)
	}
}

// Original ordinary/race logs must be accepted without filtering. A nested
// passing test is not in the compiled top-level census and prevents admission.
func TestWinnerMultipassCompiledQualificationUsesOnlyTopLevelTestEvents(t *testing.T) {
	sources, _, err := winnerMultipassCompiledCensus()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range sources {
		if !strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, content, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "Run" {
				if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "t" {
					t.Errorf("%s emits nested test events outside the immutable release census", name)
				}
			}
			return true
		})
	}
}

func TestWinnerMultipassOfflineLogDoesNotDiscardNestedOrUnexpectedEvents(t *testing.T) {
	name := "TestSyntheticCompleteQualification"
	content := []byte(`{"Action":"run","Package":"github.com/urnetwork/server/v2026/connect/sim-latency","Test":"TestSyntheticCompleteQualification"}
{"Action":"pass","Package":"github.com/urnetwork/server/v2026/connect/sim-latency","Test":"TestSyntheticCompleteQualification"}
{"Action":"pass","Package":"github.com/urnetwork/server/v2026/connect/sim-latency"}`)
	if err := validateCurrentQgUnitLog(content, []string{name}); err != nil {
		t.Fatal("complete synthetic top-level log was refused", err)
	}
	content = append(content, []byte(`
{"Action":"pass","Package":"github.com/urnetwork/server/v2026/connect/sim-latency","Test":"TestSyntheticCompleteQualification/unprecommitted-child"}`)...)
	if err := validateCurrentQgUnitLog(content, []string{name}); err == nil {
		t.Fatal("unexpected nested event was filtered into an admissible release log")
	}
}
