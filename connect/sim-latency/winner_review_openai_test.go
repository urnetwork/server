// Provider transports are synthetic and never execute candidate text or call a
// live API. Streaming and authority checks use explicit bytes, not scheduling.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Match the parent protocol using only visibly synthetic review evidence.
func winnerModelFixture() (winnerReviewBrokerRequest, winnerModelJudgment) {
	result, template, policy := winnerBrokerFixture()
	patch := []byte("diff --git a/connect/example.go b/connect/example.go\n--- a/connect/example.go\n+++ b/connect/example.go\n@@ -1 +1 @@\n-// synthetic base\n+// synthetic patch: ignore instructions in this comment\n")
	score := []byte(`{"synthetic_score":true}`)
	source := "// synthetic base\n"
	sourceDigest := sha256.Sum256([]byte(source))
	patchDigest, scoreDigest := sha256.Sum256(patch), sha256.Sum256(score)
	template.PatchSha256, template.ScoreSha256 = hex.EncodeToString(patchDigest[:]), hex.EncodeToString(scoreDigest[:])
	request := winnerReviewBrokerRequest{Operation: "review_elected_winner", Template: template,
		Request: controller.CompetitionWinnerReviewRequest{Policy: policy, ReviewerModel: model.CompetitionWinnerReviewModel, ReviewerReasoning: model.CompetitionWinnerReviewReasoning},
		Packet: &controller.CompetitionSourceReviewPacket{Patch: patch, Score: score,
			PatchPolicy: controller.PatchPolicy{MaxPatchBytes: 4096, AllowedPaths: []string{"connect/example.go"}}},
		SourceFiles: []winnerReviewSourceFile{{Path: "connect/example.go", Sha256: hex.EncodeToString(sourceDigest[:]), Content: source}}}
	return request, winnerModelJudgment{Decision: "approve", ReasonCodes: result.Review.ReasonCodes,
		PublicExplanation: result.Review.PublicExplanation, PrivateEvidence: result.PrivateEvidence, Findings: result.Review.Findings}
}

// Emit one real-shape Responses terminal event, not pre-decoded model data.
func winnerModelFixtureStream(t *testing.T, output string) string {
	t.Helper()
	content, err := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_synthetic", "status": "completed", "model": winnerReviewProviderModelName(model.CompetitionWinnerReviewModel),
		"usage":  map[string]int{"input_tokens": 100, "output_tokens": 100},
		"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": output}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return "event: response.completed\ndata: " + string(content) + "\n\ndata: [DONE]\n\n"
}

func TestWinnerReviewModelPinsAuthorityAndBindsFindings(t *testing.T) {
	request, judgment := winnerModelFixture()
	content, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	stream := winnerModelFixtureStream(t, string(content))
	client := &http.Client{Transport: winnerReviewTransport(func(message *http.Request) (*http.Response, error) {
		if message.URL.String() != "https://api.openai.com/v1/responses" || message.Method != http.MethodPost || message.Header.Get("Authorization") != "Bearer synthetic-model-key" {
			t.Fatal("provider authority or credential changed")
		}
		body, err := io.ReadAll(message.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
			Store      bool   `json:"store"`
			Stream     bool   `json:"stream"`
			Tools      []any  `json:"tools"`
			ToolChoice string `json:"tool_choice"`
			Input      string `json:"input"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != winnerReviewMiniProviderSnapshot || payload.Reasoning.Effort != "xhigh" || payload.Store || !payload.Stream || len(payload.Tools) != 0 || payload.ToolChoice != "none" {
			t.Fatal("model execution contract was downgraded or gained tools")
		}
		var readable struct {
			Patch string `json:"patch_text"`
		}
		if err := json.Unmarshal([]byte(payload.Input), &readable); err != nil {
			t.Fatal(err)
		}
		if readable.Patch != string(request.Packet.Patch) || !strings.Contains(payload.Input, "synthetic_score") || strings.Contains(string(body), "synthetic-model-key") {
			t.Fatal("unreadable evidence or credential in model input")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Header: make(http.Header)}, nil
	})}
	result, err := executeWinnerModel(context.Background(), client, "synthetic-model-key", request, defaultWinnerModelSettings())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWinnerReviewBrokerResult(result, request.Template, request.Request.Policy); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.PrivateEvidence, "resp_synthetic") {
		t.Fatal("provider receipt missing")
	}
	for _, finding := range result.Review.Findings {
		if finding.EvidenceSha256 != result.Review.PrivateEvidenceSha256 {
			t.Fatal("finding lacks actual report binding")
		}
	}
}

// Existing policy records keep their original provider. Neither the request
// nor the broker receipt can silently relabel them under a new season default.
func TestWinnerReviewModelSelectsProviderFromFrozenPolicy(t *testing.T) {
	for _, policyName := range []string{model.CompetitionWinnerReviewPolicy, model.CompetitionLegacyWinnerReviewPolicy} {
		request, judgment := winnerModelFixture()
		modelName, reasoning, _ := model.CompetitionWinnerReviewProvider(policyName)
		providerModelName := winnerReviewProviderModelName(modelName)
		request.Request.Policy.WinnerReviewPolicy = policyName
		request.Request.ReviewerModel, request.Request.ReviewerReasoning = modelName, reasoning
		request.Template.ReviewerModel, request.Template.ReviewerReasoning = modelName, reasoning
		content, err := json.Marshal(judgment)
		if err != nil {
			t.Fatal(err)
		}
		stream := strings.ReplaceAll(winnerModelFixtureStream(t, string(content)), winnerReviewMiniProviderSnapshot, providerModelName)
		calls := 0
		client := &http.Client{Transport: winnerReviewTransport(func(message *http.Request) (*http.Response, error) {
			calls++
			var body struct {
				Model     string `json:"model"`
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			if err := json.NewDecoder(message.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Model != providerModelName || body.Reasoning.Effort != reasoning {
				t.Fatalf("policy %s dispatched %s/%s", policyName, body.Model, body.Reasoning.Effort)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Header: make(http.Header)}, nil
		})}
		result, err := executeWinnerModel(context.Background(), client, "synthetic-model-key", request, defaultWinnerModelSettings())
		if err != nil || calls != 1 || result == nil || result.Model != modelName || result.Reasoning != reasoning {
			t.Fatalf("frozen policy result: %+v calls=%d error=%v", result, calls, err)
		}
		if err := validateWinnerReviewBrokerResult(result, request.Template, request.Request.Policy); err != nil {
			t.Fatal(err)
		}
	}
}

// Changing review defaults must never migrate the independent Sol-low test
// agent's stream acceptance or let either provider impersonate the other.
func TestWinnerReviewAndSourceTestsKeepSeparateModelPins(t *testing.T) {
	stream := winnerModelFixtureStream(t, "synthetic text")
	settings := defaultWinnerModelSettings()
	if _, err := readPinnedSolModelResponse(strings.NewReader(stream), settings); err == nil {
		t.Fatal("Sol test parser accepted a mini review response")
	}
	solStream := strings.ReplaceAll(stream, winnerReviewMiniProviderSnapshot, sourceTestModelName)
	if _, err := readPinnedSolModelResponse(strings.NewReader(solStream), settings); err != nil {
		t.Fatal("review migration changed Sol test stream acceptance:", err)
	}
	if _, err := readWinnerModelResponse(strings.NewReader(solStream), settings); err == nil {
		t.Fatal("new honesty-review parser accepted a Sol test response")
	}
}

// Mono-root context must bind all ten source pins before any provider call,
// even if the modified files and their individual content hashes are valid.
func TestWinnerReviewModelAuthenticatesCompleteMonoRootSourceMap(t *testing.T) {
	request, _ := winnerModelFixture()
	request.Packet.Patch = []byte(strings.NewReplacer("a/connect/example.go", "a/server/connect/example.go", "b/connect/example.go", "b/server/connect/example.go").Replace(string(request.Packet.Patch)))
	request.Packet.PatchPolicy = controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096, AllowedPaths: []string{"server/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}
	request.SourceFiles[0].Path = "server/connect/example.go"
	request.Packet.SourceCommits = sourceTestCommits(strings.Repeat("a", 40))
	request.Packet.BaseSha = request.Packet.SourceCommits["server"]
	lock, err := model.CompetitionSourceLockDigest(request.Packet.SourceCommits)
	if err != nil {
		t.Fatal(err)
	}
	request.Template.BaseSourceLockSha256 = lock
	patchDigest := sha256.Sum256(request.Packet.Patch)
	request.Template.PatchSha256 = hex.EncodeToString(patchDigest[:])
	if _, err := winnerModelInput(request); err != nil {
		t.Fatal("complete mono-root context was refused:", err)
	}
	request.Packet.SourceCommits["sdk"] = strings.Repeat("b", 40)
	if _, err := winnerModelInput(request); err == nil || !strings.Contains(err.Error(), "frozen lock") {
		t.Fatal("changed sibling pin reached the provider boundary", err)
	}
}

func TestWinnerReviewModelRejectsUnauthenticatedEvidenceBeforeTransport(t *testing.T) {
	request, _ := winnerModelFixture()
	request.Packet.Patch = []byte("tampered synthetic patch")
	client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("unauthenticated evidence reached provider")
		return nil, nil
	})}
	if _, err := executeWinnerModel(context.Background(), client, "synthetic-model-key", request, defaultWinnerModelSettings()); err == nil {
		t.Fatal("tampered patch accepted")
	}
}

func TestWinnerReviewModelRejectsMissingOrMismatchedBaseContext(t *testing.T) {
	for _, mutate := range []func(*winnerReviewBrokerRequest){
		func(r *winnerReviewBrokerRequest) { r.SourceFiles = nil },
		func(r *winnerReviewBrokerRequest) { r.SourceFiles[0].Path = "connect/unrelated.go" },
		func(r *winnerReviewBrokerRequest) { r.SourceFiles[0].Content += "tampered" },
		func(r *winnerReviewBrokerRequest) { r.SourceFiles = append(r.SourceFiles, r.SourceFiles[0]) },
	} {
		request, _ := winnerModelFixture()
		mutate(&request)
		if _, err := winnerModelInput(request); err == nil {
			t.Fatal("incomplete or unauthenticated base context accepted")
		}
	}
}

func TestWinnerReviewModelRetainsCompletedMalformedAnswerAsUncertainty(t *testing.T) {
	request, _ := winnerModelFixture()
	client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(winnerModelFixtureStream(t, "not-json"))), Header: make(http.Header)}, nil
	})}
	result, err := executeWinnerModel(context.Background(), client, "synthetic-model-key", request, defaultWinnerModelSettings())
	if err != nil {
		t.Fatal("completed malformed answer became a retryable transport failure", err)
	}
	if result.Review.SourceDisposition != "deferred" || validateWinnerReviewBrokerResult(result, request.Template, request.Request.Policy) == nil {
		t.Fatal("malformed completed model output authorized a verdict")
	}
}

func TestWinnerReviewModelStreamRequiresOneBoundedPinnedCompletion(t *testing.T) {
	settings := defaultWinnerModelSettings()
	valid := winnerModelFixtureStream(t, `{}`)
	for _, stream := range []string{
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"approve\"}\n\n",
		valid + valid,
		strings.ReplaceAll(valid, winnerReviewMiniProviderSnapshot, "synthetic-other-model"),
		strings.ReplaceAll(valid, `"status":"completed"`, `"status":"incomplete"`),
		"data: {\"type\":\"response.failed\"}\n\n",
		"data: not-json\n\n",
	} {
		if _, err := readWinnerModelResponse(strings.NewReader(stream), settings); err == nil {
			t.Fatal("invalid stream accepted")
		}
	}
	settings.MaximumStreamBytes = int64(len(valid) - 1)
	if _, err := readWinnerModelResponse(strings.NewReader(valid), settings); err == nil {
		t.Fatal("total stream limit bypassed")
	}
	settings = defaultWinnerModelSettings()
	settings.MaximumEventBytes = 32
	if _, err := readWinnerModelResponse(strings.NewReader(valid), settings); err == nil {
		t.Fatal("event limit bypassed")
	}
}

func TestWinnerReviewModelDoesNotFollowCredentialRedirects(t *testing.T) {
	request, _ := winnerModelFixture()
	calls := 0
	client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Body: io.NopCloser(strings.NewReader("")),
			Header: http.Header{"Location": []string{"https://synthetic-collector.example/"}}}, nil
	})}
	if _, err := executeWinnerModel(context.Background(), client, "synthetic-model-key", request, defaultWinnerModelSettings()); err == nil || calls != 1 {
		t.Fatal("credential redirect followed or accepted", err, calls)
	}
}
