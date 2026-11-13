// Rejected JSON/SSE responses retain bounded identities and failure causes,
// never provider text or hidden reasoning. All HTTP transports are synthetic.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

// Responses from an intercepted frontend use only synthetic accounting and
// opaque text markers that diagnostics must never copy into retained errors.
func winnerModelDiagnosticResponse(modelName string) map[string]any {
	return map[string]any{"id": "synthetic-private-response-id", "status": "completed", "model": modelName,
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 10},
		"output": []any{map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "SYNTHETIC_HIDDEN_REASONING_MUST_NOT_LEAK"}}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "SYNTHETIC_OUTPUT_MUST_NOT_LEAK"}}}},
		"error": map[string]any{"message": "SYNTHETIC_PROVIDER_ERROR_MUST_NOT_LEAK"}}
}

// The same real Responses transport must preserve terminal cause and identity
// for JSON and SSE, including vLLM's completed event with incomplete status.
func TestWinnerSnowResponseDiagnosticsRetainExactTerminalIdentity(t *testing.T) {
	provider := defaultWinnerSnowProviderSettings()
	member := provider.Members[0]
	for _, test := range []struct {
		name, event, cause, status, reason, returned string
		stream, idPresent                            bool
		mutate                                       func(map[string]any)
	}{
		{name: "json token limit", cause: "status_not_completed", status: "incomplete", reason: "max_output_tokens", returned: member.Model, idPresent: true, mutate: func(v map[string]any) {
			v["status"], v["incomplete_details"] = "incomplete", map[string]any{"reason": "max_output_tokens"}
		}},
		{name: "vllm terminal incomplete", stream: true, event: "response.completed", cause: "status_not_completed", status: "incomplete", reason: "max_output_tokens", returned: member.Model, idPresent: true, mutate: func(v map[string]any) {
			v["status"], v["incomplete_details"] = "incomplete", map[string]any{"reason": "max_output_tokens"}
		}},
		{name: "explicit incomplete event", stream: true, event: "response.incomplete", cause: "provider_terminal_failure", status: "incomplete", reason: "content_filter", returned: member.Model, idPresent: true, mutate: func(v map[string]any) {
			v["status"], v["incomplete_details"] = "incomplete", map[string]any{"reason": "content_filter"}
		}},
		{name: "failed event", stream: true, event: "response.failed", cause: "provider_terminal_failure", status: "failed", returned: member.Model, idPresent: true, mutate: func(v map[string]any) { v["status"] = "failed" }},
		{name: "error event", stream: true, event: "error", cause: "provider_terminal_failure", status: "failed", returned: member.Model, idPresent: true, mutate: func(v map[string]any) { v["status"] = "failed" }},
		{name: "json alias", cause: "model_mismatch", status: "completed", returned: "synthetic-unqualified-alias", idPresent: true, mutate: func(v map[string]any) { v["model"] = "synthetic-unqualified-alias" }},
		{name: "sse alias", stream: true, event: "response.completed", cause: "model_mismatch", status: "completed", returned: "synthetic-unqualified-alias", idPresent: true, mutate: func(v map[string]any) { v["model"] = "synthetic-unqualified-alias" }},
		{name: "json missing id", cause: "missing_response_id", status: "completed", returned: member.Model, mutate: func(v map[string]any) { delete(v, "id") }},
		{name: "sse missing id", stream: true, event: "response.completed", cause: "missing_response_id", status: "completed", returned: member.Model, mutate: func(v map[string]any) { delete(v, "id") }},
		{name: "missing terminal", stream: true, event: "response.in_progress", cause: "missing_terminal_response", status: "in_progress", returned: member.Model, idPresent: true, mutate: func(v map[string]any) { v["status"] = "in_progress" }},
	} {
		response := winnerModelDiagnosticResponse(member.Model)
		test.mutate(response)
		metadata, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		body, contentType := metadata, "application/json; charset=utf-8"
		terminalEvent := "json_response"
		if test.stream {
			contentType = "text/event-stream"
			event, err := json.Marshal(map[string]any{"type": test.event, "response": response})
			if err != nil {
				t.Fatal(err)
			}
			body = []byte("event: " + test.event + "\ndata: " + string(event) + "\n\ndata: [DONE]\n\n")
			terminalEvent = test.event
			if test.cause == "missing_terminal_response" {
				terminalEvent = ""
			}
		}
		calls := 0
		client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
			calls++
			header := make(http.Header)
			header.Set("Content-Type", contentType)
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body))}, nil
		})}
		result, requestSha, responseSha, err := executeSnowResponse(t.Context(), client, provider, member, map[string]any{"model": member.Model}, defaultWinnerSnowModelSettings())
		var diagnostic *winnerModelResponseDiagnostic
		if result != nil || calls != 1 || !validSha256(requestSha) || !validSha256(responseSha) || !errors.As(err, &diagnostic) {
			t.Fatalf("%s: rejected response gained authority, retried or lost bounded provenance: %v", test.name, err)
		}
		metadataDigest, streamDigest := sha256.Sum256(metadata), sha256.Sum256(body)
		if diagnostic.Cause != test.cause || diagnostic.TerminalEvent != terminalEvent || diagnostic.Status != test.status || diagnostic.IncompleteReason != test.reason || diagnostic.RequestedModel != member.Model || diagnostic.ReturnedModel != test.returned || diagnostic.IdPresent != test.idPresent || diagnostic.ResponseEnvelopeSha256 != hex.EncodeToString(metadataDigest[:]) || responseSha != hex.EncodeToString(streamDigest[:]) {
			t.Fatalf("%s: terminal diagnostics differ: %+v", test.name, diagnostic)
		}
		for _, forbidden := range []string{"SYNTHETIC_HIDDEN_REASONING_MUST_NOT_LEAK", "SYNTHETIC_OUTPUT_MUST_NOT_LEAK", "SYNTHETIC_PROVIDER_ERROR_MUST_NOT_LEAK", "synthetic-private-response-id"} {
			if strings.Contains(err.Error(), forbidden) {
				t.Fatalf("%s: diagnostics copied forbidden provider content", test.name)
			}
		}
	}
}

// Framing and bounded-read failures remain distinct from a model/status error,
// with the latest safe identity retained and the raw read error excluded.
func TestWinnerModelResponseDiagnosticsDistinguishFramingAndBounds(t *testing.T) {
	modelName := "synthetic-pinned-model"
	metadata, err := json.Marshal(winnerModelDiagnosticResponse(modelName))
	if err != nil {
		t.Fatal(err)
	}
	valid := "data: {\"type\":\"response.completed\",\"response\":" + string(metadata) + "}\n\n"
	for _, test := range []struct {
		name, cause string
		reader      func() io.Reader
		settings    func(*winnerModelSettings)
	}{
		{name: "malformed sse", cause: "malformed_sse_event", reader: func() io.Reader { return strings.NewReader("data: not-json\n\n") }},
		{name: "malformed terminal", cause: "malformed_terminal_response", reader: func() io.Reader {
			return strings.NewReader("data: {\"type\":\"response.completed\",\"response\":42}\n\n")
		}},
		{name: "missing terminal object", cause: "malformed_terminal_response", reader: func() io.Reader { return strings.NewReader("data: {\"type\":\"response.completed\"}\n\n") }},
		{name: "duplicate terminal", cause: "multiple_terminal_responses", reader: func() io.Reader { return strings.NewReader(valid + valid) }},
		{name: "duplicate missing-object terminal", cause: "multiple_terminal_responses", reader: func() io.Reader {
			return strings.NewReader("data: {\"type\":\"response.completed\"}\n\n" + valid)
		}},
		{name: "total limit", cause: "stream_size_limit", reader: func() io.Reader { return strings.NewReader(valid) }, settings: func(s *winnerModelSettings) { s.MaximumStreamBytes = int64(len(valid) - 1) }},
		{name: "event limit", cause: "event_size_limit", reader: func() io.Reader { return strings.NewReader(valid) }, settings: func(s *winnerModelSettings) { s.MaximumEventBytes = 32 }},
		{name: "read failure", cause: "stream_read_failed", reader: func() io.Reader {
			return io.MultiReader(strings.NewReader(valid), iotest.ErrReader(errors.New("SYNTHETIC_RAW_READ_ERROR_MUST_NOT_LEAK")))
		}},
	} {
		settings := defaultWinnerModelSettings()
		if test.settings != nil {
			test.settings(&settings)
		}
		result, err := readPinnedModelResponse(test.reader(), settings, modelName)
		var diagnostic *winnerModelResponseDiagnostic
		if result != nil || !errors.As(err, &diagnostic) || diagnostic.Cause != test.cause || diagnostic.RequestedModel != modelName {
			t.Fatalf("%s: framing/bound cause lost: %v", test.name, err)
		}
		if strings.Contains(err.Error(), "SYNTHETIC_RAW_READ_ERROR_MUST_NOT_LEAK") || strings.Contains(err.Error(), "SYNTHETIC_HIDDEN_REASONING_MUST_NOT_LEAK") {
			t.Fatal("raw provider/read content leaked through framing diagnostics")
		}
	}
}

// Untrusted status/model/reason strings are retained only as short safe atoms;
// ids and oversized or control-character values never become diagnostic text.
func TestWinnerModelResponseDiagnosticsBoundAndSanitizeProtocolMetadata(t *testing.T) {
	settings := defaultWinnerModelSettings()
	settings.MaximumDiagnosticFieldBytes = 24
	content, err := json.Marshal(map[string]any{"id": "SYNTHETIC_ID_MUST_NOT_LEAK", "status": "incomplete\nSYNTHETIC_PRIVATE", "model": strings.Repeat("x", settings.MaximumDiagnosticFieldBytes+1),
		"incomplete_details": map[string]any{"reason": "SYNTHETIC_PRIVATE reason"}, "output": "SYNTHETIC_OUTPUT_MUST_NOT_LEAK"})
	if err != nil {
		t.Fatal(err)
	}
	err = winnerModelResponseFailure("status_not_completed", "response.completed", content, "synthetic-pinned-model", settings)
	var diagnostic *winnerModelResponseDiagnostic
	if !errors.As(err, &diagnostic) || diagnostic.Status != "[oversized]" || diagnostic.ReturnedModel != "[oversized]" || diagnostic.IncompleteReason != "[invalid]" || !diagnostic.IdPresent {
		t.Fatal("unsafe metadata was not fully replaced", err)
	}
	if strings.Contains(err.Error(), "SYNTHETIC_PRIVATE") || strings.Contains(err.Error(), "SYNTHETIC_ID_MUST_NOT_LEAK") || strings.Contains(err.Error(), "SYNTHETIC_OUTPUT_MUST_NOT_LEAK") || len(err.Error()) > 1024 {
		t.Fatal("diagnostic leaked untrusted metadata or exceeded its independent bound")
	}
}

// JSON/SSE metadata is checked before decoding output; malformed output cannot
// turn a valid-looking terminal identity into an accepted response.
func TestWinnerSnowResponseDiagnosticsRejectMalformedOutput(t *testing.T) {
	provider := defaultWinnerSnowProviderSettings()
	member := provider.Members[0]
	response := winnerModelDiagnosticResponse(member.Model)
	response["output"] = "SYNTHETIC_MALFORMED_OUTPUT_MUST_NOT_LEAK"
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		captured, contentType, terminalEvent := body, "application/json", "json_response"
		if stream {
			captured = []byte("data: {\"type\":\"response.completed\",\"response\":" + string(body) + "}\n\n")
			contentType, terminalEvent = "text/event-stream", "response.completed"
		}
		client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
			header := make(http.Header)
			header.Set("Content-Type", contentType)
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(captured))}, nil
		})}
		result, _, _, err := executeSnowResponse(t.Context(), client, provider, member, map[string]any{"model": member.Model}, defaultWinnerSnowModelSettings())
		var diagnostic *winnerModelResponseDiagnostic
		if result != nil || !errors.As(err, &diagnostic) || diagnostic.Cause != "malformed_response_output" || diagnostic.TerminalEvent != terminalEvent || strings.Contains(fmt.Sprint(err), "SYNTHETIC_MALFORMED_OUTPUT_MUST_NOT_LEAK") {
			t.Fatal("malformed frontend output gained authority or leaked", err)
		}
	}
}

// A failed or over-budget HTTP capture cannot claim a complete response hash;
// its separate cause and requested model remain available without body text.
func TestWinnerSnowResponseDiagnosticsDistinguishHttpCaptureFailures(t *testing.T) {
	provider := defaultWinnerSnowProviderSettings()
	member := provider.Members[0]
	for _, test := range []struct {
		cause    string
		reader   func() io.Reader
		settings func(*winnerModelSettings)
	}{
		{cause: "response_read_failed", reader: func() io.Reader {
			return iotest.ErrReader(errors.New("SYNTHETIC_RAW_BODY_ERROR_MUST_NOT_LEAK"))
		}},
		{cause: "response_size_limit", reader: func() io.Reader { return strings.NewReader("SYNTHETIC_OVERSIZED_BODY_MUST_NOT_LEAK") }, settings: func(s *winnerModelSettings) { s.MaximumStreamBytes = 4 }},
	} {
		settings := defaultWinnerSnowModelSettings()
		if test.settings != nil {
			test.settings(&settings)
		}
		client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(test.reader())}, nil
		})}
		result, requestSha, responseSha, err := executeSnowResponse(t.Context(), client, provider, member, map[string]any{"model": member.Model}, settings)
		var diagnostic *winnerModelResponseDiagnostic
		if result != nil || !validSha256(requestSha) || responseSha != "" || !errors.As(err, &diagnostic) || diagnostic.Cause != test.cause || diagnostic.RequestedModel != member.Model || diagnostic.ResponseEnvelopeSha256 != "" || diagnostic.IdPresent {
			t.Fatal("partial HTTP capture acquired false provenance or lost its cause", err)
		}
		if strings.Contains(err.Error(), "MUST_NOT_LEAK") {
			t.Fatal("HTTP capture diagnostics retained raw provider content")
		}
	}
}
