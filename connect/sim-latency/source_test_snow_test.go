// Synthetic Responses fixtures exercise the real Snow/test journals without
// contacting providers or running candidate code. Go execution remains singular.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// Every request is intercepted; immutable configured destinations are checked
// as data rather than reached over the network.
func sourceSnowTestClient(t *testing.T, execution sourceTestExecution, calls *int, mutation string) *http.Client {
	t.Helper()
	provider := defaultWinnerSnowProviderSettings()
	return &http.Client{Transport: winnerReviewTransport(func(request *http.Request) (*http.Response, error) {
		index := *calls
		*calls++
		if index >= len(provider.Members) || request.Method != http.MethodPost || request.URL.String() != provider.Endpoint || request.Header.Get("Authorization") != "" {
			t.Fatal("Snow test request changed member order/destination or leaked a key")
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Model      string                  `json:"model"`
			Reasoning  struct{ Effort string } `json:"reasoning"`
			Store      bool                    `json:"store"`
			Stream     bool                    `json:"stream"`
			Tools      []json.RawMessage       `json:"tools"`
			ToolChoice string                  `json:"tool_choice"`
			Input      []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		member := provider.Members[index]
		if payload.Model != member.Model || payload.Reasoning.Effort != member.Reasoning || payload.Store || payload.Stream || len(payload.Tools) != 0 || payload.ToolChoice != "none" || len(payload.Input) != 1 {
			t.Fatal("Snow test gained command/tool authority or changed its pinned provider")
		}
		var observed struct {
			Execution sourceTestExecution `json:"execution"`
		}
		if err := json.Unmarshal([]byte(payload.Input[0].Content), &observed); err != nil || observed.Execution != execution {
			t.Fatal("member did not receive the exact same actual test observation", err)
		}
		if mutation == "missing-member" && index == 2 {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("synthetic unavailable member"))}, nil
		}
		judgment := map[string]any{"passed": execution.ExitCode == 0, "summary": "Synthetic observed fixed-suite result.", "image_id": execution.ImageId,
			"suite_sha256": execution.SuiteSha256, "exit_code": execution.ExitCode, "test_count": execution.TestCount, "test_logs_sha256": execution.TestLogsSha256}
		if mutation == "false-pass" {
			judgment["passed"] = true
		}
		if mutation == "substituted-log" {
			judgment["test_logs_sha256"] = strings.Repeat("0", 64)
		}
		output, err := json.Marshal(judgment)
		if err != nil {
			t.Fatal(err)
		}
		response := map[string]any{"id": "synthetic-snow-response-" + strconv.Itoa(index), "model": member.Model, "status": "completed",
			"usage":  map[string]any{"input_tokens": 17, "output_tokens": 23},
			"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": string(output)}}}}}
		if mutation == "wrong-model" {
			response["model"] = "synthetic-unapproved-member"
		}
		if mutation == "unsupported-tool" {
			response["output"] = []any{map[string]any{"type": "function_call", "name": "run_prescribed_tests", "arguments": "{}"}}
		}
		content, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(content)))}, nil
	})}
}

// The three members observe the same single execution; none can run a tool.
func TestSourceTestSnowExecutesOnceAndRequiresThreeMatchingAttestations(t *testing.T) {
	plan := sourceTestPlanFixture()
	plan.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
	settings := defaultSourceTestModelSettings()
	provider := defaultWinnerSnowProviderSettings()
	settings.SnowProvider = &provider
	execution := sourceTestExecutionFixture(plan)
	requests, executions := 0, 0
	evidence, err := executeSourceTestModel(context.Background(), sourceSnowTestClient(t, execution, &requests, ""), "", plan, settings,
		func(context.Context) (sourceTestExecution, error) { executions++; return execution, nil })
	if err != nil || evidence == nil || !evidence.Passed || evidence.Model != model.CompetitionSnowWinnerReviewModel || evidence.WinnerReviewPolicy != plan.WinnerReviewPolicy ||
		evidence.Reasoning != model.CompetitionSnowWinnerReviewReasoning || len(evidence.Members) != 3 || requests != 3 || executions != 1 || evidence.CallId != "" {
		t.Fatal("Snow test authority omitted a member or invented an execution", evidence, requests, executions, err)
	}
}

// Provider disagreement, missing members and unsupported actions never bless
// an actual failure, substitute log evidence or gain another execution.
func TestSourceTestSnowRejectsMissingMembersFalsePassesAndToolAttempts(t *testing.T) {
	for _, mutation := range []string{"missing-member", "false-pass", "wrong-model", "substituted-log", "unsupported-tool"} {
		plan := sourceTestPlanFixture()
		plan.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
		settings := defaultSourceTestModelSettings()
		provider := defaultWinnerSnowProviderSettings()
		settings.SnowProvider = &provider
		execution := sourceTestExecutionFixture(plan)
		if mutation == "false-pass" {
			execution.ExitCode = 1
		}
		requests, executions := 0, 0
		evidence, err := executeSourceTestModel(context.Background(), sourceSnowTestClient(t, execution, &requests, mutation), "", plan, settings,
			func(context.Context) (sourceTestExecution, error) { executions++; return execution, nil })
		if err == nil || evidence != nil || executions != 1 {
			t.Errorf("%s became Snow test authority: executions=%d evidence=%v error=%v", mutation, executions, evidence, err)
		}
	}
}

// Both durable journals replay exact evidence rather than drawing another
// model response or executing the prescribed Go suite again after restart.
func TestSourceTestSnowJournalReplaysThreeAttestationsAndOneExecution(t *testing.T) {
	fixture, settings := journaledSourceTestModelFixture(t)
	fixture.Plan.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
	provider := defaultWinnerSnowProviderSettings()
	settings.SnowProvider = &provider
	requests, executions := 0, 0
	client := sourceSnowTestClient(t, fixture.Execution, &requests, "")
	run := func(ctx context.Context) (sourceTestExecution, error) {
		result, err := executeJournaledSourceTest(ctx, fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings,
			func(context.Context) (*sourceTestExecution, error) {
				executions++
				value := fixture.Execution
				return &value, nil
			}, fixture.verify)
		if err != nil {
			return sourceTestExecution{}, err
		}
		return *result, nil
	}
	first, err := executeSourceTestModel(context.Background(), client, "", fixture.Plan, settings, run)
	if err != nil {
		t.Fatal(err)
	}
	second, err := executeSourceTestModel(context.Background(), client, "", fixture.Plan, settings, run)
	if err != nil || !reflect.DeepEqual(first, second) || requests != 3 || executions != 1 {
		t.Fatal("Snow restart drew new provider/execution evidence", requests, executions, err)
	}
	for _, member := range provider.Members {
		var observation sourceTestProviderObservation
		if err := readSourceTestRecord(filepath.Join(settings.ResponseJournalDirectory, "snow-"+member.Name+"-observation.json"), sourceTestProviderRecordLimit(settings), &observation); err != nil || len(observation.Stream) == 0 {
			t.Fatal("member observation was not durably retained before use", member.Name, err)
		}
	}
}

// Transport errors are durable uncertainty, not permission to repeat either
// a failed member request or the already completed actual Go execution.
func TestSourceTestSnowJournalHoldsMissingMemberWithoutReruns(t *testing.T) {
	fixture, settings := journaledSourceTestModelFixture(t)
	fixture.Plan.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
	provider := defaultWinnerSnowProviderSettings()
	settings.SnowProvider = &provider
	requests, executions := 0, 0
	client := sourceSnowTestClient(t, fixture.Execution, &requests, "missing-member")
	run := func(ctx context.Context) (sourceTestExecution, error) {
		result, err := executeJournaledSourceTest(ctx, fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings,
			func(context.Context) (*sourceTestExecution, error) {
				executions++
				value := fixture.Execution
				return &value, nil
			}, fixture.verify)
		if err != nil {
			return sourceTestExecution{}, err
		}
		return *result, nil
	}
	for range 2 {
		if evidence, err := executeSourceTestModel(context.Background(), client, "", fixture.Plan, settings, run); err == nil || evidence != nil {
			t.Fatal("missing member granted Snow test authority", evidence, err)
		}
	}
	if requests != 3 || executions != 1 {
		t.Fatal("missing Snow member retried provider or actual tests", requests, executions)
	}
}

// A missing/mixed provider cannot fall through to OpenAI or start candidate tests.
func TestSourceTestSnowPolicyRefusesFallbackBeforeAnyExecution(t *testing.T) {
	plan := sourceTestPlanFixture()
	plan.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
	client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("Snow fallback reached any provider")
		return nil, errors.New("unreachable")
	})}
	if evidence, err := executeSourceTestModel(context.Background(), client, "synthetic-unused-legacy-key", plan, defaultSourceTestModelSettings(),
		func(context.Context) (sourceTestExecution, error) {
			t.Fatal("missing Snow provider started execution")
			return sourceTestExecution{}, nil
		}); err == nil || evidence != nil {
		t.Fatal("missing Snow provider gained test authority")
	}
}
