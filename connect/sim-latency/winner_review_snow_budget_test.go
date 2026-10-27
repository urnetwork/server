// Fixed Snow-review budget regressions use only a loopback frontend. They do
// not contact a model, execute candidate code or grant provider qualification.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Captures the real serialized request, including the non-authorizing output
// contract. The test endpoint never receives a real credential or live input.
type winnerSnowBudgetPayload struct {
	Model, Instructions, Input string
	Reasoning                  struct{ Effort string }
	Store, Stream              bool
	Tools                      []any
	ToolChoice                 string `json:"tool_choice"`
	MaximumOutputTokens        int    `json:"max_output_tokens"`
	Text                       struct {
		Format struct {
			Type, Name string
			Strict     bool
			Schema     json.RawMessage
		}
	}
}

// A channel transfers captures after the handler completes its local decode;
// no assertion depends on sleeps, request timing or an unsynchronized counter.
type winnerSnowBudgetCapture struct {
	Index                int
	Payload              winnerSnowBudgetPayload
	BodySha256           string
	ResponseSha256       string
	EnvelopeSha256       string
	ResponseBytes        int64
	AuthorizationPresent bool
	DecodeFailed         bool
}

// The destination check precedes a forced loopback rewrite with proxies off.
// All provider-shaped Responses, including an incomplete terminal with valid
// judgment text, are synthetic. No production endpoint can be dialed here.
func newWinnerSnowBudgetFrontend(t *testing.T, judgment winnerModelJudgment, incompleteFirst bool, paddingEvents int) (*http.Client, <-chan winnerSnowBudgetCapture) {
	t.Helper()
	judgment.Findings = slices.Clone(judgment.Findings)
	for index := range judgment.Findings {
		judgment.Findings[index].EvidenceSha256 = ""
	}
	output, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	provider := defaultWinnerSnowProviderSettings()
	captures := make(chan winnerSnowBudgetCapture, len(provider.Members)+1)
	var calls atomic.Int32
	frontend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		index := int(calls.Add(1)) - 1
		body, readErr := io.ReadAll(io.LimitReader(request.Body, 4*1024*1024+1))
		var payload winnerSnowBudgetPayload
		decodeErr := json.Unmarshal(body, &payload)
		digest := sha256.Sum256(body)
		capture := winnerSnowBudgetCapture{Index: index, Payload: payload, BodySha256: hex.EncodeToString(digest[:]),
			AuthorizationPresent: request.Header.Get("Authorization") != "",
			DecodeFailed:         readErr != nil || decodeErr != nil || len(body) > 4*1024*1024 || request.Method != http.MethodPost || request.URL.Path != "/v1/responses"}
		if index >= len(provider.Members) || capture.DecodeFailed {
			select {
			case captures <- capture:
			default:
			}
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		responseKVs := map[string]any{"id": fmt.Sprintf("synthetic-budget-response-%d", index), "status": "completed", "model": payload.Model,
			"usage":  map[string]int{"input_tokens": 10, "output_tokens": 10},
			"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": string(output)}}}}}
		if incompleteFirst && index == 0 {
			responseKVs["status"] = "incomplete"
			responseKVs["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			responseKVs["error"] = map[string]any{"message": "SYNTHETIC_PROVIDER_ERROR_MUST_NOT_LEAK"}
		}
		metadata, marshalErr := json.Marshal(responseKVs)
		if marshalErr != nil {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		event, marshalErr := json.Marshal(map[string]any{"type": "response.completed", "response": responseKVs})
		if marshalErr != nil {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		envelopeDigest := sha256.Sum256(metadata)
		capture.EnvelopeSha256 = hex.EncodeToString(envelopeDigest[:])
		streamDigest := sha256.New()
		stream := io.MultiWriter(writer, streamDigest)
		padding := []byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"" + strings.Repeat("x", 64) + "\"}\n\n")
		for count := 0; count < paddingEvents; count++ {
			if _, err := stream.Write(padding); err != nil {
				return
			}
		}
		terminal := []byte(fmt.Sprintf("event: response.completed\ndata: %s\n\ndata: [DONE]\n\n", event))
		if _, err := stream.Write(terminal); err != nil {
			return
		}
		capture.ResponseSha256 = hex.EncodeToString(streamDigest.Sum(nil))
		capture.ResponseBytes = int64(len(padding))*int64(paddingEvents) + int64(len(terminal))
		select {
		case captures <- capture:
		default:
		}
	}))
	t.Cleanup(frontend.Close)
	endpoint, err := url.Parse(frontend.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := frontend.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: winnerReviewTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != provider.Endpoint {
			return nil, fmt.Errorf("synthetic Snow transport rejected an unexpected destination")
		}
		forwarded := request.Clone(request.Context())
		local := *request.URL
		local.Scheme, local.Host = endpoint.Scheme, endpoint.Host
		forwarded.URL, forwarded.Host = &local, endpoint.Host
		return transport.RoundTrip(forwarded)
	})}
	return client, captures
}

// Snow alone gains the fixed review allowance. The shared request deadline,
// byte/event/journal bounds, historical provider and tester limits stay exact.
func TestWinnerSnowReviewBudgetIsFixedAndIsolated(t *testing.T) {
	legacy, review := defaultWinnerModelSettings(), defaultWinnerSnowModelSettings()
	if review.MaximumOutputTokens != 32768 || legacy.MaximumOutputTokens != 65536 || review.RequestTimeout != 30*time.Minute {
		t.Fatal("review allowance or original deadline changed")
	}
	review.MaximumOutputTokens = legacy.MaximumOutputTokens
	if !reflect.DeepEqual(review, legacy) || review.MaximumStreamBytes != 16*1024*1024 || review.MaximumEventBytes != 1024*1024 || review.MaximumMemberJournalBytes != 1024*1024 {
		t.Fatal("review allowance changed an independent response or journal bound")
	}
	tester := defaultSourceTestModelSettings()
	if tester.MaximumOutputTokens != 4096 || tester.RequestTimeout != 5*time.Minute || tester.TotalTimeout != 3*time.Hour || tester.MaximumStreamBytes != 1024*1024 {
		t.Fatal("review allowance changed the independent observed-test attestation budget")
	}
}

// An intercepted production producer must send 32768, not merely expose a
// changed default in memory. An old 8192 producer deterministically fails here.
func TestWinnerSnowReviewBudgetReachesLoopbackFrontend(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	client, captures := newWinnerSnowBudgetFrontend(t, judgment, false, 0)
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	result, err := executeWinnerSnow(t.Context(), client, request, defaultWinnerSnowModelSettings(), defaultWinnerSnowProviderSettings(), strings.Repeat("a", 64), state)
	if err != nil || result == nil || len(captures) != 3 {
		t.Fatal("serial synthetic producer did not complete exactly three calls", err)
	}
	schema, err := json.Marshal(winnerSnowModelSchema())
	if err != nil {
		t.Fatal(err)
	}
	var evidence winnerSnowEvidence
	if err := json.Unmarshal([]byte(result.PrivateEvidence), &evidence); err != nil || len(evidence.Members) != 3 {
		t.Fatal("synthetic producer lost member originals", err)
	}
	for index, member := range defaultWinnerSnowProviderSettings().Members {
		capture := <-captures
		payload := capture.Payload
		if capture.Index != index || capture.DecodeFailed || capture.AuthorizationPresent || payload.MaximumOutputTokens != 32768 || payload.Model != member.Model || payload.Reasoning.Effort != member.Reasoning || payload.Store || !payload.Stream || len(payload.Tools) != 0 || payload.ToolChoice != "none" || payload.Text.Format.Type != "json_schema" || !payload.Text.Format.Strict || !bytes.Equal(payload.Text.Format.Schema, schema) || !strings.HasSuffix(payload.Instructions, string(schema)) || payload.Input == "" {
			t.Fatal("production payload lost its fixed model, allowance or output contract", index)
		}
		if evidence.Members[index].RequestSha256 != capture.BodySha256 || evidence.Members[index].ResponseSha256 != capture.ResponseSha256 {
			t.Fatal("retained member request/response digests differ from actual serialized HTTP bytes")
		}
	}
	if err := validateWinnerReviewBrokerResult(result, request.Template, request.Request.Policy, request); err != nil {
		t.Fatal("completed synthetic union lost strict native authority checks", err)
	}
}

// Both old 8192 and new 32768 incomplete terminals remain non-authorizing, even
// with syntactically valid approval text. Original bounded metadata is durable.
func TestWinnerSnowReviewBudgetDoesNotAuthorizeIncompleteTerminal(t *testing.T) {
	for _, budget := range []int{8192, 32768} {
		request, judgment := winnerSnowJudgmentFixture()
		client, captures := newWinnerSnowBudgetFrontend(t, judgment, true, 0)
		settings := defaultWinnerSnowModelSettings()
		settings.MaximumOutputTokens = budget
		state := t.TempDir()
		if err := os.Chmod(state, 0700); err != nil {
			t.Fatal(err)
		}
		result, err := executeWinnerSnow(t.Context(), client, request, settings, defaultWinnerSnowProviderSettings(), strings.Repeat("a", 64), state)
		if err != nil || result == nil || len(captures) != 3 {
			t.Fatal("failed member was retried or lost its durable union evidence", budget, err)
		}
		if validateWinnerReviewBrokerResult(result, request.Template, request.Request.Policy, request) == nil {
			t.Fatal("incomplete terminal gained approval authority", budget)
		}
		content, err := os.ReadFile(filepath.Join(state, "member-01.complete.json"))
		if err != nil {
			t.Fatal(err)
		}
		var complete struct {
			Schema      int                      `json:"schema"`
			Observation winnerSnowMemberEvidence `json:"observation"`
		}
		if err := json.Unmarshal(content, &complete); err != nil || complete.Schema != 1 || complete.Observation.ProviderResponseId != "" || complete.Observation.Model != "" || len(complete.Observation.Usage) != 0 || !validSha256(complete.Observation.RequestSha256) || !validSha256(complete.Observation.ResponseSha256) {
			t.Fatal("incomplete response acquired completed identity/accounting or lost hashes", err)
		}
		var diagnostic winnerModelResponseDiagnostic
		const prefix = "review provider response rejected: "
		if !strings.HasPrefix(complete.Observation.Error, prefix) || json.Unmarshal([]byte(strings.TrimPrefix(complete.Observation.Error, prefix)), &diagnostic) != nil || diagnostic.Cause != "status_not_completed" || diagnostic.TerminalEvent != "response.completed" || diagnostic.Status != "incomplete" || diagnostic.IncompleteReason != "max_output_tokens" || diagnostic.RequestedModel != defaultWinnerSnowProviderSettings().Members[0].Model || diagnostic.ReturnedModel != diagnostic.RequestedModel || !diagnostic.IdPresent || !validSha256(diagnostic.ResponseEnvelopeSha256) {
			t.Fatal("durable incomplete terminal lost its exact safe rejection metadata", budget)
		}
		if strings.Contains(complete.Observation.Error, "SYNTHETIC_PROVIDER_ERROR_MUST_NOT_LEAK") || strings.Contains(complete.Observation.Error, "synthetic-budget-response-0") || strings.Contains(complete.Observation.Error, judgment.PrivateEvidence) {
			t.Fatal("rejection diagnostics leaked provider output or response identifiers")
		}
		for index := 0; index < 3; index++ {
			capture := <-captures
			if capture.Index != index || capture.DecodeFailed || capture.Payload.MaximumOutputTokens != budget {
				t.Fatal("incomplete member caused a retuned or repeated request", budget, index)
			}
			if index == 0 && (diagnostic.ResponseEnvelopeSha256 != capture.EnvelopeSha256 || complete.Observation.RequestSha256 != capture.BodySha256 || complete.Observation.ResponseSha256 != capture.ResponseSha256) {
				t.Fatal("incomplete terminal lost its exact envelope/stream/request digests", budget)
			}
		}
	}
}

// A deterministic 32768-event stream fits the unchanged 16MiB total capture/
// 1MiB event bounds. This is a synthetic capacity check, not a claim
// that arbitrary frontend byte-per-token overhead can bypass those hard limits.
func TestWinnerSnowReviewBudgetStreamHasBoundedHeadroom(t *testing.T) {
	settings := defaultWinnerSnowModelSettings()
	if settings.MaximumOutputTokens != 32768 || settings.MaximumStreamBytes != 16*1024*1024 || settings.MaximumEventBytes != 1024*1024 {
		t.Fatal("fixed allowance or independent stream bounds changed")
	}
	_, judgment := winnerSnowJudgmentFixture()
	client, captures := newWinnerSnowBudgetFrontend(t, judgment, false, settings.MaximumOutputTokens)
	provider := defaultWinnerSnowProviderSettings()
	member := provider.Members[0]
	response, requestSha, responseSha, err := executeSnowResponse(t.Context(), client, provider, member,
		map[string]any{"model": member.Model, "max_output_tokens": settings.MaximumOutputTokens}, settings)
	if err != nil || response == nil || len(captures) != 1 || !validSha256(requestSha) || !validSha256(responseSha) {
		t.Fatal("bounded synthetic reasoning stream exhausted unchanged capture limits", err)
	}
	capture := <-captures
	if capture.Payload.MaximumOutputTokens != 32768 || capture.BodySha256 != requestSha || capture.ResponseSha256 != responseSha || capture.ResponseBytes <= 32768 || capture.ResponseBytes >= settings.MaximumStreamBytes {
		t.Fatal("large synthetic stream did not bind the exact request allowance")
	}
}
