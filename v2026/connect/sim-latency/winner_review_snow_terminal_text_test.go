// Synthetic completed envelopes preserve exact judgment bytes; streamed text
// never repairs a malformed terminal or authorizes a provider retry.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Exercise the real serial producer, strict decoder, union and durable journal.
// Every response is intercepted; no live endpoint or production capture is used.
func checkWinnerSnowTerminalText(t *testing.T, name string, stream bool, malformedMember int, validDeltas bool) {
	t.Helper()
	request, judgment := winnerSnowJudgmentFixture()
	judgment.PrivateEvidence = `Synthetic evidence with an escaped quote ", slash \, braces } ] and UTF-8 café.`
	for index := range judgment.Findings {
		judgment.Findings[index].EvidenceSha256 = ""
	}
	outputBytes, err := json.Marshal(judgment)
	if err != nil || len(outputBytes) == 0 || outputBytes[len(outputBytes)-1] != '}' || !json.Valid(outputBytes) {
		t.Fatalf("%s: synthetic complete judgment is invalid: %v", name, err)
	}
	output, malformed := string(outputBytes), string(outputBytes[:len(outputBytes)-1])
	if json.Valid([]byte(malformed)) {
		t.Fatalf("%s: missing outer brace did not reproduce malformed JSON", name)
	}
	provider := defaultWinnerSnowProviderSettings()
	type capture struct {
		Output, ResponseId, RequestSha256, ResponseSha256 string
	}
	captures := []capture{}
	client := &http.Client{Transport: winnerReviewTransport(func(message *http.Request) (*http.Response, error) {
		index := len(captures)
		if index >= len(provider.Members) {
			t.Fatalf("%s: completed member triggered a fourth provider call", name)
		}
		if message.Method != http.MethodPost || message.URL.String() != provider.Endpoint || message.Header.Get("Authorization") != "" {
			t.Fatalf("%s: intercepted request gained an unexpected destination or credential", name)
		}
		body, err := io.ReadAll(message.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct{ Model string }
		if err := json.Unmarshal(body, &payload); err != nil || payload.Model != provider.Members[index].Model {
			t.Fatalf("%s: member order changed or a member was retried: %v", name, err)
		}
		text := output
		if index == malformedMember {
			text = malformed
		}
		responseId := fmt.Sprintf("synthetic-terminal-response-%d", index)
		response := map[string]any{"id": responseId, "status": "completed", "model": payload.Model,
			"usage":  map[string]int{"input_tokens": 10, "output_tokens": 10},
			"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}}
		content, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		if stream {
			header.Set("Content-Type", "text/event-stream")
			var events bytes.Buffer
			if validDeltas && index == malformedMember {
				// The separate final-brace delta and complete done text are both
				// valid, but only the later terminal envelope owns the judgment.
				for _, event := range []map[string]any{
					{"type": "response.output_text.delta", "delta": malformed},
					{"type": "response.output_text.delta", "delta": output[len(output)-1:]},
					{"type": "response.output_text.done", "text": output},
				} {
					encoded, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					fmt.Fprintf(&events, "data: %s\n\n", encoded)
				}
			}
			event, err := json.Marshal(map[string]any{"type": "response.completed", "response": response})
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&events, "event: response.completed\ndata: %s\n\ndata: [DONE]\n\n", event)
			content = events.Bytes()
		}
		requestDigest, responseDigest := sha256.Sum256(body), sha256.Sum256(content)
		captures = append(captures, capture{Output: text, ResponseId: responseId,
			RequestSha256: hex.EncodeToString(requestDigest[:]), ResponseSha256: hex.EncodeToString(responseDigest[:])})
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(content))}, nil
	})}
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	result, err := executeWinnerSnow(t.Context(), client, request, defaultWinnerSnowModelSettings(), provider, strings.Repeat("a", 64), state)
	if err != nil || result == nil || len(captures) != len(provider.Members) {
		t.Fatalf("%s: serial producer did not finish exactly three intercepted calls: %v", name, err)
	}
	var evidence winnerSnowEvidence
	if err := json.Unmarshal([]byte(result.PrivateEvidence), &evidence); err != nil || len(evidence.Members) != len(provider.Members) {
		t.Fatalf("%s: complete member originals were lost: %v", name, err)
	}
	for index, member := range evidence.Members {
		captured := captures[index]
		if member.ProviderOutput != captured.Output || member.ProviderResponseId != captured.ResponseId || member.Model != provider.Members[index].Model || member.RequestSha256 != captured.RequestSha256 || member.ResponseSha256 != captured.ResponseSha256 {
			t.Fatalf("%s: member %d changed terminal text or its transport provenance", name, index)
		}
		if index == malformedMember {
			if member.Error != "Snow review judgment is malformed" || !reflect.DeepEqual(member.Judgment, winnerModelJudgment{}) {
				t.Fatalf("%s: malformed completed text was repaired or interpreted", name)
			}
		} else if member.Error != "" || !reflect.DeepEqual(member.Judgment, judgment) || member.ProviderOutput[len(member.ProviderOutput)-1] != '}' {
			t.Fatalf("%s: member %d lost the final judgment byte or decoded evidence", name, index)
		}
		content, err := os.ReadFile(filepath.Join(state, fmt.Sprintf("member-%02d.complete.json", index+1)))
		if err != nil {
			t.Fatal(err)
		}
		var journal struct {
			Schema      int                      `json:"schema"`
			Observation winnerSnowMemberEvidence `json:"observation"`
		}
		if err := json.Unmarshal(content, &journal); err != nil || journal.Schema != 1 || !reflect.DeepEqual(journal.Observation, member) {
			t.Fatalf("%s: member %d journal changed the original terminal observation: %v", name, index, err)
		}
	}
	if err := validateWinnerSnowEvidence(result, request); err != nil {
		t.Fatalf("%s: strict replay lost the original non-retryable evidence: %v", name, err)
	}
	validationErr := validateWinnerReviewBrokerResult(result, request.Template, request.Request.Policy, request)
	if malformedMember < 0 {
		if validationErr != nil || evidence.Judgment.Decision != "approve" || result.Review.RewardDisposition != "approved" || result.Review.SourceDisposition != "approved" {
			t.Fatalf("%s: complete unchanged terminal judgments lost approval: %v", name, validationErr)
		}
	} else {
		if validationErr == nil || evidence.Judgment.Decision != "uncertain" || result.Review.RewardDisposition != "pending" || result.Review.SourceDisposition != "deferred" {
			t.Fatalf("%s: malformed completed judgment acquired authority", name)
		}
		if _, err := executeWinnerSnow(t.Context(), client, request, defaultWinnerSnowModelSettings(), provider, strings.Repeat("a", 64), state); err == nil || len(captures) != len(provider.Members) {
			t.Fatalf("%s: replay rerolled a completed malformed member", name)
		}
	}
}

// Both frontend encodings must preserve the final ASCII brace and all escaped
// or multibyte evidence through decoding, aggregation and durable journaling.
func TestWinnerSnowTransportPreservesFinalJudgmentByte(t *testing.T) {
	for _, sample := range []struct {
		name   string
		stream bool
	}{
		{name: "json"},
		{name: "sse", stream: true},
	} {
		checkWinnerSnowTerminalText(t, sample.name, sample.stream, -1, false)
	}
}

// A missing outer brace stays malformed in the first or last member. The
// remaining members still run once, and the original attempt cannot reroll.
func TestWinnerSnowMissingOuterBraceIsRetainedAndNonAuthorizing(t *testing.T) {
	for _, sample := range []struct {
		name            string
		stream          bool
		malformedMember int
	}{
		{name: "json first member", malformedMember: 0},
		{name: "json last member", malformedMember: 2},
		{name: "sse first member", stream: true, malformedMember: 0},
		{name: "sse last member", stream: true, malformedMember: 2},
	} {
		checkWinnerSnowTerminalText(t, sample.name, sample.stream, sample.malformedMember, false)
	}
}

// Complete approval deltas and done text cannot supply a missing terminal byte;
// a malformed completed envelope remains the sole nonauthorizing observation.
func TestWinnerSnowMalformedTerminalIsNotRescuedByValidDeltas(t *testing.T) {
	checkWinnerSnowTerminalText(t, "valid deltas before malformed terminal", true, 0, true)
}
