// Snow attestations mechanically copy trusted execution metadata. Synthetic
// schema/producer regressions never substitute model claims for actual tests.
package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// An empty summary satisfied the old type-only constraint even though native
// attestation validation required actual summary content. Preserve that RED case.
func TestSourceTestSnowSchemaRejectsPreviouslyValidEmptySummary(t *testing.T) {
	execution := sourceTestExecutionFixture(sourceTestPlanFixture())
	instance := map[string]any{"passed": execution.ExitCode == 0, "summary": "", "image_id": execution.ImageId,
		"suite_sha256": execution.SuiteSha256, "exit_code": execution.ExitCode, "test_count": execution.TestCount, "test_logs_sha256": execution.TestLogsSha256}
	weak := sourceTestSnowAttestationSchema(execution, defaultSourceTestSnowAttestationSettings())
	weak["properties"].(map[string]any)["summary"] = map[string]any{"type": "string"}
	if err := winnerSnowResolvedSchema(t, weak).Validate(instance); err != nil {
		t.Fatal("empty synthetic summary no longer reproduces the weak schema", err)
	}
	if winnerSnowResolvedSchema(t, sourceTestSnowAttestationSchema(execution, defaultSourceTestSnowAttestationSettings())).Validate(instance) == nil {
		t.Fatal("provider contract still permits a native-invalid empty summary")
	}
}

// The provider cannot fabricate a pass, another image/suite/log identity or a
// different observed count/status, regardless of the supplied observation's result.
func TestSourceTestSnowSchemaPinsObservedTruthAndAllCopiedFields(t *testing.T) {
	for _, exitCode := range []int{0, 7} {
		execution := sourceTestExecutionFixture(sourceTestPlanFixture())
		execution.ExitCode = exitCode
		instance := map[string]any{"passed": execution.ExitCode == 0 && execution.TestCount > 0, "summary": "Synthetic trusted executor observation retained.", "image_id": execution.ImageId,
			"suite_sha256": execution.SuiteSha256, "exit_code": execution.ExitCode, "test_count": execution.TestCount, "test_logs_sha256": execution.TestLogsSha256}
		schema := winnerSnowResolvedSchema(t, sourceTestSnowAttestationSchema(execution, defaultSourceTestSnowAttestationSettings()))
		if err := schema.Validate(instance); err != nil {
			t.Fatal("actual synthetic observation did not match its fixed output contract", err)
		}
		original, err := json.Marshal(instance)
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range []struct {
			name   string
			mutate func(map[string]any)
		}{
			{name: "pass truth", mutate: func(v map[string]any) { v["passed"] = execution.ExitCode != 0 }},
			{name: "image", mutate: func(v map[string]any) { v["image_id"] = "sha256:" + strings.Repeat("9", 64) }},
			{name: "suite", mutate: func(v map[string]any) { v["suite_sha256"] = strings.Repeat("9", 64) }},
			{name: "exit code", mutate: func(v map[string]any) { v["exit_code"] = execution.ExitCode + 1 }},
			{name: "test count", mutate: func(v map[string]any) { v["test_count"] = execution.TestCount + 1 }},
			{name: "log digest", mutate: func(v map[string]any) { v["test_logs_sha256"] = strings.Repeat("9", 64) }},
			{name: "empty summary", mutate: func(v map[string]any) { v["summary"] = "" }},
			{name: "oversized summary", mutate: func(v map[string]any) {
				v["summary"] = strings.Repeat("x", defaultSourceTestSnowAttestationSettings().MaximumSummaryBytes+1)
			}},
			{name: "missing observed zero", mutate: func(v map[string]any) { delete(v, "exit_code") }},
			{name: "missing passed", mutate: func(v map[string]any) { delete(v, "passed") }},
			{name: "missing summary", mutate: func(v map[string]any) { delete(v, "summary") }},
			{name: "null test count", mutate: func(v map[string]any) { v["test_count"] = nil }},
			{name: "operator plan", mutate: func(v map[string]any) { v["plan"] = map[string]any{"synthetic": true} }},
			{name: "winner judgment", mutate: func(v map[string]any) { v["decision"] = "approve" }},
			{name: "tool request", mutate: func(v map[string]any) { v["command"] = "synthetic-command-never-executed" }},
		} {
			var changed map[string]any
			if err := json.Unmarshal(original, &changed); err != nil {
				t.Fatal(err)
			}
			change.mutate(changed)
			if schema.Validate(changed) == nil {
				t.Errorf("exit=%d attestation schema accepted changed %s", exitCode, change.name)
			}
		}
	}
}

// The actual producer runs the trusted observation callback once and sends its
// exact schema to both members for both success and genuine observed failure.
func TestSourceTestSnowProducerContractPreservesObservedPassAndFailure(t *testing.T) {
	for _, exitCode := range []int{0, 7} {
		plan := sourceTestPlanFixture()
		plan.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
		execution := sourceTestExecutionFixture(plan)
		execution.ExitCode = exitCode
		settings := defaultSourceTestModelSettings()
		provider := defaultWinnerSnowProviderSettings()
		settings.SnowProvider = &provider
		if settings.MaximumOutputTokens != 4096 || settings.RequestTimeout != 5*time.Minute || settings.TotalTimeout != 3*time.Hour {
			t.Fatal("Snow attestation silently changed its test-agent budgets")
		}
		requests, executions := 0, 0
		evidence, err := executeSourceTestModel(t.Context(), sourceSnowTestClient(t, execution, &requests, ""), "", plan, settings,
			func(context.Context) (sourceTestExecution, error) { executions++; return execution, nil })
		if err != nil || evidence == nil || evidence.Execution != execution || evidence.Passed != (exitCode == 0) || len(evidence.Members) != 2 || requests != 2 || executions != 1 {
			t.Fatal("actual observed pass/failure was replaced or execution/member census changed", evidence, err, requests, executions)
		}
		for _, member := range evidence.Members {
			if member.Passed != evidence.Passed || member.Summary == "" || member.ProviderOutput == "" {
				t.Fatal("member lost the observed truth or its original attestation")
			}
		}
	}
}

// A frontend ignoring the summary constraints still cannot confer native test
// authority, and malformed completed output cannot trigger another execution.
func TestSourceTestSnowProducerRejectsEmptyBlankAndOversizedSummaries(t *testing.T) {
	for _, mutation := range []string{"empty-summary", "blank-summary", "oversized-summary"} {
		plan := sourceTestPlanFixture()
		plan.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
		execution := sourceTestExecutionFixture(plan)
		settings := defaultSourceTestModelSettings()
		provider := defaultWinnerSnowProviderSettings()
		settings.SnowProvider = &provider
		requests, executions := 0, 0
		evidence, err := executeSourceTestModel(t.Context(), sourceSnowTestClient(t, execution, &requests, mutation), "", plan, settings,
			func(context.Context) (sourceTestExecution, error) { executions++; return execution, nil })
		if err == nil || evidence != nil || requests != 1 || executions != 1 {
			t.Fatalf("%s became native authority or reran work: evidence=%v err=%v requests=%d executions=%d", mutation, evidence, err, requests, executions)
		}
	}
}
