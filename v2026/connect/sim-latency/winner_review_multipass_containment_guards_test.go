// Private synthetic captures exercise the new containment profile. No Docker,
// firewall, provider, privilege change or production filesystem is touched.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The old synthetic helper supplies shared kernel observations only. New wire
// bytes, entrypoint and fsize are assembled explicitly, not accepted as a real
// converted historical receipt. The production authenticator never converts.
func winnerMultipassCanceledContainmentFixture(t *testing.T) ([]byte, []byte, []byte, string, currentQgQualificationSettings, string, currentQgQualificationProviderProfile, map[string][]byte) {
	t.Helper()
	_, _, diagnostics, image, settings, attempt, profile := currentQgContainmentFixture(t, true)
	var receipt currentQgQualificationWrapperReceipt
	if err := json.Unmarshal(diagnostics, &receipt); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(attempt, "evidence-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Artifacts []currentQgQualificationBinding `json:"artifacts"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	artifacts := map[string][]byte{}
	for _, binding := range manifest.Artifacts {
		content, err := os.ReadFile(filepath.Join(attempt, binding.Path))
		if err != nil {
			t.Fatal(err)
		}
		artifacts[binding.Path] = content
	}
	bundle, plan, _ := winnerMultipassProviderFixture(t)
	call := plan.Calls[0]
	payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(winnerMultipassBrokerEnvelope{Schema: 1, Kind: "winner-review-multipass-call/v1", Operation: "qualification_cancel_before_provider", Owner: plan.Owner,
		CallDeadline: time.Now().UTC().Add(time.Minute), PlanSha256: winnerMultipassSha256([]byte("synthetic-cancel-plan")), BundleSha256: plan.BundleSha256, Call: call, Payload: payload, Settings: plan.Settings})
	if err != nil {
		t.Fatal(err)
	}
	artifacts["input.json"] = input
	var inspection []map[string]any
	if err := json.Unmarshal(artifacts["container-inspect.json"], &inspection); err != nil {
		t.Fatal(err)
	}
	inspection[0]["Config"].(map[string]any)["Entrypoint"] = []string{"/review-broker", "review-multipass-broker"}
	inspection[0]["HostConfig"].(map[string]any)["Ulimits"] = []any{map[string]any{"Name": "fsize", "Soft": 16 * 1024 * 1024, "Hard": 16 * 1024 * 1024}}
	artifacts["container-inspect.json"], err = json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics = currentQgContainmentWriteFixture(t, attempt, receipt.Owner, 143, artifacts)
	return input, []byte{}, diagnostics, image, settings, attempt, profile, artifacts
}

// The new profile verifies the real-shaped pre-input cancellation boundary.
func TestWinnerMultipassContainmentAuthenticatesNewCanceledProfile(t *testing.T) {
	input, output, diagnostics, image, settings, _, profile, _ := winnerMultipassCanceledContainmentFixture(t)
	if _, err := authenticateWinnerMultipassContainmentInternal(input, output, diagnostics, true, image, settings, profile); err != nil {
		t.Fatal(err)
	}
}

// A historical entrypoint/journal ceiling cannot be relabeled as the new profile.
func TestWinnerMultipassContainmentRejectsHistoricalContainerProfile(t *testing.T) {
	_, _, _, image, _, _, profile, artifacts := winnerMultipassCanceledContainmentFixture(t)
	var inspection []map[string]any
	if err := json.Unmarshal(artifacts["container-inspect.json"], &inspection); err != nil {
		t.Fatal(err)
	}
	owner := inspection[0]["Config"].(map[string]any)["Labels"].(map[string]any)["com.urnetwork.review.owner"].(string)
	network := inspection[0]["HostConfig"].(map[string]any)["NetworkMode"].(string)
	inspection[0]["Config"].(map[string]any)["Entrypoint"] = []string{"/review-broker", "review-broker"}
	content, _ := json.Marshal(inspection)
	if validateWinnerMultipassContainer(content, owner, image, network, profile) == nil {
		t.Fatal("historical entrypoint qualified new per-call transport")
	}
	inspection[0]["Config"].(map[string]any)["Entrypoint"] = []string{"/review-broker", "review-multipass-broker"}
	inspection[0]["HostConfig"].(map[string]any)["Ulimits"] = []any{map[string]any{"Name": "fsize", "Soft": 1024 * 1024, "Hard": 1024 * 1024}}
	content, _ = json.Marshal(inspection)
	if validateWinnerMultipassContainer(content, owner, image, network, profile) == nil {
		t.Fatal("old clipped journal ceiling qualified new raw response retention")
	}
}

// Actual process authority remains bounded independently of declared source size.
func TestWinnerMultipassContainmentRejectsParentMountAndAmbientProxy(t *testing.T) {
	_, _, _, image, _, _, profile, artifacts := winnerMultipassCanceledContainmentFixture(t)
	var inspection []map[string]any
	if err := json.Unmarshal(artifacts["container-inspect.json"], &inspection); err != nil {
		t.Fatal(err)
	}
	owner := inspection[0]["Config"].(map[string]any)["Labels"].(map[string]any)["com.urnetwork.review.owner"].(string)
	network := inspection[0]["HostConfig"].(map[string]any)["NetworkMode"].(string)
	mounts := inspection[0]["Mounts"].([]any)
	inspection[0]["Mounts"] = append(mounts, map[string]any{"Type": "bind", "Source": "/synthetic/parent", "Destination": "/parent", "RW": false})
	content, _ := json.Marshal(inspection)
	if validateWinnerMultipassContainer(content, owner, image, network, profile) == nil {
		t.Fatal("parent context mount admitted")
	}
	inspection[0]["Mounts"] = mounts
	inspection[0]["Config"].(map[string]any)["Env"] = []string{"HTTP_PROXY=http://proxy.synthetic.example"}
	content, _ = json.Marshal(inspection)
	if validateWinnerMultipassContainer(content, owner, image, network, profile) == nil {
		t.Fatal("ambient provider proxy admitted")
	}
}

// A hidden started journal invalidates cancellation even if not in the manifest.
func TestWinnerMultipassContainmentCanceledStateMustActuallyBeEmpty(t *testing.T) {
	input, output, diagnostics, image, settings, _, profile, _ := winnerMultipassCanceledContainmentFixture(t)
	if err := os.WriteFile(filepath.Join(profile.StateDirectory, "call.started.json"), []byte(`{"synthetic":"hidden started call"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateWinnerMultipassContainmentInternal(input, output, diagnostics, true, image, settings, profile); err == nil {
		t.Fatal("canceled receipt concealed a dispatched call")
	}
}

// Extra or relabeled originals cannot disappear behind a valid manifest hash.
func TestWinnerMultipassContainmentRejectsUnplannedArtifactCensus(t *testing.T) {
	input, output, diagnostics, image, settings, attempt, profile, artifacts := winnerMultipassCanceledContainmentFixture(t)
	var receipt currentQgQualificationWrapperReceipt
	if err := json.Unmarshal(diagnostics, &receipt); err != nil {
		t.Fatal(err)
	}
	artifacts["unplanned-source-context.json"] = []byte(`{"synthetic":"not in the frozen input"}`)
	diagnostics = currentQgContainmentWriteFixture(t, attempt, receipt.Owner, 143, artifacts)
	if _, err := authenticateWinnerMultipassContainmentInternal(input, output, diagnostics, true, image, settings, profile); err == nil {
		t.Fatal("unplanned context artifact was silently omitted")
	}
}

// Token/raw/visible bounds are distinct, and unknown leaves have no allowance.
func TestWinnerMultipassContainmentRetainsDeclaredRawBounds(t *testing.T) {
	settings := defaultWinnerMultipassSettings()
	if winnerMultipassCapturedLeafLimit("transport.response", settings) != 16*1024*1024 || winnerMultipassCapturedLeafLimit("tokenize.response.json", settings) != 8*1024*1024 || winnerMultipassCapturedLeafLimit("broker.stdout", settings) != 4*1024*1024 || winnerMultipassCapturedLeafLimit("evidence-manifest.json", settings) != 256*1024 || winnerMultipassCapturedLeafLimit("model-key", settings) != 0 {
		t.Fatal("separate containment artifact profile changed")
	}
}
