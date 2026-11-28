// The selected Mini family is requested by exact documented snapshot; private
// evidence preserves the real provider identity rather than relabeling it.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Exact snapshot response passes while a floating alias or another revision
// cannot silently satisfy the immutable provider contract.
func TestWinnerReviewSnapshotPinRejectsFloatingOrDifferentRevision(t *testing.T) {
	stream := winnerModelFixtureStream(t, "synthetic output")
	if _, err := readWinnerModelResponse(strings.NewReader(stream), defaultWinnerModelSettings()); err != nil {
		t.Fatal(err)
	}
	for _, different := range []string{"gpt-5.4-mini", "gpt-5.4-mini-synthetic-other-revision", "gpt-5.4"} {
		changed := strings.ReplaceAll(stream, winnerReviewMiniProviderSnapshot, different)
		if _, err := readWinnerModelResponse(strings.NewReader(changed), defaultWinnerModelSettings()); err == nil {
			t.Fatalf("unqualified provider revision accepted: %s", different)
		}
	}
}

// Family-level policy authority and exact provider execution are distinct
// fields, both retained and bound by the private evidence digest.
func TestWinnerReviewEvidenceRetainsActualSnapshotAndSelectedPolicy(t *testing.T) {
	request, judgment := winnerModelFixture()
	output, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: winnerRefusalTransport(func(message *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(winnerModelFixtureStream(t, string(output))))}, nil
	})}
	result, err := executeWinnerModel(context.Background(), client, "synthetic-private-key", request, defaultWinnerModelSettings())
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Model          string `json:"model"`
		RequestedModel string `json:"requested_model"`
		PolicyModel    string `json:"policy_model"`
	}
	if err := json.Unmarshal([]byte(result.PrivateEvidence), &evidence); err != nil {
		t.Fatal(err)
	}
	if result.Model != model.CompetitionWinnerReviewModel || evidence.Model != winnerReviewMiniProviderSnapshot || evidence.RequestedModel != winnerReviewMiniProviderSnapshot || evidence.PolicyModel != model.CompetitionWinnerReviewModel {
		t.Fatalf("provider execution identity was relabeled: %+v", evidence)
	}
}
