// Storage boundary tests preserve full provider limits. Their intercepted
// responses and custody callbacks are explicitly synthetic, never qualification.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWinnerMultipassSpoolCaptureDescriptorRecordDoesNotEmbedRawOriginals(t *testing.T) {
	limits := defaultWinnerMultipassProviderLimits()
	settings := defaultWinnerMultipassSettings()
	raw := bytes.Repeat([]byte("r"), settings.MaximumResponseBytes)
	stateCopy := append([]byte{}, raw...)
	capture := winnerMultipassQualificationCapture{Input: bytes.Repeat([]byte("i"), limits.MaximumEnvelopeBytes), Output: bytes.Repeat([]byte("o"), limits.MaximumResultBytes), Diagnostics: bytes.Repeat([]byte("d"), 64*1024), ClaimContent: bytes.Repeat([]byte("c"), limits.MaximumMetadataBytes),
		Containment: &winnerMultipassContainmentReceipt{WrapperReceipt: bytes.Repeat([]byte("w"), limits.MaximumMetadataBytes), Artifacts: []currentQgQualificationBinding{{Path: "transport.response", Sha256: winnerMultipassSha256(raw), Bytes: int64(len(raw))}},
			Result: &winnerMultipassBrokerResult{VisibleResponse: bytes.Repeat([]byte("v"), limits.MaximumVisibleTerminalBytes), ProviderOutput: strings.Repeat("\x01", settings.MaximumObservationBytes)}}}
	content, err := json.Marshal(capture)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) > winnerMultipassSpoolCaptureBytes {
		t.Fatalf("descriptor capture cap silently narrowed legal raw/envelope bounds: %d", len(content))
	}
	var decoded winnerMultipassQualificationCapture
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Input, capture.Input) || !bytes.Equal(decoded.Output, capture.Output) || !bytes.Equal(decoded.Containment.Result.VisibleResponse, capture.Containment.Result.VisibleResponse) ||
		decoded.Containment.Artifacts[0].Bytes != 16*1024*1024 || decoded.Containment.Artifacts[0].Sha256 != winnerMultipassSha256(raw) || !bytes.Equal(raw, stateCopy) {
		t.Fatal("descriptor capture roundtrip clipped original data or changed separate raw custody")
	}
	t.Logf("Synthetic maximal descriptor record=%d bytes; separately retained raw/state copies=%d+%d bytes", len(content), len(raw), len(stateCopy))
}

// A real-shaped full16MiB visible SSE body passes contained capture, exact file
// retention and pure journal replay unchanged. No raw bytes enter the compact
// descriptor record; the two physical copies retain the same full digest.
func TestWinnerMultipassSpoolRetainsAndReplaysMaximumRawResponseWithLargeEnvelope(t *testing.T) {
	bundle, plan, _ := winnerMultipassProviderFixture(t)
	call := plan.Calls[0]
	payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	// Whitespace is synthetic transport padding, not an omitted source fragment.
	request["input"] = request["input"].(string) + strings.Repeat(" ", plan.Settings.MaximumRequestBytes-len(payload)-32)
	payload, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	admission := winnerMultipassAdmissionFixture(plan, call, payload, 100)
	envelope := winnerMultipassBrokerEnvelope{Schema: 1, Kind: "winner-review-multipass-call/v1", Operation: "generate", Owner: plan.Owner, CallDeadline: time.Now().UTC().Add(time.Minute), PlanSha256: winnerMultipassSha256([]byte("synthetic-large-plan")), BundleSha256: plan.BundleSha256, Call: call, Payload: payload, Settings: plan.Settings, Admission: &admission, AuthorizedClaimSha256: winnerMultipassSha256([]byte("synthetic-large-claim"))}
	observation := winnerMultipassObservation{Summary: "Synthetic bounded original observation.", Issues: []winnerMultipassIssue{}, RetainedIssueIds: []string{}, DeferredArtifactIds: []string{}}
	output, _ := json.Marshal(observation)
	terminal := winnerModelFixtureStreamForModel(t, string(output), call.Member.Model)
	padding := "event: response.in_progress\n"
	remaining := plan.Settings.MaximumResponseBytes - len(terminal)
	raw := []byte(strings.Repeat(padding, remaining/len(padding)) + terminal + strings.Repeat("\n", remaining%len(padding)))
	generations := 0
	base := winnerMultipassProviderClient(t, plan, call, string(output), &generations)
	client := &http.Client{Transport: winnerReviewTransport(func(request *http.Request) (*http.Response, error) {
		response, err := base.Transport.RoundTrip(request)
		if err != nil || request.URL.Path != "/v1/responses" {
			return response, err
		}
		response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(raw))
		return response, nil
	})}
	directory := winnerMultipassProviderDirectory(t)
	result, err := executeWinnerMultipassBroker(context.Background(), client, envelope, plan.Provider, directory, defaultWinnerMultipassProviderLimits())
	if err != nil || generations != 1 {
		t.Fatal("full declared raw-response bound was not accepted", err)
	}
	retained, err := os.ReadFile(filepath.Join(directory, "transport.response"))
	if err != nil || !bytes.Equal(retained, raw) || len(retained) != 16*1024*1024 {
		t.Fatal("full raw response was clipped", err)
	}
	copyPath := filepath.Join(winnerMultipassProviderDirectory(t), "captured-transport.response")
	if err := writePrivateReviewFile(copyPath, retained); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(copyPath)
	if err != nil || !bytes.Equal(copied, raw) {
		t.Fatal("separate captured original changed", err)
	}
	input, _ := json.Marshal(envelope)
	resultBytes, _ := json.Marshal(result)
	providerBytes, _ := json.Marshal(plan.Provider)
	members := []map[string]string{}
	for _, member := range plan.Provider.Members {
		members = append(members, map[string]string{"name": member.Name, "model": member.Model, "revision": member.Revision, "reasoning": member.Reasoning})
	}
	memberBytes, _ := json.Marshal(members)
	owner := winnerMultipassSha256([]byte("synthetic-wrapper-owner"))
	attempt := "/synthetic/attempt/invocation.max-response"
	claim, _ := json.Marshal(map[string]any{"schema": 1, "kind": "snow-multipass-single-call-claim", "idempotency_key_sha256": winnerMultipassSha256([]byte(envelope.Owner.Id + "/" + call.Id)), "input_sha256": winnerMultipassSha256(input), "provider_settings_sha256": winnerMultipassSha256(providerBytes), "model_set_sha256": winnerMultipassSha256(memberBytes), "owner": owner, "attempt_directory": attempt, "automatic_retries": false})
	artifacts := map[string][]byte{"provider-settings.json": providerBytes, "input.json": input, "call.started.json": input, "call.complete.json": resultBytes, "broker.stdout": resultBytes, "attempt-claim.json": claim, "transport.response": retained, "terminal.json": []byte(`{"synthetic":"wrapper terminal"}`)}
	if err := validateWinnerMultipassJournalBytes(artifacts, currentQgQualificationPin{Path: "/synthetic/provider.json", Sha256: winnerMultipassSha256(providerBytes)}, owner, attempt, envelope, *result, claim); err != nil {
		t.Fatal("full original journal did not replay", err)
	}
	if err := validateWinnerMultipassJournalBytes(artifacts, currentQgQualificationPin{Path: "/synthetic/provider.json", Sha256: winnerMultipassSha256(providerBytes)}, owner, attempt, envelope, *result, []byte("synthetic changed root claim")); err == nil {
		t.Fatal("substituted independent root claim accepted")
	}
	capture := winnerMultipassQualificationCapture{Input: input, Output: resultBytes, Containment: &winnerMultipassContainmentReceipt{Result: result, Artifacts: []currentQgQualificationBinding{{Path: "transport.response", Sha256: result.TransportResponseSha256, Bytes: int64(len(raw))}}}}
	encoded, err := json.Marshal(capture)
	if err != nil || len(encoded) > winnerMultipassSpoolCaptureBytes {
		t.Fatal("large actual envelope plus16MiB separate raw needs no tighter capture limit", err)
	}
}

func TestWinnerMultipassSpoolQualificationRequiresReservationBeforeAnyProviderInvocation(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	content, _ := json.Marshal(plan)
	called := 0
	hooks := winnerMultipassQualificationSpoolHooks{Mode: "offline", Now: func() time.Time { return plan.Owner.StartedAt.Add(time.Minute) }, ValidateRelease: func(winnerMultipassQualificationRelease) error { return nil }, Reserve: func(*winnerMultipassSpoolCapacity, *winnerMultipassQualificationCapacity) error {
		return errors.New("synthetic unreserved local capacity")
	},
		Load: func(winnerMultipassQualificationInvocation) (*winnerMultipassQualificationSpoolResolvedCapture, error) {
			called++
			return nil, nil
		}, Claim: func(winnerMultipassQualificationInvocation, []byte) (currentQgQualificationPin, error) {
			called++
			return currentQgQualificationPin{}, nil
		}, Invoke: func(context.Context, []byte) ([]byte, []byte, error) { called++; return nil, nil, nil }, Authenticate: func([]byte, []byte, []byte, bool) (*winnerMultipassContainmentReceipt, error) {
			called++
			return nil, nil
		}, Retain: func(context.Context, winnerMultipassQualificationCapture) (*winnerMultipassQualificationSpoolResolvedCapture, error) {
			called++
			return nil, nil
		}, RetainCase: func(int, winnerMultipassEvidence) (currentQgQualificationPin, error) {
			called++
			return currentQgQualificationPin{}, nil
		}}
	settings := defaultWinnerMultipassCapacitySettings()
	settings.MaximumDurableBytes = 128 * 1024 * 1024 * 1024
	spool := winnerMultipassSpoolSettings{LifecycleVersion: winnerMultipassSpoolLifecycle, MaximumLocalPeakBytes: 8 * 1024 * 1024 * 1024, MaximumProtectedBytes: 128 * 1024 * 1024 * 1024}
	report, err := executeWinnerMultipassSpoolQualification(context.Background(), plan, currentQgQualificationPin{Path: "/synthetic/plan", Sha256: winnerMultipassSha256(content)}, settings, spool, hooks)
	if err == nil || called != 0 || report == nil || report.Kind != "winner-review-multipass-qualification-spool-report/v1" || report.Qualified || report.Complete || report.SpoolCapacity == nil || len(report.CaptureCommits) != 0 {
		t.Fatal("unreserved spool invoked a provider or claimed qualification")
	}
	if _, err := authenticateWinnerMultipassSpoolQualification(currentQgQualificationPin{}, plan.Release, nil); err == nil {
		t.Fatal("missing protected resolver was accepted")
	}
}
