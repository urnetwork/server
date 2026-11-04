// Synthetic event streams and provider/daemon seams force prospective suite
// omissions, command downgrade and replay without running candidate code.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Every artifact is test-owned; the binary is hashed data, never executable.
func sourceMinimumWorkCorrectnessArtifacts(t *testing.T) (sourceBuildSandboxSettings, sourceBuildArtifacts, string) {
	t.Helper()
	settings := sourceSandboxTestSettings()
	settings.StateDirectory = t.TempDir()
	owner := strings.Repeat("a", 64)
	artifacts := sourceBuildArtifacts{Directory: filepath.Join(settings.StateDirectory, owner), ImageId: "sha256:" + strings.Repeat("b", 64)}
	binary := []byte("synthetic source binary artifact; never executed")
	digest := sha256.Sum256(binary)
	artifacts.SimulatorSha256 = hex.EncodeToString(digest[:])
	for name, data := range map[string][]byte{
		filepath.Join("compile", "output", "sim-latency"):   binary,
		filepath.Join("metadata", "output", "metadata.tar"): sourceBuildMetadataArchive(t, []*tar.Header{{Name: "version.mod", Typeflag: tar.TypeReg, Size: 4}}),
	} {
		path := filepath.Join(artifacts.Directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writePrivateReviewFile(path, data); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(artifacts.Directory, "correctness", "metadata"), 0o700) })
	return settings, artifacts, owner
}

// Package/name keys let each mandatory root be independently omitted or skipped.
func sourceMinimumWorkCorrectnessEvents(t *testing.T, omitted, skipped string) []byte {
	t.Helper()
	return sourceMinimumWorkCorrectnessEventsWithSupplemental(t, omitted, skipped, "")
}

// A prefix-selected product root supplements rather than replaces the census.
func sourceMinimumWorkCorrectnessEventsWithSupplemental(t *testing.T, omitted, skipped, supplemental string) []byte {
	t.Helper()
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	emit := func(action, pkg, name string) {
		if err := encoder.Encode(map[string]string{"Action": action, "Package": pkg, "Test": name}); err != nil {
			t.Fatal(err)
		}
	}
	required := sourceCorrectnessRequiredTestsForMinimumWork(true)
	packages := make([]string, 0, len(required))
	for pkg := range required {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	for _, pkg := range packages {
		for _, name := range required[pkg] {
			key := pkg + "/" + name
			if key == omitted {
				continue
			}
			emit("run", pkg, name)
			action := "pass"
			if key == skipped {
				action = "skip"
			}
			emit(action, pkg, name)
		}
		if pkg == "github.com/urnetwork/server/connect" && supplemental != "" {
			emit("run", pkg, supplemental)
			emit("pass", pkg, supplemental)
		}
		emit("pass", pkg, "")
	}
	return output.Bytes()
}

// The historical resident-contract prefix also executes new product roots;
// extending instrument coverage must not narrow that existing execution scope.
func TestSourceMinimumWorkCorrectnessRetainsSupplementalResidentRoots(t *testing.T) {
	script := sourceCorrectnessSuiteScriptForMinimumWork(true)
	match := regexp.MustCompile(`-run '([^']+)' ./connect(?:;|$)`).FindStringSubmatch(script)
	if len(match) != 2 || match[1] != "^TestResidentContract" || strings.Count(script, "' ./connect;") != 1 {
		t.Fatal("prospective suite narrowed or repeated the historical resident-contract package", script)
	}
	selector, err := regexp.Compile(match[1])
	if err != nil || !selector.MatchString("TestResidentContractSyntheticSupplemental") || selector.MatchString("TestSyntheticUnrelated") {
		t.Fatal("historical supplemental resident-contract root was not selected", err)
	}
	output := sourceMinimumWorkCorrectnessEventsWithSupplemental(t, "", "", "TestResidentContractSyntheticSupplemental")
	if count, err := countSourceCorrectnessTestsForMinimumWork(output, true); err != nil || count != 49 {
		t.Fatal("prospective event parser refused the selected supplemental root", count, err)
	}
}

// Actual source qualification must stop before host evidence when only the old
// suite passed. This covers explicit adoption and proof-free inherited policy.
func TestSourceMinimumWorkQualificationRejectsLegacyOnlyCorrectness(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		request := sourceBuildImageMinimumWorkFixture(t, inherited)
		sandbox, artifacts, owner := sourceMinimumWorkCorrectnessArtifacts(t)
		settings := defaultSourceBuildQualifierSettings()
		settings.Sandbox = sandbox
		input := sourceBuildSandboxInput{Owner: owner, Phase: "correctness", MinimumWork: true, ImageId: artifacts.ImageId, Commit: request.Proposal.Commits["server"],
			SourceDirectory: filepath.Join(artifacts.Directory, "source"), MetadataDirectory: filepath.Join(artifacts.Directory, "correctness", "metadata"),
			OutputDirectory: filepath.Join(artifacts.Directory, "correctness", "output")}
		observed := sourceContainmentInspectFixture(t, input, sandbox)
		daemon := &sourceSandboxDaemonFixture{input: input, startOutput: string(sourceCorrectnessEventFixture(t, ""))}
		starts, providerCalls, hostCalls := 0, 0, 0
		docker := func(ctx context.Context, args []string, out, diagnostics io.Writer) error {
			if args[0] == "image" {
				return json.NewEncoder(out).Encode(map[string]any{"Id": artifacts.ImageId, "Config": map[string]any{"Labels": map[string]string{
					"com.urnetwork.review-build.owner": owner, "com.urnetwork.competition.image-kind": "evaluator-base"}}})
			}
			if args[0] == "inspect" && args[2] == "{{json .}}" {
				return json.NewEncoder(out).Encode(observed)
			}
			if args[0] == "start" {
				starts++
			}
			return daemon.run(ctx, args, out, diagnostics)
		}
		client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
			providerCalls++
			stream := sourceTestToolStream(t, "run_prescribed_tests", "{}", false)
			if providerCalls == 2 {
				stream = winnerModelFixtureStreamForModel(t, `{"passed":true,"summary":"Synthetic provider observed the test result."}`, sourceTestModelName)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
		})}
		host := func(context.Context, reviewedSourceBuildRequest, sourceBuildArtifacts, string, sourceBuildSandboxSettings, sourceHostProbeSettings) (*sourceProspectiveHostProfile, string, error) {
			hostCalls++
			return nil, "", errors.New("synthetic host must not follow incomplete correctness")
		}
		qualification := sourceBuildQualificationRequest{Operation: "qualify_exact_reviewed_source_image", Owner: owner, Build: request, Artifacts: artifacts}
		receipt, err := executeSourceBuildQualifier(context.Background(), qualification, settings, client, "synthetic-provider-key", docker, host)
		if err == nil || !strings.Contains(err.Error(), "correctness suite") || receipt != nil || starts != 1 || providerCalls != 1 || hostCalls != 0 || !daemon.removed {
			t.Fatal("minimum-work qualification accepted legacy-only testing or failed at another boundary", inherited, receipt, err, starts, providerCalls, hostCalls)
		}
		if _, err := os.Lstat(filepath.Join(artifacts.Directory, "qualifier-receipt.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("incomplete correctness published a qualification receipt", err)
		}
		if receipt, err := executeSourceBuildQualifier(context.Background(), qualification, settings, client, "synthetic-provider-key", docker, host); err == nil || receipt != nil || starts != 1 || providerCalls != 1 || hostCalls != 0 {
			t.Fatal("failed correctness replay resampled its original evidence", inherited, receipt, err, starts, providerCalls, hostCalls)
		}
	}
}

// All fifteen new roots are mandatory independently; package success and every
// other passing root cannot hide even one skipped or omitted policy regression.
func TestSourceMinimumWorkCorrectnessRequiresEveryApprovedRoot(t *testing.T) {
	full := sourceMinimumWorkCorrectnessEvents(t, "", "")
	if count, err := countSourceCorrectnessTestsForMinimumWork(full, true); err != nil || count != 48 {
		t.Fatal("complete prospective test census rejected", count, err)
	}
	legacy := sourceCorrectnessRequiredTests()
	additional := 0
	for pkg, names := range sourceCorrectnessRequiredTestsForMinimumWork(true) {
		for _, name := range names {
			old := false
			for _, previous := range legacy[pkg] {
				old = old || previous == name
			}
			if old {
				continue
			}
			additional++
			key := pkg + "/" + name
			for _, output := range [][]byte{sourceMinimumWorkCorrectnessEvents(t, key, ""), sourceMinimumWorkCorrectnessEvents(t, "", key)} {
				if _, err := countSourceCorrectnessTestsForMinimumWork(output, true); err == nil {
					t.Fatal("prospective test omission or skip was accepted", key)
				}
			}
		}
	}
	if additional != 15 {
		t.Fatal("prospective test inventory lost an approved root", additional)
	}
}

// Read the approved overlay itself, not moving-main declarations. This is a
// file-only provenance check and grants no approval or image qualification.
func TestSourceMinimumWorkCorrectnessCensusMatchesApprovedPatch(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("launch", "G2-INSTRUMENT-OPERATOR-APPROVAL-2026-10-31.json"))
	if err != nil {
		t.Fatal(err)
	}
	var approval model.CompetitionTrustedInstrumentApproval
	if err := json.Unmarshal(content, &approval); err != nil {
		t.Fatal(err)
	}
	if err := controller.ValidateCompetitionTrustedInstrumentApproval(&approval); err != nil {
		t.Fatal("required test membership must use an authenticated approved patch", err)
	}
	declared := map[string]bool{}
	path := ""
	for _, line := range strings.Split(string(approval.Patch), "\n") {
		if next, ok := strings.CutPrefix(line, "+++ b/"); ok {
			path = next
		}
		if signature, ok := strings.CutPrefix(line, "+func Test"); ok {
			name, _, found := strings.Cut("Test"+signature, "(")
			if !found || !strings.HasSuffix(path, "_test.go") {
				t.Fatal("approved test declaration has no exact test-file identity")
			}
			pkg := "github.com/urnetwork/server"
			if directory := filepath.ToSlash(filepath.Dir(path)); directory != "." {
				pkg += "/" + directory
			}
			declared[pkg+"/"+name] = true
		}
	}
	legacy := sourceCorrectnessRequiredTests()
	matched := 0
	for pkg, names := range sourceCorrectnessRequiredTestsForMinimumWork(true) {
		for _, name := range names {
			old := false
			for _, previous := range legacy[pkg] {
				old = old || previous == name
			}
			if !old {
				if !declared[pkg+"/"+name] {
					t.Fatal("required root is absent from the exact approved patch", pkg, name)
				}
				matched++
			}
		}
	}
	if matched != 15 || len(declared) != matched {
		t.Fatal("the prospective suite differs from the exact approved test inventory", matched, len(declared))
	}
}

// The constant is the historical protocol's exact source-defined digest, not
// a new release identity. Legacy requests and later policy inheritance diverge.
func TestSourceMinimumWorkCorrectnessPreservesLegacySuiteIdentity(t *testing.T) {
	if sourceCorrectnessSuiteDigestForMinimumWork(false) != "db073c8785827c77de1f9be4c39d864ed0959e47ed10bc291c955f0fe80f82e5" ||
		sourceCorrectnessSuiteScriptForMinimumWork(false) != sourceCorrectnessSuiteScript() ||
		!reflect.DeepEqual(sourceCorrectnessRequiredTestsForMinimumWork(false), sourceCorrectnessRequiredTests()) {
		t.Fatal("historical correctness script, census or digest changed")
	}
	requests := []reviewedSourceBuildRequest{sourceBuildContextFixture(t), sourceBuildImageMinimumWorkFixture(t, false), sourceBuildImageMinimumWorkFixture(t, true)}
	for index, request := range requests {
		artifacts := sourceBuildArtifacts{ImageId: "sha256:" + strings.Repeat("b", 64)}
		plan := sourceCorrectnessTestPlan(request, artifacts, strings.Repeat("a", 64))
		if plan.SuiteSha256 != sourceCorrectnessSuiteDigestForMinimumWork(index != 0) || (plan.SuiteSha256 == sourceCorrectnessSuiteDigest()) != (index == 0) {
			t.Fatal("source test plan downgraded its immutable generation policy", index, plan)
		}
	}
}

// Both model journals, the prescribed execution and retained containment use
// the same prospective identity; successful replay makes no second test draw.
func TestSourceMinimumWorkIndependentTestsBindPlanExecutionContainmentAndReplay(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		request := sourceBuildImageMinimumWorkFixture(t, inherited)
		sandbox, artifacts, owner := sourceMinimumWorkCorrectnessArtifacts(t)
		input := sourceBuildSandboxInput{Owner: owner, Phase: "correctness", MinimumWork: true, ImageId: artifacts.ImageId, Commit: request.Proposal.Commits["server"],
			SourceDirectory: filepath.Join(artifacts.Directory, "source"), MetadataDirectory: filepath.Join(artifacts.Directory, "correctness", "metadata"),
			OutputDirectory: filepath.Join(artifacts.Directory, "correctness", "output")}
		observed := sourceContainmentInspectFixture(t, input, sandbox)
		daemon := &sourceSandboxDaemonFixture{input: input, startOutput: string(sourceMinimumWorkCorrectnessEvents(t, "", ""))}
		starts, providerCalls := 0, 0
		docker := func(ctx context.Context, args []string, out, diagnostics io.Writer) error {
			if args[0] == "image" {
				return json.NewEncoder(out).Encode(map[string]any{"Id": artifacts.ImageId, "Config": map[string]any{"Labels": map[string]string{
					"com.urnetwork.review-build.owner": owner, "com.urnetwork.competition.image-kind": "evaluator-base"}}})
			}
			if args[0] == "inspect" && args[2] == "{{json .}}" {
				return json.NewEncoder(out).Encode(observed)
			}
			if args[0] == "create" && !strings.HasSuffix(args[len(args)-1], sourceCorrectnessSuiteScriptForMinimumWork(true)) {
				t.Fatal("actual correctness command omitted the prospective suite")
			}
			if args[0] == "start" {
				starts++
			}
			return daemon.run(ctx, args, out, diagnostics)
		}
		plan := sourceCorrectnessTestPlan(request, artifacts, owner)
		client := &http.Client{Transport: winnerReviewTransport(func(request *http.Request) (*http.Response, error) {
			providerCalls++
			stream := sourceTestToolStream(t, "run_prescribed_tests", "{}", false)
			if providerCalls == 1 {
				var payload struct {
					Input []struct{ Content string } `json:"input"`
				}
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || len(payload.Input) != 1 {
					t.Fatal("independent model invocation lost its exact plan", err)
				}
				var actual sourceTestPlan
				if err := json.Unmarshal([]byte(payload.Input[0].Content), &actual); err != nil || actual != plan {
					t.Fatal("model received a different suite identity from its executor", actual, plan, err)
				}
			} else if providerCalls == 2 {
				stream = winnerModelFixtureStreamForModel(t, `{"passed":true,"summary":"Synthetic provider observed every prospective correctness root."}`, sourceTestModelName)
			} else {
				t.Fatal("completed model evidence was requested again")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
		})}
		evidence, digest, err := executeSourceBuildIndependentTests(context.Background(), client, "synthetic-provider-key", request, artifacts, owner,
			sandbox, defaultSourceTestModelSettings(), defaultSourceTestJournalSettings(), docker)
		if err != nil || evidence == nil || !evidence.Passed || evidence.Execution.TestCount != 48 || evidence.Execution.SuiteSha256 != plan.SuiteSha256 || !validSha256(digest) || starts != 1 || providerCalls != 2 || !daemon.removed {
			t.Fatal("prospective composed test path lost its observed policy or evidence", inherited, evidence, err, starts, providerCalls)
		}
		replay, replayDigest, err := executeSourceBuildIndependentTests(context.Background(), client, "synthetic-provider-key", request, artifacts, owner,
			sandbox, defaultSourceTestModelSettings(), defaultSourceTestJournalSettings(), docker)
		if err != nil || !reflect.DeepEqual(evidence, replay) || replayDigest != digest || starts != 1 || providerCalls != 2 {
			t.Fatal("prospective completed execution/model/containment replay changed facts", inherited, err, starts, providerCalls)
		}
	}
}

// A completed old-suite journal for the same graph must block, not be relabeled
// as new coverage or erased to obtain another build/test attempt.
func TestSourceMinimumWorkCorrectnessRejectsHistoricalJournalWithoutRerun(t *testing.T) {
	request := sourceBuildImageMinimumWorkFixture(t, false)
	sandbox, artifacts, owner := sourceMinimumWorkCorrectnessArtifacts(t)
	plan := sourceCorrectnessTestPlan(request, artifacts, owner)
	plan.SuiteSha256 = sourceCorrectnessSuiteDigest()
	execution := sourceTestExecutionFixture(plan)
	settings := defaultSourceTestJournalSettings()
	if _, err := executeJournaledSourceTest(context.Background(), filepath.Join(artifacts.Directory, "independent-test-journal"), plan,
		settings.Now().Add(time.Hour), settings, func(context.Context) (*sourceTestExecution, error) { return &execution, nil }, func(*sourceTestExecution) error { return nil }); err != nil {
		t.Fatal("synthetic completed historical journal setup failed", err)
	}
	called := false
	docker := func(context.Context, []string, io.Writer, io.Writer) error { called = true; return nil }
	if result, err := executeJournaledSourceCorrectness(context.Background(), request, artifacts, owner, sandbox, settings, docker); err == nil || result != nil || called {
		t.Fatal("historical suite journal was relabeled or resampled", result, err, called)
	}
}

// Observing the old argv cannot attest the extended suite, and a legacy suite
// digest cannot be retained against a minimum-work containment input.
func TestSourceMinimumWorkCorrectnessContainmentRejectsDowngradedSuite(t *testing.T) {
	input, settings := sourceContainmentInputFixture(), sourceSandboxTestSettings()
	observed := sourceContainmentInspectFixture(t, input, settings)
	input.MinimumWork = true
	content, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSourceCorrectnessContainment(content, observed.Id, input, settings); err == nil {
		t.Fatal("historical observed command attested prospective correctness")
	}
	execution := sourceTestExecution{ImageId: input.ImageId, SuiteSha256: sourceCorrectnessSuiteDigest(), ExitCode: 0, TestCount: 33, TestLogsSha256: strings.Repeat("a", 64)}
	if _, err := retainSourceCorrectnessContainment(input, settings, execution); err == nil || !strings.Contains(err.Error(), "complete exact-image test execution") {
		t.Fatal("historical suite digest reached prospective containment retention", err)
	}
}
