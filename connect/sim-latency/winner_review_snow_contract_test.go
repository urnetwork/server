// Synthetic schema and intercepted-transport checks exercise Snow's actual
// output contract without calling a model or supplying qualification answers.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/urnetwork/server/model"
)

// Compile the actual sent schema with the already-pinned JSON Schema engine;
// fixtures are decoded to JSON values instead of a second hand-written validator.
func winnerSnowResolvedSchema(t *testing.T, schema map[string]any) *jsonschema.Resolved {
	t.Helper()
	content, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var parsed jsonschema.Schema
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatal(err)
	}
	resolved, err := parsed.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// The previous schema accepted empty reasons, explanations and private evidence.
// Those same synthetic fields must now fail the provider contract, not be filled in.
func TestWinnerSnowSchemaRejectsEmptyRequiredEvidence(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	judgment.ReasonCodes, judgment.PrivateEvidence = []string{}, ""
	for index := range judgment.Findings {
		judgment.Findings[index].ReasonCode, judgment.Findings[index].Explanation, judgment.Findings[index].EvidenceSha256 = "", "", ""
	}
	content, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	var instance any
	if err := json.Unmarshal(content, &instance); err != nil {
		t.Fatal(err)
	}
	if err := winnerSnowResolvedSchema(t, winnerModelSchema()).Validate(instance); err != nil {
		t.Fatal("synthetic regression no longer reproduces the weak schema", err)
	}
	if winnerSnowResolvedSchema(t, winnerSnowModelSchema()).Validate(instance) == nil || validateWinnerSnowMember(judgment, request) == nil {
		t.Fatal("empty schema-valid model evidence became a native judgment")
	}
}

// Every output value has a native-compatible constraint, including exact six
// findings, recognized reasons and the operator-only fields being forbidden.
func TestWinnerSnowSchemaConstrainsCompleteJudgmentShape(t *testing.T) {
	legacyBefore, err := json.Marshal(winnerModelSchema())
	if err != nil {
		t.Fatal(err)
	}
	_, judgment := winnerModelFixture()
	for index := range judgment.Findings {
		judgment.Findings[index].EvidenceSha256 = ""
	}
	content, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	schema := winnerSnowResolvedSchema(t, winnerSnowModelSchema())
	var valid any
	if err := json.Unmarshal(content, &valid); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(valid); err != nil {
		t.Fatal("complete synthetic judgment did not satisfy its output contract", err)
	}
	for _, change := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing decision", mutate: func(v map[string]any) { delete(v, "decision") }},
		{name: "operator identity", mutate: func(v map[string]any) { v["job_id"] = "synthetic-operator-only-id" }},
		{name: "operator disposition", mutate: func(v map[string]any) { v["reward_disposition"] = "approved" }},
		{name: "unknown decision", mutate: func(v map[string]any) { v["decision"] = "pass" }},
		{name: "unknown reason", mutate: func(v map[string]any) { v["reason_codes"] = []string{"SYNTHETIC_UNKNOWN_REASON"} }},
		{name: "too many reasons", mutate: func(v map[string]any) {
			v["reason_codes"] = strings.Fields(strings.Repeat("APPROVED ", defaultWinnerSnowJudgmentSettings().MaximumReasonCodes+1))
		}},
		{name: "empty public explanation", mutate: func(v map[string]any) { v["public_explanation"] = "" }},
		{name: "oversized public explanation", mutate: func(v map[string]any) {
			v["public_explanation"] = strings.Repeat("x", defaultWinnerSnowJudgmentSettings().MaximumPublicExplanationBytes+1)
		}},
		{name: "missing finding", mutate: func(v map[string]any) { v["findings"] = v["findings"].([]any)[:5] }},
		{name: "extra finding", mutate: func(v map[string]any) { v["findings"] = append(v["findings"].([]any), v["findings"].([]any)[0]) }},
		{name: "unknown criterion", mutate: func(v map[string]any) { v["findings"].([]any)[0].(map[string]any)["criterion"] = "synthetic_unknown" }},
		{name: "duplicate criterion", mutate: func(v map[string]any) { v["findings"].([]any)[0].(map[string]any)["criterion"] = "containment" }},
		{name: "decision as disposition", mutate: func(v map[string]any) { v["findings"].([]any)[0].(map[string]any)["disposition"] = "approve" }},
		{name: "empty finding reason", mutate: func(v map[string]any) { v["findings"].([]any)[0].(map[string]any)["reason_code"] = "" }},
		{name: "empty finding explanation", mutate: func(v map[string]any) { v["findings"].([]any)[0].(map[string]any)["explanation"] = "" }},
		{name: "oversized finding explanation", mutate: func(v map[string]any) {
			v["findings"].([]any)[0].(map[string]any)["explanation"] = strings.Repeat("x", defaultWinnerSnowJudgmentSettings().MaximumFindingExplanationBytes+1)
		}},
		{name: "invented evidence hash", mutate: func(v map[string]any) {
			v["findings"].([]any)[0].(map[string]any)["evidence_sha256"] = strings.Repeat("a", 64)
		}},
		{name: "model attribution", mutate: func(v map[string]any) { v["findings"].([]any)[0].(map[string]any)["model"] = "synthetic-reviewer" }},
	} {
		var instance map[string]any
		if err := json.Unmarshal(content, &instance); err != nil {
			t.Fatal(err)
		}
		change.mutate(instance)
		if schema.Validate(instance) == nil {
			t.Errorf("Snow schema accepted %s", change.name)
		}
	}
	legacyAfter, _ := json.Marshal(winnerModelSchema())
	if !bytes.Equal(legacyBefore, legacyAfter) {
		t.Fatal("Snow changed the historical output schema")
	}
}

// Even completed uncertainty must contain genuine nonempty attributed evidence;
// schema bypass or whitespace cannot be normalized into a usable member.
func TestWinnerSnowUncertaintyRequiresCompleteNativeEvidence(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	judgment.Decision, judgment.ReasonCodes = "uncertain", []string{"SOURCE_NOT_READY"}
	content, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWinnerSnowMember(judgment, request); err != nil {
		t.Fatal("complete synthetic uncertainty was not preserved", err)
	}
	for _, mutate := range []func(*winnerModelJudgment){
		func(j *winnerModelJudgment) { j.ReasonCodes = nil },
		func(j *winnerModelJudgment) { j.ReasonCodes = []string{"SYNTHETIC_UNKNOWN_REASON"} },
		func(j *winnerModelJudgment) { j.PrivateEvidence = " \n\t" },
		func(j *winnerModelJudgment) { j.PublicExplanation = " \n\t" },
		func(j *winnerModelJudgment) { j.Findings[0].Explanation = " \n\t" },
		func(j *winnerModelJudgment) { j.Findings[0].ReasonCode = "" },
	} {
		var changed winnerModelJudgment
		if err := json.Unmarshal(content, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		if validateWinnerSnowMember(changed, request) == nil {
			t.Fatal("incomplete completed uncertainty passed native member validation")
		}
	}
}

// The real serial producer sends the same schema through text.format and
// explicit instructions, while JSON and SSE frontends retain exact authority.
func TestWinnerSnowTransportSendsExplicitJudgmentContract(t *testing.T) {
	request, judgment := winnerModelFixture()
	request.Request.Policy.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
	request.Request.ReviewerModel, request.Request.ReviewerReasoning = model.CompetitionSnowWinnerReviewModel, model.CompetitionSnowWinnerReviewReasoning
	request.Template.ReviewerModel, request.Template.ReviewerReasoning = request.Request.ReviewerModel, request.Request.ReviewerReasoning
	for index := range judgment.Findings {
		judgment.Findings[index].EvidenceSha256 = ""
	}
	output, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	provider, calls := defaultWinnerSnowProviderSettings(), 0
	schemaBytes, err := json.Marshal(winnerSnowModelSchema())
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: winnerReviewTransport(func(message *http.Request) (*http.Response, error) {
		index := calls
		calls++
		if index >= len(provider.Members) || message.Header.Get("Authorization") != "" {
			t.Fatal("native Snow producer retried or gained credentials")
		}
		var payload struct {
			Model, Instructions, Input string
			Reasoning                  struct{ Effort string }
			Text                       struct {
				Format struct {
					Type, Name string
					Strict     bool
					Schema     json.RawMessage
				}
			}
			Tools               []any
			ToolChoice          string `json:"tool_choice"`
			MaximumOutputTokens int    `json:"max_output_tokens"`
		}
		if err := json.NewDecoder(message.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != provider.Members[index].Model || payload.Reasoning.Effort != provider.Members[index].Reasoning || payload.MaximumOutputTokens != 8192 || len(payload.Tools) != 0 || payload.ToolChoice != "none" || payload.Text.Format.Type != "json_schema" || !payload.Text.Format.Strict || !bytes.Equal(payload.Text.Format.Schema, schemaBytes) || !strings.HasSuffix(payload.Instructions, string(schemaBytes)) {
			t.Fatal("native output contract lost its explicit schema, effort or token bound")
		}
		for _, required := range []string{"context.template is INPUT-ONLY", "ONLY decision, reason_codes, public_explanation, private_evidence and findings", "not a request to run or certify performance tests yourself", "Missing context", "NEVER approve/disqualify"} {
			if !strings.Contains(payload.Instructions, required) {
				t.Fatal("native output instructions omitted contract semantics", required)
			}
		}
		for _, caseName := range []string{"benign-comment-only", "measurement-fabrication", "benchmark-special-case", "prompt-injected-comment", "insufficient-transitive-context"} {
			if strings.Contains(payload.Instructions, caseName) {
				t.Fatal("provider instructions gained qualification-specific answer cues")
			}
		}
		var original any
		if err := json.Unmarshal(output, &original); err != nil {
			t.Fatal(err)
		}
		if err := winnerSnowResolvedSchema(t, winnerSnowModelSchema()).Validate(original); err != nil {
			t.Fatal("synthetic frontend response violated the actual sent schema", err)
		}
		response := map[string]any{"id": fmt.Sprintf("synthetic-contract-%d", index), "status": "completed", "model": payload.Model,
			"usage":  map[string]int{"input_tokens": 10, "output_tokens": 10},
			"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": string(output)}}}}}
		body, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		header := make(http.Header)
		header.Set("Content-Type", "Application/JSON; charset=utf-8")
		if index == 1 {
			header.Set("Content-Type", "text/event-stream")
			event, err := json.Marshal(map[string]any{"type": "response.completed", "response": response})
			if err != nil {
				t.Fatal(err)
			}
			body = []byte("event: response.completed\r\ndata: " + string(event) + "\r\n\r\ndata: [DONE]\r\n\r\n")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	result, err := executeWinnerSnow(t.Context(), client, request, defaultWinnerSnowModelSettings(), provider, strings.Repeat("a", 64), state)
	if err != nil || calls != 3 {
		t.Fatal("native composed Snow producer did not complete exactly three intercepted calls", err, calls)
	}
	if err := validateWinnerReviewBrokerResult(result, request.Template, request.Request.Policy, request); err != nil {
		t.Fatal("explicit-contract result lost native union validation", err)
	}
	properties := winnerSnowModelSchema()["properties"].(map[string]any)
	reasons := properties["reason_codes"].(map[string]any)["items"].(map[string]any)["enum"]
	if !reflect.DeepEqual(reasons, model.CompetitionIntegrityReviewReasonCodes()) {
		t.Fatal("provider enum diverged from native reason vocabulary")
	}
}
