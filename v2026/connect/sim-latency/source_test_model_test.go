// Mock provider streams prove the low test agent cannot invent execution,
// broaden its tool authority or convert failed tests into passing evidence.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func sourceTestPlanFixture() sourceTestPlan {
	return sourceTestPlan{Owner: strings.Repeat("a", 64), ImageId: "sha256:" + strings.Repeat("b", 64), SourceLockSha256: strings.Repeat("c", 64),
		PatchSha256: strings.Repeat("d", 64), SuiteSha256: strings.Repeat("e", 64)}
}

func sourceTestExecutionFixture(plan sourceTestPlan) sourceTestExecution {
	return sourceTestExecution{ImageId: plan.ImageId, SuiteSha256: plan.SuiteSha256, TestCount: 7, DurationMillis: 123, TestLogsSha256: strings.Repeat("f", 64)}
}

func sourceTestToolStream(t *testing.T, tool, arguments string, duplicate bool) string {
	t.Helper()
	call := map[string]any{"type": "function_call", "name": tool, "call_id": "call_synthetic", "arguments": arguments, "status": "completed"}
	output := []any{map[string]any{"type": "reasoning", "encrypted_content": "synthetic-opaque-encrypted-reasoning", "summary": []any{}},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Synthetic pre-tool commentary is not a test result."}}}, call}
	if duplicate {
		output = append(output, call)
	}
	content, err := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_synthetic_tool", "status": "completed", "model": "gpt-6.1-sol", "output": output}})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(content) + "\n\n"
}

func TestSourceTestModelPinsAuthorityExecutesOnceAndPreservesOpaqueState(t *testing.T) {
	plan := sourceTestPlanFixture()
	requests, executions := 0, 0
	client := &http.Client{Transport: winnerReviewTransport(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.String() != "https://api.openai.com/v1/responses" || request.Header.Get("Authorization") != "Bearer synthetic-model-key" || request.Method != http.MethodPost {
			t.Fatal("independent tester changed provider authority")
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Model             string                  `json:"model"`
			Reasoning         struct{ Effort string } `json:"reasoning"`
			Store             bool                    `json:"store"`
			Stream            bool                    `json:"stream"`
			ParallelToolCalls bool                    `json:"parallel_tool_calls"`
			Tools             []json.RawMessage       `json:"tools"`
			ToolChoice        json.RawMessage         `json:"tool_choice"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "gpt-6.1-sol" || payload.Reasoning.Effort != "low" || payload.Store || !payload.Stream || payload.ParallelToolCalls || strings.Contains(string(body), "synthetic-model-key") {
			t.Fatal("test model policy or key isolation changed")
		}
		stream := sourceTestToolStream(t, "run_prescribed_tests", "{}", false)
		if requests == 1 {
			if len(payload.Tools) != 1 || !strings.Contains(string(payload.ToolChoice), "run_prescribed_tests") {
				t.Fatal("test invocation was not forced to the only prescribed tool")
			}
		} else {
			if requests != 2 || len(payload.Tools) != 0 || string(payload.ToolChoice) != `"none"` || !strings.Contains(string(body), "synthetic-opaque-encrypted-reasoning") || !strings.Contains(string(body), "function_call_output") {
				t.Fatal("completion lost opaque tool state or gained another execution")
			}
			stream = winnerModelFixtureStreamForModel(t, `{"passed":true,"summary":"All prescribed tests passed."}`, sourceTestModelName)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Header: make(http.Header)}, nil
	})}
	settings := defaultSourceTestModelSettings()
	settings.RequestTimeout = 10 * time.Second
	settings.TotalTimeout = 2 * time.Hour
	evidence, err := executeSourceTestModel(context.Background(), client, "synthetic-model-key", plan, settings, func(ctx context.Context) (sourceTestExecution, error) {
		executions++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < time.Hour {
			t.Fatal("short provider timeout incorrectly bounded actual test execution")
		}
		return sourceTestExecutionFixture(plan), nil
	})
	if err != nil || evidence == nil || !evidence.Passed || evidence.Model != "gpt-6.1-sol" || evidence.Reasoning != "low" || requests != 2 || executions != 1 || !validSha256(evidence.PlanSha256) {
		t.Fatal("independent test evidence lost actual execution or provider provenance", evidence, err)
	}
}

// A valid terminal envelope from another review role cannot authorize either
// execution or its completion. Positive fixtures must use the requested model.
func TestSourceTestModelRejectsUnrequestedModelAtEveryPhase(t *testing.T) {
	for _, wrongPhase := range []int{1, 2} {
		plan := sourceTestPlanFixture()
		requests, executions := 0, 0
		client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
			requests++
			stream := sourceTestToolStream(t, "run_prescribed_tests", "{}", false)
			if requests == 2 {
				stream = winnerModelFixtureStreamForModel(t, `{"passed":true,"summary":"Synthetic observed execution."}`, sourceTestModelName)
			}
			if requests == wrongPhase {
				stream = strings.ReplaceAll(stream, sourceTestModelName, winnerReviewMiniProviderSnapshot)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Header: make(http.Header)}, nil
		})}
		evidence, err := executeSourceTestModel(context.Background(), client, "synthetic-model-key", plan, defaultSourceTestModelSettings(), func(context.Context) (sourceTestExecution, error) {
			executions++
			return sourceTestExecutionFixture(plan), nil
		})
		var diagnostic *winnerModelResponseDiagnostic
		if err == nil || evidence != nil || !errors.As(err, &diagnostic) || diagnostic.Cause != "model_mismatch" || requests != wrongPhase || executions != wrongPhase-1 {
			t.Fatalf("phase %d accepted an unrequested model or changed execution count: requests=%d executions=%d evidence=%v error=%v", wrongPhase, requests, executions, evidence, err)
		}
	}
}

func TestSourceTestModelRejectsToolEscapesAndRepeatedExecution(t *testing.T) {
	for _, shape := range []struct {
		tool, args string
		duplicate  bool
	}{
		{tool: "run_shell", args: "{}"}, {tool: "run_prescribed_tests", args: `{"command":"synthetic-command"}`},
		{tool: "run_prescribed_tests", args: "null"}, {tool: "run_prescribed_tests", args: "[]"}, {tool: "run_prescribed_tests", args: "{}", duplicate: true},
	} {
		client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sourceTestToolStream(t, shape.tool, shape.args, shape.duplicate))), Header: make(http.Header)}, nil
		})}
		evidence, err := executeSourceTestModel(context.Background(), client, "synthetic-model-key", sourceTestPlanFixture(), defaultSourceTestModelSettings(), func(context.Context) (sourceTestExecution, error) {
			t.Fatal("unsupported provider action reached execution")
			return sourceTestExecution{}, nil
		})
		if err == nil || evidence != nil {
			t.Fatal("tool escape produced test authority", shape)
		}
	}
}

func TestSourceTestModelCannotBlessFailureOrSubstituteEvidence(t *testing.T) {
	for _, mutation := range []string{"failed", "wrong-image", "wrong-suite", "zero-tests", "missing-logs", "model-alias", "duplicate-judgment", "missing-judgment"} {
		plan := sourceTestPlanFixture()
		requests := 0
		client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
			requests++
			stream := sourceTestToolStream(t, "run_prescribed_tests", "{}", false)
			if requests == 2 {
				output := `{"passed":true,"summary":"Synthetic completion."}`
				switch mutation {
				case "model-alias":
					output = `{"Passed":true,"summary":"Synthetic completion."}`
				case "duplicate-judgment":
					output = `{"passed":false,"passed":true,"summary":"Synthetic completion."}`
				case "missing-judgment":
					output = `{"summary":"Synthetic completion."}`
				}
				stream = winnerModelFixtureStreamForModel(t, output, sourceTestModelName)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Header: make(http.Header)}, nil
		})}
		evidence, err := executeSourceTestModel(context.Background(), client, "synthetic-model-key", plan, defaultSourceTestModelSettings(), func(context.Context) (sourceTestExecution, error) {
			execution := sourceTestExecutionFixture(plan)
			switch mutation {
			case "failed":
				execution.ExitCode = 1
			case "wrong-image":
				execution.ImageId = "sha256:" + strings.Repeat("1", 64)
			case "wrong-suite":
				execution.SuiteSha256 = strings.Repeat("1", 64)
			case "zero-tests":
				execution.TestCount = 0
			case "missing-logs":
				execution.TestLogsSha256 = ""
			}
			return execution, nil
		})
		if err == nil || evidence != nil {
			t.Fatal("failed or substituted results produced passing evidence", mutation)
		}
	}
}

func TestSourceTestModelCancellationPreventsCompletionCall(t *testing.T) {
	plan := sourceTestPlanFixture()
	requests := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sourceTestToolStream(t, "run_prescribed_tests", "{}", false))), Header: make(http.Header)}, nil
	})}
	evidence, err := executeSourceTestModel(ctx, client, "synthetic-model-key", plan, defaultSourceTestModelSettings(), func(context.Context) (sourceTestExecution, error) {
		cancel()
		return sourceTestExecutionFixture(plan), nil
	})
	if !errors.Is(err, context.Canceled) || evidence != nil || requests != 1 {
		t.Fatal("canceled test acquired another provider call or receipt", evidence, err, requests)
	}
}

func TestSourceTestModelRetainsObservedFailedExecution(t *testing.T) {
	plan := sourceTestPlanFixture()
	requests := 0
	client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
		requests++
		stream := sourceTestToolStream(t, "run_prescribed_tests", "{}", false)
		if requests == 2 {
			stream = winnerModelFixtureStreamForModel(t, `{"passed":false,"summary":"Observed prescribed tests failed."}`, sourceTestModelName)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Header: make(http.Header)}, nil
	})}
	evidence, err := executeSourceTestModel(context.Background(), client, "synthetic-model-key", plan, defaultSourceTestModelSettings(), func(context.Context) (sourceTestExecution, error) {
		execution := sourceTestExecutionFixture(plan)
		execution.ExitCode = 1
		return execution, nil
	})
	if err != nil || evidence == nil || evidence.Passed || evidence.Execution.ExitCode != 1 || requests != 2 {
		t.Fatal("observed failure was lost or relabeled", evidence, err)
	}
}

func TestSourceTestModelDoesNotFollowRedirectsOrStartCanceledWork(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		requests := 0
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
			requests++
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://redirect.example/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		evidence, err := executeSourceTestModel(ctx, client, "synthetic-model-key", sourceTestPlanFixture(), defaultSourceTestModelSettings(), func(context.Context) (sourceTestExecution, error) {
			t.Fatal("redirect or canceled request reached test execution")
			return sourceTestExecution{}, nil
		})
		cancel()
		want := 1
		if canceled {
			want = 0
		}
		if err == nil || evidence != nil || requests != want {
			t.Fatal("redirect/canceled request created unauthorized work", canceled, requests, err)
		}
	}
}
