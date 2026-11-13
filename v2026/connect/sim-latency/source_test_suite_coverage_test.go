// Synthetic manifests and observed test events bind the prospective coverage
// correctness protocol without provider, container, build or runtime authority.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

// Both adoption and later product-only generations inherit the same approved
// coverage policy. Every identity is synthetic and no source ref is changed.
func sourceCoverageCorrectnessRequest(t *testing.T, inherited bool) reviewedSourceBuildRequest {
	t.Helper()
	request := sourceBuildImageMinimumWorkFixture(t, inherited)
	manifest, err := decodeSourceManifest(request.SourceManifest)
	if err != nil {
		t.Fatal(err)
	}
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	for index := 1; index < len(manifest.EvaluationSource.Generations); index++ {
		manifest.EvaluationSource.Generations[index].WorkCoveragePolicy = policy.Clone()
	}
	for index := range manifest.EvaluationSource.Rounds {
		if manifest.EvaluationSource.Rounds[index].Epoch >= 2 {
			manifest.EvaluationSource.Rounds[index].WorkCoveragePolicy = policy.Clone()
		}
	}
	proof := manifest.EvaluationSource.Generations[1].TrustedInstrument
	proof.Approval.WorkCoveragePolicy = policy.Clone()
	proof.Approval.ApprovalSha256 = ""
	proof.Approval.ApprovalSha256, err = model.CompetitionAutomationDigest(proof.Approval)
	if err != nil {
		t.Fatal(err)
	}
	if !inherited {
		request.TrustedInstrument = proof
	}
	request.SourceManifest, err = yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSourceBuildRequest(request); err != nil {
		t.Fatal("invalid synthetic coverage correctness request", err)
	}
	return request
}

// The real trusted executor must reject a successful old48 stream when the
// authenticated generation requires G7, not merely bind G7 in an image label.
func TestSourceCoverageQualificationRejectsMinimumWorkOnlyCorrectness(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		request := sourceCoverageCorrectnessRequest(t, inherited)
		sandbox, artifacts, owner := sourceMinimumWorkCorrectnessArtifacts(t)
		daemon := &sourceSandboxDaemonFixture{input: sourceBuildSandboxInput{Owner: owner, Phase: "correctness", MinimumWork: true},
			startOutput: string(sourceMinimumWorkCorrectnessEvents(t, "", ""))}
		starts := 0
		docker := func(ctx context.Context, args []string, output, diagnostics io.Writer) error {
			if args[0] == "start" {
				starts++
			}
			return daemon.run(ctx, args, output, diagnostics)
		}
		result, err := executeSourceCorrectnessSuite(t.Context(), request, artifacts, owner, sandbox, docker)
		if err == nil || result != nil {
			t.Fatal("coverage image qualified without any G7 execution", inherited, result, err)
		}
		if starts != 1 || !daemon.removed || !strings.Contains(err.Error(), "omitted or skipped a mandatory test") {
			t.Fatal("coverage refusal did not exercise the completed native event census", inherited, starts, daemon.removed, err)
		}
	}
}

// Complete observed events, not a summary count, prove every required root.
func sourceCoverageCorrectnessEvents(t *testing.T, mutation, selected, supplemental string) []byte {
	t.Helper()
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	suite, err := newSourceCorrectnessSuite(true, &policy)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	emit := func(action, pkg, name string) {
		if err := encoder.Encode(map[string]string{"Action": action, "Package": pkg, "Test": name}); err != nil {
			t.Fatal(err)
		}
	}
	packages := []string{}
	for pkg := range suite.requiredTests {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	for _, pkg := range packages {
		for _, name := range suite.requiredTests[pkg] {
			match := pkg+"/"+name == selected
			if match && mutation == "omit" {
				continue
			}
			actualPackage := pkg
			if match && mutation == "wrong-package" {
				actualPackage += "/synthetic-wrong"
			}
			if !match || mutation != "no-run" {
				emit("run", actualPackage, name)
			}
			action := "pass"
			if match && mutation == "skip" {
				action = "skip"
			} else if match && mutation == "fail" {
				action = "fail"
			}
			emit(action, actualPackage, name)
			if match && mutation == "duplicate" {
				emit("run", actualPackage, name)
				emit("pass", actualPackage, name)
			}
		}
		if pkg == "github.com/urnetwork/server/v2026/connect" && supplemental != "" {
			emit("run", pkg, supplemental)
			emit("pass", pkg, supplemental)
		}
		emit("pass", pkg, "")
	}
	return output.Bytes()
}

// Historical source requests retain the same exact native scripts, digests
// and serialized model plans. An explicit coverage leaf alone chooses v3.
func TestSourceCoverageCorrectnessPreservesHistoricalIdentities(t *testing.T) {
	for _, minimumWork := range []bool{false, true} {
		suite, err := newSourceCorrectnessSuite(minimumWork, nil)
		if err != nil || suite.script != sourceCorrectnessSuiteScriptForMinimumWork(minimumWork) ||
			suite.sha256 != sourceCorrectnessSuiteDigestForMinimumWork(minimumWork) ||
			!reflect.DeepEqual(suite.requiredTests, sourceCorrectnessRequiredTestsForMinimumWork(minimumWork)) {
			t.Fatal("coverage protocol changed a historical suite identity", minimumWork, err)
		}
	}
	if sourceCorrectnessSuiteDigest() != "db073c8785827c77de1f9be4c39d864ed0959e47ed10bc291c955f0fe80f82e5" {
		t.Fatal("historical33 digest changed")
	}
	for _, inherited := range []bool{false, true} {
		request := sourceBuildImageMinimumWorkFixture(t, inherited)
		artifacts := sourceBuildArtifacts{ImageId: "sha256:" + strings.Repeat("a", 64)}
		plan, err := sourceCorrectnessTestPlan(request, artifacts, strings.Repeat("b", 64))
		if err != nil {
			t.Fatal(err)
		}
		legacy := sourceTestPlan{Owner: strings.Repeat("b", 64), ImageId: artifacts.ImageId,
			SourceLockSha256: request.Proposal.SourceLockSha256, PatchSha256: request.Proposal.PatchSha256,
			SuiteSha256: sourceCorrectnessSuiteDigestForMinimumWork(true)}
		actualBytes, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		expectedBytes, err := json.Marshal(legacy)
		if err != nil || !bytes.Equal(actualBytes, expectedBytes) {
			t.Fatal("coverage protocol changed an original minimum-work journal plan", err)
		}
	}
	t.Logf("historical33_suite_sha256=%s historical48_suite_sha256=%s", sourceCorrectnessSuiteDigest(), sourceCorrectnessSuiteDigestForMinimumWork(true))
}

// A real inherited generation cannot downgrade merely because it has no new
// instrument proof. Invalid explicit policy never silently selects legacy48.
func TestSourceCoverageCorrectnessSelectsAuthenticatedGeneration(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		request := sourceCoverageCorrectnessRequest(t, inherited)
		suite, err := sourceCorrectnessSuiteForRequest(request)
		if err != nil || suite.workCoveragePolicy == nil || *suite.workCoveragePolicy != model.DefaultCompetitionWorkCoveragePolicy() ||
			suite.sha256 == sourceCorrectnessSuiteDigestForMinimumWork(true) || suite.sha256 == sourceCorrectnessSuiteDigest() {
			t.Fatal("authenticated coverage generation selected an old or mismatched suite", inherited, err)
		}
		plan, err := sourceCorrectnessTestPlan(request, sourceBuildArtifacts{ImageId: "sha256:" + strings.Repeat("a", 64)}, strings.Repeat("b", 64))
		if err != nil || plan.SuiteSha256 != suite.sha256 {
			t.Fatal("provider plan lost authenticated coverage identity", inherited, err)
		}
		if count, err := countSourceCorrectnessTestEvents(sourceCoverageCorrectnessEvents(t, "", "", ""), suite.requiredTests); err != nil || count != 70 {
			t.Fatal("coverage protocol did not require the full70 roots", count, err)
		}
	}
	for _, coverage := range []*model.CompetitionWorkCoveragePolicy{
		{Kind: "synthetic-unknown/1", MinimumPageCoverageRatio: .95, MinimumByteCoverageRatio: .95},
		{Kind: model.CompetitionWorkCoverageKind, MinimumPageCoverageRatio: .5, MinimumByteCoverageRatio: .95},
		{Kind: model.CompetitionWorkCoverageKind, MinimumPageCoverageRatio: .95},
	} {
		if _, err := newSourceCorrectnessSuite(true, coverage); err == nil {
			t.Fatal("invalid coverage policy downgraded or entered a correctness suite", coverage)
		}
	}
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	if _, err := newSourceCorrectnessSuite(false, &coverage); err == nil {
		t.Fatal("coverage protocol acquired an unapproved legacy G2 generation")
	}
}

// Source request validation, not candidate options or apparent proof presence,
// rejects omission, stale generation, altered approval and malformed manifests.
func TestSourceCoverageCorrectnessRefusesDowngradedOrMalformedRequests(t *testing.T) {
	for _, mutation := range []string{"policy-downgrade", "changed-approval", "wrong-generation", "malformed"} {
		request := sourceCoverageCorrectnessRequest(t, true)
		manifest, err := decodeSourceManifest(request.SourceManifest)
		if err != nil {
			t.Fatal(err)
		}
		switch mutation {
		case "policy-downgrade":
			manifest.EvaluationSource.Generations[request.SourceGeneration].WorkCoveragePolicy = nil
		case "changed-approval":
			manifest.EvaluationSource.Generations[1].TrustedInstrument.Approval.WorkCoveragePolicy = nil
		case "wrong-generation":
			request.SourceGeneration++
		}
		request.SourceManifest, err = yaml.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if mutation == "malformed" {
			request.SourceManifest = []byte("synthetic: [malformed")
		}
		if _, err := sourceCorrectnessTestPlan(request, sourceBuildArtifacts{}, strings.Repeat("a", 64)); err == nil {
			t.Fatal("unvalidated source request produced a provider plan", mutation)
		}
	}
}

// Every new root is independently mandatory, even when all old48 and every
// other new root pass. Product-specific resident roots remain supplemental.
func TestSourceCoverageCorrectnessRequiresEveryObservedG7Root(t *testing.T) {
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	suite, err := newSourceCorrectnessSuite(true, &policy)
	if err != nil {
		t.Fatal(err)
	}
	legacy := sourceCorrectnessRequiredTestsForMinimumWork(true)
	checked := 0
	for pkg, names := range suite.requiredTests {
		for _, name := range names {
			old := false
			for _, original := range legacy[pkg] {
				old = old || original == name
			}
			if old {
				continue
			}
			checked++
			for _, mutation := range []string{"omit", "skip", "fail", "wrong-package", "no-run", "duplicate"} {
				if _, err := countSourceCorrectnessTestEvents(sourceCoverageCorrectnessEvents(t, mutation, pkg+"/"+name, ""), suite.requiredTests); err == nil {
					t.Fatal("incomplete G7 events acquired coverage authority", name, mutation)
				}
			}
		}
	}
	if checked != 22 {
		t.Fatal("coverage protocol's independent root census changed", checked)
	}
	if count, err := countSourceCorrectnessTestEvents(sourceCoverageCorrectnessEvents(t, "", "", "TestResidentContractSyntheticSupplemental"), suite.requiredTests); err != nil || count != 71 {
		t.Fatal("coverage protocol narrowed the original resident-contract prefix", count, err)
	}
}

// The protocol names actual source roots rather than plausible labels. The
// eventual approved overlay must retain these exact tests and their helpers.
func TestSourceCoverageCorrectnessCensusMatchesInstrumentSource(t *testing.T) {
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	suite, err := newSourceCorrectnessSuite(true, &policy)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, file := range []string{"work_coverage_plan_test.go", "work_coverage_test.go", "score_work_coverage_test.go", "evaluator_work_coverage_test.go", "../../model/competition_work_coverage_policy_test.go"} {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		pkg := "github.com/urnetwork/server/v2026/connect/sim-latency"
		if strings.HasPrefix(file, "../../model/") {
			pkg = "github.com/urnetwork/server/v2026/model"
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "Test") {
				declared[pkg+"/"+function.Name.Name] = true
			}
		}
	}
	legacy := sourceCorrectnessRequiredTestsForMinimumWork(true)
	for pkg, names := range suite.requiredTests {
		for _, name := range names {
			old := false
			for _, original := range legacy[pkg] {
				old = old || original == name
			}
			if !old {
				key := pkg + "/" + name
				if !declared[key] || !strings.Contains(suite.script, name) {
					t.Fatal("coverage census names a missing or unselected native root", key)
				}
				delete(declared, key)
			}
		}
	}
	if len(declared) != 0 {
		t.Fatal("coverage source contains unbound mandatory roots", declared)
	}
}

// The observed container command, execution receipt and minimum count must all
// use coverage70; old48 or a kind-only partial success cannot be retained.
func TestSourceCoverageCorrectnessContainmentRejectsDowngrade(t *testing.T) {
	input, settings := sourceContainmentInputFixture(), sourceSandboxTestSettings()
	input.MinimumWork = true
	oldObserved := sourceContainmentInspectFixture(t, input, settings)
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	input.WorkCoveragePolicy = policy.Clone()
	suite, err := newSourceCorrectnessSuite(true, &policy)
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(oldObserved)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSourceCorrectnessContainment(content, oldObserved.Id, input, settings); err == nil {
		t.Fatal("old48 observed command attested coverage70")
	}
	for _, execution := range []sourceTestExecution{
		{ImageId: input.ImageId, SuiteSha256: sourceCorrectnessSuiteDigestForMinimumWork(true), TestCount: 48, TestLogsSha256: strings.Repeat("a", 64)},
		{ImageId: input.ImageId, SuiteSha256: suite.sha256, TestCount: 69, TestLogsSha256: strings.Repeat("a", 64)},
	} {
		if _, err := retainSourceCorrectnessContainment(input, settings, execution); err == nil || !strings.Contains(err.Error(), "complete exact-image test execution") {
			t.Fatal("downgraded coverage execution entered retained containment", execution, err)
		}
	}
	input.WorkCoveragePolicy.MinimumByteCoverageRatio = .5
	if _, err := sourceBuildSandboxArguments(input, settings); err == nil {
		t.Fatal("sandbox selected a command from unsupported coverage tolerance")
	}
}

// Completed historical journals and incomplete new receipts remain immutable.
// A changed suite cannot replay or erase them to obtain a new test draw.
func TestSourceCoverageCorrectnessJournalsRejectDowngradeWithoutRerun(t *testing.T) {
	for _, historical := range []bool{false, true} {
		request := sourceCoverageCorrectnessRequest(t, false)
		sandbox, artifacts, owner := sourceMinimumWorkCorrectnessArtifacts(t)
		plan, err := sourceCorrectnessTestPlan(request, artifacts, owner)
		if err != nil {
			t.Fatal(err)
		}
		if historical {
			plan.SuiteSha256 = sourceCorrectnessSuiteDigestForMinimumWork(true)
		}
		execution := sourceTestExecutionFixture(plan)
		execution.TestCount = 48
		settings := defaultSourceTestJournalSettings()
		runs := 0
		_, setupErr := executeJournaledSourceTest(t.Context(), filepath.Join(artifacts.Directory, "independent-test-journal"), plan,
			settings.Now().Add(time.Hour), settings, func(context.Context) (*sourceTestExecution, error) { runs++; return &execution, nil }, func(*sourceTestExecution) error { return nil })
		if (setupErr == nil) != historical || runs != 1 {
			t.Fatal("synthetic historical/partial journal control failed at another boundary", historical, setupErr, runs)
		}
		called := false
		docker := func(context.Context, []string, io.Writer, io.Writer) error { called = true; return nil }
		if result, err := executeJournaledSourceCorrectness(t.Context(), request, artifacts, owner, sandbox, settings, docker); err == nil || result != nil || called || runs != 1 {
			t.Fatal("old or failed coverage journal was relabeled or resampled", historical, result, err, called)
		}
	}
}

// The complete qualifier path shares one policy/digest across actual argv,
// Go events, model plan, journals, containment and the returned typed receipt.
// All provider, daemon and host boundaries are explicitly synthetic seams.
func TestSourceCoverageQualifierBindsExecutionPlanContainmentAndReplay(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		request := sourceCoverageCorrectnessRequest(t, inherited)
		sandbox, artifacts, owner := sourceMinimumWorkCorrectnessArtifacts(t)
		artifacts.ImageId = "sha256:" + strings.Repeat("9", 64)
		settings := defaultSourceBuildQualifierSettings()
		settings.Sandbox = sandbox
		suite, err := sourceCorrectnessSuiteForRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := sourceCorrectnessTestPlan(request, artifacts, owner)
		if err != nil {
			t.Fatal(err)
		}
		input := sourceBuildSandboxInput{Owner: owner, Phase: "correctness", MinimumWork: true, WorkCoveragePolicy: suite.workCoveragePolicy.Clone(),
			ImageId: artifacts.ImageId, Commit: request.Proposal.Commits["server"],
			SourceDirectory: filepath.Join(artifacts.Directory, "source"), MetadataDirectory: filepath.Join(artifacts.Directory, "correctness", "metadata"),
			OutputDirectory: filepath.Join(artifacts.Directory, "correctness", "output")}
		observed := sourceContainmentInspectFixture(t, input, sandbox)
		daemon := &sourceSandboxDaemonFixture{input: input, startOutput: string(sourceCoverageCorrectnessEvents(t, "", "", ""))}
		starts, providerCalls, hostCalls := 0, 0, 0
		docker := func(ctx context.Context, args []string, output, diagnostics io.Writer) error {
			if args[0] == "image" {
				return json.NewEncoder(output).Encode(map[string]any{"Id": artifacts.ImageId, "Config": map[string]any{"Labels": map[string]string{
					"com.urnetwork.review-build.owner": owner, "com.urnetwork.competition.image-kind": "evaluator-base"}}})
			}
			if args[0] == "inspect" && args[2] == "{{json .}}" {
				return json.NewEncoder(output).Encode(observed)
			}
			if args[0] == "create" && !strings.HasSuffix(args[len(args)-1], suite.script) {
				t.Fatal("actual sandbox dispatch lost the coverage suite")
			}
			if args[0] == "start" {
				starts++
			}
			return daemon.run(ctx, args, output, diagnostics)
		}
		client := &http.Client{Transport: winnerReviewTransport(func(request *http.Request) (*http.Response, error) {
			providerCalls++
			stream := sourceTestToolStream(t, "run_prescribed_tests", "{}", false)
			if providerCalls == 1 {
				var payload struct{ Input []struct{ Content string } }
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || len(payload.Input) != 1 {
					t.Fatal("model input lost the authenticated plan", err)
				}
				var actual sourceTestPlan
				if err := json.Unmarshal([]byte(payload.Input[0].Content), &actual); err != nil || actual != plan {
					t.Fatal("independent model received a downgraded coverage suite", err)
				}
			} else if providerCalls == 2 {
				stream = winnerModelFixtureStreamForModel(t, `{"passed":true,"summary":"Synthetic complete coverage correctness observation."}`, sourceTestModelName)
			} else {
				t.Fatal("completed coverage model observation was requested again")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}, nil
		})}
		profile := &sourceProspectiveHostProfile{Schema: 1, Kind: "prospective-source-hardware-fingerprint/v1",
			ImageId: artifacts.ImageId, BaseImageId: request.Proposal.EvaluatedImageDigest, ReviewId: sourceBuildAuthorizationIdentity(request),
			SourceLockSha256: request.Proposal.SourceLockSha256, QualificationSha256: strings.Repeat("c", 64)}
		profileBytes, err := json.Marshal(profile)
		if err != nil {
			t.Fatal(err)
		}
		profileHash := sha256.Sum256(profileBytes)
		profileDigest := hex.EncodeToString(profileHash[:])
		host := func(context.Context, reviewedSourceBuildRequest, sourceBuildArtifacts, string, sourceBuildSandboxSettings, sourceHostProbeSettings) (*sourceProspectiveHostProfile, string, error) {
			hostCalls++
			path := filepath.Join(artifacts.Directory, "prospective-host-profile.json")
			if err := publishSourceTestRecord(path, profileBytes); errors.Is(err, os.ErrExist) {
				if err := verifySourceTestLogBundle(path, profileDigest, int64(settings.Host.MaximumDocumentBytes)); err != nil {
					return nil, "", err
				}
			} else if err != nil {
				return nil, "", err
			}
			return profile, profileDigest, nil
		}
		qualification := sourceBuildQualificationRequest{Operation: "qualify_exact_reviewed_source_image", Owner: owner, Build: request, Artifacts: artifacts}
		receipt, err := executeSourceBuildQualifier(t.Context(), qualification, settings, client, "synthetic-provider-key", docker, host)
		if err != nil || receipt == nil || validateSourceBuildReceipt(request, *receipt) != nil ||
			!reflect.DeepEqual(receipt.WorkCoveragePolicy, suite.workCoveragePolicy) || receipt.SimulatorSha256 != artifacts.SimulatorSha256 ||
			starts != 1 || providerCalls != 2 || hostCalls != 1 || !daemon.removed || profile.RuntimeQualified {
			t.Fatal("coverage qualifier lost source/policy/observed evidence or claimed runtime authority", inherited, receipt, err, starts, providerCalls, hostCalls)
		}
		var evidence sourceTestModelEvidence
		if err := readSourceTestRecord(filepath.Join(artifacts.Directory, "independent-test-model-evidence.json"), int64(settings.MaximumRequestBytes), &evidence); err != nil ||
			evidence.Execution.SuiteSha256 != suite.sha256 || evidence.Execution.TestCount != 70 || !evidence.Passed {
			t.Fatal("retained model execution lost the complete coverage census", evidence.Execution, err)
		}
		replay, err := executeSourceBuildQualifier(t.Context(), qualification, settings, client, "synthetic-provider-key", docker, host)
		if err != nil || replay == nil || !receipt.Equal(*replay) || starts != 1 || providerCalls != 2 || hostCalls != 2 {
			t.Fatal("coverage replay resampled correctness or changed retained receipt", inherited, replay, err, starts, providerCalls, hostCalls)
		}
	}
}

// The native provider boundaries reject a successful count below the required
// coverage census before issuing a completion/attestation for that claim.
func TestSourceCoverageTestModelsRejectUndercountedSuccess(t *testing.T) {
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	suite, err := newSourceCorrectnessSuite(true, &policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, snow := range []bool{false, true} {
		plan := sourceTestPlanFixture()
		plan.SuiteSha256 = suite.sha256
		settings := defaultSourceTestModelSettings()
		if snow {
			provider := defaultWinnerSnowProviderSettings()
			settings.SnowProvider = &provider
			plan.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
		}
		calls, runs := 0, 0
		client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
			calls++
			if snow || calls != 1 {
				t.Fatal("undercounted coverage success reached an attestation")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(sourceTestToolStream(t, "run_prescribed_tests", "{}", false)))}, nil
		})}
		evidence, err := executeSourceTestModel(t.Context(), client, "synthetic-provider-key", plan, settings, func(context.Context) (sourceTestExecution, error) {
			runs++
			execution := sourceTestExecutionFixture(plan)
			execution.TestCount = 48
			return execution, nil
		})
		wantedCalls := 1
		if snow {
			wantedCalls = 0
		}
		if err == nil || evidence != nil || calls != wantedCalls || runs != 1 {
			t.Fatal("partial successful coverage execution acquired provider evidence", snow, evidence, err, calls, runs)
		}
	}
}

// A genuine-shaped failed partial run remains failed, not forged success or a
// new test draw. Complete success and failure preserve the exact same schema.
func TestSourceCoverageSnowPreservesCompleteAndFailedObservedCensus(t *testing.T) {
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	suite, err := newSourceCorrectnessSuite(true, &policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, failed := range []bool{false, true} {
		plan := sourceTestPlanFixture()
		plan.SuiteSha256, plan.WinnerReviewPolicy = suite.sha256, model.CompetitionSnowWinnerReviewPolicy
		execution := sourceTestExecutionFixture(plan)
		execution.TestCount = 70
		if failed {
			execution.TestCount, execution.ExitCode = 3, 1
		}
		provider := defaultWinnerSnowProviderSettings()
		settings := defaultSourceTestModelSettings()
		settings.SnowProvider = &provider
		calls, runs := 0, 0
		client := sourceSnowTestClient(t, execution, &calls, "")
		evidence, err := executeSourceTestModel(t.Context(), client, "", plan, settings, func(context.Context) (sourceTestExecution, error) { runs++; return execution, nil })
		if err != nil || evidence == nil || evidence.Passed == failed || evidence.Execution != execution || calls != len(provider.Members) || runs != 1 {
			t.Fatal("coverage provider changed complete/failed observation or resampled tests", failed, evidence, err, calls, runs)
		}
	}
}

// The optional native technical bridge remains nonproduction. A new explicit
// coverage approval chooses scorer3/full-policy probes, never an old48 claim.
func TestSourceCoverageTechnicalBridgeBindsFullPolicyWithoutProductionAuthority(t *testing.T) {
	request := sourceCoverageCorrectnessRequest(t, false)
	approval := request.TrustedInstrument.Approval
	plan, err := planG2InstrumentBaseImage(approval, strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		ImageKind          string                               `json:"image_kind"`
		ProductionEligible bool                                 `json:"production_eligible"`
		RuntimeQualified   bool                                 `json:"runtime_qualified"`
		ScorerVersion      string                               `json:"scorer_version"`
		WorkCoveragePolicy *model.CompetitionWorkCoveragePolicy `json:"work_coverage_policy"`
	}
	if err := json.Unmarshal(plan.Identity, &identity); err != nil || identity.ImageKind != "evaluator-instrument-qualification" || identity.ProductionEligible || identity.RuntimeQualified ||
		identity.ScorerVersion != model.CompetitionWorkCoverageScorerVersion || !reflect.DeepEqual(identity.WorkCoveragePolicy, approval.WorkCoveragePolicy) {
		t.Fatal("technical coverage identity downgraded policy or acquired production authority", err)
	}
	phase := g2InstrumentBasePolicyPhase(approval.WorkCoveragePolicy)
	args, err := g2InstrumentBaseCliArguments(strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), phase, sourceSandboxTestSettings())
	if err != nil || phase != "coverage-policy" || !reflect.DeepEqual(args[len(args)-3:], []string{"score-policy", "--g2-volume-policy=minimum-work/1", "--work-coverage-policy=" + model.CompetitionWorkCoverageKind}) {
		t.Fatal("coverage technical CLI probe lost its exact fixed command", err)
	}
	content, err := json.Marshal(map[string]any{"score_schema": 1, "g2_volume_policy": model.CompetitionG2VolumePolicyMinimumWork,
		"scorer_version": model.CompetitionWorkCoverageScorerVersion, "work_coverage_policy": approval.WorkCoveragePolicy})
	if err != nil || validateG2InstrumentBaseScorePolicy(content, approval.WorkCoveragePolicy) != nil {
		t.Fatal("full compiled coverage capability was refused", err)
	}
	legacy := []byte(`{"score_schema":1,"g2_volume_policy":"minimum-work/1","scorer_version":"sim-latency-score/2"}`)
	if validateG2InstrumentBaseScorePolicy(legacy, approval.WorkCoveragePolicy) == nil || g2InstrumentBasePolicyPhase(nil) != "policy" || validateG2InstrumentBaseScorePolicy(legacy, nil) != nil {
		t.Fatal("technical coverage probe accepted legacy capability or changed historical policy")
	}
	legacyArgs, err := g2InstrumentBaseCliArguments(strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), "policy", sourceSandboxTestSettings())
	if err != nil || !reflect.DeepEqual(legacyArgs[len(legacyArgs)-2:], []string{"score-policy", "--g2-volume-policy=minimum-work/1"}) {
		t.Fatal("historical technical CLI arguments changed", err)
	}
}
