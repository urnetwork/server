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

func TestWinnerMultipassRuntimeProbeObservesDeclaredContextsWithoutAdmission(t *testing.T) {
	provider := defaultWinnerSnowProviderSettings()
	providerBytes, _ := json.Marshal(provider)
	now := time.Now().UTC()
	owner := winnerMultipassOwner{Id: "synthetic-runtime-bootstrap", StartedAt: now.Add(-time.Minute), Deadline: now.Add(9 * time.Minute)}
	for _, member := range provider.Members {
		t.Run(member.Name, func(t *testing.T) {
			envelope, err := buildWinnerMultipassRuntimeProbe(owner, member, now.Add(4*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if envelope.Settings.TokenizationProfiles[member.Name].RuntimeEvidenceSha256 != "" {
				t.Fatal("prospective declaration invented observed runtime evidence")
			}
			generations := 0
			client := winnerMultipassProviderClient(t, &winnerMultipassPlan{Provider: provider, Settings: envelope.Settings}, envelope.Call, "", &generations)
			directory := winnerMultipassProviderDirectory(t)
			result, err := executeWinnerMultipassBroker(context.Background(), client, envelope, provider, directory, defaultWinnerMultipassProviderLimits())
			if err != nil || result.Admission != nil || result.RuntimeObservation == nil || result.RuntimeObservation.ObservedMaximumContextTokens != envelope.Settings.TokenizationProfiles[member.Name].MaximumContextTokens || generations != 0 {
				t.Fatal("non-inference runtime probe failed or acquired generation authority", err)
			}
			assertWinnerMultipassJournalCensus(t, directory, []string{"call.complete.json", "call.started.json", "tokenize.request.json", "tokenize.response.json"})
			input, _ := json.Marshal(envelope)
			output, _ := json.Marshal(result)
			artifacts := map[string][]byte{"provider-settings.json": providerBytes, "input.json": input, "call.started.json": input, "call.complete.json": output, "broker.stdout": output}
			for _, name := range []string{"tokenize.request.json", "tokenize.response.json"} {
				artifacts[name], err = os.ReadFile(filepath.Join(directory, name))
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := validateWinnerMultipassJournalBytes(artifacts, currentQgQualificationPin{Path: "/synthetic/provider.json", Sha256: winnerMultipassSha256(providerBytes)}, "synthetic-owner", "/synthetic/runtime-attempt", envelope, *result, nil); err != nil {
				t.Fatal("observed runtime did not replay exact original journals", err)
			}
			result.RuntimeObservation.Metadata[1].Body = []byte(`{"version":"synthetic-wrong"}`)
			if err := validateWinnerMultipassRuntimeObservation(envelope, result.RuntimeObservation, artifacts["tokenize.request.json"], artifacts["tokenize.response.json"]); err == nil {
				t.Fatal("runtime version substitution accepted")
			}
			if _, err := executeWinnerMultipassBroker(context.Background(), client, envelope, provider, directory, defaultWinnerMultipassProviderLimits()); err == nil || generations != 0 {
				t.Fatal("started synthetic probe silently retried")
			}
		})
	}
}

func TestWinnerMultipassRuntimeProbeCannotCarrySourceOrGenerate(t *testing.T) {
	now := time.Now().UTC()
	provider := defaultWinnerSnowProviderSettings()
	owner := winnerMultipassOwner{Id: "synthetic-runtime-boundary", StartedAt: now.Add(-time.Second), Deadline: now.Add(9 * time.Minute)}
	envelope, err := buildWinnerMultipassRuntimeProbe(owner, provider.Members[0], now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*winnerMultipassBrokerEnvelope){
		func(value *winnerMultipassBrokerEnvelope) { value.Operation = "generate" },
		func(value *winnerMultipassBrokerEnvelope) { value.Kind = "winner-review-multipass-call/v1" },
		func(value *winnerMultipassBrokerEnvelope) {
			value.Payload = bytes.Replace(value.Payload, []byte("Synthetic complete-coverage"), []byte("Unapproved private source"), 1)
		},
		func(value *winnerMultipassBrokerEnvelope) {
			value.AuthorizedClaimSha256 = winnerMultipassSha256([]byte("synthetic claim"))
		},
		func(value *winnerMultipassBrokerEnvelope) {
			profile := value.Settings.TokenizationProfiles[provider.Members[0].Name]
			profile.RuntimeEvidenceSha256 = winnerMultipassSha256([]byte("invented runtime"))
			value.Settings.TokenizationProfiles[provider.Members[0].Name] = profile
		},
		func(value *winnerMultipassBrokerEnvelope) {
			value.CallDeadline = now.Add(5*time.Minute + time.Nanosecond)
		},
	} {
		content, _ := json.Marshal(envelope)
		var changed winnerMultipassBrokerEnvelope
		if err := json.Unmarshal(content, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		if err := validateWinnerMultipassBrokerEnvelope(changed, provider, now, defaultWinnerMultipassProviderLimits()); err == nil {
			t.Fatal("synthetic runtime probe admitted a different payload, fabricated evidence or generation")
		}
	}
	owner.Deadline = owner.StartedAt.Add(10*time.Minute + time.Nanosecond)
	if _, err := buildWinnerMultipassRuntimeProbe(owner, provider.Members[0], now.Add(time.Minute)); err == nil {
		t.Fatal("bootstrap silently expanded its separate owner clock")
	}
}

func TestWinnerMultipassRuntimeProbeMismatchRetainsOriginalWithoutRetry(t *testing.T) {
	now := time.Now().UTC()
	provider := defaultWinnerSnowProviderSettings()
	envelope, err := buildWinnerMultipassRuntimeProbe(winnerMultipassOwner{Id: "synthetic-mismatch-probe", StartedAt: now.Add(-time.Minute), Deadline: now.Add(9 * time.Minute)}, provider.Members[0], now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"context", "version", "resident"} {
		t.Run(field, func(t *testing.T) {
			generations, requests := 0, 0
			client := winnerMultipassProviderClient(t, &winnerMultipassPlan{Provider: provider, Settings: envelope.Settings}, envelope.Call, "", &generations)
			original := client.Transport
			client.Transport = winnerReviewTransport(func(request *http.Request) (*http.Response, error) {
				requests++
				response, err := original.RoundTrip(request)
				if err != nil {
					return nil, err
				}
				content, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					return nil, err
				}
				if field == "context" && strings.HasSuffix(request.URL.Path, "/tokenize") {
					content = bytes.Replace(content, []byte(`"max_model_len":98304`), []byte(`"max_model_len":131072`), 1)
				}
				if field == "version" && strings.HasSuffix(request.URL.Path, "/version") {
					content = []byte(`{"version":"synthetic-wrong"}`)
				}
				if field == "resident" && request.URL.Path == "/running" {
					content = []byte(`{"running":[{"model":"synthetic-wrong","state":"ready"}]}`)
				}
				response.Body = io.NopCloser(bytes.NewReader(content))
				return response, nil
			})
			directory := winnerMultipassProviderDirectory(t)
			result, err := executeWinnerMultipassBroker(context.Background(), client, envelope, provider, directory, defaultWinnerMultipassProviderLimits())
			if err == nil || result == nil || result.Error == "" || result.Admission != nil || result.RuntimeObservation == nil || len(result.RuntimeObservation.Metadata) != 3 || generations != 0 {
				t.Fatal("mismatched runtime gained admission or lost original metadata")
			}
			complete, err := os.ReadFile(filepath.Join(directory, "call.complete.json"))
			if err != nil || !bytes.Contains(complete, []byte(`"runtime_observation"`)) {
				t.Fatal("failed runtime original completion missing", err)
			}
			before := requests
			if _, err := executeWinnerMultipassBroker(context.Background(), client, envelope, provider, directory, defaultWinnerMultipassProviderLimits()); err == nil || requests != before {
				t.Fatal("failed probe rerolled network work")
			}
		})
	}
}

func TestWinnerMultipassRuntimeProbeReleaseCannotQualifyNormalPortableCalls(t *testing.T) {
	originals, proof, release, profile, provider, _, _ := winnerMultipassPortableFixture(t)
	if validateWinnerMultipassQualificationReleaseShape(release) != nil || validateWinnerMultipassRuntimeProbeReleaseShape(release) == nil {
		t.Fatal("observed release was mislabeled an empty bootstrap declaration")
	}
	for name, tokenization := range release.TokenizationProfiles {
		tokenization.RuntimeEvidenceSha256 = ""
		release.TokenizationProfiles[name] = tokenization
	}
	if validateWinnerMultipassRuntimeProbeReleaseShape(release) != nil || validateWinnerMultipassQualificationReleaseShape(release) == nil {
		t.Fatal("empty bootstrap declaration qualified normal review")
	}
	releaseBytes, _ := json.Marshal(release)
	proof.ReleaseSha256 = winnerMultipassSha256(releaseBytes)
	content, _ := json.Marshal(proof)
	minimum := time.Now().UTC().Add(time.Hour)
	reference := winnerMultipassProtectedReference{Key: "synthetic/bootstrap/proof", VersionId: "synthetic-bootstrap-version", Sha256: winnerMultipassSha256(content), Bytes: int64(len(content)), RetainUntil: minimum.Add(time.Hour)}
	authenticated, err := loadWinnerMultipassPhysicalAuthentication(context.Background(), reference, release, minimum, func(context.Context, winnerMultipassProtectedReference) ([]byte, error) { return content, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replayWinnerMultipassPortableContainmentForProfile(originals, nil, authenticated, profile, provider); err == nil {
		t.Fatal("protected bootstrap proof authenticated a normal call")
	}
	first := release.TokenizationProfiles["qwen"]
	first.RuntimeEvidenceSha256 = winnerMultipassSha256([]byte("synthetic partial observation"))
	release.TokenizationProfiles["qwen"] = first
	if validateWinnerMultipassRuntimeProbeReleaseShape(release) == nil || validateWinnerMultipassQualificationReleaseShape(release) == nil {
		t.Fatal("partial pair of runtime observations qualified either boundary")
	}
}
