// Provider schema echoes are metadata, not review-decision field aliases.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the same terminal validator used by broker capture and replay.
func TestWinnerMultipassVisibleResponseAllowsSchemaEcho(t *testing.T) {
	bundle, plan, judgment := winnerMultipassFixture(t)
	evidence := winnerMultipassEvidenceFixture(t, bundle, plan, judgment).Calls[0]
	var response map[string]any
	if err := json.Unmarshal(evidence.VisibleResponse, &response); err != nil {
		t.Fatal(err)
	}
	response["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{
		"type": "object", "additionalProperties": false, "properties": map[string]any{
			"summary": map[string]any{"type": "string", "minLength": 1, "maxLength": 100},
			"issues":  map[string]any{"type": "array", "maxItems": 10},
		},
	}}}
	evidence.VisibleResponse, _ = json.Marshal(response)
	evidence.VisibleResponseSha256 = winnerMultipassSha256(evidence.VisibleResponse)
	original := bytes.Clone(evidence.VisibleResponse)
	if _, err := winnerMultipassVisibleResponse(evidence, plan.Calls[0], plan.Settings); err != nil {
		t.Fatal("standard echoed schema keywords rejected a completed observation", err)
	}
	if !bytes.Equal(original, evidence.VisibleResponse) {
		t.Fatal("metadata validation changed original terminal bytes")
	}
}

// Both supported transports retain the original schema echo, complete through
// the broker, replay independently, and cannot acquire a second provider draw.
func TestWinnerMultipassSchemaEchoComposedCaptureAndReplay(t *testing.T) {
	for _, mediaType := range []string{"application/json", "text/event-stream"} {
		func() {
			bundle, plan, judgment := winnerMultipassProviderFixture(t)
			call := plan.Calls[0]
			evidence := winnerMultipassEvidenceFixture(t, bundle, plan, judgment).Calls[0]
			var response map[string]any
			if err := json.Unmarshal(evidence.VisibleResponse, &response); err != nil {
				t.Fatal(err)
			}
			response["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"summary": map[string]any{"type": "string", "minLength": 1, "maxLength": 100}}}}}
			terminal, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			transport := string(terminal)
			if mediaType == "text/event-stream" {
				transport = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + transport + "}\n\ndata: [DONE]\n\n"
			}
			generations := 0
			base := winnerMultipassProviderClient(t, plan, call, evidence.ProviderOutput, &generations)
			client := &http.Client{Transport: winnerReviewTransport(func(request *http.Request) (*http.Response, error) {
				result, err := base.Transport.RoundTrip(request)
				if err == nil && request.URL.Path == "/v1/responses" {
					result.Body.Close()
					result.Body = io.NopCloser(strings.NewReader(transport))
					result.Header.Set("Content-Type", mediaType)
				}
				return result, err
			})}
			admission := winnerMultipassAdmissionFixture(plan, call, evidence.Request, 100)
			envelope := winnerMultipassBrokerEnvelope{Schema: 1, Kind: "winner-review-multipass-call/v1", Operation: "generate", Owner: plan.Owner, CallDeadline: time.Now().UTC().Add(time.Minute), PlanSha256: winnerMultipassSha256([]byte("synthetic-plan")), BundleSha256: plan.BundleSha256, Call: call, Payload: evidence.Request, Settings: plan.Settings, Admission: &admission, AuthorizedClaimSha256: winnerMultipassSha256([]byte("synthetic-claim"))}
			directory := winnerMultipassProviderDirectory(t)
			result, err := executeWinnerMultipassBroker(context.Background(), client, envelope, plan.Provider, directory, defaultWinnerMultipassProviderLimits())
			if err != nil || result.Error != "" || generations != 1 {
				t.Fatal("schema echo failed the composed single-draw capture", err)
			}
			retained, err := os.ReadFile(filepath.Join(directory, result.TransportResponseArtifact))
			if err != nil || string(retained) != transport || winnerMultipassSha256(retained) != result.TransportResponseSha256 || !bytes.Equal(result.VisibleResponse, terminal) {
				t.Fatal("original transport or terminal was changed", err)
			}
			evidence.VisibleResponse = result.VisibleResponse
			evidence.VisibleResponseSha256 = result.VisibleResponseSha256
			evidence.TransportResponseSha256 = result.TransportResponseSha256
			if _, err := winnerMultipassVisibleResponse(evidence, call, plan.Settings); err != nil {
				t.Fatal("independent terminal replay rejected schema metadata", err)
			}
			if _, err := executeWinnerMultipassBroker(context.Background(), client, envelope, plan.Provider, directory, defaultWinnerMultipassProviderLimits()); err == nil || generations != 1 {
				t.Fatal("schema compatibility allowed another provider draw")
			}
		}()
	}
}

// Exempt only the exact ignored schema subtree, never envelope/decision fields.
func TestWinnerMultipassVisibleResponseRejectsAliasesOutsideSchema(t *testing.T) {
	bundle, plan, judgment := winnerMultipassFixture(t)
	baseline := winnerMultipassEvidenceFixture(t, bundle, plan, judgment).Calls[0]
	for _, field := range []string{"Status", "Model", "Usage", "Output", "Text", "Format", "Schema", "Observation"} {
		func() {
			evidence := baseline
			var response map[string]any
			if err := json.Unmarshal(evidence.VisibleResponse, &response); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "Format":
				response["text"] = map[string]any{"Format": map[string]any{"schema": map[string]any{"additionalProperties": false}}}
			case "Schema":
				response["text"] = map[string]any{"format": map[string]any{"Schema": map[string]any{"additionalProperties": false}}}
			case "Observation":
				var observation map[string]any
				if err := json.Unmarshal([]byte(evidence.ProviderOutput), &observation); err != nil {
					t.Fatal(err)
				}
				observation["Summary"] = "synthetic ambiguous decision"
				output, _ := json.Marshal(observation)
				evidence.ProviderOutput = string(output)
				response["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = string(output)
			default:
				response[field] = "synthetic alias"
			}
			evidence.VisibleResponse, _ = json.Marshal(response)
			evidence.VisibleResponseSha256 = winnerMultipassSha256(evidence.VisibleResponse)
			if _, err := winnerMultipassVisibleResponse(evidence, plan.Calls[0], plan.Settings); err == nil {
				t.Fatal("noncanonical field outside ignored schema accepted")
			}
		}()
	}
}
