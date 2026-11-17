// Visible output stays exact, while unexpected private reasoning cannot enter
// the durable transcript even when a backend violates the requested privacy.
package main

import (
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

// Alternate typed fields and unparsed transports must not bypass the canonical
// reasoning-event check. A quoted word in visible output is not a hidden field.
func TestWinnerMultipassPrivacyRejectsAlternateAndUnparsedReasoning(t *testing.T) {
	for index, item := range []struct{ Content, MediaType string }{
		{Content: `{"reasoning_content":"synthetic private analysis"}`, MediaType: "application/json"},
		{Content: `{"Reasoning_Content":"synthetic private analysis"}`, MediaType: "application/json"},
		{Content: `{"content":{"analysis":["synthetic private analysis"]}}`, MediaType: "application/json"},
		{Content: `{"reasoning":{"text":"synthetic private analysis"}}`, MediaType: "application/json"},
		{Content: `{"reasoning":"synthetic private analysis"}`, MediaType: "application/json"},
		{Content: `{"channel":"analysis","content":"synthetic private analysis"}`, MediaType: "application/json"},
		{Content: `{"encrypted_content":{"unexpected":"shape"}}`, MediaType: "application/json"},
		{Content: `{"type":"reasoning_summary","text":"synthetic private summary"}`, MediaType: "application/json"},
		{Content: "event: response.reasoning_text.delta\ndata: {\"delta\":\"synthetic private analysis\"}\n\n", MediaType: "text/event-stream"},
		{Content: "unparsed synthetic private analysis\ndata: {\"type\":\"response.completed\"}\n\n", MediaType: "text/event-stream"},
		{Content: ": synthetic private analysis\ndata: {\"type\":\"response.completed\"}\n\n", MediaType: "text/event-stream"},
		{Content: "synthetic private analysis", MediaType: "text/plain"},
		{Content: `{"synthetic_visible":true}`, MediaType: "application/json-unpinned"},
		{Content: "data: [DONE]\n\n", MediaType: "text/event-stream"},
	} {
		if winnerMultipassVisibleTransport([]byte(item.Content), item.MediaType, 4096) == nil {
			t.Fatalf("unproven private transport %d accepted for retention", index)
		}
	}
	visible := `{"reasoning":{"effort":"high"},"usage":{"output_tokens_details":{"reasoning_tokens":30}},"reasoning_content":null,"analysis_text":"","output":[{"type":"message","content":[{"type":"output_text","text":"Visible quoted text may contain analysis or reasoning_content."}]}]}`
	for _, item := range []struct{ Content, MediaType string }{
		{Content: visible, MediaType: "application/json; charset=utf-8"},
		{Content: "event: response.completed\ndata: " + visible + "\n\ndata: [DONE]\n\n", MediaType: "text/event-stream; charset=utf-8"},
	} {
		if err := winnerMultipassVisibleTransport([]byte(item.Content), item.MediaType, 4096); err != nil {
			t.Fatal("visible output/configuration/usage was mistaken for hidden reasoning", err)
		}
	}
}

// Optional empty configuration is not private reasoning, including in echoed
// streaming response envelopes from newer provider schemas.
func TestWinnerMultipassPrivacyAllowsNullReasoningConfiguration(t *testing.T) {
	for _, field := range []string{"context", "mode"} {
		content := `{"type":"response.created","response":{"reasoning":{"effort":"xhigh","summary":null,"generate_summary":null,"` + field + `":null},"output":[]}}`
		for _, transport := range []struct{ Content, MediaType string }{
			{Content: content, MediaType: "application/json"},
			{Content: "event: response.created\ndata: " + content + "\n\n", MediaType: "text/event-stream"},
		} {
			if err := winnerMultipassVisibleTransport([]byte(transport.Content), transport.MediaType, 4096); err != nil {
				t.Errorf("null configuration field %s was mistaken for private reasoning: %v", field, err)
			}
		}
	}
}

// Compatibility is restricted to known null fields, not arbitrary configuration
// values, unknown fields or private reasoning disguised as metadata.
func TestWinnerMultipassPrivacyRejectsNonemptyAndUnknownReasoningConfiguration(t *testing.T) {
	for _, content := range []string{
		`{"reasoning":{"effort":"xhigh","context":"synthetic unexpected content"}}`,
		`{"reasoning":{"effort":"xhigh","mode":"synthetic unexpected content"}}`,
		`{"reasoning":{"effort":"xhigh","unrecognized":null}}`,
		`{"reasoning":{"effort":"xhigh","context":{"text":"synthetic private content"}}}`,
		`{"reasoning":{"effort":"xhigh","summary":"synthetic private summary"}}`,
	} {
		if winnerMultipassVisibleTransport([]byte(content), "application/json", 4096) == nil {
			t.Error("nonempty or unrecognized reasoning configuration was accepted")
		}
	}
}

// The broker retains the exact benign envelope without weakening its one-draw
// claim or changing the request's privacy, tool and truncation settings.
func TestWinnerMultipassPrivacyRetainsNullReasoningConfigurationTransport(t *testing.T) {
	bundle, plan, _ := winnerMultipassProviderFixture(t)
	call := plan.Calls[0]
	payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	observation := winnerMultipassObservation{Summary: "Synthetic scoped review.", Issues: []winnerMultipassIssue{}, RetainedIssueIds: []string{}, DeferredArtifactIds: []string{}}
	output, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	generations := 0
	base := winnerMultipassProviderClient(t, plan, call, string(output), &generations)
	transport := ""
	client := &http.Client{Transport: winnerReviewTransport(func(request *http.Request) (*http.Response, error) {
		response, err := base.Transport.RoundTrip(request)
		if err != nil || request.URL.Path != "/v1/responses" {
			return response, err
		}
		content, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		transport = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"reasoning\":{\"effort\":\"xhigh\",\"context\":null,\"mode\":null,\"summary\":null,\"generate_summary\":null},\"output\":[]}}\n\n" + string(content)
		response.Body = io.NopCloser(strings.NewReader(transport))
		return response, nil
	})}
	admission := winnerMultipassAdmissionFixture(plan, call, payload, 100)
	envelope := winnerMultipassBrokerEnvelope{Schema: 1, Kind: "winner-review-multipass-call/v1", Operation: "generate", Owner: plan.Owner, CallDeadline: time.Now().UTC().Add(time.Minute),
		PlanSha256: winnerMultipassSha256([]byte("synthetic-plan")), BundleSha256: plan.BundleSha256, Call: call, Payload: payload, Settings: plan.Settings, Admission: &admission, AuthorizedClaimSha256: winnerMultipassSha256([]byte("synthetic-claim"))}
	directory := winnerMultipassProviderDirectory(t)
	result, err := executeWinnerMultipassBroker(context.Background(), client, envelope, plan.Provider, directory, defaultWinnerMultipassProviderLimits())
	if err != nil || result.Error != "" || generations != 1 {
		t.Fatal("benign metadata prevented the single admitted response from completing", err)
	}
	retained, err := os.ReadFile(filepath.Join(directory, result.TransportResponseArtifact))
	if err != nil || string(retained) != transport || winnerMultipassSha256(retained) != result.TransportResponseSha256 {
		t.Fatal("the original visible transport was altered or not retained", err)
	}
	if _, err := executeWinnerMultipassBroker(context.Background(), client, envelope, plan.Provider, directory, defaultWinnerMultipassProviderLimits()); err == nil || generations != 1 {
		t.Fatal("metadata compatibility permitted another draw")
	}
}

// The complete contained path retains a digest and terminal failure but no
// private payload bytes; a second invocation cannot repeat the failed draw.
func TestWinnerMultipassPrivacyFailureRetainsDigestNotHiddenPayloadOrRetry(t *testing.T) {
	bundle, plan, _ := winnerMultipassProviderFixture(t)
	call := plan.Calls[0]
	payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	admission := winnerMultipassAdmissionFixture(plan, call, payload, 100)
	envelope := winnerMultipassBrokerEnvelope{Schema: 1, Kind: "winner-review-multipass-call/v1", Operation: "generate", Owner: plan.Owner, CallDeadline: time.Now().UTC().Add(time.Minute),
		PlanSha256: winnerMultipassSha256([]byte("synthetic-plan")), BundleSha256: plan.BundleSha256, Call: call, Payload: payload, Settings: plan.Settings, Admission: &admission, AuthorizedClaimSha256: winnerMultipassSha256([]byte("synthetic-claim"))}
	generations := 0
	base := winnerMultipassProviderClient(t, plan, call, "unused synthetic output", &generations)
	secret := "synthetic-hidden-content-never-retained"
	private, _ := json.Marshal(map[string]any{"reasoning_content": secret})
	client := &http.Client{Transport: winnerReviewTransport(func(request *http.Request) (*http.Response, error) {
		response, err := base.Transport.RoundTrip(request)
		if err != nil || request.URL.Path != "/v1/responses" {
			return response, err
		}
		response.Body.Close()
		response.Body = io.NopCloser(strings.NewReader(string(private)))
		response.Header.Set("Content-Type", "application/json")
		return response, nil
	})}
	directory := winnerMultipassProviderDirectory(t)
	result, err := executeWinnerMultipassBroker(context.Background(), client, envelope, plan.Provider, directory, defaultWinnerMultipassProviderLimits())
	if err == nil || result.Error == "" || generations != 1 || result.TransportResponseSha256 != winnerMultipassSha256(private) || result.TransportResponseArtifact != "" || len(result.VisibleResponse) != 0 {
		t.Fatal("privacy failure was accepted, discarded, or retained a hidden transcript")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 2 {
		t.Fatal("privacy failure did not retain exactly its original intent and terminal failure", err)
	}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil || strings.Contains(string(content), secret) {
			t.Fatal("hidden payload entered a retained journal", err)
		}
	}
	if _, err := executeWinnerMultipassBroker(context.Background(), client, envelope, plan.Provider, directory, defaultWinnerMultipassProviderLimits()); err == nil || generations != 1 {
		t.Fatal("privacy failure was rerolled")
	}
}
