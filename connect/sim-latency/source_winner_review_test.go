// Pure broker/API seams prove that performance or an empty model response
// never substitutes for an exact, complete, authenticated honesty decision.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// A completed uncertain judgment stays uncertain across supervisor retries and
// clock renewal, rather than repeatedly sampling until approval appears.
func TestWinnerReviewCompletedUncertaintyCannotBeResampled(t *testing.T) {
	result, template, policy := winnerBrokerFixture()
	result.Review.SourceDisposition = "deferred"
	content, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	request := winnerReviewBrokerRequest{Operation: "review_elected_winner", Template: template,
		Request: controller.CompetitionWinnerReviewRequest{Policy: policy, StartedAt: time.Unix(10, 0), Deadline: time.Unix(20, 0)}}
	path := filepath.Join(t.TempDir(), "completed.json")
	calls := 0
	run := func() ([]byte, error) {
		calls++
		if calls > 1 {
			t.Fatal("uncertain completed review was sampled again")
		}
		return content, nil
	}
	for i := range 2 {
		request.Request.StartedAt = request.Request.StartedAt.Add(time.Duration(i) * time.Hour)
		request.Request.Deadline = request.Request.Deadline.Add(time.Duration(i) * time.Hour)
		output, _, err := winnerReviewCompletedResponse(path, request, run)
		if err != nil || string(output) != string(content) {
			t.Fatal("completed uncertainty was not retained", err)
		}
		var replay winnerReviewBrokerResult
		if err := json.Unmarshal(output, &replay); err != nil {
			t.Fatal(err)
		}
		if validateWinnerReviewBrokerResult(&replay, template, policy) == nil {
			t.Fatal("retained uncertainty became an accepted judgment")
		}
	}
	if calls != 1 {
		t.Fatal("wrong broker invocation count", calls)
	}
}

// Malformed and empty completed model output is evidence, not permission to
// obtain another model answer under the same immutable candidate identity.
func TestWinnerReviewCompletedMalformedResponsesAreRetained(t *testing.T) {
	for _, content := range [][]byte{nil, []byte("not-json")} {
		path := filepath.Join(t.TempDir(), "completed.json")
		request := winnerReviewBrokerRequest{Operation: "review_elected_winner"}
		for i := range 2 {
			output, _, err := winnerReviewCompletedResponse(path, request, func() ([]byte, error) {
				if i != 0 {
					t.Fatal("malformed completed response was sampled again")
				}
				return content, nil
			})
			if err != nil || string(output) != string(content) {
				t.Fatal("completed malformed response changed", err)
			}
		}
	}
}

// A failed transport has no completed judgment to preserve and may retry.
// Once completed, a changed packet or policy must fail closed, not rerun.
func TestWinnerReviewCompletedResponseBindsContextAndRetriesTransport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completed.json")
	request := winnerReviewBrokerRequest{Operation: "review_elected_winner"}
	transportErr := errors.New("synthetic transport unavailable")
	if _, _, err := winnerReviewCompletedResponse(path, request, func() ([]byte, error) {
		return nil, transportErr
	}); !errors.Is(err, transportErr) {
		t.Fatal("transport error was lost", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("transport failure cached as a completed judgment", err)
	}
	if _, _, err := winnerReviewCompletedResponse(path, request, func() ([]byte, error) {
		return []byte("synthetic completed response"), nil
	}); err != nil {
		t.Fatal(err)
	}
	request.Instructions = "changed trusted instructions"
	if _, _, err := winnerReviewCompletedResponse(path, request, func() ([]byte, error) {
		t.Fatal("different request reused the response identity")
		return nil, nil
	}); err == nil {
		t.Fatal("changed review context was accepted")
	}
}

// Every finding is real fixture data, never a default approval in production.
func winnerBrokerFixture() (winnerReviewBrokerResult, model.CompetitionIntegrityReviewRequest, model.CompetitionAutomationPolicy) {
	evidence := "Synthetic explanation of all six review findings."
	digest := sha256.Sum256([]byte(evidence))
	template := model.CompetitionIntegrityReviewRequest{JobId: server.NewId(), ExpectedRevision: 1, IdempotencyKey: "test-winner-review", CriteriaVersion: "review/v2",
		PatchSha256: strings.Repeat("a", 64), BaseSourceLockSha256: strings.Repeat("b", 64), ScoreSha256: strings.Repeat("c", 64), BaselineSha256: strings.Repeat("d", 64), ArtifactManifestSha256: strings.Repeat("e", 64),
		ExpectedDecisionSha256: strings.Repeat("f", 64), ReviewerModel: model.CompetitionWinnerReviewModel, ReviewerReasoning: model.CompetitionWinnerReviewReasoning}
	review := template
	review.RewardDisposition, review.SourceDisposition = "approved", "approved"
	review.PublicExplanation, review.WithheldEvidenceExplanation = "All six synthetic criteria passed.", "Detailed analysis retained privately."
	review.PrivateEvidenceSha256 = hex.EncodeToString(digest[:])
	review.ReasonCodes = []string{"APPROVED"}
	for _, criterion := range []string{"measurement_integrity", "workload_generalization", "containment", "patch_scope", "reproducibility", "source_safety"} {
		review.Findings = append(review.Findings, model.CompetitionReviewFinding{Criterion: criterion, Disposition: "pass", ReasonCode: "APPROVED", Explanation: "Synthetic criterion evidence was reviewed.", EvidenceSha256: review.PrivateEvidenceSha256})
	}
	return winnerReviewBrokerResult{Schema: 1, Model: model.CompetitionWinnerReviewModel, Reasoning: model.CompetitionWinnerReviewReasoning, NoMainCredentials: true, NoCandidateExecution: true, Review: review, PrivateEvidence: evidence}, template,
		model.CompetitionAutomationPolicy{ReviewCriteriaVersion: "review/v2", RepeatRewardPolicy: "allow", WinnerReviewPolicy: model.CompetitionWinnerReviewPolicy}
}

func TestWinnerReviewBrokerRequiresExactCompletePinnedJudgment(t *testing.T) {
	result, template, policy := winnerBrokerFixture()
	if err := validateWinnerReviewBrokerResult(&result, template, policy); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*winnerReviewBrokerResult){
		func(r *winnerReviewBrokerResult) { r.Model = "other-model" }, func(r *winnerReviewBrokerResult) { r.Reasoning = "low" },
		func(r *winnerReviewBrokerResult) { r.NoMainCredentials = false }, func(r *winnerReviewBrokerResult) { r.NoCandidateExecution = false },
		func(r *winnerReviewBrokerResult) { r.Review.JobId = server.NewId() }, func(r *winnerReviewBrokerResult) { r.Review.ExpectedRevision++ },
		func(r *winnerReviewBrokerResult) { r.Review.ExpectedDecisionSha256 = strings.Repeat("0", 64) },
		func(r *winnerReviewBrokerResult) { r.Review.SourceDisposition = "deferred" }, func(r *winnerReviewBrokerResult) { r.Review.Findings = r.Review.Findings[:5] },
		func(r *winnerReviewBrokerResult) { r.PrivateEvidence += "changed" }, func(r *winnerReviewBrokerResult) { r.Review.PrivateEvidenceSha256 = strings.Repeat("0", 64) },
		func(r *winnerReviewBrokerResult) { r.Review.PublicExplanation = "" }, func(r *winnerReviewBrokerResult) { r.Review.ReviewerReasoning = "low" },
	} {
		candidate, identity, frozen := winnerBrokerFixture()
		mutate(&candidate)
		if validateWinnerReviewBrokerResult(&candidate, identity, frozen) == nil {
			t.Fatal("incomplete, stale or unqualified broker judgment was accepted")
		}
	}
}

func TestWinnerReviewResponseRejectsCaseAliasesAndDuplicates(t *testing.T) {
	for _, content := range []string{`{"model":"gpt-6.1-sol","Model":"other"}`, `{"review":{"reviewer_model":"gpt-6.1-sol","Reviewer_Model":"other"}}`} {
		if rejectWinnerReviewCaseAliases([]byte(content)) == nil {
			t.Fatal("case-folded broker field alias accepted")
		}
	}
	var result winnerReviewBrokerResult
	if decodeStrictJSONBytes([]byte(`{"model":"other","model":"gpt-6.1-sol"}`), &result, "synthetic broker") == nil {
		t.Fatal("duplicate broker field accepted")
	}
}

func TestWinnerReviewBrokerRejectsOnlyWithSubstantiatedDisqualification(t *testing.T) {
	r, template, policy := winnerBrokerFixture()
	r.Review.RewardDisposition, r.Review.SourceDisposition = "disqualified", "rejected"
	r.Review.ReasonCodes = []string{"SCORE_TAMPERING"}
	if validateWinnerReviewBrokerResult(&r, template, policy) == nil {
		t.Fatal("all-passing findings manufactured a disqualification")
	}
	r.Review.Findings[0].Disposition, r.Review.Findings[0].ReasonCode = "fail", "SCORE_TAMPERING"
	if err := validateWinnerReviewBrokerResult(&r, template, policy); err != nil {
		t.Fatal(err)
	}
	r.Review.ReasonCodes = []string{"PRODUCT_INTEGRATION_RISK"}
	if validateWinnerReviewBrokerResult(&r, template, policy) == nil {
		t.Fatal("an integration problem was turned into dishonesty")
	}
}

// This transport executes no network I/O and uses only synthetic identities.
type winnerReviewTransport func(*http.Request) (*http.Response, error)

// Retry keys survive lost API responses, and no later answer can replace the
// accepted bytes. A partial staging file never becomes the published cache.
func TestWinnerReviewJudgmentCacheIsAtomicAndNonOverwriting(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "judgment.json")
	partial := filepath.Join(root, ".winner-review-cache-interrupted")
	if err := os.WriteFile(partial, []byte("partial"), 0o400); err != nil {
		t.Fatal(err)
	}
	content := []byte("{\"review\":\"synthetic accepted judgment\"}\n")
	if err := cacheWinnerReviewJudgment(path, content); err != nil {
		t.Fatal(err)
	}
	if err := cacheWinnerReviewJudgment(path, content); err != nil {
		t.Fatal("identical retry failed", err)
	}
	if err := cacheWinnerReviewJudgment(path, []byte("different")); err == nil {
		t.Fatal("accepted judgment was overwritten")
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(content) {
		t.Fatal("cache bytes changed", err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatal("historical interrupted evidence was deleted", err)
	}
}

func (self winnerReviewTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

func TestWinnerReviewApiBindsCredentialPathAndReceipt(t *testing.T) {
	result, _, _ := winnerBrokerFixture()
	round := server.NewId()
	request := controller.CompetitionWinnerReviewRequest{RoundId: round, JobId: result.Review.JobId}
	digest, _ := model.CompetitionAutomationDigest(result.Review)
	receipt := model.CompetitionIntegrityReviewResult{ReviewId: server.NewId(), RoundId: round, Request: result.Review, RequestSha256: digest}
	content, _ := json.Marshal(receipt)
	calls := 0
	client := &http.Client{Transport: winnerReviewTransport(func(message *http.Request) (*http.Response, error) {
		calls++
		if message.URL.Path != "/competition/v2/rounds/"+round.String()+"/reviews" || message.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Fatal("wrong review authority or API path")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(content))), Header: http.Header{}}, nil
	})}
	if _, err := postWinnerReview(context.Background(), client, "https://api.example", "synthetic-token", request, result.Review); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unexpected review requests")
	}
	receipt.Request.PublicExplanation = "tampered receipt"
	content, _ = json.Marshal(receipt)
	if _, err := postWinnerReview(context.Background(), client, "https://api.example", "synthetic-token", request, result.Review); err == nil {
		t.Fatal("receipt body was not authenticated")
	}
}

func TestWinnerReviewApiDoesNotFollowCredentialRedirect(t *testing.T) {
	result, _, _ := winnerBrokerFixture()
	calls := 0
	client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://unrelated.example/"}}, Body: io.NopCloser(strings.NewReader("private error must stay private"))}, nil
	})}
	_, err := postWinnerReview(context.Background(), client, "https://api.example", "synthetic-token", controller.CompetitionWinnerReviewRequest{RoundId: server.NewId(), JobId: result.Review.JobId}, result.Review)
	if err == nil || calls != 1 || strings.Contains(err.Error(), "private error") {
		t.Fatal("redirect leaked authority or private response", err)
	}
}

func TestWinnerReviewTrustedExecutableMayExceedOld64MiBLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("#!/bin/sh\nprintf 'accepted'\nexit 0\n"); err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(64*1024*1024 + 1); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	file.Close()
	output, err := runSourceSupervisorProgram(context.Background(), path, hex.EncodeToString(hash.Sum(nil)), nil, []string{"PATH=/usr/bin:/bin"})
	if err != nil || string(output) != "accepted" {
		t.Fatalf("qualified larger control executable rejected: %s %v", output, err)
	}
}
