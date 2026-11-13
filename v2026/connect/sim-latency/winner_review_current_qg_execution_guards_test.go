// Offline hooks cannot reach an installed wrapper, provider socket or runtime
// directory. These tests exercise the real current executor/native validators.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// Every identity and observation is explicitly synthetic. No private/live
// source or runtime artifact is needed by the offline adapter suite.
func currentQgExecutionFixture(t *testing.T) (currentQgQualificationPlan, []currentQgQualificationExpectation, []currentQgQualificationSource, currentQgQualificationSettings) {
	t.Helper()
	workload, production := currentQgQualificationReadCensuses(t)
	sources, err := currentQgQualificationSources(workload, production)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := winnerSnowJudgmentFixture()
	seed.Packet.SourceCommits = map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		seed.Packet.SourceCommits[repository] = strings.Repeat("a", 40)
	}
	seed.Packet.Entry.BaseSourceLockSha256, err = model.CompetitionSourceLockDigest(seed.Packet.SourceCommits)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := currentQgQualificationRequests(seed.Packet, seed.WorkloadReview, sources)
	if err != nil {
		t.Fatal(err)
	}
	expectations := []currentQgQualificationExpectation{}
	for _, source := range sources {
		expectations = append(expectations, source.Expectation)
	}
	config := currentQgExecutionConfig{TotalTimeoutSeconds: 3600, Broker: currentQgQualificationPin{Sha256: strings.Repeat("b", 64)}, ProviderSettings: currentQgQualificationPin{Sha256: strings.Repeat("e", 64)}, Image: currentQgQualificationPin{Sha256: strings.Repeat("d", 64)}}
	plan, err := currentQgExecutionPlan(config, &currentQgQualificationImage{ImageId: "sha256:" + strings.Repeat("c", 64)}, defaultWinnerSnowProviderSettings(), requests, expectations)
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultCurrentQgQualificationSettings()
	settings.CaseTimeout, settings.TotalTimeout, settings.CleanupGrace = time.Minute, 5*time.Minute, time.Second
	return plan, expectations, sources, settings
}

// The mock constructs actual native evidence shape, without transport or a
// claim that its causal statements are real provider reasoning.
func currentQgExecutionMockResponse(t *testing.T, request winnerReviewBrokerRequest, expected currentQgQualificationExpectation, responseIds []string) []byte {
	t.Helper()
	_, base := winnerSnowJudgmentFixture()
	provider := defaultWinnerSnowProviderSettings()
	members := []winnerSnowMemberEvidence{}
	for index, member := range provider.Members {
		serialized, _ := json.Marshal(base)
		var judgment winnerModelJudgment
		if err := json.Unmarshal(serialized, &judgment); err != nil {
			t.Fatal(err)
		}
		judgment.Decision = expected.Decision
		judgment.WorkloadPreservation.Disposition = expected.WorkloadDisposition
		judgment.ProductionBenefit.Disposition = expected.ProductionDisposition
		judgment.ProductionBenefit.SourcePaths = []string{currentQgFixturePath}
		judgment.PublicExplanation, judgment.PrivateEvidence = "Synthetic offline response contract.", "Synthetic offline evidence, not a provider receipt."
		for index := range judgment.Findings {
			judgment.Findings[index].Disposition, judgment.Findings[index].ReasonCode, judgment.Findings[index].EvidenceSha256 = "pass", "APPROVED", ""
		}
		switch expected.Decision {
		case "approve":
			judgment.ReasonCodes = []string{"APPROVED"}
		case "disqualify":
			judgment.ReasonCodes = []string{"NO_PRODUCTION_BENEFIT"}
			judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "fail", "NO_PRODUCTION_BENEFIT"
			if expected.WorkloadDisposition == "fail" {
				judgment.ReasonCodes = append(judgment.ReasonCodes, "WORKLOAD_SPECIAL_CASING")
				judgment.Findings[5].Disposition, judgment.Findings[5].ReasonCode = "fail", "WORKLOAD_SPECIAL_CASING"
			}
		case "uncertain":
			judgment.ReasonCodes = []string{"SOURCE_NOT_READY"}
			judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "uncertain", "SOURCE_NOT_READY"
		}
		output, err := json.Marshal(judgment)
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("synthetic-%s-%s", request.Packet.Entry.JobId.String(), member.Name)
		if len(responseIds) == 2 {
			id = responseIds[index]
		}
		members = append(members, winnerSnowMemberEvidence{Name: member.Name, RequestedModel: member.Model, Model: member.Model, Revision: member.Revision, Reasoning: member.Reasoning,
			ProviderResponseId: id, RequestSha256: strings.Repeat("1", 64), ResponseSha256: strings.Repeat("2", 64), Usage: json.RawMessage(`{"input_tokens":10,"output_tokens":10}`), Judgment: judgment, ProviderOutput: string(output)})
	}
	input, err := winnerModelInput(request)
	if err != nil {
		t.Fatal(err)
	}
	evidence := winnerSnowEvidence{Kind: "snow-model-union-review/v1", Model: model.CompetitionSnowWinnerReviewModel, PolicyModel: model.CompetitionSnowWinnerReviewModel,
		Reasoning: model.CompetitionSnowWinnerReviewReasoning, ProviderSettings: provider, ProviderSettingsSha256: strings.Repeat("e", 64), InputSha256: currentQgQualificationHash(input), Members: members,
		Judgment: aggregateWinnerSnow(members, provider, request)}
	content, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	result := winnerSnowBrokerResult(request, evidence.Judgment, content)
	content, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// Hooks return fixture bytes only; real wrapper/containment paths are absent.
func currentQgExecutionOfflineHooks(t *testing.T, plan currentQgQualificationPlan, expectations []currentQgQualificationExpectation) currentQgQualificationHooks {
	t.Helper()
	now := time.Now().UTC()
	hooks := currentQgQualificationHooks{Mode: "offline_unit_mock", Now: func() time.Time { return now },
		ValidatePlan: func(actual currentQgQualificationPlan) error {
			if !reflect.DeepEqual(actual, plan) {
				return errors.New("synthetic changed frozen plan")
			}
			return nil
		}, AuthenticateContainment: func(_, _, _ []byte, _ bool) (string, error) { return strings.Repeat("9", 64), nil }}
	hooks.Invoke = func(_ context.Context, input []byte) ([]byte, []byte, error) {
		var request winnerReviewBrokerRequest
		if err := json.Unmarshal(input, &request); err != nil {
			return nil, nil, err
		}
		if currentQgQualificationContainsExpectedLabels(input) {
			t.Fatal("private expected labels entered the mock broker input")
		}
		if request.Operation == "qualification_cancel_before_provider" {
			return nil, nil, errors.New("synthetic actual-shape cancellation")
		}
		for index, item := range plan.Cases {
			if item.Request.Packet.Entry.JobId == request.Packet.Entry.JobId {
				return currentQgExecutionMockResponse(t, request, expectations[index], nil), nil, nil
			}
		}
		return nil, nil, errors.New("synthetic unexpected case")
	}
	hooks.ValidateResponse = func(output []byte, request winnerReviewBrokerRequest, expected string) (currentQgQualificationObservation, error) {
		for index, item := range plan.Cases {
			if item.Request.Packet.Entry.JobId == request.Packet.Entry.JobId && expected == expectations[index].Decision {
				return validateCurrentQgExecutionResponse(output, request, expectations[index], defaultWinnerSnowProviderSettings(), strings.Repeat("e", 64))
			}
		}
		return currentQgQualificationObservation{}, errors.New("synthetic unexpected outcome")
	}
	return hooks
}

// Complete offline shape cannot manufacture provider qualification authority.
func TestCurrentQgQualificationExecutionOfflineNeverQualifies(t *testing.T) {
	plan, expectations, _, settings := currentQgExecutionFixture(t)
	directory := t.TempDir()
	report, err := executeCurrentQgQualification(t.Context(), directory, plan, settings, currentQgExecutionOfflineHooks(t, plan, expectations))
	if err != nil || report == nil || !report.Complete || report.Qualified || !report.NoMainWrites || len(report.Observations) != 19 {
		t.Fatalf("offline adapter lost a case or claimed genuine authority: %+v %v", report, err)
	}
	for index, observation := range report.Observations {
		if !observation.Passed || observation.Expected != expectations[index].Decision || observation.Observed != expectations[index].Decision {
			t.Fatal("offline typed observation mismatch", index)
		}
	}
}

// A completed mismatched judgment is retained and stops the very next draw.
func TestCurrentQgQualificationExecutionStopsOnMismatchWithoutRetry(t *testing.T) {
	plan, expectations, _, settings := currentQgExecutionFixture(t)
	hooks := currentQgExecutionOfflineHooks(t, plan, expectations)
	invoke, calls := hooks.Invoke, 0
	hooks.Invoke = func(ctx context.Context, input []byte) ([]byte, []byte, error) {
		calls++
		if calls == 2 {
			return []byte(`{"synthetic_completed_but_wrong":true}`), nil, nil
		}
		return invoke(ctx, input)
	}
	directory := t.TempDir()
	report, err := executeCurrentQgQualification(t.Context(), directory, plan, settings, hooks)
	if err == nil || calls != 2 || report == nil || report.Qualified || report.Complete || len(report.Observations) != 2 {
		t.Fatal("completed mismatch was retried, skipped or authorized", calls, err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "case-02", "broker.stdout"))
	if err != nil || !bytes.Contains(content, []byte("synthetic_completed_but_wrong")) {
		t.Fatal("original completed mismatch was not retained", err)
	}
}

// A response identity cannot be reused across cases even with its model name
// swapped. The global census uses raw ids, not model-prefixed aliases.
func TestCurrentQgQualificationExecutionRejectsGlobalResponseReplay(t *testing.T) {
	plan, expectations, _, settings := currentQgExecutionFixture(t)
	hooks := currentQgExecutionOfflineHooks(t, plan, expectations)
	invoke, calls := hooks.Invoke, 0
	hooks.Invoke = func(ctx context.Context, input []byte) ([]byte, []byte, error) {
		calls++
		if calls == 2 || calls == 3 {
			var request winnerReviewBrokerRequest
			if err := json.Unmarshal(input, &request); err != nil {
				t.Fatal(err)
			}
			ids := []string{"synthetic-shared-a", "synthetic-shared-b"}
			if calls == 3 {
				ids[0], ids[1] = ids[1], ids[0]
			}
			return currentQgExecutionMockResponse(t, request, expectations[calls-1], ids), nil, nil
		}
		return invoke(ctx, input)
	}
	report, err := executeCurrentQgQualification(t.Context(), t.TempDir(), plan, settings, hooks)
	if err == nil || calls != 3 || report == nil || report.Complete || report.Qualified {
		t.Fatal("provider response id replay was masked by a different member name", err)
	}
	// Even a broken offline response hook cannot turn a missing identity into
	// a complete qualifying shape. The first real-shape judgment stops here.
	hooks = currentQgExecutionOfflineHooks(t, plan, expectations)
	invoke, calls = hooks.Invoke, 0
	validate := hooks.ValidateResponse
	hooks.Invoke = func(ctx context.Context, input []byte) ([]byte, []byte, error) {
		calls++
		return invoke(ctx, input)
	}
	hooks.ValidateResponse = func(output []byte, request winnerReviewBrokerRequest, expected string) (currentQgQualificationObservation, error) {
		observation, err := validate(output, request, expected)
		observation.ProviderResponseIds, observation.ProviderResponseId = nil, ""
		return observation, err
	}
	report, err = executeCurrentQgQualification(t.Context(), t.TempDir(), plan, settings, hooks)
	if err == nil || calls != 2 || report == nil || report.Complete || report.Qualified {
		t.Fatal("offline transport mode suppressed the missing native receipt identity guard", err)
	}
}

// Output roots/clocks/receipt formatting never change a semantic draw key.
// Actual wrapper/image/source changes are separately precommitted releases.
func TestCurrentQgQualificationExecutionClaimsBindSemanticRelease(t *testing.T) {
	plan, _, _, _ := currentQgExecutionFixture(t)
	key, err := currentQgQualificationSnowClaimKey(plan, plan.Cases[1])
	if err != nil {
		t.Fatal(err)
	}
	changed := plan
	changed.ImageReceiptSha256, changed.UnitReceiptSha256, changed.ProviderSettingsSha256 = strings.Repeat("7", 64), strings.Repeat("8", 64), strings.Repeat("9", 64)
	second, err := currentQgQualificationSnowClaimKey(changed, changed.Cases[1])
	if err != nil || second != key {
		t.Fatal("receipt/raw settings formatting allowed a second draw", err)
	}
	changed.ActualImageId = "sha256:" + strings.Repeat("f", 64)
	third, err := currentQgQualificationSnowClaimKey(changed, changed.Cases[1])
	if err != nil || third == key {
		t.Fatal("actual immutable image release was not bound", err)
	}
	item := plan.Cases[1]
	item.Request.Instructions += " substituted"
	if _, err := currentQgQualificationSnowClaimKey(plan, item); err == nil {
		t.Fatal("unhashed semantic request mutation accepted")
	}
}

// Actual unit streams, not booleans/count assertions, must include each exact
// top-level test once with a single package pass and no skips/failures.
func TestCurrentQgQualificationExecutionUnitLogCensusIsExact(t *testing.T) {
	names := currentQgExecutionTestNames()
	stream := []byte{}
	appendEvent := func(action, test string) {
		content, _ := json.Marshal(map[string]any{"Action": action, "Package": "github.com/urnetwork/server/v2026/connect/sim-latency", "Test": test})
		stream = append(stream, append(content, '\n')...)
	}
	appendEvent("start", "")
	for _, name := range names {
		appendEvent("run", name)
		appendEvent("pass", name)
	}
	appendEvent("pass", "")
	if err := validateCurrentQgUnitLog(stream, names); err != nil {
		t.Fatal("complete synthetic event census refused", err)
	}
	for _, changed := range [][]byte{bytes.Replace(stream, []byte(`"Action":"pass"`), []byte(`"Action":"skip"`), 1), bytes.Replace(stream, []byte(names[0]), []byte("TestSyntheticUnexpected"), 1), append(append([]byte{}, stream...), stream...), stream[:len(stream)-2]} {
		if validateCurrentQgUnitLog(changed, names) == nil {
			t.Fatal("skipped/unknown/duplicate/truncated unit event census accepted")
		}
	}
}

// Changing authorization timestamp/file/output root cannot consume the same
// semantic complete plan twice. All state stays under this private fixture.
func TestCurrentQgQualificationExecutionAuthorizationIsSingleAttempt(t *testing.T) {
	plan, _, _, settings := currentQgExecutionFixture(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || os.Chmod(parent, 0700) != nil {
		t.Fatal("private authorization fixture unavailable", err)
	}
	settings.StateRoot = filepath.Join(parent, "attempts")
	config := currentQgExecutionConfig{OutputDirectory: filepath.Join(parent, "first"), TotalTimeoutSeconds: 3600, ExecutionPlan: currentQgQualificationPin{Sha256: strings.Repeat("a", 64)}}
	for index := 0; index < 2; index++ {
		config.OutputDirectory = filepath.Join(parent, fmt.Sprintf("output-%d", index))
		authorization := currentQgExecutionAuthorization{Schema: 1, Kind: "winner-review-current-qg-one-attempt-authorization/v1", Policy: model.CompetitionSnowWinnerReviewPolicy,
			ExecutionPlanSha256: config.ExecutionPlan.Sha256, OutputDirectory: config.OutputDirectory, TotalTimeoutSeconds: config.TotalTimeoutSeconds, AuthorizedAt: time.Now().UTC().Add(-time.Minute), SingleAttempt: true, NoExternalProviders: true}
		content, _ := json.Marshal(authorization)
		path := filepath.Join(parent, fmt.Sprintf("authority-%d.json", index))
		if err := writePrivateReviewFile(path, content); err != nil {
			t.Fatal(err)
		}
		config.Authorization = currentQgQualificationPin{Path: path, Sha256: currentQgQualificationHash(content)}
		err := consumeCurrentQgExecutionAuthorization(config, settings, plan)
		if index == 0 && err != nil || index == 1 && err == nil {
			t.Fatal("semantic authorization was not consumed exactly once", index, err)
		}
	}
}

// Old Mini/three-model configs stop before any protected path or broker call.
func TestCurrentQgQualificationExecutionRejectsHistoricalRelease(t *testing.T) {
	for _, kind := range []string{"winner-review-provider-qualification-config", "winner-review-snow-provider-qualification-config", ""} {
		if _, _, err := authenticateCurrentQgExecutionRelease(currentQgExecutionConfig{Schema: 1, Kind: kind}, defaultCurrentQgQualificationSettings()); err == nil {
			t.Fatal("historical/incomplete release enabled the current genuine command")
		}
	}
}

// Explicit source names are part of the independently authenticated unit
// receipt; this is a lower bound, not a claim of full dependency closure.
func currentQgExecutionSourceNames() []string {
	return []string{"winner_review_current_qg_preparation_test.go", "winner_review_current_qg_preparation_guards_test.go", "winner_review_current_qg_execution_test.go",
		"winner_review_current_qg_execution_support_test.go", "winner_review_current_qg_execution_guards_test.go", "winner_review_current_qg_containment_test.go",
		"winner_review_current_qg_snow_containment_test.go", "winner_review_current_qg_containment_guards_test.go", "winner_review_current_qg_fixtures_test.go",
		"winner_review_current_qg_fixture_guards_test.go", "review-qualification-current-qg-observation-probe.go.txt", "workload-review-qualification-v1.json", "production-benefit-review-qualification-v1.json",
		"run-broker.sh", "Dockerfile.snow", "winner_review_snow.go", "winner_review_openai.go", "winner_review_production.go", "winner_review_workload.go", "source_winner_review.go", "source_review_template.go", "competition_production_benefit_policy.go"}
}

// No ordinary test may silently disappear from the three independently read
// qualification streams. Command helpers are inert passes, never skipped.
func currentQgExecutionTestNames() []string {
	return []string{"TestCurrentQgQualificationPreparationCommand", "TestCurrentQgQualificationPreparationClosedCensus", "TestCurrentQgQualificationPreparationBindsContextWithoutLabels",
		"TestCurrentQgQualificationPreparationExportIsPrivateAndNonAuthorizing", "TestCurrentQgQualificationPreparationChecksEveryNativeMember", "TestCurrentQgQualificationPreparationSeparatesLabelKeysAndIdentity",
		"TestCurrentQgQualificationExecutionCommand", "TestCurrentQgQualificationFixtureCommand", "TestCurrentQgQualificationExecutionOfflineNeverQualifies",
		"TestCurrentQgQualificationExecutionStopsOnMismatchWithoutRetry", "TestCurrentQgQualificationExecutionRejectsGlobalResponseReplay", "TestCurrentQgQualificationExecutionClaimsBindSemanticRelease",
		"TestCurrentQgQualificationExecutionUnitLogCensusIsExact", "TestCurrentQgQualificationExecutionAuthorizationIsSingleAttempt", "TestCurrentQgQualificationExecutionRejectsHistoricalRelease",
		"TestCurrentQgQualificationFixtureEvidenceBindsEveryVariant", "TestCurrentQgQualificationFixtureClocksStayOutsideSemanticInput",
		"TestCurrentQgQualificationContainmentAuthenticatesCompleteCapture", "TestCurrentQgQualificationContainmentCancellationIsBeforeInput", "TestCurrentQgQualificationContainmentRejectsExtraAuthority"}
}
